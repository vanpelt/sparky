package host

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"
)

func serveProbeHTTP(t *testing.T, handler http.Handler) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	go server.Serve(listener) //nolint:errcheck
	t.Cleanup(func() { server.Close() })
	addr := listener.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func waitConnections(t *testing.T, connections *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for connections.Load() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
}

func TestProbeSupportedPortsAcceptsAnyHTTPStatus(t *testing.T) {
	hostIP := ""
	ports := make([]int, 0, 4)
	for _, status := range []int{http.StatusNoContent, http.StatusFound, http.StatusNotFound, http.StatusInternalServerError} {
		ip, port := serveProbeHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		hostIP = ip
		ports = append(ports, port)
	}

	services, _, err := probeSupportedPorts(context.Background(), hostIP, ports, nil, time.Now())
	if err != nil {
		t.Fatalf("probeSupportedPorts: %v", err)
	}
	got := make([]int, len(services))
	for i, service := range services {
		got[i] = service.Port
	}
	want := append([]int(nil), ports...)
	sort.Ints(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HTTP ports = %v, want %v", got, want)
	}
}

func TestProbeSupportedPortNamesService(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		body    string
		want    string
	}{
		{name: "html title", headers: map[string]string{"Content-Type": "text/html"}, body: "<html><head><title>  Jaeger   UI </title></head></html>", want: "Jaeger UI"},
		{name: "powered by", headers: map[string]string{"X-Powered-By": "Express"}, body: "not html", want: "Express"},
		{name: "server", headers: map[string]string{"Server": "ClickHouse"}, want: "ClickHouse"},
		{name: "json", headers: map[string]string{"Content-Type": "application/problem+json"}, body: `{\"detail\":\"missing\"}`, want: "JSON API"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hostIP, port := serveProbeHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for key, value := range tc.headers {
					w.Header().Set(key, value)
				}
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, tc.body) //nolint:errcheck
			}))
			state := probeSupportedPort(context.Background(), hostIP, port, portProbeState{}, time.Now())
			if !state.http || state.name != tc.want {
				t.Fatalf("state = %+v, want HTTP named %q", state, tc.want)
			}
		})
	}
}

func TestProbeSupportedPortBacksOffHTTPWhileTCPStaysOpen(t *testing.T) {
	var requests atomic.Int32
	hostIP, port := serveProbeHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	now := time.Now()
	state := probeSupportedPort(context.Background(), hostIP, port, portProbeState{}, now)
	if !state.http || requests.Load() != 1 {
		t.Fatalf("first probe = %+v, requests = %d", state, requests.Load())
	}
	state = probeSupportedPort(context.Background(), hostIP, port, state, now.Add(portScanTTL))
	if !state.http || requests.Load() != 1 {
		t.Fatalf("cached probe = %+v, requests = %d, want no second HTTP request", state, requests.Load())
	}
	state = probeSupportedPort(context.Background(), hostIP, port, state, now.Add(portHTTPBackoff))
	if !state.http || requests.Load() != 2 {
		t.Fatalf("refresh probe = %+v, requests = %d, want HTTP refresh", state, requests.Load())
	}
}

func TestProbeSupportedPortRejectsNonHTTPListener(t *testing.T) {
	var connections atomic.Int32
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			fmt.Fprintln(conn, "Port 9000 is for clickhouse-client program") //nolint:errcheck
			conn.Close()                                                     //nolint:errcheck
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	now := time.Now()
	state := probeSupportedPort(context.Background(), addr.IP.String(), addr.Port, portProbeState{}, now)
	if state.http || !state.tcpOpen {
		t.Fatalf("plain TCP listener state = %+v, want TCP-open but not HTTP", state)
	}
	waitConnections(t, &connections, 2)
	if got := connections.Load(); got != 2 {
		t.Fatalf("initial connections = %d, want TCP check plus HTTP classification", got)
	}
	state = probeSupportedPort(context.Background(), addr.IP.String(), addr.Port, state, now.Add(portScanTTL))
	waitConnections(t, &connections, 3)
	if got := connections.Load(); got != 3 {
		t.Fatalf("backoff connections = %d, want only one additional TCP check", got)
	}
	probeSupportedPort(context.Background(), addr.IP.String(), addr.Port, state, now.Add(portHTTPBackoff))
	waitConnections(t, &connections, 5)
	if got := connections.Load(); got != 5 {
		t.Fatalf("refresh connections = %d, want TCP check plus one HTTP retry", got)
	}
}

func TestProbeSupportedPortsHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := probeSupportedPorts(ctx, "127.0.0.1", []int{3000}, nil, time.Now()); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
