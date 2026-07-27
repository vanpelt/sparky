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
		KeyFP: response.GetKeyFingerprint(), Sandbox: response.GetSandbox(),
		Image: response.GetImage(), Box: response.GetNode(),
	}, nil
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
	case codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("%w: %w: %v", metadata.ErrNoIssuer, ErrUnavailable, err)
	default:
		return err
	}
}
