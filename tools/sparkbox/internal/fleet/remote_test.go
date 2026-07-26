package fleet_test

// The gateway's half of a link, against a machine that is really on the other
// end of one.
//
// The transport is net.Pipe rather than SSH because none of what is under test
// is about the transport: the frames are newline-delimited JSON either way, and
// what matters here is what the gateway makes of the answers — which node a
// record is attributed to, and what a caller reads when the machine stops
// answering mid-sentence.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// remoteRig is one live link with a machine this test controls: the fleet's
// Node on one side, a hand-written responder on the other.
type remoteRig struct {
	node    fleet.Node
	conn    *nodelink.Conn // the machine's end
	nodeEnd net.Conn

	mu   sync.Mutex
	seen []nodeFrame
}

// nodeFrame is one request or event the machine received, kept whole: the body
// is compared as it travelled, so a field renamed on the wire fails here rather
// than in production six months later.
type nodeFrame struct {
	typ  string
	body json.RawMessage
}

// newRemoteRig links a machine and hands back its Node.
//
// The hello is written by hand and the welcome is read by the machine's own
// Conn, which sees it as a reply to a request it never made and drops it — that
// is fine and is the only shortcut here: the handshake's own behaviour belongs
// to the link tests, and what this file needs is an authenticated Client.
func newRemoteRig(t *testing.T, answer func(*nodelink.Conn, *remoteRig)) *remoteRig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	gwEnd, nodeEnd := net.Pipe()
	t.Cleanup(func() { gwEnd.Close(); nodeEnd.Close() })

	r := &remoteRig{nodeEnd: nodeEnd}
	r.conn = nodelink.NewConn(nodeEnd, nodeEnd, "n", discardLog())
	if answer != nil {
		answer(r.conn, r)
	}

	hello, err := json.Marshal(nodelink.Hello{Protocol: nodelink.Protocol, Node: "node-b", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	// A goroutine because net.Pipe is unbuffered: nobody is reading the
	// gateway's end until ReadHello below.
	go json.NewEncoder(nodeEnd).Encode(nodelink.Frame{ //nolint:errcheck // ReadHello reports it
		ID: "n1", Type: nodelink.TypeHello, Body: hello,
	})

	greeting, err := nodelink.ReadHello(ctx, gwEnd, 5*time.Second)
	if err != nil {
		t.Fatalf("ReadHello: %v", err)
	}
	go r.conn.Serve(ctx) //nolint:errcheck // ends with the pipe

	client, wait, err := nodelink.Serve(ctx, nodelink.ServerOptions{
		// The AUTHENTICATED roster name. The hello above says "node-b" too, but
		// nothing consults it: this is the name the SSH key resolved to.
		Node: "node-b", Greeting: greeting, Session: gwEnd, Log: discardLog(),
		// Far longer than any test here, so the liveness prober never becomes
		// the reason a link ended.
		PingEvery: time.Hour,
	})
	if err != nil {
		t.Fatalf("serve link: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- wait() }()
	t.Cleanup(func() {
		client.Close()
		select {
		case <-served:
		case <-time.After(5 * time.Second):
			t.Error("the link outlived the test that was serving it")
		}
	})

	r.node = fleet.Remote(client)
	return r
}

// record notes one frame the machine received.
func (r *remoteRig) record(typ string, body json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, nodeFrame{typ: typ, body: append(json.RawMessage(nil), body...)})
}

func (r *remoteRig) frames() []nodeFrame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]nodeFrame(nil), r.seen...)
}

// answers registers a responder for every lifecycle verb: it records what
// arrived and replies with the shape the gateway expects. reply is consulted
// per type so a single test can make one verb refuse.
func answers(reply func(typ string) (any, error)) func(*nodelink.Conn, *remoteRig) {
	return func(c *nodelink.Conn, r *remoteRig) {
		for _, typ := range requestVerbs {
			c.Handle(typ, func(_ context.Context, body json.RawMessage) (any, error) {
				r.record(typ, body)
				return reply(typ)
			})
		}
		for _, typ := range eventVerbs {
			c.OnEvent(typ, func(body json.RawMessage) { r.record(typ, body) })
		}
	}
}

// requestVerbs and eventVerbs are §2.5's catalogue, split by whether an answer
// comes back. Nothing derives one from the other on purpose: which of the two a
// verb is, is a design decision (touch is an event because it fires on every
// session teardown), and a test that inferred it could not catch it changing.
var requestVerbs = []string{
	nodelink.TypeCreate, nodelink.TypeEnsureRunning, nodelink.TypePause,
	nodelink.TypeArchive, nodelink.TypeResize, nodelink.TypeReboot,
	nodelink.TypeRename, nodelink.TypeDestroy, nodelink.TypeSetPinned,
	nodelink.TypeResyncEnv, nodelink.TypeSnapshotCreate, nodelink.TypeSnapshotDelete,
	nodelink.TypeSnapshotFork,
}

var eventVerbs = []string{nodelink.TypeTouch, nodelink.TypeRecordKey}

// okReply is the successful answer to every verb.
func okReply(typ string) (any, error) {
	switch typ {
	case nodelink.TypeCreate, nodelink.TypeEnsureRunning, nodelink.TypeSnapshotFork:
		return nodelink.SandboxResp{Sandbox: nodelink.SandboxRow{
			Name: "demo", Owner: "alice", Image: "ubuntu", State: string(vmm.StateRunning),
			VCPUs: 2, MemMB: 2048, DiskMB: 1024, DiskTotalMB: 25600, SSHUser: "sparky",
			CreatedAt: time.Now(), LastActive: time.Now(),
		}}, nil
	case nodelink.TypeSnapshotCreate:
		return nodelink.SnapshotResp{Snapshot: nodelink.SnapshotRow{
			Name: "base", Owner: "alice", Image: "snap-alice-base", FromBox: "demo",
			CreatedAt: time.Now(),
		}}, nil
	default:
		return nodelink.EmptyResp{}, nil
	}
}

// verb is one Node method as this file drives it: what it is called, what it is
// expected to put on the wire, and what op name its failure must carry.
type verb struct {
	name string
	op   string
	typ  string
	// body is the request as it should arrive, marshalled and compared whole.
	body any
	// event marks the two verbs that never wait for an answer.
	event bool
	call  func(ctx context.Context, n fleet.Node) (any, error)
}

func lifecycleVerbs() []verb {
	return []verb{
		{
			name: "create", op: "create", typ: nodelink.TypeCreate,
			body: nodelink.CreateReq{Name: "demo", Owner: "alice", Image: "ubuntu", VCPUs: 2, MemMB: 2048},
			call: func(ctx context.Context, n fleet.Node) (any, error) {
				return n.Create(ctx, "demo", "alice", "ubuntu", 2, 2048)
			},
		},
		{
			name: "restore", op: "restore", typ: nodelink.TypeEnsureRunning,
			body: nodelink.NameReq{Name: "demo"},
			call: func(ctx context.Context, n fleet.Node) (any, error) { return n.EnsureRunning(ctx, "demo") },
		},
		{
			name: "pause", op: "pause", typ: nodelink.TypePause,
			body: nodelink.NameReq{Name: "demo"},
			call: func(ctx context.Context, n fleet.Node) (any, error) { return nil, n.Pause(ctx, "demo") },
		},
		{
			name: "archive", op: "archive", typ: nodelink.TypeArchive,
			body: nodelink.NameReq{Name: "demo"},
			call: func(ctx context.Context, n fleet.Node) (any, error) { return nil, n.Archive(ctx, "demo") },
		},
		{
			name: "resize", op: "resize", typ: nodelink.TypeResize,
			body: nodelink.ResizeReq{Name: "demo", SizeMB: 40960},
			call: func(ctx context.Context, n fleet.Node) (any, error) { return nil, n.Resize(ctx, "demo", 40960) },
		},
		{
			name: "reboot", op: "reboot", typ: nodelink.TypeReboot,
			body: nodelink.NameReq{Name: "demo"},
			call: func(ctx context.Context, n fleet.Node) (any, error) { return nil, n.Reboot(ctx, "demo") },
		},
		{
			name: "rename", op: "rename", typ: nodelink.TypeRename,
			body: nodelink.RenameReq{Name: "demo", NewName: "demo2", Owner: "alice"},
			call: func(ctx context.Context, n fleet.Node) (any, error) {
				return nil, n.Rename(ctx, "demo", "demo2", "alice")
			},
		},
		{
			name: "rm", op: "rm", typ: nodelink.TypeDestroy,
			body: nodelink.NameReq{Name: "demo"},
			call: func(ctx context.Context, n fleet.Node) (any, error) { return nil, n.Destroy(ctx, "demo") },
		},
		{
			name: "pin", op: "pin", typ: nodelink.TypeSetPinned,
			body: nodelink.PinReq{Name: "demo", Pinned: true},
			call: func(ctx context.Context, n fleet.Node) (any, error) { return nil, n.SetPinned(ctx, "demo", true) },
		},
		{
			name: "unpin", op: "unpin", typ: nodelink.TypeSetPinned,
			body: nodelink.PinReq{Name: "demo", Pinned: false},
			call: func(ctx context.Context, n fleet.Node) (any, error) { return nil, n.SetPinned(ctx, "demo", false) },
		},
		{
			name: "secrets.sync", op: "secrets.sync", typ: nodelink.TypeResyncEnv,
			body: nodelink.NameReq{Name: "demo"},
			call: func(ctx context.Context, n fleet.Node) (any, error) { return nil, n.ResyncEnv(ctx, "demo") },
		},
		{
			name: "snapshot.create", op: "snapshot.create", typ: nodelink.TypeSnapshotCreate,
			body: nodelink.SnapshotReq{Sandbox: "demo", Snapshot: "base", Owner: "alice"},
			call: func(ctx context.Context, n fleet.Node) (any, error) {
				return n.Snapshotter(ctx, "demo", "base", "alice")
			},
		},
		{
			name: "snapshot.rm", op: "snapshot.rm", typ: nodelink.TypeSnapshotDelete,
			body: nodelink.DeleteSnapshotReq{Snapshot: "base", Owner: "alice"},
			call: func(ctx context.Context, n fleet.Node) (any, error) {
				return nil, n.DeleteSnapshot(ctx, "base", "alice")
			},
		},
		{
			name: "fork", op: "fork", typ: nodelink.TypeSnapshotFork,
			body: nodelink.ForkReq{Snapshot: "base", Name: "demo", Owner: "alice", VCPUs: 1, MemMB: 512},
			call: func(ctx context.Context, n fleet.Node) (any, error) {
				return n.Fork(ctx, "base", "demo", "alice", 1, 512)
			},
		},
		{
			name: "touch", op: "touch", typ: nodelink.TypeTouch, event: true,
			body: nodelink.NameReq{Name: "demo"},
			call: func(ctx context.Context, n fleet.Node) (any, error) { return nil, n.Touch(ctx, "demo") },
		},
		{
			name: "keys.record", op: "keys.record", typ: nodelink.TypeRecordKey, event: true,
			body: nodelink.KeyReq{Name: "demo", KeyFP: "SHA256:abc"},
			call: func(ctx context.Context, n fleet.Node) (any, error) {
				return nil, n.RecordKey(ctx, "demo", "SHA256:abc")
			},
		},
	}
}

// TestRemoteRunsEveryVerbOverTheLink is the catalogue in one sweep: every Node
// method puts the right message on the wire with the right body, and every
// record that comes back is attributed to the machine that answered.
func TestRemoteRunsEveryVerbOverTheLink(t *testing.T) {
	ctx := context.Background()

	for _, v := range lifecycleVerbs() {
		t.Run(v.name, func(t *testing.T) {
			rig := newRemoteRig(t, answers(okReply))

			got, err := v.call(ctx, rig.node)
			if err != nil {
				t.Fatalf("%s: %v", v.name, err)
			}
			if v.event {
				// Nothing acknowledges an event, so the only observation is that
				// it arrived.
				waitFor(t, v.typ+" to reach the machine", func() bool { return len(rig.frames()) > 0 })
			}
			frames := rig.frames()
			if len(frames) != 1 {
				t.Fatalf("%s put %d frames on the link, want exactly one", v.name, len(frames))
			}
			if frames[0].typ != v.typ {
				t.Errorf("%s sent %q, want %q", v.name, frames[0].typ, v.typ)
			}
			want, err := json.Marshal(v.body)
			if err != nil {
				t.Fatal(err)
			}
			if string(frames[0].body) != string(want) {
				t.Errorf("%s body = %s, want %s", v.name, frames[0].body, want)
			}
			// Invariant 1, swept across every verb that answers with a record:
			// whatever the machine said, the record is attributed to the name its
			// key resolved to.
			assertAttributed(t, got)
		})
	}
}

// assertAttributed checks the node stamp and the synthetic addressing on any
// record a verb produced.
func assertAttributed(t *testing.T, got any) {
	t.Helper()
	switch rec := got.(type) {
	case *host.Sandbox:
		if rec.Node != "node-b" {
			t.Errorf("record claims node %q, want the authenticated node-b", rec.Node)
		}
		if want := fleet.Host(rec.Name, "node-b"); rec.HostIP != want {
			t.Errorf("record's HostIP = %q, want the synthetic %q", rec.HostIP, want)
		}
		if !strings.HasPrefix(rec.SSHAddr, rec.HostIP+":") {
			t.Errorf("record's SSHAddr = %q, want it under %q", rec.SSHAddr, rec.HostIP)
		}
		if rec.GuestV6 != "" {
			t.Errorf("record carries a guest IPv6 %q; the wire carries none", rec.GuestV6)
		}
	case *host.Snapshot:
		if rec.Node != "node-b" {
			t.Errorf("template claims node %q, want the authenticated node-b", rec.Node)
		}
	}
}

// TestRemoteOverwritesNodeField is the invariant stated as an attack.
//
// The machine answers with a hand-written body that names another node and
// quotes a real guest address — the two things a compromised or merely confused
// node could say to be attributed somebody else's sandbox, or to talk the
// gateway into dialing one of its OWN guests. Neither survives the projection,
// and neither can: the wire's SandboxRow has no node field and no address
// fields, and the gateway stamps both from state only it holds.
func TestRemoteOverwritesNodeField(t *testing.T) {
	rig := newRemoteRig(t, answers(func(typ string) (any, error) {
		if typ != nodelink.TypeCreate {
			return okReply(typ)
		}
		return json.RawMessage(`{"sandbox":{
			"name":"demo","owner":"alice","image":"ubuntu","state":"running",
			"node":"evil","host_ip":"172.30.4.2","ssh_addr":"172.30.4.2:22","guest_v6":"fd00::4"
		}}`), nil
	}))

	b, err := rig.node.Create(context.Background(), "demo", "alice", "ubuntu", 1, 512)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if b.Node != "node-b" {
		t.Errorf("a node claiming to be %q was believed; the roster name is node-b", b.Node)
	}
	if b.HostIP == "172.30.4.2" || b.SSHAddr == "172.30.4.2:22" {
		t.Errorf("a relayed guest address survived the projection: %s / %s", b.HostIP, b.SSHAddr)
	}
	if b.GuestV6 != "" {
		t.Errorf("a relayed guest IPv6 survived the projection: %s", b.GuestV6)
	}
	if b.HostIP != fleet.Host("demo", "node-b") {
		t.Errorf("HostIP = %q, want the synthetic %q", b.HostIP, fleet.Host("demo", "node-b"))
	}
}

// TestRemoteScrubsNodeAuthoredDisplayStrings is the same invariant aimed at the
// other thing a compromised machine can do with a record: not claim somebody
// else's sandbox, but forge lines in somebody's terminal.
//
// `ctl list` prints a sandbox's name and state into a fixed-width, CRLF-framed
// line at a raw terminal (sshgw/control.go), and `ctl snapshot ls` does the same
// with a template's name and the box it came from. Every one of those strings
// arrives from the machine. A node that puts an escape sequence in one of them
// writes whatever it likes on that user's screen — "your key was revoked,
// re-enrol at ..." being the obvious use — so none of them may survive a
// projection as typed. The pause reason is scrubbed at the link for exactly this
// reason (nodelink's TypePaused reader); this is the rest of the surface.
func TestRemoteScrubsNodeAuthoredDisplayStrings(t *testing.T) {
	const forged = "running\r\n\x1b[2Ksparkbox: your key was revoked\r\n"

	rig := newRemoteRig(t, nil)
	inv := nodelink.InventoryMsg{
		Sandboxes: []nodelink.SandboxRow{
			{Name: "honest", Owner: "alice", Image: "ubu\x1b[31mntu", State: forged,
				SSHUser: "spa\rrky", KeyFP: "SHA256:\x1bok"},
			// A name that is not a name. It cannot be shown, cannot be a DNS
			// label, and cannot be routed to, so it is not served at all.
			{Name: "bad\r\nname", Owner: "alice", State: string(vmm.StateRunning)},
		},
		Snapshots: []nodelink.SnapshotRow{
			{Name: "base", Owner: "alice", Image: "sn\x1bap", FromBox: "not a box\r\n"},
			{Name: "worse\x1b[2Kname", Owner: "alice"},
		},
		At: time.Now(),
	}
	var ack nodelink.InventoryAck
	if err := rig.conn.Request(context.Background(), nodelink.TypeInventory, inv, &ack); err != nil {
		t.Fatalf("inventory: %v", err)
	}

	b, ok := rig.node.Box("honest")
	if !ok {
		t.Fatal("the honestly-named row was dropped")
	}
	if b.State != "" {
		t.Errorf("state = %q; a word outside the vmm vocabulary must not be repeated", b.State)
	}
	for _, f := range []struct{ what, got string }{
		{"image", b.Image}, {"ssh user", b.SSHUser}, {"key fingerprint", b.KeyFP},
	} {
		if strings.ContainsAny(f.got, "\x1b\r\n") {
			t.Errorf("%s = %q; it reaches a raw terminal unescaped", f.what, f.got)
		}
	}

	if _, ok := rig.node.Box("bad\r\nname"); ok {
		t.Error("a row named with control characters was served")
	}
	if boxes := rig.node.Boxes(); len(boxes) != 1 || boxes[0].Name != "honest" {
		t.Errorf("Boxes() = %+v, want only the honestly-named one", boxes)
	}

	snaps := rig.node.Templates()
	if len(snaps) != 1 || snaps[0].Name != "base" {
		t.Fatalf("Templates() = %+v, want only the honestly-named one", snaps)
	}
	if snaps[0].FromBox != "" {
		t.Errorf("from-box = %q; a name that is not a name is dropped, not printed", snaps[0].FromBox)
	}
	if strings.ContainsAny(snaps[0].Image, "\x1b\r\n") {
		t.Errorf("template image = %q; it reaches a raw terminal unescaped", snaps[0].Image)
	}
}

// TestRemoteRepliesCannotRenameTheirOwnRecord is the create/restore/fork half.
//
// Those three answer with a record that goes straight back to the caller without
// passing through Fleet.serve, which is what re-derives name and owner from the
// ledger everywhere else. The gateway asked for a name and reserved the ledger
// row under it a moment earlier, so a reply naming something else is either a
// broken machine or one aiming at sshgw's reconnect hint, which concatenates a
// sandbox name into a sentence written to a terminal in raw mode.
func TestRemoteRepliesCannotRenameTheirOwnRecord(t *testing.T) {
	const forged = `{"sandbox":{"name":"demo\r\n\u001b[2Kssh evil@elsewhere","owner":"mallory","state":"running"}}`
	rig := newRemoteRig(t, answers(func(typ string) (any, error) {
		switch typ {
		case nodelink.TypeCreate, nodelink.TypeEnsureRunning, nodelink.TypeSnapshotFork:
			return json.RawMessage(forged), nil
		}
		return okReply(typ)
	}))
	ctx := context.Background()

	cases := []struct {
		name       string
		call       func() (*host.Sandbox, error)
		wantName   string
		wantOwner  string
		ownerKnown bool
	}{
		{"create", func() (*host.Sandbox, error) {
			return rig.node.Create(ctx, "demo", "alice", "ubuntu", 1, 512)
		}, "demo", "alice", true},
		{"fork", func() (*host.Sandbox, error) {
			return rig.node.Fork(ctx, "base", "demo2", "alice", 1, 512)
		}, "demo2", "alice", true},
		// A resume carries no owner — it is not an ownership-changing operation
		// — so the node's advisory one rides on, but only if it is a handle this
		// platform would issue.
		{"restore", func() (*host.Sandbox, error) {
			return rig.node.EnsureRunning(ctx, "demo3")
		}, "demo3", "mallory", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := c.call()
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if b.Name != c.wantName {
				t.Errorf("name = %q, want the requested %q", b.Name, c.wantName)
			}
			if b.Owner != c.wantOwner {
				t.Errorf("owner = %q, want %q", b.Owner, c.wantOwner)
			}
			if b.HostIP != fleet.Host(c.wantName, "node-b") {
				t.Errorf("HostIP = %q; it must be synthesised from the requested name", b.HostIP)
			}
			if b.State != vmm.StateRunning {
				t.Errorf("state = %q, want the node's own running", b.State)
			}
		})
	}
}

// TestRemoteCacheIsAttributedToo covers the other door into the index: the rows
// a node reports in its inventory, which is where every listing and every
// ownership check reads a remote sandbox from.
func TestRemoteCacheIsAttributedToo(t *testing.T) {
	rig := newRemoteRig(t, nil)
	ctx := context.Background()

	inv := nodelink.InventoryMsg{
		Node: "evil", // ignored: the link decides which machine this is
		Sandboxes: []nodelink.SandboxRow{{
			Name: "far-away", Owner: "alice", Image: "ubuntu", State: string(vmm.StateRunning),
			VCPUs: 2, MemMB: 2048, DiskMB: 900, LastActive: time.Now(),
		}},
		Snapshots: []nodelink.SnapshotRow{{
			Name: "base", Owner: "alice", Image: "snap-alice-base", FromBox: "far-away",
		}},
		At: time.Now(),
	}
	var ack nodelink.InventoryAck
	if err := rig.conn.Request(ctx, nodelink.TypeInventory, inv, &ack); err != nil {
		t.Fatalf("inventory: %v", err)
	}

	b, ok := rig.node.Box("far-away")
	if !ok {
		t.Fatal("the link took an inventory but cannot answer for what was in it")
	}
	assertAttributed(t, b)
	if b.State != vmm.StateRunning || b.MemMB != 2048 || b.DiskMB != 900 {
		t.Errorf("cached record = %+v, want the node's own state, memory and disk", b)
	}
	if _, ok := rig.node.Box("nothing-here"); ok {
		t.Error("the link answered for a sandbox no inventory mentioned")
	}

	boxes := rig.node.Boxes()
	if len(boxes) != 1 {
		t.Fatalf("the machine holds %d sandboxes, want 1", len(boxes))
	}
	assertAttributed(t, boxes[0])

	snaps := rig.node.Templates()
	if len(snaps) != 1 {
		t.Fatalf("the machine holds %d templates, want 1", len(snaps))
	}
	assertAttributed(t, snaps[0])
}

// TestOfflineNodeReturnsUnreachable is invariant 2, and the failure it prevents
// is specific: Conn.Serve records io.EOF on a clean end of stream, which is
// what a node restart, a gateway restart, a superseded link and a killed
// process all look like. Left alone it reaches a user as `sparkbox: rm failed:
// EOF` — unreadable, and easily mistaken for "there is no such sandbox".
func TestOfflineNodeReturnsUnreachable(t *testing.T) {
	ctx := context.Background()

	for _, v := range lifecycleVerbs() {
		t.Run(v.name, func(t *testing.T) {
			rig := newRemoteRig(t, answers(okReply))

			// The machine goes away. Closing the node's end of the pipe is a
			// clean end of stream, which is precisely the io.EOF case.
			rig.nodeEnd.Close()
			waitFor(t, "the gateway to notice the link ended", func() bool { return !rig.node.Online() })

			_, err := v.call(ctx, rig.node)
			if v.event {
				// Fire-and-forget: there was never a caller to report to, and a
				// failed touch must not fail the session that triggered it.
				if err != nil {
					t.Fatalf("%s reported %v; it is an event and answers nil", v.name, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s succeeded against a machine that is gone", v.name)
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("%s leaked io.EOF to its caller: %v", v.name, err)
			}
			assertUnreachable(t, v.op, err)
		})
	}
}

// TestUnreachableWhileInFlight is the other half of the same invariant: the
// link dies under a request that has already been sent, which is the case a
// waiter — not a pre-flight check — has to answer.
func TestUnreachableWhileInFlight(t *testing.T) {
	started := make(chan struct{})
	rig := newRemoteRig(t, func(c *nodelink.Conn, _ *remoteRig) {
		c.Handle(nodelink.TypePause, func(ctx context.Context, _ json.RawMessage) (any, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	})

	done := make(chan error, 1)
	go func() { done <- rig.node.Pause(context.Background(), "demo") }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the machine never received the pause")
	}
	rig.nodeEnd.Close()

	select {
	case err := <-done:
		if errors.Is(err, io.EOF) {
			t.Fatalf("a request in flight when the link died leaked io.EOF: %v", err)
		}
		assertUnreachable(t, "pause", err)
	case <-time.After(5 * time.Second):
		t.Fatal("a request in flight when the link died never returned")
	}
}

func assertUnreachable(t *testing.T, op string, err error) {
	t.Helper()
	if !fleet.IsNodeUnreachable(err) {
		t.Fatalf("got %v (%T), want a node-unreachable error", err, err)
	}
	e := ctlops.AsError(op, err)
	if e.Op != op {
		t.Errorf("error op = %q, want %q", e.Op, op)
	}
	if e.ExitCode() != 1 || e.HTTPStatus() != http.StatusServiceUnavailable {
		t.Errorf("exit %d / HTTP %d, want 1 / 503", e.ExitCode(), e.HTTPStatus())
	}
	if got, _ := e.Details["node"].(string); got != "node-b" {
		t.Errorf("error names node %q, want node-b", got)
	}
	if !strings.Contains(e.Msg, "node-b") || !strings.Contains(e.Msg, "offline") {
		t.Errorf("message = %q, want it to name the machine and say it is offline", e.Msg)
	}
}

// TestRemotePassesTheMachinesOwnAnswerThrough is the boundary of the previous
// test: a refusal the machine chose is not an outage, and turning it into one
// would replace the only sentence that explains what to do next.
func TestRemotePassesTheMachinesOwnAnswerThrough(t *testing.T) {
	rig := newRemoteRig(t, answers(func(typ string) (any, error) {
		return nil, &host.CapacityError{RequestedMB: 4096, UsedMB: 60000, BudgetMB: 61440}
	}))

	_, err := rig.node.Create(context.Background(), "demo", "alice", "ubuntu", 2, 4096)
	if err == nil {
		t.Fatal("a refused create reported success")
	}
	if fleet.IsNodeUnreachable(err) {
		t.Fatalf("a machine's own refusal was reported as an outage: %v", err)
	}
	var capacity *host.CapacityError
	if !errors.As(err, &capacity) {
		t.Fatalf("got %v (%T), want the machine's *host.CapacityError", err, err)
	}
	if capacity.BudgetMB != 61440 {
		t.Errorf("budget = %d MB, want the 61440 the machine reported", capacity.BudgetMB)
	}
}

// TestRemoteCallerCancellationIsNotAnOutage keeps the third class honest. A
// caller's own deadline or cancellation is classified exactly as it would be
// for a sandbox on this machine; blaming the node would send somebody to look
// at a machine that is fine.
func TestRemoteCallerCancellationIsNotAnOutage(t *testing.T) {
	rig := newRemoteRig(t, func(c *nodelink.Conn, _ *remoteRig) {
		c.Handle(nodelink.TypePause, func(ctx context.Context, _ json.RawMessage) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	err := rig.node.Pause(ctx, "demo")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v (%T), want the caller's own context.Canceled", err, err)
	}
	if fleet.IsNodeUnreachable(err) {
		t.Error("a caller hanging up was reported as the machine being offline")
	}
}

// TestRemoteDialGuestOnALinkThatCarriesNoStreams pins invariant 2 on the data
// plane's one entry point.
//
// This rig's link is a net.Pipe: it carries control frames and has no SSH
// connection under it, which is exactly the shape of a link whose transport has
// died — Client.DialSandbox has nothing to open a channel on. What must NOT
// come back is the sentence it says about itself, which is a remark about this
// process's plumbing that no user can act on. The gateway owes the caller the
// same answer it owes for any other machine that is not answering.
func TestRemoteDialGuestOnALinkThatCarriesNoStreams(t *testing.T) {
	rig := newRemoteRig(t, nil)

	conn, err := rig.node.DialGuest(context.Background(), "demo", nodelink.StreamSSH, 0)
	if conn != nil {
		t.Error("DialGuest handed back a connection it cannot have opened")
	}
	if err == nil {
		t.Fatal("DialGuest reported success")
	}
	if !fleet.IsNodeUnreachable(err) {
		t.Fatalf("DialGuest failed with %v (%T), want the node-unreachable answer", err, err)
	}
	e := ctlops.AsError("dial", err)
	if e.Op != "dial" {
		t.Errorf("op = %q, want the op the caller asked for", e.Op)
	}
	if !strings.Contains(e.Msg, "demo") || !strings.Contains(e.Msg, "node-b") {
		t.Errorf("refusal = %q, want it to name the sandbox and the machine", e.Msg)
	}
	if strings.Contains(e.Msg, "channel") {
		t.Errorf("refusal = %q, want no plumbing detail in a sentence a user reads", e.Msg)
	}
}
