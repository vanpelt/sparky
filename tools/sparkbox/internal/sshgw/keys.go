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
func LoadOrCreateKey(dir, name string) (xssh.Signer, error) {
	path := filepath.Join(dir, name+".pem")
	if data, err := os.ReadFile(path); err == nil {
		return xssh.ParsePrivateKey(data)
	} else if !os.IsNotExist(err) {
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
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, err
	}
	return xssh.NewSignerFromKey(priv)
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
