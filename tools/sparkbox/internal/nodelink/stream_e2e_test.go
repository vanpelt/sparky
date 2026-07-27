package nodelink

// The data plane, end to end, against a real manager on the node side.
//
// stream_test.go proves the multiplexer's properties with a hand-written
// resolver. This file proves the thing W2 shipped without: that a node resolves
// a stream from its OWN records, that the gateway can drive a guest through the
// result, and that a link which ends says so to everything riding on it.
//
// Everything here runs over a real loopback SSH connection with a real
// *host.Manager and the mock VMM driver, which boots a guest sshd on an
// ephemeral 127.0.0.1 port. That the port is ephemeral is the point: it is
// knowledge only the node has, so a test that passed against a hardcoded
// address would prove nothing about the rule this file exists to enforce.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// dataLink is a gateway and a node that really are two halves of one link: a
// *Client on one end and a manager full of guests on the other.
type dataLink struct {
	client *Client
	mgr    *host.Manager
	// nodeConn is exposed only to same-package integration benchmarks that add a
	// narrowly scoped control handler to the real-SSH fixture.
	nodeConn *Conn
	// guestKey is the gateway's upstream key. The mock driver makes it both the
	// guests' host key and the only key they accept, which is the same
	// arrangement a real deployment has.
	guestKey xssh.Signer
	// relay is the splice the node dialed through, when the test asked for one.
	relay *relay
}

// dataLinkOption tunes the rig for lifecycle faults, fixed-address netem, and
// optional gateway-side transport metrics.
type dataLinkOption func(*dataLinkOptions)

type dataLinkOptions struct {
	relayed     bool
	gatewayAddr string
	metrics     *fleetmetrics.Registry
}

// throughARelay puts a TCP splice between the node and the gateway that a test
// can black-hole. See relay.
func throughARelay() dataLinkOption { return func(o *dataLinkOptions) { o.relayed = true } }

func atGatewayAddr(addr string) dataLinkOption {
	return func(o *dataLinkOptions) { o.gatewayAddr = addr }
}

func withDataLinkMetrics(metrics *fleetmetrics.Registry) dataLinkOption {
	return func(o *dataLinkOptions) { o.metrics = metrics }
}

// relay is a TCP splice with a switch.
//
// Flipping drop makes it keep reading and ACKing in both directions and discard
// every byte. That is a BLACK HOLE and not a disconnection, and the difference
// is the whole point: no FIN and no reset means neither kernel ever learns the
// peer is gone, no retransmission timeout fires, and x/crypto's mux goes on
// reading a socket that will never answer. It is the shape a wifi or tailnet
// partition has — the expected way a node fails — and it is the one shape in
// which nothing in x/crypto ends a channel by itself.
type relay struct {
	addr string
	drop atomic.Bool

	mu    sync.Mutex
	conns []net.Conn
}

func newRelay(t *testing.T, upstream string) *relay {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("relay listen: %v", err)
	}
	r := &relay{addr: ln.Addr().String()}
	// One cleanup owning every connection, rather than a t.Cleanup per accept:
	// registering a cleanup from a goroutine that outlives the test is itself a
	// panic.
	t.Cleanup(func() {
		ln.Close()
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, c := range r.conns {
			c.Close()
		}
	})
	go func() {
		for {
			down, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", upstream)
			if err != nil {
				down.Close()
				return
			}
			r.mu.Lock()
			r.conns = append(r.conns, down, up)
			r.mu.Unlock()
			go r.splice(up, down)
			go r.splice(down, up)
		}
	}()
	return r
}

// splice copies one direction, dropping instead of forwarding once the switch
// is flipped. It keeps READING either way, because a relay that stopped reading
// would fill the sender's window and turn the black hole into a stall the
// sender can observe.
func (r *relay) splice(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !r.drop.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func newDataLink(t *testing.T, opts ...dataLinkOption) *dataLink {
	t.Helper()
	var cfg dataLinkOptions
	for _, o := range opts {
		o(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dl := &dataLink{guestKey: testSigner(t)}

	dir := t.TempDir()
	driver := mock.New(dir, dl.guestKey)
	t.Cleanup(func() { driver.Close() })
	mgr, err := host.NewManager(host.Options{
		StateDir:         dir,
		Driver:           driver,
		GatewayPublicKey: string(xssh.MarshalAuthorizedKey(dl.guestKey.PublicKey())),
		Logger:           slog.New(slog.DiscardHandler),
		NodeName:         "node-b",
	})
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	dl.mgr = mgr

	listenAddr := cfg.gatewayAddr
	if listenAddr == "" {
		listenAddr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ready := make(chan struct{})
	srv := &gssh.Server{
		Handler: func(s gssh.Session) {
			conn, _ := s.Context().Value(gssh.ContextKeyConn).(xssh.Conn)
			g, err := ReadHello(s.Context(), s, HelloTimeout)
			if err != nil {
				return
			}
			c, wait, err := Serve(s.Context(), ServerOptions{
				Node: "node-b", Greeting: g, Session: s, Conn: conn,
				// Far longer than any test here: the liveness prober must never
				// be the reason a link in this file ended.
				PingEvery: time.Hour,
				Log:       slog.New(slog.DiscardHandler),
				Metrics:   cfg.metrics,
			})
			if err != nil {
				return
			}
			dl.client = c
			close(ready)
			_ = wait()
		},
		PublicKeyHandler: func(gssh.Context, gssh.PublicKey) bool { return true },
	}
	srv.AddHostKey(testSigner(t))
	go srv.Serve(ln) //nolint:errcheck // returns on Close
	t.Cleanup(func() { srv.Close() })

	gatewayAddr := ln.Addr().String()
	if cfg.relayed {
		dl.relay = newRelay(t, gatewayAddr)
		gatewayAddr = dl.relay.addr
	}

	nodeSSH, err := xssh.Dial("tcp", gatewayAddr, &xssh.ClientConfig{
		User:            User,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(testSigner(t))},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatalf("node dial: %v", err)
	}
	t.Cleanup(func() { nodeSSH.Close() })

	// The accept loop starts before the handshake, for the reason runOnce does
	// the same: an unserved channel queue blocks x/crypto's mux and with it
	// every frame on this connection.
	go ServeStreams(ctx, nodeSSH.HandleChannelOpen(StreamChannel), StreamResolver(mgr), nil)

	sess, err := nodeSSH.NewSession()
	if err != nil {
		t.Fatalf("node session: %v", err)
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
	body, err := marshalBody(Hello{Protocol: Protocol, Node: "node-b", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if err := newEncoder(stdin).encode(&Frame{ID: "n1", Type: TypeHello, Body: body}); err != nil {
		t.Fatalf("hello: %v", err)
	}
	// Started after the hello is on the wire, so the two writers to stdin never
	// overlap. It reads the welcome as a reply to a request it never made and
	// drops it, which is the same shortcut remote_test.go takes: the handshake's
	// own behaviour belongs to the link tests.
	nodeConn := NewConn(stdout, stdin, "n", nil)
	dl.nodeConn = nodeConn
	pingHandler(nodeConn)
	go nodeConn.Serve(ctx) //nolint:errcheck // ends with the link

	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway never served the link")
	}
	return dl
}

// running builds a sandbox on the node and returns its record.
func (dl *dataLink) running(t *testing.T, name string) *host.Sandbox {
	t.Helper()
	b, err := dl.mgr.Create(context.Background(), name, "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if b.State != vmm.StateRunning {
		t.Fatalf("%s came up %s", name, b.State)
	}
	return b
}

// TestAGuestIsReachableThroughTheLink is W21's headline: the gateway names a
// sandbox, the node turns that into an address only it knows, and a real SSH
// client handshakes over the result and runs a command inside the guest.
func TestAGuestIsReachableThroughTheLink(t *testing.T) {
	dl := newDataLink(t)
	box := dl.running(t, "demo")

	conn, err := dl.client.DialSandbox(context.Background(), "demo", StreamSSH, 0)
	if err != nil {
		t.Fatalf("DialSandbox: %v", err)
	}
	defer conn.Close()

	cc, chans, reqs, err := xssh.NewClientConn(conn, "demo.node-b.sandbox.invalid:ssh", &xssh.ClientConfig{
		User:            box.SSHUser,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(dl.guestKey)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("handshake with the guest: %v", err)
	}
	guest := xssh.NewClient(cc, chans, reqs)
	defer guest.Close()

	sess, err := guest.NewSession()
	if err != nil {
		t.Fatalf("guest session: %v", err)
	}
	defer sess.Close()
	out, err := sess.Output("echo hi")
	if err != nil {
		t.Fatalf("echo hi: %v", err)
	}
	if got := string(bytes.TrimSpace(out)); got != "hi" {
		t.Fatalf("the guest said %q, want %q", got, "hi")
	}
}

// TestAGuestPortIsReachableThroughTheLink is the other kind of stream: a TCP
// port a user started something on, which is what every proxied web route is.
func TestAGuestPortIsReachableThroughTheLink(t *testing.T) {
	dl := newDataLink(t)
	dl.running(t, "web")

	// The mock driver gives its guests HostIP 127.0.0.1, so a listener here is a
	// listener "inside" that guest as far as the resolver is concerned.
	addr := echoServer(t)
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := dl.client.DialSandbox(context.Background(), "web", StreamTCP, port)
	if err != nil {
		t.Fatalf("DialSandbox: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echoed %q", buf)
	}
}

// TestTheNodeRefusesWhatItCannotServe walks the rows of §2.6's reject table
// that the resolver owns, over a real link, and checks each arrives as the
// typed carrier the edge's error pages branch on.
//
// The distinction between the two Prohibited messages is not cosmetic: "unknown
// sandbox" means the gateway authorized a request for a sandbox this machine
// has never held, which is a control-plane fault it logs at Error, while
// "sandbox not running" is an ordinary state a user can act on.
func TestTheNodeRefusesWhatItCannotServe(t *testing.T) {
	dl := newDataLink(t)
	dl.running(t, "demo")
	if err := dl.mgr.Pause(context.Background(), "demo"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	dl.running(t, "up")

	cases := []struct {
		name    string
		sandbox string
		kind    string
		port    int
		reason  xssh.RejectionReason
		message string
	}{
		{
			name:    "a sandbox this machine has never held",
			sandbox: "ghost", kind: StreamSSH,
			reason: xssh.Prohibited, message: "unknown sandbox",
		},
		{
			name:    "a sandbox that is paused",
			sandbox: "demo", kind: StreamSSH,
			reason: xssh.Prohibited, message: "sandbox not running",
		},
		{
			name:    "a kind this node does not serve",
			sandbox: "up", kind: "quic",
			reason: xssh.Prohibited, message: "sandbox not running",
		},
		{
			// Nothing is listening on this port inside the guest. This is the
			// one that must NOT be Prohibited: it is the app's problem, and the
			// edge renders it as the existing "nothing is listening" page.
			name:    "a port with nothing behind it",
			sandbox: "up", kind: StreamTCP, port: 1,
			reason:  xssh.ConnectionFailed,
			message: "nothing accepted a connection on that port in the sandbox",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := dl.client.DialSandbox(context.Background(), tc.sandbox, tc.kind, tc.port)
			if err == nil {
				conn.Close()
				t.Fatal("the node accepted a stream it should have refused")
			}
			var refused *xssh.OpenChannelError
			if !errors.As(err, &refused) {
				t.Fatalf("refusal = %v (%T), want an *ssh.OpenChannelError the edge can classify", err, err)
			}
			if refused.Reason != tc.reason {
				t.Errorf("reason = %v, want %v", refused.Reason, tc.reason)
			}
			if tc.message != "" && refused.Message != tc.message {
				t.Errorf("message = %q, want %q", refused.Message, tc.message)
			}
			// No refusal may carry a guest address. The message is the only
			// free-form string a node puts on this wire, and it travels from
			// here into a user's terminal and a public error page — while the
			// address it would name (172.30.<idx>.2 on a real node, an
			// ephemeral loopback port here) means something else entirely on
			// the gateway, where every machine mints the same guest IPs.
			for _, bad := range []string{"127.0.0.1", "dial tcp", "172.30."} {
				if strings.Contains(refused.Message, bad) {
					t.Errorf("the node's refusal carries %q: %q", bad, refused.Message)
				}
			}
		})
	}
}

// TestTheNodeResolvesFromItsOwnRecord is the rule the wire format exists to
// make enforceable, stated as a test: the gateway supplies no address, so a
// sandbox that has moved on since the gateway last looked cannot be reached at
// the address it used to have.
//
// The proof is that the same request answered a moment ago now fails, with
// nothing having changed but the node's own record.
func TestTheNodeResolvesFromItsOwnRecord(t *testing.T) {
	dl := newDataLink(t)
	dl.running(t, "demo")

	conn, err := dl.client.DialSandbox(context.Background(), "demo", StreamSSH, 0)
	if err != nil {
		t.Fatalf("DialSandbox while running: %v", err)
	}
	conn.Close()

	if err := dl.mgr.Pause(context.Background(), "demo"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	conn, err = dl.client.DialSandbox(context.Background(), "demo", StreamSSH, 0)
	if err == nil {
		conn.Close()
		t.Fatal("a paused sandbox was still reachable at the address it used to have")
	}
	var refused *xssh.OpenChannelError
	if !errors.As(err, &refused) || refused.Message != "sandbox not running" {
		t.Fatalf("refusal = %v, want the not-running answer", err)
	}
}

// TestManyStreamsWhileTheControlChannelKeepsWorking is W21's concurrency
// criterion. stream_test.go proves the mux does not stall; this proves it does
// not stall when the resolver is a real manager taking a real lock on every
// open, which is the version that ships.
func TestManyStreamsWhileTheControlChannelKeepsWorking(t *testing.T) {
	const streams = 50
	dl := newDataLink(t)
	dl.running(t, "web")
	addr := echoServer(t)
	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	// Every stream is opened, exercised and then HELD until the control-plane
	// assertions below have run. Letting them finish as they please raced the
	// thing under test: on a warm machine all fifty were done inside a couple of
	// milliseconds and the ping loop sampled a link that was already idle.
	errs := make(chan error, streams)
	up := make(chan struct{}, streams)
	release := make(chan struct{})
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := dl.client.DialSandbox(context.Background(), "web", StreamTCP, port)
			if err != nil {
				errs <- fmt.Errorf("stream %d: %w", i, err)
				up <- struct{}{}
				return
			}
			defer conn.Close()
			want := fmt.Sprintf("s%03d", i)
			if _, err := conn.Write([]byte(want)); err != nil {
				errs <- fmt.Errorf("stream %d: write: %w", i, err)
				up <- struct{}{}
				return
			}
			buf := make([]byte, len(want))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errs <- fmt.Errorf("stream %d: read: %w", i, err)
				up <- struct{}{}
				return
			}
			if string(buf) != want {
				errs <- fmt.Errorf("stream %d: echoed %q", i, buf)
			}
			up <- struct{}{}
			<-release
		}(i)
	}
	go func() { wg.Wait(); close(done) }()

	for i := 0; i < streams; i++ {
		select {
		case <-up:
		case <-time.After(30 * time.Second):
			t.Fatalf("only %d of %d streams came up; the link stalled", i, streams)
		}
	}
	if n := dl.client.LiveStreams(); n != streams {
		t.Fatalf("the link is carrying %d streams, want all %d live at once", n, streams)
	}

	// Now, with all of them live, the control channel must still work.
	const pingsWanted = 5
	pings := 0
	for pings < pingsWanted {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var echo PingReq
		want := PingReq{Nonce: fmt.Sprintf("p%d", pings)}
		err := dl.client.Do(ctx, TypePing, want, &echo)
		cancel()
		if err != nil {
			t.Fatalf("control round trip %d during %d streams: %v", pings, streams, err)
		}
		if echo.Nonce != want.Nonce {
			t.Fatalf("control round trip %d: nonce %q came back", pings, echo.Nonce)
		}
		pings++
	}

	close(release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the streams never finished")
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestALinkThatEndsFailsItsStreamsInWords is the lifecycle half of W21.
//
// The failure mode it exists to prevent is the quiet one. A stream whose link
// has gone and which reports a clean io.EOF is indistinguishable from a guest
// that finished talking: the proxy relays a truncated 200, the browser renders
// half a page, and no line is written anywhere. Worse, on a black-holed link —
// no FIN, no reset — nothing in x/crypto ends the channel at all and the read
// never returns. Both are answered the same way: whoever ends the link ends its
// streams, and states why.
//
// The read below is genuinely blocked when the link is hung up, which is what
// makes this deterministic rather than a race: the only thing that can wake it
// is the teardown under test.
func TestALinkThatEndsFailsItsStreamsInWords(t *testing.T) {
	dl := newDataLink(t)
	dl.running(t, "web")
	port, portStr := silentGuestPort(t)

	conn, err := dl.client.DialSandbox(context.Background(), "web", StreamTCP, port)
	if err != nil {
		t.Fatalf("DialSandbox: %v", err)
	}
	if n := dl.client.LiveStreams(); n != 1 {
		t.Fatalf("the link is carrying %d streams, want 1", n)
	}

	read := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := conn.Read(buf)
		read <- err
	}()
	// Give the read a moment to park. If it has not parked yet the assertion
	// still holds — fail records the reason before it closes anything — so this
	// is about testing what is claimed, not about making the test pass.
	time.Sleep(20 * time.Millisecond)

	dl.client.Hangup(CodeProtocolError, "the test ended this link")

	select {
	case err := <-read:
		if err == nil {
			t.Fatal("the read succeeded after the link ended")
		}
		if errors.Is(err, io.EOF) {
			t.Fatalf("the read reported %v, which a caller cannot tell from the guest hanging up", err)
		}
		if !errors.Is(err, ErrLinkClosed) {
			t.Fatalf("the read reported %v (%T), want it to name the link", err, err)
		}
		var op *net.OpError
		if !errors.As(err, &op) {
			t.Errorf("the read reported %T, want the *net.OpError shape every caller here already handles", err)
		} else if op.Addr == nil || op.Addr.String() != "web:"+portStr {
			t.Errorf("the failure names %v, want the stream it happened on", op.Addr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the read never returned; the link ended and left a half-open stream behind")
	}

	if n := dl.client.LiveStreams(); n != 0 {
		t.Errorf("the link is still carrying %d streams after it ended", n)
	}
	// And nothing new may be opened on it, rather than being handed a stream
	// nobody will ever tear down.
	if c, err := dl.client.DialSandbox(context.Background(), "web", StreamTCP, port); err == nil {
		c.Close()
		t.Error("a dead link opened a new stream")
	}
}

// silentGuestPort starts a listener that accepts and then says nothing, and
// returns the port twice — as a number to dial and as the string the stream's
// address is rendered with. A reader on a stream to it is parked in Read with
// no way out but the teardown under test, which is what makes the two lifecycle
// tests deterministic rather than racy.
func silentGuestPort(t *testing.T) (int, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu   sync.Mutex
		held []net.Conn
	)
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return port, portStr
}

// TestABlackHoledLinkStillFailsItsStreams is the case the registry was built
// for, and the one the test above cannot reach.
//
// TestALinkThatEndsFailsItsStreamsInWords hangs up over healthy loopback, so
// the channel close it sends is answered, x/crypto's mux runs channel.close()
// on the reply, and the parked read is unblocked by the round trip rather than
// by anything this package does. A node does not usually fail that politely: it
// drops off wifi or the tailnet and its socket goes silent with no FIN and no
// reset, which is what the relay here reproduces.
//
// In that state nothing in x/crypto ends a channel — the peer will never answer
// and the mux's own read never fails — the link carries no idle timeout by
// design, and the transport is not closed anywhere else. So if hanging up did
// not force the issue, every in-flight proxy body, ssh session and browser
// terminal on that link would stay parked in Read forever while LiveStreams
// reported zero, and only a control-plane restart would free them.
func TestABlackHoledLinkStillFailsItsStreams(t *testing.T) {
	dl := newDataLink(t, throughARelay())
	dl.running(t, "web")
	port, portStr := silentGuestPort(t)

	conn, err := dl.client.DialSandbox(context.Background(), "web", StreamTCP, port)
	if err != nil {
		t.Fatalf("DialSandbox: %v", err)
	}
	read := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := conn.Read(buf)
		read <- err
	}()
	time.Sleep(20 * time.Millisecond)

	// From here on nothing either machine writes reaches the other, and neither
	// end can tell.
	dl.relay.drop.Store(true)
	// Which is what the liveness prober notices, in its own words: pingLoop
	// gives up after two missed answers and hangs up exactly like this.
	dl.client.Hangup(CodeProtocolError, "no answer to two pings")

	select {
	case err := <-read:
		if err == nil {
			t.Fatal("the read succeeded on a link whose packets go nowhere")
		}
		if errors.Is(err, io.EOF) {
			t.Fatalf("the read reported %v, which a caller cannot tell from the guest hanging up", err)
		}
		if !errors.Is(err, ErrLinkClosed) {
			t.Fatalf("the read reported %v (%T), want it to name the link", err, err)
		}
		var op *net.OpError
		if !errors.As(err, &op) {
			t.Errorf("the read reported %T, want the *net.OpError shape every caller here already handles", err)
		} else if op.Addr == nil || op.Addr.String() != "web:"+portStr {
			t.Errorf("the failure names %v, want the stream it happened on", op.Addr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the read never returned: a black-holed link left every stream on it parked in Read")
	}

	if n := dl.client.LiveStreams(); n != 0 {
		t.Errorf("the link is still carrying %d streams after it ended", n)
	}
}

// TestAClosedStreamIsForgotten pins the registry's other half: an ordinary
// close must leave nothing behind, or a busy gateway accumulates one entry per
// request it ever served.
func TestAClosedStreamIsForgotten(t *testing.T) {
	dl := newDataLink(t)
	dl.running(t, "web")
	addr := echoServer(t)
	_, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	for i := 0; i < 5; i++ {
		conn, err := dl.client.DialSandbox(context.Background(), "web", StreamTCP, port)
		if err != nil {
			t.Fatalf("DialSandbox %d: %v", i, err)
		}
		if n := dl.client.LiveStreams(); n != 1 {
			t.Fatalf("open %d: the link is carrying %d streams, want 1", i, n)
		}
		conn.Close()
		// Idempotent: a double close must not corrupt the count, because
		// net/http closes a connection it has already given up on.
		conn.Close()
		if n := dl.client.LiveStreams(); n != 0 {
			t.Fatalf("close %d: the link is still carrying %d streams", i, n)
		}
	}
}
