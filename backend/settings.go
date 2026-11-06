package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// settingsPath is <dataDir>/settings.json — a small JSON blob the UI uses for
// plugin-wide preferences (terminal scheme, log-highlight rules). Not
// sensitive; still written 0600 + atomic like every other file we own.
func (manager *Manager) settingsPath() string {
	return filepath.Join(manager.dataDir, "settings.json")
}

func (manager *Manager) loadSettings() {
	if manager.dataDir == "" {
		return
	}
	raw, err := os.ReadFile(manager.settingsPath())
	if err != nil {
		return
	}
	var settings map[string]any
	if json.Unmarshal(raw, &settings) == nil && settings != nil {
		manager.settings = settings
	}
}

// GetSettings returns the stored preferences (an empty map before the first
// write — the UI falls back to its defaults in that case).
func (manager *Manager) GetSettings() map[string]any {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if manager.settings == nil {
		return map[string]any{}
	}
	return manager.settings
}

// SetSettings merges a patch into the stored blob (a null value deletes the
// key), persists the result atomically and broadcasts settings.changed so
// every open surface (other connection tabs, the mini window, the launcher
// home) re-applies live: the config is plugin-wide, not per-session. Merge —
// not replace — because different UI surfaces own different keys (style keys
// vs behavior toggles like autoReconnect/sessionLog); a replace here would
// let each surface's save silently wipe the others' settings.
func (manager *Manager) SetSettings(patch map[string]any) {
	manager.mutex.Lock()
	merged := make(map[string]any, len(manager.settings)+len(patch))
	for key, value := range manager.settings {
		merged[key] = value
	}
	for key, value := range patch {
		if value == nil {
			delete(merged, key)
		} else {
			merged[key] = value
		}
	}
	manager.settings = merged
	manager.mutex.Unlock()
	if manager.dataDir != "" {
		if raw, err := json.MarshalIndent(merged, "", "  "); err == nil {
			temp := manager.settingsPath() + ".tmp"
			if os.WriteFile(temp, raw, 0o600) == nil {
				_ = os.Rename(temp, manager.settingsPath())
			}
		}
	}
	manager.emit("settings.changed", map[string]any{"settings": merged})
	// behavior keys the backend itself reacts to (session-log sinks attach
	// or detach on live shells; the retention pass honors new limits at once)
	manager.applySessionLogSetting()
	go manager.pruneSessionLogs()
}
