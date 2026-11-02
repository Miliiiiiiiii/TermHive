package main

import (
	"errors"
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
