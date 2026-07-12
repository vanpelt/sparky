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
)

type Server struct {
	mgr          *host.Manager
	log          *slog.Logger
	defaultImage string
}

func New(mgr *host.Manager, defaultImage string, log *slog.Logger) *Server {
	return &Server{mgr: mgr, log: log, defaultImage: defaultImage}
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
	return mux
}

type createRequest struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Image string `json:"image,omitempty"`
	VCPUs int64  `json:"vcpus,omitempty"`
	MemMB int64  `json:"mem_mb,omitempty"`
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

func statusFor(err error) int {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		return http.StatusNotFound
	case strings.Contains(msg, "already exists"):
		return http.StatusConflict
	case strings.Contains(msg, "invalid"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
