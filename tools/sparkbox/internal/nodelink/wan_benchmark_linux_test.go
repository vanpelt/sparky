//go:build linux

package nodelink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
)

// TestWANBenchmarkCell is an opt-in measurement cell driven by
// hack/fleet-wan-bench.sh. It uses the existing real-SSH fixture and emits one
// machine-readable result; ordinary go test runs skip it.
func TestWANBenchmarkCell(t *testing.T) {
	if os.Getenv("SPARKBOX_WAN_BENCH") != "1" {
		t.Skip("set SPARKBOX_WAN_BENCH=1; normally run through hack/fleet-wan-bench.sh")
	}
	concurrency := benchEnvInt(t, "SPARKBOX_WAN_CONCURRENCY", 0)
	samples := benchEnvInt(t, "SPARKBOX_WAN_SAMPLES", 50)
	if samples < 1 {
		t.Fatal("SPARKBOX_WAN_SAMPLES must be at least 1")
	}
	benchmarkHost := strings.TrimSpace(os.Getenv("SPARKBOX_WAN_BENCH_HOST"))
	if benchmarkHost == "" {
		t.Fatal("SPARKBOX_WAN_BENCH_HOST must name the Linux host producing this measurement")
	}
	addr := os.Getenv("SPARKBOX_WAN_GATEWAY_ADDR")
	if addr == "" {
		addr = "127.0.0.1:22222"
	}

	metrics := fleetmetrics.New()
	dl := newDataLink(t, atGatewayAddr(addr), withDataLinkMetrics(metrics))
	dl.running(t, "web")
	target := echoServer(t)
	_, rawPort, err := net.SplitHostPort(target)
	if err != nil {
		t.Fatal(err)
	}
	httpPort := wanHTTPServer(t)
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}

	held := openHeldStreams(t, dl, port, concurrency)
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()

	if os.Getenv("SPARKBOX_WAN_STALLED_READER") == "1" {
		stalled := stalledTCPPeer(t)
		_, rawStallPort, _ := net.SplitHostPort(stalled)
		stallPort, _ := strconv.Atoi(rawStallPort)
		c, err := dl.client.DialSandbox(context.Background(), "web", StreamTCP, stallPort)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		go io.Copy(c, io.LimitReader(zeroReader{}, 32<<20)) //nolint:errcheck // deliberate blocked writer
	}

	control := make([]time.Duration, 0, samples)
	opened := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		var pong PingReq
		err := dl.client.Do(ctx, TypePing, PingReq{Nonce: strconv.Itoa(i)}, &pong)
		control = append(control, time.Since(start))
		cancel()
		if err != nil {
			t.Fatalf("control sample %d: %v", i, err)
		}

		start = time.Now()
		c, err := dl.client.DialSandbox(context.Background(), "web", StreamTCP, port)
		opened = append(opened, time.Since(start))
		if err != nil {
			t.Fatalf("stream-open sample %d: %v", i, err)
		}
		c.Close()
	}

	dialHTTP := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dl.client.DialSandbox(ctx, "web", StreamTCP, httpPort)
	}
	coldTransport := &http.Transport{
		DialContext: dialHTTP, DisableKeepAlives: true,
	}
	defer coldTransport.CloseIdleConnections()
	coldClient := &http.Client{Transport: coldTransport}
	coldTTFB := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		sample, err := measureHTTPTTFB(coldClient)
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
	if _, err := measureHTTPTTFB(warmClient); err != nil {
		t.Fatalf("warm HTTP preflight: %v", err)
	}
	warmTTFB := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		sample, err := measureHTTPTTFB(warmClient)
		if err != nil {
			t.Fatalf("warm HTTP TTFB sample %d: %v", i, err)
		}
		warmTTFB = append(warmTTFB, sample)
	}

	transportTotals, err := parseWANTransportTotals(
		scrapeMetrics(t, metrics), "node-b", "ssh",
	)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{
		"benchmark_host":     benchmarkHost,
		"rtt_ms":             os.Getenv("SPARKBOX_WAN_RTT_MS"),
		"loss_percent":       os.Getenv("SPARKBOX_WAN_LOSS_PERCENT"),
		"bandwidth_mbps":     os.Getenv("SPARKBOX_WAN_BANDWIDTH_MBPS"),
		"concurrent_streams": concurrency,
		"stalled_reader":     os.Getenv("SPARKBOX_WAN_STALLED_READER") == "1",
		"samples":            samples,
		"control_ms":         percentiles(control),
		"stream_open_ms":     percentiles(opened),
		"warm_http_ttfb_ms":  percentiles(warmTTFB),
		"cold_http_ttfb_ms":  percentiles(coldTTFB),
		"transport_totals":   transportTotals,
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("WANBENCH_RESULT %s", raw)
}

func wanHTTPServer(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
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

func measureHTTPTTFB(client *http.Client) (time.Duration, error) {
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

func openHeldStreams(t *testing.T, dl *dataLink, port, n int) []net.Conn {
	t.Helper()
	if n < 0 || n >= MaxLiveStreams {
		t.Fatalf("concurrency %d must be between 0 and %d", n, MaxLiveStreams-1)
	}
	conns := make([]net.Conn, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := dl.client.DialSandbox(context.Background(), "web", StreamTCP, port)
			if err == nil {
				want := fmt.Sprintf("%04d", i)
				_, err = c.Write([]byte(want))
				if err == nil {
					got := make([]byte, len(want))
					_, err = io.ReadFull(c, got)
				}
			}
			if err != nil {
				errs <- fmt.Errorf("held stream %d: %w", i, err)
				if c != nil {
					c.Close()
				}
				return
			}
			conns[i] = c
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}
	return conns
}

func stalledTCPPeer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err == nil {
			<-t.Context().Done()
			c.Close()
		}
	}()
	return ln.Addr().String()
}

func benchEnvInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	if os.Getenv(name) == "" {
		return fallback
	}
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return n
}

func percentiles(in []time.Duration) map[string]float64 {
	values := append([]time.Duration(nil), in...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	at := func(p int) float64 {
		i := (len(values)*p + 99) / 100
		if i < 1 {
			i = 1
		}
		return float64(values[i-1].Microseconds()) / 1000
	}
	return map[string]float64{"p50": at(50), "p95": at(95), "p99": at(99)}
}
