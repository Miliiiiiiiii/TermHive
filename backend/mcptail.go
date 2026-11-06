package main

// Background log-follow tasks for the MCP tool set: tail_start / tail_read /
// tail_stop. The host's mcp/call budget (~120s) forbids a blocking follow, so
// the model gets a poll model instead: tail_start launches `tail -F` or
// `journalctl -f` on a fresh exec channel, output lands in a keep-tail buffer,
// and tail_read drains the new bytes and returns immediately. All three tools
// are annotated read-only — the claim is honest because the command line is
// NEVER taken from the caller: it is assembled here from a validated path or
// unit name into one of exactly two shapes (`tail -f` can never be expressed
// through the free-form exec_readonly tool, where the flag is deny-listed).
// Tasks die with their connection (the watcher selects on conn.done), are
// bounded per connection, and reclaim themselves if nobody polls them.

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	tailMaxPerConn   = 2                       // live follow tasks per connection
	tailBufferCap    = 256 * 1024              // unread bytes kept before dropping the head
	tailIdleTimeout  = 2 * time.Minute         // no tail_read for this long → reclaim
	tailStartGrace   = 1500 * time.Millisecond // immediate-exit detection window
	tailLinesDefault = 100
	tailLinesMax     = 10000
)

// tailBuffer accumulates unread output, keeping the TAIL when it overflows
// (the newest bytes are what a log reader wants) and counting what was
// dropped so tail_read can report `skipped` — the model learns it missed
// content instead of silently seeing a seam. Safe for concurrent use: the
// session's stdout and stderr writers race; readers poll separately.
type tailBuffer struct {
	mu      sync.Mutex
	data    []byte
	skipped int64
}

// Write makes tailBuffer an io.Writer for session.Stdout/Stderr.
func (buf *tailBuffer) Write(chunk []byte) (int, error) {
	if len(chunk) == 0 {
		return 0, nil
	}
	buf.mu.Lock()
	defer buf.mu.Unlock()
	total := len(buf.data) + len(chunk)
	if total > tailBufferCap {
		excess := total - tailBufferCap
		buf.skipped += int64(excess)
		if excess >= len(buf.data) {
			// chunk alone fills (or exceeds) the cap: keep its tail only
			buf.data = append(buf.data[:0], chunk[len(chunk)-tailBufferCap:]...)
		} else {
			copy(buf.data, buf.data[excess:])
			buf.data = buf.data[:len(buf.data)-excess]
			buf.data = append(buf.data, chunk...)
		}
	} else {
		buf.data = append(buf.data, chunk...)
	}
	return len(chunk), nil
}

// take drains up to max bytes and reports how many were skipped over since
// the previous take (then resets that counter). The returned slice is owned
// by the caller.
func (buf *tailBuffer) take(max int) ([]byte, int64) {
	buf.mu.Lock()
	defer buf.mu.Unlock()
	skipped := buf.skipped
	buf.skipped = 0
	if max <= 0 || len(buf.data) <= max {
		out := buf.data
		buf.data = nil
		return out, skipped
	}
	out := make([]byte, max)
	copy(out, buf.data[:max])
	buf.data = buf.data[max:]
	return out, skipped
}

// tailTask is one live follow stream. Lifetime: created by tail_start,
// registered in Manager.tailTasks; the run goroutine ends it when the remote
// program exits; the watcher goroutine kills the session when the owning
// connection closes; the idle timer reclaims tasks nobody polls. A finished
// task STAYS registered (alive=false) until drained and stopped so the model
// can read the tail end and see why the stream ended.
type tailTask struct {
	id        string
	connID    string
	conn      *Conn
	session   *ssh.Session
	createdAt time.Time

	buf tailBuffer

	done     chan struct{}
	doneOnce sync.Once
	exit     atomic.Value // string, set before done closes

	lastRead  time.Time // guarded by Manager.mutex
	idleTimer *time.Timer
}

// finish records the stream's end exactly once; the channel close publishes
// exit to readers (happens-before via close).
func (task *tailTask) finish(err error) {
	reason := ""
	if err != nil {
		var exitErr *ssh.ExitError
		var missingErr *ssh.ExitMissingError
		switch {
		case errors.As(err, &exitErr):
			reason = fmt.Sprintf("exit status %d", exitErr.ExitStatus())
		case errors.As(err, &missingErr):
			reason = "no exit status"
		default:
			reason = err.Error()
		}
	}
	task.doneOnce.Do(func() {
		task.exit.Store(reason)
		close(task.done)
	})
}

func (task *tailTask) exitReason() string {
	if value, ok := task.exit.Load().(string); ok {
		return value
	}
	return ""
}

func (task *tailTask) alive() bool {
	select {
	case <-task.done:
		return false
	default:
		return true
	}
}

var journalUnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@:+\\-]*$`)

// posixSingleQuote wraps s in single quotes, the one POSIX quoting form where
// everything but the quote itself is literal; embedded quotes close, escape
// and reopen ('\” is the standard idiom). PowerShell/cmd connections are
// refused earlier, so POSIX rules are the only ones that apply.
func posixSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tailFollowCommand assembles the ONLY two command shapes the follow tools
// ever run. `--` keeps a leading-dash path from becoming a flag; -F (follow by
// name, retry) survives log rotation where -f would sit on a renamed inode.
func tailFollowCommand(target, unit string, lines int) string {
	if unit != "" {
		return fmt.Sprintf("journalctl -u %s -n %d -f --no-pager", posixSingleQuote(unit), lines)
	}
	return fmt.Sprintf("tail -n %d -F -- %s", lines, posixSingleQuote(target))
}

// registerTailTask enforces the per-connection live-task bound under the
// registry lock and inserts the task. Only LIVE tasks count toward the bound
// (each holds a real SSH channel); finished-but-undrained tasks are bounded
// separately by a total cap so a model that keeps abandoning dead streams
// cannot grow the registry forever — the oldest finished one is evicted then.
func (manager *Manager) registerTailTask(task *tailTask) error {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	live, total := 0, 0
	var oldestFinished *tailTask
	for _, existing := range manager.tailTasks {
		if existing.connID != task.connID {
			continue
		}
		total++
		if existing.alive() {
			live++
			continue
		}
		if oldestFinished == nil || existing.createdAt.Before(oldestFinished.createdAt) {
			oldestFinished = existing
		}
	}
	if live >= tailMaxPerConn {
		return fmt.Errorf("connection already has %d live follow tasks; stop one with tail_stop first", live)
	}
	if total >= tailMaxPerConn*2 && oldestFinished != nil {
		delete(manager.tailTasks, oldestFinished.id)
		// writeDiag takes diagMutex (not manager.mutex), so this is safe here.
		manager.writeDiag("mcp tail: 自动挤掉已结束的跟随任务 " + oldestFinished.id)
	}
	manager.tailTasks[task.id] = task
	return nil
}

// reapTailTask removes one task by id; idle=true only reclaims it when the
// reader really went quiet (a read racing the timer re-arms instead). The
// timer callback and tail_read both run under the registry lock, so the
// lastRead check cannot tear.
func (manager *Manager) reapTailTask(id string, idle bool) {
	manager.mutex.Lock()
	task := manager.tailTasks[id]
	if task == nil {
		manager.mutex.Unlock()
		return
	}
	if idle && time.Since(task.lastRead) < tailIdleTimeout {
		manager.mutex.Unlock()
		if task.idleTimer != nil {
			task.idleTimer.Reset(tailIdleTimeout)
		}
		return
	}
	delete(manager.tailTasks, id)
	manager.mutex.Unlock()
	if task.idleTimer != nil {
		task.idleTimer.Stop()
	}
	if task.session != nil {
		task.session.Close()
	}
}

// stopTailTasksForConn tears down every follow task of one connection — the
// user disconnected it, so nothing of it may keep running.
func (manager *Manager) stopTailTasksForConn(connID string) {
	manager.mutex.Lock()
	var doomed []*tailTask
	for id, task := range manager.tailTasks {
		if task.connID == connID {
			doomed = append(doomed, task)
			delete(manager.tailTasks, id)
		}
	}
	manager.mutex.Unlock()
	for _, task := range doomed {
		if task.idleTimer != nil {
			task.idleTimer.Stop()
		}
		if task.session != nil {
			task.session.Close()
		}
	}
}

// ---- handlers ----

func mcpTailStart(ctx *mcpContext, args map[string]any) (string, error) {
	if kind := ctx.conn.detectShellKind(); kind == kindCmd || kind == kindPowerShell {
		return "", fmt.Errorf("log follow needs POSIX programs (tail/journalctl); this connection's shell family is %q", kind)
	}
	path := cleanRemote(str(args["path"]))
	unit := strings.TrimSpace(str(args["unit"]))
	// cleanRemote("") is ".", so the emptiness test must treat "." as absent
	hasPath := path != "" && path != "."
	if !hasPath && unit == "" {
		return "", errors.New("pass path (a log file) or unit (a systemd unit), not neither")
	}
	if hasPath && unit != "" {
		return "", errors.New("pass either path or unit, not both")
	}
	if unit != "" && !journalUnitPattern.MatchString(unit) {
		return "", errors.New("invalid unit name: " + unit)
	}
	lines := intOf(args["lines"])
	if lines <= 0 {
		lines = tailLinesDefault
	}
	if lines > tailLinesMax {
		lines = tailLinesMax
	}
	command := tailFollowCommand(path, unit, lines)
	if len(command) > mcpMaxCommandLength {
		return "", fmt.Errorf("path too long (command would exceed %d bytes)", mcpMaxCommandLength)
	}
	if ctx.conn.Dead() {
		return "", errors.New("connection is closed")
	}
	session, err := ctx.conn.client.NewSession()
	if err != nil {
		return "", err
	}

	task := &tailTask{
		id:        NewID("tf"),
		connID:    ctx.conn.Spec.ConnectionID,
		conn:      ctx.conn,
		session:   session,
		createdAt: time.Now(),
		done:      make(chan struct{}),
		lastRead:  time.Now(),
	}
	if err := ctx.manager.registerTailTask(task); err != nil {
		session.Close()
		return "", err
	}
	session.Stdout = &task.buf
	session.Stderr = &task.buf
	go func() { task.finish(session.Run(command)) }()
	// The watcher turns connection death into session death (Run unblocks
	// with an error) so the task can never outlive its transport.
	go func() {
		select {
		case <-task.conn.done:
			task.session.Close()
		case <-task.done:
		}
	}()

	// Grace window: a follow command that dies immediately (journalctl
	// without journals, tail blocked by missing permissions on some
	// systems) is reported as a failed start carrying whatever the program
	// printed, instead of handing the model a taskId that reads "ended".
	select {
	case <-task.done:
		ctx.manager.reapTailTask(task.id, false)
		tail, _ := task.buf.take(tailBufferCap)
		reason := task.exitReason()
		if reason == "" {
			reason = "stream ended"
		}
		return "", fmt.Errorf("follow stream ended immediately (%s); output:\n%s",
			reason, strings.ToValidUTF8(string(tail), "�"))
	case <-time.After(tailStartGrace):
	}
	task.idleTimer = time.AfterFunc(tailIdleTimeout, func() {
		ctx.manager.reapTailTask(task.id, true)
	})

	target, mode := path, "file"
	if unit != "" {
		target, mode = unit, "journal"
	}
	payload, _ := json.Marshal(map[string]any{
		"taskId":     task.id,
		"connection": ctx.conn.Spec.Name,
		"mode":       mode,
		"target":     target,
		"command":    command,
		"hint":       "poll tail_read every few seconds (it returns at once); finish with tail_stop; unread output is buffered (oldest dropped past 256 KiB); unused tasks reclaim after ~2 min",
	})
	return string(payload), nil
}

// tailTaskFor resolves and claims a task: it must exist and belong to the
// caller's connection — a task id from another connection is refused, not
// borrowed, so one server's log stream can never leak through another's tool
// call. Claiming also stamps lastRead and re-arms the idle timer.
func (ctx *mcpContext) tailTaskFor(id string) (*tailTask, error) {
	if id == "" {
		return nil, errors.New("taskId is required")
	}
	ctx.manager.mutex.Lock()
	task := ctx.manager.tailTasks[id]
	if task == nil || task.connID != ctx.conn.Spec.ConnectionID {
		ctx.manager.mutex.Unlock()
		return nil, errors.New("no follow task " + id + " on this connection")
	}
	task.lastRead = time.Now()
	ctx.manager.mutex.Unlock()
	if task.idleTimer != nil {
		task.idleTimer.Reset(tailIdleTimeout)
	}
	return task, nil
}

func mcpTailRead(ctx *mcpContext, args map[string]any) (string, error) {
	task, err := ctx.tailTaskFor(str(args["taskId"]))
	if err != nil {
		return "", err
	}
	limit := intOf(args["maxBytes"])
	if limit <= 0 {
		limit = mcpReadOnlyOutputCap
	}
	if limit > mcpReadMaxBytes {
		limit = mcpReadMaxBytes
	}
	chunk, skipped := task.buf.take(limit)
	alive := task.alive()
	exit := task.exitReason()

	if len(chunk) == 0 {
		if alive {
			return "[follow " + task.id + ": no new output, stream alive]", nil
		}
		reason := exit
		if reason == "" {
			reason = "stream ended"
		}
		return "[follow " + task.id + ": ended (" + reason + "), no more output — stop polling]", nil
	}

	var header strings.Builder
	fmt.Fprintf(&header, "[follow %s | %d bytes", task.id, len(chunk))
	if skipped > 0 {
		fmt.Fprintf(&header, " | %d bytes skipped (buffer cap reached — older output was dropped)", skipped)
	}
	if !alive {
		reason := exit
		if reason == "" {
			reason = "stream ended"
		}
		fmt.Fprintf(&header, " | ENDED (%s)", reason)
	}
	header.WriteString("]\n")
	return header.String() + strings.ToValidUTF8(string(chunk), "�"), nil
}

func mcpTailStop(ctx *mcpContext, args map[string]any) (string, error) {
	task, err := ctx.tailTaskFor(str(args["taskId"]))
	if err != nil {
		return "", err
	}
	ctx.manager.reapTailTask(task.id, false)
	return "[stopped " + task.id + "]", nil
}
