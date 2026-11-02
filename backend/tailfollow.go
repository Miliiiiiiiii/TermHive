package main

// Log follow mode for the file panel: `tail/start` begins following a remote
// file and streams new bytes to the UI on the binary channel tail/<id>,
// gated by `tail/ready` exactly like downloads (frames nobody subscribed to
// are dropped by the UI demux, so the follower must not speak before the
// viewer is listening).
//
// Unlike the MCP follow tools (mcptail.go, which runs `tail -F` over an exec
// channel for the AI to poll), this one serves the human viewer: it polls
// SFTP by path every second — cross-platform, shell-independent, and immune
// to SFTP channel rebuilds because no handle is held between polls. Rotation
// and truncation both surface as size-shrink: the follower emits a marker and
// restarts from byte 0, which is the correct resume point for either.

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	followPollInterval = time.Second
	followChunk        = 128 * 1024 // per-frame cap, matches downChunk
	followInitialMax   = 128 * 1024 // initial tail window read at start
	followMaxPerConn   = 3
	followReadyTimeout = 60 * time.Second
)

// logFollower is one live tail -f on a remote file, keyed by id and owned by
// a connection. The offset is the absolute byte position already streamed;
// carry holds trailing bytes that might be half of a multibyte character so
// GBK/UTF-8 sequences split across polls decode cleanly (no U+FFFD seams).
type logFollower struct {
	id       string
	connID   string
	conn     *Conn
	path     string
	encoding string
	lines    int
	offset   int64
	carry    []byte

	stopCh  chan struct{}
	stopped bool // under Manager.mutex
	readyCh chan struct{}
	ready   bool // under Manager.mutex
}

// splitDecodable splits chunk at the widest point where everything before it
// decodes standalone: a trailing partial UTF-8 sequence or GBK lead byte is
// held back as carry until the next poll completes it.
func splitDecodable(encoding string, chunk []byte) (emit, carry []byte) {
	if len(chunk) == 0 {
		return nil, nil
	}
	switch strings.ToLower(encoding) {
	case "gbk", "gb18030":
		// Two-byte encodings. Trail bytes overlap the lead range, so the
		// ONLY reliable split is alignment: walk pairs from the chunk start
		// (alignment is consistent because every emit ends on a boundary)
		// and hold back a lead byte missing its trail. Four-byte GB18030
		// sequences walk as two pairs; a split one degrades to a replacement
		// character, which is acceptable for a log viewer.
		i := 0
		for i < len(chunk) {
			if chunk[i] >= 0x81 && chunk[i] <= 0xFE {
				if i+1 >= len(chunk) {
					return chunk[:i], chunk[i:]
				}
				i += 2
			} else {
				i++
			}
		}
		return chunk, nil
	default:
		// UTF-8: walk back at most 4 bytes to a lead byte and count whether
		// its continuation bytes all arrived.
		for back := 1; back <= 3 && back <= len(chunk); back++ {
			b := chunk[len(chunk)-back]
			if b&0xC0 == 0x80 { // continuation byte, keep walking
				continue
			}
			if b&0x80 == 0 { // ASCII lead: complete
				return chunk, nil
			}
			// multibyte lead: expects (back-1) more continuation bytes
			want := 0
			switch {
			case b&0xE0 == 0xC0:
				want = 2
			case b&0xF0 == 0xE0:
				want = 3
			case b&0xF8 == 0xF0:
				want = 4
			default:
				return chunk, nil // not a valid lead; let the decoder cope
			}
			if back >= want {
				return chunk, nil
			}
			return chunk[:len(chunk)-back], chunk[len(chunk)-back:]
		}
		return chunk, nil
	}
}

// readAt reads up to followChunk bytes from offset and returns them plus the
// new offset. The file is opened and closed per poll — one extra round trip
// per second is nothing for a log viewer, and it makes rotation-by-rename and
// SFTP channel rebuilds invisible.
func (follower *logFollower) readAt(offset int64) ([]byte, int64, error) {
	client, err := follower.conn.SFTP()
	if err != nil {
		return nil, offset, err
	}
	source, _, err := (&SftpFS{client: client}).OpenRead(follower.path)
	if err != nil {
		return nil, offset, err
	}
	defer source.Close()
	if seeker, ok := source.(io.Seeker); ok {
		if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
			return nil, offset, err
		}
	} else if _, err := io.CopyN(io.Discard, source, offset); err != nil {
		return nil, offset, err
	}
	buf := make([]byte, followChunk)
	count, err := io.ReadFull(source, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, offset, err
	}
	return buf[:count], offset + int64(count), nil
}

// StartFollow handles tail/start: registers a follower and launches its poll
// loop (which waits for tail/ready before its first emission).
func (manager *Manager) StartFollow(shellID, remotePath string, lines int) (string, error) {
	conn, err := manager.ConnForShell(shellID)
	if err != nil {
		return "", err
	}
	target := cleanRemote(remotePath)
	if target == "" || target == "." {
		return "", errors.New("path is required")
	}
	follower := &logFollower{
		id:       NewID("fw"),
		connID:   conn.Spec.ConnectionID,
		conn:     conn,
		path:     target,
		encoding: conn.Spec.Encoding,
		lines:    lines,
		stopCh:   make(chan struct{}),
		readyCh:  make(chan struct{}),
	}
	manager.mutex.Lock()
	live := 0
	for _, existing := range manager.followers {
		if existing.connID == follower.connID {
			live++
		}
	}
	if live >= followMaxPerConn {
		manager.mutex.Unlock()
		return "", fmt.Errorf("连接已有 %d 个跟随视图, 请先关闭一个", followMaxPerConn)
	}
	manager.followers[follower.id] = follower
	manager.mutex.Unlock()
	go follower.pump(manager)
	return follower.id, nil
}

// ReadyFollow unblocks the follower's first emission (the viewer subscribed).
func (manager *Manager) ReadyFollow(id string) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if follower := manager.followers[id]; follower != nil && !follower.ready {
		follower.ready = true
		close(follower.readyCh)
	}
}

// StopFollow handles tail/stop and every other teardown path.
func (manager *Manager) StopFollow(id string) {
	manager.mutex.Lock()
	follower := manager.followers[id]
	if follower == nil || follower.stopped {
		delete(manager.followers, id)
		manager.mutex.Unlock()
		return
	}
	follower.stopped = true
	delete(manager.followers, id)
	close(follower.stopCh)
	manager.mutex.Unlock()
}

// stopFollowersForConn tears down every follower of a disconnected
// connection.
func (manager *Manager) stopFollowersForConn(connID string) {
	manager.mutex.Lock()
	var doomed []*logFollower
	for id, follower := range manager.followers {
		if follower.connID == connID {
			follower.stopped = true
			doomed = append(doomed, follower)
			delete(manager.followers, id)
		}
	}
	manager.mutex.Unlock()
	for _, follower := range doomed {
		close(follower.stopCh)
	}
}

func (follower *logFollower) finish(manager *Manager, reason string) {
	manager.StopFollow(follower.id)
	manager.emit("tail.done", map[string]any{"taskId": follower.id, "reason": reason})
}

// emitFollow decodes (with carry) and pushes one chunk to the viewer.
func (follower *logFollower) emitFollow(manager *Manager, raw []byte) {
	if len(raw) == 0 {
		return
	}
	combined := append(follower.carry, raw...)
	follower.carry = nil
	emit, carry := splitDecodable(follower.encoding, combined)
	follower.carry = carry
	if len(emit) == 0 {
		return
	}
	decoded, err := decodeToUTF8(follower.encoding, emit)
	if err != nil {
		return
	}
	manager.emitBinary("tail/"+follower.id, decoded)
}

// pump is the follower's poll loop. Phases: wait for ready (viewer must be
// subscribed), stream the initial tail window, then poll for growth once a
// second until stopped, the connection dies, or the file becomes unreadable.
func (follower *logFollower) pump(manager *Manager) {
	select {
	case <-follower.readyCh:
	case <-follower.stopCh:
		follower.finish(manager, "canceled")
		return
	case <-time.After(followReadyTimeout):
		follower.finish(manager, "等待视图就绪超时")
		return
	case <-follower.conn.done:
		follower.finish(manager, "connection closed")
		return
	}

	// Initial window: the last followInitialMax bytes, cut to the last
	// `lines` complete lines (a viewer wants context, not a byte-precise
	// replay; 0/negative lines means "everything in the window").
	client, err := follower.conn.SFTP()
	if err != nil {
		follower.finish(manager, err.Error())
		return
	}
	entry, err := (&SftpFS{client: client}).Stat(follower.path)
	if err != nil {
		follower.finish(manager, err.Error())
		return
	}
	window := int64(followInitialMax)
	if entry.Size < window {
		window = entry.Size
	}
	follower.offset = entry.Size - window
	if raw, next, err := follower.readAt(follower.offset); err == nil {
		follower.offset = next
		if follower.lines > 0 {
			raw = lastLines(raw, follower.lines)
		}
		follower.emitFollow(manager, raw)
	} else {
		follower.finish(manager, err.Error())
		return
	}

	ticker := time.NewTicker(followPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-follower.stopCh:
			follower.finish(manager, "stopped") // idempotent: registry entry is already gone
			return
		case <-follower.conn.done:
			follower.finish(manager, "connection closed")
			return
		case <-ticker.C:
			client, err := follower.conn.SFTP()
			if err != nil {
				follower.finish(manager, "connection closed")
				return
			}
			entry, err := (&SftpFS{client: client}).Stat(follower.path)
			if err != nil {
				// rotated away and not yet replaced — keep watching quietly
				continue
			}
			if entry.Size < follower.offset {
				// truncation or rotation-by-rename: restart from the top
				manager.emitBinary("tail/"+follower.id,
					[]byte("\n[termhive: 文件已轮转/截断, 从头继续跟随]\n"))
				follower.offset = 0
				follower.carry = nil
			}
			if entry.Size > follower.offset {
				raw, next, err := follower.readAt(follower.offset)
				if err != nil {
					follower.finish(manager, err.Error())
					return
				}
				follower.offset = next
				follower.emitFollow(manager, raw)
			}
		}
	}
}

// lastLines keeps the trailing `lines` complete lines (plus any unterminated
// tail, which is the freshest output).
func lastLines(raw []byte, lines int) []byte {
	if lines <= 0 || len(raw) == 0 {
		return raw
	}
	ends := 0
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == '\n' {
			ends++
			if ends >= lines {
				return raw[i+1:]
			}
		}
	}
	return raw
}
