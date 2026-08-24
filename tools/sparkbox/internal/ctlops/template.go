package ctlops

import "context"

func (o *Ops) ListSnapshots(ctx context.Context, c Caller) ([]SnapshotInfo, error) {
	snaps := o.templates.Snapshots(c.Handle)
	out := make([]SnapshotInfo, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, SnapshotInfo{
			Name: s.Name, Owner: s.Owner, FromBox: s.FromBox, CreatedAt: s.CreatedAt,
		})
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
	return SnapshotInfo{Name: s.Name, Owner: s.Owner, FromBox: s.FromBox, CreatedAt: s.CreatedAt}, nil
}

func (o *Ops) DeleteSnapshot(ctx context.Context, c Caller, name string) error {
	const op = "snapshot.rm"
	if _, err := o.ownedSnapshot(op, name, c); err != nil {
		return err
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
	if err := o.stampTags(a.Name, c.Handle, tags); err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	box, err := o.templates.Fork(ctx, a.Snapshot, a.Name, c.Handle, 0, 0)
	if err != nil {
		o.clearTags(a.Name, c.Handle, tags)
		return SandboxInfo{}, Fail(op, err)
	}
	o.log.Info("sandbox forked", "user", c.Handle, "snapshot", a.Snapshot, "name", a.Name, "tags", tags)
	return o.info(box), nil
}
