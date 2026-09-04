package launch

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
)

// post answers POST /{owner}/{repo}: the button on the confirm page.
//
// The form carries no fields and no hidden inputs, and that is a design choice
// rather than a shortcut. Every value this handler acts on is re-derived here,
// from the path and the query it validated on the GET and validates again now,
// and from the verified session — so there is nothing in the request body for a
// visitor (or a page in another tab, or an old bookmark) to have edited. A form
// is not a promise.
//
// What it deliberately does NOT read: a raw tag, a node, a sandbox name, a
// command, or any URL-valued parameter. An environment is resolved and
// owner-checked again inside createOrReuse before ctlops receives its name.
func (h *Handler) post(w http.ResponseWriter, r *http.Request) {
	sess, _ := edgeauth.From(r.Context())
	c := ctlops.Caller{Handle: sess.Handle}

	t, bad := parseTarget(r.PathValue("owner"), r.PathValue("repo"),
		r.URL.Query().Get("ref"), r.URL.Query().Get("env"), r.URL.Query().Get(freshParam))
	if bad != nil {
		h.refuse(w, r, t, bad)
		return
	}

	info, err := h.createOrReuse(r.Context(), c, t)
	if err != nil {
		e := ctlops.AsError(op, err)
		h.log.Info("launch create refused", "user", c.Handle, "slug", t.Slug, "ref", t.Ref,
			"kind", e.Kind.String(), "code", e.Code)
		h.refuse(w, r, t, e)
		return
	}
	h.log.Info("launch created a sandbox", "user", c.Handle, "sandbox", info.Name,
		"slug", t.Slug, "ref", t.Ref)
	h.handoff(w, r, t, info)
}

// createOrReuse is the create, collapsed so that N simultaneous presses of the
// same button produce one sandbox.
//
// # Why singleflight, and what it is actually protecting against
//
// The realistic case is not an attack. It is a double-click, a second browser
// tab still open on the same confirm page, a mobile browser that reissues a
// request when the connection flaps, or somebody pressing the button again
// because a create takes up to fifteen seconds and nothing on a zero-JS page
// says it is working. Without collapsing, each of those is a whole extra
// sandbox: a VM, a 25 GB disk, a slot against --max-running-per-owner, and a
// secret-env push.
//
// golang.org/x/sync/singleflight rather than a mutex map keyed on
// URL-supplied strings, for the reason internal/host/manager.go:507 and
// internal/fleet/fleet.go:222 already use it: it deletes its own key when the
// call completes, so nothing accumulates for keys a visitor invented and never
// used again. A hand-rolled map keyed on attacker-supplied text with no
// eviction is a memory leak that `go test -race` does not catch.
//
// # Why the resolve runs again INSIDE the group
//
// This is the idempotency, and it is not optional. The follower of a collapsed
// pair gets the leader's answer, so a double-click lands both requests in the
// same sandbox. But a click that arrives a minute later — a retry after a lost
// response, a link reopened from history — is not collapsed by anything, and
// the only thing that stops it building a second sandbox is that the resolve it
// runs finds the first one. The GET's resolve cannot do this job: it happened
// before the button was pressed, and the world may have changed since.
//
// restapi's idempotency-key machinery is unexported and would not help here
// anyway: a form POST has no place to carry a key, and the natural key is
// exactly the tuple below.
func (h *Handler) createOrReuse(ctx context.Context, c ctlops.Caller, t target) (ctlops.SandboxInfo, error) {
	// The handle first, so one visitor's key can never collide with another's,
	// and lower-cased slug because the store's slug columns are COLLATE NOCASE
	// and `WANDB/Hivemind` and `wandb/hivemind` are one repository. NUL as the
	// separator because it is the one byte that cannot appear in a handle, a
	// slug or a ref — all three are validated against charsets that exclude it
	// — so no pair of distinct tuples can render to the same key.
	//
	// The ref in the key is the NORMALIZED one, not the raw query value, and
	// that is the whole reason for the pre-read below. The reuse rule already
	// treats a link with no `?ref=` and a link naming the attachment's own
	// default branch as the same target — that is what normalizeRef is for — so
	// keying on the raw value would give those two spellings different keys.
	// Two tabs, one on the README's no-ref badge and one on a `?ref=main`
	// badge, would then each become a leader, each re-resolve before the other
	// committed, each find nothing, and each build a sandbox: two VMs, two
	// 25GB disks, and both slots of the default --max-running-per-owner spent
	// on one branch. Folding first makes those two links one flight.
	//
	// A failed or empty pre-read is not an error here. It only costs collapsing,
	// never correctness: the resolve INSIDE the flight is the authority and runs
	// again regardless, so a stale read can at worst produce a key that fails to
	// merge two requests, which is exactly the behaviour we had before.
	want := t.Ref
	if list, err := h.repos.ListRepos(c.Handle); err == nil {
		if att, ok := findRepo(list, t.Slug); ok {
			want = normalizeRef(att.Ref, t.Ref)
		}
	}
	key := c.Handle + "\x00" + strings.ToLower(t.Slug) + "\x00" + want + "\x00" + strings.ToLower(t.Env)
	if t.Fresh {
		// A DIFFERENT flight, because the two buttons want opposite things from
		// the same tuple. Collapsing them would make the outcome depend on
		// which press arrived first: a "create a new one" that merged into an
		// in-flight reuse would hand back the very sandbox the visitor had just
		// said they did not want, and it would do it silently, with the
		// redirect looking exactly like a success.
		//
		// Two presses of the SAME button still collapse, which is the whole
		// point of the group — a double-click on "create a new one" is one new
		// sandbox, not two.
		key += "\x00new"
	}

	// The shared work is detached from THIS request's cancellation, deliberately
	// — the same reason internal/host/manager.go holds a process-lifetime ctx
	// for its own singleflight group (manager.go:508, "process lifetime for
	// shared restore/resume work").
	//
	// singleflight runs the function on the leader's goroutine, so with a plain
	// r.Context() the leader closing its tab would cancel a create that a
	// follower is still waiting on, and — worse — would abandon it midway,
	// which is exactly the state that leaves a name taken and rows written for
	// a sandbox nobody can see. Detaching is safe because ctlops.Ops.Create
	// imposes its own fifteen-second budget on the context it is handed
	// (internal/ctlops/sandbox.go:53), so nothing here can run unbounded; and a
	// create that finishes after its visitor left is not waste, because the
	// next click's resolve finds it.
	shared := context.WithoutCancel(ctx)

	got, err, _ := h.creates.Do(key, func() (any, error) {
		att, best, _, err := h.resolve(shared, c, t)
		if err != nil {
			return nil, err
		}
		// The reuse, and the one thing `?new=1` turns off. Note what that
		// costs: the idempotency this resolve provides is gone with it, so a
		// second press a minute later — a retry after a lost response, the
		// page reopened from history — really does build a second sandbox.
		// That is the request, not a defect: the visitor pressed a button that
		// says "create a new one" on a page that had already offered them the
		// one they have. The ceiling is still --max-running-per-owner.
		if best != nil && !t.Fresh {
			return best.Info, nil
		}

		// The SCOPED ref form, always — `{Slug: att.Slug, Ref: want}` and never
		// a bare `{Ref: want}`. An unscoped override applies to every
		// repository the create's tags select, so on a tag carrying two
		// repositories ctlops.resolveRepoRefs refuses it as ambiguous_ref. The
		// scoped form makes that failure structurally impossible rather than
		// merely unlikely, which matters because the second repository usually
		// arrives long after the badge was posted — somebody adds a repo to
		// `default` and every launch link in every comment starts 400ing.
		//
		// nil when the branch folds to the attachment's own default:
		// repos.SetSandboxRefs refuses an empty override (store.go:470-472),
		// and an override that merely restates the default is a row that says
		// nothing.
		var refs []ctlops.RepoRef
		if want := normalizeRef(att.Ref, t.Ref); want != "" {
			refs = []ctlops.RepoRef{{Slug: att.Slug, Ref: want}}
		}

		// On the ordinary path tags come from the attachment and from nowhere
		// else — never from the URL, and never derived from the slug or branch.
		// On the environment path ctlops expands the owner-checked Env. NormalizeTags
		// does not validate the charset, so a tag synthesised from a repository
		// name would sail past it and die inside stampTags as a 500 on a
		// half-built sandbox.
		//
		// launchTags may leave one of them off, and only ever one that names an
		// environment: two environments on an attachment bind two base images,
		// and a sandbox has one rootfs. See its comment for why the alternative
		// — Create's own `template_ambiguous` refusal — is the wrong answer to
		// give somebody who arrived by clicking a link. It is the SAME call the
		// confirm page made, so the pills the visitor agreed to are the tags
		// this creates with.
		//
		// Everything else Create does we reimplement none of: name generation,
		// the free-name and placement refusals, template resolution, the
		// tags-before-create ordering the secret push depends on, and the
		// rollback of both tag rows and ref rows on any failure after stamping.
		args := ctlops.CreateArgs{Refs: refs}
		if t.Env != "" {
			// Env is the constrained override. Passing the attachment's complete
			// tag set as well would accidentally combine every environment that
			// happens to carry this repository and therefore their secrets.
			args.Env = t.Env
		} else {
			// An ordinary link keeps the base branch's ambiguity fallback: retain
			// every non-environment tag and select the most recently updated one
			// when this attachment belongs to several environments.
			args.Tags = h.launchTags(c, att).Tags
		}
		si, err := h.ops.Create(shared, c, args)
		if err != nil {
			return nil, err
		}

		// Read it back before handing out a URL. internal/xterm's own resolve
		// answers 404 identically for "no such sandbox" and "not yours", with
		// no retry affordance and no explanation (xterm.go:294-309), so a
		// redirect to a record the control plane cannot yet see renders as a
		// flat "not found" on a sandbox that was just built — and this tree has
		// been bitten by post-create visibility races on fleet nodes before.
		// One Get is a cheap way to make the 303 a promise.
		back, err := h.ops.Get(shared, c, si.Name)
		if err != nil {
			return nil, err
		}
		// The secrets go in BEFORE the 303, for the reason the two attach paths
		// already document at their own doors: pam_env reads /etc/environment
		// once, at session setup, and Create's push is asynchronous.
		//
		// This door needs its own barrier because neither attach path can be
		// this one's. internal/xterm gates its wait on `box.State !=
		// StateRunning` (ws.go:189) — but a sandbox this handler just built is
		// ALREADY running when the browser follows the redirect, so that gate
		// reads false and the terminal opens with the push still in flight. The
		// SSH gateway does not have the bug only because it carries a second
		// term for exactly this case, `viaNewDoor` (gateway.go:547), and a
		// redirect into a browser terminal has no equivalent to carry.
		//
		// It is here, inside the flight and after the read-back, so that the
		// followers of a collapsed double-click are held by it too: they take
		// the leader's answer, so a barrier outside the group would be one the
		// leader alone waited at.
		h.awaitEnv(shared, back.Name)
		return back, nil
	})
	if err != nil {
		return ctlops.SandboxInfo{}, err
	}
	info, ok := got.(ctlops.SandboxInfo)
	if !ok {
		// Unreachable: the closure above returns a SandboxInfo or an error.
		// Asserted rather than assumed because a type assertion on a shared
		// any is exactly where a future edit that returns a *SandboxInfo would
		// otherwise panic inside a request goroutine.
		return ctlops.SandboxInfo{}, ctlops.Fail(op, errBadShare)
	}
	return info, nil
}

// errBadShare names the unreachable case above so the assertion has something
// to report other than a panic.
var errBadShare = errors.New("launch: the shared create returned an unexpected value")

// envAwaiter is the synchronous secret-env delivery, taken by assertion rather
// than added to Sandboxes. Sandboxes is narrow on purpose and this is not a
// capability a deployment chooses — the one real implementation, *ctlops.Ops,
// always has it, and a test fake that does not is a fake with no guest to
// deliver into. The same shape, and the same reasoning, as internal/xterm's
// assertion off Attacher (ws.go:373).
type envAwaiter interface {
	AwaitEnv(ctx context.Context, name string) error
}

// envAwaitBudget bounds the wait. The push dials the guest's sshd, which the
// browser is about to reach through the terminal anyway, so this mostly
// overlaps a wait the click was already going to make; the bound is there so a
// guest that never answers costs a slow redirect rather than a hung one.
const envAwaitBudget = 30 * time.Second

// awaitEnv delivers the owner's secrets into a freshly created sandbox before
// the redirect that opens a session on it.
//
// Never fatal. A sandbox worth building is worth handing over without its
// environment, the next transition to running pushes again, and failing the
// create here would turn a missing variable into a button that does nothing.
// Always logged, though — an environment that quietly failed to arrive is the
// failure this barrier exists to end, and the launch door's user is in a
// browser with no stderr to be told on.
func (h *Handler) awaitEnv(ctx context.Context, name string) {
	a, ok := h.ops.(envAwaiter)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, envAwaitBudget)
	defer cancel()
	if err := a.AwaitEnv(ctx, name); err != nil {
		h.log.Warn("secrets not delivered before the launch redirect", "name", name, "err", err)
	}
}
