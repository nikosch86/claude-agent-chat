package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// chatPollInterval is how often the interactive client rechecks the log for
// new records. Matches watchPollInterval; kept separate so tests can tune it.
var chatPollInterval = 200 * time.Millisecond

// runChat is an interactive, full-duplex client for a human on the bus: it
// follows the log like `watch` while letting you type messages like `send`,
// with a pinned input line so incoming traffic never scrambles your typing.
func runChat(args []string) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	as := fs.String("as", "", "your nick (overrides the resolver)")
	tail := fs.Int("tail", 30, "initial backlog count")
	noColor := fs.Bool("no-color", false, "disable ANSI color")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	nick, err := resolveNick(*as)
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		return 2
	}
	if !stdinIsTTY() {
		fmt.Fprintln(os.Stderr, "chat: stdin is not a terminal — run it in an interactive shell")
		return 2
	}

	restore, err := enterRaw()
	if err != nil {
		fmt.Fprintf(os.Stderr, "chat: %v\n", err)
		return 1
	}
	defer restore()

	// Register the signal handler before drawing anything so a Ctrl-C during
	// startup still reaches us and lets `restore` put the terminal back.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	return chatLoop(nick, *tail, !*noColor, sig)
}

func chatLoop(nick string, tail int, color bool, sig <-chan os.Signal) int {
	w := bufio.NewWriter(os.Stdout)
	ed := &lineEditor{} // the in-progress input line + recall history
	var pending []byte  // bytes of an in-progress multi-byte UTF-8 rune

	drawInput := func() {
		fmt.Fprintf(w, "\r\x1b[K%s%s", chatPrompt(nick, color), string(ed.line))
		// Park the terminal cursor at the edit position (columns back from end).
		if back := len(ed.line) - ed.cursor; back > 0 {
			fmt.Fprintf(w, "\x1b[%dD", back)
		}
		w.Flush()
	}
	// note prints a local, un-sent status line above the input and redraws it.
	note := func(s string) {
		fmt.Fprintf(w, "\r\x1b[K%s\r\n", chatNote(s, color))
		drawInput()
	}

	// Lifecycle events mirror `watch`, so a human running chat shows up in
	// `peers` while connected and disappears on exit.
	_ = appendRecord(Record{Ts: nowEpochMs(), From: nick, Event: "joined"})
	// Wrapped in a closure so nowEpochMs() runs at exit, stamping the quit
	// record with the exit time.
	defer func() {
		_ = appendRecord(Record{Ts: nowEpochMs(), From: nick, Event: "quit"})
	}()

	backlog, cursor := readBacklog(tail, "")
	for _, line := range backlog {
		if r, ok := parseRecord(line); ok {
			fmt.Fprintf(w, "\r\x1b[K%s\r\n", renderWatch(r, color, false))
		}
	}
	if chatNickInUse(nick, backlog) {
		note(fmt.Sprintf("[chat] heads-up: '%s' already appears on this bus (possibly an agent). "+
			"Restart with `agent-chat chat --as <another-nick>` to avoid a clash.", nick))
	} else {
		drawInput()
	}

	keys := make(chan byte, 256)
	go readKeys(keys)

	ticker := time.NewTicker(chatPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sig:
			fmt.Fprint(w, "\r\n")
			w.Flush()
			return 0

		case <-ticker.C:
			// Cheap guard: only repaint when the log actually grew, so an idle
			// bus doesn't rewrite the input line every tick.
			if currentLogSize() == cursor {
				continue
			}
			fmt.Fprint(w, "\r\x1b[K")
			cursor = drainWatch(cursor, "", color, false, w)
			drawInput()

		case b, ok := <-keys:
			if !ok { // stdin closed (EOF)
				fmt.Fprint(w, "\r\n")
				w.Flush()
				return 0
			}
			switch {
			case b == 4: // Ctrl-D
				fmt.Fprint(w, "\r\n")
				w.Flush()
				return 0
			case b == '\r' || b == '\n':
				pending = pending[:0]
				text := ed.submit()
				trimmed := strings.TrimSpace(text)
				if trimmed == "/quit" || trimmed == "/q" || trimmed == "/exit" {
					fmt.Fprint(w, "\r\n")
					w.Flush()
					return 0
				}
				if trimmed != "" {
					if warn := compose(nick, text); warn != "" {
						note("[chat] " + warn)
						continue
					}
					// The sent record comes back through the follow tick and is
					// rendered there — one render path, no local double-echo.
				}
				drawInput()
			case b == 127 || b == 8: // Backspace — delete the rune before the cursor
				pending = pending[:0]
				ed.backspace()
				drawInput()
			case b == 1: // Ctrl-A — start of line
				ed.home()
				drawInput()
			case b == 5: // Ctrl-E — end of line
				ed.end()
				drawInput()
			case b == 27: // ESC — an arrow / nav / edit key sequence
				pending = pending[:0]
				switch readEscape(keys) {
				case keyLeft:
					ed.left()
				case keyRight:
					ed.right()
				case keyHome:
					ed.home()
				case keyEnd:
					ed.end()
				case keyDelete:
					ed.del()
				case keyUp:
					ed.prev()
				case keyDown:
					ed.next()
				default:
					continue // bare/unknown ESC: nothing changed, skip redraw
				}
				drawInput()
			case b >= 32 && b != 127:
				// Accumulate bytes until they form a complete UTF-8 rune, so a
				// multi-byte character is inserted atomically.
				pending = append(pending, b)
				if utf8.FullRune(pending) {
					r, _ := utf8.DecodeRune(pending)
					pending = pending[:0]
					ed.insert(r)
					drawInput()
				}
			}
		}
	}
}

// splitCompose separates leading @nick/* recipient tokens from the message
// body. Recipient tokens and surrounding whitespace are stripped; the body is
// preserved verbatim. With no leading recipient token, recipients is empty.
func splitCompose(text string) (recipients []string, body string) {
	rest := strings.TrimSpace(text)
	for {
		rest = strings.TrimLeft(rest, " \t")
		tok := rest
		if i := strings.IndexAny(rest, " \t"); i >= 0 {
			tok = rest[:i]
		}
		if tok == "" || !validRecipient(tok) {
			break
		}
		recipients = append(recipients, tok)
		rest = rest[len(tok):]
	}
	return recipients, strings.TrimSpace(rest)
}

// compose parses a typed line into recipients + body and appends one record
// per recipient. With no leading recipient token, the message broadcasts to
// "*". Returns a non-empty warning string when nothing was sent.
func compose(nick, text string) string {
	recipients, body := splitCompose(text)
	if body == "" {
		return "nothing to send (only recipients given)"
	}
	if len(recipients) == 0 {
		recipients = []string{"*"}
	}
	ts := nowEpochMs()
	for _, r := range recipients {
		if err := appendRecord(Record{Ts: ts, From: nick, To: r, Text: body}); err != nil {
			return "send failed: " + err.Error()
		}
	}
	return ""
}

// chatNickInUse reports whether `nick` has already authored an actual message
// (not just a joined/quit event) in the backlog — a cheap guard against a human
// unknowingly sharing a nick with an agent that resolved to the same $USER.
func chatNickInUse(nick string, backlog []string) bool {
	for _, line := range backlog {
		if r, ok := parseRecord(line); ok && r.From == nick && r.Event == "" {
			return true
		}
	}
	return false
}

func chatPrompt(nick string, color bool) string {
	return colorizeNick(nick, color) + "> "
}

func chatNote(s string, color bool) string {
	if color {
		return ansiDimIt + s + ansiReset
	}
	return s
}

// readKeys forwards each byte read from stdin to keys, closing it on EOF/error.
// Raw mode (min 1 time 0) makes each Read return as soon as a byte is available.
func readKeys(keys chan<- byte) {
	var b [1]byte
	for {
		n, err := os.Stdin.Read(b[:])
		if n > 0 {
			keys <- b[0]
		}
		if err != nil {
			close(keys)
			return
		}
	}
}

// escKey names the navigation/editing keys that arrive as ESC-introduced
// sequences. keyNone means "unrecognized" — the sequence is dropped.
type escKey int

const (
	keyNone escKey = iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyDelete
)

// readEscape consumes the rest of an ESC-introduced sequence (the leading ESC
// is already read) and classifies it, handling both CSI (ESC [ …) and SS3
// (ESC O …); the whole sequence is consumed, and unknown or timed-out ones give keyNone.
func readEscape(keys <-chan byte) escKey {
	intro, ok := nextByte(keys)
	if !ok {
		return keyNone
	}
	if intro == 'O' { // SS3: ESC O <final>
		if f, ok := nextByte(keys); ok {
			return finalToKey(f, 0)
		}
		return keyNone
	}
	if intro != '[' {
		return keyNone
	}
	// CSI: read the first numeric parameter, then skip any remaining params /
	// separators / intermediates until the final byte.
	first, gotFirst := 0, false
	for {
		c, ok := nextByte(keys)
		if !ok {
			return keyNone
		}
		if c >= '0' && c <= '9' {
			first = first*10 + int(c-'0')
			gotFirst = true
			continue
		}
		if c >= 0x40 && c <= 0x7e { // final byte
			return finalToKey(c, firstParam(first, gotFirst))
		}
		// separator (';', ':') or intermediate — consume to the final byte.
		for {
			d, ok := nextByte(keys)
			if !ok {
				return keyNone
			}
			if d >= 0x40 && d <= 0x7e {
				return finalToKey(d, firstParam(first, gotFirst))
			}
		}
	}
}

func firstParam(n int, got bool) int {
	if got {
		return n
	}
	return 0
}

// finalToKey maps a CSI/SS3 final byte (plus the first numeric parameter, for
// the ~-terminated forms) to an editing key. Unknown combinations are keyNone.
func finalToKey(final byte, param int) escKey {
	switch final {
	case 'A':
		return keyUp
	case 'B':
		return keyDown
	case 'C':
		return keyRight
	case 'D':
		return keyLeft
	case 'H':
		return keyHome
	case 'F':
		return keyEnd
	case '~':
		switch param {
		case 1, 7:
			return keyHome
		case 4, 8:
			return keyEnd
		case 3:
			return keyDelete
		}
	}
	return keyNone
}

// nextByte reads one byte from keys, giving up after a short wait — the bytes of
// a terminal escape sequence arrive together, so a timeout means the sequence
// has ended (e.g. a bare ESC keypress).
func nextByte(keys <-chan byte) (byte, bool) {
	select {
	case b, ok := <-keys:
		return b, ok
	case <-time.After(50 * time.Millisecond):
		return 0, false
	}
}

// lineEditor is the interactive input buffer: a rune slice with a cursor, plus
// a recall history navigated with Up/Down. It is deliberately independent of
// the terminal so its editing rules are unit-tested without a pty.
type lineEditor struct {
	line    []rune
	cursor  int
	history [][]rune
	histIdx int    // == len(history) while editing a fresh (unsent) line
	stash   []rune // the fresh line, saved when navigating up into history
}

func (e *lineEditor) insert(r rune) {
	e.line = append(e.line, 0)
	copy(e.line[e.cursor+1:], e.line[e.cursor:])
	e.line[e.cursor] = r
	e.cursor++
}

func (e *lineEditor) backspace() {
	if e.cursor == 0 {
		return
	}
	e.line = append(e.line[:e.cursor-1], e.line[e.cursor:]...)
	e.cursor--
}

func (e *lineEditor) del() {
	if e.cursor >= len(e.line) {
		return
	}
	e.line = append(e.line[:e.cursor], e.line[e.cursor+1:]...)
}

func (e *lineEditor) left() {
	if e.cursor > 0 {
		e.cursor--
	}
}

func (e *lineEditor) right() {
	if e.cursor < len(e.line) {
		e.cursor++
	}
}

func (e *lineEditor) home() { e.cursor = 0 }
func (e *lineEditor) end()  { e.cursor = len(e.line) }

// load replaces the current line with a copy of rs (history is never aliased).
func (e *lineEditor) load(rs []rune) {
	e.line = append([]rune(nil), rs...)
	e.cursor = len(e.line)
}

func (e *lineEditor) prev() {
	if len(e.history) == 0 || e.histIdx == 0 {
		return
	}
	if e.histIdx == len(e.history) {
		e.stash = append([]rune(nil), e.line...)
	}
	e.histIdx--
	e.load(e.history[e.histIdx])
}

func (e *lineEditor) next() {
	if e.histIdx >= len(e.history) {
		return
	}
	e.histIdx++
	if e.histIdx == len(e.history) {
		e.load(e.stash) // back to the fresh line we stashed on the way up
	} else {
		e.load(e.history[e.histIdx])
	}
}

// submit returns the current line, appends it to history (skipping blank lines
// and immediate duplicates), and resets the editor to a fresh empty line.
func (e *lineEditor) submit() string {
	s := string(e.line)
	if strings.TrimSpace(s) != "" {
		if n := len(e.history); n == 0 || string(e.history[n-1]) != s {
			e.history = append(e.history, append([]rune(nil), e.line...))
		}
	}
	e.line = e.line[:0]
	e.cursor = 0
	e.histIdx = len(e.history)
	e.stash = nil
	return s
}

func stdinIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// enterRaw switches the terminal to byte-at-a-time, no-echo input via stty and
// returns a restore func that puts the saved settings back. Output processing
// is left untouched, so "\n" still maps to CRLF on write.
func enterRaw() (func(), error) {
	orig, err := sttyCapture("-g")
	if err != nil {
		return nil, fmt.Errorf("cannot read terminal state via stty (need a POSIX terminal): %w", err)
	}
	orig = strings.TrimSpace(orig)
	if err := sttyRun("-echo", "-icanon", "min", "1", "time", "0"); err != nil {
		return nil, fmt.Errorf("cannot enter raw mode via stty: %w", err)
	}
	return func() {
		_ = sttyRun(orig)
		fmt.Fprint(os.Stdout, "\r\n")
	}, nil
}

func sttyRun(args ...string) error {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func sttyCapture(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}
