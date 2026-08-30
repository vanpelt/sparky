package ctlops

import (
	"context"
	"fmt"
	"strings"
)

func (o *Ops) ListSnapshots(ctx context.Context, c Caller) ([]SnapshotInfo, error) {
	snaps := o.templates.Snapshots(c.Handle)
	bound := o.boundTagsMap(c.Handle)
	// Node is reported only where a machine name means something. See
	// SnapshotInfo.Node: a single-machine host stamps "local" on every record,
	// and printing it there would invent a fleet nobody has.
	_, named := o.boxes.(placer)
	out := make([]SnapshotInfo, 0, len(snaps))
	for _, s := range snaps {
		si := SnapshotInfo{
			Name: s.Name, Owner: s.Owner, FromBox: s.FromBox, CreatedAt: s.CreatedAt,
			BoundTags: bound[s.Name],
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
	return si, nil
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
	o.log.Info("sandbox forked", "user", c.Handle, "snapshot", a.Snapshot, "name", a.Name, "tags", tags)
	return o.info(box), nil
}
