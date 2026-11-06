package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
	"golang.org/x/crypto/ssh"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const ringCapacity = 128 * 1024
const termChunk = 32 * 1024

// flushWindow bounds how long terminal output may sit unflushed: short
// enough to be invisible while typing, long enough to merge a fast burst
// into one bridge frame.
const flushWindow = 4 * time.Millisecond

// Shell is one PTY session multiplexed over the connection's shared SSH
// client. Output is broadcast to the host and replayed from a ring buffer on
// attach.
type Shell struct {
	ID        string
	Config    ConnSpec
	CreatedAt time.Time

	emitter *dbxpluginsdk.Emitter

	session *ssh.Session

	writeCh   chan []byte
	ring      bytes.Buffer
	ringMutex sync.Mutex

	// echo-hiding window: once hideStart appears in the output (the tty echo
	// of the injected hook line), everything up to hideEnd (the sentinel the
	// hook echoes) is swallowed — ring AND host — so the terminal never sees
	// the hook, not even in replay. A cls only erases the live screen while
	// the bytes live on in the ring buffer and in ConPTY's resize
	// re-emission, hence this stream-level hiding. While scanning for the
	// start marker the last len(marker)-1 bytes are HELD BACK (a marker
	// split across flushes must not leak its beginning); the banner before
	// the marker flows with at most that sub-millisecond delay.
	suppressMutex sync.Mutex
	hideStart     [][]byte
	hideFrags     [][]byte
	hideEnd       []byte
	hideDeadline  time.Time
	hideBuf       []byte
	// hiding the injected echo leaves the cursor parked at the start of the
	// prompt line (the surviving bytes end in CR) while the shell's line
	// editor still models it after the prompt. One same-size window-change
	// once the window closes makes readline repaint the prompt line, which
	// parks the cursor where it belongs. POSIX shells only — ConPTY answers
	// a no-op resize with a full-screen re-emit.
	hideRedrawOnClose bool
	hideRedrawFired   bool
	lastCols          int
	lastRows          int

	closed      atomic.Bool
	closeMutex  sync.Mutex
	closeReason string

	// onClosed fires once when this shell ends, set before the pumps start
	// (the auto-reconnect supervisor listens for transport death here; a shell
	// EOF usually notices a dead link long before the keepalive does).
	onClosed func(*Shell)

	// logSink mirrors the flush-closure stream to disk when session logging
	// is on; swapped live by the settings toggle. Atomic so the pumps never
	// read a half-attached sink.
	logSink atomic.Pointer[sessionLogSink]

	writeErr atomic.Value // error
}

func (shell *Shell) Closed() bool        { return shell.closed.Load() }
func (shell *Shell) CloseReason() string { return shell.closeReason }

// hideIdleWindow bounds how long the echo-hiding window may stay open with
// NO bytes arriving — activity keeps it alive, so a slow shell startup
// (Windows ConPTY cold starts can take seconds before the first echo) never
// expires it mid-handshake, while a dead/echoless shell releases the stream
// after this much silence.
const hideIdleWindow = 3 * time.Second

// BeginHideWindow arms the echo-hiding window: a rolling region filter that
// drops every paint region containing ANY of startMarkers (the echoed hook
// lines — the injection may be several lines, e.g. the HISTCONTROL preset
// line ahead of the hook itself — plus any repaint/redraw copies of them)
// and closes at endMarker — the first LIVE OSC 7 escape sequence, which only
// exists once the hook actually took effect (the echoed lines carry it as
// literal "$E]7;" / '\033]7;' text, never the escape byte, so repaints cannot
// spoof the close). Bytes outside marker regions — the user's keystrokes
// included — flow live the whole time; the window self-cancels after
// hideIdleWindow of silence.
func (shell *Shell) BeginHideWindow(startMarkers []string, endMarker string) {
	shell.suppressMutex.Lock()
	defer shell.suppressMutex.Unlock()
	shell.hideStart = make([][]byte, 0, len(startMarkers))
	for _, marker := range startMarkers {
		shell.hideStart = append(shell.hideStart, []byte(marker))
	}
	// pre-expand every marker line into overlapping fragments: readline's
	// redraw of a wrapped echo emits byte sequences that are NOT verbatim
	// substrings of the injected text (spliced runs like `};PROMPT_COMMAND="_`),
	// but each visual run still carries a contiguous stretch of the original —
	// any fragment hit classifies the region as injection residue.
	shell.hideFrags = markerFragments(shell.hideStart)
	shell.hideEnd = []byte(endMarker)
	shell.hideDeadline = time.Now().Add(hideIdleWindow)
	shell.hideBuf = nil
}

// markerFragments slides a window over each marker line, producing every
// contiguous fragment of fragLen bytes (step fragLen/2, so no boundary split
// can dodge all of them). Lines shorter than the window fall back to the
// whole line.
const fragmentLen = 12

func markerFragments(lines [][]byte) [][]byte {
	var frags [][]byte
	for _, line := range lines {
		if len(line) <= fragmentLen {
			frags = append(frags, line)
			continue
		}
		for at := 0; at+fragmentLen <= len(line); at += fragmentLen / 2 {
			frags = append(frags, line[at:at+fragmentLen])
		}
	}
	return frags
}

// armHideRedrawLocked schedules the one-shot same-size window-change for
// after the hide window closes (caller holds suppressMutex; the network call
// runs on its own goroutine, never under the lock).
func (shell *Shell) armHideRedrawLocked() {
	if !shell.hideRedrawOnClose || shell.hideRedrawFired {
		return
	}
	shell.hideRedrawFired = true
	go shell.nudgeRedraw()
}

// nudgeRedraw re-sends the last known window size: readline answers WINCH by
// repainting the current (prompt) line, which also restores the cursor to
// the position the shell's line editor models — fixing the row-1 cursor the
// swallowed echo leaves behind. Harmless if the size is still the default.
func (shell *Shell) nudgeRedraw() {
	shell.suppressMutex.Lock()
	cols, rows := shell.lastCols, shell.lastRows
	shell.suppressMutex.Unlock()
	if cols <= 0 || rows <= 0 {
		return
	}
	_ = shell.Resize(cols, rows)
}

// firstPaintBoundaryEnd returns the index just past the first control that
// ends a paint region: an absolute cursor-position sequence (ESC [ r;c H/F,
// how ConPTY positions prompt lines) or an erase-line sequence (ESC [ K /
// 0K / 1K / 2K, how readline terminates a redraw), or -1 when there is none.
// CR is handled separately (a plain byte scan is enough there).
func firstPaintBoundaryEnd(buf []byte) int {
	escape := []byte("\x1b[")
	for i := 0; i+2 <= len(buf); {
		idx := bytes.Index(buf[i:], escape)
		if idx < 0 {
			return -1
		}
		j := i + idx + 2
		for j < len(buf) && (buf[j] == ';' || (buf[j] >= '0' && buf[j] <= '9')) {
			j++
		}
		if j < len(buf) {
			// EL takes no parameters ("\x1b[K" is the common form); a CUP
			// always carries row;col
			if buf[j] == 'K' && j >= i+idx+2 {
				return j + 1
			}
			if (buf[j] == 'H' || buf[j] == 'F') && j > i+idx+2 {
				return j + 1
			}
		}
		i += idx + 2
	}
	return -1
}

// filterOsc7Regions is the rolling hide filter. The buffer is walked in paint
// regions — a region ends at a newline or carriage return (inclusive) or at
// the end of a cursor-position / erase-line sequence, whichever comes first —
// and every region WITHOUT any hook marker flows out live (the user's
// keystrokes must never be held back or dropped just because the window is
// open); regions WITH a marker — the echoed hook lines and any repaint or
// readline-redraw copies of them — are dropped. CR and EL boundaries are what
// keep a redraw copy from swallowing the user bytes that follow it. The first
// LIVE OSC 7 trigger closes the window: its region is emitted (the follow
// feature listens for those bytes), along with everything behind it.
// final=true is the idle-expiry variant: the trailing incomplete region is
// decided now (dropped only if it holds a marker) instead of being held for
// more bytes. Returns the bytes to emit, the unconsumed tail (only when
// !done), done.
func filterOsc7Regions(buf []byte, markers, frags [][]byte, trigger []byte, final bool) (kept, tail []byte, done bool) {
	kept = make([]byte, 0, len(buf))
	pos := 0
	for pos < len(buf) {
		end := -1
		if nl := bytes.IndexAny(buf[pos:], "\r\n"); nl >= 0 {
			end = pos + nl + 1
		}
		if ctrl := firstPaintBoundaryEnd(buf[pos:]); ctrl > 0 && (end < 0 || pos+ctrl < end) {
			end = pos + ctrl
		}
		if end < 0 {
			break
		}
		region := buf[pos:end]
		if at := bytes.Index(region, trigger); at >= 0 {
			// the live trigger: emit its region (dropping any marker bytes
			// glued ahead of it in the same region) plus everything after it
			if hideMatches(region[:at], markers, frags) {
				kept = append(kept, region[at:]...)
			} else {
				kept = append(kept, region...)
			}
			return kept, nil, true
		}
		if !hideMatches(region, markers, frags) {
			kept = append(kept, region...)
			pos = end
		} else if region[len(region)-1] == '\r' && end < len(buf) && buf[end] == '\n' {
			// the dropped region ended at the CR of a CRLF pair — swallow the
			// orphan LF too, or it leaks as a stray blank line
			pos = end + 1
		} else {
			pos = end
		}
	}
	tail = buf[pos:]
	if at := bytes.Index(tail, trigger); at >= 0 {
		if hideMatches(tail[:at], markers, frags) {
			kept = append(kept, tail[at:]...)
		} else {
			kept = append(kept, tail...)
		}
		return kept, nil, true
	}
	if final {
		if !hideMatches(tail, markers, frags) {
			kept = append(kept, tail...)
		}
		return kept, nil, true
	}
	return kept, tail, false
}

// containsAny reports whether the region carries any of the hook markers.
// Kept for the trigger-glue checks; region classification goes through
// hideMatches below.
func containsAny(region []byte, markers [][]byte) bool {
	for _, marker := range markers {
		if bytes.Contains(region, marker) {
			return true
		}
	}
	return false
}

// hideMatches classifies one paint region against the injected hook lines.
// Two shapes must match:
//   - a redraw/echo copy CARRIES a line (or its distinctive head), and
//   - a hard-wrapped CONTINUATION of a line carries no line-start bytes at
//     all: the tty driver folds an echo longer than the PTY width by emitting
//     CRLF mid-line, which the region walker treats as a line end — the tail
//     fragment then has to be recognized as a fragment OF one of the lines.
//
// Fragment matching requires substance (≥8 content bytes after trimming the
// trailing CRLF/CSI boundary) so a user keystroke or stray short region can
// never be mistaken for injection residue. The window only spans the first
// seconds of a fresh session, so real output colliding with these specific
// command texts is not a practical concern.
func hideMatches(region []byte, lines, frags [][]byte) bool {
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		head := line
		if len(head) > 16 {
			head = head[:16]
		}
		if bytes.Contains(region, head) {
			return true
		}
		content := trimRegionBoundary(region)
		if len(content) >= 8 && bytes.Contains(line, content) {
			return true
		}
	}
	// the fragment set catches readline-redraw splices: runs that are not
	// verbatim substrings still carry contiguous stretches of the original
	if containsAny(region, frags) {
		return true
	}
	return false
}

// trimRegionBoundary strips the region's terminating CRLF and/or one trailing
// CSI sequence (EL/CUP paint boundary) so the remaining content can be
// matched as a plain substring of an injected line.
func trimRegionBoundary(region []byte) []byte {
	content := region
	for len(content) > 0 && (content[len(content)-1] == '\n' || content[len(content)-1] == '\r') {
		content = content[:len(content)-1]
	}
	if at := bytes.LastIndex(content, []byte("\x1b[")); at >= 0 {
		rest := content[at+2:]
		if len(rest) > 0 {
			isBoundary := true
			for _, b := range rest {
				if !(b == ';' || (b >= '0' && b <= '9') || b == 'K' || b == 'H' || b == 'F') {
					isBoundary = false
					break
				}
			}
			if isBoundary {
				content = content[:at]
			}
		}
	}
	return content
}

// suppressChunk runs one flush through the hiding window. It returns the
// bytes to emit (possibly none) and whether the hiding window is finished.
func (shell *Shell) suppressChunk(chunk []byte) (emit []byte, done bool) {
	shell.suppressMutex.Lock()
	defer shell.suppressMutex.Unlock()
	if len(shell.hideStart) == 0 {
		return chunk, true
	}
	if len(chunk) > 0 && time.Now().Before(shell.hideDeadline) {
		// bytes are still flowing (slow shell startup): keep the window
		// alive — an absolute deadline would expire mid-handshake on cold
		// Windows servers, where the echo round trip alone can take seconds
		shell.hideDeadline = time.Now().Add(hideIdleWindow)
	}
	if time.Now().After(shell.hideDeadline) {
		// idle for the whole window: decide everything now (marker regions
		// still dropped, so expiring does not UNDO the hiding) and disarm
		held, _, _ := filterOsc7Regions(shell.hideBuf, shell.hideStart, shell.hideFrags, shell.hideEnd, true)
		shell.hideStart = nil
		shell.hideEnd = nil
		shell.hideBuf = nil
		shell.armHideRedrawLocked()
		return append(held, chunk...), true
	}
	shell.hideBuf = append(shell.hideBuf, chunk...)
	kept, rest, closed := filterOsc7Regions(shell.hideBuf, shell.hideStart, shell.hideFrags, shell.hideEnd, false)
	if closed {
		shell.hideStart = nil
		shell.hideEnd = nil
		shell.hideBuf = nil
		shell.armHideRedrawLocked()
		return kept, true
	}
	shell.hideBuf = append([]byte(nil), rest...)
	if len(shell.hideBuf) > 256*1024 {
		// pathological repaint storm: never let the held tail wedge the
		// stream — decide it now
		marker, end := shell.hideStart, shell.hideEnd
		held := shell.hideBuf
		shell.hideStart = nil
		shell.hideEnd = nil
		shell.hideBuf = nil
		out, _, _ := filterOsc7Regions(held, marker, markerFragments(marker), end, true)
		return append(kept, out...), true
	}
	return kept, false
}

func (shell *Shell) markClosed(reason string) {
	shell.closeMutex.Lock()
	var callback func(*Shell)
	if !shell.closed.Load() {
		shell.closed.Store(true)
		shell.closeReason = reason
		close(shell.writeCh)
		callback = shell.onClosed
	}
	shell.closeMutex.Unlock()
	if callback != nil {
		go callback(shell) // never block a pump on the supervisor
	}
	if sink := shell.logSink.Swap(nil); sink != nil {
		_ = sink.Close() // flush the session log's tail
	}
}

func (shell *Shell) Close(reason string) {
	shell.markClosed(reason)
	if shell.session != nil {
		shell.session.Close()
	}
}

// emit is safe to call from any goroutine once the shell is connected.
func (shell *Shell) emit(method string, params any) {
	if shell.emitter != nil {
		_ = shell.emitter.Event(method, params)
	}
}

// authMethods builds the auth list for one dial attempt. keyboard-interactive
// is a separate attempt (see dialSSH): stacking it after ssh.Password breaks
// against Windows OpenSSH, which answers USERAUTH_FAILURE where x/crypto
// expects the first interactive question, surfacing as a confusing
// "unexpected message type 51" instead of a plain wrong-password error.
func authMethods(auth AuthConfig, interactive bool) ([]ssh.AuthMethod, error) {
	switch auth.Method {
	case "", "password":
		if auth.Password == "" {
			return nil, errors.New("未填写密码: 请在连接表单填入密码, 或把认证方式切换为\"私钥\"并选择私钥文件")
		}
		if !interactive {
			return []ssh.AuthMethod{ssh.Password(auth.Password)}, nil
		}
		return []ssh.AuthMethod{
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for index := range answers {
					answers[index] = auth.Password
				}
				return answers, nil
			}),
		}, nil
	case "key":
		signer, err := cachedSigner(auth.KeyPath, []byte(auth.Passphrase))
		if err != nil {
			return nil, err
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	default:
		return nil, fmt.Errorf("unsupported auth method: %s", auth.Method)
	}
}

// signerCache reuses parsed private keys across dials: re-reading and
// re-parsing an (often encrypted) key on every reconnect is pure overhead,
// and ssh.Signer is stateless enough to share. The cache key includes the
// file mtime, so a regenerated key reloads; stale entries from old mtimes
// are bounded by how often keys actually change.
var signerCache sync.Map

func cachedSigner(path string, passphrase []byte) (ssh.Signer, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	passDigest := sha256.Sum256(passphrase)
	cacheKey := fmt.Sprintf("%s|%d|%x", path, info.ModTime().UnixNano(), passDigest[:8])
	if cached, ok := signerCache.Load(cacheKey); ok {
		return cached.(ssh.Signer), nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	var signer ssh.Signer
	if len(passphrase) > 0 {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(raw, passphrase)
	} else {
		signer, err = ssh.ParsePrivateKey(raw)
	}
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	signerCache.Store(cacheKey, signer)
	return signer, nil
}

func sshClientConfig(spec ConnSpec, timeout time.Duration, interactive bool, hostKeys *HostKeyStore) (*ssh.ClientConfig, error) {
	methods, err := authMethods(spec.Auth, interactive)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User:            spec.Username,
		Auth:            methods,
		HostKeyCallback: hostKeys.Callback(spec.Host, spec.Port),
		Timeout:         timeout,
	}, nil
}

// ConnectShell establishes the SSH connection, requests a PTY and a shell,
// then starts the pump goroutines. It returns once the shell channel is up.
// ConnectShell opens one PTY session multiplexed over the shared SSH client
// of an established connection. All sessions of one connection share the
// single TCP/SSH channel, so closing one session never disturbs the others —
// which dialing a fresh TCP connection per session could not guarantee.
// onClosed (optional) fires exactly once when the session ends, from its own
// goroutine, set here BEFORE the pumps start so even an immediate EOF cannot
// slip past it.
func ConnectShell(shared *ssh.Client, config ConnSpec, cols, rows int, emitter *dbxpluginsdk.Emitter, onClosed func(*Shell)) (*Shell, error) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	session, err := shared.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	// ECHO only. IGNCR must never be requested: on a POSIX tty it makes the
	// line discipline discard every CR on input, and the CR is exactly what
	// the xterm front end sends for Enter — commands would never execute.
	// Windows OpenSSH ignores the flag, which is why the breakage only
	// showed against Linux servers.
	modes := ssh.TerminalModes{
		ssh.ECHO: 1,
	}
	if err := session.RequestPty("xterm-256color", cols, rows, modes); err != nil {
		session.Close()
		return nil, fmt.Errorf("request pty: %w", err)
	}

	shell := &Shell{
		ID:        NewID("sh"),
		Config:    config,
		CreatedAt: time.Now(),
		emitter:   emitter,
		session:   session,
		writeCh:   make(chan []byte, 64),
		onClosed:  onClosed,
		lastCols:  cols,
		lastRows:  rows,
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		session.Close()
		return nil, err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		session.Close()
		return nil, err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		session.Close()
		return nil, err
	}

	var reader io.Reader = stdout
	var writer io.Writer = stdin
	switch strings.ToLower(config.Encoding) {
	case "", "utf-8", "utf8":
		// pass through
	case "gbk":
		reader = simplifiedchinese.GBK.NewDecoder().Reader(stdout)
		writer = simplifiedchinese.GBK.NewEncoder().Writer(stdin)
	case "gb18030":
		reader = simplifiedchinese.GB18030.NewDecoder().Reader(stdout)
		writer = simplifiedchinese.GB18030.NewEncoder().Writer(stdin)
	default:
		session.Close()
		return nil, fmt.Errorf("unsupported encoding: %s", config.Encoding)
	}

	if err := session.Shell(); err != nil {
		session.Close()
		return nil, fmt.Errorf("start shell: %w", err)
	}
	if config.StartupCmd != "" {
		if _, err := stdin.Write([]byte(config.StartupCmd + "\n")); err != nil {
			session.Close()
			return nil, fmt.Errorf("run startup command: %w", err)
		}
	}

	go shell.pumpOutput(reader)
	go shell.pumpOutput(stderr)
	go shell.pumpInput(writer)
	return shell, nil
}

// pumpOutput batches terminal output into micro-batches: a reader goroutine
// feeds a channel, and the pump flushes whatever accumulated within a
// flushWindow tick (or once flushMax bytes pile up). High-throughput scenes
// — build logs, cat of a big file — cut bridge frames by an order of
// magnitude, while interactive output still flushes within one window. The
// ring buffer and the host see the same byte stream; only the framing
// changes.
func (shell *Shell) pumpOutput(source io.Reader) {
	chunks := make(chan []byte, 64)
	go func() {
		buf := make([]byte, termChunk)
		for {
			count, err := source.Read(buf)
			if count > 0 {
				chunks <- append([]byte(nil), buf[:count]...)
			}
			if err != nil {
				close(chunks)
				return
			}
		}
	}()

	pending := make([]byte, 0, termChunk)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		// the hide window can emit bytes (the banner before the marker)
		// while still active, so only an EMPTY emit means "swallow this
		// flush" — readiness is the state machine's business, not ours
		emit, _ := shell.suppressChunk(pending)
		pending = pending[:0]
		if len(emit) == 0 {
			return
		}
		chunk := emit
		shell.ringMutex.Lock()
		if shell.ring.Len()+len(chunk) > ringCapacity {
			shell.ring.Next(ringCapacity / 4)
		}
		shell.ring.Write(chunk)
		shell.ringMutex.Unlock()
		// session log: same post-suppress stream as ring and host; replay
		// bypasses flush, so reconnects never double-log
		if sink := shell.logSink.Load(); sink != nil {
			_, _ = sink.Write(chunk)
		}
		if shell.emitter != nil {
			_ = shell.emitter.Binary("term/"+shell.ID, chunk)
		}
	}

	ticker := time.NewTicker(flushWindow)
	defer ticker.Stop()
	drained := false
	for !drained {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				drained = true
				break
			}
			pending = append(pending, chunk...)
			if len(pending) >= termChunk {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
	flush()
	shell.Close("connection closed")
	shell.emit("shell.closed", map[string]any{"shellId": shell.ID, "reason": shell.closeReason})
}

func (shell *Shell) pumpInput(sink io.Writer) {
	for chunk := range shell.writeCh {
		if _, err := sink.Write(chunk); err != nil {
			shell.writeErr.Store(err)
			shell.Close("write failed: " + err.Error())
			shell.emit("shell.closed", map[string]any{"shellId": shell.ID, "reason": shell.closeReason})
			return
		}
	}
}

// Write queues terminal input bytes; the dedicated pump preserves ordering.
func (shell *Shell) Write(data []byte) error {
	shell.closeMutex.Lock()
	defer shell.closeMutex.Unlock()
	if shell.closed.Load() {
		return errors.New("shell is closed")
	}
	select {
	case shell.writeCh <- append([]byte(nil), data...):
		return nil
	default:
		return errors.New("terminal input queue is full")
	}
}

// Replay pushes the ring-buffer tail so a freshly attached tab catches up.
// The tail is wrapped in cursor-home/erase sequences: the interactive shell
// is a full-screen app, so replayed text must not push the viewport down or
// it renders as a mostly-blank screen above the prompt.
func (shell *Shell) Replay() {
	shell.ringMutex.Lock()
	tail := append([]byte(nil), shell.ring.Bytes()...)
	shell.ringMutex.Unlock()
	if len(tail) == 0 || shell.emitter == nil {
		return
	}
	payload := append([]byte("\x1b[2J\x1b[H"), tail...)
	payload = append(payload, []byte("\x1b[2004h")...)
	_ = shell.emitter.Binary("term/"+shell.ID, payload)
}

func (shell *Shell) Resize(cols, rows int) error {
	if shell.Closed() || shell.session == nil {
		return errors.New("shell is closed")
	}
	shell.suppressMutex.Lock()
	shell.lastCols, shell.lastRows = cols, rows
	shell.suppressMutex.Unlock()
	_, err := shell.session.SendRequest("window-change", false, ssh.Marshal(struct {
		W, H, X, Y uint32
	}{W: uint32(cols), H: uint32(rows)}))
	return err
}

// SetHideRedrawOnClose arms the post-hide cursor fixup for shells whose line
// editor repaints on WINCH (the POSIX family).
func (shell *Shell) SetHideRedrawOnClose(on bool) {
	shell.suppressMutex.Lock()
	shell.hideRedrawOnClose = on
	shell.suppressMutex.Unlock()
}
