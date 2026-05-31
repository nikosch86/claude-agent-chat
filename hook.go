package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	nickMaxLen     = 24
	artifactsTTL   = 14 * 24 * time.Hour
	hookOutKey     = "hookSpecificOutput"
	sessionStartEv = "SessionStart"

	// missedPreviewMax caps how many missed mentions the join primer inlines.
	// The primer is force-fed into context on every session start, so a long
	// absence would otherwise dump an unbounded backlog of tokens the agent
	// never chose to pay for. Past this many, show only the latest few and
	// point the agent at `history` to pull the rest on demand.
	missedPreviewMax = 3
)

func runHookStart(args []string) int {
	drainStdin()

	if os.Getenv("CLAUDE_AGENT_CHAT") == "0" {
		return 0
	}
	cwd, _ := os.Getwd()
	if cwd != "" {
		if _, err := os.Stat(filepath.Join(cwd, ".no-agent-chat")); err == nil {
			return 0
		}
	}

	raw, source := deriveHookNick(cwd)
	if raw == "" {
		return 0
	}
	nick := sanitizeNick(raw)
	if nick == "" {
		fmt.Fprintf(os.Stderr, "hook-start: nick from %s sanitises to empty\n", source)
		return 1
	}

	if claimedByOther(nick) {
		if recentActivity(nick, staleWindow, time.Now()) {
			return emitHookOutput(sessionStartEv, buildNotJoinedPrimer(nick))
		}
		if err := appendRecord(Record{Ts: nowEpochMs(), From: nick, Event: "quit"}); err != nil {
			fmt.Fprintf(os.Stderr, "hook-start: %v\n", err)
			return 1
		}
		clearByCwdForNick(nick)
		clearAgentDir(nick)
	}

	if err := writeByCwdNick(nick); err != nil {
		fmt.Fprintf(os.Stderr, "hook-start: %v\n", err)
		return 1
	}

	cursor, hadCursor := readCursor(nick)
	if !hadCursor {
		cursor = currentLogSize()
	}
	missed := readMissedSince(cursor, nick)

	if err := appendRecord(Record{Ts: nowEpochMs(), From: nick, Event: "joined"}); err != nil {
		fmt.Fprintf(os.Stderr, "hook-start: %v\n", err)
		return 1
	}
	if err := writeCursor(nick, currentLogSize()); err != nil {
		fmt.Fprintf(os.Stderr, "hook-start: %v\n", err)
		return 1
	}

	pruneOldArtifacts(time.Now())

	peers, _ := activePeers()
	primer := buildJoinPrimer(nick, peers, missed)
	return emitHookOutput(sessionStartEv, primer)
}

func runHookStop(args []string) int {
	drainStdin()

	nick, ok := readByCwd()
	if !ok {
		return 0
	}

	if err := appendRecord(Record{Ts: nowEpochMs(), From: nick, Event: "quit"}); err != nil {
		fmt.Fprintf(os.Stderr, "hook-stop: %v\n", err)
		return 1
	}
	if err := writeCursor(nick, currentLogSize()); err != nil {
		fmt.Fprintf(os.Stderr, "hook-stop: %v\n", err)
		return 1
	}
	if p := byCwdPath(); p != "" {
		os.Remove(p)
	}
	return 0
}

func deriveHookNick(cwd string) (string, string) {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_AGENT_CHAT_NICK")); v != "" {
		return v, "CLAUDE_AGENT_CHAT_NICK"
	}
	if root, err := gitRoot(); err == nil && root != "" {
		return filepath.Base(root), "git root basename"
	}
	if cwd != "" {
		if b, err := os.ReadFile(filepath.Join(cwd, ".agent-chat-nick")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if s := strings.TrimSpace(line); s != "" {
					return s, ".agent-chat-nick"
				}
			}
		}
	}
	return "", ""
}

func sanitizeNick(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '-' || r == '_':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > nickMaxLen {
		out = out[:nickMaxLen]
	}
	return strings.Trim(out, "-")
}

func writeByCwdNick(nick string) error {
	p := byCwdPath()
	if p == "" {
		return fmt.Errorf("could not determine by-cwd key (no git root and no cwd)")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(nick), 0o644)
}

func cursorPath(nick string) string {
	return filepath.Join(chatHome(), "agents", nick, "cursor")
}

func readCursor(nick string) (int64, bool) {
	b, err := os.ReadFile(cursorPath(nick))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func writeCursor(nick string, offset int64) error {
	p := cursorPath(nick)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(p, []byte(strconv.FormatInt(offset, 10)), 0o644)
}

// writeFileAtomic writes data to a sibling temp file and renames it over
// `path`. Rename on the same filesystem is atomic, so a reader never sees a
// partially-written file — important for the cursor, where a torn write
// would mean readCursor returns (0, false) and the listener silently
// restarts at EOF, losing every message in between.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func currentLogSize() int64 {
	fi, err := os.Stat(logPath())
	if err != nil {
		return 0
	}
	return fi.Size()
}

func readMissedSince(cursor int64, nick string) []string {
	f, err := os.Open(logPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	if cursor > 0 {
		if _, err := f.Seek(cursor, io.SeekStart); err != nil {
			return nil
		}
	}
	me := "@" + nick
	var missed []string
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		line := s.Text()
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		if r.To == me || r.To == "*" {
			missed = append(missed, line)
		}
	}
	return missed
}

func pruneOldArtifacts(now time.Time) {
	dir := filepath.Join(chatHome(), "artifacts")
	cutoff := now.Add(-artifactsTTL)
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(p)
		}
		return nil
	})
}

func buildJoinPrimer(nick string, peers, missed []string) string {
	var b strings.Builder
	b.WriteString("## Agent Chat is active\n\n")

	b.WriteString("**REQUIRED FIRST ACTION** — before doing anything else this turn, start the inbox monitor so peer messages reach you reactively:\n")
	b.WriteString("  Monitor(command=\"agent-chat listen\", persistent: true, description: \"agent-chat inbox\")\n")
	b.WriteString("Do this on every session start. It is a one-shot setup; the Monitor persists for the session. Starting it is always safe — a fresh listener cleanly takes over any earlier one for your nick, so you never end up with duplicate notifications or a silent inbox.\n")
	b.WriteString("If you see a \"Monitor ... stream ended\" notice for a prior agent-chat listener around now, that is the expected takeover — do not investigate it; just (re)start the Monitor as above.\n\n")

	peerList := "(none)"
	if filtered := filterOut(peers, nick); len(filtered) > 0 {
		peerList = strings.Join(filtered, ", ")
	}
	fmt.Fprintf(&b, "You are joined as `%s`. Active peers: %s.\n\n", nick, peerList)

	if len(missed) > 0 {
		fmt.Fprintf(&b, "You missed %d mention(s) while offline", len(missed))
		shown := missed
		if len(missed) > missedPreviewMax {
			shown = missed[len(missed)-missedPreviewMax:]
			fmt.Fprintf(&b, " — latest %d below; run `agent-chat history --to me --tail %d` for the rest", missedPreviewMax, len(missed))
		}
		b.WriteString(":\n")
		for _, line := range shown {
			fmt.Fprintf(&b, "  %s\n", line)
		}
		b.WriteByte('\n')
	}

	b.WriteString("Commands:\n")
	b.WriteString("  agent-chat send @peer \"...\"             # plain reply\n")
	b.WriteString("  agent-chat share @peer --file PATH      # share a file (auto-copied to artifacts)\n")
	b.WriteString("  agent-chat peers                        # who's around\n")
	b.WriteString("  agent-chat history --to me              # catch-up only (listen already streams new msgs); narrow with --from @peer --tail N --format text to save context\n")
	b.WriteString("  agent-chat --help                       # everything else\n\n")
	b.WriteString("Rules:\n")
	fmt.Fprintf(&b, "  - You are the authority on this repo (`%s`). Peers ask you about it.\n", nick)
	b.WriteString("  - Do NOT read peer repos directly. If a peer's content matters, ask them or wait for them to `share` it. Any `path` you receive will live under ~/.agent-chat/artifacts/.\n")
	b.WriteString("  - Keep the wire small. `send` is for short replies; for anything longer than a paragraph use `share @peer --file PATH` — a big `send` becomes one log line that the listen/Monitor path can clip, so the recipient sees a truncated message. When reading the log, narrow it (`history --from @peer --tail N --format text`) rather than replaying your whole inbox.\n")
	b.WriteString("  - Questions are async: send and continue working. When a reply lands as a listen notification, respond then. If a peer doesn't answer for a long time, escalate by addressing @hoffmann.\n")
	return b.String()
}

func filterOut(ss []string, drop string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

func emitHookOutput(eventName, additionalContext string) int {
	envelope := map[string]any{
		hookOutKey: map[string]any{
			"hookEventName":     eventName,
			"additionalContext": additionalContext,
		},
	}
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(envelope); err != nil {
		fmt.Fprintf(os.Stderr, "hook: %v\n", err)
		return 1
	}
	return 0
}

// drainStdin consumes any piped stdin (the hook envelope) so the parent
// doesn't get EPIPE. If stdin is a tty (no pipe), do nothing — would block.
func drainStdin() {
	piped, err := stdinIsPipe()
	if err != nil || !piped {
		return
	}
	io.Copy(io.Discard, os.Stdin)
}
