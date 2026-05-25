package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

func runHistory(args []string) int {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	as := fs.String("as", "", "your nick (resolves `--to me`)")
	fromFlag := fs.String("from", "", "filter by sender (@nick or nick)")
	toFlag := fs.String("to", "", "filter by recipient (@nick or 'me')")
	since := fs.String("since", "", "DUR (1h, 30m, 2d) or DATE (2026-05-24 / RFC3339)")
	tail := fs.Int("tail", 0, "keep only the last N matches (0 = all)")
	format := fs.String("format", "json", "json | text")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *format != "json" && *format != "text" {
		fmt.Fprintf(os.Stderr, "history: unknown --format %q (want json|text)\n", *format)
		return 2
	}

	fromNick := strings.TrimPrefix(*fromFlag, "@")

	var (
		wantTo string
		meMode bool
	)
	if *toFlag != "" {
		if *toFlag == "me" {
			nick, err := resolveNick(*as)
			if err != nil {
				fmt.Fprintf(os.Stderr, "history: --to me: %v\n", err)
				return 2
			}
			wantTo = "@" + nick
			meMode = true
		} else {
			t := *toFlag
			if !strings.HasPrefix(t, "@") {
				t = "@" + t
			}
			if len(t) < 2 {
				fmt.Fprintf(os.Stderr, "history: invalid --to %q\n", *toFlag)
				return 2
			}
			wantTo = t
		}
	}

	var sinceTs float64
	haveSince := false
	if *since != "" {
		t, err := parseSince(*since, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "history: %v\n", err)
			return 2
		}
		sinceTs = float64(t.UnixMilli()) / 1000.0
		haveSince = true
	}

	f, err := os.Open(logPath())
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		fmt.Fprintf(os.Stderr, "history: %v\n", err)
		return 1
	}
	defer f.Close()

	rc := writeHistory(f, os.Stdout, *format, fromNick, wantTo, meMode, sinceTs, haveSince, *tail)
	if rc == 0 {
		if n, err := resolveNick(*as); err == nil {
			maybeWarnListener(os.Stderr, n)
		}
	}
	return rc
}

func writeHistory(in io.Reader, out io.Writer, format, fromNick, wantTo string, meMode bool, sinceTs float64, haveSince bool, tail int) int {
	type entry struct {
		raw []byte
		rec Record
	}
	var matched []entry

	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for s.Scan() {
		line := append([]byte(nil), s.Bytes()...)
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if fromNick != "" && r.From != fromNick {
			continue
		}
		if wantTo != "" {
			if !(r.To == wantTo || (meMode && r.To == "*")) {
				continue
			}
		}
		if haveSince && float64(r.Ts) < sinceTs {
			continue
		}
		matched = append(matched, entry{line, r})
	}
	if err := s.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "history: %v\n", err)
		return 1
	}

	if tail > 0 && len(matched) > tail {
		matched = matched[len(matched)-tail:]
	}

	bw := bufio.NewWriter(out)
	defer bw.Flush()
	for _, e := range matched {
		if format == "text" {
			fmt.Fprintln(bw, renderText(e.rec))
			continue
		}
		bw.Write(e.raw)
		bw.WriteByte('\n')
	}
	return 0
}

func renderText(r Record) string {
	t := time.UnixMilli(int64(float64(r.Ts) * 1000)).Format("15:04:05")
	switch {
	case r.Event != "":
		return fmt.Sprintf("%s -- %s %s", t, r.From, r.Event)
	case r.Path != "":
		note := ""
		if r.Note != "" {
			note = " — " + r.Note
		}
		return fmt.Sprintf("%s %s -> %s [file] %s%s", t, r.From, r.To, r.Path, note)
	default:
		return fmt.Sprintf("%s %s -> %s  %s", t, r.From, r.To, r.Text)
	}
}

func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty --since")
	}
	if isDurationLike(s) {
		d, err := parseDur(s)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid --since duration %q: %w", s, err)
		}
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (want DUR like 1h/30m/2d or date YYYY-MM-DD)", s)
}

func isDurationLike(s string) bool {
	if s == "" {
		return false
	}
	last := s[len(s)-1]
	if last != 'h' && last != 'm' && last != 's' && last != 'd' && last != 'w' {
		return false
	}
	for i := 0; i < len(s)-1; i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) >= 2
}

func parseDur(s string) (time.Duration, error) {
	last := s[len(s)-1]
	if last == 'd' || last == 'w' {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil {
			return 0, err
		}
		mult := 24 * time.Hour
		if last == 'w' {
			mult = 7 * 24 * time.Hour
		}
		return time.Duration(n) * mult, nil
	}
	return time.ParseDuration(s)
}
