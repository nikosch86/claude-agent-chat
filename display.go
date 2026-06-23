package main

import "strings"

// sanitizeDisplay escapes C0 control bytes and DEL to a visible \xNN form so a
// peer-supplied string cannot inject ANSI/terminal-control sequences when
// printed. Printable content, including multi-byte UTF-8, passes through.
func sanitizeDisplay(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	const hex = "0123456789abcdef"
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		if isControl(r) {
			b.WriteString(`\x`)
			b.WriteByte(hex[byte(r)>>4])
			b.WriteByte(hex[byte(r)&0xf])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f
}
