package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

var errFakeDial = errors.New("fake dial failure")

func fakeConn(spec ConnSpec) *Conn {
	return &Conn{Spec: spec, done: make(chan struct{})}
}

// TestConnSingleFlight: concurrent first calls for one connection id must
// share a single dial — pre-single-flight they raced DialConn and closed each
// other's client via the old-entry swap ("unexpected packet in response to
// channel open").
func TestConnSingleFlight(t *testing.T) {
	manager := NewManager(t.TempDir())
	release := make(chan struct{})
	var calls atomic.Int32
	manager.dialConn = func(spec ConnSpec, emitter *dbxpluginsdk.Emitter, hostKeys *HostKeyStore) (*Conn, error) {
		calls.Add(1)
		<-release
		return fakeConn(spec), nil
	}

	const n = 8
	var wg sync.WaitGroup
	results := make(chan *Conn, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := manager.conn("c1", nil, ConnSpec{ConnectionID: "c1", Kind: "ssh", Host: "h", Port: 22, Username: "u"})
			if err != nil {
				t.Error(err)
				return
			}
			results <- conn
		}()
	}
	time.Sleep(150 * time.Millisecond) // let every caller pile onto one flight
	close(release)
	wg.Wait()
	close(results)

	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent conn() dialed %d times, want exactly 1", got)
	}
	seen := map[*Conn]bool{}
	for conn := range results {
		seen[conn] = true
	}
	if len(seen) != 1 {
		t.Fatalf("callers got %d distinct conns, want 1 shared", len(seen))
	}
}

// TestConnFlightErrorShared: a failed dial must reach every waiter, and a
// retry afterwards must be able to dial fresh (flight cleaned up).
func TestConnFlightErrorShared(t *testing.T) {
	manager := NewManager(t.TempDir())
	release := make(chan struct{})
	var calls atomic.Int32
	fail := atomic.Bool{}
	fail.Store(true)
	manager.dialConn = func(spec ConnSpec, emitter *dbxpluginsdk.Emitter, hostKeys *HostKeyStore) (*Conn, error) {
		if calls.Add(1) == 1 && fail.Load() {
			// Hold the failing dial open until every waiter piles onto the flight.
			<-release
			return nil, errFakeDial
		}
		return fakeConn(spec), nil
	}

	spec := ConnSpec{ConnectionID: "c1", Kind: "ssh", Host: "h", Port: 22, Username: "u"}
	const n = 4
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := manager.conn("c1", nil, spec); err == nil {
				t.Error("waiter did not inherit the dial error")
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("failed dial ran %d times, want 1", got)
	}
	conn, err := manager.conn("c1", nil, spec)
	if err != nil || conn == nil {
		t.Fatalf("retry after failed flight did not dial fresh: %v", err)
	}
}

// TestConnFlightEndpointMismatch: a caller whose known spec targets a
// different server than the in-flight dial waits one round, then dials its
// own endpoint — the result must match its spec, not the leader's.
func TestConnFlightEndpointMismatch(t *testing.T) {
	manager := NewManager(t.TempDir())
	release := make(chan struct{})
	var calls atomic.Int32
	manager.dialConn = func(spec ConnSpec, emitter *dbxpluginsdk.Emitter, hostKeys *HostKeyStore) (*Conn, error) {
		calls.Add(1)
		if spec.Host == "slow" {
			<-release
		}
		return fakeConn(spec), nil
	}

	specSlow := ConnSpec{ConnectionID: "c1", Kind: "ssh", Host: "slow", Port: 22, Username: "u"}
	specFast := ConnSpec{ConnectionID: "c1", Kind: "ssh", Host: "fast", Port: 22, Username: "u"}

	go func() {
		if _, err := manager.conn("c1", nil, specSlow); err != nil {
			t.Error(err)
		}
	}()
	time.Sleep(100 * time.Millisecond) // slow dial now in flight

	fastDone := make(chan *Conn, 1)
	go func() {
		conn, err := manager.conn("c1", nil, specFast)
		if err != nil {
			t.Error(err)
			fastDone <- nil
			return
		}
		fastDone <- conn
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)

	select {
	case conn := <-fastDone:
		if conn == nil || conn.Spec.Host != "fast" {
			t.Fatalf("cross-endpoint caller got %v, want the fast endpoint", conn)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("dialed %d times, want 2 (one per endpoint)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cross-endpoint caller never returned")
	}
}

// TestShellIntegrationDisabledSetting: the escape-hatch toggle defaults to
// off, honors true, and a null patch (delete) restores the default.
func TestShellIntegrationDisabledSetting(t *testing.T) {
	manager := NewManager(t.TempDir())
	if manager.shellIntegrationDisabled() {
		t.Fatal("shell integration must be on by default")
	}
	manager.SetSettings(map[string]any{"disableShellIntegration": true})
	if !manager.shellIntegrationDisabled() {
		t.Fatal("disableShellIntegration=true not honored")
	}
	manager.SetSettings(map[string]any{"disableShellIntegration": nil})
	if manager.shellIntegrationDisabled() {
		t.Fatal("null delete must restore the default")
	}
}
