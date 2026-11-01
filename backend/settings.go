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

// SetSettings replaces the whole blob, persists it atomically and broadcasts
// settings.changed so every open surface (other connection tabs, the mini
// window, the launcher home) re-applies live: the config is plugin-wide, not
// per-session.
func (manager *Manager) SetSettings(settings map[string]any) {
	manager.mutex.Lock()
	manager.settings = settings
	manager.mutex.Unlock()
	if manager.dataDir != "" {
		if raw, err := json.MarshalIndent(settings, "", "  "); err == nil {
			temp := manager.settingsPath() + ".tmp"
			if os.WriteFile(temp, raw, 0o600) == nil {
				_ = os.Rename(temp, manager.settingsPath())
			}
		}
	}
	manager.emit("settings.changed", map[string]any{"settings": settings})
}
