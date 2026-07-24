package nodelink

// The gateway half held against a hostile node: what it caches on that
// machine's say-so, and what it does when the gateway decides that machine is
// no longer welcome.
//
// The link here is a net.Pipe rather than an SSH connection, for the same
// reason conn_test.go's is: everything these tests are about — the cache, the
// bye, what a pending request reports — is above the transport, and a socket
// would only add a way for them to be flaky.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// servedLink stands a real gateway-side Client up against a hand-driven node.
//
// The node end drains continuously: net.Pipe is unbuffered, so a gateway that
// writes a welcome, a ping or a bye is blocked until somebody reads it, and a
// test that only wrote would deadlock the very teardown it is checking.
type servedLink struct {
	client *Client
	wait   chan error

	enc    *encoder
	frames chan *Frame
	// log is what an operator would have seen. The sandbox ceiling's whole
	// operator surface is a log line, so the tests that cover it assert on one.
	log *recordingLog
}

func newServedLink(t *testing.T, node string) *servedLink {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gwEnd, nodeEnd := net.Pipe()
	t.Cleanup(func() { gwEnd.Close(); nodeEnd.Close() })

	p := &servedLink{
		enc:    newEncoder(nodeEnd),
		frames: make(chan *Frame, 4096),
		wait:   make(chan error, 1),
		log:    newRecordingLog(),
	}
	go func() {
		dec := newDecoder(nodeEnd)
		for {
			f, err := dec.next()
			if err != nil {
				return
			}
			select {
			case p.frames <- f:
			default:
			}
		}
	}()

	body, err := marshalBody(Hello{Protocol: Protocol, Node: node, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	go p.enc.encode(&Frame{ID: "n1", Type: TypeHello, Body: body}) //nolint:errcheck // ReadHello reports it

	g, err := ReadHello(ctx, gwEnd, 5*time.Second)
	if err != nil {
		t.Fatalf("ReadHello: %v", err)
	}
	c, wait, err := Serve(ctx, ServerOptions{
		Node: node, Greeting: g, Session: gwEnd,
		// A ping cadence far longer than any test here, so the liveness prober
		// never races the thing under test for the reason a link ended.
		PingEvery: time.Hour,
		Log:       slog.New(p.log),
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	p.client = c
	go func() { p.wait <- wait() }()
	t.Cleanup(func() { c.Close() })
	// Drain the welcome, which is a reply to the hello and would otherwise be
	// the first reply every test here read.
	p.awaitReply(t, "n1")
	return p
}

// send writes one frame from the node. It tolerates a write failure because
// several of these tests exist precisely to make the gateway hang up mid-stream.
func (p *servedLink) send(f *Frame) error { return p.enc.encode(f) }

// awaitFrame reads until a frame of the named type arrives.
func (p *servedLink) awaitFrame(t *testing.T, typ string) *Frame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-p.frames:
			if f.Type == typ {
				return f
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %q frame", typ)
		}
	}
}

// awaitReply reads until the answer to one request arrives. Replies are matched
// by ID and not by type, because every reply on this link is TypeReply — the
// welcome included.
func (p *servedLink) awaitReply(t *testing.T, id string) *Frame {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case f := <-p.frames:
			if f.Type == TypeReply && f.ID == id {
				return f
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the reply to %q", id)
		}
	}
}

func changedFrame(t *testing.T, name string) *Frame {
	t.Helper()
	body, err := marshalBody(ChangedMsg{Node: "node-b", Sandbox: SandboxRow{Name: name}, Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return &Frame{Type: TypeChanged, Body: body}
}

// smallInventory asks the gateway a question only a live link can answer. It
// doubles as a barrier: the reply cannot arrive before every event sent ahead of
// it on the same stream has been dispatched.
func (p *servedLink) smallInventory(t *testing.T, id string, names ...string) *Frame {
	t.Helper()
	rows := make([]SandboxRow, 0, len(names))
	for _, n := range names {
		rows = append(rows, SandboxRow{Name: n})
	}
	body, err := marshalBody(InventoryMsg{Node: "node-b", Sandboxes: rows})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.send(&Frame{ID: id, Type: TypeInventory, Body: body}); err != nil {
		t.Fatalf("the link was gone before an inventory could be sent: %v", err)
	}
	return p.awaitReply(t, id)
}

// overfullLine is the one thing an operator ever sees about a machine past the
// sandbox ceiling, so it is asserted on rather than assumed.
func (p *servedLink) overfullLine(t *testing.T) logLine {
	t.Helper()
	for _, l := range p.log.at(slog.LevelError) {
		if l.Attrs["limit"] == fmt.Sprint(MaxSandboxesPerNode) {
			return l
		}
	}
	t.Fatalf("nothing legible was logged about a node past the %d-sandbox ceiling; lines: %v",
		MaxSandboxesPerNode, p.log.at(slog.LevelError))
	return logLine{}
}

// TestChangedEventsCannotGrowTheCacheForever is the abuse the cap exists for. A
// `changed` event is unacknowledged and unbounded, and only a `gone` naming one
// name or a whole inventory ever prunes what it added — so an approved machine
// streaming distinct synthetic names costs the gateway a permanent row each,
// about two hundred bytes on the wire, until the process dies of it.
//
// The cache is what must be bounded. The LINK must not be: a machine that
// honestly holds more sandboxes than this gateway will track is not lying, and
// hanging up on it produced a permanent reconnect loop — refused inventory,
// first change event drops the link, redial, repeat — that no operator could
// diagnose from the outside.
func TestChangedEventsCannotGrowTheCacheForever(t *testing.T) {
	p := newServedLink(t, "node-b")

	const beyond = 64
	for i := 0; i < MaxSandboxesPerNode+beyond; i++ {
		if err := p.send(changedFrame(t, fmt.Sprintf("box-%05d", i))); err != nil {
			t.Fatalf("the gateway hung up after %d change events: %v", i, err)
		}
	}
	// A request behind the flood: its reply proves the link is still serving,
	// and it lands after every event ahead of it has been dispatched.
	if reply := p.smallInventory(t, "n2", "box-00000"); reply.Err != nil {
		t.Fatalf("the link stopped serving a node past the ceiling: %+v", reply.Err)
	}
	if boxes, _ := p.client.Snapshot(); len(boxes) != 1 {
		t.Errorf("cached %d sandboxes after a one-row inventory, want 1", len(boxes))
	}

	// Nothing was hung up on, and nothing said goodbye.
	for {
		select {
		case f := <-p.frames:
			if f.Type == TypeBye {
				t.Fatal("the gateway said goodbye to a node that reported too many sandboxes")
			}
			continue
		default:
		}
		break
	}
	select {
	case err := <-p.wait:
		t.Fatalf("the link ended (%v) on a node that reported too many sandboxes", err)
	default:
	}

	line := p.overfullLine(t)
	if line.Attrs["node"] != "node-b" {
		t.Errorf("the ceiling was logged without naming the machine: %v", line)
	}
	if line.Attrs["next"] == "" {
		t.Errorf("the ceiling was logged with nothing an operator could do about it: %v", line)
	}
}

// TestInventoryBeyondTheCapIsRefused pins the other door into the same cache.
// It is refused rather than fatal because an inventory is a request: the node
// gets a sentence it can log, and the picture the gateway already had stands.
//
// Both doors answer the same way — the report is not cached, the link is kept,
// and the operator gets the same line — because a ceiling enforced two ways is a
// ceiling that turns one honest machine into a reconnect loop.
func TestInventoryBeyondTheCapIsRefused(t *testing.T) {
	p := newServedLink(t, "node-b")

	rows := make([]SandboxRow, MaxSandboxesPerNode+1)
	for i := range rows {
		rows[i] = SandboxRow{Name: fmt.Sprintf("box-%05d", i)}
	}
	body, err := marshalBody(InventoryMsg{Node: "node-b", Sandboxes: rows})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.send(&Frame{ID: "n2", Type: TypeInventory, Body: body}); err != nil {
		t.Fatalf("send inventory: %v", err)
	}

	reply := p.awaitReply(t, "n2")
	if reply.Err == nil {
		t.Fatalf("an inventory of %d sandboxes was accepted", len(rows))
	}
	if reply.Err.Code != "inventory_too_large" {
		t.Errorf("error code = %q, want inventory_too_large", reply.Err.Code)
	}
	if boxes, _ := p.client.Snapshot(); len(boxes) != 0 {
		t.Errorf("cached %d sandboxes from a refused inventory, want 0", len(boxes))
	}
	// The same operator line as the event door, and a link that is still up.
	p.overfullLine(t)
	if reply := p.smallInventory(t, "n3", "box-00000"); reply.Err != nil {
		t.Fatalf("the link stopped serving after one refused inventory: %+v", reply.Err)
	}
	select {
	case err := <-p.wait:
		t.Fatalf("the link ended (%v) on an inventory the gateway merely refused", err)
	default:
	}
}

// TestRevokeFailsInFlightRequestsTypedAndClosesTheLink is what an operator
// removing a connected machine has to feel like from both ends: the node is
// told why, and whatever the gateway had already asked of it comes back as the
// reason rather than as a closed socket — or, worse, as nothing at all, since
// this link carries no idle timeout by design.
func TestRevokeFailsInFlightRequestsTypedAndClosesTheLink(t *testing.T) {
	p := newServedLink(t, "node-b")

	// A request the node never answers, so it is genuinely in flight when the
	// revocation lands.
	done := make(chan error, 1)
	go func() {
		var echo PingReq
		done <- p.client.Do(context.Background(), TypePing, PingReq{Nonce: "abc"}, &echo)
	}()
	p.awaitFrame(t, TypePing)

	reason := &ctlops.Error{
		Kind: ctlops.KindCapacity, Op: "node.revoke", Code: "node_revoked",
		Msg: `node "node-b" is no longer part of this fleet`, Verbatim: true,
	}
	p.client.Revoke(CodeRevoked, reason)

	select {
	case err := <-done:
		var typed *ctlops.Error
		if !errors.As(err, &typed) {
			t.Fatalf("in-flight request failed with %v (%T), want a *ctlops.Error", err, err)
		}
		if typed.Code != "node_revoked" {
			t.Errorf("error code = %q, want node_revoked", typed.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an in-flight request hung when its link was revoked")
	}

	bye := p.awaitFrame(t, TypeBye)
	var b Bye
	if err := json.Unmarshal(bye.Body, &b); err != nil {
		t.Fatalf("bye body: %v", err)
	}
	if b.Code != CodeRevoked {
		t.Errorf("bye code = %q, want %q", b.Code, CodeRevoked)
	}
	select {
	case <-p.wait:
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked link stayed up")
	}
	if p.client.Online() {
		t.Error("a revoked node still reports itself online")
	}
}
