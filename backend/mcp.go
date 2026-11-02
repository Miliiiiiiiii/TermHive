package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
	"golang.org/x/crypto/ssh"
)

// The DBX host exposes plugin AI tools over the sidecar's regular JSON-RPC
// channel: `mcp/tools` (discovery, called lazily once a connection is open)
// and `mcp/call` (execution). The host enforces the outer contract — only
// connections the user has open are callable, every tool not annotated
// read-only pauses for desktop approval, results are truncated and audited.
// This file owns the plugin's share of the safety contract: the read-only
// classifier deciding which commands may run approval-free (readOnlyHint is
// this plugin's claim — keep it honest and conservative) and a hard
// blocklist of catastrophic patterns checked even after user approval.

const (
	mcpExecReadonlyTimeout = 30 * time.Second
	mcpMaxCommandLength    = 8192
	mcpReadOnlyOutputCap   = 32 * 1024
	mcpExecOutputCap       = 48 * 1024
	mcpWriteContentCap     = 256 * 1024
	mcpReadDefaultBytes    = 16 * 1024
	mcpReadMaxBytes        = 128 * 1024
)

// ---- protocol ----

type mcpTool struct {
	Name        string
	Description string
	ReadOnly    bool
	Properties  map[string]any
	Required    []string
	Handler     func(ctx *mcpContext, args map[string]any) (string, error)
}

type mcpContext struct {
	manager *Manager
	conn    *Conn
	emitter *dbxpluginsdk.Emitter
}

// connectionProperties are shared by every tool schema. dbx_connection is
// what the model fills to pick between open connections; connectionId is
// declared so the host strips model-supplied values and injects the real
// binding (the authoritative id the handler resolves first).
func connectionProperties() map[string]any {
	return map[string]any{
		"dbx_connection": map[string]any{
			"type":        "string",
			"description": "Name of the open TermHive connection to use. Optional when only one connection is open.",
		},
		"connectionId": map[string]any{
			"type":        "string",
			"description": "Injected by DBX; never set this yourself.",
		},
	}
}

func (tool *mcpTool) listing() map[string]any {
	properties := connectionProperties()
	for key, value := range tool.Properties {
		properties[key] = value
	}
	listing := map[string]any{
		"name":        tool.Name,
		"description": tool.Description,
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": properties,
			"required":   tool.Required,
		},
	}
	if tool.ReadOnly {
		listing["annotations"] = map[string]any{"readOnlyHint": true}
	}
	return listing
}

// MCPTools answers the host's discovery call. The catalog is the same for
// every connection; the connectionId param it passes is simply not needed.
func (manager *Manager) MCPTools(_ map[string]any) (any, *dbxpluginsdk.PluginError) {
	tools := make([]map[string]any, 0, len(mcpCatalog))
	for _, tool := range mcpCatalog {
		tools = append(tools, tool.listing())
	}
	return map[string]any{"tools": tools}, nil
}

// MCPCall executes one tool. Protocol-level failures (unknown tool) reply as
// JSON-RPC errors; tool-level failures reply as MCP isError results so the
// model sees the reason and can adjust.
func (manager *Manager) MCPCall(values map[string]any, emitter *dbxpluginsdk.Emitter) (any, *dbxpluginsdk.PluginError) {
	name := str(values["tool"])
	var tool *mcpTool
	for _, candidate := range mcpCatalog {
		if candidate.Name == name {
			tool = candidate
			break
		}
	}
	if tool == nil {
		return nil, dbxpluginsdk.NewError(-32602, "unknown mcp tool: "+name)
	}
	arguments, _ := values["arguments"].(map[string]any)
	if arguments == nil {
		arguments = map[string]any{}
	}
	lifecycle, _ := values["lifecycle"].(map[string]any)
	conn, err := manager.mcpConnection(arguments, lifecycle, emitter)
	if err != nil {
		return mcpTextResult(err.Error(), true), nil
	}
	text, err := tool.Handler(&mcpContext{manager: manager, conn: conn, emitter: emitter}, arguments)
	if err != nil {
		return mcpTextResult(err.Error(), true), nil
	}
	return mcpTextResult(text, false), nil
}

func mcpTextResult(text string, isErr bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isErr,
	}
}

// mcpConnection resolves which connection a tool call targets. The host
// binds each call to one connection: it injects the record id as
// connectionId and attaches the full record as lifecycle — the payload
// connection/connect gets. That spec is the authority on which server the
// id means and is dialed from directly: the MCP bridge runs its own sidecar
// of this plugin, whose spec cache can predate the connection, and a cache
// lookup that guessed (findSpec's single-spec fallback) would run the
// command on whichever server this sidecar happened to know. Without a
// lifecycle (older hosts) the id must match the cache exactly. Either way
// the shared client is redialed when it dropped — the same recovery a
// workbench tab gets after a sidecar restart.
func (manager *Manager) mcpConnection(arguments, lifecycle map[string]any, emitter *dbxpluginsdk.Emitter) (*Conn, error) {
	known, haveKnown := manager.lifecycleSpec(lifecycle)
	var candidates []string
	if id := str(arguments["connectionId"]); id != "" {
		candidates = append(candidates, id)
	}
	if named := str(arguments["dbx_connection"]); named != "" {
		candidates = append(candidates, manager.connectionIdByName(named)...)
		candidates = append(candidates, named)
	}
	if len(candidates) == 0 && haveKnown {
		candidates = append(candidates, known.ConnectionID)
	}
	if len(candidates) == 0 {
		if single := manager.singleLiveConnection(); single != "" {
			candidates = append(candidates, single)
		}
	}
	var lastErr error
	for _, id := range candidates {
		if id == "" {
			continue
		}
		var conn *Conn
		var err error
		if haveKnown && id == known.ConnectionID {
			conn, err = manager.conn(id, emitter, known)
		} else {
			conn, err = manager.connExact(id, emitter)
		}
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no open TermHive connection: open the SSH connection in DBX first")
}

// lifecycleSpec is the dial recipe the host attached to an mcp/call, with
// credentials the host left out filled from the cached spec of the same
// server (the workbench's sidecar stored them on connect).
func (manager *Manager) lifecycleSpec(lifecycle map[string]any) (ConnSpec, bool) {
	if lifecycle == nil {
		return ConnSpec{}, false
	}
	request, err := parseLifecycle(lifecycle)
	if err != nil {
		return ConnSpec{}, false
	}
	spec, err := request.spec()
	if err != nil {
		return ConnSpec{}, false
	}
	if spec.Auth.Password == "" && spec.Auth.KeyPath == "" {
		if cached, ok := manager.cachedSpec(spec.ConnectionID); ok && sameEndpoint(cached, spec) {
			spec.Auth = cached.Auth
		}
	}
	return spec, true
}

func (manager *Manager) connectionIdByName(named string) []string {
	needle := strings.ToLower(strings.TrimSpace(named))
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	ids := make([]string, 0, 2)
	for id, conn := range manager.conns {
		if strings.EqualFold(id, needle) || strings.EqualFold(conn.Spec.Name, needle) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		for id, spec := range manager.specs {
			if strings.EqualFold(id, needle) || strings.EqualFold(spec.Name, needle) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func (manager *Manager) singleLiveConnection() string {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if len(manager.conns) == 1 {
		for id := range manager.conns {
			return id
		}
	}
	return ""
}

// ---- exec tools ----

func (ctx *mcpContext) exec(command string, timeout time.Duration, cap int) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", errors.New("command is empty")
	}
	if len(command) > mcpMaxCommandLength {
		return "", fmt.Errorf("command too long (max %d bytes)", mcpMaxCommandLength)
	}
	if why := dangerousCommand(command); why != "" {
		return "", fmt.Errorf("blocked by TermHive safety gate: %s", why)
	}
	return ctx.conn.execCapture(command, timeout, cap)
}

func mcpExecReadonly(ctx *mcpContext, args map[string]any) (string, error) {
	command := str(args["command"])
	if err := isReadOnlyCommandFor(command, ctx.conn.detectShellKind()); err != nil {
		return "", fmt.Errorf("not a read-only command (%v); use the exec tool, which requires user approval", err)
	}
	return ctx.exec(command, mcpExecReadonlyTimeout, mcpReadOnlyOutputCap)
}

func mcpExec(ctx *mcpContext, args map[string]any) (string, error) {
	timeout := intOf(args["timeoutSecs"])
	if timeout <= 0 {
		timeout = 60
	}
	if timeout > 110 {
		timeout = 110 // stay under the host's 120s mcp/call budget
	}
	return ctx.exec(str(args["command"]), time.Duration(timeout)*time.Second, mcpExecOutputCap)
}

// cappedWriter keeps the first max bytes and swallows the rest, so a runaway
// `cat /dev/urandom` cannot balloon the sidecar's memory.
type cappedWriter struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (writer *cappedWriter) Write(chunk []byte) (int, error) {
	room := writer.max - writer.buf.Len()
	if room > 0 {
		if len(chunk) <= room {
			writer.buf.Write(chunk)
		} else {
			writer.buf.Write(chunk[:room])
			writer.truncated = true
		}
	} else if len(chunk) > 0 {
		writer.truncated = true
	}
	return len(chunk), nil
}

func (writer *cappedWriter) finish(suffix string) string {
	text := strings.ToValidUTF8(writer.buf.String(), "�")
	if writer.truncated {
		suffix += "\n[termhive: output truncated]"
	}
	return text + suffix
}

// execCapture runs one command on a fresh exec channel — the user's PTY is
// untouched — capturing combined output up to max bytes. The timeout closes
// the session, which unblocks the pending Run.
func (conn *Conn) execCapture(command string, timeout time.Duration, max int) (string, error) {
	if conn.Dead() {
		return "", errors.New("connection is closed")
	}
	session, err := conn.client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	output := &cappedWriter{max: max}
	session.Stdout = output
	session.Stderr = output
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(timeout):
		_ = session.Close()
		runErr = <-done
		return output.finish("\n[termhive: timed out after " + timeout.String() + ", partial output]"), nil
	}
	suffix := ""
	if runErr != nil {
		var exitErr *ssh.ExitError
		var missingErr *ssh.ExitMissingError
		switch {
		case errors.As(runErr, &exitErr):
			suffix = fmt.Sprintf("\n[termhive: exit status %d]", exitErr.ExitStatus())
		case errors.As(runErr, &missingErr):
			// server sent no exit status; the output is all we get
		default:
			return "", runErr
		}
	}
	return output.finish(suffix), nil
}

// ---- read-only classifier ----

// readOnlyAnyArgs: programs that only read state, with any arguments
// (redirection and substitution are rejected before this table is consulted).
// Deliberately excludes env/printenv/history (secret leakage), curl/wget
// (network), ping (open-ended), sudo, every package manager, and the
// general-purpose interpreters awk/sed (sed -i writes files; the read-only
// claim must stay provable, not case-by-case).
var readOnlyAnyArgs = map[string]bool{
	"ls": true, "cat": true, "head": true, "wc": true,
	"grep": true, "egrep": true, "fgrep": true,
	"df": true, "du": true, "ps": true, "free": true, "uname": true, "uptime": true,
	"id": true, "who": true, "whoami": true, "w": true, "stat": true, "file": true,
	"which": true, "whereis": true, "pwd": true, "echo": true, "date": true,
	"hostname": true, "lsblk": true, "lscpu": true, "lsusb": true, "lspci": true,
	"vmstat": true, "dig": true, "nslookup": true, "host": true,
	"ss": true, "netstat": true, "last": true, "lsof": true, "arch": true, "nproc": true,
	// pure text processors for pipeline tails (log slicing/counting)
	"sort": true, "uniq": true, "cut": true, "tr": true, "nl": true,
	// compressed-log readers and read-only inspection
	"zgrep": true, "zcat": true, "strings": true,
	"md5sum": true, "sha1sum": true, "sha256sum": true,
	// Windows (cmd.exe default shell on Windows OpenSSH)
	"dir": true, "type": true, "ver": true, "vol": true, "tasklist": true,
	"ipconfig": true, "systeminfo": true, "findstr": true,
	"quser": true, "qwinsta": true, "driverquery": true,
}

// readOnlySubcommands: programs allowed only with these first subcommands
// (leading global flags like --no-pager are skipped when locating it).
var readOnlySubcommands = map[string]map[string]bool{
	"systemctl": {
		"status": true, "show": true, "cat": true, "help": true,
		"list-units": true, "list-unit-files": true, "list-sockets": true, "list-timers": true,
		"is-active": true, "is-enabled": true, "is-failed": true,
	},
	"ip": {"addr": true, "address": true, "a": true, "link": true, "l": true,
		"route": true, "r": true, "neigh": true, "n": true, "rule": true},
	"git": {"status": true, "log": true, "diff": true, "show": true, "blame": true,
		"rev-parse": true, "ls-remote": true, "describe": true, "shortlog": true},
	"docker": {"ps": true, "images": true, "info": true, "version": true,
		"stats": true, "top": true, "logs": true, "inspect": true, "port": true},
	"podman": {"ps": true, "images": true, "info": true, "version": true,
		"stats": true, "top": true, "logs": true, "inspect": true, "port": true},
	"sc": {"query": true, "queryex": true},
}

// readOnlyFlagDenylist: allowed programs whose arguments must not contain
// these flags — they are the write/exit escape hatches of read commands.
var readOnlyFlagDenylist = map[string][]string{
	"tail":       {"-f", "--follow"},
	"find":       {"-delete", "-exec", "-execdir", "-ok", "-okdir", "-fls", "-fprint", "-fprintf", "-fprint0"},
	"journalctl": {"--vacuum", "--rotate", "--flush"},
}

// isReadOnlyCommandFor decides whether the whole command line may run without
// per-call approval on a server whose login shell is kind. Strict by design:
// whitelisted programs only, no redirection, no substitution, no command
// chaining into unknown territory — judged on the words the shell would
// hand to each program (quoting modeled per shell family by
// scanShellCommand), so `grep "a\|b"` is the literal pattern it is while
// `find x "-delete"` is still the delete it is. Anything rejected here can
// still run through the exec tool, which the host gates behind explicit
// user approval.
func isReadOnlyCommandFor(command string, kind shellKind) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return errors.New("empty command")
	}
	if len(command) > mcpMaxCommandLength {
		return errors.New("command too long")
	}
	segments, err := scanShellCommand(command, kind)
	if err != nil {
		return err
	}
	for _, words := range segments {
		if err := classifyReadOnlyWords(words); err != nil {
			return err
		}
	}
	return nil
}

// isReadOnlyCommand is isReadOnlyCommandFor with the shell unknown: only
// double quotes protect, which is the rule set every supported shell agrees on.
func isReadOnlyCommand(command string) error {
	return isReadOnlyCommandFor(command, kindUnknown)
}

func classifyReadOnlyWords(words []string) error {
	if len(words) == 0 {
		return nil
	}
	program := words[0]
	if index := strings.LastIndexAny(program, "/\\"); index >= 0 {
		program = program[index+1:]
	}
	_, anyArgs := readOnlyAnyArgs[program]
	subs, subGated := readOnlySubcommands[program]
	denied, flagGated := readOnlyFlagDenylist[program]
	switch {
	case anyArgs:
	case subGated:
		index := 1
		for index < len(words) && strings.HasPrefix(words[index], "-") {
			index++
		}
		if index >= len(words) || !subs[words[index]] {
			return fmt.Errorf("%s is only read-only with subcommands like status/show/list", program)
		}
	case flagGated:
	default:
		return fmt.Errorf("%q is not on the read-only allowlist", program)
	}
	for _, flag := range denied {
		for _, word := range words {
			if strings.HasPrefix(strings.ToLower(word), flag) {
				return fmt.Errorf("%s %s would not be read-only", program, word)
			}
		}
	}
	return nil
}

// ---- catastrophic-pattern blocklist ----

// dangerousPatterns are refused outright — before the approval dialog, so the
// user is never offered a yes/no on certain destruction. Heuristic by
// nature; the host approval gate remains the real control around exec.
var dangerousPatterns = []struct {
	pattern *regexp.Regexp
	why     string
}{
	{regexp.MustCompile(`(?i)\brm\s+[^;&|]*-[a-z]*[rf][a-z]*[rf][a-z]*\b[^;&|]*\s(/|~|\$HOME|\*)(\s|/|\*|$)`),
		"recursive rm aimed at a filesystem root or home"},
	{regexp.MustCompile(`(?i)\brm\s+[^;&|]*(-r\s+-f|-f\s+-r|--recursive\b[^;&|]*--force\b|--force\b[^;&|]*--recursive\b)[^;&|]*\s(/|~|\$HOME|\*)(\s|/|\*|$)`),
		"recursive rm aimed at a filesystem root or home"},
	{regexp.MustCompile(`(?i)\bmkfs(\.[a-z0-9]+)?\b`), "filesystem formatting"},
	{regexp.MustCompile(`(?i)\bdd\b[^;&|]*\bof=/dev/`), "raw write to a disk device"},
	{regexp.MustCompile(`(?i)\b(shred|blkdiscard|wipefs|badblocks)\b`), "disk wipe tool"},
	{regexp.MustCompile(`(?i)\bdiskutil\s+erase`), "disk erase"},
	{regexp.MustCompile(`(?i)[>|]{1,2}\s*/dev/(sd|nvme|vd|hd|mmcblk|disk)`), "redirect into a device"},
	{regexp.MustCompile(`(?i)\b(shutdown|reboot|halt|poweroff)\b`), "power state change"},
	{regexp.MustCompile(`(?i)\b(init|telinit)\s+[06]\b`), "runlevel change"},
	{regexp.MustCompile(`:\(\)\s*\{`), "fork bomb"},
	{regexp.MustCompile(`(?i)\bchmod\s+(-r|--recursive)\s+777\s+/(\s|$)`), "world-writable filesystem root"},
	{regexp.MustCompile(`(?i)\bchown\s+(-r|--recursive)\b[^;&|]*\s+/(\s|$)`), "recursive chown of the filesystem root"},
	{regexp.MustCompile(`(?i)[>|]{1,2}\s*/etc/(passwd|shadow|sudoers|fstab|hosts)\b`), "overwrite of a critical system file"},
	{regexp.MustCompile(`(?i)\bformat\s+[a-z]:`), "Windows format"},
	{regexp.MustCompile(`(?i)\b(rd|rmdir)\s+[^;&|]*\s?/s\b`), "Windows recursive directory delete"},
	{regexp.MustCompile(`(?i)\bvssadmin\b[^;&|]*\bdelete\b`), "Windows shadow copy delete"},
	{regexp.MustCompile(`(?i)\b(curl|wget)\b[^;&|]*\|\s*[^;&|]*\b(sh|bash|zsh|python3?|perl)\b`),
		"piping a download straight into an interpreter"},
}

func dangerousCommand(command string) string {
	for _, rule := range dangerousPatterns {
		if rule.pattern.MatchString(command) {
			return rule.why
		}
	}
	return ""
}

// ---- sftp tools ----

func (ctx *mcpContext) fs() (RemoteFS, ConnSpec, error) {
	client, err := ctx.conn.SFTP()
	if err != nil {
		return nil, ConnSpec{}, err
	}
	return &SftpFS{client: client}, ctx.conn.Spec, nil
}

func mcpSftpList(ctx *mcpContext, args map[string]any) (string, error) {
	fs, spec, err := ctx.fs()
	if err != nil {
		return "", err
	}
	target := cleanRemote(strDefault(args, "path", "."))
	entries, err := fs.List(target)
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]any{
		"connection": spec.Name,
		"path":       target,
		"entries":    entries,
	})
	return string(payload), nil
}

func mcpSftpStat(ctx *mcpContext, args map[string]any) (string, error) {
	fs, _, err := ctx.fs()
	if err != nil {
		return "", err
	}
	entry, err := fs.Stat(cleanRemote(str(args["path"])))
	if err != nil {
		return "", err
	}
	payload, _ := json.Marshal(entry)
	return string(payload), nil
}

func mcpSftpRead(ctx *mcpContext, args map[string]any) (string, error) {
	fs, spec, err := ctx.fs()
	if err != nil {
		return "", err
	}
	target := cleanRemote(str(args["path"]))
	if target == "" || target == "." {
		return "", errors.New("path is required")
	}
	offset := int64Of(args["offset"])
	if offset < 0 {
		offset = 0
	}
	limit := intOf(args["limitBytes"])
	if limit <= 0 {
		limit = mcpReadDefaultBytes
	}
	if limit > mcpReadMaxBytes {
		limit = mcpReadMaxBytes
	}
	reader, size, err := fs.OpenRead(target)
	if err != nil {
		return "", err
	}
	defer reader.Close()
	if seeker, ok := reader.(io.Seeker); ok {
		if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
			return "", err
		}
	} else if _, err := io.CopyN(io.Discard, reader, offset); err != nil {
		return "", err
	}
	raw := make([]byte, limit)
	count, readErr := io.ReadFull(reader, raw)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return "", readErr
	}
	raw = raw[:count]
	if bytes.IndexByte(raw, 0) != -1 {
		return fmt.Sprintf("[binary file %s: %d bytes total; use the TermHive file panel to download]", target, size), nil
	}
	decoded, err := decodeToUTF8(spec.Encoding, raw)
	if err != nil {
		return "", err
	}
	header := fmt.Sprintf("[file %s | %d bytes total | showing %d from offset %d]\n",
		target, size, count, offset)
	return header + strings.ToValidUTF8(string(decoded), "�"), nil
}

func mcpSftpWrite(ctx *mcpContext, args map[string]any) (string, error) {
	content := str(args["content"])
	if len(content) > mcpWriteContentCap {
		return "", fmt.Errorf("content too large for the AI write tool (max %d bytes); use the TermHive file panel to upload", mcpWriteContentCap)
	}
	target := cleanRemote(str(args["path"]))
	if err := guardWritablePath(target); err != nil {
		return "", err
	}
	fs, spec, err := ctx.fs()
	if err != nil {
		return "", err
	}
	encoded, err := encodeFromUTF8(spec.Encoding, []byte(content))
	if err != nil {
		return "", err
	}
	// temp + rename so a failed write never truncates the original
	temp := target + ".termhive.tmp"
	if err := writeAll(fs, temp, encoded); err != nil {
		_ = fs.Remove(temp)
		return "", explainWriteError(target, err)
	}
	if err := fs.RenameForce(temp, target); err != nil {
		_ = fs.Remove(temp)
		if err2 := writeAll(fs, target, encoded); err2 != nil {
			return "", err
		}
	}
	return fmt.Sprintf("[wrote %d bytes to %s]", len(encoded), target), nil
}

func mcpSftpMkdir(ctx *mcpContext, args map[string]any) (string, error) {
	target := cleanRemote(str(args["path"]))
	if err := guardWritablePath(target); err != nil {
		return "", err
	}
	fs, _, err := ctx.fs()
	if err != nil {
		return "", err
	}
	if boolOf(args["recursive"]) {
		err = fs.MkdirAll(target)
	} else {
		err = fs.Mkdir(target)
	}
	if err != nil {
		return "", err
	}
	return "[created " + target + "]", nil
}

func mcpSftpRemove(ctx *mcpContext, args map[string]any) (string, error) {
	recursive := boolOf(args["recursive"])
	target := cleanRemote(str(args["path"]))
	if err := guardRemovablePath(target, recursive); err != nil {
		return "", err
	}
	fs, _, err := ctx.fs()
	if err != nil {
		return "", err
	}
	if recursive {
		err = fs.RemoveAll(target)
	} else {
		err = fs.Remove(target)
	}
	if err != nil {
		return "", err
	}
	return "[removed " + target + "]", nil
}

func mcpSftpRename(ctx *mcpContext, args map[string]any) (string, error) {
	from, to := cleanRemote(str(args["from"])), cleanRemote(str(args["to"]))
	if err := guardWritablePath(from); err != nil {
		return "", err
	}
	if err := guardWritablePath(to); err != nil {
		return "", err
	}
	fs, _, err := ctx.fs()
	if err != nil {
		return "", err
	}
	if err := fs.RenameForce(from, to); err != nil {
		return "", err
	}
	return "[renamed " + from + " → " + to + "]", nil
}

// guardWritablePath refuses targets where a write attacks the system rather
// than a file: filesystem roots (posix and drive letters) and pseudo trees.
func guardWritablePath(p string) error {
	trimmed := strings.Trim(p, "/")
	if trimmed == "" || p == "." {
		return errors.New("refusing to operate on a filesystem root")
	}
	if len(trimmed) == 2 && trimmed[1] == ':' {
		return errors.New("refusing to operate on a drive root")
	}
	lower := strings.ToLower(p)
	for _, prefix := range []string{"/dev/", "/proc/", "/sys/", "/run/"} {
		if strings.HasPrefix(lower, prefix) {
			return fmt.Errorf("refusing to operate under %s", prefix)
		}
	}
	return nil
}

// guardRemovablePath additionally keeps recursive deletes two or more path
// components away from the root ("/tmp/x" is fine, "/tmp" is not; a leading
// drive letter does not count as a component, so "C:/Windows" is depth 1).
func guardRemovablePath(p string, recursive bool) error {
	if err := guardWritablePath(p); err != nil {
		return err
	}
	if !recursive {
		return nil
	}
	segments := strings.Split(strings.Trim(p, "/"), "/")
	depth := len(segments)
	if depth > 0 && len(segments[0]) == 2 && segments[0][1] == ':' {
		depth--
	}
	if depth < 2 {
		return errors.New("refusing a recursive delete this close to the filesystem root")
	}
	return nil
}

// ---- catalog ----

var mcpCatalog = []*mcpTool{
	{
		Name:     "exec_readonly",
		ReadOnly: true,
		Description: "Run a read-only shell command on the open SSH connection: whitelisted programs only " +
			"(ls, cat, head, tail, grep, find, df, du, ps, free, ss, ip addr, systemctl status, journalctl, git log, docker ps, dir …) " +
			"with no unquoted redirection or substitution (quoted |, >, < are fine, e.g. grep \"a\\|b\"). " +
			"Runs without per-call approval; anything not clearly read-only is rejected — use exec for that.",
		Properties: map[string]any{
			"command": map[string]any{"type": "string", "description": "The command line to run"},
		},
		Required: []string{"command"},
		Handler:  mcpExecReadonly,
	},
	{
		Name: "exec",
		Description: "Run an arbitrary shell command on the open SSH connection (non-interactive exec channel, " +
			"combined stdout+stderr, exit status reported). Every call requires explicit user approval in DBX; " +
			"obviously catastrophic patterns are refused outright. Commands are killed at the timeout and partial output returned.",
		Properties: map[string]any{
			"command":     map[string]any{"type": "string", "description": "The command line to run"},
			"timeoutSecs": map[string]any{"type": "integer", "description": "Kill the command after this many seconds (1..110, default 60)"},
		},
		Required: []string{"command"},
		Handler:  mcpExec,
	},
	{
		Name:        "sftp_list_dir",
		ReadOnly:    true,
		Description: "List one remote directory of the open SSH connection (JSON entries with name, size, isDir, mode).",
		Properties: map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory path; defaults to the SFTP start directory"},
		},
		Handler: mcpSftpList,
	},
	{
		Name:        "sftp_stat",
		ReadOnly:    true,
		Description: "Stat one remote path (size, type, mode, mtime).",
		Properties: map[string]any{
			"path": map[string]any{"type": "string", "description": "File or directory path"},
		},
		Required: []string{"path"},
		Handler:  mcpSftpStat,
	},
	{
		Name:     "sftp_read_file",
		ReadOnly: true,
		Description: "Read a slice of a remote text file (default first 16 KiB, max 128 KiB per call; pass offset to page). " +
			"Connection encoding is transcoded; binary files are reported, not dumped.",
		Properties: map[string]any{
			"path":       map[string]any{"type": "string", "description": "File path"},
			"offset":     map[string]any{"type": "integer", "description": "Byte offset to start reading from (default 0)"},
			"limitBytes": map[string]any{"type": "integer", "description": "Bytes to read, 1..131072 (default 16384)"},
		},
		Required: []string{"path"},
		Handler:  mcpSftpRead,
	},
	{
		Name: "sftp_write_file",
		Description: "Write a remote text file (UTF-8, up to 256 KiB, temp+rename so failures never truncate the original). " +
			"Requires user approval in DBX for every call.",
		Properties: map[string]any{
			"path":    map[string]any{"type": "string", "description": "Target file path"},
			"content": map[string]any{"type": "string", "description": "Full file content in UTF-8"},
		},
		Required: []string{"path", "content"},
		Handler:  mcpSftpWrite,
	},
	{
		Name:        "sftp_mkdir",
		Description: "Create one remote directory (recursive optional). Requires user approval in DBX.",
		Properties: map[string]any{
			"path":      map[string]any{"type": "string", "description": "Directory path to create"},
			"recursive": map[string]any{"type": "boolean", "description": "Create missing parents (mkdir -p)"},
		},
		Required: []string{"path"},
		Handler:  mcpSftpMkdir,
	},
	{
		Name: "sftp_remove",
		Description: "Delete one remote file, or a directory tree with recursive=true. Requires user approval in DBX; " +
			"recursive deletes near the filesystem root are refused.",
		Properties: map[string]any{
			"path":      map[string]any{"type": "string", "description": "Path to delete"},
			"recursive": map[string]any{"type": "boolean", "description": "Remove directories recursively"},
		},
		Required: []string{"path"},
		Handler:  mcpSftpRemove,
	},
	{
		Name:        "sftp_rename",
		Description: "Rename/move a remote path, overwriting an existing destination. Requires user approval in DBX.",
		Properties: map[string]any{
			"from": map[string]any{"type": "string", "description": "Existing path"},
			"to":   map[string]any{"type": "string", "description": "New path"},
		},
		Required: []string{"from", "to"},
		Handler:  mcpSftpRename,
	},
}

func strDefault(values map[string]any, key, fallback string) string {
	if value := str(values[key]); value != "" {
		return value
	}
	return fallback
}
