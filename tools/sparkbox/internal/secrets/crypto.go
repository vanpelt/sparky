package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"log/slog"

	"golang.org/x/crypto/hkdf"
)

// kekInfo domain-separates the secrets KEK from every other use of the OIDC
// key material (edgeauth derives its session-MAC key from the same ikm under a
// different info string, so neither key reveals anything about the other).
const kekInfo = "sparkbox-secrets-kek/v1"

// keyID names the derivation scheme a row was encrypted under. Stored per row
// so a future re-encryption migration can tell old rows from new instead of
// guessing.
const keyID = "oidc-hkdf-v1"

// aadPrefix versions the additional-authenticated-data layout. The AAD binds a
// ciphertext to its exact row, so blobs cannot be spliced between owners or
// env names at the database level.
const aadPrefix = "sparkbox-secret/v1"

// DeriveKEK derives the 32-byte AES-256 key-encryption key from ikm (the OIDC
// private key's scalar bytes) — the edgeauth.NewSigner precedent, so secrets
// add no new fleet secret. It panics only if the platform's SHA-256 is
// unavailable, which never happens.
func DeriveKEK(ikm []byte) []byte {
	r := hkdf.New(sha256.New, ikm, nil, []byte(kekInfo))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		panic("secrets: hkdf: " + err.Error())
	}
	return key
}

// newAEAD builds the AES-256-GCM cipher used for every row.
func newAEAD(kek []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// aadFor is the per-row AAD: owner, env name, and row id are all
// authenticated, so re-homing a ciphertext under a different owner or name
// fails decryption rather than leaking a value.
func aadFor(owner, envName, id string) []byte {
	return []byte(aadPrefix + "|" + owner + "|" + envName + "|" + id)
}

// seal encrypts plaintext under aad with a fresh random 96-bit nonce and
// returns nonce||ciphertext, the on-disk blob layout.
func seal(aead cipher.AEAD, aad, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, aad), nil
}

// unseal decrypts a nonce||ciphertext blob. Every failure mode — truncated
// blob, wrong key, tampered AAD — collapses to ErrUndecryptable: the caller
// only needs "this row cannot be trusted", never why.
func unseal(aead cipher.AEAD, aad, blob []byte) ([]byte, error) {
	if len(blob) < aead.NonceSize() {
		return nil, ErrUndecryptable
	}
	pt, err := aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], aad)
	if err != nil {
		return nil, ErrUndecryptable
	}
	return pt, nil
}

// Resealer re-encrypts stored blobs under a different owner. It exists for
// offline migrations (cmd/rename-user): the AAD binds each ciphertext to its
// owner, so a handle rename must unseal and re-seal every row — through this
// type, under the same AAD layout and blob format the store itself uses.
type Resealer struct{ aead cipher.AEAD }

// NewResealer builds a Resealer from the KEK (see DeriveKEK).
func NewResealer(kek []byte) (*Resealer, error) {
	aead, err := newAEAD(kek)
	if err != nil {
		return nil, err
	}
	return &Resealer{aead: aead}, nil
}

// Reseal unseals blob bound to (from, envName, id) and seals it bound to
// (to, envName, id) with a fresh nonce.
func (r *Resealer) Reseal(from, to, envName, id string, blob []byte) ([]byte, error) {
	pt, err := unseal(r.aead, aadFor(from, envName, id), blob)
	if err != nil {
		return nil, err
	}
	return seal(r.aead, aadFor(to, envName, id), pt)
}

// secretValue wraps a plaintext secret so it cannot leak into a log line:
// slog renders it via LogValue, fmt via String/GoString — all redacted. Any
// code in this package that must mention a value logs it wrapped.
type secretValue string

var _ slog.LogValuer = secretValue("")

func (secretValue) LogValue() slog.Value { return slog.StringValue("[redacted]") }
func (secretValue) String() string       { return "[redacted]" }
func (secretValue) GoString() string     { return "[redacted]" }
