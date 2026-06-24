package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// stripJSONC must remove // and /* */ comments but never touch a // that lives
// inside a string — e.g. the https:// in the $schema URL.
func TestStripJSONCPreservesURLs(t *testing.T) {
	in := []byte(`{
  // a line comment
  "$schema": "https://app.kilo.ai/config.json", /* block */
  "permission": { "bash": { "rm *": "deny" } }
}`)
	var m map[string]any
	if err := json.Unmarshal(stripJSONC(in), &m); err != nil {
		t.Fatalf("stripped output did not parse: %v", err)
	}
	if got := m["$schema"]; got != "https://app.kilo.ai/config.json" {
		t.Errorf("schema URL corrupted: %v", got)
	}
}

// A // sequence inside a string value must survive verbatim.
func TestStripJSONCKeepsSlashesInsideStrings(t *testing.T) {
	in := []byte(`{"note": "see http://x // not a comment"}`)
	var m map[string]any
	if err := json.Unmarshal(stripJSONC(in), &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := m["note"]; got != "see http://x // not a comment" {
		t.Errorf("string body altered: %q", got)
	}
}

func TestAddBashPermissionCreatesMapWhenUnset(t *testing.T) {
	s := map[string]any{}
	if !addBashPermission(s) {
		t.Fatal("expected a change")
	}
	bash := s["permission"].(map[string]any)["bash"].(map[string]any)
	if bash[bashPattern] != "allow" {
		t.Errorf("bash[%q] = %v, want allow", bashPattern, bash[bashPattern])
	}
}

// A blanket string policy already covers agent-chat; we must not downgrade it
// to an object (which would re-prompt every other command).
func TestAddBashPermissionLeavesStringPolicy(t *testing.T) {
	s := map[string]any{"permission": map[string]any{"bash": "allow"}}
	if addBashPermission(s) {
		t.Error("string bash policy must be left untouched")
	}
	if s["permission"].(map[string]any)["bash"] != "allow" {
		t.Error("string policy was modified")
	}
}

func TestAddBashPermissionAppendsToObjectAndIsIdempotent(t *testing.T) {
	s := map[string]any{"permission": map[string]any{"bash": map[string]any{"rm *": "deny"}}}
	if !addBashPermission(s) {
		t.Fatal("expected a change")
	}
	bash := s["permission"].(map[string]any)["bash"].(map[string]any)
	if bash["rm *"] != "deny" || bash[bashPattern] != "allow" {
		t.Errorf("existing rule lost or allow not added: %v", bash)
	}
	if addBashPermission(s) {
		t.Error("second call should be a no-op")
	}
}

// install then uninstall must return the settings to their original shape.
func TestAddRemovePluginRoundTrips(t *testing.T) {
	const p = "/abs/plugin/agent-chat.js"
	s := map[string]any{}
	if addPlugin(s, p) == 0 {
		t.Fatal("expected add")
	}
	if addPlugin(s, p) != 0 {
		t.Error("duplicate add must be a no-op")
	}
	if removePlugin(s, p) == 0 {
		t.Fatal("expected remove")
	}
	if !reflect.DeepEqual(s, map[string]any{}) {
		t.Errorf("settings not restored after remove: %v", s)
	}
}

// removePlugin must keep unrelated plugin entries.
func TestRemovePluginKeepsOthers(t *testing.T) {
	const p = "/abs/plugin/agent-chat.js"
	s := map[string]any{"plugin": []any{"@vendor/other", p}}
	removePlugin(s, p)
	got := s["plugin"].([]any)
	if len(got) != 1 || got[0] != "@vendor/other" {
		t.Errorf("unrelated plugin not preserved: %v", got)
	}
}
