package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTailBufferDrainAndCap(t *testing.T) {
	buf := &tailBuffer{}
	buf.Write([]byte("hello "))
	buf.Write([]byte("world"))
	chunk, skipped := buf.take(0)
	if string(chunk) != "hello world" || skipped != 0 {
		t.Fatalf("take all = %q, skipped %d", chunk, skipped)
	}

	// Filling past the cap keeps the tail and reports the dropped head.
	payload := strings.Repeat("a", tailBufferCap) + "TAIL"
	buf.Write([]byte(payload))
	chunk, skipped = buf.take(0)
	if len(chunk) != tailBufferCap || !strings.HasSuffix(string(chunk), "TAIL") {
		t.Fatalf("expected last %d bytes ending in TAIL, got %d bytes", tailBufferCap, len(chunk))
	}
	if skipped != int64(len(payload)-tailBufferCap) {
		t.Fatalf("skipped = %d, want %d", skipped, len(payload)-tailBufferCap)
	}

	// A chunk larger than the cap on its own keeps only its own tail.
	oversized := strings.Repeat("b", tailBufferCap+4096)
	buf.Write([]byte(oversized))
	chunk, skipped = buf.take(0)
	if len(chunk) != tailBufferCap || !strings.HasPrefix(string(chunk), "b") {
		t.Fatalf("oversized chunk not capped to tail: %d bytes", len(chunk))
	}
	if skipped != 4096 {
		t.Fatalf("skipped = %d, want 4096", skipped)
	}
}

func TestTailBufferTakeBatches(t *testing.T) {
	buf := &tailBuffer{}
	buf.Write([]byte("0123456789"))
	first, _ := buf.take(4)
	if string(first) != "0123" {
		t.Fatalf("first batch = %q", first)
	}
	second, skipped := buf.take(100)
	if string(second) != "456789" || skipped != 0 {
		t.Fatalf("second batch = %q (skipped %d)", second, skipped)
	}
	third, _ := buf.take(10)
	if len(third) != 0 {
		t.Fatalf("drained buffer should be empty, got %q", third)
	}
}

func TestTailFollowCommand(t *testing.T) {
	if got := tailFollowCommand("/var/log/app.log", "", 100); got != "tail -n 100 -F -- '/var/log/app.log'" {
		t.Fatalf("file command = %q", got)
	}
	// A path with spaces, quotes and a leading dash stays one literal argument.
	if got := tailFollowCommand("/opt/my logs/it's.log", "", 5); got != "tail -n 5 -F -- '/opt/my logs/it'\\''s.log'" {
		t.Fatalf("quoted path command = %q", got)
	}
	if got := tailFollowCommand("", "ssh", 50); got != "journalctl -u 'ssh' -n 50 -f --no-pager" {
		t.Fatalf("journal command = %q", got)
	}
}

func newTestTailTask(id, connID string, alive bool) *tailTask {
	task := &tailTask{id: id, connID: connID, createdAt: time.Now(), done: make(chan struct{}), lastRead: time.Now()}
	if !alive {
		task.finish(nil)
	}
	return task
}

func TestTailRegistryPerConnLimit(t *testing.T) {
	manager := NewManager(t.TempDir())
	if err := manager.registerTailTask(newTestTailTask("tf1", "conn-a", true)); err != nil {
		t.Fatal(err)
	}
	if err := manager.registerTailTask(newTestTailTask("tf2", "conn-a", true)); err != nil {
		t.Fatal(err)
	}
	// Third live task on the same connection is refused; other connections are unaffected.
	if err := manager.registerTailTask(newTestTailTask("tf3", "conn-a", true)); err == nil {
		t.Fatal("expected per-connection limit to reject the third live task")
	}
	if err := manager.registerTailTask(newTestTailTask("tf4", "conn-b", true)); err != nil {
		t.Fatalf("other connection should be unaffected: %v", err)
	}

	// Finished tasks never block: draining one to dead frees its live slot.
	manager.tailTasks["tf1"].finish(nil)
	if err := manager.registerTailTask(newTestTailTask("tf5", "conn-a", true)); err != nil {
		t.Fatalf("dead task should free its live slot: %v", err)
	}

	// Abandoned dead streams cannot pile up: past the total cap the oldest
	// finished one is evicted to make room.
	for _, existing := range manager.tailTasks {
		existing.finish(nil)
	}
	for i := 0; i < tailMaxPerConn*2+2; i++ {
		if err := manager.registerTailTask(newTestTailTask(fmt.Sprintf("tfx%d", i), "conn-a", false)); err != nil {
			t.Fatalf("finished tasks must not be refused: %v", err)
		}
	}
	count := 0
	for _, task := range manager.tailTasks {
		if task.connID == "conn-a" {
			count++
		}
	}
	if count > tailMaxPerConn*2 {
		t.Fatalf("total per connection should stay bounded at %d, got %d", tailMaxPerConn*2, count)
	}
}

func TestTailReadRefusesOtherConnection(t *testing.T) {
	manager := NewManager(t.TempDir())
	task := newTestTailTask("tf1", "conn-a", false)
	manager.tailTasks["tf1"] = task
	ctx := &mcpContext{manager: manager, conn: &Conn{Spec: ConnSpec{ConnectionID: "conn-b", Name: "other"}}}
	if _, err := mcpTailRead(ctx, map[string]any{"taskId": "tf1"}); err == nil {
		t.Fatal("reading a task of another connection must be refused")
	}
	if _, err := mcpTailStop(ctx, map[string]any{"taskId": "tf1"}); err == nil {
		t.Fatal("stopping a task of another connection must be refused")
	}
	// Same connection works and reports the ended stream.
	ctx.conn = &Conn{Spec: ConnSpec{ConnectionID: "conn-a", Name: "mine"}}
	out, err := mcpTailRead(ctx, map[string]any{"taskId": "tf1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ended") {
		t.Fatalf("finished task should report ended, got %q", out)
	}
}

func TestTailReapIdle(t *testing.T) {
	manager := NewManager(t.TempDir())
	task := newTestTailTask("tf1", "conn-a", false)
	manager.tailTasks["tf1"] = task

	// Recent read re-arms instead of reaping.
	if _, err := (&mcpContext{manager: manager, conn: &Conn{Spec: ConnSpec{ConnectionID: "conn-a"}}}).tailTaskFor("tf1"); err != nil {
		t.Fatal(err)
	}
	manager.reapTailTask("tf1", true)
	if _, present := manager.tailTasks["tf1"]; !present {
		t.Fatal("freshly-read task must not be reaped as idle")
	}

	// A stale lastRead is reaped.
	task.lastRead = time.Now().Add(-2 * tailIdleTimeout)
	manager.reapTailTask("tf1", true)
	if _, present := manager.tailTasks["tf1"]; present {
		t.Fatal("stale task should have been reaped")
	}
}
