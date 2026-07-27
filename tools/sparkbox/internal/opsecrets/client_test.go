package opsecrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubOP writes a fake `op` and returns its path plus the directory it records
// invocations into. Items live as files under fixtures/: the stub cats one
// verbatim, so a payload's exact bytes (trailing newline and all) are what the
// client sees. The error wordings are copied from the real CLI (op 2.33.0).
func stubOP(t *testing.T, fixtures map[string]string) (bin, recordDir string) {
	t.Helper()
	dir := t.TempDir()
	fixDir := filepath.Join(dir, "fixtures")
	if err := os.MkdirAll(fixDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, payload := range fixtures {
		if err := os.WriteFile(filepath.Join(fixDir, name), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recordDir = filepath.Join(dir, "record")
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		t.Fatal(err)
	}

	script := `#!/bin/sh
ref=""
for a in "$@"; do
  case "$a" in op://*) ref="$a" ;; esac
done
printf '%s\n' "$@" > "` + recordDir + `/argv"
printf '%s' "${OP_SERVICE_ACCOUNT_TOKEN-}" > "` + recordDir + `/token"

vault=$(printf '%s' "$ref" | cut -d/ -f3)
item=$(printf '%s' "$ref" | cut -d/ -f4)
field=$(printf '%s' "$ref" | cut -d/ -f5)

if [ "$vault" != "Sparkbox" ]; then
  echo "[ERROR] could not read secret '$ref': could not get item $vault/$item: \"$vault\" isn't a vault in this account. Specify the vault with its ID or name." >&2
  exit 1
fi
if [ "$item" = "signed-out" ]; then
  echo "[ERROR] could not read secret '$ref': you are not currently signed in. Please run 'op signin --help'." >&2
  exit 1
fi
if [ ! -f "` + fixDir + `/$item" ]; then
  echo "[ERROR] could not read secret '$ref': could not get item Sparkbox/$item: \"$item\" isn't an item in the \"Sparkbox\" vault." >&2
  exit 1
fi
if [ "$field" != "password" ]; then
  echo "[ERROR] could not read secret '$ref': item 'Sparkbox/$item' does not have a field '$field'" >&2
  exit 1
fi
cat "` + fixDir + `/$item"
`
	bin = filepath.Join(dir, "op")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, recordDir
}

const testPEM = "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEEIL\n-----END EC PRIVATE KEY-----\n"

func newTestClient(t *testing.T, bin string, mutate func(*Config)) *Client {
	t.Helper()
	cfg := Config{Vault: "Sparkbox", Bin: bin}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// A PEM must survive the round trip byte for byte — including its trailing
// newline, which is why the client passes --no-newline rather than trimming.
func TestReadIsByteExact(t *testing.T) {
	bin, _ := stubOP(t, map[string]string{"oidc-signing-key": testPEM})
	got, err := newTestClient(t, bin, nil).Read(context.Background(), "oidc-signing-key")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != testPEM {
		t.Errorf("payload = %q, want %q", got, testPEM)
	}
}

func TestReadPassesNoNewline(t *testing.T) {
	bin, rec := stubOP(t, map[string]string{"console-password": "hunter2"})
	if _, err := newTestClient(t, bin, nil).Read(context.Background(), "console-password"); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(rec, "argv"))
	if !strings.Contains(string(argv), "--no-newline") {
		t.Errorf("argv missing --no-newline:\n%s", argv)
	}
	if !strings.Contains(string(argv), "op://Sparkbox/console-password/password") {
		t.Errorf("argv missing the expected reference:\n%s", argv)
	}
}

// A missing item and a missing field both mean "not stored"; a missing vault
// and a signed-out session must NOT, or a typo would silently blank every
// optional secret instead of failing where the operator can see it.
func TestReadClassifiesFailures(t *testing.T) {
	bin, _ := stubOP(t, map[string]string{"present": "v"})
	for _, tc := range []struct {
		name      string
		item      string
		vault     string
		field     string
		wantErr   error
		wantNotIs error
	}{
		{name: "missing item", item: "absent", wantErr: ErrNotFound},
		{name: "missing field", item: "present", field: "nope", wantErr: ErrNotFound},
		{name: "missing vault", item: "present", vault: "Typo", wantNotIs: ErrNotFound},
		{name: "signed out", item: "signed-out", wantErr: ErrNotSignedIn, wantNotIs: ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, bin, func(cfg *Config) {
				if tc.vault != "" {
					cfg.Vault = tc.vault
				}
				cfg.Field = tc.field
			})
			_, err := c.Read(context.Background(), tc.item)
			if err == nil {
				t.Fatal("want an error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantNotIs != nil && errors.Is(err, tc.wantNotIs) {
				t.Errorf("error %v must not be treated as %v", err, tc.wantNotIs)
			}
		})
	}
}

// The token is a bearer credential for the whole vault: it must reach op through
// the environment, never argv, where any process on the host could read it.
func TestTokenGoesToEnvNotArgv(t *testing.T) {
	bin, rec := stubOP(t, map[string]string{"present": "v"})
	c := newTestClient(t, bin, func(cfg *Config) { cfg.Token = "ops_secret_token" })
	if _, err := c.Read(context.Background(), "present"); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(rec, "argv"))
	if strings.Contains(string(argv), "ops_secret_token") {
		t.Errorf("token leaked into argv:\n%s", argv)
	}
	tok, _ := os.ReadFile(filepath.Join(rec, "token"))
	if string(tok) != "ops_secret_token" {
		t.Errorf("token in child env = %q, want it passed through", tok)
	}
}

// In account mode an inherited token would silently redirect the read to
// whichever account minted it, so the client strips it from the child env.
func TestAccountModeStripsInheritedToken(t *testing.T) {
	bin, rec := stubOP(t, map[string]string{"present": "v"})
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "inherited_token")
	c := newTestClient(t, bin, func(cfg *Config) { cfg.Account = "vanpelt.1password.com" })
	if _, err := c.Read(context.Background(), "present"); err != nil {
		t.Fatal(err)
	}
	if tok, _ := os.ReadFile(filepath.Join(rec, "token")); string(tok) != "" {
		t.Errorf("inherited token = %q, want it stripped in account mode", tok)
	}
	argv, _ := os.ReadFile(filepath.Join(rec, "argv"))
	if !strings.Contains(string(argv), "--account") {
		t.Errorf("argv missing --account:\n%s", argv)
	}
}

func TestMissingBinaryIsNotNotFound(t *testing.T) {
	c := newTestClient(t, filepath.Join(t.TempDir(), "no-such-op"), nil)
	_, err := c.Read(context.Background(), "present")
	if err == nil {
		t.Fatal("want an error when op is not installed")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("a missing op binary must not read as a missing secret: %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no vault", Config{}},
		{"vault with slash", Config{Vault: "a/b"}},
		{"field with slash", Config{Vault: "v", Field: "a/b"}},
		{"both auth modes", Config{Vault: "v", Account: "acct", Token: "tok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Error("want a validation error")
			}
		})
	}
}

func TestReadRejectsSlashInItem(t *testing.T) {
	bin, _ := stubOP(t, nil)
	// A name with a slash would silently change which field the reference
	// addresses, so it is refused rather than sent to op.
	if _, err := newTestClient(t, bin, nil).Read(context.Background(), "a/b"); err == nil {
		t.Error("want an error for an item name containing a slash")
	}
}
