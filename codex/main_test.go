package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddRemoveHookRoundTrip(t *testing.T) {
	root := map[string]any{}
	cmdStart := "/bin/agent-chat hook-start --emit codex"
	cmdStop := "/bin/agent-chat hook-stop"

	if !addHook(root, "SessionStart", cmdStart, 0) {
		t.Fatal("first add reported no change")
	}
	if addHook(root, "SessionStart", cmdStart, 0) {
		t.Fatal("second add must be a no-op (idempotent)")
	}
	if !addHook(root, "SessionEnd", cmdStop, 3) {
		t.Fatal("SessionEnd add reported no change")
	}

	// Shape check: {"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":...}]}]}}
	b, _ := json.Marshal(root)
	s := string(b)
	for _, want := range []string{`"type":"command"`, `"timeout":3`, cmdStop} {
		if !strings.Contains(s, want) {
			t.Errorf("serialized hooks.json missing %q:\n%s", want, s)
		}
	}

	if !removeHook(root, "SessionStart", cmdStart) {
		t.Fatal("remove SessionStart reported no change")
	}
	if !removeHook(root, "SessionEnd", cmdStop) {
		t.Fatal("remove SessionEnd reported no change")
	}
	if len(root) != 0 {
		t.Errorf("empty hook sets must be pruned entirely; got %v", root)
	}
}

// Foreign entries under the same events survive install + uninstall.
func TestRemoveHookKeepsForeignEntries(t *testing.T) {
	root := map[string]any{}
	if !addHook(root, "SessionStart", "other-tool --hello", 0) {
		t.Fatal("seed add failed")
	}
	addHook(root, "SessionStart", "/bin/agent-chat hook-start --emit codex", 0)
	if !removeHook(root, "SessionStart", "/bin/agent-chat hook-start --emit codex") {
		t.Fatal("remove reported no change")
	}
	b, _ := json.Marshal(root)
	if !strings.Contains(string(b), "other-tool --hello") {
		t.Errorf("foreign hook entry was dropped:\n%s", b)
	}
}

func TestAddTomlBlockFreshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	res, err := addTomlBlock(path, "/home/u/.agent-chat")
	if err != nil {
		t.Fatal(err)
	}
	if res != tomlAdded {
		t.Fatalf("result = %v, want tomlAdded", res)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{tomlMarkerBegin, "[sandbox_workspace_write]", `writable_roots = ["/home/u/.agent-chat"]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("config.toml missing %q:\n%s", want, b)
		}
	}

	// Idempotent: a second add is a quiet no-op.
	if res, err = addTomlBlock(path, "/home/u/.agent-chat"); err != nil || res != tomlPresent {
		t.Errorf("second add: res=%v err=%v, want tomlPresent and nil", res, err)
	}
}

func TestAddTomlBlockAppendsToExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := "model = \"gpt-5\"\napproval_policy = \"on-request\"\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := addTomlBlock(path, "/h/.agent-chat"); err != nil || res != tomlAdded {
		t.Fatalf("res=%v err=%v", res, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), orig) {
		t.Errorf("existing config content was disturbed:\n%s", b)
	}

	removed, err := removeTomlBlock(path)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	b, _ = os.ReadFile(path)
	if string(b) != orig {
		t.Errorf("uninstall must restore the file byte-identical:\n got %q\nwant %q", b, orig)
	}
}

// A config.toml that already declares [sandbox_workspace_write] is never
// touched — appending a duplicate table would corrupt the TOML.
func TestAddTomlBlockRefusesForeignSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := "[sandbox_workspace_write]\nwritable_roots = [\"/somewhere\"]\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := addTomlBlock(path, "/h/.agent-chat")
	if err != nil {
		t.Fatal(err)
	}
	if res != tomlManual {
		t.Fatalf("result = %v, want tomlManual", res)
	}
	b, _ := os.ReadFile(path)
	if string(b) != orig {
		t.Errorf("file with a foreign section must not be modified:\n%s", b)
	}
}

func TestRemoveTomlBlockNoBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("model = \"gpt-5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := removeTomlBlock(path)
	if err != nil || removed {
		t.Errorf("removed=%v err=%v, want false and nil", removed, err)
	}
	// Missing file is a quiet no-op too.
	removed, err = removeTomlBlock(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil || removed {
		t.Errorf("missing file: removed=%v err=%v, want false and nil", removed, err)
	}
}

// Codex rewrites config.toml itself and appends new tables at the very end of
// the document, before any trailing comment — observed with Codex 0.149, which
// persisted its hook-trust [hooks.state."…"] tables directly after our block.
// Uninstall must remove only our lines and leave Codex's tables intact.
func TestRemoveTomlBlockKeepsTablesCodexAppendedAfterOurs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := "[projects.\"/x\"]\ntrust_level = \"trusted\"\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := addTomlBlock(path, "/h/.agent-chat"); err != nil || res != tomlAdded {
		t.Fatalf("res=%v err=%v", res, err)
	}
	codexTables := "\n[hooks.state]\n\n[hooks.state.\"/h/.codex/hooks.json:session_start:0:0\"]\ntrusted_hash = \"sha256:abc\"\n"
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(b, codexTables...), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, err := removeTomlBlock(path)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	b, _ = os.ReadFile(path)
	got := string(b)
	if strings.Contains(got, tomlMarkerBegin) || strings.Contains(got, "writable_roots") || strings.Contains(got, "[sandbox_workspace_write]") {
		t.Errorf("our lines survived uninstall:\n%s", got)
	}
	if !strings.HasPrefix(got, orig) {
		t.Errorf("content before our block was disturbed:\n%s", got)
	}
	for _, want := range []string{"[hooks.state]", "trusted_hash = \"sha256:abc\""} {
		if !strings.Contains(got, want) {
			t.Errorf("Codex's own table %q was stripped by uninstall:\n%s", want, got)
		}
	}
}

// An install made by an older version wrote a closing marker, and Codex may
// since have appended tables between the two markers. Uninstall must still
// remove only our lines (and the stray end marker), never what sits between.
func TestRemoveTomlBlockLegacyEndMarkerWithCodexTablesInside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	legacy := "model = \"gpt-5\"\n\n" + tomlMarkerBegin + "\n# Allow agent-chat to write its home from inside the Codex sandbox.\n# Managed by the agent-chat installer; do not edit between the markers.\n[sandbox_workspace_write]\nwritable_roots = [\"/h/.agent-chat\"]\n\n[hooks.state]\n\n[hooks.state.\"k\"]\ntrusted_hash = \"sha256:abc\"\n" + tomlMarkerEnd + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := removeTomlBlock(path)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	want := "model = \"gpt-5\"\n\n[hooks.state]\n\n[hooks.state.\"k\"]\ntrusted_hash = \"sha256:abc\"\n"
	if got != want {
		t.Errorf("legacy uninstall:\n got %q\nwant %q", got, want)
	}
}

// Keys the user added under our [sandbox_workspace_write] header must not be
// re-parented into the previous table by removing the header: only our own
// writable_roots line and the marker comments go.
func TestRemoveTomlBlockKeepsHeaderWhenUserAddedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if res, err := addTomlBlock(path, "/h/.agent-chat"); err != nil || res != tomlAdded {
		t.Fatalf("res=%v err=%v", res, err)
	}
	b, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(b, "network_access = true\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := removeTomlBlock(path)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	b, _ = os.ReadFile(path)
	want := "[sandbox_workspace_write]\nnetwork_access = true\n"
	if string(b) != want {
		t.Errorf("got %q\nwant %q", b, want)
	}
}
