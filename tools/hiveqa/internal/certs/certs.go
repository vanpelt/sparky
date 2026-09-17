// Package certs provides ACME certificate management for hiveqa.
// It wraps certmagic to provision TLS certificates for stack subdomains
// when running with headscale (which lacks Tailscale's built-in cert provisioning).
package certs

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"sync"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/cloudflare"
	"github.com/libdns/googleclouddns"
	"github.com/libdns/route53"
)

// DNSProvider identifies a supported DNS-01 challenge provider.
type DNSProvider string

const (
	DNSCloudflare DNSProvider = "cloudflare"
	DNSGCloud     DNSProvider = "gcloud"
	DNSRoute53    DNSProvider = "route53"
)

// Manager provisions and caches ACME certificates for hiveqa stacks.
type Manager struct {
	// ParentDomain is the base domain (e.g., "hiveqa.dev").
	// Stacks get certs for {name}.{ParentDomain}.
	ParentDomain string

	// Email is the ACME registration email.
	Email string

	// DNSProviderName is the DNS-01 provider to use.
	// Empty means HTTP-01 challenges (requires port 80 from internet).
	DNSProviderName DNSProvider

	// CertStorage is an optional path for cert storage.
	// Defaults to certmagic's default ($HOME/.local/share/certmagic).
	CertStorage string

	magic *certmagic.Config
	mu    sync.Mutex
}

// Init initializes the ACME certificate manager.
func (m *Manager) Init(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.CertStorage != "" {
		certmagic.Default.Storage = &certmagic.FileStorage{Path: m.CertStorage}
	}

	cfg := certmagic.NewDefault()

	// Set up the ACME issuer.
	issuerCfg := certmagic.ACMEIssuer{
		Email:  m.Email,
		Agreed: true,
	}

	if m.DNSProviderName != "" {
		solver, err := m.buildDNS01Solver()
		if err != nil {
			return fmt.Errorf("configuring DNS-01 provider %q: %w", m.DNSProviderName, err)
		}
		issuerCfg.DNS01Solver = solver
		log.Printf("[certs] using DNS-01 challenges via %s", m.DNSProviderName)
	} else {
		log.Printf("[certs] using HTTP-01 challenges (port 80 must be reachable)")
	}

	issuer := certmagic.NewACMEIssuer(cfg, issuerCfg)
	cfg.Issuers = []certmagic.Issuer{issuer}

	// On-demand TLS: provision certs as stacks come up.
	cfg.OnDemand = &certmagic.OnDemandConfig{
		DecisionFunc: func(ctx context.Context, name string) error {
			if !isSubdomain(name, m.ParentDomain) {
				return fmt.Errorf("domain %q is not a subdomain of %q", name, m.ParentDomain)
			}
			return nil
		},
	}

	m.magic = cfg
	log.Printf("[certs] ACME manager initialized for *.%s", m.ParentDomain)
	return nil
}

// TLSConfig returns a *tls.Config that provisions certs on demand
// for any subdomain of ParentDomain. Pass this to proxy instances.
func (m *Manager) TLSConfig() *tls.Config {
	return m.magic.TLSConfig()
}

// DomainFor returns the FQDN for a stack name.
func (m *Manager) DomainFor(stackName string) string {
	return stackName + "." + m.ParentDomain
}

// buildDNS01Solver creates the appropriate libdns provider and wraps it
// in a certmagic DNS01Solver.
func (m *Manager) buildDNS01Solver() (*certmagic.DNS01Solver, error) {
	switch m.DNSProviderName {
	case DNSCloudflare:
		// Reads from CLOUDFLARE_API_TOKEN env var.
		token := os.Getenv("CLOUDFLARE_API_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("CLOUDFLARE_API_TOKEN environment variable is required for cloudflare DNS-01")
		}
		provider := &cloudflare.Provider{
			APIToken: token,
		}
		return &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{
				DNSProvider: provider,
			},
		}, nil

	case DNSGCloud:
		// Reads from GCP_PROJECT env var. Uses Application Default Credentials.
		project := os.Getenv("GCP_PROJECT")
		if project == "" {
			return nil, fmt.Errorf("GCP_PROJECT environment variable is required for gcloud DNS-01")
		}
		provider := &googleclouddns.Provider{
			Project: project,
		}
		return &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{
				DNSProvider: provider,
			},
		}, nil

	case DNSRoute53:
		// Uses AWS credentials from environment (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION)
		// or IAM role when running on EC2/ECS/EKS.
		provider := &route53.Provider{}
		return &certmagic.DNS01Solver{
			DNSManager: certmagic.DNSManager{
				DNSProvider: provider,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unsupported DNS-01 provider %q (supported: cloudflare, gcloud, route53)", m.DNSProviderName)
	}
}

// isSubdomain checks if name is a subdomain of parent.
func isSubdomain(name, parent string) bool {
	if len(name) <= len(parent)+1 {
		return false
	}
	suffix := "." + parent
	return name[len(name)-len(suffix):] == suffix
}
