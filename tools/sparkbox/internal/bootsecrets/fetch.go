// Package bootsecrets pulls the fleet's secrets from a secret store into tmpfs,
// replacing the plaintext copies that used to ride in cloud-init user-data.
// Private keys land as PEM files under KeyDir; the two passwords are written as
// a systemd EnvironmentFile. Everything is meant for /run, so a captured disk or
// a pulled drive yields no key material.
//
// Which store the secrets come from is a Source (see source.go): Scaleway Secret
// Manager on rented metal, 1Password on hardware you own. The manifest below —
// what the fleet needs and where each secret lands — is the same either way.
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

// Config controls a fetch. Credentials belong to the Source, which takes them
// from the environment rather than argv so they can't leak into the process
// table.
type Config struct {
	Source Source // where secrets are read from (required)
	KeyDir string // where PEMs are written (put on tmpfs)
	EnvOut string // EnvironmentFile to write (put on tmpfs)
	Log    io.Writer
}

type kind int

const (
	sshKeyPEM kind = iota // type=ssh_key: JSON-wrapped PEM
	opaquePEM             // opaque: payload is already a PEM
	envVar                // opaque: payload is a value for an env var
)

// manifest is the fixed set of fleet secrets and how each maps onto the host.
// required decides whether a missing secret fails the boot. The original three
// keys remain load-bearing. Node-control PKI material is optional at fetch time
// so an SSH-only fleet can roll out this binary before its new secrets exist;
// `serve --require-keys --node-control-transport=auto|grpc` is the point that
// fails closed until the operator uploads them. The GitHub App key is optional
// for the same reason and one stronger: most fleets will never create an App,
// and a fleet that has not is not broken — repo attachment simply answers "not
// enabled on this host". Making it required would turn a feature nobody opted
// into a boot failure on every existing host.
var manifest = []struct {
	name     string
	kind     kind
	dest     string // filename under KeyDir, or env var name
	required bool
}{
	{"gateway-host-key", sshKeyPEM, "gateway_host_key.pem", true},
	{"gateway-upstream-key", sshKeyPEM, "gateway_upstream_key.pem", true},
	{"oidc-signing-key", opaquePEM, "oidc_signing_key.pem", true},
	{"node-control-ca-cert", opaquePEM, "node_ca_cert.pem", false},
	{"node-control-ca-key", opaquePEM, "node_ca_key.pem", false},
	{"gateway-control-key", opaquePEM, "gateway_control_key.pem", false},
	// opaquePEM, not envVar: GitHub hands out an RSA private key, and
	// writeEnvFile refuses any value containing a newline — a PEM is nothing
	// but newlines. It also belongs on disk beside the other keys, because
	// ghapp.LoadKeyIfPresent reads it from KeyDir by name.
	{"github-app-key", opaquePEM, "github_app_key.pem", false},
	// The client secret is needed only by the browser OAuth flow. Device flow
	// inside a VM and bot installation credentials continue without it.
	{"github-app-client-secret", envVar, "SPARKBOX_GITHUB_APP_CLIENT_SECRET", false},
	// envVar, not opaquePEM: the App's webhook secret is a random string an
	// operator pastes into two places, with no structure and no newlines, so it
	// travels the same way the console password does. Optional for the same
	// reason as the key above — and independently of it, because the two are
	// used by different halves of the App and a fleet may well have one and not
	// the other. Absent, cmd/sparkbox mounts no webhook receiver at all rather
	// than mounting one that verifies nothing.
	{"github-webhook-secret", envVar, "SPARKBOX_GITHUB_WEBHOOK_SECRET", false},
	{"cloudflare-api-token", envVar, "CLOUDFLARE_API_TOKEN", false},
	{"console-password", envVar, "SPARKBOX_CONSOLE_PASSWORD", false},
}

// KeyFiles lists the fleet key PEM basenames materialized under KeyDir — the
// same files `sparkbox serve` loads and `sparkbox doctor` checks for. Owned
// here so adding or renaming a fleet key is a one-place change.
func KeyFiles() []string {
	return keyFiles(true)
}

// OptionalKeyFiles lists the fleet key PEMs a host may have and works without:
// node-control PKI on an SSH-only fleet, and the GitHub App key on a fleet with
// no App. They are deliberately absent from KeyFiles, because a doctor check
// that failed on them would fail on every correctly configured standalone host.
//
// They still deserve a line in the report. An optional key is invisible
// otherwise, and "the App is not installed" and "this host never got the key"
// produce the same symptom inside a VM — a clone that cannot authenticate —
// with only one of them fixable from the host.
func OptionalKeyFiles() []string {
	return keyFiles(false)
}

func keyFiles(required bool) []string {
	var out []string
	for _, s := range manifest {
		if s.kind != envVar && s.required == required {
			out = append(out, s.dest)
		}
	}
	return out
}

// Fetch reads every manifest secret and materializes it. It is safe to re-run:
// each write is atomic, so a reader never sees a half-written key.
func Fetch(ctx context.Context, cfg Config) error {
	if cfg.Source == nil {
		return errors.New("no secret source configured")
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

	fmt.Fprintf(cfg.Log, "fetch-secrets: reading from %s\n", cfg.Source.Describe())
	env := map[string]string{}

	for _, s := range manifest {
		payload, secretType, err := cfg.Source.Get(ctx, s.name)
		if errors.Is(err, ErrNotFound) {
			if s.required {
				return fmt.Errorf("required secret %s is missing from %s", s.name, cfg.Source.Describe())
			}
			fmt.Fprintf(cfg.Log, "fetch-secrets: %s not set, skipping\n", s.name)
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", s.name, err)
		}

		switch s.kind {
		case sshKeyPEM:
			pem, err := unwrapSSHKey(s.name, payload, secretType)
			if err != nil {
				return err
			}
			if err := writeFileAtomic(filepath.Join(cfg.KeyDir, s.dest), normalizePEM(pem), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cfg.Log, "fetch-secrets: wrote %s (%d bytes)\n", s.dest, len(pem))
		case opaquePEM:
			if err := writeFileAtomic(filepath.Join(cfg.KeyDir, s.dest), normalizePEM(payload), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cfg.Log, "fetch-secrets: wrote %s (%d bytes)\n", s.dest, len(payload))
		case envVar:
			// Trim only a trailing line ending, not all whitespace: a value
			// pasted into a secret store often picks one up, and writeEnvFile
			// refuses newlines outright. Interior or leading spaces stay,
			// because they may be part of the password.
			env[s.dest] = strings.TrimRight(string(payload), "\r\n")
			fmt.Fprintf(cfg.Log, "fetch-secrets: set %s\n", s.dest)
		}
	}

	if err := writeEnvFile(cfg.EnvOut, env); err != nil {
		return err
	}
	fmt.Fprintf(cfg.Log, "fetch-secrets: wrote %s (%d var(s))\n", cfg.EnvOut, len(env))
	return nil
}

// unwrapSSHKey turns a stored SSH key into a bare PEM.
//
// Scaleway types its secrets and validates an ssh_key's JSON envelope on write,
// so a mistyped secret there is a real problem and stays a hard error: writing a
// JSON blob where a PEM belongs would fail much later and much less clearly.
// Stores with no notion of types report "" and hold the PEM verbatim, which is
// how the same key looks in 1Password.
func unwrapSSHKey(name string, payload []byte, secretType string) ([]byte, error) {
	switch secretType {
	case "":
		return payload, nil
	case "ssh_key":
		pem, err := scwsecrets.UnwrapSSHKey(payload)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return pem, nil
	default:
		return nil, fmt.Errorf("%s: expected type ssh_key, got %q", name, secretType)
	}
}

// normalizePEM guarantees the single trailing newline a PEM file is supposed to
// end with. Secret stores are byte-exact, so this only matters when a human put
// the value there — a paste that dropped the final newline produces a file some
// parsers accept and others reject, which is a miserable thing to debug at boot.
func normalizePEM(payload []byte) []byte {
	if len(payload) == 0 || payload[len(payload)-1] == '\n' {
		return payload
	}
	return append(payload, '\n')
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
