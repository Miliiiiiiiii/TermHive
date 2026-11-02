package main

// Session logging: mirror each PTY's post-processed output stream (already
// UTF-8, already echo-hide-filtered — the injected OSC 7 hook never lands in
// a log) to <dataDir>/logs/<user@host_port>/<timestamp>-<shellID>.log with a
// per-line timestamp, SecureCRT-style. Raw ANSI is preserved so the log is a
// faithful record of what the terminal rendered.
//
// Design notes:
//   - The sink hooks into pumpOutput's flush closure, next to the ring write;
//     Replay() bypasses flush, so a reconnect replay is never double-logged.
//   - Line timestamps are stamped as lines COMPLETE (a partial trailing line
//     is carried until its newline arrives — possibly in a later chunk).
//   - A single session's log is capped (64 MiB): long-lived servers shouldn't
//     quietly fill the disk; rotation is a later feature, not a silent one.
//   - Errors are swallowed by design: the log sink must never disturb the
//     terminal stream it observes.

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const sessionLogCap = 64 * 1024 * 1024

// sessionLogSink is one shell's log file. Safe for the two racing pump
// goroutines (stdout+stderr both flush into it) plus the closer.
type sessionLogSink struct {
	mu      sync.Mutex
	file    *os.File
	writer  *bufio.Writer
	carry   []byte
	path    string
	written int64
	stopped bool
	clock   func() time.Time // injectable for tests
}

// sanitizeLogName makes a filesystem-safe connection identity. The DBX
// connection id is NOT stable across host re-issues; user@host_port (the
// sameEndpoint identity) is what survives them.
func sanitizeLogName(spec ConnSpec) string {
	name := fmt.Sprintf("%s@%s_%d", spec.Username, spec.Host, spec.Port)
	var builder strings.Builder
	for _, r := range name {
		if r == ':' || r == '\\' || r == '/' || r < 0x20 || r == 0x7f {
			r = '_'
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func openSessionLogSink(dataDir string, spec ConnSpec, shellID string, now time.Time) (*sessionLogSink, error) {
	dir := filepath.Join(dataDir, "logs", sanitizeLogName(spec))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, now.Format("20060102-150405")+"-"+shellID+".log")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &sessionLogSink{
		file:   file,
		writer: bufio.NewWriterSize(file, 64*1024),
		path:   path,
		clock:  time.Now,
	}, nil
}

// Write consumes one flushed chunk, stamping each completed line. io.Writer
// so it drops straight into the flush closure.
func (sink *sessionLogSink) Write(chunk []byte) (int, error) {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.stopped || sink.file == nil || sink.written >= sessionLogCap {
		if !sink.stopped && sink.file != nil {
			sink.stopped = true
			fmt.Fprintf(sink.writer, "[%s] %s\n",
				sink.now().Format("2006-01-02 15:04:05.000"),
				"termhive: session log size cap reached, logging stopped")
			_ = sink.writer.Flush()
		}
		return len(chunk), nil
	}
	sink.carry = append(sink.carry, chunk...)
	for {
		end := bytes.IndexByte(sink.carry, '\n')
		if end < 0 {
			break
		}
		line := sink.carry[:end+1]
		sink.carry = sink.carry[end+1:]
		stamp := sink.now().Format("[2006-01-02 15:04:05.000] ")
		n, _ := sink.writer.WriteString(stamp + string(line))
		sink.written += int64(n)
	}
	// Session output is low-volume and must be on disk while the session is
	// still running (a crash is exactly when the log matters), so flush per
	// chunk rather than buffering 64 KiB behind a bufio.
	_ = sink.writer.Flush()
	return len(chunk), nil
}

func (sink *sessionLogSink) now() time.Time {
	if sink.clock != nil {
		return sink.clock()
	}
	return time.Now()
}

// Close flushes the buffered tail and the carried partial line (stamped as a
// line of its own — it never got its newline).
func (sink *sessionLogSink) Close() error {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.file == nil {
		return nil
	}
	if len(sink.carry) > 0 && !sink.stopped {
		stamp := sink.now().Format("[2006-01-02 15:04:05.000] ")
		n, _ := sink.writer.WriteString(stamp + string(sink.carry) + "\n")
		sink.written += int64(n)
		sink.carry = nil
	}
	err := sink.writer.Flush()
	sink.file.Close()
	sink.file = nil
	return err
}

// ---- manager integration ----

// sessionLogEnabled: the feature is OFF unless explicitly turned on (logging
// terminal output to disk is a privacy-relevant opt-in).
func (manager *Manager) sessionLogEnabled() bool {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	value, ok := manager.settings["sessionLog"].(bool)
	return ok && value
}

// attachSessionLog opens a sink for a live shell and stores it; the flush
// closure picks it up on the next chunk.
func (manager *Manager) attachSessionLog(shell *Shell) {
	if manager.dataDir == "" || shell.logSink.Load() != nil {
		return
	}
	sink, err := openSessionLogSink(manager.dataDir, shell.Config, shell.ID, time.Now())
	if err != nil {
		manager.writeDiag("会话日志开启失败: " + err.Error())
		return
	}
	shell.logSink.Store(sink)
}

// detachSessionLog closes and clears a shell's sink.
func (manager *Manager) detachSessionLog(shell *Shell) {
	if sink := shell.logSink.Swap(nil); sink != nil {
		_ = sink.Close()
	}
}

// applySessionLogSetting reacts to a settings toggle by attaching or
// detaching sinks on every shell we still hold.
func (manager *Manager) applySessionLogSetting() {
	enabled := manager.sessionLogEnabled()
	manager.mutex.Lock()
	shells := make([]*Shell, 0, len(manager.shells))
	for _, shell := range manager.shells {
		shells = append(shells, shell)
	}
	manager.mutex.Unlock()
	for _, shell := range shells {
		if enabled {
			manager.attachSessionLog(shell)
		} else {
			manager.detachSessionLog(shell)
		}
	}
}
