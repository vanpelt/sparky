package bootsecrets

import (
	"context"
	"errors"
	"fmt"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/opsecrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/scwsecrets"
)

// ErrNotFound means the store has no such secret. Fetch turns it into a skip
// for an optional manifest entry and a hard failure for a required one, so a
// Source MUST return it only for a genuinely absent secret — never for a
// misconfigured store or a failed authentication, which would make an optional
// secret vanish silently.
var ErrNotFound = errors.New("secret not found")

// Source resolves a fleet secret by its manifest name. It is the seam between
// "what the fleet needs" (the manifest, which is provider-agnostic) and "where
// the fleet keeps it" — Scaleway Secret Manager on rented metal, 1Password on
// hardware you own.
type Source interface {
	// Get returns the raw stored payload. secretType is the store's own type
	// name for stores that have one ("ssh_key", "opaque"); stores with no such
	// notion return "". Absent secrets must be reported as ErrNotFound.
	Get(ctx context.Context, name string) (payload []byte, secretType string, err error)

	// Describe names the store for logs and error messages.
	Describe() string
}

// ScalewayConfig addresses a folder of secrets in Scaleway Secret Manager.
type ScalewayConfig struct {
	BaseURL   string // "" for the real API; set in tests
	Region    string
	ProjectID string
	Token     string // Scaleway secret key, from the environment
	Path      string // Secret Manager folder, e.g. "/sparkbox/fleet"
}

// NewScalewaySource builds a Source over Scaleway Secret Manager.
func NewScalewaySource(cfg ScalewayConfig) (Source, error) {
	if cfg.Token == "" {
		return nil, errors.New("SCW_SECRET_KEY is not set (the host's Secret Manager access key)")
	}
	if cfg.ProjectID == "" {
		return nil, errors.New("project ID is required (SCW_DEFAULT_PROJECT_ID)")
	}
	return &scwSource{
		client: scwsecrets.New(cfg.BaseURL, cfg.Region, cfg.ProjectID, cfg.Token),
		path:   cfg.Path,
	}, nil
}

type scwSource struct {
	client *scwsecrets.Client
	path   string
}

func (s *scwSource) Get(ctx context.Context, name string) ([]byte, string, error) {
	payload, secretType, err := s.client.AccessByPath(ctx, s.path, name)
	if errors.Is(err, scwsecrets.ErrNotFound) {
		return nil, "", ErrNotFound
	}
	return payload, secretType, err
}

func (s *scwSource) Describe() string {
	return fmt.Sprintf("Scaleway Secret Manager %s", s.path)
}

// OnePasswordConfig addresses one 1Password vault holding the fleet's secrets,
// one item per manifest name.
type OnePasswordConfig struct {
	Vault   string // vault name or UUID
	Account string // desktop-app account shorthand; empty uses op's default
	Field   string // item field holding the payload ("" means opsecrets.DefaultField)
	Bin     string // op executable ("" means "op")
	Token   string // service-account token, passed to op by environment
}

// NewOnePasswordSource builds a Source over a 1Password vault.
func NewOnePasswordSource(cfg OnePasswordConfig) (Source, error) {
	client, err := opsecrets.New(opsecrets.Config{
		Vault:   cfg.Vault,
		Account: cfg.Account,
		Field:   cfg.Field,
		Bin:     cfg.Bin,
		Token:   cfg.Token,
	})
	if err != nil {
		return nil, err
	}
	return &opSource{client: client}, nil
}

type opSource struct {
	client *opsecrets.Client
}

// Get reads the item named for the manifest entry. 1Password has no notion of
// secret types, so it returns "" and Fetch stores the payload verbatim — which
// is why a 1Password-held SSH key is a bare PEM, not Scaleway's JSON envelope.
func (s *opSource) Get(ctx context.Context, name string) ([]byte, string, error) {
	payload, err := s.client.Read(ctx, name)
	if errors.Is(err, opsecrets.ErrNotFound) {
		return nil, "", ErrNotFound
	}
	return payload, "", err
}

func (s *opSource) Describe() string { return s.client.Describe() }
