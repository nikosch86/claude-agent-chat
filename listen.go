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
const listenFarewell = `[agent-chat] inbox listener stopped: superseded by a newer listener or the session ended (expected after /clear or reconnect). This is not an error and needs no investigation — there is no state file to inspect. If this session is still active and has no other agent-chat inbox Monitor, start one with Monitor(command="agent-chat listen", persistent: true); otherwise ignore this.`

func runListen(args []string) int {
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	as := fs.String("as", "", "your nick (overrides the resolver)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	nick, err := resolveNick(*as)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		return 2
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return listenLoop(ctx, nick, os.Stdout)
}

func listenLoop(ctx context.Context, nick string, out io.Writer) int {
	me := "@" + nick
	cursor, ok := readCursor(nick)
	if !ok {
		// No prior join — start at the current EOF so a manual `listen`
		// run does not spam the entire historical log.
		cursor = currentLogSize()
		_ = writeCursor(nick, cursor)
	}

	touchListenerHeartbeat(nick)
	cursor = drainListen(cursor, me, nick, out)

	pollTicker := time.NewTicker(listenPollInterval)
	defer pollTicker.Stop()
	hbTicker := time.NewTicker(listenHeartbeatInterval)
	defer hbTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, listenFarewell)
			return 0
		case <-hbTicker.C:
			touchListenerHeartbeat(nick)
		case <-pollTicker.C:
			cursor = drainListen(cursor, me, nick, out)
		}
	}
}

// drainListen reads from `cursor` to EOF, emitting matching lines (direct to
// @nick or broadcast "*") as raw JSON to `out`. The persistent cursor is
// advanced past every line scanned — matching or not — so non-matching
// traffic isn't rescanned on the next poll, and a crash loses at most one
// in-flight emit (the one that was being written when we died).
func drainListen(cursor int64, me, nick string, out io.Writer) int64 {
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
		cursor += int64(len(line)) + 1
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if r.To != me && r.To != "*" {
			continue
		}
		out.Write(append(line, '\n'))
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
