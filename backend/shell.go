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
	hideStart     []byte
	hideEnd       []byte
	hideDeadline  time.Time
	hideBuf       []byte

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
// drops every paint region containing startMarker (the echoed hook line and
// any repaint/redraw copies of it) and closes at endMarker — the first LIVE
// OSC 7 escape sequence, which only exists once the hook actually took effect
// (the echoed line carries it as literal "$E]7;" / '\033]7;' text, never the
// escape byte, so repaints cannot spoof the close). Bytes outside marker
// regions — the user's keystrokes included — flow live the whole time; the
// window self-cancels after hideIdleWindow of silence.
func (shell *Shell) BeginHideWindow(startMarker, endMarker string) {
	shell.suppressMutex.Lock()
	defer shell.suppressMutex.Unlock()
	shell.hideStart = []byte(startMarker)
	shell.hideEnd = []byte(endMarker)
	shell.hideDeadline = time.Now().Add(hideIdleWindow)
	shell.hideBuf = nil
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
// and every region WITHOUT the hook marker flows out live (the user's
// keystrokes must never be held back or dropped just because the window is
// open); regions WITH the marker — the echoed hook line and any repaint or
// readline-redraw copies of it — are dropped. CR and EL boundaries are what
// keep a redraw copy from swallowing the user bytes that follow it. The first
// LIVE OSC 7 trigger closes the window: its region is emitted (the follow
// feature listens for those bytes), along with everything behind it.
// final=true is the idle-expiry variant: the trailing incomplete region is
// decided now (dropped only if it holds the marker) instead of being held for
// more bytes. Returns the bytes to emit, the unconsumed tail (only when
// !done), done.
func filterOsc7Regions(buf, marker, trigger []byte, final bool) (kept, tail []byte, done bool) {
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
			if bytes.Contains(region, marker) {
				kept = append(kept, region[at:]...)
			} else {
				kept = append(kept, region...)
			}
			return kept, nil, true
		}
		if !bytes.Contains(region, marker) {
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
		if bytes.Contains(tail[:at], marker) {
			kept = append(kept, tail[at:]...)
		} else {
			kept = append(kept, tail...)
		}
		return kept, nil, true
	}
	if final {
		if !bytes.Contains(tail, marker) {
			kept = append(kept, tail...)
		}
		return kept, nil, true
	}
	return kept, tail, false
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
		held, _, _ := filterOsc7Regions(shell.hideBuf, shell.hideStart, shell.hideEnd, true)
		shell.hideStart = nil
		shell.hideEnd = nil
		shell.hideBuf = nil
		return append(held, chunk...), true
	}
	shell.hideBuf = append(shell.hideBuf, chunk...)
	kept, rest, closed := filterOsc7Regions(shell.hideBuf, shell.hideStart, shell.hideEnd, false)
	if closed {
		shell.hideStart = nil
		shell.hideEnd = nil
		shell.hideBuf = nil
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
		out, _, _ := filterOsc7Regions(held, marker, end, true)
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
	_, err := shell.session.SendRequest("window-change", false, ssh.Marshal(struct {
		W, H, X, Y uint32
	}{W: uint32(cols), H: uint32(rows)}))
	return err
}
