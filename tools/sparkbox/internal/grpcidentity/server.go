// Package grpcidentity carries a node's two workload-identity requests to the
// gateway over the fleet's authenticated gRPC control fabric.
package grpcidentity

import (
	"context"
	"crypto/tls"
	"errors"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Gateway is the authorization and signing surface supplied by fleet.Fleet.
// The node argument must come from the authenticated transport, never the RPC
// body.
type Gateway interface {
	IdentityToken(context.Context, string, nodelink.IdentityReq) (nodelink.IdentityTokenResp, error)
	IdentityDoc(context.Context, string, nodelink.IdentityReq) (nodelink.IdentityDocResp, error)
}

// Server exposes a gateway's identity signer to authenticated fleet nodes.
type Server struct {
	nodev1.UnimplementedGatewayIdentityServer
	gateway Gateway
	revoked nodecert.Revoked
}

func NewServer(gateway Gateway, revoked ...nodecert.Revoked) (*Server, error) {
	if gateway == nil {
		return nil, errors.New("grpcidentity: gateway is required")
	}
	var check nodecert.Revoked
	if len(revoked) > 0 {
		check = revoked[0]
	}
	return &Server{gateway: gateway, revoked: check}, nil
}

// NewRPCServer registers GatewayIdentity on a server which requires an mTLS
// node certificate. tlsConfig should normally come from
// nodecert.ServerTLSConfigForRole.
func NewRPCServer(service *Server, tlsConfig *tls.Config, opts ...grpc.ServerOption) (*grpc.Server, error) {
	if service == nil {
		return nil, errors.New("grpcidentity: service is required")
	}
	if tlsConfig == nil {
		return nil, errors.New("grpcidentity: TLS configuration is required")
	}
	if len(tlsConfig.Certificates) == 0 && tlsConfig.GetCertificate == nil {
		return nil, errors.New("grpcidentity: gateway server certificate is required")
	}
	if tlsConfig.MinVersion < tls.VersionTLS13 {
		return nil, errors.New("grpcidentity: server TLS must require TLS 1.3")
	}
	if tlsConfig.VerifyConnection == nil {
		return nil, errors.New("grpcidentity: server TLS must verify the node SPIFFE identity")
	}
	switch tlsConfig.ClientAuth {
	case tls.RequireAnyClientCert, tls.RequireAndVerifyClientCert:
	default:
		return nil, errors.New("grpcidentity: server TLS must require a client certificate")
	}
	opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfig.Clone())))
	server := grpc.NewServer(opts...)
	nodev1.RegisterGatewayIdentityServer(server, service)
	return server, nil
}

func (s *Server) IssueToken(ctx context.Context, request *nodev1.IssueTokenRequest) (*nodev1.IssueTokenResponse, error) {
	if request.GetSandbox() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox is required")
	}
	node, err := s.authenticatedNode(ctx)
	if err != nil {
		return nil, err
	}
	response, err := s.gateway.IdentityToken(ctx, node, nodelink.IdentityReq{
		Sandbox: request.GetSandbox(),
		Aud:     request.GetAudience(),
	})
	if err != nil {
		return nil, gatewayError(err)
	}
	expires := timestamppb.New(response.ExpiresAt)
	if response.Token == "" || !expires.IsValid() {
		return nil, status.Error(codes.Internal, "gateway produced an invalid identity token")
	}
	return &nodev1.IssueTokenResponse{Token: response.Token, ExpiresAt: expires}, nil
}

func (s *Server) DescribeIdentity(ctx context.Context, request *nodev1.DescribeIdentityRequest) (*nodev1.IdentityDescription, error) {
	if request.GetSandbox() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox is required")
	}
	node, err := s.authenticatedNode(ctx)
	if err != nil {
		return nil, err
	}
	response, err := s.gateway.IdentityDoc(ctx, node, nodelink.IdentityReq{
		Sandbox: request.GetSandbox(),
	})
	if err != nil {
		return nil, gatewayError(err)
	}
	return &nodev1.IdentityDescription{
		Issuer:         response.Issuer,
		Subject:        response.Subject,
		Owner:          response.Owner,
		Github:         response.GitHub,
		KeyFingerprint: response.KeyFP,
		Sandbox:        response.Sandbox,
		SandboxId:      response.SandboxID,
		Image:          response.Image,
		Node:           response.Box,
	}, nil
}

func (s *Server) authenticatedNode(ctx context.Context) (string, error) {
	transportPeer, ok := peer.FromContext(ctx)
	if !ok || transportPeer.AuthInfo == nil {
		return "", status.Error(codes.Unauthenticated, "node mTLS identity is required")
	}
	tlsInfo, ok := transportPeer.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return "", status.Error(codes.Unauthenticated, "node mTLS identity is required")
	}
	identity, err := nodecert.PeerFromCertificate(tlsInfo.State.PeerCertificates[0])
	if err != nil || identity.Role != nodecert.RoleNode {
		return "", status.Error(codes.Unauthenticated, "node mTLS identity is invalid")
	}
	if s.revoked != nil &&
		s.revoked(nodecert.Serial(tlsInfo.State.PeerCertificates[0].SerialNumber)) {
		return "", status.Error(codes.PermissionDenied, "node mTLS identity is no longer approved")
	}
	return identity.Name, nil
}

func gatewayError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	var typed *ctlops.Error
	if errors.As(err, &typed) {
		switch typed.Code {
		case nodelink.CodeIdentityAudience:
			return status.Error(codes.InvalidArgument, typed.Msg)
		case nodelink.CodeNoIssuer:
			return status.Error(codes.FailedPrecondition, typed.Msg)
		case nodelink.CodeNotYours:
			return status.Error(codes.PermissionDenied, typed.Msg)
		}
		if typed.Msg != "" {
			return status.Error(codes.Internal, typed.Msg)
		}
	}
	return status.Error(codes.Internal, "gateway could not issue workload identity")
}
