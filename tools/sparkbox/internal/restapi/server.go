// Package restapi is the authenticated REST surface at api.<domain>: the same
// operations `ssh ctl@<domain>` performs, over HTTPS, for callers that have a
// session token but no SSH client. Every handler is a thin shell over
// internal/ctlops — decode arguments, call one method, render the typed result
// or the typed error — so ownership, timeout budgets and the tags-before-create
// ordering are enforced in exactly one place for both transports.
//
// It is emphatically NOT internal/api, which takes its owner from the request
// body, performs no authorization, and is bound to loopback for that reason.
// This package authenticates with the zone-wide edge session (cookie or
// `Authorization: Bearer`) through edgeauth.Require, gates mutations behind
// edgeauth.RequireMutation's CSRF check, and scopes every operation to the
// session's handle — the session handle is the only source of identity, and no
// request field can name a different owner.
//
// Two conventions differ from the user console on purpose. Errors are a
// structured envelope ({"error":{"code":…,"message":…}}) rather than a bare
// string, because this is a documented contract that clients switch on; and
// long operations answer 202 with a job resource rather than holding a
// connection open for fifteen minutes, which no proxy on the path would
// tolerate. Both are described in openapi.json, which is served from this
// package and pinned to the route table by a test.
package restapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
)

// maxBodyBytes bounds a request body. Every body this API accepts is a handful
// of short fields; the one that could plausibly be long is an authorized_keys
// line, which is still under a kilobyte. The limit exists so a hostile client
// cannot make the idempotency cache fingerprint an arbitrarily large payload.
const maxBodyBytes = 1 << 20

// Terminal is the browser-terminal WebSocket bridge, kept behind an interface
// so this package neither imports internal/xterm nor knows how a PTY is framed:
// it proves who is asking, resolves the sandbox name from the path, and hands
// both to whatever the integrator wired. *xterm.Bridge satisfies it
// structurally, exactly as *envsync.Syncer satisfies userconsole.OwnerSyncer —
// no adapter to write, and no import cycle when xterm wants to reuse this
// package's error envelope. A nil Terminal answers 501 rather than panicking,
// which is what a host serving no browser terminals wants.
type Terminal interface {
	ServeTerminal(w http.ResponseWriter, r *http.Request, c ctlops.Caller, sandbox string)
}

// Config wires the handler. Ops, Signer and Accounts are required; a nil
// Terminal disables the WebSocket endpoint, and everything else has a sensible
// zero value.
type Config struct {
	Ops      *ctlops.Ops       // required: the control-plane core
	Accounts edgeauth.Accounts // resolves operator status for the session
	Signer   *edgeauth.Signer  // verifies the edge session (cookie or Bearer)
	Terminal Terminal          // optional: nil makes the terminal endpoint 501

	Subdomain string // --api-subdomain, "api" by default
	Domain    string // base zone, e.g. "catnip.sh"
	// XtermSubdomain is --xterm-subdomain. It reaches nothing at runtime; it is
	// here so the served spec's terminal examples name the subtree this host
	// actually serves. Empty takes xterm.DefaultSubdomain, because a document
	// that says "https://demo..catnip.sh" helps nobody.
	XtermSubdomain string
	// LoginURL is where an unauthenticated browser is sent. Empty derives
	// "https://login.<domain>/", which is right only while --login-subdomain is
	// at its default — main passes the configured one.
	LoginURL string

	Log *slog.Logger // required in production; nil discards
}

// Handler serves the REST API, the OpenAPI document and the docs page.
type Handler struct {
	ops      *ctlops.Ops
	accounts edgeauth.Accounts
	signer   *edgeauth.Signer
	terminal Terminal
	log      *slog.Logger

	origin   string // first-party Origin accepted by the CSRF gate
	loginURL string // where unauthenticated browsers are sent

	// specJSON and specYAML are this host's copy of the embedded document, with
	// every example rewritten to name the real zone, composed once at New.
	specJSON *blob
	specYAML *blob

	idem *replayCache
}

// New builds a handler for <subdomain>.<domain>.
func New(cfg Config) *Handler {
	sub := cfg.Subdomain
	if sub == "" {
		sub = "api"
	}
	// A leading-dot --proxy-domain (".catnip.sh") is tolerated everywhere else
	// in the tree; normalize it here too so the CSRF origin and the login URL
	// match the ones the login handler built.
	domain := strings.TrimPrefix(cfg.Domain, ".")
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	origin := "https://" + sub
	loginURL := cfg.LoginURL
	if domain != "" {
		origin = "https://" + sub + "." + domain
		if loginURL == "" {
			loginURL = "https://login." + domain + "/"
		}
	}

	xtermSub := cfg.XtermSubdomain
	if xtermSub == "" {
		xtermSub = defaultXtermSubdomain
	}
	specJSON, specYAML := specFor(domain, specHosts{
		API:   hostUnder(sub, domain),
		Xterm: hostUnder(xtermSub, domain),
		Login: hostOf(loginURL),
	})
	return &Handler{
		ops: cfg.Ops, accounts: cfg.Accounts, signer: cfg.Signer,
		terminal: cfg.Terminal, log: log,
		origin: origin, loginURL: loginURL,
		specJSON: specJSON, specYAML: specYAML,
		idem: newReplayCache(),
	}
}

// defaultXtermSubdomain mirrors xterm.DefaultSubdomain. It is copied rather
// than imported because this package deliberately does not depend on
// internal/xterm — the Terminal interface above is the whole relationship, and
// keeping it that way is what leaves xterm free to import this package's error
// envelope later.
const defaultXtermSubdomain = "xterm"

// hostUnder renders "<label>.<zone>", or nothing when either half is missing —
// an empty answer tells specFor to leave the placeholder alone rather than
// publish "https://api." as a base URL.
func hostUnder(label, domain string) string {
	if label == "" || domain == "" {
		return ""
	}
	return label + "." + domain
}

// hostOf pulls the bare host out of a configured URL, so the spec's sign-in
// prose names the login subtree this host actually redirects to rather than the
// default label.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// authKind says how a route is gated. It is a field on the route table rather
// than a decision made at registration time so the spec test can assert that
// every documented operation declares the security scheme it actually enforces.
type authKind int

const (
	authPublic authKind = iota // the docs page and the spec itself
	authRead                   // edgeauth.Require: a valid session
	authMutate                 // edgeauth.RequireMutation: session + CSRF proof
)

// route is one entry of the single table that builds the mux AND is compared
// against openapi.json by openapi_test.go. Registering a handler anywhere else
// would defeat that check, so nothing else in this package touches the mux.
type route struct {
	method  string
	pattern string // a Go 1.22 mux pattern; also the OpenAPI path, verbatim
	opID    string // the OpenAPI operationId
	auth    authKind
	h       http.HandlerFunc
}

// routes is the whole surface. Order is documentation: reads, then mutations,
// grouped as the docs page groups them.
func (h *Handler) routes() []route {
	return []route{
		// Discovery. Public because a docs page that 401s before you have a
		// token is a docs page nobody can use to learn how to get one.
		// "/{$}" and not "/": under Go 1.22's mux the bare "/" is the
		// least-specific pattern and therefore a catch-all, so every mistyped
		// GET under /v1 would 303 to the docs page — HTML into a JSON parser,
		// from a route that is authPublic and so never reaches the auth gate.
		// "{$}" means "exactly /". The document still calls this path "/"; see
		// specPath.
		{"GET", "/{$}", "root", authPublic, h.root},
		{"GET", "/docs", "docs", authPublic, h.docs},
		{"GET", "/openapi.json", "openapi.json", authPublic, h.openapiJSON},
		{"GET", "/openapi.yaml", "openapi.yaml", authPublic, h.openapiYAML},

		// Identity and host capabilities.
		{"GET", "/v1/capabilities", "capabilities", authRead, h.capabilities},
		{"GET", "/v1/whoami", "whoami", authRead, h.whoami},

		// Sandboxes.
		{"GET", "/v1/sandboxes", "list", authRead, h.listSandboxes},
		{"POST", "/v1/sandboxes", "create", authMutate, h.createSandbox},
		{"GET", "/v1/sandboxes/{name}", "get", authRead, h.getSandbox},
		{"DELETE", "/v1/sandboxes/{name}", "rm", authMutate, h.destroySandbox},
		{"POST", "/v1/sandboxes/{name}/pause", "pause", authMutate, h.pause},
		{"POST", "/v1/sandboxes/{name}/resume", "restore", authMutate, h.resume},
		{"POST", "/v1/sandboxes/{name}/archive", "archive", authMutate, h.archive},
		{"POST", "/v1/sandboxes/{name}/resize", "resize", authMutate, h.resize},
		{"POST", "/v1/sandboxes/{name}/reboot", "reboot", authMutate, h.reboot},
		{"POST", "/v1/sandboxes/{name}/rename", "rename", authMutate, h.rename},
		{"POST", "/v1/sandboxes/{name}/pin", "pin", authMutate, h.pin},
		{"POST", "/v1/sandboxes/{name}/unpin", "unpin", authMutate, h.unpin},
		{"GET", "/v1/sandboxes/{name}/tags", "tags.get", authRead, h.getTags},
		{"PUT", "/v1/sandboxes/{name}/tags", "tags.set", authMutate, h.setTags},
		{"GET", "/v1/sandboxes/{name}/visibility", "share.get", authRead, h.getVisibility},
		{"PUT", "/v1/sandboxes/{name}/visibility", "share.set", authMutate, h.setVisibility},
		{"GET", "/v1/sandboxes/{name}/terminal", "attach", authRead, h.terminalWS},

		// Snapshots and forks.
		{"GET", "/v1/snapshots", "snapshot.list", authRead, h.listSnapshots},
		{"POST", "/v1/snapshots", "snapshot.create", authMutate, h.createSnapshot},
		{"DELETE", "/v1/snapshots/{name}", "snapshot.rm", authMutate, h.deleteSnapshot},
		{"POST", "/v1/snapshots/{name}/fork", "fork", authMutate, h.fork},

		// Schedules.
		{"GET", "/v1/schedules", "schedule.list", authRead, h.listSchedules},
		{"POST", "/v1/schedules", "schedule.add", authMutate, h.addSchedule},
		{"DELETE", "/v1/schedules/{id}", "schedule.rm", authMutate, h.deleteSchedule},

		// Account.
		{"GET", "/v1/account/keys", "keys.list", authRead, h.listKeys},
		{"POST", "/v1/account/keys", "keys.add", authMutate, h.addKey},
		{"DELETE", "/v1/account/keys", "keys.rm", authMutate, h.removeKey},
		{"POST", "/v1/account/keys/import-github", "keys.import-github", authMutate, h.importGitHubKeys},
		{"POST", "/v1/account/github", "keys.verify-github", authMutate, h.verifyGitHub},
		{"GET", "/v1/account/passkeys", "passkey.list", authRead, h.listPasskeys},
		{"DELETE", "/v1/account/passkeys/{id}", "passkey.rm", authMutate, h.removePasskey},
		{"GET", "/v1/account/email", "email.get", authRead, h.getEmail},
		{"PUT", "/v1/account/email", "email.set", authMutate, h.setEmail},
		{"POST", "/v1/account/tokens", "session-token", authMutate, h.mintToken},
		{"POST", "/v1/account/invites", "invite", authMutate, h.invite},

		// Jobs — where the long operations live once they escalate.
		{"GET", "/v1/jobs", "jobs.list", authRead, h.listJobs},
		{"GET", "/v1/jobs/{id}", "jobs.get", authRead, h.getJob},
		{"DELETE", "/v1/jobs/{id}", "jobs.cancel", authMutate, h.cancelJob},
	}
}

// Handler builds the mux. Mutating routes additionally pass through the
// idempotency cache, which sits INSIDE the auth gate so a replay key is scoped
// to the handle that created it and an unauthenticated caller cannot probe for
// one.
func (h *Handler) Handler() http.Handler {
	auth := edgeauth.Require(h.signer, h.accounts, h.loginURL)
	csrf := edgeauth.RequireMutation(h.signer, h.accounts, h.loginURL, h.origin)

	mux := http.NewServeMux()
	for _, rt := range h.routes() {
		var wrapped http.Handler = limitBody(rt.h)
		switch rt.auth {
		case authRead:
			wrapped = auth(wrapped)
		case authMutate:
			// Only mutations are replayable. A read has nothing to remember, and
			// the terminal upgrade — a GET, deliberately — must reach the
			// hijacker underneath rather than a buffering recorder.
			wrapped = csrf(h.idem.wrap(wrapped))
		}
		mux.Handle(rt.method+" "+rt.pattern, wrapped)
	}
	// Everything the table does not claim. Without it the contract this package
	// documents — "errors are an envelope" — would hold for every failure except
	// the two a client hits first: a typo'd path, and the right path with the
	// wrong verb.
	mux.Handle("/", h.unrouted())
	return mux
}

// unrouted answers a request no route matched, in the error envelope rather
// than net/http's plain text.
//
// It has to tell "no such path" from "wrong method for a path that exists", and
// registering a catch-all on the real mux destroys the mux's own 405: "/"
// matches every method, so the method-mismatch branch is never reached. The
// answer is a second mux carrying the same patterns with no method attached —
// a path it matches is a path this API serves, so the failure was the verb.
func (h *Handler) unrouted() http.Handler {
	allowed := map[string][]string{}
	for _, rt := range h.routes() {
		allowed[rt.pattern] = append(allowed[rt.pattern], rt.method)
	}
	paths := http.NewServeMux()
	for pattern, methods := range allowed {
		allow := strings.Join(methods, ", ")
		paths.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allow)
			writeJSON(w, http.StatusMethodNotAllowed, errorEnvelope{Error: apiError{
				Kind: "invalid", Op: "route", Code: "method_not_allowed",
				Message: r.Method + " is not allowed on " + r.URL.Path,
				Hint:    "This path accepts " + allow + ".",
			}})
		}))
	}
	paths.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, errorEnvelope{Error: apiError{
			Kind: "not_found", Op: "route", Code: "unknown_endpoint",
			Message: "no such endpoint: " + r.Method + " " + r.URL.Path,
			Hint:    "The full surface is at /openapi.json.",
		}})
	})
	return paths
}

// specPath renders a mux pattern as the OpenAPI path it documents. Only the
// root differs: "{$}" is Go's "match exactly this path" wildcard and has no
// meaning in OpenAPI, where the root is written "/".
func specPath(pattern string) string {
	if pattern == "/{$}" {
		return "/"
	}
	return pattern
}

// limitBody caps the request body. It wraps rather than being applied in each
// handler because a handler that forgets is a handler that reads whatever the
// client sends.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// root points a bare visit at the docs rather than 404ing. 303 rather than 301
// because the redirect target is a page, not a permanent identity for "/", and
// a cached permanent redirect is impossible to take back.
func (h *Handler) root(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/docs", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// The caller
// ---------------------------------------------------------------------------

// caller is the identity every ctlops call is made under. It comes from the
// verified session and nowhere else: no body field, header or query parameter
// can name a different handle, which is what makes the owner check in ctlops
// meaningful. KeyFP stays empty because an HTTP request carries no SSH key —
// ctlops documents that and the commands that need one ask for it explicitly.
func caller(r *http.Request) ctlops.Caller {
	sess, _ := edgeauth.From(r.Context())
	return ctlops.Caller{Handle: sess.Handle}
}

// ---------------------------------------------------------------------------
// JSON
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// writeRaw writes an already-marshalled body — a job's stored result, which was
// encoded when the work finished and must not be re-shaped on the way out.
func writeRaw(w http.ResponseWriter, code int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	w.Write(body) //nolint:errcheck
}

// apiError is the wire shape of a failure. It is a projection of ctlops.Error
// rather than the struct itself for two reasons: Kind must render as its stable
// string, not an integer whose value would shift if a Kind were ever inserted;
// and Err — the wrapped cause, which may name an internal address or a driver
// path — must be impossible to serialize by accident.
type apiError struct {
	Kind    string         `json:"kind"`
	Op      string         `json:"op"`
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Hint    string         `json:"hint,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

func apiErrorOf(e *ctlops.Error) apiError {
	if e == nil {
		return apiError{}
	}
	return apiError{
		Kind: e.Kind.String(), Op: e.Op, Code: e.Code,
		Message: e.Msg, Hint: e.Hint, Details: e.Details,
	}
}

// fail renders a ctlops error. Every failure in this package goes through it,
// so the status comes from the typed Kind rather than from substring-matching
// the message — which is what the two shipped statusFor() functions do, and
// what quietly turns a reworded error into a changed status code.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	e := ctlops.AsError(op, err)
	status := e.HTTPStatus()

	// Log the wrapped cause; never render it. This surface faces strangers, so
	// it follows the proxy's rule (keep the detail in the log) rather than the
	// console's (show the operator everything). Only a genuine 500 is an ERROR:
	// a 501 is how a host says it did not configure a feature, and a 502 is
	// github.com having a bad day — neither is this process misbehaving.
	level := slog.LevelInfo
	switch {
	case status == http.StatusInternalServerError:
		level = slog.LevelError
	case status >= 500 && status != statusClientClosed:
		level = slog.LevelWarn
	}
	h.log.Log(r.Context(), level, "api call failed",
		"op", e.Op, "code", e.Code, "status", status,
		"user", caller(r).Handle, "path", r.URL.Path, "err", errors.Unwrap(e))

	// 499 means the client hung up: there is nobody left to read a body, and
	// writing one would only be a second error in the log.
	if status == statusClientClosed {
		return
	}
	writeJSON(w, status, errorEnvelope{Error: apiErrorOf(e)})
}

// statusClientClosed mirrors the constant ctlops assigns to a canceled request.
// Duplicated rather than exported from there because it is nginx's convention,
// not part of either package's contract.
const statusClientClosed = 499

// decode reads a JSON request body into dst. An ABSENT body is not an error:
// most endpoints here take no arguments at all, and the ones that do validate
// their own fields in ctlops, which produces a better message than "unexpected
// end of JSON input" ever could. It returns false having already answered.
func (h *Handler) decode(w http.ResponseWriter, r *http.Request, op string, dst any) bool {
	if r.Body == nil {
		return true
	}
	dec := json.NewDecoder(r.Body)
	// Unknown fields are refused: silently ignoring a misspelled "vcpu" would
	// hand the caller a sandbox with defaults they did not ask for.
	dec.DisallowUnknownFields()
	switch err := dec.Decode(dst); {
	case err == nil, errors.Is(err, io.EOF):
		return true
	default:
		h.fail(w, r, op, ctlops.Invalid(op, "malformed_body", "malformed request body: %v", err))
		return false
	}
}
