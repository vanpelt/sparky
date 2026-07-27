package oidc

// The same crash-loop sshgw/keys_test.go covers, one component along: an
// interrupted write here leaves an issuer that signs nothing, and every boot
// after it fails on a file that exists.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreateKeyIsStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateKey(dir, "oidc_signing_key")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateKey(dir, "oidc_signing_key")
	if err != nil {
		t.Fatal(err)
	}
	// The kid is derived from the public key, so a changed key is a changed
	// JWKS and every relying party bound to the old one breaks.
	if jwkOf(&first.PublicKey).Kid != jwkOf(&second.PublicKey).Kid {
		t.Error("the signing key changed between calls; every issued token's kid would stop resolving")
	}
}

func TestLoadOrCreateKeyRecoversFromAnEmptyKeyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "oidc_signing_key.pem"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := LoadOrCreateKey(dir, "oidc_signing_key")
	if err != nil {
		t.Fatalf("an empty key file bricked the issuer: %v", err)
	}
	again, err := LoadOrCreateKey(dir, "oidc_signing_key")
	if err != nil {
		t.Fatal(err)
	}
	if jwkOf(&again.PublicKey).Kid != jwkOf(&key.PublicKey).Kid {
		t.Error("the recovered key was not persisted")
	}
}

// A signing key that exists and is merely unparseable by THIS build must be
// reported, never replaced: rotating it silently invalidates every token in
// flight and every cached JWKS.
func TestLoadOrCreateKeyRefusesToOverwriteACorruptKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oidc_signing_key.pem")
	garbage := []byte("-----BEGIN PRIVATE KEY-----\ntruncated")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(dir, "oidc_signing_key"); err == nil {
		t.Fatal("a corrupt signing key was silently replaced")
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(garbage) {
		t.Error("the corrupt key file was modified")
	}
}

func TestLoadOrCreateKeyLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateKey(dir, "oidc_signing_key"); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "oidc_signing_key.pem" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want just the key", names)
	}
}
