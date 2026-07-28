// Package userconsole is the self-service web UI for sandbox owners, served
// at my.<domain> through the proxy edge (so it rides the same wildcard TLS
// cert as the sandbox web routes). It is the owner-scoped sibling of the
// operator console (internal/console): same embedded single-page app pattern,
// but authenticated with the zone-wide edge session (cookie or Bearer) via
// edgeauth.Require, and every resource it touches is filtered to the session's
// handle.
//
// Owner scoping is deliberately unrevealing: acting on another owner's
// sandbox, route, snapshot, or secret answers exactly like a missing one — a
// 404 with the same body — so the API never confirms which names exist.
// Operators bypass the owner check (resolved once per request by the
// middleware). Mutations additionally pass the CSRF gate in RequireMutation:
// SameSite=Lax alone cannot fence off sandbox subdomains, which are same-site
// with the console.
package userconsole

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/domainmeta"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netrules"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/webui"
)

//go:embed index.html
var indexTemplate []byte

// indexPage is the console SPA composed against the shared design system,
// minified, and pre-gzipped once at package init — see internal/webui.
var indexPage = webui.Build(indexTemplate)

// Local names for the dashboard probe budgets, which are stated once in
// internal/webui so both consoles time a fleet the same way. probeTimeout also
// bounds the mem/CPU stat reads below; tunneledProbeTimeout is spent inside
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
// name reserved forever, and a rename or a fork would take a name no row ever
// recorded. Satisfied structurally by both *host.Manager and *fleet.Fleet
// (importing neither), and on a one-machine deployment the fleet is the
// manager, so the console behaves exactly as it did before it existed.
type Sandboxes interface {
	Get(name string) (*host.Sandbox, bool)
	ListByOwner(owner string) []*host.Sandbox
	EnsureReady(ctx context.Context, name string) (*host.Sandbox, error)
	Pause(ctx context.Context, name string) error
	Archive(ctx context.Context, name string) error
	Destroy(ctx context.Context, name string) error
	Reboot(ctx context.Context, name string) error
	SetTurbo(ctx context.Context, name string, on bool) error
	Rename(ctx context.Context, oldName, newName, owner string) error
	SetPinned(name string, pinned bool) error
	Snapshots(owner string) []*host.Snapshot
	Snapshot(ctx context.Context, box, snapName, owner string) (*host.Snapshot, error)
	DeleteSnapshot(ctx context.Context, snapName, owner string) error
	Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*host.Sandbox, error)
}

var _ Sandboxes = (*host.Manager)(nil)

// Action budgets copy the gateway's (internal/sshgw): pausing writes the
// guest's full memory snapshot, and archive/restore/snapshot move the whole
// rootfs — sized for the slowest plausible transfer, not a dial.
const (
	pauseTimeout   = 3 * time.Minute
	archiveTimeout = 15 * time.Minute
)

// OwnerSyncer re-pushes an owner's secret environment into their running
// sandboxes after a tag or secret mutation. Satisfied structurally by
// *envsync.Syncer (avoids importing it); nil disables change-time pushes,
// leaving the manager's push-on-EnsureRunning hook as the only reconciler.
type OwnerSyncer interface {
	SyncOwner(ctx context.Context, owner string)
}

// NetPlane is the egress plane as the console needs it: read one sandbox's
// meter, and re-push the whole fleet's policy after a rule or tag changes.
// *fleet.Fleet satisfies it.
//
// It is the FLEET rather than this machine's own netpush.Syncer because both
// operations are per-machine and the console does not know — must not need to
// know — which machine a sandbox is on. Reading the local syncer directly, as
// this used to, answered a VM on another machine with a panel of zeroes: the
// syncer looks up taps in the local manager's list, the name is not in it, and
// an absent tap is indistinguishable in the result from an idle one.
type NetPlane interface {
	NetUsage(ctx context.Context, name string) (netpush.VMUsage, error)
	PushNet(ctx context.Context) error
	// NetMetered reports whether the machine holding this sandbox meters at
	// all, so a caller can distinguish "no traffic" from "not measured".
	NetMetered(name string) bool
}

// Handler serves the user console UI and its JSON API.
type Handler struct {
	// mgr is this machine's own manager, kept for the handful of reads that are
	// genuinely about THIS machine (its node name, its capacity) rather than
	// about a sandbox.
	mgr *host.Manager
	// vitals answers the balloon and CPU reads. It defaults to mgr and is
	// pointed at the fleet by SetVitals: those counters can only be read on the
	// machine running the VM, but "which machine" is a question the fleet
	// answers, so they are routable after all — see webui.Probe.Vitals.
	vitals   webui.VitalsReader
	boxes    Sandboxes
	routes   *routes.Store            // optional: nil hides web routes and disables port/visibility
	secrets  *secrets.Store           // optional: nil disables tags + secrets endpoints
	netrules *netrules.Store          // optional: nil disables network-rule endpoints (501)
	netplane NetPlane                 // optional: nil disables bandwidth + policy push (501)
	favicons *domainmeta.FaviconCache // optional: nil serves the globe fallback
	accounts edgeauth.Accounts
	signer   *edgeauth.Signer
	syncer   OwnerSyncer
	domain   string // base zone, e.g. "hivemind.tools"
	// xtermSub is --xterm-subdomain, or "" when browser terminals are off.
	// Nothing here routes to it; it is handed to the SPA so the page can build
	// the <name>.<xtermSub>.<domain> link itself, and hide the Terminal button
	// entirely on a host that serves no terminals.
	xtermSub string
	secure   bool // set the Secure flag when clearing the session cookie
	log      *slog.Logger
	loginURL string // where unauthenticated browsers are sent
	origin   string // first-party Origin accepted by the CSRF gate

	// probe carries this machine's name and the fleet dialer: together they
	// decide which rows are remote and how long their port probes may take.
	probe webui.Probe
}

// New builds a user-console handler for <subdomain>.<domain> (subdomain is
// the --user-console-subdomain value, "my" by default; empty falls back to
// "my"). accounts resolves operator status (a *users.Store satisfies it),
// signer verifies the edge session, and syncer (nil-safe) propagates
// tag/secret changes to running sandboxes. xtermSub is --xterm-subdomain
// ("" when browser terminals are disabled). secure should be true when the
// proxy edge serves TLS.
func New(mgr *host.Manager, routeStore *routes.Store, secretsStore *secrets.Store,
	netrulesStore *netrules.Store, netPlane NetPlane, favicons *domainmeta.FaviconCache,
	accounts edgeauth.Accounts, signer *edgeauth.Signer, syncer OwnerSyncer,
	subdomain, domain, xtermSub string, secure bool, log *slog.Logger) *Handler {
	if subdomain == "" {
		subdomain = "my"
	}
	// A leading-dot --proxy-domain (".hivemind.tools") is tolerated by the
	// proxy and the login handler, which normalize it; normalize here too so
	// the logout cookie Domain, login URL, and CSRF origin match the ones
	// login built.
	domain = strings.TrimPrefix(domain, ".")
	h := &Handler{
		mgr: mgr, boxes: mgr, routes: routeStore, secrets: secretsStore,
		netrules: netrulesStore, netplane: netPlane, favicons: favicons,
		accounts: accounts, signer: signer, syncer: syncer,
		domain: domain, xtermSub: strings.Trim(xtermSub, "."), secure: secure, log: log,
		loginURL: "https://login." + domain + "/",
		origin:   "https://" + subdomain + "." + domain,
	}
	if mgr != nil {
		// Assigned inside the guard rather than in the literal above: a nil
		// *host.Manager stored in an interface is not a nil interface, so the
		// nil check webui.Probe.Vitals makes would pass and the first dashboard
		// load would panic in a lock.
		h.vitals = mgr
		h.probe.Node = mgr.NodeName()
	}
	return h
}

// SetSandboxes points the console's lifecycle actions and its listing at the
// fleet router rather than straight at this machine's manager, so an action
// reaches the machine that actually holds the sandbox and every name it takes
// or releases is recorded in the placement ledger. Unset, the console drives
// the manager it was built with — which is what a one-machine deployment wants
// and what every test builds.
func (h *Handler) SetSandboxes(s Sandboxes) { h.boxes = s }

// SetDialer routes the listening-port probe through d instead of dialing the
// guest's address on the host network — see webui.Probe.Dial for why a fleet
// cannot be probed directly.
func (h *Handler) SetDialer(d Dialer) { h.probe.Dial = d }

// SetVitals points the dashboard's memory and CPU reads at the fleet, which
// asks the machine holding each sandbox. Unset, the console reads the manager
// it was built with, which answers for its own VMs and reports nothing for
// anyone else's — right for one machine, and the reason a fleet must call this.
func (h *Handler) SetVitals(v webui.VitalsReader) { h.vitals = v }

func (h *Handler) Handler() http.Handler {
	auth := edgeauth.Require(h.signer, h.accounts, h.loginURL)
	csrf := edgeauth.RequireMutation(h.signer, h.accounts, h.loginURL, h.origin)
	require := func(f http.HandlerFunc) http.Handler { return auth(f) }
	mutate := func(f http.HandlerFunc) http.Handler { return csrf(f) }

	mux := http.NewServeMux()
	mux.Handle("GET /api/me", require(h.me))
	mux.Handle("POST /api/logout", mutate(h.logout))
	mux.Handle("GET /api/machines", require(h.machines))
	mux.Handle("POST /api/machines/{name}/pause", mutate(h.pause))
	mux.Handle("POST /api/machines/{name}/resume", mutate(h.resume))
	mux.Handle("DELETE /api/machines/{name}", mutate(h.destroy))
	mux.Handle("POST /api/machines/{name}/archive", mutate(h.archive))
	mux.Handle("POST /api/machines/{name}/pin", mutate(h.pin))
	mux.Handle("POST /api/machines/{name}/unpin", mutate(h.unpin))
	mux.Handle("POST /api/machines/{name}/snapshot", mutate(h.snapshot))
	mux.Handle("POST /api/machines/{name}/rename", mutate(h.rename))
	mux.Handle("POST /api/machines/{name}/reboot", mutate(h.reboot))
	mux.Handle("POST /api/machines/{name}/turbo", mutate(h.turbo))
	mux.Handle("POST /api/machines/{name}/port", mutate(h.setPort))
	mux.Handle("PUT /api/machines/{name}/tags", mutate(h.setTags))
	mux.Handle("POST /api/routes/{subdomain}/visibility", mutate(h.setVisibility))
	mux.Handle("GET /api/snapshots", require(h.listSnapshots))
	mux.Handle("POST /api/snapshots/{snapshot}/fork", mutate(h.fork))
	mux.Handle("POST /api/snapshots/{snapshot}/delete", mutate(h.deleteSnapshot))
	mux.Handle("GET /api/secrets", require(h.listSecrets))
	mux.Handle("PUT /api/secrets/{env_name}", mutate(h.putSecret))
	mux.Handle("DELETE /api/secrets/{env_name}", mutate(h.deleteSecret))
	mux.Handle("GET /api/network-rules", require(h.listNetRules))
	mux.Handle("PUT /api/network-rules/{name}", mutate(h.putNetRule))
	mux.Handle("DELETE /api/network-rules/{name}", mutate(h.deleteNetRule))
	mux.Handle("GET /api/machines/{name}/bandwidth", require(h.bandwidth))
	mux.Handle("GET /api/favicon", require(h.favicon))
	mux.HandleFunc("GET /", h.index)
	return mux
}

// index always serves the single-page app; the page itself calls the API and
// renders the sign-in state when that returns 401. The CSP keeps all resources
// first-party: favicons are served from /api/favicon (this origin), so no icon
// CDN needs whitelisting and the page can't be made to fetch a third party.
// 'unsafe-inline' is required because the SPA inlines its <style>/<script>
// (a strict nonce would mean reworking the webui minify pipeline).
func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
			"script-src 'self' 'unsafe-inline'; base-uri 'none'; form-action 'self'")
	indexPage.ServeHTTP(w, r)
}

type meResponse struct {
	Handle   string `json:"handle"`
	Email    string `json:"email,omitempty"`
	Operator bool   `json:"operator"`
	// TerminalSubdomain is the reserved label browser terminals are served
	// under; the SPA joins it with its own zone to link a machine's terminal.
	// Omitted when terminals are disabled, which is what hides the button —
	// a host with no --proxy or no --xterm-subdomain must not offer a link to
	// a name that resolves nowhere.
	TerminalSubdomain string `json:"terminal_subdomain,omitempty"`
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	sess, _ := edgeauth.From(r.Context())
	writeJSON(w, http.StatusOK, meResponse{
		Handle: sess.Handle, Email: sess.Email, Operator: sess.Operator,
		TerminalSubdomain: h.xtermSub,
	})
}

// logout clears the zone-wide session cookie (Domain "."+domain, matching how
// the login handler set it). The token itself stays valid until expiry —
// sessions are stateless — so this signs out this browser, nothing more.
func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	sess, _ := edgeauth.From(r.Context())
	http.SetCookie(w, &http.Cookie{
		Name: edgeauth.CookieName, Value: "", Path: "/",
		Domain: "." + h.domain, MaxAge: -1,
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode,
	})
	h.log.Info("user console logout", "handle", sess.Handle)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// routeStatus is a web route as shown in the UI: where it points, who may
// reach it, and whether a TCP dial to the forwarded port currently succeeds.
// Listening is only meaningful while the sandbox is running.
type routeStatus struct {
	Subdomain  string `json:"subdomain"`
	Port       int    `json:"port"`
	Visibility string `json:"visibility"`
	Listening  bool   `json:"listening"`
}

// sandboxView is a Sandbox plus its routes, tags, and live stats for the
// dashboard. CPUSeconds is cumulative host CPU time of the VM process — the
// SPA computes a percentage client-side from poll deltas ÷ vcpus.
// EnvUndecryptable surfaces the loud failure mode of key rotation: this
// sandbox's secret set exists but cannot be decrypted, and nothing was pushed.
type sandboxView struct {
	*host.Sandbox
	Routes           []routeStatus `json:"routes"`
	Tags             []string      `json:"tags"`
	MemUsedMB        *int64        `json:"mem_used_mb,omitempty"`
	CPUSeconds       *float64      `json:"cpu_seconds,omitempty"`
	EnvUndecryptable bool          `json:"env_undecryptable,omitempty"`
}

func (h *Handler) machines(w http.ResponseWriter, r *http.Request) {
	sess, _ := edgeauth.From(r.Context())
	boxes := h.boxes.ListByOwner(sess.Handle)
	views := make([]sandboxView, len(boxes))
	var wg sync.WaitGroup
	for i, b := range boxes {
		remote := h.probe.Remote(b)
		views[i] = sandboxView{Sandbox: webui.Public(b), Routes: []routeStatus{}, Tags: []string{}}
		if h.secrets != nil {
			if tags, err := h.secrets.TagsFor(b.Name); err != nil {
				h.log.Warn("tag list failed", "sandbox", b.Name, "err", err)
			} else if tags != nil {
				views[i].Tags = tags
			}
			// Values are computed only to detect the undecryptable state and are
			// discarded here — they never reach the response.
			if _, err := h.secrets.EnvForSandbox(b.Name, b.Owner); errors.Is(err, secrets.ErrUndecryptable) {
				views[i].EnvUndecryptable = true
			}
		}
		// Read the guest's real memory use and cumulative CPU time concurrently,
		// under the budget its placement deserves, so one slow VM can't stall
		// the dashboard. A sandbox on another machine is asked of the machine
		// running it — a balloon and a VMM process can only be asked there —
		// which is the whole reason this goes through the fleet rather than the
		// local manager it used to.
		if b.State == vmm.StateRunning {
			wg.Add(1)
			go func(box *host.Sandbox, mem **int64, cpu **float64) {
				defer wg.Done()
				v, err := h.probe.Vitals(r.Context(), h.vitals, box)
				if err != nil {
					h.log.Debug("vitals unavailable", "sandbox", box.Name, "node", box.Node, "err", err)
					return
				}
				*mem, *cpu = v.MemUsedMB, v.CPUSeconds
			}(b, &views[i].MemUsedMB, &views[i].CPUSeconds)
		}
		if h.routes == nil {
			continue
		}
		rs, err := h.routes.ListBySandbox(b.Name)
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

// ownedBox resolves the {name} path value to a sandbox the session may act
// on. ok=false covers both "does not exist" and "belongs to someone else" —
// callers answer with the identical not-found body so ownership is never
// leaked. Operators pass for any sandbox.
func (h *Handler) ownedBox(r *http.Request) (box *host.Sandbox, name string, ok bool) {
	name = r.PathValue("name")
	sess, _ := edgeauth.From(r.Context())
	box, found := h.boxes.Get(name)
	if !found || (box.Owner != sess.Handle && !sess.Operator) {
		return nil, name, false
	}
	return box, name, true
}

func notFoundBox(w http.ResponseWriter, name string) {
	writeErr(w, http.StatusNotFound, fmt.Sprintf("no sandbox named %q", name))
}

// handleFrom returns the session's handle for logging.
func handleFrom(r *http.Request) string {
	sess, _ := edgeauth.From(r.Context())
	return sess.Handle
}

func (h *Handler) pause(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pauseTimeout)
	defer cancel()
	if err := h.boxes.Pause(ctx, name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console paused sandbox", "name", name, "handle", handleFrom(r))
	box, _ := h.boxes.Get(name)
	writeJSON(w, http.StatusOK, webui.Public(box))
}

// resume also restores an archived sandbox (EnsureRunning folds restore in),
// hence the archive-sized budget. The owner check strictly precedes the
// EnsureRunning call, so a cross-owner probe can never wake a sandbox.
func (h *Handler) resume(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), archiveTimeout)
	defer cancel()
	box, err := h.boxes.EnsureReady(ctx, name)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console resumed sandbox", "name", name, "handle", handleFrom(r))
	writeJSON(w, http.StatusOK, webui.Public(box))
}

// destroy permanently removes a sandbox: its VM and local disk, and — when the
// box is archived — its rootfs object in storage (Manager.Destroy folds that
// cleanup in). Routes, schedules, and tags are dropped with it. Irreversible,
// so the console gates it behind a confirmation modal. The archive-sized budget
// covers the object-store delete round-trip on an archived box.
func (h *Handler) destroy(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), archiveTimeout)
	defer cancel()
	if err := h.boxes.Destroy(ctx, name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console destroyed sandbox", "name", name, "handle", handleFrom(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// archive parks a sandbox's rootfs in object storage and frees its host disk.
// 501 when archiving isn't enabled on this host (statusFor maps the manager's
// "not enabled" error).
func (h *Handler) archive(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), archiveTimeout)
	defer cancel()
	if err := h.boxes.Archive(ctx, name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console archived sandbox", "name", name, "handle", handleFrom(r))
	box, _ := h.boxes.Get(name)
	writeJSON(w, http.StatusOK, webui.Public(box))
}

// pin marks a sandbox always-on and resumes it so its in-guest daemons start
// running immediately. unpin clears the flag, letting the reaper pause it.
func (h *Handler) pin(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	if err := h.boxes.SetPinned(name, true); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), archiveTimeout)
	defer cancel()
	box, err := h.boxes.EnsureReady(ctx, name)
	if err != nil {
		// The flag stuck; it just isn't warm yet. Surface the reason.
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console pinned sandbox", "name", name, "handle", handleFrom(r))
	writeJSON(w, http.StatusOK, webui.Public(box))
}

func (h *Handler) unpin(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	if err := h.boxes.SetPinned(name, false); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console unpinned sandbox", "name", name, "handle", handleFrom(r))
	box, _ := h.boxes.Get(name)
	writeJSON(w, http.StatusOK, webui.Public(box))
}

type snapshotReq struct {
	SnapshotName string `json:"snapshot_name"`
}

// snapshot captures a sandbox's current disk as a fork-able template. The
// template is owned by the sandbox's owner (identical to the session handle
// except under operator bypass, where the owner keeps their own snapshot).
func (h *Handler) snapshot(w http.ResponseWriter, r *http.Request) {
	box, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	var req snapshotReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), archiveTimeout)
	defer cancel()
	snap, err := h.boxes.Snapshot(ctx, name, req.SnapshotName, box.Owner)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console snapshot created", "sandbox", name, "snapshot", req.SnapshotName, "handle", handleFrom(r))
	writeJSON(w, http.StatusCreated, snap)
}

type renameReq struct {
	NewName string `json:"new_name"`
}

// rename gives a sandbox a new name (and with it a new subdomain and SSH
// address). The manager auto-pauses a running sandbox and refuses archived
// ones; the guest's own hostname catches up on its next reboot.
func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	box, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	var req renameReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pauseTimeout)
	defer cancel()
	if err := h.boxes.Rename(ctx, name, req.NewName, box.Owner); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console renamed sandbox", "old", name, "new", req.NewName, "handle", handleFrom(r))
	box, _ = h.boxes.Get(req.NewName)
	writeJSON(w, http.StatusOK, webui.Public(box))
}

// reboot cold-restarts the guest — the only way already-running processes
// pick up a changed environment (new SSH sessions see it immediately).
func (h *Handler) reboot(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pauseTimeout)
	defer cancel()
	if err := h.boxes.Reboot(ctx, name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console rebooted sandbox", "name", name, "handle", handleFrom(r))
	box, _ := h.boxes.Get(name)
	writeJSON(w, http.StatusOK, webui.Public(box))
}

type turboReq struct {
	On bool `json:"on"`
}

// turbo restarts a machine with doubled CPU and RAM, or back at its own size.
// It is a cold boot — firecracker has no CPU hotplug and a balloon can only
// give memory back, never borrow more — so the guest's processes stop, which
// is what the console's confirmation says before it gets here.
//
// The extra allocation lasts one run: the manager hands it back on the next
// pause, idle reap included. It takes the same action budget as pause and
// reboot, which is what the round trip is: a pause plus a cold boot.
func (h *Handler) turbo(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	var req turboReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pauseTimeout)
	defer cancel()
	if err := h.boxes.SetTurbo(ctx, name, req.On); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console set turbo", "name", name, "on", req.On, "handle", handleFrom(r))
	box, _ := h.boxes.Get(name)
	writeJSON(w, http.StatusOK, webui.Public(box))
}

type portReq struct {
	Port int `json:"port"`
}

// setPort points the sandbox's default route (subdomain = name) at a new
// guest port. Upsert's ON CONFLICT updates only the port, so a route the
// owner made public stays public.
func (h *Handler) setPort(w http.ResponseWriter, r *http.Request) {
	box, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	if h.routes == nil {
		writeErr(w, http.StatusNotImplemented, "web routes are not enabled on this host")
		return
	}
	var req portReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := h.routes.Upsert(routes.Route{Subdomain: name, Sandbox: name, Owner: box.Owner, Port: req.Port}); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console changed route port", "sandbox", name, "port", req.Port, "handle", handleFrom(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type tagsReq struct {
	Tags []string `json:"tags"`
}

// setTags replaces the sandbox's tag set and re-pushes the owner's secret
// environment — removal included: an emptied tag set clears the pushed block.
func (h *Handler) setTags(w http.ResponseWriter, r *http.Request) {
	box, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	if h.secrets == nil {
		writeErr(w, http.StatusNotImplemented, "tags are not enabled on this host")
		return
	}
	var req tagsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if err := h.secrets.SetTags(name, box.Owner, req.Tags); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console set tags", "sandbox", name, "tags", len(req.Tags), "handle", handleFrom(r))
	h.syncOwner(r.Context(), box.Owner)
	h.pushNet() // tags govern network rules too: re-push this VM's egress policy
	tags, err := h.secrets.TagsFor(name)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if tags == nil {
		tags = []string{}
	}
	writeJSON(w, http.StatusOK, map[string][]string{"tags": tags})
}

type visibilityReq struct {
	Visibility string `json:"visibility"`
}

// setVisibility flips one route between public and private — finer-grained
// than `ctl@ share`, which flips every route of a sandbox together.
func (h *Handler) setVisibility(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("subdomain")
	sess, _ := edgeauth.From(r.Context())
	if h.routes == nil {
		writeErr(w, http.StatusNotImplemented, "web routes are not enabled on this host")
		return
	}
	var req visibilityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if !routes.ValidVisibility(req.Visibility) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("visibility must be %q or %q", routes.VisibilityPublic, routes.VisibilityPrivate))
		return
	}
	rt, found, err := h.routes.GetBySubdomain(sub)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found || (rt.Owner != sess.Handle && !sess.Operator) {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("no route named %q", sub))
		return
	}
	if err := h.routes.SetVisibility(sub, req.Visibility); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console changed route visibility", "subdomain", sub, "visibility", req.Visibility, "handle", sess.Handle)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// listSnapshots lists the session's own snapshots — owner-scoped, unlike the
// operator console's AllSnapshots.
func (h *Handler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	snaps := h.boxes.Snapshots(handleFrom(r))
	if snaps == nil {
		snaps = []*host.Snapshot{}
	}
	writeJSON(w, http.StatusOK, snaps)
}

type forkReq struct {
	Name string `json:"name"`
}

// fork spins up a new sandbox from one of the session's snapshots. Owner is
// always the session handle, never request data, so a cross-owner snapshot
// name simply doesn't resolve (the manager's "not found" → 404).
func (h *Handler) fork(w http.ResponseWriter, r *http.Request) {
	var req forkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	snap := r.PathValue("snapshot")
	ctx, cancel := context.WithTimeout(r.Context(), pauseTimeout)
	defer cancel()
	box, err := h.boxes.Fork(ctx, snap, req.Name, handleFrom(r), 0, 0)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console forked snapshot", "snapshot", snap, "into", req.Name, "handle", handleFrom(r))
	writeJSON(w, http.StatusCreated, webui.Public(box))
}

func (h *Handler) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	snap := r.PathValue("snapshot")
	ctx, cancel := context.WithTimeout(r.Context(), pauseTimeout)
	defer cancel()
	if err := h.boxes.DeleteSnapshot(ctx, snap, handleFrom(r)); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console deleted snapshot", "snapshot", snap, "handle", handleFrom(r))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// listSecrets returns the session's secrets as metadata only. There is no
// value-read endpoint anywhere on this API: values are write-only and only
// ever decrypted for delivery into a sandbox.
func (h *Handler) listSecrets(w http.ResponseWriter, r *http.Request) {
	if h.secrets == nil {
		writeErr(w, http.StatusNotImplemented, "secrets are not enabled on this host")
		return
	}
	metas, err := h.secrets.ListSecrets(handleFrom(r))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if metas == nil {
		metas = []secrets.SecretMeta{}
	}
	writeJSON(w, http.StatusOK, metas)
}

type secretReq struct {
	Value string   `json:"value"`
	Tags  []string `json:"tags"`
}

// putSecret creates or updates a secret and re-pushes the owner's running
// sandboxes. The value is never echoed back and never logged.
func (h *Handler) putSecret(w http.ResponseWriter, r *http.Request) {
	if h.secrets == nil {
		writeErr(w, http.StatusNotImplemented, "secrets are not enabled on this host")
		return
	}
	envName := r.PathValue("env_name")
	var req secretReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	handle := handleFrom(r)
	if err := h.secrets.PutSecret(handle, envName, req.Value, req.Tags); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console saved secret", "env", envName, "handle", handle)
	h.syncOwner(r.Context(), handle)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) deleteSecret(w http.ResponseWriter, r *http.Request) {
	if h.secrets == nil {
		writeErr(w, http.StatusNotImplemented, "secrets are not enabled on this host")
		return
	}
	envName := r.PathValue("env_name")
	handle := handleFrom(r)
	if err := h.secrets.DeleteSecret(handle, envName); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console deleted secret", "env", envName, "handle", handle)
	h.syncOwner(r.Context(), handle)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// listNetRules returns the session's network rule-sets in full (patterns +
// tags) — unlike secrets, allow patterns are policy, not sensitive.
func (h *Handler) listNetRules(w http.ResponseWriter, r *http.Request) {
	if h.netrules == nil {
		writeErr(w, http.StatusNotImplemented, "network rules are not enabled on this host")
		return
	}
	rules, err := h.netrules.ListRules(handleFrom(r))
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	if rules == nil {
		rules = []netrules.RuleMeta{}
	}
	writeJSON(w, http.StatusOK, rules)
}

type netRuleReq struct {
	Allow []string `json:"allow"`
	Tags  []string `json:"tags"`
}

// putNetRule creates or updates a rule-set and re-pushes egress policy so any
// running VM carrying one of its tags picks up the change.
func (h *Handler) putNetRule(w http.ResponseWriter, r *http.Request) {
	if h.netrules == nil {
		writeErr(w, http.StatusNotImplemented, "network rules are not enabled on this host")
		return
	}
	name := r.PathValue("name")
	var req netRuleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	handle := handleFrom(r)
	if err := h.netrules.PutRule(handle, name, netrules.RuleSpec{Allow: req.Allow}, req.Tags); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console saved network rule", "name", name, "tags", len(req.Tags), "handle", handle)
	h.pushNet()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) deleteNetRule(w http.ResponseWriter, r *http.Request) {
	if h.netrules == nil {
		writeErr(w, http.StatusNotImplemented, "network rules are not enabled on this host")
		return
	}
	name := r.PathValue("name")
	handle := handleFrom(r)
	if err := h.netrules.DeleteRule(handle, name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console deleted network rule", "name", name, "handle", handle)
	h.pushNet()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// bwDomain is one destination's bandwidth, enriched with a display name so the
// SPA can render a Ubiquiti-style row without re-deriving it client-side.
type bwDomain struct {
	Domain   string `json:"domain"`
	Display  string `json:"display"`
	Resolved bool   `json:"resolved"`
	TxBytes  uint64 `json:"tx_bytes"`
	RxBytes  uint64 `json:"rx_bytes"`
	Total    uint64 `json:"total"`
}

type bwResponse struct {
	Name    string     `json:"name"`
	TxBytes uint64     `json:"tx_bytes"`
	RxBytes uint64     `json:"rx_bytes"`
	Domains []bwDomain `json:"domains"`
}

// bandwidth returns one VM's per-domain egress breakdown from sluice, already
// sorted by total bytes and labelled with display names. Owner-scoped via
// ownedBox, so a cross-owner name 404s like any other.
func (h *Handler) bandwidth(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	if h.netplane == nil {
		writeErr(w, http.StatusNotImplemented, "network metering is not enabled on this host")
		return
	}
	// Routed by name, so a sandbox on a fleet node is answered by that node's
	// own meter. A machine that runs no sluice refuses with a typed
	// KindDisabled, which statusFor turns back into the 501 this endpoint has
	// always answered — but now it is a statement about the machine holding
	// THIS sandbox rather than about the gateway rendering the page.
	u, err := h.netplane.NetUsage(r.Context(), name)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	resp := bwResponse{
		Name: name, TxBytes: u.TxBytes, RxBytes: u.RxBytes, Domains: []bwDomain{},
	}
	for _, d := range u.Domains {
		resp.Domains = append(resp.Domains, bwDomain{
			Domain:   d.Domain,
			Display:  domainmeta.DisplayName(d.Domain),
			Resolved: d.Resolved,
			TxBytes:  d.TxBytes,
			RxBytes:  d.RxBytes,
			Total:    d.TxBytes + d.RxBytes,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// favicon serves a domain's icon from the host-side cache, always 200 (the
// neutral globe on any miss) so the SPA's <img> never breaks. Cached hard at
// the browser since a site's icon rarely changes.
func (h *Handler) favicon(w http.ResponseWriter, r *http.Request) {
	domain := r.URL.Query().Get("domain")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if h.favicons == nil || domain == "" {
		data, ct := domainmeta.GlobeSVG()
		w.Header().Set("Content-Type", ct)
		w.Write(data) //nolint:errcheck
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data, ct, _ := h.favicons.Get(ctx, domain)
	w.Header().Set("Content-Type", ct)
	w.Write(data) //nolint:errcheck
}

// syncOwner fires the change-time env push. Nil-safe; the syncer itself is
// async and best-effort, so this never delays or fails the response.
func (h *Handler) syncOwner(ctx context.Context, owner string) {
	if h.syncer == nil {
		return
	}
	h.syncer.SyncOwner(ctx, owner)
}

// pushNet re-pushes the whole fleet's egress policy to sluice after a rule or
// tag change. Best-effort and async on a fresh context so it never delays or
// fails the response; the syncer sends a full snapshot, so a single push
// reconciles every affected VM.
func (h *Handler) pushNet() {
	if h.netplane == nil {
		return
	}
	go func() {
		// Longer than the old ten seconds because this now fans out over every
		// machine in the fleet, each of which may be a link across a network
		// rather than a unix socket. It is still bounded, and still async, so a
		// slow node delays nothing the user is waiting on.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.netplane.PushNet(ctx); err != nil {
			h.log.Warn("push egress policy", "err", err)
		}
	}()
}

// statusFor maps store/manager errors onto HTTP statuses by their sentinel or
// message, per the local-copy convention (internal/console has its own).
func statusFor(err error) int {
	// A typed error already carries its status, and the ones that reach here
	// from another machine are rebuilt from the wire with their Kind intact —
	// so classifying them by their own mapping beats re-deriving one from the
	// sentence, which is what the string matching below has to do for the
	// errors that predate ctlops.
	var typed *ctlops.Error
	if errors.As(err, &typed) {
		return typed.HTTPStatus()
	}
	switch {
	case err == nil:
		return http.StatusInternalServerError
	case errors.Is(err, secrets.ErrNoSuchSecret), errors.Is(err, routes.ErrNoSuchRoute),
		errors.Is(err, netrules.ErrNoSuchRule):
		return http.StatusNotFound
	case errors.Is(err, netrules.ErrInvalidRule):
		return http.StatusBadRequest
	case errors.Is(err, routes.ErrSubdomainTaken):
		return http.StatusConflict
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		return http.StatusNotFound
	case strings.Contains(msg, "not enabled"), strings.Contains(msg, "not supported"):
		return http.StatusNotImplemented
	case strings.Contains(msg, "pool full"):
		return http.StatusInsufficientStorage
	case strings.Contains(msg, "invalid"), strings.Contains(msg, "exceeds"),
		strings.Contains(msg, "cannot be an env var"):
		return http.StatusBadRequest
	case strings.Contains(msg, "already exists"), strings.Contains(msg, "already taken"),
		strings.Contains(msg, "is reserved"):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
