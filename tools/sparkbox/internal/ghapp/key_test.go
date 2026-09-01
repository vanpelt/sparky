package ghapp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testKey is generated once for the whole package. A 2048-bit keygen is a
// tenth of a second and these tests want a dozen apps; generating per test
// turns a fast suite into a slow one for no coverage.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
})

func pkcs1PEM(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func pkcs8PEM(t *testing.T, key any) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// Both encodings have to load: PKCS#1 is what github's settings page downloads,
// PKCS#8 is what the same key looks like after a round trip through openssl or
// a secret manager that re-encodes.
func TestLoadKeyAcceptsBothEncodings(t *testing.T) {
	want := testKey()
	for name, data := range map[string][]byte{
		"pkcs1": pkcs1PEM(t, want),
		"pkcs8": pkcs8PEM(t, want),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := LoadKey(data)
			if err != nil {
				t.Fatalf("LoadKey: %v", err)
			}
			if got.N.Cmp(want.N) != 0 || got.D.Cmp(want.D) != 0 {
				t.Fatal("loaded a different key than was written")
			}
		})
	}
}

// The realistic way to land a non-RSA key here is another fleet PEM being
// copied into the app key's slot, so the error names what was found.
func TestLoadKeyRefusesANonRSAKey(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadKey(pkcs8PEM(t, ec))
	if err == nil {
		t.Fatal("an ECDSA key was accepted as a github app key")
	}
	if !strings.Contains(err.Error(), "ecdsa") {
		t.Errorf("error %q does not name what it found", err)
	}
}

func TestLoadKeyRefusesGarbage(t *testing.T) {
	if _, err := LoadKey([]byte("not a pem file at all")); err == nil {
		t.Fatal("a non-PEM file was accepted")
	}
	if _, err := LoadKey(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("junk")})); err == nil {
		t.Fatal("a PEM block full of junk was accepted")
	}
}

// A locally generated placeholder is the failure this catches: it parses, it
// looks like a key, and every mint with it would fail with a 401 that names
// none of this.
func TestLoadKeyRefusesAnUndersizedKey(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	_, err = LoadKey(pkcs1PEM(t, small))
	if err == nil {
		t.Fatal("a 1024-bit key was accepted")
	}
	if !strings.Contains(err.Error(), "1024") {
		t.Errorf("error %q does not say how big the key was", err)
	}
}

// Absence is the normal state — the app key is an optional fleet secret — so it
// must not be an error. A file that exists and is broken still must be.
func TestLoadKeyIfPresent(t *testing.T) {
	dir := t.TempDir()

	key, err := LoadKeyIfPresent(dir, "github_app_key")
	if err != nil || key != nil {
		t.Fatalf("missing key: got (%v, %v), want (nil, nil)", key, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "empty.pem"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if key, err := LoadKeyIfPresent(dir, "empty"); err != nil || key != nil {
		t.Fatalf("zero-byte key: got (%v, %v), want (nil, nil)", key, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "broken.pem"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyIfPresent(dir, "broken"); err == nil {
		t.Fatal("a corrupt key file was silently treated as no key at all")
	}

	if err := os.WriteFile(filepath.Join(dir, "good.pem"), pkcs1PEM(t, testKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadKeyIfPresent(dir, "good")
	if err != nil || got == nil {
		t.Fatalf("good key: got (%v, %v)", got, err)
	}
	got, err = LoadKeyFileIfPresent(filepath.Join(dir, "good.pem"))
	if err != nil || got == nil {
		t.Fatalf("good key by path: got (%v, %v)", got, err)
	}
}
