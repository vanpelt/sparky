package restapi

// Environments: the named object over the tag primitive. A tag already decides
// which secrets, which checkouts, which egress policy and which base image a
// sandbox gets — four stores joined through one column — and an environment is
// that grouping with a name, a description, plain variables, and the setup
// script that turns a fresh checkout into a working project.
//
// The verb set is smaller than the SSH one by exactly nothing, and larger than
// the repos family by one idea: a BUILD. `POST /v1/environments/{name}/build`
// boots a throwaway sandbox, runs the setup script in it (or asks an agent to
// write one), captures the disk and binds it to the tag — so the next sandbox
// created with that tag starts with the work already done.
//
// Two of those are not ordinary requests and are shaped accordingly. `build`
// answers immediately with the row in `building`, because the work outlives any
// reasonable HTTP wait and the environment row is the durable state a client
// polls — it is not job-backed, because a job registry that is in-memory and
// evicted would be a second, worse answer to a question the row already holds.
// `capture` IS job-backed: it packs a rootfs, which is minutes, and that is the
// same operation `snapshot.create` runs through runJob.

import (
	"context"
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
)

// environmentList is an object rather than a bare array, for the reason
// repoList documents: a collection needs somewhere to grow a cursor without
// breaking every client's parser.
type environmentList struct {
	Environments []ctlops.EnvironmentInfo `json:"environments"`
}

// environmentRequest is create-and-update in one, because they are the same
// gesture: naming an environment that exists adds to it. Everything here ADDS
// except the variables, which are keyed by (owner, tag, name) and so have no
// other owner to union with.
//
// Description is a pointer because "" is a real value. Omitted means "leave the
// description alone"; sending the empty string clears it. Before the field was
// optional, any call that set a variable also wiped the description — which is
// the one field somebody writes once and never types again.
type environmentRequest struct {
	Name        string            `json:"name"`
	Description *string           `json:"description,omitempty"`
	Repos       []environmentRepo `json:"repos,omitempty"`
	Secrets     []string          `json:"secrets,omitempty"`
	Rules       []string          `json:"rules,omitempty"`
	Vars        []ctlops.EnvVar   `json:"vars,omitempty"`
	// Runner is the VMM this environment's sandboxes must run on, and it is a
	// pointer for the same reason Description is: omitted leaves the requirement
	// alone, `""` drops it. A plain string would make every call that set a
	// variable also un-pin the environment from its VMM — and unlike a wiped
	// description that failure is invisible, because the sandbox still boots,
	// just on the wrong hypervisor.
	Runner *string `json:"runner,omitempty"`
	// OpenEgress opts OUT of the default egress rule-set a NEW environment is
	// given. It is a per-call gesture and is never stored: it means "do not
	// create one now", not "this environment is permanently open".
	OpenEgress bool `json:"open_egress,omitempty"`
	// Adopt agrees to create an environment over a tag that is already carrying
	// repositories, secrets, rule-sets, variables, a base image or sandboxes.
	// Without it that create answers 409 `env_tag_in_use`, whose `details` list
	// exactly what would be adopted — so the intended flow is to call once, show
	// the caller what came back, and call again with this set.
	Adopt bool `json:"adopt,omitempty"`
}

// environmentRepo is one attachment, with the same per-repo passthrough
// `POST /v1/repos` takes. `slug` alone is the common case.
type environmentRepo struct {
	Slug  string   `json:"slug"`
	Host  string   `json:"host,omitempty"`
	Ref   string   `json:"ref,omitempty"`
	Path  string   `json:"path,omitempty"`
	Write bool     `json:"write,omitempty"`
	Tags  []string `json:"tags,omitempty"`
}

// environmentScript is both the read and the write shape. `from` is read-only:
// a script sent here is recorded as `manual`, because it came from a person
// whatever the row said before, and telling the next build it came from a
// repository would send it looking for a file that does not exist.
type environmentScript struct {
	Script string `json:"script"`
	From   string `json:"from,omitempty"`
}

func (h *Handler) listEnvironments(w http.ResponseWriter, r *http.Request) {
	const op = "env.list"
	list, err := h.ops.ListEnvironments(caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	if list == nil {
		list = []ctlops.EnvironmentInfo{}
	}
	writeJSON(w, http.StatusOK, environmentList{Environments: list})
}

func (h *Handler) getEnvironment(w http.ResponseWriter, r *http.Request) {
	const op = "env.get"
	info, err := h.ops.GetEnvironment(caller(r), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// putEnvironment is POST on the collection rather than PUT on the item, and
// that is the same choice `POST /v1/repos` makes for the same reason: it is not
// a replace. The named attachments are unioned with what the tag already
// carries, so a second call adding a secret does not detach the first one's.
func (h *Handler) putEnvironment(w http.ResponseWriter, r *http.Request) {
	const op = "env.set"
	var req environmentRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	args := ctlops.EnvArgs{
		Name: req.Name, Description: req.Description,
		Secrets: req.Secrets, Rules: req.Rules, Vars: req.Vars, OpenEgress: req.OpenEgress,
		Adopt: req.Adopt, Runner: req.Runner,
	}
	for _, rp := range req.Repos {
		args.Repos = append(args.Repos, ctlops.RepoArgs{
			Slug: rp.Slug, Host: rp.Host, Ref: rp.Ref, Path: rp.Path,
			Write: rp.Write, Tags: rp.Tags,
		})
	}
	info, err := h.ops.PutEnvironment(r.Context(), caller(r), args)
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// deleteEnvironment answers with what SURVIVED, which is the point of the verb:
// removing a grouping must never destroy the things grouped, and a client that
// printed `{"deleted": true}` would leave somebody wondering whether their
// credential went with it.
func (h *Handler) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	const op = "env.rm"
	res, err := h.ops.DeleteEnvironment(r.Context(), caller(r), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *Handler) getEnvScript(w http.ResponseWriter, r *http.Request) {
	const op = "env.script"
	script, from, err := h.ops.EnvScript(caller(r), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, environmentScript{Script: script, From: from})
}

func (h *Handler) putEnvScript(w http.ResponseWriter, r *http.Request) {
	const op = "env.script.set"
	var req environmentScript
	if !h.decode(w, r, op, &req) {
		return
	}
	if err := h.ops.SetEnvScript(caller(r), r.PathValue("name"), req.Script, envs.SetupFromManual); err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, environmentScript{Script: req.Script, From: envs.SetupFromManual})
}

// adoptRepoScript replaces this environment's setup script with the
// .sparkbox/setup.sh in one of its repositories.
//
// POST and not PUT, because the caller is not supplying the representation —
// they are asking the server to go and get one, and which file that turns out
// to be is the server's answer. It returns the environment rather than the
// script so a console can re-render the card, drift verdict and all, from one
// response.
func (h *Handler) adoptRepoScript(w http.ResponseWriter, r *http.Request) {
	const op = "env.script.from_repo"
	info, err := h.ops.AdoptRepoScript(r.Context(), caller(r), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// buildEnvironment starts a build and returns the row, in `building`, right
// away. See the file header for why it is not job-backed.
//
// 200 and not 202, deliberately. A 202 in this API is a promise about the job
// contract — a Location to poll and a Prefer to widen the wait — and there is
// no job here. The request really did complete; what is unfinished is a STATE
// on the resource, which the answer carries as `"state": "building"`.
func (h *Handler) buildEnvironment(w http.ResponseWriter, r *http.Request) {
	const op = "env.build"
	info, err := h.ops.BuildEnvironment(r.Context(), caller(r), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// captureEnvironment adopts a failed build's paused builder exactly as it
// stands: the recovery path for a build somebody finished by hand. It packs a
// rootfs, so it runs as a job on the same budget `snapshot.create` uses.
func (h *Handler) captureEnvironment(w http.ResponseWriter, r *http.Request) {
	const op = "env.capture"
	name := r.PathValue("name")
	c := caller(r)
	h.runJob(w, r, op, ctlops.Ref{Type: "environment", Name: name}, ctlops.ArchiveTimeout,
		http.StatusOK,
		func(ctx context.Context) (any, error) {
			return h.ops.CaptureEnvironment(ctx, c, name)
		})
}
