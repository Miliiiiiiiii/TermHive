package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeLogFixture(t *testing.T, path string, size int, age time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-age)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}
}

func logPathAt(dataDir, name string) string {
	return filepath.Join(dataDir, "logs", "user@host_22", name)
}

// setRetention writes retention keys directly (no SetSettings) so the test's
// prune calls are the ONLY ones in flight — SetSettings fires an async prune
// whose policy read can race the fixture writes.
func setRetention(manager *Manager, age, totalMB float64) {
	manager.mutex.Lock()
	if manager.settings == nil {
		manager.settings = map[string]any{}
	}
	manager.settings["sessionLogMaxAgeDays"] = age
	manager.settings["sessionLogMaxTotalMB"] = totalMB
	manager.mutex.Unlock()
}

// TestPruneSessionLogsByAge: files older than the retention window die, fresh
// ones survive, and 0 days disables age eviction entirely.
func TestPruneSessionLogsByAge(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir)
	time.Sleep(150 * time.Millisecond) // let the startup prune settle before fixtures land
	setRetention(manager, 7, 0)
	os.MkdirAll(filepath.Dir(logPathAt(dir, "a.log")), 0o700)
	writeLogFixture(t, logPathAt(dir, "old.log"), 1024, 10*24*time.Hour)
	writeLogFixture(t, logPathAt(dir, "new.log"), 1024, time.Hour)

	manager.pruneSessionLogs()
	if _, err := os.Stat(logPathAt(dir, "old.log")); !os.IsNotExist(err) {
		t.Fatal("stale log survived age eviction")
	}
	if _, err := os.Stat(logPathAt(dir, "new.log")); err != nil {
		t.Fatal("fresh log was evicted by age")
	}

	// 0 = keep forever
	setRetention(manager, 0, 0)
	writeLogFixture(t, logPathAt(dir, "older.log"), 1024, 365*24*time.Hour)
	manager.pruneSessionLogs()
	if _, err := os.Stat(logPathAt(dir, "older.log")); err != nil {
		t.Fatalf("age eviction disabled (0) but still deleted: %v", err)
	}
}

// TestPruneSessionLogsByTotal: over budget the oldest die first until the
// total fits; an open (live) sink's file is never touched.
func TestPruneSessionLogsByTotal(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(dir)
	time.Sleep(150 * time.Millisecond) // let the startup prune settle before fixtures land
	setRetention(manager, 0, 1)        // 1 MiB budget
	os.MkdirAll(filepath.Dir(logPathAt(dir, "a.log")), 0o700)
	// 3 × 600 KiB = 1800 KiB against a 1024 KiB budget → oldest must go
	writeLogFixture(t, logPathAt(dir, "oldest.log"), 600*1024, 3*24*time.Hour)
	writeLogFixture(t, logPathAt(dir, "middle.log"), 600*1024, 2*24*time.Hour)
	writeLogFixture(t, logPathAt(dir, "newest.log"), 600*1024, 1*24*time.Hour)

	manager.pruneSessionLogs()
	// 1800 KiB against a 1024 KiB budget: evicting the oldest (600) leaves
	// 1200 — still over — so middle must go too; newest (600) fits.
	for _, gone := range []string{"oldest.log", "middle.log"} {
		if _, err := os.Stat(logPathAt(dir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the size budget", gone)
		}
	}
	if _, err := os.Stat(logPathAt(dir, "newest.log")); err != nil {
		t.Fatal("newest log was evicted though the budget already fit")
	}

	// a live sink's file is immune even when it alone busts the budget
	sink, err := openSessionLogSink(dir, ConnSpec{Username: "user", Host: "host", Port: 22}, "sh-live", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	shell := &Shell{ID: "sh-live"}
	shell.logSink.Store(sink)
	manager.mutex.Lock()
	manager.shells[shell.ID] = shell
	manager.mutex.Unlock()
	writeLogFixture(t, sink.path, 2*1024*1024, 100*24*time.Hour)
	manager.pruneSessionLogs()
	if _, err := os.Stat(sink.path); err != nil {
		t.Fatal("live sink's file was pruned")
	}
}
