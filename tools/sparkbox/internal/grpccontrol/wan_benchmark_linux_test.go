//go:build linux

package grpccontrol

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/vanpelt/sparky/tools/sparkbox/api/node/v1"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/eventjournal"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/operationjournal"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routedguest"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"google.golang.org/grpc"
)

// TestSplitWANBenchmarkCell is the gRPC-control/routed-data companion to
// nodelink.TestWANBenchmarkCell. The shell harness puts its fixed control and
// guest addresses through the same tc netem cell, while the test keeps the
// transports independent:
//
//   - one long-lived, TLS 1.3/mTLS HTTP/2 connection carries NodeControl.Health;
//   - ordinary TCP connections opened by routedguest.Dialer carry guest data.
//
// It is opt-in because it binds addresses installed only inside the harness's
// temporary network namespace.
func TestSplitWANBenchmarkCell(t *testing.T) {
	if os.Getenv("SPARKBOX_SPLIT_WAN_BENCH") != "1" {
		t.Skip("set SPARKBOX_SPLIT_WAN_BENCH=1; normally run through hack/fleet-split-wan-bench.sh")
	}
	concurrency := splitBenchEnvInt(t, "SPARKBOX_WAN_CONCURRENCY", 0)
	samples := splitBenchEnvInt(t, "SPARKBOX_WAN_SAMPLES", 50)
	if samples < 1 {
		t.Fatal("SPARKBOX_WAN_SAMPLES must be at least 1")
	}
	if concurrency < 0 || concurrency > 4096 {
		t.Fatalf("SPARKBOX_WAN_CONCURRENCY must be between 0 and 4096, got %d", concurrency)
	}
	benchmarkHost := strings.TrimSpace(os.Getenv("SPARKBOX_WAN_BENCH_HOST"))
	if benchmarkHost == "" {
		t.Fatal("SPARKBOX_WAN_BENCH_HOST must name the Linux host producing this measurement")
	}
	controlAddr := strings.TrimSpace(os.Getenv("SPARKBOX_SPLIT_WAN_CONTROL_ADDR"))
	if controlAddr == "" {
		controlAddr = "100.100.0.2:24443"
	}
	guestIP := strings.TrimSpace(os.Getenv("SPARKBOX_SPLIT_WAN_GUEST_IP"))
	if guestIP == "" {
		guestIP = "10.250.0.2"
	}
	guestPrefix := strings.TrimSpace(os.Getenv("SPARKBOX_SPLIT_WAN_GUEST_PREFIX"))
	if guestPrefix == "" {
		guestPrefix = "10.250.0.0/20"
	}

	control := newSplitWANControl(t, controlAddr)
	data, observations := newSplitWANData(t, guestPrefix, guestIP)
	echoPort := splitEchoServer(t, guestIP)
	httpPort := splitHTTPServer(t, guestIP)

	held := splitOpenHeldStreams(t, data, echoPort, concurrency)
	defer func() {
		for _, connection := range held {
			connection.Close()
		}
	}()

	if os.Getenv("SPARKBOX_WAN_STALLED_READER") == "1" {
		stalledPort := splitStalledTCPPeer(t, guestIP)
		connection, err := data.DialGuest(
			context.Background(), "web", nodelink.StreamTCP, stalledPort,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		go io.Copy(connection, io.LimitReader(splitZeroReader{}, 32<<20)) //nolint:errcheck // deliberate blocked writer
	}

	controlSamples := make([]time.Duration, 0, samples)
	opened := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		started := time.Now()
		health, err := control.Health(ctx)
		controlSamples = append(controlSamples, time.Since(started))
		cancel()
		if err != nil {
			t.Fatalf("control sample %d: %v", i, err)
		}
		if health.GetStatus() != nodev1.HealthStatus_HEALTH_STATUS_SERVING {
			t.Fatalf("control sample %d: health status %s", i, health.GetStatus())
		}

		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		started = time.Now()
		connection, err := data.DialGuest(ctx, "web", nodelink.StreamTCP, echoPort)
		opened = append(opened, time.Since(started))
		cancel()
		if err != nil {
			t.Fatalf("stream-open sample %d: %v", i, err)
		}
		connection.Close()
	}

	dialHTTP := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return data.DialGuest(ctx, "web", nodelink.StreamTCP, httpPort)
	}
	coldTransport := &http.Transport{
		DialContext: dialHTTP, DisableKeepAlives: true,
	}
	defer coldTransport.CloseIdleConnections()
	coldClient := &http.Client{Transport: coldTransport}
	coldTTFB := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		sample, err := splitMeasureHTTPTTFB(coldClient)
		if err != nil {
			t.Fatalf("cold HTTP TTFB sample %d: %v", i, err)
		}
		coldTTFB = append(coldTTFB, sample)
	}

	warmTransport := &http.Transport{
		DialContext: dialHTTP, MaxIdleConns: 1, MaxIdleConnsPerHost: 1,
	}
	defer warmTransport.CloseIdleConnections()
	warmClient := &http.Client{Transport: warmTransport}
	if _, err := splitMeasureHTTPTTFB(warmClient); err != nil {
		t.Fatalf("warm HTTP preflight: %v", err)
	}
	warmTTFB := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		sample, err := splitMeasureHTTPTTFB(warmClient)
		if err != nil {
			t.Fatalf("warm HTTP TTFB sample %d: %v", i, err)
		}
		warmTTFB = append(warmTTFB, sample)
	}

	result := map[string]any{
		"benchmark_host":     benchmarkHost,
		"rtt_ms":             os.Getenv("SPARKBOX_WAN_RTT_MS"),
		"loss_percent":       os.Getenv("SPARKBOX_WAN_LOSS_PERCENT"),
		"bandwidth_mbps":     os.Getenv("SPARKBOX_WAN_BANDWIDTH_MBPS"),
		"concurrent_streams": concurrency,
		"stalled_reader":     os.Getenv("SPARKBOX_WAN_STALLED_READER") == "1",
		"samples":            samples,
		"control_transport":  "grpc_mtls",
		"data_transport":     "routed_tcp",
		"control_operation":  "health",
		"control_ms":         splitPercentiles(controlSamples),
		"stream_open_ms":     splitPercentiles(opened),
		"warm_http_ttfb_ms":  splitPercentiles(warmTTFB),
		"cold_http_ttfb_ms":  splitPercentiles(coldTTFB),
		"route_observations": observations.snapshot(),
		"control_totals": map[string]any{
			"failures_total": 0,
			"connection":     "reused",
		},
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SPLIT_WANBENCH_RESULT %s", raw)
}

func newSplitWANControl(t *testing.T, address string) *Client {
	t.Helper()
	backend := newFakeBackend()
	operations, err := operationjournal.Open(t.TempDir() + "/operations.db")
	if err != nil {
		t.Fatal(err)
	}
	events, err := eventjournal.Open(t.TempDir()+"/events.db", 16)
	if err != nil {
		operations.Close()
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	service, err := NewServer(ServerConfig{
		Context: runContext, Backend: backend, Operations: operations, Events: events,
		Node: "node-a", Version: "benchmark",
	})
	if err != nil {
		cancel()
		operations.Close()
		events.Close()
		t.Fatal(err)
	}
	authority, _, _, err := nodecert.NewCA("benchmark")
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority.Certificate())
	nodeLeaf := splitIssueCertificate(
		t, authority, nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"},
	)
	gatewayLeaf := splitIssueCertificate(
		t, authority, nodecert.Peer{Role: nodecert.RoleGateway, Name: "benchmark"},
	)
	serverTLS, err := nodecert.ServerTLSConfig(
		nodeLeaf, roots,
		nodecert.Peer{Role: nodecert.RoleGateway, Name: "benchmark"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewRPCServer(service, serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener) //nolint:errcheck // stopped by cleanup

	clientTLS, err := nodecert.ClientTLSConfig(
		gatewayLeaf, roots,
		nodecert.Peer{Role: nodecert.RoleNode, Name: "node-a"}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	dialContext, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()
	client, err := DialTLS(dialContext, address, clientTLS, grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		cancel()
		server.Stop()
		listener.Close()
		operations.Close()
		events.Close()
	})
	return client
}

func splitIssueCertificate(
	t *testing.T,
	authority *nodecert.CA,
	peer nodecert.Peer,
) tls.Certificate {
	t.Helper()
	key, csr, err := nodecert.NewCSR(peer.Name)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM, _, _, err := authority.SignCSR(csr, peer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

type splitRouteObservations struct {
	mu     sync.Mutex
	counts map[routedguest.Outcome]int
}

func (o *splitRouteObservations) observe(observation routedguest.Observation) {
	o.mu.Lock()
	o.counts[observation.Outcome]++
	o.mu.Unlock()
}

func (o *splitRouteObservations) snapshot() map[string]int {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make(map[string]int, len(o.counts))
	for outcome, count := range o.counts {
		out[string(outcome)] = count
	}
	for _, outcome := range []routedguest.Outcome{
		routedguest.OutcomeConnected,
		routedguest.OutcomeRejected,
		routedguest.OutcomeRouteFailure,
		routedguest.OutcomeCanceled,
	} {
		if _, ok := out[string(outcome)]; !ok {
			out[string(outcome)] = 0
		}
	}
	return out
}

func newSplitWANData(
	t *testing.T,
	rawPrefix, guestIP string,
) (*routedguest.Dialer, *splitRouteObservations) {
	t.Helper()
	prefix, err := netip.ParsePrefix(rawPrefix)
	if err != nil {
		t.Fatal(err)
	}
	address, err := netip.ParseAddr(guestIP)
	if err != nil || !prefix.Contains(address) {
		t.Fatalf("guest IP %q is not inside %s", guestIP, prefix)
	}
	box := &host.Sandbox{
		Name: "web", State: vmm.StateRunning, HostIP: guestIP,
		SSHAddr: net.JoinHostPort(guestIP, "22"),
	}
	observations := &splitRouteObservations{
		counts: make(map[routedguest.Outcome]int),
	}
	dialer, err := routedguest.New(prefix, func(name string) (*host.Sandbox, bool) {
		if name != box.Name {
			return nil, false
		}
		cloned := *box
		return &cloned, true
	}, routedguest.Options{
		Dialer:   &net.Dialer{Timeout: 30 * time.Second, KeepAlive: -1},
		Observer: observations.observe,
	})
	if err != nil {
		t.Fatal(err)
	}
	return dialer, observations
}

func splitEchoServer(t *testing.T, ip string) int {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	return splitListenerPort(t, listener)
}

func splitHTTPServer(t *testing.T, ip string) int {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.Header().Set("Content-Type", "text/plain")
			response.Header().Set("Content-Length", "2")
			_, _ = io.WriteString(response, "ok")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go server.Serve(listener) //nolint:errcheck // closed by cleanup
	t.Cleanup(func() {
		server.Close()
		listener.Close()
	})
	return splitListenerPort(t, listener)
}

func splitStalledTCPPeer(t *testing.T, ip string) int {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			<-t.Context().Done()
			connection.Close()
		}
	}()
	return splitListenerPort(t, listener)
}

func splitListenerPort(t *testing.T, listener net.Listener) int {
	t.Helper()
	_, rawPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func splitOpenHeldStreams(
	t *testing.T,
	dialer *routedguest.Dialer,
	port, count int,
) []net.Conn {
	t.Helper()
	connections := make([]net.Conn, count)
	errs := make(chan error, count)
	var wait sync.WaitGroup
	for i := range count {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			connection, err := dialer.DialGuest(ctx, "web", nodelink.StreamTCP, port)
			if err == nil {
				want := fmt.Sprintf("%04d", i)
				_, err = connection.Write([]byte(want))
				if err == nil {
					got := make([]byte, len(want))
					_, err = io.ReadFull(connection, got)
					if err == nil && string(got) != want {
						err = fmt.Errorf("echo = %q, want %q", got, want)
					}
				}
			}
			if err != nil {
				errs <- fmt.Errorf("held stream %d: %w", i, err)
				if connection != nil {
					connection.Close()
				}
				return
			}
			connections[i] = connection
		}(i)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
	return connections
}

func splitMeasureHTTPTTFB(client *http.Client) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	var firstByte time.Duration
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstByte = time.Since(started) },
	}
	request, err := http.NewRequestWithContext(
		httptrace.WithClientTrace(ctx, trace), http.MethodGet, "http://web/", nil,
	)
	if err != nil {
		return 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	_, copyErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP status %s", response.Status)
	}
	if firstByte <= 0 {
		return 0, errors.New("HTTP response completed without a first-byte trace")
	}
	return firstByte, nil
}

func splitBenchEnvInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	if os.Getenv(name) == "" {
		return fallback
	}
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return value
}

func splitPercentiles(values []time.Duration) map[string]float64 {
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	at := func(percent int) float64 {
		index := (len(sorted)*percent + 99) / 100
		if index < 1 {
			index = 1
		}
		return float64(sorted[index-1].Microseconds()) / 1000
	}
	return map[string]float64{"p50": at(50), "p95": at(95), "p99": at(99)}
}

type splitZeroReader struct{}

func (splitZeroReader) Read(buffer []byte) (int, error) {
	for i := range buffer {
		buffer[i] = 0
	}
	return len(buffer), nil
}
