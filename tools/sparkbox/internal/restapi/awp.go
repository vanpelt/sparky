package restapi

import (
	"context"
	"net/http"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// awpCreateRequest is the provisional control-plane action contract. It has no
// bootstrap token by design: the guest exchanges its Sparkbox workload OIDC
// identity for the run-scoped AWP credential after boot.
type awpCreateRequest struct {
	SandboxID       string `json:"sandbox_id"`
	RunID           string `json:"run_id"`
	TenantID        string `json:"tenant_id"`
	ControlPlaneURL string `json:"control_plane_url"`
	OIDCAudience    string `json:"oidc_audience"`
	Node            string `json:"node,omitempty"`
	VCPUs           int64  `json:"vcpus,omitempty"`
	MemMB           int64  `json:"mem_mb,omitempty"`
}

func (h *Handler) createAWPSandbox(w http.ResponseWriter, r *http.Request) {
	const op = "awp.create"
	var req awpCreateRequest
	if !h.decode(w, r, op, &req) {
		return
	}
	c := caller(r)
	args := ctlops.AWPCreateArgs{
		SandboxID: req.SandboxID, RunID: req.RunID, TenantID: req.TenantID,
		ControlPlaneURL: req.ControlPlaneURL, OIDCAudience: req.OIDCAudience,
		Node: req.Node, VCPUs: req.VCPUs, MemMB: req.MemMB,
	}
	ref := ctlops.Ref{Type: "awp-sandbox", Name: req.SandboxID, Args: req.RunID}
	h.runJob(w, r, op, ref, ctlops.DialTimeout, http.StatusCreated,
		func(ctx context.Context) (any, error) { return h.ops.CreateAWPSandbox(ctx, c, args) })
}

func (h *Handler) getAWPSandbox(w http.ResponseWriter, r *http.Request) {
	const op = "awp.get"
	box, err := h.ops.GetAWPSandbox(r.Context(), caller(r), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, op, err)
		return
	}
	writeJSON(w, http.StatusOK, box)
}

func (h *Handler) deleteAWPSandbox(w http.ResponseWriter, r *http.Request) {
	const op = "awp.rm"
	c, name := caller(r), r.PathValue("name")
	h.runJob(w, r, op, ctlops.Ref{Type: "awp-sandbox", Name: name}, ctlops.PauseTimeout, http.StatusOK,
		func(ctx context.Context) (any, error) {
			if err := h.ops.DeleteAWPSandbox(ctx, c, name); err != nil {
				return nil, err
			}
			return deleted{Name: name, Deleted: true}, nil
		})
}
