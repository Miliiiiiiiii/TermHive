package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionLogSinkLinesAndCarry(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 2, 10, 30, 0, 0, time.UTC)
	sink, err := openSessionLogSink(dir, ConnSpec{Username: "root", Host: "10.20.144.205", Port: 22}, "shabc123", base)
	if err != nil {
		t.Fatal(err)
	}
	sink.clock = func() time.Time { return base }

	// A newline split across two chunks still logs as one line, stamped once.
	sink.Write([]byte("first line be"))
	sink.Write([]byte("gins\r\nsecond line\r\npartial without newline"))
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(sink.path)
	if err != nil {
		t.Fatal(err)
	}
	want := "[2026-11-02 10:30:00.000] first line begins\r\n" +
		"[2026-11-02 10:30:00.000] second line\r\n" +
		"[2026-11-02 10:30:00.000] partial without newline\n"
	if string(raw) != want {
		t.Fatalf("log = %q, want %q", raw, want)
	}
	if !strings.Contains(filepath.ToSlash(filepath.Dir(sink.path)), "logs/root@10.20.144.205_22") {
		t.Fatalf("per-connection dir layout wrong: %s", sink.path)
	}
}

func TestSessionLogSinkCapStops(t *testing.T) {
	dir := t.TempDir()
	sink, err := openSessionLogSink(dir, ConnSpec{Username: "u", Host: "h", Port: 22}, "sh1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sink.written = sessionLogCap // simulate a full log
	sink.Write([]byte("one more line\r\n"))
	if !sink.stopped {
		t.Fatal("cap must stop further logging")
	}
	sink.Close()
	raw, _ := os.ReadFile(sink.path)
	if !strings.Contains(string(raw), "size cap reached") {
		t.Fatalf("cap marker missing: %q", raw)
	}
	if strings.Contains(string(raw), "one more line") {
		t.Fatalf("post-cap line must not be logged: %q", raw)
	}
}

func TestSanitizeLogName(t *testing.T) {
	if got := sanitizeLogName(ConnSpec{Username: `ad\min`, Host: "srv:1", Port: 22}); got != `ad_min@srv_1_22` {
		t.Fatalf("sanitize = %q", got)
	}
}

func TestSessionLogSettingAttachesAndDetaches(t *testing.T) {
	manager := NewManager(t.TempDir())
	shell := &Shell{ID: "shtest1", Config: ConnSpec{Username: "u", Host: "h", Port: 22, ConnectionID: "c1"}}
	manager.shells[shell.ID] = shell

	// Default off: dial-time attach is skipped, toggle attaches live shells.
	if manager.sessionLogEnabled() {
		t.Fatal("session log must default to off")
	}
	manager.SetSettings(map[string]any{"sessionLog": true})
	if shell.logSink.Load() == nil {
		t.Fatal("toggle on must attach a sink to live shells")
	}
	path := shell.logSink.Load().path

	manager.SetSettings(map[string]any{"sessionLog": false})
	if shell.logSink.Load() != nil {
		t.Fatal("toggle off must detach the sink")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log file must exist after detach: %v", err)
	}
}
