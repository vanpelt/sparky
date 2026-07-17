// Package api exposes the control-plane HTTP API. MVP scope: sandbox CRUD +
// pause/resume on the local host. Bind it to localhost or a private network —
// there is no auth layer yet.
package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

type Server struct {
	mgr          *host.Manager
	routes       *routes.Store
	log          *slog.Logger
	defaultImage string
}

func New(mgr *host.Manager, store *routes.Store, defaultImage string, log *slog.Logger) *Server {
	return &Server{mgr: mgr, routes: store, log: log, defaultImage: defaultImage}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/sandboxes", s.list)
	mux.HandleFunc("POST /v1/sandboxes", s.create)
	mux.HandleFunc("GET /v1/sandboxes/{name}", s.get)
	mux.HandleFunc("DELETE /v1/sandboxes/{name}", s.destroy)
	mux.HandleFunc("POST /v1/sandboxes/{name}/pause", s.pause)
	mux.HandleFunc("POST /v1/sandboxes/{name}/resume", s.resume)
	mux.HandleFunc("POST /v1/sandboxes/{name}/archive", s.archive)
	mux.HandleFunc("POST /v1/sandboxes/{name}/snapshot", s.snapshot)
	mux.HandleFunc("POST /v1/snapshots/{snapshot}/fork", s.fork)
	mux.HandleFunc("GET /v1/sandboxes/{name}/routes", s.listRoutes)
	mux.HandleFunc("POST /v1/sandboxes/{name}/routes", s.addRoute)
	mux.HandleFunc("DELETE /v1/routes/{subdomain}", s.deleteRoute)
	return mux
}

type createRequest struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Image string `json:"image,omitempty"`
	VCPUs int64  `json:"vcpus,omitempty"`
	MemMB int64  `json:"mem_mb,omitempty"`
	// Subdomain/Port customise the sandbox's web route at create time. Omitted,
	// the sandbox is reachable at <name>.<domain> -> :8000.
	Subdomain string `json:"subdomain,omitempty"`
	Port      int    `json:"port,omitempty"`
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Owner == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name and owner are required"))
		return
	}
	if req.Image == "" {
		req.Image = s.defaultImage
	}
	box, err := s.mgr.Create(r.Context(), req.Name, req.Owner, req.Image, req.VCPUs, req.MemMB)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	// Manager already registered the default route <name> -> :8000. Override it
	// (or add a second subdomain) if the request customised the web route.
	if s.routes != nil && (req.Subdomain != "" || req.Port != 0) {
		route := routes.Route{
			Subdomain: orDefault(req.Subdomain, req.Name),
			Sandbox:   req.Name,
			Owner:     req.Owner,
			Port:      orDefaultInt(req.Port, routes.DefaultPort),
		}
		if err := s.routes.Upsert(route); err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
	}
	writeJSON(w, http.StatusCreated, box)
}

func (s *Server) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.mgr.List())
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	box, ok := s.mgr.Get(r.PathValue("name"))
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	writeJSON(w, http.StatusOK, box)
}

func (s *Server) destroy(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Destroy(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pause(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Pause(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	box, err := s.mgr.EnsureRunning(r.Context(), r.PathValue("name"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, box)
}

// archive parks the sandbox's rootfs in object storage and frees its host disk.
// Restore is the existing resume endpoint (EnsureRunning restores transparently).
func (s *Server) archive(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.Archive(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type snapshotRequest struct {
	SnapshotName string `json:"snapshot_name"`
}

// snapshot captures a sandbox's current disk as a fork-able template owned by
// the sandbox's owner.
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	box, ok := s.mgr.Get(name)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	var req snapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	snap, err := s.mgr.Snapshot(r.Context(), name, req.SnapshotName, box.Owner)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, snap)
}

type forkRequest struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	VCPUs int64  `json:"vcpus,omitempty"`
	MemMB int64  `json:"mem_mb,omitempty"`
}

// fork creates a new sandbox from one of an owner's snapshots.
func (s *Server) fork(w http.ResponseWriter, r *http.Request) {
	var req forkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Owner == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name and owner are required"))
		return
	}
	box, err := s.mgr.Fork(r.Context(), r.PathValue("snapshot"), req.Name, req.Owner, req.VCPUs, req.MemMB)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, box)
}

// listRoutes returns every web route pointing at a sandbox.
func (s *Server) listRoutes(w http.ResponseWriter, r *http.Request) {
	if s.routes == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("routing not enabled"))
		return
	}
	name := r.PathValue("name")
	if _, ok := s.mgr.Get(name); !ok {
		writeErr(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	rs, err := s.routes.ListBySandbox(name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if rs == nil {
		rs = []routes.Route{}
	}
	writeJSON(w, http.StatusOK, rs)
}

type routeRequest struct {
	Subdomain string `json:"subdomain,omitempty"`
	Port      int    `json:"port,omitempty"`
}

// addRoute creates or updates a web route for a sandbox. Subdomain defaults to
// the sandbox name, port to 8000.
func (s *Server) addRoute(w http.ResponseWriter, r *http.Request) {
	if s.routes == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("routing not enabled"))
		return
	}
	name := r.PathValue("name")
	box, ok := s.mgr.Get(name)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("not found"))
		return
	}
	var req routeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	route := routes.Route{
		Subdomain: orDefault(req.Subdomain, name),
		Sandbox:   name,
		Owner:     box.Owner,
		Port:      orDefaultInt(req.Port, routes.DefaultPort),
	}
	if err := s.routes.Upsert(route); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	// Echo the stored row so created_at reflects reality (Upsert preserves the
	// original timestamp on updates).
	if stored, ok, _ := s.routes.GetBySubdomain(route.Subdomain); ok {
		route = stored
	}
	writeJSON(w, http.StatusCreated, route)
}

// deleteRoute removes a single route by subdomain.
func (s *Server) deleteRoute(w http.ResponseWriter, r *http.Request) {
	if s.routes == nil {
		writeErr(w, http.StatusNotImplemented, errors.New("routing not enabled"))
		return
	}
	if err := s.routes.Delete(r.PathValue("subdomain")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func statusFor(err error) int {
	if errors.Is(err, routes.ErrSubdomainTaken) {
		return http.StatusConflict
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		return http.StatusNotFound
	case strings.Contains(msg, "already exists"):
		return http.StatusConflict
	case strings.Contains(msg, "invalid"):
		return http.StatusBadRequest
	case strings.Contains(msg, "not enabled"):
		return http.StatusNotImplemented
	case strings.Contains(msg, "pool full"):
		return http.StatusInsufficientStorage
	default:
		return http.StatusInternalServerError
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func orDefaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
