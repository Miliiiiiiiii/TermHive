package main

// Spec-cache regressions for the rc.21 reconnect fixes:
//   A. a caller-supplied dial recipe (MCP bridge lifecycle, workbench
//      connect) is cached in memory and on disk once proven dialable, so
//      id-only recovery paths (ensure, the reconnect supervisor) can redial
//      after the link drops;
//   B. recipe resolution is exact-by-id — an unknown id must fail with
//      errNoConnSpec instead of silently dialing whichever single server
//      happens to be cached (wrong-host);
//   supervisor: it redials with the dead connection's own spec and treats a
//   missing recipe as a permanent dead end (no 20-attempt spin).

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// TestConnCachesCallerSuppliedSpec: dialing with a caller-supplied spec must
// publish the recipe to the in-memory cache and the shared on-disk cache.
// Pre-rc.21 only ConnectConnection cached, so connections the MCP bridge
// dialed (mcp/call with a lifecycle payload) left no recipe behind — their
// auto-reconnect supervisor spun on 连接尚未建立 forever (field log
// 2026-11-09 21:18: six supervisors × 20 attempts, all instant failures).
func TestConnCachesCallerSuppliedSpec(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir)
	var calls atomic.Int32
	manager.dialConn = func(spec ConnSpec, emitter *dbxpluginsdk.Emitter, hostKeys *HostKeyStore) (*Conn, error) {
		calls.Add(1)
		return fakeConn(spec), nil
	}

	spec := ConnSpec{ConnectionID: "mcp-1", Kind: "ssh", Host: "10.20.147.68", Port: 22, Username: "root"}
	if _, err := manager.conn("mcp-1", nil, spec); err != nil {
		t.Fatalf("known-spec dial failed: %v", err)
	}

	manager.mutex.Lock()
	cached, ok := manager.specs["mcp-1"]
	manager.mutex.Unlock()
	if !ok || cached.Host != spec.Host {
		t.Fatalf("dialable caller spec must be cached in memory, got %+v (ok=%v)", cached, ok)
	}
	disk, _, ok := manager.readSpecsFile()
	if !ok || disk["mcp-1"].Host != spec.Host {
		t.Fatalf("dialable caller spec must be flushed to disk for the other sidecar, disk=%v ok=%v", disk, ok)
	}
}

// TestKnownSpecCachedEnablesIdOnlyRedial is the end-to-end healing the field
// log demanded: an MCP-bridged connection drops, and the id-only redial
// (what the reconnect supervisor and connection/ensure do) must succeed
// against the recipe cached by fix A.
func TestKnownSpecCachedEnablesIdOnlyRedial(t *testing.T) {
	manager := NewManager(t.TempDir())
	var calls atomic.Int32
	manager.dialConn = func(spec ConnSpec, emitter *dbxpluginsdk.Emitter, hostKeys *HostKeyStore) (*Conn, error) {
		calls.Add(1)
		return fakeConn(spec), nil
	}

	spec := ConnSpec{ConnectionID: "mcp-1", Kind: "ssh", Host: "h1", Port: 22, Username: "u"}
	first, err := manager.conn("mcp-1", nil, spec)
	if err != nil {
		t.Fatalf("initial dial failed: %v", err)
	}
	first.markDead() // network drop

	second, err := manager.conn("mcp-1", nil) // id-only: supervisor/ensure path
	if err != nil {
		t.Fatalf("id-only redial must use the cached recipe: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected a fresh dial for the redial, dial calls = %d", got)
	}
	if second.Spec.Host != spec.Host {
		t.Fatalf("redial used the wrong recipe: %+v", second.Spec)
	}
}

// TestEnsureUnknownIdNeverDialsOtherServer: with exactly one other
// connection cached, ensure() for an unknown id must fail with the
// dedicated code and dial nothing. Pre-rc.21 the single-spec fallback would
// have silently connected to that other server and handed its shell back as
// the requested connection.
func TestEnsureUnknownIdNeverDialsOtherServer(t *testing.T) {
	manager := NewManager(t.TempDir())
	var calls atomic.Int32
	manager.dialConn = func(spec ConnSpec, emitter *dbxpluginsdk.Emitter, hostKeys *HostKeyStore) (*Conn, error) {
		calls.Add(1)
		return fakeConn(spec), nil
	}
	manager.specs["other-1"] = ConnSpec{ConnectionID: "other-1", Kind: "ssh", Host: "other", Port: 22, Username: "u"}

	_, pluginError := manager.EnsureConnection("unknown-1", nil)
	if pluginError == nil {
		t.Fatal("ensure for an unknown id must fail")
	}
	if pluginError.Code != errCodeNoConnSpec {
		t.Fatalf("unknown id must map to errCodeNoConnSpec (%d), got %d: %s", errCodeNoConnSpec, pluginError.Code, pluginError.Message)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("ensure must not dial the unrelated cached server, dial calls = %d", got)
	}
}

// TestSupervisorRedialsFromDeadConnSpec: the supervisor redials with the
// dead connection's own recipe — an id whose spec was never cached anywhere
// (the pre-rc.21 MCP case) still recovers instead of spinning.
func TestSupervisorRedialsFromDeadConnSpec(t *testing.T) {
	origBackoffs := reconnectBackoffs
	reconnectBackoffs = []time.Duration{time.Millisecond}
	defer func() { reconnectBackoffs = origBackoffs }()

	manager := NewManager(t.TempDir())
	var calls atomic.Int32
	var dialed atomic.Value // ConnSpec
	manager.dialConn = func(spec ConnSpec, emitter *dbxpluginsdk.Emitter, hostKeys *HostKeyStore) (*Conn, error) {
		calls.Add(1)
		dialed.Store(spec)
		return fakeConn(spec), nil
	}

	spec := ConnSpec{ConnectionID: "c1", Kind: "ssh", Host: "h1", Port: 22, Username: "u"}
	conn := &Conn{Spec: spec, done: make(chan struct{})}
	conn.markDead()
	manager.conns["c1"] = conn
	// deliberately NOT cached in manager.specs — the supervisor must not care

	manager.reconnects["c1"] = make(chan struct{})
	manager.superviseReconnect("c1", make(chan struct{}))

	if got := calls.Load(); got != 1 {
		t.Fatalf("supervisor must redial exactly once, dial calls = %d", got)
	}
	if dialed.Load().(ConnSpec).Host != spec.Host {
		t.Fatalf("redial must use the dead conn's own spec, got %+v", dialed.Load())
	}
	if _, running := manager.reconnects["c1"]; running {
		t.Fatal("supervisor must clear its registry entry after a successful redial")
	}
}

// TestSupervisorNoSpecIsPermanentDeadEnd: a redial that fails because no
// recipe exists is permanent — the supervisor must stop after one attempt
// (not burn the whole attempt cap) and mark the failure deadEnd for the UI.
func TestSupervisorNoSpecIsPermanentDeadEnd(t *testing.T) {
	origBackoffs := reconnectBackoffs
	reconnectBackoffs = []time.Duration{time.Millisecond}
	defer func() { reconnectBackoffs = origBackoffs }()

	manager := NewManager(t.TempDir())
	var calls atomic.Int32
	manager.dialConn = func(spec ConnSpec, emitter *dbxpluginsdk.Emitter, hostKeys *HostKeyStore) (*Conn, error) {
		calls.Add(1)
		return nil, fmt.Errorf("connect %s:22: %w", spec.Host, errNoConnSpec)
	}

	spec := ConnSpec{ConnectionID: "c1", Kind: "ssh", Host: "h1", Port: 22, Username: "u"}
	conn := &Conn{Spec: spec, done: make(chan struct{})}
	conn.markDead()
	manager.conns["c1"] = conn
	manager.SetSettings(map[string]any{"autoReconnectMaxAttempts": 20.0})

	manager.reconnects["c1"] = make(chan struct{})
	manager.superviseReconnect("c1", make(chan struct{}))

	if got := calls.Load(); got != 1 {
		t.Fatalf("missing recipe is permanent: 1 attempt expected, got %d", got)
	}
	if _, running := manager.reconnects["c1"]; running {
		t.Fatal("supervisor must clear its registry entry on a permanent failure")
	}
}

func TestNoConnSpecIsPermanentFailure(t *testing.T) {
	if !isPermanentConnectFailure(errNoConnSpec) {
		t.Fatal("errNoConnSpec must be permanent")
	}
	if !isPermanentConnectFailure(fmt.Errorf("wrapped: %w", errNoConnSpec)) {
		t.Fatal("wrapped errNoConnSpec must be permanent")
	}
	if !errors.Is(errNoConnSpec, errNoConnSpec) {
		t.Fatal("sentinel must satisfy errors.Is on itself")
	}
}
