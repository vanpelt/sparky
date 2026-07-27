package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
)

func newTestAuthority(t *testing.T, cluster string) (*nodecert.CA, *x509.CertPool) {
	t.Helper()
	authority, _, _, err := nodecert.NewCA(cluster)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority.Certificate())
	return authority, roots
}

func issueTestIdentity(t *testing.T, authority *nodecert.CA, peer nodecert.Peer, ttl time.Duration) tls.Certificate {
	t.Helper()
	key, csr, err := nodecert.NewCSR(peer.Name)
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	certificatePEM, _, _, err := authority.SignCSR(csr, peer, ttl)
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	leaf, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	leaf.Leaf, err = x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return leaf
}

func tlsHandshake(t *testing.T, serverConfig, clientConfig *tls.Config) (serverErr, clientErr error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close() //nolint:errcheck
	serverResult := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverResult <- err
			return
		}
		defer conn.Close() //nolint:errcheck
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			serverResult <- err
			return
		}
		serverResult <- tls.Server(conn, serverConfig.Clone()).Handshake()
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close() //nolint:errcheck
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	clientErr = tls.Client(conn, clientConfig.Clone()).Handshake()
	return <-serverResult, clientErr
}

func TestNodeTLSStateRotatesLeafAndExactGatewayTrustTogether(t *testing.T) {
	nodePeer := nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"}
	gatewayOne := nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-one"}
	caOne, rootsOne := newTestAuthority(t, gatewayOne.Name)
	nodeOne := issueTestIdentity(t, caOne, nodePeer, 4*time.Hour)
	gatewayLeafOne := issueTestIdentity(t, caOne, gatewayOne, 4*time.Hour)

	state, err := newNodeTLSState(nodeOne, rootsOne, gatewayOne, time.Now)
	if err != nil {
		t.Fatalf("newNodeTLSState: %v", err)
	}
	clientOne, err := nodecert.ClientTLSConfig(gatewayLeafOne, rootsOne, nodePeer, nil)
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if serverErr, clientErr := tlsHandshake(t, state.serverConfig(), clientOne); serverErr != nil || clientErr != nil {
		t.Fatalf("initial handshake: server=%v client=%v", serverErr, clientErr)
	}

	// A certificate from the right CA but for a different gateway SPIFFE name
	// must still be refused.
	wrongGateway := nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-other"}
	wrongLeaf := issueTestIdentity(t, caOne, wrongGateway, 4*time.Hour)
	wrongClient, err := nodecert.ClientTLSConfig(wrongLeaf, rootsOne, nodePeer, nil)
	if err != nil {
		t.Fatalf("wrong ClientTLSConfig: %v", err)
	}
	serverErr, _ := tlsHandshake(t, state.serverConfig(), wrongClient)
	if !errors.Is(serverErr, nodecert.ErrIdentity) {
		t.Fatalf("wrong gateway handshake server error = %v, want ErrIdentity", serverErr)
	}

	gatewayTwo := nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster-two"}
	caTwo, rootsTwo := newTestAuthority(t, gatewayTwo.Name)
	nodeTwo := issueTestIdentity(t, caTwo, nodePeer, 5*time.Hour)
	gatewayLeafTwo := issueTestIdentity(t, caTwo, gatewayTwo, 5*time.Hour)
	if err := state.update(nodeTwo, rootsTwo, gatewayTwo); err != nil {
		t.Fatalf("rotate node TLS state: %v", err)
	}
	clientTwo, err := nodecert.ClientTLSConfig(gatewayLeafTwo, rootsTwo, nodePeer, nil)
	if err != nil {
		t.Fatalf("renewed ClientTLSConfig: %v", err)
	}
	if serverErr, clientErr := tlsHandshake(t, state.serverConfig(), clientTwo); serverErr != nil || clientErr != nil {
		t.Fatalf("renewed handshake: server=%v client=%v", serverErr, clientErr)
	}
	if serverErr, clientErr := tlsHandshake(t, state.serverConfig(), clientOne); serverErr == nil && clientErr == nil {
		t.Fatal("pre-renewal gateway still authenticated after the trust snapshot rotated")
	}
}

func TestNodeTLSStateFailsClosedAtLeafExpiry(t *testing.T) {
	nodePeer := nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"}
	gateway := nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster"}
	authority, roots := newTestAuthority(t, gateway.Name)
	leaf := issueTestIdentity(t, authority, nodePeer, 2*time.Hour)
	now := leaf.Leaf.NotBefore.Add(time.Minute)
	state, err := newNodeTLSState(leaf, roots, gateway, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newNodeTLSState: %v", err)
	}
	if _, err := state.currentConfig(nil); err != nil {
		t.Fatalf("current config before expiry: %v", err)
	}

	now = leaf.Leaf.NotAfter
	if _, err := state.currentConfig(nil); !errors.Is(err, errNodeCertificateExpired) {
		t.Fatalf("config at expiry = %v, want fail-closed expiry", err)
	}
	if err := state.update(leaf, roots, gateway); !errors.Is(err, errNodeCertificateExpired) {
		t.Fatalf("install expired leaf = %v, want expiry refusal", err)
	}
}

func TestRenewNodeCertificateUpdatesInPlaceOnSchedule(t *testing.T) {
	nodePeer := nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"}
	gateway := nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster"}
	authority, roots := newTestAuthority(t, gateway.Name)
	initial := issueTestIdentity(t, authority, nodePeer, 4*time.Hour)
	renewed := issueTestIdentity(t, authority, nodePeer, 6*time.Hour)
	now := initial.Leaf.NotBefore.Add(10 * time.Minute)
	state, err := newNodeTLSState(initial, roots, gateway, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newNodeTLSState: %v", err)
	}
	initialSerial, _ := state.details()

	ctx, cancel := context.WithCancel(context.Background())
	var waits []time.Duration
	wait := func(_ context.Context, delay time.Duration) bool {
		waits = append(waits, delay)
		if len(waits) == 1 {
			return true
		}
		cancel()
		return false
	}
	loadCalls := 0
	load := func(context.Context) (tls.Certificate, *x509.CertPool, nodecert.Peer, error) {
		loadCalls++
		return renewed, roots, gateway, nil
	}
	renewNodeCertificateWithWait(ctx, state, load,
		slog.New(slog.DiscardHandler), func() time.Time { return now }, wait)

	if loadCalls != 1 || len(waits) != 2 {
		t.Fatalf("load calls/waits = %d/%v, want one renewal and a second scheduled wait", loadCalls, waits)
	}
	wantFirstDelay := initial.Leaf.NotAfter.Add(-certificateRenewNow).Sub(now)
	if waits[0] != wantFirstDelay {
		t.Errorf("first renewal delay = %v, want %v", waits[0], wantFirstDelay)
	}
	renewedSerial, renewedExpiry := state.details()
	if renewedSerial == initialSerial || renewedSerial != nodecert.Serial(renewed.Leaf.SerialNumber) {
		t.Errorf("renewed serial = %q; initial=%q want=%q",
			renewedSerial, initialSerial, nodecert.Serial(renewed.Leaf.SerialNumber))
	}
	if !renewedExpiry.Equal(renewed.Leaf.NotAfter) {
		t.Errorf("renewed expiry = %v, want %v", renewedExpiry, renewed.Leaf.NotAfter)
	}
}

func TestRenewNodeCertificateFailureKeepsCurrentLeafAndBacksOff(t *testing.T) {
	nodePeer := nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"}
	gateway := nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster"}
	authority, roots := newTestAuthority(t, gateway.Name)
	initial := issueTestIdentity(t, authority, nodePeer, 4*time.Hour)
	now := initial.Leaf.NotBefore.Add(10 * time.Minute)
	state, err := newNodeTLSState(initial, roots, gateway, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newNodeTLSState: %v", err)
	}
	initialSerial, _ := state.details()

	ctx, cancel := context.WithCancel(context.Background())
	var waits []time.Duration
	wait := func(_ context.Context, delay time.Duration) bool {
		waits = append(waits, delay)
		if len(waits) == 1 {
			return true
		}
		cancel()
		return false
	}
	loadErr := errors.New("gateway unavailable")
	renewNodeCertificateWithWait(ctx, state,
		func(context.Context) (tls.Certificate, *x509.CertPool, nodecert.Peer, error) {
			return tls.Certificate{}, nil, nodecert.Peer{}, loadErr
		},
		slog.New(slog.DiscardHandler), func() time.Time { return now }, wait)

	if len(waits) != 2 || waits[1] != enrollmentRetry {
		t.Fatalf("renewal waits = %v, want scheduled attempt then %v backoff", waits, enrollmentRetry)
	}
	if serial, _ := state.details(); serial != initialSerial {
		t.Errorf("failed renewal replaced current serial %q with %q", initialSerial, serial)
	}
	if _, err := state.currentConfig(nil); err != nil {
		t.Errorf("still-valid current leaf was lost after renewal failure: %v", err)
	}
}

func TestShortLivedNodeCertificateDoesNotRenewInATightLoop(t *testing.T) {
	nodePeer := nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"}
	gateway := nodecert.Peer{Role: nodecert.RoleGateway, Name: "cluster"}
	authority, roots := newTestAuthority(t, gateway.Name)
	leaf := issueTestIdentity(t, authority, nodePeer, 30*time.Minute)
	now := leaf.Leaf.NotBefore.Add(5 * time.Minute)
	state, err := newNodeTLSState(leaf, roots, gateway, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newNodeTLSState: %v", err)
	}
	delay := state.renewalDelay(now)
	if delay <= 0 || delay >= leaf.Leaf.NotAfter.Sub(now) {
		t.Fatalf("short-lived renewal delay = %v; want a positive time before %v",
			delay, leaf.Leaf.NotAfter.Sub(now))
	}
}

func TestNodeControlCapabilitiesMirrorRoutedGuestHello(t *testing.T) {
	for _, tc := range []struct {
		transport string
		routed    bool
	}{
		{"ssh", false},
		{"auto", true},
		{"routed", true},
	} {
		t.Run(tc.transport, func(t *testing.T) {
			capabilities := nodeControlCapabilities(nodeOptions{guestDataTransport: tc.transport})
			if !slices.Contains(capabilities, nodelink.CapabilityGRPCControlV1) {
				t.Errorf("capabilities = %v, missing gRPC control", capabilities)
			}
			if got := slices.Contains(capabilities, nodelink.CapabilityRoutedGuestV1); got != tc.routed {
				t.Errorf("routed capability = %v, want %v in %v", got, tc.routed, capabilities)
			}
		})
	}
}
