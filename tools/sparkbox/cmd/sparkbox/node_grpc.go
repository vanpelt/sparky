package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sort"
	"sync"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/eventjournal"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/grpccontrol"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodepki"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/operationjournal"
	"google.golang.org/grpc"
)

const (
	nodeControlDB       = "node-control.db"
	enrollmentRetry     = 2 * time.Second
	enrollmentBudget    = 30 * time.Second
	certificateRenewNow = time.Hour
)

var errNodeCertificateExpired = errors.New("node control certificate is expired")

// nodeTLSState is the complete credential snapshot used by a node-control TLS
// handshake. The leaf, roots and exact expected gateway identity rotate
// together: a connection can observe either snapshot, never a new leaf with
// stale trust state (or the reverse).
type nodeTLSState struct {
	mu        sync.RWMutex
	config    *tls.Config
	notBefore time.Time
	notAfter  time.Time
	serial    string
	now       func() time.Time
}

func newNodeTLSState(
	leaf tls.Certificate,
	roots *x509.CertPool,
	gateway nodecert.Peer,
	now func() time.Time,
) (*nodeTLSState, error) {
	if now == nil {
		now = time.Now
	}
	state := &nodeTLSState{now: now}
	if err := state.update(leaf, roots, gateway); err != nil {
		return nil, err
	}
	return state, nil
}

func (s *nodeTLSState) update(
	leaf tls.Certificate,
	roots *x509.CertPool,
	gateway nodecert.Peer,
) error {
	if leaf.Leaf == nil {
		return errors.New("node control certificate has no parsed leaf")
	}
	if !leaf.Leaf.NotAfter.After(s.now()) {
		return errNodeCertificateExpired
	}
	config, err := nodecert.ServerTLSConfig(leaf, roots, gateway, nil)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.config = config
	s.notBefore = leaf.Leaf.NotBefore
	s.notAfter = leaf.Leaf.NotAfter
	s.serial = nodecert.Serial(leaf.Leaf.SerialNumber)
	s.mu.Unlock()
	return nil
}

// serverConfig returns the stable outer config handed to gRPC. TLS invokes
// currentConfig for every new connection, so renewals require no listener or
// gRPC restart. The initial certificate and ClientAuth fields are retained on
// the outer config because grpccontrol validates that callers supplied a
// certificate-requiring server configuration before constructing the server.
func (s *nodeTLSState) serverConfig() *tls.Config {
	s.mu.RLock()
	config := s.config.Clone()
	s.mu.RUnlock()
	config.GetConfigForClient = s.currentConfig
	return config
}

func (s *nodeTLSState) currentConfig(_ *tls.ClientHelloInfo) (*tls.Config, error) {
	s.mu.RLock()
	if s.config == nil || !s.notAfter.After(s.now()) {
		s.mu.RUnlock()
		return nil, errNodeCertificateExpired
	}
	config := s.config.Clone()
	s.mu.RUnlock()
	return config, nil
}

func (s *nodeTLSState) renewalDelay(now time.Time) time.Duration {
	s.mu.RLock()
	notBefore, notAfter := s.notBefore, s.notAfter
	s.mu.RUnlock()
	if !notAfter.After(now) {
		return 0
	}
	lead := certificateRenewNow
	// A deployment may deliberately issue leaves shorter than one hour. Renew
	// those after roughly two thirds of their validity rather than immediately
	// entering a tight loop because the ordinary one-hour lead exceeds the TTL.
	if third := notAfter.Sub(notBefore) / 3; third > 0 && third < lead {
		lead = third
	}
	delay := notAfter.Add(-lead).Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

func (s *nodeTLSState) details() (serial string, expires time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.serial, s.notAfter
}

type nodeCertificateLoader func(context.Context) (tls.Certificate, *x509.CertPool, nodecert.Peer, error)

func waitNodeCertificate(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// renewNodeCertificate keeps the gRPC server's workload identity short-lived
// without making the node's VM manager depend on gateway health. Failures leave
// the old snapshot in place until its leaf expires; currentConfig then refuses
// new handshakes while this loop continues retrying over the SSH bootstrap.
func renewNodeCertificate(
	ctx context.Context,
	state *nodeTLSState,
	load nodeCertificateLoader,
	log *slog.Logger,
) {
	renewNodeCertificateWithWait(ctx, state, load, log, time.Now, waitNodeCertificate)
}

func renewNodeCertificateWithWait(
	ctx context.Context,
	state *nodeTLSState,
	load nodeCertificateLoader,
	log *slog.Logger,
	now func() time.Time,
	wait func(context.Context, time.Duration) bool,
) {
	for {
		if !wait(ctx, state.renewalDelay(now())) {
			return
		}
		leaf, roots, gateway, err := load(ctx)
		if err == nil {
			err = state.update(leaf, roots, gateway)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("node control certificate renewal failed",
				"retry_in", enrollmentRetry, "err", err)
			if !wait(ctx, enrollmentRetry) {
				return
			}
			continue
		}
		serial, expires := state.details()
		log.Info("node control certificate renewed",
			"serial", serial, "certificate_expires", expires)
	}
}

type nodeControlState struct {
	operations     *operationjournal.Journal
	events         *eventjournal.Journal
	observer       *grpccontrol.EventObserver
	observerCancel context.CancelFunc
}

func openNodeControlState(stateDir string, log *slog.Logger) (*nodeControlState, error) {
	path := filepath.Join(stateDir, nodeControlDB)
	operations, err := operationjournal.Open(path)
	if err != nil {
		return nil, fmt.Errorf("node operation journal: %w", err)
	}
	events, err := eventjournal.Open(path, eventjournal.DefaultRetain)
	if err != nil {
		operations.Close() //nolint:errcheck
		return nil, fmt.Errorf("node event journal: %w", err)
	}
	observerCtx, observerCancel := context.WithCancel(context.Background())
	return &nodeControlState{
		operations:     operations,
		events:         events,
		observer:       grpccontrol.NewEventObserver(observerCtx, events, log),
		observerCancel: observerCancel,
	}, nil
}

func (s *nodeControlState) Close(log *slog.Logger) {
	if s == nil {
		return
	}
	flushCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if err := s.observer.Flush(flushCtx); err != nil {
		log.Warn("final node event flush failed", "err", err)
	}
	cancel()
	s.observerCancel()
	if err := s.events.Close(); err != nil {
		log.Warn("close node event journal", "err", err)
	}
	if err := s.operations.Close(); err != nil {
		log.Warn("close node operation journal", "err", err)
	}
}

// startNodeControl binds synchronously, then waits for an SSH-enrolled
// certificate before serving. A missing gateway never stops the VM manager:
// enrollment retries on the node's long-lived uplink while the listener stays
// reserved.
func startNodeControl(
	ctx context.Context,
	opts nodeOptions,
	mgr *host.Manager,
	network *netpush.Syncer,
	uplink *nodelink.Uplink,
	state *nodeControlState,
	identity *relayIdentity,
	log *slog.Logger,
) error {
	if opts.grpcAddr == "" || opts.controlTransport == "ssh" {
		if opts.controlTransport == "grpc" {
			return errors.New("--node-control-transport=grpc requires --node-grpc-addr")
		}
		return nil
	}
	listener, err := net.Listen("tcp", opts.grpcAddr)
	if err != nil {
		return fmt.Errorf("node gRPC listen: %w", err)
	}
	service, err := grpccontrol.NewServer(grpccontrol.ServerConfig{
		Context: ctx, Backend: mgr,
		Operations: state.operations, Events: state.events,
		Network: networkHooks(network),
		Node:    opts.nodeName, Version: version, StartedAt: time.Now().UTC(),
		Architecture: opts.arch, Release: version, Driver: opts.driverName,
		Capabilities:              nodeControlCapabilities(opts),
		SandboxEventsFromObserver: true,
	})
	if err != nil {
		listener.Close() //nolint:errcheck
		return err
	}
	go func() {
		defer listener.Close() //nolint:errcheck
		controlCtx, cancelControl := context.WithCancel(ctx)
		defer cancelControl()
		loadCertificate := func(loadCtx context.Context) (tls.Certificate, *x509.CertPool, nodecert.Peer, error) {
			leaf, roots, gateway, err := enrolledNodeCertificate(loadCtx, opts, uplink, log)
			if err == nil && identity != nil {
				if identityErr := identity.configureGRPC(loadCtx, opts.stateDir, opts.nodeName); identityErr != nil {
					log.Warn("gateway identity gRPC configuration failed; using SSH fallback", "err", identityErr)
				}
			}
			return leaf, roots, gateway, err
		}
		leaf, roots, gateway, err := loadCertificate(controlCtx)
		if err != nil {
			if ctx.Err() == nil {
				log.Error("node control enrollment stopped", "err", err)
			}
			return
		}
		tlsState, err := newNodeTLSState(leaf, roots, gateway, time.Now)
		if err != nil {
			log.Error("node control TLS configuration failed", "err", err)
			return
		}
		server, err := grpccontrol.NewRPCServer(service, tlsState.serverConfig())
		if err != nil {
			log.Error("node control server configuration failed", "err", err)
			return
		}
		go renewNodeCertificate(controlCtx, tlsState, loadCertificate, log)
		go func() {
			<-controlCtx.Done()
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
		serial, expires := tlsState.details()
		log.Info("mTLS node control enabled", "addr", listener.Addr(),
			"certificate_serial", serial, "certificate_expires", expires)
		if err := server.Serve(listener); err != nil && ctx.Err() == nil &&
			!errors.Is(err, grpc.ErrServerStopped) {
			log.Error("node control server stopped", "err", err)
		}
	}()
	return nil
}

func nodeControlCapabilities(opts nodeOptions) []string {
	capabilities := []string{
		"durable_operations_v1",
		"inventory_events_v1",
		nodelink.CapabilityGRPCControlV1,
	}
	if opts.guestDataTransport != "ssh" {
		capabilities = append(capabilities, nodelink.CapabilityRoutedGuestV1)
	}
	return capabilities
}

func enrolledNodeCertificate(
	ctx context.Context,
	opts nodeOptions,
	uplink *nodelink.Uplink,
	log *slog.Logger,
) (tls.Certificate, *x509.CertPool, nodecert.Peer, error) {
	for {
		leaf, roots, certErr := nodepki.LoadNodeCertificate(opts.stateDir, opts.nodeName)
		gateway, identityErr := nodepki.LoadGatewayIdentity(opts.stateDir)
		if certErr == nil && identityErr == nil &&
			leaf.Leaf.NotAfter.After(time.Now().Add(certificateRenewNow)) {
			return leaf, roots, gateway, nil
		}
		csr, err := nodepki.NodeCSR(opts.stateDir, opts.nodeName)
		if err != nil {
			return tls.Certificate{}, nil, nodecert.Peer{}, err
		}
		enrollCtx, cancel := context.WithTimeout(ctx, enrollmentBudget)
		response, err := uplink.EnrollCertificate(enrollCtx, csr)
		cancel()
		if err == nil {
			leaf, roots, err = nodepki.InstallNodeCertificate(
				opts.stateDir, opts.nodeName,
				response.CertificatePEM, response.CACertificatePEM,
			)
			if err == nil {
				err = nodepki.StoreGatewayIdentity(opts.stateDir, response.GatewayIdentity)
			}
			if err == nil {
				err = nodepki.StoreGatewayGRPCAddr(opts.stateDir, response.GatewayGRPCAddr)
			}
			if err == nil {
				gateway, err = nodepki.GatewayPeer(response.GatewayIdentity)
			}
			if err == nil {
				return leaf, roots, gateway, nil
			}
		}
		if ctx.Err() != nil {
			return tls.Certificate{}, nil, nodecert.Peer{}, ctx.Err()
		}
		log.Debug("node control certificate not ready", "retry_in", enrollmentRetry, "err", err)
		timer := time.NewTimer(enrollmentRetry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return tls.Certificate{}, nil, nodecert.Peer{}, ctx.Err()
		case <-timer.C:
		}
	}
}

type grpcNetworkHooks struct{ syncer *netpush.Syncer }

func networkHooks(syncer *netpush.Syncer) grpccontrol.NetworkHooks {
	if syncer == nil || !syncer.Enabled() {
		return nil
	}
	return grpcNetworkHooks{syncer: syncer}
}

func (n grpcNetworkHooks) ApplyNetworkPolicy(ctx context.Context, policies []*nodev1.NetworkPolicy) error {
	allow := make(map[string][]string, len(policies))
	for _, policy := range policies {
		if policy == nil || policy.GetSandbox() == "" {
			continue
		}
		allow[policy.GetSandbox()] = append([]string(nil), policy.GetAllowedDestinations()...)
	}
	return n.syncer.Apply(ctx, allow)
}

func (n grpcNetworkHooks) GetNetworkUsage(ctx context.Context) ([]*nodev1.NetworkUsage, error) {
	report, err := n.syncer.Usage(ctx, "")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(report))
	for name := range report {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*nodev1.NetworkUsage, 0, len(names))
	for _, name := range names {
		usage := report[name]
		out = append(out, &nodev1.NetworkUsage{
			Sandbox: name, RxBytes: usage.RxBytes, TxBytes: usage.TxBytes,
		})
	}
	return out, nil
}
