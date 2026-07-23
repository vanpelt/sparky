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
	"context"
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
	"github.com/vanpelt/sparky/tools/sparkbox/internal/webui"
)

//go:embed index.html
var indexTemplate []byte

// indexPage is the console SPA composed against the shared design system,
// minified, and pre-gzipped once at package init — see internal/webui.
var indexPage = webui.Build(indexTemplate)

const (
	cookieName   = "spark_console"
	cookieMaxAge = 12 * time.Hour
	tokenSalt    = "sparkbox-console/v1"
)

// Local names for the dashboard probe budgets, which are stated once in
// internal/webui so both consoles time a fleet the same way. probeTimeout also
// bounds the balloon read below; tunneledProbeTimeout is spent inside
// webui.Probe.Listening.
const (
	probeTimeout         = webui.ProbeTimeout
	tunneledProbeTimeout = webui.TunneledProbeTimeout
)

// Dialer is net.Dialer.DialContext's shape — see SetDialer.
type Dialer = webui.Dialer

// Sandboxes is the sandbox lifecycle and inventory this console drives. It is
// an interface so the console can be handed the fleet router instead of one
// machine's manager: sandbox names are allocated fleet-wide in the placement
// ledger, so a destroy that went straight to the local manager would leave the
// name reserved forever and a fork would take a name no row ever recorded.
// Satisfied structurally by both *host.Manager and *fleet.Fleet (importing
// neither), and on a one-machine deployment the fleet is the manager, so the
// console behaves exactly as it did before it existed.
type Sandboxes interface {
	Get(name string) (*host.Sandbox, bool)
	List() []*host.Sandbox
	EnsureRunning(ctx context.Context, name string) (*host.Sandbox, error)
	Pause(ctx context.Context, name string) error
	Archive(ctx context.Context, name string) error
	Destroy(ctx context.Context, name string) error
	SetPinned(name string, pinned bool) error
	Snapshot(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error)
	DeleteSnapshot(ctx context.Context, snapName, owner string) error
	Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error)
}

var _ Sandboxes = (*host.Manager)(nil)

// Handler serves the console UI and its JSON API.
type Handler struct {
	// mgr is this machine's own manager. It is kept alongside boxes because the
	// balloon and snapshot reads below can only be answered by the machine
	// running the VM — they are not routable, so a sandbox that lives elsewhere
	// is skipped rather than asked.
	mgr    *host.Manager
	boxes  Sandboxes
	store  *routes.Store   // optional: nil hides web routes from the UI
	sched  *schedule.Store // optional: nil hides the next-wake column
	domain string          // base domain for building route URLs, e.g. "hivemind.tools"
	log    *slog.Logger
	token  string // expected cookie value, derived from the password
	secure bool   // set the Secure flag on the auth cookie (proxy terminates TLS)

	// probe carries this machine's name and the fleet dialer: together they
	// decide which rows are remote and how long their port probes may take.
	probe webui.Probe

	capacities func() []host.NodeCapacity // every machine's resource picture
}

// SetSchedules attaches the platform-scheduler store so the dashboard can show
// each sandbox's next scheduled wake. Optional; nil leaves the column blank.
func (h *Handler) SetSchedules(s *schedule.Store) { h.sched = s }

// New builds a console handler. password must be non-empty; callers gate on that
// so an unset password disables the console entirely rather than shipping an
// empty-password login. secure should be true when the proxy edge serves TLS.
func New(mgr *host.Manager, store *routes.Store, domain, password string, secure bool, log *slog.Logger) *Handler {
	h := &Handler{
		mgr: mgr, boxes: mgr, store: store, domain: domain, log: log,
		token: deriveToken(password), secure: secure,
	}
	if mgr != nil {
		h.probe.Node = mgr.NodeName()
	}
	h.capacities = func() []host.NodeCapacity { return []host.NodeCapacity{h.mgr.Capacity()} }
	return h
}

// SetSandboxes points the console's lifecycle actions and its listing at the
// fleet router rather than straight at this machine's manager, so an action
// reaches the machine that actually holds the sandbox and every name it takes
// or releases is recorded in the placement ledger. Unset, the console drives
// the manager it was built with — which is what a one-machine deployment wants
// and what every test builds.
func (h *Handler) SetSandboxes(s Sandboxes) { h.boxes = s }

// SetCapacities replaces the cluster endpoint's one-machine answer with the
// fleet's. The page has always summed over the array it is handed, so pointing
// this at the fleet is the whole of the multi-machine capacity story on the
// front end.
func (h *Handler) SetCapacities(f func() []host.NodeCapacity) { h.capacities = f }

// SetDialer routes the listening-port probe through d instead of dialing the
// guest's address on the host network — see webui.Probe.Dial for why a fleet
// cannot be probed directly.
func (h *Handler) SetDialer(d Dialer) { h.probe.Dial = d }

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
	mux.HandleFunc("DELETE /api/sandboxes/{name}", h.requireAuth(h.destroy))
	mux.HandleFunc("POST /api/sandboxes/{name}/archive", h.requireAuth(h.archive))
	mux.HandleFunc("POST /api/sandboxes/{name}/snapshot", h.requireAuth(h.snapshot))
	mux.HandleFunc("GET /api/snapshots", h.requireAuth(h.listSnapshots))
	mux.HandleFunc("POST /api/snapshots/{snapshot}/fork", h.requireAuth(h.fork))
	mux.HandleFunc("POST /api/snapshots/{snapshot}/delete", h.requireAuth(h.deleteSnapshot))
	mux.HandleFunc("POST /api/sandboxes/{name}/pin", h.requireAuth(h.pin))
	mux.HandleFunc("POST /api/sandboxes/{name}/unpin", h.requireAuth(h.unpin))
	mux.HandleFunc("GET /", h.index)
	return mux
}

// index always serves the single-page app; the page itself calls /api/sandboxes
// and renders the login form if that returns 401.
func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	indexPage.ServeHTTP(w, r)
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
	Subdomain  string `json:"subdomain"`
	Port       int    `json:"port"`
	Visibility string `json:"visibility"`
	Listening  bool   `json:"listening"`
}

// sandboxView is a Sandbox plus its web routes and next scheduled wake for the
// dashboard. NextWake is the soonest upcoming platform-scheduler fire across the
// sandbox's jobs; Schedules is how many it has.
type sandboxView struct {
	*host.Sandbox
	Routes    []routeStatus `json:"routes"`
	NextWake  *time.Time    `json:"next_wake,omitempty"`
	Schedules int           `json:"schedules,omitempty"`
	// MemUsedMB is the guest's real memory use in MiB (from balloon stats), the
	// live-overcommit signal against MemMB's ceiling. nil when unavailable.
	MemUsedMB *int64 `json:"mem_used_mb,omitempty"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	boxes := h.boxes.List()
	views := make([]sandboxView, len(boxes))
	now := time.Now()
	var wg sync.WaitGroup
	for i, b := range boxes {
		remote := h.probe.Remote(b)
		views[i] = sandboxView{Sandbox: webui.Public(b), Routes: []routeStatus{}}
		views[i].NextWake, views[i].Schedules = h.nextWake(b.Name, now)
		// Read the guest's real memory use concurrently (balloon stats); bounded
		// by probeTimeout so one slow VM can't stall the dashboard. Only for the
		// sandboxes on this machine: a balloon can only be asked of the host
		// running the VM, so a remote name would just miss in the local
		// manager's map and report nothing.
		if b.State == vmm.StateRunning && !remote {
			wg.Add(1)
			go func(name string, dst **int64) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
				defer cancel()
				if used, ok := h.mgr.MemStats(ctx, name); ok {
					*dst = &used
				}
			}(b.Name, &views[i].MemUsedMB)
		}
		if h.store == nil {
			continue
		}
		rs, err := h.store.ListBySandbox(b.Name)
		if err != nil {
			h.log.Warn("route list failed", "sandbox", b.Name, "err", err)
			continue
		}
		for _, rt := range rs {
			views[i].Routes = append(views[i].Routes, routeStatus{Subdomain: rt.Subdomain, Port: rt.Port, Visibility: rt.Visibility})
		}
		// Probe every forwarded port of a running sandbox concurrently; the
		// whole fan-out is bounded by one probe budget, not routes × timeout.
		// b, not the view, carries the address: the view's copy has had it
		// dropped on the way to the browser.
		if b.State != vmm.StateRunning || b.HostIP == "" {
			continue
		}
		for j := range views[i].Routes {
			wg.Add(1)
			go func(addr string, listening *bool) {
				defer wg.Done()
				*listening = h.listening(r.Context(), addr, remote)
			}(net.JoinHostPort(b.HostIP, strconv.Itoa(views[i].Routes[j].Port)), &views[i].Routes[j].Listening)
		}
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, views)
}

// listening is the shared port probe (webui.Probe) bound to this console's
// dialer and node.
func (h *Handler) listening(ctx context.Context, addr string, remote bool) bool {
	return h.probe.Listening(ctx, addr, remote)
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

// clusterResponse reports capacity as a list of nodes, which is why the shape
// survived the fleet arriving unchanged: a single-box deployment is the
// one-element case, not a different payload.
type clusterResponse struct {
	Domain string              `json:"domain"`
	Nodes  []host.NodeCapacity `json:"nodes"`
}

func (h *Handler) cluster(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, clusterResponse{
		Domain: h.domain,
		Nodes:  h.capacities(),
	})
}

func (h *Handler) pause(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.boxes.Pause(r.Context(), name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console paused sandbox", "name", name)
	box, _ := h.boxes.Get(name)
	writeJSON(w, http.StatusOK, webui.Public(box))
}

func (h *Handler) resume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	box, err := h.boxes.EnsureRunning(r.Context(), name)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console resumed sandbox", "name", name)
	writeJSON(w, http.StatusOK, webui.Public(box))
}

// destroy permanently removes a sandbox: its VM and local disk, and — when the
// box is archived — its rootfs object in storage (Manager.Destroy handles that
// cleanup). Routes, schedules, and tags are dropped with it. Irreversible, so
// the console gates it behind a confirmation modal.
func (h *Handler) destroy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.boxes.Destroy(r.Context(), name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console destroyed sandbox", "name", name)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// archive parks a sandbox's rootfs in object storage and frees its host disk.
// Restore is the resume action (EnsureRunning restores an archived sandbox
// transparently), so the UI reuses the resume button for archived rows.
func (h *Handler) archive(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.boxes.Archive(r.Context(), name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console archived sandbox", "name", name)
	box, _ := h.boxes.Get(name)
	writeJSON(w, http.StatusOK, webui.Public(box))
}

type snapshotReq struct {
	SnapshotName string `json:"snapshot_name"`
}

// snapshot captures a sandbox's current disk as a fork-able template (owned by
// the sandbox's owner).
func (h *Handler) snapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	box, ok := h.boxes.Get(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	var req snapshotReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	snap, err := h.boxes.Snapshot(r.Context(), name, req.SnapshotName, box.Owner)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console snapshot created", "sandbox", name, "snapshot", req.SnapshotName)
	writeJSON(w, http.StatusCreated, snap)
}

// listSnapshots is every owner's templates on THIS machine. A template is a
// reflink source in one machine's image directory and can only be forked where
// it lies, so there is no fleet-wide listing to ask for; a remote machine's
// templates are its operator's business until the fleet grows one.
func (h *Handler) listSnapshots(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.mgr.AllSnapshots())
}

type forkReq struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

// fork spins up a new sandbox from a snapshot. Owner comes from the request
// (the operator picks who owns the fork); the manager verifies the snapshot
// belongs to that owner.
func (h *Handler) fork(w http.ResponseWriter, r *http.Request) {
	var req forkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if req.Name == "" || req.Owner == "" {
		writeErr(w, http.StatusBadRequest, "name and owner are required")
		return
	}
	box, err := h.boxes.Fork(r.Context(), r.PathValue("snapshot"), req.Name, req.Owner, 0, 0)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console forked snapshot", "snapshot", r.PathValue("snapshot"), "into", req.Name)
	writeJSON(w, http.StatusCreated, webui.Public(box))
}

func (h *Handler) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	var req forkReq // reuse: only Owner is needed to scope the delete
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := h.boxes.DeleteSnapshot(r.Context(), r.PathValue("snapshot"), req.Owner); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console deleted snapshot", "snapshot", r.PathValue("snapshot"), "owner", req.Owner)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// pin marks a sandbox always-on and resumes it so its in-guest daemons start
// running immediately. unpin clears the flag, letting the reaper pause it again.
func (h *Handler) pin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.boxes.SetPinned(name, true); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	box, err := h.boxes.EnsureRunning(r.Context(), name)
	if err != nil {
		// The flag stuck; it just isn't warm yet. Surface the reason.
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console pinned sandbox", "name", name)
	writeJSON(w, http.StatusOK, webui.Public(box))
}

func (h *Handler) unpin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := h.boxes.SetPinned(name, false); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("console unpinned sandbox", "name", name)
	box, _ := h.boxes.Get(name)
	writeJSON(w, http.StatusOK, webui.Public(box))
}

func statusFor(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	switch {
	case contains(err.Error(), "not found"):
		return http.StatusNotFound
	case contains(err.Error(), "not enabled"):
		return http.StatusNotImplemented
	case contains(err.Error(), "pool full"):
		return http.StatusInsufficientStorage
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
