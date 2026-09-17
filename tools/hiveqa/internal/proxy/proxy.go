// Package proxy manages tsnet instances and reverse proxies for hiveqa stacks.
package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"tailscale.com/tsnet"
)

// TLSMode determines how the proxy terminates TLS.
type TLSMode string

const (
	TLSTailscale TLSMode = "tailscale" // Tailscale SaaS cert provisioning
	TLSACME      TLSMode = "acme"      // Self-managed ACME certs
	TLSNone      TLSMode = "none"      // Plain HTTP only
)

// Target describes a backend service to proxy to.
type Target struct {
	Service     string
	ContainerIP string
	Port        int
}

// InstanceConfig holds the configuration for creating a proxy instance.
type InstanceConfig struct {
	Name       string
	StateDir   string
	Targets    []Target
	ControlURL string  // empty = Tailscale SaaS
	TLSMode    TLSMode
	TLSConfig  *tls.Config // used when TLSMode == TLSACME
}

// Instance represents a tsnet node proxying traffic to a docker compose stack.
type Instance struct {
	cfg InstanceConfig

	server   *tsnet.Server
	httpSrv  *http.Server
	httpsSrv *http.Server
	ln80     net.Listener
	ln443    net.Listener
	fqdn     string
	mu       sync.Mutex
}

// NewInstance creates a proxy instance. Call Start to begin serving.
func NewInstance(cfg InstanceConfig) *Instance {
	return &Instance{cfg: cfg}
}

// Start initializes the tsnet server and begins proxying.
func (inst *Instance) Start(ctx context.Context) error {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	inst.server = &tsnet.Server{
		Hostname:  inst.cfg.Name,
		Dir:       inst.cfg.StateDir,
		Ephemeral: true,
	}
	if inst.cfg.ControlURL != "" {
		inst.server.ControlURL = inst.cfg.ControlURL
	}

	status, err := inst.server.Up(ctx)
	if err != nil {
		return fmt.Errorf("tsnet up: %w", err)
	}

	// Get the FQDN for logging.
	if status.Self != nil {
		inst.fqdn = strings.TrimSuffix(status.Self.DNSName, ".")
	}

	handler := inst.buildHandler()

	switch inst.cfg.TLSMode {
	case TLSTailscale:
		if err := inst.startTailscaleTLS(handler); err != nil {
			inst.server.Close()
			return err
		}
	case TLSACME:
		if err := inst.startACMETLS(handler); err != nil {
			inst.server.Close()
			return err
		}
	case TLSNone:
		if err := inst.startPlainHTTP(handler); err != nil {
			inst.server.Close()
			return err
		}
	}

	log.Printf("[%s] proxying at %s", inst.cfg.Name, inst.URL())
	return nil
}

// startTailscaleTLS uses Tailscale's built-in ListenTLS (works with SaaS only).
func (inst *Instance) startTailscaleTLS(handler http.Handler) error {
	var err error
	inst.ln443, err = inst.server.ListenTLS("tcp", ":443")
	if err != nil {
		return fmt.Errorf("listen TLS :443: %w", err)
	}

	inst.httpsSrv = &http.Server{Handler: handler}
	go inst.serve(inst.httpsSrv, inst.ln443, "HTTPS")

	// HTTP → HTTPS redirect on :80.
	inst.startHTTPRedirect()
	return nil
}

// startACMETLS uses an externally-provided tls.Config (from certmagic, etc.).
func (inst *Instance) startACMETLS(handler http.Handler) error {
	if inst.cfg.TLSConfig == nil {
		return fmt.Errorf("ACME TLS mode requires TLSConfig to be set")
	}

	// Listen on the raw tsnet socket, wrap with our own TLS.
	rawLn, err := inst.server.Listen("tcp", ":443")
	if err != nil {
		return fmt.Errorf("listen :443: %w", err)
	}
	inst.ln443 = tls.NewListener(rawLn, inst.cfg.TLSConfig)

	inst.httpsSrv = &http.Server{Handler: handler}
	go inst.serve(inst.httpsSrv, inst.ln443, "HTTPS")

	// HTTP → HTTPS redirect on :80.
	inst.startHTTPRedirect()
	return nil
}

// startPlainHTTP serves without TLS (for development or tailnet-only use).
func (inst *Instance) startPlainHTTP(handler http.Handler) error {
	var err error
	inst.ln80, err = inst.server.Listen("tcp", ":80")
	if err != nil {
		return fmt.Errorf("listen :80: %w", err)
	}

	inst.httpSrv = &http.Server{Handler: handler}
	go inst.serve(inst.httpSrv, inst.ln80, "HTTP")
	return nil
}

func (inst *Instance) startHTTPRedirect() {
	var err error
	inst.ln80, err = inst.server.Listen("tcp", ":80")
	if err != nil {
		log.Printf("[%s] warning: could not listen on :80: %v", inst.cfg.Name, err)
		return
	}
	redirectHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
	inst.httpSrv = &http.Server{Handler: redirectHandler}
	go inst.serve(inst.httpSrv, inst.ln80, "HTTP redirect")
}

func (inst *Instance) serve(srv *http.Server, ln net.Listener, label string) {
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Printf("[%s] %s serve error: %v", inst.cfg.Name, label, err)
	}
}

// URL returns the primary URL for this instance.
func (inst *Instance) URL() string {
	if inst.cfg.TLSMode == TLSNone {
		return "http://" + inst.fqdn
	}
	return "https://" + inst.fqdn
}

// FQDN returns the tailnet FQDN for this instance, available after Start.
func (inst *Instance) FQDN() string {
	return inst.fqdn
}

// Name returns the instance name.
func (inst *Instance) Name() string {
	return inst.cfg.Name
}

// Stop shuts down the proxy and tsnet server.
func (inst *Instance) Stop() error {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.httpsSrv != nil {
		inst.httpsSrv.Close()
	}
	if inst.httpSrv != nil {
		inst.httpSrv.Close()
	}
	if inst.server != nil {
		inst.server.Close()
	}
	log.Printf("[%s] stopped", inst.cfg.Name)
	return nil
}

// UpdateTargets replaces the proxy targets (e.g., after container restart).
func (inst *Instance) UpdateTargets(targets []Target) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	inst.cfg.Targets = targets
}

func (inst *Instance) buildHandler() http.Handler {
	targets := inst.cfg.Targets

	// Single service: proxy everything to it.
	if len(targets) == 1 {
		t := targets[0]
		target := &url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s:%d", t.ContainerIP, t.Port),
		}
		return httputil.NewSingleHostReverseProxy(target)
	}

	// Multiple services: route by path prefix /{service}/
	mux := http.NewServeMux()
	for _, t := range targets {
		t := t
		target := &url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s:%d", t.ContainerIP, t.Port),
		}
		proxy := httputil.NewSingleHostReverseProxy(target)
		prefix := "/" + t.Service + "/"
		mux.Handle(prefix, http.StripPrefix("/"+t.Service, proxy))
	}

	// Default to first service for root path.
	if len(targets) > 0 {
		t := targets[0]
		target := &url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s:%d", t.ContainerIP, t.Port),
		}
		mux.Handle("/", httputil.NewSingleHostReverseProxy(target))
	}

	return mux
}
