package main

import (
	"strings"
	"testing"
	"time"
)

// All hide-window fixtures share the one mechanism: regions carrying the hook
// marker (the echoed injection line and any redraw copies of it) are dropped;
// everything else — banner, user keystrokes — flows live; the first LIVE OSC 7
// sequence closes the window and is emitted (the follow feature listens for
// it). Echoed snippets only ever carry the sequence as "$E]7;" / '\033]7;'
// TEXT, never the escape byte, so echo/redraw copies cannot spoof the close.

const testOsc7 = "\x1b]7;file://"

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
	snippet := ` __osc7(){ printf '\033]7;file://%s\033\134' "$PWD"; };PROMPT_COMMAND="__osc7${PROMPT_COMMAND:+;$PROMPT_COMMAND}";[ -n "$HISTFILE" ]&&{ history -c;history -r; }`
	// one marker per snippet line, exactly how injectFollowHook arms the window
	markers := []string{}
	for _, line := range strings.Split(snippet, "\n") {
		if len(line) > 16 {
			line = line[:16]
		}
		markers = append(markers, line)
	}
	shell.BeginHideWindow(markers, osc7Trigger)

	// banner first, then the echoed injection line (bash/readline echo),
	// user keystrokes while the window is still open, and finally the first
	// prompt rendered with the hook live (OSC 7 + PS1)
	got := feed(t, shell, []string{
		"Last login: Thu Nov 5 16:52 from 10.0.0.9\r\n",
		"root@server:~# " + snippet + "\r\n",
		"echo ZZMARK7\r",
		testOsc7 + "/root\x1b\\root@server:~# ",
	})
	if !strings.Contains(got, "Last login") {
		t.Errorf("banner was eaten: %q", got)
	}
	if strings.Contains(got, "__osc7") {
		t.Errorf("injection echo leaked: %q", got)
	}
	if !strings.Contains(got, "echo ZZMARK7") {
		t.Errorf("user keystrokes held past the close: %q", got)
	}
	if !strings.Contains(got, testOsc7+"/root") {
		t.Errorf("live OSC 7 must be emitted — the follow feature listens for it: %q", got)
	}
	if !strings.Contains(got, "root@server:~#") {
		t.Errorf("post-close prompt missing: %q", got)
	}
}

func TestHideWindowMarkerSplitAcrossFlushes(t *testing.T) {
	shell := &Shell{}
	snippet := " __osc7(){ printf"
	shell.BeginHideWindow([]string{snippet}, osc7Trigger)

	got := feed(t, shell, []string{
		"TEST-BANNER\r\nECHO:",
		" __osc7",                 // marker split: first half
		"(){ printf hook stuff\r", // marker complete; line still open
		"\nmore-output\r\n",       // echo line closed — dropped
		testOsc7 + "/x\x1b\\AFTER",
	})
	if !strings.Contains(got, "TEST-BANNER") {
		t.Errorf("banner was eaten: %q", got)
	}
	if strings.Contains(got, "osc7") {
		t.Errorf("injection echo leaked: %q", got)
	}
	if !strings.Contains(got, "more-output") {
		t.Errorf("non-marker bytes between echo and close must flow live: %q", got)
	}
	if !strings.Contains(got, "AFTER") {
		t.Errorf("post-trigger bytes missing: %q", got)
	}
}

func TestHideWindowDeadlineFallback(t *testing.T) {
	shell := &Shell{}
	shell.BeginHideWindow([]string{" __osc7"}, testOsc7)
	// the window survives while bytes keep flowing; it disarms only after
	// hideIdleWindow of SILENCE
	time.Sleep(hideIdleWindow + 150*time.Millisecond)
	emit, done := shell.suppressChunk([]byte("everything flows"))
	if !done || string(emit) != "everything flows" {
		t.Errorf("idle window should disarm and forward, got %q done=%v", emit, done)
	}
}

// the frozen-screen regression: when the whole handshake lands in ONE flush
// (marker line, the live OSC 7, the prompt), the window must close in that
// same call — the prompt renders afterwards and output goes silent, so
// waiting for "the next flush" would freeze the window open
func TestHideWindowWholeHandshakeSameFlush(t *testing.T) {
	shell := &Shell{}
	snippet := ` PROMPT $E]7;file:///$P$E\gordo@MILII $P$G`
	shell.BeginHideWindow([]string{snippet[:16]}, osc7Trigger)

	emit, done := shell.suppressChunk([]byte(
		"Microsoft Windows [版本 10.0.26200.9711]\r\n" +
			"(c) Microsoft Corporation。保留所有权利。\r\n" +
			"gordo@MILII C:\\Users\\gordo>" + snippet + "\r\n" +
			"\x1b[6;1H" + testOsc7 + "/C:\\Users\\gordo\x1b\\gordo@MILII C:\\Users\\gordo>"))
	if !done {
		t.Fatalf("window must close on the flush carrying the live OSC 7, emit=%q", emit)
	}
	got := string(emit)
	if !strings.Contains(got, "Microsoft Windows") || !strings.Contains(got, "(c) Microsoft") {
		t.Errorf("banner was eaten: %q", got)
	}
	if strings.Contains(got, "PROMPT $E") {
		t.Errorf("injection echo leaked: %q", got)
	}
	if !strings.Contains(got, testOsc7+"/C:\\Users\\gordo") {
		t.Errorf("live OSC 7 must be emitted: %q", got)
	}
	if !strings.Contains(got, "gordo@MILII C:\\Users\\gordo>") {
		t.Errorf("post-close prompt missing: %q", got)
	}
}

// ConPTY positions the prompt with an absolute cursor sequence instead of a
// newline; a CUP closes the previous paint region, so the banner's copyright
// line must survive the echo-line drop
func TestHideWindowCupKeepsCopyright(t *testing.T) {
	shell := &Shell{}
	snippet := ` PROMPT $E]7;file:///$P$E\gordo@MILII $P$G`
	shell.BeginHideWindow([]string{snippet[:16]}, osc7Trigger)

	emit, done := shell.suppressChunk([]byte(
		"Microsoft Windows [版本 10.0.26200.9711]\r\n" +
			"(c) Microsoft Corporation。保留所有权利。" + // no newline: CUP follows
			"\x1b[6;1H" +
			"gordo@MILII C:\\Users\\gordo>" + snippet + "\r\n" +
			"\x1b[7;1H" + testOsc7 + "/C:\\x\x1b\\gordo@MILII C:\\Users\\gordo>"))
	if !done {
		t.Fatalf("window must close in-flush, emit=%q", emit)
	}
	got := string(emit)
	if !strings.Contains(got, "(c) Microsoft Corporation") {
		t.Errorf("copyright line was eaten by the region drop: %q", got)
	}
	if strings.Contains(got, "PROMPT $E") {
		t.Errorf("injection echo leaked: %q", got)
	}
}

// the idle-expiry fallback must drop the echoed hook line (marker regions and
// a marker-carrying tail), or it blurts into the terminal as junk text
func TestHideWindowExpiryDropsEchoLine(t *testing.T) {
	shell := &Shell{}
	shell.BeginHideWindow([]string{" __osc7"}, osc7Trigger)
	shell.hideBuf = []byte("banner kept\r\necho:  __osc7(){ printf 'x'\r\nlater-bytes")
	shell.hideDeadline = time.Now().Add(-time.Millisecond)
	emit, done := shell.suppressChunk([]byte("TAIL"))
	if !done {
		t.Fatalf("expired window must disarm")
	}
	got := string(emit)
	if strings.Contains(got, "__osc7") {
		t.Errorf("echoed hook line leaked on expiry: %q", got)
	}
	if !strings.Contains(got, "banner kept") || !strings.Contains(got, "later-bytes") || !strings.Contains(got, "TAIL") {
		t.Errorf("bytes around the dropped line must survive: %q", got)
	}
}

// ---- the two live regressions this design exists for ----

// ConPTY repaints re-deliver the echoed hook line; under the old text-sentinel
// semantics the copy's sentinel closed the window early and the delayed real
// output leaked glued to the user's first keystrokes. Repaint copies must now
// simply be dropped and the window must stay open until the LIVE OSC 7.
func TestHideWindowOsc7RepaintCopyKeepsWindowOpen(t *testing.T) {
	shell := &Shell{}
	snippet := ` PROMPT $E]7;file:///$P$E\gordo@MILII $P$G`
	shell.BeginHideWindow([]string{snippet[:16]}, osc7Trigger)

	// the captured timeline: banner; tty echo of the hook line (cmd has NOT
	// executed it yet); a ConPTY repaint carrying a COPY of the echoed line;
	// the user typing before the prompt ever rendered; then cmd finally
	// executing the hook and the new prompt rendering the live OSC 7
	got := feed(t, shell, []string{
		"Microsoft Windows [版本 10.0.26200.9711]\r\n(c) Microsoft Corporation。保留所有权利。\x1b[6;1H",
		"gordo@MILII C:\\Users\\gordo>" + snippet + "\r\n",
		"\x1b[?25l" + snippet + "\x1b[6;1H", // repaint copy — must NOT close
		"echo ZZMARK7\r",                    // user typing into the void
		"\x1b[7;1H" + testOsc7 + "/C:\\Users\\gordo\x1b\\gordo@MILII C:\\Users\\gordo>",
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
	if !strings.Contains(got, testOsc7+"/C:\\Users\\gordo") {
		t.Errorf("live OSC 7 must be emitted: %q", got)
	}
	if !strings.Contains(got, "gordo@MILII C:\\Users\\gordo>") {
		t.Errorf("prompt missing after close: %q", got)
	}
}

// the POSIX freeze (live: four bash tabs, server-side bash healthy in
// n_tty_read while the client swallowed the prompt): a readline re-render
// re-delivering the echoed line must neither close the window early nor hold
// the user's input; even a shell that NEVER renders OSC 7 only delays its own
// prompt line by the idle window — typing still echoes live
func TestHideWindowPosixRedrawCopyAndNoOsc7(t *testing.T) {
	shell := &Shell{}
	snippet := ` __osc7(){ printf '\033]7;file://%s\033\134' "$PWD"; };PROMPT_COMMAND="__osc7${PROMPT_COMMAND:+;$PROMPT_COMMAND}";[ -n "$HISTFILE" ]&&{ history -c;history -r; }`
	markers := []string{}
	for _, line := range strings.Split(snippet, "\n") {
		if len(line) > 16 {
			line = line[:16]
		}
		markers = append(markers, line)
	}
	shell.BeginHideWindow(markers, osc7Trigger)

	// MOTD, the echoed injection line at 80 cols, a readline full-line
	// re-render (same bytes again), then the user typing while bash has not
	// yet produced any prompt the client would show
	got := feed(t, shell, []string{
		"Welcome to Ubuntu 22.04\r\n",
		"root@srv:~# " + snippet + "\r\n",
		"\rroot@srv:~# " + snippet + "\x1b[K", // readline redraw copy
		"echo ZZMARK7\r\n",
	})
	if !strings.Contains(got, "Welcome to Ubuntu") {
		t.Errorf("MOTD was eaten: %q", got)
	}
	if strings.Contains(got, "__osc7") {
		t.Errorf("injection echo (or redraw copy) leaked: %q", got)
	}
	if !strings.Contains(got, "echo ZZMARK7") {
		t.Errorf("user keystrokes must flow live while the window is open: %q", got)
	}
}

// TestHideWindowWrappedEchoContinuation: the tty driver hard-wraps an echo
// longer than the PTY width by inserting CRLF mid-line; the region walker
// sees the wrapped chunks as separate regions, and the continuation chunks
// carry no line-start bytes. Field repro (10.20.152.209 / 10.20.43.134):
// only the head chunk was dropped, the wrapped tail ("istory -r; }history
// -c;h") leaked onto the screen. With full-line markers and fragment
// matching every chunk dies while the user's own keystrokes still flow.
func TestHideWindowWrappedEchoContinuation(t *testing.T) {
	line := []byte(` PROMPT_COMMAND="__osc7; printf '\033]7;file://%s\033\\' \"$PWD\";${PROMPT_COMMAND:+;$PROMPT_COMMAND}\";[ -n \"$HISTFILE\" ]&&{ history -c;history -r; }`)
	markers := [][]byte{line}
	trigger := []byte("\x1b]7;file://")

	// the echo comes back folded at 80 columns: CRLF inserted mid-line
	fold := func(src []byte, width int) []byte {
		out := make([]byte, 0, len(src)+16)
		col := 0
		for _, b := range src {
			if col == width {
				out = append(out, '\r', '\n')
				col = 0
			}
			out = append(out, b)
			col++
		}
		out = append(out, '\r', '\n')
		return out
	}
	folded := fold(line, 80)
	kept, tail, done := filterOsc7Regions(folded, markers, markerFragments(markers), trigger, false)
	if done {
		t.Fatal("window closed on folded echo (no trigger present)")
	}
	if len(kept) != 0 {
		t.Fatalf("wrapped echo leaked %d bytes: %q", len(kept), kept)
	}
	if len(tail) != 0 {
		t.Fatalf("incomplete region held back unexpectedly: %q", tail)
	}

	// the user's keystrokes right behind the folded echo still flow live
	typed := []byte("ls -l\r\n")
	kept2, _, _ := filterOsc7Regions(typed, markers, markerFragments(markers), trigger, false)
	if string(kept2) != string(typed) {
		t.Fatalf("user keystrokes swallowed after wrapped echo: %q", kept2)
	}
}
