package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

type plugin struct {
	manager *Manager
}

// diag appends one line to <dataDir>/diag.log (capped) so the whole UI ↔
// backend conversation can be inspected from disk while debugging the host.
func (app *plugin) diag(line string) {
	app.manager.writeDiag(line)
}

func redactParams(values map[string]any) string {
	if values == nil {
		return "{}"
	}
	clone := make(map[string]any, len(values))
	for key, value := range values {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "password") || strings.Contains(lower, "passphrase") || strings.Contains(lower, "secret") {
			clone[key] = "***"
			continue
		}
		clone[key] = value
	}
	raw, err := json.Marshal(clone)
	if err != nil {
		return "{}"
	}
	text := string(raw)
	if len(text) > 220 {
		text = text[:220] + "…"
	}
	return text
}

func (app *plugin) Handle(
	context dbxpluginsdk.RequestContext,
	method string,
	params json.RawMessage,
	emitter *dbxpluginsdk.Emitter,
) (any, *dbxpluginsdk.PluginError) {
	app.manager.setEmitter(emitter)
	var values map[string]any
	if len(params) > 0 {
		if err := json.Unmarshal(params, &values); err != nil {
			app.diag(fmt.Sprintf("call %s ← bad params", method))
			return nil, dbxpluginsdk.NewError(-32602, "invalid params")
		}
	}
	started := time.Now()
	result, pluginError := app.handle(method, values, emitter)
	if method != "plugin/ping" {
		outcome := "ok"
		if pluginError != nil {
			outcome = "失败: " + pluginError.Message
		}
		summary := ""
		if resultMap, ok := result.(map[string]any); ok && len(resultMap) > 0 {
			summary = redactParams(resultMap)
		}
		app.diag(fmt.Sprintf("call %s(%s) → %s %dms %s",
			method, redactParams(values), outcome, time.Since(started).Milliseconds(), summary))
	}
	return result, pluginError
}

func (app *plugin) handle(
	method string,
	values map[string]any,
	emitter *dbxpluginsdk.Emitter,
) (any, *dbxpluginsdk.PluginError) {
	switch method {
	case "plugin/ping":
		return map[string]any{"ok": true, "plugin": "com.milii.termhive"}, nil

	case "diag/log":
		app.diag("UI: " + str(values["line"]))
		return map[string]any{"ok": true}, nil

	// ---- DBX connection lifecycle (host-side stored connections) ----

	case "connection/test":
		request, err := parseLifecycle(values)
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32602, err.Error())
		}
		if pluginError := app.manager.TestConnection(request); pluginError != nil {
			return nil, pluginError
		}
		return map[string]any{"success": true}, nil

	case "connection/connect":
		request, err := parseLifecycle(values)
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32602, err.Error())
		}
		info, pluginError := app.manager.ConnectConnection(request, emitter)
		if pluginError != nil {
			return nil, pluginError
		}
		info["success"] = true
		return info, nil

	case "connection/disconnect":
		request, err := parseLifecycle(values)
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32602, err.Error())
		}
		app.manager.DisconnectConnection(request)
		return map[string]any{"success": true}, nil

	// Workbench-side connection helpers: the UI only knows the connectionId
	// from its tab context; credentials come back from the sidecar's
	// in-memory copy on explicit request.
	case "connection/ensure":
		info, pluginError := app.manager.EnsureConnection(str(values["connectionId"]), emitter)
		if pluginError != nil {
			return nil, pluginError
		}
		return info, nil

	case "connection/credentials":
		credentials, pluginError := app.manager.Credentials(str(values["connectionId"]))
		if pluginError != nil {
			return nil, pluginError
		}
		return credentials, nil

	case "connection/list":
		return map[string]any{"connections": app.manager.ListConnections()}, nil

	// ---- plugin-wide UI preferences (scheme, highlight rules) ----

	case "settings/get":
		return map[string]any{"settings": app.manager.GetSettings()}, nil

	case "settings/put":
		settings, _ := values["settings"].(map[string]any)
		if settings == nil {
			settings = map[string]any{}
		}
		app.manager.SetSettings(settings)
		return map[string]any{"ok": true}, nil

	// ---- last session layout (one-click workspace restore) ----

	case "monitor/sample":
		sample, pluginError := app.manager.SampleMonitor(str(values["connectionId"]), emitter)
		if pluginError != nil {
			return nil, pluginError
		}
		return sample, nil

	case "monitor/kill":
		if pluginError := app.manager.MonitorKill(
			str(values["connectionId"]), intOf(values["pid"]), values["force"] == true, emitter); pluginError != nil {
			return nil, pluginError
		}
		return map[string]any{"ok": true}, nil

	case "workspace/get":
		return map[string]any{"workspace": app.manager.GetWorkspace()}, nil

	case "workspace/put":
		payload, _ := values["workspace"].(map[string]any)
		if payload == nil {
			payload = map[string]any{}
		}
		app.manager.SetWorkspace(payload)
		return map[string]any{"ok": true}, nil

	// ---- recorded server fingerprints (TOFU known_hosts management) ----

	case "hostkey/list":
		return map[string]any{"hosts": app.manager.hostKeys.List()}, nil

	case "hostkey/remove":
		removed := app.manager.hostKeys.Remove(str(values["host"]), intOf(values["port"]))
		return map[string]any{"removed": removed}, nil

	case "session/newShell":
		info, pluginError := app.manager.NewShellForConnection(
			str(values["connectionId"]), intOf(values["cols"]), intOf(values["rows"]), emitter)
		if pluginError != nil {
			return nil, pluginError
		}
		return info, nil

	case "session/shells":
		return app.manager.LiveShells(), nil

	case "session/attach":
		shell, err := app.manager.Shell(str(values["shellId"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32004, err.Error())
		}
		shell.Replay()
		return map[string]any{"shellId": shell.ID, "closed": shell.Closed()}, nil

	case "session/resize":
		shell, err := app.manager.Shell(str(values["shellId"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32004, err.Error())
		}
		if err := shell.Resize(intOf(values["cols"]), intOf(values["rows"])); err != nil {
			return nil, dbxpluginsdk.NewError(-32005, err.Error())
		}
		return map[string]any{"ok": true}, nil

	case "session/close":
		app.manager.CloseShell(str(values["shellId"]))
		return map[string]any{"ok": true}, nil

	case "fs/list":
		fs, _, err := app.manager.FS(str(values["shellId"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32004, err.Error())
		}
		target := cleanRemote(str(values["path"]))
		entries, err := fs.List(target)
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32006, err.Error())
		}
		// echo the normalized path so the browser never accumulates ".."
		return map[string]any{"path": target, "entries": entries}, nil

	case "fs/home":
		fs, _, err := app.manager.FS(str(values["shellId"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32004, err.Error())
		}
		home, err := fs.Home()
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32006, err.Error())
		}
		return map[string]any{"path": home}, nil

	case "fs/stat":
		fs, _, err := app.manager.FS(str(values["shellId"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32004, err.Error())
		}
		entry, err := fs.Stat(str(values["path"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32006, err.Error())
		}
		return entry, nil

	case "fs/mkdir", "fs/mkdirAll":
		fs, _, err := app.manager.FS(str(values["shellId"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32004, err.Error())
		}
		if method == "fs/mkdir" {
			err = fs.Mkdir(str(values["path"]))
		} else {
			err = fs.MkdirAll(str(values["path"]))
		}
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32006, err.Error())
		}
		return map[string]any{"ok": true}, nil

	case "fs/remove":
		fs, _, err := app.manager.FS(str(values["shellId"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32004, err.Error())
		}
		if boolOf(values["recursive"]) {
			err = fs.RemoveAll(str(values["path"]))
		} else {
			err = fs.Remove(str(values["path"]))
		}
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32006, err.Error())
		}
		return map[string]any{"ok": true}, nil

	case "fs/rename":
		fs, _, err := app.manager.FS(str(values["shellId"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32004, err.Error())
		}
		if err := fs.Rename(str(values["from"]), str(values["to"])); err != nil {
			return nil, dbxpluginsdk.NewError(-32006, err.Error())
		}
		return map[string]any{"ok": true}, nil

	// ---- in-app remote file editor ----

	case "fs/read":
		result, err := app.manager.ReadRemoteFile(str(values["shellId"]), str(values["path"]), int64Of(values["offset"]), str(values["encoding"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32006, err.Error())
		}
		return result, nil

	case "fs/write":
		if err := app.manager.WriteRemoteFile(
			str(values["shellId"]), str(values["path"]), str(values["contentBase64"]), str(values["encoding"]),
		); err != nil {
			return nil, dbxpluginsdk.NewError(-32006, err.Error())
		}
		return map[string]any{"ok": true}, nil

	case "transfer/startUpload":
		transferID, err := app.manager.StartUpload(str(values["shellId"]), str(values["remotePath"]), int64Of(values["size"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32007, err.Error())
		}
		return map[string]any{"transferId": transferID}, nil

	case "transfer/startDownload":
		transferID, size, err := app.manager.StartDownload(str(values["shellId"]), str(values["remotePath"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32007, err.Error())
		}
		return map[string]any{"transferId": transferID, "size": size}, nil

	case "transfer/startDownloadZip":
		paths := make([]string, 0, 8)
		if rawList, ok := values["paths"].([]any); ok {
			for _, raw := range rawList {
				if text, ok := raw.(string); ok {
					paths = append(paths, text)
				}
			}
		}
		transferID, size, err := app.manager.StartDownloadZip(str(values["shellId"]), paths)
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32007, err.Error())
		}
		return map[string]any{"transferId": transferID, "size": size}, nil

	case "transfer/ready":
		app.manager.ReadyDownload(str(values["transferId"]))
		return map[string]any{"ok": true}, nil

	case "transfer/ackDown":
		app.manager.AckDownload(str(values["transferId"]))
		return map[string]any{"ok": true}, nil

	case "transfer/cancel":
		app.manager.CancelTransfer(str(values["transferId"]))
		return map[string]any{"ok": true}, nil

	// ---- log follow mode (file panel tail -f viewer) ----

	case "tail/start":
		taskID, err := app.manager.StartFollow(
			str(values["shellId"]), str(values["path"]), intOf(values["lines"]))
		if err != nil {
			return nil, dbxpluginsdk.NewError(-32006, err.Error())
		}
		return map[string]any{"taskId": taskID}, nil

	case "tail/ready":
		app.manager.ReadyFollow(str(values["taskId"]))
		return map[string]any{"ok": true}, nil

	case "tail/stop":
		app.manager.StopFollow(str(values["taskId"]))
		return map[string]any{"ok": true}, nil

	// ---- AI tools (DBX MCP bridge: discovery + gated execution) ----

	case "mcp/tools":
		return app.manager.MCPTools(values)

	case "mcp/call":
		return app.manager.MCPCall(values, emitter)

	default:
		return nil, dbxpluginsdk.MethodNotFound(method)
	}
}

// HandleBinary routes UI binary frames: term/<shell> input, up/<transfer>
// upload chunks.
func (app *plugin) HandleBinary(channel string, data []byte, emitter *dbxpluginsdk.Emitter) *dbxpluginsdk.PluginError {
	app.manager.setEmitter(emitter)
	parts := splitChannel(channel)
	if parts == nil {
		return dbxpluginsdk.NewError(-32602, "invalid channel: "+channel)
	}
	kind, id := parts[0], parts[1]
	switch kind {
	case "term":
		shell, err := app.manager.Shell(id)
		if err != nil {
			return dbxpluginsdk.NewError(-32004, err.Error())
		}
		if err := shell.Write(data); err != nil {
			return dbxpluginsdk.NewError(-32005, err.Error())
		}
		return nil
	case "up":
		if err := app.manager.UploadChunk(id, data); err != nil {
			return dbxpluginsdk.NewError(-32007, err.Error())
		}
		return nil
	default:
		return dbxpluginsdk.NewError(-32601, "unknown binary channel: "+channel)
	}
}

func splitChannel(channel string) []string {
	for index := 0; index < len(channel); index++ {
		if channel[index] == '/' {
			return []string{channel[:index], channel[index+1:]}
		}
	}
	return nil
}

func str(value any) string {
	text, _ := value.(string)
	return text
}

func intOf(value any) int {
	number, _ := value.(float64)
	return int(number)
}

func int64Of(value any) int64 {
	number, _ := value.(float64)
	return int64(number)
}

func boolOf(value any) bool {
	flag, _ := value.(bool)
	return flag
}

func main() {
	// The real host always provides the directory; the dev host does not, so
	// fall back to its .dbx-dev convention to keep credential persistence
	// working while developing.
	dataDir := os.Getenv("DBX_PLUGIN_DATA_DIR")
	if dataDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			dataDir = filepath.Join(cwd, ".dbx-dev", "plugin-data")
		}
	}
	metadata := dbxpluginsdk.Metadata{
		ID:           "com.milii.termhive",
		Version:      "0.1.0-rc.18",
		Capabilities: []string{"workbench", "binary"},
	}
	server := dbxpluginsdk.NewServer(metadata, &plugin{manager: NewManager(dataDir)})
	server = server.WithTransport(dbxpluginsdk.TransportFramed)
	if err := server.Serve(); err != nil {
		log.Fatal(err)
	}
}
