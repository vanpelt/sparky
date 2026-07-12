package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"go.uber.org/zap"
	"golang.org/x/crypto/acme/autocert"
)

type tlsParams struct {
	provider string // "cloudflare" | "autocert"
	domain   string // base domain, e.g. hivemind.tools
	email    string // ACME account email (optional but recommended)
	stateDir string // certs are cached under <stateDir>/{certmagic,autocert}
}

// setupProxyTLS configures srv.TLSConfig for the chosen provider. For
// "cloudflare" it obtains (and thereafter auto-renews) a single wildcard
// certificate via the ACME DNS-01 challenge — this call blocks until the cert
// is in hand. For "autocert" it wires ACME on-demand per-host certs and starts
// the :80 challenge/redirect listener.
func setupProxyTLS(ctx context.Context, srv *http.Server, p tlsParams) error {
	switch p.provider {
	case "cloudflare":
		cfg, err := wildcardTLSConfig(ctx, p)
		if err != nil {
			return err
		}
		srv.TLSConfig = cfg
		return nil
	case "autocert":
		am := &autocert.Manager{
			Prompt: autocert.AcceptTOS,
			Email:  p.email,
			Cache:  autocert.DirCache(filepath.Join(p.stateDir, "autocert")),
			HostPolicy: func(_ context.Context, h string) error {
				if h == p.domain || strings.HasSuffix(h, "."+p.domain) {
					return nil
				}
				return fmt.Errorf("host %q not under %s", h, p.domain)
			},
		}
		srv.TLSConfig = am.TLSConfig()
		// Port 80 answers ACME HTTP-01 challenges and redirects the rest.
		go http.ListenAndServe(":80", am.HTTPHandler(nil)) //nolint:errcheck
		return nil
	default:
		return fmt.Errorf("unknown --tls-provider %q (want cloudflare | autocert)", p.provider)
	}
}

// wildcardTLSConfig obtains and maintains one certificate covering <domain> and
// *.<domain> from Let's Encrypt using the DNS-01 challenge through Cloudflare.
// A single wildcard cert means every sandbox subdomain is already covered — no
// per-name issuance, so no brush with Let's Encrypt's per-name rate limits no
// matter how many ephemeral sandboxes come and go. Needs a Cloudflare API token
// (scoped Zone.DNS:Edit) in CLOUDFLARE_API_TOKEN.
func wildcardTLSConfig(ctx context.Context, p tlsParams) (*tls.Config, error) {
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	if token == "" {
		return nil, errors.New("cloudflare TLS needs a scoped Zone.DNS:Edit token in CLOUDFLARE_API_TOKEN")
	}

	certmagic.Default.Logger = zap.NewNop()
	certmagic.Default.Storage = &certmagic.FileStorage{Path: filepath.Join(p.stateDir, "certmagic")}

	certmagic.DefaultACME.Agreed = true
	certmagic.DefaultACME.Email = p.email
	// Wildcards are only issuable via DNS-01; force it so the apex uses it too.
	certmagic.DefaultACME.DisableHTTPChallenge = true
	certmagic.DefaultACME.DisableTLSALPNChallenge = true
	certmagic.DefaultACME.DNS01Solver = &certmagic.DNS01Solver{
		DNSManager: certmagic.DNSManager{
			DNSProvider: &cloudflare.Provider{APIToken: token},
		},
	}

	cfg := certmagic.NewDefault()
	domains := []string{p.domain, "*." + p.domain}
	if err := cfg.ManageSync(ctx, domains); err != nil {
		return nil, fmt.Errorf("obtain wildcard cert for %v: %w", domains, err)
	}
	return cfg.TLSConfig(), nil
}
