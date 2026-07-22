package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"go.uber.org/zap"
	"golang.org/x/crypto/acme/autocert"
)

// publicResolvers are the recursive resolvers the ACME DNS-01 solver consults
// for its own bookkeeping. Two operators, so one being unreachable is not an
// outage; both answer the public view of a zone, which is the only view that
// matters when proving control of a name to a certificate authority.
var publicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

type tlsParams struct {
	provider string       // "cloudflare" | "autocert"
	domain   string       // base domain, e.g. hivemind.tools
	email    string       // ACME account email (optional but recommended)
	stateDir string       // certs are cached under <stateDir>/{certmagic,autocert}
	log      *slog.Logger // partial-issuance warnings; nil takes slog.Default()
}

// setupProxyTLS configures srv.TLSConfig for the chosen provider and reports
// the names it manages. For "cloudflare" it obtains (and thereafter
// auto-renews) wildcard certificates via the ACME DNS-01 challenge — this call
// blocks until they are in hand. For "autocert" it wires ACME on-demand
// per-host certs and starts the :80 challenge/redirect listener, so there is no
// fixed name list to report and it returns nil.
func setupProxyTLS(ctx context.Context, srv *http.Server, p tlsParams) ([]string, error) {
	if p.log == nil {
		p.log = slog.Default()
	}
	switch p.provider {
	case "cloudflare":
		cfg, managed, err := wildcardTLSConfig(ctx, p)
		if err != nil {
			return nil, err
		}
		srv.TLSConfig = cfg
		return managed, nil
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
		// No name list: HostPolicy already accepts any depth under the domain,
		// so <name>-xterm.<domain> is issued on first SNI with no extra wiring.
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown --tls-provider %q (want cloudflare | autocert)", p.provider)
	}
}

// wildcardTLSConfig obtains and maintains certificates covering <domain> and
// *.<domain> from Let's Encrypt using the DNS-01 challenge through Cloudflare.
// Wildcards mean every sandbox subdomain is already covered without per-name
// issuance, so no brush with Let's Encrypt's per-name rate limits no matter how
// many ephemeral sandboxes come and go. Needs a Cloudflare API token (scoped
// Zone.DNS:Edit) in CLOUDFLARE_API_TOKEN.
//
// ONE wildcard is deliberate and load-bearing. A wildcard matches exactly one
// label (RFC 6125), so this pair covers browser terminals only because they are
// served at <name>-xterm.<domain> rather than <name>.xterm.<domain>. The dotted
// form needed a second wildcard here, which worked — and then failed anyway
// wherever a hosted TLS front end terminates in front of us with a certificate
// it issued for the zone: Cloudflare's universal certificate is
// `<domain>, *.<domain>` and the deeper name is simply not in it, so the
// handshake dies at the edge with no request and no log line to explain it. If
// a future name needs its own subtree, give it a hyphen, not a dot.
func wildcardTLSConfig(ctx context.Context, p tlsParams) (*tls.Config, []string, error) {
	token := os.Getenv("CLOUDFLARE_API_TOKEN")
	if token == "" {
		return nil, nil, errors.New("cloudflare TLS needs a scoped Zone.DNS:Edit token in CLOUDFLARE_API_TOKEN")
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
			// Resolve challenge bookkeeping against public DNS, never the host's
			// own view of the world. Before an ACME challenge is presented, the
			// solver walks up the name looking for the zone's SOA — and on a
			// tailnet-edge host, this very binary is what answers the zone: the
			// split-DNS entry points <domain> at our dnsedge responder, which
			// serves A/AAAA and an empty NOERROR for everything else. An SOA
			// query therefore comes back empty, the walk climbs past the zone to
			// the TLD, and issuance dies on "could not determine zone" — for a
			// name that resolves perfectly well everywhere else on the internet.
			//
			// This is not only about new names. The same walk runs on every
			// RENEWAL, so a host that shadows its own zone eventually cannot
			// replace the wildcard the whole edge depends on, and only finds out
			// when the certificate expires.
			Resolvers: publicResolvers,
		},
	}

	cfg := certmagic.NewDefault()

	// The pair is load-bearing: without it the edge has no certificate at all
	// and there is nothing to serve, so failure is fatal. ManageSync blocks
	// until the cert is in hand and its error kills `serve`.
	base := []string{p.domain, "*." + p.domain}
	if err := cfg.ManageSync(ctx, base); err != nil {
		return nil, nil, fmt.Errorf("obtain wildcard cert for %v: %w", base, err)
	}
	return cfg.TLSConfig(), base, nil
}
