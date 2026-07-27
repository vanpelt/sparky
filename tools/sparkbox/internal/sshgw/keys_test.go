package sshgw

// The failure these cover is not hypothetical: it took down the provision-smoke
// job, and on a real host it is unrecoverable without an operator knowing which
// file to delete. A gateway whose key file is present but unreadable exits at
// startup, systemd's Restart=always brings it straight back, and it exits
// again — forever, across reboots and reprovisions.

import (
	"os"
	"path/filepath"
	"testing"

	xssh "golang.org/x/crypto/ssh"
)

func TestLoadOrCreateKeyIsStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatal(err)
	}
	if PublicKeyLine(first) != PublicKeyLine(second) {
		t.Error("the key changed between calls; clients pin this and rootfs images trust it")
	}
}

// The wreckage an older build could leave behind: os.WriteFile creates the file
// and then writes it, so a disk that filled underneath left zero bytes. Every
// boot after that read the empty file, got `ssh: no key found`, and died.
//
// Nothing can be lost by minting over a file of length zero — no write ever
// completed into it — and doing so is what gets an already-bricked host back
// without anyone having to know this path exists.
func TestLoadOrCreateKeyRecoversFromAnEmptyKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway_host_key.pem")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatalf("an empty key file bricked the host: %v", err)
	}
	if signer == nil {
		t.Fatal("no signer")
	}
	// And it is now durable: the next boot reads what this one wrote.
	again, err := LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatal(err)
	}
	if PublicKeyLine(again) != PublicKeyLine(signer) {
		t.Error("the recovered key was not persisted")
	}
}

// The line the recovery must not cross. A short-but-non-empty file could be a
// key this build cannot parse, and replacing it would silently change an
// identity clients have pinned. Report it; never overwrite it.
func TestLoadOrCreateKeyRefusesToOverwriteACorruptKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway_host_key.pem")
	garbage := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\ntruncated")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKey(dir, "gateway_host_key"); err == nil {
		t.Fatal("a corrupt key file was silently replaced")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(garbage) {
		t.Error("the corrupt key file was modified; it is the operator's evidence")
	}
}

// The property that makes the empty-file case unreachable going forward: the
// key file is either absent or complete, never half-written, because it is
// renamed into place rather than written in place.
func TestLoadOrCreateKeyLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateKey(dir, "gateway_host_key"); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "gateway_host_key.pem" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want just the key: a temp file survived", names)
	}
	info, err := os.Stat(filepath.Join(dir, "gateway_host_key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	// The rename must not have widened the mode: this is a private key.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key mode = %o, want 600", perm)
	}
}

// writeFileAtomic must publish all-or-nothing, and must not leave its temp file
// behind when the write itself fails.
func TestWriteFileAtomicPublishesWholeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.pem")
	if err := writeFileAtomic(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("content = %q, want the second write whole", got)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Errorf("%d files in the directory, want 1: a temp file leaked", len(ents))
	}
}

// A key that IS valid must still be loaded rather than re-minted, whatever the
// recovery paths above do.
func TestLoadOrCreateKeyLoadsAnExistingValidKey(t *testing.T) {
	dir := t.TempDir()
	made, err := LoadOrCreateKey(dir, "gateway_upstream_key")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "gateway_upstream_key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := xssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatalf("what was written is not a parseable key: %v", err)
	}
	if PublicKeyLine(parsed) != PublicKeyLine(made) {
		t.Error("the file on disk is a different key from the one returned")
	}
}
