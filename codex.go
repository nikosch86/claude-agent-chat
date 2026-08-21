package main

// Codex CLI wiring. Codex has no equivalent of Claude Code's Monitor tool, so
// incoming chat traffic is delivered by a small detached bridge process: it
// runs the same listen loop as `agent-chat listen` and forwards each matching
// line into the running Codex session via `codex queue --thread <id>
// --message <text>` (durable queue with idle dispatch, Codex CLI >= 0.149).
//
//	SessionStart hook  -> hook-start --emit codex   (join + primer + spawn bridge)
//	<bridge>           -> codex queue per incoming message
//	SessionEnd hook    -> hook-stop                 (quit record + stop bridge)
//
// The bridge holds the same per-nick listener singleton lock as `listen`, so a
// Claude Monitor listener and a codex bridge can never double-consume one
// nick's cursor — newest wins, exactly like two listeners.

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// bridgeMaxDeliverFails is how many consecutive `codex queue` failures the
// bridge tolerates before concluding the session (or the codex binary) is gone
// and exiting. A success resets the counter.
var bridgeMaxDeliverFails = 5

func runCodexBridge(args []string) int {
	as, args, err := extractAs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codex-bridge: %v\n", err)
		return 2
	}
	fs := flag.NewFlagSet("codex-bridge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	thread := fs.String("thread", "", "codex thread/session id to queue messages into")
	foreground := fs.Bool("foreground", false, "run in the foreground instead of detaching")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: agent-chat codex-bridge --thread ID [--as NICK] [--foreground]")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *thread == "" {
		fmt.Fprintln(os.Stderr, "codex-bridge: --thread is required (the codex session id from the hook payload)")
		return 2
	}
	nick, err := resolveNick(as)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codex-bridge: %v\n", err)
		return 2
	}
	if !*foreground {
		if err := spawnCodexBridge(nick, *thread); err != nil {
			fmt.Fprintf(os.Stderr, "codex-bridge: %v\n", err)
			return 1
		}
		return 0
	}
	return codexBridgeForeground(nick, *thread)
}

// spawnCodexBridge starts the detached bridge process for nick, logging to
// codex-bridge.log in the nick's agent dir. Called by hook-start --emit codex
// and by a bare `codex-bridge` invocation (which re-execs itself).
func spawnCodexBridge(nick, thread string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot locate own binary: %w", err)
	}
	logPath := bridgeLogPath(nick)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(self, "codex-bridge", "--foreground", "--thread", thread, "--as", nick)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bridge: %w", err)
	}
	return cmd.Process.Release()
}

func codexBridgeForeground(nick, thread string) int {
	// Same singleton as listen: a newer listener/bridge for this nick evicts us
	// (SIGTERM), and starting here evicts any incumbent.
	defer tryLockListener(nick)()

	if err := writeBridgePid(nick); err != nil {
		fmt.Fprintf(os.Stderr, "codex-bridge: warning: cannot write pidfile: %v\n", err)
	}
	defer os.Remove(bridgePidPath(nick))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(os.Stderr, "codex-bridge: forwarding messages for @%s into codex thread %s\n", nick, thread)
	w := &bridgeWriter{
		cancel: cancel,
		deliver: func(msg string) error {
			return codexQueue(ctx, thread, msg)
		},
	}
	return listenLoop(ctx, nick, w)
}

// bridgeWriter adapts listenLoop's line stream into `codex queue` deliveries.
// Internal notices (the "[agent-chat]" farewell/self-heal lines) are dropped —
// they explain Monitor mechanics that do not exist on the codex side.
type bridgeWriter struct {
	buf     []byte
	deliver func(string) error
	cancel  context.CancelFunc
	fails   int
}

func (w *bridgeWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		nl := bytes.IndexByte(w.buf, '\n')
		if nl < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:nl]))
		w.buf = w.buf[nl+1:]
		if line == "" || strings.HasPrefix(line, "[agent-chat]") {
			continue
		}
		msg := "New agent-chat message:\n" + renderBridgeLine(line)
		if err := w.deliver(msg); err != nil {
			w.fails++
			fmt.Fprintf(os.Stderr, "codex-bridge: deliver failed (%d/%d): %v\n", w.fails, bridgeMaxDeliverFails, err)
			if w.fails >= bridgeMaxDeliverFails {
				fmt.Fprintln(os.Stderr, "codex-bridge: codex queue keeps failing — session likely gone; exiting")
				w.cancel()
			}
			continue
		}
		w.fails = 0
	}
	return len(p), nil
}

// renderBridgeLine turns one raw listen line (JSON) into the text queued into
// the codex session — the Go twin of the kilo plugin's formatLine. Unparseable
// lines pass through raw.
func renderBridgeLine(line string) string {
	var r struct {
		From    string  `json:"from"`
		Text    *string `json:"text"`
		Path    string  `json:"path"`
		Note    string  `json:"note"`
		Clipped bool    `json:"clipped"`
		Bytes   int     `json:"bytes"`
		Preview string  `json:"preview"`
		Full    string  `json:"full"`
	}
	if json.Unmarshal([]byte(line), &r) != nil {
		return line
	}
	who := "someone"
	if r.From != "" {
		who = "@" + r.From
	}
	if r.Clipped {
		what := ""
		if r.Path != "" {
			what = fmt.Sprintf(" (file: %s)", r.Path)
		}
		return fmt.Sprintf("%s sent a %d-byte message%s, too long to show here. It starts: %q — run `%s` to read all of it.", who, r.Bytes, what, r.Preview, r.Full)
	}
	if r.Path != "" {
		s := fmt.Sprintf("%s shared a file: %s", who, r.Path)
		if r.Note != "" {
			s += " — " + r.Note
		}
		return s
	}
	if r.Text != nil {
		return who + ": " + *r.Text
	}
	return line
}

// codexQueue pushes one message into the codex session's durable queue. The
// body travels as a single argv element — no shell in between, so peer text
// can never be expanded or split.
func codexQueue(ctx context.Context, thread, msg string) error {
	cmd := exec.CommandContext(ctx, codexBin(), "queue", "--thread", thread, "--message", msg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func codexBin() string {
	if v := os.Getenv("AGENT_CHAT_CODEX_BIN"); v != "" {
		return v
	}
	return "codex"
}

func bridgePidPath(nick string) string {
	return filepath.Join(chatHome(), "agents", nick, "codex-bridge.pid")
}

func bridgeLogPath(nick string) string {
	return filepath.Join(chatHome(), "agents", nick, "codex-bridge.log")
}

func writeBridgePid(nick string) error {
	p := bridgePidPath(nick)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
}

// stopCodexBridge terminates the recorded bridge for nick, if any. The pidfile
// is removed unconditionally; the signal is only sent when the pid still looks
// like one of our own bridges (recycled-pid guard, same approach as the
// listener lock takeover). No-op when no bridge was ever started — i.e. on
// every Claude/kilo session.
func stopCodexBridge(nick string) {
	p := bridgePidPath(nick)
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	os.Remove(p)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return
	}
	terminateBridge(pid)
}
