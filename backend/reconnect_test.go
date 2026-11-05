package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReconnectBackoff(t *testing.T) {
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 60 * time.Second, 60 * time.Second}
	for attempt, expected := range want {
		if got := reconnectBackoff(attempt + 1); got != expected {
			t.Fatalf("reconnectBackoff(%d) = %v, want %v", attempt+1, got, expected)
		}
	}
}

func TestAutoReconnectEnabledDefault(t *testing.T) {
	manager := NewManager(t.TempDir())
	if !manager.autoReconnectEnabled() {
		t.Fatal("auto-reconnect must default to on (absent key)")
	}
	manager.SetSettings(map[string]any{"autoReconnect": false})
	if manager.autoReconnectEnabled() {
		t.Fatal("explicit false must disable auto-reconnect")
	}
	manager.SetSettings(map[string]any{"autoReconnect": nil}) // null deletes
	if !manager.autoReconnectEnabled() {
		t.Fatal("deleting the key must restore the default")
	}
}

func TestAutoReconnectMaxAttempts(t *testing.T) {
	manager := NewManager(t.TempDir())
	if got := manager.autoReconnectMaxAttempts(); got != defaultReconnectMaxAttempts {
		t.Fatalf("default = %d, want %d", got, defaultReconnectMaxAttempts)
	}
	manager.SetSettings(map[string]any{"autoReconnectMaxAttempts": 3.0})
	if got := manager.autoReconnectMaxAttempts(); got != 3 {
		t.Fatalf("explicit 3 = %d", got)
	}
	manager.SetSettings(map[string]any{"autoReconnectMaxAttempts": -5.0})
	if got := manager.autoReconnectMaxAttempts(); got != 1 {
		t.Fatalf("negative must clamp to 1, got %d", got)
	}
	manager.SetSettings(map[string]any{"autoReconnectMaxAttempts": 9999.0})
	if got := manager.autoReconnectMaxAttempts(); got != 500 {
		t.Fatalf("absurd must clamp to 500, got %d", got)
	}
	manager.SetSettings(map[string]any{"autoReconnectMaxAttempts": nil})
	if got := manager.autoReconnectMaxAttempts(); got != defaultReconnectMaxAttempts {
		t.Fatalf("deleting the key must restore the default, got %d", got)
	}
}

// The give-up flow: an unreachable server must not be retried forever. With
// the attempt cap at 1 and milliseconds of backoff, the supervisor must
// emit connection.failed, clear its registry entry and return.
func TestSuperviseReconnectGivesUpAfterLimit(t *testing.T) {
	origBackoffs := reconnectBackoffs
	reconnectBackoffs = []time.Duration{time.Millisecond}
	defer func() { reconnectBackoffs = origBackoffs }()

	dataDir := t.TempDir()
	manager := NewManager(dataDir)
	manager.SetSettings(map[string]any{"autoReconnectMaxAttempts": 1.0})
	// a spec pointing at a port nothing listens on: refused fast, transient
	manager.specs["give-up-1"] = ConnSpec{ConnectionID: "give-up-1", Host: "127.0.0.1", Port: 1}
	conn := &Conn{Spec: manager.specs["give-up-1"], done: make(chan struct{})}
	conn.markDead() // skip the probe's client round trip
	manager.conns["give-up-1"] = conn

	manager.reconnects["give-up-1"] = make(chan struct{})
	manager.superviseReconnect("give-up-1", make(chan struct{}))

	if _, running := manager.reconnects["give-up-1"]; running {
		t.Fatal("supervisor must clear its registry entry after giving up")
	}
	raw, err := os.ReadFile(filepath.Join(dataDir, "diag.log"))
	if err != nil {
		t.Fatalf("diag.log missing: %v", err)
	}
	if !strings.Contains(string(raw), "自动重连第 1/1 次尝试失败") {
		t.Fatalf("give-up must log the capped attempt, diag: %s", raw)
	}
}

func TestSetSettingsMergesAndDeletes(t *testing.T) {
	manager := NewManager(t.TempDir())
	manager.SetSettings(map[string]any{"scheme": "oneDark", "rules": []any{1, 2}})
	// A style-only save (the most common writer) must not wipe behavior keys.
	manager.SetSettings(map[string]any{"autoReconnect": false, "sessionLog": true, "scheme": "nord"})
	settings := manager.GetSettings()
	if settings["autoReconnect"] != false || settings["sessionLog"] != true {
		t.Fatalf("behavior keys must survive a style save: %v", settings)
	}
	if settings["scheme"] != "nord" {
		t.Fatalf("patch must overwrite same key: %v", settings["scheme"])
	}
	rules, ok := settings["rules"].([]any)
	if !ok || len(rules) != 2 {
		t.Fatalf("untouched keys must persist: %v", settings["rules"])
	}
	// Null deletes.
	manager.SetSettings(map[string]any{"sessionLog": nil})
	if _, present := manager.GetSettings()["sessionLog"]; present {
		t.Fatal("null value must delete the key")
	}
}

func TestIsPermanentConnectFailure(t *testing.T) {
	permanent := []error{
		errors.New("认证失败: 用户名或密码/私钥不被服务器接受 (unable to authenticate)"),
		errors.New("安全警报: 服务器 x 的主机密钥已变更!"),
		errors.New("ssh: handshake failed: unable to authenticate"),
	}
	for _, err := range permanent {
		if !isPermanentConnectFailure(err) {
			t.Fatalf("expected permanent: %v", err)
		}
	}
	transient := []error{
		errors.New("dial 10.0.0.1:22: i/o timeout"),
		errors.New("connect 10.0.0.1:22: connection refused"),
		errors.New("read tcp: connection reset by peer"),
	}
	for _, err := range transient {
		if isPermanentConnectFailure(err) {
			t.Fatalf("expected transient: %v", err)
		}
	}
}

func TestNotifyConnectionTroubleDedupes(t *testing.T) {
	manager := NewManager(t.TempDir())
	// No conn registered → the supervisor exits on its first gate; dedupe is
	// what we assert here: repeated notices reuse the registry entry.
	manager.reconnects["conn-a"] = make(chan struct{})
	before := manager.reconnects["conn-a"]
	manager.notifyConnectionTrouble("conn-a")
	after := manager.reconnects["conn-a"]
	if after != before {
		t.Fatal("a running supervisor must absorb further trouble notices")
	}
}
