package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/grpccontrol"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/grpcidentity"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodeenroll"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodepki"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
	"google.golang.org/grpc"
)

const gatewayCertificateRefresh = 30 * time.Minute

// gatewayCertificateSource keeps the gateway's short-lived client leaf fresh
// without rebuilding every node connection. gRPC's TLS transport calls
// GetClientCertificate again on each reconnect.
type gatewayCertificateSource struct {
	mu   sync.RWMutex
	leaf tls.Certificate
}

func newGatewayCertificateSource(leaf tls.Certificate) *gatewayCertificateSource {
	return &gatewayCertificateSource{leaf: leaf}
}

func (s *gatewayCertificateSource) current() tls.Certificate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.leaf
}

func (s *gatewayCertificateSource) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	leaf := s.leaf
	return &leaf, nil
}

func (s *gatewayCertificateSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	leaf := s.leaf
	return &leaf, nil
}

func (s *gatewayCertificateSource) run(
	ctx context.Context,
	authority *nodepki.Authority,
	stateDir, keyDir, clusterID string,
	requireKeys bool,
	log *slog.Logger,
) {
	ticker := time.NewTicker(gatewayCertificateRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		leaf, err := authority.GatewayCertificateFrom(
			stateDir, keyDir, clusterID, nodecert.DefaultTTL, requireKeys,
		)
		if err != nil {
			log.Error("renew gateway control certificate", "err", err)
			continue
		}
		s.mu.Lock()
		s.leaf = leaf
		s.mu.Unlock()
	}
}

// gatewayNodeControl owns gateway-side authenticated gRPC bindings. SSH still
// bootstraps enrollment and remains the independently supervised guest-data
// path; a failed gRPC health check therefore changes only the configured
// control selector.
type gatewayNodeControl struct {
	ctx          context.Context
	fleet        *fleet.Fleet
	roster       *nodes.Store
	issuer       *nodeenroll.Issuer
	authority    *nodepki.Authority
	leaf         *gatewayCertificateSource
	mode         fleet.ControlTransport
	guestMode    fleet.GuestTransport
	rollout      fleet.ControlRollout
	shadow       bool
	routedCanary int
	identityAddr string
	metrics      *fleetmetrics.Registry
	log          *slog.Logger
}

func newGatewayNodeControl(
	ctx context.Context,
	flt *fleet.Fleet,
	roster *nodes.Store,
	issuer *nodeenroll.Issuer,
	authority *nodepki.Authority,
	leaf *gatewayCertificateSource,
	mode fleet.ControlTransport,
	guestMode fleet.GuestTransport,
	rollout fleet.ControlRollout,
	shadow bool,
	routedCanary int,
	identityAddr string,
	metrics *fleetmetrics.Registry,
	log *slog.Logger,
) *gatewayNodeControl {
	return &gatewayNodeControl{
		ctx: ctx, fleet: flt, roster: roster, issuer: issuer,
		authority: authority, leaf: leaf, mode: mode, metrics: metrics, log: log,
		guestMode: guestMode, rollout: rollout, shadow: shadow,
		routedCanary: routedCanary, identityAddr: identityAddr,
	}
}

func controlRolloutStageConfig(stage string) (fleet.ControlRollout, bool, error) {
	ssh := fleet.ControlTransportSSH
	grpcMode := fleet.ControlTransportGRPC
	switch stage {
	case "", "inherit":
		return fleet.ControlRollout{}, false, nil
	case "shadow":
		return fleet.ControlRollout{
			ReadOnly: ssh, Idempotent: ssh, Destructive: ssh,
		}, true, nil
	case "read-only":
		return fleet.ControlRollout{
			ReadOnly: grpcMode, Idempotent: ssh, Destructive: ssh,
		}, true, nil
	case "idempotent":
		return fleet.ControlRollout{
			ReadOnly: grpcMode, Idempotent: grpcMode, Destructive: ssh,
		}, true, nil
	case "grpc":
		return fleet.ControlRollout{
			ReadOnly: grpcMode, Idempotent: grpcMode, Destructive: grpcMode,
		}, true, nil
	default:
		return fleet.ControlRollout{}, false, errors.New(
			"node control rollout must be inherit, shadow, read-only, idempotent, or grpc",
		)
	}
}

// Enroll is directly assignable to fleet.Options.OnCertificateEnroll. The
// response is produced first; installing the client is asynchronous because
// the node cannot start its TLS server until this response reaches it.
func (g *gatewayNodeControl) Enroll(
	ctx context.Context,
	authenticatedNode string,
	request nodelink.CertificateEnrollRequest,
) (nodelink.CertificateEnrollResponse, error) {
	if g == nil || g.issuer == nil {
		return nodelink.CertificateEnrollResponse{}, errors.New("gateway node control is not configured")
	}
	response, err := g.issuer.Issue(ctx, authenticatedNode, request)
	if err != nil {
		return nodelink.CertificateEnrollResponse{}, err
	}
	response.GatewayGRPCAddr = g.identityAddr
	go g.install(authenticatedNode)
	return response, nil
}

// newGatewayIdentityServer binds the gateway-hosted half of workload identity.
// Its listener is separate from NodeControl because that service is node-hosted;
// both directions reuse the same CA and renewable gateway leaf.
func newGatewayIdentityServer(
	ctx context.Context,
	address string,
	flt *fleet.Fleet,
	roster *nodes.Store,
	authority *nodepki.Authority,
	leaf *gatewayCertificateSource,
) (*grpc.Server, net.Listener, error) {
	if address == "" {
		return nil, nil, nil
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, err
	}
	revoked := nodeenroll.RevocationCallback(roster, time.Now)
	service, err := grpcidentity.NewServer(flt, revoked)
	if err != nil {
		listener.Close() //nolint:errcheck
		return nil, nil, err
	}
	tlsConfig, err := nodecert.ServerTLSConfigForRole(
		leaf.current(), authority.Roots, nodecert.RoleNode,
		revoked,
	)
	if err != nil {
		listener.Close() //nolint:errcheck
		return nil, nil, err
	}
	// Fetch the renewable gateway leaf on every new handshake.
	tlsConfig.Certificates = nil
	tlsConfig.GetCertificate = leaf.GetCertificate
	server, err := grpcidentity.NewRPCServer(service, tlsConfig)
	if err != nil {
		listener.Close() //nolint:errcheck
		return nil, nil, err
	}
	go func() {
		<-ctx.Done()
		stopped := make(chan struct{})
		go func() {
			server.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(2 * time.Second):
			server.Stop()
		}
	}()
	return server, listener, nil
}

// StartExisting restores gRPC bindings after a gateway restart. TLS and the
// roster's current-serial lookup decide whether a persisted certificate is
// still usable; unhealthy auto-mode bindings leave SSH control selected.
func (g *gatewayNodeControl) StartExisting() error {
	if g == nil {
		return nil
	}
	rows, err := g.roster.List()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Status == nodes.StatusApproved && row.GRPCAddr != "" &&
			row.CertSerial != "" && row.CertExpiresAt != nil &&
			row.CertExpiresAt.After(time.Now()) {
			go g.install(row.Name)
		}
	}
	return nil
}

func (g *gatewayNodeControl) install(node string) {
	row, err := g.roster.Get(node)
	if err != nil || row.Status != nodes.StatusApproved || row.GRPCAddr == "" {
		if err != nil {
			g.log.Warn("load node gRPC configuration", "node", node, "err", err)
		} else if row.GRPCAddr == "" {
			g.log.Warn("node has no approved gRPC address", "node", node)
		}
		return
	}
	tlsConfig, err := nodecert.ClientTLSConfig(
		g.leaf.current(),
		g.authority.Roots,
		nodecert.Peer{Role: nodecert.RoleNode, Name: node},
		nodeenroll.RevocationCallback(g.roster, time.Now),
	)
	if err != nil {
		g.log.Error("configure node gRPC authentication", "node", node, "err", err)
		return
	}
	// Force every new TLS handshake through the renewable source rather than
	// retaining the construction-time leaf in the cloned credentials config.
	tlsConfig.Certificates = nil
	tlsConfig.GetClientCertificate = g.leaf.GetClientCertificate
	client, err := grpccontrol.DialTLS(g.ctx, row.GRPCAddr, tlsConfig)
	if err != nil {
		g.log.Warn("dial node gRPC control", "node", node, "addr", row.GRPCAddr, "err", err)
		return
	}
	control, _, err := g.fleet.InstallGRPCControl(g.ctx, g.mode, fleet.GRPCControlOptions{
		Node: node, Client: client, Metrics: g.metrics, Log: g.log,
	})
	if err != nil {
		_ = client.Close()
		g.log.Error("install node gRPC control", "node", node, "err", err)
		return
	}
	if err := g.fleet.ConfigureControlRollout(node, g.rollout); err != nil {
		g.log.Error("configure node control rollout", "node", node, "err", err)
		return
	}
	_, err = g.fleet.ConfigureShadowInventory(
		node,
		g.shadow,
		func(report fleet.ShadowInventoryReport) {
			if !report.Available {
				g.log.Debug("node shadow inventory unavailable", "node", node)
				return
			}
			level := slog.LevelInfo
			if !report.Match {
				level = slog.LevelWarn
			}
			g.log.Log(g.ctx, level, "node shadow inventory compared",
				"node", node,
				"match", report.Match,
				"ssh_sandboxes", report.SSHSandboxes,
				"grpc_sandboxes", report.GRPCSandboxes,
				"missing_on_ssh", report.MissingOnSSH,
				"missing_on_grpc", report.MissingOnGRPC,
				"sandbox_diffs", report.SandboxDiffs,
				"snapshot_diffs", report.SnapshotDiffs,
				"capacity_diff", report.CapacityDiff,
				"facts_diff", report.FactsDiff,
			)
		},
	)
	if err != nil {
		g.log.Error("configure node shadow inventory", "node", node, "err", err)
		return
	}
	if g.guestMode != fleet.GuestTransportSSH {
		if err := g.fleet.InstallRoutedGuest(
			node, g.guestMode, row.ApprovedGuestSubnet,
		); err != nil {
			g.log.Error("install routed node guest transport",
				"node", node, "prefix", row.ApprovedGuestSubnet,
				"mode", g.guestMode, "err", err)
		} else if err := g.fleet.ConfigureRoutedCanary(node, g.routedCanary); err != nil {
			g.log.Error("configure routed guest canary",
				"node", node, "percent", g.routedCanary, "err", err)
		}
	}
	g.log.Info("node gRPC control configured",
		"node", node,
		"addr", row.GRPCAddr,
		"shadow_inventory", g.shadow,
		"routed_canary_percent", g.routedCanary,
	)
	go func() {
		readyCtx, cancel := context.WithTimeout(g.ctx, 30*time.Second)
		defer cancel()
		if err := control.WaitReady(readyCtx); err != nil && g.ctx.Err() == nil {
			g.log.Warn("node gRPC control not ready; rollout selector remains degraded",
				"node", node, "mode", g.mode, "err", err)
		}
	}()
}
