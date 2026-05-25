package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func runShare(args []string) int {
	var (
		as, file, note string
		recipients     []string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprintln(os.Stdout, shareUsage)
			return 0
		case a == "--as":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "share: --as requires a value")
				return 2
			}
			i++
			as = args[i]
		case strings.HasPrefix(a, "--as="):
			as = a[len("--as="):]
		case a == "--file":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "share: --file requires a value")
				return 2
			}
			i++
			file = args[i]
		case strings.HasPrefix(a, "--file="):
			file = a[len("--file="):]
		case a == "--note":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "share: --note requires a value")
				return 2
			}
			i++
			note = args[i]
		case strings.HasPrefix(a, "--note="):
			note = a[len("--note="):]
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "share: unknown flag %q\n", a)
			return 2
		default:
			recipients = append(recipients, a)
		}
	}

	nick, err := resolveNick(as)
	if err != nil {
		fmt.Fprintf(os.Stderr, "share: %v\n", err)
		return 2
	}
	if len(recipients) == 0 {
		fmt.Fprintln(os.Stderr, "share: at least one recipient (@nick or *) is required")
		return 2
	}
	for _, r := range recipients {
		if !validRecipient(r) {
			fmt.Fprintf(os.Stderr, "share: invalid recipient %q (expected @nick or *)\n", r)
			return 2
		}
	}

	stdinPiped, err := stdinIsPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "share: %v\n", err)
		return 1
	}

	var (
		content  []byte
		basename string
	)
	switch {
	case file != "" && stdinPiped:
		fmt.Fprintln(os.Stderr, "share: --file and piped stdin are mutually exclusive")
		return 2
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "share: %v\n", err)
			return 1
		}
		content = b
		basename = filepath.Base(file)
	case stdinPiped:
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "share: %v\n", err)
			return 1
		}
		content = b
		basename = "stdin"
	default:
		fmt.Fprintln(os.Stderr, "share: provide --file PATH or pipe content on stdin")
		return 2
	}

	ts := nowEpochMs()
	tsMs := int64(float64(ts) * 1000)
	artPath, err := writeArtifact(nick, tsMs, basename, content)
	if err != nil {
		fmt.Fprintf(os.Stderr, "share: %v\n", err)
		return 1
	}

	for _, r := range recipients {
		rec := Record{Ts: ts, From: nick, To: r, Path: artPath, Note: note}
		if err := appendRecord(rec); err != nil {
			fmt.Fprintf(os.Stderr, "share: %v\n", err)
			return 1
		}
	}
	maybeWarnListener(os.Stderr, nick)
	return 0
}

func writeArtifact(nick string, unixMs int64, basename string, content []byte) (string, error) {
	dir := filepath.Join(chatHome(), "artifacts", nick)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(content)
	h.Write(strconv.AppendInt(nil, unixMs, 10))
	short := hex.EncodeToString(h.Sum(nil))[:6]
	name := strconv.FormatInt(unixMs, 10) + "-" + short + "-" + basename
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, content, 0o644); err != nil {
		return "", err
	}
	return full, nil
}

func stdinIsPipe() (bool, error) {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false, err
	}
	return fi.Mode()&os.ModeCharDevice == 0, nil
}

const shareUsage = `usage: agent-chat share [--as NICK] <recipient>... (--file PATH | <stdin>) [--note "..."]`
