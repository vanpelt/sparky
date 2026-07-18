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
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
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
)

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
	mgr    *host.Manager
	store  *routes.Store
	domain string // base domain, e.g. "hivemind.tools"
	log    *slog.Logger
	rp     *httputil.ReverseProxy

	// reserved maps subdomains owned by built-in handlers (operator console,
	// OIDC issuer, login, user console) — checked before route lookup, so a
	// sandbox route can never shadow them (see SetReserved).
	reserved map[string]http.Handler

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

func New(mgr *host.Manager, store *routes.Store, domain string, log *slog.Logger) *Server {
	s := &Server{
		mgr:    mgr,
		store:  store,
		domain: strings.ToLower(strings.TrimPrefix(domain, ".")),
		log:    log,
	}
	s.rp = &httputil.ReverseProxy{
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
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.log.Warn("proxy upstream error", "host", r.Host, "err", err)
			route, _ := r.Context().Value(routeKey).(routes.Route)
			s.errorPage(w, r, http.StatusBadGateway,
				fmt.Sprintf("Nothing is listening on port %d", route.Port),
				fmt.Sprintf("Sandbox %q is up, but nothing answered on the forwarded port (%v).", route.Sandbox, err),
				template.HTML(fmt.Sprintf(
					"Start a server inside the sandbox — e.g. <code>python3 -m http.server %d --bind 0.0.0.0</code> — or point this subdomain at another port via the routes API.",
					route.Port)))
		},
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.subdomainOf(r.Host)
	if !ok {
		http.Error(w, "sparkbox: request host is not under "+s.domain, http.StatusNotFound)
		return
	}
	// Reserved subdomains (operator console, OIDC issuer, login, user console)
	// belong to built-in handlers and are not sandbox routes.
	if h, ok := s.reserved[sub]; ok {
		h.ServeHTTP(w, r)
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
	}

	// The forwarded port comes from the URL (…:4444) when the visitor named one,
	// otherwise the route's configured port. This is what makes any-port URLs
	// work with no per-port route row.
	port := s.targetPort(r, route)

	// Resume-on-connect: bring the sandbox up if it was reaped, and keep it
	// marked active so the reaper leaves it alone while it's serving traffic.
	ctx, cancel := context.WithTimeout(ctxr, resumeTimeout)
	defer cancel()
	box, err := s.mgr.EnsureRunning(ctx, route.Sandbox)
	if err != nil {
		s.log.Warn("resume for proxy failed", "sandbox", route.Sandbox, "err", err)
		s.errorPage(w, r, http.StatusBadGateway, "The sandbox couldn't be started",
			fmt.Sprintf("Sandbox %q exists but didn't come up: %v.", route.Sandbox, err),
			"Retry in a moment — if the host is at capacity, pausing another sandbox frees room.")
		return
	}
	if box.HostIP == "" {
		s.errorPage(w, r, http.StatusBadGateway, "The sandbox has no network address",
			fmt.Sprintf("Sandbox %q is up but reported no guest IP to forward to.", route.Sandbox), "")
		return
	}

	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(box.HostIP, strconv.Itoa(port))}
	// Report the actually-forwarded port in any upstream error, not the route's
	// default, since an any-port URL can override it.
	route.Port = port
	ctx2 := context.WithValue(ctxr, targetKey, target)
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
// The edge's own listen port is ignored — dialing the edge directly means "the
// default web route", not "forward to guest:443".
func (s *Server) targetPort(r *http.Request, route routes.Route) int {
	if p, ok := r.Context().Value(portKey).(int); ok && p > 0 && p != s.listenPort {
		return p
	}
	if _, portStr, err := net.SplitHostPort(r.Host); err == nil {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 && p != s.listenPort {
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
