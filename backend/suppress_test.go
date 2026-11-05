package main

import (
	"strings"
	"testing"
	"time"
)

func feed(t *testing.T, shell *Shell, chunks []string) string {
	t.Helper()
	out := ""
	for _, chunk := range chunks {
		emit, done := shell.suppressChunk([]byte(chunk))
		out += string(emit)
		if done {
			// window closed: subsequent chunks must pass verbatim
			return out
		}
	}
	return out
}

func TestHideWindowBannerSurvives(t *testing.T) {
	shell := &Shell{}
	snippet := ` __osc7(){ printf 'x'; };PROMPT_COMMAND="__osc7"`
	sentinel := "__th7_abc__"
	shell.BeginHideWindow(snippet[:16], sentinel, 2)

	// banner first, then the echoed injection line (sentinel occurrence #1),
	// then the echo output (occurrence #2) and the bytes that follow
	got := feed(t, shell, []string{
		"TEST-BANNER\r\n",
		"ECHO:" + snippet + ";echo " + sentinel,
		"\r\n\x1b]7;file:///C:\\x\x1b\\",
		sentinel + "\r\nrest-of-stream",
	})
	if !strings.Contains(got, "TEST-BANNER") {
		t.Errorf("banner was eaten: %q", got)
	}
	if strings.Contains(got, "__osc7") || strings.Contains(got, sentinel) {
		t.Errorf("injection echo leaked: %q", got)
	}
	if !strings.Contains(got, "rest-of-stream") {
		t.Errorf("post-sentinel bytes missing: %q", got)
	}
}

func TestHideWindowMarkerSplitAcrossFlushes(t *testing.T) {
	shell := &Shell{}
	snippet := " __osc7(){ printf"
	sentinel := "__th7_q__"
	shell.BeginHideWindow(snippet, sentinel, 2)

	got := feed(t, shell, []string{
		"TEST-BANNER\r\nECHO:",
		" __osc7",                            // marker split: first half
		"(){ printf hook stuff;echo __th7_q", // marker complete + echo, sentinel split
		"__\r\n",                             // echo line complete (occurrence #1)
		"\x1b[?25l",
		sentinel + "AFTER", // echo output (occurrence #2) + tail
	})
	if !strings.Contains(got, "TEST-BANNER") {
		t.Errorf("banner was eaten: %q", got)
	}
	if strings.Contains(got, "osc7") || strings.Contains(got, sentinel) {
		t.Errorf("injection echo leaked: %q", got)
	}
	if !strings.Contains(got, "AFTER") {
		t.Errorf("post-sentinel bytes missing: %q", got)
	}
}

func TestHideWindowDeadlineFallback(t *testing.T) {
	shell := &Shell{}
	shell.BeginHideWindow(" __osc7", "SENT", 2)
	// the window survives while bytes keep flowing; it disarms only after
	// hideIdleWindow of SILENCE
	time.Sleep(hideIdleWindow + 150*time.Millisecond)
	emit, done := shell.suppressChunk([]byte("everything flows"))
	if !done || string(emit) != "everything flows" {
		t.Errorf("idle window should disarm and forward, got %q done=%v", emit, done)
	}
}

// the frozen-screen regression: when the whole handshake (marker, echoed
// sentinel, echo output sentinel, prompt) lands in ONE flush, the window must
// close in that same call — the prompt renders afterwards and output goes
// silent, so a state machine that waits for the next flush never runs
func TestHideWindowBothSentinelsSameFlush(t *testing.T) {
	shell := &Shell{}
	snippet := ` PROMPT $E]7;file:///$P$E\gordo@MILII $P$G`
	sentinel := "__th7_x__"
	shell.BeginHideWindow(snippet[:16], sentinel, 2)

	emit, done := shell.suppressChunk([]byte(
		"Microsoft Windows [版本 10.0.26200.9711]\r\n" +
			"(c) Microsoft Corporation。保留所有权利。\r\n" +
			"gordo@MILII C:\\Users\\gordo>" + snippet + "&echo " + sentinel + "\r\n" +
			sentinel + "\r\n" +
			"\x1b[6;1Hgordo@MILII C:\\Users\\gordo>"))
	if !done {
		t.Fatalf("window must close on the flush carrying both sentinels, emit=%q", emit)
	}
	got := string(emit)
	if !strings.Contains(got, "Microsoft Windows") || !strings.Contains(got, "(c) Microsoft") {
		t.Errorf("banner was eaten: %q", got)
	}
	if strings.Contains(got, "PROMPT $E") || strings.Contains(got, sentinel) {
		t.Errorf("injection echo leaked: %q", got)
	}
	if !strings.Contains(got, "gordo@MILII C:\\Users\\gordo>") {
		t.Errorf("post-sentinel prompt missing: %q", got)
	}
}

// ConPTY positions the prompt with an absolute cursor sequence instead of a
// newline; the swallow must start there, not at the previous newline (which
// would eat the banner's copyright line together with the prompt)
func TestHideWindowCupKeepsCopyright(t *testing.T) {
	shell := &Shell{}
	snippet := ` PROMPT $E]7;file:///$P$E\gordo@MILII $P$G`
	sentinel := "__th7_y__"
	shell.BeginHideWindow(snippet[:16], sentinel, 2)

	emit, done := shell.suppressChunk([]byte(
		"Microsoft Windows [版本 10.0.26200.9711]\r\n" +
			"(c) Microsoft Corporation。保留所有权利。" + // no newline: CUP follows
			"\x1b[6;1H" +
			"gordo@MILII C:\\Users\\gordo>" + snippet + "&echo " + sentinel + "\r\n" +
			sentinel + "\r\n" +
			"\x1b[7;1Hgordo@MILII C:\\Users\\gordo>"))
	if !done {
		t.Fatalf("window must close in-flush, emit=%q", emit)
	}
	got := string(emit)
	if !strings.Contains(got, "(c) Microsoft Corporation") {
		t.Errorf("copyright line was eaten by the newline-based swallow: %q", got)
	}
	if strings.Contains(got, "PROMPT $E") || strings.Contains(got, sentinel) {
		t.Errorf("injection echo leaked: %q", got)
	}
}

// the idle-expiry fallback must drop BOTH buffered sentinel occurrences, or
// the second one leaks into the terminal as junk text
func TestHideWindowExpiryDropsBothSentinels(t *testing.T) {
	shell := &Shell{}
	shell.BeginHideWindow(" MARKER", "SENT", 2)
	// mid-hide state with both occurrences still buffered (the live loop now
	// consumes them in-flush, so construct the state directly)
	shell.hideStarted = true
	shell.hideBuf = []byte("line: SENT mid SENT end")
	shell.hideDeadline = time.Now().Add(-time.Millisecond)
	emit, done := shell.suppressChunk([]byte("TAIL"))
	if !done {
		t.Fatalf("expired window must disarm")
	}
	got := string(emit)
	if strings.Contains(got, "SENT") {
		t.Errorf("sentinel leaked on expiry: %q", got)
	}
	if !strings.Contains(got, "TAIL") {
		t.Errorf("post-expiry bytes missing: %q", got)
	}
}

// ---- OSC 7 close mode (cmd / PowerShell, ConPTY) ----
//
// The live failure this mode fixes: ConPTY repaints re-deliver the echoed
// hook line, and a text sentinel in that copy would close the window early —
// after which the delayed REAL output leaks onto the screen glued to the
// user's first keystrokes. Fixtures below replay the byte choreography
// captured from the real local sshd (see scripts/freeze-repro.mjs).

func TestHideWindowOsc7RepaintCopyKeepsWindowOpen(t *testing.T) {
	shell := &Shell{}
	snippet := ` PROMPT $E]7;file:///$P$E\gordo@MILII $P$G`
	shell.BeginHideWindow(snippet[:16], osc7Trigger, 1)

	// the captured timeline: banner; tty echo of the hook line (cmd has NOT
	// executed it yet); a ConPTY repaint carrying a COPY of the echoed line;
	// the user typing before the prompt ever rendered; then cmd finally
	// executing the hook and the new prompt rendering the live OSC 7
	got := feed(t, shell, []string{
		"Microsoft Windows [版本 10.0.26200.9711]\r\n(c) Microsoft Corporation。保留所有权利。\x1b[6;1H",
		"gordo@MILII C:\\Users\\gordo>" + snippet + "\r\n",
		"\x1b[?25l" + snippet + "\x1b[6;1H", // repaint copy — must NOT close
		"echo ZZMARK7\r",                    // user typing into the void
		"\x1b[7;1H\x1b]7;file:///C:\\Users\\gordo\x1b\\gordo@MILII C:\\Users\\gordo>",
	})
	if !strings.Contains(got, "Microsoft Windows") {
		t.Errorf("banner was eaten: %q", got)
	}
	if strings.Contains(got, "PROMPT $E") {
		t.Errorf("injection echo (or its repaint copy) leaked: %q", got)
	}
	if !strings.Contains(got, "echo ZZMARK7") {
		t.Errorf("user keystrokes held past the close: %q", got)
	}
	if !strings.Contains(got, "\x1b]7;file:///C:\\Users\\gordo") {
		t.Errorf("live OSC 7 must be emitted — the follow feature listens for it: %q", got)
	}
	if !strings.Contains(got, "gordo@MILII C:\\Users\\gordo>") {
		t.Errorf("prompt missing after close: %q", got)
	}
}

// the trigger sequence itself is the payload the UI needs — closing must
// emit it, not swallow it; and everything AFTER it flows verbatim
func TestHideWindowOsc7EmitsTriggerAndTail(t *testing.T) {
	shell := &Shell{}
	shell.BeginHideWindow(" PROMPT $E]7;fil", osc7Trigger, 1)
	emit, done := shell.suppressChunk([]byte(
		"ECHO:  PROMPT $E]7;file:///$P$E\\gordo $P$G\r\n" + osc7Trigger + "/F:/x\x1b\\AFTER"))
	if !done {
		t.Fatalf("window must close on the first live OSC 7, emit=%q", emit)
	}
	got := string(emit)
	if strings.Contains(got, "PROMPT $E") {
		t.Errorf("echo line leaked: %q", got)
	}
	if !strings.Contains(got, osc7Trigger+"/F:/x") {
		t.Errorf("trigger bytes must be emitted verbatim: %q", got)
	}
	if !strings.Contains(got, "AFTER") {
		t.Errorf("post-trigger bytes missing: %q", got)
	}
}

// a shell that never renders the live sequence (hook failed, exotic prompt)
// must still not blurt the echoed hook line when the idle window expires
func TestHideWindowOsc7ExpiryDropsEchoLine(t *testing.T) {
	shell := &Shell{}
	shell.BeginHideWindow(" PROMPT $E", osc7Trigger, 1)
	shell.hideStarted = true
	shell.hideBuf = []byte("banner kept\r\ngordo> PROMPT $E]7;file:///$P$E\\x\r\nlater-bytes")
	shell.hideDeadline = time.Now().Add(-time.Millisecond)
	emit, done := shell.suppressChunk([]byte("TAIL"))
	if !done {
		t.Fatalf("expired window must disarm")
	}
	got := string(emit)
	if strings.Contains(got, "PROMPT $E") {
		t.Errorf("echoed hook line leaked on expiry: %q", got)
	}
	if !strings.Contains(got, "banner kept") || !strings.Contains(got, "later-bytes") || !strings.Contains(got, "TAIL") {
		t.Errorf("bytes around the dropped line must survive: %q", got)
	}
}
