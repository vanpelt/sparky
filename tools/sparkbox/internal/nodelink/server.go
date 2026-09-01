package nodelink

// The gateway half of a link: reading the first frame before any trust exists,
// refusing it in the wire's own shape, and — once the roster has said yes —
// owning one node's control channel for as long as it lasts.
//
// Nothing here decides who may connect. The door that calls this has already
// resolved an SSH key to a roster row; this file takes the answer as given and
// never looks at hello.Node for anything but a log line.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// HelloTimeout is how long a connected machine has to introduce itself. It is
// short because the hello is the first thing a node writes after its exec: a
// connection that has opened the fleet door and then says nothing is either
// broken or probing, and either way it is holding a session open.
const HelloTimeout = 10 * time.Second

// OpLink is the op every refusal at this door carries, so a node's log line
// reads `node.link failed: …` whatever went wrong.
const OpLink = "node.link"

// The three stable codes an identity request can come back refused with. They
// are codes rather than sentences because the NODE has to act on the
// difference: it turns them back into the errors internal/metadata classifies
// on, and a guest is told 400, 403 or 503 accordingly. A sentence would make
// that a string comparison.
const (
	// CodeNoIssuer is a gateway that issues no workload identity at all — it
	// has no OIDC key configured. Permanent until an operator changes it, so
	// the guest's retry will not fix it, but retrying costs nothing either.
	CodeNoIssuer = "identity_not_issued"
	// CodeIdentityAudience is a `?aud=` this issuer will not mint for. The
	// guest asked for something wrong; nothing on the host is broken.
	CodeIdentityAudience = "identity_audience"
	// CodeNotYours is the one that matters: this node asked to be spoken for
	// about a sandbox the ledger does not place on it. Nothing legitimate
	// produces it.
	CodeNotYours = "identity_not_on_this_node"
	// CodeNoRepos is a gateway that serves no repo attachments at all — no
	// attachment store, or no GitHub App key. The guest is told 501, the same
	// answer a gateway-local sandbox gets from a metadata service with no repo
	// source wired, so a fleet reads the same on every machine in it.
	CodeNoRepos = "repos_not_enabled"
	// CodeNoSuchRepo is a credential asked for a repository this sandbox has no
	// attachment to. The guest is told 404 — and it must be 404 rather than a
	// refusal, because git asks the credential helper about every host it
	// touches and an unattached repository is an ordinary miss, not a fault.
	CodeNoSuchRepo = "repo_not_attached"
	// CodeRepoDenied is a credential this fleet will not mint for a repository
	// the sandbox IS attached to: the owner's GitHub link is not strong enough
	// to reach an access verb, their account is not active, or the App is not
	// installed on it. The guest is told 403 and the sentence rides along,
	// because each of those has a different fix and none of them is a retry.
	CodeRepoDenied = "repo_denied"
	// CodeRepoUpstream is github.com not answering, or answering with something
	// this gateway cannot act on. The guest is told 503: the repair is to wait,
	// which is the one thing a 500 would talk it out of.
	CodeRepoUpstream = "repo_upstream_unavailable"
	// CodeNoSelfLifecycle is a gateway with no guest self-service lifecycle
	// wired — no control plane installed on the fleet. Answered on the nil hook
	// rather than left to produce an unregistered-type error, for the reason
	// noRepos gives: "this deployment does not do that" and "this gateway is too
	// old to speak it" must not read as the same diagnosis to whoever is
	// looking at the node's log.
	CodeNoSelfLifecycle = "self_lifecycle_not_enabled"
	// CodeNoCertificateIssuer means this gateway has no internal CA/roster
	// signer wired. It is distinct from a pending or disabled authenticated row.
	CodeNoCertificateIssuer = "certificate_not_issued"
	// CodeCertificateNotApproved is a roster row that ceased to be approved
	// before its signed leaf could be recorded.
	CodeCertificateNotApproved = "certificate_node_not_approved"
)

// registerUplinkOps installs requests that travel node -> gateway.
//
// Every other Handle on this side of the link answers something a node
// volunteered; these answer something it needs, and the node blocks a guest's
// HTTP request on the reply — so both are cheap by construction and neither
// touches a disk.
//
// node is the AUTHENTICATED link name and is what the hooks are handed. It is
// closed over here rather than read from the request precisely so that no
// future edit to the payload can introduce a second answer to "which machine
// is asking".
func registerUplinkOps(conn *Conn, node string, hooks Hooks) {
	conn.Handle(TypeIdentityToken, func(ctx context.Context, raw json.RawMessage) (any, error) {
		req, err := identityRequest(raw)
		if err != nil {
			return nil, err
		}
		if hooks.OnIdentityToken == nil {
			return nil, noIssuer()
		}
		return hooks.OnIdentityToken(ctx, node, req)
	})
	conn.Handle(TypeIdentityDoc, func(ctx context.Context, raw json.RawMessage) (any, error) {
		req, err := identityRequest(raw)
		if err != nil {
			return nil, err
		}
		if hooks.OnIdentityDoc == nil {
			return nil, noIssuer()
		}
		return hooks.OnIdentityDoc(ctx, node, req)
	})
	conn.Handle(TypeSelfVisibility, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req SelfVisibilityReq
		if err := json.Unmarshal(raw, &req); err != nil || req.Sandbox == "" ||
			(req.Visibility != "public" && req.Visibility != "private") {
			return nil, ctlops.Invalid(OpLink, "bad_self_visibility_request",
				"a self-visibility request needs a sandbox and public or private visibility")
		}
		if hooks.OnSelfVisibility == nil {
			return nil, ctlops.Disabled(OpLink, "route visibility is not enabled on this gateway")
		}
		return hooks.OnSelfVisibility(ctx, node, req)
	})
	conn.Handle(TypeSelfPort, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req SelfPortReq
		if err := json.Unmarshal(raw, &req); err != nil || req.Sandbox == "" || req.Port < 1 || req.Port > 65535 {
			return nil, ctlops.Invalid(OpLink, "bad_self_port_request",
				"a self-port request needs a sandbox and a port from 1 through 65535")
		}
		if hooks.OnSelfPort == nil {
			return nil, ctlops.Disabled(OpLink, "default route configuration is not enabled on this gateway")
		}
		return hooks.OnSelfPort(ctx, node, req)
	})
	conn.Handle(TypeSelfPause, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req SelfPauseReq
		if err := json.Unmarshal(raw, &req); err != nil || req.Sandbox == "" {
			return nil, ctlops.Invalid(OpLink, "bad_self_pause_request",
				"a self-pause request has to name a sandbox")
		}
		if hooks.OnSelfPause == nil {
			return nil, noSelfLifecycle()
		}
		return hooks.OnSelfPause(ctx, node, req)
	})
	conn.Handle(TypeSelfSnapshotPlan, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req SelfSnapshotPlanReq
		if err := json.Unmarshal(raw, &req); err != nil || req.Sandbox == "" ||
			len(req.Tag) > MaxSelfTagBytes || len(req.Name) > MaxSelfNameBytes {
			return nil, ctlops.Invalid(OpLink, "bad_self_snapshot_plan_request",
				"a capture plan needs a sandbox, a tag of at most %d bytes and a name of at most %d bytes",
				MaxSelfTagBytes, MaxSelfNameBytes)
		}
		if hooks.OnSelfSnapshotPlan == nil {
			return nil, noSelfLifecycle()
		}
		return hooks.OnSelfSnapshotPlan(ctx, node, req)
	})
	conn.Handle(TypeSelfSnapshot, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req SelfSnapshotReq
		if err := json.Unmarshal(raw, &req); err != nil || req.Sandbox == "" ||
			req.Tag == "" || req.Name == "" ||
			len(req.Tag) > MaxSelfTagBytes || len(req.Name) > MaxSelfNameBytes {
			return nil, ctlops.Invalid(OpLink, "bad_self_snapshot_request",
				"a capture needs a sandbox, the plan's tag and the plan's snapshot name")
		}
		if hooks.OnSelfSnapshot == nil {
			return nil, noSelfLifecycle()
		}
		return hooks.OnSelfSnapshot(ctx, node, req)
	})
	conn.Handle(TypeSelfRepos, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req SelfReposReq
		if err := json.Unmarshal(raw, &req); err != nil || req.Sandbox == "" {
			return nil, ctlops.Invalid(OpLink, "bad_self_repos_request",
				"a repo manifest request has to name a sandbox")
		}
		if hooks.OnSelfRepos == nil {
			return nil, noRepos()
		}
		return hooks.OnSelfRepos(ctx, node, req)
	})
	conn.Handle(TypeSelfRepoCred, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req SelfRepoCredReq
		if err := json.Unmarshal(raw, &req); err != nil || req.Sandbox == "" ||
			req.Slug == "" || len(req.Slug) > MaxRepoSlugBytes {
			return nil, ctlops.Invalid(OpLink, "bad_self_repo_credential_request",
				"a repo credential request has to name a sandbox and a repository of at most %d bytes",
				MaxRepoSlugBytes)
		}
		if hooks.OnSelfRepoCred == nil {
			return nil, noRepos()
		}
		return hooks.OnSelfRepoCred(ctx, node, req)
	})
	conn.Handle(TypeSelfRepoAuthStart, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req SelfRepoAuthStartReq
		if err := json.Unmarshal(raw, &req); err != nil || req.Sandbox == "" || req.Slug == "" || len(req.Slug) > MaxRepoSlugBytes {
			return nil, ctlops.Invalid(OpLink, "bad_self_repo_authorization_start", "a repo authorization needs a sandbox and repository")
		}
		if hooks.OnSelfRepoAuthStart == nil {
			return nil, noRepos()
		}
		return hooks.OnSelfRepoAuthStart(ctx, node, req)
	})
	conn.Handle(TypeSelfRepoAuthPoll, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var req SelfRepoAuthPollReq
		if err := json.Unmarshal(raw, &req); err != nil || req.Sandbox == "" || len(req.ID) < 20 || len(req.ID) > 128 {
			return nil, ctlops.Invalid(OpLink, "bad_self_repo_authorization_poll", "a repo authorization poll needs a sandbox and flow id")
		}
		if hooks.OnSelfRepoAuthPoll == nil {
			return nil, noRepos()
		}
		return hooks.OnSelfRepoAuthPoll(ctx, node, req)
	})
}

// identityRequest parses and bounds one identity request. The name is checked
// for shape here rather than at the ledger so a malformed one is an invalid
// request, which is what it is, instead of a lookup miss.
func identityRequest(raw json.RawMessage) (IdentityReq, error) {
	var req IdentityReq
	if err := json.Unmarshal(raw, &req); err != nil {
		return IdentityReq{}, ctlops.Invalid(OpLink, "bad_identity_request",
			"that identity request could not be read: %v", err)
	}
	if req.Sandbox == "" {
		return IdentityReq{}, ctlops.Invalid(OpLink, "bad_identity_request",
			"an identity request has to name a sandbox")
	}
	return req, nil
}

// noIssuer is built field by field rather than through ctlops.Disabled, whose
// Code is derived from the op: the node switches on CodeNoIssuer, so it has to
// be the code that actually goes out.
func noIssuer() error {
	return &ctlops.Error{
		Kind: ctlops.KindDisabled, Op: OpLink, Code: CodeNoIssuer, Verbatim: true,
		Msg: "this gateway issues no workload identity: it has no OIDC signing key configured.",
	}
}

// noRepos is the repo pair's equivalent, and is built by hand for the same
// reason: the node switches on CodeNoRepos to decide the guest sees 501 rather
// than the 500 an unclassifiable failure earns.
//
// It is answered here, on the nil hook, rather than left to produce an
// unregistered-type error, so that a gateway which simply has no App key and
// one which is too old to speak repos at all are not the same diagnosis.
func noRepos() error {
	return &ctlops.Error{
		Kind: ctlops.KindDisabled, Op: OpLink, Code: CodeNoRepos, Verbatim: true,
		Msg: "this gateway serves no repo attachments: it has no attachment store or no GitHub App key configured.",
	}
}

// noSelfLifecycle is the lifecycle trio's equivalent of noRepos, hand-built for
// the same reason: the node maps CodeNoSelfLifecycle onto the 501 a
// gateway-local guest would get from a metadata service with no lifecycle
// wired, so the two machines answer the same thing.
func noSelfLifecycle() error {
	return &ctlops.Error{
		Kind: ctlops.KindDisabled, Op: OpLink, Code: CodeNoSelfLifecycle, Verbatim: true,
		Msg: "this gateway does not run guest self-service lifecycle: no control plane is installed on its fleet.",
	}
}

// CodeRevoked is the bye a node is sent when the gateway takes its approval
// away — it was removed from the roster, or disabled, while it was connected.
// It is its own code rather than one of the three in frame.go because a node
// reading its bye should reconnect on CodeSuperseded, complain on
// CodeProtocolError, and on this one go back to being a machine that has to be
// approved before it is anything.
const CodeRevoked = "node_revoked"

// MaxSandboxesPerNode bounds the picture one link may build of its machine.
//
// The cache is grown by `changed`, which is an event: nothing acknowledges it,
// nothing bounds how many of them arrive, and the only two things that ever
// prune the map are a `gone` naming one sandbox and an inventory replacing it
// wholesale. A node that streams changes for synthetic names therefore costs
// the gateway a permanent SandboxRow per ~200 bytes on the wire, which ends
// with the process dying.
//
// The number is what a machine could honestly hold with room to spare: a node
// gives every guest its own rootfs on its own disk, so it runs out of hardware
// two orders of magnitude before it runs out of this.
//
// Both doors into the cache — the inventory request and the change event — treat
// passing it the same way: the report is not cached, the link is KEPT, and an
// operator is told at a bounded rate. That agreement is the whole point.
// Hanging up on the event door while merely refusing at the inventory door
// bricked the one machine most likely to be honest about holding this many: its
// inventory was refused, its first change event dropped the link, it redialed,
// and it did that forever, with nothing to see from the outside but one Error
// line per cycle. Refusing to cache is already the entire memory bound —
// dropping the link buys nothing on top of it, and costs every stream riding on
// that link.
const MaxSandboxesPerNode = 1024

// MaxReasonText bounds the pause reason a node may hand the gateway. It is the
// fragment in "sandbox %q %s — reconnect with: …", so it is measured against
// what a person reads on one line of a terminal, not against what fits in a
// frame: the sentences the tree actually sends are "was paused" and "went idle
// for 30m", and a peer with more to say than this is not describing a pause.
const MaxReasonText = 120

// Greeting is a link's first frame, read before any policy has run.
//
// It carries the reader as well as the message because reading a line off a
// stream buffers what came after it: continuing from the raw session instead of
// from here would silently drop every frame that arrived in the same packet as
// the hello.
type Greeting struct {
	Hello Hello

	// id is the hello's request ID. A refusal is a reply to it, which is what
	// makes the node's own Request return a typed error instead of timing out.
	id   string
	rest io.Reader
}

// ReadHello reads and parses a link's first frame.
//
// The timeout is enforced here rather than with a read deadline because an SSH
// session offers none: the read runs on its own goroutine and is abandoned on
// expiry, which is harmless only because the caller's next act on this path is
// to close the session — the read then ends and the goroutine finishes writing
// into a buffered channel nobody is listening to.
func ReadHello(ctx context.Context, r io.Reader, within time.Duration) (*Greeting, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	type result struct {
		line []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := readFrameLine(br)
		done <- result{line, err}
	}()

	var res result
	select {
	case res = <-done:
	case <-time.After(within):
		return nil, fmt.Errorf("nodelink: no hello within %s", within)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if res.err != nil {
		return nil, res.err
	}

	var f Frame
	if err := json.Unmarshal(res.line, &f); err != nil {
		return nil, fmt.Errorf("nodelink: malformed hello: %w", err)
	}
	if f.Type != TypeHello {
		return nil, fmt.Errorf("nodelink: first frame is %q, want %q", f.Type, TypeHello)
	}
	// A hello with no ID could not be answered — not with a welcome and not
	// with a refusal — so it is refused as a protocol error rather than left to
	// hang whatever is waiting on the other end.
	if f.ID == "" {
		return nil, errors.New("nodelink: hello is not a request (no id), so nothing can be replied to it")
	}
	g := &Greeting{id: f.ID, rest: br}
	if err := json.Unmarshal(f.Body, &g.Hello); err != nil {
		return nil, fmt.Errorf("nodelink: malformed hello body: %w", err)
	}
	return g, nil
}

// readFrameLine reads one newline-terminated frame with the same ceiling the
// framing itself applies. bufio.Reader is used rather than the decoder because
// its buffer survives the read: the Greeting hands it on to the Conn.
func readFrameLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > MaxFrameBytes {
			return nil, fmt.Errorf("nodelink: frame exceeds %d bytes", MaxFrameBytes)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return line, nil
	}
}

// Refusal builds the typed error a refused hello is answered with. The Kind
// comes from the code because these are the only refusals this door has: a
// pending or disabled node is denied, a name argument the roster disagrees with
// is a conflict, and anything malformed is invalid — the same three answers the
// control plane gives everywhere else, so a node renders them with the renderer
// it already has.
func Refusal(code, format string, a ...any) *ctlops.Error {
	msg := fmt.Sprintf(format, a...)
	switch code {
	case CodeNodePending, CodeNodeDisabled:
		return ctlops.Denied(OpLink, code, msg)
	case CodeNodeNameTaken, CodeNodeNameMismatch, CodeNodeEnrolFull:
		return &ctlops.Error{Kind: ctlops.KindConflict, Op: OpLink, Code: code, Msg: msg, Verbatim: true}
	default:
		return ctlops.Invalid(OpLink, code, "%s", msg)
	}
}

// Refuse answers a hello this gateway will not admit: the typed error as the
// reply to the hello, then a bye. Both, because the reply is what the node's
// code reads and the bye is what tells it the link is over rather than merely
// off to a bad start.
func Refuse(w io.Writer, g *Greeting, err *ctlops.Error) error {
	enc := newEncoder(w)
	if g != nil && g.id != "" {
		if werr := enc.encode(&Frame{ID: g.id, Type: TypeReply, Err: ctlops.ToWire(OpLink, err)}); werr != nil {
			return werr
		}
	}
	// Every bye names a reason. A fault the gateway could not classify — a
	// store that would not answer, say — has no refusal code, so it goes out
	// under its kind rather than under a code that would claim to be a refusal.
	code := err.Code
	if code == "" {
		code = err.Kind.String()
	}
	return encodeBye(enc, code, err.Msg)
}

// SendBye ends a link that never got as far as a hello. There is nothing to
// reply to at that point, but a machine reading frames still deserves a reason.
func SendBye(w io.Writer, code, msg string) error {
	return encodeBye(newEncoder(w), code, msg)
}

func encodeBye(enc *encoder, code, msg string) error {
	body, err := marshalBody(Bye{Code: code, Msg: msg})
	if err != nil {
		return err
	}
	return enc.encode(&Frame{Type: TypeBye, Body: body})
}

// Hooks is what the gateway integrator supplies. They are plain funcs rather
// than an interface from the layer above because this package must never import
// internal/fleet: the import DAG stays acyclic by the gateway half knowing
// nothing about what a fleet is.
//
// Event hooks run on the link's read goroutine and must not block. Request
// hooks run in Conn's bounded request pools with the caller's deadline, so
// signing and durable bookkeeping may block without stalling heartbeats.
type Hooks struct {
	OnInventory func(node string, inv InventoryMsg) InventoryAck
	OnHeartbeat func(node string, hb Heartbeat)
	OnChanged   func(node string, m ChangedMsg)
	OnGone      func(node string, m GoneMsg)
	OnPaused    func(node string, m PausedMsg)

	// The uplink handlers answer requests that travel node -> gateway. Each is
	// handed the AUTHENTICATED link name, exactly as
	// the event hooks are and for exactly the same reason: the node the
	// gateway will speak for must be the node that asked, never a name in a
	// payload.
	//
	// Nil means this deployment issues no workload identity — it has no OIDC
	// key — and the node is told so in a sentence rather than left waiting.
	OnIdentityToken  func(ctx context.Context, node string, req IdentityReq) (IdentityTokenResp, error)
	OnIdentityDoc    func(ctx context.Context, node string, req IdentityReq) (IdentityDocResp, error)
	OnSelfVisibility func(ctx context.Context, node string, req SelfVisibilityReq) (SelfVisibilityResp, error)
	OnSelfPort       func(ctx context.Context, node string, req SelfPortReq) (SelfPortResp, error)
	// The repo pair. Nil means this deployment attaches no repositories — no
	// store, or no GitHub App key — and, as above, the node is told that in a
	// sentence it can turn into a 501 rather than being left to infer it.
	OnSelfRepos         func(ctx context.Context, node string, req SelfReposReq) (SelfReposResp, error)
	OnSelfRepoCred      func(ctx context.Context, node string, req SelfRepoCredReq) (SelfRepoCredResp, error)
	OnSelfRepoAuthStart func(ctx context.Context, node string, req SelfRepoAuthStartReq) (SelfRepoAuthStartResp, error)
	OnSelfRepoAuthPoll  func(ctx context.Context, node string, req SelfRepoAuthPollReq) (SelfRepoAuthPollResp, error)
	// The lifecycle trio. OnSelfSnapshotPlan is a pure read and answers every
	// refusal a guest can act on; OnSelfPause and OnSelfSnapshot are the two
	// that stop the VM, and both return the instant the work is accepted — the
	// node's guest has already been told, and the box it would report to is
	// about to be paused.
	OnSelfPause        func(ctx context.Context, node string, req SelfPauseReq) (SelfPauseResp, error)
	OnSelfSnapshotPlan func(ctx context.Context, node string, req SelfSnapshotPlanReq) (SelfSnapshotPlanResp, error)
	OnSelfSnapshot     func(ctx context.Context, node string, req SelfSnapshotReq) (SelfSnapshotResp, error)
	// OnCertificateEnroll receives the same authenticated roster name. The CSR
	// payload deliberately has no identity field.
	OnCertificateEnroll func(ctx context.Context, node string, req CertificateEnrollRequest) (CertificateEnrollResponse, error)
}

// ServerOptions is one link's whole configuration. The door fills in the
// identity and the transport; whoever joins the node to the fleet fills in the
// hooks and calls Serve.
type ServerOptions struct {
	// Node is the AUTHENTICATED roster name. The name in the hello is advisory
	// and is never used for anything but a diagnostic: the row the key resolved
	// to is what this link is called, everywhere.
	Node string
	// Greeting is the already-read first frame. Reads continue from it, not
	// from Session, because it holds whatever was buffered behind the hello.
	Greeting *Greeting
	// Session is where frames are written. It is the same object the greeting
	// was read from; only the reading half has moved.
	Session io.ReadWriter
	// Stderr carries human diagnostics and is never parsed.
	Stderr io.Writer
	// Conn is the SSH connection data channels are opened on. Nil means this
	// link can carry control frames but no streams.
	Conn xssh.Conn
	// Welcome is the reply to the hello. Protocol, Node and HeartbeatSeconds
	// are filled in here so no caller can state a different answer to them.
	Welcome Welcome
	Hooks   Hooks
	Log     *slog.Logger
	// Metrics is optional and is deliberately independent of the SSH transport.
	Metrics *fleetmetrics.Registry

	// Grace, PingEvery and PingBudget default to the package constants. They
	// are settable so a test can drive the liveness machinery without waiting
	// on a real cadence.
	Grace      time.Duration
	PingEvery  time.Duration
	PingBudget time.Duration
	// Now is the clock Online is judged against; nil is time.Now.
	Now func() time.Time
}

// Client is the gateway's handle on one connected node: the framed control
// channel, the last thing that machine said about itself, and the ability to
// open a stream into one of its guests.
type Client struct {
	node       string
	hello      Hello
	conn       *Conn
	ssh        xssh.Conn
	split      bool
	generation string
	log        *slog.Logger
	hooks      Hooks
	metrics    *fleetmetrics.Registry

	grace  time.Duration
	now    func() time.Time
	cancel context.CancelFunc

	// over rate-limits what is said about a machine past MaxSandboxesPerNode.
	// The condition is persistent by nature — a node that holds too many holds
	// them until somebody moves them — so the line repeats slowly rather than
	// once, which is the difference between a fact an operator finds and one
	// that scrolled past at four in the morning.
	over busySignal

	mu       sync.Mutex
	lastSeen time.Time
	capacity host.NodeCapacity
	boxes    map[string]SandboxRow
	snaps    map[string]SnapshotRow

	// The live data streams, under their own lock.
	//
	// Their own lock and not c.mu because the busy path here is a stream
	// opening and closing, and c.mu is the lock Box and Capacity take — the
	// reads that sit under every authorization decision and every heartbeat.
	// A proxy under load must not be able to slow those down.
	//
	// The registry exists so a link that ends takes its streams with it and
	// SAYS SO, and the case it is really for is the one x/crypto cannot see.
	//
	// When the node's TCP connection closes cleanly, x/crypto's mux drops every
	// channel itself and a reader gets io.EOF: not a hang, and a truncated HTTP
	// body is still caught by its own framing above. The stream that genuinely
	// never errors is the one on a BLACK-HOLED link — no FIN, no reset, the mux
	// reading a socket that will never speak again. Nothing in x/crypto ends
	// that; the liveness prober does, by hanging up after two missed pings, and
	// hanging up is exactly the path that would otherwise leave every stream on
	// that link blocked in Read forever.
	//
	// So this is not decoration on the clean case. It is the whole answer to
	// the half-open one, and it is deterministic there precisely because
	// nothing else has touched those channels. Asking each channel to close is
	// not enough to end a read on a link nothing answers on, though — see
	// dropStreams, which records every reason and then closes the transport
	// under them.
	smu     sync.Mutex
	streams map[*streamConn]*dataLane
	lanes   map[string]*dataLane
	opening int
	dead    error
}

// overfullLogEvery is how often a link repeats that its machine holds more
// sandboxes than this gateway will track.
const overfullLogEvery = 5 * time.Minute

// tooManySandboxes is the operator's whole view of the ceiling, so it says what
// happened, what it cost, and what to do about it — and it says it again every
// overfullLogEvery for as long as the machine keeps reporting, because a
// condition that persists and is announced once is a condition nobody knows
// about.
//
// claimed is how many sandboxes the machine says it has in this report; example
// is one of the names that did not fit, which is what turns "too many" into
// something an operator can go and look at.
func (c *Client) tooManySandboxes(claimed int, example string) {
	ignored, window, say := c.over.record(c.now(), overfullLogEvery)
	if !say {
		return
	}
	c.mu.Lock()
	tracked := len(c.boxes)
	c.mu.Unlock()
	c.log.Error("this node reports more sandboxes than the gateway tracks for one machine; the ones past the ceiling are invisible to the fleet",
		"limit", MaxSandboxesPerNode,
		"claimed", claimed,
		"tracked", tracked,
		"ignored", ignored,
		"over", window.Round(time.Second),
		"example", example,
		"next", "the link is kept and everything already tracked keeps working: raise nodelink.MaxSandboxesPerNode and restart the gateway, or move sandboxes off this machine")
}

// Serve completes the handshake and returns the link plus the function that
// runs it. Splitting the two is what lets the caller register the node with
// whatever it keeps its fleet in *before* the first frame is dispatched, so no
// event can arrive against a machine nothing yet knows about.
//
// The returned wait function blocks until the link ends and returns nil on a
// clean end of stream: a node reconnecting is routine.
func Serve(ctx context.Context, opts ServerOptions) (*Client, func() error, error) {
	if opts.Node == "" {
		return nil, nil, errors.New("nodelink: a link needs the authenticated node name")
	}
	if opts.Greeting == nil {
		return nil, nil, errors.New("nodelink: a link needs the greeting it started with")
	}
	if opts.Session == nil {
		return nil, nil, errors.New("nodelink: a link needs a session to write frames to")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("node", opts.Node)
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	linkCtx, cancel := context.WithCancel(ctx)
	c := &Client{
		node:    opts.Node,
		hello:   opts.Greeting.Hello,
		ssh:     opts.Conn,
		log:     log,
		hooks:   opts.Hooks,
		metrics: opts.Metrics,
		grace:   orDuration(opts.Grace, DefaultGrace),
		now:     now,
		cancel:  cancel,
		boxes:   map[string]SandboxRow{},
		snaps:   map[string]SnapshotRow{},
		streams: map[*streamConn]*dataLane{},
		lanes:   map[string]*dataLane{},
	}
	c.conn = NewConn(opts.Greeting.rest, opts.Session, "g", log)
	c.conn.SetMetrics(opts.Metrics, opts.Node, "ssh")
	// One place ends the streams, whichever way the link ends: a bye, a revoke,
	// a superseding connection, the node's TCP dying, or this process shutting
	// down. All five cancel linkCtx, and none of them would otherwise reach
	// something the proxy is holding in an idle pool.
	//
	// Registered AFTER c.conn exists, because a linkCtx that is already done
	// runs this immediately and dropStreams closes the control channel before
	// it touches the transport.
	context.AfterFunc(linkCtx, func() { c.dropStreams(ErrLinkClosed) })
	// Handlers derive their work from the link's lifetime, not from a request's:
	// everything the gateway answers here is bookkeeping whose value dies with
	// the connection that asked for it.
	c.conn.SetBaseContext(linkCtx)
	// The hello itself is a sign of life, so the node is online from the moment
	// it is admitted rather than from its first heartbeat.
	c.seen()
	c.register()

	w := opts.Welcome
	w.Protocol = Protocol
	w.Node = opts.Node
	w.Capabilities = negotiatedCapabilities(opts.Greeting.Hello.Capabilities, w.Capabilities)
	if hasCapability(w.Capabilities, CapabilitySSHDataPoolV1) {
		generation, err := newControlGeneration()
		if err != nil {
			cancel()
			return nil, nil, err
		}
		c.split = true
		c.generation = generation
		c.ssh = nil
		w.ControlGeneration = generation
	} else {
		w.ControlGeneration = ""
	}
	if w.HeartbeatSeconds == 0 {
		w.HeartbeatSeconds = int(DefaultHeartbeat / time.Second)
	}
	c.conn.reply(&Frame{ID: opts.Greeting.id, Type: TypeHello}, w, nil)

	go c.pingLoop(linkCtx, orDuration(opts.PingEvery, DefaultPingEvery), orDuration(opts.PingBudget, DefaultPingBudget))

	wait := func() error {
		defer cancel()
		return c.conn.Serve(linkCtx)
	}
	return c, wait, nil
}

func orDuration(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// register wires the frames a node sends unprompted. Each one refreshes the
// liveness clock: any frame at all proves the machine is there, which is why
// the grace period is expressed in missed heartbeats rather than in silence.
func (c *Client) register() {
	c.conn.OnEvent(TypeHeartbeat, func(raw json.RawMessage) {
		var hb Heartbeat
		if err := json.Unmarshal(raw, &hb); err != nil {
			c.log.Warn("nodelink: malformed heartbeat", "err", err)
			return
		}
		c.mu.Lock()
		c.lastSeen = c.now()
		c.capacity = hb.Capacity
		c.mu.Unlock()
		if c.hooks.OnHeartbeat != nil {
			c.hooks.OnHeartbeat(c.node, hb)
		}
	})

	c.conn.Handle(TypeInventory, func(_ context.Context, raw json.RawMessage) (any, error) {
		var inv InventoryMsg
		if err := json.Unmarshal(raw, &inv); err != nil {
			return nil, ctlops.Invalid(OpLink, "bad_inventory", "that inventory could not be read: %v", err)
		}
		// The same ceiling the events are held to, and the same disposition:
		// nothing is cached and the link stands. An inventory is a request, so
		// the node also gets a sentence it can log — the old picture stays,
		// which is the conservative answer to a report this gateway cannot hold.
		if len(inv.Sandboxes) > MaxSandboxesPerNode {
			c.tooManySandboxes(len(inv.Sandboxes), inv.Sandboxes[MaxSandboxesPerNode].Name)
			return nil, ctlops.Invalid(OpLink, "inventory_too_large",
				"that inventory lists %d sandboxes; this gateway tracks at most %d per node, so it kept the picture it had.",
				len(inv.Sandboxes), MaxSandboxesPerNode)
		}
		c.ingest(inv)
		if c.hooks.OnInventory != nil {
			return c.hooks.OnInventory(c.node, inv), nil
		}
		return InventoryAck{}, nil
	})

	registerUplinkOps(c.conn, c.node, c.hooks)
	registerCertificateEnroll(c.conn, c.node, c.hooks)

	c.conn.OnEvent(TypeChanged, func(raw json.RawMessage) {
		var m ChangedMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			c.log.Warn("nodelink: malformed change event", "err", err)
			return
		}
		c.seen()
		c.mu.Lock()
		_, known := c.boxes[m.Sandbox.Name]
		full := !known && len(c.boxes) >= MaxSandboxesPerNode
		if !full {
			c.boxes[m.Sandbox.Name] = m.Sandbox
		}
		c.mu.Unlock()
		if full {
			// See MaxSandboxesPerNode. Not caching it is the memory bound, and
			// the hook is skipped with it: forwarding an event this link will
			// not remember would put a row in the fleet's index that no
			// Snapshot of this node ever mentions again.
			c.tooManySandboxes(MaxSandboxesPerNode+1, m.Sandbox.Name)
			return
		}
		if c.hooks.OnChanged != nil {
			c.hooks.OnChanged(c.node, m)
		}
	})

	c.conn.OnEvent(TypeGone, func(raw json.RawMessage) {
		var m GoneMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			c.log.Warn("nodelink: malformed gone event", "err", err)
			return
		}
		c.seen()
		c.mu.Lock()
		delete(c.boxes, m.Name)
		c.mu.Unlock()
		if c.hooks.OnGone != nil {
			c.hooks.OnGone(c.node, m)
		}
	})

	c.conn.OnEvent(TypePaused, func(raw json.RawMessage) {
		var m PausedMsg
		if err := json.Unmarshal(raw, &m); err != nil {
			c.log.Warn("nodelink: malformed pause event", "err", err)
			return
		}
		c.seen()
		// The one field on the link that is written verbatim to a human's
		// terminal: the gateway interpolates it into the goodbye it sends every
		// session attached to the sandbox, in raw mode (sshgw.hangUp). Scrubbed
		// here, at the boundary, for the same reason ctlops.FromWire scrubs the
		// prose in an error — a machine that is merely misconfigured should not
		// be able to garble somebody's terminal, and one that is compromised
		// should not be able to forge a line in it. It is scrubbed here rather
		// than at the far end because a pause event is consumed as prose and
		// never becomes a record: the display strings a node's rows carry —
		// state, image, login, the names on a template — reach a terminal
		// through fleet's remote adapter instead, and are clamped there, where
		// the gateway also knows which of them it authored itself. See
		// fleet.remoteNode.record.
		m.Reason = ctlops.SafeText(m.Reason, MaxReasonText)
		if c.hooks.OnPaused != nil {
			c.hooks.OnPaused(c.node, m)
		}
	})

	// A node that says goodbye is shutting down or has been superseded. Closing
	// here rather than waiting for the stream to end is what turns a planned
	// restart into an immediate offline instead of one that waits out the grace.
	c.conn.OnEvent(TypeBye, func(raw json.RawMessage) {
		var b Bye
		_ = json.Unmarshal(raw, &b)
		c.log.Info("node said goodbye", "code", b.Code, "msg", b.Msg)
		_ = c.Close()
	})
}

// ingest replaces the cached picture of a node wholesale. An inventory is the
// node's complete answer, so merging it with what came before would keep alive
// exactly the records it is telling us are gone.
func (c *Client) ingest(inv InventoryMsg) {
	boxes := make(map[string]SandboxRow, len(inv.Sandboxes))
	for _, b := range inv.Sandboxes {
		boxes[b.Name] = b
	}
	snaps := make(map[string]SnapshotRow, len(inv.Snapshots))
	for _, s := range inv.Snapshots {
		snaps[s.Name] = s
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSeen = c.now()
	c.boxes, c.snaps = boxes, snaps
	// An inventory carries capacity too, so a node that has just reconnected is
	// not reported as having none until its first heartbeat.
	c.capacity = inv.Capacity
}

func (c *Client) seen() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastSeen = c.now()
}

// pingLoop is the gateway's own liveness probe. A heartbeat only proves the
// node's writer works; a link can be dead in one direction only, and the side
// that is not being answered is the side that has to notice.
func (c *Client) pingLoop(ctx context.Context, every, budget time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		want := PingReq{Nonce: nonce()}
		reqCtx, cancel := context.WithTimeout(ctx, budget)
		var echo PingReq
		err := c.conn.Request(reqCtx, TypePing, want, &echo)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err == nil && echo.Nonce == want.Nonce {
			misses = 0
			c.seen()
			continue
		}
		misses++
		c.metrics.IncLivenessFailure(c.node, "ssh", "gateway")
		c.log.Warn("node did not answer a ping", "misses", misses, "err", err)
		// Two, not one: a single missed answer is a busy machine or a slow
		// network, and dropping a link costs every stream riding on it.
		if misses >= 2 {
			c.Hangup(CodeProtocolError, "no answer to two pings")
			return
		}
	}
}

// nonce correlates one message with its answer. It only has to be unlikely to
// repeat within a single link's lifetime — the reply is already matched by
// request ID — so the clock is enough and no entropy source is involved.
func nonce() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

// Name is the AUTHENTICATED roster name, which is the only name this node is
// ever known by on the gateway.
func (c *Client) Name() string { return c.node }

// Hello is what the node said about itself when it connected.
func (c *Client) Hello() Hello { return c.hello }

// Online reports whether this machine is answering: the link is up and it has
// said something within the grace period. Offline is never a verdict about the
// sandboxes on it — they are still running, on a machine that stopped talking.
func (c *Client) Online() bool {
	if c.conn.Err() != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.lastSeen.IsZero() && c.now().Sub(c.lastSeen) <= c.grace
}

// LastSeen is when this node last said anything.
func (c *Client) LastSeen() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSeen
}

// Capacity is the node's last reported resource picture, stamped with the
// authenticated name and the time it was reported. The stamp is not cosmetic:
// a node naming itself something else in its own capacity report would
// otherwise be attributed to whatever it claimed.
func (c *Client) Capacity() host.NodeCapacity {
	c.mu.Lock()
	defer c.mu.Unlock()
	reported := c.capacity
	reported.Node = c.node
	if !c.lastSeen.IsZero() {
		t := c.lastSeen
		reported.LastSeenAt = &t
	}
	return reported
}

// Box is one row from the node's last known inventory.
//
// It exists beside Snapshot because the two have different callers: a listing
// wants the whole picture, while an ownership check wants one name and sits
// under every authorized operation and every browser terminal request. Serving
// that from Snapshot would copy and sort up to MaxSandboxesPerNode rows per
// lookup.
func (c *Client) Box(name string) (SandboxRow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, ok := c.boxes[name]
	return row, ok
}

// NoteSnapshot and ForgetSnapshot fold a template this gateway just created or
// deleted on the node into its cached picture, rather than waiting for the next
// inventory.
//
// Without them a capture succeeds and then does not exist. The cached snapshot
// set is only ever written wholesale by ingest, which runs when a link comes up
// or when the gateway asks for a fresh inventory — and nothing asks after a
// capture. So `snapshot create` reported success while `snapshot ls` stayed
// empty and `snapshot bind` answered "no snapshot named ..." (masked
// not-found, which reads as a permissions bug) until the link happened to
// reconnect. On a deployment where every sandbox is remote, that is every
// capture there is.
//
// Sandbox rows never had this problem: the node streams sandbox.changed and
// TypeGone for those. Templates have no such event, and the gateway does not
// need one, because every mutation of a node's template set ORIGINATES here —
// Fleet.Snapshot and Fleet.DeleteSnapshot are the only two callers, and each
// already holds the answer it is recording. The gRPC transport has done exactly
// this since it was written (GRPCControl.putSnapshot / deleteSnapshot); this is
// the SSH transport catching up, and the asymmetry is why the bug was invisible
// until a cluster on the SSH transport captured its first template.
//
// Capped like the sandbox cache and for the same reason. A refusal is silent
// here rather than logged loudly: the create itself succeeded and the caller is
// about to hear so, and the next inventory reconciles it.
func (c *Client) NoteSnapshot(row SnapshotRow) {
	if row.Name == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, known := c.snaps[row.Name]; !known && len(c.snaps) >= MaxSandboxesPerNode {
		return
	}
	c.snaps[row.Name] = row
}

func (c *Client) ForgetSnapshot(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.snaps, name)
}

// Snapshot is the node's last known inventory, name-sorted so every listing
// built from it is stable whatever the map iteration does.
func (c *Client) Snapshot() ([]SandboxRow, []SnapshotRow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	boxes := make([]SandboxRow, 0, len(c.boxes))
	for _, b := range c.boxes {
		boxes = append(boxes, b)
	}
	snaps := make([]SnapshotRow, 0, len(c.snaps))
	for _, s := range c.snaps {
		snaps = append(snaps, s)
	}
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].Name < boxes[j].Name })
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Name < snaps[j].Name })
	return boxes, snaps
}

// Do sends a request and waits for its answer. The caller's context is the
// budget: it rides the wire as a deadline so the node gives up first.
func (c *Client) Do(ctx context.Context, typ string, body, out any) error {
	return c.conn.Request(ctx, typ, body, out)
}

// Cast sends something nobody will wait for. It reports nothing because its
// callers — the touch and key-fingerprint writes on every session teardown —
// have nothing to do with a failure and must not pay for a round trip.
func (c *Client) Cast(typ string, body any) {
	if err := c.conn.Event(typ, body); err != nil {
		c.log.Debug("nodelink: event not delivered", "type", typ, "err", err)
	}
}

// DialSandbox opens a stream to a port inside one of this node's guests. The
// request names a sandbox and never an address: the node re-resolves it against
// its own records, so nothing the gateway believes about where a guest lives can
// talk that machine into dialing something else.
// The stream is registered before it is returned, so that a link which dies
// while somebody is mid-transfer ends that transfer with a stated reason rather
// than with the io.EOF a closed SSH channel reads.
func (c *Client) DialSandbox(ctx context.Context, sandbox, kind string, port int) (out net.Conn, err error) {
	started := time.Now()
	metricKind := metricStreamKind(kind)
	metricTransport := "ssh"
	defer func() {
		c.metrics.ObserveStreamOpen(c.node, metricTransport, metricKind, metricOutcome(err), time.Since(started))
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.smu.Lock()
	if c.dead != nil {
		err := c.dead
		c.smu.Unlock()
		return nil, err
	}
	if len(c.streams)+c.opening >= MaxLiveStreams {
		c.smu.Unlock()
		return nil, errors.New("nodelink: stream limit")
	}
	lane := c.chooseDataLane()
	c.opening++
	transport := c.ssh
	if lane != nil {
		transport = lane.conn
		metricTransport = "ssh-data"
	}
	if transport == nil {
		c.opening--
		c.smu.Unlock()
		return nil, errors.New("nodelink: this link has no healthy data lanes")
	}
	c.smu.Unlock()

	s, err := openStream(transport, StreamOpen{
		Sandbox: sandbox,
		Kind:    kind,
		Port:    uint32(port), //nolint:gosec // a port is range-checked by the caller
		Nonce:   nonce(),
	})
	c.smu.Lock()
	c.opening--
	c.smu.Unlock()
	if err != nil {
		if lane != nil {
			c.smu.Lock()
			lane.active--
			c.smu.Unlock()
		}
		return nil, err
	}
	if err := c.track(s, lane); err != nil {
		return nil, err
	}
	s.setMetrics(c.metrics, c.node, metricTransport, metricKind)
	return s, nil
}

// track registers a live stream, or refuses it because the link is already
// gone. The refusal matters: OpenChannel on a connection that died a
// microsecond ago can still succeed against x/crypto's own bookkeeping, and a
// stream nobody will ever tear down is exactly the half-open connection this
// registry exists to prevent.
func (c *Client) track(s *streamConn, lane *dataLane) error {
	c.smu.Lock()
	if c.dead != nil {
		reason := c.dead
		if lane != nil {
			lane.active--
		}
		c.smu.Unlock()
		s.fail(reason)
		return reason
	}
	if lane != nil && c.lanes[lane.id] != lane {
		lane.active--
		c.smu.Unlock()
		err := errors.New("nodelink: data lane closed during stream open")
		s.fail(err)
		return err
	}
	// Set before the stream is reachable from the map, so dropStreams can never
	// race a half-initialised entry.
	s.released = func() { c.untrack(s) }
	c.streams[s] = lane
	c.smu.Unlock()
	c.metrics.AddLiveStreams(c.node, streamTransport(lane), metricStreamKind(s.kind), 1)
	return nil
}

func (c *Client) untrack(s *streamConn) {
	c.smu.Lock()
	lane, tracked := c.streams[s]
	delete(c.streams, s)
	if tracked && lane != nil {
		lane.active--
	}
	c.smu.Unlock()
	if tracked {
		c.metrics.AddLiveStreams(c.node, streamTransport(lane), metricStreamKind(s.kind), -1)
	}
}

func streamTransport(lane *dataLane) string {
	if lane != nil {
		return "ssh-data"
	}
	return "ssh"
}

// dropStreams ends every live stream with one reason and latches, so anything
// opened afterwards is refused rather than added to a registry nobody will
// visit again.
func (c *Client) dropStreams(reason error) {
	c.smu.Lock()
	if c.dead == nil {
		c.dead = reason
	}
	live := make([]*streamConn, 0, len(c.streams))
	for s, lane := range c.streams {
		live = append(live, s)
		c.metrics.AddLiveStreams(c.node, streamTransport(lane), metricStreamKind(s.kind), -1)
	}
	clear(c.streams)
	lanes := make([]*dataLane, 0, len(c.lanes))
	for _, lane := range c.lanes {
		lanes = append(lanes, lane)
	}
	clear(c.lanes)
	c.smu.Unlock()
	// Outside the lock: fail closes, close releases, and release wants this
	// lock. The map is already empty, so the release is a no-op — but a
	// self-deadlock that only fires when a link dies under load is not a bug
	// anyone finds twice.
	for _, s := range live {
		s.fail(reason)
	}
	for _, lane := range lanes {
		_ = lane.conn.Close()
	}
	// And then the transport, which is what actually unparks them.
	//
	// s.fail RECORDS the reason and then asks the channel to close, and
	// x/crypto's Channel.Close is only a request: it sends one CHANNEL_CLOSE
	// packet and returns. The routine that unblocks a goroutine parked in
	// Read — channel.close(), which runs pending.eof() — has exactly two
	// triggers, and neither of them is us calling Close. It runs when the PEER's
	// close arrives, or when the mux loop's read of the transport fails and it
	// drops every channel at once.
	//
	// On a live link the first trigger fires and everything above is enough. On
	// a BLACK-HOLED one — no FIN, no reset, the shape a wifi or tailnet
	// partition has — nothing answers, so no packet ever comes back and the mux
	// keeps happily reading a socket that will never speak again. That is
	// precisely the case this registry exists for (see the streams field), and
	// without this close it would be the case it did not handle: the reason
	// would be recorded and never delivered, because failure() is only consulted
	// after c.ch.Read returns. The read would stay parked forever, holding its
	// goroutine, its pooled http transport connection and its gliderlabs
	// session, with nothing else in the tree to end it — Close() closes the
	// framed control channel and not this connection, and the node door
	// deliberately runs without an idle timeout (sshgw.Gateway.Server).
	//
	// So the order here is load-bearing. Every reason is recorded first; only
	// then is the transport closed, so the io.EOF the mux hands each dropped
	// channel is substituted for the reason rather than reaching a caller as a
	// clean end of stream.
	//
	// Closing it is safe wherever this runs, because this runs in exactly one
	// place: the AfterFunc on the link's context, i.e. the link is already over
	// by every definition the rest of the package uses. The control session
	// rides the same connection and has nothing left to say.
	//
	// It does have something left to WRITE, though, and that is why the control
	// channel is shut down first rather than being cut off with the socket. The
	// last frame on a link is the bye that explains why it is ending — a
	// supersession, a revoke — and it is queued, not written inline (see
	// writer). Conn.Close is what gives that queue its flushGrace to reach the
	// peer, and its once means calling it here waits for the flush whether this
	// path or Client.Close got there first. Skipping it would replace every
	// stated goodbye with a socket that simply stopped, which is the diagnosis a
	// node then has to infer from a grace period.
	_ = c.conn.Close()
	if c.ssh != nil {
		_ = c.ssh.Close()
	}
}

// LiveStreams is how many data streams this link is carrying. It is here for
// the tests that assert a link tore its streams down, and for a future
// operator listing; nothing in the control plane branches on it.
func (c *Client) LiveStreams() int {
	c.smu.Lock()
	defer c.smu.Unlock()
	return len(c.streams)
}

// Hangup says why and then closes. The bye is best-effort: the usual reason for
// hanging up is that the far side has stopped listening.
func (c *Client) Hangup(code, msg string) {
	if err := c.conn.Event(TypeBye, Bye{Code: code, Msg: msg}); err != nil {
		c.log.Debug("nodelink: goodbye not delivered", "code", code, "err", err)
	}
	_ = c.Close()
}

// Revoke ends a link the gateway has withdrawn permission for, and states the
// reason to everything that was riding on it.
//
// Hangup alone would not do. It leaves every in-flight request to fail with
// ErrLinkClosed, a sentence about a socket, which a caller cannot tell apart
// from the machine simply dying — and the difference is the whole of what an
// operator wants said. Nothing else would have ended those waits either: the
// link deliberately carries no idle timeout. Failing the conn before closing it
// is what makes the answer typed, because Conn.Fail keeps the first reason it
// is given and so wins over the ErrLinkClosed the close records a moment later.
func (c *Client) Revoke(code string, reason error) {
	c.conn.Fail(reason)
	msg := ""
	if reason != nil {
		msg = reason.Error()
	}
	c.Hangup(code, msg)
}

// Close ends the link. It is idempotent, which matters because both the
// supersession path and the session teardown call it.
func (c *Client) Close() error {
	c.cancel()
	return c.conn.Close()
}
