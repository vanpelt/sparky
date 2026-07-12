// Package console is a small password-gated web UI for operators: it lists the
// sandboxes on this host and lets you pause a running one (or resume a paused
// one). It is meant to be served at console.<domain> through the proxy edge, so
// it rides the same wildcard TLS cert as the sandbox web routes.
//
// Auth is deliberately minimal — a single shared password set via
// --console-password. A correct login mints a cookie whose value is
// HMAC(password, salt); every request re-derives and constant-time-compares it,
// so no session state is kept server-side and the raw password is never stored
// or echoed back.
package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

//go:embed index.html
var indexHTML []byte

const (
	cookieName   = "spark_console"
	cookieMaxAge = 12 * time.Hour
	tokenSalt    = "sparkbox-console/v1"
)

// Handler serves the console UI and its JSON API.
type Handler struct {
	mgr    *host.Manager
	log    *slog.Logger
	token  string // expected cookie value, derived from the password
	secure bool   // set the Secure flag on the auth cookie (proxy terminates TLS)
}

// New builds a console handler. password must be non-empty; callers gate on that
// so an unset password disables the console entirely rather than shipping an
// empty-password login. secure should be true when the proxy edge serves TLS.
func New(mgr *host.Manager, password string, secure bool, log *slog.Logger) *Handler {
	return &Handler{mgr: mgr, log: log, token: deriveToken(password), secure: secure}
}

// deriveToken maps a password to the opaque cookie value. Same password in,
// same token out, so validation needs no server-side session store.
func deriveToken(password string) string {
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write([]byte(tokenSalt))
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *Handler) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", h.login)
	mux.HandleFunc("POST /logout", h.logout)
	mux.HandleFunc("GET /api/sandboxes", h.requireAuth(h.list))
	mux.HandleFunc("POST /api/sandboxes/{name}/pause", h.requireAuth(h.pause))
	mux.HandleFunc("POST /api/sandboxes/{name}/resume", h.requireAuth(h.resume))
	mux.HandleFunc("GET /", h.index)
	return mux
}

// index always serves the single-page app; the page itself calls /api/sandboxes
// and renders the login form if that returns 401.
func (h *Handler) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML) //nolint:errcheck
}

type loginRequest struct {
	Password string `json:"password"`
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	// Constant-time compare of derived tokens: equal-length, and no early-out on
	// the password itself.
	got := deriveToken(req.Password)
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.token)) != 1 {
		writeErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    h.token,
		Path:     "/",
		MaxAge:   int(cookieMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   h.secure,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) logout(w http.ResponseWriter, _ *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// requireAuth wraps a handler, rejecting requests without a valid session cookie.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(h.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "not authenticated")
			return
		}
		next(w, r)
	}
}

func (h *Handler) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.mgr.List())
}

func (h *Handler) pause(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.mgr.Pause(r.Context(), name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console paused sandbox", "name", name)
	box, _ := h.mgr.Get(name)
	writeJSON(w, http.StatusOK, box)
}

func (h *Handler) resume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	box, err := h.mgr.EnsureRunning(r.Context(), name)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console resumed sandbox", "name", name)
	writeJSON(w, http.StatusOK, box)
}

func statusFor(err error) int {
	if err != nil && contains(err.Error(), "not found") {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
