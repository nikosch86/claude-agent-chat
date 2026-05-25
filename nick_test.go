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

func TestResolveNickFromUser(t *testing.T) {
	cleanResolverEnv(t)
	t.Setenv("USER", "  someuser  ")
	got, err := resolveNick("")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "someuser" {
		t.Errorf("got %q, want someuser (trimmed)", got)
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
	if got, err := resolveNick(""); err != nil || got != "user-nick" {
		t.Errorf("user fallback, got %q err %v", got, err)
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
