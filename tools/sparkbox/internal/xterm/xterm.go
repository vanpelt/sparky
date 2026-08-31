// Package xterm serves the browser terminal at https://<name>-xterm.<domain>:
// an xterm.js page, the vendored assets it needs, and the WebSocket that
// bridges it to a real PTY inside that sandbox.
//
// It exists because a shell is the one thing a sandbox is for that a browser
// could not previously reach. `ssh <name>.<domain>` needs a registered key and
// a terminal; this needs a tab. The two share everything that matters — the
// same ownership rule, the same resume-on-connect, the same live-session
// registry — and differ only in transport, which is the whole design: a
// browser terminal that took a different path to the guest would be a second
// place for the ownership check to be forgotten.
//
// The sandbox is named by DNS, not by a path or a query parameter, so that the
// browser's own origin isolation does the work: one sandbox per origin means a
// page served for `a-xterm.<domain>` cannot script the terminal of
// `b-xterm.<domain>`, and the WebSocket's origin check (ws.go) reduces to
// "Origin must equal my own host". It is a separate host from the sandbox's own
// front door `a.<domain>` for the same reason — the guest's app must not be
// able to script the terminal that shells into it.
package xterm

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/reserved"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/webui"
)

// DefaultSubdomain is the reserved name segment the edge dispatches on: the
// terminal for sandbox "demo" is served at "demo-<DefaultSubdomain>.<domain>".
// It is a constant rather than a bare string in main so the flag default, the
// reserved-name lists and this package cannot drift apart.
const DefaultSubdomain = "xterm"

// ReservedSuffix is the sandbox- and route-name suffix this package's dispatch
// claims for a given label. A name ending in it is unreachable over HTTP — the
// edge sends it to the terminal handler before any route lookup — so the
// stores refuse to create one.
func ReservedSuffix(label string) string { return "-" + label }

// Attacher is the slice of *host.Manager a terminal needs: resolve a sandbox,
// resume it, and mark it active. Stated as an interface so this package's tests
// run against an in-memory fake with no VM driver — and deliberately narrow, so
// that nothing here can pause, destroy or re-tag a box.
type Attacher interface {
	Get(name string) (*host.Sandbox, bool)
	EnsureReady(ctx context.Context, name string) (*host.Sandbox, error)
	MarkActive(name string)
}

// SessionConn is the slice of a live terminal that the SSH gateway's hang-up
// path needs: somewhere to write the goodbye, and a way to end the session.
//
// It is declared method-for-method identical to sshgw's own (unexported)
// sessionConn, down to io.ReadWriter rather than an equivalent anonymous
// interface, because Go's structural satisfaction is by identical types. That
// identity is the point: the adapter this package registers satisfies both
// interfaces, so the two packages share one session registry without either
// importing the other's types. There must be exactly one registry —
// host.Manager takes a single SessionCloser — and a second would mean pausing a
// sandbox silently strands every browser terminal attached to it.
type SessionConn interface {
	Stderr() io.ReadWriter
	Close() error
}

// HungUpMarker mirrors sshgw.HungUpMarker for the same reason SessionConn
// mirrors its sessionConn, and is satisfied by the same adapter: it is how the
// gateway claims a terminal synchronously, before the sandbox stops, so that a
// bridge unwinding on the dead guest does not answer "shell exited" for a
// sandbox that was paused. Declared here so the two halves are visible together
// and a rename on either side is a compile error rather than a silent return to
// the race — see the assertion in conn.go.
type HungUpMarker interface {
	MarkHungUp()
}

// Config wires the handler. Sandboxes, Sessions and UpstreamKey are required;
// the rest degrade rather than panic, because a unit test and a
// minimally-configured host both want that.
type Config struct {
	// Sandboxes resolves and resumes the target VM. Satisfied by *host.Manager.
	Sandboxes Attacher
	// Accounts resolves operator status. Satisfied by *users.Store; nil means
	// nobody is an operator, which fails closed.
	Accounts edgeauth.Accounts
	// Vitals reads live CPU, memory and network counters for the sandbox this
	// page is attached to, feeding its instrument strip. Satisfied by
	// *fleet.Fleet, which routes the read to the machine holding the VM — a
	// balloon and a VMM process can only be asked of the host running them, so
	// this is the one meter that cannot be answered locally for a sandbox on
	// another node. *host.Manager satisfies it too, for a build with no fleet.
	// Nil serves the page with no meters, which is what a test and a stats-less
	// driver both want.
	Vitals VitalsReader
	// Node is this machine's name, and is used for one thing: deciding how long
	// a vitals read may take. A sandbox on another machine costs a round trip
	// to it before its balloon is even touched, so it gets webui's tunneled
	// budget rather than the local one. Empty makes every sandbox look local,
	// which is right for a single-machine deployment and for every test.
	Node string
	// Turbo restarts the attached sandbox with doubled CPU and RAM, or back at
	// its own size — the header's turbo button. Satisfied by *fleet.Fleet, which
	// routes the restart to the machine holding the VM. Nil serves the page with
	// no turbo button, which is what every test wants and what a deployment that
	// would rather not hand out double resources gets by leaving it unset.
	Turbo Turbocharger
	// Sessions verifies the edge session token carried by the cookie or a
	// Bearer header.
	Sessions *edgeauth.Signer
	// UpstreamKey authenticates the control plane into the guest's sshd. It is
	// the same key the SSH gateway dials with — this package opens a second
	// session over the same trust relationship, it does not invent a new one.
	UpstreamKey xssh.Signer

	// Domain is the base zone ("catnip.sh"). Empty accepts any zone, which is
	// what a test with Host "demo-xterm.example" wants and what a real
	// deployment never wants — main always sets it.
	Domain string
	// Subdomain is the reserved segment appended to the sandbox name, after a
	// hyphen, to form the host: "demo-xterm.<zone>". Empty takes
	// DefaultSubdomain.
	Subdomain string
	// LoginURL is where an unauthenticated browser is sent; it comes back to
	// the URL it asked for. Empty turns the redirect into a plain 401.
	LoginURL string

	// SSHHost and SSHPort are what the page's "copy the ssh command" row
	// spells, and they are the ADVERTISED pair — the hostname and port a person
	// types, which on a host with an edge DNAT is not the address the gateway
	// binds. main already computes both for the login page and the ctl channel;
	// they are threaded here rather than recomposed so all three surfaces
	// cannot disagree about what to tell somebody to type.
	//
	// Empty SSHHost drops the row rather than printing a command that would
	// resolve to somebody else's machine. Port 0 or 22 prints no -p.
	SSHHost string
	SSHPort int
	// ConsoleURL is the user console this sandbox's owner came from, linked
	// from the page's menu. Empty renders no link — a host with no console has
	// no hostname to guess, and the launch door treats the same emptiness the
	// same way.
	ConsoleURL string
	// ProxyPort resolves the sandbox's current default route port. Nil uses the
	// platform default. It is separate from Proxy's stable portless URL because
	// the page needs the numeric port to explain why that URL is not ready yet.
	ProxyPort func(sandbox string) (int, bool)

	// Track registers a live terminal with the SSH gateway's session registry
	// and returns the unregister func — *sshgw.Gateway's tracker, passed as a
	// function so neither package imports the other. Nil skips registration,
	// which costs the clean hang-up on pause; tests leave it nil.
	Track func(sandbox string, s SessionConn, isPTY bool) func()

	// Dial opens the TCP connection to the guest's sshd. Nil dials
	// box.SSHAddr over the host network, which is what a single-box
	// deployment and every unit test want — so unlike Sandboxes and Sessions
	// it is deliberately not on New's panic-on-missing list. A fleet passes
	// its own dialer here, and a sandbox on another machine is then reached
	// over that machine's link instead of a host route that does not exist.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)

	Log *slog.Logger
}

// Handler serves the terminal page, its assets, and the WebSocket bridge.
type Handler struct {
	mgr          Attacher
	accounts     edgeauth.Accounts
	sessions     *edgeauth.Signer
	upstreamKey  xssh.Signer
	vitalsOf     VitalsReader
	turbocharger Turbocharger
	// probe carries this machine's name, which is how a local sandbox is told
	// from a remote one when budgeting a vitals read.
	probe webui.Probe

	domain    string
	subdomain string
	loginURL  string
	// sshCommand is the whole `ssh …` line, composed once in New rather than
	// per request: it is a function of configuration and the sandbox name, and
	// the name is the only half that varies.
	sshCommand func(sandbox string) string
	consoleURL string
	// proxyURL composes the sandbox's default HTTPS route. Like sshCommand it
	// is nil when this host has no advertised domain, so the page never guesses
	// a public hostname from its configurable terminal label.
	proxyURL  func(sandbox string) string
	proxyPort func(sandbox string) (int, bool)

	track func(sandbox string, s SessionConn, isPTY bool) func()
	dial  func(ctx context.Context, network, addr string) (net.Conn, error)
	log   *slog.Logger

	// open dials the guest and allocates the PTY. A field rather than a direct
	// call so bridge_test can attach a fake guest and exercise the framing,
	// resize clamping and close codes with no VM anywhere.
	open func(ctx context.Context, box *host.Sandbox, term string, rows, cols int) (PTY, error)

	mux http.Handler
}

// New builds the handler. It panics on a missing required dependency, at
// startup and in one place, rather than nil-dereferencing on the first
// connection an hour later.
func New(cfg Config) *Handler {
	if cfg.Sandboxes == nil {
		panic("xterm: Sandboxes is required")
	}
	if cfg.Sessions == nil {
		panic("xterm: Sessions signer is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	sub := cfg.Subdomain
	if sub == "" {
		sub = DefaultSubdomain
	}
	h := &Handler{
		mgr: cfg.Sandboxes, accounts: cfg.Accounts, sessions: cfg.Sessions,
		upstreamKey: cfg.UpstreamKey, vitalsOf: cfg.Vitals, turbocharger: cfg.Turbo,
		probe:      webui.Probe{Node: cfg.Node},
		domain:     strings.ToLower(strings.Trim(cfg.Domain, ".")),
		subdomain:  strings.ToLower(sub),
		loginURL:   cfg.LoginURL,
		sshCommand: sshCommand(cfg.SSHHost, cfg.SSHPort),
		consoleURL: cfg.ConsoleURL,
		proxyURL:   sandboxProxyURL(cfg.Domain),
		proxyPort:  cfg.ProxyPort,
		track:      cfg.Track,
		dial:       cfg.Dial,
		log:        cfg.Log,
	}
	h.open = h.dialPTY

	require := edgeauth.Require(cfg.Sessions, cfg.Accounts, cfg.LoginURL)
	mux := http.NewServeMux()
	// Assets are ungated on purpose: they are vendored MIT libraries with
	// nothing sandbox-specific in them, and gating them would make the page
	// render blank rather than redirect while a session expires mid-load.
	mux.Handle("GET /assets/{file}", http.StripPrefix("/assets", assetServer()))
	mux.Handle("GET /{$}", require(http.HandlerFunc(h.page)))
	mux.Handle("GET /ws", require(http.HandlerFunc(h.ws)))
	mux.Handle("GET /vitals", require(http.HandlerFunc(h.vitals)))
	mux.Handle("POST /turbo", require(http.HandlerFunc(h.turbo)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sparkbox: not found", http.StatusNotFound)
	})
	h.mux = mux
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// Handler returns h itself, matching the shape main.go already uses to mount
// the consoles (px.SetReserved(sub, uc.Handler())).
func (h *Handler) Handler() http.Handler { return h }

// Subdomain is the reserved label this handler answers under, so the edge and
// the collision warnings in main quote the handler rather than a second copy of
// the string.
func (h *Handler) Subdomain() string { return h.subdomain }

// sshCommand builds the `ssh …` line for a sandbox name, or nil when this host
// advertises no SSH endpoint to put in one.
//
// A closure over the configuration rather than a method taking three arguments,
// so the "is there anything to show" question is answered once, at startup, and
// the page's data path is a nil check.
//
// The port is omitted at 22 and at 0. Printing `-p 22` is not wrong, but this
// string exists to be pasted into a terminal by somebody who has never used
// this host, and every token in it that does not have to be there is one more
// thing to wonder about. 0 means main could not parse a listen address, which
// is a fact about the process and not a port anybody should type.
func sshCommand(host string, port int) func(string) string {
	if host == "" {
		return nil
	}
	prefix := "ssh "
	if port != 0 && port != 22 {
		prefix = "ssh -p " + strconv.Itoa(port) + " "
	}
	return func(sandbox string) string { return prefix + sandbox + "@" + host }
}

// sandboxProxyURL builds the stable, portless URL whose route store selects
// the sandbox's current default port. The public proxy is an HTTPS product
// surface even when a local development process has TLS termination disabled,
// matching the console and launch URLs main advertises.
func sandboxProxyURL(domain string) func(string) string {
	domain = strings.ToLower(strings.Trim(domain, "."))
	if domain == "" {
		return nil
	}
	return func(sandbox string) string {
		return "https://" + sandbox + "." + domain + "/"
	}
}

// SandboxName reads the target sandbox out of a request host.
//
// The edge dispatches by suffix, so the host is always
// <name>-<subdomain>.<zone>: one label, because that is the only shape a
// zone's existing *.<zone> wildcard certificate covers (see
// proxy.SetReservedSuffix for why that matters). The zone is checked when
// configured: without it a request forged with `Host: victim-xterm.evil.example`
// would still resolve, and while the ownership check would still hold, the
// WebSocket's origin gate compares against this same host and must not be
// talked into accepting a foreign one.
//
// Anything before a dot in the first label is rejected rather than trimmed. The
// edge hands "a.demo" to us for a.demo-xterm.<zone>, and answering that host as
// "demo" would give one sandbox two origins — which is exactly what the
// WebSocket origin gate assumes cannot happen.
func (h *Handler) SandboxName(host string) (string, bool) {
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		host = hostOnly
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	first, zone, found := strings.Cut(host, ".")
	if !found {
		return "", false
	}
	if h.domain != "" && zone != h.domain {
		return "", false
	}
	name, ok := strings.CutSuffix(first, "-"+h.subdomain)
	if !ok {
		return "", false
	}
	// The manager's own create-time charset, asked of the one predicate that
	// owns it rather than re-implemented here. Enforcing it keeps a hostile Host
	// header from reaching the manager as a lookup key at all, and because it is
	// literally the same rule, a name the manager would accept can never be
	// rejected here.
	if !reserved.ValidLabel(name) {
		return "", false
	}
	return name, true
}

// resolve is the single owner gate every entry point passes through. Missing
// and not-yours produce the identical answer — a 404 with the message a
// genuinely absent sandbox gets — so a stranger can neither confirm a name nor,
// because this runs strictly before EnsureRunning, wake someone else's VM.
//
// Strict owner, with no operator bypass, and that is deliberate. Operators do
// get one on the metadata surfaces (proxy.mayView, the user console), but a PTY
// is a different class of authority: it hands over the owner's credentials,
// agent tokens and repos, on a cookie alone, with no SSH key or passkey
// re-proof. No other door to this feature grants it — ctlops.owned has no
// operator concept, so the REST terminal 404s, and sshgw's `ssh <name>.<domain>`
// compares owners with no operator branch either — and openapi.json documents
// the two as the same session. Three doors, one rule.
func (h *Handler) resolve(w http.ResponseWriter, r *http.Request) (*host.Sandbox, bool) {
	name, ok := h.SandboxName(r.Host)
	if !ok {
		http.Error(w, "sparkbox: no sandbox named here", http.StatusNotFound)
		return nil, false
	}
	sess, _ := edgeauth.From(r.Context())
	box, found := h.mgr.Get(name)
	if !found || box.Owner != sess.Handle {
		notFound(w, name)
		return nil, false
	}
	return box, true
}

func notFound(w http.ResponseWriter, name string) {
	http.Error(w, fmt.Sprintf("sparkbox: no sandbox named %q", name), http.StatusNotFound)
}

// page serves the terminal itself. It resolves and owner-checks first so a
// non-owner gets a 404 instead of a page that would only fail at the upgrade —
// the sandbox's very existence is the secret being kept.
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	box, ok := h.resolve(w, r)
	if !ok {
		return
	}
	hdr := w.Header()
	// First-party CSP. 'unsafe-inline' is needed twice over: the page inlines
	// its own style and script the way the consoles do, and xterm.js sets
	// inline styles on every row it renders. connect-src 'self' is what lets
	// the WebSocket back to this same host through.
	//
	// frame-ancestors is spelled out because it does NOT fall back to
	// default-src, and this is the one document in the product that turns
	// keystrokes into commands in a root-capable shell. The session cookie's
	// Domain is ".<zone>", so a page served from a sandbox's own web route —
	// arbitrary user code on a same-site origin — could frame this page, ride
	// the visitor's own cookie past the owner check and the Origin gate (the
	// framed document's origin IS this host), overlay a decoy, and typejack the
	// shell blind. The page is a full-viewport terminal; nothing ever needs to
	// embed it.
	hdr.Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
			"script-src 'self' 'unsafe-inline'; connect-src 'self'; base-uri 'none'; "+
			"form-action 'self'; frame-ancestors 'none'")
	// The pre-CSP spelling of the same rule, for browsers that predate
	// frame-ancestors. Belt and braces on a rule worth two implementations.
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("Cache-Control", "no-store")
	// The page is a shell around a WebSocket, so the sandbox name is the only
	// state it needs and it comes from the host it was served on — but a
	// referrer carrying that name to a link the user clicks inside the terminal
	// would leak which sandbox they are on to a third party.
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("X-Content-Type-Options", "nosniff")
	h.log.Debug("terminal page", "sandbox", box.Name)
	indexPage.ServeHTTP(w, r)
}
