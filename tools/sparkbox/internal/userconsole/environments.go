package userconsole

// The Environments panel: the fifth view of the one object the other four
// panels each show a slice of.
//
// Secrets, Network and Repos are all "things attached to a tag", and this panel
// is the tag itself — named, described, with the setup script that turns a bare
// checkout into a working project and the snapshot that has already run it. It
// is the panel that makes the other three read as parts of something rather
// than as three unrelated lists that happen to share a column.
//
// EVERY WRITE GOES THROUGH ctlops, and that is the one structural difference
// between this file and its neighbours. listNetRules and putRepo talk to their
// stores directly and do their own owner scoping, which is affordable because
// each is one row in one table. An environment is not: creating one writes the
// environment row, retags secrets, attaches repositories through the same
// GitHub-identity gate `repo add` applies, unions rule-set tags and may create
// an egress rule-set — five stores with an ordering rule ("every refusal comes
// before the first write") that exists precisely because nothing spans them in
// a transaction. Re-implementing that here would be a second authorization path
// and a second orchestration of the same five stores, which is the thing
// TemplateTags' comment says this package must not grow. So the seam is the
// interface below, `*ctlops.Ops` satisfies it, and the owner scoping is the
// Caller — the same one the SSH door and the REST API pass.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
)

// Environments is the control plane as this panel needs it. Optional: a nil one
// answers 501 from every route, which is a host with no environment store —
// the tab still renders and says so, exactly as the Network and Repos tabs do.
type Environments interface {
	ListEnvironments(c ctlops.Caller) ([]ctlops.EnvironmentInfo, error)
	GetEnvironment(c ctlops.Caller, name string) (ctlops.EnvironmentInfo, error)
	PutEnvironment(ctx context.Context, c ctlops.Caller, a ctlops.EnvArgs) (ctlops.EnvironmentInfo, error)
	DeleteEnvironment(ctx context.Context, c ctlops.Caller, name string) (ctlops.EnvDeleteResult, error)
	UnsetEnvVar(ctx context.Context, c ctlops.Caller, env, name string) error
	EnvScript(c ctlops.Caller, name string) (script, from string, err error)
	SetEnvScript(c ctlops.Caller, name, script, from string) error
	AdoptRepoScript(ctx context.Context, c ctlops.Caller, name string) (ctlops.EnvironmentInfo, error)
	BuildEnvironment(ctx context.Context, c ctlops.Caller, name string) (ctlops.EnvironmentInfo, error)
	CaptureEnvironment(ctx context.Context, c ctlops.Caller, name string) (ctlops.EnvironmentInfo, error)
}

// SetEnvironments enables the Environments panel. Like SetTemplateTags and
// SetGitHubApp it is a setter rather than a New parameter, because the control
// plane is built after the console's stores and passing it in would reorder
// main.go for nothing.
func (h *Handler) SetEnvironments(e Environments) { h.envs = e }

// envCaller is caller(r) in this package: the session's handle and nothing
// else, so ctlops applies exactly the scoping it applies to `ctl env`.
//
// No KeyFP, and that is not an omission. KeyFP names the SSH key a request
// arrived on; this one arrived on an edge session, and inventing a fingerprint
// for it would put a fiction in an audit line.
func envCaller(r *http.Request) ctlops.Caller {
	return ctlops.Caller{Handle: handleFrom(r)}
}

// envRequest is the panel's PUT body: create-and-update in one, like the verb
// underneath it.
//
// The four attachment lists ADD, because that is what the tag primitive does
// and what `ctl env set` does — a secret can belong to three environments at
// once, and a console that silently detached the ones it did not list would be
// deleting other environments' composition from a form. Detaching stays where
// it already is: the Secrets, Network and Repos panels, each of which edits its
// own object's tags.
//
// Vars are the exception, and they are a pointer for the reason Description is.
// A var IS keyed by (owner, tag, name), so there is no other owner to union
// with and a form that shows four variables and saves three means "delete the
// fourth". Absent (nil), they are left alone; present, the list is the whole
// set and anything missing from it is unset.
type envRequest struct {
	Description *string   `json:"description,omitempty"`
	Repos       []envRepo `json:"repos,omitempty"`
	Secrets     []string  `json:"secrets,omitempty"`
	Rules       []string  `json:"rules,omitempty"`
	Vars        *[]envVar `json:"vars,omitempty"`
	OpenEgress  bool      `json:"open_egress,omitempty"`
	// Adopt agrees to create an environment over a tag that is already in use.
	// The panel never sends it on the first attempt: it sends the form, gets a
	// 409 naming what the tag carries, asks, and sends the same body again with
	// this set.
	Adopt bool `json:"adopt,omitempty"`
}

// envRepo is one attachment, with the same per-repo passthrough `repo add`
// takes. Slug alone is the common case and everything else is optional.
type envRepo struct {
	Slug  string `json:"slug"`
	Ref   string `json:"ref,omitempty"`
	Path  string `json:"path,omitempty"`
	Write bool   `json:"write,omitempty"`
}

type envVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// envScriptRequest carries a setup script from the editor. `from` is recorded
// as `manual` and is not a field: a script somebody typed into this box came
// from a person, whatever the row said before.
type envScriptRequest struct {
	Script string `json:"script"`
}

// maxConsoleScript is the panel's own cap on a pasted setup script, and it is
// ctlops' own limit rather than a second opinion — the same rule `env script
// --set` follows over SSH. The decoder below is bounded well above it by the
// shared body limit, so an over-long script is refused with the store's
// sentence instead of arriving silently truncated to a script that runs and
// does the wrong half of the job.
const maxConsoleScript = ctlops.MaxSetupScript

func (h *Handler) listEnvironments(w http.ResponseWriter, r *http.Request) {
	if h.envs == nil {
		writeErr(w, http.StatusNotImplemented, "environments are not enabled on this host")
		return
	}
	list, err := h.envs.ListEnvironments(envCaller(r))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	// make, not nil: an owner with no environments has to serialise as [] for
	// the SPA's Array.isArray guard, which renders null as an empty table with
	// nothing anywhere to explain it.
	if list == nil {
		list = []ctlops.EnvironmentInfo{}
	}
	writeJSON(w, http.StatusOK, list)
}

// putEnvironment creates or updates one environment, then reconciles its
// variables when the caller sent a set.
//
// The two halves are deliberately in this order and deliberately not atomic.
// PutEnvironment is the one that can refuse — a repository this account may not
// attach, a secret that does not exist, a var whose name is reserved — and it
// makes all of those refusals before its own first write. Unsetting runs after
// it, touches only rows this environment owns outright, and its failure is
// reported without pretending the environment was not saved: it was.
func (h *Handler) putEnvironment(w http.ResponseWriter, r *http.Request) {
	if h.envs == nil {
		writeErr(w, http.StatusNotImplemented, "environments are not enabled on this host")
		return
	}
	name := r.PathValue("name")
	var req envRequest
	if !decodeBody(w, r, &req) {
		return
	}
	args := ctlops.EnvArgs{
		Name: name, Description: req.Description,
		Secrets: req.Secrets, Rules: req.Rules, OpenEgress: req.OpenEgress,
		Adopt: req.Adopt,
	}
	for _, rp := range req.Repos {
		args.Repos = append(args.Repos, ctlops.RepoArgs{
			Slug: rp.Slug, Ref: rp.Ref, Path: rp.Path, Write: rp.Write,
		})
	}
	// The vars that are staying, read BEFORE the write so the set that is
	// disappearing can be worked out from what the environment actually had
	// rather than from what the browser last saw. A stale form must not delete
	// a variable somebody added from `ctl` thirty seconds ago... and it still
	// can, if they were looking at the same row — which is why this reconcile
	// only runs when `vars` was sent at all.
	var drop []string
	if req.Vars != nil {
		keep := map[string]bool{}
		for _, v := range *req.Vars {
			keep[v.Name] = true
			args.Vars = append(args.Vars, ctlops.EnvVar{Name: v.Name, Value: v.Value})
		}
		if before, err := h.envs.GetEnvironment(envCaller(r), name); err == nil {
			for _, v := range before.Vars {
				if !keep[v.Name] {
					drop = append(drop, v.Name)
				}
			}
		}
	}

	info, err := h.envs.PutEnvironment(r.Context(), envCaller(r), args)
	if err != nil {
		// The one place this panel needs the machine token rather than the
		// sentence: an `env_tag_in_use` 409 is answered by asking the person and
		// sending the same request again with `adopt`, and the page cannot tell
		// that refusal from any other 409 by reading prose.
		writeOpErr(w, statusFor(err), err)
		return
	}
	for _, n := range drop {
		if err := h.envs.UnsetEnvVar(r.Context(), envCaller(r), name, n); err != nil {
			h.log.Warn("could not unset an environment variable the console dropped",
				"handle", handleFrom(r), "env", name, "var", n, "err", err)
		}
	}
	if len(drop) > 0 {
		// Re-read, because the answer this panel renders has to be the state
		// after the unsets rather than the state between the two halves.
		if after, err := h.envs.GetEnvironment(envCaller(r), name); err == nil {
			info = after
		}
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *Handler) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	if h.envs == nil {
		writeErr(w, http.StatusNotImplemented, "environments are not enabled on this host")
		return
	}
	res, err := h.envs.DeleteEnvironment(r.Context(), envCaller(r), r.PathValue("name"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	// The whole result, not an {"ok":true}: what SURVIVED the delete is the
	// point of the verb. The panel prints it, so nobody has to wonder whether
	// removing an environment took their GITHUB_TOKEN with it.
	writeJSON(w, http.StatusOK, res)
}

// getEnvScript is a route of its own for the reason ctlops.EnvScript is a verb
// of its own: a listing that inlined a page of shell per row would be
// unreadable, and the editor wants only the text.
func (h *Handler) getEnvScript(w http.ResponseWriter, r *http.Request) {
	if h.envs == nil {
		writeErr(w, http.StatusNotImplemented, "environments are not enabled on this host")
		return
	}
	script, from, err := h.envs.EnvScript(envCaller(r), r.PathValue("name"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"script": script, "from": from})
}

func (h *Handler) putEnvScript(w http.ResponseWriter, r *http.Request) {
	if h.envs == nil {
		writeErr(w, http.StatusNotImplemented, "environments are not enabled on this host")
		return
	}
	var req envScriptRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Script) > maxConsoleScript {
		writeErr(w, http.StatusBadRequest, "the setup script is too long")
		return
	}
	// `manual`, always. A script typed into this box came from a person, and
	// recording it as `repo` or `agent` would tell the next build to go looking
	// for a source that did not write it.
	if err := h.envs.SetEnvScript(envCaller(r), r.PathValue("name"), req.Script, envs.SetupFromManual); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// adoptRepoScript replaces this environment's script with the one in an
// attached repository. It is the card's "Use <repo>'s" button, and it is the
// only action in this panel that discards a setup script — which is why the
// page asks before calling it, and why nothing about a build does this by
// itself.
func (h *Handler) adoptRepoScript(w http.ResponseWriter, r *http.Request) {
	if h.envs == nil {
		writeErr(w, http.StatusNotImplemented, "environments are not enabled on this host")
		return
	}
	info, err := h.envs.AdoptRepoScript(r.Context(), envCaller(r), r.PathValue("name"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// buildEnvironment starts a build and RETURNS. It is not a job: the environment
// row is the durable state, `state` moves to `building` immediately, and the
// panel's four-second poll is already the progress bar. A 202 with a Location
// would be a second place to look for an answer the row already holds.
func (h *Handler) buildEnvironment(w http.ResponseWriter, r *http.Request) {
	if h.envs == nil {
		writeErr(w, http.StatusNotImplemented, "environments are not enabled on this host")
		return
	}
	info, err := h.envs.BuildEnvironment(r.Context(), envCaller(r), r.PathValue("name"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// captureEnvironment adopts a failed build's paused builder exactly as it
// stands. Unlike the build above it is SYNCHRONOUS and takes minutes — it packs
// a rootfs — so the panel disables the button and waits, the way the Snapshots
// panel already does for `snapshot create`.
func (h *Handler) captureEnvironment(w http.ResponseWriter, r *http.Request) {
	if h.envs == nil {
		writeErr(w, http.StatusNotImplemented, "environments are not enabled on this host")
		return
	}
	info, err := h.envs.CaptureEnvironment(r.Context(), envCaller(r), r.PathValue("name"))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// decodeBody reads a JSON body and answers the caller itself on a malformed
// one, so every handler above is three lines shorter and they all refuse the
// same way. Unknown fields are rejected: a client sending `descripton` should
// find out now rather than watch its typo silently do nothing.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		msg := "malformed request"
		var typed *json.UnmarshalTypeError
		if errors.As(err, &typed) {
			msg = "malformed request: " + typed.Field + " is the wrong type"
		} else if strings.Contains(err.Error(), "unknown field") {
			msg = "malformed request: " + err.Error()
		}
		writeErr(w, http.StatusBadRequest, msg)
		return false
	}
	return true
}
