package restapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// sandboxList wraps the array so the listing can grow a cursor or a summary
// without every client's parser breaking.
type sandboxList struct {
	Sandboxes []ctlops.SandboxInfo `json:"sandboxes"`
}

// deleted is what every removal answers with. A 204 would be tidier for the
// synchronous ones, but destroy is job-backed and a job's result has to be a
// JSON document — so all six deletes share one shape rather than splitting
// between "204 unless it took a while".
type deleted struct {
	Name    string `json:"name"`
	Deleted bool   `json:"deleted"`
}

type createRequest struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
	// Node is the machine to build on; omit it to let the gateway choose, which
	// today means its own. It is the REST form of the new@ door's --node.
	Node  string `json:"node,omitempty"`
	VCPUs int64  `json:"vcpus"`
	MemMB int64  `json:"mem_mb"`
}

type resizeRequest struct {
	Size   string `json:"size"`    // "25G", "512M", or a bare number meaning GB
	SizeMB int64  `json:"size_mb"` // the same thing already in MiB
}

type renameRequest struct {
	Name string `json:"name"`
}

type tagsRequest struct {
	Tags []string `json:"tags"` // the WHOLE set; [] or null clears it
}

type visibilityRequest struct {
	Visibility string `json:"visibility"` // "public" | "private"
}

type tagsResponse struct {
	Tags []string `json:"tags"`
}

func (h *Handler) listSandboxes(w http.ResponseWriter, r *http.Request) {
	const op = "list"
	boxes, err := h.ops.List(r.Context(), caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, sandboxList{Sandboxes: boxes})
}

func (h *Handler) getSandbox(w http.ResponseWriter, r *http.Request) {
	const op = "get"
	box, err := h.ops.Get(r.Context(), caller(r), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, box)
}

// createSandbox is the REST form of the `new@` door, with that door's one
// ambiguity removed: a JSON body has named fields, so there is no reason to
// read bare words as tags and therefore no way to accidentally launch a command
// as a side effect of tagging.
//
// The name is generated HERE when the caller omits one, rather than inside
// ctlops.Create, so that the job's resource ref names a real sandbox. Without
// that, two concurrent unnamed creates would share the ref {sandbox,""} and the
// job de-duplicator would collapse them into one.
func (h *Handler) createSandbox(w http.ResponseWriter, r *http.Request) {
	const op = "create"
	var req createRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	name := req.Name
	if name == "" {
		name = h.ops.GenerateName()
	}
	c := caller(r)
	args := ctlops.CreateArgs{Name: name, Tags: req.Tags, Node: req.Node, VCPUs: req.VCPUs, MemMB: req.MemMB}
	// The chosen machine rides in the job's resource ref. A ref is compared
	// whole by the job de-duplicator (ctlops.Ops.Go), so two creates of one name
	// onto two machines would otherwise collapse into a single job and the
	// second caller would be handed the first one's answer — a sandbox on a
	// machine they did not ask for, reported as theirs.
	h.runJob(w, r, op, ctlops.Ref{Type: "sandbox", Name: name, Args: req.Node}, ctlops.DialTimeout, http.StatusCreated,
		func(ctx context.Context) (any, error) { return h.ops.Create(ctx, c, args) })
}

func (h *Handler) pause(w http.ResponseWriter, r *http.Request) {
	const op = "pause"
	c, name := caller(r), r.PathValue("name")
	h.runJob(w, r, op, ctlops.Ref{Type: "sandbox", Name: name}, ctlops.PauseTimeout, http.StatusOK,
		func(ctx context.Context) (any, error) { return h.ops.Pause(ctx, c, name) })
}

// resume is ctl's `restore`: it starts a paused sandbox and folds in the
// download-and-unpack for an archived one, which is why it carries the archive
// budget rather than the pause one.
func (h *Handler) resume(w http.ResponseWriter, r *http.Request) {
	const op = "restore"
	c, name := caller(r), r.PathValue("name")
	h.runJob(w, r, op, ctlops.Ref{Type: "sandbox", Name: name}, ctlops.ArchiveTimeout, http.StatusOK,
		func(ctx context.Context) (any, error) { return h.ops.Resume(ctx, c, name) })
}

func (h *Handler) archive(w http.ResponseWriter, r *http.Request) {
	const op = "archive"
	c, name := caller(r), r.PathValue("name")
	h.runJob(w, r, op, ctlops.Ref{Type: "sandbox", Name: name}, ctlops.ArchiveTimeout, http.StatusOK,
		func(ctx context.Context) (any, error) { return h.ops.Archive(ctx, c, name) })
}

// resize accepts either the human size `ctl resize` takes ("25G") or a plain
// MiB integer, because a script that already computed a number should not have
// to render it back into a string to be parsed again. Growing the disk pauses
// the guest and DISCARDS its memory snapshot — running processes die — which is
// documented on the operation rather than warned about here, since there is no
// terminal to print a warning to.
func (h *Handler) resize(w http.ResponseWriter, r *http.Request) {
	const op = "resize"
	var req resizeRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	sizeMB := req.SizeMB
	if req.Size != "" {
		var err error
		if sizeMB, err = ctlops.ParseSize(req.Size); err != nil {
			h.fail(w, r, op, ctlops.Invalid(op, "bad_size", "%v", err))
			return
		}
	}
	if sizeMB <= 0 {
		h.fail(w, r, op, ctlops.Invalid(op, "bad_size",
			"a size is required — send `size` (e.g. \"25G\") or `size_mb`"))
		return
	}
	c, name := caller(r), r.PathValue("name")
	// The size goes in the ref's Args: two resizes of one sandbox to different
	// sizes are different work, and without it the de-duplicator would answer
	// the second with the first's result and never run it.
	ref := ctlops.Ref{Type: "sandbox", Name: name, Args: strconv.FormatInt(sizeMB, 10)}
	h.runJob(w, r, op, ref, ctlops.ResizeTimeout, http.StatusOK,
		func(ctx context.Context) (any, error) { return h.ops.Resize(ctx, c, name, sizeMB) })
}

func (h *Handler) reboot(w http.ResponseWriter, r *http.Request) {
	const op = "reboot"
	c, name := caller(r), r.PathValue("name")
	h.runJob(w, r, op, ctlops.Ref{Type: "sandbox", Name: name}, ctlops.PauseTimeout, http.StatusOK,
		func(ctx context.Context) (any, error) { return h.ops.Reboot(ctx, c, name) })
}

func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	const op = "rename"
	var req renameRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	c, name := caller(r), r.PathValue("name")
	// The destination name discriminates, for the same reason resize's size does.
	ref := ctlops.Ref{Type: "sandbox", Name: name, Args: req.Name}
	h.runJob(w, r, op, ref, ctlops.PauseTimeout, http.StatusOK,
		func(ctx context.Context) (any, error) { return h.ops.Rename(ctx, c, name, req.Name) })
}

func (h *Handler) destroySandbox(w http.ResponseWriter, r *http.Request) {
	const op = "rm"
	c, name := caller(r), r.PathValue("name")
	h.runJob(w, r, op, ctlops.Ref{Type: "sandbox", Name: name}, ctlops.PauseTimeout, http.StatusOK,
		func(ctx context.Context) (any, error) {
			if err := h.ops.Destroy(ctx, c, name); err != nil {
				return nil, err
			}
			return deleted{Name: name, Deleted: true}, nil
		})
}

// pin and unpin set the always-on flag and nothing else. `ctl pin` also resumes
// the sandbox and reports exit 1 when the flag stuck but the resume did not;
// the REST API keeps the two apart precisely so there is no half-succeeded
// state to invent a status code for. Callers that want ctl's behaviour pin and
// then POST /resume.
func (h *Handler) pin(w http.ResponseWriter, r *http.Request)   { h.setPinned(w, r, true) }
func (h *Handler) unpin(w http.ResponseWriter, r *http.Request) { h.setPinned(w, r, false) }

func (h *Handler) setPinned(w http.ResponseWriter, r *http.Request, pinned bool) {
	op := "unpin"
	if pinned {
		op = "pin"
	}
	box, err := h.ops.SetPinned(r.Context(), caller(r), r.PathValue("name"), pinned)
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, box)
}

func (h *Handler) getTags(w http.ResponseWriter, r *http.Request) {
	const op = "tags.get"
	tags, err := h.ops.Tags(r.Context(), caller(r), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, tagsResponse{Tags: tags})
}

// setTags replaces the whole set, which is why it is a PUT: there is no way to
// express "add one tag" and no way to clear the set with a PATCH-shaped verb.
// ctlops re-pushes the guest's secret environment afterwards, so the change
// takes effect immediately rather than at the next resume.
func (h *Handler) setTags(w http.ResponseWriter, r *http.Request) {
	const op = "tags.set"
	var req tagsRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	tags, err := h.ops.SetTags(r.Context(), caller(r), r.PathValue("name"), req.Tags)
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, tagsResponse{Tags: tags})
}

type visibilityResponse struct {
	Routes  []ctlops.RouteInfo `json:"routes"`
	Changed int                `json:"changed,omitempty"`
}

func (h *Handler) getVisibility(w http.ResponseWriter, r *http.Request) {
	const op = "share.get"
	rs, err := h.ops.Visibility(r.Context(), caller(r), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, visibilityResponse{Routes: rs})
}

// setVisibility flips EVERY route of the sandbox together — the per-sandbox
// granularity `ctl share` has always had, and the one that matches how people
// think about "who can reach this VM". The user console's per-route endpoint is
// a different operation and stays where it is.
func (h *Handler) setVisibility(w http.ResponseWriter, r *http.Request) {
	const op = "share.set"
	var req visibilityRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	res, err := h.ops.SetVisibility(r.Context(), caller(r), r.PathValue("name"), req.Visibility)
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, visibilityResponse{Routes: res.Routes, Changed: res.Changed})
}
