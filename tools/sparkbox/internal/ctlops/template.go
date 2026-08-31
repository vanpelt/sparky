package ctlops

import (
	"context"
	"fmt"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

func (o *Ops) ListSnapshots(ctx context.Context, c Caller) ([]SnapshotInfo, error) {
	snaps := o.templates.Snapshots(c.Handle)
	bound := o.boundTagsMap(c.Handle)
	ports := o.snapshotPortMap(c.Handle)
	// Node is reported only where a machine name means something. See
	// SnapshotInfo.Node: a single-machine host stamps "local" on every record,
	// and printing it there would invent a fleet nobody has.
	_, named := o.boxes.(placer)
	out := make([]SnapshotInfo, 0, len(snaps))
	for _, s := range snaps {
		si := SnapshotInfo{
			Name: s.Name, Owner: s.Owner, FromBox: s.FromBox, CreatedAt: s.CreatedAt,
			BoundTags: bound[s.Name], Port: ports[s.Name],
		}
		if named {
			si.Node = s.Node
		}
		out = append(out, si)
	}
	return out, nil
}

// CreateSnapshot implicitly PAUSES a running sandbox: the manager strips the
// managed env block, pauses, then compacts.
func (o *Ops) CreateSnapshot(ctx context.Context, c Caller, sandbox, name string) (SnapshotInfo, error) {
	const op = "snapshot.create"
	if _, err := o.owned(op, sandbox, c); err != nil {
		return SnapshotInfo{}, err
	}
	if !o.templates.Snapshotter() {
		return SnapshotInfo{}, Disabled(op, "snapshots are not supported by this driver")
	}
	if name == "" {
		return SnapshotInfo{}, Invalid(op, "missing_name", "a snapshot name is required")
	}
	ctx, cancel := withBudget(ctx, ArchiveTimeout)
	defer cancel()
	s, err := o.templates.Snapshot(ctx, sandbox, name, c.Handle)
	if err != nil {
		return SnapshotInfo{}, Fail(op, err)
	}
	o.log.Info("snapshot created", "user", c.Handle, "sandbox", sandbox, "snapshot", name)
	si := SnapshotInfo{Name: s.Name, Owner: s.Owner, FromBox: s.FromBox, CreatedAt: s.CreatedAt}
	// A fresh capture is normally unbound, but not always: a name that was
	// deleted and taken again is bound by whatever pointed at the old one, and
	// that binding now decides what the tag boots. It is worth saying so at the
	// moment it becomes true rather than the next time somebody lists.
	si.BoundTags = o.boundTagsFor(c.Handle, s.Name)
	if _, named := o.boxes.(placer); named {
		si.Node = s.Node
	}
	si.Port = o.captureDefaultPort(c.Handle, sandbox, s.Name)
	return si, nil
}

// captureDefaultPort records the port the captured sandbox served on, so a
// sandbox booted from this template lands on it instead of the stock default.
// It returns the port it recorded, or 0 when there was nothing worth recording.
//
// The port is read HERE, at capture, and not at bind: this is the only moment
// the source sandbox is known to still exist and to still be serving what the
// template contains. A binding made a week later would be reading a route that
// has since moved, or none at all.
//
// It is BEST EFFORT, and that is a deliberate asymmetry with resolveTemplate's
// refusal to fall back to the default image. The expensive, unrepeatable half
// of a capture has already succeeded by the time this runs — the sandbox is
// paused and a re-run would compact a different filesystem — so failing the
// whole operation over the port would throw away minutes to reach a state the
// user can fix with one `sparkbox port`. The failure is also self-announcing in
// a way the wrong rootfs is not: a box that boots on 8000 with the dev server
// on 5173 says so immediately, where a box missing its toolchain says nothing
// for twenty minutes. The port that was recorded travels back in SnapshotInfo
// and prints in `snapshot ls`, so silence there is the signal.
func (o *Ops) captureDefaultPort(owner, sandbox, snapshot string) int {
	if o.routes == nil || o.templateTags == nil {
		return 0
	}
	// The DEFAULT route only — subdomain == the sandbox's own name. The custom
	// routes an owner adds with `share` are furniture for one box, and cloning
	// them onto every fork would hand out subdomains nobody asked for.
	row, ok, err := o.routes.GetBySubdomain(sandbox)
	if err != nil {
		o.log.Error("could not read the default route of a sandbox being captured",
			"user", owner, "sandbox", sandbox, "snapshot", snapshot, "err", err,
			"next", "the template keeps the stock default port; set it on a new box with `sparkbox port`")
		return 0
	}
	// Not this sandbox's own default route, so there is no port here that
	// belongs to the template. Nothing to record and nothing to warn about.
	if !ok || row.Sandbox != sandbox || row.Port == 0 || row.Port == routes.DefaultPort {
		return 0
	}
	if err := o.templateTags.SetSnapshotPort(owner, snapshot, row.Port); err != nil {
		o.log.Error("could not record the default port of a new snapshot",
			"user", owner, "snapshot", snapshot, "port", row.Port, "err", err,
			"next", "the template keeps the stock default port; set it on a new box with `sparkbox port`")
		return 0
	}
	o.log.Info("snapshot carries a default port", "user", owner, "snapshot", snapshot, "port", row.Port)
	return row.Port
}

// adoptTemplatePort points a just-created sandbox's default route at the port
// its template was captured on.
//
// It is a SECOND write, correcting the routes.DefaultPort row that
// host.Manager.Create (or fleet.mint, for a sandbox on another machine) has
// already written. Threading the port down through Create, CreateOn and the
// node link instead would put it on the wire in three more places to close a
// window nobody can observe: the row is corrected before this call returns and
// the caller has not yet been told the sandbox's name. Upsert's ON CONFLICT
// touches only the port, so this cannot disturb a route's visibility.
//
// Best-effort, for the same reason the capture is: the sandbox exists and
// boots, and failing a create the user would have to repeat — paying another
// cold boot — over a port they can change in one command is the worse trade.
func (o *Ops) adoptTemplatePort(name, owner string, port int) {
	if port == 0 || o.routes == nil {
		return
	}
	if err := o.routes.Upsert(routes.Route{
		Subdomain: name, Sandbox: name, Owner: owner, Port: port,
	}); err != nil {
		o.log.Error("could not point a new sandbox's default route at its template's port",
			"name", name, "owner", owner, "port", port, "err", err,
			"next", "the sandbox serves on the stock default port; `sparkbox port` from inside it moves the route")
		return
	}
	o.log.Info("sandbox inherited its template's default port", "name", name, "owner", owner, "port", port)
}

// snapshotPortMap is every port this owner has recorded, for the listing paths.
// It reads the table once rather than once per row, and answers an empty map on
// failure: a port column is decoration on a listing, and losing `snapshot ls`
// entirely because one of its five columns could not be filled is a trade
// nobody wants. See boundTagsMap, which swallows for the same reason.
func (o *Ops) snapshotPortMap(owner string) map[string]int {
	if o.templateTags == nil {
		return nil
	}
	ports, err := o.templateTags.SnapshotPorts(owner)
	if err != nil {
		o.log.Error("could not read the default ports recorded for snapshots",
			"user", owner, "err", err)
		return nil
	}
	return ports
}

// templatePort is the port recorded for one of the caller's snapshots, or 0.
//
// A store failure answers 0 rather than failing whatever asked. Every caller is
// on a create path where the alternative is refusing to make a sandbox at all
// over a routing detail — see adoptTemplatePort — so the error is logged where
// an operator can see it and the create goes on.
func (o *Ops) templatePort(owner, snapshot string) int {
	if o.templateTags == nil {
		return 0
	}
	port, err := o.templateTags.SnapshotPort(owner, snapshot)
	if err != nil {
		o.log.Error("could not read the default port recorded for a snapshot",
			"user", owner, "snapshot", snapshot, "err", err)
		return 0
	}
	return port
}

// SnapshotToTag captures a sandbox and points a tag at what it captured. It is
// ONE operation with three doors — `ssh ctl@gw snapshot create <box> <name>
// --tag web`, the REST create with a tag, and `sparkbox snapshot web` from
// inside the box — so the ordering and the half-failure policy are decided
// here rather than three times in three transports.
//
// It composes CreateSnapshot and BindTemplate and reimplements neither. An
// empty Tag makes it exactly CreateSnapshot, which is what keeps every caller
// that never asked for a binding byte-identical to what shipped.
//
// CAPTURE FIRST, BIND SECOND, and it cannot be the other way. Binding first
// would leave the tag pointing at an image that does not exist yet, so every
// `--tag web` create in that window would resolve to a missing rootfs — a
// failure that spreads forward to other people. Capture-first's failure mode is
// strictly benign: one unbound snapshot.
func (o *Ops) SnapshotToTag(ctx context.Context, c Caller, a SnapshotToTagArgs) (SnapshotToTagResult, error) {
	const op = "snapshot.create"
	// Refused before the capture, not after: two minutes of work whose whole
	// point was the binding must not end in "this host cannot record one".
	if a.Tag != "" && o.templateTags == nil {
		return SnapshotToTagResult{}, Disabled(op, "template bindings are not enabled on this host")
	}
	si, err := o.CreateSnapshot(ctx, c, a.Sandbox, a.Name)
	if err != nil {
		return SnapshotToTagResult{}, err
	}
	if a.Tag == "" {
		return SnapshotToTagResult{Snapshot: si}, nil
	}
	res, err := o.BindTemplate(ctx, c, si.Name, a.Tag)
	if err != nil {
		// THE SNAPSHOT IS KEPT. The expensive, unrepeatable half succeeded —
		// the sandbox is paused now, and re-running would capture a different
		// filesystem — so deleting it to reach a state `snapshot rm` already
		// reaches would throw away minutes the user has paid for. The half-state
		// that actually hurts is "the user cannot tell", not "an extra template
		// exists", so the repair travels in the sentence and `snapshot ls` shows
		// the truth on its own: the new snapshot with an empty tag column above
		// the one that still holds the tag.
		return SnapshotToTagResult{Snapshot: si}, &Error{
			Kind: KindConflict, Op: op, Code: "snapshot_not_bound",
			Msg: fmt.Sprintf("captured %q, but binding it to tag `%s` failed: %v. "+
				"The snapshot is kept — finish it with:\n  %s snapshot bind %s --tag %s",
				si.Name, a.Tag, err, o.ctlHint(), si.Name, a.Tag),
			Details:  map[string]any{"snapshot": si.Name, "tag": a.Tag},
			Verbatim: true,
			Err:      err,
		}
	}
	// Re-read rather than assumed: CreateSnapshot filled BoundTags before this
	// bind existed, and a result that named the tag in one field and omitted it
	// from the other would be its own small lie.
	si.BoundTags = o.boundTagsFor(c.Handle, si.Name)
	o.log.Info("snapshot captured and bound", "user", c.Handle, "sandbox", a.Sandbox,
		"snapshot", si.Name, "tag", res.Binding.Tag, "previous", res.Previous)
	return SnapshotToTagResult{
		Snapshot: si, Tag: res.Binding.Tag, Bound: true, Previous: res.Previous,
	}, nil
}

func (o *Ops) DeleteSnapshot(ctx context.Context, c Caller, name string) error {
	const op = "snapshot.rm"
	if _, err := o.ownedSnapshot(op, name, c); err != nil {
		return err
	}
	// A bound snapshot is not this owner's alone to remove: it is the base image
	// of every future sandbox on its tags, and deleting it turns each of those
	// creates into resolveTemplate's template_missing refusal. The store failure
	// is fatal here rather than logged, unlike the listing paths — a delete that
	// could not check whether anything depends on the file must not go ahead and
	// delete it.
	bound, err := o.boundTags(c.Handle)
	if err != nil {
		return Fail(op, err)
	}
	if tags := bound[name]; len(tags) > 0 {
		return &Error{
			Kind: KindConflict, Op: op, Code: "snapshot_bound",
			Msg: fmt.Sprintf("snapshot %q is the base image bound to %s, so deleting it would break every "+
				"sandbox created on those tags from now on.", name, strings.Join(tags, ", ")),
			Hint:     fmt.Sprintf("snapshot unbind --tag %s", tags[0]),
			Details:  map[string]any{"snapshot": name, "tags": tags},
			Verbatim: true,
		}
	}
	ctx, cancel := withBudget(ctx, PauseTimeout)
	defer cancel()
	if err := o.templates.DeleteSnapshot(ctx, name, c.Handle); err != nil {
		return Fail(op, err)
	}
	// After the template file is gone, and not fatally. A port row that outlives
	// its snapshot would be inherited by the next capture to take the name — a
	// create landing on a port from a template its owner already deleted — but
	// a delete that has already removed the disk cannot be un-done by returning
	// an error here, and reporting failure for a snapshot that is genuinely gone
	// would send the user back to re-run a command that now has nothing to do.
	if o.templateTags != nil {
		if err := o.templateTags.ForgetSnapshotPort(c.Handle, name); err != nil {
			o.log.Error("could not forget the default port of a deleted snapshot",
				"user", c.Handle, "snapshot", name, "err", err,
				"next", "a snapshot later captured under this name would inherit that port; clear it by capturing over it")
		}
	}
	o.log.Info("snapshot deleted", "user", c.Handle, "snapshot", name)
	return nil
}

// Fork creates a sandbox from one of the caller's snapshots. Tags go on before
// the fork for the same reason they do on Create — the fork IS a create, and it
// fires the same asynchronous secret-env push.
//
// It is deliberately UNCHANGED by tag templates and must stay that way: a fork
// names its snapshot explicitly and must never consult a binding. That is the
// whole distinction between the two verbs — a fork is "boot this snapshot once",
// a binding is the durable form of the same idea — and a fork that quietly
// resolved a bound template instead would leave no way to boot a named snapshot
// at all. It is also why the tags a fork carries select secrets, repos and
// egress rules but not an image.
func (o *Ops) Fork(ctx context.Context, c Caller, a ForkArgs) (SandboxInfo, error) {
	const op = "fork"
	if _, err := o.ownedSnapshot(op, a.Snapshot, c); err != nil {
		return SandboxInfo{}, err
	}
	if a.Name == "" {
		return SandboxInfo{}, Invalid(op, "missing_name", "a name for the new sandbox is required")
	}
	tags, err := NormalizeTags(a.Tags)
	if err != nil {
		return SandboxInfo{}, Invalid(op, "bad_tag", "%v", err)
	}
	if len(tags) > 0 && o.tags == nil {
		return SandboxInfo{}, Disabled(op, "tagging is not enabled on this host")
	}
	// A fork is a create: same reasoning as Create's, and a forked box that
	// silently loses its owner's secrets is the same bug wearing a snapshot.
	tags = o.defaultTags(tags)

	ctx, cancel := withBudget(ctx, PauseTimeout)
	defer cancel()

	// The DESTINATION name, not the snapshot: ownedSnapshot above gated the
	// source, and without this a fork onto a stranger's sandbox name would strip
	// their tags before the manager ever refused the name. See nameIsFree.
	if err := o.nameIsFree(op, a.Name); err != nil {
		return SandboxInfo{}, err
	}
	// A fork is the case --ref exists for: the snapshot already HAS the
	// checkout, on whatever branch it was captured on, so this is the only way
	// to ask for a different one. Resolved before the first write, as in
	// Create.
	refs, err := o.resolveRepoRefs(op, c.Handle, tags, a.Refs)
	if err != nil {
		return SandboxInfo{}, err
	}
	if err := o.stampTags(a.Name, c.Handle, tags); err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	if err := o.writeRepoRefs(c.Handle, a.Name, refs); err != nil {
		o.clearTags(a.Name, c.Handle, tags)
		return SandboxInfo{}, Fail(op, err)
	}
	box, err := o.templates.Fork(ctx, a.Snapshot, a.Name, c.Handle, 0, 0)
	if err != nil {
		o.clearTags(a.Name, c.Handle, tags)
		o.clearRepoRefs(c.Handle, a.Name, refs)
		return SandboxInfo{}, Fail(op, err)
	}
	// A fork consults the SNAPSHOT it was told to boot and never a binding —
	// see this function's own comment — so the port comes from the same place
	// the disk did, by name. That is exactly the rule tag templates follow too;
	// the two paths differ in how they choose a snapshot, not in what one is.
	o.adoptTemplatePort(a.Name, c.Handle, o.templatePort(c.Handle, a.Snapshot))
	o.log.Info("sandbox forked", "user", c.Handle, "snapshot", a.Snapshot, "name", a.Name, "tags", tags)
	return o.info(box), nil
}
