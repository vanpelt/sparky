package grpcidentity

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// ErrUnavailable marks a transport failure for which a mixed-version node may
// retry through its negotiated SSH fallback.
var ErrUnavailable = errors.New("gateway identity gRPC transport is unavailable")

// Client is the node-side metadata.Identity backed by GatewayIdentity.
type Client struct {
	connection *grpc.ClientConn
	identity   nodev1.GatewayIdentityClient
}

func DialTLS(ctx context.Context, target string, tlsConfig *tls.Config, opts ...grpc.DialOption) (*Client, error) {
	if target == "" {
		return nil, errors.New("grpcidentity: target is required")
	}
	if tlsConfig == nil {
		return nil, errors.New("grpcidentity: TLS configuration is required")
	}
	if len(tlsConfig.Certificates) == 0 && tlsConfig.GetClientCertificate == nil {
		return nil, errors.New("grpcidentity: node client certificate is required")
	}
	if tlsConfig.MinVersion < tls.VersionTLS13 {
		return nil, errors.New("grpcidentity: client TLS must require TLS 1.3")
	}
	if tlsConfig.VerifyConnection == nil {
		return nil, errors.New("grpcidentity: client TLS must verify the gateway SPIFFE identity")
	}
	opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig.Clone())))
	connection, err := grpc.DialContext(ctx, target, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{
		connection: connection,
		identity:   nodev1.NewGatewayIdentityClient(connection),
	}, nil
}

func WrapClient(identity nodev1.GatewayIdentityClient) (*Client, error) {
	if identity == nil {
		return nil, errors.New("grpcidentity: GatewayIdentity client is required")
	}
	return &Client{identity: identity}, nil
}

func (c *Client) Close() error {
	if c == nil || c.connection == nil {
		return nil
	}
	return c.connection.Close()
}

func (c *Client) Issue(ctx context.Context, box *host.Sandbox, audience string) (metadata.Token, error) {
	if box == nil || box.Name == "" {
		return metadata.Token{}, errors.New("grpcidentity: sandbox is required")
	}
	response, err := c.identity.IssueToken(ctx, &nodev1.IssueTokenRequest{
		Sandbox: box.Name, Audience: audience,
	})
	if err != nil {
		return metadata.Token{}, clientError(ctx, err)
	}
	if response.GetToken() == "" || response.GetExpiresAt() == nil ||
		!response.GetExpiresAt().IsValid() {
		return metadata.Token{}, errors.New("grpcidentity: gateway returned an invalid identity token")
	}
	return metadata.Token{
		JWT: response.GetToken(), ExpiresAt: response.GetExpiresAt().AsTime(),
	}, nil
}

func (c *Client) Describe(ctx context.Context, box *host.Sandbox) (metadata.Doc, error) {
	if box == nil || box.Name == "" {
		return metadata.Doc{}, errors.New("grpcidentity: sandbox is required")
	}
	response, err := c.identity.DescribeIdentity(ctx, &nodev1.DescribeIdentityRequest{
		Sandbox: box.Name,
	})
	if err != nil {
		return metadata.Doc{}, clientError(ctx, err)
	}
	return metadata.Doc{
		Issuer: response.GetIssuer(), Subject: response.GetSubject(),
		Owner: response.GetOwner(), GitHub: response.GetGithub(),
		GitHubID: response.GetGithubId(),
		KeyFP:    response.GetKeyFingerprint(), Sandbox: response.GetSandbox(),
		SandboxID: response.GetSandboxId(), Image: response.GetImage(), Box: response.GetNode(),
	}, nil
}

// Manifest is the node's metadata.RepoAccess half that says what a guest should
// have checked out. Like Issue it sends the sandbox name and nothing else.
func (c *Client) Manifest(ctx context.Context, box *host.Sandbox) (metadata.Manifest, error) {
	if box == nil || box.Name == "" {
		return metadata.Manifest{}, errors.New("grpcidentity: sandbox is required")
	}
	response, err := c.identity.ListRepos(ctx, &nodev1.ListReposRequest{Sandbox: box.Name})
	if err != nil {
		return metadata.Manifest{}, clientRepoError(ctx, err)
	}
	// Never nil: the guest's clone script distinguishes "no attachments" from a
	// failure by an empty list, and a JSON null would read as neither.
	manifest := metadata.Manifest{Repos: make([]metadata.RepoEntry, 0, len(response.GetRepos()))}
	for _, repo := range response.GetRepos() {
		if repo.GetSlug() == "" {
			// A row with no slug is nothing a guest can clone, and passing it on
			// would put an empty path into a shell script.
			return metadata.Manifest{}, errors.New("grpcidentity: gateway returned a repo with no slug")
		}
		manifest.Repos = append(manifest.Repos, metadata.RepoEntry{
			Host: repo.GetHost(), Slug: repo.GetSlug(), Ref: repo.GetRef(),
			Path: repo.GetPath(), Access: repo.GetAccess(),
			Instance: repo.GetInstance(),
		})
	}
	return manifest, nil
}

// Credential mints a git credential for one of the sandbox's repositories.
//
// The response is validated before it is returned for the same reason Issue's
// is: an empty password reaching git is a prompt the guest cannot answer, on a
// non-interactive helper, and the failure would surface as a hung clone rather
// than as the error it is.
func (c *Client) Credential(ctx context.Context, box *host.Sandbox, slug string) (metadata.Credential, error) {
	if box == nil || box.Name == "" {
		return metadata.Credential{}, errors.New("grpcidentity: sandbox is required")
	}
	if slug == "" {
		return metadata.Credential{}, errors.New("grpcidentity: repository slug is required")
	}
	response, err := c.identity.IssueRepoCredential(ctx, &nodev1.IssueRepoCredentialRequest{
		Sandbox: box.Name, Slug: slug,
	})
	if err != nil {
		return metadata.Credential{}, clientRepoError(ctx, err)
	}
	if response.GetPassword() == "" || response.GetExpiresAt() == nil ||
		!response.GetExpiresAt().IsValid() {
		return metadata.Credential{}, errors.New("grpcidentity: gateway returned an invalid git credential")
	}
	return metadata.Credential{
		Username: response.GetUsername(), Password: response.GetPassword(),
		ExpiresAt: response.GetExpiresAt().AsTime(),
	}, nil
}

// clientRepoError is clientError plus the one class only the repo endpoints
// have a sentinel for.
//
// PermissionDenied is handled here rather than in clientError because the two
// callers want opposite things from it: a repo refusal is a 403 whose sentence
// names the fix — verify a GitHub link, install the App — while an identity
// refusal has no such sentinel and keeps the 500 an unclassified refusal has
// always produced there, which is the reading CodeNotYours was given on purpose.
//
// gRPC has one code for "authenticated, not permitted" and this fabric already
// spends it on the placement refusal, so a node asking about a sandbox it does
// not hold reads as 403 on a repo call where the SSH fallback would make it a
// 500. Terminal either way, nothing about another tenant is disclosed, and the
// distinction survives in full in the gateway's own log.
func clientRepoError(ctx context.Context, err error) error {
	if ctx.Err() == nil && status.Code(err) == codes.PermissionDenied {
		return fmt.Errorf("%w: %s", metadata.ErrRepoDenied, status.Convert(err).Message())
	}
	return clientError(ctx, err)
}

func clientError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	switch status.Code(err) {
	case codes.InvalidArgument:
		return fmt.Errorf("%w: %s", metadata.ErrAudience, status.Convert(err).Message())
	case codes.FailedPrecondition:
		return fmt.Errorf("%w: %s", metadata.ErrNoIssuer, status.Convert(err).Message())
	case codes.NotFound:
		// A repository this sandbox has no attachment to. 404, and deliberately
		// not fallback-eligible: the SSH uplink would resolve the same ledger
		// and reach the same answer.
		return fmt.Errorf("%w: %s", metadata.ErrNoSuchRepo, status.Convert(err).Message())
	case codes.Unimplemented:
		// Either this gateway attaches no repositories, or it predates the RPC.
		// Both are "not enabled here" to a guest, and neither is repaired by
		// retrying or by trying the other transport.
		return fmt.Errorf("%w: %s", metadata.ErrNotEnabled, status.Convert(err).Message())
	case codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("%w: %w: %v", metadata.ErrNoIssuer, ErrUnavailable, err)
	default:
		return err
	}
}
