package ctlops

// `--ref`: the branch ONE sandbox starts on.
//
// Every other thing a create decides comes from a tag — the secrets, the egress
// rules, the repositories, and now the rootfs. This does not, and the asymmetry
// is the whole point of it. A tag says "every box like this checks out
// hivemind"; `--ref feat/x` says "this one box, the one I am asking for right
// now, starts on feat/x". There is no tag that can mean the second thing, and
// inventing one would leave a tag behind that re-points every future create.
//
// So it is stored per sandbox (internal/repos, SetSandboxRefs) and applied as
// an overlay inside ReposForSandbox. Nothing else changes: the manifest already
// carries a ref, the node link already carries it, and the guest already reads
// it. The per-instance branch reaches a sandbox on any machine in the fleet the
// day the overlay lands, with no new endpoint and no new fleet capability.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
)

// RepoRef is one `--ref` as the caller stated it. Slug empty means the bare
// form — "the attachment", which is only unambiguous when there is one.
type RepoRef struct {
	Slug string `json:"slug,omitempty"`
	Ref  string `json:"ref"`
}

// ParseRepoRef reads one `--ref` value.
//
// The scoped form is `owner/name=ref`. The discriminator is a '=' whose
// left-hand side contains a '/', which is deliberately narrow: a branch name
// may legally contain '=', so treating any '=' as the scope separator would
// silently reinterpret `--ref weird=name` as a slug nobody attached. A slug
// always has exactly one '/', so requiring it makes the two forms
// distinguishable without a second flag.
func ParseRepoRef(value string) RepoRef {
	if i := strings.Index(value, "="); i > 0 && strings.Contains(value[:i], "/") {
		return RepoRef{Slug: value[:i], Ref: value[i+1:]}
	}
	return RepoRef{Ref: value}
}

// resolveRepoRefs turns the caller's `--ref` flags into the rows a sandbox's
// manifest will read, refusing anything it cannot place.
//
// A slug that matches no attachment is a REFUSAL rather than a silent no-op,
// and so is a bare `--ref` on a tag set carrying more than one repository. Both
// are the same mistake seen twice: somebody typed a branch and expects to find
// it checked out. A create that quietly ignored the flag would hand them a box
// on the wrong branch and no reason to look at the command they ran — they
// would find out in a build, twenty minutes later.
//
// It runs before any row is written, next to nameIsFree and placeable, on
// Create's own rule: a create that cannot possibly do what was asked must not
// leave rows behind for a sandbox that never exists.
func (o *Ops) resolveRepoRefs(op string, handle string, tags []string, want []RepoRef) ([]repos.SandboxRef, error) {
	if len(want) == 0 {
		return nil, nil
	}
	if o.repos == nil {
		return nil, Disabled(op, "repo attachments are not enabled on this host, so there is no checkout for --ref to name a branch of")
	}
	attached, err := o.reposForTags(op, handle, tags)
	if err != nil {
		return nil, err
	}
	if len(attached) == 0 {
		return nil, Invalid(op, "no_attachment", "--ref names a branch to check out, and no repository is attached to %s. Attach one first:\n  %s repo add <owner>/<name> --tag %s",
			tagList(tags), o.ctlHint(), firstTag(tags))
	}

	// Bare first, then scoped, so a scoped flag wins over a bare one however
	// they were ordered on the command line. Within each form the last wins,
	// which is what repeating a flag that names one thing means everywhere else
	// on this door.
	chosen := map[string]repos.SandboxRef{}
	for _, w := range want {
		if w.Slug != "" {
			continue
		}
		if len(attached) > 1 {
			return nil, Invalid(op, "ambiguous_ref",
				"--ref %s does not say which repository, and %s selects %d of them. Name one:\n  --ref %s=%s",
				w.Ref, tagList(tags), len(attached), attached[0].Slug, w.Ref)
		}
		chosen[key(attached[0].Host, attached[0].Slug)] = repos.SandboxRef{
			Host: attached[0].Host, Slug: attached[0].Slug, Ref: w.Ref,
		}
	}
	for _, w := range want {
		if w.Slug == "" {
			continue
		}
		match, ok := findRepo(attached, w.Slug)
		if !ok {
			return nil, Invalid(op, "no_such_attachment",
				"--ref names %s, which %s does not select. Attached here: %s",
				w.Slug, tagList(tags), slugList(attached))
		}
		chosen[key(match.Host, match.Slug)] = repos.SandboxRef{Host: match.Host, Slug: match.Slug, Ref: w.Ref}
	}

	out := make([]repos.SandboxRef, 0, len(chosen))
	for _, r := range chosen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Slug < out[j].Slug
	})
	return out, nil
}

// reposForTags is which of the owner's attachments this tag set selects.
//
// Computed here from ListRepos rather than asked of the store, because at this
// point in a create the sandbox has no sandbox_tags rows — it does not exist —
// so ReposForSandbox has nothing to join against. It is also the query ctlops
// deliberately does not have (see the Repos interface), and this is not a way
// around that: the answer is about the caller's OWN tags, computed from the
// caller's OWN attachments, both already in hand.
func (o *Ops) reposForTags(op, handle string, tags []string) ([]repos.Repo, error) {
	list, err := o.repos.ListRepos(handle)
	if err != nil {
		return nil, Fail(op, err)
	}
	carried := map[string]bool{}
	for _, t := range tags {
		carried[t] = true
	}
	var out []repos.Repo
	for _, r := range list {
		for _, t := range r.Tags {
			if carried[t] {
				out = append(out, r)
				break
			}
		}
	}
	return out, nil
}

// writeRepoRefs records the resolved overrides, and is a no-op when there are
// none — including the clear-on-rollback path, where passing an empty list is
// how a failed create removes rows it had already written.
func (o *Ops) writeRepoRefs(handle, sandbox string, refs []repos.SandboxRef) error {
	if o.repos == nil {
		return nil
	}
	return o.repos.SetSandboxRefs(handle, sandbox, refs)
}

// clearRepoRefs is writeRepoRefs' rollback half, swallowing its error the way
// clearTags does: the create has already failed and the caller has a better
// sentence to return than this one.
func (o *Ops) clearRepoRefs(handle, sandbox string, refs []repos.SandboxRef) {
	if o.repos == nil || len(refs) == 0 {
		return
	}
	if err := o.repos.SetSandboxRefs(handle, sandbox, nil); err != nil {
		o.log.Warn("could not clear the repo ref overrides of a sandbox that failed to build",
			"sandbox", sandbox, "err", err)
	}
}

func key(host, slug string) string { return host + "/" + strings.ToLower(slug) }

// findRepo matches a slug the way github.com does, which is without regard to
// case on both halves — the same rule the store's COLLATE NOCASE column
// applies, restated here because this comparison happens in Go.
func findRepo(list []repos.Repo, slug string) (repos.Repo, bool) {
	for _, r := range list {
		if strings.EqualFold(r.Slug, slug) {
			return r, true
		}
	}
	return repos.Repo{}, false
}

func slugList(list []repos.Repo) string {
	out := make([]string, 0, len(list))
	for _, r := range list {
		out = append(out, r.Slug)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func tagList(tags []string) string {
	if len(tags) == 0 {
		return "this sandbox's tags"
	}
	if len(tags) == 1 {
		return fmt.Sprintf("tag %s", tags[0])
	}
	return fmt.Sprintf("tags %s", strings.Join(tags, ", "))
}

func firstTag(tags []string) string {
	if len(tags) == 0 {
		return "<tag>"
	}
	return tags[0]
}
