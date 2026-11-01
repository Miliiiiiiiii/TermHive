package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
)

// editorWindowBytes is one read window of the in-app viewer: files up to this
// size load whole (and stay editable); anything larger loads windowed and
// read-only with navigation in the UI. The whole point is quick touch-ups of
// configs and tail-viewing of logs, not editing gigabyte files over SFTP.
const editorWindowBytes = 2 * 1024 * 1024

// fsWithSpec mirrors Manager.FS but also returns the owning connection's
// spec so file bytes can be transcoded with the same encoding hint the
// terminal uses.
func (manager *Manager) fsWithSpec(shellID string) (RemoteFS, ConnSpec, error) {
	manager.mutex.Lock()
	shell := manager.shells[shellID]
	var conn *Conn
	if shell != nil {
		conn = manager.conns[shell.Config.ConnectionID]
	}
	manager.mutex.Unlock()
	if shell == nil || shell.Closed() {
		return nil, ConnSpec{}, errors.New("shell is not connected: " + shellID)
	}
	if conn == nil || conn.Dead() {
		return nil, ConnSpec{}, errors.New("connection is closed")
	}
	client, err := conn.SFTP()
	if err != nil {
		return nil, ConnSpec{}, err
	}
	return &SftpFS{client: client}, conn.Spec, nil
}

func decodeToUTF8(encoding string, raw []byte) ([]byte, error) {
	switch strings.ToLower(encoding) {
	case "gbk":
		return io.ReadAll(transform.NewReader(bytes.NewReader(raw), simplifiedchinese.GBK.NewDecoder()))
	case "gb18030":
		return io.ReadAll(transform.NewReader(bytes.NewReader(raw), simplifiedchinese.GB18030.NewDecoder()))
	default:
		return raw, nil
	}
}

func encodeFromUTF8(encoding string, raw []byte) ([]byte, error) {
	switch strings.ToLower(encoding) {
	case "gbk":
		return io.ReadAll(transform.NewReader(bytes.NewReader(raw), simplifiedchinese.GBK.NewEncoder()))
	case "gb18030":
		return io.ReadAll(transform.NewReader(bytes.NewReader(raw), simplifiedchinese.GB18030.NewEncoder()))
	default:
		return raw, nil
	}
}

// ReadRemoteFile loads one window of a remote file for the editor, transcoded
// to UTF-8. Files larger than editorWindowBytes are reported as windowed (the
// UI shows them read-only with window navigation). Binary content is reported
// instead of shipped (probed on the first window only).
func (manager *Manager) ReadRemoteFile(shellID, path string, offset int64) (map[string]any, error) {
	fs, spec, err := manager.fsWithSpec(shellID)
	if err != nil {
		return nil, err
	}
	reader, size, err := fs.OpenRead(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	if offset < 0 {
		offset = 0
	}
	if offset > size {
		offset = size
	}
	limit := size - offset
	if limit > editorWindowBytes {
		limit = editorWindowBytes
	}
	if seeker, ok := reader.(io.Seeker); ok {
		if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	} else if _, err := io.CopyN(io.Discard, reader, offset); err != nil {
		return nil, err
	}
	raw := make([]byte, limit)
	if _, err := io.ReadFull(reader, raw); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if offset == 0 {
		sample := raw
		if len(sample) > 4096 {
			sample = sample[:4096]
		}
		if bytes.IndexByte(sample, 0) != -1 {
			return map[string]any{"binary": true, "size": size}, nil
		}
	}
	if !utf8.Valid(raw) && strings.ToLower(spec.Encoding) == "utf-8" {
		// the file declares nothing; if it is not valid UTF-8 the editor would
		// show mojibake — still load it (replace errors) so users can fix it
		raw = []byte(strings.ToValidUTF8(string(raw), "\uFFFD"))
	}
	content, err := decodeToUTF8(spec.Encoding, raw)
	if err != nil {
		return nil, err
	}
	// windowed describes the FILE (bigger than one window), not the response:
	// the tail window reaches EOF but must stay read-only windowed mode too —
	// an editable partial view could be saved back over the whole file.
	windowed := size > editorWindowBytes
	moreAfter := offset+limit < size
	// A window edge can split a multi-byte character; the replacement char at
	// the seam is decoding noise, not file content — trim it off both edges
	// that are not file edges.
	text := string(content)
	if offset > 0 {
		text = strings.TrimPrefix(text, "\uFFFD")
	}
	if moreAfter {
		text = strings.TrimSuffix(text, "\uFFFD")
	}
	return map[string]any{
		"contentBase64": base64.StdEncoding.EncodeToString([]byte(text)),
		"size":          size,
		"offset":        offset,
		"limit":         limit,
		"windowed":      windowed,
		"encoding":      spec.Encoding,
	}, nil
}

// WriteRemoteFile stores editor content (UTF-8) back: transcode to the
// connection encoding, write a sibling temp file, then rename over the
// target — a failed write never truncates the original. Servers without
// rename-over fall back to a direct overwrite.
func (manager *Manager) WriteRemoteFile(shellID, path, contentBase64 string) error {
	fs, spec, err := manager.fsWithSpec(shellID)
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(contentBase64)
	if err != nil {
		return err
	}
	encoded, err := encodeFromUTF8(spec.Encoding, raw)
	if err != nil {
		return err
	}
	p := cleanRemote(path)
	temp := p + ".termhive.tmp"
	if err := writeAll(fs, temp, encoded); err != nil {
		_ = fs.Remove(temp)
		return explainWriteError(p, err)
	}
	if err := fs.RenameForce(temp, p); err != nil {
		_ = fs.Remove(temp)
		if err2 := writeAll(fs, p, encoded); err2 != nil {
			return err
		}
	}
	return nil
}

func writeAll(fs RemoteFS, path string, data []byte) error {
	sink, err := fs.OpenWrite(path, int64(len(data)))
	if err != nil {
		return err
	}
	if _, err := sink.Write(data); err != nil {
		sink.Close()
		return err
	}
	return sink.Close()
}
