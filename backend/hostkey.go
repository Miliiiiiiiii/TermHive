package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// HostKeyEntry is one recorded server fingerprint.
type HostKeyEntry struct {
	Algorithm   string `json:"algo"`
	Fingerprint string `json:"fingerprint"`
	FirstSeen   string `json:"firstSeen"`
	UpdatedAt   string `json:"updatedAt"`
}

// HostKeyStore keeps every server fingerprint the sidecar has seen, keyed by
// the "host:port" of the actual dial endpoint, mirrored to
// <dataDir>/known_hosts.json (0600). First sighting is trusted and recorded
// (TOFU); a later mismatch is a hard dial error — that is the MITM alarm.
type HostKeyStore struct {
	mutex sync.Mutex
	path  string
	hosts map[string]HostKeyEntry
}

func NewHostKeyStore(dataDir string) *HostKeyStore {
	store := &HostKeyStore{hosts: map[string]HostKeyEntry{}}
	if dataDir != "" {
		store.path = filepath.Join(dataDir, "known_hosts.json")
		if raw, err := os.ReadFile(store.path); err == nil {
			var dumped struct {
				Hosts map[string]HostKeyEntry `json:"hosts"`
			}
			if json.Unmarshal(raw, &dumped) == nil && dumped.Hosts != nil {
				store.hosts = dumped.Hosts
			}
		}
	}
	return store
}

func hostKeyEndpoint(host string, port int) string {
	if port <= 0 {
		port = 22
	}
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(port))
}

// Callback builds the ssh.HostKeyCallback for one dial against host:port.
// A nil store degrades to the old insecure behavior — that path only exists
// for direct tests dialing without a Manager.
func (store *HostKeyStore) Callback(host string, port int) ssh.HostKeyCallback {
	if store == nil {
		return ssh.InsecureIgnoreHostKey()
	}
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		return store.verify(hostKeyEndpoint(host, port), key)
	}
}

func (store *HostKeyStore) verify(endpoint string, key ssh.PublicKey) error {
	fingerprint := ssh.FingerprintSHA256(key)
	algo := key.Type()
	now := time.Now().Format(time.RFC3339)
	store.mutex.Lock()
	entry, seen := store.hosts[endpoint]
	if !seen {
		store.hosts[endpoint] = HostKeyEntry{
			Algorithm:   algo,
			Fingerprint: fingerprint,
			FirstSeen:   now,
			UpdatedAt:   now,
		}
		store.saveLocked()
		store.mutex.Unlock()
		return nil
	}
	store.mutex.Unlock()
	if entry.Fingerprint == fingerprint {
		return nil
	}
	return fmt.Errorf(
		"安全警报: 服务器 %s 的主机密钥已变更!\n当前指纹: %s (%s)\n已记录指纹: %s (首次记录于 %s)\n如果服务器确实重装或更换了 SSH 密钥, 清除该记录后重试即可; 否则这可能是中间人攻击, 请勿继续连接。\n重置方法: 删除插件数据目录下的 known_hosts.json, 或调用 hostkey/remove 清除该主机记录。",
		endpoint, fingerprint, algo, entry.Fingerprint, entry.FirstSeen,
	)
}

// Remove drops one recorded fingerprint (the "server legitimately changed its
// key" escape hatch) and reports whether an entry existed.
func (store *HostKeyStore) Remove(host string, port int) bool {
	if store == nil {
		return false
	}
	endpoint := hostKeyEndpoint(host, port)
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if _, ok := store.hosts[endpoint]; !ok {
		return false
	}
	delete(store.hosts, endpoint)
	store.saveLocked()
	return true
}

// List returns every recorded fingerprint sorted by endpoint.
func (store *HostKeyStore) List() []map[string]any {
	if store == nil {
		return []map[string]any{}
	}
	store.mutex.Lock()
	defer store.mutex.Unlock()
	endpoints := make([]string, 0, len(store.hosts))
	for endpoint := range store.hosts {
		endpoints = append(endpoints, endpoint)
	}
	sort.Strings(endpoints)
	out := make([]map[string]any, 0, len(endpoints))
	for _, endpoint := range endpoints {
		entry := store.hosts[endpoint]
		out = append(out, map[string]any{
			"endpoint":    endpoint,
			"algo":        entry.Algorithm,
			"fingerprint": entry.Fingerprint,
			"firstSeen":   entry.FirstSeen,
		})
	}
	return out
}

func (store *HostKeyStore) saveLocked() {
	if store.path == "" {
		return
	}
	raw, err := json.MarshalIndent(struct {
		Version int                     `json:"version"`
		Hosts   map[string]HostKeyEntry `json:"hosts"`
	}{Version: 1, Hosts: store.hosts}, "", "  ")
	if err != nil {
		return
	}
	temp := store.path + ".tmp"
	if os.WriteFile(temp, raw, 0o600) != nil {
		return
	}
	_ = os.Rename(temp, store.path)
}
