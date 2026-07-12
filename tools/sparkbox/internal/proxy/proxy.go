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
	"fmt"
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

type ctxKey int

const targetKey ctxKey = iota

type Server struct {
	mgr    *host.Manager
	store  *routes.Store
	domain string // base domain, e.g. "hivemind.tools"
	log    *slog.Logger
	rp     *httputil.ReverseProxy
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
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, "sparkbox: upstream not reachable — is your app listening on the forwarded port?\n(%v)\n", err)
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
	route, ok, err := s.store.GetBySubdomain(sub)
	if err != nil {
		s.log.Error("route lookup failed", "subdomain", sub, "err", err)
		http.Error(w, "sparkbox: route lookup failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, fmt.Sprintf("sparkbox: no route for %s.%s", sub, s.domain), http.StatusNotFound)
		return
	}

	// Resume-on-connect: bring the sandbox up if it was reaped, and keep it
	// marked active so the reaper leaves it alone while it's serving traffic.
	ctx, cancel := context.WithTimeout(r.Context(), resumeTimeout)
	defer cancel()
	box, err := s.mgr.EnsureRunning(ctx, route.Sandbox)
	if err != nil {
		s.log.Warn("resume for proxy failed", "sandbox", route.Sandbox, "err", err)
		http.Error(w, fmt.Sprintf("sparkbox: sandbox %q unavailable: %v", route.Sandbox, err), http.StatusBadGateway)
		return
	}
	if box.HostIP == "" {
		http.Error(w, "sparkbox: sandbox has no network address", http.StatusBadGateway)
		return
	}

	target := &url.URL{Scheme: "http", Host: net.JoinHostPort(box.HostIP, strconv.Itoa(route.Port))}
	r = r.WithContext(context.WithValue(r.Context(), targetKey, target))
	s.rp.ServeHTTP(w, r)
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
