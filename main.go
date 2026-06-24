package main

import (
	"fmt"
	"os"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "send":
		return runSend(rest)
	case "share":
		return runShare(rest)
	case "history":
		return runHistory(rest)
	case "peers":
		return runPeers(rest)
	case "listen":
		return runListen(rest)
	case "watch":
		return runWatch(rest)
	case "hook-start":
		return runHookStart(rest)
	case "hook-stop":
		return runHookStop(rest)
	case "reset":
		return runReset(rest)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "agent-chat: unknown subcommand %q\n", verb)
		usage(os.Stderr)
		return 2
	}
}

func usage(w *os.File) {
	fmt.Fprintln(w, "usage: agent-chat <subcommand> [args...]")
	fmt.Fprintln(w, "  send [--as NICK] <recipient>... \"text\"")
	fmt.Fprintln(w, "  share [--as NICK] <recipient>... (--file PATH | <stdin>) [--note \"...\"]")
	fmt.Fprintln(w, "  history [--from @nick] [--to @nick|me] [--since DUR|DATE] [--tail N] [--format json|text] [--as NICK]")
	fmt.Fprintln(w, "  peers")
	fmt.Fprintln(w, "  listen [--as NICK]                      # stream new matching traffic to stdout")
	fmt.Fprintln(w, "  watch [--as NICK] [--filter @nick] [--tail N] [--no-color] [--date]")
	fmt.Fprintln(w, "  hook-start [--emit claude|text|json]     # SessionStart hook entry point (text/json for the kilo plugin)")
	fmt.Fprintln(w, "  hook-stop                               # SessionEnd hook entry point")
	fmt.Fprintln(w, "  reset [<nick>]                          # release a nick claim (defaults to resolver-derived)")
}
