package host

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func serveProbeHTTP(t *testing.T, status int, headStatus int) (string, int) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead && headStatus != 0 {
			w.WriteHeader(headStatus)
			return
		}
		w.WriteHeader(status)
	})}
	go server.Serve(listener) //nolint:errcheck
	t.Cleanup(func() { server.Close() })
	addr := listener.Addr().(*net.TCPAddr)
	return addr.IP.String(), addr.Port
}

func TestProbeHTTPPortsKeepsOnlySuccessAndRedirect(t *testing.T) {
	hostIP, successPort := serveProbeHTTP(t, http.StatusNoContent, 0)
	_, redirectPort := serveProbeHTTP(t, http.StatusFound, 0)
	_, missingPort := serveProbeHTTP(t, http.StatusNotFound, 0)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := probeHTTPPorts(ctx, hostIP, []int{missingPort, successPort, redirectPort})
	if err != nil {
		t.Fatalf("probeHTTPPorts: %v", err)
	}
	want := []int{successPort, redirectPort}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HTTP-ready ports = %v, want %v", got, want)
	}
}

func TestProbeHTTPPortFallsBackToGETWhenHEADIsUnsupported(t *testing.T) {
	hostIP, port := serveProbeHTTP(t, http.StatusOK, http.StatusMethodNotAllowed)
	if !probeHTTPPort(context.Background(), hostIP, port) {
		t.Fatal("GET-capable service was hidden after HEAD returned 405")
	}
}

func TestProbeHTTPPortRejectsNonHTTPListener(t *testing.T) {
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
			fmt.Fprintln(conn, "Port 9000 is for clickhouse-client program") //nolint:errcheck
			conn.Close()                                                     //nolint:errcheck
		}
	}()
	addr := listener.Addr().(*net.TCPAddr)
	if probeHTTPPort(context.Background(), addr.IP.String(), addr.Port) {
		t.Fatal("plain TCP listener was reported as an HTTP service")
	}
}

func TestProbeHTTPPortsHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := probeHTTPPorts(ctx, "127.0.0.1", []int{3000}); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
