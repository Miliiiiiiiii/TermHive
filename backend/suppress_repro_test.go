package main

import (
	"strings"
	"testing"
	"time"
)

const reproOsc7 = "\x1b]7;file://"

// TestHideWindowRealCmdStream replays the byte stream captured from the real
// local cmd/ConPTY session (two-shell repro) through the hide window with the
// flush boundaries the instrumented run reported: the first flush after
// arming starts AT the echoed hook line (the banner+prompt had already flowed
// before the window armed, because the shell-kind probe holds the arming back
// on cold Windows), and the live OSC 7 arrives with the new prompt.
func TestHideWindowRealCmdStream(t *testing.T) {
	snippet := " PROMPT $E]7;file:///$P$E\\gordo@MILII $P$G"
	marker := snippet[:16]

	// flush1: the echoed hook line
	echoHead := " PROMPT $E]7;file:///$P$E\\gordo@MILII $P$G"
	// flush2: line end + the live OSC 7 of the new prompt + cursor hide
	echoTail := "\r\n\x1b]7;file:///C:\\Users\\gordo\x1b\\\x1b[?25l"
	// flush3: the new prompt (the old sentinel echo no longer exists — the
	// injection is the bare snippet now)
	echoOut := "\x1b[14;1Hgordo@MILII C:\\Users\\gordo>\x1b[?25h"

	shell := &Shell{}
	shell.BeginHideWindow([]string{marker}, osc7Trigger)

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
	if strings.Contains(got, "PROMPT $E") {
		t.Errorf("injection leaked: %q", got)
	}
	if !strings.Contains(got, reproOsc7+"/C:\\Users\\gordo") {
		t.Errorf("live OSC 7 must be emitted: %q", got)
	}
	if !strings.Contains(got, "gordo@MILII C:\\Users\\gordo>") {
		t.Errorf("post-close prompt missing: %q", got)
	}
	if !strings.Contains(got, "USER-TYPING") {
		t.Errorf("post-window input missing: %q", got)
	}
}

// TestHideWindowLateArmAfterPrompt guards the other real-world order: the
// window arms AFTER the banner+first prompt already streamed out (slow probe),
// so the echo line begins a fresh held buffer at offset 0; must still hide and
// still forward the rest.
func TestHideWindowLateArmAfterPrompt(t *testing.T) {
	snippet := " PROMPT $E]7;file:///$P$E\\gordo@MILII $P$G"
	shell := &Shell{}
	shell.BeginHideWindow([]string{snippet[:16]}, osc7Trigger)
	_ = time.Now

	emit, done := shell.suppressChunk([]byte(" PROMPT $E]7;file:///$P$E\\gordo@MILII $P$G\r\n"))
	if done || len(emit) != 0 {
		t.Fatalf("echo flush should be swallowed whole, got emit=%q done=%v", emit, done)
	}
	emit, done = shell.suppressChunk([]byte(reproOsc7 + "/C:\\x\x1b\\" + "PROMPT>"))
	if !done {
		t.Fatalf("the live OSC 7 must close the window")
	}
	if !strings.Contains(string(emit), "PROMPT>") {
		t.Errorf("bytes after the trigger lost: %q", emit)
	}
}
