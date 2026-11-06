package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// workspace.json remembers the last session layout (which connections had
// how many sessions, tab titles, groups) so a restarted workbench can offer
// one-click restoration. The blob is opaque to the backend — the UI owns the
// schema ({v, savedAt, tabs:[{connectionId,title}], groups:[{name,members}]}).
// Not sensitive; still written 0600 + atomic like every other file we own.
func (manager *Manager) workspacePath() string {
	return filepath.Join(manager.dataDir, "workspace.json")
}

// GetWorkspace returns the saved layout; an empty map before the first write.
func (manager *Manager) GetWorkspace() map[string]any {
	if manager.dataDir == "" {
		return map[string]any{}
	}
	raw, err := os.ReadFile(manager.workspacePath())
	if err != nil {
		return map[string]any{}
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil || payload == nil {
		return map[string]any{}
	}
	return payload
}

// SetWorkspace persists the layout atomically.
func (manager *Manager) SetWorkspace(payload map[string]any) {
	if manager.dataDir == "" {
		return
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	temp := manager.workspacePath() + ".tmp"
	if os.WriteFile(temp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(temp, manager.workspacePath())
}
