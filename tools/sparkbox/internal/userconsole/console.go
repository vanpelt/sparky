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
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/domainmeta"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghapp"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ghuser"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netrules"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/templates"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/webui"
)

//go:embed index.html
var indexTemplate []byte

//go:embed sparkbox-logo.png
var sparkboxLogoPNG []byte

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
	// CapacityForOwner is the footprint card's numbers. It belongs on this
	// interface rather than beside it because it has to be the FLEET's answer
	// for the same reason every other method here does: an owner's disk and
	// memory are charged by the machine holding each VM, and a rollup taken
	// from the local manager alone would report a person with three sandboxes
	// on a node as using nothing at all.
	CapacityForOwner(owner string) host.OwnerCapacity
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
	repos    *repos.Store             // optional: nil disables repo endpoints (501)
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
	// launchSub is the reserved launch-link label. Empty hides environment
	// launch controls, just as an empty xtermSub hides terminal controls.
	launchSub string
	secure    bool // set the Secure flag when clearing the session cookie
	log       *slog.Logger
	loginURL  string // where unauthenticated browsers are sent
	origin    string // first-party Origin accepted by the CSRF gate

	// probe carries this machine's name and the fleet dialer: together they
	// decide which rows are remote and how long their port probes may take.
	probe webui.Probe

	// app answers "is the GitHub App installed on that repository", the one
	// fact about an attachment that this host does not own and cannot infer.
	// Optional: nil renders every row's App state as appOff, which is the
	// honest answer on a host configured with no App — attachment still works,
	// public repos still clone, and nothing here pretends to know more.
	// Set by SetGitHubApp, not by New, because it arrives from a fleet secret
	// that may be absent.
	app *ghapp.App
	// userAuth is the encrypted per-repository grant manager shared with the
	// VM device flow. A client secret enables its browser OAuth front door;
	// without one, the VM command still works and this panel shows bot fallback.
	userAuth *ghuser.Manager
	// appMu guards the probe cache below.
	appMu sync.Mutex
	// appSeen remembers what the App last said about one account's attachment,
	// keyed by probeKey. See appState for why this cache exists at all.
	appSeen map[string]appProbe
	// appBusy is the set of keys with a probe already in flight, so a poll that
	// arrives mid-probe does not start a second one.
	appBusy map[string]bool

	// binds answers which of the session's tags boot from which snapshot, for
	// the Snapshots panel's read-only Tags column. Optional: nil is a host with
	// no binding store, where the column is simply empty. Set by
	// SetTemplateTags.
	binds TemplateTags

	// envs is the control plane, and it is the only store-shaped field here
	// that is not a store: an environment composes five of them under an
	// ordering rule, so this panel goes through ctlops rather than reaching
	// past it. Optional: nil answers 501 from every environment route. Set by
	// SetEnvironments. See environments.go.
	envs Environments
}

// TemplateTags is the one question this console asks of the tag-to-base-image
// store: which of an owner's tags boot from which of their snapshots.
//
// *templates.Store satisfies it. Both methods are READS and that is the whole
// reason the seam is this narrow: no bind, unbind or port write lives here on
// purpose, because those are ownership-checked writes that belong in
// internal/ctlops, and this handler does not go through ctlops at all (see
// listSnapshots and deleteSnapshot, which call the manager directly). Adding
// one here would mean a second authorization path for the same operation,
// which is the thing this package must not grow.
type TemplateTags interface {
	BindingsForOwner(owner string) ([]templates.Binding, error)
	// SnapshotPorts is the other column: the default port each template was
	// captured on, for the snapshots that carry one.
	SnapshotPorts(owner string) (map[string]int, error)
}

// New builds a user-console handler for <subdomain>.<domain> (subdomain is
// the --user-console-subdomain value, "my" by default; empty falls back to
// "my"). accounts resolves operator status (a *users.Store satisfies it),
// signer verifies the edge session, and syncer (nil-safe) propagates
// tag/secret changes to running sandboxes. xtermSub is --xterm-subdomain
// ("" when browser terminals are disabled). secure should be true when the
// proxy edge serves TLS.
func New(mgr *host.Manager, routeStore *routes.Store, secretsStore *secrets.Store,
	netrulesStore *netrules.Store, reposStore *repos.Store, netPlane NetPlane,
	favicons *domainmeta.FaviconCache,
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
		netrules: netrulesStore, repos: reposStore, netplane: netPlane, favicons: favicons,
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

// SetGitHubApp gives the Repos panel the App it asks whether an attachment is
// actually reachable. It is a seam rather than a New parameter because the App
// is built from a fleet secret that is routinely absent: a host with no key
// still stores attachments, still clones public repositories, and must still
// serve this console — it simply stops claiming to know what github.com thinks.
func (h *Handler) SetGitHubApp(a *ghapp.App) { h.app = a }

// SetGitHubUserAuth enables the Repos panel's browser authorization controls.
// The manager remains optional because device-only and bot-only deployments
// are both supported configurations.
func (h *Handler) SetGitHubUserAuth(a *ghuser.Manager) { h.userAuth = a }

// SetLaunchSubdomain gives ready environment cards their go.<domain> link.
// The launch service remains the authority for ownership and repo membership;
// this value only controls whether the SPA can render a route to that service.
func (h *Handler) SetLaunchSubdomain(sub string) { h.launchSub = strings.Trim(sub, ".") }

// SetTemplateTags gives the Snapshots panel its Tags column. It is a seam for
// the same reason SetGitHubApp is: a host with no binding store still serves
// this console, still lists snapshots and still forks them — it simply has no
// bindings to show.
//
// It grants no writes. Binding and unbinding are ctlops operations reached over
// ssh and REST; nothing on this console may point a tag at a snapshot, and the
// panel says so by having no control for it.
func (h *Handler) SetTemplateTags(t TemplateTags) { h.binds = t }

func (h *Handler) Handler() http.Handler {
	auth := edgeauth.Require(h.signer, h.accounts, h.loginURL)
	csrf := edgeauth.RequireMutation(h.signer, h.accounts, h.loginURL, h.origin)
	require := func(f http.HandlerFunc) http.Handler { return auth(f) }
	mutate := func(f http.HandlerFunc) http.Handler { return csrf(f) }

	mux := http.NewServeMux()
	mux.Handle("GET /api/me", require(h.me))
	mux.Handle("POST /api/logout", mutate(h.logout))
	mux.Handle("GET /api/machines", require(h.machines))
	mux.Handle("GET /api/usage", require(h.usage))
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
	mux.Handle("DELETE /api/routes/{subdomain}/ports/{port}", mutate(h.forgetPort))
	mux.Handle("GET /api/snapshots", require(h.listSnapshots))
	mux.Handle("POST /api/snapshots/{snapshot}/fork", mutate(h.fork))
	mux.Handle("POST /api/snapshots/{snapshot}/delete", mutate(h.deleteSnapshot))
	mux.Handle("GET /api/secrets", require(h.listSecrets))
	mux.Handle("PUT /api/secrets/{env_name}", mutate(h.putSecret))
	mux.Handle("DELETE /api/secrets/{env_name}", mutate(h.deleteSecret))
	mux.Handle("GET /api/network-rules", require(h.listNetRules))
	mux.Handle("PUT /api/network-rules/{name}", mutate(h.putNetRule))
	mux.Handle("DELETE /api/network-rules/{name}", mutate(h.deleteNetRule))
	mux.Handle("GET /api/environments", require(h.listEnvironments))
	mux.Handle("PUT /api/environments/{name}", mutate(h.putEnvironment))
	mux.Handle("DELETE /api/environments/{name}", mutate(h.deleteEnvironment))
	mux.Handle("GET /api/environments/{name}/script", require(h.getEnvScript))
	mux.Handle("PUT /api/environments/{name}/script", mutate(h.putEnvScript))
	mux.Handle("POST /api/environments/{name}/build", mutate(h.buildEnvironment))
	mux.Handle("POST /api/environments/{name}/capture", mutate(h.captureEnvironment))
	mux.Handle("GET /api/repos", require(h.listRepos))
	mux.Handle("POST /api/repos/{slug}/authorize", mutate(h.authorizeRepo))
	mux.Handle("PUT /api/repos/{slug}", mutate(h.putRepo))
	mux.Handle("DELETE /api/repos/{slug}", mutate(h.deleteRepo))
	mux.Handle("GET /github/repo/callback", require(h.githubRepoCallback))
	mux.Handle("GET /api/machines/{name}/bandwidth", require(h.bandwidth))
	mux.Handle("GET /api/favicon", require(h.favicon))
	mux.HandleFunc("GET /sparkbox-logo.png", h.logo)
	mux.HandleFunc("GET /", h.index)
	return mux
}

func (h *Handler) logo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "sparkbox-logo.png", time.Time{}, bytes.NewReader(sparkboxLogoPNG))
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
	// LaunchSubdomain is the reserved go-service label used by ready
	// environment cards. Omitted when launch links are disabled.
	LaunchSubdomain string `json:"launch_subdomain,omitempty"`
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	sess, _ := edgeauth.From(r.Context())
	writeJSON(w, http.StatusOK, meResponse{
		Handle: sess.Handle, Email: sess.Email, Operator: sess.Operator,
		TerminalSubdomain: h.xtermSub,
		LaunchSubdomain:   h.launchSub,
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

// routeStatus is one addressable PORT as shown in the UI — not one route row.
// Visibility is settled per port, so a route contributes its own port (Default,
// reached at the portless URL) plus an entry for every other port under the
// same hostname: ones the owner configured, and ones the host's scan found
// listening. Listening is only meaningful while the sandbox is running.
type routeStatus struct {
	Subdomain  string `json:"subdomain"`
	Port       int    `json:"port"`
	Visibility string `json:"visibility"`
	Listening  bool   `json:"listening"`
	// Default marks the route's own port: the portless URL, which the strip
	// pins to the front because it is the URL people actually have.
	Default bool `json:"default,omitempty"`
	// Pinned marks a port the owner has an opinion about, as opposed to one
	// that merely turned up in a scan. It is what keeps a port on the strip
	// when nothing is listening, and only a pinned port can be forgotten.
	Pinned bool `json:"pinned,omitempty"`
	// Service is the small display name the host's port scan read off the
	// listener's own HTTP response ("Vite", "Jupyter"), when it found one.
	Service string `json:"service,omitempty"`
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
	// The HiveMind reading, from the same vitals reply the meters come from and
	// therefore free. It is what lets the Environments tab link a build in
	// flight to the transcript of the agent running it: the builder is an
	// ordinary sandbox in this very list, so the panel finds it by name rather
	// than asking the control plane a second question about it.
	//
	// Absent — not empty — on a host with no --hivemind-api and on a machine
	// that has never heard from HiveMind about this sandbox. "No session" and
	// "nobody asks" render identically here (no link), which is right for a
	// dashboard and is why nothing branches on the difference.
	HiveMindSessionURL   string `json:"hivemind_session_url,omitempty"`
	HiveMindSessionTitle string `json:"hivemind_session_title,omitempty"`
	HiveMindActive       bool   `json:"hivemind_active,omitempty"`
}

func (h *Handler) machines(w http.ResponseWriter, r *http.Request) {
	sess, _ := edgeauth.From(r.Context())
	boxes := h.boxes.ListByOwner(sess.Handle)
	// The console is an activity view, so put the machine the user touched most
	// recently first. The stores deliberately keep their general-purpose list
	// methods name-sorted; this presentation-specific order belongs here.
	sort.SliceStable(boxes, func(i, j int) bool {
		if boxes[i].LastActive.Equal(boxes[j].LastActive) {
			return boxes[i].Name < boxes[j].Name
		}
		return boxes[i].LastActive.After(boxes[j].LastActive)
	})
	views := make([]sandboxView, len(boxes))
	// scans holds each running box's port scan, filled by the vitals read below
	// and consumed after the wait. The scan is why the strip can show ports
	// nobody configured: the host already probes the supported HTTPS ports for
	// the terminal's menu, so listing them here costs one field, not a fan-out.
	scans := make([]host.Vitals, len(boxes))
	var wg sync.WaitGroup
	for i, b := range boxes {
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
		// Read the guest's non-available memory and cumulative CPU time concurrently,
		// under the budget its placement deserves, so one slow VM can't stall
		// the dashboard. A sandbox on another machine is asked of the machine
		// running it — a balloon and a VMM process can only be asked there —
		// which is the whole reason this goes through the fleet rather than the
		// local manager it used to.
		if b.State == vmm.StateRunning {
			wg.Add(1)
			go func(box *host.Sandbox, view *sandboxView, scan *host.Vitals) {
				defer wg.Done()
				v, err := h.probe.Vitals(r.Context(), h.vitals, box)
				if err != nil {
					h.log.Debug("vitals unavailable", "sandbox", box.Name, "node", box.Node, "err", err)
					return
				}
				view.MemUsedMB, view.CPUSeconds = v.MemUsedMB, v.CPUSeconds
				*scan = v
				if hm := v.HiveMind; hm != nil {
					// SessionLink, because a node is a separate trust domain
					// and this is the one field in the reply that becomes an
					// href. The title is painted with textContent by the page.
					view.HiveMindSessionURL = webui.SessionLink(hm.SessionURL)
					view.HiveMindSessionTitle = hm.SessionTitle
					view.HiveMindActive = hm.Presence.Live()
				}
			}(b, &views[i], &scans[i])
		}
	}
	wg.Wait()

	// Second wave, once the scans are in: turn each sandbox's routes, its
	// configured extra ports and whatever the scan found listening into one
	// ordered strip, then dial the configured ports the scan does not cover.
	if h.routes == nil {
		writeJSON(w, http.StatusOK, views)
		return
	}
	var wg2 sync.WaitGroup
	for i, b := range boxes {
		views[i].Routes = h.portStrip(b, scans[i])
		// Probe every configured port of a running sandbox concurrently; the
		// whole fan-out is bounded by one probe budget, not ports × timeout.
		// b, not the view, carries the address: the view's copy has had it
		// dropped on the way to the browser.
		if b.State != vmm.StateRunning || b.HostIP == "" {
			continue
		}
		remote := h.probe.Remote(b)
		for j := range views[i].Routes {
			if views[i].Routes[j].Listening {
				continue // the scan already answered for this one
			}
			wg2.Add(1)
			go func(addr string, listening *bool) {
				defer wg2.Done()
				*listening = h.listening(r.Context(), addr, remote)
			}(net.JoinHostPort(b.HostIP, strconv.Itoa(views[i].Routes[j].Port)), &views[i].Routes[j].Listening)
		}
	}
	wg2.Wait()
	writeJSON(w, http.StatusOK, views)
}

// portStrip is every port of one sandbox the console can address, in the order
// the page draws them: the default hostname's own port first — it is the URL
// people have — then the rest of that hostname's ports by number, then any
// other route rows with their own ports.
//
// Three sources feed it and they mean different things. A route row is a
// hostname and the port it forwards to. A route_ports row is an opinion the
// owner recorded about another port, which is what holds a port on the strip
// while nothing is listening on it. The scan is what is listening right now,
// which is how a dev server that just came up appears without being configured
// at all — private, like every port nobody has said anything about.
func (h *Handler) portStrip(b *host.Sandbox, scan host.Vitals) []routeStatus {
	rs, err := h.routes.ListBySandbox(b.Name)
	if err != nil {
		h.log.Warn("route list failed", "sandbox", b.Name, "err", err)
		return []routeStatus{}
	}
	if len(rs) == 0 {
		return []routeStatus{}
	}
	pinned, err := h.routes.ListPortsBySandbox(b.Name)
	if err != nil {
		h.log.Warn("route port list failed", "sandbox", b.Name, "err", err)
	}
	// The default hostname is the one named after the sandbox; a box whose
	// route was renamed out from under it falls back to its first row, which
	// is also the only row it has.
	sort.SliceStable(rs, func(i, j int) bool {
		if (rs[i].Subdomain == b.Name) != (rs[j].Subdomain == b.Name) {
			return rs[i].Subdomain == b.Name
		}
		return rs[i].Subdomain < rs[j].Subdomain
	})
	def := rs[0]

	names := make(map[int]string, len(scan.PortServices))
	for _, svc := range scan.PortServices {
		names[svc.Port] = svc.Name
	}
	live := make(map[int]bool, len(scan.ListeningPorts))
	for _, p := range scan.ListeningPorts {
		live[p] = true
	}

	out := make([]routeStatus, 0, len(rs)+len(pinned)+len(scan.ListeningPorts))
	entry := func(sub string, port int, vis string, isDefault, isPinned bool) routeStatus {
		return routeStatus{
			Subdomain: sub, Port: port, Visibility: vis,
			Default: isDefault, Pinned: isPinned,
			Listening: live[port], Service: names[port],
		}
	}
	// A route's own port is never "pinned": pinned means there is a
	// route_ports row behind it, which is exactly the set of entries that can
	// be forgotten. A route's port is the route's, and goes with it.
	out = append(out, entry(def.Subdomain, def.Port, def.Visibility, true, false))

	// The default hostname's other ports: everything configured under it, plus
	// everything the scan found, minus the default port itself. Sorted by
	// number so the strip does not reshuffle as services come and go.
	extra := map[int]string{} // port -> visibility, "" for scan-only
	for _, p := range pinned {
		if p.Subdomain == def.Subdomain && p.Port != def.Port {
			extra[p.Port] = p.Visibility
		}
	}
	for _, p := range scan.ListeningPorts {
		if p != def.Port {
			if _, ok := extra[p]; !ok {
				extra[p] = ""
			}
		}
	}
	ports := make([]int, 0, len(extra))
	for p := range extra {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	for _, p := range ports {
		vis, isPinned := extra[p], extra[p] != ""
		if !isPinned {
			vis = routes.VisibilityPrivate
		}
		out = append(out, entry(def.Subdomain, p, vis, false, isPinned))
	}

	// Any other hostname pointed at this sandbox, with its own ports.
	for _, rt := range rs[1:] {
		out = append(out, entry(rt.Subdomain, rt.Port, rt.Visibility, false, false))
		for _, p := range pinned {
			if p.Subdomain == rt.Subdomain && p.Port != rt.Port {
				out = append(out, entry(rt.Subdomain, p.Port, p.Visibility, false, true))
			}
		}
	}
	return out
}

// usageView is the Machines tab's footprint card: what this owner's sandboxes
// cost, as opposed to what they were provisioned.
//
// It is a separate request from the machine list rather than a field on it
// because the two answer different questions and the page combines them. The
// list is per-sandbox and carries the live balloon reading for each running VM,
// which is the honest memory number and is already routed to the machine
// holding each one. This is the pooled arithmetic no browser can do: the
// reflink baseline that turns six 25 GB disks into the three gigabytes actually
// written, and the owner pool budgets, which are the node configuration and
// appear in no per-sandbox record.
type usageView struct {
	host.OwnerCapacity
	// SharedDiskMB is the sharing dividend spelled out: template blocks this
	// owner sandboxes read but never paid for, because a fork reflinks them
	// instead of copying. Derived from the pair above it so the page does not
	// have to know which way round the subtraction goes.
	SharedDiskMB int64 `json:"shared_disk_mb"`
}

// usage answers the footprint card. Owner-scoped like everything else here:
// the session handle is the only input, so there is nothing to authorize
// beyond having a session, and an operator sees their own footprint rather
// than a privileged view of somebody else.
func (h *Handler) usage(w http.ResponseWriter, r *http.Request) {
	sess, _ := edgeauth.From(r.Context())
	c := h.boxes.CapacityForOwner(sess.Handle)
	shared := c.RawDiskMB - c.UsedDiskMB
	if shared < 0 {
		// A baseline larger than the disk measured against it, which
		// pooledDiskMB already floors at zero per sandbox. Belt and braces:
		// this is a subtraction of two independently sampled figures and a
		// negative "you saved" would be nonsense on a page.
		shared = 0
	}
	writeJSON(w, http.StatusOK, usageView{OwnerCapacity: c, SharedDiskMB: shared})
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
	h.pushNet()       // tags govern network rules too: re-push this VM's egress policy
	h.syncRepos(name) // and repos: a tag decides which repositories land in this box
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
	// Port names one guest port under this hostname. Zero means the route's
	// own port — the portless URL — which is what this endpoint has always
	// meant and what a client that predates per-port visibility sends.
	Port int `json:"port,omitempty"`
}

// setVisibility opens or closes ONE port of one hostname. It is the console's
// finest-grained visibility control: `ctl@ share` can speak for a whole
// sandbox, this always names exactly what it changes.
//
// Setting a port private is not the same as forgetting it. The store keeps the
// row either way, and the row is what holds the port on the strip so it can be
// pre-authorised before anything is listening on it; forgetPort removes it.
func (h *Handler) setVisibility(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("subdomain")
	sess, _ := edgeauth.From(r.Context())
	var req visibilityReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	if !routes.ValidVisibility(req.Visibility) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("visibility must be %q or %q", routes.VisibilityPublic, routes.VisibilityPrivate))
		return
	}
	rt, ok := h.ownedRoute(w, r, sub)
	if !ok {
		return
	}
	port := req.Port
	if port == 0 {
		port = rt.Port
	}
	if port < 1 || port > 65535 {
		writeErr(w, http.StatusBadRequest, "port must be from 1 through 65535")
		return
	}
	// SetPortVisibility writes the route's own port through to routes.visibility
	// and any other port to its own row, so this one call covers both without
	// the console having to know which kind of port it was handed.
	if err := h.routes.SetPortVisibility(sub, port, req.Visibility); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console changed port visibility",
		"subdomain", sub, "port", port, "visibility", req.Visibility, "handle", sess.Handle)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// forgetPort drops a port the owner had recorded an opinion about, taking it
// off the strip. The port stays private — it goes back to being one of the
// ports nobody has said anything about, which is what every port is until
// somebody does. A route's own port has no row to drop and is refused.
func (h *Handler) forgetPort(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("subdomain")
	sess, _ := edgeauth.From(r.Context())
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		writeErr(w, http.StatusBadRequest, "port must be from 1 through 65535")
		return
	}
	rt, ok := h.ownedRoute(w, r, sub)
	if !ok {
		return
	}
	if port == rt.Port {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("port %d is the hostname's own port — set it private instead", port))
		return
	}
	if err := h.routes.ForgetPort(sub, port); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console forgot a port", "subdomain", sub, "port", port, "handle", sess.Handle)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ownedRoute resolves {subdomain} to a route the session may act on. Like
// ownedBox, "does not exist" and "belongs to someone else" answer identically
// so ownership is never leaked. It writes the error itself; ok=false means the
// caller must stop.
func (h *Handler) ownedRoute(w http.ResponseWriter, r *http.Request, sub string) (routes.Route, bool) {
	if h.routes == nil {
		writeErr(w, http.StatusNotImplemented, "web routes are not enabled on this host")
		return routes.Route{}, false
	}
	sess, _ := edgeauth.From(r.Context())
	rt, found, err := h.routes.GetBySubdomain(sub)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return routes.Route{}, false
	}
	if !found || (rt.Owner != sess.Handle && !sess.Operator) {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("no route named %q", sub))
		return routes.Route{}, false
	}
	return rt, true
}

// snapshotRow is one snapshot as this console serves it: the stored record,
// plus the tags whose sandboxes boot from it.
//
// The embedded POINTER is what keeps this a compatible change. encoding/json
// promotes an embedded struct's fields inline, so the object on the wire is the
// one that shipped with exactly one new key — a client decoding into
// []*host.Snapshot still works, which is what console_test.go asserts.
type snapshotRow struct {
	*host.Snapshot
	// BoundTags is omitempty rather than never-nil: a column printed empty on
	// every row of a panel where almost nothing is bound buries the one row
	// where it matters.
	BoundTags []string `json:"bound_tags,omitempty"`
	// Port is the default port a sandbox booted from this template inherits,
	// omitted for the templates captured from a box on the stock one. It is
	// shown for the same reason it is shown in `snapshot ls`: a box whose URL
	// answers nothing because its template chose 5173 has no other way to
	// explain itself, and this panel is where its owner is already looking.
	Port int `json:"port,omitempty"`
}

// listSnapshots lists the session's own snapshots — owner-scoped, unlike the
// operator console's AllSnapshots.
func (h *Handler) listSnapshots(w http.ResponseWriter, r *http.Request) {
	handle := handleFrom(r)
	snaps := h.boxes.Snapshots(handle)
	out := make([]snapshotRow, 0, len(snaps))
	bound := h.boundTags(handle)
	ports := h.snapshotPorts(handle)
	for _, s := range snaps {
		out = append(out, snapshotRow{Snapshot: s, BoundTags: bound[s.Name], Port: ports[s.Name]})
	}
	writeJSON(w, http.StatusOK, out)
}

// boundTags maps the owner's snapshot names to the tags bound to them, or
// nothing at all when there is no store or the store is unhappy.
//
// A store failure is logged and served as empty rather than failing the
// request: the bindings are decoration on a row here, and a Snapshots panel
// that goes blank because one extra column could not be filled in is worse than
// one that renders the rows. The authoritative refusals live where the writes
// do — ctlops refuses to delete a bound snapshot, and refuses a create whose
// tags disagree — not in this projection.
func (h *Handler) boundTags(owner string) map[string][]string {
	if h.binds == nil {
		return nil
	}
	list, err := h.binds.BindingsForOwner(owner)
	if err != nil {
		h.log.Warn("user console could not read template bindings", "handle", owner, "err", err)
		return nil
	}
	m := make(map[string][]string, len(list))
	for _, b := range list {
		// The store orders by tag, so the column reads the same way twice.
		m[b.Snapshot] = append(m[b.Snapshot], b.Tag)
	}
	return m
}

// snapshotPorts maps the owner's snapshot names to the default port each was
// captured on, and swallows a store failure for exactly the reason boundTags
// does: it is one more column on a panel, and losing the Snapshots list because
// it could not be filled is the worse outcome.
func (h *Handler) snapshotPorts(owner string) map[string]int {
	if h.binds == nil {
		return nil
	}
	ports, err := h.binds.SnapshotPorts(owner)
	if err != nil {
		h.log.Warn("user console could not read template ports", "handle", owner, "err", err)
		return nil
	}
	return ports
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

// deleteSnapshot removes one of the session's snapshots.
//
// KNOWN GAP, stated rather than papered over: this calls the manager directly,
// so it does NOT pass ctlops.DeleteSnapshot's refusal to delete a snapshot a tag
// is bound to. A delete from this panel can therefore leave a binding pointing
// at a file that is gone. What catches it is ctlops.resolveTemplate, which
// refuses the next create on that tag with `template_missing` instead of quietly
// falling back to the stock image — which is exactly why that fallback must
// never be added. Routing this console through ctlops is a separate change with
// its own ordering bug to solve (see ctlops/sandbox.go's comment on Create).
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

// The install states the Repos panel renders. They are strings rather than a
// bool because "installed" and "not installed" are only two of the four answers
// a host can honestly give, and collapsing the other two into false would tell
// a user their App is missing when the truth is that this host has no App at
// all, or that github.com did not answer.
const (
	appOff      = "off"      // no GitHub App configured on this host
	appChecking = "checking" // asked; the answer lands in time for the next poll
	appReady    = "ready"    // installed, and this account may use it
	appMissing  = "missing"  // the App is not installed on it — clones will fail
	appBlocked  = "blocked"  // installed, but refused for this account
	appUnknown  = "unknown"  // github.com did not answer
)

// How long one probe's answer is trusted, and how long the probe itself may
// take. The TTL is short because the state it caches is one a user changes on
// github.com and then comes straight back here to watch flip; the timeout is
// generous because it is spent on a background goroutine nobody is waiting on.
const (
	appProbeTTL     = 90 * time.Second
	appProbeTimeout = 20 * time.Second
	// appProbeCap bounds the cache. It is per-host, not per-owner, so it is
	// sized well above the store's own per-owner attachment cap.
	appProbeCap = 512
)

// appProbe is one remembered answer.
type appProbe struct {
	state string
	note  string
	at    time.Time
}

// repoView is one attachment as the panel renders it: the stored row plus what
// the App last said about it. The state is not part of repos.Repo because it is
// not stored anywhere and is not this host's to store — it is a fact about
// github.com that changes without any write here. It is on the list response
// rather than behind a per-row button because "attached, and the App cannot
// reach it" is the failure mode of this whole feature, and it is otherwise
// invisible until a clone fails inside a booting guest.
type repoView struct {
	repos.Repo
	App string `json:"app"` // one of the app* constants above
	// AppNote is the sentence behind a blocked or unknown state — ghapp writes
	// errors a human reads, and the panel has nowhere else to put the reason.
	AppNote string `json:"app_note,omitempty"`
	// InstallURL is where the missing installation gets installed. Only set
	// when it would help, so the page never renders a call to action next to a
	// repository that is already reachable.
	InstallURL string `json:"install_url,omitempty"`
	// UserAuth says whether GitHub operations use the owner or the App bot.
	// UserAuthEnabled controls whether the browser can change that choice; the
	// VM device-flow command may still be available when it is false.
	UserAuth        string `json:"user_auth"`
	UserAuthEnabled bool   `json:"user_auth_enabled,omitempty"`
	GitHubLogin     string `json:"github_login,omitempty"`
}

// listRepos returns the session's repo attachments in full, each carrying the
// App's view of it. Like network rules and unlike secrets, nothing here is
// sensitive: a slug is configuration.
func (h *Handler) listRepos(w http.ResponseWriter, r *http.Request) {
	if h.repos == nil {
		writeErr(w, http.StatusNotImplemented, "repos are not enabled on this host")
		return
	}
	handle := handleFrom(r)
	list, err := h.repos.ListRepos(handle)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	// Built with make so an owner with no attachments serialises as [] rather
	// than null: the SPA's Array.isArray guard renders null as an empty table,
	// with no error anywhere to explain it.
	views := make([]repoView, 0, len(list))
	var u users.User
	var userErr error
	if h.accounts != nil {
		u, userErr = h.accounts.Get(handle)
	} else {
		userErr = errors.New("accounts are not enabled")
	}
	strongUser := userErr == nil && u.Active() && u.GitHubVerifiedAt != nil && users.StrongGitHubLink(u.GitHubVia) && u.GitHubID > 0
	for _, rp := range list {
		state, note := h.appState(handle, rp)
		v := repoView{Repo: rp, App: state, AppNote: note, UserAuth: "bot"}
		if strongUser {
			v.GitHubLogin = u.GitHubLogin
			v.UserAuthEnabled = rp.Access == repos.AccessWrite && state == appReady && h.userAuth != nil && h.userAuth.WebEnabled()
			if rp.Access == repos.AccessWrite && state == appReady && h.userAuth != nil && h.userAuth.Authorized(handle, rp.Slug, u.GitHubID) {
				v.UserAuth = "user"
			}
		}
		if state == appMissing {
			v.InstallURL = h.app.InstallURL()
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, views)
}

const githubWebAuthorizationBudget = 12 * time.Second

// authorizeRepo starts a browser OAuth flow for one write attachment. The
// repository is resolved and authorized before GitHub sees the browser, so a
// typed slug cannot turn the endpoint into an installation oracle.
func (h *Handler) authorizeRepo(w http.ResponseWriter, r *http.Request) {
	if h.userAuth == nil || !h.userAuth.WebEnabled() {
		writeErr(w, http.StatusNotImplemented, "browser GitHub authorization is not enabled on this host")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), githubWebAuthorizationBudget)
	defer cancel()
	subject, err := h.repoAuthorizationSubject(ctx, handleFrom(r), r.PathValue("slug"))
	if err != nil {
		writeErr(w, repoAuthorizationStatus(err), err.Error())
		return
	}
	location, err := h.userAuth.StartWeb(subject, h.origin+"/github/repo/callback")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "could not start GitHub authorization: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": location})
}

func (h *Handler) githubRepoCallback(w http.ResponseWriter, r *http.Request) {
	if h.userAuth == nil || !h.userAuth.WebEnabled() {
		writeErr(w, http.StatusNotImplemented, "browser GitHub authorization is not enabled on this host")
		return
	}
	q := r.URL.Query()
	if q.Get("error") != "" {
		h.userAuth.CancelWeb(handleFrom(r), q.Get("state"))
		h.redirectRepoAuthorization(w, r, "declined", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), githubWebAuthorizationBudget)
	defer cancel()
	status, err := h.userAuth.FinishWeb(ctx, handleFrom(r), q.Get("state"), q.Get("code"))
	if err != nil {
		h.log.Warn("github browser repository authorization failed", "handle", handleFrom(r), "err", err)
		h.redirectRepoAuthorization(w, r, "failed", "")
		return
	}
	h.redirectRepoAuthorization(w, r, "authorized", status.Slug)
}

func (h *Handler) redirectRepoAuthorization(w http.ResponseWriter, r *http.Request, state, slug string) {
	q := url.Values{"github": {state}}
	if slug != "" {
		q.Set("repo", slug)
	}
	http.Redirect(w, r, "/?"+q.Encode()+"#repos", http.StatusSeeOther)
}

func (h *Handler) repoAuthorizationSubject(ctx context.Context, handle, slug string) (ghuser.Subject, error) {
	if h.repos == nil || h.app == nil || h.accounts == nil {
		return ghuser.Subject{}, errors.New("repositories are not enabled on this host")
	}
	u, err := h.accounts.Get(handle)
	if err != nil || !u.Active() || u.GitHubVerifiedAt == nil || !users.StrongGitHubLink(u.GitHubVia) || u.GitHubID <= 0 {
		return ghuser.Subject{}, errors.New("this account has no verified GitHub link")
	}
	list, err := h.repos.ListRepos(handle)
	if err != nil {
		return ghuser.Subject{}, err
	}
	var entry repos.Repo
	for _, candidate := range list {
		if strings.EqualFold(candidate.Slug, slug) {
			entry = candidate
			break
		}
	}
	if entry.Slug == "" {
		return ghuser.Subject{}, repos.ErrNoSuchRepo
	}
	if entry.Access != repos.AccessWrite {
		return ghuser.Subject{}, errors.New("user authorization is only available for read + push attachments")
	}
	owner, name, ok := repos.SplitSlug(entry.Slug)
	if !ok {
		return ghuser.Subject{}, fmt.Errorf("invalid stored slug %q", entry.Slug)
	}
	inst, err := h.app.InstallationFor(ctx, owner, name)
	if err != nil {
		return ghuser.Subject{}, err
	}
	if err := h.app.Authorize(ctx, inst, u.GitHubID, u.GitHubLogin); err != nil {
		return ghuser.Subject{}, err
	}
	repoID, err := h.app.RepositoryID(ctx, inst, owner, name)
	if err != nil {
		return ghuser.Subject{}, err
	}
	perms := inst.Narrow(map[string]string{"contents": ghapp.PermWrite, "pull_requests": ghapp.PermWrite, "issues": ghapp.PermWrite})
	perms["contents"] = ghapp.PermWrite
	return ghuser.Subject{Owner: handle, GitHubID: u.GitHubID, InstallationID: inst.ID, RepoID: repoID,
		Slug: entry.Slug, Target: inst.AccountLogin, Permissions: perms}, nil
}

func repoAuthorizationStatus(err error) int {
	switch {
	case errors.Is(err, repos.ErrNoSuchRepo), errors.Is(err, ghapp.ErrNotInstalled):
		return http.StatusNotFound
	case errors.Is(err, ghapp.ErrUpstream):
		return http.StatusBadGateway
	case errors.Is(err, ghapp.ErrNotConfigured):
		return http.StatusNotImplemented
	default:
		return http.StatusForbidden
	}
}

// appState reports what the App last said about one of handle's attachments,
// and starts a probe when it has nothing fresh to say.
//
// It never calls github.com on the request goroutine, which matters more than
// it looks: the SPA re-lists every four seconds, an owner may hold a hundred
// attachments, and ghapp deliberately does not cache a failure — so an inline
// probe would put an uncached fan-out of API calls behind a four-second poll,
// and would spend the App's rate limit on exactly the rows that are already
// broken. A miss therefore answers appChecking and the answer lands in time for
// the next poll; a stale hit answers with the old value while the refresh runs
// behind it, so a settled row never flickers back to "checking".
//
// The answer is per ACCOUNT, not per repository, because it includes
// ghapp.Authorize — see probeApp. Two people attaching the same slug get two
// cache entries, which is the point.
func (h *Handler) appState(handle string, rp repos.Repo) (state, note string) {
	if h.app == nil {
		return appOff, ""
	}
	owner, name, ok := repos.SplitSlug(rp.Slug)
	if !ok {
		// Unreachable through this API — the store refuses such a slug — but a
		// row written by an older or a future writer must not send nonsense to
		// github.com on a four-second timer.
		return appUnknown, "this attachment's slug is not an owner/name repository"
	}
	key := probeKey(handle, rp.Host, rp.Slug)
	h.appMu.Lock()
	defer h.appMu.Unlock()
	p, have := h.appSeen[key]
	if (!have || time.Since(p.at) >= appProbeTTL) && !h.appBusy[key] {
		if h.appBusy == nil {
			h.appBusy = map[string]bool{}
		}
		h.appBusy[key] = true
		go h.probeApp(key, handle, owner, name)
	}
	if have {
		return p.state, p.note
	}
	return appChecking, ""
}

// probeApp asks github.com once and records the answer.
//
// It is ctlops.checkRepo's two questions in this console's shape, and it asks
// both for the same reason that one does: an installation the caller may not
// use is not a reachable repository, and reporting it as one here while
// `ctl repo check` calls it unreachable would leave two surfaces disagreeing
// about the only thing either is for. Authorize is also what keeps this panel
// from answering "is the App installed on <someone else's private repo>" for
// any slug an account cares to type.
//
// Every outcome is recorded, including the failures ghapp refuses to cache:
// this cache is not ghapp's — it exists to keep a four-second poll off the
// network, and a repository nobody installed the App on is precisely the row
// that would otherwise be re-asked fifteen times a minute forever.
func (h *Handler) probeApp(key, handle, owner, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), appProbeTimeout)
	defer cancel()
	state, note := h.probeOnce(ctx, handle, owner, name)

	h.appMu.Lock()
	defer h.appMu.Unlock()
	delete(h.appBusy, key)
	if h.appSeen == nil {
		h.appSeen = map[string]appProbe{}
	}
	if len(h.appSeen) >= appProbeCap {
		for k, v := range h.appSeen {
			if time.Since(v.at) >= appProbeTTL {
				delete(h.appSeen, k)
			}
		}
		// Still full: this host holds more live attachments than the cap, so
		// drop the lot. Rebuilding costs one poll's worth of "checking", which
		// is the right price for a bound that cannot be exceeded.
		if len(h.appSeen) >= appProbeCap {
			h.appSeen = map[string]appProbe{}
		}
	}
	h.appSeen[key] = appProbe{state: state, note: note, at: time.Now()}
}

// attachAllowed is the console's half of the one attachment rule, delegating to
// ctlops.AttachGate so this door and the ctl/REST doors cannot drift apart. The
// gate returns a *ctlops.Error, which statusFor already honours — so a missing
// link is a 409 (something to go and do) and a weak one a 403 (something to
// redo), rather than one indistinguishable 400.
func (h *Handler) attachAllowed(handle string) error {
	u, err := h.accounts.Get(handle)
	if err != nil {
		// The session already passed edgeauth, which reads the same record, so
		// this is a store that broke between two reads rather than an unknown
		// account. Either way there is no identity to authorize with.
		return ctlops.Fail("repo.attach", err)
	}
	return ctlops.AttachGate("repo.attach", u)
}

// probeOnce is the two upstream questions, mapped onto the panel's vocabulary.
// The distinction it preserves is between an answer ("not installed", "you may
// not use this installation") and no answer at all ("github.com did not
// respond"): the first two are things to go and fix, the third is a thing to
// ask again about in a minute, and a UI that renders them alike sends people to
// fix something that was never broken.
func (h *Handler) probeOnce(ctx context.Context, handle, owner, name string) (state, note string) {
	u, err := h.accounts.Get(handle)
	if err != nil {
		// The session already passed edgeauth, which reads the same record, so
		// this is a store that broke between two reads rather than an unknown
		// account. Either way there is no identity to authorize with.
		return appUnknown, "could not read this account's GitHub link"
	}
	inst, err := h.app.InstallationFor(ctx, owner, name)
	if err != nil {
		switch {
		case errors.Is(err, ghapp.ErrNotInstalled):
			return appMissing, err.Error()
		case errors.Is(err, ghapp.ErrNotConfigured):
			return appOff, ""
		case errors.Is(err, ghapp.ErrUpstream):
			return appUnknown, err.Error()
		}
		return appBlocked, err.Error()
	}
	if err := h.app.Authorize(ctx, inst, u.GitHubID, u.GitHubLogin); err != nil {
		if errors.Is(err, ghapp.ErrUpstream) {
			return appUnknown, err.Error()
		}
		return appBlocked, err.Error()
	}
	return appReady, ""
}

// probeKey folds the slug because github.com does: wandb/Hivemind and
// wandb/hivemind are one repository, and the store's own lookups already treat
// them as one row.
func probeKey(handle, host, slug string) string {
	return handle + "\x00" + host + "/" + strings.ToLower(slug)
}

// forgetAppProbe drops a remembered answer so the next listing re-asks. Called
// on every repo mutation, because the gesture that follows "the App is not
// installed on that repository" is: install it on github.com, come back, and
// save the attachment again — and a cache that outlived that gesture would go
// on insisting the repository is unreachable for another minute and a half.
func (h *Handler) forgetAppProbe(handle, host, slug string) {
	key := probeKey(handle, host, slug)
	h.appMu.Lock()
	defer h.appMu.Unlock()
	delete(h.appSeen, key)
}

// repoReq is the attach/update body. There is no owner field and never will be:
// the owner is the session's handle, and the store's WHERE clause is what makes
// a cross-owner slug answer exactly like a missing one.
type repoReq struct {
	Ref    string   `json:"ref"`
	Path   string   `json:"path"`
	Access string   `json:"access"`
	Tags   []string `json:"tags"`
}

// putRepo attaches a repository to the session's tags, or updates an existing
// attachment. It re-pushes egress policy because an attachment implies the
// clone domains in the effective allowlist (the overlay inside
// netrules.AllowForSandbox): without the push, a tagged machine keeps the old
// enforced list and the clone it was just promised fails on DNS.
//
// It does not call syncOwner — that is the secret env push, and a repo slug is
// not a secret. Nor does it clone into anything already running: retagging
// never clones, by design.
func (h *Handler) putRepo(w http.ResponseWriter, r *http.Request) {
	if h.repos == nil {
		writeErr(w, http.StatusNotImplemented, "repos are not enabled on this host")
		return
	}
	slug := r.PathValue("slug")
	var req repoReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed request")
		return
	}
	handle := handleFrom(r)
	// The same attachment gate ctl and the REST API apply (ctlops.attachIdentity),
	// applied here because this is the third door onto one verb and a gate on two
	// of three is not a gate. It is defence in depth rather than the last line —
	// metadata.LocalRepos.Credential re-checks this before every mint, so a weak
	// link could never have obtained a credential — but an attachment it will
	// never honour is a promise the platform cannot keep, and attaching also
	// widens the tag's effective egress through the netrules overlay, which is a
	// real change made by an account that never proved the link.
	if err := h.attachAllowed(handle); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	// Host is left empty on purpose: the store defaults it to github.com and
	// refuses anything else, so naming it here would only add a second place
	// for the two to disagree.
	rp := repos.Repo{Slug: slug, Ref: req.Ref, Path: req.Path, Access: req.Access}
	if err := h.repos.PutRepo(handle, rp, req.Tags); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console attached repo", "slug", slug, "tags", len(req.Tags),
		"access", req.Access, "handle", handle)
	h.forgetAppProbe(handle, defaultRepoHost, slug)
	h.pushNet()
	// The boxes already carrying one of these tags have a manifest that just
	// changed under them. A failure to resolve them is not this call's failure:
	// the attachment is stored, and every one of those boxes checks out from
	// the same ledger at its next start.
	if affected, err := h.repos.SandboxesForRepo(handle, rp.Host, slug); err != nil {
		h.log.Warn("resolve sandboxes for repo", "slug", slug, "handle", handle, "err", err)
	} else {
		h.syncRepos(affected...)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// deleteRepo detaches a repository. Nothing is removed from any guest: a clone
// already on a rootfs stays there — this drops it from the manifest a new
// sandbox reads, and from the egress overlay the push below recomputes.
func (h *Handler) deleteRepo(w http.ResponseWriter, r *http.Request) {
	if h.repos == nil {
		writeErr(w, http.StatusNotImplemented, "repos are not enabled on this host")
		return
	}
	slug := r.PathValue("slug")
	handle := handleFrom(r)
	if err := h.repos.DeleteRepo(handle, defaultRepoHost, slug); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console detached repo", "slug", slug, "handle", handle)
	h.forgetAppProbe(handle, defaultRepoHost, slug)
	h.pushNet()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// defaultRepoHost is the one host internal/repos accepts. The console has no
// field for it because there is nothing to choose: GHES would need its own App
// installation and its own clone domains, and until it has both, a second host
// here would only produce rows no credential path can serve.
const defaultRepoHost = "github.com"

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

// syncRepos nudges the sandboxes reached by a repo or tag change into
// reconciling their checkouts, asynchronously and best-effort.
//
// Async here and synchronous in ctlops, deliberately: `ctl tags set` is one
// person waiting on one box and can afford to report what happened, whereas
// this is an HTTP handler whose response the browser blocks a control on. The
// console shows checkout state on its own (the sandbox view reads the guest's
// report), so a nudge that lands a second after the 200 is invisible in the
// right way.
//
// Reached by type assertion for the reason the ctlops copy is: only a machine
// with the repos table and the App key can answer at all, and adding it to the
// Sandboxes interface would put a method on host.Manager's console-facing
// surface that a node could never implement.
func (h *Handler) syncRepos(names ...string) {
	r, ok := h.boxes.(interface {
		ResyncRepos(ctx context.Context, name string) error
	})
	if !ok || len(names) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, name := range names {
			if err := r.ResyncRepos(ctx, name); err != nil {
				h.log.Warn("sync checkouts", "sandbox", name, "err", err)
			}
		}
	}()
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
		errors.Is(err, netrules.ErrNoSuchRule), errors.Is(err, repos.ErrNoSuchRepo):
		return http.StatusNotFound
	case errors.Is(err, netrules.ErrInvalidRule), errors.Is(err, repos.ErrInvalidRepo):
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

// writeOpErr is writeErr for an error that may be a *ctlops.Error, adding the
// machine token as `code` alongside the sentence.
//
// The console's error body has always been `{"error": "<sentence>"}` and stays
// that way — every existing caller reads that field and keeps working. What it
// could not do is tell one refusal from another without matching on prose,
// which is exactly the coupling `Code` exists to prevent, and the environment
// adoption conflict is the first refusal this surface has to ACT on rather than
// merely display: the page re-sends the request with `adopt` set.
//
// Only the code travels, never Details. The console renders its own sentence
// from what the user typed and has the composition on screen already; a nested
// object here would be a second, staler copy of the Environments tab.
func writeOpErr(w http.ResponseWriter, code int, err error) {
	body := map[string]string{"error": err.Error()}
	var typed *ctlops.Error
	if errors.As(err, &typed) && typed.Code != "" {
		body["code"] = typed.Code
	}
	writeJSON(w, code, body)
}
