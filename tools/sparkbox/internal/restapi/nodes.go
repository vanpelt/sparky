package restapi

// The fleet's machines. These are the only endpoints here besides invites that
// an ordinary session cannot reach: ctlops resolves the operator bit from the
// account store on every call, so there is nothing to check in this file — and
// deliberately no way for a handler to assert one.

import (
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// nodeList is an object rather than a bare array, for the same reason jobList
// is: a collection needs somewhere to grow a cursor without breaking clients.
type nodeList struct {
	Nodes []ctlops.NodeInfo `json:"nodes"`
}

func (h *Handler) listNodes(w http.ResponseWriter, r *http.Request) {
	const op = "nodes.list"
	list, err := h.ops.ListNodes(r.Context(), caller(r))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, nodeList{Nodes: list})
}

func (h *Handler) approveNode(w http.ResponseWriter, r *http.Request) {
	const op = "nodes.approve"
	n, err := h.ops.ApproveNode(r.Context(), caller(r), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (h *Handler) removeNode(w http.ResponseWriter, r *http.Request) {
	const op = "nodes.rm"
	name := r.PathValue("name")
	if err := h.ops.RemoveNode(r.Context(), caller(r), name); err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, deleted{Name: name, Deleted: true})
}
