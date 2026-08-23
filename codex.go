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
// nick's cursor — newest wins, exactly like two listeners.
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

// bridgeMaxDeliverFails is how many consecutive `codex queue` failures the
// bridge tolerates before concluding the session (or the codex binary) is gone
// and exiting. A success resets the counter.
var bridgeMaxDeliverFails = 5

// Liveness probe of the codex thread writer lock (see the package comment).
// The lock must be observed held at least once before its absence counts, so
// a thread hosted elsewhere (remote app server) or a slow start never trips
// it; after that, bridgeDeadProbes consecutive free/missing observations end
// the bridge.
var (
	bridgeProbeInterval = 5 * time.Second
	bridgeDeadProbes    = 3
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
			return codexQueue(ctx, thread, msg)
		},
	}
	return listenLoop(ctx, nick, w)
}

// bridgeWriter adapts listenLoop's line stream into `codex queue` deliveries.
// Internal notices (the "[agent-chat]" farewell/self-heal lines) are dropped —
// they explain Monitor mechanics that do not exist on the codex side.
type bridgeWriter struct {
	buf     []byte
	deliver func(string) error
	cancel  context.CancelFunc
	fails   int
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
		if line == "" || strings.HasPrefix(line, "[agent-chat]") {
			continue
		}
		msg := "New agent-chat message:\n" + renderBridgeLine(line)
		if err := w.deliver(msg); err != nil {
			w.fails++
			fmt.Fprintf(os.Stderr, "codex-bridge: deliver failed (%d/%d): %v\n", w.fails, bridgeMaxDeliverFails, err)
			if w.fails >= bridgeMaxDeliverFails {
				fmt.Fprintln(os.Stderr, "codex-bridge: codex queue keeps failing — session likely gone; exiting")
				w.cancel()
			}
			continue
		}
		w.fails = 0
	}
	return len(p), nil
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
