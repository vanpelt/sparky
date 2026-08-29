package restapi

import (
	"context"
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

type snapshotList struct {
	Snapshots []ctlops.SnapshotInfo `json:"snapshots"`
}

type snapshotRequest struct {
	Sandbox string `json:"sandbox"`
	Name    string `json:"name"`
}

type forkRequest struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// bindRequest points a tag at a snapshot. The tag is in the path and the
// snapshot in the body, because the tag is the resource: a tag has exactly one
// base image, so PUT-ing a snapshot onto it is the whole operation and DELETE
// on the same URL is its inverse.
type bindRequest struct {
	Snapshot string `json:"snapshot"`
}

func (h *Handler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	const op = "snapshot.list"
	snaps, err := h.ops.ListSnapshots(r.Context(), caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshotList{Snapshots: snaps})
}

// createSnapshot implicitly PAUSES a running sandbox: the manager strips the
// managed env block from the rootfs, pauses the guest, then compacts. That is
// documented on the operation because there is no way to do it otherwise — a
// snapshot of a running filesystem would not be restorable.
func (h *Handler) createSnapshot(w http.ResponseWriter, r *http.Request) {
	const op = "snapshot.create"
	var req snapshotRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	c := caller(r)
	// The ref names the snapshot being made; the SOURCE sandbox is what
	// distinguishes two calls that would otherwise collapse into one, so it goes
	// in Args.
	ref := ctlops.Ref{Type: "snapshot", Name: req.Name, Args: req.Sandbox}
	h.runJob(w, r, op, ref, ctlops.ArchiveTimeout,
		http.StatusCreated,
		func(ctx context.Context) (any, error) {
			return h.ops.CreateSnapshot(ctx, c, req.Sandbox, req.Name)
		})
}

func (h *Handler) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	const op = "snapshot.rm"
	name := r.PathValue("name")
	if err := h.ops.DeleteSnapshot(r.Context(), caller(r), name); err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, deleted{Name: name, Deleted: true})
}

// fork creates a sandbox from one of the caller's snapshots. Tags are stamped
// before the fork inside ctlops, for the same reason they are on create: the
// fork IS a create, and it fires the same asynchronous secret-env push whose
// contents the tags decide.
func (h *Handler) fork(w http.ResponseWriter, r *http.Request) {
	const op = "fork"
	var req forkRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	c := caller(r)
	args := ctlops.ForkArgs{Snapshot: r.PathValue("name"), Name: req.Name, Tags: req.Tags}
	// The job's resource is the sandbox being made, not the snapshot being read:
	// two forks of one template are different work and must not de-duplicate
	// into each other.
	h.runJob(w, r, op, ctlops.Ref{Type: "sandbox", Name: req.Name}, ctlops.PauseTimeout,
		http.StatusCreated,
		func(ctx context.Context) (any, error) { return h.ops.Fork(ctx, c, args) })
}

// bindTemplate makes one of the caller's snapshots the base image of one of
// their tags, so every sandbox they create carrying it boots from that disk.
//
// Neither this nor unbindTemplate is a job. Both are a single sqlite write
// behind an ownership check — there is nothing to wait on, no guest to reach
// and no VM to pause — so they answer 200 directly rather than escalating
// through runJob, and the spec documents no 202 and no Prefer for either.
func (h *Handler) bindTemplate(w http.ResponseWriter, r *http.Request) {
	const op = "snapshot.bind"
	var req bindRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	res, err := h.ops.BindTemplate(r.Context(), caller(r), req.Snapshot, r.PathValue("tag"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// unbindTemplate drops the caller's binding for a tag and reports what it was.
// Nothing is deleted: sandboxes already built from the template keep running,
// and the next create on that tag takes the host's default image again.
func (h *Handler) unbindTemplate(w http.ResponseWriter, r *http.Request) {
	const op = "snapshot.unbind"
	b, err := h.ops.UnbindTemplate(r.Context(), caller(r), r.PathValue("tag"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}
