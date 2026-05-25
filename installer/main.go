// Standalone installer that merges agent-chat's hook + permission entries
// into ~/.claude/settings.json (or removes them with --uninstall). Idempotent:
// re-running install never duplicates entries; running uninstall on a clean
// settings.json is a no-op. A one-shot .bak of the pre-install file is kept
// so a human can diff or hand-restore.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const (
	binaryRelPath = ".claude/agent-chat/agent-chat"
	settingsRel   = ".claude/settings.json"
)

func main() {
	uninstall := flag.Bool("uninstall", false, "remove agent-chat entries from settings.json")
	flag.Parse()

	home, err := os.UserHomeDir()
	if err != nil {
		die("cannot determine home: %v", err)
	}
	settingsPath := filepath.Join(home, settingsRel)
	binPath := filepath.Join(home, binaryRelPath)
	// Double slash anchors to filesystem root; single slash would be parsed
	// as project-root-relative. See https://code.claude.com/docs/en/permissions.md
	artifactsGlob := "/" + filepath.Join(home, ".agent-chat", "artifacts", "**")

	settings, existed, err := loadSettings(settingsPath)
	if err != nil {
		die("read %s: %v", settingsPath, err)
	}

	if *uninstall {
		if !existed {
			fmt.Println("no settings.json found; nothing to uninstall")
			return
		}
		changed := removeEntries(settings, binPath, artifactsGlob)
		if !changed {
			fmt.Println("settings.json contained no agent-chat entries; nothing to do")
			return
		}
		if err := writeSettings(settingsPath, settings); err != nil {
			die("write %s: %v", settingsPath, err)
		}
		fmt.Printf("removed agent-chat entries from %s\n", settingsPath)
		return
	}

	if existed {
		bak := settingsPath + ".bak"
		if _, err := os.Stat(bak); os.IsNotExist(err) {
			data, _ := os.ReadFile(settingsPath)
			if err := os.WriteFile(bak, data, 0o644); err != nil {
				die("write backup: %v", err)
			}
			fmt.Printf("backed up %s -> %s\n", settingsPath, bak)
		}
	}

	changed := addEntries(settings, binPath, artifactsGlob)
	if !changed {
		fmt.Println("settings.json already contains agent-chat entries; nothing to do")
		return
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		die("mkdir: %v", err)
	}
	if err := writeSettings(settingsPath, settings); err != nil {
		die("write %s: %v", settingsPath, err)
	}
	fmt.Printf("merged hook + permission entries into %s\n", settingsPath)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "installer: "+format+"\n", args...)
	os.Exit(1)
}

func loadSettings(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var m map[string]any
	if len(data) == 0 {
		return map[string]any{}, true, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, true, fmt.Errorf("parse: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, true, nil
}

func writeSettings(path string, m map[string]any) error {
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	return os.WriteFile(path, out, 0o644)
}

func permissionPatterns(binPath, artifactsGlob string) []string {
	return []string{
		"Bash(agent-chat send *)",
		"Bash(agent-chat share *)",
		"Bash(agent-chat peers)",
		"Bash(agent-chat history *)",
		"Bash(agent-chat reset *)",
		"Bash(agent-chat listen *)",
		fmt.Sprintf("Read(%s)", artifactsGlob),
	}
}

func hookCommand(binPath, sub string) string {
	return binPath + " " + sub
}

func addEntries(settings map[string]any, binPath, artifactsGlob string) bool {
	changed := false
	if addHook(settings, "SessionStart", hookCommand(binPath, "hook-start")) {
		changed = true
	}
	if addHook(settings, "SessionEnd", hookCommand(binPath, "hook-stop")) {
		changed = true
	}
	for _, p := range permissionPatterns(binPath, artifactsGlob) {
		if addPermission(settings, p) {
			changed = true
		}
	}
	return changed
}

func removeEntries(settings map[string]any, binPath, artifactsGlob string) bool {
	changed := false
	if removeHook(settings, "SessionStart", hookCommand(binPath, "hook-start")) {
		changed = true
	}
	if removeHook(settings, "SessionEnd", hookCommand(binPath, "hook-stop")) {
		changed = true
	}
	for _, p := range permissionPatterns(binPath, artifactsGlob) {
		if removePermission(settings, p) {
			changed = true
		}
	}
	return changed
}

// addHook appends a Claude Code hook entry of the form
//   { "hooks": [ { "type": "command", "command": "<command>" } ] }
// to settings.hooks.<event>, creating intermediate maps/arrays as needed.
// Returns false if an entry with this exact command already exists.
func addHook(settings map[string]any, event, command string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	entries, _ := hooks[event].([]any)
	if hookExists(entries, command) {
		return false
	}
	entry := map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": command},
		},
	}
	hooks[event] = append(entries, entry)
	return true
}

func removeHook(settings map[string]any, event, command string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
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
		delete(settings, "hooks")
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

func addPermission(settings map[string]any, pattern string) bool {
	perms, _ := settings["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
		settings["permissions"] = perms
	}
	allow, _ := perms["allow"].([]any)
	for _, p := range allow {
		if s, _ := p.(string); s == pattern {
			return false
		}
	}
	allow = append(allow, pattern)
	sort.SliceStable(allow, func(i, j int) bool {
		si, _ := allow[i].(string)
		sj, _ := allow[j].(string)
		return si < sj
	})
	perms["allow"] = allow
	return true
}

func removePermission(settings map[string]any, pattern string) bool {
	perms, _ := settings["permissions"].(map[string]any)
	if perms == nil {
		return false
	}
	allow, _ := perms["allow"].([]any)
	kept := allow[:0]
	removed := false
	for _, p := range allow {
		if s, _ := p.(string); s == pattern {
			removed = true
			continue
		}
		kept = append(kept, p)
	}
	if !removed {
		return false
	}
	if len(kept) == 0 {
		delete(perms, "allow")
	} else {
		perms["allow"] = kept
	}
	if len(perms) == 0 {
		delete(settings, "permissions")
	}
	return true
}
