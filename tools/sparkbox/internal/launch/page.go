package launch

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// controlUser is the SSH account every `ssh <user>@<domain> …` line on these
// pages names. internal/sshgw.ControlUser is the authority on it; the value is
// restated here rather than imported because importing internal/sshgw would
// pull an entire SSH server — gliderlabs/ssh, the PTY layer, the whole control
// surface — into an HTTP handler that needs one four-letter string. If that
// account is ever renamed, the compiler will not find this copy: grep for
// ControlUser and fix both.
const controlUser = "ctl"

// pageData is the whole contract between page.go and page.html, and it is
// deliberately flat strings rather than the domain types.
//
// The reason is the one that decides everything about this page: it renders for
// somebody who arrived from a stranger's comment, so every string in it has to
// be one this package chose. Handing the template a repos.Repo or a
// ctlops.SandboxInfo would put the store's fields one dot away from a future
// {{.Att.Something}} that prints a value nobody audited — an access level, an
// internal id, a node name. Flattening here means the set of facts this page
// can possibly disclose is the list below, readable in one screen.
//
// html/template escapes every action regardless; this is about what we choose
// to say, not about injection.
type pageData struct {
	// Title is the browser tab, Heading and HeadingSlug the page's own headline.
	// HeadingSlug is separated so the repository renders in the monospace face
	// without the heading having to carry markup through the template.
	Title       string
	Heading     string
	HeadingSlug string
	// Lead is the one paragraph under the headline that says what is about to
	// happen, in plain language, for a reader who has never seen sparkbox.
	Lead string
	// Facts is the label/value block: what the create would do, itemised.
	Facts []fact
	// Caution is the single amber sentence about running a branch somebody else
	// chose. Amber and not red on purpose — this is the thing being agreed to,
	// not an error, and a red panel over a routine action teaches people to
	// click through red panels.
	Caution string
	// Command is one copy-pasteable shell line. Exactly one, because a page
	// offering three commands is a page nobody reads.
	Command string
	// Steps is the numbered follow-up under the command.
	Steps []step
	// Action is the form's action — a path and query, never an absolute URL, so
	// the POST cannot be aimed anywhere but back at this door. Empty renders no
	// form at all, which is what makes the not-attached screen buttonless
	// rather than a button that would build an empty sandbox.
	Action     string
	Submit     string
	ActionNote string
	// Alt is the second button beside Action, when the screen has two honest
	// answers rather than one. Only the reuse screen does: "open the one you
	// have" and "build another anyway". Nil renders nothing, so every other
	// screen stays exactly the one-button page it was.
	Alt *altAction
	// Stale is the sentence about an existing sandbox whose disk predates its
	// environment's current build. Empty on every other screen, and empty on
	// this one too whenever the two cannot be compared — see staleNote, which
	// says nothing rather than guessing.
	Stale string
	// Home is the user console, when this host has one configured. Empty
	// renders no link rather than a guessed hostname.
	Home string
	// Others is the escape hatch: the caller's other sandboxes on this
	// repository, each a link straight to its own terminal.
	Others []otherBox
	Foot   string
}

// altAction is the secondary button: its own form, its own action, its own
// label. It carries no fields, exactly like the primary one — everything the
// POST acts on is re-derived from the path, the query and the session — so the
// difference between the two buttons is entirely in the URL they post to.
type altAction struct {
	Action string
	Submit string
}

// fact is one row of the "here is what this will do" block.
type fact struct {
	Label string
	Value string
	// Sub is the second, quieter line under Value — the "why" for a row whose
	// value alone would raise a question.
	Sub string
	// Mono renders Value in the monospace face: for a slug, a branch or a name,
	// where the difference between an l and a 1 is the difference between two
	// repositories.
	Mono bool
	// Pills renders a set instead of Value, one rounded chip each. Tags are a
	// set and reading them as a comma-joined sentence invites the reader to
	// think the order or the separator means something.
	Pills []string
}

// step is one numbered instruction, with an optional bold opener.
type step struct {
	Lead string
	Body string
}

// otherBox is one of the caller's existing sandboxes as the page shows it.
//
// Name, branch, state and its own terminal URL, and nothing else: not the node
// it lives on, not its size, not its tags. This list exists to let a human land
// on a sandbox the ref rule could not decide for them, and every additional
// field is one more thing about the visitor's account rendered onto a page they
// reached by clicking a link somebody else wrote.
type otherBox struct {
	Name   string
	Branch string
	State  string
	// StateClass is the CSS class carrying the state dot's colour. It is
	// computed here from a fixed set rather than interpolated from Info.State,
	// so an unrecognised state from a future vmm renders as a plain pill and
	// never as a class name nobody wrote a rule for.
	StateClass string
	URL        string
}

// foot is the last line of every screen: what this link could and could not do.
//
// It is on every screen, including the refusals, because the question it
// answers — "I clicked something a stranger posted; what did I just agree to?"
// — is loudest on the screens where something went wrong.
const foot = "A launch link carries no authority of its own. It signs you in as yourself, and anything it builds is yours."

// render writes one screen.
//
// The template is executed into a buffer first and only then written out. A
// template that fails halfway — a nil map, a method that errors — has already
// written a partial page and a 200 status by the time Execute returns the
// error, and the visitor gets half a screen with no indication anything is
// wrong. Buffering costs a few kilobytes on a page nobody loads in a loop and
// makes the failure a 500 that says so.
func (h *Handler) render(w http.ResponseWriter, status int, data pageData) {
	var buf bytes.Buffer
	if err := pageTpl.Execute(&buf, data); err != nil {
		h.log.Error("launch page render failed", "err", err, "heading", data.Heading)
		http.Error(w, "sparkbox: the launch page could not be rendered", http.StatusInternalServerError)
		return
	}

	head := w.Header()
	head.Set("Content-Type", "text/html; charset=utf-8")
	// edgeauth.Require already stamps this on every response it allows, but the
	// not-found screen is mounted outside the gate and this page names the
	// visitor's own repositories and sandboxes either way. It is not a thing a
	// shared cache may keep.
	head.Set("Cache-Control", "no-store")
	// default-src 'none', with three named exceptions and no fourth: style-src
	// for the inline <style> the design system is composed into, img-src for
	// this origin and the data: favicon, and script-src naming the sha256 of
	// progress.js — the one script on the page — and nothing else. No
	// 'unsafe-inline', no nonce machinery, no host: a script the browser is
	// willing to run here is a script whose bytes hash to a constant compiled
	// into this binary. base-uri 'none' stops an injected <base> from re-aiming
	// the form's relative action, and form-action is composed by pageCSP — it
	// cannot be a bare 'self', for a reason worth reading there before
	// tightening it.
	//
	// The create is still a plain form POST that the server re-validates from
	// the path, the query and the session; progress.js only paints the wait.
	// Turning it off leaves the page working, which is the property that lets
	// the policy stay this narrow.
	head.Set("Content-Security-Policy", h.csp)
	// The handoff after a create is a top-level navigation to the terminal,
	// which sets frame-ancestors 'none' itself. This page refuses framing for
	// the same reason: it carries a single-click create, and a click on a
	// button the visitor cannot see is the entire clickjacking family.
	head.Set("X-Frame-Options", "DENY")
	// The URL of this page names a repository, which on a private one is
	// information, so nothing off this origin is told where the visitor came
	// from. "same-origin" and NOT "no-referrer", and the difference is the
	// whole create path rather than a shade of privacy.
	//
	// Under "no-referrer" the Fetch standard's "append a request Origin header"
	// step serializes the origin of a non-CORS, non-GET request as the literal
	// `null` — and that includes this page's own same-origin form POST. firstParty
	// (csrf.go) would then see `Origin: null`, which is not the configured origin,
	// not requestOrigin(r), and not a header a bare form can set, so every press of
	// "Create a sandbox" would 403 and the button would never once have worked.
	// The test suite cannot catch it, because a test sets Origin by hand; only a
	// real browser derives it from this header.
	//
	// "same-origin" keeps the full referrer on this origin — where the Origin
	// header is computed normally — and sends nothing at all cross-origin, which
	// is the property the paragraph above is actually asking for. Do not
	// "fix" a future 403 here by teaching firstParty to accept `null`: a
	// sandboxed iframe and some redirect chains send that too, so it would widen
	// the gate rather than restore it.
	head.Set("Referrer-Policy", "same-origin")
	head.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	w.Write(buf.Bytes()) //nolint:errcheck // the client hung up; there is nothing to do about it and nothing to log
}

// pageCSP composes the policy every screen carries. It is computed once in New
// and stored on the handler, so the test that pins it compares against the same
// string the handler writes rather than a second copy that could drift into
// agreement with a weakened header.
//
// Everything is 'none' except what this page provably needs: an inline <style>
// block (the design system arrives through webui.Compose, not a stylesheet
// request), images from this origin or a data: URI, and exactly one script,
// admitted by the hash of its bytes.
//
// That script-src clause is the narrowest form that admits anything at all. It
// names no host, so nothing can be fetched; it carries no 'unsafe-inline', so
// an injected <script> — or an onclick, or a javascript: URL — is refused even
// though it sits in the same document as the one we wrote; and the digest is
// computed at package init from the same bytes the template embeds
// (assets.go), so the two cannot drift apart without the browser noticing
// first and refusing to run either.
//
// form-action is the one directive that cannot be 'self'. A successful create
// answers 303 to the sandbox's terminal on a DIFFERENT host —
// <name>-<xterm>.<domain> — and an expired session answers 303 to the login
// host. Firefox evaluates form-action against every URL in the redirect chain,
// not just the form's immediate action, so 'self' alone blocks the handoff
// AFTER a real VM has been built and charged against the owner's running limit:
// the visitor is left on a stalled page with a console warning and a sandbox
// they were never shown. Chrome and Safari do not follow redirects for this
// directive, which is exactly why the bug would survive every browser most
// people would test in.
//
// The zone wildcard is the narrowest form that covers both targets without
// naming a per-sandbox host that changes on every create, and it is derived
// from the configured domain rather than a literal so a relabelled deployment
// cannot end up with a policy for somebody else's zone. A host with no
// configured domain has no wildcard to grant and keeps 'self'.
func pageCSP(domain string) string {
	formAction := "'self'"
	if domain != "" {
		formAction += " https://*." + domain
	}
	return "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; " +
		"script-src " + progressJSHash + "; " +
		"form-action " + formAction + "; base-uri 'none'; frame-ancestors 'none'"
}

// get answers GET /{owner}/{repo}: resolve, and either hand the visitor back a
// sandbox they already have or show them what building one would do.
//
// It writes nothing, anywhere, on any branch. That is the contract a link in a
// public comment needs, because such a link is fetched by scanners, previewers
// and prefetchers nobody consented to — see TestGetIsSideEffectFree, which
// drives every GET route against a fake whose Create fails the test.
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	// Verbatim from internal/restapi/server.go:389. The handle comes from the
	// verified session and never from the URL: the whole request line is
	// attacker-supplied text out of a public comment, and KeyFP stays empty
	// because there is no SSH key behind an HTTP request.
	sess, _ := edgeauth.From(r.Context())
	c := ctlops.Caller{Handle: sess.Handle}

	t, bad := parseTarget(r.PathValue("owner"), r.PathValue("repo"),
		r.URL.Query().Get("ref"), r.URL.Query().Get("env"), r.URL.Query().Get(freshParam))
	if bad != nil {
		h.refuse(w, r, t, bad)
		return
	}
	h.noteUnknownParams(r)

	att, best, others, err := h.resolve(r.Context(), c, t)
	if err != nil {
		h.refuse(w, r, t, ctlops.AsError(op, err))
		return
	}
	// `?new=1` demotes the match instead of dropping it. The visitor asked to
	// build a second sandbox, so `best` must not be the thing the button opens
	// — but it is still one of their boxes on this repository, and the one they
	// were most likely looking at when they pressed the link, so it belongs at
	// the head of the list below rather than nowhere. rank already put it
	// first, which is why prepending preserves the order.
	if t.Fresh && best != nil {
		others = append([]candidate{*best}, others...)
		best = nil
	}
	if best != nil && t.Env == "" {
		h.handoff(w, r, t, best.Info)
		return
	}
	h.render(w, http.StatusOK, h.confirm(c, t, att, best, others, refererIsOwnDomain(r, h.domain)))
}

// refererIsOwnDomain reports whether r arrived with a Referer naming this
// deployment's own zone or one of its subdomains — the signal that the
// visitor followed a link inside the product, such as an environment page's
// Launch button, rather than one posted somewhere else.
//
// An absent or unparsable Referer answers false, same as a foreign one: a
// stripped Referer is indistinguishable from "came from outside" and gets the
// same caution a stranger's link would.
func refererIsOwnDomain(r *http.Request, domain string) bool {
	if domain == "" {
		return false
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		return false
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// home answers GET / on this door: a bare visit to go.<domain>, with no
// repository named.
//
// It happens when somebody trims a launch link back to its hostname to find out
// what it is, which is a reasonable thing to do with a URL a stranger posted.
// When this host has a user console configured it is the better answer and we
// send them there; otherwise the page explains the link grammar rather than
// 404ing somebody who was trying to understand what they clicked.
func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	if h.homeURL != "" {
		http.Redirect(w, r, h.homeURL, http.StatusSeeOther)
		return
	}
	h.render(w, http.StatusOK, pageData{
		Title:   "Launch links · sparkbox",
		Heading: "Launch links",
		Lead: "This door turns a link in a pull-request comment into a sandbox of your own, " +
			"with the repository already checked out. A link names a repository and may select a branch or one of your environments.",
		Command: h.origin + "/<owner>/<repo>?ref=<branch>&env=<environment>",
		Steps: []step{
			{Lead: "Nothing in the link names you.", Body: "Whoever clicks it signs in as themselves and gets their own sandbox, built from their own attachments and their own secrets."},
			{Lead: "The repository has to be attached to your account first.", Body: "A sandbox clones what you attached, not what the link's author attached."},
			{Lead: "Click the same link again later.", Body: "It finds the sandbox it made for you the first time instead of building a second one."},
		},
		Home: h.homeURL,
		Foot: foot,
	})
}

// notFound answers every other path under this door.
//
// It is text/plain and mounted OUTSIDE the auth gate on purpose. A path that is
// not a repository is not a thing to sign anybody in for, and bouncing an
// unknown URL through a login page — so the visitor authenticates, comes back,
// and is then told the page does not exist — spends somebody's credentials on a
// typo.
func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, "sparkbox: a launch link is %s/<owner>/<repo> — %q is not one\n", h.origin, r.URL.Path)
}

// handoff is the fast path, and the one that happens fifty times a day: the
// visitor already has this sandbox, so no page paints at all.
//
// The destination is SandboxInfo.TerminalURL verbatim. It is composed by
// ctlops.Ops.info from the host's configured --xterm-subdomain, and rebuilding
// it here from the literal "xterm" would hand out a URL that 404s on any host
// that relabelled it — the same discipline internal/restapi keeps by threading
// the label through rather than writing it down twice.
//
// 303 and never 301: a permanent redirect to one sandbox would be cached by the
// browser and would survive that sandbox's destruction, and there is no way to
// take it back. 303 also states the method change explicitly, which matters
// because the POST path lands here too.
func (h *Handler) handoff(w http.ResponseWriter, r *http.Request, t target, info ctlops.SandboxInfo) {
	if info.TerminalURL == "" {
		// A sandbox with no terminal URL means this host has no browser
		// terminals — Ops.info leaves it empty unless both the zone and the
		// xterm label are configured. Redirecting to "" would send the browser
		// back to this same URL, which on the POST path is an infinite create
		// loop. Say what is actually wrong instead.
		h.refuse(w, r, t, ctlops.Disabled(op,
			"this host has no browser terminals, so there is nowhere for a launch link to land."))
		return
	}
	http.Redirect(w, r, info.TerminalURL, http.StatusSeeOther)
}

// confirm builds the screen that offers to create.
//
// Everything on it is a fact about what the create will do, taken from the
// attachment and never from the URL: the repository under the casing its owner
// attached it with, the branch, the exact tag set the sandbox will carry, and
// the other repositories those tags will drag along with it. The last one is
// the surprise this page exists to remove — `default` is stamped on every
// create, so a visitor who has three repositories on `default` gets three
// checkouts from a link that named one.
func (h *Handler) confirm(c ctlops.Caller, t target, att repos.Repo, best *candidate, others []candidate, trustedReferrer bool) pageData {
	choice := h.launchTags(c, att)
	if t.Env != "" {
		choice = tagChoice{Tags: []string{t.Env}, Chosen: t.Env}
	}
	tags := createTags(choice.Tags)
	facts := []fact{{
		Label: "Repository",
		Value: att.Slug,
		Mono:  true,
		// The stored casing, never the URL's. A link written as WANDB/Hivemind
		// matches the attachment (the store's slug columns are COLLATE NOCASE)
		// and must still show the repository the way its owner attached it.
		Sub: "cloned into the sandbox with a GitHub token minted for it",
	}}
	if t.Env != "" {
		facts = append(facts, fact{Label: "Environment", Value: t.Env, Mono: true,
			Sub: "selects this environment's disk, repositories, secrets, egress, and variables"})
	}
	if want := normalizeRef(att.Ref, t.Ref); want != "" {
		facts = append(facts, fact{Label: "Branch", Value: want, Mono: true})
	} else if att.Ref != "" {
		facts = append(facts, fact{Label: "Branch", Value: att.Ref, Mono: true,
			Sub: "the branch your attachment already points at"})
	} else {
		facts = append(facts, fact{Label: "Branch", Value: "its default branch"})
	}
	tagSub := "tags decide which of your secrets are pushed into the sandbox"
	// A dropped environment is the one thing on this page the visitor could not
	// work out from the pills, because it is named by its ABSENCE. They are
	// about to land in `web` when this repository is also in `ci`, and if that
	// is not what they wanted, the Others list below is how they get there.
	if len(choice.Dropped) > 0 {
		tagSub = "this repository is in more than one environment, so it opens in " + choice.Chosen +
			" — the one you changed most recently. Left off: " + strings.Join(choice.Dropped, ", ")
	}
	facts = append(facts, fact{
		Label: "Tags", Pills: tags,
		Sub: tagSub,
	})
	if also := h.alsoClones(c, att, tags); len(also) > 0 {
		facts = append(facts, fact{
			Label: "Also clones", Pills: also,
			Sub: "your other repositories on those tags come along",
		})
	}

	data := pageData{
		Title:       "Open " + att.Slug + " · sparkbox",
		Heading:     "Create a sandbox for",
		HeadingSlug: att.Slug,
		Lead: "You have no sandbox holding this repository on this branch yet. " +
			"Building one takes a few seconds, and it is yours — clicking this link again lands you straight back in it.",
		Facts:  facts,
		Action: formAction(t),
		Submit: "Create a sandbox",
		Home:   h.homeURL,
		Others: h.otherBoxes(others),
		Foot:   foot,
	}
	if !trustedReferrer {
		// A visitor who navigated here from inside the product — the
		// environment page's own Launch button, say — already trusts the
		// branch it names; the caution is for whoever followed a link a
		// stranger posted, which is everyone else, including a bare paste
		// with no Referer at all.
		data.Caution = "Whoever posted this link chose the branch. Its code, and anything an agent reads there, " +
			"runs in your sandbox with a GitHub token minted for this repository. Only click through for a branch you would run."
	}
	if t.Fresh {
		// The visitor arrived here by pressing "create a new one" on the screen
		// below, so the lead must not go on claiming they have nothing — they
		// can see the box they already have, listed underneath.
		data.Lead = "This builds a second sandbox on this repository, alongside the ones below. " +
			"It is a separate box with its own disk; nothing about the ones you already have changes."
	}
	if best != nil {
		data.Heading = "Open a sandbox for"
		data.Lead = "This environment already has a matching sandbox. Confirm the environment selection, then open it."
		data.Submit = "Open " + best.Info.Name
		// The second door, and it is a form rather than a link because it
		// creates: a GET on this URL paints the confirm screen again, and
		// nothing on this page may build a VM without a press.
		data.Alt = &altAction{
			Action: formAction(t.fresh()),
			Submit: "Create a new one",
		}
		data.Stale = h.staleNote(c, t, *best)
	}
	if len(data.Others) > 0 {
		// Only when there is in fact something below to point at. A note
		// promising an alternative that is not on the page reads as a rendering
		// bug, and this screen's whole job is to be trustworthy to somebody who
		// has never seen the product.
		data.ActionNote = "or open one you already have, below"
	}
	return data
}

// staleNote is the warning that the sandbox this screen offers to open was
// forked from an older disk than the environment now has.
//
// THE COMPARISON IS TIMES, NOT IMAGES, and that is deliberate rather than a
// shortcut. A sandbox's rootfs is fixed at create and never re-forked, so a box
// made before its environment's current build CANNOT be running that build's
// disk — the two facts needed to say so are already on the payloads this door
// holds (SandboxInfo.CreatedAt) or can read (EnvironmentInfo.BuiltAt), and the
// alternative would mean putting the template name on the public sandbox
// payload, which internal/ctlops.SandboxInfo drops on purpose along with every
// other piece of internal topology.
//
// It says NOTHING wherever the comparison cannot be made honestly: a host with
// no environment store, a link with no ?env=, an environment that has never
// been built, a store that would not answer. The one thing this must never do
// is warn on a box that is perfectly current — this screen is the first thing a
// stranger sees of the product, and a scary sentence that is usually wrong is
// worse than no sentence at all.
//
// The store is read a second time here (resolve already fetched this
// environment to check readiness) rather than threaded through resolve's return
// values. It is one sqlite row on a path that is already rendering a page, and
// it costs nothing on the fast path that matters — the handoff redirect never
// reaches this function.
func (h *Handler) staleNote(c ctlops.Caller, t target, best candidate) string {
	if h.envs == nil || t.Env == "" {
		return ""
	}
	env, err := h.envs.GetEnvironment(c, t.Env)
	if err != nil || env.BuiltAt == nil {
		return ""
	}
	if !best.Info.CreatedAt.Before(*env.BuiltAt) {
		return ""
	}
	return "Heads up: " + best.Info.Name + " was created before " + env.Name +
		" was last built, so it is running the older disk — whatever that build " +
		"installed is not in it. Creating a new one gets you the current image."
}

// refuse renders a *ctlops.Error as a screen rather than a status line.
//
// Two of these are not really errors from the visitor's side and get taught,
// not reported: "nothing of yours is attached to this repository", which is the
// most-visited state this door will ever have because it is what a first-time
// arrival from a public comment hits, and "you are at this host's running
// limit", which is the ordinary failure of a click-to-create button on a host
// whose --max-running-per-owner default is two. Everything else gets one
// sentence and the status its Kind implies.
func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, t target, e *ctlops.Error) {
	// The slug as the URL spelled it, because on these screens there may be no
	// attachment to take the stored casing from. It is escaped by the template
	// like everything else.
	slug := t.Slug
	if slug == "" {
		slug = strings.Trim(r.PathValue("owner")+"/"+r.PathValue("repo"), "/")
	}

	switch {
	case e.Code == codeNoAttachment:
		h.render(w, e.HTTPStatus(), h.attachScreen(slug))
	case e.Code == codeUntaggedAttachment:
		h.render(w, e.HTTPStatus(), h.untaggedScreen(slug))
	case e.Kind == ctlops.KindLimit:
		h.render(w, e.HTTPStatus(), h.limitScreen(t, e))
	default:
		if e.Kind == ctlops.KindInternal {
			// The sentence a KindInternal carries is a store or driver message.
			// It goes to the log, where an operator reads it; the page gets the
			// generic one, because this surface faces strangers.
			h.log.Error("launch failed", "op", e.Op, "code", e.Code, "slug", slug, "err", e.Err)
		}
		h.render(w, e.HTTPStatus(), h.problemScreen(e))
	}
}

// attachScreen is the front door's front door.
//
// It is reached by somebody who has an account, clicked a button in a public
// comment, and has never attached that repository — which is the single most
// likely thing to happen to this feature. So it teaches rather than refuses:
// what an attachment is, why theirs is the only one that counts, and the one
// command that fixes it.
//
// It carries NO create button, deliberately. A sandbox built with no attachment
// selecting this repository would come up with an empty working tree, which
// looks like the feature is broken rather than like a step was missed. And we
// cannot attach it for them: ctlops' AttachGate refuses any account whose
// GitHub link is a weak `assertion`, which is exactly the account a first-time
// visitor has.
func (h *Handler) attachScreen(slug string) pageData {
	return pageData{
		Title:       "Attach " + slug + " · sparkbox",
		Heading:     "Nothing of yours is attached to",
		HeadingSlug: slug,
		Lead: "A sandbox clones the repositories YOU have attached to your sparkbox account — not the ones " +
			"whoever posted this link attached. So there is nothing here for a sandbox to check out yet. " +
			"Attach it once, from a terminal that holds your SSH key:",
		Command: fmt.Sprintf("ssh %s@%s repo add %s --tag <t>", controlUser, h.sshHost(), slug),
		Steps: []step{
			{Lead: "Pick a tag.", Body: "A tag groups a repository with the secrets that belong to it, and every " +
				"sandbox carrying that tag gets both. Any word will do; " +
				"`repo ls` on the same connection shows the ones you already use."},
			{Lead: "Then click the link again.", Body: "It will offer to build the sandbox, and it will keep " +
				"finding that same sandbox every time afterwards."},
		},
		Home: h.homeURL,
		Foot: foot,
	}
}

// untaggedScreen is shown when the repository is attached and carries no tag.
//
// It is a separate screen from attachScreen, and the distinction is the whole
// reason it exists: the visitor did the thing the other screen asks for, and
// telling them to attach a repository they have already attached would read as
// the door being broken. What is missing is the tag — the only thing that
// connects an attachment to a sandbox — so that is what this page asks for, and
// it deliberately carries no create button, because a create here provably
// cannot clone the repository the link names.
func (h *Handler) untaggedScreen(slug string) pageData {
	return pageData{
		Title:       "Tag " + slug + " · sparkbox",
		Heading:     "No tag connects you to",
		HeadingSlug: slug,
		Lead: "You have attached this repository, but the attachment carries no tag — and a tag is the only " +
			"thing that puts a repository into a sandbox. As it stands, a sandbox built from this link would " +
			"come up with nothing checked out. Give the attachment a tag and the link starts working:",
		Command: fmt.Sprintf("ssh %s@%s repo add %s --tag <t>", controlUser, h.sshHost(), slug),
		Steps: []step{
			{Lead: "Re-running `repo add` is the way to set tags.", Body: "It replaces the attachment's tag set " +
				"rather than adding a second attachment, so this is an edit and not a duplicate."},
			{Lead: "`--tag default` makes it universal.", Body: "Every sandbox carries `default`, so an " +
				"attachment on it is cloned into all of them — convenient, and a checkout in every box you " +
				"ever build. A narrower word is usually the one you want."},
		},
		Home: h.homeURL,
		Foot: foot,
	}
}

// limitScreen is the one failure a click-to-create button hits routinely.
//
// --max-running-per-owner defaults to 2 (cmd/sparkbox/main.go:125), enforced by
// host.Manager.admitCost, which is a *host.LimitError, which ctlops classifies
// as KindLimit and answers 429 with. A visitor who has two sandboxes running
// and clicks a third badge sees this — not as a stack trace and not as a bare
// "429 Too Many Requests", but as the sentence naming which of THEIR sandboxes
// is in the way and the command that frees a slot.
//
// The names come from the error's own Details["running"], which
// ctlops.AsError fills from the manager's LimitError. They are the caller's own
// sandbox names, resolved under the caller's handle, so naming them here
// discloses nothing they cannot already list.
func (h *Handler) limitScreen(t target, e *ctlops.Error) pageData {
	// A concrete name in the command beats a placeholder: the visitor can
	// select the line and run it. Falling back to <name> keeps the screen
	// correct if a future LimitError stops carrying the list.
	victim := "<name>"
	if running, ok := e.Details["running"].([]string); ok && len(running) > 0 {
		victim = running[0]
	}
	return pageData{
		Title:   "Sandbox limit reached · sparkbox",
		Heading: "You are already running as many sandboxes as this host allows",
		Lead: e.Msg + ". A paused sandbox keeps its disk and everything on it — pausing is not losing anything, " +
			"and the sandbox comes back warm.",
		Command: fmt.Sprintf("ssh %s@%s pause %s", controlUser, h.sshHost(), victim),
		Steps: []step{
			{Lead: "Pause one you are not using.", Body: "The command above, or the pause button in your console."},
			{Lead: "Then press the button again.", Body: "Nothing was created, so there is nothing to clean up."},
		},
		Action:     formAction(t),
		Submit:     "Try again",
		Home:       h.homeURL,
		ActionNote: "after you have freed a slot",
		Foot:       foot,
	}
}

// problemScreen is everything else: a malformed link, a disk pool that is full,
// a host at capacity, a store that failed.
//
// The sentence is the error's own Msg, which every ctlops constructor
// guarantees is already a complete, user-facing sentence with no "sparkbox: "
// prefix — except for KindInternal, whose Msg is a store or driver string that
// refuse() has already diverted to the log. Hint is rendered when the error
// carries one, because the errors that carry a Hint are exactly the ones with a
// remedy the reader can act on.
func (h *Handler) problemScreen(e *ctlops.Error) pageData {
	lead := e.Msg
	if e.Kind == ctlops.KindInternal {
		lead = "Something on this host failed while working out what to do with that link. " +
			"It has been logged; trying again in a moment is reasonable."
	}
	data := pageData{
		Title:   "That link did not work · sparkbox",
		Heading: "That link did not work",
		Lead:    lead,
		Home:    h.homeURL,
		Foot:    foot,
	}
	if e.Hint != "" {
		data.Steps = []step{{Body: e.Hint}}
	}
	// A malformed link is the one case where showing the grammar helps: the
	// reader is looking at a URL somebody typed by hand into a comment, and the
	// shape of the correct one is the whole answer.
	if e.Kind == ctlops.KindInvalid {
		data.Command = h.origin + "/<owner>/<repo>?ref=<branch>"
	}
	return data
}

// otherBoxes flattens the resolve's near-misses into the escape-hatch list.
//
// It is the answer to the one branch case no ref rule can decide: an attachment
// with no default branch recorded, and a link that spells the default branch
// out loud. Nothing in this tree knows what a repository's default branch is
// called, so the page cannot fold those two together — but a human looking at
// "crafty-axolotl · main · running" can, in one click.
func (h *Handler) otherBoxes(in []candidate) []otherBox {
	out := make([]otherBox, 0, len(in))
	for _, cand := range in {
		if cand.Info.TerminalURL == "" {
			// Nothing to link to. A row that looked like a link and went
			// nowhere would be worse than an absent row.
			continue
		}
		branch := cand.Ref
		if branch == "" {
			branch = "default branch"
		}
		state, class := cand.Info.State, stateClass(cand.Info.State)
		if cand.Info.Unreachable {
			// About the MACHINE, not the box: it is almost certainly still
			// running and we simply cannot reach the host that holds it. The
			// user console draws the same distinction with the same word.
			state, class = "offline", "offline"
		}
		out = append(out, otherBox{
			Name: cand.Info.Name, Branch: branch,
			State: state, StateClass: class, URL: cand.Info.TerminalURL,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// stateClass maps a sandbox state to the dot colour the consoles already use
// for it.
//
// The states are quoted from internal/vmm rather than written out, for the same
// reason stateRank quotes them: a renamed state becomes a compile error here
// instead of a pill with no colour. An unrecognised one gets no class at all,
// which renders as a plain bordered pill — honest, and impossible to confuse
// with a state the page does understand.
func stateClass(state string) string {
	switch state {
	case string(vmm.StateRunning):
		return "running"
	case string(vmm.StatePaused):
		return "paused"
	case string(vmm.StateArchived):
		return "archived"
	default:
		return ""
	}
}

// createTags is the exact tag set the sandbox this page offers would carry.
//
// It is att.Tags plus secrets.DefaultTag, because ctlops.Ops.defaultTags stamps
// `default` on EVERY create (internal/ctlops/sandbox.go:238-248) — so a page
// that showed only the attachment's own tags would understate what the sandbox
// gets by exactly the tag most people keep their shared secrets on. Sorted, and
// deduplicated by the Contains check, so the page reads the same as `ls` will.
//
// It takes the CHOSEN tags rather than the attachment. launchTags may drop an
// ambiguous environment, while an explicit owner-checked environment replaces
// the ordinary choice with its single tag. Nothing accepts a raw tag from the
// URL. The page must list exactly what Create will carry.
func createTags(chosen []string) []string {
	out := slices.Clone(chosen)
	if !slices.Contains(out, secrets.DefaultTag) {
		out = append(out, secrets.DefaultTag)
	}
	slices.Sort(out)
	return out
}

// alsoClones lists the caller's OTHER repositories that the create's tags would
// drag into the same sandbox.
//
// This is the surprise the confirm page exists to remove. A create carries
// `default` whether or not the attachment does, and every attachment on a
// shared tag is cloned into every sandbox carrying it — so a link naming one
// repository can produce a sandbox holding four, three of which the visitor did
// not think they were agreeing to. Naming them before the button is pressed is
// the difference between a feature and a surprise.
//
// It costs one store read beyond the resolve's three, and it is taken ONLY on
// this page — never on the 303 fast path, which is the click that happens fifty
// times a day. A failure is logged and swallowed: the inventory is advisory,
// and refusing to render a create screen because a second read hiccuped would
// turn an informational row into an outage.
func (h *Handler) alsoClones(c ctlops.Caller, att repos.Repo, tags []string) []string {
	list, err := h.repos.ListRepos(c.Handle)
	if err != nil {
		h.log.Warn("launch could not list the tag's other repositories", "user", c.Handle, "err", err)
		return nil
	}
	var out []string
	for _, r := range list {
		if strings.EqualFold(r.Slug, att.Slug) {
			continue
		}
		for _, tag := range r.Tags {
			if slices.Contains(tags, tag) {
				out = append(out, r.Slug)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// formAction is the URL the confirm page's form posts to.
//
// It is a path and a query and never an absolute URL, which is what lets the
// Content-Security-Policy say `form-action 'self'` and mean it. The values are
// re-escaped from the parsed target rather than echoed from r.URL, so the bytes
// the form carries are ones parseTarget already validated — the server then
// re-parses them on the POST anyway, because a form is not a promise.
//
// The slug is written literally: both halves were validated against the
// store's own grammar, so neither can contain a '/', a '?' or a '#', and
// percent-encoding a path segment here would arrive back as a segment no route
// matches. url.Values escapes a ref like `feat/x` as `feat%2Fx` — deliberately
// different from the literal `?ref=feat/x` the badge
// markdown emits. That form is written for a human retyping it into a comment,
// where a %2F is one more thing to get wrong; this one is written by a program
// and read by a browser, where the fully-escaped form is the one that cannot be
// wrong. Both decode to the same ref, which is what r.URL.Query() gives the
// POST handler back.
func formAction(t target) string {
	action := "/" + t.Slug
	query := url.Values{}
	if t.Ref != "" {
		query.Set("ref", t.Ref)
	}
	if t.Env != "" {
		query.Set("env", t.Env)
	}
	if t.Fresh {
		query.Set(freshParam, "1")
	}
	if encoded := query.Encode(); encoded != "" {
		action += "?" + encoded
	}
	return action
}

// sshHost is the hostname the `ssh ctl@…` lines on these pages name.
//
// It is the configured zone, never a literal. A host that is not configured
// with one has nothing honest to print, and printing a placeholder inside a
// command somebody will paste into a terminal is worse than printing the
// placeholder-shaped thing it is — so <domain> is what they get, and it will
// not silently resolve to somebody else's machine.
func (h *Handler) sshHost() string {
	if h.domain == "" {
		return "<domain>"
	}
	return h.domain
}

// noteUnknownParams logs, at debug, any query parameter this door does not
// know.
//
// It never refuses one. That is a forward-compatibility promise with teeth: the
// URL goes into comments that outlive the deployment they were written for, so
// a link written today must not 400 on a host that learned a parameter after it
// was written, and a link a future host writes must still work here. The log
// line exists so that "somebody is passing us tag= from an old build" is a
// question an operator can answer, rather than something that vanishes.
func (h *Handler) noteUnknownParams(r *http.Request) {
	var unknown []string
	for key := range r.URL.Query() {
		if key != "ref" && key != "env" && key != freshParam {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		h.log.Debug("launch link carried parameters this build does not know", "params", unknown, "path", r.URL.Path)
	}
}
