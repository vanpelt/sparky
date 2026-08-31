// Package edgeauth is the browser-session layer for the authenticated HTTP
// edge: a user mints a signed session token from their SSH key (via
// `ssh ctl@<gateway> session-token`), and the proxy verifies it to gate private
// routes and to forward the visitor's identity upstream.
//
// The token asserts one thing — "this handle/email, until this expiry" — to the
// edge and to nobody else. That narrow audience is why it is a keyed MAC, not a
// public-key JWT: the edge is the only verifier, so a per-request HMAC is both
// faster and simpler than an ES256 verify, and needs no key distribution beyond
// the fleet secret it is derived from.
//
// The MAC key is derived (HKDF-SHA256) from the OIDC signing key already
// present on every host, so authenticated forwarding adds no new fleet secret.
// An OIDC key rotation therefore also invalidates outstanding sessions — an
// acceptable, arguably desirable, side effect: a session is re-minted with one
// ssh command.
package edgeauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

// CookieName is the session cookie the edge sets and reads. The same token
// value is also accepted as an Authorization: Bearer credential.
const CookieName = "spark_session"

// TokenPrefix versions the wire format so a future change is unambiguous.
// Exported because the proxy's Rewrite hook recognises (and strips) session
// tokens in guest-bound Authorization headers by this prefix — the two must
// never drift apart.
const TokenPrefix = "spk_v1."

// hkdfInfo domain-separates the edge-session key from any other use of the
// OIDC key material.
const hkdfInfo = "sparkbox-edge-session/v1"

// Identity is what a valid session asserts about a visitor.
type Identity struct {
	Handle string `json:"h"`
	Email  string `json:"e,omitempty"`
}

// claims is the wire payload: an Identity plus issued-at and expiry.
type claims struct {
	Identity
	IssuedAt int64 `json:"iat"`
	Expiry   int64 `json:"exp"`
}

// Signer mints and verifies session tokens with a key derived from the fleet's
// OIDC signing material.
type Signer struct {
	key []byte
}

// NewSigner derives the MAC key from ikm (the OIDC private key's scalar bytes).
// It panics only if the platform's SHA-256 is unavailable, which never happens.
func NewSigner(ikm []byte) *Signer {
	r := hkdf.New(sha256.New, ikm, nil, []byte(hkdfInfo))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		panic("edgeauth: hkdf: " + err.Error())
	}
	return &Signer{key: key}
}

// Mint returns a signed token for id, valid for ttl, and its expiry time.
func (s *Signer) Mint(id Identity, ttl time.Duration) (string, time.Time, error) {
	if id.Handle == "" {
		return "", time.Time{}, fmt.Errorf("session token needs a handle")
	}
	now := time.Now().UTC()
	exp := now.Add(ttl)
	body, err := json.Marshal(claims{Identity: id, IssuedAt: now.Unix(), Expiry: exp.Unix()})
	if err != nil {
		return "", time.Time{}, err
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return TokenPrefix + payload + "." + s.mac(TokenPrefix, payload), exp, nil
}

// mac is the base64url HMAC-SHA256 over the versioned payload. The prefix is
// part of the signed input so a token can't be replayed under a future format —
// and, since TicketPrefix is a different string, so a credential minted for one
// purpose can never verify as the other. See MintTicket.
func (s *Signer) mac(prefix, payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(prefix))
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Verify checks a token's MAC and expiry and returns the asserted identity.
func (s *Signer) Verify(token string) (Identity, bool) {
	rest, ok := strings.CutPrefix(token, TokenPrefix)
	if !ok {
		return Identity{}, false
	}
	payload, sig, ok := strings.Cut(rest, ".")
	if !ok {
		return Identity{}, false
	}
	// Constant-time compare so a caller can't time-probe the MAC byte by byte.
	if subtle.ConstantTimeCompare([]byte(sig), []byte(s.mac(TokenPrefix, payload))) != 1 {
		return Identity{}, false
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Identity{}, false
	}
	var c claims
	if err := json.Unmarshal(body, &c); err != nil {
		return Identity{}, false
	}
	if c.Handle == "" || time.Now().Unix() >= c.Expiry {
		return Identity{}, false
	}
	return c.Identity, true
}

// IdentityFrom extracts and verifies the caller's session from a request,
// preferring the session cookie and falling back to an Authorization: Bearer
// header (the programmatic path). It returns ok=false when neither carries a
// valid token.
func (s *Signer) IdentityFrom(r *http.Request) (Identity, bool) {
	if c, err := r.Cookie(CookieName); err == nil {
		if id, ok := s.Verify(c.Value); ok {
			return id, true
		}
	}
	if h := r.Header.Get("Authorization"); h != "" {
		if tok, ok := strings.CutPrefix(h, "Bearer "); ok {
			if id, ok := s.Verify(strings.TrimSpace(tok)); ok {
				return id, true
			}
		}
	}
	return Identity{}, false
}
