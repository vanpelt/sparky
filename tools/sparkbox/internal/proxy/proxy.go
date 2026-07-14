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
// The proxy is intentionally unauthenticated: these are public web previews of
// whatever the sandbox serves, the same model as exe.dev's per-sandbox URLs.
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

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
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
	targetKey ctxKey = iota
	routeKey         // the routes.Route being served, for error reporting
)

type Server struct {
	mgr    *host.Manager
	store  *routes.Store
	domain string // base domain, e.g. "hivemind.tools"
	log    *slog.Logger
	rp     *httputil.ReverseProxy

	// console, if set, is served at <consoleSub>.<domain> instead of being
	// treated as a sandbox web route (see SetConsole).
	console    http.Handler
	consoleSub string
}

// SetConsole reserves a subdomain (e.g. "console") for the operator console,
// served by h rather than proxied to a sandbox. Call once before serving.
func (s *Server) SetConsole(sub string, h http.Handler) {
	s.consoleSub = strings.ToLower(sub)
	s.console = h
}

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
	// The operator console owns its subdomain and is not a sandbox route.
	if s.console != nil && sub == s.consoleSub {
		s.console.ServeHTTP(w, r)
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

	// Resume-on-connect: bring the sandbox up if it was reaped, and keep it
	// marked active so the reaper leaves it alone while it's serving traffic.
	ctx, cancel := context.WithTimeout(r.Context(), resumeTimeout)
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

	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(box.HostIP, strconv.Itoa(route.Port))}
	ctx2 := context.WithValue(r.Context(), targetKey, target)
	r = r.WithContext(context.WithValue(ctx2, routeKey, route))
	s.rp.ServeHTTP(w, r)
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
