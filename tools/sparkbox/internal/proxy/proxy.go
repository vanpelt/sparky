// Package proxy is the HTTP edge: it maps <subdomain>.<domain> (e.g.
// myvm.hivemind.tools) to a sandbox VM's guest IP and a configured port, resuming
// the sandbox on demand (resume-on-connect, like the SSH gateway) before
// reverse-proxying the request through.
//
// Routes come from internal/routes (sqlite). A sandbox named "myvm" gets a
// default route myvm -> :8000 at create time, so it is reachable at
// myvm.hivemind.tools with no extra setup; users can add routes on other
// subdomains or ports via the control API.
//
// A route is public or private (routes.Visibility). Public routes are
// unauthenticated web previews — whatever the sandbox serves, the same model as
// exe.dev's per-sandbox URLs. Private routes (the default) are gated: a visitor
// must present a session identifying a handle that owns the sandbox, or an
// operator, and that identity is forwarded upstream as X-Forwarded-* headers.
// Gating is active only when SetAuth has wired a session verifier; without it
// (local/mock runs) every route serves openly.
package proxy

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// resumeTimeout bounds how long a request will wait for a paused sandbox to
// come back before giving up (a cold firecracker boot is well under this).
const resumeTimeout = 30 * time.Second

//go:embed error.html
var errorHTML string

// errorTpl renders proxy error responses for browsers; API clients (no
// text/html in Accept) get the plain-text equivalent.
var errorTpl = template.Must(template.New("error").Parse(errorHTML))

type ctxKey int

const (
	targetKey   ctxKey = iota
	routeKey           // the routes.Route being served, for error reporting
	identityKey        // the authenticated visitor (edgeauth.Identity), for header injection
	portKey            // the original dialed port recovered below TLS (SO_ORIGINAL_DST)
	suffixKey          // the labels preceding a reserved suffix (see SetReservedSuffix)
)

// newUpstreamTransport dials guest apps. It is deliberately not http.DefaultTransport:
//
//   - MaxIdleConnsPerHost defaults to 2, which for a proxy means the third
//     concurrent request to the same guest pays a fresh TCP handshake. A single
//     page load of a dev server easily exceeds that.
//   - ResponseHeaderTimeout stays unset: a guest app is allowed to think for as
//     long as it likes (long-poll, a slow first compile) without the edge
//     deciding it is dead.
//   - Compression is passed through rather than managed. The client's
//     Accept-Encoding reaches the app verbatim and the app's encoded bytes reach
//     the client untouched — no transparent gunzip that would desynchronise
//     Content-Encoding from the body.
//
// It is built per Server rather than shared package-wide because the idle
// connection pool is keyed on the target host string alone: two Servers with
// different dialers sharing one transport would hand each other's pooled
// connections out.
//
// The result must stay a concrete *http.Transport and must never be wrapped in
// a RoundTripper: httputil.ReverseProxy's 101-upgrade path requires the
// response Body to implement io.ReadWriteCloser, and only *http.Transport
// returns one — a wrapper turns every WebSocket into "101 switching protocols
// response with non-writable body".
func newUpstreamTransport(dial Dialer) *http.Transport {
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	return &http.Transport{
		DialContext:           dial,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
	}
}

// Resumer is the one manager method the edge drives: every request is a
// resume-on-connect. It is an interface rather than *host.Manager so the edge
// can be handed a fleet router that forwards to whichever machine holds the
// sandbox; a single-box deployment passes its *host.Manager and nothing about
// the path changes.
type Resumer interface {
	Get(name string) (*host.Sandbox, bool)
	EnsureReady(ctx context.Context, name string) (*host.Sandbox, error)
	MarkActive(name string)
}

// Dialer is net.Dialer.DialContext's shape — see SetDialer.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Accounts is the slice of the user store the edge needs to authorise a
// visitor: resolving a handle to its operator status and email. *users.Store
// satisfies it.
type Accounts interface {
	Get(handle string) (users.User, error)
}

// WithOriginalPort stamps the pre-DNAT destination port on a connection's
// context. The edge listens on one port but iptables REDIRECTs the whole
// private-port range to it; this is how a request learns it was dialed on
// :4444. Set from the http.Server's ConnContext (see cmd/sparkbox).
func WithOriginalPort(ctx context.Context, port int) context.Context {
	return context.WithValue(ctx, portKey, port)
}

type Server struct {
	mgr    Resumer
	store  *routes.Store
	domain string // base domain, e.g. "hivemind.tools"
	log    *slog.Logger
	rp     *httputil.ReverseProxy

	// transport carries every upstream request and owns the idle-connection
	// pool. SetDialer replaces it wholesale.
	transport *http.Transport
	metrics   *fleetmetrics.Registry

	// reserved maps subdomains owned by built-in handlers (operator console,
	// OIDC issuer, login, user console) — checked before route lookup, so a
	// sandbox route can never shadow them (see SetReserved).
	reserved map[string]http.Handler

	// reservedSuffix maps a trailing name segment to the handler that owns
	// every subdomain ending in "-<segment>" (the browser terminal). Also
	// checked before route lookup — see SetReservedSuffix.
	reservedSuffix map[string]http.Handler

	// login/session/accounts, if set (via SetAuth), turn on the private-route
	// gate: login serves the browser sign-in at <loginSub>.<domain>, session
	// verifies the cookie/bearer token, and accounts authorises the handle.
	login    http.Handler
	loginSub string
	session  *edgeauth.Signer
	accounts Accounts

	// listenPort is the edge's own listen port (e.g. 443). A request whose
	// recovered/Host port equals it means "dialed the edge directly" — the
	// default web route — not "forward to guest:<listenPort>".
	listenPort int

	// home is the subdomain a bare <domain> request is redirected to (see
	// SetHome). Empty leaves the apex answering a 404.
	home string
}

// SetListenPort records the edge's own listen port so targetPort can tell a
// direct hit from an any-port URL. Optional; 0 disables the check.
func (s *Server) SetListenPort(p int) { s.listenPort = p }

// SetAuth turns on authenticated forwarding. loginSub reserves a subdomain for
// the browser sign-in handler; session verifies visitor tokens; accounts maps a
// handle to its record (operator status, email). Until this is called, every
// route — even one marked private — serves without a gate, which is what a
// local mock run wants. Call once before serving.
func (s *Server) SetAuth(loginSub string, login http.Handler, session *edgeauth.Signer, accounts Accounts) {
	s.loginSub = strings.ToLower(loginSub)
	s.login = login
	s.session = session
	s.accounts = accounts
	if login != nil {
		s.SetReserved(loginSub, login)
	}
}

// SetReserved dedicates a subdomain to a built-in handler: requests for
// <sub>.<domain> are served by h instead of being looked up as a sandbox
// route, so a sandbox can never shadow it. Call before serving.
func (s *Server) SetReserved(sub string, h http.Handler) {
	if s.reserved == nil {
		s.reserved = make(map[string]http.Handler)
	}
	s.reserved[strings.ToLower(sub)] = h
}

// SetReservedSuffix dedicates a family of subdomains to a built-in handler:
// every request for <name>-<label>.<domain> is served by h, with <name>
// recoverable through SuffixName, instead of being looked up as a sandbox
// route. This is how the browser terminal gets one host per sandbox off a
// single handler.
//
// The separator is a HYPHEN, not a dot, and that is the whole reason this
// feature is reachable from an ordinary browser. A wildcard matches exactly one
// label — RFC 4592 in DNS, RFC 6125 in certificates — so a dotted
// <name>.xterm.<domain> is covered by neither the zone's wildcard record nor
// its wildcard certificate, and needs a second one of each. Hosted edges that
// terminate TLS in front of us (Cloudflare's universal certificate is the case
// that bit us) will not issue that second wildcard without a paid add-on, and
// the failure lands inside the TLS handshake: the browser shows
// ERR_SSL_VERSION_OR_CIPHER_MISMATCH and sparkbox logs nothing at all, so it
// reads like a DNS bug for hours. Keeping the terminal in ONE label puts it
// under the *.<domain> wildcard that already exists for every sandbox.
//
// It cannot be expressed with SetReserved because the name varies per sandbox:
// subdomainOf returns "demo-xterm", not "xterm", so an exact-match map never
// sees these hosts at all.
//
// Dispatch runs before the route lookup, and that ordering is the security
// property, not a nicety: routes.ValidSubdomain permits both hyphens and dots
// (the advertised `web-myvm` and `api.myvm` shapes), so a route row literally
// named "demo-xterm" is creatable today, and a route-first edge would let its
// owner serve another user's terminal host. For the same reason any subdomain
// ENDING in "-<label>" is claimed — even a deeper "a.demo-xterm.<domain>",
// which h answers for by rejecting the name rather than falling through to a
// route that could have been squatted.
//
// The claim also means a sandbox or route whose own name ends in "-<label>"
// goes dark; host.Manager and the route store refuse to create one, and main
// warns about any that predate the reservation. Call before serving.
func (s *Server) SetReservedSuffix(label string, h http.Handler) {
	if s.reservedSuffix == nil {
		s.reservedSuffix = make(map[string]http.Handler)
	}
	s.reservedSuffix[strings.ToLower(label)] = h
}

// SuffixName returns the name that preceded the reserved suffix this request
// was dispatched on: "demo" for demo-xterm.<domain>. ok is false for any
// request that did not arrive through SetReservedSuffix dispatch, which is how
// such a handler distinguishes "mounted on the edge" from "reached directly"
// (a test server, a future loopback mount) and can pick its own host parsing.
func SuffixName(r *http.Request) (string, bool) {
	name, ok := r.Context().Value(suffixKey).(string)
	return name, ok
}

// suffixHandler resolves sub against the reserved-suffix registry, splitting
// "demo-xterm" into the handler for "xterm" and the name "demo". The reserved
// segment is always the LAST one, so the split is on the final hyphen — which
// is also why a sandbox name may contain hyphens ("crafty-axolotl-xterm" splits
// to "crafty-axolotl") without the two conventions colliding.
func (s *Server) suffixHandler(sub string) (http.Handler, string, bool) {
	i := strings.LastIndexByte(sub, '-')
	if i <= 0 { // no hyphen, or an empty name — not a terminal host
		return nil, "", false
	}
	h, ok := s.reservedSuffix[sub[i+1:]]
	if !ok {
		return nil, "", false
	}
	return h, sub[:i], true
}

// SetConsole reserves a subdomain (e.g. "console") for the operator console,
// served by h rather than proxied to a sandbox. Call once before serving.
func (s *Server) SetConsole(sub string, h http.Handler) { s.SetReserved(sub, h) }

// SetIssuer reserves a subdomain (e.g. "oidc") for the OIDC issuer's discovery
// document and JWKS, served by h rather than proxied to a sandbox. Call once
// before serving.
//
// It lives on the proxy edge because a verifier requires the issuer to be
// reachable over public https — it fetches the discovery document, follows
// jwks_uri, and refuses anything that isn't a public address. The edge already
// terminates TLS for the wildcard, so this is two GET handlers, not a new
// listener.
func (s *Server) SetIssuer(sub string, h http.Handler) { s.SetReserved(sub, h) }

// SetHome names the subdomain the zone apex sends visitors to. Without it,
// https://<domain>/ answers "request host is not under <domain>" — technically
// true and useless, since the apex is the one hostname a person types from
// memory. Empty (the default) keeps that behaviour. Call before serving.
//
// A redirect rather than serving the console at two hostnames: the session
// cookie, the WebSocket origin gate and every absolute URL the console builds
// are all scoped to one host, and a second origin serving the same page is a
// second place for those to disagree.
func (s *Server) SetHome(sub string) { s.home = strings.ToLower(sub) }

// redirectApex sends a bare <domain> request to the home subdomain, reporting
// whether it handled the request. Path and query survive, so a link someone
// trimmed back to the apex still lands where it was going.
//
// 302, not 301: this is "where the front page lives for now", and a permanent
// redirect is cached by browsers past the point where we can take it back.
func (s *Server) redirectApex(w http.ResponseWriter, r *http.Request) bool {
	if s.home == "" {
		return false
	}
	h := strings.ToLower(r.Host)
	port := ""
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h, port = h[:i], h[i:]
	}
	if strings.TrimSuffix(h, ".") != s.domain {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Behind a TLS-terminating front end (the tunnel) the connection is plain,
	// so the header is what knows. Same rule as the terminal's origin check.
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme = strings.ToLower(strings.TrimSpace(strings.Split(fwd, ",")[0]))
	}
	http.Redirect(w, r, scheme+"://"+s.home+"."+s.domain+port+r.URL.RequestURI(), http.StatusFound)
	return true
}

func New(mgr Resumer, store *routes.Store, domain string, log *slog.Logger) *Server {
	s := &Server{
		mgr:       mgr,
		store:     store,
		domain:    strings.ToLower(strings.TrimPrefix(domain, ".")),
		log:       log,
		transport: newUpstreamTransport(nil),
	}
	s.rp = &httputil.ReverseProxy{
		Transport: s.transport,
		// -1 means "flush after every write", i.e. no response buffering at all.
		// The stdlib default only streams eagerly for text/event-stream and for
		// unknown-length bodies, which quietly stalls anything else that trickles:
		// a chunked log tail, an LLM token stream that sets its own Content-Type,
		// a progress endpoint. This edge fronts one guest app for one user, so
		// there is no throughput case for buffering, and "whatever the app writes
		// appears when it writes it" is the behaviour a web app author expects.
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			target, _ := pr.In.Context().Value(targetKey).(*url.URL)
			pr.SetURL(target)
			pr.SetXForwarded()
			// Preserve the client-facing host so apps that key off Host (or
			// build absolute URLs) see myvm.hivemind.tools, not the guest IP.
			pr.Out.Host = pr.In.Host
			// Identity headers: always strip any client-supplied copies first so
			// a request can't spoof them, then set our own from the verified
			// session. The names are the oauth2-proxy convention, so upstreams
			// already written for that ecosystem work unmodified. A public route
			// carries no identity, so all three stay absent (never blank).
			for _, h := range []string{"X-Forwarded-User", "X-Forwarded-Email", "X-Forwarded-Preferred-Username"} {
				pr.Out.Header.Del(h)
			}
			if id, ok := pr.In.Context().Value(identityKey).(edgeauth.Identity); ok {
				pr.Out.Header.Set("X-Forwarded-User", id.Handle)
				pr.Out.Header.Set("X-Forwarded-Preferred-Username", id.Handle)
				if id.Email != "" {
					pr.Out.Header.Set("X-Forwarded-Email", id.Email)
				}
			}
			// The zone-wide session cookie must never reach a guest: it is valid
			// on every subdomain, so forwarding it would hand each sandbox app its
			// visitors' session tokens. Only that pair is removed, by textual
			// surgery rather than a parse/re-serialize round-trip — Go's cookie
			// parser silently drops or re-quotes pairs it can't round-trip (raw
			// UTF-8 values, non-token names), and a guest app's cookies must
			// survive byte-for-byte. A header without the pair stays untouched.
			if lines := pr.Out.Header.Values("Cookie"); len(lines) > 0 {
				if kept, changed := stripSessionCookie(lines); changed {
					pr.Out.Header.Del("Cookie")
					for _, l := range kept {
						pr.Out.Header.Add("Cookie", l)
					}
				}
			}
			// The same session token is also accepted as an Authorization: Bearer
			// credential (edgeauth.IdentityFrom), so that channel is closed too:
			// a bearer value carrying the session-token prefix is stripped —
			// valid, stale, or forged alike — while a guest app's own bearer
			// tokens, which never carry it, pass through untouched.
			if tok, ok := strings.CutPrefix(pr.Out.Header.Get("Authorization"), "Bearer "); ok {
				if strings.HasPrefix(strings.TrimSpace(tok), edgeauth.TokenPrefix) {
					pr.Out.Header.Del("Authorization")
				}
			}
		},
		// ErrorHandler only ever runs before anything has been written to the
		// client: the stdlib handles a failure part-way through a body by
		// panicking with http.ErrAbortHandler (aborting the connection) rather
		// than calling here, so this is always free to write a fresh response.
		// Do not add a "have we started?" guard — the upgrade-negotiation errors
		// that reach this path deserve a real 502, and suppressing them would
		// leave a failed websocket handshake hanging with no answer.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("proxy upstream error", "host", r.Host, "err", err)
			route, _ := r.Context().Value(routeKey).(routes.Route)
			if s.machineFailed(w, r, route, err) {
				return
			}
			s.errorPage(w, r, http.StatusBadGateway,
				fmt.Sprintf("Nothing is listening on port %d", route.Port),
				// The underlying dial error goes to the log, not the page: on a
				// public route this is served to strangers, and it would spell
				// out the guest's internal address for them.
				fmt.Sprintf("Sandbox %s is running, but nothing answered on port %d.", route.Sandbox, route.Port),
				s.notListeningHint(r, route.Port))
		},
	}
	return s
}

// SetDialer routes upstream connections through d instead of a plain TCP dial,
// which is how a sandbox living on another machine is reached: the dialer sees
// the synthetic host name in the target URL and turns it into a stream to that
// machine. It rebuilds the transport, dropping any pooled connections with it,
// so call it before the server starts serving.
func (s *Server) SetDialer(d Dialer) {
	s.transport = newUpstreamTransport(d)
	s.rp.Transport = s.transport
}

// SetMetrics enables bounded warm/cold upstream first-byte observations.
func (s *Server) SetMetrics(metrics *fleetmetrics.Registry) { s.metrics = metrics }

// ---------------------------------------------------------------------------
// When the failure is the machine, not the app
// ---------------------------------------------------------------------------
//
// Everything the edge could not reach used to render as one page: 502, "nothing
// is listening on port N". On a single box that was always true — the only
// thing between the edge and the guest was the guest. In a fleet there are two
// more things in the path, and telling a user their app is not listening when
// the machine holding their sandbox is simply offline sends them to debug a
// program that is running perfectly well on a computer that is asleep.
//
// So there are three answers now, and the SEPARATION is the point:
//
//   - 503, the machine is offline — the link to the node holding this sandbox
//     is down. Nothing was tried and nothing is wrong with the sandbox.
//   - 503, the sandbox is not running there — the machine answered and refused:
//     it has no such sandbox, or the box is paused. (Between the resume above
//     and the dial, a reaper on the far machine can pause it.)
//   - 502, nothing is listening — the machine reached the guest and the guest
//     refused the port. This is the original page and the original wording,
//     because it is the original situation.
//
// The node's name appears ONLY for a request that carried an authenticated
// identity. A public route is served to strangers who merely typed a URL, and
// which machine holds which sandbox is fleet topology: an outage must not
// become a free map of the deployment. This is the same rule the 502 page has
// always followed by keeping the dial error in the log.

// machineFailed answers an upstream failure that is about the machine rather
// than the guest app, and reports whether it wrote a response. False means the
// caller renders its own page — which for every single-box deployment is every
// time, since neither branch can be reached without a node in the path.
func (s *Server) machineFailed(w http.ResponseWriter, r *http.Request, route routes.Route, err error) bool {
	if ctlops.IsNodeUnreachable(err) {
		s.errorPage(w, r, http.StatusServiceUnavailable,
			"The machine hosting this sandbox is offline",
			fmt.Sprintf("Sandbox %s runs on another machine%s, which is not answering right now.",
				route.Sandbox, s.nodeAside(r, err)),
			"Nothing has been lost. It is reachable again as soon as that machine reconnects.")
		return true
	}
	// A refusal from the node itself, typed rather than described: the stream
	// never got as far as the guest. internal/nodelink refuses with Prohibited
	// when it has no such sandbox or the box is not running, and with
	// ConnectionFailed when it did dial the guest and the guest said no — which
	// is the 502 below and must stay there.
	var refused *xssh.OpenChannelError
	if errors.As(err, &refused) && refused.Reason == xssh.Prohibited {
		s.errorPage(w, r, http.StatusServiceUnavailable,
			"The sandbox isn't running on its machine",
			fmt.Sprintf("Sandbox %s did not come up on the machine that holds it.", route.Sandbox),
			"Try again in a moment; if it persists, `ssh ctl@ list` shows what that machine thinks it has.")
		return true
	}
	return false
}

// becauseFor renders ": <reason>" — the sentence explaining why a resume
// failed — and withholds it only in the one case where it would say something
// the reader is not entitled to.
//
// The reasons this page has always carried are the user's own situation: over
// their running limit, host at capacity. Those name no machine and stay, so a
// single-box deployment's page is byte-identical to what it always was. The
// router's sentences are different in kind — "sandbox %q lives on node %q,
// which is offline", "is not on node %q any more" — and are withheld from a
// visitor the edge has not authenticated, because a public URL is served to
// strangers and which machine holds whose work is not theirs to collect. The
// reason always reaches the log either way.
func (s *Server) becauseFor(r *http.Request, err error) string {
	if node, ok := ctlops.MachineNamed(err); ok && node != "" {
		if _, signedIn := r.Context().Value(identityKey).(edgeauth.Identity); !signedIn {
			return ""
		}
	}
	return ": " + err.Error()
}

// nodeAside renders " (node \"b\")" for a caller the edge has authenticated,
// and nothing at all for anyone else. See the section comment above: a node
// name is topology, and the owner of the sandbox is the only visitor entitled
// to it.
func (s *Server) nodeAside(r *http.Request, err error) string {
	if _, ok := r.Context().Value(identityKey).(edgeauth.Identity); !ok {
		return ""
	}
	node, ok := ctlops.UnreachableNode(err)
	if !ok {
		return ""
	}
	return fmt.Sprintf(" (node %q)", node)
}

// notListeningHint is the advice on the 502 page, and the closest thing sparkbox
// has to onboarding: this URL is the first thing a new sandbox shows its owner.
// It answers the three questions that actually come up, in the order they bite.
func (s *Server) notListeningHint(r *http.Request, port int) template.HTML {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = template.HTMLEscapeString(host)
	return template.HTML(fmt.Sprintf(
		`<p><strong>Bind 0.0.0.0, not 127.0.0.1.</strong> The edge reaches your app `+
			`across the sandbox's network interface, so a loopback-only listener is `+
			`invisible from out here — this is the usual cause.</p>`+
			`<p><strong>Nothing is reserved.</strong> Port %d is simply where this URL `+
			`forwards by default; no sparkbox process is holding it. Start your app and `+
			`reload.</p>`+
			`<p><strong>Any other port works too</strong>, with no configuration: put it `+
			`in the URL, e.g. <code>https://%s:5173</code>.</p>`,
		port, host))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.subdomainOf(r.Host)
	if !ok {
		// The apex is not a subdomain, so it lands here rather than in any
		// lookup below — the one hostname with somewhere better to be.
		if s.redirectApex(w, r) {
			return
		}
		http.Error(w, "sparkbox: request host is not under "+s.domain, http.StatusNotFound)
		return
	}
	// Reserved subdomains (operator console, OIDC issuer, login, user console)
	// belong to built-in handlers and are not sandbox routes.
	if h, ok := s.reserved[sub]; ok {
		h.ServeHTTP(w, r)
		return
	}
	// A reserved suffix owns every name ending in it, so <name>-xterm.<domain>
	// reaches the terminal handler with <name> attached. Exact reservations are
	// consulted first (they are the more specific claim), but both must precede
	// the route lookup or a route row could shadow the terminal.
	if h, name, ok := s.suffixHandler(sub); ok {
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), suffixKey, name)))
		return
	}
	route, ok, err := s.store.GetBySubdomain(sub)
	if err != nil {
		s.log.Error("route lookup failed", "subdomain", sub, "err", err)
		s.errorPage(w, r, http.StatusInternalServerError, "Route lookup failed",
			"The proxy couldn't consult its routing table. This is a sparkbox fault, not yours.", "")
		return
	}
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "Nothing is forwarded here",
			fmt.Sprintf("%s.%s isn't mapped to any sandbox port.", sub, s.domain),
			template.HTML(fmt.Sprintf(
				"Create a sandbox with <code>ssh new@%s</code> (it gets this URL automatically), or add a route to an existing sandbox via the routes API.",
				template.HTMLEscapeString(s.domain))))
		return
	}

	// The auth gate runs BEFORE resume-on-connect: an unauthenticated hit on a
	// paused private sandbox must redirect to login without waking the VM, so
	// the gate is never a free way to spin up someone else's box.
	ctxr := r.Context()
	if id, gated := s.authorize(w, r, route); gated {
		return // authorize already wrote the redirect/401/403
	} else if id != nil {
		ctxr = context.WithValue(ctxr, identityKey, *id)
		// Rebound here rather than only just before ServeHTTP, because the
		// error pages between here and there have to know whether they are
		// talking to somebody who signed in: it is what decides whether a
		// failure may name the machine a sandbox lives on. See machineFailed.
		r = r.WithContext(ctxr)
	}

	// The forwarded port comes from the URL (…:4444) when the visitor named one,
	// otherwise the route's configured port. This is what makes any-port URLs
	// work with no per-port route row.
	port := s.targetPort(r, route)

	// Resume-on-connect: bring the sandbox up if it was reaped, and keep it
	// marked active so the reaper leaves it alone while it's serving traffic.
	temperature := "cold"
	if cached, ok := s.mgr.Get(route.Sandbox); ok && cached.State == vmm.StateRunning {
		temperature = "warm"
	}
	ttfbStarted := time.Now()
	ctx, cancel := context.WithTimeout(ctxr, resumeTimeout)
	defer cancel()
	box, err := host.Prepare(ctx, s.mgr, route.Sandbox)
	if err != nil {
		s.log.Warn("resume for proxy failed", "sandbox", route.Sandbox, "err", err)
		if s.machineFailed(w, r, route, err) {
			return
		}
		s.errorPage(w, r, http.StatusBadGateway, "The sandbox couldn't be started",
			// Only a caller the edge authenticated is told WHY. The reason is
			// a curated sentence for the failures a user can act on (at
			// capacity, over their limit) but it is also the router's, and the
			// router's sentences name machines: `sandbox %q is not on node %q
			// any more` is a true and useful thing to tell an owner and a free
			// map of the deployment to hand a stranger who typed a public URL.
			fmt.Sprintf("Sandbox %q exists but didn't come up%s.", route.Sandbox, s.becauseFor(r, err)),
			"Retry in a moment — if the host is at capacity, pausing another sandbox frees room.")
		return
	}
	if box.HostIP == "" {
		s.errorPage(w, r, http.StatusBadGateway, "The sandbox has no network address",
			fmt.Sprintf("Sandbox %q is up but reported no guest IP to forward to.", route.Sandbox), "")
		return
	}

	// HostIP is whatever the record says, and for a sandbox on another machine
	// that is a synthetic per-sandbox name rather than an address. Both the dial
	// and the idle-connection pool are keyed on this string, and that is load
	// bearing: every machine mints the same guest IPs, so a shared address would
	// let one machine's request be answered over a pooled connection to
	// another's sandbox — a cross-tenant bleed with no error and no log line.
	// The guest never sees the name: Rewrite restores the client's Host.
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(box.HostIP, strconv.Itoa(port))}
	// Report the actually-forwarded port in any upstream error, not the route's
	// default, since an any-port URL can override it.
	route.Port = port
	ctx2 := context.WithValue(ctxr, targetKey, target)
	ctx2 = httptrace.WithClientTrace(ctx2, &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			s.metrics.ObserveProxyTTFB(box.Node, temperature, time.Since(ttfbStarted))
		},
	})
	r = r.WithContext(context.WithValue(ctx2, routeKey, route))
	s.rp.ServeHTTP(w, r)
}

// stripSessionCookie removes the edge session cookie pair from Cookie header
// lines, leaving every other pair byte-for-byte intact — the stdlib parser
// (Cookies/AddCookie) would drop pairs with non-token names or non-ASCII
// values and re-quote ones with spaces. It returns the surviving lines (a line
// whose only pair was the session cookie disappears entirely) and whether
// anything was removed; when nothing was, the input lines come back unchanged.
func stripSessionCookie(lines []string) (kept []string, changed bool) {
	for _, line := range lines {
		parts := strings.Split(line, ";")
		surviving := parts[:0]
		hit := false
		for _, part := range parts {
			name, _, _ := strings.Cut(part, "=")
			if strings.TrimSpace(name) == edgeauth.CookieName {
				hit = true
				continue
			}
			surviving = append(surviving, part)
		}
		switch {
		case !hit:
			kept = append(kept, line)
		case len(surviving) > 0:
			changed = true
			kept = append(kept, strings.TrimSpace(strings.Join(surviving, ";")))
		default:
			changed = true
		}
	}
	return kept, changed
}

// targetPort resolves which guest port to forward to. Preference order:
// the pre-DNAT port recovered below TLS (authoritative when iptables REDIRECT
// funnelled an any-port URL in), then an explicit port in the Host header
// (covers direct binds and non-Linux dev), then the route's configured port.
// An original destination equal to the edge's listen port is ignored: a local
// direct dial names the default route. An explicit Host/:authority port remains
// authoritative even when it happens to equal the internal listen port. That
// distinction lets a load balancer expose guest :8081 while targeting the
// gateway's own :8081 listener.
func (s *Server) targetPort(r *http.Request, route routes.Route) int {
	if p, ok := r.Context().Value(portKey).(int); ok && p > 0 && p != s.listenPort {
		return p
	}
	if _, portStr, err := net.SplitHostPort(r.Host); err == nil {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			return p
		}
	}
	return route.Port
}

// authorize enforces a route's visibility. It returns gated=true when it has
// already written a response (redirect to login, 401, or 403) and the caller
// must stop. On an allowed request it returns the verified identity (nil for a
// public route or when auth isn't wired) to forward upstream.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, route routes.Route) (id *edgeauth.Identity, gated bool) {
	// Auth not wired (local/mock) or an explicitly public route: serve openly,
	// with no identity to forward.
	if s.session == nil || route.Visibility == routes.VisibilityPublic {
		return nil, false
	}
	visitor, ok := s.session.IdentityFrom(r)
	if !ok {
		s.challenge(w, r)
		return nil, true
	}
	if !s.mayView(visitor.Handle, route) {
		// They are signed in, just not allowed here — a 403, never a redirect
		// loop back to a login that would change nothing.
		s.errorPage(w, r, http.StatusForbidden, "You don't have access to this sandbox",
			fmt.Sprintf("This URL is private to %s. You're signed in as %s.", route.Owner, visitor.Handle),
			"Ask the owner to make this port public, or sign in with an account that owns it.")
		return nil, true
	}
	return &visitor, false
}

// mayView is the picked access scope: the sandbox owner, or an operator.
func (s *Server) mayView(handle string, route routes.Route) bool {
	if handle == route.Owner {
		return true
	}
	if s.accounts == nil {
		return false
	}
	u, err := s.accounts.Get(handle)
	return err == nil && u.IsOperator()
}

// challenge sends an unauthenticated visitor to the login page (browsers) or
// answers 401 (API clients), preserving the original URL to return to.
func (s *Server) challenge(w http.ResponseWriter, r *http.Request) {
	if s.login == nil || !strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "sparkbox: authentication required — mint a token with `ssh ctl@"+s.domain+
			" session-token` and send it as `Authorization: Bearer <token>`", http.StatusUnauthorized)
		return
	}
	ret := "https://" + r.Host + r.URL.RequestURI()
	dest := "https://" + s.loginSub + "." + s.domain + "/?return=" + url.QueryEscape(ret)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// errorPage writes a friendly HTML error for browsers, or a terse text/plain
// line for everything else (curl, health checks). hint may embed markup.
func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, code int, title, message string, hint template.HTML) {
	w.Header().Set("Cache-Control", "no-store")
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Error(w, fmt.Sprintf("sparkbox: %s — %s", title, message), code)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	err := errorTpl.Execute(w, struct {
		Code          int
		Status, Title string
		Message       string
		Hint          template.HTML
	}{code, http.StatusText(code), title, message, hint})
	if err != nil {
		s.log.Error("error page render failed", "err", err)
	}
}

// subdomainOf extracts the label(s) in host that precede ".<domain>".
// "myvm.hivemind.tools:8081" with domain "hivemind.tools" -> ("myvm", true).
// A bare "hivemind.tools" or a host outside the domain returns ok=false.
func (s *Server) subdomainOf(host string) (string, bool) {
	h := strings.ToLower(host)
	if i := strings.IndexByte(h, ':'); i >= 0 { // strip :port
		h = h[:i]
	}
	suffix := "." + s.domain
	if !strings.HasSuffix(h, suffix) {
		return "", false
	}
	sub := strings.TrimSuffix(h, suffix)
	if sub == "" {
		return "", false
	}
	return sub, true
}
