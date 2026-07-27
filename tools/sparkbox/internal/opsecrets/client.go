// Package opsecrets is a minimal 1Password client, scoped to the one thing a
// sparkbox operator or host does with it: read a fleet secret by name out of a
// single vault. Like scwsecrets it deliberately wraps one operation rather than
// pulling in a whole SDK — here by shelling out to the `op` CLI, which is the
// only way to get both authentication modes from one code path:
//
//   - on a laptop, `op` talks to the desktop app (Touch ID, no token anywhere);
//   - on a host, OP_SERVICE_ACCOUNT_TOKEN authenticates with no app at all.
//
// The official Go SDK would embed this instead of shelling out, but as of
// v0.4.x it does not compile with CGO_ENABLED=0 (client_builder_no_cgo.go
// references an undefined identifier in a function body, which Go type-checks
// whether or not you call it), and the version that does build costs ~13 MB of
// embedded WASM core. sparkbox is a pure-Go cross-compiled binary, so we shell
// out and keep bootsecrets.Source as the seam to switch later.
package opsecrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultField is the item field holding a secret's payload. Every fleet secret
// is stored the same way — a Password item whose `password` field is the whole
// payload — so one reference shape covers PEMs and tokens alike.
const DefaultField = "password"

// DefaultTimeout bounds a single read. The desktop-app path can block on a
// biometric prompt, so this is generous compared to an HTTP call.
const DefaultTimeout = 60 * time.Second

// ErrNotFound is returned when the item exists no more than the field does, so
// callers can treat an optional secret as absent rather than fatal.
var ErrNotFound = errors.New("secret not found")

// ErrNotSignedIn is returned when `op` has no usable session. It is separated
// from a generic failure because it is the one error an operator can act on
// directly, and because it must never be mistaken for "the secret isn't there".
var ErrNotSignedIn = errors.New("1Password CLI is not signed in")

// Config describes which vault to read and how to authenticate.
type Config struct {
	Vault   string        // vault name or UUID (required)
	Account string        // account shorthand/URL for desktop-app auth; empty uses op's default
	Field   string        // item field to read ("" means DefaultField)
	Bin     string        // op executable ("" means "op"; tests point this at a stub)
	Token   string        // service-account token, passed by env and never in argv
	Timeout time.Duration // per-read bound ("" means DefaultTimeout)
}

// Client reads secrets from one 1Password vault. The zero value is not usable;
// use New.
type Client struct {
	cfg Config
}

// New validates the configuration and returns a client.
func New(cfg Config) (*Client, error) {
	if cfg.Vault == "" {
		return nil, errors.New("opsecrets: a vault is required")
	}
	if strings.ContainsAny(cfg.Vault, "/") {
		return nil, fmt.Errorf("opsecrets: vault %q cannot contain a slash", cfg.Vault)
	}
	if cfg.Field == "" {
		cfg.Field = DefaultField
	}
	if strings.ContainsAny(cfg.Field, "/") {
		return nil, fmt.Errorf("opsecrets: field %q cannot contain a slash", cfg.Field)
	}
	if cfg.Bin == "" {
		cfg.Bin = "op"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	// Both auth modes at once is ambiguous rather than additive: `op` silently
	// prefers the token, so an operator who passed --op-account would be reading
	// from whatever account minted the token instead. Refuse instead of guessing.
	if cfg.Account != "" && cfg.Token != "" {
		return nil, errors.New("opsecrets: set either an account (desktop app) or a service-account token, not both")
	}
	return &Client{cfg: cfg}, nil
}

// Describe names the store for logs and errors.
func (c *Client) Describe() string {
	if c.cfg.Account != "" {
		return fmt.Sprintf("1Password vault %q (account %s)", c.cfg.Vault, c.cfg.Account)
	}
	return fmt.Sprintf("1Password vault %q", c.cfg.Vault)
}

// Read returns the exact bytes stored in item's field. No trailing newline is
// added or removed: `op read --no-newline` yields the stored value verbatim, so
// a PEM round-trips byte for byte.
func (c *Client) Read(ctx context.Context, item string) ([]byte, error) {
	if item == "" {
		return nil, errors.New("opsecrets: an item name is required")
	}
	if strings.ContainsAny(item, "/") {
		return nil, fmt.Errorf("opsecrets: item %q cannot contain a slash", item)
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	ref := fmt.Sprintf("op://%s/%s/%s", c.cfg.Vault, item, c.cfg.Field)
	args := []string{"read", "--no-newline", ref}
	if c.cfg.Account != "" {
		args = append(args, "--account", c.cfg.Account)
	}

	cmd := exec.CommandContext(ctx, c.cfg.Bin, args...)
	cmd.Env = c.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("op read %s: %w (is the 1Password desktop app waiting for approval?)", ref, ctx.Err())
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			// Couldn't run op at all — not installed, not executable, wrong path.
			return nil, fmt.Errorf("running %s: %w", c.cfg.Bin, err)
		}
		return nil, classify(ref, stderr.String())
	}
	return stdout.Bytes(), nil
}

// env builds the child environment. The service-account token goes here rather
// than in argv so it never appears in the process table, and it is stripped in
// account mode so an inherited token can't silently redirect the read to a
// different account than the one the operator named.
func (c *Client) env() []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, "OP_SERVICE_ACCOUNT_TOKEN=") {
			continue
		}
		out = append(out, kv)
	}
	if c.cfg.Token != "" {
		out = append(out, "OP_SERVICE_ACCOUNT_TOKEN="+c.cfg.Token)
	}
	return out
}

// notFoundMarkers are the two `op read` failures that mean "this secret is not
// stored", taken from the CLI's own wording:
//
//	"zz-missing" isn't an item in the "Sparkbox" vault.
//	item 'Sparkbox/zz-exists' does not have a field 'no-such-field'
//
// The list is deliberately exhaustive rather than a catch-all. A missing VAULT
// reports `"NoSuchVault" isn't a vault in this account`, which is a typo in the
// configuration, not an absent secret — treating it as ErrNotFound would make
// every optional secret silently vanish and every required one fail with the
// wrong explanation. Same for an expired session. So anything unrecognized is a
// hard error: if 1Password rewords these strings, optional secrets start failing
// loudly instead of being skipped in silence.
var notFoundMarkers = []string{
	"isn't an item in",
	"does not have a field",
}

// signedOutMarkers are the wordings that mean "authenticate first".
var signedOutMarkers = []string{
	"not currently signed in",
	"no account found",
	"session expired",
	"you are not currently signed in",
	"cannot setup session",
	"authorization timeout",
}

func classify(ref, stderr string) error {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = "no error output"
	}
	lower := strings.ToLower(msg)
	for _, m := range notFoundMarkers {
		if strings.Contains(lower, m) {
			return fmt.Errorf("%s: %w", ref, ErrNotFound)
		}
	}
	for _, m := range signedOutMarkers {
		if strings.Contains(lower, m) {
			return fmt.Errorf("%s: %w: %s", ref, ErrNotSignedIn, firstLine(msg))
		}
	}
	return fmt.Errorf("op read %s: %s", ref, firstLine(msg))
}

// firstLine keeps error output to one line. `op` prefixes its messages with a
// timestamp and can append usage text; the first line carries the reason.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
