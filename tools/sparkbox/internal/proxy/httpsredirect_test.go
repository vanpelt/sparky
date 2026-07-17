package proxy

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// testTLSConfig returns a self-signed cert good for the duration of a test.
func testTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"turbo-gecko.hivemind.tools"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

// TestRedirectPlainHTTP proves the wrapped listener answers a cleartext request
// with a 308 to the https:// URL (same host, port, path, and query) while a real
// TLS handshake through the same listener still reaches the server.
func TestRedirectPlainHTTP(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := RedirectPlainHTTP(raw, slog.New(slog.NewTextHandler(io.Discard, nil)))

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "hello-tls")
		}),
		TLSConfig: testTLSConfig(t),
	}
	go srv.ServeTLS(ln, "", "") //nolint:errcheck
	t.Cleanup(func() { srv.Close() })

	addr := raw.Addr().String()

	t.Run("plaintext redirects to https", func(t *testing.T) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		io.WriteString(c, "GET /foo?x=1 HTTP/1.1\r\nHost: turbo-gecko.hivemind.tools:4444\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusPermanentRedirect {
			t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusPermanentRedirect)
		}
		if got, want := resp.Header.Get("Location"), "https://turbo-gecko.hivemind.tools:4444/foo?x=1"; got != want {
			t.Fatalf("Location = %q, want %q", got, want)
		}
	})

	t.Run("tls still passes through", func(t *testing.T) {
		c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		io.WriteString(c, "GET / HTTP/1.1\r\nHost: turbo-gecko.hivemind.tools\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), "hello-tls") {
			t.Fatalf("body = %q, want it to contain hello-tls", body)
		}
	})
}
