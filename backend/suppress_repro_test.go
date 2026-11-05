package main

import (
	"strings"
	"testing"
	"time"
)

// TestHideWindowRealCmdStream replays the byte stream captured from the real
// local cmd/ConPTY session (two-shell repro) through the hide window with the
// flush boundaries the instrumented run reported: the scanner's first flush
// after arming was 68 bytes STARTING AT the marker (the banner+prompt had
// already flowed before the window armed, because the shell-kind probe holds
// the arming back by seconds on cold Windows), and that flush ends
// mid-sentinel — the rest of the echoed line plus the sentinel echo arrive in
// later flushes.
func TestHideWindowRealCmdStream(t *testing.T) {
	snippet := " PROMPT $E]7;file:///$P$E\\gordo@MILII $P$G"
	sentinel := "__th7_411bc988f9ab__"
	marker := snippet[:16]

	// what the pump saw AFTER the window armed (banner+prompt already out):
	// flush1: the echoed line INCLUDING the complete sentinel (the captured
	// flush was 68 bytes = 48 line prefix + the whole 20-byte sentinel)
	echoHead := " PROMPT $E]7;file:///$P$E\\gordo@MILII $P$G&echo " + sentinel
	// flush2: line end + OSC7 of the new prompt + cursor hide
	echoTail := "\r\n\x1b]7;file:///C:\\Users\\gordo\x1b\\\x1b[?25l"
	// flush3: the sentinel echo (command output) + new prompt
	echoOut := sentinel + "\x1b[14;1Hgordo@MILII C:\\Users\\gordo>\x1b[?25h"

	shell := &Shell{}
	shell.BeginHideWindow(marker, sentinel, 2)

	var out strings.Builder
	feeds := []string{echoHead, echoTail, echoOut, "USER-TYPING"}
	for index, chunk := range feeds {
		emit, done := shell.suppressChunk([]byte(chunk))
		out.WriteString(string(emit))
		if done {
			// window closed: every remaining chunk passes verbatim
			for _, rest := range feeds[index+1:] {
				out.WriteString(rest)
			}
			break
		}
	}
	got := out.String()
	if strings.Contains(got, "PROMPT $E") || strings.Contains(got, sentinel) {
		t.Errorf("injection leaked: %q", got)
	}
	if !strings.Contains(got, "gordo@MILII C:\\Users\\gordo>") {
		t.Errorf("post-sentinel prompt missing: %q", got)
	}
	// the OSC 7 between the two sentinel occurrences belongs to the hidden
	// region by design; the NEXT prompt reports again, so it is not asserted
	// here
	if !strings.Contains(got, "USER-TYPING") {
		t.Errorf("post-window input missing: %q", got)
	}
}

// TestHideWindowLateArmAfterPrompt guards the other real-world order: the
// window arms AFTER the banner+first prompt already streamed out (slow probe),
// so the echo line begins a fresh held buffer at offset 0 — same as above but
// with the prompt already gone; must still hide and still forward the rest.
func TestHideWindowLateArmAfterPrompt(t *testing.T) {
	snippet := " PROMPT $E]7;file:///$P$E\\gordo@MILII $P$G"
	sentinel := "__th7_50317a9b97ee__"
	shell := &Shell{}
	shell.BeginHideWindow(snippet[:16], sentinel, 2)
	_ = time.Now

	emit, done := shell.suppressChunk([]byte(" PROMPT $E]7;file:///$P$E\\gordo@MILII $P$G&echo " + sentinel))
	if done || len(emit) != 0 {
		t.Fatalf("first flush should hide from offset 0, got emit=%q done=%v", emit, done)
	}
	emit, done = shell.suppressChunk([]byte("\r\n\x1b]7;file:///C:\\x\x1b\\" + sentinel + "PROMPT>"))
	if !done {
		t.Fatalf("sentinel in second flush must close the window")
	}
	if !strings.Contains(string(emit), "PROMPT>") {
		t.Errorf("bytes after sentinel lost: %q", emit)
	}
}
