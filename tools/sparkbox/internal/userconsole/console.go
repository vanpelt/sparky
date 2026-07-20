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

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
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

// probeTimeout bounds the per-route TCP dial that checks whether anything is
// listening on a forwarded port, and the per-sandbox mem/CPU stat reads.
// Guest IPs are on a local bridge, so a live listener answers in
// microseconds; anything slower is effectively down.
const probeTimeout = 300 * time.Millisecond

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

// Handler serves the user console UI and its JSON API.
type Handler struct {
	mgr      *host.Manager
	routes   *routes.Store  // optional: nil hides web routes and disables port/visibility
	secrets  *secrets.Store // optional: nil disables tags + secrets endpoints
	accounts edgeauth.Accounts
	signer   *edgeauth.Signer
	syncer   OwnerSyncer
	domain   string // base zone, e.g. "hivemind.tools"
	secure   bool   // set the Secure flag when clearing the session cookie
	log      *slog.Logger
	loginURL string // where unauthenticated browsers are sent
	origin   string // first-party Origin accepted by the CSRF gate
}

// New builds a user-console handler for <subdomain>.<domain> (subdomain is
// the --user-console-subdomain value, "my" by default; empty falls back to
// "my"). accounts resolves operator status (a *users.Store satisfies it),
// signer verifies the edge session, and syncer (nil-safe) propagates
// tag/secret changes to running sandboxes. secure should be true when the
// proxy edge serves TLS.
func New(mgr *host.Manager, routeStore *routes.Store, secretsStore *secrets.Store,
	accounts edgeauth.Accounts, signer *edgeauth.Signer, syncer OwnerSyncer,
	subdomain, domain string, secure bool, log *slog.Logger) *Handler {
	if subdomain == "" {
		subdomain = "my"
	}
	// A leading-dot --proxy-domain (".hivemind.tools") is tolerated by the
	// proxy and the login handler, which normalize it; normalize here too so
	// the logout cookie Domain, login URL, and CSRF origin match the ones
	// login built.
	domain = strings.TrimPrefix(domain, ".")
	return &Handler{
		mgr: mgr, routes: routeStore, secrets: secretsStore,
		accounts: accounts, signer: signer, syncer: syncer,
		domain: domain, secure: secure, log: log,
		loginURL: "https://login." + domain + "/",
		origin:   "https://" + subdomain + "." + domain,
	}
}

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
	mux.Handle("POST /api/machines/{name}/port", mutate(h.setPort))
	mux.Handle("PUT /api/machines/{name}/tags", mutate(h.setTags))
	mux.Handle("POST /api/routes/{subdomain}/visibility", mutate(h.setVisibility))
	mux.Handle("GET /api/snapshots", require(h.listSnapshots))
	mux.Handle("POST /api/snapshots/{snapshot}/fork", mutate(h.fork))
	mux.Handle("POST /api/snapshots/{snapshot}/delete", mutate(h.deleteSnapshot))
	mux.Handle("GET /api/secrets", require(h.listSecrets))
	mux.Handle("PUT /api/secrets/{env_name}", mutate(h.putSecret))
	mux.Handle("DELETE /api/secrets/{env_name}", mutate(h.deleteSecret))
	mux.HandleFunc("GET /", h.index)
	return mux
}

// index always serves the single-page app; the page itself calls the API and
// renders the sign-in state when that returns 401.
func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	indexPage.ServeHTTP(w, r)
}

type meResponse struct {
	Handle   string `json:"handle"`
	Email    string `json:"email,omitempty"`
	Operator bool   `json:"operator"`
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	sess, _ := edgeauth.From(r.Context())
	writeJSON(w, http.StatusOK, meResponse{Handle: sess.Handle, Email: sess.Email, Operator: sess.Operator})
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
	boxes := h.mgr.ListByOwner(sess.Handle)
	views := make([]sandboxView, len(boxes))
	var wg sync.WaitGroup
	for i, b := range boxes {
		views[i] = sandboxView{Sandbox: b, Routes: []routeStatus{}, Tags: []string{}}
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
		// Read the guest's real memory use and cumulative CPU time concurrently;
		// bounded by probeTimeout so one slow VM can't stall the dashboard.
		if b.State == vmm.StateRunning {
			wg.Add(1)
			go func(name string, mem **int64, cpu **float64) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
				defer cancel()
				if used, ok := h.mgr.MemStats(ctx, name); ok {
					*mem = &used
				}
				if secs, ok := h.mgr.CPUSeconds(ctx, name); ok {
					*cpu = &secs
				}
			}(b.Name, &views[i].MemUsedMB, &views[i].CPUSeconds)
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

// ownedBox resolves the {name} path value to a sandbox the session may act
// on. ok=false covers both "does not exist" and "belongs to someone else" —
// callers answer with the identical not-found body so ownership is never
// leaked. Operators pass for any sandbox.
func (h *Handler) ownedBox(r *http.Request) (box *host.Sandbox, name string, ok bool) {
	name = r.PathValue("name")
	sess, _ := edgeauth.From(r.Context())
	box, found := h.mgr.Get(name)
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
	if err := h.mgr.Pause(ctx, name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console paused sandbox", "name", name, "handle", handleFrom(r))
	box, _ := h.mgr.Get(name)
	writeJSON(w, http.StatusOK, box)
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
	box, err := h.mgr.EnsureRunning(ctx, name)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console resumed sandbox", "name", name, "handle", handleFrom(r))
	writeJSON(w, http.StatusOK, box)
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
	if err := h.mgr.Destroy(ctx, name); err != nil {
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
	if err := h.mgr.Archive(ctx, name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console archived sandbox", "name", name, "handle", handleFrom(r))
	box, _ := h.mgr.Get(name)
	writeJSON(w, http.StatusOK, box)
}

// pin marks a sandbox always-on and resumes it so its in-guest daemons start
// running immediately. unpin clears the flag, letting the reaper pause it.
func (h *Handler) pin(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	if err := h.mgr.SetPinned(name, true); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), archiveTimeout)
	defer cancel()
	box, err := h.mgr.EnsureRunning(ctx, name)
	if err != nil {
		// The flag stuck; it just isn't warm yet. Surface the reason.
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console pinned sandbox", "name", name, "handle", handleFrom(r))
	writeJSON(w, http.StatusOK, box)
}

func (h *Handler) unpin(w http.ResponseWriter, r *http.Request) {
	_, name, ok := h.ownedBox(r)
	if !ok {
		notFoundBox(w, name)
		return
	}
	if err := h.mgr.SetPinned(name, false); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console unpinned sandbox", "name", name, "handle", handleFrom(r))
	box, _ := h.mgr.Get(name)
	writeJSON(w, http.StatusOK, box)
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
	snap, err := h.mgr.Snapshot(ctx, name, req.SnapshotName, box.Owner)
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
	if err := h.mgr.Rename(ctx, name, req.NewName, box.Owner); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console renamed sandbox", "old", name, "new", req.NewName, "handle", handleFrom(r))
	box, _ = h.mgr.Get(req.NewName)
	writeJSON(w, http.StatusOK, box)
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
	if err := h.mgr.Reboot(ctx, name); err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console rebooted sandbox", "name", name, "handle", handleFrom(r))
	box, _ := h.mgr.Get(name)
	writeJSON(w, http.StatusOK, box)
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
	snaps := h.mgr.Snapshots(handleFrom(r))
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
	box, err := h.mgr.Fork(ctx, snap, req.Name, handleFrom(r), 0, 0)
	if err != nil {
		writeErr(w, statusFor(err), err.Error())
		return
	}
	h.log.Info("user console forked snapshot", "snapshot", snap, "into", req.Name, "handle", handleFrom(r))
	writeJSON(w, http.StatusCreated, box)
}

func (h *Handler) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	snap := r.PathValue("snapshot")
	ctx, cancel := context.WithTimeout(r.Context(), pauseTimeout)
	defer cancel()
	if err := h.mgr.DeleteSnapshot(ctx, snap, handleFrom(r)); err != nil {
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

// syncOwner fires the change-time env push. Nil-safe; the syncer itself is
// async and best-effort, so this never delays or fails the response.
func (h *Handler) syncOwner(ctx context.Context, owner string) {
	if h.syncer == nil {
		return
	}
	h.syncer.SyncOwner(ctx, owner)
}

// statusFor maps store/manager errors onto HTTP statuses by their sentinel or
// message, per the local-copy convention (internal/console has its own).
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusInternalServerError
	case errors.Is(err, secrets.ErrNoSuchSecret), errors.Is(err, routes.ErrNoSuchRoute):
		return http.StatusNotFound
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
