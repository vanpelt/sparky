// Package oidc makes sparkbox an OpenID Connect identity-token issuer: it
// mints short-lived ES256 JWTs describing who owns a sandbox, and serves the
// discovery document and JWKS that let a relying party verify them.
//
// Only the id-token half of OIDC exists here — no authorization or token
// endpoints — which is the same shape GitHub Actions' issuer takes. Workloads
// don't log in; they present a token they were handed by a channel that
// already authenticates them (see internal/metadata).
//
// ES256, not our ed25519 gateway keys: hivemind's exchange handler allowlists
// RS256/ES256 only, so this needs its own P-256 key. It is a fleet secret
// distributed exactly like the gateway keys.
package oidc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
)

// LoadOrCreateKey returns the ES256 (P-256) signing key at <dir>/<name>.pem,
// generating it on first use — the same lifecycle as the gateway SSH keys, so
// a single-host dev run needs no setup and a fleet host gets the key injected
// by cloud-init before sparkbox first starts.
func LoadOrCreateKey(dir, name string) (*ecdsa.PrivateKey, error) {
	path := filepath.Join(dir, name+".pem")
	if data, err := os.ReadFile(path); err == nil {
		return parseECPrivateKey(data)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// LoadKeyIfPresent returns the key at <dir>/<name>.pem, or nil when the file
// doesn't exist. Used for the optional previous key that keeps a rotation
// verifiable while old tokens are still in flight.
func LoadKeyIfPresent(dir, name string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(filepath.Join(dir, name+".pem"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseECPrivateKey(data)
}

// parseECPrivateKey accepts both PKCS#8 ("PRIVATE KEY", what we write) and
// SEC1 ("EC PRIVATE KEY", what `openssl ecparam -genkey` hands an operator).
func parseECPrivateKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in oidc signing key")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ec, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("oidc signing key is %T, need an ECDSA P-256 key", key)
		}
		return checkP256(ec)
	}
	ec, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("oidc signing key: %w", err)
	}
	return checkP256(ec)
}

func checkP256(key *ecdsa.PrivateKey) (*ecdsa.PrivateKey, error) {
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("oidc signing key uses %s, but ES256 requires P-256", key.Curve.Params().Name)
	}
	return key, nil
}

// JWK is a public key in JSON Web Key form (RFC 7517), the shape a verifier
// fetches from jwks_uri.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	Kid string `json:"kid"`
}

// jwkOf renders a P-256 public key as a JWK with an RFC 7638 thumbprint kid,
// so the kid is derived from the key itself and stays stable across restarts.
func jwkOf(pub *ecdsa.PublicKey) JWK {
	x := coord(pub.X)
	y := coord(pub.Y)
	return JWK{Kty: "EC", Crv: "P-256", X: x, Y: y, Alg: "ES256", Use: "sig", Kid: thumbprint(x, y)}
}

// coord renders a curve coordinate as a fixed-width 32-byte base64url value —
// fixed width matters: a leading zero byte must be kept, not trimmed.
func coord(v *big.Int) string {
	b := make([]byte, 32)
	v.FillBytes(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// thumbprint is the RFC 7638 JWK thumbprint: SHA-256 over the canonical JSON
// of the required members, in lexicographic order and with no whitespace.
func thumbprint(x, y string) string {
	canonical, _ := json.Marshal(struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{"P-256", "EC", x, y})
	sum := sha256.Sum256(canonical)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
