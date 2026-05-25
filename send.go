package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func runSend(args []string) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	as := fs.String("as", "", "sender nick (overrides the resolver)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	nick, err := resolveNick(*as)
	if err != nil {
		fmt.Fprintf(os.Stderr, "send: %v\n", err)
		return 2
	}
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, `send: usage: send --as NICK <recipient>... "text"`)
		return 2
	}
	text := rest[len(rest)-1]
	recipients := rest[:len(rest)-1]
	if text == "" {
		fmt.Fprintln(os.Stderr, "send: empty text")
		return 2
	}
	for _, r := range recipients {
		if !validRecipient(r) {
			fmt.Fprintf(os.Stderr, "send: invalid recipient %q (expected @nick or *)\n", r)
			return 2
		}
	}
	ts := nowEpochMs()
	for _, r := range recipients {
		rec := Record{Ts: ts, From: nick, To: r, Text: text}
		if err := appendRecord(rec); err != nil {
			fmt.Fprintf(os.Stderr, "send: %v\n", err)
			return 1
		}
	}
	maybeWarnListener(os.Stderr, nick)
	return 0
}

func validRecipient(s string) bool {
	if s == "*" {
		return true
	}
	return strings.HasPrefix(s, "@") && len(s) > 1
}
