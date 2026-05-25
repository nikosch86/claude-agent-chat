package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// staleWindow is the period of inactivity after which a claimed nick is
// considered abandoned and may be reclaimed by another session. Single
// source of truth for tuning (acceptance criterion: "configurable via
// constant").
var staleWindow = 30 * time.Minute

const resetUsage = `usage: agent-chat reset [--as NICK] [<nick>]`

func runReset(args []string) int {
	var as, nick string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stdout, resetUsage)
			return 0
		case a == "--as":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "reset: --as requires a value")
				return 2
			}
			i++
			as = args[i]
		case strings.HasPrefix(a, "--as="):
			as = a[len("--as="):]
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "reset: unknown flag %q\n", a)
			return 2
		default:
			if nick != "" {
				fmt.Fprintln(os.Stderr, "reset: expected at most one nick argument")
				return 2
			}
			nick = strings.TrimPrefix(a, "@")
		}
	}
	if nick == "" {
		n, err := resolveNick(as)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reset: %v\n", err)
			return 2
		}
		nick = n
	}
	if len(byCwdEntriesForNick(nick)) == 0 {
		return 0
	}
	if err := appendRecord(Record{Ts: nowEpochMs(), From: nick, Event: "quit"}); err != nil {
		fmt.Fprintf(os.Stderr, "reset: %v\n", err)
		return 1
	}
	clearByCwdForNick(nick)
	clearAgentDir(nick)
	return 0
}

// byCwdEntriesForNick returns the absolute paths of every by-cwd/*.nick
// file whose content equals nick. Used to detect collisions (any entry
// other than my own cwd's) and to clean them up on stale recovery / reset.
func byCwdEntriesForNick(nick string) []string {
	dir := filepath.Join(chatHome(), "by-cwd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var matches []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".nick") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(b)) == nick {
			matches = append(matches, p)
		}
	}
	return matches
}

// claimedByOther reports whether nick has an active by-cwd claim from a
// cwd other than this process's cwd.
func claimedByOther(nick string) bool {
	matches := byCwdEntriesForNick(nick)
	if len(matches) == 0 {
		return false
	}
	mine := byCwdPath()
	for _, m := range matches {
		if m != mine {
			return true
		}
	}
	return false
}

// recentActivity reports whether the log contains any record from `nick`
// with a timestamp at or after (now - window).
func recentActivity(nick string, window time.Duration, now time.Time) bool {
	f, err := os.Open(logPath())
	if err != nil {
		return false
	}
	defer f.Close()
	cutoffMs := now.Add(-window).UnixMilli()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		var r Record
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			continue
		}
		if r.From != nick {
			continue
		}
		if int64(float64(r.Ts)*1000) >= cutoffMs {
			return true
		}
	}
	return false
}

func clearByCwdForNick(nick string) {
	for _, p := range byCwdEntriesForNick(nick) {
		os.Remove(p)
	}
}

func clearAgentDir(nick string) {
	os.RemoveAll(filepath.Join(chatHome(), "agents", nick))
}

func buildNotJoinedPrimer(nick string) string {
	return fmt.Sprintf("## Agent Chat: NOT JOINED\n\n"+
		"Nick `%s` is already in use by another active session. To join with\n"+
		"a distinct identity, restart this session with\n"+
		"CLAUDE_AGENT_CHAT_NICK=%s-2 (or any other unused nick). To proceed\n"+
		"without chat, ignore this notice.\n", nick, nick)
}
