package sparkbox_test

// Transparency tests for the HTTP edge. The proxy exists to assert private-vs-
// public access, and beyond that it should be invisible: whatever a user runs in
// their sandbox — a websocket server, an SSE endpoint, a chunked upload handler,
// a big file — must behave the same through the edge as it does on localhost.
//
// These run against a real listener (httptest.Server) rather than a
// ResponseRecorder, because the behaviour under test — connection hijacking for
// 101 Switching Protocols, and flush timing — only exists on a real connection.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

// serveSandbox creates a sandbox named sub, starts handler on a loopback port
// standing in for a service inside the VM, points sub.hivemind.tools at it, and
// fronts the proxy with a real listener. It returns that listener's address.
func serveSandbox(t *testing.T, sub string, handler http.Handler) (edgeAddr string) {
	t.Helper()
	ps := newProxyStack(t)
	if _, err := ps.mgr.Create(context.Background(), sub, "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := &http.Server{Handler: handler}
	go backend.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { backend.Close() })

	if err := ps.store.Upsert(routes.Route{
		Subdomain: sub, Sandbox: sub, Owner: "alice",
		Port: ln.Addr().(*net.TCPAddr).Port,
	}); err != nil {
		t.Fatal(err)
	}
	edge := httptest.NewServer(ps.proxy)
	t.Cleanup(edge.Close)
	return strings.TrimPrefix(edge.URL, "http://")
}

// dialEdge opens a raw connection to the edge, so a test can drive the wire
// protocol directly (upgrade handshakes, deliberately malformed responses).
func dialEdge(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestProxyWebsocketUpgrade drives a real 101 handshake and then talks raw bytes
// in both directions over the upgraded connection. It uses a hand-rolled upgrade
// rather than a websocket library on purpose: what needs proving is that the
// edge hands the connection over intact and never touches the bytes again, which
// is protocol-agnostic — the same path carries websockets, and anything else
// that upgrades.
func TestProxyWebsocketUpgrade(t *testing.T) {
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		// The handshake response, then a byte-for-byte echo of whatever follows.
		fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		io.Copy(conn, conn) //nolint:errcheck
	})
	addr := serveSandbox(t, "wsvm", backend)

	c := dialEdge(t, addr)
	fmt.Fprint(c, "GET /socket HTTP/1.1\r\nHost: wsvm.hivemind.tools\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")

	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("expected 101 Switching Protocols, got %q", strings.TrimSpace(status))
	}
	for { // drain the handshake headers
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	// Bidirectional and framing-free: two separate writes must arrive as written,
	// with the second round-trip proving the tunnel stays open.
	for _, msg := range []string{"ping-one", "and-again"} {
		if _, err := io.WriteString(c, msg); err != nil {
			t.Fatalf("write %q: %v", msg, err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(br, got); err != nil {
			t.Fatalf("read echo of %q: %v", msg, err)
		}
		if string(got) != msg {
			t.Fatalf("echo mismatch: sent %q, got %q", msg, got)
		}
	}
}

// TestProxyStreamsWithoutBuffering asserts the edge forwards each write as it
// happens. The response deliberately carries a plain Content-Type and a known
// Content-Length — the case the stdlib's default flush policy does NOT stream
// eagerly — so a regression that drops FlushInterval fails here rather than
// silently stalling a user's log tail or token stream in production.
func TestProxyStreamsWithoutBuffering(t *testing.T) {
	const first, second = "first-chunk\n", "second-chunk\n"
	released := make(chan struct{}) // closed once the client has seen `first`

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprint(len(first)+len(second)))
		io.WriteString(w, first) //nolint:errcheck
		w.(http.Flusher).Flush()
		select {
		case <-released:
		case <-time.After(5 * time.Second):
			t.Error("client never saw the first chunk — response was buffered")
		}
		io.WriteString(w, second) //nolint:errcheck
	})
	addr := serveSandbox(t, "streamvm", backend)

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/stream", nil)
	req.Host = "streamvm.hivemind.tools"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got := make([]byte, len(first))
	if _, err := io.ReadFull(resp.Body, got); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if string(got) != first {
		t.Fatalf("first chunk = %q, want %q", got, first)
	}
	close(released)

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read second chunk: %v", err)
	}
	if string(rest) != second {
		t.Fatalf("second chunk = %q, want %q", rest, second)
	}
}

// TestProxyChunkedUpload covers the request direction: a body with no
// Content-Length (chunked) must reach the app whole. This is what a file upload
// or a streaming API client sends.
func TestProxyChunkedUpload(t *testing.T) {
	payload := bytes.Repeat([]byte("upload"), 200_000) // ~1.2 MB
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != -1 {
			t.Errorf("upstream saw Content-Length %d, want chunked (-1)", r.ContentLength)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		fmt.Fprintf(w, "%d", len(body))
	})
	addr := serveSandbox(t, "uploadvm", backend)

	// A pipe body has unknown length, so net/http sends it chunked.
	pr, pw := io.Pipe()
	go func() {
		pw.Write(payload) //nolint:errcheck
		pw.Close()
	}()
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/upload", pr)
	req.Host = "uploadvm.hivemind.tools"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if string(got) != fmt.Sprint(len(payload)) {
		t.Fatalf("upstream received %s bytes, sent %d", got, len(payload))
	}
}

// TestProxyLargeBodyIntegrity round-trips several megabytes of random bytes to
// catch any truncation or corruption the flush-every-write path might introduce.
func TestProxyLargeBodyIntegrity(t *testing.T) {
	payload := make([]byte, 8<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload) //nolint:errcheck
	})
	addr := serveSandbox(t, "bigvm", backend)

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/big", nil)
	req.Host = "bigvm.hivemind.tools"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("body corrupted: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestProxyMidStreamFailureIsNotAnErrorPage guards the ErrorHandler. Once bytes
// are on the wire the edge cannot retract them, so a failing upstream must not
// try to render a 502 over the top: the client would receive a truncated body
// with an HTML page glued onto the end, which looks like the app corrupting its
// own output. A short read is the honest outcome.
func TestProxyMidStreamFailureIsNotAnErrorPage(t *testing.T) {
	const partial = "half-a-resp"
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		// Promise 500 bytes, deliver a few, then vanish — a crashed app.
		fmt.Fprint(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 500\r\n\r\n")
		fmt.Fprint(conn, partial)
		conn.Close()
	})
	addr := serveSandbox(t, "crashvm", backend)

	req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	req.Host = "crashvm.hivemind.tools"
	req.Header.Set("Accept", "text/html") // the shape that would get an HTML page
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the upstream's own 200 to stand, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Errorf("expected a truncated-body error, got a clean read of %q", body)
	}
	if string(body) != partial {
		t.Fatalf("body = %q, want exactly the upstream's %q", body, partial)
	}
	if strings.Contains(string(body), "<html") || strings.Contains(string(body), "sparkbox") {
		t.Fatalf("an error page was appended to a live response: %q", body)
	}
}
