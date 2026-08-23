package ctlops

import (
	"context"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// Get resolves a sandbox the caller may act on. Missing and not-yours return the
// identical *Error, and every method below calls the same internal gate before
// touching the manager — so a cross-owner probe can never confirm a name and can
// never wake a VM.
func (o *Ops) Get(ctx context.Context, c Caller, name string) (SandboxInfo, error) {
	box, err := o.owned("get", name, c)
	if err != nil {
		return SandboxInfo{}, err
	}
	return o.info(box), nil
}

func (o *Ops) List(ctx context.Context, c Caller) ([]SandboxInfo, error) {
	boxes := o.boxes.ListByOwner(c.Handle)
	out := make([]SandboxInfo, 0, len(boxes))
	for _, b := range boxes {
		out = append(out, o.info(b))
	}
	return out, nil
}

// Create stamps tags BEFORE Sandboxes.Create, because Create fires the
// secret-env push asynchronously and the tags decide its contents; it clears the
// tag rows again if the create fails. This is the ordering userconsole's fork
// gets wrong, fixed once here for every caller.
func (o *Ops) Create(ctx context.Context, c Caller, a CreateArgs) (SandboxInfo, error) {
	const op = "create"
	tags, err := NormalizeTags(a.Tags)
	if err != nil {
		return SandboxInfo{}, Invalid(op, "bad_tag", "%v", err)
	}
	if len(tags) > 0 && o.tags == nil {
		return SandboxInfo{}, Disabled(op, "tagging is not enabled on this host")
	}
	name := a.Name
	if name == "" {
		name = o.GenerateName()
	}

	ctx, cancel := withBudget(ctx, DialTimeout)
	defer cancel()

	if err := o.nameIsFree(op, name); err != nil {
		return SandboxInfo{}, err
	}
	if err := o.stampTags(name, c.Handle, tags); err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	box, err := o.build(ctx, op, a.Node, name, c.Handle, a.VCPUs, a.MemMB)
	if err != nil {
		// Don't strand tag rows for a sandbox that never came into being.
		o.clearTags(name, c.Handle, tags)
		return SandboxInfo{}, Fail(op, err)
	}
	o.log.Info("sandbox created", "user", c.Handle, "name", name, "node", a.Node, "tags", tags)
	return o.info(box), nil
}

// placer is the sandbox store that can build on a NAMED machine.
//
// It is a type assertion rather than a field on Config because the thing that
// satisfies it — internal/fleet — is the same value already wired in as
// Sandboxes, and because *host.Manager is not one: a single box has exactly one
// machine and no way to answer a question about a second. Asserting keeps that
// distinction where it belongs, at the store, instead of asking every caller to
// wire two things that are always the same thing or always nil.
type placer interface {
	CreateOn(ctx context.Context, node, name, owner, image string, vcpus, memMB int64) (*host.Sandbox, error)
}

// build runs the create on the machine the caller asked for, or on whichever
// one the store chooses when they did not ask.
//
// Naming a machine on a host that has no fleet is a request this deployment
// cannot satisfy at all, and it is answered as such rather than silently built
// here: a caller who says --node laptop and gets a sandbox on the gateway has
// been told nothing and has to discover it later, on the machine's bill.
func (o *Ops) build(ctx context.Context, op, node, name, owner string, vcpus, memMB int64) (*host.Sandbox, error) {
	if node == "" {
		return o.boxes.Create(ctx, name, owner, o.defaultImage, vcpus, memMB)
	}
	p, ok := o.boxes.(placer)
	if !ok {
		return nil, Disabled(op, "this host runs a single machine, so a sandbox can't be placed on a named one.")
	}
	return p.CreateOn(ctx, node, name, owner, o.defaultImage, vcpus, memMB)
}

// nameIsFree refuses a name that already names a sandbox — anyone's — BEFORE a
// create or a fork writes anything under it.
//
// The manager makes the same check, but it makes it too late: stampTags keys on
// the name alone and runs first (it has to; see stampTags), so a create destined
// to fail with NameTaken would already have replaced the tags of whoever holds
// the name, and the rollback would delete them a second time. Tags are not
// decoration — they select the secrets pushed into the guest and the per-tag
// egress allowlist — so that is a cross-tenant downgrade, not a cosmetic one.
// The tag store refuses a cross-owner write as well; this is the check that
// keeps the refusal from turning a stranger's create into a 500.
//
// The masked-not-found rule does not apply here: `create` has always answered
// "that name is taken" for a name in use, because the namespace is global and a
// caller who cannot learn that cannot pick a name at all.
func (o *Ops) nameIsFree(op, name string) error {
	if _, taken := o.boxes.Get(name); taken {
		return AsError(op, &host.NameError{Problem: host.NameTaken, Noun: "sandbox", Name: name})
	}
	return nil
}

// stampTags writes a create's tags under the name the sandbox is ABOUT to have.
// It runs before Create deliberately: Create kicks off the secret-env push on a
// goroutine, and tags are exactly what decides that push's contents, so stamping
// afterwards races it and usually loses — the box comes up without the secrets
// its tags select and only picks them up on some later resume.
func (o *Ops) stampTags(name, owner string, tags []string) error {
	if len(tags) == 0 || o.tags == nil {
		return nil
	}
	return o.tags.SetTags(name, owner, tags)
}

// clearTags is the rollback half, best-effort by design: a failed cleanup must
// not mask the create error the caller actually needs to read.
func (o *Ops) clearTags(name, owner string, tags []string) {
	if len(tags) == 0 || o.tags == nil {
		return
	}
	if err := o.tags.SetTags(name, owner, nil); err != nil {
		o.log.Warn("tag rollback failed", "name", name, "err", err)
	}
}

func (o *Ops) Pause(ctx context.Context, c Caller, name string) (SandboxInfo, error) {
	const op = "pause"
	if _, err := o.owned(op, name, c); err != nil {
		return SandboxInfo{}, err
	}
	ctx, cancel := withBudget(ctx, PauseTimeout)
	defer cancel()
	if err := o.boxes.Pause(ctx, name); err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	o.log.Info("sandbox paused", "user", c.Handle, "name", name)
	return o.reread(op, name, c)
}

// Resume is EnsureRunning: it starts a paused box and folds in the
// download+unpack for an archived one, which is why ctl calls it "restore".
func (o *Ops) Resume(ctx context.Context, c Caller, name string) (SandboxInfo, error) {
	const op = "restore"
	if _, err := o.owned(op, name, c); err != nil {
		return SandboxInfo{}, err
	}
	// The archive budget, not the pause one: a restore may have to download and
	// decompress the whole rootfs before it can boot.
	ctx, cancel := withBudget(ctx, ArchiveTimeout)
	defer cancel()
	box, err := o.boxes.EnsureReady(ctx, name)
	if err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	o.log.Info("sandbox resumed", "user", c.Handle, "name", name)
	return o.info(box), nil
}

func (o *Ops) Archive(ctx context.Context, c Caller, name string) (SandboxInfo, error) {
	const op = "archive"
	if _, err := o.owned(op, name, c); err != nil {
		return SandboxInfo{}, err
	}
	// Asking first turns "this host has no object store" into a KindDisabled a
	// client can hide the button for, rather than a KindInternal it discovers by
	// waiting for a failure.
	if !o.boxes.ArchivingEnabled() {
		return SandboxInfo{}, Disabled(op, "archiving is not enabled on this host")
	}
	ctx, cancel := withBudget(ctx, ArchiveTimeout)
	defer cancel()
	if err := o.boxes.Archive(ctx, name); err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	o.log.Info("sandbox archived", "user", c.Handle, "name", name)
	return o.reread(op, name, c)
}

func (o *Ops) Checkpoint(ctx context.Context, c Caller, name string) (SandboxInfo, error) {
	const op = "checkpoint"
	if _, err := o.owned(op, name, c); err != nil {
		return SandboxInfo{}, err
	}
	if o.checkpoints == nil || !o.checkpoints.Enabled(name) {
		return SandboxInfo{}, Disabled(op, "checkpointing is not enabled on this host")
	}
	ctx, cancel := withBudget(ctx, ArchiveTimeout)
	defer cancel()
	if err := o.checkpoints.Checkpoint(ctx, name); err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	o.log.Info("sandbox checkpointed", "user", c.Handle, "name", name)
	return o.reread(op, name, c)
}

func (o *Ops) RestoreCheckpoint(ctx context.Context, c Caller, name string) (SandboxInfo, error) {
	const op = "checkpoint.restore"
	if _, err := o.owned(op, name, c); err != nil {
		return SandboxInfo{}, err
	}
	if o.checkpoints == nil || !o.checkpoints.Enabled(name) {
		return SandboxInfo{}, Disabled(op, "checkpointing is not enabled on this host")
	}
	ctx, cancel := withBudget(ctx, ArchiveTimeout)
	defer cancel()
	if err := o.checkpoints.RestoreCheckpoint(ctx, name); err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	o.log.Info("sandbox checkpoint restored", "user", c.Handle, "name", name)
	return o.reread(op, name, c)
}

// Resize grows the root disk. It pauses, DISCARDS the memory snapshot, resizes
// and cold-boots, so in-guest processes die; surfacing that warning is the
// caller's job.
func (o *Ops) Resize(ctx context.Context, c Caller, name string, sizeMB int64) (SandboxInfo, error) {
	const op = "resize"
	if _, err := o.owned(op, name, c); err != nil {
		return SandboxInfo{}, err
	}
	if sizeMB <= 0 {
		return SandboxInfo{}, Invalid(op, "bad_size", "size must be positive, got %d MB", sizeMB)
	}
	if sizeMB > MaxDiskMB {
		return SandboxInfo{}, Invalid(op, "bad_size", "size %d MB exceeds the %d GB per-sandbox limit",
			sizeMB, MaxDiskMB/1024)
	}
	ctx, cancel := withBudget(ctx, ResizeTimeout)
	defer cancel()
	if err := o.boxes.Resize(ctx, name, sizeMB); err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	o.log.Info("sandbox resized", "user", c.Handle, "name", name, "size_mb", sizeMB)
	return o.reread(op, name, c)
}

func (o *Ops) Reboot(ctx context.Context, c Caller, name string) (SandboxInfo, error) {
	const op = "reboot"
	if _, err := o.owned(op, name, c); err != nil {
		return SandboxInfo{}, err
	}
	ctx, cancel := withBudget(ctx, PauseTimeout)
	defer cancel()
	if err := o.boxes.Reboot(ctx, name); err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	o.log.Info("sandbox rebooted", "user", c.Handle, "name", name)
	return o.reread(op, name, c)
}

func (o *Ops) Rename(ctx context.Context, c Caller, name, newName string) (SandboxInfo, error) {
	const op = "rename"
	if _, err := o.owned(op, name, c); err != nil {
		return SandboxInfo{}, err
	}
	if newName == "" {
		return SandboxInfo{}, Invalid(op, "missing_name", "a new name is required")
	}
	ctx, cancel := withBudget(ctx, PauseTimeout)
	defer cancel()
	// The manager takes the owner too and re-checks it; passing the caller's
	// handle rather than the record's is what keeps that check meaningful.
	if err := o.boxes.Rename(ctx, name, newName, c.Handle); err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	o.log.Info("sandbox renamed", "user", c.Handle, "from", name, "to", newName)
	return o.reread(op, newName, c)
}

func (o *Ops) Destroy(ctx context.Context, c Caller, name string) error {
	const op = "rm"
	if _, err := o.owned(op, name, c); err != nil {
		return err
	}
	ctx, cancel := withBudget(ctx, PauseTimeout)
	defer cancel()
	if err := o.boxes.Destroy(ctx, name); err != nil {
		return Fail(op, err)
	}
	o.log.Info("sandbox destroyed", "user", c.Handle, "name", name)
	return nil
}

// SetPinned sets the always-on flag and NOTHING else. `ctl pin` composes it with
// Resume in its renderer and keeps its half-succeeded exit 1; the REST API keeps
// them separate so there is no partial state to invent a status code for.
func (o *Ops) SetPinned(ctx context.Context, c Caller, name string, pinned bool) (SandboxInfo, error) {
	op := "unpin"
	if pinned {
		op = "pin"
	}
	if _, err := o.owned(op, name, c); err != nil {
		return SandboxInfo{}, err
	}
	if err := o.boxes.SetPinned(name, pinned); err != nil {
		return SandboxInfo{}, Fail(op, err)
	}
	o.log.Info("sandbox pin changed", "user", c.Handle, "name", name, "pinned", pinned)
	return o.reread(op, name, c)
}

func (o *Ops) Tags(ctx context.Context, c Caller, name string) ([]string, error) {
	const op = "tags.get"
	if _, err := o.owned(op, name, c); err != nil {
		return nil, err
	}
	if o.tags == nil {
		return nil, Disabled(op, "tagging is not enabled on this host")
	}
	tags, err := o.tags.TagsFor(name)
	if err != nil {
		return nil, Fail(op, err)
	}
	if tags == nil {
		tags = []string{}
	}
	return tags, nil
}

// SetTags replaces the whole set (nil or empty clears it) and then ResyncEnv's
// the box, so the guest's secret env matches its new tags immediately rather
// than at the next resume.
func (o *Ops) SetTags(ctx context.Context, c Caller, name string, tags []string) ([]string, error) {
	const op = "tags.set"
	if _, err := o.owned(op, name, c); err != nil {
		return nil, err
	}
	if o.tags == nil {
		return nil, Disabled(op, "tagging is not enabled on this host")
	}
	want, err := NormalizeTags(tags)
	if err != nil {
		return nil, Invalid(op, "bad_tag", "%v", err)
	}
	if err := o.tags.SetTags(name, c.Handle, want); err != nil {
		return nil, Fail(op, err)
	}
	// Secrets follow tags, so the box needs a re-push to match its new set.
	o.boxes.ResyncEnv(ctx, name)
	o.log.Info("sandbox tags set", "user", c.Handle, "name", name, "tags", want)
	if want == nil {
		want = []string{}
	}
	return want, nil
}

// Attach is the owner gate plus resume for an interactive session. It is the
// only method that returns an SSH address, and it exists so the terminal bridge
// cannot skip the check every other command performs. The ownership check
// strictly precedes EnsureRunning, so a cross-owner probe can never wake a
// stranger's VM — and the returned Endpoint comes from the RESUMED record,
// because the pre-resume one has SSHAddr and HostIP cleared while paused.
func (o *Ops) Attach(ctx context.Context, c Caller, name string) (Endpoint, error) {
	const op = "attach"
	if _, err := o.owned(op, name, c); err != nil {
		return Endpoint{}, err
	}
	ctx, cancel := withBudget(ctx, DialTimeout)
	defer cancel()
	box, err := host.Prepare(ctx, o.boxes, name)
	if err != nil {
		return Endpoint{}, Fail(op, err)
	}
	o.log.Info("terminal attached", "user", c.Handle, "name", name)
	return Endpoint{Name: box.Name, SSHAddr: box.SSHAddr, SSHUser: box.SSHUser}, nil
}

// Touch marks a sandbox active. Called once at session end by the terminal
// bridge, exactly as the SSH gateway defers mgr.Touch — never on a keepalive,
// because a ping-driven Touch turns a forgotten browser tab into a permanently
// pinned VM.
func (o *Ops) MarkActive(name string) { o.boxes.MarkActive(name) }

// reread re-resolves a sandbox after a mutation so the result carries the state
// the manager actually settled on rather than the one we asked for. A record
// that vanished under us (a racing destroy) reports the same masked not-found
// any other caller would get.
func (o *Ops) reread(op, name string, c Caller) (SandboxInfo, error) {
	box, err := o.owned(op, name, c)
	if err != nil {
		return SandboxInfo{}, err
	}
	return o.info(box), nil
}
