package main

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dbxpluginsdk "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

const downChunk = 128 * 1024
const downOutstanding = 4
const downWorkers = 4

// readyTimeout bounds a gated pump's wait for transfer/ready: the UI's save
// dialog runs before transfer/startDownload, so ready should follow within
// one round trip; the timeout reaps transfers whose UI side vanished.
const readyTimeout = 60 * time.Second

// chunkBufPool recycles the 128 KiB download buffers: pump loops emit
// through Emitter.Binary, which copies synchronously into its own frame, so
// the buffer is reusable the moment the call returns.
var chunkBufPool = sync.Pool{
	New: func() any { return make([]byte, downChunk) },
}

// Transfer tracks one in-flight file move between the UI and a remote FS.
// Uploads: UI streams chunks on channel up/<id>; the sidecar acks each chunk.
// Downloads: the sidecar streams chunks on down/<id>, but only after the UI
// confirms via transfer/ready that its channel subscription is live — the
// UI-side demux drops frames nobody subscribed to; the UI acks each chunk
// via transfer/ackDown, which throttles the pump to the bridge's throughput.
// Written is written by pump goroutines and read by finishTransfer, so it is
// atomic; done/Canceled/ready are only touched under Manager.mutex.
type Transfer struct {
	ID       string
	ShellID  string
	Remote   string
	Expected int64
	written  atomic.Int64
	Canceled bool
	done     bool
	sink     io.WriteCloser
	source   io.ReadCloser
	ackCh    chan struct{}
	ready    bool
	readyCh  chan struct{}
	doneCh   chan struct{}
	cleanup  func()
}

// Written snapshots the transfer progress for event reporting.
func (transfer *Transfer) Written() int64 {
	return transfer.written.Load()
}

// Manager owns every live session. Connections are keyed by the DBX
// connection id: conns maps to the shared SSH client (one per saved
// connection, all PTY sessions multiplex over it), specs keeps the dial
// recipe mirrored to disk so reconnects survive sidecar restarts.
type Manager struct {
	mutex     sync.Mutex
	shells    map[string]*Shell
	conns     map[string]*Conn
	transfers map[string]*Transfer
	specs     map[string]ConnSpec
	settings  map[string]any
	dataDir   string
	hostKeys  *HostKeyStore

	diagMutex sync.Mutex

	emitterMutex sync.Mutex
	emitter      *dbxpluginsdk.Emitter
}

func NewManager(dataDir string) *Manager {
	manager := &Manager{
		shells:    map[string]*Shell{},
		conns:     map[string]*Conn{},
		transfers: map[string]*Transfer{},
		specs:     map[string]ConnSpec{},
		dataDir:   dataDir,
		hostKeys:  NewHostKeyStore(dataDir),
	}
	if dataDir != "" {
		_ = os.MkdirAll(dataDir, 0o700)
		manager.loadSpecs()
		manager.loadSettings()
	}
	return manager
}

func (manager *Manager) specsPath() string {
	return filepath.Join(manager.dataDir, "specs.json")
}

func (manager *Manager) loadSpecs() {
	raw, err := os.ReadFile(manager.specsPath())
	if err != nil {
		return
	}
	// Current format: sealed envelope. Sealed but undecryptable (data dir
	// moved to another machine or Windows account) starts empty while the
	// file is kept for forensics — reconnecting rebuilds the cache.
	if plaintext, openErr := manager.openSpecs(raw); openErr == nil {
		var specs map[string]ConnSpec
		if json.Unmarshal(plaintext, &specs) == nil && specs != nil {
			manager.specs = specs
		} else {
			manager.writeDiag("specs.json 解密成功但内容不可解析, 已忽略")
		}
		return
	} else if !errors.Is(openErr, errNotSealed) {
		manager.writeDiag("specs.json 解密失败(" + openErr.Error() + "), 已忽略; 重新连接后会重建凭据缓存")
		return
	}
	// Legacy plaintext (pre-0.4.5): hydrate, then seal it immediately.
	var specs map[string]ConnSpec
	if json.Unmarshal(raw, &specs) == nil && specs != nil {
		manager.specs = specs
		manager.persistSpecs()
	}
}

// writeDiag appends a timestamped line to <dataDir>/diag.log, trimming the
// head when it grows past 128 KiB. Purely a debugging aid for host-side
// issues; safe to leave enabled.
func (manager *Manager) writeDiag(line string) {
	if manager.dataDir == "" {
		return
	}
	manager.diagMutex.Lock()
	defer manager.diagMutex.Unlock()
	path := filepath.Join(manager.dataDir, "diag.log")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.Size() > 128*1024 {
		// keep the tail: read back, truncate, rewrite the last half
		if raw, readErr := os.ReadFile(path); readErr == nil && len(raw) > 64*1024 {
			_ = os.WriteFile(path, raw[len(raw)-64*1024:], 0o600)
			file.Close()
			file, err = os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return
			}
			defer file.Close()
		}
	}
	fmt.Fprintf(file, "%s %s\n", time.Now().Format("15:04:05.000"), line)
}

// persistSpecsLocked flushes the credential cache; caller holds the mutex.
// The file is sealed at rest (DPAPI on Windows, AES-GCM elsewhere): plaintext
// lives only in memory, which is what "passwords stay recoverable" actually
// needs, while a stolen data dir alone no longer yields credentials. On seal
// failure the flush is skipped rather than falling back to plaintext.
func (manager *Manager) persistSpecsLocked() {
	if manager.dataDir == "" {
		return
	}
	raw, err := json.MarshalIndent(manager.specs, "", "  ")
	if err != nil {
		return
	}
	sealed, err := manager.sealSpecs(raw)
	if err != nil {
		manager.writeDiag("specs.json 加密失败, 跳过本次落盘: " + err.Error())
		return
	}
	temp := manager.specsPath() + ".tmp"
	if os.WriteFile(temp, sealed, 0o600) != nil {
		return
	}
	_ = os.Rename(temp, manager.specsPath())
}

// persistSpecs is persistSpecsLocked for callers without the mutex held.
func (manager *Manager) persistSpecs() {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	manager.persistSpecsLocked()
}

func (manager *Manager) setEmitter(emitter *dbxpluginsdk.Emitter) {
	manager.emitterMutex.Lock()
	manager.emitter = emitter
	manager.emitterMutex.Unlock()
}

func (manager *Manager) emit(method string, params any) {
	manager.emitterMutex.Lock()
	emitter := manager.emitter
	manager.emitterMutex.Unlock()
	if emitter != nil {
		_ = emitter.Event(method, params)
	}
}

// CloseShell tears down one PTY session. Its sibling sessions of the same
// connection are untouched — they share only the transport, not the channel.
func (manager *Manager) CloseShell(id string) {
	manager.mutex.Lock()
	shell := manager.shells[id]
	delete(manager.shells, id)
	manager.mutex.Unlock()
	if shell != nil {
		shell.Close("closed by user")
	}
}

// LiveShells lists every live session so the launcher tab can offer re-entry.
func (manager *Manager) LiveShells() []map[string]any {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	out := []map[string]any{}
	for _, shell := range manager.shells {
		info := connectionInfo(shell.Config, shell.ID)
		info["closed"] = shell.Closed()
		info["createdAt"] = shell.CreatedAt.UnixMilli()
		out = append(out, info)
	}
	return out
}

func (manager *Manager) Shell(id string) (*Shell, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	shell, ok := manager.shells[id]
	if !ok || shell.Closed() {
		return nil, errors.New("shell is not connected: " + id)
	}
	return shell, nil
}

// ConnForShell resolves the shared connection behind a live PTY session.
func (manager *Manager) ConnForShell(shellID string) (*Conn, error) {
	manager.mutex.Lock()
	shell := manager.shells[shellID]
	var conn *Conn
	if shell != nil {
		conn = manager.conns[shell.Config.ConnectionID]
	}
	manager.mutex.Unlock()
	if shell == nil || shell.Closed() {
		return nil, errors.New("shell is not connected: " + shellID)
	}
	if conn == nil || conn.Dead() {
		return nil, errors.New("connection is closed")
	}
	return conn, nil
}

// FS resolves the SFTP file system over the shell's shared connection.
func (manager *Manager) FS(id string) (RemoteFS, func(), error) {
	manager.mutex.Lock()
	shell := manager.shells[id]
	var conn *Conn
	if shell != nil {
		conn = manager.conns[shell.Config.ConnectionID]
	}
	manager.mutex.Unlock()
	if shell == nil || shell.Closed() {
		return nil, nil, errors.New("shell is not connected: " + id)
	}
	if conn == nil || conn.Dead() {
		return nil, nil, errors.New("connection is closed")
	}
	client, err := conn.SFTP()
	if err != nil {
		return nil, nil, err
	}
	return &SftpFS{client: client}, func() {}, nil
}

// ---- transfers ----

// ListConnections returns the cached dial recipes (never credentials) so a
// workbench can open sessions of other saved servers — e.g. side-by-side
// split view across servers. Only connections the sidecar has dialed at
// least once are known (that is what the spec cache holds).
func (manager *Manager) ListConnections() []map[string]any {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	out := make([]map[string]any, 0, len(manager.specs))
	for _, spec := range manager.specs {
		out = append(out, map[string]any{
			"connectionId": spec.ConnectionID,
			"name":         spec.Name,
			"host":         spec.Host,
			"port":         spec.Port,
			"username":     spec.Username,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return fmt.Sprint(out[i]["name"]) < fmt.Sprint(out[j]["name"])
	})
	return out
}

func (manager *Manager) StartUpload(shellID, remotePath string, size int64) (string, error) {
	fs, _, err := manager.FS(shellID)
	if err != nil {
		return "", err
	}
	// A directory target is the signature of a dropped/picked folder
	// masquerading as a file — say so instead of letting SSH_FX_FAILURE
	// guess for us.
	if entry, statErr := fs.Stat(cleanRemote(remotePath)); statErr == nil && entry.IsDir {
		return "", fmt.Errorf(
			"无法上传 %s: 它是一个文件夹。请改用\"上传文件夹\"按钮(或把文件夹直接拖进面板)来上传整个目录",
			path.Base(cleanRemote(remotePath)),
		)
	}
	sink, err := fs.OpenWrite(cleanRemote(remotePath), size)
	if err != nil {
		return "", explainWriteError(remotePath, err)
	}
	transfer := &Transfer{
		ID:       NewID("t"),
		ShellID:  shellID,
		Remote:   remotePath,
		Expected: size,
		sink:     sink,
		doneCh:   make(chan struct{}),
	}
	manager.mutex.Lock()
	manager.transfers[transfer.ID] = transfer
	manager.mutex.Unlock()
	return transfer.ID, nil
}

// UploadChunk persists one chunk and reports total written.
func (manager *Manager) UploadChunk(transferID string, data []byte) error {
	manager.mutex.Lock()
	transfer := manager.transfers[transferID]
	finished := transfer != nil && (transfer.done || transfer.Canceled)
	manager.mutex.Unlock()
	if transfer == nil {
		return errors.New("unknown transfer: " + transferID)
	}
	if finished {
		return errors.New("transfer already finished: " + transferID)
	}
	count, err := transfer.sink.Write(data)
	if err != nil {
		manager.finishTransfer(transfer.ID, err)
		return err
	}
	written := transfer.written.Add(int64(count))
	manager.emit("transfer.ack", map[string]any{"transferId": transferID, "received": written})
	if transfer.Expected > 0 && written >= transfer.Expected {
		manager.finishTransfer(transfer.ID, nil)
	}
	return nil
}

// explainWriteError turns SFTP's generic "Failure" reply into something a
// user can act on (Windows OpenSSH returns it for invalid paths, missing
// permissions and locked files alike).
func explainWriteError(remotePath string, err error) error {
	if strings.Contains(err.Error(), "Failure") || strings.Contains(err.Error(), "SSH_FX_FAILURE") {
		return fmt.Errorf("无法写入 %s: 目标路径无效/无权限/文件被占用 (SSH_FX_FAILURE)", remotePath)
	}
	return err
}

// StartDownloadZip bundles the given remote files and directories into one
// zip stream so the UI can offer a single save dialog for multi-select.
// The archive is zipped straight into the transfer pipe — downloading starts
// on the first byte instead of after a full temp-file pass, with no temp
// disk usage and no size known upfront (progress shows bytes only).
func (manager *Manager) StartDownloadZip(shellID string, paths []string) (string, int64, error) {
	fs, _, err := manager.FS(shellID)
	if err != nil {
		return "", 0, err
	}
	pipeReader, pipeWriter := io.Pipe()
	transfer := &Transfer{
		ID:       NewID("t"),
		ShellID:  shellID,
		Remote:   "zip:" + strings.Join(paths, ","),
		Expected: 0,
		source:   pipeReader,
		ackCh:    make(chan struct{}, downOutstanding),
		readyCh:  make(chan struct{}),
		doneCh:   make(chan struct{}),
		// Unblock the zipper goroutine when the transfer ends early.
		cleanup: func() { _ = pipeWriter.CloseWithError(errors.New("transfer finished")) },
	}
	for i := 0; i < downOutstanding; i++ {
		transfer.ackCh <- struct{}{}
	}
	manager.mutex.Lock()
	manager.transfers[transfer.ID] = transfer
	manager.mutex.Unlock()
	go manager.zipInto(transfer, fs, paths, pipeWriter)
	go manager.gateAndPump(transfer, 0)
	return transfer.ID, 0, nil
}

// zipInto walks the selected paths and streams the archive into the
// transfer's pipe. A walk error aborts the stream and surfaces on
// transfer.done instead of being reported as a completed download.
func (manager *Manager) zipInto(transfer *Transfer, fs RemoteFS, paths []string, pipeWriter *io.PipeWriter) {
	zipErr := func() error {
		zipWriter := zip.NewWriter(pipeWriter)
		buildErr := func() error {
			for _, raw := range paths {
				p := cleanRemote(raw)
				entry, statErr := fs.Stat(p)
				if statErr != nil {
					return statErr
				}
				if entry.IsDir {
					if walkErr := zipDir(fs, zipWriter, p, path.Base(p)+"/"); walkErr != nil {
						return walkErr
					}
					continue
				}
				if fileErr := zipFile(fs, zipWriter, p, path.Base(p)); fileErr != nil {
					return fileErr
				}
			}
			return nil
		}()
		if buildErr != nil {
			zipWriter.Close()
			return buildErr
		}
		return zipWriter.Close()
	}()
	_ = pipeWriter.CloseWithError(zipErr) // nil → clean EOF for the reader
}

func zipFile(fs RemoteFS, writer *zip.Writer, remotePath, name string) error {
	source, _, err := fs.OpenRead(remotePath)
	if err != nil {
		return err
	}
	defer source.Close()
	entry, err := writer.Create(name)
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, source)
	return err
}

func zipDir(fs RemoteFS, writer *zip.Writer, remotePath, prefix string) error {
	entries, err := fs.List(remotePath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := remotePath + "/" + entry.Name
		if entry.IsDir {
			if err := zipDir(fs, writer, child, prefix+entry.Name+"/"); err != nil {
				return err
			}
			continue
		}
		if err := zipFile(fs, writer, child, prefix+entry.Name); err != nil {
			return err
		}
	}
	return nil
}

func (manager *Manager) StartDownload(shellID, remotePath string) (string, int64, error) {
	fs, _, err := manager.FS(shellID)
	if err != nil {
		return "", 0, err
	}
	// The transfer length is the size stat'ed at open time. The UI's panel
	// listing may be stale, and a growing log must download as the snapshot
	// taken here — never truncated to an old listing size (the retired
	// sizeHint override did exactly that on files > downChunk).
	source, size, err := fs.OpenRead(remotePath)
	if err != nil {
		return "", 0, err
	}
	transfer := &Transfer{
		ID:       NewID("t"),
		ShellID:  shellID,
		Remote:   remotePath,
		Expected: size,
		source:   source,
		ackCh:    make(chan struct{}, downOutstanding),
		readyCh:  make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	for i := 0; i < downOutstanding; i++ {
		transfer.ackCh <- struct{}{}
	}
	manager.mutex.Lock()
	manager.transfers[transfer.ID] = transfer
	manager.mutex.Unlock()
	go manager.gateAndPump(transfer, size)
	return transfer.ID, size, nil
}

// gateAndPump waits for the UI's transfer/ready before launching a download
// pump: the UI subscribes to down/<id> only after its save dialog resolves,
// and frames arriving earlier are dropped by its unbuffered demux. A pump
// started straight out of StartDownload would finish the whole transfer
// while the dialog was still open, leaving a 0-byte local file.
func (manager *Manager) gateAndPump(transfer *Transfer, size int64) {
	select {
	case <-transfer.readyCh:
	case <-transfer.doneCh:
		return
	case <-time.After(readyTimeout):
		manager.finishTransfer(transfer.ID, errors.New("等待 UI 确认接收超时, 下载已取消"))
		return
	}
	// SFTP file handles expose ReadAt: striped concurrent fetches cut the
	// round-trip stalls that dominate large downloads on high-latency links.
	// Small files and plain readers stay on the sequential pump.
	if ra, ok := transfer.source.(io.ReaderAt); ok && size > downChunk {
		manager.pumpDownloadAt(transfer, ra, size)
	} else {
		manager.pumpDownload(transfer)
	}
}

// ReadyDownload unblocks a gated download pump once the UI has subscribed to
// the transfer's binary channel.
func (manager *Manager) ReadyDownload(transferID string) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	transfer := manager.transfers[transferID]
	if transfer != nil && !transfer.ready {
		transfer.ready = true
		close(transfer.readyCh)
	}
}

// pumpDownload streams a source sequentially through pooled buffers. A read
// error other than EOF fails the transfer; only io.EOF completes it (zip
// pipes surface walk failures exactly this way).
func (manager *Manager) pumpDownload(transfer *Transfer) {
	for {
		select {
		case <-transfer.ackCh:
		case <-transfer.doneCh:
			return
		}
		buf := chunkBufPool.Get().([]byte)
		count, err := transfer.source.Read(buf)
		if count > 0 {
			manager.emitBinary("down/"+transfer.ID, buf[:count])
			transfer.written.Add(int64(count))
			manager.emit("transfer.progress", map[string]any{
				"transferId": transfer.ID,
				"bytes":      transfer.Written(),
				"total":      transfer.Expected,
			})
		}
		chunkBufPool.Put(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				manager.finishTransfer(transfer.ID, nil)
			} else {
				manager.finishTransfer(transfer.ID, err)
			}
			return
		}
	}
}

// pumpDownloadAt streams a ranged source (SFTP exposes ReadAt on file
// handles) through a window of concurrent stripe fetches, emitting chunks in
// strict offset order — the UI appends them into one file. Flow control is
// the same ack budget as the sequential pump: the dispatcher acquires a slot
// per dispatched stripe, so at most downOutstanding stripes are in flight.
func (manager *Manager) pumpDownloadAt(transfer *Transfer, source io.ReaderAt, size int64) {
	type stripe struct {
		index int64
		buf   []byte // full pooled buffer; data is buf[:n]
		n     int
		err   error
	}
	total := (size + downChunk - 1) / downChunk
	jobs := make(chan int64)
	results := make(chan stripe)
	quit := transfer.doneCh

	var workers sync.WaitGroup
	for i := 0; i < downWorkers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				buf := chunkBufPool.Get().([]byte)
				read, err := source.ReadAt(buf, index*downChunk)
				select {
				case results <- stripe{index: index, buf: buf, n: read, err: err}:
				case <-quit:
					chunkBufPool.Put(buf)
					return
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		for index := int64(0); index < total; index++ {
			select {
			case <-transfer.ackCh:
			case <-quit:
				return
			}
			select {
			case jobs <- index:
			case <-quit:
				return
			}
		}
	}()

	next := int64(0)
	ready := make(map[int64]stripe)
	deliver := func(s stripe) {
		manager.emitBinary("down/"+transfer.ID, s.buf[:s.n])
		transfer.written.Add(int64(s.n))
		manager.emit("transfer.progress", map[string]any{
			"transferId": transfer.ID,
			"bytes":      transfer.Written(),
			"total":      transfer.Expected,
		})
		chunkBufPool.Put(s.buf)
	}
	for {
		select {
		case s := <-results:
			if s.err != nil && !errors.Is(s.err, io.EOF) {
				manager.finishTransfer(transfer.ID, s.err)
				return
			}
			ready[s.index] = s
			for {
				s, ok := ready[next]
				if !ok {
					break
				}
				delete(ready, next)
				deliver(s)
				next++
			}
			if next >= total {
				manager.finishTransfer(transfer.ID, nil)
				return
			}
		case <-quit:
			return
		}
	}
}

// AckDownload releases one outstanding-chunk slot for a download pump.
func (manager *Manager) AckDownload(transferID string) {
	manager.mutex.Lock()
	transfer := manager.transfers[transferID]
	manager.mutex.Unlock()
	if transfer != nil && transfer.ackCh != nil {
		select {
		case transfer.ackCh <- struct{}{}:
		default:
		}
	}
}

func (manager *Manager) CancelTransfer(transferID string) {
	manager.finishTransfer(transferID, errors.New("canceled"))
}

func (manager *Manager) finishTransfer(transferID string, cause error) {
	manager.mutex.Lock()
	transfer := manager.transfers[transferID]
	if transfer == nil || transfer.done {
		manager.mutex.Unlock()
		return
	}
	transfer.done = true
	transfer.Canceled = cause != nil && cause.Error() == "canceled"
	delete(manager.transfers, transferID)
	close(transfer.doneCh)
	cleanup := transfer.cleanup
	manager.mutex.Unlock()
	if transfer.sink != nil {
		if cause != nil {
			if closer, ok := transfer.sink.(interface{ CloseWithError(error) error }); ok {
				closer.CloseWithError(cause)
			}
		}
		transfer.sink.Close()
	}
	if transfer.source != nil {
		transfer.source.Close()
	}
	if cleanup != nil {
		cleanup()
	}
	manager.emit("transfer.done", map[string]any{
		"transferId": transferID,
		"bytes":      transfer.Written(),
		"ok":         cause == nil,
		"error":      errorText(cause),
	})
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (manager *Manager) emitBinary(channel string, data []byte) {
	manager.emitterMutex.Lock()
	emitter := manager.emitter
	manager.emitterMutex.Unlock()
	if emitter != nil {
		_ = emitter.Binary(channel, data)
	}
}

// NewID returns a short unique id with the given prefix.
func NewID(prefix string) string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + hex.EncodeToString(make([]byte, 6))
	}
	return prefix + hex.EncodeToString(raw[:])
}
