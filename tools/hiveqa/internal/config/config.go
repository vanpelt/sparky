// Package config provides the hiveqa daemon configuration.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// TLSMode determines how TLS certificates are provisioned.
type TLSMode string

const (
	// TLSTailscale uses Tailscale's built-in cert provisioning (ListenTLS).
	// Works with Tailscale SaaS only — not headscale.
	TLSTailscale TLSMode = "tailscale"

	// TLSACME uses an ACME client (certmagic) to provision certs for a
	// custom domain. Works with headscale or any control server.
	TLSACME TLSMode = "acme"

	// TLSNone disables TLS. Traffic is proxied over plain HTTP on the tailnet.
	TLSNone TLSMode = "none"
)

// Config holds all daemon configuration.
type Config struct {
	// StateDir is the root directory for daemon state.
	StateDir string

	// ControlURL is the Tailscale/headscale control server URL.
	// Empty string means Tailscale SaaS (the default).
	ControlURL string

	// AuthKey is the Tailscale/headscale pre-auth key.
	AuthKey string

	// TLS controls how HTTPS certificates are provisioned.
	TLS TLSMode

	// ACMEDomain is the parent domain for ACME certs (e.g., "hiveqa.dev").
	// Each stack gets a subdomain: {name}.{ACMEDomain}.
	// Only used when TLS == TLSACME.
	ACMEDomain string

	// ACMEEmail is the contact email for ACME registration.
	ACMEEmail string

	// ACMEDNS01Provider is the DNS-01 challenge provider name.
	// Supported: "cloudflare", "gcloud", "route53".
	// If empty, HTTP-01 challenges are used instead.
	ACMEDNS01Provider string
}

// Parse reads configuration from CLI flags and environment variables.
// Env vars take precedence for secrets (TS_AUTHKEY); flags take precedence
// for everything else, falling back to env vars.
func Parse() (*Config, error) {
	cfg := &Config{}

	flag.StringVar(&cfg.StateDir, "state-dir", envOr("HIVEQA_STATE_DIR", ""), "state directory (default: ~/.local/share/hiveqa)")
	flag.StringVar(&cfg.ControlURL, "control-url", envOr("HIVEQA_CONTROL_URL", ""), "Tailscale/headscale control server URL (empty = Tailscale SaaS)")
	flag.StringVar(&cfg.AuthKey, "auth-key", "", "Tailscale/headscale auth key (prefer TS_AUTHKEY env var)")
	tlsMode := flag.String("tls", envOr("HIVEQA_TLS", "tailscale"), "TLS mode: tailscale, acme, or none")
	flag.StringVar(&cfg.ACMEDomain, "acme-domain", envOr("HIVEQA_ACME_DOMAIN", ""), "parent domain for ACME certs (e.g., hiveqa.dev)")
	flag.StringVar(&cfg.ACMEEmail, "acme-email", envOr("HIVEQA_ACME_EMAIL", ""), "ACME registration email")
	flag.StringVar(&cfg.ACMEDNS01Provider, "acme-dns-provider", envOr("HIVEQA_ACME_DNS_PROVIDER", ""), "DNS-01 provider: cloudflare, gcloud, route53 (empty = HTTP-01)")
	flag.Parse()

	// Auth key: flag > env var.
	if cfg.AuthKey == "" {
		cfg.AuthKey = os.Getenv("TS_AUTHKEY")
	}

	// Default state dir.
	if cfg.StateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot determine home dir: %w", err)
		}
		cfg.StateDir = filepath.Join(home, ".local", "share", "hiveqa")
	}

	// Validate TLS mode.
	switch TLSMode(*tlsMode) {
	case TLSTailscale, TLSACME, TLSNone:
		cfg.TLS = TLSMode(*tlsMode)
	default:
		return nil, fmt.Errorf("invalid --tls mode %q: must be tailscale, acme, or none", *tlsMode)
	}

	// Validate required fields.
	if cfg.AuthKey == "" {
		return nil, fmt.Errorf("TS_AUTHKEY environment variable or --auth-key flag is required\nGenerate one at https://login.tailscale.com/admin/settings/keys")
	}

	if cfg.TLS == TLSACME {
		if cfg.ACMEDomain == "" {
			return nil, fmt.Errorf("--acme-domain is required when --tls=acme")
		}
		if cfg.ACMEEmail == "" {
			return nil, fmt.Errorf("--acme-email is required when --tls=acme")
		}
	}

	if cfg.TLS == TLSTailscale && cfg.ControlURL != "" {
		fmt.Fprintln(os.Stderr, "warning: --tls=tailscale with a custom --control-url (headscale) will likely fail")
		fmt.Fprintln(os.Stderr, "         headscale does not support Tailscale's cert provisioning")
		fmt.Fprintln(os.Stderr, "         consider using --tls=acme or --tls=none")
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
