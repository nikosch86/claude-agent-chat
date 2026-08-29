package main

// Codex CLI wiring. Codex has no equivalent of Claude Code's Monitor tool, so
// incoming chat traffic is delivered by a small detached bridge process: it
// runs the same listen loop as `agent-chat listen` and forwards each matching
// line into the running Codex session via `codex queue --thread <id>
// --message <text>` (durable queue with idle dispatch, Codex CLI >= 0.149).
//
//	SessionStart hook  -> hook-start --emit codex   (join + primer + spawn bridge)
//	<bridge>           -> codex queue per incoming message
//	SessionEnd hook    -> hook-stop                 (quit record + stop bridge)
//
// The bridge holds the same per-nick listener singleton lock as `listen`, so a
// Claude Monitor listener and a codex bridge can never double-consume one
// nick's cursor — newest wins, exactly like two listeners. When it does lose
// the lock (or exits for any other reason) it forwards the listen loop's
// farewell into the session first: the bridge is that session's only inbox, so
// an unannounced exit is indistinguishable from a quiet chat.
//
// Delivery is at-least-once, not fire-and-forget. A failed `codex queue` is
// reported back to drainListen, which holds the read cursor behind the
// undelivered record and re-offers it on the next poll; only a delivery that
// succeeded advances the cursor. Retries are throttled to bridgeRetryInterval,
// and the bridge gives up (assuming the session is gone) once delivery has
// been broken for bridgeDeliverGrace.
//
// Lifetime: the SessionEnd hook stops the bridge on a clean exit. `codex
// queue` accepts a thread id whether or not a session is running it (the
// queue is durable and replayed on resume), so a bridge orphaned by an unclean
// exit would keep swallowing messages into a dead thread. As a backstop the
// bridge also watches the liveness signal Codex maintains itself: it holds an
// exclusive flock on $CODEX_HOME/thread-writer-locks/<thread>.lock for as long
// as the session owns the thread (released or deleted when the process goes
// away). Once that lock has been seen held and is then observed free on
// several consecutive probes, the bridge exits.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// bridgeDeliverGrace is how long `codex queue` may keep failing before the
// bridge concludes the session (or the codex binary) is gone and exits. This
// is a time window rather than an attempt count because a failed delivery now
// stalls the listen cursor and is retried on the next poll — counting attempts
// would burn the whole budget within a second of one transient hiccup. A
// success resets it.
var bridgeDeliverGrace = 60 * time.Second

// bridgeRetryInterval throttles those retries: the listen loop polls five
// times a second and there is no point spawning `codex queue` that often.
var bridgeRetryInterval = 1 * time.Second

// codexQueueTimeout bounds a single `codex queue` invocation.
var codexQueueTimeout = 10 * time.Second

// codexNotifyMaxBytes is the per-message cap on the codex path. `codex queue`
// takes the body as one argv element, so the real ceiling is the kernel's
// per-argument limit (MAX_ARG_STRLEN — 128 KiB on Linux), not the few hundred
// bytes Claude's Monitor cuts at; staying well under it leaves room for the
// framing the bridge adds. Anything past it still arrives as a clipped notice
// carrying a `full` command, exactly as on the Claude path.
const codexNotifyMaxBytes = 96 * 1024

// codexBridgeFarewell replaces the Monitor-worded farewell on this path. It
// must reach the session: the bridge is a codex session'"'"'s only inbox, so an
// unannounced exit is silent deafness.
const codexBridgeFarewell = `[agent-chat] inbox bridge stopped — this codex session no longer receives peer messages automatically. Usually it was superseded by a newer bridge for the same nick (harmless: that one owns the inbox now) or the session is ending. If this session is still working, restart delivery with: agent-chat codex-bridge --thread <this session id> --as <nick>. Until then read the inbox by hand: agent-chat history --to me --tail 20 --format text`

// codexListenOpts is the bridge'"'"'s framing of the shared listen loop.
func codexListenOpts() listenOpts {
	return listenOpts{
		format:   func(line []byte, r Record) []byte { return notifyLineMax(line, r, codexNotifyMaxBytes) },
		farewell: codexBridgeFarewell,
	}
}

// Liveness probe of the codex thread writer lock (see the package comment).
// The lock must be observed held at least once before its absence counts, so
// a thread hosted elsewhere (remote app server) or a slow start never trips
// it; after that, bridgeDeadProbes consecutive free/missing observations end
// the bridge.
//
// The window is a minute rather than the fifteen seconds it started at.
// Codex has been observed leaving the lock file present but unheld while its
// session is still very much alive (seen on a resumed thread), which killed a
// live session's bridge and left it silently deaf. Holding the lock is the
// normal steady state, so those releases look transient — a minute of
// *continuous* absence still reaps a genuinely orphaned bridge promptly while
// no longer tripping over one.
var (
	bridgeProbeInterval = 5 * time.Second
	bridgeDeadProbes    = 12
)

func runCodexBridge(args []string) int {
	as, args, err := extractAs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codex-bridge: %v\n", err)
		return 2
	}
	fs := flag.NewFlagSet("codex-bridge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	thread := fs.String("thread", "", "codex thread/session id to queue messages into")
	foreground := fs.Bool("foreground", false, "run in the foreground instead of detaching")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: agent-chat codex-bridge --thread ID [--as NICK] [--foreground]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *thread == "" {
		fmt.Fprintln(os.Stderr, "codex-bridge: --thread is required (the codex session id from the hook payload)")
		return 2
	}
	nick, err := resolveNick(as)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codex-bridge: %v\n", err)
		return 2
	}
	if !*foreground {
		if err := spawnCodexBridge(nick, *thread); err != nil {
			fmt.Fprintf(os.Stderr, "codex-bridge: %v\n", err)
			return 1
		}
		return 0
	}
	return codexBridgeForeground(nick, *thread)
}

// spawnCodexBridge starts the detached bridge process for nick, logging to
// codex-bridge.log in the nick's agent dir. Called by hook-start --emit codex
// and by a bare `codex-bridge` invocation (which re-execs itself).
func spawnCodexBridge(nick, thread string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate own binary: %w", err)
	}
	logPath := bridgeLogPath(nick)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(self, "codex-bridge", "--foreground", "--thread", thread, "--as", nick)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bridge: %w", err)
	}
	return cmd.Process.Release()
}

func codexBridgeForeground(nick, thread string) int {
	// Same singleton as listen: a newer listener/bridge for this nick evicts us
	// (SIGTERM), and starting here evicts any incumbent.
	defer tryLockListener(nick)()

	if err := writeBridgePid(nick); err != nil {
		fmt.Fprintf(os.Stderr, "codex-bridge: warning: cannot write pidfile: %v\n", err)
	}
	defer os.Remove(bridgePidPath(nick))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stderr, "codex-bridge: forwarding messages for @%s into codex thread %s\n", nick, thread)
	go watchCodexThread(ctx, cancel, codexThreadLockPath(thread), bridgeProbeIntervalFromEnv(), bridgeDeadProbes, os.Stderr)
	w := &bridgeWriter{
		cancel: cancel,
		deliver: func(msg string) error {
			// Deliberately not the loop context: the farewell is delivered
			// after cancel, and a takeover SIGTERM must not abort a delivery
			// already in flight. Bounded by its own timeout instead.
			dctx, dcancel := context.WithTimeout(context.Background(), codexQueueTimeout)
			defer dcancel()
			return codexQueue(dctx, thread, msg)
		},
	}
	return listenLoopOpts(ctx, nick, w, codexListenOpts())
}

// bridgeWriter adapts the listen loop's line stream into `codex queue`
// deliveries. A delivery that fails is reported back to the caller rather than
// swallowed, so drainListen holds the cursor and retries the message instead
// of losing it.
type bridgeWriter struct {
	buf     []byte
	deliver func(string) error
	cancel  context.CancelFunc

	// now is the clock, injectable for tests; nil means time.Now.
	now       func() time.Time
	firstFail time.Time
	lastTry   time.Time
}

// errBridgeBackoff stalls the cursor without spending an attempt, while we
// wait out bridgeRetryInterval after a failure.
var errBridgeBackoff = errors.New("codex-bridge: waiting to retry delivery")

func (w *bridgeWriter) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *bridgeWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		nl := bytes.IndexByte(w.buf, '\n')
		if nl < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:nl]))
		w.buf = w.buf[nl+1:]
		if line == "" {
			continue
		}
		// Internal notices (the "[agent-chat]" farewell) are session-level
		// news, not peer traffic: delivered verbatim, without the message
		// framing, so a bridge going away says so in-session.
		msg := line
		if !strings.HasPrefix(line, "[agent-chat]") {
			msg = "New agent-chat message:\n" + renderBridgeLine(line)
		}
		if err := w.attempt(msg); err != nil {
			// Undelivered. Drop what we buffered: the stalled cursor means
			// this line and anything after it are re-read next poll, so
			// keeping them here would deliver them twice.
			w.buf = w.buf[:0]
			return len(p), err
		}
	}
	return len(p), nil
}

// attempt delivers one message, rate-limiting retries after a failure and
// ending the bridge once delivery has been broken for bridgeDeliverGrace.
func (w *bridgeWriter) attempt(msg string) error {
	now := w.clock()
	if !w.firstFail.IsZero() && now.Sub(w.lastTry) < bridgeRetryInterval {
		return errBridgeBackoff
	}
	w.lastTry = now
	err := w.deliver(msg)
	if err == nil {
		w.firstFail = time.Time{}
		return nil
	}
	if w.firstFail.IsZero() {
		w.firstFail = now
	}
	broken := now.Sub(w.firstFail)
	fmt.Fprintf(os.Stderr, "codex-bridge: deliver failed (retrying; broken for %s of %s): %v\n",
		broken.Round(time.Second), bridgeDeliverGrace, err)
	if broken >= bridgeDeliverGrace {
		fmt.Fprintln(os.Stderr, "codex-bridge: codex queue has been failing too long — session likely gone; exiting")
		w.cancel()
	}
	return err
}

// renderBridgeLine turns one raw listen line (JSON) into the text queued into
// the codex session — the Go twin of the kilo plugin's formatLine. Unparseable
// lines pass through raw.
func renderBridgeLine(line string) string {
	var r struct {
		From    string  `json:"from"`
		Text    *string `json:"text"`
		Path    string  `json:"path"`
		Note    string  `json:"note"`
		Clipped bool    `json:"clipped"`
		Bytes   int     `json:"bytes"`
		Preview string  `json:"preview"`
		Full    string  `json:"full"`
	}
	if json.Unmarshal([]byte(line), &r) != nil {
		return line
	}
	who := "someone"
	if r.From != "" {
		who = "@" + r.From
	}
	if r.Clipped {
		what := ""
		if r.Path != "" {
			what = fmt.Sprintf(" (file: %s)", r.Path)
		}
		return fmt.Sprintf("%s sent a %d-byte message%s, too long to show here. It starts: %q — run `%s` to read all of it.", who, r.Bytes, what, r.Preview, r.Full)
	}
	if r.Path != "" {
		s := fmt.Sprintf("%s shared a file: %s", who, r.Path)
		if r.Note != "" {
			s += " — " + r.Note
		}
		return s
	}
	if r.Text != nil {
		return who + ": " + *r.Text
	}
	return line
}

// codexQueue pushes one message into the codex session's durable queue. The
// body travels as a single argv element — no shell in between, so peer text
// can never be expanded or split.
func codexQueue(ctx context.Context, thread, msg string) error {
	cmd := exec.CommandContext(ctx, codexBin(), "queue", "--thread", thread, "--message", msg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// bridgeProbeIntervalFromEnv lets tests shorten the liveness probe interval
// (AGENT_CHAT_CODEX_PROBE_MS); otherwise bridgeProbeInterval.
func bridgeProbeIntervalFromEnv() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("AGENT_CHAT_CODEX_PROBE_MS")); err == nil && v > 0 {
		return time.Duration(v) * time.Millisecond
	}
	return bridgeProbeInterval
}

// codexHome mirrors Codex's own resolution: $CODEX_HOME, else ~/.codex.
func codexHome() string {
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(home, ".codex")
}

func codexThreadLockPath(thread string) string {
	return filepath.Join(codexHome(), "thread-writer-locks", thread+".lock")
}

// watchCodexThread polls the thread writer lock every interval and calls
// cancel once the lock — previously seen held — has been free or missing for
// deadProbes consecutive probes. Returns when ctx ends.
func watchCodexThread(ctx context.Context, cancel context.CancelFunc, lockPath string, interval time.Duration, deadProbes int, log *os.File) {
	t := time.NewTicker(interval)
	defer t.Stop()
	seenHeld := false
	dead := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		switch probeThreadLock(lockPath) {
		case lockHeld:
			seenHeld = true
			dead = 0
		case lockFree, lockMissing:
			if !seenHeld {
				continue
			}
			dead++
			if dead >= deadProbes {
				fmt.Fprintf(log, "codex-bridge: codex thread writer lock %s no longer held — session gone; exiting\n", lockPath)
				cancel()
				return
			}
		case lockUnknown:
			// Cannot tell (no flock on this platform, EACCES, …): never exit on it.
		}
	}
}

type lockState int

const (
	lockUnknown lockState = iota
	lockMissing
	lockFree
	lockHeld
)

func codexBin() string {
	if v := os.Getenv("AGENT_CHAT_CODEX_BIN"); v != "" {
		return v
	}
	return "codex"
}

func bridgePidPath(nick string) string {
	return filepath.Join(chatHome(), "agents", nick, "codex-bridge.pid")
}

func bridgeLogPath(nick string) string {
	return filepath.Join(chatHome(), "agents", nick, "codex-bridge.log")
}

func writeBridgePid(nick string) error {
	p := bridgePidPath(nick)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
}

// stopCodexBridge terminates the recorded bridge for nick, if any. The pidfile
// is removed unconditionally; the signal is only sent when the pid still looks
// like one of our own bridges (recycled-pid guard, same approach as the
// listener lock takeover). No-op when no bridge was ever started — i.e. on
// every Claude/kilo session.
func stopCodexBridge(nick string) {
	p := bridgePidPath(nick)
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	os.Remove(p)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return
	}
	terminateBridge(pid)
}
