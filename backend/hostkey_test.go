package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func testSigner(t *testing.T) gossh.PublicKey {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

// TestHostKeyTOFU walks the whole trust lifecycle: first sighting records,
// same key keeps passing, a different key is a hard error, the record
// survives a store reload, and Remove resets it.
func TestHostKeyTOFU(t *testing.T) {
	dir := t.TempDir()
	store := NewHostKeyStore(dir)
	first := testSigner(t)
	if err := store.verify("127.0.0.1:22", first); err != nil {
		t.Fatalf("first sighting must be trusted (TOFU): %v", err)
	}
	if err := store.verify("127.0.0.1:22", first); err != nil {
		t.Fatalf("same key must keep verifying: %v", err)
	}
	second := testSigner(t)
	err := store.verify("127.0.0.1:22", second)
	if err == nil {
		t.Fatal("changed key must be rejected")
	}
	if !strings.Contains(err.Error(), "主机密钥已变更") {
		t.Fatalf("mismatch error must carry the Chinese MITM warning: %v", err)
	}

	// persistence: a fresh store reloads the record from known_hosts.json
	reloaded := NewHostKeyStore(dir)
	if err := reloaded.verify("127.0.0.1:22", second); err == nil {
		t.Fatal("reloaded store must still reject the changed key")
	}
	if err := reloaded.verify("127.0.0.1:22", first); err != nil {
		t.Fatalf("reloaded store must accept the original key: %v", err)
	}

	// endpoint-scoped: another port is an independent TOFU record
	if err := reloaded.verify("127.0.0.1:2222", second); err != nil {
		t.Fatalf("different endpoint is an independent record: %v", err)
	}

	entries := reloaded.List()
	if len(entries) != 2 || entries[0]["endpoint"] != "127.0.0.1:22" {
		t.Fatalf("unexpected list: %v", entries)
	}

	// Remove is the legitimate-key-change escape hatch
	if !reloaded.Remove("127.0.0.1", 22) {
		t.Fatal("remove should drop an existing record")
	}
	if reloaded.Remove("127.0.0.1", 22) {
		t.Fatal("second remove should report false")
	}
	if err := reloaded.verify("127.0.0.1:22", second); err != nil {
		t.Fatalf("after remove the new key is trusted again: %v", err)
	}

	// nil store degrades to the insecure callback instead of panicking
	if reloaded.Callback("h", 22) == nil {
		t.Fatal("Callback must always produce a callback")
	}
}
