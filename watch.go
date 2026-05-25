package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// watchPollInterval is how often watch rechecks the log for new bytes.
// Kept as a var so tests can shrink it; deliberately distinct from
// listenPollInterval so neither tunes the other under parallel tests.
var watchPollInterval = 200 * time.Millisecond

// nickPalette is the per-nick color pool: bright fg followed by base fg,
// avoiding black/white. 12 entries — within the 8–12 distinct range the
// design brief calls for, and well clear of ANSI control codes that some
// terminals reserve.
var nickPalette = []int{91, 92, 93, 94, 95, 96, 31, 32, 33, 34, 35, 36}

const (
	ansiReset  = "\x1b[0m"
	ansiDimIt  = "\x1b[2;3m"
	dateFormat = "2006-01-02 15:04:05"
	timeFormat = "15:04:05"
)

func runWatch(args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	as := fs.String("as", "", "your nick (overrides the resolver)")
	filterFlag := fs.String("filter", "", "narrow to traffic mentioning this nick (@nick or nick)")
	tail := fs.Int("tail", 30, "initial backlog count")
	noColor := fs.Bool("no-color", false, "disable ANSI color")
	withDate := fs.Bool("date", false, "include the date in timestamps")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	nick, err := resolveNick(*as)
	if err != nil {
		fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		return 2
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	return watchSession(ctx, nick, normalizeFilter(*filterFlag), *tail, !*noColor, *withDate, os.Stdout)
}

// watchSession bookends watchLoop with the lifecycle events the design
// brief requires: a `joined` line on entry (so watchers appear in `peers`)
// and a `quit` on the way out. Both are best-effort — losing a lifecycle
// emit must not prevent the viewer from running.
func watchSession(ctx context.Context, nick, filter string, tail int, color, withDate bool, out io.Writer) int {
	_ = appendRecord(Record{Ts: nowEpochMs(), From: nick, Event: "joined"})
	defer func() {
		_ = appendRecord(Record{Ts: nowEpochMs(), From: nick, Event: "quit"})
	}()
	return watchLoop(ctx, out, filter, tail, color, withDate)
}

func watchLoop(ctx context.Context, out io.Writer, filter string, tail int, color, withDate bool) int {
	bw := bufio.NewWriter(out)
	defer bw.Flush()

	backlog, cursor := readBacklog(tail, filter)
	for _, line := range backlog {
		if r, ok := parseRecord(line); ok {
			fmt.Fprintln(bw, renderWatch(r, color, withDate))
		}
	}
	bw.Flush()

	ticker := time.NewTicker(watchPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
			cursor = drainWatch(cursor, filter, color, withDate, bw)
			bw.Flush()
		}
	}
}

// readBacklog scans the whole log, keeps lines matching `filter`, returns
// the last `tail` of them plus the byte offset of EOF as observed at the end
// of the scan. Tracking the offset by accumulated line lengths (rather than
// a trailing os.Stat) means we can't drop or duplicate a line that arrives
// while we're scanning — the follow loop picks up from exactly where the
// scanner stopped.
func readBacklog(tail int, filter string) ([]string, int64) {
	f, err := os.Open(logPath())
	if err != nil {
		return nil, 0
	}
	defer f.Close()

	var (
		lines []string
		pos   int64
	)
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		line := s.Text()
		pos += int64(len(line)) + 1
		r, ok := parseRecord(line)
		if !ok {
			continue
		}
		if !watchMatches(r, filter) {
			continue
		}
		lines = append(lines, line)
	}
	if tail > 0 && len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return lines, pos
}

func drainWatch(cursor int64, filter string, color, withDate bool, out io.Writer) int64 {
	f, err := os.Open(logPath())
	if err != nil {
		return cursor
	}
	defer f.Close()
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
	for s.Scan() {
		line := s.Text()
		cursor += int64(len(line)) + 1
		r, ok := parseRecord(line)
		if !ok {
			continue
		}
		if !watchMatches(r, filter) {
			continue
		}
		fmt.Fprintln(out, renderWatch(r, color, withDate))
	}
	return cursor
}

func parseRecord(s string) (Record, bool) {
	var r Record
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return r, false
	}
	return r, true
}

func normalizeFilter(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "@")
}

// watchMatches returns true when filter is empty, or `filter` is either
// sender or recipient on the record, or the record is a broadcast.
func watchMatches(r Record, filter string) bool {
	if filter == "" {
		return true
	}
	if r.From == filter {
		return true
	}
	if r.To == "@"+filter {
		return true
	}
	if r.To == "*" {
		return true
	}
	return false
}

func renderWatch(r Record, color, withDate bool) string {
	tStr := formatTs(r.Ts, withDate)
	switch {
	case r.Event != "":
		text := fmt.Sprintf("%s -- %s %s", tStr, r.From, r.Event)
		if color {
			return ansiDimIt + text + ansiReset
		}
		return text
	case r.Path != "":
		note := ""
		if r.Note != "" {
			note = " — " + r.Note
		}
		return fmt.Sprintf("%s %s -> %s [file] %s%s",
			tStr, colorizeNick(r.From, color), colorizeTo(r.To, color), r.Path, note)
	default:
		return fmt.Sprintf("%s %s -> %s  %s",
			tStr, colorizeNick(r.From, color), colorizeTo(r.To, color), r.Text)
	}
}

func formatTs(ts epochMs, withDate bool) string {
	t := time.UnixMilli(int64(float64(ts) * 1000))
	if withDate {
		return t.Format(dateFormat)
	}
	return t.Format(timeFormat)
}

func colorizeNick(nick string, color bool) string {
	if !color || nick == "" {
		return nick
	}
	return fmt.Sprintf("\x1b[%dm%s\x1b[0m", nickColor(nick), nick)
}

// colorizeTo preserves the leading "@" on recipient nicks while coloring the
// nick portion. Broadcast ("*") and empty values pass through uncolored.
func colorizeTo(to string, color bool) string {
	if !color || to == "" || to == "*" {
		return to
	}
	if strings.HasPrefix(to, "@") {
		return "@" + colorizeNick(to[1:], color)
	}
	return colorizeNick(to, color)
}

func nickColor(nick string) int {
	sum := sha256.Sum256([]byte(nick))
	return nickPalette[int(sum[0])%len(nickPalette)]
}
