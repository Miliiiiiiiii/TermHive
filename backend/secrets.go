package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// errNotSealed marks a specs.json that predates encryption (legacy plaintext).
var errNotSealed = errors.New("specs.json is not a sealed envelope")

// specs.json is written as a sealed envelope: the JSON of the specs map is
// encrypted at rest. Scheme "dpapi" (Windows) binds the ciphertext to the
// OS user account; scheme "aesgcm" uses a random key kept in
// <dataDir>/.secret.key (0600) for other platforms.
const sealedVersion = 1

type sealedBox struct {
	Version int    `json:"v"`
	Scheme  string `json:"scheme"`
	Data    string `json:"data"` // base64 ciphertext
}

func (manager *Manager) sealSpecs(plaintext []byte) ([]byte, error) {
	box := sealedBox{Version: sealedVersion}
	if runtime.GOOS == "windows" {
		data, err := dpapiSeal(plaintext)
		if err != nil {
			return nil, err
		}
		box.Scheme, box.Data = "dpapi", data
	} else {
		key, err := manager.specsKey()
		if err != nil {
			return nil, err
		}
		data, err := aesgcmSeal(key, plaintext)
		if err != nil {
			return nil, err
		}
		box.Scheme, box.Data = "aesgcm", data
	}
	return json.MarshalIndent(box, "", "  ")
}

func (manager *Manager) openSpecs(raw []byte) ([]byte, error) {
	var box sealedBox
	if err := json.Unmarshal(raw, &box); err != nil || box.Scheme == "" || box.Data == "" {
		return nil, errNotSealed
	}
	switch box.Scheme {
	case "dpapi":
		return dpapiOpen(box.Data)
	case "aesgcm":
		key, err := manager.specsKey()
		if err != nil {
			return nil, err
		}
		return aesgcmOpen(key, box.Data)
	default:
		return nil, fmt.Errorf("unknown seal scheme %q", box.Scheme)
	}
}

// specsKey returns (creating on first use) the 32-byte key for the aesgcm
// scheme. Windows payloads are DPAPI-sealed instead, so this file only comes
// into play for data dirs written on other platforms.
func (manager *Manager) specsKey() ([]byte, error) {
	if manager.dataDir == "" {
		return nil, errors.New("no data dir: cannot store the specs encryption key")
	}
	path := filepath.Join(manager.dataDir, ".secret.key")
	if raw, err := os.ReadFile(path); err == nil && len(raw) == 32 {
		return raw, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func aesgcmSeal(key, plaintext []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plaintext, nil)), nil
}

func aesgcmOpen(key []byte, data string) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext shorter than nonce")
	}
	nonce, body := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, body, nil)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
