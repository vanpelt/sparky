package oidc

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// TokenTTL is how long a minted id token stays valid. One hour matches the
// kubernetes projected-token shape relying parties are built around. Tokens
// are single-use at the exchange (the verifier remembers jti), so a longer TTL
// buys no reuse — it only widens the window on a leaked token.
const TokenTTL = time.Hour

// Claims is the identity sparkbox asserts about a workload. The registered
// claims are filled in by Mint; the rest describe the sandbox and its owner.
type Claims struct {
	Subject  string
	Audience string

	Owner     string // sandbox owner's handle
	GitHub    string // verified github login, empty when unlinked
	KeyFP     string // ssh key that last authenticated the sandbox's session
	Sandbox   string // sandbox name
	SandboxID string // immutable sandbox identity, stable across rename
	Image     string // rootfs template
	Box       string // host node name
}

// SubjectFor builds the stable per-user subject. It is deliberately not
// per-sandbox: one hivemind service account (sub_selector
// "sparkbox:user:vanpelt") then covers all of a user's sandboxes, and
// per-sandbox scoping goes in the CEL policy against the `sandbox` claim.
func SubjectFor(handle string) string { return "sparkbox:user:" + handle }

// Issuer mints id tokens and serves the discovery + JWKS documents that let a
// relying party verify them.
type Issuer struct {
	issuerURL string
	signer    *ecdsa.PrivateKey
	kid       string
	jwks      []JWK    // signing key first, then any retired keys still verifiable
	audiences []string // allowlist; empty means "any audience"
}

type Options struct {
	// IssuerURL is the public https origin, e.g. "https://oidc.hivemind.tools".
	// It must match what relying parties are configured with exactly.
	IssuerURL string
	// Signer is the current ES256 key; every token is signed with it.
	Signer *ecdsa.PrivateKey
	// Previous, if set, is a retired key published in the JWKS but never used
	// to sign. Publishing it makes rotation a non-event: deploy the new key,
	// keep the old one verifiable for one TTL, then drop it.
	Previous *ecdsa.PrivateKey
	// Audiences allowlists the `aud` values this issuer will mint for. Empty
	// allows any audience.
	Audiences []string
}

func New(opts Options) (*Issuer, error) {
	if opts.Signer == nil {
		return nil, fmt.Errorf("oidc issuer needs a signing key")
	}
	if !strings.HasPrefix(opts.IssuerURL, "https://") && !strings.HasPrefix(opts.IssuerURL, "http://") {
		return nil, fmt.Errorf("oidc issuer url %q must be absolute", opts.IssuerURL)
	}
	jwk := jwkOf(&opts.Signer.PublicKey)
	iss := &Issuer{
		issuerURL: strings.TrimSuffix(opts.IssuerURL, "/"),
		signer:    opts.Signer,
		kid:       jwk.Kid,
		jwks:      []JWK{jwk},
		audiences: opts.Audiences,
	}
	if opts.Previous != nil {
		iss.jwks = append(iss.jwks, jwkOf(&opts.Previous.PublicKey))
	}
	return iss, nil
}

// URL is the issuer origin, as it appears in the `iss` claim.
func (i *Issuer) URL() string { return i.issuerURL }

// AudienceAllowed reports whether this issuer will mint for aud.
func (i *Issuer) AudienceAllowed(aud string) bool {
	return len(i.audiences) == 0 || slices.Contains(i.audiences, aud)
}

// Mint returns a fresh, signed id token. Every call produces a new jti: the
// exchange remembers (iss, jti) and accepts each token exactly once, so tokens
// must never be cached or served twice.
func (i *Issuer) Mint(c Claims) (string, time.Time, error) {
	if c.Subject == "" || c.Audience == "" {
		return "", time.Time{}, fmt.Errorf("id token needs a subject and audience")
	}
	if !i.AudienceAllowed(c.Audience) {
		return "", time.Time{}, fmt.Errorf("audience %q is not allowed by this issuer", c.Audience)
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", time.Time{}, err
	}
	now := time.Now().UTC()
	exp := now.Add(TokenTTL)

	payload := map[string]any{
		"iss": i.issuerURL,
		"sub": c.Subject,
		"aud": c.Audience,
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": exp.Unix(),
		"jti": hex.EncodeToString(jti),
	}
	for k, v := range map[string]string{
		"owner": c.Owner, "github": c.GitHub, "key_fp": c.KeyFP,
		"sandbox": c.Sandbox, "sandbox_id": c.SandboxID, "image": c.Image, "box": c.Box,
	} {
		// `github` must be absent rather than empty when unverified: a policy
		// like claims.github == "x" should fail closed, and an empty string is a
		// value a caller could otherwise be tempted to treat as a match.
		if v != "" {
			payload[k] = v
		}
	}

	token, err := i.sign(payload)
	if err != nil {
		return "", time.Time{}, err
	}
	return token, exp, nil
}

// sign renders a compact JWS: base64url(header).base64url(payload).base64url(sig).
func (i *Issuer) sign(payload map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": i.kid})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	signingInput := b64(header) + "." + b64(body)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, i.signer, digest[:])
	if err != nil {
		return "", err
	}
	// JWS ES256 signatures are the raw r||s pair, each left-padded to the
	// curve's 32-byte coordinate size — not the ASN.1 DER that crypto/ecdsa's
	// SignASN1 produces.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// DiscoveryPath and JWKSPath are the two GET endpoints a verifier fetches.
const (
	DiscoveryPath = "/.well-known/openid-configuration"
	JWKSPath      = "/jwks.json"
)

// Handler serves the discovery document and JWKS. Mount it at the issuer
// origin — every other path 404s, since this issuer has no login flow.
func (i *Issuer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+DiscoveryPath, func(w http.ResponseWriter, _ *http.Request) {
		// The *_supported fields are boilerplate verifiers expect to find, not
		// capabilities we implement: there is no authorization_endpoint because
		// nothing here logs a human in.
		writeJSON(w, map[string]any{
			"issuer":                                i.issuerURL,
			"jwks_uri":                              i.issuerURL + JWKSPath,
			"response_types_supported":              []string{"id_token"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"ES256"},
			"scopes_supported":                      []string{"openid"},
			"claims_supported": []string{
				"iss", "sub", "aud", "exp", "iat", "nbf", "jti",
				"owner", "github", "key_fp", "sandbox", "sandbox_id", "image", "box",
			},
		})
	})
	mux.HandleFunc("GET "+JWKSPath, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"keys": i.jwks})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Verifiers cache JWKS on their own schedule; a short TTL keeps a rotation
	// from taking hours to propagate.
	w.Header().Set("Cache-Control", "public, max-age=300")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}
