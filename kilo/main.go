// Standalone installer that wires agent-chat into the kilo CLI: it drops the
// bundled plugin into the kilo config dir and merges a `plugin` entry plus an
// `agent-chat *` bash-permission allow into kilo.jsonc (or removes them with
// --uninstall). Idempotent, and keeps a one-shot .bak of the pre-install file.
//
// kilo.jsonc is parsed as JSON; // and /* */ comments are stripped first.
// Re-serialisation drops comments and reformats — the .bak preserves the
// original. Trailing commas are not supported; clean those by hand if present.
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed plugin/agent-chat.js
var pluginJS []byte

const bashPattern = "agent-chat *"

func main() {
	uninstall := flag.Bool("uninstall", false, "remove agent-chat entries from kilo.jsonc and delete the plugin")
	flag.Parse()

	cfgDir := kiloConfigDir()
	settingsPath := filepath.Join(cfgDir, "kilo.jsonc")
	pluginPath := filepath.Join(cfgDir, "plugin", "agent-chat.js")

	settings, existed, err := loadSettings(settingsPath)
	if err != nil {
		die("read %s: %v", settingsPath, err)
	}

	if *uninstall {
		if existed {
			changed := removePlugin(settings, pluginPath) | boolToInt(removeBashPermission(settings))
			if changed != 0 {
				if err := writeSettings(settingsPath, settings); err != nil {
					die("write %s: %v", settingsPath, err)
				}
				fmt.Printf("removed agent-chat entries from %s\n", settingsPath)
			} else {
				fmt.Println("kilo.jsonc contained no agent-chat entries; nothing to do")
			}
		}
		if err := os.Remove(pluginPath); err == nil {
			fmt.Printf("removed %s\n", pluginPath)
		}
		return
	}

	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o755); err != nil {
		die("mkdir: %v", err)
	}
	if err := os.WriteFile(pluginPath, pluginJS, 0o644); err != nil {
		die("write plugin: %v", err)
	}
	fmt.Printf("installed plugin -> %s\n", pluginPath)

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

	changed := addPlugin(settings, pluginPath) | boolToInt(addBashPermission(settings))
	if changed == 0 {
		fmt.Println("kilo.jsonc already contains agent-chat entries; nothing to do")
		return
	}
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		die("mkdir: %v", err)
	}
	if err := writeSettings(settingsPath, settings); err != nil {
		die("write %s: %v", settingsPath, err)
	}
	fmt.Printf("merged plugin + permission entries into %s\n", settingsPath)
	fmt.Println("restart any open kilo sessions for the plugin to load.")
}

func kiloConfigDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "kilo")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		die("cannot determine home: %v", err)
	}
	return filepath.Join(home, ".config", "kilo")
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "kilo-installer: "+format+"\n", args...)
	os.Exit(1)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func loadSettings(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 {
		return map[string]any{}, true, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		// Retry with comments stripped (kilo.jsonc may carry // and /* */).
		if err2 := json.Unmarshal(stripJSONC(data), &m); err2 != nil {
			return nil, true, fmt.Errorf("parse (after stripping comments): %w", err2)
		}
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
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

func addPlugin(settings map[string]any, pluginPath string) int {
	arr, _ := settings["plugin"].([]any)
	for _, p := range arr {
		if s, _ := p.(string); s == pluginPath {
			return 0
		}
	}
	settings["plugin"] = append(arr, pluginPath)
	return 1
}

func removePlugin(settings map[string]any, pluginPath string) int {
	arr, _ := settings["plugin"].([]any)
	if len(arr) == 0 {
		return 0
	}
	kept := arr[:0]
	removed := 0
	for _, p := range arr {
		if s, _ := p.(string); s == pluginPath {
			removed = 1
			continue
		}
		kept = append(kept, p)
	}
	if removed == 0 {
		return 0
	}
	if len(kept) == 0 {
		delete(settings, "plugin")
	} else {
		settings["plugin"] = kept
	}
	return 1
}

// addBashPermission allows `agent-chat *` without prompting. It only ever adds
// a narrow allow rule: if bash is already a blanket string policy it is left
// alone (agent-chat is already covered, and we must not downgrade it).
func addBashPermission(settings map[string]any) bool {
	perms, _ := settings["permission"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
		settings["permission"] = perms
	}
	switch b := perms["bash"].(type) {
	case string:
		return false
	case map[string]any:
		if _, ok := b[bashPattern]; ok {
			return false
		}
		b[bashPattern] = "allow"
		return true
	default:
		perms["bash"] = map[string]any{bashPattern: "allow"}
		return true
	}
}

func removeBashPermission(settings map[string]any) bool {
	perms, _ := settings["permission"].(map[string]any)
	if perms == nil {
		return false
	}
	b, ok := perms["bash"].(map[string]any)
	if !ok {
		return false
	}
	if _, present := b[bashPattern]; !present {
		return false
	}
	delete(b, bashPattern)
	if len(b) == 0 {
		delete(perms, "bash")
	}
	if len(perms) == 0 {
		delete(settings, "permission")
	}
	return true
}

// stripJSONC removes // line and /* */ block comments, skipping anything inside
// a JSON string so URLs like "https://..." survive untouched.
func stripJSONC(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			out = append(out, c)
			continue
		}
		if c == '/' && i+1 < len(b) {
			if b[i+1] == '/' {
				for i < len(b) && b[i] != '\n' {
					i++
				}
				if i < len(b) {
					out = append(out, b[i]) // keep the newline
				}
				continue
			}
			if b[i+1] == '*' {
				i += 2
				for i < len(b) {
					if b[i] == '*' && i+1 < len(b) && b[i+1] == '/' {
						i++ // outer i++ steps past the closing '/'
						break
					}
					i++
				}
				continue
			}
		}
		out = append(out, c)
	}
	return out
}
