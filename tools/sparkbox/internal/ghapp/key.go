package ghapp

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
)

// minKeyBits refuses a key too small to be one GitHub issued. Every App key
// github hands out is 2048 bits; anything shorter arrived some other way, and
// the likeliest way is an operator or a script generating a placeholder to make
// a missing-secret error go away — which would then fail every mint with a 401
// that names none of this. Failing at load, on the sentence "this is not the
// key github gave you", is much cheaper.
const minKeyBits = 2048

// LoadKey parses a GitHub App private key.
//
// PKCS#1 first, because that is what the App settings page downloads: a file
// beginning "-----BEGIN RSA PRIVATE KEY-----". PKCS#8 ("BEGIN PRIVATE KEY") is
// the fallback, because an operator who has round-tripped the key through
// `openssl pkcs8` or a secret manager's re-encoding will have that instead, and
// there is no reason to make them convert it back. This mirrors how
// oidc.parseECPrivateKey accepts both of its own two encodings.
func LoadKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in the github app key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return checkRSA(key)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		// Name what was found. The realistic way to get here is a fleet's OIDC
		// key or an ed25519 host key landing in the App key's slot, and "github
		// app key is a *ecdsa.PrivateKey" says which file was copied over which
		// far better than "invalid key" does.
		return nil, fmt.Errorf("github app key is %T, need an RSA key — github issues app keys as PKCS#1 RSA", key)
	}
	return checkRSA(rsaKey)
}

// LoadKeyIfPresent returns the key at <dir>/<name>.pem, or nil when the file
// does not exist.
//
// Absence is the normal state, not a failure: the App key is an OPTIONAL fleet
// secret (internal/bootsecrets), so a fleet that has not created an App boots
// exactly as before and answers repo verbs Disabled. A file that exists and
// cannot be parsed is still an error — that one is a broken deploy, and the
// difference between "no app" and "a broken app" is worth a loud sentence.
func LoadKeyIfPresent(dir, name string) (*rsa.PrivateKey, error) {
	return LoadKeyFileIfPresent(filepath.Join(dir, name+".pem"))
}

// LoadKeyFileIfPresent is LoadKeyIfPresent for a fully qualified path. It is
// used when the GitHub App credentials have a lifecycle and mount separate
// from the fleet identity key directory.
func LoadKeyFileIfPresent(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		// bootsecrets writes atomically, so a zero-byte file is not a partial
		// write it produced; it is a placeholder somebody touched. Treat it as
		// absent rather than as a parse failure that blocks the whole boot.
		return nil, nil
	}
	return LoadKey(data)
}

func checkRSA(key *rsa.PrivateKey) (*rsa.PrivateKey, error) {
	if bits := key.N.BitLen(); bits < minKeyBits {
		return nil, fmt.Errorf("github app key is %d bits, need at least %d — this is not a key github issued", bits, minKeyBits)
	}
	// Precompute is what makes repeated signing cheap, and every API call here
	// signs. ParsePKCS1PrivateKey already does it; the PKCS#8 path may not, and
	// doing it twice costs nothing.
	key.Precompute()
	return key, nil
}
