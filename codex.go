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
// Codex fires SessionStart more than once per thread — at startup, on resume,
// and after every automatic compaction — so hook-start spawns a bridge only
// when none is alive for this nick and thread (bridgeAlive). Respawning on
// every SessionStart evicted a perfectly good bridge at each compaction, and
// the evicted one announced its exit into a session that was still being
// served by its replacement.
//
// The bridge holds the same per-nick listener singleton lock as `listen`, so a
// Claude Monitor listener and a codex bridge can never double-consume one
// nick's cursor — newest wins, exactly like two listeners.
//
// Exit and farewell policy. The bridge is that session's only inbox, so an
// exit that leaves the session deaf must be announced in-session — but only
// such an exit. Three ways out:
//   - evicted (SIGTERM/SIGINT: another listener or bridge took the nick, or
//     someone killed us), unless the session is demonstrably gone — its
//     writer lock was seen held earlier and every fresh probe now finds it
//     free: the farewell is queued, the session is alive and now deaf;
//   - quiet stop (SIGUSR1: hook-stop at SessionEnd, or a bridge for the same
//     thread taking over): nothing — delivery continues elsewhere, or the
//     session is ending;
//   - session gone (writer lock released, or `codex queue` failing for
//     bridgeDeliverGrace): nothing — there is nobody to tell, and Codex's
//     queue is durable, so a farewell parked in an ended thread would be
//     replayed as the first turn if that thread were ever resumed.
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
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

// codexBridgeFarewell is the notice queued into a session whose bridge was
// evicted while the session is still alive. It names the nick and thread so
// the recovery command can be run verbatim, and says where to run it: a
// `codex-bridge` started from inside Codex's sandbox dies with the tool
// call's PID namespace and cannot write the queue database anyway.
func codexBridgeFarewell(nick, thread string) string {
	return fmt.Sprintf("[agent-chat] inbox bridge stopped — this codex session no longer receives peer messages automatically. "+
		"Another agent-chat listener or bridge took over nick %s (a Claude Code session started in this repo, an `agent-chat listen`, or a second bridge). "+
		"If this session is still working, restart delivery from OUTSIDE the Codex sandbox — a host shell, or a command run with escalated permissions: `agent-chat codex-bridge --thread %s --as %s`. "+
		"Running `agent-chat listen` here does not help (it cannot deliver into this session). "+
		"Until the bridge is back, read the inbox by hand: agent-chat history --to me --tail 20 --format text",
		nick, thread, nick)
}

// codexListenOpts is the bridge's framing of the shared listen loop. The
// farewell is decided per exit by bridgeRun and filled in by the caller.
func codexListenOpts() listenOpts {
	return listenOpts{
		format: func(line []byte, r Record) []byte { return notifyLineMax(line, r, codexNotifyMaxBytes) },
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
		// A manual restart from inside Codex's sandbox can never work: the
		// queue database is read-only there and the detached process dies
		// with the tool call's PID namespace. Say so instead of spawning a
		// bridge that fails for a minute and vanishes.
		if err := codexHomeWritable(); err != nil {
			fmt.Fprintf(os.Stderr, "codex-bridge: %s is not writable here (%v) — this looks like Codex's sandbox, where a bridge cannot write the queue database and dies with the tool call. Run this from a host shell, or with escalated permissions.\n", codexHome(), err)
			return 1
		}
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

// bridgeAlive reports the pid of a codex-bridge currently serving nick and
// thread, judged from the pidfile plus that process's argv — never the pidfile
// alone, since a stale one (or a recycled pid) must not count. It is what
// lets hook-start leave a working bridge alone when Codex re-fires
// SessionStart for a thread that already has one.
func bridgeAlive(nick, thread string) (int, bool) {
	pid := readPidFile(bridgePidPath(nick))
	if pid <= 0 {
		return 0, false
	}
	argv := procArgv(pid)
	if !isAgentChatArgv(argv, []string{"codex-bridge"}) || bridgeArgvThread(argv) != thread {
		return 0, false
	}
	return pid, true
}

// bridgeArgvThread returns the --thread value from a codex-bridge argv, or ""
// when there is none.
func bridgeArgvThread(argv []string) string {
	for i, a := range argv {
		if a == "--thread" && i+1 < len(argv) {
			return argv[i+1]
		}
		if strings.HasPrefix(a, "--thread=") {
			return strings.TrimPrefix(a, "--thread=")
		}
	}
	return ""
}

// bridgeExitReason is why a bridge is stopping; it decides whether the
// session is told (see the package comment).
type bridgeExitReason int32

const (
	exitUnknown     bridgeExitReason = iota
	exitEvicted                      // SIGTERM/SIGINT: another consumer took the nick, or we were killed
	exitQuiet                        // SIGUSR1: hook-stop, or a same-thread bridge taking over
	exitSessionGone                  // writer lock released, or delivery broken for the whole grace period
)

// How an evicted bridge re-checks its thread before deciding nobody is left
// to tell: a few fresh probes spread over about a second (well inside the
// evictor's 3-second takeover grace), all of which must agree the lock is
// free. One probe is not enough — Codex has been seen leaving the lock
// unheld for a moment on a live session.
var (
	bridgeFarewellSamples   = 4
	bridgeFarewellSampleGap = 300 * time.Millisecond
)

// bridgeRun is one bridge's identity plus the reason it is stopping. The
// first stop wins: a takeover signal landing after the watcher already gave
// up on the session must not turn a quiet exit into a farewell.
type bridgeRun struct {
	nick, thread string
	cancel       context.CancelFunc
	reason       atomic.Int32
	// seenHeld records that the thread's writer lock was observed held at
	// least once — the same precondition the liveness watcher uses. A lock
	// never seen held (remote app-server thread, unusual CODEX_HOME) says
	// nothing about the session.
	seenHeld atomic.Bool
	probe    func() lockState
	log      io.Writer
}

func (b *bridgeRun) stop(r bridgeExitReason) {
	b.reason.CompareAndSwap(int32(exitUnknown), int32(r))
	b.cancel()
}

// farewell is the listen loop's exit line for this bridge: the notice to queue
// into the session, or "" to leave quietly.
func (b *bridgeRun) farewell() string {
	reason := bridgeExitReason(b.reason.Load())
	seenHeld := b.seenHeld.Load()
	var samples []lockState
	if reason != exitQuiet && reason != exitSessionGone && seenHeld {
		samples = sampleThreadLock(b.probe, bridgeFarewellSamples, bridgeFarewellSampleGap)
	}
	if !wantFarewell(reason, seenHeld, samples) {
		switch reason {
		case exitQuiet:
			fmt.Fprintln(b.log, "codex-bridge: stopping quietly (session ending, or handed over to a newer bridge for the same thread)")
		case exitSessionGone:
			// Already logged by whoever decided it.
		default:
			fmt.Fprintln(b.log, "codex-bridge: evicted, but the thread's writer lock is no longer held — session gone; not queueing a farewell")
		}
		return ""
	}
	fmt.Fprintln(b.log, "codex-bridge: evicted by another listener or bridge for this nick while the session is alive — queueing a farewell")
	return codexBridgeFarewell(b.nick, b.thread)
}

// wantFarewell applies the exit policy: a quiet stop or a session already
// judged gone says nothing; an eviction is announced unless the session is
// demonstrably gone (sessionGoneAtEviction). When in doubt the session is
// told — a stale farewell replayed on a later resume is a nuisance, a live
// session left silently deaf loses messages.
func wantFarewell(reason bridgeExitReason, seenHeld bool, samples []lockState) bool {
	switch reason {
	case exitQuiet, exitSessionGone:
		return false
	}
	return !sessionGoneAtEviction(seenHeld, samples)
}

// sessionGoneAtEviction is true only when the thread's writer lock was seen
// held earlier and every fresh sample now finds it free or missing — the
// state after `/new`, or after Codex died without running SessionEnd.
func sessionGoneAtEviction(seenHeld bool, samples []lockState) bool {
	if !seenHeld || len(samples) == 0 {
		return false
	}
	for _, s := range samples {
		if s != lockFree && s != lockMissing {
			return false
		}
	}
	return true
}

func sampleThreadLock(probe func() lockState, n int, gap time.Duration) []lockState {
	out := make([]lockState, 0, n)
	for i := 0; i < n; i++ {
		if i > 0 {
			time.Sleep(gap)
		}
		out = append(out, probe())
	}
	return out
}

func codexBridgeForeground(nick, thread string) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lockPath := codexThreadLockPath(thread)
	b := &bridgeRun{
		nick: nick, thread: thread, cancel: cancel, log: os.Stderr,
		probe: func() lockState { return probeThreadLock(lockPath) },
	}

	// Signals first, so a takeover landing while we acquire the lock is
	// handled rather than taken at its default disposition (killed, no
	// cleanup, no farewell).
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, bridgeSignals()...)
	defer signal.Stop(sigs)
	go func() {
		select {
		case s := <-sigs:
			if isQuietSignal(s) {
				b.stop(exitQuiet)
			} else {
				b.stop(exitEvicted)
			}
		case <-ctx.Done():
		}
	}()

	// The pidfile goes down before the lock is contended, so hook-start and
	// an attached `listen` see a serving bridge throughout a handover. On the
	// way out it is removed before the lock is released, and only if it still
	// names us: a successor that had to fail open past a stuck incumbent
	// keeps its own.
	if err := writeBridgePid(nick); err != nil {
		fmt.Fprintf(os.Stderr, "codex-bridge: warning: cannot write pidfile: %v\n", err)
	}
	// Same singleton as listen: a newer listener/bridge for this nick evicts
	// us, and starting here evicts any incumbent — quietly when it serves the
	// same thread, since delivery simply continues from here.
	release := tryLockListenerFor(nick, thread)
	defer func() {
		removeBridgePidIfOurs(nick)
		release()
	}()
	// No shortcut for a signal that landed during acquisition: once the lock
	// is ours we are the inbox, however briefly, and an eviction then must be
	// announced like any other — the loop below sees the cancelled context
	// right after its first drain and exits through the farewell.

	// Heartbeat from its own goroutine: the listen loop's own heartbeat is
	// starved while a `codex queue` call is in flight (up to
	// codexQueueTimeout), and an attached `listen` reads it to tell a live
	// bridge from a dead one.
	go func() {
		t := time.NewTicker(listenHeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				touchListenerHeartbeat(nick)
			}
		}
	}()

	fmt.Fprintf(os.Stderr, "codex-bridge: forwarding messages for @%s into codex thread %s\n", nick, thread)
	sessionGone := func() { b.stop(exitSessionGone) }
	go watchCodexThread(ctx, sessionGone, func() { b.seenHeld.Store(true) }, lockPath, bridgeProbeIntervalFromEnv(), bridgeDeadProbes, os.Stderr)
	w := &bridgeWriter{
		cancel: sessionGone,
		deliver: func(msg string) error {
			// Deliberately not the loop context: the farewell is delivered
			// after cancel, and a takeover signal must not abort a delivery
			// already in flight. Bounded by its own timeout instead.
			dctx, dcancel := context.WithTimeout(context.Background(), codexQueueTimeout)
			defer dcancel()
			return codexQueue(dctx, thread, msg)
		},
	}
	opts := codexListenOpts()
	opts.farewell = b.farewell
	return listenLoopOpts(ctx, nick, w, opts)
}

// bridgeWriter adapts the listen loop's line stream into `codex queue`
// deliveries. A delivery that fails is reported back to the caller rather than
// swallowed, so drainListen holds the cursor and retries the message instead
// of losing it.
type bridgeWriter struct {
	buf     []byte
	deliver func(string) error
	cancel  func()

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
		// framing, and outside the retry bookkeeping — it is the last thing
		// this bridge says, and a backoff left over from an earlier failure
		// must not swallow it.
		if strings.HasPrefix(line, "[agent-chat]") {
			if err := w.deliver(line); err != nil {
				fmt.Fprintf(os.Stderr, "codex-bridge: could not deliver notice: %v\n", err)
			}
			continue
		}
		msg := "New agent-chat message:\n" + renderBridgeLine(line)
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
// stop once the lock — previously seen held — has been free or missing for
// deadProbes consecutive probes; held (optional) is called on every probe
// that finds it held. Returns when ctx ends.
func watchCodexThread(ctx context.Context, stop, held func(), lockPath string, interval time.Duration, deadProbes int, log *os.File) {
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
			if held != nil {
				held()
			}
		case lockFree, lockMissing:
			if !seenHeld {
				continue
			}
			dead++
			if dead >= deadProbes {
				fmt.Fprintf(log, "codex-bridge: codex thread writer lock %s no longer held — session gone; exiting\n", lockPath)
				stop()
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

// removeBridgePidIfOurs removes the pidfile only while it still names this
// process, so a successor's pidfile is never taken down by a slow incumbent.
func removeBridgePidIfOurs(nick string) {
	p := bridgePidPath(nick)
	if readPidFile(p) == os.Getpid() {
		os.Remove(p)
	}
}

// stopCodexBridge stops the recorded bridge for nick, if any — quietly, since
// the session is ending (see terminateBridge). The pidfile is removed
// unconditionally; the signal is only sent when the pid still looks like one
// of our own bridges (recycled-pid guard, same approach as the listener lock
// takeover). No-op when no bridge was ever started — i.e. on every Claude/kilo
// session.
func stopCodexBridge(nick string) {
	p := bridgePidPath(nick)
	pid := readPidFile(p)
	if pid == 0 {
		return
	}
	os.Remove(p)
	if pid <= 0 || pid == os.Getpid() {
		return
	}
	terminateBridge(pid)
}
