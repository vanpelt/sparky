package ctlops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// The real stores must keep satisfying these interfaces structurally — that is
// the entire justification for declaring them here rather than importing a
// concrete type, so a signature drift should fail the build of this package's
// tests rather than the integrator's.
var (
	_ Sandboxes = (*host.Manager)(nil)
	_ Templates = (*host.Manager)(nil)
	_ Accounts  = (*users.Store)(nil)
	_ Tagger    = (*secrets.Store)(nil)
	_ Schedules = (*schedule.Store)(nil)
	_ Routes    = (*routes.Store)(nil)
	_ Minter    = (*edgeauth.Signer)(nil)
)

// calls is the shared recorder every fake writes to. Ownership tests assert on
// it in the negative — the point is that a cross-owner request reaches NO store
// method at all, which a per-fake spy would let you check only one store at a
// time.
type calls struct {
	mu sync.Mutex
	ss []string
}

func (c *calls) add(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ss = append(c.ss, fmt.Sprintf(format, a...))
}

func (c *calls) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ss...)
}

func (c *calls) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ss = nil
}

// mutatingVerbs is every fake method that changes state or wakes a VM. The set
// is stated positively rather than as "everything except these reads" so that a
// fake gaining a new write method fails the ownership tests loudly instead of
// slipping through an out-of-date exclusion list.
var mutatingVerbs = map[string]bool{
	"Create": true, "EnsureRunning": true, "Pause": true, "Archive": true,
	"Resize": true, "Reboot": true, "Rename": true, "Destroy": true,
	"SetPinned": true, "ResyncEnv": true, "Touch": true, "Snapshot": true,
	"DeleteSnapshot": true, "Fork": true, "SetTags": true, "AddKey": true,
	"RemoveKey": true, "LinkGitHub": true, "SetEmail": true, "RemovePasskey": true,
	"NewInvite": true, "schedules.Add": true, "schedules.Delete": true,
	"SetVisibility": true, "Mint": true,
	"ApproveNode": true, "RemoveNode": true,
}

// mutating reports the recorded calls that could have changed state or woken a
// VM. Reads are fine on a masked path — the owner gate is itself a read — but a
// mutation means the gate ran too late.
func (c *calls) mutating() []string {
	var out []string
	for _, s := range c.all() {
		verb := s
		if i := strings.IndexByte(s, ' '); i > 0 {
			verb = s[:i]
		}
		if mutatingVerbs[verb] {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------

type fakeSandboxes struct {
	c         *calls
	boxes     map[string]*host.Sandbox
	archiving bool
	err       error // returned by every mutating method when set
}

func (f *fakeSandboxes) Get(name string) (*host.Sandbox, bool) {
	f.c.add("Get %s", name)
	b, ok := f.boxes[name]
	if !ok {
		return nil, false
	}
	cp := *b
	return &cp, true
}

func (f *fakeSandboxes) ListByOwner(owner string) []*host.Sandbox {
	f.c.add("ListByOwner %s", owner)
	var out []*host.Sandbox
	for _, b := range f.boxes {
		if b.Owner == owner {
			cp := *b
			out = append(out, &cp)
		}
	}
	return out
}

func (f *fakeSandboxes) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error) {
	f.c.add("Create %s owner=%s image=%s", name, owner, image)
	if f.err != nil {
		return nil, f.err
	}
	b := &host.Sandbox{
		Name: name, Owner: owner, Image: image, State: vmm.StateRunning,
		VCPUs: vcpus, MemMB: memMB, CreatedAt: time.Unix(0, 0).UTC(),
		SSHAddr: "127.0.0.1:2200", SSHUser: "sparky",
	}
	f.boxes[name] = b
	cp := *b
	return &cp, nil
}

func (f *fakeSandboxes) EnsureRunning(ctx context.Context, name string) (*host.Sandbox, error) {
	f.c.add("EnsureRunning %s", name)
	if f.err != nil {
		return nil, f.err
	}
	b, ok := f.boxes[name]
	if !ok {
		return nil, fmt.Errorf("sandbox %q not found", name)
	}
	b.State = vmm.StateRunning
	b.SSHAddr, b.SSHUser = "127.0.0.1:2200", "sparky"
	cp := *b
	return &cp, nil
}

func (f *fakeSandboxes) Pause(ctx context.Context, name string) error {
	f.c.add("Pause %s", name)
	if f.err != nil {
		return f.err
	}
	if b, ok := f.boxes[name]; ok {
		b.State, b.SSHAddr = vmm.StatePaused, ""
	}
	return nil
}

func (f *fakeSandboxes) Archive(ctx context.Context, name string) error {
	f.c.add("Archive %s", name)
	if f.err != nil {
		return f.err
	}
	if b, ok := f.boxes[name]; ok {
		b.State = vmm.StateArchived
	}
	return nil
}

func (f *fakeSandboxes) Resize(ctx context.Context, name string, sizeMB int64) error {
	f.c.add("Resize %s %d", name, sizeMB)
	return f.err
}

func (f *fakeSandboxes) Reboot(ctx context.Context, name string) error {
	f.c.add("Reboot %s", name)
	return f.err
}

func (f *fakeSandboxes) Rename(ctx context.Context, oldName, newName, owner string) error {
	f.c.add("Rename %s->%s owner=%s", oldName, newName, owner)
	if f.err != nil {
		return f.err
	}
	b, ok := f.boxes[oldName]
	if !ok {
		return fmt.Errorf("sandbox %q not found", oldName)
	}
	delete(f.boxes, oldName)
	b.Name = newName
	f.boxes[newName] = b
	return nil
}

func (f *fakeSandboxes) Destroy(ctx context.Context, name string) error {
	f.c.add("Destroy %s", name)
	if f.err != nil {
		return f.err
	}
	delete(f.boxes, name)
	return nil
}

func (f *fakeSandboxes) SetPinned(name string, pinned bool) error {
	f.c.add("SetPinned %s %v", name, pinned)
	if f.err != nil {
		return f.err
	}
	if b, ok := f.boxes[name]; ok {
		b.Pinned = pinned
	}
	return nil
}

func (f *fakeSandboxes) ResyncEnv(ctx context.Context, name string) { f.c.add("ResyncEnv %s", name) }
func (f *fakeSandboxes) Touch(name string)                          { f.c.add("Touch %s", name) }
func (f *fakeSandboxes) ArchivingEnabled() bool                     { return f.archiving }

// ---------------------------------------------------------------------------

type fakeTemplates struct {
	c     *calls
	snaps map[string]*host.Snapshot // keyed owner/name
	boxes *fakeSandboxes
	on    bool
	err   error
}

func (f *fakeTemplates) Snapshots(owner string) []*host.Snapshot {
	f.c.add("Snapshots %s", owner)
	var out []*host.Snapshot
	for _, s := range f.snaps {
		if s.Owner == owner {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out
}

func (f *fakeTemplates) Snapshot(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error) {
	f.c.add("Snapshot box=%s name=%s owner=%s", box, snapName, owner)
	if f.err != nil {
		return nil, f.err
	}
	s := &host.Snapshot{Name: snapName, Owner: owner, FromBox: box, CreatedAt: time.Unix(0, 0).UTC()}
	f.snaps[owner+"/"+snapName] = s
	cp := *s
	return &cp, nil
}

func (f *fakeTemplates) DeleteSnapshot(ctx context.Context, snapName, owner string) error {
	f.c.add("DeleteSnapshot %s owner=%s", snapName, owner)
	if f.err != nil {
		return f.err
	}
	delete(f.snaps, owner+"/"+snapName)
	return nil
}

func (f *fakeTemplates) Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error) {
	f.c.add("Fork %s->%s owner=%s", snapName, newName, owner)
	if f.err != nil {
		return nil, f.err
	}
	return f.boxes.Create(ctx, newName, owner, "snap-"+owner+"-"+snapName, vcpus, memMB)
}

func (f *fakeTemplates) Snapshotter() bool { return f.on }

// ---------------------------------------------------------------------------

type fakeAccounts struct {
	c        *calls
	users    map[string]users.User
	keys     map[string][]users.Key
	passkeys map[string][]users.Passkey
	invites  map[string]int
	err      error
	addErr   error
}

func (f *fakeAccounts) Get(handle string) (users.User, error) {
	f.c.add("accounts.Get %s", handle)
	u, ok := f.users[handle]
	if !ok {
		return users.User{}, errors.New("no such user")
	}
	return u, nil
}

func (f *fakeAccounts) Keys(handle string) ([]users.Key, error) {
	f.c.add("Keys %s", handle)
	return f.keys[handle], nil
}

func (f *fakeAccounts) AddKey(handle string, key xssh.PublicKey, label, via string) error {
	f.c.add("AddKey %s via=%s", handle, via)
	if f.addErr != nil {
		return f.addErr
	}
	fp := xssh.FingerprintSHA256(key)
	for _, k := range f.keys[handle] {
		if k.FP == fp {
			return nil // AddKey is idempotent
		}
	}
	f.keys[handle] = append(f.keys[handle], users.Key{
		FP: fp, Label: label, Via: via, AddedAt: time.Unix(0, 0).UTC(),
		AuthorizedKey: string(xssh.MarshalAuthorizedKey(key)),
	})
	return nil
}

func (f *fakeAccounts) RemoveKey(handle, fp string) error {
	f.c.add("RemoveKey %s %s", handle, fp)
	if f.err != nil {
		return f.err
	}
	ks := f.keys[handle]
	for i, k := range ks {
		if k.FP == fp {
			if len(ks) <= 1 {
				return users.ErrLastKey
			}
			f.keys[handle] = append(ks[:i:i], ks[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("no key %s on this account", fp)
}

func (f *fakeAccounts) LinkGitHub(handle, login string) error {
	f.c.add("LinkGitHub %s %s", handle, login)
	if f.err != nil {
		return f.err
	}
	u := f.users[handle]
	now := time.Unix(0, 0).UTC()
	u.GitHubLogin, u.GitHubVerifiedAt = login, &now
	f.users[handle] = u
	return nil
}

func (f *fakeAccounts) SetEmail(handle, email string) error {
	f.c.add("SetEmail %s %s", handle, email)
	if f.err != nil {
		return f.err
	}
	u := f.users[handle]
	u.Email = email
	f.users[handle] = u
	return nil
}

func (f *fakeAccounts) Passkeys(handle string) ([]users.Passkey, error) {
	f.c.add("Passkeys %s", handle)
	return f.passkeys[handle], nil
}

func (f *fakeAccounts) RemovePasskey(handle, idPrefix string) error {
	f.c.add("RemovePasskey %s %s", handle, idPrefix)
	var hits []users.Passkey
	for _, p := range f.passkeys[handle] {
		if strings.HasPrefix(p.ID, idPrefix) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 0:
		return users.ErrNoSuchPasskey
	case 1:
		return nil
	default:
		return users.ErrAmbiguousPasskey
	}
}

func (f *fakeAccounts) NewInvite(createdBy string) (string, error) {
	f.c.add("NewInvite %s", createdBy)
	if f.err != nil {
		return "", f.err
	}
	f.invites[createdBy]++
	return "invite-code", nil
}

func (f *fakeAccounts) InviteCount(handle string) (int, error) {
	f.c.add("InviteCount %s", handle)
	return f.invites[handle], nil
}

// ---------------------------------------------------------------------------

type fakeTagger struct {
	c    *calls
	tags map[string][]string
	err  error
}

func (f *fakeTagger) TagsFor(sandbox string) ([]string, error) {
	f.c.add("TagsFor %s", sandbox)
	return f.tags[sandbox], nil
}

func (f *fakeTagger) SetTags(sandbox, owner string, tags []string) error {
	f.c.add("SetTags %s owner=%s tags=%v", sandbox, owner, tags)
	if f.err != nil {
		return f.err
	}
	if len(tags) == 0 {
		delete(f.tags, sandbox)
		return nil
	}
	f.tags[sandbox] = append([]string(nil), tags...)
	return nil
}

// ---------------------------------------------------------------------------

type fakeSchedules struct {
	c       *calls
	entries map[string]schedule.Entry
	err     error
	next    int
}

func (f *fakeSchedules) Add(e schedule.Entry) (schedule.Entry, error) {
	f.c.add("schedules.Add sandbox=%s owner=%s spec=%s", e.Sandbox, e.Owner, e.Spec)
	if f.err != nil {
		return schedule.Entry{}, f.err
	}
	f.next++
	e.ID = fmt.Sprintf("sch%d", f.next)
	e.CreatedAt = time.Unix(0, 0).UTC()
	f.entries[e.ID] = e
	return e, nil
}

func (f *fakeSchedules) Get(id string) (schedule.Entry, error) {
	f.c.add("schedules.Get %s", id)
	e, ok := f.entries[id]
	if !ok {
		return schedule.Entry{}, schedule.ErrNotFound
	}
	return e, nil
}

func (f *fakeSchedules) ListByOwner(owner string) ([]schedule.Entry, error) {
	f.c.add("schedules.ListByOwner %s", owner)
	var out []schedule.Entry
	for _, e := range f.entries {
		if e.Owner == owner {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeSchedules) Delete(id string) error {
	f.c.add("schedules.Delete %s", id)
	if f.err != nil {
		return f.err
	}
	delete(f.entries, id)
	return nil
}

// ---------------------------------------------------------------------------

type fakeRoutes struct {
	c   *calls
	rs  map[string][]routes.Route // by sandbox
	err error
}

func (f *fakeRoutes) ListBySandbox(sandbox string) ([]routes.Route, error) {
	f.c.add("ListBySandbox %s", sandbox)
	return f.rs[sandbox], nil
}

func (f *fakeRoutes) SetVisibility(subdomain, visibility string) error {
	f.c.add("SetVisibility %s %s", subdomain, visibility)
	if f.err != nil {
		return f.err
	}
	for box, list := range f.rs {
		for i := range list {
			if list[i].Subdomain == subdomain {
				f.rs[box][i].Visibility = visibility
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------

type fakeMinter struct {
	c   *calls
	err error
}

func (f *fakeMinter) Mint(id edgeauth.Identity, ttl time.Duration) (string, time.Time, error) {
	f.c.add("Mint %s ttl=%s", id.Handle, ttl)
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return "spk_v1.fake", time.Unix(0, 0).UTC().Add(ttl), nil
}

// ---------------------------------------------------------------------------

// fakeNodes is a roster in a slice. The default rig has none — a host with no
// fleet is the shape most of this suite asserts against — so a node test asks
// for one with rig.withNodes().
type fakeNodes struct {
	c    *calls
	list []NodeInfo
	err  error
}

func (f *fakeNodes) ListNodes() ([]NodeInfo, error) {
	f.c.add("ListNodes")
	if f.err != nil {
		return nil, f.err
	}
	return append([]NodeInfo(nil), f.list...), nil
}

func (f *fakeNodes) ApproveNode(fp, by string) (NodeInfo, error) {
	f.c.add("ApproveNode %s by=%s", fp, by)
	if f.err != nil {
		return NodeInfo{}, f.err
	}
	for i := range f.list {
		if f.list[i].FP != "" && f.list[i].FP == fp {
			f.list[i].Status = "approved"
			f.list[i].ApprovedBy = by
			return f.list[i], nil
		}
	}
	return NodeInfo{}, errors.New("no such node")
}

func (f *fakeNodes) RemoveNode(name string) error {
	f.c.add("RemoveNode %s", name)
	if f.err != nil {
		return f.err
	}
	for i := range f.list {
		if f.list[i].Name == name {
			f.list = append(f.list[:i:i], f.list[i+1:]...)
			return nil
		}
	}
	return errors.New("no such node")
}

// ---------------------------------------------------------------------------

type fakeGitHub struct {
	c      *calls
	keys   map[string][]xssh.PublicKey
	listed bool
	err    error
}

func (f *fakeGitHub) Fetch(ctx context.Context, login string) ([]xssh.PublicKey, error) {
	f.c.add("github.Fetch %s", login)
	if f.err != nil {
		return nil, f.err
	}
	return f.keys[login], nil
}

func (f *fakeGitHub) Verify(ctx context.Context, login string, key xssh.PublicKey) (bool, error) {
	f.c.add("github.Verify %s", login)
	if f.err != nil {
		return false, f.err
	}
	return f.listed, nil
}

// ---------------------------------------------------------------------------

// rig is the whole package under test with every dependency faked: no sqlite,
// no temp dir, no VM driver, so the suite runs in milliseconds.
type rig struct {
	ops    *Ops
	calls  *calls
	boxes  *fakeSandboxes
	tmpl   *fakeTemplates
	accts  *fakeAccounts
	tagger *fakeTagger
	sched  *fakeSchedules
	routes *fakeRoutes
	minter *fakeMinter
	github *fakeGitHub
	nodes  *fakeNodes
}

// withNodes turns the rig's host into a fleet gateway holding one approved,
// online machine and one that has enrolled and is waiting. Assigning the field
// directly is what parse_test.go already does to take a store away; this does
// it in the other direction so the default rig stays a single box.
func (r *rig) withNodes() *fakeNodes {
	n := &fakeNodes{c: r.calls, list: []NodeInfo{
		{Name: "here", Status: "approved", Online: true, Local: true, Arch: "arm64", Sandboxes: 1},
		{Name: "node-b", Status: "approved", Online: true, FP: fpNodeB, Arch: "amd64", Sandboxes: 2},
		{Name: "newcomer", Status: "pending", FP: fpNewcomer},
	}}
	r.nodes = n
	r.ops.nodes = n
	return n
}

// The roster fixture's fingerprints, full length. ApproveNode checks the shape
// of what it is given before it looks anything up — a prefix is not a
// fingerprint, and neither is a name — so a short stand-in here would be
// rejected as malformed long before it reached the roster, and every node test
// would be asserting against the wrong refusal.
const (
	fpNodeB    = "SHA256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fpNewcomer = "SHA256:ccccccccccccccccccccccccccccccccccccccccccc"
	// fpNobody is well-formed and belongs to no row in the fixture.
	fpNobody = "SHA256:ddddddddddddddddddddddddddddddddddddddddddd"
)

// testKey is a fixed public key so fingerprints are stable across runs.
const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJmoo1J5B5cKpVXFRc2A7lZ5m6BqDkVL1kJvbjRJgqQK alice@laptop"

func mustKey(t *testing.T, line string) (xssh.PublicKey, string) {
	t.Helper()
	k, _, _, _, err := xssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	return k, xssh.FingerprintSHA256(k)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	c := &calls{}
	now := time.Unix(0, 0).UTC()
	key, fp := mustKey(t, testKey)

	boxes := &fakeSandboxes{c: c, archiving: true, boxes: map[string]*host.Sandbox{
		"alicebox": {
			Name: "alicebox", Owner: "alice", State: vmm.StateRunning, VCPUs: 2, MemMB: 8192,
			CreatedAt: now, LastActive: now, SSHAddr: "127.0.0.1:2200", SSHUser: "sparky",
		},
	}}
	tmpl := &fakeTemplates{c: c, on: true, boxes: boxes, snaps: map[string]*host.Snapshot{
		"alice/alicesnap": {Name: "alicesnap", Owner: "alice", FromBox: "alicebox", CreatedAt: now},
	}}
	accts := &fakeAccounts{
		c: c,
		users: map[string]users.User{
			"alice":   {Handle: "alice", Status: "active", InvitedBy: "opsy"},
			"mallory": {Handle: "mallory", Status: "active", InvitedBy: "opsy"},
			"opsy":    {Handle: "opsy", Status: "active", InvitedBy: users.OperatorInviter},
		},
		keys: map[string][]users.Key{
			"alice": {{FP: fp, Label: "laptop", Via: "seed", AddedAt: now,
				AuthorizedKey: string(xssh.MarshalAuthorizedKey(key))}},
		},
		passkeys: map[string][]users.Passkey{
			"alice": {{ID: "abc123", Handle: "alice", Label: "MacBook", CreatedAt: now}},
		},
		invites: map[string]int{},
	}
	tagger := &fakeTagger{c: c, tags: map[string][]string{}}
	sched := &fakeSchedules{c: c, entries: map[string]schedule.Entry{
		"sch-alice": {ID: "sch-alice", Sandbox: "alicebox", Owner: "alice",
			Spec: "*/30 * * * *", Command: "make", CreatedAt: now},
	}}
	rt := &fakeRoutes{c: c, rs: map[string][]routes.Route{
		"alicebox": {{Subdomain: "alicebox", Sandbox: "alicebox", Owner: "alice",
			Port: 8000, Visibility: routes.VisibilityPrivate}},
	}}
	minter := &fakeMinter{c: c}
	gh := &fakeGitHub{c: c, keys: map[string][]xssh.PublicKey{}}

	ops := New(Config{
		Sandboxes: boxes, Templates: tmpl, Accounts: accts,
		Tags: tagger, Schedules: sched, Routes: rt, Sessions: minter, GitHub: gh,
		DefaultImage: "base", Domain: "example.test", XtermSubdomain: "xterm",
		InvitesPerUser: 0,
		NewName:        func() string { return "generated-name" },
		Now:            func() time.Time { return now },
		Log:            slog.New(slog.DiscardHandler),
	})
	t.Cleanup(ops.Close)

	return &rig{ops: ops, calls: c, boxes: boxes, tmpl: tmpl, accts: accts,
		tagger: tagger, sched: sched, routes: rt, minter: minter, github: gh}
}

func alice() Caller   { return Caller{Handle: "alice"} }
func mallory() Caller { return Caller{Handle: "mallory"} }
