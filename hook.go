package main

import (
	"bufio"
	"encoding/json"
	"flag"
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
	sessionID := readHookEnvelope()

	// --emit selects the output framing. "claude" (default) prints the
	// SessionStart hook envelope Claude Code consumes; "text" prints the bare
	// primer to stdout for harnesses (e.g. the kilo plugin) that inject it
	// themselves. The side effects — nick claim, join record, missed scan — are
	// identical for both.
	fs := flag.NewFlagSet("hook-start", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	emit := fs.String("emit", "claude", "output format: claude (hook envelope) | text (plain primer)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	mode := *emit

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
			// Plugin modes signal "claimed by an active peer — did NOT join" with
			// a distinct exit code so the consumer knows not to start a listener
			// under this nick (which would hijack the live owner's inbox). They
			// key off the code, not stdout, so emit nothing.
			if mode == "text" || mode == "json" {
				return 3
			}
			return emitHookOutput(sessionStartEv, buildNotJoinedPrimer(nick))
		}
		if err := appendRecord(Record{Ts: nowEpochMs(), From: nick, Event: "quit"}); err != nil {
			fmt.Fprintf(os.Stderr, "hook-start: %v\n", err)
			return 1
		}
		clearByCwdForNick(nick)
		clearAgentDir(nick)
	}

	if err := writeByCwdNick(nick, sessionID); err != nil {
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
	switch mode {
	case "text":
		// Human/debug view: the passive kilo primer only.
		return emitPrimer(mode, sessionStartEv, buildJoinPrimerKilo(nick, peers))
	case "json":
		// kilo plugin view: passive primer for system context + the capped
		// missed mentions for the plugin to inject as an actionable catch-up
		// turn (mirrors how Claude surfaces missed mentions at session start).
		shown, hint := missedSection(missed)
		return emitKiloJSON(buildJoinPrimerKilo(nick, peers), shown, hint)
	default:
		return emitPrimer(mode, sessionStartEv, buildJoinPrimer(nick, peers, missed))
	}
}

// kiloHookOutput is the --emit json payload consumed by the kilo plugin.
type kiloHookOutput struct {
	Primer   string   `json:"primer"`
	Missed   []string `json:"missed,omitempty"`
	MoreHint string   `json:"moreHint,omitempty"`
}

func emitKiloJSON(primer string, missed []string, moreHint string) int {
	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(kiloHookOutput{Primer: primer, Missed: missed, MoreHint: moreHint}); err != nil {
		fmt.Fprintf(os.Stderr, "hook-start: %v\n", err)
		return 1
	}
	return 0
}

// emitPrimer renders the join primer in the framing selected by --emit:
// "text" prints it raw to stdout (the consumer injects it), anything else
// wraps it in the Claude Code SessionStart hook envelope.
func emitPrimer(mode, eventName, primer string) int {
	if mode == "text" {
		fmt.Println(primer)
		return 0
	}
	return emitHookOutput(eventName, primer)
}

// runHookStop releases the ending session's nick claim(s). Claims are matched
// by owner stamp, not by cwd: a SessionEnd can fire from a directory whose
// claim belongs to a *different live* session — a second session sharing the
// repo, or this session having been relocated into the main checkout after
// its worktree was removed — and tearing that claim down strips the survivor
// of its identity (its next send would resolve to somebody else's nick).
// Matching by owner both spares foreign claims and finds our own claim even
// when it is keyed under a directory we are no longer in.
func runHookStop(args []string) int {
	sessionID := readHookEnvelope()

	claims := claimsOwnedBy(sessionID)
	if len(claims) == 0 {
		// No owner-stamped claim of ours anywhere: fall back to the cwd's
		// claim, but only when it is unstamped (written by a plugin bridge
		// such as kilo, or by an older binary). A claim stamped by a
		// different session is not ours to release.
		nick, owner, ok := readClaimFile(byCwdPath())
		if !ok || owner != "" {
			return 0
		}
		claims = []claim{{path: byCwdPath(), nick: nick}}
	}

	quit := map[string]bool{}
	for _, c := range claims {
		if !quit[c.nick] {
			quit[c.nick] = true
			if err := appendRecord(Record{Ts: nowEpochMs(), From: c.nick, Event: "quit"}); err != nil {
				fmt.Fprintf(os.Stderr, "hook-stop: %v\n", err)
				return 1
			}
			if err := writeCursor(c.nick, currentLogSize()); err != nil {
				fmt.Fprintf(os.Stderr, "hook-stop: %v\n", err)
				return 1
			}
		}
		os.Remove(c.path)
	}
	return 0
}

func deriveHookNick(cwd string) (string, string) {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_AGENT_CHAT_NICK")); v != "" {
		return v, "CLAUDE_AGENT_CHAT_NICK"
	}
	return deriveDirNick(cwd)
}

// deriveDirNick derives a nick from the directory alone: the git top-level's
// basename when inside a repo, else the first non-empty line of a
// .agent-chat-nick file in cwd. Shared by the join hook and the runtime
// resolver, so a session whose by-cwd claim vanished re-derives exactly the
// nick it joined under. Returns the raw (unsanitized) nick and its source.
func deriveDirNick(cwd string) (string, string) {
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

// writeByCwdNick claims this directory's key for nick, stamping the owning
// session id (when known) so hook-stop can prove ownership before tearing the
// claim down. See parseClaim for the file format.
func writeByCwdNick(nick, sessionID string) error {
	p := byCwdPath()
	if p == "" {
		return fmt.Errorf("could not determine by-cwd key (no git root and no cwd)")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	content := nick + "\n"
	if sessionID != "" {
		content += sessionID + "\n"
	}
	return os.WriteFile(p, []byte(content), 0o644)
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
			// Shape as listen does: these lines are injected verbatim into
			// SessionStart context, where nothing would clip an oversized body,
			// so it has to be bounded here.
			missed = append(missed, string(notifyLine([]byte(line), r)))
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

	b.WriteString("**REQUIRED FIRST ACTION** — before doing anything else this turn, start the inbox monitor so peer messages reach you reactively. Monitor is usually a deferred tool whose schema is not loaded at session start, so this takes two calls:\n")
	b.WriteString("  ToolSearch(\"select:Monitor\")   # load the deferred tool's schema first (skip only if Monitor is already callable)\n")
	b.WriteString("  Monitor(command=\"agent-chat listen\", persistent: true, description: \"agent-chat inbox\")\n")
	b.WriteString("Do not narrate or explain this setup — but if you write any reply this turn (even a bare \"standing by\"), state in it that the listener is running, so a skipped start is visible immediately instead of surfacing later as an unanswered peer. Do this on every session start. It is a one-shot setup; the Monitor persists for the session. Starting it is always safe — a fresh listener cleanly takes over any earlier one for your nick, so you never end up with duplicate notifications or a silent inbox.\n")
	b.WriteString("A \"Monitor ... stream ended\" notice for a prior agent-chat listener around now is the expected takeover, not a fault. Do not investigate, acknowledge, or explain it — write nothing about it and carry on.\n\n")

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
			// Anchor recovery on the oldest missed line's time, not --tail N:
			// the agent has just started a listener, so new matching traffic
			// can land before it runs this — and that would push the oldest
			// (un-inlined) mentions out of a tail-N view, silently losing the
			// very messages this points at. --since is exact and complete.
			if anchor := missedSinceAnchor(missed[0]); anchor != "" {
				fmt.Fprintf(&b, " — latest %d below; run `agent-chat history --to me --since %s` for the rest", missedPreviewMax, anchor)
			} else {
				fmt.Fprintf(&b, " — latest %d below; run `agent-chat history --to me --tail %d` for the rest", missedPreviewMax, len(missed))
			}
		}
		b.WriteString(":\n")
		for _, line := range shown {
			fmt.Fprintf(&b, "  %s\n", line)
		}
		b.WriteByte('\n')
	}

	b.WriteString("Commands:\n")
	b.WriteString("  agent-chat send @peer '...'             # plain reply (single-quote the body)\n")
	b.WriteString("  agent-chat share @peer --file PATH      # share a file (auto-copied to artifacts)\n")
	b.WriteString("  agent-chat peers                        # who's around\n")
	b.WriteString("  agent-chat history --to me              # catch-up only (listen already streams new msgs); narrow with --from @peer --tail N --format text to save context\n")
	b.WriteString("  agent-chat --help                       # everything else\n\n")
	b.WriteString("Rules:\n")
	fmt.Fprintf(&b, "  - You are the authority on this repo (`%s`). Peers ask you about it.\n", nick)
	b.WriteString("  - Do NOT read peer repos directly. If a peer's content matters, ask them or wait for them to `share` it. Any `path` you receive will live under ~/.agent-chat/artifacts/.\n")
	b.WriteString("  - `send` has NO size limit — write the message the length it needs to be, and never shorten and resend one you already sent (it was delivered whole the first time; resending only duplicates it). A message too long for one inbox notification arrives as a `\"clipped\":true` notice carrying a preview plus a `full` command — run that command to read the body in one piece. Use `share @peer --file PATH` for files, not to dodge a size limit.\n")
	b.WriteString("  - When reading the log, narrow it (`history --from @peer --tail N --format text`) rather than replaying your whole inbox.\n")
	b.WriteString("  - Single-quote message bodies: `agent-chat send @peer 'text'`. A double-quoted body lets YOUR shell expand backticks and $(...) in it before agent-chat runs — which can silently execute a local command and drop the message with no error. Single quotes (or a heredoc) keep the body literal.\n")
	b.WriteString("  - Questions are async: send and continue working. When a reply lands as a listen notification, respond then. If a peer doesn't answer for a long time, escalate by addressing @hoffmann.\n")
	return b.String()
}

// buildJoinPrimerKilo renders the join context for the kilo plugin, which
// injects it as a SYSTEM message rather than a user turn. The framing is
// deliberately passive: a small/eager model that receives an imperative
// user-role primer will act on it unprompted (read the repo, message peers on a
// bare "hi"). This version states plainly that it is ambient context and that
// the agent must take no action until a real incoming message arrives or the
// user asks.
func buildJoinPrimerKilo(nick string, peers []string) string {
	var b strings.Builder
	b.WriteString("## Agent Chat — ambient context, NOT a task\n\n")
	b.WriteString("You are connected to a shared chat between agents as `" + nick + "`. This is background information only. Do NOT act on it: do not read files, do not contact peers, do not reply to this notice. Just do what the user asks.\n\n")
	b.WriteString("Messages addressed to you are delivered into this session automatically as they arrive, prefixed \"New agent-chat message\". ONLY when such a message arrives — or when the user explicitly asks you to — use:\n")
	b.WriteString("  agent-chat send @peer 'text'            # reply (single-quote the body; any length)\n")
	b.WriteString("  agent-chat share @peer --file PATH      # share a file\n")
	b.WriteString("  agent-chat peers                        # who's around\n")
	b.WriteString("  agent-chat history --to me              # catch up on earlier messages\n")
	b.WriteString("  agent-chat history --id TS --format text  # read a message that arrived clipped\n\n")

	peerList := "(none)"
	if filtered := filterOut(peers, nick); len(filtered) > 0 {
		peerList = strings.Join(filtered, ", ")
	}
	fmt.Fprintf(&b, "Active peers: %s.\n", peerList)

	b.WriteString("\nNotes:\n")
	fmt.Fprintf(&b, "  - You are the authority on this repo (`%s`); peers may ask you about it.\n", nick)
	b.WriteString("  - Single-quote message bodies so your shell does not expand $(...) or backticks.\n")
	b.WriteString("  - Do not read peer repos directly; ask a peer to `share` a file instead. Shared paths live under ~/.agent-chat/artifacts/.\n")
	return b.String()
}

// missedSection caps the missed mentions to the latest missedPreviewMax and,
// when there are more, returns a `history` command covering the remainder —
// the same cap the Claude join primer applies, reused for the kilo --emit json
// catch-up so a long absence never dumps an unbounded backlog into one turn.
func missedSection(missed []string) (shown []string, hint string) {
	if len(missed) <= missedPreviewMax {
		return missed, ""
	}
	shown = missed[len(missed)-missedPreviewMax:]
	if anchor := missedSinceAnchor(missed[0]); anchor != "" {
		hint = fmt.Sprintf("agent-chat history --to me --since %s", anchor)
	} else {
		hint = fmt.Sprintf("agent-chat history --to me --tail %d", len(missed))
	}
	return shown, hint
}

// missedSinceAnchor returns an RFC3339 timestamp one second before the oldest
// missed line, for use as `history --since`. The one-second slack keeps the
// `>=` comparison in history inclusive despite sub-second rounding. Returns ""
// if the line can't be parsed (it always can — readMissedSince only keeps lines
// that already unmarshalled — but the caller falls back to --tail if not).
func missedSinceAnchor(oldest string) string {
	var r Record
	if err := json.Unmarshal([]byte(oldest), &r); err != nil {
		return ""
	}
	return time.UnixMilli(int64(float64(r.Ts) * 1000)).Add(-time.Second).Format(time.RFC3339)
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

// hookEnvelope is the JSON Claude Code pipes to hook commands on stdin. Only
// session_id is consumed: it stamps the by-cwd claim so hook-stop can prove
// ownership before releasing it.
type hookEnvelope struct {
	SessionID string `json:"session_id"`
}

// readHookEnvelope consumes piped stdin — fully, so the parent never sees
// EPIPE — and returns the envelope's session id. Returns "" when stdin is a
// tty or carries no parseable envelope (plugin bridges such as kilo invoke
// the hooks bare). If stdin is a tty, nothing is read — it would block.
func readHookEnvelope() string {
	piped, err := stdinIsPipe()
	if err != nil || !piped {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	io.Copy(io.Discard, os.Stdin)
	if err != nil {
		return ""
	}
	var env hookEnvelope
	if json.Unmarshal(b, &env) != nil {
		return ""
	}
	return strings.TrimSpace(env.SessionID)
}
