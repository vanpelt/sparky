package ctlops

// The plan half of the in-guest capture verb: `sparkbox snapshot <tag>`, run
// from inside the sandbox it is about to pause.
//
// Everything here is a READ. Not "a read by convention" — the whole design of
// the guest door rests on it, because the box that issues the request is the
// box that stops, so nothing refusable may be discovered after the acceptance
// has been written. Every condition a user can act on is answered from this
// file, synchronously, on a healthy connection, while the VM is fully alive.
//
// The two restrictions that are not about state are here for the same reason
// rather than at bind time:
//
//   - `default` is refused, because every sandbox carries it, so "a tag you
//     carry" does not exclude it — and because a bind-time refusal would land
//     two minutes after the pause with nobody left to read it.
//   - a guest may re-point only a tag its own sandbox already carries. That
//     caps the new capability at the trust the box already had: secrets are
//     tag-scoped and pushed at create time, so re-pointing a tag makes every
//     future sandbox on it boot an image this box authored AND hands it that
//     tag's secrets. Restricted to a tag this box was already given, the
//     foothold buys persistence and no new material. Founding a NEW binding is
//     minting rather than moving, and minting stays at the operator door.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"context"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
)

// selfStemMax is how much of a tag survives into the snapshot name.
//
// The arithmetic is exact and it is the reason for the number: host's
// snapNameRe admits 41 characters, the timestamp is 11 (YYMMDD-HHMM) and the
// separator is 1, so the stem gets 29. secrets.tagRe admits 40, so a long tag
// is truncated rather than refused — refusing a tag somebody is ALREADY using,
// at the moment they want to capture, is a worse outcome than a name that is a
// legible prefix, and the plan prints the exact name before the prompt.
const selfStemMax = 29

// selfSnapNameRe mirrors host's snapNameRe (internal/host/snapshot.go), which
// is unexported and is the authority — Manager.Snapshot re-checks the name, and
// does it BEFORE the strip and before the pause, so a name it refuses costs an
// error and nothing else.
//
// The copy exists only so a bad explicit name is refused by the plan, in the
// session, instead of arriving as a driver failure after the acceptance. If the
// two ever disagree the manager wins and the only cost is a worse message.
var selfSnapNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// diskBusy is the optional "what is already running on this sandbox's disk"
// probe. An optional type assertion on the sandbox store, exactly like placer:
// *host.Manager has it, a fleet does not, and a store without it simply skips
// the warning rather than failing the plan.
type diskBusy interface {
	DiskOperation(name string) (string, bool)
}

// PlanSelfSnapshot answers "what would `sparkbox snapshot <tag>` do, and may I".
//
// It mutates nothing and wakes nothing. The tag may be empty, in which case it
// is defaulted from the tags this sandbox carries — `default` is never a
// candidate, because a template bound to it would become the base image of
// every box its owner ever creates.
//
// name may be empty too, in which case it is derived. Both are echoed back in
// the plan and the commit sends those values rather than re-deriving them, so
// the capture happens under exactly the name the user was shown.
func (o *Ops) PlanSelfSnapshot(ctx context.Context, c Caller, sandbox, tag, name string) (SelfSnapshotPlan, error) {
	const op = "snapshot.self"
	box, err := o.owned(op, sandbox, c)
	if err != nil {
		return SelfSnapshotPlan{}, err
	}
	// Driver first: a host that cannot capture at all has nothing else worth
	// saying, and the answer does not depend on any argument.
	if !o.templates.Snapshotter() {
		return SelfSnapshotPlan{}, Disabled(op,
			"this host cannot capture templates (its VM driver has no snapshot support). Nothing was changed.")
	}
	if o.tags == nil {
		return SelfSnapshotPlan{}, Disabled(op,
			"tagging is not enabled on this host, and a capture from inside a sandbox is bound to one of its tags.")
	}
	// Refused HERE and not at bind. This is the check that keeps us from
	// capturing for two minutes and then failing to record what it was for,
	// with the session already gone.
	if o.templateTags == nil {
		return SelfSnapshotPlan{}, Disabled(op, fmt.Sprintf(
			"this gateway cannot bind a snapshot to a tag. Capture it by hand and it will still be forkable:\n"+
				"  %s snapshot create %s <name>", o.ctlHint(), box.Name))
	}

	carried, err := o.tags.TagsFor(box.Name)
	if err != nil {
		return SelfSnapshotPlan{}, Fail(op, err)
	}
	sort.Strings(carried)
	tag, err = o.selfTag(op, box.Name, tag, carried)
	if err != nil {
		return SelfSnapshotPlan{}, err
	}

	if name == "" {
		name = snapshotNameFor(tag, o.now())
	}
	if !selfSnapNameRe.MatchString(name) {
		return SelfSnapshotPlan{}, Invalid(op, "invalid_name",
			"invalid snapshot name %q (want [a-z0-9][a-z0-9-]*, max 41 chars)", name)
	}

	// One pass over the owner's snapshots answers two questions: is the name
	// free, and where does the tag's current template live. Two passes would be
	// two answers to a store that can change between them.
	var boundFrom, boundNode string
	current, boundAt, err := o.currentBinding(op, c.Handle, tag)
	if err != nil {
		return SelfSnapshotPlan{}, err
	}
	for _, s := range o.templates.Snapshots(c.Handle) {
		if s.Name == name {
			return SelfSnapshotPlan{}, &Error{
				Kind: KindConflict, Op: op, Code: "snapshot_name_taken",
				Msg: fmt.Sprintf("a snapshot named %q already exists. Wait a minute and try again, "+
					"or name it yourself:\n  sparkbox snapshot %s my-name", name, tag),
				Details:  map[string]any{"snapshot": name, "tag": tag},
				Verbatim: true,
			}
		}
		if s.Name == current {
			boundFrom, boundNode = s.FromBox, s.Node
		}
	}

	plan := SelfSnapshotPlan{
		Sandbox: box.Name, Tags: carried, Tag: tag, Snapshot: name,
		Bound: current, BoundFrom: boundFrom, BoundAt: boundAt,
		Turbo: box.Turbo, DiskMB: box.DiskMB,
		CtlHint: o.ctlHint(), SSHHint: o.sshHint(box.Name),
	}
	// Node is reported on the same terms SnapshotInfo.Node is: only where a
	// machine name means something. A one-machine host stamps "local" on every
	// record, and a plan that said "this will live on local" would invent a
	// fleet nobody has.
	if _, named := o.boxes.(placer); named {
		plan.Node = box.Node
		if boundNode != "" && boundNode != box.Node {
			plan.BoundNode = boundNode
		}
	}
	plan.Carriers = o.carriersOf(c.Handle, box.Name, tag)
	if probe, ok := o.boxes.(diskBusy); ok {
		if what, busy := probe.DiskOperation(box.Name); busy {
			plan.Busy = what
		}
	}
	plan.Token = plan.digest()
	return plan, nil
}

// selfTag resolves and authorizes the tag a capture will be bound to.
//
// The order of the three refusals is the order a user can act on them: a tag
// that is not a tag at all is a typo (exit 2), a tag that is `default` or that
// this box does not carry is a refusal about authority (exit 3). Answering
// "you don't carry it" for a malformed tag would send somebody looking at their
// tag list for a string that could never have been in it.
func (o *Ops) selfTag(op, sandbox, tag string, carried []string) (string, error) {
	if tag == "" {
		var candidates []string
		for _, t := range carried {
			if t != secrets.DefaultTag {
				candidates = append(candidates, t)
			}
		}
		switch len(candidates) {
		case 1:
			return candidates[0], nil
		case 0:
			return "", Invalid(op, "no_candidate_tag",
				"this sandbox carries no tag to capture into (only %q, which cannot carry a template).\n\n"+
					"A capture is bound to a tag this sandbox already has, so the image and the secrets it was "+
					"built with stay together. Give this box a tag from outside, then try again:\n"+
					"  %s tags %s web\n  sparkbox snapshot web",
				secrets.DefaultTag, o.ctlHint(), sandbox)
		default:
			lines := make([]string, 0, len(candidates))
			for _, t := range candidates {
				lines = append(lines, "  sparkbox snapshot "+t)
			}
			return "", Invalid(op, "several_candidate_tags",
				"this sandbox carries more than one tag, so it cannot guess. Name one:\n%s",
				strings.Join(lines, "\n"))
		}
	}

	want := strings.ToLower(strings.TrimSpace(tag))
	if !secrets.ValidTag(want) {
		return "", Invalid(op, "bad_tag", "invalid tag %q (want [a-z0-9][a-z0-9-]*, max 40 chars)", tag)
	}
	if want == secrets.DefaultTag {
		// Refused HERE as well as at bind, and that is not belt and braces: a
		// bind-time refusal would land two minutes after the pause, with the
		// session gone and nobody to read it.
		//
		// The sentence is the store's, owner-scoped rather than fleet-scoped,
		// because that is what the schema actually says: template_tags is keyed
		// (owner, tag) and every sandbox_tags join in this tree carries the
		// owner on both sides, so a `default` binding reaches its own owner's
		// sandboxes and nobody else's. Two doors telling one user two different
		// stories about the same refusal is worse than either wording.
		return "", &Error{
			Kind: KindDenied, Op: op, Code: "tag_is_default",
			Msg: fmt.Sprintf("a template cannot be bound to the tag %q — every sandbox you create carries "+
				"that tag, so this snapshot would silently become the base image for all of them, "+
				"including ones you make months from now. Bind it to a name you also put on the "+
				"sandboxes you mean to re-base.", secrets.DefaultTag),
			Details:  map[string]any{"tag": want},
			Verbatim: true,
		}
	}
	for _, t := range carried {
		if t == want {
			return want, nil
		}
	}
	// Checked against the store's answer for THIS sandbox, never against
	// anything the request asserted. Both hints are printed because they are
	// genuinely different gestures: widen this box's trust, or do the whole
	// thing from the door that can already mint a binding.
	//
	// The `tags` hint repeats the tags the box already has, because SetTags
	// REPLACES the set — a hint naming only the new tag would silently take
	// away the secrets and checkouts this box is running on. `default` is left
	// out of it because Create stamps it back on unconditionally.
	widen := []string{want}
	for _, t := range carried {
		if t != secrets.DefaultTag {
			widen = append(widen, t)
		}
	}
	return "", &Error{
		Kind: KindDenied, Op: op, Code: "tag_not_on_sandbox",
		Msg: fmt.Sprintf("%s does not carry the tag `%s`, and a sandbox may only re-point a tag it already "+
			"carries — it already holds that tag's secrets and checkouts, so it widens nothing. It carries: %s.\n\n"+
			"Give this box the tag and try again:\n  %s tags %s %s\n"+
			"or do the whole thing from outside:\n  %s snapshot create %s <name> --tag %s",
			sandbox, want, strings.Join(carried, ", "),
			o.ctlHint(), sandbox, strings.Join(widen, " "),
			o.ctlHint(), sandbox, want),
		Details:  map[string]any{"tag": want, "tags": carried},
		Verbatim: true,
	}
}

// currentBinding is what this tag boots from today, or ("", zero) when nothing
// is bound. A store failure is fatal to the plan rather than logged: the whole
// gesture is "re-point this tag", and a plan that could not read the binding
// cannot tell the user what they are about to replace.
func (o *Ops) currentBinding(op, owner, tag string) (string, time.Time, error) {
	bindings, err := o.templateTags.BindingsForTags(owner, []string{tag})
	if err != nil {
		return "", time.Time{}, Fail(op, err)
	}
	for _, b := range bindings {
		if b.Tag == tag {
			return b.Snapshot, b.CreatedAt, nil
		}
	}
	return "", time.Time{}, nil
}

// carriersOf lists the owner's sandboxes carrying tag, with their states.
//
// It is the input to the plan's subtlest warning — re-pointing a tag does not
// re-base a sandbox that already exists, running OR paused — and it costs no
// new store query beyond the tag lookups the listing already does.
//
// A tag-store hiccup on one box drops that box from the warning rather than
// failing the plan: this is decoration on a decision, the same rule info()
// applies, and a capture refused because one row could not be read would be a
// worse answer than a warning listing three sandboxes instead of four.
func (o *Ops) carriersOf(owner, self, tag string) []TaggedSandbox {
	var out []TaggedSandbox
	for _, b := range o.boxes.ListByOwner(owner) {
		tags, err := o.tags.TagsFor(b.Name)
		if err != nil {
			o.log.Warn("could not read a sandbox's tags while planning a capture",
				"user", owner, "sandbox", b.Name, "err", err)
			continue
		}
		for _, t := range tags {
			if t == tag {
				out = append(out, TaggedSandbox{Name: b.Name, State: string(b.State), Self: b.Name == self})
				break
			}
		}
	}
	// This sandbox first, then alphabetical. The box the reader is sitting in is
	// the one whose session is about to end, so it leads the list rather than
	// being hunted for in it; everything after it is ordered so the warning
	// reads the same way twice.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Self != out[j].Self {
			return out[i].Self
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// snapshotNameFor derives the name a capture takes: <stem>-<YYMMDD-HHMM> UTC.
//
// Versioned rather than named after the tag, because Manager.Snapshot refuses a
// name that already exists — so a bare `web` would break the SECOND
// `sparkbox snapshot web`, which is the gesture this feature exists for.
// Overwrite is worse still: it needs delete-then-create, is not atomic,
// destroys the only thing that makes a bad re-point recoverable, and fights the
// refusal that keeps a bound snapshot from being deleted.
//
// The previous generation survives, unbound, so `snapshot ls` becomes that
// tag's history and a rollback is one `snapshot bind` away.
func snapshotNameFor(tag string, at time.Time) string {
	return snapshotStem(tag) + "-" + at.UTC().Format(snapshotStampLayout)
}

// snapshotStampLayout and snapshotStampRe are the timestamp half of that name,
// written down once because two things now depend on it: the writer above, and
// the retention sweep in envbuild.go, which recognises the captures an `env
// build` made by the shape of the name it gave them. A sweep matching a pattern
// the writer had moved on from would silently stop deleting anything, so the
// pattern and the format string live beside each other.
const snapshotStampLayout = "060102-1504"

var snapshotStampRe = regexp.MustCompile(`^\d{6}-\d{4}$`)

// snapshotStem is the part of a generated name that identifies the tag: the tag
// itself, truncated to what host's 41-character limit leaves after the stamp,
// with any trailing separator removed so the one below is the only one.
func snapshotStem(tag string) string {
	stem := tag
	if len(stem) > selfStemMax {
		stem = stem[:selfStemMax]
	}
	return strings.TrimRight(stem, "-")
}

// digest is the plan's token: a fingerprint of every fact the plan REPORTED.
//
// The commit re-runs the plan and compares, so a binding, a carrier set or a
// tag list that moved while the user was reading is refused rather than acted
// on — they agreed to warnings about a world that no longer exists. It covers
// exactly what was rendered and nothing else: a field the user was never shown
// changing is not a reason to make them run the command again.
func (p SelfSnapshotPlan) digest() string {
	h := sha256.New()
	add := func(parts ...string) {
		for _, s := range parts {
			h.Write([]byte(s)) //nolint:errcheck
			h.Write([]byte{0}) //nolint:errcheck
		}
	}
	add(p.Sandbox, p.Tag, p.Snapshot, p.Node, p.Bound, p.BoundNode)
	add(p.Tags...)
	for _, c := range p.Carriers {
		add(c.Name, c.State)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ctlHint is the `ssh ctl@<domain>` prefix every hint that names the gateway is
// built from. Host-authored on purpose: a guest is never told its own domain,
// so a shell composing this would have to guess. A host with no --proxy-domain
// prints the placeholder the rest of this surface already uses.
func (o *Ops) ctlHint() string {
	if o.domain == "" {
		return "ssh ctl@<gateway>"
	}
	return "ssh ctl@" + o.domain
}

// sshHint is how the person reading a plan gets back into this sandbox after it
// is paused — the one line that makes "nothing is lost" checkable.
//
// The sandbox is the SSH USER, not the host. `ssh <sandbox>.<domain>` resolves
// (the wildcard points at this gateway) and then fails with `no sandbox named
// <your local username>`, because routing reads the user and ssh sends whoever
// you are locally. It is printed to somebody whose snapshot or build just
// stopped, which is the worst moment to hand out a command that almost works;
// the ctlHint above has always had the shape right.
func (o *Ops) sshHint(sandbox string) string {
	if o.domain == "" {
		return "ssh " + sandbox + "@<gateway>"
	}
	return "ssh " + sandbox + "@" + o.domain
}
