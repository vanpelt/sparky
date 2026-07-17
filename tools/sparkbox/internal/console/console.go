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
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

//go:embed index.html
var indexHTML []byte

const (
	cookieName   = "spark_console"
	cookieMaxAge = 12 * time.Hour
	tokenSalt    = "sparkbox-console/v1"
)

// probeTimeout bounds the per-route TCP dial that checks whether anything is
// listening on a forwarded port. Guest IPs are on a local bridge, so a live
// listener answers in microseconds; anything slower is effectively down.
const probeTimeout = 300 * time.Millisecond

// Handler serves the console UI and its JSON API.
type Handler struct {
	mgr    *host.Manager
	store  *routes.Store   // optional: nil hides web routes from the UI
	sched  *schedule.Store // optional: nil hides the next-wake column
	domain string          // base domain for building route URLs, e.g. "hivemind.tools"
	log    *slog.Logger
	token  string // expected cookie value, derived from the password
	secure bool   // set the Secure flag on the auth cookie (proxy terminates TLS)
}

// SetSchedules attaches the platform-scheduler store so the dashboard can show
// each sandbox's next scheduled wake. Optional; nil leaves the column blank.
func (h *Handler) SetSchedules(s *schedule.Store) { h.sched = s }

// New builds a console handler. password must be non-empty; callers gate on that
// so an unset password disables the console entirely rather than shipping an
// empty-password login. secure should be true when the proxy edge serves TLS.
func New(mgr *host.Manager, store *routes.Store, domain, password string, secure bool, log *slog.Logger) *Handler {
	return &Handler{mgr: mgr, store: store, domain: domain, log: log, token: deriveToken(password), secure: secure}
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
	mux.HandleFunc("GET /api/cluster", h.requireAuth(h.cluster))
	mux.HandleFunc("POST /api/sandboxes/{name}/pause", h.requireAuth(h.pause))
	mux.HandleFunc("POST /api/sandboxes/{name}/resume", h.requireAuth(h.resume))
	mux.HandleFunc("POST /api/sandboxes/{name}/pin", h.requireAuth(h.pin))
	mux.HandleFunc("POST /api/sandboxes/{name}/unpin", h.requireAuth(h.unpin))
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

// routeStatus is a web route as shown in the UI: where it points and whether a
// TCP dial to the forwarded port currently succeeds. Listening is only
// meaningful while the sandbox is running (a paused sandbox has no address).
type routeStatus struct {
	Subdomain string `json:"subdomain"`
	Port      int    `json:"port"`
	Listening bool   `json:"listening"`
}

// sandboxView is a Sandbox plus its web routes and next scheduled wake for the
// dashboard. NextWake is the soonest upcoming platform-scheduler fire across the
// sandbox's jobs; Schedules is how many it has.
type sandboxView struct {
	*host.Sandbox
	Routes    []routeStatus `json:"routes"`
	NextWake  *time.Time    `json:"next_wake,omitempty"`
	Schedules int           `json:"schedules,omitempty"`
}

func (h *Handler) list(w http.ResponseWriter, _ *http.Request) {
	boxes := h.mgr.List()
	views := make([]sandboxView, len(boxes))
	now := time.Now()
	var wg sync.WaitGroup
	for i, b := range boxes {
		views[i] = sandboxView{Sandbox: b, Routes: []routeStatus{}}
		views[i].NextWake, views[i].Schedules = h.nextWake(b.Name, now)
		if h.store == nil {
			continue
		}
		rs, err := h.store.ListBySandbox(b.Name)
		if err != nil {
			h.log.Warn("route list failed", "sandbox", b.Name, "err", err)
			continue
		}
		for _, rt := range rs {
			views[i].Routes = append(views[i].Routes, routeStatus{Subdomain: rt.Subdomain, Port: rt.Port})
		}
		// Probe every forwarded port of a running sandbox concurrently; the
		// whole fan-out is bounded by probeTimeout, not routes × timeout.
		if b.State != vmm.StateRunning || b.HostIP == "" {
			continue
		}
		for j := range views[i].Routes {
			wg.Add(1)
			go func(addr string, listening *bool) {
				defer wg.Done()
				conn, err := net.DialTimeout("tcp", addr, probeTimeout)
				if err == nil {
					conn.Close()
					*listening = true
				}
			}(net.JoinHostPort(b.HostIP, strconv.Itoa(views[i].Routes[j].Port)), &views[i].Routes[j].Listening)
		}
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, views)
}

// nextWake returns the soonest upcoming scheduled fire for a sandbox and the
// number of schedules it has. Zero values when scheduling is disabled or the
// sandbox has none.
func (h *Handler) nextWake(sandbox string, now time.Time) (*time.Time, int) {
	if h.sched == nil {
		return nil, 0
	}
	entries, err := h.sched.ListBySandbox(sandbox)
	if err != nil {
		h.log.Warn("schedule list failed", "sandbox", sandbox, "err", err)
		return nil, 0
	}
	var soonest time.Time
	for _, e := range entries {
		t, err := schedule.NextRun(e.Spec, now)
		if err != nil {
			continue
		}
		if soonest.IsZero() || t.Before(soonest) {
			soonest = t
		}
	}
	if soonest.IsZero() {
		return nil, len(entries)
	}
	return &soonest, len(entries)
}

// clusterResponse reports capacity as a list of nodes so the payload shape
// already fits a future multi-box deployment (today it has exactly one entry).
type clusterResponse struct {
	Domain string              `json:"domain"`
	Nodes  []host.NodeCapacity `json:"nodes"`
}

func (h *Handler) cluster(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, clusterResponse{
		Domain: h.domain,
		Nodes:  []host.NodeCapacity{h.mgr.Capacity()},
	})
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

// pin marks a sandbox always-on and resumes it so its in-guest daemons start
// running immediately. unpin clears the flag, letting the reaper pause it again.
func (h *Handler) pin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.mgr.SetPinned(name, true); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	box, err := h.mgr.EnsureRunning(r.Context(), name)
	if err != nil {
		// The flag stuck; it just isn't warm yet. Surface the reason.
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console pinned sandbox", "name", name)
	writeJSON(w, http.StatusOK, box)
}

func (h *Handler) unpin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.mgr.SetPinned(name, false); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console unpinned sandbox", "name", name)
	box, _ := h.mgr.Get(name)
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
