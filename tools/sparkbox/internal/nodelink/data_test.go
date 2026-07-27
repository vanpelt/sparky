package nodelink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

func TestDataPoolNegotiationIsAdditive(t *testing.T) {
	t.Run("new peer gets split pool", func(t *testing.T) {
		client, welcome, transport := negotiateDataPool(t, []string{CapabilitySSHDataPoolV1})
		if !welcome.SupportsDataPool() {
			t.Fatalf("welcome did not select the data pool: %+v", welcome)
		}
		if client.ssh != nil || !client.split {
			t.Fatal("negotiated client retained the combined control transport")
		}
		if welcome.ControlGeneration != client.ControlGeneration() {
			t.Fatal("welcome and live client disagree on the opaque control generation")
		}
		if transport.isClosed() {
			t.Fatal("negotiation closed the live control SSH connection")
		}
	})

	t.Run("old peer keeps combined link with nil metrics", func(t *testing.T) {
		client, welcome, transport := negotiateDataPool(t, nil)
		if welcome.SupportsDataPool() || welcome.ControlGeneration != "" {
			t.Fatalf("old peer was opted into a data pool: %+v", welcome)
		}
		if client.ssh != transport || client.split {
			t.Fatal("old peer did not retain its combined control/data transport")
		}
		// Metrics is nil in this rig. Opening and closing through the fallback
		// also pins that observability remains optional across a rolling
		// upgrade.
		stream, err := client.DialSandbox(context.Background(), "demo", StreamTCP, 8080)
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.Close()
	})
}

func negotiateDataPool(t *testing.T, capabilities []string) (*Client, Welcome, *fakeDataConn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	gateway, node := net.Pipe()
	transport := &fakeDataConn{}
	t.Cleanup(func() {
		cancel()
		_ = gateway.Close()
		_ = node.Close()
	})

	body, err := marshalBody(Hello{
		Protocol: Protocol, Node: "node-b", Capabilities: capabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	go newEncoder(node).encode(&Frame{ID: "hello-1", Type: TypeHello, Body: body}) //nolint:errcheck
	greeting, err := ReadHello(ctx, gateway, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	replies := make(chan *Frame, 1)
	go func() {
		frame, _ := newDecoder(node).next()
		replies <- frame
	}()
	client, _, err := Serve(ctx, ServerOptions{
		Node: "node-b", Greeting: greeting, Session: gateway, Conn: transport,
		Welcome:   Welcome{Capabilities: []string{CapabilitySSHDataPoolV1}},
		PingEvery: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	select {
	case reply := <-replies:
		if reply == nil || reply.Err != nil {
			t.Fatalf("welcome reply = %+v", reply)
		}
		var welcome Welcome
		if err := json.Unmarshal(reply.Body, &welcome); err != nil {
			t.Fatal(err)
		}
		return client, welcome, transport
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for welcome")
		return nil, Welcome{}, nil
	}
}

func TestDataPoolGenerationSelectionAndLaneLoss(t *testing.T) {
	c := dataPoolClient()
	a := &fakeDataConn{}
	b := &fakeDataConn{}
	detachA, err := c.AttachDataLane(DataHello{
		Protocol: Protocol, Node: "node-b", Generation: c.generation, Lane: "lane-a",
	}, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AttachDataLane(DataHello{
		Protocol: Protocol, Node: "node-b", Generation: "old-generation", Lane: "stale",
	}, &fakeDataConn{}); err == nil {
		t.Fatal("a data lane from an old control generation was accepted")
	}
	if _, err := c.AttachDataLane(DataHello{
		Protocol: Protocol, Node: "node-b", Generation: c.generation, Lane: "lane-b",
	}, b); err != nil {
		t.Fatal(err)
	}

	var streams []net.Conn
	for i := 0; i < 3; i++ {
		stream, err := c.DialSandbox(context.Background(), "demo", StreamTCP, 8080)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		streams = append(streams, stream)
	}
	if got := a.openCount(); got != 2 {
		t.Errorf("lane-a got %d opens, want 2 from least-loaded selection", got)
	}
	if got := b.openCount(); got != 1 {
		t.Errorf("lane-b got %d opens, want 1 from least-loaded selection", got)
	}

	// Losing one lane tears down only its streams. The control client and the
	// other lane remain usable and the node is not made offline.
	detachA()
	if got := c.LiveStreams(); got != 1 {
		t.Fatalf("lane loss left %d live streams, want only lane-b's one", got)
	}
	stream, err := c.DialSandbox(context.Background(), "demo", StreamTCP, 8080)
	if err != nil {
		t.Fatalf("healthy lane stopped serving after its peer died: %v", err)
	}
	streams = append(streams, stream)
	if got := b.openCount(); got != 2 {
		t.Errorf("healthy lane got %d opens, want 2", got)
	}
	for _, stream := range streams {
		_ = stream.Close()
	}
}

func TestControlLossInvalidatesEveryDataLane(t *testing.T) {
	c := dataPoolClient()
	a := &fakeDataConn{}
	b := &fakeDataConn{}
	for lane, conn := range map[string]*fakeDataConn{"lane-a": a, "lane-b": b} {
		if _, err := c.AttachDataLane(DataHello{
			Protocol: Protocol, Node: "node-b", Generation: c.generation, Lane: lane,
		}, conn); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := c.DialSandbox(context.Background(), "demo", StreamSSH, 0); err != nil {
			t.Fatal(err)
		}
	}

	c.dropStreams(ErrLinkClosed)
	if !a.isClosed() || !b.isClosed() {
		t.Fatal("control loss did not close every dedicated data connection")
	}
	if c.LiveStreams() != 0 {
		t.Fatalf("control loss left %d streams registered", c.LiveStreams())
	}
	if _, err := c.DialSandbox(context.Background(), "demo", StreamSSH, 0); !errors.Is(err, ErrLinkClosed) {
		t.Fatalf("dial after control loss = %v, want ErrLinkClosed", err)
	}
}

func TestStreamLimitAppliesAcrossDataPool(t *testing.T) {
	c := dataPoolClient()
	a := &fakeDataConn{}
	b := &fakeDataConn{}
	for lane, conn := range map[string]*fakeDataConn{"lane-a": a, "lane-b": b} {
		if _, err := c.AttachDataLane(DataHello{
			Protocol: Protocol, Node: "node-b", Generation: c.generation, Lane: lane,
		}, conn); err != nil {
			t.Fatal(err)
		}
	}
	streams := make([]net.Conn, 0, MaxLiveStreams)
	for i := 0; i < MaxLiveStreams; i++ {
		stream, err := c.DialSandbox(context.Background(), "demo", StreamTCP, 8080)
		if err != nil {
			t.Fatalf("stream %d was refused below the pool-wide ceiling: %v", i, err)
		}
		streams = append(streams, stream)
	}
	if _, err := c.DialSandbox(context.Background(), "demo", StreamTCP, 8080); err == nil {
		t.Fatalf("stream %d crossed the pool-wide ceiling", MaxLiveStreams+1)
	}
	if got := a.openCount() + b.openCount(); got != MaxLiveStreams {
		t.Fatalf("lanes opened %d streams, want exactly the shared limit %d", got, MaxLiveStreams)
	}
	_ = streams[0].Close()
	replacement, err := c.DialSandbox(context.Background(), "demo", StreamTCP, 8080)
	if err != nil {
		t.Fatalf("released pool capacity was not reusable: %v", err)
	}
	_ = replacement.Close()
	for _, stream := range streams[1:] {
		_ = stream.Close()
	}
}

func TestSplitControlRejectsGuestChannels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	open := &fakeNewDataChannel{rejected: make(chan xssh.RejectionReason, 1)}
	chans := make(chan xssh.NewChannel, 1)
	chans <- open
	close(chans)
	var enabled atomic.Bool
	ServeStreamsWithOptions(
		ctx,
		chans,
		func(string, string, int) (string, error) {
			t.Fatal("disabled control stream reached the guest resolver")
			return "", ErrUnknownSandbox
		},
		nil,
		nil,
		"node-b",
		"ssh",
		NewStreamLimiter(MaxLiveStreams),
		&enabled,
	)
	select {
	case reason := <-open.rejected:
		if reason != xssh.Prohibited {
			t.Fatalf("control stream rejected as %v, want Prohibited", reason)
		}
	default:
		t.Fatal("split control connection accepted or ignored a guest channel")
	}
}

func dataPoolClient() *Client {
	return &Client{
		node:       "node-b",
		split:      true,
		generation: "current-generation",
		log:        slog.New(slog.DiscardHandler),
		conn:       NewConn(bytes.NewReader(nil), io.Discard, "g", nil),
		streams:    map[*streamConn]*dataLane{},
		lanes:      map[string]*dataLane{},
	}
}

type fakeDataConn struct {
	mu     sync.Mutex
	opens  int
	closed bool
}

func (c *fakeDataConn) User() string          { return User }
func (c *fakeDataConn) SessionID() []byte     { return nil }
func (c *fakeDataConn) ClientVersion() []byte { return nil }
func (c *fakeDataConn) ServerVersion() []byte { return nil }
func (c *fakeDataConn) RemoteAddr() net.Addr  { return fakeDataAddr("remote") }
func (c *fakeDataConn) LocalAddr() net.Addr   { return fakeDataAddr("local") }
func (c *fakeDataConn) Wait() error           { return nil }
func (c *fakeDataConn) SendRequest(string, bool, []byte) (bool, []byte, error) {
	return true, nil, nil
}
func (c *fakeDataConn) OpenChannel(string, []byte) (xssh.Channel, <-chan *xssh.Request, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil, net.ErrClosed
	}
	c.opens++
	reqs := make(chan *xssh.Request)
	close(reqs)
	return &fakeDataChannel{}, reqs, nil
}
func (c *fakeDataConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}
func (c *fakeDataConn) openCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opens
}
func (c *fakeDataConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type fakeDataChannel struct {
	mu     sync.Mutex
	closed bool
}

func (c *fakeDataChannel) Read([]byte) (int, error) { return 0, io.EOF }
func (c *fakeDataChannel) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	return len(p), nil
}
func (c *fakeDataChannel) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}
func (c *fakeDataChannel) CloseWrite() error { return nil }
func (c *fakeDataChannel) SendRequest(string, bool, []byte) (bool, error) {
	return true, nil
}
func (c *fakeDataChannel) Stderr() io.ReadWriter { return &bytes.Buffer{} }

type fakeNewDataChannel struct {
	rejected chan xssh.RejectionReason
}

func (c *fakeNewDataChannel) Accept() (xssh.Channel, <-chan *xssh.Request, error) {
	return nil, nil, errors.New("unexpected accept")
}
func (c *fakeNewDataChannel) Reject(reason xssh.RejectionReason, _ string) error {
	c.rejected <- reason
	return nil
}
func (c *fakeNewDataChannel) ChannelType() string { return StreamChannel }
func (c *fakeNewDataChannel) ExtraData() []byte   { return nil }

type fakeDataAddr string

func (a fakeDataAddr) Network() string { return "test" }
func (a fakeDataAddr) String() string  { return string(a) }
