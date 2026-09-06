package ctlops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netrules"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/templates"
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
	_ Secrets   = (*secrets.Store)(nil)
	_ Schedules = (*schedule.Store)(nil)
	_ Routes    = (*routes.Store)(nil)
	_ Minter    = (*edgeauth.Signer)(nil)

	_ TemplateBindings = (*templates.Store)(nil)

	// The environment surface, asserted the same way and for the same reason.
	// EnvVars and SecretTags are both satisfied by the ONE secrets store: the
	// plain vars and the encrypted secrets share a database file, a tag
	// namespace and an /etc/environment block, and splitting them into two
	// stores would mean two answers to "what does this sandbox see".
	_ Environments = (*envs.Store)(nil)
	_ EnvVars      = (*secrets.Store)(nil)
	_ SecretTags   = (*secrets.Store)(nil)
	_ NetRules     = (*netrules.Store)(nil)
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

// has reports whether an exact recorded call is present.
func (c *calls) has(call string) bool {
	for _, s := range c.all() {
		if s == call {
			return true
		}
	}
	return false
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
	"Checkpoint": true, "RestoreCheckpoint": true,
	"Resize": true, "Reboot": true, "Rename": true, "Destroy": true,
	"SetPinned": true, "ResyncEnv": true, "ResyncRepos": true, "Touch": true, "Snapshot": true,
	"DeleteSnapshot": true, "Fork": true, "SetTags": true, "AddKey": true,
	"RemoveKey": true, "LinkGitHub": true, "SetEmail": true, "RemovePasskey": true,
	"NewInvite": true, "schedules.Add": true, "schedules.Delete": true,
	"SetVisibility": true, "Mint": true,
	"ApproveNode": true, "RemoveNode": true,
	"accounts.Create": true, "secrets.Put": true, "secrets.Delete": true,
	// The binding verbs. Omitting them here would not fail anything visibly —
	// it would quietly drop the new verbs out of every ownership assertion,
	// since mutating() only reports what this table names.
	"Bind": true, "Unbind": true,
	// The template-port verbs, for the same reason. Upsert is the route write
	// that points a new sandbox at the port its template was captured on.
	"SetSnapshotPort": true, "ForgetSnapshotPort": true, "Upsert": true,
	// The environment verbs. Named here for the reason the binding verbs are:
	// mutating() only reports what this table lists, so a write left out of it
	// silently drops out of every ownership assertion in the package.
	"envs.Put": true, "envs.Delete": true, "envs.SetScript": true, "envs.SetState": true,
	// The build nudge. It runs a script inside somebody's guest, which is the
	// most consequential thing on this list, so it must be visible to every
	// ownership assertion in the package.
	"StartSetup": true,
	"vars.Put":   true, "vars.Delete": true, "vars.DeleteForTag": true,
	"netrules.Put": true, "secrets.Retag": true, "repos.Put": true,
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
	c           *calls
	boxes       map[string]*host.Sandbox
	archiving   bool
	err         error // returned by every mutating method when set
	repoSyncErr error // returned by ResyncRepos when set
	// destroyErr fails only the destroy, which is the one half-failure an
	// environment build treats as non-fatal: the disk exists and the tag points
	// at it, so a leftover box must not turn a finished build into a failed one.
	destroyErr error
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

func (f *fakeSandboxes) EnsureReady(ctx context.Context, name string) (*host.Sandbox, error) {
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
	if f.destroyErr != nil {
		return f.destroyErr
	}
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

// ResyncRepos is reached by type assertion rather than through the Sandboxes
// interface (see Ops.syncRepos), so this method is the whole seam. repoSyncErr
// is what a caller's guest returns; nil is the ordinary "it took the job".
func (f *fakeSandboxes) ResyncRepos(ctx context.Context, name string) error {
	f.c.add("ResyncRepos %s", name)
	return f.repoSyncErr
}

func (f *fakeSandboxes) AwaitEnv(ctx context.Context, name string) error {
	f.c.add("AwaitEnv %s", name)
	return nil
}
func (f *fakeSandboxes) MarkActive(name string) { f.c.add("Touch %s", name) }
func (f *fakeSandboxes) ArchivingEnabled() bool { return f.archiving }

// ---------------------------------------------------------------------------

type fakeCheckpoints struct {
	c       *calls
	enabled map[string]bool
	err     error
}

func (f *fakeCheckpoints) Enabled(name string) bool {
	return f != nil && f.enabled[name]
}

func (f *fakeCheckpoints) Checkpoint(_ context.Context, name string) error {
	f.c.add("Checkpoint %s", name)
	return f.err
}

func (f *fakeCheckpoints) RestoreCheckpoint(_ context.Context, name string) error {
	f.c.add("RestoreCheckpoint %s", name)
	return f.err
}

// ---------------------------------------------------------------------------

type fakeTemplates struct {
	c     *calls
	snaps map[string]*host.Snapshot // keyed owner/name
	boxes *fakeSandboxes
	on    bool
	err   error
	// node is stamped onto every capture, the way host.Manager stamps its own
	// name (snapshot.go). Placement tests set it, or re-stamp a fixture record
	// directly, to say which machine holds a template.
	node string
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
	s := &host.Snapshot{
		Name: snapName, Owner: owner, FromBox: box, Node: f.node,
		// The real manager derives the template basename from (owner, name) and
		// stores it on the record; resolveTemplate reads it back rather than
		// recomputing the rule, so the fake has to carry it too.
		Image:     "snap-" + owner + "-" + snapName,
		CreatedAt: time.Unix(0, 0).UTC(),
	}
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
		// The real store's sentinel, not a look-alike: user.go's provisioning
		// path distinguishes "no such account" from a store failure with
		// errors.Is, and a fake that only matched by message would let that
		// branch rot untested.
		return users.User{}, users.ErrNoSuchUser
	}
	return u, nil
}

// Create registers an account the way users.Store does, including the two
// refusals provisioning has to handle: a taken handle, and a key some other
// account already claims.
func (f *fakeAccounts) CreateKeyless(handle, invitedBy string) error {
	f.c.add("accounts.CreateKeyless %s by=%s", handle, invitedBy)
	if f.err != nil {
		return f.err
	}
	if _, taken := f.users[handle]; taken {
		return users.ErrHandleTaken
	}
	if f.users == nil {
		f.users = map[string]users.User{}
	}
	f.users[handle] = users.User{
		Handle: handle, Status: users.StatusActive, InvitedBy: invitedBy,
		CreatedAt: time.Unix(0, 0).UTC(),
	}
	return nil
}

func (f *fakeAccounts) Create(handle string, key xssh.PublicKey, label, via, invitedBy string) error {
	f.c.add("accounts.Create %s via=%s by=%s", handle, via, invitedBy)
	if f.err != nil {
		return f.err
	}
	if _, taken := f.users[handle]; taken {
		return users.ErrHandleTaken
	}
	fp := xssh.FingerprintSHA256(key)
	for other, ks := range f.keys {
		for _, k := range ks {
			if k.FP == fp && other != handle {
				return users.ErrKeyLinked
			}
		}
	}
	if f.users == nil {
		f.users = map[string]users.User{}
	}
	f.users[handle] = users.User{
		Handle: handle, Status: users.StatusActive, InvitedBy: invitedBy,
		CreatedAt: time.Unix(0, 0).UTC(),
	}
	if f.keys == nil {
		f.keys = map[string][]users.Key{}
	}
	f.keys[handle] = append(f.keys[handle], users.Key{
		FP: fp, Label: label, Via: via, AddedAt: time.Unix(0, 0).UTC(),
		AuthorizedKey: string(xssh.MarshalAuthorizedKey(key)),
	})
	return nil
}

func (f *fakeAccounts) List() ([]users.User, error) {
	f.c.add("accounts.List")
	if f.err != nil {
		return nil, f.err
	}
	out := make([]users.User, 0, len(f.users))
	for _, u := range f.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out, nil
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

func (f *fakeAccounts) LinkGitHub(handle, login, via string, id int64) error {
	f.c.add("LinkGitHub %s %s %s %d", handle, login, via, id)
	if f.err != nil {
		return f.err
	}
	u := f.users[handle]
	now := time.Unix(0, 0).UTC()
	u.GitHubLogin, u.GitHubVerifiedAt = login, &now
	u.GitHubVia, u.GitHubID = via, id
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
	c     *calls
	rs    map[string][]routes.Route     // by sandbox
	ports map[string][]routes.PortRoute // non-default ports, by subdomain
	err   error
}

func (f *fakeRoutes) ListBySandbox(sandbox string) ([]routes.Route, error) {
	f.c.add("ListBySandbox %s", sandbox)
	return f.rs[sandbox], nil
}

// GetBySubdomain scans the by-sandbox map, since the fake's index is the
// sandbox and the store's is the subdomain. Small enough that a second index
// would be more bookkeeping than the tests are worth.
func (f *fakeRoutes) GetBySubdomain(subdomain string) (routes.Route, bool, error) {
	f.c.add("GetBySubdomain %s", subdomain)
	if f.err != nil {
		return routes.Route{}, false, f.err
	}
	for _, list := range f.rs {
		for _, r := range list {
			if r.Subdomain == subdomain {
				return r, true, nil
			}
		}
	}
	return routes.Route{}, false, nil
}

// Upsert reproduces the one behaviour ctlops leans on: an existing row has its
// port replaced and everything else — visibility above all — left alone. See
// routes.Store.Upsert, where that is enforced by the ON CONFLICT clause.
func (f *fakeRoutes) Upsert(r routes.Route) error {
	f.c.add("Upsert %s -> %s:%d", r.Subdomain, r.Sandbox, r.Port)
	if f.err != nil {
		return f.err
	}
	for box, list := range f.rs {
		for i := range list {
			if list[i].Subdomain == r.Subdomain {
				f.rs[box][i].Port = r.Port
				return nil
			}
		}
	}
	if r.Visibility == "" {
		r.Visibility = routes.VisibilityPrivate
	}
	f.rs[r.Sandbox] = append(f.rs[r.Sandbox], r)
	return nil
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

// The per-port half. ports is keyed by subdomain and holds only the NON-default
// ports, exactly as routes.Store's route_ports table does — the route's own
// port lives on the Route above, and SetPortVisibility writes through to it.
func (f *fakeRoutes) ListPortsBySandbox(sandbox string) ([]routes.PortRoute, error) {
	f.c.add("ListPortsBySandbox %s", sandbox)
	if f.err != nil {
		return nil, f.err
	}
	var out []routes.PortRoute
	for _, r := range f.rs[sandbox] {
		out = append(out, f.ports[r.Subdomain]...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Subdomain != out[j].Subdomain {
			return out[i].Subdomain < out[j].Subdomain
		}
		return out[i].Port < out[j].Port
	})
	return out, nil
}

func (f *fakeRoutes) SetPortVisibility(subdomain string, port int, visibility string) error {
	f.c.add("SetPortVisibility %s:%d %s", subdomain, port, visibility)
	if f.err != nil {
		return f.err
	}
	for box, list := range f.rs {
		for i := range list {
			if list[i].Subdomain != subdomain {
				continue
			}
			if list[i].Port == port {
				f.rs[box][i].Visibility = visibility
				return nil
			}
		}
	}
	if f.ports == nil {
		f.ports = map[string][]routes.PortRoute{}
	}
	for i, p := range f.ports[subdomain] {
		if p.Port == port {
			f.ports[subdomain][i].Visibility = visibility
			return nil
		}
	}
	f.ports[subdomain] = append(f.ports[subdomain],
		routes.PortRoute{Subdomain: subdomain, Port: port, Visibility: visibility})
	return nil
}

func (f *fakeRoutes) ForgetPort(subdomain string, port int) error {
	f.c.add("ForgetPort %s:%d", subdomain, port)
	if f.err != nil {
		return f.err
	}
	kept := f.ports[subdomain][:0]
	for _, p := range f.ports[subdomain] {
		if p.Port != port {
			kept = append(kept, p)
		}
	}
	f.ports[subdomain] = kept
	return nil
}

func (f *fakeRoutes) PrivatizeAll(subdomain string) (int, error) {
	f.c.add("PrivatizeAll %s", subdomain)
	if f.err != nil {
		return 0, f.err
	}
	changed := 0
	for box, list := range f.rs {
		for i := range list {
			if list[i].Subdomain == subdomain {
				f.rs[box][i].Visibility = routes.VisibilityPrivate
				changed++
			}
		}
	}
	for i, p := range f.ports[subdomain] {
		if p.Visibility != routes.VisibilityPrivate {
			f.ports[subdomain][i].Visibility = routes.VisibilityPrivate
			changed++
		}
	}
	return changed, nil
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
	// id is what the profile lookup reports, and profileErr makes that lookup
	// fail on its own — separately from err, because the interesting case is a
	// key check that PASSED followed by a profile fetch that could not be made.
	id         int64
	profileErr error
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

func (f *fakeGitHub) Profile(ctx context.Context, login string) (users.GitHubProfile, error) {
	f.c.add("github.Profile %s", login)
	if f.profileErr != nil {
		return users.GitHubProfile{}, f.profileErr
	}
	if f.err != nil {
		return users.GitHubProfile{}, f.err
	}
	return users.GitHubProfile{Login: login, ID: f.id}, nil
}

// ---------------------------------------------------------------------------

// rig is the whole package under test with every dependency faked: no sqlite,
// no temp dir, no VM driver, so the suite runs in milliseconds.
// fakeSecrets is the value half of secrets.Store. It keeps the store's one
// structural promise — every row is keyed by (owner, name), so no caller
// mistake can reach another owner's value — and its one delivery rule, that
// SandboxesForSecret names the boxes sharing a tag with the secret.
type fakeSecrets struct {
	c     *calls
	vals  map[string]string   // "owner/NAME" -> value
	tags  map[string][]string // "owner/NAME" -> tags
	boxes *fakeTagger         // to resolve tag -> sandbox
	err   error
	// putErr fails only the write, so a test can prove nothing is reported as
	// resynced when the value never landed.
	putErr error
}

func secretKey(owner, name string) string { return owner + "/" + name }

func (f *fakeSecrets) PutSecret(owner, envName, value string, tags []string) error {
	f.c.add("secrets.Put %s/%s tags=%v", owner, envName, tags)
	if f.putErr != nil {
		return f.putErr
	}
	if len(tags) == 0 {
		tags = []string{secrets.DefaultTag} // the real store's default
	}
	f.vals[secretKey(owner, envName)] = value
	f.tags[secretKey(owner, envName)] = tags
	return nil
}

// RetagSecret changes WHICH sandboxes a secret reaches without touching what it
// says — the one secret write that needs no value, which is exactly why it
// exists (see secrets.Store.RetagSecret).
func (f *fakeSecrets) RetagSecret(owner, envName string, tags []string) error {
	f.c.add("secrets.Retag %s/%s tags=%v", owner, envName, tags)
	if f.err != nil {
		return f.err
	}
	k := secretKey(owner, envName)
	if _, ok := f.vals[k]; !ok {
		return secrets.ErrNoSuchSecret
	}
	if len(tags) == 0 {
		tags = []string{secrets.DefaultTag}
	}
	f.tags[k] = tags
	return nil
}

func (f *fakeSecrets) DeleteSecret(owner, envName string) error {
	f.c.add("secrets.Delete %s/%s", owner, envName)
	if f.err != nil {
		return f.err
	}
	k := secretKey(owner, envName)
	if _, ok := f.vals[k]; !ok {
		return errors.New("no such secret")
	}
	delete(f.vals, k)
	delete(f.tags, k)
	return nil
}

func (f *fakeSecrets) ListSecrets(owner string) ([]secrets.SecretMeta, error) {
	f.c.add("secrets.List %s", owner)
	if f.err != nil {
		return nil, f.err
	}
	var out []secrets.SecretMeta
	for k, tags := range f.tags {
		o, name, _ := strings.Cut(k, "/")
		if o != owner {
			continue
		}
		out = append(out, secrets.SecretMeta{Name: name, Tags: tags, Version: 1})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeSecrets) SandboxesForSecret(owner, envName string) ([]string, error) {
	f.c.add("secrets.SandboxesFor %s/%s", owner, envName)
	if f.err != nil {
		return nil, f.err
	}
	want := map[string]bool{}
	for _, t := range f.tags[secretKey(owner, envName)] {
		want[t] = true
	}
	var out []string
	for box, tags := range f.boxes.tags {
		for _, t := range tags {
			if want[t] {
				out = append(out, box)
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------------------

// fakeTemplateTags is templates.Store in a map. It reproduces the two store
// behaviours ctlops actually depends on — Bind reports the snapshot it replaced,
// and the `default` tag is refused with an ErrInvalidBinding whose sentence the
// caller passes through untouched — so the pass-through can be asserted without
// a sqlite file. The exact wording lives in internal/templates and is tested
// there; what this fake pins is that ctlops does not rewrite it.
type fakeTemplateTags struct {
	c    *calls
	rows map[string]templates.Binding // "owner\x00tag"
	// ports is the snapshot_ports table: "owner\x00snapshot" -> port. Sparse,
	// exactly like the real one, so a lookup for a snapshot captured on the
	// stock port answers 0 here too.
	ports   map[string]int
	err     error // returned by every method when set
	portErr error // returned by the port methods only, when set
}

func bindKey(owner, tag string) string { return owner + "\x00" + tag }

func (f *fakeTemplateTags) SetSnapshotPort(owner, snapshot string, port int) error {
	f.c.add("SetSnapshotPort %s snapshot=%s port=%d", owner, snapshot, port)
	if f.portErr != nil {
		return f.portErr
	}
	if f.ports == nil {
		f.ports = map[string]int{}
	}
	f.ports[bindKey(owner, snapshot)] = port
	return nil
}

func (f *fakeTemplateTags) SnapshotPort(owner, snapshot string) (int, error) {
	f.c.add("SnapshotPort %s snapshot=%s", owner, snapshot)
	if f.portErr != nil {
		return 0, f.portErr
	}
	return f.ports[bindKey(owner, snapshot)], nil
}

func (f *fakeTemplateTags) SnapshotPorts(owner string) (map[string]int, error) {
	f.c.add("SnapshotPorts %s", owner)
	if f.portErr != nil {
		return nil, f.portErr
	}
	out := map[string]int{}
	for k, port := range f.ports {
		if o, snap, ok := strings.Cut(k, "\x00"); ok && o == owner {
			out[snap] = port
		}
	}
	return out, nil
}

func (f *fakeTemplateTags) ForgetSnapshotPort(owner, snapshot string) error {
	f.c.add("ForgetSnapshotPort %s snapshot=%s", owner, snapshot)
	if f.portErr != nil {
		return f.portErr
	}
	delete(f.ports, bindKey(owner, snapshot))
	return nil
}

// defaultTagRefusal is the shape of the store's refusal, not its text: a
// sentence naming the tag and wrapping the sentinel.
func defaultTagRefusal() error {
	return fmt.Errorf("%w: a template cannot be bound to the %q tag — every sandbox you create carries "+
		"that tag, so this snapshot would silently become the base image for all of them.",
		templates.ErrInvalidBinding, secrets.DefaultTag)
}

func (f *fakeTemplateTags) Bind(owner, tag, snapshot string) (templates.Binding, string, error) {
	f.c.add("Bind %s tag=%s snapshot=%s", owner, tag, snapshot)
	if f.err != nil {
		return templates.Binding{}, "", f.err
	}
	if strings.EqualFold(tag, secrets.DefaultTag) {
		return templates.Binding{}, "", defaultTagRefusal()
	}
	prev := ""
	if old, ok := f.rows[bindKey(owner, tag)]; ok {
		prev = old.Snapshot
	}
	b := templates.Binding{Owner: owner, Tag: tag, Snapshot: snapshot, CreatedAt: time.Unix(0, 0).UTC()}
	f.rows[bindKey(owner, tag)] = b
	return b, prev, nil
}

func (f *fakeTemplateTags) Unbind(owner, tag string) (templates.Binding, error) {
	f.c.add("Unbind %s tag=%s", owner, tag)
	if f.err != nil {
		return templates.Binding{}, f.err
	}
	b, ok := f.rows[bindKey(owner, tag)]
	if !ok {
		return templates.Binding{}, templates.ErrNoSuchBinding
	}
	delete(f.rows, bindKey(owner, tag))
	return b, nil
}

func (f *fakeTemplateTags) BindingsForOwner(owner string) ([]templates.Binding, error) {
	f.c.add("BindingsForOwner %s", owner)
	if f.err != nil {
		return nil, f.err
	}
	return f.selectRows(owner, nil), nil
}

func (f *fakeTemplateTags) BindingsForTags(owner string, tags []string) ([]templates.Binding, error) {
	f.c.add("BindingsForTags %s tags=%v", owner, tags)
	if len(tags) == 0 {
		// The store answers without touching the database, and the resolver
		// leans on that: an untagged create must not pay a query.
		return nil, nil
	}
	if f.err != nil {
		return nil, f.err
	}
	want := map[string]bool{}
	for _, t := range tags {
		want[t] = true
	}
	return f.selectRows(owner, want), nil
}

// selectRows returns the owner's bindings ordered by tag, which is the store's
// ORDER BY and the reason the ambiguity message reads the same way every time.
func (f *fakeTemplateTags) selectRows(owner string, want map[string]bool) []templates.Binding {
	var out []templates.Binding
	for _, b := range f.rows {
		if b.Owner != owner || (want != nil && !want[b.Tag]) {
			continue
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}

// bind is the test-only seeder: it writes a row without going through Ops, so a
// test can set up a binding without recording a Bind call it would then have to
// filter out of its own assertions.
func (f *fakeTemplateTags) bind(owner, tag, snapshot string) {
	f.rows[bindKey(owner, tag)] = templates.Binding{
		Owner: owner, Tag: tag, Snapshot: snapshot, CreatedAt: time.Unix(0, 0).UTC(),
	}
}

// ---------------------------------------------------------------------------

type rig struct {
	ops         *Ops
	calls       *calls
	boxes       *fakeSandboxes
	checkpoints *fakeCheckpoints
	tmpl        *fakeTemplates
	accts       *fakeAccounts
	tagger      *fakeTagger
	sched       *fakeSchedules
	routes      *fakeRoutes
	minter      *fakeMinter
	github      *fakeGitHub
	secrets     *fakeSecrets
	bindings    *fakeTemplateTags
	nodes       *fakeNodes
	hivemind    *fakeHiveMind
	// The environment stores are nil on a plain rig — a host with no
	// environment support is the shipped default and the state every
	// KindDisabled assertion needs. withEnvs installs them.
	envs     *fakeEnvs
	envVars  *fakeEnvVars
	netrules *fakeNetRules
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
			ID:   "11111111-2222-3333-4444-555555555555",
			Name: "alicebox", Owner: "alice", State: vmm.StateRunning, VCPUs: 2, MemMB: 8192,
			CreatedAt: now, LastActive: now, SSHAddr: "127.0.0.1:2200", SSHUser: "sparky",
		},
	}}
	tmpl := &fakeTemplates{c: c, on: true, boxes: boxes, snaps: map[string]*host.Snapshot{
		"alice/alicesnap": {Name: "alicesnap", Owner: "alice", FromBox: "alicebox",
			Image: "snap-alice-alicesnap", CreatedAt: now},
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
	checkpoints := &fakeCheckpoints{c: c, enabled: map[string]bool{"alicebox": true}}
	secretStore := &fakeSecrets{c: c, vals: map[string]string{}, tags: map[string][]string{}, boxes: tagger}
	bindings := &fakeTemplateTags{c: c, rows: map[string]templates.Binding{}, ports: map[string]int{}}
	hm := &fakeHiveMind{}

	ops := New(Config{
		Sandboxes: boxes, Templates: tmpl, Accounts: accts,
		Checkpoints: checkpoints,
		Tags:        tagger, Schedules: sched, Routes: rt, Sessions: minter, GitHub: gh,
		Secrets: secretStore,
		// The same store a third time, through its retag half: adding a tag to
		// a secret needs no value, and PutSecret has no way to get one.
		SecretTags:   secretStore,
		TemplateTags: bindings,
		HiveMind:     hm,
		DefaultImage: "base", Domain: "example.test", XtermSubdomain: "xterm",
		InvitesPerUser: 0,
		NewName:        func() string { return "generated-name" },
		Now:            func() time.Time { return now },
		Log:            slog.New(slog.DiscardHandler),
	})
	t.Cleanup(ops.Close)

	return &rig{ops: ops, calls: c, boxes: boxes, checkpoints: checkpoints, tmpl: tmpl, accts: accts,
		tagger: tagger, sched: sched, routes: rt, minter: minter, github: gh, secrets: secretStore,
		bindings: bindings, hivemind: hm}
}

func alice() Caller   { return Caller{Handle: "alice"} }
func mallory() Caller { return Caller{Handle: "mallory"} }

// fakeHiveMind stands in for the SaaS. It records which sandbox was asked
// about, because the one property worth pinning at this layer is that the
// question is bound to the box the caller named and to no other.
type fakeHiveMind struct {
	mu       sync.Mutex
	asked    []string
	pageSize int
	snapshot host.HiveMindSessionSnapshot
	err      error
}

func (f *fakeHiveMind) Sessions(
	_ context.Context,
	box *host.Sandbox,
	pageSize int,
) (host.HiveMindSessionSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, box.ID)
	f.pageSize = pageSize
	return f.snapshot, f.err
}

// ---------------------------------------------------------------------------

// fakeEnvs is envs.Store in a map. It reproduces the three refusals ctlops
// actually maps onto its taxonomy — the reserved `default`, the tag grammar and
// the per-owner cap — so the mapping can be asserted without a sqlite file, and
// it keys every row by (owner, name) so no caller mistake can reach another
// owner's environment. The exact sentences live in internal/envs and are tested
// there; what this pins is that ctlops does not rewrite them.
type fakeEnvs struct {
	c    *calls
	rows map[string]envs.Environment // "owner\x00name"
	err  error
	// cap mirrors maxEnvironmentsPerOwner; 0 means the fixture's default.
	cap int
	// now stamps updated_at the way the real store's clock does. The build
	// reconciler decides staleness from that column, so a fake that left it at
	// the zero time would make every build look infinitely old — or, with the
	// rig's epoch clock, infinitely young. nil means the rig's epoch.
	now func() time.Time
	// buildingErr fails Building() alone, so a reconciler test can prove that a
	// store hiccup fails no environment.
	buildingErr error
}

func (f *fakeEnvs) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Unix(0, 0).UTC()
}

func envKey(owner, name string) string { return owner + "\x00" + name }

func (f *fakeEnvs) Put(owner, name, description string, adopted *envs.Adopted) (envs.Environment, error) {
	f.c.add("envs.Put %s/%s", owner, name)
	if f.err != nil {
		return envs.Environment{}, f.err
	}
	if strings.EqualFold(name, secrets.DefaultTag) {
		return envs.Environment{}, fmt.Errorf("%w: an environment cannot be named %q", envs.ErrReservedName, secrets.DefaultTag)
	}
	if !secrets.ValidTag(name) {
		return envs.Environment{}, fmt.Errorf("%w: name %q", envs.ErrInvalidName, name)
	}
	now := time.Unix(0, 0).UTC()
	k := envKey(owner, name)
	e, ok := f.rows[k]
	if !ok {
		limit := f.cap
		if limit == 0 {
			limit = 32
		}
		n := 0
		for _, row := range f.rows {
			if row.Owner == owner {
				n++
			}
		}
		if n >= limit {
			return envs.Environment{}, fmt.Errorf("%w (%d, max %d)", envs.ErrTooManyEnvironments, n, limit)
		}
		// Honoured on the insert branch only, exactly as the real store does —
		// the whole point of the record is that it says what was there on the
		// day, so an update must never restate it.
		e = envs.Environment{Owner: owner, Name: name, State: envs.StateDraft, CreatedAt: now, Adopted: adopted}
	}
	// The real store touches description and updated_at ONLY: a second
	// `env create` must not reset a build somebody is using.
	e.Description, e.UpdatedAt = description, now
	f.rows[k] = e
	return e, nil
}

func (f *fakeEnvs) Get(owner, name string) (envs.Environment, error) {
	f.c.add("envs.Get %s/%s", owner, name)
	if f.err != nil {
		return envs.Environment{}, f.err
	}
	e, ok := f.rows[envKey(owner, name)]
	if !ok {
		return envs.Environment{}, envs.ErrNoSuchEnvironment
	}
	return e, nil
}

func (f *fakeEnvs) List(owner string) ([]envs.Environment, error) {
	f.c.add("envs.List %s", owner)
	if f.err != nil {
		return nil, f.err
	}
	out := []envs.Environment{}
	for _, e := range f.rows {
		if e.Owner == owner {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeEnvs) Delete(owner, name string) error {
	f.c.add("envs.Delete %s/%s", owner, name)
	if f.err != nil {
		return f.err
	}
	k := envKey(owner, name)
	if _, ok := f.rows[k]; !ok {
		return envs.ErrNoSuchEnvironment
	}
	delete(f.rows, k)
	return nil
}

func (f *fakeEnvs) SetScript(owner, name, script, from string) error {
	f.c.add("envs.SetScript %s/%s from=%s", owner, name, from)
	k := envKey(owner, name)
	e, ok := f.rows[k]
	if !ok {
		return envs.ErrNoSuchEnvironment
	}
	e.SetupScript, e.SetupFrom = script, from
	e.UpdatedAt = f.clock()
	f.rows[k] = e
	return nil
}

// SetSeededScript mirrors the real store: it is the ONLY writer that stamps
// the seed. A fake that stamped it from SetScript too would make every test
// agree that a repaired script is still a clean copy of its repository, which
// is the exact confusion the column exists to prevent.
func (f *fakeEnvs) SetSeededScript(owner, name, script string) error {
	f.c.add("envs.SetSeededScript %s/%s", owner, name)
	k := envKey(owner, name)
	e, ok := f.rows[k]
	if !ok {
		return envs.ErrNoSuchEnvironment
	}
	e.SetupScript, e.SetupFrom = script, envs.SetupFromRepo
	e.SetupSeedSHA = envs.ScriptSHA(script)
	e.UpdatedAt = f.clock()
	f.rows[k] = e
	return nil
}

func (f *fakeEnvs) SetBuildSession(owner, name, url string) error {
	f.c.add("envs.SetBuildSession %s/%s %s", owner, name, url)
	k := envKey(owner, name)
	e, ok := f.rows[k]
	if !ok {
		// The store answers a missing row with nil here too: this is colour on
		// a build, not the write that decides its outcome.
		return nil
	}
	e.BuildSession = url
	e.UpdatedAt = f.clock()
	f.rows[k] = e
	return nil
}

// Unlike SetBuildSession above, a missing row IS an error here — the real
// store's contract, and the difference matters: this is somebody changing where
// a named environment runs, not colour on a build.
func (f *fakeEnvs) SetRunner(owner, name string, runner vmm.Runner) error {
	f.c.add("envs.SetRunner %s/%s %s", owner, name, runner)
	if _, err := vmm.ParseRequirement(string(runner)); err != nil {
		return fmt.Errorf("%w: %s", envs.ErrInvalidName, err)
	}
	k := envKey(owner, name)
	e, ok := f.rows[k]
	if !ok {
		return envs.ErrNoSuchEnvironment
	}
	e.Runner = runner
	e.UpdatedAt = f.clock()
	f.rows[k] = e
	return nil
}

func (f *fakeEnvs) SetBuildDenials(owner, name string, domains []envs.BuildDeniedDomain, overflow uint64) error {
	f.c.add("envs.SetBuildDenials %s/%s domains=%d", owner, name, len(domains))
	k := envKey(owner, name)
	e, ok := f.rows[k]
	if !ok {
		return nil
	}
	e.BuildDenials = append([]envs.BuildDeniedDomain(nil), domains...)
	e.BuildDenialOverflow = overflow
	e.UpdatedAt = f.clock()
	f.rows[k] = e
	return nil
}

func (f *fakeEnvs) SetState(owner, name string, st envs.State, box, buildErr string) error {
	f.c.add("envs.SetState %s/%s state=%s", owner, name, st)
	k := envKey(owner, name)
	e, ok := f.rows[k]
	if !ok {
		return envs.ErrNoSuchEnvironment
	}
	if st != envs.StateFailed {
		buildErr = ""
	}
	e.State, e.BuildBox, e.BuildError = st, box, buildErr
	if st == envs.StateBuilding {
		e.BuildDenials = nil
		e.BuildDenialOverflow = 0
	}
	// updated_at moves on every state change, exactly as the real UPDATE does.
	// It is what buildStartedAt reads, so a fake that skipped it would make the
	// reconciler untestable.
	e.UpdatedAt = f.clock()
	if st == envs.StateReady {
		at := f.clock()
		e.BuiltAt = &at
	}
	f.rows[k] = e
	return nil
}

// Building is the one query with no owner term, and the fake keeps it that way:
// it returns every owner's in-flight build, sorted, because what this package
// does with the owner afterwards — compare it against the sandbox's — is the
// security property the build tests exist to pin.
func (f *fakeEnvs) Building() ([]envs.Environment, error) {
	f.c.add("envs.Building")
	if f.buildingErr != nil {
		return nil, f.buildingErr
	}
	if f.err != nil {
		return nil, f.err
	}
	out := []envs.Environment{}
	for _, e := range f.rows {
		if e.State == envs.StateBuilding {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Owner != out[j].Owner {
			return out[i].Owner < out[j].Owner
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ---------------------------------------------------------------------------

// fakeEnvVars is the plain-var half of secrets.Store, keyed by (owner, tag,
// name) exactly as the real table is — so the same name under two tags is two
// rows, which is the whole difference between a var and a secret.
type fakeEnvVars struct {
	c    *calls
	rows map[string]secrets.Var // "owner\x00tag\x00name"
	err  error
}

func varKey(owner, tag, name string) string { return owner + "\x00" + tag + "\x00" + name }

func (f *fakeEnvVars) PutVar(owner, tag, name, value string) error {
	f.c.add("vars.Put %s/%s/%s", owner, tag, name)
	if f.err != nil {
		return f.err
	}
	// The real store refuses a reserved name, an oversized value and a '#'
	// here. A fake that accepted them would let an ordering regression — a
	// grammar refusal that lands after the first write — pass every test in
	// this package, which is exactly how the original one got in.
	if err := secrets.ValidateVar(name, value); err != nil {
		return err
	}
	f.rows[varKey(owner, tag, name)] = secrets.Var{Tag: tag, Name: name, Value: value}
	return nil
}

func (f *fakeEnvVars) DeleteVar(owner, tag, name string) error {
	f.c.add("vars.Delete %s/%s/%s", owner, tag, name)
	if f.err != nil {
		return f.err
	}
	k := varKey(owner, tag, name)
	if _, ok := f.rows[k]; !ok {
		return secrets.ErrNoSuchVar
	}
	delete(f.rows, k)
	return nil
}

func (f *fakeEnvVars) VarsForTag(owner, tag string) ([]secrets.Var, error) {
	f.c.add("vars.ForTag %s/%s", owner, tag)
	if f.err != nil {
		return nil, f.err
	}
	var out []secrets.Var
	for k, v := range f.rows {
		if strings.HasPrefix(k, owner+"\x00"+tag+"\x00") {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeEnvVars) DeleteVarsForTag(owner, tag string) error {
	f.c.add("vars.DeleteForTag %s/%s", owner, tag)
	if f.err != nil {
		return f.err
	}
	for k := range f.rows {
		if strings.HasPrefix(k, owner+"\x00"+tag+"\x00") {
			delete(f.rows, k)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------

// fakeNetRules is the egress rule-set store in a map. It keeps the one refusal
// ctlops depends on — `default` is not a rule-set tag, because a subtractive
// policy on the tag every sandbox carries would cut the whole fleet down to the
// base allowlist.
type fakeNetRules struct {
	c    *calls
	rows map[string]netrules.RuleMeta // "owner\x00name"
	err  error
}

func (f *fakeNetRules) ListRules(owner string) ([]netrules.RuleMeta, error) {
	f.c.add("netrules.List %s", owner)
	if f.err != nil {
		return nil, f.err
	}
	var out []netrules.RuleMeta
	for k, r := range f.rows {
		if o, _, _ := strings.Cut(k, "\x00"); o == owner {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeNetRules) PutRule(owner, name string, spec netrules.RuleSpec, tags []string) error {
	f.c.add("netrules.Put %s/%s tags=%v", owner, name, tags)
	if f.err != nil {
		return f.err
	}
	if slices.Contains(tags, secrets.DefaultTag) {
		return fmt.Errorf("an egress rule-set cannot be tagged %q", secrets.DefaultTag)
	}
	f.rows[owner+"\x00"+name] = netrules.RuleMeta{Name: name, Tags: tags, Spec: spec, Version: 1}
	return nil
}

func (f *fakeNetRules) DeleteRule(owner, name string) error {
	f.c.add("netrules.Delete %s/%s", owner, name)
	if f.err != nil {
		return f.err
	}
	delete(f.rows, owner+"\x00"+name)
	return nil
}

// withEnvs gives a rig the three optional stores the environment verbs need,
// the same way withRepos does for the repo verbs: by setting the unexported
// fields directly, which is this package's idiom for reshaping a host after New
// and keeps the shared rig a plain one with no environment support at all.
func withEnvs(r *rig) (*fakeEnvs, *fakeEnvVars, *fakeNetRules) {
	e := &fakeEnvs{c: r.calls, rows: map[string]envs.Environment{}}
	v := &fakeEnvVars{c: r.calls, rows: map[string]secrets.Var{}}
	n := &fakeNetRules{c: r.calls, rows: map[string]netrules.RuleMeta{}}
	r.envs, r.envVars, r.netrules = e, v, n
	r.ops.envs, r.ops.envVars, r.ops.netrules = e, v, n
	return e, v, n
}
