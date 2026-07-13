package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// listenNotifyMaxBytes caps the JSON event listen emits. Measured against the
// live harness: events past roughly 500 bytes are cut and marked "(truncated)";
// 400 leaves headroom for JSON escaping (<, > and & each cost six bytes).
const listenNotifyMaxBytes = 400

// noticePreviewRunes is the opening slice of an oversized body shown inline, so
// the recipient can judge urgency without a round-trip. Shrunk further if the
// envelope still will not fit.
const noticePreviewRunes = 180

// notice stands in for a record whose line is too long to survive delivery. It
// carries no `text` key on purpose: a peer must not be able to mistake the
// preview for the whole message. `full` is the command that prints it in one piece.
type notice struct {
	Ts      epochMs `json:"ts"`
	From    string  `json:"from"`
	To      string  `json:"to,omitempty"`
	Path    string  `json:"path,omitempty"`
	Clipped bool    `json:"clipped"`
	Bytes   int     `json:"bytes"`
	Preview string  `json:"preview,omitempty"`
	Full    string  `json:"full"`
}

// notifyLine returns the bytes listen should emit for one log line: the line
// itself when it fits, otherwise a notice shaped to stay under the cap so that
// it cannot itself be clipped.
func notifyLine(line []byte, r Record) []byte {
	if len(line) <= listenNotifyMaxBytes {
		return line
	}

	body := r.Text
	if body == "" {
		body = r.Note
	}
	n := notice{
		Ts:      r.Ts,
		From:    r.From,
		To:      r.To,
		Path:    r.Path,
		Clipped: true,
		Bytes:   len(body),
		Full:    fmt.Sprintf("agent-chat history --id %s --format text", r.Ts),
	}

	// Shed detail until the encoded notice fits, least useful first. Escaping
	// means encoded size is not a fixed function of rune count, so measure the
	// real thing at every step rather than budget for a worst case.
	for budget := noticePreviewRunes; budget >= 0; budget -= 20 {
		n.Preview = previewOf(body, budget)
		if out, ok := fitNotice(n); ok {
			return out
		}
	}

	// The fixed fields alone overflow the cap. Nothing shed here is lost: `full`
	// refetches the record whole. Drop the path rather than shorten it — a half
	// path invites a peer to read something that is not there.
	n.Path = ""
	if out, ok := fitNotice(n); ok {
		return out
	}
	n.From, n.To = clampNick(n.From), clampNick(n.To)
	if out, ok := fitNotice(n); ok {
		return out
	}
	return line
}

// fitNotice encodes a notice and reports whether it survives delivery intact.
func fitNotice(n notice) ([]byte, bool) {
	out, err := json.Marshal(n)
	if err != nil {
		return nil, false
	}
	return out, len(out) <= listenNotifyMaxBytes
}

// clampNick bounds a nick to the length the join path already enforces, for the
// senders that reach the log around it (`send --as` takes any string).
func clampNick(nick string) string {
	if utf8.RuneCountInString(nick) <= nickMaxLen {
		return nick
	}
	cut := 0
	for i := range nick {
		if cut == nickMaxLen {
			return nick[:i] + "…"
		}
		cut++
	}
	return nick
}

// previewOf returns the first `budget` runes of a body as a single line, with
// interior whitespace collapsed so a preview of a multi-line message stays one
// readable fragment.
func previewOf(body string, budget int) string {
	if budget <= 0 {
		return ""
	}
	flat := strings.Join(strings.Fields(body), " ")
	if utf8.RuneCountInString(flat) <= budget {
		return flat
	}
	cut := 0
	for i := range flat {
		if cut == budget {
			return strings.TrimSpace(flat[:i]) + "…"
		}
		cut++
	}
	return flat
}
