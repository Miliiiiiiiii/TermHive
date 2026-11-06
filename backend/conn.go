package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
	"golang.org/x/crypto/ssh"
)

// Conn is one authenticated SSH connection shared by every PTY session of a
// saved connection. Multiplexing all sessions over a single client is what
// makes closing one session independent of the others; it also gives SFTP
// and the cwd probe a stable transport. A keepalive pump watches the link.
type Conn struct {
	Spec      ConnSpec
	CreatedAt time.Time

	client *ssh.Client

	sftpMutex  sync.Mutex
	sftpClient *sftp.Client

	kindOnce   sync.Once
	shellKind  shellKind
	promptBase string

	// monitor sampling state: the previous /proc/stat and /proc/net/dev
	// readings the CPU percentage and network rates are differenced
	// against (see monitor.go)
	monitorMutex sync.Mutex
	lastCpu      cpuCounters
	lastNet      map[string]netCounters
	lastNetAt    time.Time

	closed atomic.Bool
	dead   atomic.Bool
	done   chan struct{}

	// onDead fires (own goroutine) when the keepalive pump concludes the
	// transport is gone — the auto-reconnect supervisor listens here.
	onDead func()
}

// DialConn establishes and authenticates the shared SSH client and starts
// its keepalive pump. Fingerprints are verified against the connection's
// HostKeyStore (TOFU).
func DialConn(spec ConnSpec, emitter *dbxpluginsdk.Emitter, hostKeys *HostKeyStore) (*Conn, error) {
	client, err := dialSSH(spec, hostKeys)
	if err != nil {
		return nil, err
	}
	conn := &Conn{
		Spec:      spec,
		CreatedAt: time.Now(),
		client:    client,
		done:      make(chan struct{}),
	}
	go conn.pumpKeepalive(emitter)
	return conn, nil
}

func (conn *Conn) Client() *ssh.Client { return conn.client }

// Dead reports whether the shared transport can no longer carry sessions.
func (conn *Conn) Dead() bool { return conn.dead.Load() }

// markDead flags the transport as gone without closing it, so the next
// manager.conn() redials instead of handing out the cached client. Used when
// evidence ahead of the keepalive (every PTY hitting EOF) says the link is
// down even though the keepalive round has not fired yet.
func (conn *Conn) markDead() { conn.dead.Store(true) }

func (conn *Conn) Close() {
	if !conn.closed.CompareAndSwap(false, true) {
		return
	}
	close(conn.done)
	conn.sftpMutex.Lock()
	if conn.sftpClient != nil {
		conn.sftpClient.Close()
		conn.sftpClient = nil
	}
	conn.sftpMutex.Unlock()
	if conn.client != nil {
		conn.client.Close()
	}
}

func (conn *Conn) pumpKeepalive(emitter *dbxpluginsdk.Emitter) {
	interval := conn.Spec.KeepaliveSecs
	if interval <= 0 {
		interval = 30
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-conn.done:
			return
		case <-ticker.C:
			if _, _, err := conn.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				conn.dead.Store(true)
				if emitter != nil {
					_ = emitter.Event("connection.dead", map[string]any{
						"connectionId": conn.Spec.ConnectionID,
						"reason":       err.Error(),
					})
				}
				if conn.onDead != nil {
					go conn.onDead()
				}
				return
			}
		}
	}
}

// sftpHealthy probes the cached SFTP channel: any server-side answer proves
// the subsystem transport is alive; only transport death (EOF, lost
// connection) marks it for rebuild. One cheap round trip per FS() call is
// negligible next to a user action, and it is what lets the file panel
// recover transparently after the SFTP channel drops while the SSH client
// itself is still usable.
func sftpHealthy(client *sftp.Client) bool {
	_, err := client.Stat(".")
	if err == nil {
		return true
	}
	text := err.Error()
	for _, marker := range []string{"connection lost", "EOF", "use of closed", "broken pipe"} {
		if strings.Contains(text, marker) {
			return false
		}
	}
	return true
}

// SFTP lazily opens one SFTP client over the shared SSH connection; every
// session tab's file panel funnels through this single subsystem channel.
// A dead channel is detected and rebuilt here, so callers never see stale
// handles after a subsystem-level drop.
func (conn *Conn) SFTP() (*sftp.Client, error) {
	conn.sftpMutex.Lock()
	defer conn.sftpMutex.Unlock()
	if conn.Dead() {
		return nil, errors.New("connection is closed")
	}
	if conn.sftpClient != nil {
		if sftpHealthy(conn.sftpClient) {
			return conn.sftpClient, nil
		}
		_ = conn.sftpClient.Close()
		conn.sftpClient = nil
	}
	client, err := sftp.NewClient(conn.client,
		sftp.UseConcurrentWrites(true),
		sftp.UseConcurrentReads(true))
	if err != nil {
		return nil, fmt.Errorf("open sftp: %w", err)
	}
	conn.sftpClient = client
	return client, nil
}

// ---- terminal follow (OSC 7 shell integration) ----
//
// A separate exec channel can NEVER observe the interactive shell's cwd —
// every SSH session is its own process starting in the home directory, so the
// old "poll pwd over exec" design could only ever compare home == home. The
// working mechanism is the one used by MobaXterm/VS Code: the interactive
// shell itself reports its cwd as an OSC 7 file:// URL whenever it renders a
// prompt. The sequences are invisible in the terminal and parsed by the UI.

type shellKind string

const (
	kindUnknown    shellKind = "unknown"
	kindBash       shellKind = "bash"
	kindZsh        shellKind = "zsh"
	kindFish       shellKind = "fish"
	kindPosixOther shellKind = "posix-other"
	kindPowerShell shellKind = "powershell"
	kindCmd        shellKind = "cmd"
)

// detectShellKind classifies the server's default shell once per connection
// (both exec requests and PTY shells run the same DefaultShell). Probed lazily
// on the first PTY so dialing is never delayed by it more than once.
func (conn *Conn) detectShellKind() shellKind {
	conn.kindOnce.Do(func() {
		conn.shellKind = conn.probeShellKind()
	})
	return conn.shellKind
}

func (conn *Conn) probeShellKind() shellKind {
	// POSIX servers answer uname; Windows shells fail with "not recognized".
	if out, err := conn.runCapture("uname -s"); err == nil && strings.TrimSpace(out) != "" {
		// One round trip: each candidate shell expands only its own version
		// var, so the echoed line identifies the login shell.
		if v, err := conn.runCapture(`echo "b:$BASH_VERSION z:$ZSH_VERSION f:$FISH_VERSION"`); err == nil {
			if fieldOf(v, "b:") != "" {
				return kindBash
			}
			if fieldOf(v, "z:") != "" {
				return kindZsh
			}
			if fieldOf(v, "f:") != "" {
				return kindFish
			}
		}
		return kindPosixOther
	}
	// Windows: PowerShell expands $PSHOME to its install path; cmd.exe echoes
	// the literal text back.
	if out, err := conn.runCapture("echo $PSHOME"); err == nil {
		trimmed := strings.TrimSpace(out)
		if trimmed != "" && trimmed != "$PSHOME" {
			return kindPowerShell
		}
		if trimmed == "$PSHOME" {
			// Windows OpenSSH seeds cmd sessions with a PROMPT env var
			// (e.g. "gordo@MILII $P$G"); read it so the injected hook can
			// PREPEND the OSC 7 report to the user's existing prompt style
			// instead of flattening it to plain $P$G.
			if base, err := conn.runCapture("echo %PROMPT%"); err == nil {
				base = strings.TrimSpace(base)
				if base != "" && base != "%PROMPT%" && !strings.ContainsAny(base, "\r\n") {
					conn.promptBase = base
				}
			}
			return kindCmd
		}
	}
	return kindUnknown
}

// integrationSnippet is the startup hook that makes the shell emit
// OSC 7 file:// URLs on every prompt. The tty echo of this line never reaches
// the terminal: injectFollowHook wraps it in a sentinel-suppressed output
// window, so no cls/clear is needed and the login banner stays untouched.
//
// History hygiene (pressing Up must surface ONLY the user's own commands):
// after installing the hook, the line clears the in-memory history list and
// re-reads it from the history file — the injected entry (never written to
// disk) evaporates and the list returns to its just-logged-in state, with
// ZERO residue on every bash/zsh version. `history -d $((HISTCMD))` was
// tried first: fine on bash 5, but CentOS 7's bash 4.2 mis-expands HISTCMD
// and the delete misses. A HISTCONTROL=ignorespace preset line was tried
// next: works, but leaves that one visible entry behind. The clear+re-read
// tail is guarded on a non-empty HISTFILE so a shell without a history file
// never loses its in-memory list; it runs at connect time when the user has
// typed nothing yet, so the reset is lossless. The leading space stays as a
// bonus for machines already ignoring space-prefixed commands. fish ignores
// space-prefixed commands by built-in default. All snippets are pure ASCII
// so GBK/GB18030 connections pass them through unchanged. printf
// '\033]7;file://%s\033\134' emits ESC ] 7 ; file:///current/path ESC \
// (empty host = localhost).
func (kind shellKind) integrationSnippet(promptBase string) string {
	switch kind {
	case kindBash:
		return ` __osc7(){ printf '\033]7;file://%s\033\134' "$PWD"; };PROMPT_COMMAND="__osc7${PROMPT_COMMAND:+;$PROMPT_COMMAND}";[ -n "$HISTFILE" ]&&{ history -c;history -r; }`
	case kindZsh:
		return ` __osc7(){ printf '\033]7;file://%s\033\134' "$PWD"; };precmd_functions=(__osc7 $precmd_functions);[[ -n "$HISTFILE" ]]&&{ fc -c;fc -R "$HISTFILE"; }`
	case kindFish:
		return ` function __osc7; printf '\033]7;file://%s\033\134' "$PWD"; end;functions -c fish_prompt __th7;function fish_prompt;__osc7;__th7;end`
	case kindPowerShell:
		return ` $global:__th7=$function:prompt;function global:prompt{$e=[char]27;$p=$ExecutionContext.SessionState.Path.CurrentFileSystemLocation.Path.Replace('\','/');[Console]::Write("$e]7;file:///$p$e\");& $global:__th7}`
	case kindCmd:
		// $P$G (or the server's seeded PROMPT value, e.g. "user@host $P$G")
		// keeps the visible prompt EXACTLY as it was; the OSC 7 preamble
		// rides along invisibly.
		if promptBase == "" {
			promptBase = "$P$G"
		}
		return " PROMPT $E]7;file:///$P$E\\" + promptBase
	}
	return ""
}

func fieldOf(line, prefix string) string {
	for _, field := range strings.Fields(line) {
		if strings.HasPrefix(field, prefix) {
			return strings.TrimPrefix(field, prefix)
		}
	}
	return ""
}

// osc7Trigger is the prefix of the live cwd report the hook makes the shell
// emit on every prompt (see integrationSnippet). As a hide-window close
// trigger it cannot be spoofed by the echoed hook line or by repaints of it
// (ConPTY full-screen re-emissions, readline redraws): the snippet carries
// the sequence as TEXT ("$E]7;file://" / '\033]7;file://'), while the escape
// bytes only exist once the shell actually renders a prompt with the hook
// installed — which is exactly when hiding should end.
const osc7Trigger = "\x1b]7;file://"

func (conn *Conn) runCapture(command string) (string, error) {
	session, err := conn.client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	raw, err := session.Output(command)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
