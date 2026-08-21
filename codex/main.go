// Standalone installer that wires agent-chat into OpenAI's Codex CLI: it
// merges SessionStart/SessionEnd hook entries into $CODEX_HOME/hooks.json
// (default ~/.codex/hooks.json) and adds ~/.agent-chat to the sandbox's
// writable roots in config.toml (or removes both with --uninstall).
// Idempotent, and keeps a one-shot .bak of each pre-install file.
//
// hooks.json uses Codex's Claude-compatible hooks schema (stable since Codex
// CLI 0.124). The config.toml edit is marker-delimited and only appended when
// the file has no [sandbox_workspace_write] section of its own; otherwise the
// snippet is printed for the user to merge by hand — this installer never
// rewrites TOML it does not own.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const binaryRelPath = ".claude/agent-chat/agent-chat"

const (
	tomlMarkerBegin = "# >>> agent-chat >>>"
	tomlMarkerEnd   = "# <<< agent-chat <<<"
)

func main() {
	uninstall := flag.Bool("uninstall", false, "remove agent-chat entries from hooks.json and config.toml")
	flag.Parse()

	home, err := os.UserHomeDir()
	if err != nil {
		die("cannot determine home: %v", err)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	hooksPath := filepath.Join(codexHome, "hooks.json")
	configPath := filepath.Join(codexHome, "config.toml")
	binPath := filepath.Join(home, binaryRelPath)
	chatHome := os.Getenv("AGENT_CHAT_HOME")
	if chatHome == "" {
		chatHome = filepath.Join(home, ".agent-chat")
	}

	hooks, existed, err := loadJSON(hooksPath)
	if err != nil {
		die("read %s: %v", hooksPath, err)
	}

	if *uninstall {
		changedStart := removeHook(hooks, "SessionStart", hookCommand(binPath, "hook-start --emit codex"))
		changedEnd := removeHook(hooks, "SessionEnd", hookCommand(binPath, "hook-stop"))
		if changedStart || changedEnd {
			if err := writeJSON(hooksPath, hooks); err != nil {
				die("write %s: %v", hooksPath, err)
			}
			fmt.Printf("removed agent-chat hook entries from %s\n", hooksPath)
		} else if existed {
			fmt.Println("hooks.json contained no agent-chat entries; nothing to do")
		}
		if removed, err := removeTomlBlock(configPath); err != nil {
			die("update %s: %v", configPath, err)
		} else if removed {
			fmt.Printf("removed agent-chat block from %s\n", configPath)
		}
		return
	}

	if existed {
		backupOnce(hooksPath)
	}
	changed := addHook(hooks, "SessionStart", hookCommand(binPath, "hook-start --emit codex"), 0)
	// SessionEnd hooks get very little time in Codex (1s default, 3s max);
	// hook-stop is a couple of file ops, but claim the maximum anyway.
	if addHook(hooks, "SessionEnd", hookCommand(binPath, "hook-stop"), 3) {
		changed = true
	}
	if changed {
		if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
			die("mkdir: %v", err)
		}
		if err := writeJSON(hooksPath, hooks); err != nil {
			die("write %s: %v", hooksPath, err)
		}
		fmt.Printf("merged hook entries into %s\n", hooksPath)
	} else {
		fmt.Println("hooks.json already contains agent-chat entries; nothing to do")
	}

	added, err := addTomlBlock(configPath, chatHome)
	if err != nil {
		die("update %s: %v", configPath, err)
	}
	switch added {
	case tomlAdded:
		fmt.Printf("added %s to sandbox writable_roots in %s\n", chatHome, configPath)
	case tomlPresent:
		// Already ours; quiet.
	case tomlManual:
		fmt.Printf("config.toml already has a [sandbox_workspace_write] section — merge this yourself:\n")
		fmt.Printf("  writable_roots += [%q]\n", chatHome)
	}

	fmt.Println("\nDone. Notes:")
	fmt.Println("  - Requires Codex CLI >= 0.124 (hooks); live message delivery needs >= 0.149 (`codex queue`).")
	fmt.Println("  - Codex asks you to review/approve new command hooks once on next start — approve both.")
	fmt.Println("  - Restart any open Codex sessions; the wiring takes effect on the next session.")
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "codex-installer: "+format+"\n", args...)
	os.Exit(1)
}

func backupOnce(path string) {
	bak := path + ".bak"
	if _, err := os.Stat(bak); !os.IsNotExist(err) {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := os.WriteFile(bak, data, 0o644); err != nil {
		die("write backup: %v", err)
	}
	fmt.Printf("backed up %s -> %s\n", path, bak)
}

func loadJSON(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, true, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, true, fmt.Errorf("parse: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, true, nil
}

func writeJSON(path string, m map[string]any) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

func hookCommand(binPath, sub string) string {
	return binPath + " " + sub
}

// addHook appends a Codex hooks.json entry for event, creating intermediate
// maps/arrays as needed. timeoutSec > 0 sets an explicit per-hook timeout.
// Returns false if an entry with this exact command already exists.
func addHook(root map[string]any, event, command string, timeoutSec int) bool {
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	entries, _ := hooks[event].([]any)
	if hookExists(entries, command) {
		return false
	}
	h := map[string]any{"type": "command", "command": command}
	if timeoutSec > 0 {
		h["timeout"] = timeoutSec
	}
	hooks[event] = append(entries, map[string]any{"hooks": []any{h}})
	return true
}

func removeHook(root map[string]any, event, command string) bool {
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		return false
	}
	entries, _ := hooks[event].([]any)
	if len(entries) == 0 {
		return false
	}
	kept := entries[:0]
	removed := false
	for _, e := range entries {
		if hookEntryHasCommand(e, command) {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	if !removed {
		return false
	}
	if len(kept) == 0 {
		delete(hooks, event)
	} else {
		hooks[event] = kept
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	return true
}

func hookExists(entries []any, command string) bool {
	for _, e := range entries {
		if hookEntryHasCommand(e, command) {
			return true
		}
	}
	return false
}

func hookEntryHasCommand(entry any, command string) bool {
	m, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	inner, _ := m["hooks"].([]any)
	for _, h := range inner {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if c, _ := hm["command"].(string); c == command {
			return true
		}
	}
	return false
}

type tomlResult int

const (
	tomlAdded tomlResult = iota
	tomlPresent
	tomlManual
)

// tomlBlock renders the marker-delimited section appended to config.toml.
func tomlBlock(chatHome string) string {
	return fmt.Sprintf(`%s
# Allow agent-chat to write its home from inside the Codex sandbox.
# Managed by the agent-chat installer; do not edit between the markers.
[sandbox_workspace_write]
writable_roots = [%q]
%s
`, tomlMarkerBegin, chatHome, tomlMarkerEnd)
}

// addTomlBlock appends the writable-roots block to config.toml. It refuses to
// touch a file that already declares [sandbox_workspace_write] outside our
// markers — appending a duplicate table would corrupt the TOML — and reports
// tomlManual so the caller can print merge instructions instead.
func addTomlBlock(path, chatHome string) (tomlResult, error) {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return tomlManual, err
	}
	s := string(data)
	if strings.Contains(s, tomlMarkerBegin) {
		return tomlPresent, nil
	}
	if strings.Contains(s, "[sandbox_workspace_write]") {
		return tomlManual, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return tomlManual, err
	}
	if len(s) > 0 {
		backupOnce(path)
	}
	if len(s) > 0 && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	s += "\n" + tomlBlock(chatHome)
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		return tomlManual, err
	}
	return tomlAdded, nil
}

// removeTomlBlock deletes the marker-delimited block, leaving the rest of
// config.toml byte-identical.
func removeTomlBlock(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s := string(data)
	begin := strings.Index(s, tomlMarkerBegin)
	if begin < 0 {
		return false, nil
	}
	end := strings.Index(s, tomlMarkerEnd)
	if end < 0 {
		return false, fmt.Errorf("found %q but no closing %q — remove the block by hand", tomlMarkerBegin, tomlMarkerEnd)
	}
	end += len(tomlMarkerEnd)
	for end < len(s) && s[end] == '\n' {
		end++
	}
	// Also swallow the blank line the installer added before the block.
	for begin > 0 && s[begin-1] == '\n' {
		begin--
	}
	out := s[:begin]
	if len(out) > 0 && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if rest := s[end:]; len(rest) > 0 {
		out += rest
	}
	return true, os.WriteFile(path, []byte(out), 0o644)
}
