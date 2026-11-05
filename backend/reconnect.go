package main

// Auto-reconnect supervisor: when a connection's transport dies, redial it
// with exponential backoff so an open tab heals itself instead of parking on
// the manual 重连 overlay. Two independent death notices feed it — the
// keepalive pump (Conn.onDead, up to one keepalive interval late) and every
// PTY's EOF (Shell.onClosed, usually within milliseconds) — and both are
// allowed to fire; the supervisor trusts neither, it probes. The supervisor
// never opens replacement shells: rebinding tabs is the UI's job (it listens
// for connection.reconnected and reuses its manual-reconnect path), and any
// later interaction self-heals through EnsureConnection anyway.
//
// The loop is deliberately conservative about what it fixes: it exits
// silently when the connection actually answers (a user-closed tab is not an
// outage), when the user disconnected the connection, when the setting is
// off, and permanently on auth failure or a host-key mismatch — auto-retrying
// either would hammer a server or, worse, train users to click through the
// MITM alarm.

import (
	"fmt"
	"strings"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// reconnectBackoffs is the wait before attempt N (2s→60s, then steady).
var reconnectBackoffs = []time.Duration{
	2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second,
}

const reconnectBackoffCap = 60 * time.Second

func reconnectBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(reconnectBackoffs) {
		return reconnectBackoffCap
	}
	return reconnectBackoffs[attempt-1]
}

// autoReconnectEnabled reads the plugin setting; the feature is on unless the
// user explicitly turned it off (absent key = default on).
func (manager *Manager) autoReconnectEnabled() bool {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	value, ok := manager.settings["autoReconnect"].(bool)
	return !ok || value
}

// defaultReconnectMaxAttempts bounds the supervisor: an unreachable server
// (VPN dropped, host down) would otherwise be retried forever — one attempt
// per backoff tick, for as long as the sidecar lives. After this many FAILED
// redials the supervisor gives up and hands the connection to the manual
// reconnect overlay.
const defaultReconnectMaxAttempts = 20

// autoReconnectMaxAttempts reads the plugin setting (default 20, clamped
// 1..500 — 0 would race the toggle off, absurd values just hammer servers).
func (manager *Manager) autoReconnectMaxAttempts() int {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	value, ok := manager.settings["autoReconnectMaxAttempts"].(float64)
	if !ok {
		return defaultReconnectMaxAttempts
	}
	attempts := int(value)
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 500 {
		attempts = 500
	}
	return attempts
}

func (manager *Manager) currentEmitter() *dbxpluginsdk.Emitter {
	manager.emitterMutex.Lock()
	defer manager.emitterMutex.Unlock()
	return manager.emitter
}

// connProbeAlive asks the transport directly: one keepalive round trip. This
// is what keeps a supervisor honest — PTY EOFs and keepalive failures are
// evidence, the probe is the verdict.
func connProbeAlive(conn *Conn) bool {
	if conn == nil || conn.Dead() {
		return false
	}
	_, _, err := conn.Client().SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}

// isPermanentConnectFailure: retrying cannot fix wrong credentials, and
// auto-retrying a host-key change would normalize dismissing the MITM alarm.
func isPermanentConnectFailure(err error) bool {
	if err == nil {
		return false
	}
	return isAuthFailure(err) ||
		strings.Contains(err.Error(), "主机密钥已变更") ||
		strings.Contains(err.Error(), "安全警报")
}

// notifyConnectionTrouble starts the supervisor for a connection unless one
// is already running for it (both death notices may arrive, and every shell
// closing reports in — dedupe is the registry's job, keyed by connection id).
func (manager *Manager) notifyConnectionTrouble(connectionID string) {
	if connectionID == "" {
		return
	}
	manager.mutex.Lock()
	if _, running := manager.reconnects[connectionID]; running {
		manager.mutex.Unlock()
		return
	}
	cancel := make(chan struct{})
	manager.reconnects[connectionID] = cancel
	manager.mutex.Unlock()
	go manager.superviseReconnect(connectionID, cancel)
}

// stopReconnectSupervisor cancels the connection's supervisor (disconnect,
// sidecar shutdown).
func (manager *Manager) stopReconnectSupervisor(connectionID string) {
	manager.mutex.Lock()
	cancel := manager.reconnects[connectionID]
	delete(manager.reconnects, connectionID)
	manager.mutex.Unlock()
	if cancel != nil {
		close(cancel)
	}
}

// superviseReconnect owns one connection's recovery. Sequence per attempt:
// gate (setting still on, connection still known), probe (alive → spurious
// notice or someone else fixed it already), announce, wait (cancellable
// backoff), redial. markDead forces manager.conn() past its cached-client
// fast path, which would otherwise keep handing back the corpse.
func (manager *Manager) superviseReconnect(connectionID string, cancel chan struct{}) {
	for attempt := 1; ; attempt++ {
		if !manager.autoReconnectEnabled() {
			return
		}
		manager.mutex.Lock()
		conn := manager.conns[connectionID]
		manager.mutex.Unlock()
		if conn == nil {
			return // user disconnected this connection
		}
		if connProbeAlive(conn) {
			// Transport answers: a shell ended (user close, one dropped
			// channel) but the connection is fine — nothing to fix. On a
			// later attempt this means someone else redialed (manual 重连
			// raced us); report the win once.
			if attempt > 1 {
				manager.emit("connection.reconnected", map[string]any{
					"connectionId": connectionID, "attempts": attempt - 1,
				})
			}
			manager.finishSupervision(connectionID)
			return
		}
		wait := reconnectBackoff(attempt)
		manager.emit("connection.reconnecting", map[string]any{
			"connectionId": connectionID,
			"attempt":      attempt,
			"nextInMs":     wait.Milliseconds(),
		})
		select {
		case <-cancel:
			return
		case <-time.After(wait):
		}
		if !manager.autoReconnectEnabled() {
			return
		}
		// Re-read after the wait: a manual reconnect may have already
		// replaced the corpse with a live client.
		manager.mutex.Lock()
		conn = manager.conns[connectionID]
		manager.mutex.Unlock()
		if conn == nil {
			return
		}
		if connProbeAlive(conn) {
			manager.emit("connection.reconnected", map[string]any{
				"connectionId": connectionID, "attempts": attempt,
			})
			manager.finishSupervision(connectionID)
			return
		}
		conn.markDead()
		if _, err := manager.conn(connectionID, manager.currentEmitter()); err != nil {
			if isPermanentConnectFailure(err) {
				manager.emit("connection.failed", map[string]any{
					"connectionId": connectionID, "reason": err.Error(),
				})
				manager.finishSupervision(connectionID)
				return
			}
			max := manager.autoReconnectMaxAttempts()
			manager.writeDiag(fmt.Sprintf("自动重连第 %d/%d 次尝试失败: %s", attempt, max, err.Error()))
			if attempt >= max {
				manager.emit("connection.failed", map[string]any{
					"connectionId": connectionID,
					"reason":       fmt.Sprintf("已连续重试 %d 次未成功，等待手动重连（次数上限可在设置页调整）", attempt),
				})
				manager.finishSupervision(connectionID)
				return
			}
			continue
		}
		manager.emit("connection.reconnected", map[string]any{
			"connectionId": connectionID, "attempts": attempt,
		})
		manager.finishSupervision(connectionID)
		return
	}
}

// finishSupervision drops this supervisor's registry entry. The cancel
// channel is intentionally NOT closed here: only stopReconnectSupervisor
// closes it, after deleting the entry it found — so a close can never repeat,
// and an exiting supervisor has nobody left to signal anyway.
func (manager *Manager) finishSupervision(connectionID string) {
	manager.mutex.Lock()
	delete(manager.reconnects, connectionID)
	manager.mutex.Unlock()
}
