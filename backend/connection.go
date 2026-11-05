package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
	"golang.org/x/crypto/ssh"
)

// AuthConfig keeps credentials in plaintext in memory only: the sidecar needs
// the real password for SSH auth and the owner explicitly wants it readable.
// At-rest persistence goes through the sealed envelope (secrets.go).
type AuthConfig struct {
	Method     string // "password" | "key"
	Password   string
	KeyPath    string
	Passphrase string
}

// ConnSpec is the dial recipe for one DBX connection. It is captured from the
// connection lifecycle payload so extra shells and reconnects can redial
// without asking the host again.
type ConnSpec struct {
	ConnectionID  string
	Name          string
	Kind          string // always "ssh"
	Host          string
	Port          int
	Username      string
	Auth          AuthConfig
	Encoding      string // "" | "utf-8" | "gbk" | "gb18030"
	KeepaliveSecs int
	TimeoutSecs   int // TCP/auth dial timeout; 0 → default
	StartupCmd    string
}

// ---- host lifecycle payload ----

type LifecycleProvider struct {
	ID           string `json:"id"`
	DatabaseType string `json:"databaseType"`
}

type LifecycleConnection struct {
	ID                string            `json:"id"`
	DBType            string            `json:"db_type"`
	PluginID          string            `json:"plugin_id"`
	PluginProvider    string            `json:"plugin_connection_provider"`
	PluginType        string            `json:"plugin_connection_type"`
	Name              string            `json:"name"`
	Host              string            `json:"host"`
	Port              int               `json:"port"`
	Username          string            `json:"username"`
	Password          string            `json:"password"`
	ExternalConfig    map[string]any    `json:"external_config"`
	ConnectionSecrets map[string]string `json:"connection_secrets"`
}

type LifecycleRuntime struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type LifecycleRequest struct {
	Provider   LifecycleProvider   `json:"provider"`
	Connection LifecycleConnection `json:"connection"`
	Runtime    *LifecycleRuntime   `json:"runtime"`
}

func parseLifecycle(values map[string]any) (LifecycleRequest, error) {
	raw, err := json.Marshal(values)
	if err != nil {
		return LifecycleRequest{}, err
	}
	var request LifecycleRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return LifecycleRequest{}, err
	}
	if request.Connection.ID == "" {
		return LifecycleRequest{}, errors.New("lifecycle payload has no connection id")
	}
	return request, nil
}

func configString(config map[string]any, key, fallback string) string {
	if value, ok := config[key].(string); ok && value != "" {
		return value
	}
	return fallback
}

func configInt(config map[string]any, key string, fallback int) int {
	if value, ok := config[key].(float64); ok && value > 0 {
		return int(value)
	}
	return fallback
}

// spec converts the lifecycle payload into a dial recipe. runtime.host/port
// overrides the logical endpoint when DBX tunnels the connection for us.
func (request LifecycleRequest) spec() (ConnSpec, error) {
	connection := request.Connection
	host, port := connection.Host, connection.Port
	if request.Runtime != nil && request.Runtime.Host != "" {
		host = request.Runtime.Host
		if request.Runtime.Port > 0 {
			port = request.Runtime.Port
		}
	}
	if host == "" {
		return ConnSpec{}, errors.New("connection host is empty")
	}
	if connection.Username == "" {
		return ConnSpec{}, errors.New("未填写用户名: 请在连接表单填入登录用户名(本机连接通常是你自己的账户名)")
	}
	if port <= 0 {
		port = 22
	}
	name := connection.Name
	if name == "" || name == request.Provider.ID {
		name = fmt.Sprintf("%s@%s", connection.Username, host)
	}
	keyPath := configString(connection.ExternalConfig, "key_path", "")
	method := configString(connection.ExternalConfig, "auth_method", "password")
	// Forms can end up with auth_method=password but no password and a key
	// path present (dropdown not switched, or the hidden password field's
	// value dropped on save); dial with whichever credential is real.
	if method != "key" && connection.Password == "" && keyPath != "" {
		method = "key"
	}
	if method == "key" && keyPath == "" && connection.Password != "" {
		method = "password"
	}
	spec := ConnSpec{
		ConnectionID:  connection.ID,
		Name:          name,
		Kind:          "ssh",
		Host:          host,
		Port:          port,
		Username:      connection.Username,
		Encoding:      configString(connection.ExternalConfig, "encoding", "utf-8"),
		KeepaliveSecs: configInt(connection.ExternalConfig, "keepalive_secs", 30),
		TimeoutSecs:   configInt(connection.ExternalConfig, "connect_timeout", 20),
		StartupCmd:    configString(connection.ExternalConfig, "startup_cmd", ""),
		Auth: AuthConfig{
			Method:     method,
			Password:   connection.Password,
			KeyPath:    keyPath,
			Passphrase: connection.ConnectionSecrets["key_passphrase"],
		},
	}
	return spec, nil
}

// ---- manager lifecycle methods ----

// TestConnection dials, authenticates and immediately closes: the host uses it
// for the "测试连接" button on the connection form.
func (manager *Manager) TestConnection(request LifecycleRequest) *dbxpluginsdk.PluginError {
	spec, err := request.spec()
	if err != nil {
		return dbxpluginsdk.NewError(-32002, err.Error())
	}
	client, dialErr := dialSSH(spec, manager.hostKeys)
	if dialErr != nil {
		return dbxpluginsdk.NewError(-32002, dialErr.Error())
	}
	client.Close()
	return nil
}

func connectionInfo(spec ConnSpec, shellID string) map[string]any {
	return map[string]any{
		"connectionId": spec.ConnectionID,
		"shellId":      shellID,
		"kind":         spec.Kind,
		"name":         spec.Name,
		"host":         spec.Host,
		"username":     spec.Username,
	}
}

// sameEndpoint reports whether two specs point at the same server identity:
// the fallback path (host reissue) must never absorb a *different* server.
func sameEndpoint(a, b ConnSpec) bool {
	return a.Host == b.Host && a.Port == b.Port && a.Username == b.Username
}

// ConnectConnection handles connection/connect. The host may call it again
// after a sidecar restart, so it is idempotent per connection id: the shared
// client is established once and the primary PTY opened over it.
func (manager *Manager) ConnectConnection(request LifecycleRequest, emitter *dbxpluginsdk.Emitter) (map[string]any, *dbxpluginsdk.PluginError) {
	spec, err := request.spec()
	if err != nil {
		return nil, dbxpluginsdk.NewError(-32002, err.Error())
	}
	if info := manager.livePrimaryFor(spec); info != nil {
		return info, nil
	}
	conn, dialErr := manager.conn(spec.ConnectionID, emitter, spec)
	if dialErr != nil {
		return nil, dbxpluginsdk.NewError(-32002, dialErr.Error())
	}
	manager.mutex.Lock()
	manager.specs[spec.ConnectionID] = spec
	manager.persistSpecsLocked()
	manager.mutex.Unlock()
	shellID, dialErr := manager.dial(conn, 80, 24, emitter)
	if dialErr != nil {
		return nil, dbxpluginsdk.NewError(-32002, dialErr.Error())
	}
	return connectionInfo(spec, shellID), nil
}

// DisconnectConnection handles connection/disconnect: every PTY of the
// connection is torn down and the shared SSH client is closed.
func (manager *Manager) DisconnectConnection(request LifecycleRequest) {
	connectionID := request.Connection.ID
	manager.mutex.Lock()
	shellIDs := make([]string, 0, 4)
	for id, shell := range manager.shells {
		if shell.Config.ConnectionID == connectionID {
			shellIDs = append(shellIDs, id)
		}
	}
	conn := manager.conns[connectionID]
	delete(manager.conns, connectionID)
	delete(manager.specs, connectionID)
	manager.persistSpecsLocked()
	manager.mutex.Unlock()
	for _, id := range shellIDs {
		manager.CloseShell(id)
	}
	// AI follow tasks, log followers and any auto-reconnect attempt must not
	// survive the disconnect either.
	manager.stopTailTasksForConn(connectionID)
	manager.stopFollowersForConn(connectionID)
	manager.stopReconnectSupervisor(connectionID)
	if conn != nil {
		conn.Close()
	}
}

// findSpec resolves the dial recipe for a connection id. The host can issue
// a fresh record id for the same saved connection (recreate, re-import),
// which would strand already-open tabs. An exact miss falls back to the only
// cached spec when there is exactly one — with multiple servers cached the
// id is required, since the request carries nothing else to match on.
func (manager *Manager) findSpec(connectionID string) (ConnSpec, bool) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if spec, ok := manager.specs[connectionID]; ok {
		return spec, true
	}
	if len(manager.specs) == 1 {
		for _, spec := range manager.specs {
			return spec, true
		}
	}
	return ConnSpec{}, false
}

// cachedSpec is the spec cached for exactly this id, refreshing from disk
// on a miss: the workbench's sidecar may have connected the id after this
// sidecar loaded the cache (see mergeSpecsFromDisk).
func (manager *Manager) cachedSpec(connectionID string) (ConnSpec, bool) {
	manager.mutex.Lock()
	spec, ok := manager.specs[connectionID]
	manager.mutex.Unlock()
	if ok {
		return spec, true
	}
	manager.mergeSpecsFromDisk()
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	spec, ok = manager.specs[connectionID]
	return spec, ok
}

// connExact is conn for callers that hold nothing but an id: it must be
// cached exactly. findSpec's single-spec fallback is deliberately skipped —
// it serves the workbench reconnect path (host reissued the record id) and
// would otherwise answer with whichever one server this sidecar knows,
// running the caller's command on the wrong host.
func (manager *Manager) connExact(connectionID string, emitter *dbxpluginsdk.Emitter) (*Conn, error) {
	spec, ok := manager.cachedSpec(connectionID)
	if !ok {
		return nil, errors.New("连接尚未建立: 请在左侧连接列表断开后重新双击该连接")
	}
	return manager.conn(connectionID, emitter, spec)
}

// EnsureConnection is the workbench entry point: it returns a live PTY of
// the connection, opening one over the shared client when the sidecar has
// none — e.g. the tab outlived a network drop or the backend was restarted.
func (manager *Manager) EnsureConnection(connectionID string, emitter *dbxpluginsdk.Emitter) (map[string]any, *dbxpluginsdk.PluginError) {
	if info := manager.livePrimary(connectionID); info != nil {
		return info, nil
	}
	conn, err := manager.conn(connectionID, emitter)
	if err != nil {
		return nil, dbxpluginsdk.NewError(-32001, err.Error())
	}
	shellID, dialErr := manager.dial(conn, 80, 24, emitter)
	if dialErr != nil {
		return nil, dbxpluginsdk.NewError(-32002, dialErr.Error())
	}
	return connectionInfo(conn.Spec, shellID), nil
}

// Credentials returns the plaintext auth material the sidecar received at
// connect time, so the UI can offer "复制密码".
func (manager *Manager) Credentials(connectionID string) (map[string]any, *dbxpluginsdk.PluginError) {
	spec, ok := manager.findSpec(connectionID)
	if !ok {
		return nil, dbxpluginsdk.NewError(-32001,
			"连接尚未建立: 请在左侧连接列表断开后重新双击该连接")
	}
	return map[string]any{
		"connectionId": spec.ConnectionID,
		"method":       spec.Auth.Method,
		"password":     spec.Auth.Password,
		"keyPath":      spec.Auth.KeyPath,
		"passphrase":   spec.Auth.Passphrase,
	}, nil
}

// NewShellForConnection opens one more PTY session multiplexed over the
// connection's shared SSH client (the "＋" button); existing sessions are
// untouched — closing one can never affect the others.
func (manager *Manager) NewShellForConnection(connectionID string, cols, rows int, emitter *dbxpluginsdk.Emitter) (map[string]any, *dbxpluginsdk.PluginError) {
	conn, err := manager.conn(connectionID, emitter)
	if err != nil {
		return nil, dbxpluginsdk.NewError(-32002, err.Error())
	}
	shellID, dialErr := manager.dial(conn, cols, rows, emitter)
	if dialErr != nil {
		return nil, dbxpluginsdk.NewError(-32002, dialErr.Error())
	}
	return connectionInfo(conn.Spec, shellID), nil
}

// conn returns the live shared SSH connection for an id, dialing it (again)
// when the host has no live client — e.g. after a sidecar restart. The spec
// to dial with is supplied by the caller when known (lifecycle payloads carry
// it), falling back to the cached spec otherwise.
func (manager *Manager) conn(connectionID string, emitter *dbxpluginsdk.Emitter, known ...ConnSpec) (*Conn, error) {
	manager.mutex.Lock()
	cached := manager.conns[connectionID]
	manager.mutex.Unlock()
	if cached != nil && !cached.Dead() {
		if len(known) == 0 || sameEndpoint(cached.Spec, known[0]) {
			return cached, nil
		}
		// The live client points at another server than the spec the caller
		// holds — a cross-wired entry; replace it below rather than hand it out.
		manager.writeDiag(fmt.Sprintf("conn %s: cached client is %s@%s:%d but the spec says %s@%s:%d, redialing",
			connectionID, cached.Spec.Username, cached.Spec.Host, cached.Spec.Port,
			known[0].Username, known[0].Host, known[0].Port))
	}
	var spec ConnSpec
	var ok bool
	if len(known) > 0 {
		spec, ok = known[0], true
	} else {
		spec, ok = manager.findSpec(connectionID)
	}
	if !ok {
		return nil, errors.New("连接尚未建立: 请在左侧连接列表断开后重新双击该连接")
	}
	conn, err := DialConn(spec, emitter, manager.hostKeys)
	if err != nil {
		return nil, err
	}
	conn.onDead = func() { manager.notifyConnectionTrouble(connectionID) }
	manager.mutex.Lock()
	old := manager.conns[connectionID]
	manager.conns[connectionID] = conn
	manager.mutex.Unlock()
	if old != nil {
		old.Close()
	}
	return conn, nil
}

// dial opens one PTY session over the connection's shared client. The shell
// reports its own death to the reconnect supervisor — PTY EOF is usually the
// first symptom of a dropped link.
func (manager *Manager) dial(conn *Conn, cols, rows int, emitter *dbxpluginsdk.Emitter) (string, error) {
	onClosed := func(*Shell) { manager.notifyConnectionTrouble(conn.Spec.ConnectionID) }
	shell, err := ConnectShell(conn.Client(), conn.Spec, cols, rows, emitter, onClosed)
	if err != nil {
		return "", err
	}
	manager.mutex.Lock()
	manager.shells[shell.ID] = shell
	manager.mutex.Unlock()
	if manager.sessionLogEnabled() {
		manager.attachSessionLog(shell)
	}
	manager.injectFollowHook(conn, shell)
	return shell.ID, nil
}

// injectFollowHook installs the OSC 7 cwd hook into a freshly opened PTY so
// the file panel's follow feature has something to listen to. The probe runs
// once per connection; each PTY needs its own hook because the variables live
// in the shell process.
//
// Echo hiding: the typed hook line would linger in the session history — a
// later cls only erases the live screen while the bytes survive in the ring
// buffer (replayed on attach) and in ConPTY's resize re-emission, which is
// exactly the "opens with junk, one Enter fixes it" regression. Instead the
// output pump drops every paint region carrying the hook line (echo and any
// redraw copies of it) until the first LIVE OSC 7 report arrives — real
// escape bytes the echoed text can never contain — so the echo never enters
// the ring at all, the login banner stays untouched, and the user's own
// keystrokes flow live throughout. With a startup command configured the
// window is skipped — its output may still be in flight and must not be eaten.
func (manager *Manager) injectFollowHook(conn *Conn, shell *Shell) {
	kind := conn.detectShellKind()
	snippet := kind.integrationSnippet(conn.promptBase)
	manager.writeDiag(fmt.Sprintf("follow hook: %s probed as %s (snippet %d bytes)", conn.Spec.Host, kind, len(snippet)))
	if snippet == "" {
		return
	}
	if conn.Spec.StartupCmd == "" {
		// hide the echo of the line we type (from a distinctive prefix of the
		// snippet) until the first LIVE OSC 7 report proves the hook took
		// effect. One mechanism for every shell family: the trigger is real
		// escape bytes that no echo/redraw copy of the injected line can
		// carry, and the rolling filter never holds the user's own keystrokes
		// — the shell kind only shapes the snippet itself
		prefix := snippet
		if len(prefix) > 16 {
			prefix = prefix[:16]
		}
		shell.BeginHideWindow(prefix, osc7Trigger)
		err := shell.Write([]byte(snippet + "\n"))
		if err != nil {
			manager.writeDiag("follow hook write failed: " + err.Error())
		}
		return
	}
	if err := shell.Write([]byte(snippet + "\n")); err != nil {
		manager.writeDiag("follow hook write failed: " + err.Error())
	}
}

// livePrimaryFor is livePrimary gated for connection/connect, which knows
// the full spec: the single-live-connection fallback (DBX reissued the record
// id of the same saved connection) only applies when the live connection is
// the same server, so connecting a second server is never absorbed into the
// first one's session.
func (manager *Manager) livePrimaryFor(spec ConnSpec) map[string]any {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	if info := livePrimaryLocked(manager, spec.ConnectionID); info != nil {
		return info
	}
	if len(manager.conns) == 1 {
		for id, conn := range manager.conns {
			if sameEndpoint(conn.Spec, spec) {
				return livePrimaryLocked(manager, id)
			}
		}
	}
	return nil
}

// livePrimaryLocked is the resolve step of livePrimary; caller holds the
// mutex (it cannot take it itself when called with the mutex already held).
func livePrimaryLocked(manager *Manager, connectionID string) map[string]any {
	conn, ok := manager.conns[connectionID]
	if !ok || conn.Dead() {
		return nil
	}
	for shellID, shell := range manager.shells {
		if shell.Config.ConnectionID == connectionID && !shell.Closed() {
			return connectionInfo(conn.Spec, shellID)
		}
	}
	return nil
}

// livePrimary returns the connection info when any PTY of the connection is
// alive (or when the id misses but exactly one connection exists).
func (manager *Manager) livePrimary(connectionID string) map[string]any {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	resolve := func(id string) map[string]any {
		conn, ok := manager.conns[id]
		if !ok || conn.Dead() {
			return nil
		}
		for shellID, shell := range manager.shells {
			if shell.Config.ConnectionID == id && !shell.Closed() {
				return connectionInfo(conn.Spec, shellID)
			}
		}
		return nil
	}
	if info := resolve(connectionID); info != nil {
		return info
	}
	if len(manager.conns) == 1 {
		for id := range manager.conns {
			return resolve(id)
		}
	}
	return nil
}

// dialSSH opens and authenticates one SSH connection without a PTY; used by
// connection/test and as the first half of DialConn. Password auth runs
// as its own attempt and keyboard-interactive is retried separately (some
// PAM setups accept only the latter), so failures surface as readable
// "wrong credentials" errors instead of protocol noise. Host key fingerprints
// are verified TOFU-style through the connection's HostKeyStore. The dial
// timeout comes from the connection's connect_timeout (clamped 5..120s).
func dialSSH(spec ConnSpec, hostKeys *HostKeyStore) (*ssh.Client, error) {
	timeout := time.Duration(spec.TimeoutSecs) * time.Second
	if timeout < 5*time.Second {
		timeout = 20 * time.Second
	}
	if timeout > 120*time.Second {
		timeout = 120 * time.Second
	}
	client, err := dialSSHAttempt(spec, timeout, false, hostKeys)
	if err == nil {
		return client, nil
	}
	if spec.Auth.Method != "" && spec.Auth.Method != "password" {
		return nil, err
	}
	if !isAuthFailure(err) {
		return nil, err
	}
	if client, retryErr := dialSSHAttempt(spec, timeout, true, hostKeys); retryErr == nil {
		return client, nil
	}
	return nil, friendlyAuthError(err)
}

func dialSSHAttempt(spec ConnSpec, timeout time.Duration, interactive bool, hostKeys *HostKeyStore) (*ssh.Client, error) {
	address := net.JoinHostPort(spec.Host, strconv.Itoa(spec.Port))
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}
	clientConfig, err := sshClientConfig(spec, timeout, interactive, hostKeys)
	if err != nil {
		conn.Close()
		return nil, err
	}
	sshConn, channels, requests, err := ssh.NewClientConn(conn, spec.Host, clientConfig)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect %s: %w", address, err)
	}
	return ssh.NewClient(sshConn, channels, requests), nil
}

func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	for _, marker := range []string{
		"unable to authenticate",
		"password authentication failed",
		"unexpected message type",
		"no supported authentication methods",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// friendlyAuthError turns x/crypto's protocol-level auth failures into an
// actionable message; Windows OpenSSH rejects Microsoft-account passwords
// surprisingly often, hence the explicit hints.
func friendlyAuthError(err error) error {
	return fmt.Errorf(
		"认证失败: 用户名或密码/私钥不被服务器接受。若使用微软账户登录 Windows, 请确认输入的是账户密码(不是 PIN), 或把用户名换成 MicrosoftAccount\\邮箱; 也可改用私钥认证 (%w)",
		err,
	)
}
