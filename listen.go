package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Tuning knobs. Vars (not consts) so tests can crank them down.
var (
	// listenPollInterval is how often the loop rechecks the log for new bytes.
	listenPollInterval = 200 * time.Millisecond

	// listenHeartbeatInterval is how often listen touches its heartbeat
	// file so other verbs can tell it is alive.
	listenHeartbeatInterval = 1 * time.Second

	// listenerStaleThreshold is how old the heartbeat may get before the
	// self-heal warning treats the listener as stopped.
	listenerStaleThreshold = 5 * time.Second
)

// listenFarewell is the last line listen emits when it exits on a signal
// (SIGTERM/SIGINT). It is the in-context explanation for the "Monitor stream
// ended" notice the harness shows when our process exits — most often because a
// newer session took the listener over (the expected churn after /clear). The
// previous agent, seeing only a bare "stream ended", wasted a turn hunting for a
// nonexistent state file before restarting; this line tells it the exit is
// normal, needs no investigation, and exactly what (not) to do next.
const listenFarewell = `[agent-chat] inbox listener stopped — superseded by a newer listener or the session ended (expected after /clear or reconnect; not an error and needs no investigation). Say nothing about it. If this session is still active with no other agent-chat Monitor, restart with Monitor(command="agent-chat listen", persistent: true); otherwise ignore.`

func runListen(args []string) int {
	as, args, err := extractAs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		return 2
	}
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprintln(os.Stderr, "usage: agent-chat listen [--as NICK]") }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	nick, err := resolveNick(as)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Inside a Codex session the detached codex-bridge already is this nick's
	// inbox, and it delivers into the session — which a listener started from
	// a tool call cannot. Attach to it instead of competing, and never run a
	// listener from there (see listenInsideCodex).
	if thread := codexThreadFromEnv(); thread != "" {
		return listenInsideCodex(ctx, nick, thread, os.Stdout)
	}

	// Singleton via takeover. At most one live listener per nick may run — a
	// second would duplicate notifications and race on the cursor. Rather than
	// refuse to start when the lock is held (which made a fresh Monitor's
	// listen exit silently and the session go deaf while a listener orphaned
	// from a dead session kept eating this nick's messages), we evict the
	// incumbent and take over. The newest listener is the one wired to the
	// session the user is looking at, so newest-wins is correct; listen always
	// runs and never goes silent. See tryLockListener for the mechanism.
	defer tryLockListener(nick)()

	return listenLoop(ctx, nick, os.Stdout)
}

// codexThreadFromEnv reports the Codex thread id when this process runs inside
// a Codex session's tool call — Codex exports CODEX_THREAD_ID to the commands
// it executes — and "" everywhere else.
func codexThreadFromEnv() string {
	return strings.TrimSpace(os.Getenv("CODEX_THREAD_ID"))
}

// bridgeStaleThreshold is how old the bridge's heartbeat may get before
// bridgeServing treats the bridge as dead. The bridge touches it every second
// from a dedicated goroutine, so anything past a few seconds means the
// process is gone or wedged; the margin covers a suspend/resume or a busy
// host without tripping.
var bridgeStaleThreshold = 15 * time.Second

// bridgeAttachMisses is how many consecutive one-second checks must find no
// serving bridge before an attached listen concludes the bridge is gone —
// long enough to ride out a same-thread handover, during which the pidfile
// and the lock holder briefly disagree.
var bridgeAttachMisses = 10

// bridgeServing reports whether a codex-bridge is currently serving nick,
// judged from the filesystem alone: its pidfile names the process that holds
// the listener lock (a Claude listener taking the nick overwrites the lock
// holder with its own pid, so a stale pidfile cannot pass), and the heartbeat
// is fresh. A process check is useless where this matters — Codex's sandbox
// runs each command in its own PID namespace, where the bridge and every
// other host process are invisible, which is exactly what led agents to
// conclude "no listener running" and start a competing one.
func bridgeServing(nick string, now time.Time) bool {
	pid := readPidFile(bridgePidPath(nick))
	if pid <= 0 || readLockHolder(listenerLockPath(nick)) != pid {
		return false
	}
	fi, err := os.Stat(heartbeatPath(nick))
	if err != nil {
		return false
	}
	return now.Sub(fi.ModTime()) < bridgeStaleThreshold
}

const listenAttachNotice = `[agent-chat] attached: this Codex session's inbox is its codex-bridge, which delivers peer messages automatically as "New agent-chat message" turns. No listener is needed, so this command stays attached and idle instead of competing with the bridge for the inbox. Read the inbox by hand with: agent-chat history --to me --tail 20 --format text`

const listenNoBridgeNotice = `[agent-chat] no codex-bridge is serving this Codex session, and a listener started from inside it cannot deliver into the session (and could not be evicted later, splitting the inbox with the next bridge), so nothing was started. Restart the bridge from outside the sandbox — a host shell, or a command run with escalated permissions: agent-chat codex-bridge --thread %s --as %s. Until then read the inbox by hand: agent-chat history --to me --tail 20 --format text`

// listenInsideCodex is `listen` run from a Codex session's tool call. With a
// bridge serving nick it announces the attachment and idles until stopped,
// touching neither the lock nor the cursor (exit 0). Without one — from the
// start, or after bridgeAttachMisses consecutive misses — it prints the
// restart command and exits 1 rather than running a listener: a listener here
// cannot deliver into the session, and inside the sandbox it could not even be
// evicted later (its pid is namespace-local), so it would split the inbox
// with the next bridge for good.
func listenInsideCodex(ctx context.Context, nick, thread string, out io.Writer) int {
	if !bridgeServing(nick, time.Now()) {
		fmt.Fprintf(out, listenNoBridgeNotice+"\n", thread, nick)
		return 1
	}
	fmt.Fprintln(out, listenAttachNotice)
	t := time.NewTicker(listenHeartbeatInterval)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-t.C:
			if bridgeServing(nick, time.Now()) {
				misses = 0
				continue
			}
			if misses++; misses >= bridgeAttachMisses {
				fmt.Fprintf(out, "[agent-chat] the codex-bridge stopped. "+listenNoBridgeNotice[len("[agent-chat] "):]+"\n", thread, nick)
				return 1
			}
		}
	}
}

// readPidFile returns the pid stamped in a one-line file (a pidfile, or the
// listener lock file), or 0 when the file is missing or malformed.
func readPidFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

// readLockHolder is the pid the current listener lock holder stamped into
// the lock file (see recordLockHolder), or 0.
func readLockHolder(path string) int { return readPidFile(path) }

// listenOpts carries the parts of the loop that differ per consumer: how a
// matching log line is framed for delivery, and what is said on the way out.
// Claude reads events through Monitor (small cap, Monitor-specific farewell);
// the codex bridge queues them into a session (large cap, its own farewell).
type listenOpts struct {
	format func([]byte, Record) []byte
	// farewell is consulted once, on exit; "" (or nil) leaves quietly. The
	// codex bridge decides per exit whether the session must be told.
	farewell func() string
}

// claudeListenOpts is the Monitor framing — the historical behaviour, and what
// bare listenLoop still does.
func claudeListenOpts() listenOpts {
	return listenOpts{format: notifyLine, farewell: func() string { return listenFarewell }}
}

func listenLoop(ctx context.Context, nick string, out io.Writer) int {
	return listenLoopOpts(ctx, nick, out, claudeListenOpts())
}

func listenLoopOpts(ctx context.Context, nick string, out io.Writer, opts listenOpts) int {
	me := "@" + nick
	cursor, ok := readCursor(nick)
	if !ok {
		// No prior join — start at the current EOF so a manual `listen`
		// run does not spam the entire historical log.
		cursor = currentLogSize()
		_ = writeCursor(nick, cursor)
	}

	touchListenerHeartbeat(nick)
	cursor = drainListen(cursor, me, nick, out, opts.format)

	pollTicker := time.NewTicker(listenPollInterval)
	defer pollTicker.Stop()
	hbTicker := time.NewTicker(listenHeartbeatInterval)
	defer hbTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			if opts.farewell != nil {
				if msg := opts.farewell(); msg != "" {
					fmt.Fprintln(out, msg)
				}
			}
			return 0
		case <-hbTicker.C:
			touchListenerHeartbeat(nick)
		case <-pollTicker.C:
			cursor = drainListen(cursor, me, nick, out, opts.format)
		}
	}
}

// drainListen reads from `cursor` to EOF, emitting matching lines (direct to
// @nick or broadcast "*") as raw JSON to `out`, framed by `format`. The
// persistent cursor is advanced past every line scanned — matching or not —
// so non-matching traffic isn't rescanned on the next poll, and a crash loses
// at most one in-flight emit (the one that was being written when we died).
//
// A write that reports an error stops the drain with the cursor still behind
// the offending record, so the next poll retries it rather than losing it.
// os.Stdout never errors in practice, so this is inert for the Claude path;
// it is what keeps the codex bridge from dropping a message whose `codex
// queue` call failed.
func drainListen(cursor int64, me, nick string, out io.Writer, format func([]byte, Record) []byte) int64 {
	f, err := os.Open(logPath())
	if err != nil {
		return cursor
	}
	defer f.Close()

	// Truncation/rotation guard: if our offset is past EOF, restart from 0.
	if fi, err := f.Stat(); err == nil && cursor > fi.Size() {
		cursor = 0
	}
	if cursor > 0 {
		if _, err := f.Seek(cursor, io.SeekStart); err != nil {
			return cursor
		}
	}

	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	startCursor := cursor
	for s.Scan() {
		line := append([]byte(nil), s.Bytes()...)
		next := cursor + int64(len(line)) + 1
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			cursor = next
			continue
		}
		if r.To != me && r.To != "*" {
			cursor = next
			continue
		}
		if _, err := out.Write(append(format(line, r), '\n')); err != nil {
			// Undelivered: leave the cursor before this record and stop, so
			// this line and everything after it are re-read next poll.
			break
		}
		cursor = next
		_ = writeCursor(nick, cursor)
	}
	// Persist once at the end so non-matching traffic isn't rescanned next
	// poll, but only if we actually advanced — avoids a disk write on every
	// idle tick.
	if cursor != startCursor {
		_ = writeCursor(nick, cursor)
	}
	return cursor
}

func heartbeatPath(nick string) string {
	return filepath.Join(chatHome(), "agents", nick, "listener-heartbeat")
}

// listenerLockPath is the per-nick singleton lock file, co-located with the
// heartbeat. The flock(2) is the lock; the holder also stamps its pid into
// the file (see recordLockHolder) so a taking-over listener can signal it.
func listenerLockPath(nick string) string {
	return filepath.Join(chatHome(), "agents", nick, "listener.lock")
}

func touchListenerHeartbeat(nick string) {
	p := heartbeatPath(nick)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	f.Close()
	now := time.Now()
	_ = os.Chtimes(p, now, now)
}

func listenerIsAlive(nick string, now time.Time) bool {
	fi, err := os.Stat(heartbeatPath(nick))
	if err != nil {
		return false
	}
	return now.Sub(fi.ModTime()) < listenerStaleThreshold
}

// maybeWarnListener writes the self-heal warning to w when, for `nick`:
//  1. a cursor file exists (i.e. we have ever joined as listener),
//  2. the heartbeat is missing or stale (listener appears stopped), and
//  3. there is matching unread traffic from someone else past the cursor.
//
// Silent otherwise. Called from every verb except `listen` itself and the
// hook/reset plumbing.
func maybeWarnListener(w io.Writer, nick string) {
	if nick == "" {
		return
	}
	cursor, ok := readCursor(nick)
	if !ok {
		return
	}
	if listenerIsAlive(nick, time.Now()) {
		return
	}
	n := countUnreadFromOthers(nick, cursor)
	if n == 0 {
		return
	}
	fmt.Fprintf(w, "[agent-chat] listener appears stopped — you have %d unread; run Monitor(agent-chat listen, persistent: true)\n", n)
}

// countUnreadFromOthers tallies matching log records past `cursor` that were
// authored by someone other than `nick` (self-sends don't count as unread).
func countUnreadFromOthers(nick string, cursor int64) int {
	f, err := os.Open(logPath())
	if err != nil {
		return 0
	}
	defer f.Close()
	if cursor > 0 {
		if _, err := f.Seek(cursor, io.SeekStart); err != nil {
			return 0
		}
	}
	me := "@" + nick
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	n := 0
	for s.Scan() {
		var r Record
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			continue
		}
		if r.From == nick {
			continue
		}
		if r.To == me || r.To == "*" {
			n++
		}
	}
	return n
}

// isOwnAgentChatProc reports whether pid is one of our own agent-chat
// processes running one of the given verbs, judged from its argv (see
// procArgv). Unreadable means false.
func isOwnAgentChatProc(pid int, verbs ...string) bool {
	return isAgentChatArgv(procArgv(pid), verbs)
}

// isAgentChatArgv applies the process heuristic to an argv: some argument
// names the binary and one of the bare verbs appears alongside it.
func isAgentChatArgv(argv, verbs []string) bool {
	var sawBinary, sawVerb bool
	for _, a := range argv {
		if strings.Contains(a, "agent-chat") {
			sawBinary = true
		}
		for _, v := range verbs {
			if a == v {
				sawVerb = true
			}
		}
	}
	return sawBinary && sawVerb
}
