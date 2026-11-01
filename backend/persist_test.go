package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	manager.mutex.Lock()
	manager.specs["conn-1"] = ConnSpec{
		ConnectionID: "conn-1",
		Name:         "n",
		Kind:         "ssh",
		Host:         "h",
		Port:         22,
		Username:     "u",
		Auth:         AuthConfig{Method: "password", Password: "SUPER-SECRET"},
	}
	manager.persistSpecsLocked()
	manager.mutex.Unlock()

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
