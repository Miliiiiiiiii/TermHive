package main

import (
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/pkg/sftp"
)

type DirEntry struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	IsDir     bool   `json:"isDir"`
	IsSymlink bool   `json:"isSymlink"`
	Mode      string `json:"mode,omitempty"`
	ModTime   int64  `json:"modTime"`
}

// RemoteFS abstracts the remote file operations behind one interface so the
// transfer engine and the UI protocol stay independent of the protocol
// underneath (SFTP today; the abstraction keeps that swappable).
type RemoteFS interface {
	Kind() string
	List(dir string) ([]DirEntry, error)
	Stat(p string) (*DirEntry, error)
	Mkdir(p string) error
	MkdirAll(p string) error
	Remove(p string) error
	RemoveAll(p string) error
	Rename(from, to string) error
	RenameForce(from, to string) error
	Home() (string, error)
	OpenRead(p string) (io.ReadCloser, int64, error)
	OpenWrite(p string, size int64) (io.WriteCloser, error)
}

func cleanRemote(p string) string {
	if p == "" {
		return "."
	}
	return path.Clean(strings.ReplaceAll(p, "\\", "/"))
}

// ---- SFTP ----

type SftpFS struct {
	client *sftp.Client
}

func (fs *SftpFS) Kind() string { return "sftp" }

func (fs *SftpFS) List(dir string) ([]DirEntry, error) {
	infos, err := fs.client.ReadDir(cleanRemote(dir))
	if err != nil {
		return nil, err
	}
	entries := make([]DirEntry, 0, len(infos))
	for _, info := range infos {
		if info.Name() == "." || info.Name() == ".." {
			continue
		}
		entry := DirEntry{
			Name:      info.Name(),
			Size:      info.Size(),
			IsDir:     info.IsDir(),
			IsSymlink: info.Mode()&os.ModeSymlink != 0,
			Mode:      info.Mode().String(),
			ModTime:   info.ModTime().UnixMilli(),
		}
		entries = append(entries, entry)
	}
	sortEntries(entries)
	return entries, nil
}

func sortEntries(entries []DirEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
}

func (fs *SftpFS) Stat(p string) (*DirEntry, error) {
	info, err := fs.client.Stat(cleanRemote(p))
	if err != nil {
		return nil, err
	}
	return &DirEntry{
		Name:      info.Name(),
		Size:      info.Size(),
		IsDir:     info.IsDir(),
		IsSymlink: info.Mode()&os.ModeSymlink != 0,
		Mode:      info.Mode().String(),
		ModTime:   info.ModTime().UnixMilli(),
	}, nil
}

func (fs *SftpFS) Mkdir(p string) error { return fs.client.Mkdir(cleanRemote(p)) }
func (fs *SftpFS) MkdirAll(p string) error {
	return fs.client.MkdirAll(cleanRemote(p))
}

func (fs *SftpFS) Remove(p string) error {
	p = cleanRemote(p)
	info, err := fs.client.Stat(p)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fs.client.RemoveDirectory(p)
	}
	return fs.client.Remove(p)
}

func (fs *SftpFS) RemoveAll(p string) error {
	p = cleanRemote(p)
	info, err := fs.client.Stat(p)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fs.client.Remove(p)
	}
	children, err := fs.client.ReadDir(p)
	if err != nil {
		return err
	}
	for _, child := range children {
		if err := fs.RemoveAll(path.Join(p, child.Name())); err != nil {
			return err
		}
	}
	return fs.client.RemoveDirectory(p)
}

func (fs *SftpFS) Rename(from, to string) error {
	return fs.client.Rename(cleanRemote(from), cleanRemote(to))
}

func (fs *SftpFS) Home() (string, error) {
	return fs.client.RealPath(".")
}

func (fs *SftpFS) OpenRead(p string) (io.ReadCloser, int64, error) {
	p = cleanRemote(p)
	info, err := fs.client.Stat(p)
	if err != nil {
		return nil, 0, err
	}
	file, err := fs.client.Open(p)
	if err != nil {
		return nil, 0, err
	}
	return file, info.Size(), nil
}

func (fs *SftpFS) OpenWrite(p string, _ int64) (io.WriteCloser, error) {
	return fs.client.Create(cleanRemote(p))
}

// RenameForce replaces an existing destination: the plain SSH_FXP_RENAME
// refuses when the target exists, but OpenSSH's posix-rename extension
// (which the pkg/sftp server also implements) overwrites. Falling back to
// the plain rename keeps servers without the extension working for the
// new-file case.
func (fs *SftpFS) RenameForce(from, to string) error {
	from, to = cleanRemote(from), cleanRemote(to)
	if err := fs.client.PosixRename(from, to); err == nil {
		return nil
	}
	return fs.client.Rename(from, to)
}
