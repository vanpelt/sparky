package sshgw

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	xssh "golang.org/x/crypto/ssh"
)

// LoadOrCreateKey returns a persistent ed25519 signer stored at
// <dir>/<name>.pem, generating it on first use. Used for both the gateway's
// host key and the key it authenticates upstream (into VMs) with.
//
// # Why the write is atomic
//
// os.WriteFile creates the file and then writes it, so an interrupted write —
// the disk filling underneath it is the one we hit — leaves a zero-byte
// <name>.pem behind. The first boot then fails with the write's own error, and
// every boot after it fails with `ssh: no key found`, because the file now
// exists and ReadFile succeeds. Restart=always turns that into a permanent
// crash loop that no restart, reboot or reprovision recovers: the repair is
// deleting a file by hand, and nothing tells the operator which one.
//
// A gateway that cannot mint its own host key is a host nobody can log into, so
// this is written the way the rest of the tree writes anything durable: to a
// temp file in the same directory, fsynced, then renamed. rename(2) within a
// directory is atomic, so the key file is either absent or complete, and the
// failure mode above cannot be produced.
func LoadOrCreateKey(dir, name string) (xssh.Signer, error) {
	path := filepath.Join(dir, name+".pem")
	switch data, err := os.ReadFile(path); {
	case err == nil && len(data) > 0:
		return xssh.ParsePrivateKey(data)
	case err == nil:
		// Zero bytes, which is the wreckage of a pre-atomic-write interruption
		// on a host provisioned by an older build. There is nothing in an empty
		// file to lose and nothing that could have been derived from it, so
		// minting over it is safe — and it is the only reading that gets the
		// host back without an operator knowing to delete this exact path.
		//
		// Deliberately only for a file of length zero. A short-but-non-empty
		// file could be a key in a format this build does not parse, and
		// replacing THAT would silently change an identity clients have pinned
		// and rootfs images trust — so it falls through to ParsePrivateKey
		// below and is reported, not overwritten.
	case !os.IsNotExist(err):
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := xssh.MarshalPrivateKey(priv, "sparkbox "+name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, pem.EncodeToMemory(block)); err != nil {
		return nil, err
	}
	return xssh.NewSignerFromKey(priv)
}

// writeFileAtomic writes data to path via a temp file in the same directory,
// fsynced and renamed, so a reader never sees a partial file and an interrupted
// write leaves nothing behind.
//
// The temp file is created in the DESTINATION directory rather than TempDir:
// rename(2) is only atomic within a filesystem, and a key directory is
// routinely a mount of its own (the data volume `setup` provisions is exactly
// that).
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Best-effort cleanup on every failure path below. Harmless after a
	// successful rename, when the name no longer exists.
	defer os.Remove(tmp) //nolint:errcheck
	if err := f.Chmod(0o600); err != nil {
		f.Close() //nolint:errcheck
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close() //nolint:errcheck
		return err
	}
	// Sync before the rename, not after: the rename is what publishes the file,
	// and publishing a name that points at unflushed blocks is the crash-safety
	// hole this whole function exists to close.
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadKey returns the signer at <dir>/<name>.pem, erroring if it does not
// exist. A fleet host uses this instead of LoadOrCreateKey: its keys are
// hydrated from Secret Manager before sparkbox starts, so a missing file means
// the fetch failed — and silently generating a fresh one would mint a new fleet
// identity that no rootfs trusts and no client has pinned, locking everyone out.
func LoadKey(dir, name string) (xssh.Signer, error) {
	data, err := os.ReadFile(filepath.Join(dir, name+".pem"))
	if err != nil {
		return nil, err
	}
	return xssh.ParsePrivateKey(data)
}

// PublicKeyLine renders a signer's public key as an authorized_keys line.
func PublicKeyLine(s xssh.Signer) string {
	return fmt.Sprintf("%s", xssh.MarshalAuthorizedKey(s.PublicKey()))
}
