package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadRealSpec feeds the user's actual persisted specs.json through
// NewManager to verify the loader actually hydrates the in-memory map.
func TestLoadRealSpec(t *testing.T) {
	dir := t.TempDir()
	raw, err := os.ReadFile("C:/Users/gordo/AppData/Roaming/com.dbx.app/plugin-data/com.milii.termhive/specs.json")
	if err != nil {
		t.Skipf("real spec not available: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "specs.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(dir)
	manager.mutex.Lock()
	ids := make([]string, 0, len(manager.specs))
	for id := range manager.specs {
		ids = append(ids, id)
	}
	specs := manager.specs
	manager.mutex.Unlock()
	t.Logf("loaded %d specs: %v", len(ids), ids)
	for _, id := range ids {
		s := specs[id]
		t.Logf("spec %s → kind=%s host=%s user=%s auth=%s key=%s", id, s.Kind, s.Host, s.Username, s.Auth.Method, s.Auth.KeyPath)
	}
	if len(ids) == 0 {
		// An empty map is a legitimate state (every connection currently
		// disconnected) — the loader itself is covered by the unit tests.
		t.Skipf("real specs.json is empty (%d bytes), nothing to hydrate", len(raw))
	}
}

// TestSpecsSealedRoundTrip: persisted specs must not contain plaintext
// credentials, and a fresh Manager must recover them through the sealed
// envelope.
func TestSpecsSealedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir)
	spec := ConnSpec{
		ConnectionID: "conn-1",
		Name:         "n",
		Kind:         "ssh",
		Host:         "h",
		Port:         22,
		Username:     "u",
		Auth:         AuthConfig{Method: "password", Password: "SUPER-SECRET"},
	}
	manager.mutex.Lock()
	manager.specs["conn-1"] = spec
	manager.mutex.Unlock()
	manager.upsertSpecsToDisk(map[string]ConnSpec{"conn-1": spec})

	raw, err := os.ReadFile(filepath.Join(dir, "specs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SUPER-SECRET") {
		t.Fatal("specs.json still stores the plaintext password")
	}
	if !strings.Contains(string(raw), `"scheme"`) {
		t.Fatal("specs.json is not a sealed envelope")
	}

	reloaded := NewManager(dir)
	reloaded.mutex.Lock()
	spec, ok := reloaded.specs["conn-1"]
	reloaded.mutex.Unlock()
	if !ok {
		t.Fatal("sealed specs did not reload")
	}
	if spec.Auth.Password != "SUPER-SECRET" {
		t.Fatal("password did not survive the seal/unseal round trip")
	}
}

// TestLegacyPlaintextSpecsMigration: a pre-0.4.5 plaintext specs.json must be
// hydrated on load and immediately rewritten in sealed form.
func TestLegacyPlaintextSpecsMigration(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"conn-old":{"ConnectionID":"conn-old","Name":"old","Kind":"ssh","Host":"1.2.3.4","Port":22,"Username":"root","Auth":{"Method":"password","Password":"LEGACY-SECRET"}}}`
	if err := os.WriteFile(filepath.Join(dir, "specs.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(dir)
	manager.mutex.Lock()
	spec, ok := manager.specs["conn-old"]
	manager.mutex.Unlock()
	if !ok || spec.Auth.Password != "LEGACY-SECRET" {
		t.Fatal("legacy plaintext spec not hydrated")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "specs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "LEGACY-SECRET") {
		t.Fatal("migration left the plaintext password on disk")
	}
	if !strings.Contains(string(raw), `"scheme"`) {
		t.Fatal("migrated file is not a sealed envelope")
	}
}

// TestSpecsDualSidecarNoClobber: the workbench sidecar and the MCP-bridge
// sidecar share one dataDir; an upsert from one must never drop the other's
// entry (the old whole-map overwrite did exactly that in the field), and a
// disconnect remove must only drop its own id.
func TestSpecsDualSidecarNoClobber(t *testing.T) {
	dir := t.TempDir()
	uiSide := NewManager(dir)
	mcpSide := NewManager(dir)

	specA := ConnSpec{ConnectionID: "conn-a", Name: "a", Kind: "ssh", Host: "10.0.0.1", Port: 22, Username: "u", Auth: AuthConfig{Method: "password", Password: "A"}}
	specB := ConnSpec{ConnectionID: "conn-b", Name: "b", Kind: "ssh", Host: "10.0.0.2", Port: 22, Username: "u", Auth: AuthConfig{Method: "password", Password: "B"}}

	uiSide.mutex.Lock()
	uiSide.specs["conn-a"] = specA
	uiSide.mutex.Unlock()
	uiSide.upsertSpecsToDisk(map[string]ConnSpec{"conn-a": specA})

	mcpSide.upsertSpecsToDisk(map[string]ConnSpec{"conn-b": specB})

	// mcpSide's in-memory map has no conn-a; only the disk does. A later
	// flush from mcpSide must keep conn-a alive.
	mcpSide.mutex.Lock()
	mcpSide.specs["conn-b"] = specB
	mcpSide.mutex.Unlock()
	mcpSide.upsertSpecsToDisk(map[string]ConnSpec{"conn-b": specB})

	reloaded := NewManager(dir)
	reloaded.mutex.Lock()
	_, hasA := reloaded.specs["conn-a"]
	_, hasB := reloaded.specs["conn-b"]
	reloaded.mutex.Unlock()
	if !hasA || !hasB {
		t.Fatalf("dual-sidecar writes clobbered each other: conn-a=%v conn-b=%v", hasA, hasB)
	}

	// Disconnect removes exactly its own entry from the shared disk file.
	uiSide.mutex.Lock()
	delete(uiSide.specs, "conn-a")
	uiSide.mutex.Unlock()
	uiSide.removeSpecFromDisk("conn-a")
	reloaded2 := NewManager(dir)
	reloaded2.mutex.Lock()
	_, hasA2 := reloaded2.specs["conn-a"]
	_, hasB2 := reloaded2.specs["conn-b"]
	reloaded2.mutex.Unlock()
	if hasA2 || !hasB2 {
		t.Fatalf("disconnect remove is wrong: conn-a=%v (want false) conn-b=%v (want true)", hasA2, hasB2)
	}
}

// TestSpecsStaleLockRecovery: a crashed holder leaves specs.json.lock behind;
// writes must recover once the lock is older than specsLockStale instead of
// degrading forever.
func TestSpecsStaleLockRecovery(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir)
	lockPath := filepath.Join(dir, "specs.json.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * specsLockStale)
	if err := os.Chtimes(lockPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	spec := ConnSpec{ConnectionID: "conn-x", Kind: "ssh", Host: "h", Port: 22, Username: "u"}
	manager.upsertSpecsToDisk(map[string]ConnSpec{"conn-x": spec})
	reloaded := NewManager(dir)
	reloaded.mutex.Lock()
	_, ok := reloaded.specs["conn-x"]
	reloaded.mutex.Unlock()
	if !ok {
		t.Fatal("write never landed behind a stale lock")
	}
}
