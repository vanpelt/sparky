package nodelink

// The node's half of the lifecycle verbs, driven over a real loopback SSH link.
//
// What is under test here is not the manager — it is a fake, and its own
// behaviour is host's business — but the hop: that every verb reaches the right
// manager call with the arguments the gateway sent, that the record comes back
// intact, and above all that a refusal arrives on the far side as the SAME
// typed error the manager raised. That last property is what lets the SSH
// gateway and the browser terminal keep their errors.As switches without one
// line changed for a sandbox that happens to live on another machine.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// managerCall is one lifecycle call as the fake saw it: the manager method and
// the arguments it was handed, in order. Comparing whole calls rather than
// counting them is what catches a handler that transposes two same-typed
// fields — Rename's three strings, Fork's owner and name.
type managerCall struct {
	verb string
	args []any
}

// opsManager is fakeManager (client_test.go) with the lifecycle half a link now
// asks for. The reporting half stays where the inventory tests can see it; this
// type adds the recorder, the programmable refusal and the one knob that makes
// a handler outlive its deadline.
//
// Its own mutex is deliberately NOT called mu: fakeManager has one, and a
// shallower field of the same name would silently shadow it for every test that
// already locks the embedded fake.
type opsManager struct {
	*fakeManager

	opsMu    sync.Mutex
	calls    []managerCall
	failWith error
	// hang makes every context-carrying call wait for its context instead of
	// answering, which is how the deadline test observes the node giving up
	// first.
	hang bool
}

func (m *opsManager) record(verb string, args ...any) error {
	m.opsMu.Lock()
	m.calls = append(m.calls, managerCall{verb: verb, args: args})
	err := m.failWith
	m.opsMu.Unlock()
	return err
}

// waited blocks until ctx is done when the fake is in hang mode. It is called
// after record so the call is still observable.
func (m *opsManager) waited(ctx context.Context) error {
	m.opsMu.Lock()
	hang := m.hang
	m.opsMu.Unlock()
	if !hang {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (m *opsManager) took() []managerCall {
	m.opsMu.Lock()
	defer m.opsMu.Unlock()
	return append([]managerCall(nil), m.calls...)
}

func (m *opsManager) refuse(err error) {
	m.opsMu.Lock()
	defer m.opsMu.Unlock()
	m.failWith = err
}

// built is the record every creating verb answers with. The fields are echoed
// from the request so a test can tell a row that round-tripped from one the
// handler invented.
func built(name, owner, image string, vcpus, memMB int64) *host.Sandbox {
	return &host.Sandbox{
		Name: name, Owner: owner, Image: image, VCPUs: vcpus, MemMB: memMB,
		State: vmm.StateRunning, SSHUser: "sparky", DiskMB: 1024, DiskTotalMB: 25600,
		// Present on the record and absent from the wire on purpose: every node
		// mints its guests the same addresses, so one relayed up would resolve
		// to the gateway's own sandbox.
		HostIP: "172.30.9.2", SSHAddr: "172.30.9.2:22", GuestV6: "fd00::9",
		CreatedAt: time.Now().Add(-time.Minute), LastActive: time.Now(),
	}
}

func (m *opsManager) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	if err := m.record("create", name, owner, image, vcpus, memMB); err != nil {
		return nil, err
	}
	if err := m.waited(ctx); err != nil {
		return nil, err
	}
	return built(name, owner, image, vcpus, memMB), nil
}

func (m *opsManager) EnsureRunning(ctx context.Context, name string) (*host.Sandbox, error) {
	if err := m.record("ensure_running", name); err != nil {
		return nil, err
	}
	if err := m.waited(ctx); err != nil {
		return nil, err
	}
	return built(name, "alice", "ubuntu", 2, 2048), nil
}

func (m *opsManager) simple(ctx context.Context, verb string, args ...any) error {
	if err := m.record(verb, args...); err != nil {
		return err
	}
	return m.waited(ctx)
}

func (m *opsManager) Pause(ctx context.Context, name string) error {
	return m.simple(ctx, "pause", name)
}

func (m *opsManager) Archive(ctx context.Context, name string) error {
	return m.simple(ctx, "archive", name)
}

func (m *opsManager) Resize(ctx context.Context, name string, sizeMB int64) error {
	return m.simple(ctx, "resize", name, sizeMB)
}

func (m *opsManager) Reboot(ctx context.Context, name string) error {
	return m.simple(ctx, "reboot", name)
}

func (m *opsManager) Rename(ctx context.Context, oldName, newName, owner string) error {
	return m.simple(ctx, "rename", oldName, newName, owner)
}

func (m *opsManager) Destroy(ctx context.Context, name string) error {
	return m.simple(ctx, "destroy", name)
}

func (m *opsManager) SetPinned(name string, pinned bool) error {
	return m.record("set_pinned", name, pinned)
}

func (m *opsManager) ResyncEnv(ctx context.Context, name string) {
	_ = m.simple(ctx, "resync_env", name)
}

func (m *opsManager) Touch(name string) { _ = m.record("touch", name) }

func (m *opsManager) RecordKey(name, fp string) { _ = m.record("record_key", name, fp) }

func (m *opsManager) Snapshot(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error) {
	if err := m.record("snapshot", box, snapName, owner); err != nil {
		return nil, err
	}
	if err := m.waited(ctx); err != nil {
		return nil, err
	}
	return &host.Snapshot{
		Name: snapName, Owner: owner, Image: "snap-" + owner + "-" + snapName,
		FromBox: box, CreatedAt: time.Now(),
		// Node-authored and dropped by the projection: which machine holds a
		// template is decided by the link it arrived on.
		Node: "somewhere-else",
	}, nil
}

func (m *opsManager) DeleteSnapshot(ctx context.Context, snapName, owner string) error {
	return m.simple(ctx, "delete_snapshot", snapName, owner)
}

func (m *opsManager) Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error) {
	if err := m.record("fork", snapName, newName, owner, vcpus, memMB); err != nil {
		return nil, err
	}
	if err := m.waited(ctx); err != nil {
		return nil, err
	}
	return built(newName, owner, "snap-"+owner+"-"+snapName, vcpus, memMB), nil
}

// nodeOpsLink is a gateway and a node sharing a real SSH connection, with the
// node running the lifecycle handlers over a fake manager.
func nodeOpsLink(t *testing.T) (*linkPair, *opsManager) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	mgr := testManager()
	p := newLinkPair(t, linkOptions{
		nodeSetup: func(c *Conn) {
			pingHandler(c)
			registerOps(ctx, c, mgr, slog.New(slog.DiscardHandler))
		},
	})
	return p, mgr
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestNodeRunsEveryLifecycleVerb walks the whole catalogue. It is table-driven
// on purpose: the catalogue is derivable from fleet.Node, so a verb added there
// and forgotten here is one row, and a verb whose arguments are transposed on
// the way through is a failing want.
func TestNodeRunsEveryLifecycleVerb(t *testing.T) {
	p, mgr := nodeOpsLink(t)
	ctx := testCtx(t)

	cases := []struct {
		name  string
		typ   string
		body  any
		reply any
		want  managerCall
		check func(t *testing.T, reply any)
	}{
		{
			name: "create", typ: TypeCreate,
			body:  CreateReq{Name: "demo", Owner: "alice", Image: "ubuntu", VCPUs: 2, MemMB: 2048},
			reply: &SandboxResp{},
			want:  managerCall{"create", []any{"demo", "alice", "ubuntu", int64(2), int64(2048)}},
			check: func(t *testing.T, reply any) {
				got := reply.(*SandboxResp).Sandbox
				want := sandboxRow(built("demo", "alice", "ubuntu", 2, 2048))
				if got.Name != want.Name || got.Owner != want.Owner || got.Image != want.Image {
					t.Errorf("row identity = %+v, want %+v", got, want)
				}
				if got.VCPUs != want.VCPUs || got.MemMB != want.MemMB ||
					got.State != want.State || got.SSHUser != want.SSHUser ||
					got.DiskMB != want.DiskMB || got.DiskTotalMB != want.DiskTotalMB {
					t.Errorf("row = %+v, want %+v", got, want)
				}
			},
		},
		{
			name: "ensure_running", typ: TypeEnsureRunning,
			body:  NameReq{Name: "demo"},
			reply: &SandboxResp{},
			want:  managerCall{"ensure_running", []any{"demo"}},
			check: func(t *testing.T, reply any) {
				if got := reply.(*SandboxResp).Sandbox; got.Name != "demo" {
					t.Errorf("restored %q, want demo", got.Name)
				}
			},
		},
		{
			name: "pause", typ: TypePause, body: NameReq{Name: "demo"}, reply: &EmptyResp{},
			want: managerCall{"pause", []any{"demo"}},
		},
		{
			name: "archive", typ: TypeArchive, body: NameReq{Name: "demo"}, reply: &EmptyResp{},
			want: managerCall{"archive", []any{"demo"}},
		},
		{
			name: "resize", typ: TypeResize,
			body:  ResizeReq{Name: "demo", SizeMB: 40960},
			reply: &EmptyResp{},
			want:  managerCall{"resize", []any{"demo", int64(40960)}},
		},
		{
			name: "reboot", typ: TypeReboot, body: NameReq{Name: "demo"}, reply: &EmptyResp{},
			want: managerCall{"reboot", []any{"demo"}},
		},
		{
			name: "rename", typ: TypeRename,
			body:  RenameReq{Name: "demo", NewName: "demo2", Owner: "alice"},
			reply: &EmptyResp{},
			want:  managerCall{"rename", []any{"demo", "demo2", "alice"}},
		},
		{
			name: "destroy", typ: TypeDestroy, body: NameReq{Name: "demo"}, reply: &EmptyResp{},
			want: managerCall{"destroy", []any{"demo"}},
		},
		{
			name: "set_pinned", typ: TypeSetPinned,
			body:  PinReq{Name: "demo", Pinned: true},
			reply: &EmptyResp{},
			want:  managerCall{"set_pinned", []any{"demo", true}},
		},
		{
			name: "resync_env", typ: TypeResyncEnv, body: NameReq{Name: "demo"}, reply: &EmptyResp{},
			want: managerCall{"resync_env", []any{"demo"}},
		},
		{
			name: "snapshot.create", typ: TypeSnapshotCreate,
			body:  SnapshotReq{Sandbox: "demo", Snapshot: "base", Owner: "alice"},
			reply: &SnapshotResp{},
			want:  managerCall{"snapshot", []any{"demo", "base", "alice"}},
			check: func(t *testing.T, reply any) {
				got := reply.(*SnapshotResp).Snapshot
				if got.Name != "base" || got.Owner != "alice" || got.FromBox != "demo" {
					t.Errorf("template = %+v, want base/alice from demo", got)
				}
			},
		},
		{
			name: "snapshot.delete", typ: TypeSnapshotDelete,
			body:  DeleteSnapshotReq{Snapshot: "base", Owner: "alice"},
			reply: &EmptyResp{},
			want:  managerCall{"delete_snapshot", []any{"base", "alice"}},
		},
		{
			name: "snapshot.fork", typ: TypeSnapshotFork,
			body:  ForkReq{Snapshot: "base", Name: "clone", Owner: "alice", VCPUs: 1, MemMB: 512},
			reply: &SandboxResp{},
			want:  managerCall{"fork", []any{"base", "clone", "alice", int64(1), int64(512)}},
			check: func(t *testing.T, reply any) {
				if got := reply.(*SandboxResp).Sandbox; got.Name != "clone" || got.MemMB != 512 {
					t.Errorf("fork produced %+v, want clone at 512 MB", got)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(mgr.took())
			if err := p.gw.Request(ctx, tc.typ, tc.body, tc.reply); err != nil {
				t.Fatalf("%s: %v", tc.typ, err)
			}
			calls := mgr.took()
			if len(calls) != before+1 {
				t.Fatalf("%s made %d manager calls, want exactly one", tc.typ, len(calls)-before)
			}
			if got := calls[len(calls)-1]; !sameCall(got, tc.want) {
				t.Errorf("%s called %v, want %v", tc.typ, got, tc.want)
			}
			if tc.check != nil {
				tc.check(t, tc.reply)
			}
		})
	}
}

func sameCall(a, b managerCall) bool {
	if a.verb != b.verb || len(a.args) != len(b.args) {
		return false
	}
	for i := range a.args {
		if a.args[i] != b.args[i] {
			return false
		}
	}
	return true
}

// TestNodeRefusalsSurviveTheHop is the property the whole error taxonomy rests
// on: a machine's own refusal reaches the gateway as the same Go type, with the
// same sentence, the same exit code and the same HTTP status it would have had
// if the sandbox were on the gateway itself — and with the typed fields the
// renderers dereference still populated. sshgw.failStart reads
// limit.Running[0]; a LimitError that arrived as a map would panic the session
// that was only trying to explain itself.
func TestNodeRefusalsSurviveTheHop(t *testing.T) {
	p, mgr := nodeOpsLink(t)
	ctx := testCtx(t)

	cases := []struct {
		name string
		err  error
		as   func(t *testing.T, got error)
	}{
		{
			name: "limit",
			err:  &host.LimitError{Max: 3, Running: []string{"one", "two", "three"}},
			as: func(t *testing.T, got error) {
				var e *host.LimitError
				if !errors.As(got, &e) {
					t.Fatalf("got %T, want *host.LimitError", got)
				}
				if e.Max != 3 || len(e.Running) != 3 || e.Running[0] != "one" {
					t.Errorf("limit = %+v, want max 3 and the three running names", e)
				}
			},
		},
		{
			name: "capacity",
			err:  &host.CapacityError{RequestedMB: 4096, UsedMB: 60000, BudgetMB: 61440},
			as: func(t *testing.T, got error) {
				var e *host.CapacityError
				if !errors.As(got, &e) {
					t.Fatalf("got %T, want *host.CapacityError", got)
				}
				if e.BudgetMB != 61440 || e.UsedMB != 60000 || e.RequestedMB != 4096 {
					t.Errorf("capacity = %+v, want the three megabyte figures intact", e)
				}
			},
		},
		{
			name: "quota",
			err:  &host.DiskQuotaError{Owner: "alice", RequestedMB: 25600, UsedMB: 90000, PoolMB: 102400},
			as: func(t *testing.T, got error) {
				var e *host.DiskQuotaError
				if !errors.As(got, &e) {
					t.Fatalf("got %T, want *host.DiskQuotaError", got)
				}
				if e.Owner != "alice" || e.PoolMB != 102400 {
					t.Errorf("quota = %+v, want alice's 102400 MB pool", e)
				}
			},
		},
		{
			name: "missing",
			err:  &host.MissingError{Noun: "sandbox", Name: "ghost"},
			as: func(t *testing.T, got error) {
				var e *host.MissingError
				if !errors.As(got, &e) {
					t.Fatalf("got %T, want *host.MissingError", got)
				}
				if e.Noun != "sandbox" || e.Name != "ghost" {
					t.Errorf("missing = %+v, want sandbox ghost", e)
				}
			},
		},
		{
			name: "state",
			err:  &host.StateError{Code: "sandbox_archived", Msg: `sandbox "demo" is archived`},
			as: func(t *testing.T, got error) {
				var e *host.StateError
				if !errors.As(got, &e) {
					t.Fatalf("got %T, want *host.StateError", got)
				}
				if e.Code != "sandbox_archived" {
					t.Errorf("state code = %q, want sandbox_archived", e.Code)
				}
			},
		},
		{
			name: "disabled",
			err:  &host.DisabledError{Code: "archiving_disabled", Msg: "archiving isn't configured on this host."},
			as: func(t *testing.T, got error) {
				var e *host.DisabledError
				if !errors.As(got, &e) {
					t.Fatalf("got %T, want *host.DisabledError", got)
				}
			},
		},
		{
			name: "name",
			err:  &host.NameError{Problem: host.NameTaken, Noun: "sandbox", Name: "demo"},
			as: func(t *testing.T, got error) {
				var e *host.NameError
				if !errors.As(got, &e) {
					t.Fatalf("got %T, want *host.NameError", got)
				}
				if e.Problem != host.NameTaken || e.Name != "demo" {
					t.Errorf("name error = %+v, want a taken `demo`", e)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr.refuse(tc.err)
			defer mgr.refuse(nil)

			got := p.gw.Request(ctx, TypePause, NameReq{Name: "demo"}, &EmptyResp{})
			if got == nil {
				t.Fatal("a refused pause reported success")
			}

			// The comparison is against what this same error would have been
			// classified as locally, so the assertion cannot drift with the
			// taxonomy: both sides are computed from the same table.
			want, have := ctlops.AsError(TypePause, tc.err), ctlops.AsError(TypePause, got)
			if have.Msg != want.Msg || have.Kind != want.Kind || have.Code != want.Code {
				t.Errorf("across the link: %s/%s %q\nlocally:          %s/%s %q",
					have.Kind, have.Code, have.Msg, want.Kind, want.Code, want.Msg)
			}
			if have.ExitCode() != want.ExitCode() || have.HTTPStatus() != want.HTTPStatus() {
				t.Errorf("across the link exits %d / HTTP %d, locally %d / %d",
					have.ExitCode(), have.HTTPStatus(), want.ExitCode(), want.HTTPStatus())
			}
			tc.as(t, got)
		})
	}
}

// TestNodeGivesUpBeforeTheGatewayDoes pins the deadline half of the protocol.
//
// Conn.Request subtracts LinkMargin from the caller's remaining budget before
// it rides as deadline_ms, so the responder runs out of time first and the
// caller gets a typed answer rather than a timeout it has to interpret. The
// budget below is a hair over the margin, so the node's own deadline is tens of
// milliseconds while the gateway is still willing to wait seconds.
func TestNodeGivesUpBeforeTheGatewayDoes(t *testing.T) {
	p, mgr := nodeOpsLink(t)

	mgr.opsMu.Lock()
	mgr.hang = true
	mgr.opsMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), LinkMargin+150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := p.gw.Request(ctx, TypePause, NameReq{Name: "demo"}, &EmptyResp{})
	if err == nil {
		t.Fatal("a pause that never finished reported success")
	}
	if ctx.Err() != nil {
		t.Fatalf("the gateway's own budget expired first (%s); the node should have answered", time.Since(start))
	}
	e := ctlops.AsError(TypePause, err)
	if e.Kind != ctlops.KindInternal || e.Code != "timeout" {
		t.Fatalf("a node that ran out of time answered %s/%s (%q), want internal/timeout", e.Kind, e.Code, e.Msg)
	}
}

// TestTouchAndRecordKeyAreEventsNotRequests holds the line under §2.5's two
// events. They are the highest-frequency writes in the system — one per SSH
// session teardown, one per browser keystroke batch — and a reply would put a
// network round trip inside both. Asking for either as a request must therefore
// find no handler at all.
func TestTouchAndRecordKeyAreEventsNotRequests(t *testing.T) {
	p, mgr := nodeOpsLink(t)
	ctx := testCtx(t)

	cast(t, p.gw, TypeTouch, NameReq{Name: "demo"})
	cast(t, p.gw, TypeRecordKey, KeyReq{Name: "demo", KeyFP: "SHA256:abc"})

	waitForCalls(t, mgr, []managerCall{
		{"touch", []any{"demo"}},
		{"record_key", []any{"demo", "SHA256:abc"}},
	})

	for _, typ := range []string{TypeTouch, TypeRecordKey} {
		err := p.gw.Request(ctx, typ, NameReq{Name: "demo"}, nil)
		if err == nil {
			t.Fatalf("%s answered a request; it is an event and must have no handler", typ)
		}
		e := ctlops.AsError(typ, err)
		if e.Code != "unknown_request" {
			t.Errorf("%s as a request answered %s/%s, want unknown_request", typ, e.Kind, e.Code)
		}
	}
}

// cast sends an event with the test's error handling, so a queue that refused a
// frame fails the test rather than turning into a mysterious missing call.
func cast(t *testing.T, c *Conn, typ string, body any) {
	t.Helper()
	if err := c.Event(typ, body); err != nil {
		t.Fatalf("send %s: %v", typ, err)
	}
}

// waitForCalls polls until the fake has recorded the calls, because the two
// events are applied off the read goroutine by design and nothing acknowledges
// them.
func waitForCalls(t *testing.T, mgr *opsManager, want []managerCall) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := mgr.took()
		if len(got) >= len(want) {
			ok := true
			for i := range want {
				if !sameCall(got[i], want[i]) {
					ok = false
					break
				}
			}
			if ok {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %v; the node recorded %v", want, mgr.took())
}
