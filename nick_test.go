package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// chdirTo changes process cwd for the test and restores it on cleanup.
// Process-wide state — do not call t.Parallel() in any test that uses this.
func chdirTo(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
}

// cleanResolverEnv wipes everything the nick resolver might consult so each
// test can opt back in to exactly the layer(s) it cares about. Returns the
// AGENT_CHAT_HOME so callers can read the log file.
func cleanResolverEnv(t *testing.T) string {
	t.Helper()
	home := withTempHome(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AGENT_CHAT_NICK", "")
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "")
	t.Setenv("USER", "")
	chdirTo(t, t.TempDir())
	return home
}

func writeByCwd(t *testing.T, key, nick string) {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	dir := filepath.Join(chatHome(), "by-cwd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, hex.EncodeToString(sum[:])+".nick")
	if err := os.WriteFile(p, []byte(nick), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveNickFromFlag(t *testing.T) {
	cleanResolverEnv(t)
	t.Setenv("AGENT_CHAT_NICK", "envnick")
	t.Setenv("USER", "username")
	got, err := resolveNick("flagnick")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "flagnick" {
		t.Errorf("got %q, want flagnick", got)
	}
}

func TestResolveNickFromEnv(t *testing.T) {
	cleanResolverEnv(t)
	t.Setenv("AGENT_CHAT_NICK", "envnick")
	t.Setenv("USER", "username")
	got, err := resolveNick("")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "envnick" {
		t.Errorf("got %q, want envnick", got)
	}
}

func TestResolveNickFromByCwdNoGit(t *testing.T) {
	cleanResolverEnv(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	writeByCwd(t, cwd, "cwdnick")
	got, err := resolveNick("")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "cwdnick" {
		t.Errorf("got %q, want cwdnick", got)
	}
}

func TestResolveNickFromByCwdUsesGitRoot(t *testing.T) {
	cleanResolverEnv(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	chdirTo(t, sub)
	// `git rev-parse --show-toplevel` resolves symlinks (e.g. /tmp →
	// /private/tmp on macOS), so canonicalise before hashing.
	abs, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	writeByCwd(t, abs, "rootnick")
	got, err := resolveNick("")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "rootnick" {
		t.Errorf("got %q, want rootnick (git root lookup)", got)
	}
}

func TestResolveNickFromConfigFile(t *testing.T) {
	cleanResolverEnv(t)
	cfgRoot := t.TempDir()
	cfg := filepath.Join(cfgRoot, "agent-chat", "nick")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("\nconfignick\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfgRoot)
	got, err := resolveNick("")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "confignick" {
		t.Errorf("got %q, want confignick (first non-empty line)", got)
	}
}

func TestResolveNickHumanFromUser(t *testing.T) {
	cleanResolverEnv(t)
	t.Setenv("USER", "  someuser  ")
	got, err := resolveNickHuman("")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "someuser" {
		t.Errorf("got %q, want someuser (trimmed)", got)
	}
}

// The agent-facing resolver must never fall back to $USER: a session whose
// by-cwd claim was torn down would silently relabel its traffic as the human
// (the "agent sends as eljeffe" incident). It errors instead.
func TestResolveNickAgentNeverFallsBackToUser(t *testing.T) {
	cleanResolverEnv(t)
	t.Setenv("USER", "someuser")
	if got, err := resolveNick(""); err == nil {
		t.Errorf("want error, got %q", got)
	}
}

func TestResolveNickFromClaudeEnv(t *testing.T) {
	cleanResolverEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "madrid migration!")
	t.Setenv("USER", "someuser")
	cwd, _ := os.Getwd()
	writeByCwd(t, cwd, "cwdnick")
	got, err := resolveNick("")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// Sanitized the same way the join hook sanitizes, and wins over by-cwd —
	// the launch env var is the session's identity of record.
	if got != "madridmigration" {
		t.Errorf("got %q, want madridmigration", got)
	}
}

// A session in a git repo whose claim file vanished re-derives the nick the
// join hook would have derived (git root basename) instead of falling
// through to the config/user tiers.
func TestResolveNickDerivesFromGitRootWhenClaimGone(t *testing.T) {
	cleanResolverEnv(t)
	root := initGitRepo(t, t.TempDir())
	chdirTo(t, root)
	t.Setenv("USER", "someuser")

	cfgRoot := t.TempDir()
	cfg := filepath.Join(cfgRoot, "agent-chat", "nick")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("confignick"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfgRoot)

	got, err := resolveNick("")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if want := sanitizeNick(filepath.Base(root)); got != want {
		t.Errorf("got %q, want %q (git root derivation beats config)", got, want)
	}
}

func TestResolveNickDerivesFromAgentChatNickFile(t *testing.T) {
	cleanResolverEnv(t)
	cwd, _ := os.Getwd()
	if err := os.WriteFile(filepath.Join(cwd, ".agent-chat-nick"), []byte("\nfile-nick\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := resolveNick("")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "file-nick" {
		t.Errorf("got %q, want file-nick", got)
	}
}

func TestResolveNickErrorMentionsEnvOverride(t *testing.T) {
	cleanResolverEnv(t)
	_, err := resolveNick("")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "CLAUDE_AGENT_CHAT_NICK") {
		t.Errorf("error %q does not mention CLAUDE_AGENT_CHAT_NICK", err.Error())
	}
}

func TestResolveNickPrecedence(t *testing.T) {
	cleanResolverEnv(t)

	cwd, _ := os.Getwd()
	writeByCwd(t, cwd, "by-cwd-nick")

	cfgRoot := t.TempDir()
	cfg := filepath.Join(cfgRoot, "agent-chat", "nick")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("config-nick"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", cfgRoot)

	t.Setenv("USER", "user-nick")
	t.Setenv("AGENT_CHAT_NICK", "env-nick")

	if got, err := resolveNick("flag-nick"); err != nil || got != "flag-nick" {
		t.Errorf("flag should win, got %q err %v", got, err)
	}
	if got, err := resolveNick(""); err != nil || got != "env-nick" {
		t.Errorf("env should win over by-cwd, got %q err %v", got, err)
	}

	t.Setenv("AGENT_CHAT_NICK", "")
	if got, err := resolveNick(""); err != nil || got != "by-cwd-nick" {
		t.Errorf("by-cwd should win over config, got %q err %v", got, err)
	}

	if err := os.RemoveAll(filepath.Join(chatHome(), "by-cwd")); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveNick(""); err != nil || got != "config-nick" {
		t.Errorf("config should win over user, got %q err %v", got, err)
	}

	if err := os.RemoveAll(cfgRoot); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveNickHuman(""); err != nil || got != "user-nick" {
		t.Errorf("human user fallback, got %q err %v", got, err)
	}
	if got, err := resolveNick(""); err == nil {
		t.Errorf("agent resolver must not fall back to $USER, got %q", got)
	}
}

// Retrofit smoke tests: every verb that took --as must now consult the resolver
// when --as is omitted.

func TestSendUsesResolverWhenNoAs(t *testing.T) {
	home := cleanResolverEnv(t)
	t.Setenv("AGENT_CHAT_NICK", "resolved")
	if rc := run([]string{"send", "@bob", "hi"}); rc != 0 {
		t.Fatalf("send rc = %d", rc)
	}
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"from":"resolved"`) {
		t.Errorf("from not from resolver: %s", lines[0])
	}
}

func TestShareUsesResolverWhenNoAs(t *testing.T) {
	home := cleanResolverEnv(t)
	t.Setenv("AGENT_CHAT_NICK", "resolved")
	withStdin(t, "x")
	if rc := run([]string{"share", "@bob"}); rc != 0 {
		t.Fatalf("share rc = %d", rc)
	}
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"from":"resolved"`) {
		t.Errorf("from not from resolver: %s", lines[0])
	}
	wantPrefix := filepath.Join(home, "artifacts", "resolved") + string(filepath.Separator)
	if !strings.Contains(lines[0], wantPrefix) {
		t.Errorf("artifact path not under artifacts/resolved/: %s", lines[0])
	}
}

func TestHistoryToMeUsesResolverWhenNoAs(t *testing.T) {
	home := cleanResolverEnv(t)
	t.Setenv("AGENT_CHAT_NICK", "alice")
	writeLog(t, home, lineAliceBob, lineBobAlice, lineBroadcast)
	out, rc := captureStdout(t, func() int { return run([]string{"history", "--to", "me"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if !strings.Contains(out, "hi alice") {
		t.Errorf("missing @alice line:\n%s", out)
	}
	if !strings.Contains(out, "hello room") {
		t.Errorf("missing broadcast:\n%s", out)
	}
	if strings.Contains(out, "hi bob") {
		t.Errorf("bob-recipient leaked:\n%s", out)
	}
}

// Every verb must accept --as at any argument position.

func TestExtractAs(t *testing.T) {
	cases := []struct {
		args     []string
		wantAs   string
		wantRest []string
		wantErr  bool
	}{
		{[]string{"--as", "a", "@b", "hi"}, "a", []string{"@b", "hi"}, false},
		{[]string{"@b", "--as", "a", "hi"}, "a", []string{"@b", "hi"}, false},
		{[]string{"@b", "hi", "--as", "a"}, "a", []string{"@b", "hi"}, false},
		{[]string{"@b", "hi", "--as=a"}, "a", []string{"@b", "hi"}, false},
		{[]string{"@b", "hi", "-as", "a"}, "a", []string{"@b", "hi"}, false},
		{[]string{"@b", "hi", "-as=a"}, "a", []string{"@b", "hi"}, false},
		{[]string{"--as", "a", "--", "--as", "x"}, "a", []string{"--", "--as", "x"}, false},
		{[]string{"@b", "hi"}, "", []string{"@b", "hi"}, false},
		{[]string{"@b", "hi", "--as"}, "", nil, true},
	}
	for _, c := range cases {
		as, rest, err := extractAs(c.args)
		if (err != nil) != c.wantErr {
			t.Errorf("extractAs(%q) err = %v, wantErr %v", c.args, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if as != c.wantAs || strings.Join(rest, "\x00") != strings.Join(c.wantRest, "\x00") {
			t.Errorf("extractAs(%q) = %q, %q; want %q, %q", c.args, as, rest, c.wantAs, c.wantRest)
		}
	}
}

func TestSendAcceptsAsAtAnyPosition(t *testing.T) {
	for _, args := range [][]string{
		{"send", "@bob", "hi", "--as", "alice"},
		{"send", "@bob", "--as", "alice", "hi"},
		{"send", "--as=alice", "@bob", "hi"},
		{"send", "@bob", "hi", "--as=alice"},
		{"send", "@bob", "hi", "-as", "alice"},
		{"send", "@bob", "-as=alice", "hi"},
	} {
		home := cleanResolverEnv(t)
		if rc := run(args); rc != 0 {
			t.Fatalf("run(%q) rc = %d, want 0", args, rc)
		}
		lines := readLines(t, filepath.Join(home, "log.jsonl"))
		if len(lines) != 1 {
			t.Fatalf("run(%q): want 1 line, got %d", args, len(lines))
		}
		m := decodeOne(t, lines[0])
		if m["from"] != "alice" || m["to"] != "@bob" || m["text"] != "hi" {
			t.Errorf("run(%q): unexpected record %v", args, m)
		}
	}
}

func TestSendMissingAsValue(t *testing.T) {
	cleanResolverEnv(t)
	if rc := run([]string{"send", "@bob", "hi", "--as"}); rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
}

func TestShareAcceptsTrailingAs(t *testing.T) {
	home := cleanResolverEnv(t)
	f := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := run([]string{"share", "@bob", "--file", f, "--as", "alice"}); rc != 0 {
		t.Fatalf("share rc = %d, want 0", rc)
	}
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"from":"alice"`) {
		t.Errorf("from not alice: %s", lines[0])
	}
	wantPrefix := filepath.Join(home, "artifacts", "alice") + string(filepath.Separator)
	if !strings.Contains(lines[0], wantPrefix) {
		t.Errorf("artifact path not under artifacts/alice/: %s", lines[0])
	}
}

func TestHistoryAcceptsTrailingAs(t *testing.T) {
	home := cleanResolverEnv(t)
	writeLog(t, home, lineAliceBob, lineBobAlice)
	out, rc := captureStdout(t, func() int {
		return run([]string{"history", "--to", "me", "--format", "text", "--as", "alice"})
	})
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if !strings.Contains(out, "hi alice") {
		t.Errorf("missing @alice line:\n%s", out)
	}
	if strings.Contains(out, "hi bob") {
		t.Errorf("bob-recipient leaked:\n%s", out)
	}
}

func TestPeersAcceptsAs(t *testing.T) {
	cleanResolverEnv(t)
	if _, rc := captureStdout(t, func() int { return run([]string{"peers", "--as", "alice"}) }); rc != 0 {
		t.Errorf("peers --as rc = %d, want 0", rc)
	}
}

func TestResetAcceptsTrailingAs(t *testing.T) {
	cleanResolverEnv(t)
	if rc := run([]string{"reset", "@ghost", "--as", "alice"}); rc != 0 {
		t.Errorf("reset rc = %d, want 0", rc)
	}
}

func TestDanglingAsFailsPerVerb(t *testing.T) {
	for _, verb := range []string{"share", "history", "peers", "listen", "watch", "chat", "reset"} {
		t.Run(verb, func(t *testing.T) {
			cleanResolverEnv(t)
			stderr, rc := captureStderr(t, func() int { return run([]string{verb, "--as"}) })
			if rc != 2 {
				t.Errorf("rc = %d, want 2", rc)
			}
			if want := verb + ": --as requires a value"; !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, want)
			}
		})
	}
}

func TestBadFlagPrintsUsagePerVerb(t *testing.T) {
	for _, verb := range []string{"send", "history", "peers", "listen", "watch", "chat"} {
		t.Run(verb, func(t *testing.T) {
			cleanResolverEnv(t)
			stderr, rc := captureStderr(t, func() int { return run([]string{verb, "--bogus"}) })
			if rc != 2 {
				t.Errorf("rc = %d, want 2", rc)
			}
			if want := "usage: agent-chat " + verb; !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, want)
			}
		})
	}
}
