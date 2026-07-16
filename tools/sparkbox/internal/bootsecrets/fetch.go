// Package bootsecrets pulls the fleet's secrets from Scaleway Secret Manager at
// host boot into tmpfs, replacing the plaintext copies that used to ride in
// cloud-init user-data. The three private keys land as PEM files under KeyDir;
// the two passwords are written as a systemd EnvironmentFile. Everything is
// meant for /run, so a captured disk or a pulled drive yields no key material.
package bootsecrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/scwsecrets"
)

// Config controls a fetch. Token is the Scaleway secret key; it comes from the
// environment, never an argv flag, so it can't leak into the process table.
type Config struct {
	BaseURL   string // "" for the real API; set in tests
	Region    string
	ProjectID string
	Token     string
	Path      string // Secret Manager folder, e.g. "/sparkbox/fleet"
	KeyDir    string // where PEMs are written (put on tmpfs)
	EnvOut    string // EnvironmentFile to write (put on tmpfs)
	Log       io.Writer
}

type kind int

const (
	sshKeyPEM kind = iota // type=ssh_key: JSON-wrapped PEM
	opaquePEM             // opaque: payload is already a PEM
	envVar                // opaque: payload is a value for an env var
)

// manifest is the fixed set of fleet secrets and how each maps onto the host.
// required decides whether a missing secret fails the boot: the three keys are
// load-bearing; the two passwords are optional (a box may run without a console
// or Cloudflare).
var manifest = []struct {
	name     string
	kind     kind
	dest     string // filename under KeyDir, or env var name
	required bool
}{
	{"gateway-host-key", sshKeyPEM, "gateway_host_key.pem", true},
	{"gateway-upstream-key", sshKeyPEM, "gateway_upstream_key.pem", true},
	{"oidc-signing-key", opaquePEM, "oidc_signing_key.pem", true},
	{"cloudflare-api-token", envVar, "CLOUDFLARE_API_TOKEN", false},
	{"console-password", envVar, "SPARKBOX_CONSOLE_PASSWORD", false},
}

// Fetch reads every manifest secret and materializes it. It is safe to re-run:
// each write is atomic, so a reader never sees a half-written key.
func Fetch(ctx context.Context, cfg Config) error {
	if cfg.Token == "" {
		return errors.New("SCW_SECRET_KEY is not set (the host's Secret Manager access key)")
	}
	if cfg.ProjectID == "" {
		return errors.New("project ID is required (SCW_DEFAULT_PROJECT_ID)")
	}
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	if err := os.MkdirAll(cfg.KeyDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.EnvOut), 0o700); err != nil {
		return err
	}

	client := scwsecrets.New(cfg.BaseURL, cfg.Region, cfg.ProjectID, cfg.Token)
	env := map[string]string{}

	for _, s := range manifest {
		payload, secretType, err := client.AccessByPath(ctx, cfg.Path, s.name)
		if errors.Is(err, scwsecrets.ErrNotFound) {
			if s.required {
				return fmt.Errorf("required secret %s/%s is missing", strings.TrimRight(cfg.Path, "/"), s.name)
			}
			fmt.Fprintf(cfg.Log, "fetch-secrets: %s not set, skipping\n", s.name)
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}

		switch s.kind {
		case sshKeyPEM:
			if secretType != "ssh_key" {
				return fmt.Errorf("%s: expected type ssh_key, got %q", s.name, secretType)
			}
			pem, err := scwsecrets.UnwrapSSHKey(payload)
			if err != nil {
				return fmt.Errorf("%s: %w", s.name, err)
			}
			if err := writeFileAtomic(filepath.Join(cfg.KeyDir, s.dest), pem, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cfg.Log, "fetch-secrets: wrote %s (%d bytes)\n", s.dest, len(pem))
		case opaquePEM:
			if err := writeFileAtomic(filepath.Join(cfg.KeyDir, s.dest), payload, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cfg.Log, "fetch-secrets: wrote %s (%d bytes)\n", s.dest, len(payload))
		case envVar:
			env[s.dest] = string(payload)
			fmt.Fprintf(cfg.Log, "fetch-secrets: set %s\n", s.dest)
		}
	}

	if err := writeEnvFile(cfg.EnvOut, env); err != nil {
		return err
	}
	fmt.Fprintf(cfg.Log, "fetch-secrets: wrote %s (%d var(s))\n", cfg.EnvOut, len(env))
	return nil
}

// writeFileAtomic writes data to a temp file in the same directory, sets its
// mode, and renames it into place.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil { // WriteFile mode is pre-umask
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writeEnvFile writes a systemd EnvironmentFile with 0600 perms. Values are
// double-quoted with backslash/quote escaping; systemd does not expand $ inside
// EnvironmentFile values, so only quotes, backslashes, and newlines need care.
func writeEnvFile(path string, env map[string]string) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable output regardless of fetch order

	var b strings.Builder
	b.WriteString("# written by `sparkbox fetch-secrets` — do not edit; regenerated every boot\n")
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	for _, k := range keys {
		v := env[k]
		if strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("secret for %s contains a newline and cannot be an env var", k)
		}
		fmt.Fprintf(&b, "%s=\"%s\"\n", k, esc.Replace(v))
	}
	return writeFileAtomic(path, []byte(b.String()), 0o600)
}
