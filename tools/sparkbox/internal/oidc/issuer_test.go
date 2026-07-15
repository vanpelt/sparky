package oidc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
)

func testIssuer(t *testing.T, auds ...string) (*Issuer, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	iss, err := New(Options{IssuerURL: "https://oidc.example.test", Signer: key, Audiences: auds})
	if err != nil {
		t.Fatal(err)
	}
	return iss, key
}

// verify is a from-scratch ES256 check: split the compact JWS, re-hash the
// signing input, and check the raw r||s pair against the public key. If a
// relying party can't do exactly this, federation doesn't work — so the test
// deliberately doesn't reuse the signing code's helpers.
func verify(t *testing.T, token string, pub *ecdsa.PublicKey) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("want 3 JWS parts, got %d", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature is not base64url: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("ES256 signature must be 64 bytes (r||s), got %d", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		t.Fatal("signature does not verify")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func TestMintProducesAVerifiableES256Token(t *testing.T) {
	iss, key := testIssuer(t)
	token, exp, err := iss.Mint(Claims{
		Subject: SubjectFor("vanpelt"), Audience: "https://hivemind.wandb.tools",
		Owner: "vanpelt", GitHub: "vanpelt", Sandbox: "tidy-meteor",
		Image: "universal", Box: "sparkbox-07151236",
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := verify(t, token, &key.PublicKey)

	for field, want := range map[string]string{
		"iss": "https://oidc.example.test", "sub": "sparkbox:user:vanpelt",
		"aud": "https://hivemind.wandb.tools", "owner": "vanpelt",
		"github": "vanpelt", "sandbox": "tidy-meteor", "image": "universal",
		"box": "sparkbox-07151236",
	} {
		if got, _ := claims[field].(string); got != want {
			t.Errorf("claim %s = %q, want %q", field, got, want)
		}
	}
	// Hivemind requires all of these to be present.
	for _, field := range []string{"exp", "iat", "nbf", "jti"} {
		if claims[field] == nil {
			t.Errorf("claim %s is missing", field)
		}
	}
	if got := int64(claims["exp"].(float64)); got != exp.Unix() {
		t.Errorf("exp claim %d disagrees with returned expiry %d", got, exp.Unix())
	}

	// The header must name ES256 and the kid that's published in the JWKS, or a
	// verifier can't pick the right key.
	head, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]string
	if err := json.Unmarshal(head, &header); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "ES256" {
		t.Errorf("alg = %q, want ES256 (verifiers allowlist RS256/ES256 only)", header["alg"])
	}
	if header["kid"] != iss.jwks[0].Kid {
		t.Errorf("kid %q is not the published JWKS kid %q", header["kid"], iss.jwks[0].Kid)
	}
}

// Each token is exchangeable exactly once — the verifier remembers (iss, jti)
// — so a repeated jti would make the second fetch useless.
func TestMintUsesAFreshJTIEveryTime(t *testing.T) {
	iss, key := testIssuer(t)
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		token, _, err := iss.Mint(Claims{Subject: "s", Audience: "a"})
		if err != nil {
			t.Fatal(err)
		}
		jti, _ := verify(t, token, &key.PublicKey)["jti"].(string)
		if jti == "" {
			t.Fatal("empty jti")
		}
		if seen[jti] {
			t.Fatalf("jti %q reused — the second token would be rejected as a replay", jti)
		}
		seen[jti] = true
	}
}

// A policy like `claims.github == "x"` must fail closed for an unlinked user,
// so the claim has to be absent rather than empty.
func TestUnverifiedGitHubClaimIsAbsentNotEmpty(t *testing.T) {
	iss, key := testIssuer(t)
	token, _, err := iss.Mint(Claims{Subject: "s", Audience: "a", Owner: "vanpelt"})
	if err != nil {
		t.Fatal(err)
	}
	claims := verify(t, token, &key.PublicKey)
	if _, present := claims["github"]; present {
		t.Errorf("github claim present (%v) for an unlinked user; it must be omitted", claims["github"])
	}
}

func TestAudienceAllowlistIsClosedByDefault(t *testing.T) {
	iss, _ := testIssuer(t, "https://hivemind.wandb.tools")
	if _, _, err := iss.Mint(Claims{Subject: "s", Audience: "https://evil.example"}); err == nil {
		t.Error("minted a token for an audience outside the allowlist")
	}
	if _, _, err := iss.Mint(Claims{Subject: "s", Audience: "https://hivemind.wandb.tools"}); err != nil {
		t.Errorf("refused an allowlisted audience: %v", err)
	}

	// An empty allowlist means any audience.
	open, _ := testIssuer(t)
	if _, _, err := open.Mint(Claims{Subject: "s", Audience: "https://anything.example"}); err != nil {
		t.Errorf("empty allowlist should permit any audience: %v", err)
	}
}

// The discovery document and JWKS are the entire contract a verifier consumes:
// it fetches the former, follows jwks_uri, and matches the token's kid.
func TestDiscoveryAndJWKSAreConsistent(t *testing.T) {
	iss, key := testIssuer(t)
	prev, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	withPrev, err := New(Options{IssuerURL: "https://oidc.example.test", Signer: key, Previous: prev})
	if err != nil {
		t.Fatal(err)
	}

	get := func(h *Issuer, path string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		h.Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	disc := get(iss, DiscoveryPath)
	if disc["issuer"] != "https://oidc.example.test" {
		t.Errorf("issuer = %v", disc["issuer"])
	}
	if disc["jwks_uri"] != "https://oidc.example.test/jwks.json" {
		t.Errorf("jwks_uri = %v", disc["jwks_uri"])
	}
	if algs, _ := disc["id_token_signing_alg_values_supported"].([]any); len(algs) != 1 || algs[0] != "ES256" {
		t.Errorf("id_token_signing_alg_values_supported = %v, want [ES256]", disc["id_token_signing_alg_values_supported"])
	}

	// Rotation: both keys are published, but only the current one signs.
	keys, _ := get(withPrev, JWKSPath)["keys"].([]any)
	if len(keys) != 2 {
		t.Fatalf("want current + previous key in JWKS, got %d", len(keys))
	}
	first, _ := keys[0].(map[string]any)
	if first["crv"] != "P-256" || first["kty"] != "EC" || first["alg"] != "ES256" {
		t.Errorf("JWKS entry is malformed: %v", first)
	}
	token, _, err := withPrev.Mint(Claims{Subject: "s", Audience: "a"})
	if err != nil {
		t.Fatal(err)
	}
	verify(t, token, &key.PublicKey) // signed by the current key, never the retired one
}

// A coordinate with a leading zero byte must stay 32 bytes wide, or verifiers
// compute a different thumbprint and reject the key.
func TestJWKCoordinatesAreFixedWidth(t *testing.T) {
	small := big.NewInt(1)
	if got := coord(small); len(mustDecode(t, got)) != 32 {
		t.Errorf("coord(1) decoded to %d bytes, want 32", len(mustDecode(t, got)))
	}
}

func mustDecode(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
