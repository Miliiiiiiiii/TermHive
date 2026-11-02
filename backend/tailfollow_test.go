package main

import (
	"bytes"
	"testing"
)

func TestSplitDecodableUTF8(t *testing.T) {
	// A 3-byte CJK char split 2/1: the partial sequence must be held back.
	complete := []byte("ok")
	partial := []byte{0xE4, 0xBD} // 你 (0xE4 0xBD 0xA0) missing its last byte
	emit, carry := splitDecodable("", append(append([]byte{}, complete...), partial...))
	if string(emit) != "ok" || !bytes.Equal(carry, partial) {
		t.Fatalf("emit=%q carry=%x", emit, carry)
	}
	// Completing the char on the next poll yields it whole.
	emit, carry = splitDecodable("", append(carry, 0xA0))
	if string(emit) != "你" || len(carry) != 0 {
		t.Fatalf("emit=%q carry=%x", emit, carry)
	}
	// Plain ASCII passes untouched.
	emit, carry = splitDecodable("", []byte("plain ascii\n"))
	if string(emit) != "plain ascii\n" || carry != nil {
		t.Fatalf("emit=%q carry=%v", emit, carry)
	}
}

func TestSplitDecodableGBK(t *testing.T) {
	// GBK lead byte at the boundary waits for its trail byte.
	emit, carry := splitDecodable("gbk", []byte{0xC4, 0xE3, 0xBA, 0xC3, 0xD6}) // "你好" + lead of 好' trail
	if !bytes.Equal(emit, []byte{0xC4, 0xE3, 0xBA, 0xC3}) || !bytes.Equal(carry, []byte{0xD6}) {
		t.Fatalf("emit=%x carry=%x", emit, carry)
	}
	emit, carry = splitDecodable("gbk", append(carry, 0xD0))
	if !bytes.Equal(emit, []byte{0xD6, 0xD0}) || carry != nil {
		t.Fatalf("emit=%x carry=%v", emit, carry)
	}
}

func TestLastLines(t *testing.T) {
	raw := []byte("l1\nl2\nl3\nl4")
	if got := string(lastLines(raw, 2)); got != "l3\nl4" {
		t.Fatalf("lastLines(2) = %q", got)
	}
	if got := string(lastLines(raw, 10)); got != "l1\nl2\nl3\nl4" {
		t.Fatalf("lastLines(10) = %q", got)
	}
	if got := string(lastLines(raw, 0)); got != "l1\nl2\nl3\nl4" {
		t.Fatalf("lastLines(0) must keep everything, got %q", got)
	}
}

func TestStartFollowNeedsLiveShell(t *testing.T) {
	manager := NewManager(t.TempDir())
	if _, err := manager.StartFollow("sh-missing", "/var/log/app.log", 100); err == nil {
		t.Fatal("follow must require a live shell to resolve the connection")
	}
}

func TestStopFollowIdempotent(t *testing.T) {
	manager := NewManager(t.TempDir())
	follower := &logFollower{id: "fw1", connID: "c1", stopCh: make(chan struct{})}
	manager.followers["fw1"] = follower
	manager.StopFollow("fw1")
	manager.StopFollow("fw1") // must not double-close stopCh
	select {
	case <-follower.stopCh:
	default:
		t.Fatal("stop channel should be closed")
	}
}
