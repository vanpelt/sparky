package nodelink

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// Everything in this file runs over a REAL loopback SSH connection — a
// gliderlabs server standing in for the gateway, an x/crypto client standing in
// for the node, one control session and N data channels on the same TCP socket.
// A net.Pipe would prove nothing here: the properties under test are properties
// of x/crypto's multiplexer, and the one that matters most (its 16-deep,
// blocking incoming-channel queue) does not exist anywhere else.

// linkPair is one gateway and one node sharing a connection.
type linkPair struct {
	gw     *Conn     // the gateway's control channel
	node   *Conn     // the node's
	gwConn xssh.Conn // the gateway's handle for opening streams back

	nodeStdin  io.WriteCloser // raw access, for the framing tests
	gwServeErr chan error
	ndServeErr chan error

	srv *gssh.Server
}

type linkOptions struct {
	resolve   Resolver
	gwSetup   func(*Conn)
	nodeSetup func(*Conn)
}

func newLinkPair(t *testing.T, opts linkOptions) *linkPair {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if opts.resolve == nil {
		opts.resolve = func(string, string, int) (string, error) { return "", ErrUnknownSandbox }
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	p := &linkPair{
		gwServeErr: make(chan error, 1),
		ndServeErr: make(chan error, 1),
	}
	ready := make(chan struct{})

	srv := &gssh.Server{
		Handler: func(s gssh.Session) {
			conn, _ := s.Context().Value(gssh.ContextKeyConn).(xssh.Conn)
			gw := NewConn(s, s, "g", nil)
			if opts.gwSetup != nil {
				opts.gwSetup(gw)
			}
			p.gwConn = conn
			p.gw = gw
			close(ready)
			p.gwServeErr <- gw.Serve(s.Context())
		},
		PublicKeyHandler: func(gssh.Context, gssh.PublicKey) bool { return true },
	}
	srv.AddHostKey(testSigner(t))
	go srv.Serve(ln) //nolint:errcheck // returns on Close
	t.Cleanup(func() { srv.Close() })
	p.srv = srv

	client, err := xssh.Dial("tcp", ln.Addr().String(), &xssh.ClientConfig{
		User:            User,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(testSigner(t))},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	go ServeStreams(ctx, client.HandleChannelOpen(StreamChannel), opts.resolve, nil)

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	if err := sess.Start(LinkCommand); err != nil {
		t.Fatalf("start %s: %v", LinkCommand, err)
	}
	p.nodeStdin = stdin
	p.node = NewConn(stdout, stdin, "n", nil)
	if opts.nodeSetup != nil {
		opts.nodeSetup(p.node)
	}
	go func() { p.ndServeErr <- p.node.Serve(ctx) }()

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway session never opened")
	}
	return p
}

func testSigner(t *testing.T) xssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	s, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s
}

// echoServer is a stand-in for whatever is listening inside a guest.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c) //nolint:errcheck // the test asserts on byte counts
			}()
		}
	}()
	return ln.Addr().String()
}

// staticResolver maps sandbox names to addresses the way a node's manager does,
// and refuses everything else exactly as ServeStreams' table expects.
func staticResolver(m map[string]string) Resolver {
	return func(sandbox, _ string, _ int) (string, error) {
		addr, ok := m[sandbox]
		if !ok {
			return "", ErrUnknownSandbox
		}
		return addr, nil
	}
}

// pingHandler answers the gateway's liveness probe by echoing the nonce.
func pingHandler(c *Conn) {
	c.Handle(TypePing, func(_ context.Context, body json.RawMessage) (any, error) {
		var req PingReq
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		return req, nil
	})
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { return len(p), nil }

// TestControlChannelSurvivesBulkStreams is the head-of-line-blocking proof, in
// the direction bulk transfer threatens: the control channel and every data
// channel share one TCP socket and one multiplexer, so a design that let bulk
// bytes queue ahead of a frame would make the control plane unusable exactly
// when a sandbox is busiest.
func TestControlChannelSurvivesBulkStreams(t *testing.T) {
	const (
		streams = 3
		payload = 8 << 20
		// Rounds only stretch the load out far enough to take a useful number of
		// latency samples; loopback moves 24 MiB in a fraction of a second.
		rounds = 4
	)
	target := echoServer(t)
	p := newLinkPair(t, linkOptions{
		resolve:   staticResolver(map[string]string{"echo": target}),
		nodeSetup: pingHandler,
	})

	// Bulk failures are collected rather than reported from their goroutine: the
	// ping loop can end this test at any moment, and a t.Errorf arriving after
	// that panics and hides whatever actually went wrong.
	bulkErrs := make(chan error, streams*rounds)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				conn, err := OpenStream(p.gwConn, StreamOpen{
					Sandbox: "echo", Kind: StreamTCP, Port: 8080, Nonce: fmt.Sprintf("bulk%d-%d", i, r)})
				if err != nil {
					bulkErrs <- fmt.Errorf("stream %d: open: %w", i, err)
					return
				}
				go func() {
					io.Copy(conn, io.LimitReader(zeroReader{}, payload)) //nolint:errcheck // n is asserted on the read side
					conn.(*streamConn).CloseWrite()                      //nolint:errcheck
				}()
				n, err := io.Copy(io.Discard, conn)
				switch {
				case err != nil:
					bulkErrs <- fmt.Errorf("stream %d: read back: %w", i, err)
				case n != payload:
					bulkErrs <- fmt.Errorf("stream %d: echoed %d bytes, want %d", i, n, payload)
				}
				conn.Close()
			}
		}(i)
	}
	go func() { wg.Wait(); close(done) }()

	var samples []time.Duration
	pings := 0
	for {
		select {
		case <-done:
			close(bulkErrs)
			for err := range bulkErrs {
				t.Error(err)
			}
			if pings < 5 {
				t.Fatalf("only %d pings landed during the transfer; the test proved nothing", pings)
			}
			// Judged on the distribution, because that is what the claim is
			// about. A control channel queueing behind 24 MiB is slow for as
			// long as there are bytes to drain, so it fails nearly every
			// sample; one slow sample among fifty is a busy machine, which is
			// what a shared CI runner running -race is. Asserting on the worst
			// single round trip conflated the two and failed on a 247ms first
			// ping — the one that pays for stream setup and a cold runtime —
			// while the other 52 were comfortably inside the bound.
			slices.Sort(samples)
			p90 := samples[len(samples)*9/10]
			worst := samples[len(samples)-1]
			t.Logf("%d pings during %d x %d x %d MiB, p90 %v, worst %v",
				pings, streams, rounds, payload>>20, p90, worst)
			if p90 > 200*time.Millisecond {
				t.Errorf("p90 round trip %v under load; the control channel is queueing behind bulk data", p90)
			}
			// A percentile alone would hide a real stall that happened to fit
			// inside the tail, so the tail keeps a bound of its own — loose
			// enough that scheduling noise cannot reach it, tight enough that
			// anything actually waiting on bulk data does.
			if worst > time.Second {
				t.Errorf("worst round trip %v under load; that is a stall, not jitter", worst)
			}
			return
		// Sampled far faster than the 200ms latency bound being asserted, so the
		// number of samples is set by this interval rather than by how quickly
		// the host happens to move 24 MiB over loopback. At 100ms a warm machine
		// finished the transfer in ~560ms and took only 4 samples, failing the
		// floor below on speed alone.
		case <-time.After(20 * time.Millisecond):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		sent := time.Now()
		var out PingReq
		err := p.gw.Request(ctx, TypePing, PingReq{Nonce: fmt.Sprintf("p%d", pings)}, &out)
		took := time.Since(sent)
		cancel()
		if err != nil {
			t.Fatalf("ping %d during bulk transfer: %v", pings, err)
		}
		if out.Nonce != fmt.Sprintf("p%d", pings) {
			t.Fatalf("ping %d: nonce %q came back", pings, out.Nonce)
		}
		samples = append(samples, took)
		pings++
	}
}

// TestConcurrentOpensDoNotStallTheLink is the other half of the proof, and the
// one that would fail if ServeStreams ever did work inline. x/crypto's mux
// hands an incoming channel over with a blocking send into a 16-deep queue from
// its own read loop, so a slow inline accept stops the node reading the socket
// at all — and the observable damage is on gateway->node requests, not on the
// node's own heartbeats, which are written from an unrelated goroutine.
func TestConcurrentOpensDoNotStallTheLink(t *testing.T) {
	const opens = 40
	target := echoServer(t)
	slow := func(sandbox, _ string, _ int) (string, error) {
		// Stands in for a manager lookup plus a guest that is slow to accept.
		time.Sleep(50 * time.Millisecond)
		if sandbox != "echo" {
			return "", ErrUnknownSandbox
		}
		return target, nil
	}
	p := newLinkPair(t, linkOptions{resolve: slow, nodeSetup: pingHandler})

	burstErrs := make(chan error, opens)
	done := make(chan struct{})
	begun := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < opens; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := OpenStream(p.gwConn, StreamOpen{
				Sandbox: "echo", Kind: StreamTCP, Port: 80, Nonce: fmt.Sprintf("burst%d", i)})
			if err != nil {
				burstErrs <- fmt.Errorf("open %d: %w", i, err)
				return
			}
			defer conn.Close()
			if _, err := conn.Write([]byte("hi")); err != nil {
				burstErrs <- fmt.Errorf("open %d: write: %w", i, err)
				return
			}
			buf := make([]byte, 2)
			if _, err := io.ReadFull(conn, buf); err != nil {
				burstErrs <- fmt.Errorf("open %d: read: %w", i, err)
			}
		}(i)
	}
	go func() { wg.Wait(); close(done) }()

	pings := 0
	for {
		select {
		case <-done:
			close(burstErrs)
			for err := range burstErrs {
				t.Error(err)
			}
			// Served concurrently the burst costs about one resolve; served
			// inline it would cost forty, one after another.
			if took := time.Since(begun); took > time.Second {
				t.Errorf("%d opens took %v; they are being served one at a time", opens, took)
			}
			if pings < 3 {
				t.Fatalf("only %d pings landed during %d concurrent opens", pings, opens)
			}
			return
		case <-time.After(5 * time.Millisecond):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		sent := time.Now()
		err := p.gw.Request(ctx, TypePing, PingReq{Nonce: "burst"}, nil)
		took := time.Since(sent)
		cancel()
		if err != nil {
			t.Fatalf("ping during %d concurrent opens: %v", opens, err)
		}
		if took > 500*time.Millisecond {
			t.Errorf("ping took %v while %d channels were opening; the accept loop is doing work inline", took, opens)
		}
		pings++
	}
}

// TestHTTPOverStream is the proxy's path: an http.Transport whose DialContext
// is a channel open, against a server that only exists on the node side.
func TestHTTPOverStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("no hijacker")
				return
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer conn.Close()
			fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: sparkbox\r\nConnection: Upgrade\r\n\r\n")
			io.Copy(conn, buf) //nolint:errcheck // the client asserts on what comes back
			return
		}
		fmt.Fprint(w, "hello from the guest")
	}))
	t.Cleanup(srv.Close)

	p := newLinkPair(t, linkOptions{resolve: staticResolver(map[string]string{
		"web": strings.TrimPrefix(srv.URL, "http://")})})

	tr := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return OpenStream(p.gwConn, StreamOpen{Sandbox: "web", Kind: StreamTCP, Port: 80})
		},
	}
	t.Cleanup(tr.CloseIdleConnections)

	resp, err := (&http.Client{Transport: tr}).Get("http://web.node.sandbox.invalid/")
	if err != nil {
		t.Fatalf("GET over stream: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "hello from the guest" {
		t.Fatalf("GET = %d %q", resp.StatusCode, body)
	}

	// The 101 path is why the proxy's transport must stay a concrete
	// *http.Transport: only that returns a body a caller can write back into.
	req, _ := http.NewRequest(http.MethodGet, "http://web.node.sandbox.invalid/ws", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "sparkbox")
	up, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("upgrade over stream: %v", err)
	}
	defer up.Body.Close()
	if up.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade status = %d, want 101", up.StatusCode)
	}
	rwc, ok := up.Body.(io.ReadWriteCloser)
	if !ok {
		t.Fatal("101 body is not writable")
	}
	if _, err := rwc.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write after upgrade: %v", err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(rwc, got); err != nil {
		t.Fatalf("read after upgrade: %v", err)
	}
	if string(got) != "ping\n" {
		t.Fatalf("after upgrade got %q", got)
	}
}

// TestSSHOverStream runs the gateway's own upstream dial through a stream and
// out into a mock guest, which is the shape `ssh <box>@gateway` takes once the
// box is on another machine.
func TestSSHOverStream(t *testing.T) {
	upstream := testSigner(t)
	driver := mock.New(t.TempDir(), testSigner(t))
	t.Cleanup(func() { driver.Close() })

	inst, err := driver.Create(context.Background(), vmm.Config{
		Name:             "demo",
		MemMB:            256,
		GatewayPublicKey: string(xssh.MarshalAuthorizedKey(upstream.PublicKey())),
	})
	if err != nil {
		t.Fatalf("create mock vm: %v", err)
	}

	// The guest's sshd is on an ephemeral loopback port here and on 172.30.x.2:22
	// under firecracker, which is exactly why a stream names a sandbox and lets
	// the node resolve the address.
	p := newLinkPair(t, linkOptions{resolve: func(sandbox, kind string, _ int) (string, error) {
		if sandbox != "demo" || kind != StreamSSH {
			return "", ErrUnknownSandbox
		}
		return inst.SSHAddr, nil
	}})

	conn, err := OpenStream(p.gwConn, StreamOpen{Sandbox: "demo", Kind: StreamSSH})
	if err != nil {
		t.Fatalf("open ssh stream: %v", err)
	}
	cc, chans, reqs, err := xssh.NewClientConn(conn, "demo", &xssh.ClientConfig{
		User:            inst.SSHUser,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(upstream)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("ssh handshake through the stream: %v", err)
	}
	guest := xssh.NewClient(cc, chans, reqs)
	defer guest.Close()

	sess, err := guest.NewSession()
	if err != nil {
		t.Fatalf("guest session: %v", err)
	}
	defer sess.Close()
	out, err := sess.Output("echo tunneled")
	if err != nil {
		t.Fatalf("guest command: %v", err)
	}
	if strings.TrimSpace(string(out)) != "tunneled" {
		t.Fatalf("guest said %q", out)
	}
}

// TestSetDeadlineUnblocksRead pins the close-on-expiry contract. x/crypto's own
// tunneled conn refuses both setters, which would leave every caller that arms
// one around a handshake with no bound at all.
func TestSetDeadlineUnblocksRead(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	// Accept and say nothing, ever — the peer that stalls after connecting.
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		<-make(chan struct{})
		c.Close()
	}()

	p := newLinkPair(t, linkOptions{resolve: staticResolver(map[string]string{"mute": ln.Addr().String()})})
	conn, err := OpenStream(p.gwConn, StreamOpen{Sandbox: "mute", Kind: StreamTCP, Port: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	read := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		read <- err
	}()
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("Read returned no error after its deadline")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Read never returned after its deadline expired")
	}
}

// TestPooledHTTPSetsNoDeadline pins the assumption close-on-expiry rests on: a
// pooled connection outlives the request that dialed it, and an expired
// close-on-expiry deadline is not recoverable, so net/http's client transport
// must never set one. It does not today; this fails the day it starts.
func TestPooledHTTPSetsNoDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)

	p := newLinkPair(t, linkOptions{resolve: staticResolver(map[string]string{
		"web": strings.TrimPrefix(srv.URL, "http://")})})

	var (
		mu    sync.Mutex
		dials int
		conns []*streamConn
	)
	tr := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			c, err := OpenStream(p.gwConn, StreamOpen{Sandbox: "web", Kind: StreamTCP, Port: 80})
			if err != nil {
				return nil, err
			}
			mu.Lock()
			dials++
			conns = append(conns, c.(*streamConn))
			mu.Unlock()
			return c, nil
		},
	}
	t.Cleanup(tr.CloseIdleConnections)

	client := &http.Client{Transport: tr}
	for i := 0; i < 20; i++ {
		resp, err := client.Get("http://web.node.sandbox.invalid/")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // drained so the conn returns to the pool
		resp.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if dials != 1 {
		t.Fatalf("20 keep-alive requests took %d dials, want 1", dials)
	}
	if n := conns[0].deadlineCalls.Load(); n != 0 {
		t.Fatalf("net/http set %d deadlines on a pooled stream; close-on-expiry is no longer safe", n)
	}
}

// TestStreamRejectionsAreTyped: the reject reason is the only thing that lets
// the edge tell "that machine is confused about placement" (503) from "nothing
// is listening in the guest" (502), so each refusal must arrive as a typed
// *xssh.OpenChannelError rather than a generic failure.
func TestStreamRejectionsAreTyped(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := dead.Addr().String()
	dead.Close() // nothing is listening there now

	p := newLinkPair(t, linkOptions{resolve: func(sandbox, _ string, _ int) (string, error) {
		switch sandbox {
		case "paused":
			return "", ErrNotRunning
		case "noaddr":
			return "", nil
		case "refused":
			return deadAddr, nil
		}
		return "", ErrUnknownSandbox
	}})

	for _, tc := range []struct {
		sandbox string
		reason  xssh.RejectionReason
		msg     string
	}{
		{"ghost", xssh.Prohibited, "unknown sandbox"},
		{"paused", xssh.Prohibited, "sandbox not running"},
		{"noaddr", xssh.Prohibited, "sandbox not running"},
		{"refused", xssh.ConnectionFailed, ""},
	} {
		t.Run(tc.sandbox, func(t *testing.T) {
			_, err := OpenStream(p.gwConn, StreamOpen{Sandbox: tc.sandbox, Kind: StreamTCP, Port: 80})
			var oce *xssh.OpenChannelError
			if !errors.As(err, &oce) {
				t.Fatalf("err = %v (%T), want *ssh.OpenChannelError", err, err)
			}
			if oce.Reason != tc.reason {
				t.Errorf("reason = %v, want %v", oce.Reason, tc.reason)
			}
			if tc.msg != "" && oce.Message != tc.msg {
				t.Errorf("message = %q, want %q", oce.Message, tc.msg)
			}
			wantRetry := tc.sandbox == "paused" || tc.sandbox == "noaddr"
			if got := IsNotRunning(err); got != wantRetry {
				t.Errorf("IsNotRunning = %v, want %v", got, wantRetry)
			}
		})
	}
}

// TestStreamAddrNamesTheSandbox: a tunneled connection has no host address
// worth printing, and printing the SSH transport's would name the wrong machine.
func TestStreamAddrNamesTheSandbox(t *testing.T) {
	target := echoServer(t)
	p := newLinkPair(t, linkOptions{resolve: staticResolver(map[string]string{"box": target})})
	for _, tc := range []struct {
		req  StreamOpen
		want string
	}{
		{StreamOpen{Sandbox: "box", Kind: StreamTCP, Port: 3000}, "box:3000"},
		{StreamOpen{Sandbox: "box", Kind: StreamSSH}, "box:ssh"},
	} {
		conn, err := OpenStream(p.gwConn, tc.req)
		if err != nil {
			t.Fatalf("open %v: %v", tc.req, err)
		}
		if got := conn.RemoteAddr().String(); got != tc.want {
			t.Errorf("RemoteAddr = %q, want %q", got, tc.want)
		}
		if got := conn.RemoteAddr().Network(); got != "sandbox" {
			t.Errorf("Network = %q, want %q", got, "sandbox")
		}
		conn.Close()
	}
}
