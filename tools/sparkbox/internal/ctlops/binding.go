package ctlops

// Tag templates: binding a tag to the snapshot that every sandbox carrying it
// boots from.
//
// This is the fourth thing a tag can mean. It already selects secrets
// (internal/secrets), a repo checkout (internal/repos) and an egress rule set
// (internal/netrules); now it can also select the rootfs. The difference that
// shapes this whole file is that the other three COMPOSE — two tags mean the
// union of two secret sets, or netrules' subtraction — while a sandbox has
// exactly one disk. Two tags binding two snapshots have no answer that is not a
// coin flip, so the create is refused and both are named. A precedence rule
// would mean somebody gets a sandbox with the wrong CUDA in it and finds out
// twenty minutes later.
//
// The second thing that shapes it is that a snapshot is a file in ONE machine's
// image directory. Binding a tag to one quietly turns `--tag cuda` into a
// placement directive, so the resolution carries the snapshot's node and build()
// follows it — see the canPlace gate in sandbox.go, which is what keeps that
// from breaking every single-machine host.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/templates"
)

// resolvedTemplate is the answer to "which disk does this sandbox boot from",
// computed once at the top of Create and carried into build().
//
// Image is always set — it falls back to the operator's DefaultImage — so a
// caller can use it without asking whether a binding was involved. Tag and
// Snapshot are empty exactly when no binding was: they exist so a placement
// failure can explain itself, and `Tag != ""` is the test build() uses to decide
// whether a failure has a binding to blame.
type resolvedTemplate struct {
	Image    string // the driver-level image/template basename
	Node     string // the machine holding it; "" when no binding was involved
	Tag      string // the tag that bound it
	Snapshot string // the user-facing snapshot name the tag points at
	// Port is the default port the snapshot was captured on, or 0 for the stock
	// one. It rides here rather than being looked up again after the build
	// because this is the function that already knows WHICH snapshot the tags
	// resolved to — a second lookup downstream would have to re-run the
	// ambiguity rules to find out, and could disagree with this one.
	Port int
}

// BindTemplate points one of the caller's tags at one of their snapshots, so
// every sandbox they create carrying that tag boots from it.
//
// The ownership gate runs FIRST, before the tag is even looked at, so binding a
// snapshot that belongs to somebody else produces the byte-identical masked
// not-found a stranger gets for a name nobody holds — see ownedSnapshot. A
// refusal that said "bad tag" for one and "no such snapshot" for the other would
// confirm the existence of another owner's template.
func (o *Ops) BindTemplate(ctx context.Context, c Caller, snapshot, tag string) (TemplateBindResult, error) {
	const op = "snapshot.bind"
	if o.templateTags == nil {
		return TemplateBindResult{}, Disabled(op, "template bindings are not enabled on this host")
	}
	if _, err := o.ownedSnapshot(op, snapshot, c); err != nil {
		return TemplateBindResult{}, err
	}
	tags, err := NormalizeTags([]string{tag})
	if err != nil {
		return TemplateBindResult{}, Invalid(op, "bad_tag", "%v", err)
	}
	switch len(tags) {
	case 0:
		return TemplateBindResult{}, Invalid(op, "missing_tag", "a tag is required: snapshot bind %s --tag <tag>", snapshot)
	case 1:
	default:
		// NormalizeTags splits on commas, so this is reached by `--tag a,b`.
		// bind takes exactly one tag — a tag has one template, so `--tag a
		// --tag b` has no meaning here — and refusing it up front is also what
		// keeps this verb free of the only partial-failure mode it could have
		// had: two writes where the second fails and the first has landed.
		return TemplateBindResult{}, Invalid(op, "too_many_tags",
			"bind takes exactly one tag: a tag has one template, so naming two says nothing about which snapshot either of them gets.")
	}

	b, prev, err := o.templateTags.Bind(c.Handle, tags[0], snapshot)
	if err != nil {
		// The store's refusals — the `default` sentence above all — are already
		// user-facing and more specific than anything this layer could say about
		// the same argument, so they pass through whole. See templates.Bind.
		if errors.Is(err, templates.ErrInvalidBinding) {
			return TemplateBindResult{}, verbatim(Invalid(op, "bad_binding", "%v", err))
		}
		return TemplateBindResult{}, Fail(op, err)
	}
	o.log.Info("template bound", "user", c.Handle, "tag", b.Tag, "snapshot", b.Snapshot, "previous", prev)
	return TemplateBindResult{
		Binding:  TemplateBinding{Tag: b.Tag, Snapshot: b.Snapshot, CreatedAt: b.CreatedAt},
		Previous: prev,
	}, nil
}

// UnbindTemplate drops the caller's binding for a tag and reports what it was.
//
// Nothing is deleted and no sandbox changes: boxes already created from the
// template keep running on their own reflinked rootfs, and the next create on
// that tag takes the operator's default image again. The binding comes back
// because an unbind that only says "ok" is indistinguishable from an unbind of
// the wrong tag.
func (o *Ops) UnbindTemplate(ctx context.Context, c Caller, tag string) (TemplateBinding, error) {
	const op = "snapshot.unbind"
	if o.templateTags == nil {
		return TemplateBinding{}, Disabled(op, "template bindings are not enabled on this host")
	}
	tags, err := NormalizeTags([]string{tag})
	if err != nil {
		return TemplateBinding{}, Invalid(op, "bad_tag", "%v", err)
	}
	switch len(tags) {
	case 0:
		return TemplateBinding{}, Invalid(op, "missing_tag", "a tag is required: snapshot unbind --tag <tag>")
	case 1:
	default:
		return TemplateBinding{}, Invalid(op, "too_many_tags",
			"unbind takes exactly one tag: each tag has its own binding, and dropping several at once would report one outcome for several answers.")
	}

	b, err := o.templateTags.Unbind(c.Handle, tags[0])
	if err != nil {
		// Owner scoping is structural in the store's query, so another owner's
		// tag and a tag nobody has bound arrive here as the same sentinel and
		// leave as the same masked answer.
		if errors.Is(err, templates.ErrNoSuchBinding) {
			return TemplateBinding{}, NotFound(op, "template_binding", tags[0])
		}
		if errors.Is(err, templates.ErrInvalidBinding) {
			return TemplateBinding{}, verbatim(Invalid(op, "bad_binding", "%v", err))
		}
		return TemplateBinding{}, Fail(op, err)
	}
	o.log.Info("template unbound", "user", c.Handle, "tag", b.Tag, "snapshot", b.Snapshot)
	return TemplateBinding{Tag: b.Tag, Snapshot: b.Snapshot, CreatedAt: b.CreatedAt}, nil
}

// resolveTemplate decides which disk a create boots from, from the tags Create
// has already computed for a sandbox that does not exist yet.
//
// It reads those tags rather than joining sandbox_tags on the new name, and that
// is not an optimisation: stampTags has not run and MUST not run before the
// refusal (Create's own comment at sandbox.go says why), so a join would find no
// rows and answer "no template" for every create there has ever been.
//
// A store failure is fatal to the create. Unlike info()'s tag lookup, which is
// decoration on a row, this one decides which rootfs boots — swallowing it would
// silently hand somebody the stock image and let them discover twenty minutes
// later that none of their toolchain is there.
func (o *Ops) resolveTemplate(op, owner string, tags []string) (resolvedTemplate, error) {
	if o.templateTags == nil || len(tags) == 0 {
		return resolvedTemplate{Image: o.defaultImage}, nil
	}
	bindings, err := o.templateTags.BindingsForTags(owner, tags)
	if err != nil {
		return resolvedTemplate{}, Fail(op, err)
	}
	if len(bindings) == 0 {
		return resolvedTemplate{Image: o.defaultImage}, nil
	}
	// Several tags may agree — binding `cuda` and `ml` to one snapshot is a
	// perfectly ordinary thing to do — so what is refused is more than one
	// distinct SNAPSHOT, not more than one binding.
	for _, b := range bindings[1:] {
		if b.Snapshot != bindings[0].Snapshot {
			return resolvedTemplate{}, ambiguousTemplate(op, bindings)
		}
	}

	// Resolve through the caller's own snapshot list, which is where both the
	// image basename and the machine holding it live (host.Snapshot carries
	// Image and Node). Recomputing `snap-<owner>-<name>` here would be a second
	// copy of a rule the manager already owns, and would still not answer the
	// node question.
	want := bindings[0]
	for _, s := range o.templates.Snapshots(owner) {
		if s.Name == want.Snapshot {
			// The tag reported is the first in the store's tag order, which is
			// deterministic; when several tags agree they are interchangeable
			// by construction, since they all named this one snapshot.
			return resolvedTemplate{
				Image: s.Image, Node: s.Node, Tag: want.Tag, Snapshot: s.Name,
				Port: o.templatePort(owner, s.Name),
			}, nil
		}
	}
	// The binding points at a snapshot that is gone — deleted through a surface
	// that does not route through DeleteSnapshot's refusal, which today means
	// the user console (userconsole/console.go calls the manager directly).
	//
	// This must NEVER degrade into a silent fall back to the default image. A
	// create that quietly boots the stock rootfs when the user asked for their
	// CUDA one is exactly the failure this whole design exists to refuse, and it
	// is invisible until an agent inside the guest cannot find its toolchain.
	// WHEN THE TAG IS AN ENVIRONMENT, THE REPAIR IS A DIFFERENT VERB. `snapshot
	// unbind` throws the pointer away and reads as an instruction to dismantle
	// the thing you were trying to use; for an environment the disk is
	// derivable — the setup script is on the row — so the repair is to build it
	// again. Saying "unbind" to somebody whose environment lost its snapshot
	// sends them to demolish an object that is one command from working.
	//
	// The store read only happens on this failure path, never on the hot create
	// path, and a nil store or a miss falls back to the sentence below.
	if o.envs != nil {
		if e, err := o.envs.Get(owner, want.Tag); err == nil {
			// The second clause is not decoration. An environment can be
			// `ready` with NO script — somebody bound a snapshot to the tag by
			// hand, or finished a build with `env capture` — and telling that
			// person "the setup script is still on the environment, so nothing
			// is lost but the time" would promise them a rebuild that
			// reproduces their disk when nothing on the row can reproduce it.
			// `env rebuild` is still the right verb; what it costs is not.
			hint := fmt.Sprintf("Build its disk again with `env rebuild %s` — the setup script is still "+
				"on the environment, so nothing is lost but the time.", want.Tag)
			if strings.TrimSpace(e.SetupScript) == "" {
				hint = fmt.Sprintf("This environment has no setup script stored, so a rebuild starts from "+
					"whatever its repositories and `env script %s --set` provide — check `env show %s` "+
					"before running `env rebuild %s`.", want.Tag, want.Tag, want.Tag)
			}
			return resolvedTemplate{}, &Error{
				Kind: KindConflict, Op: op, Code: "template_missing",
				Msg: fmt.Sprintf("environment %q boots from snapshot %q, which no longer exists.",
					want.Tag, want.Snapshot),
				Hint: hint,
				Details: map[string]any{
					"template_tag": want.Tag, "template_snapshot": want.Snapshot, "environment": want.Tag,
				},
				Verbatim: true,
			}
		}
	}
	return resolvedTemplate{}, &Error{
		Kind: KindConflict, Op: op, Code: "template_missing",
		Msg: fmt.Sprintf("tag %q boots from snapshot %q, which no longer exists. "+
			"Bind the tag to a snapshot you still have, or unbind it to go back to the default image.",
			want.Tag, want.Snapshot),
		Hint:     fmt.Sprintf("snapshot unbind --tag %s", want.Tag),
		Details:  map[string]any{"template_tag": want.Tag, "template_snapshot": want.Snapshot},
		Verbatim: true,
	}
}

// ambiguousTemplate is the refusal for two tags that bind two different
// snapshots. It names every tag and every snapshot involved, because the caller
// typed the tags and has no other way to see which of them carries a binding —
// `snapshot ls` shows the mapping, but only after they know to go and look.
func ambiguousTemplate(op string, bindings []templates.Binding) *Error {
	pairs := make([]string, 0, len(bindings))
	tags := make([]string, 0, len(bindings))
	seen := map[string]bool{}
	var snaps []string
	for _, b := range bindings {
		pairs = append(pairs, fmt.Sprintf("%s→%s", b.Tag, b.Snapshot))
		tags = append(tags, b.Tag)
		if !seen[b.Snapshot] {
			seen[b.Snapshot] = true
			snaps = append(snaps, b.Snapshot)
		}
	}
	sort.Strings(snaps)
	return &Error{
		Kind: KindConflict, Op: op, Code: "template_ambiguous",
		Msg: fmt.Sprintf("these tags bind different base images (%s), and a sandbox has exactly one rootfs. "+
			"Create it with only one of those tags, or take a snapshot that has both and bind that.",
			strings.Join(pairs, ", ")),
		Details:  map[string]any{"tags": tags, "snapshots": snaps},
		Verbatim: true,
	}
}

// templateNodeAgrees refuses an explicit --node that contradicts the machine the
// bound template lives on.
//
// It is a refusal rather than an override in either direction because both
// overrides are wrong: honouring --node would build from the default image while
// the user believes they got their template, and honouring the binding would
// build somewhere they explicitly said not to. Either is silent, and the design
// (Part 4) names this the one combination that must be answered out loud.
//
// Either side being empty is agreement: an untagged create names no machine, and
// a snapshot with no node comes from a host that has no second machine to
// disagree about.
func (o *Ops) templateNodeAgrees(op, node string, t resolvedTemplate) error {
	if node == "" || t.Node == "" || node == t.Node {
		return nil
	}
	return &Error{
		Kind: KindConflict, Op: op, Code: "template_node_conflict",
		Msg: fmt.Sprintf("tag %q boots from snapshot %q, which only exists on %s, so this sandbox can't be built on %s. "+
			"Drop --node, or create it without that tag.", t.Tag, t.Snapshot, t.Node, node),
		// Details["node"] is the machine the binding names, not the one the
		// caller asked for: MachineNamed is documented to answer with the
		// machine the sentence is ABOUT, and the sentence is about where the
		// template is.
		Details: map[string]any{
			"node": t.Node, "requested_node": node,
			"template_tag": t.Tag, "template_snapshot": t.Snapshot,
		},
		Verbatim: true,
	}
}

// templatePlacementFailed re-states a placement failure that a binding caused.
//
// Without this, `--tag cuda` fails with a capacity or reachability sentence
// about a machine the user never named, and nothing anywhere connects that
// machine to the word they typed — which the design (Part 4) calls out as the
// most confusing error this feature could ship.
//
// It COPIES the classified error before rewriting it. AsError hands back the
// same *Error pointer for anything already classified (errors.go, the errors.As
// branch), so rewriting in place would edit internal/fleet's own error value —
// still referenced by whatever raised it, and by any other caller holding it.
// The Details map is cloned for the same reason. Kind, Code, Exit and Status all
// survive, because what happened is unchanged; only the explanation grows.
//
// The explanation goes in Msg and never in Hint: sshgw's failCtl prints only
// Msg for a verbatim error, and fail() renders only the error's own string for
// the rest, so a Hint would be invisible on the surface where this is read.
func (o *Ops) templatePlacementFailed(op string, t resolvedTemplate, err error) *Error {
	e := AsError(op, err)
	cp := *e
	cp.Details = maps.Clone(e.Details)
	if cp.Details == nil {
		cp.Details = map[string]any{}
	}
	cp.Details["node"] = t.Node
	cp.Details["template_tag"] = t.Tag
	cp.Details["template_snapshot"] = t.Snapshot

	msg := strings.TrimRight(cp.Msg, " ")
	if msg != "" && !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	cp.Msg = strings.TrimSpace(msg + fmt.Sprintf(
		" Tag %q binds snapshot %q, which only exists on %s, so this sandbox could not be built anywhere else.",
		t.Tag, t.Snapshot, t.Node))
	return &cp
}

// boundTags maps each of the owner's snapshots to the tags bound to it. A nil
// store answers an empty map rather than an error: a host with no bindings has
// no bound snapshots, which is a true answer rather than a missing one.
func (o *Ops) boundTags(owner string) (map[string][]string, error) {
	if o.templateTags == nil {
		return nil, nil
	}
	all, err := o.templateTags.BindingsForOwner(owner)
	if err != nil {
		return nil, err
	}
	m := make(map[string][]string, len(all))
	for _, b := range all {
		// The store orders by tag, so each list is deterministic and the column
		// it feeds reads the same way twice.
		m[b.Snapshot] = append(m[b.Snapshot], b.Tag)
	}
	return m, nil
}

// boundTagsMap is boundTags for the listing paths, where the bindings are
// decoration on a row rather than its subject — the same log-and-continue rule
// info() applies to a tag-store hiccup. A `snapshot ls` that fails outright
// because one extra column could not be filled in is worse than one that prints
// the rows.
func (o *Ops) boundTagsMap(owner string) map[string][]string {
	m, err := o.boundTags(owner)
	if err != nil {
		o.log.Warn("could not read template bindings", "user", owner, "err", err)
		return nil
	}
	return m
}

// boundTagsFor is the single-snapshot form of boundTagsMap, for the paths that
// project exactly one record.
func (o *Ops) boundTagsFor(owner, snapshot string) []string {
	return o.boundTagsMap(owner)[snapshot]
}
