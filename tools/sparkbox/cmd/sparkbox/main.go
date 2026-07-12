// sparkbox is a single-host MVP of an exe.dev-style agentic sandbox service:
// on-demand sandbox VMs behind a smart SSH gateway with resume-on-connect.
//
//	sparkbox serve --driver mock  --state-dir ./state --users users.conf
//	sparkbox serve --driver firecracker --kernel vmlinux --image-dir ./images ...
//
// Then: ssh -p 2222 new@localhost (creates a sandbox), ssh -p 2222 <name>@localhost.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/api"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/console"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/proxy"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	fcdriver "github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/firecracker"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: sparkbox serve [flags]")
		os.Exit(2)
	}
	if err := serve(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "sparkbox:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var (
		driverName   = fs.String("driver", "mock", "vm driver: mock | firecracker")
		stateDir     = fs.String("state-dir", "./state", "directory for keys, state, and VM data")
		usersPath    = fs.String("users", "", "users file: '<user> <authorized_keys line>' per line (required)")
		sshAddr      = fs.String("ssh-addr", ":2222", "SSH gateway listen address")
		apiAddr      = fs.String("api-addr", "127.0.0.1:8080", "control API listen address (no auth — keep private)")
		defaultImage = fs.String("default-image", "ubuntu", "rootfs template for new sandboxes")
		idleTimeout  = fs.Duration("idle-timeout", 30*time.Minute, "pause sandboxes idle longer than this")
		kernelPath   = fs.String("kernel", "", "firecracker: vmlinux path")
		imageDir     = fs.String("image-dir", "", "firecracker: directory of <image>.ext4 templates")
		subnet6      = fs.String("subnet6", "", "firecracker: routable IPv6 /64 delegated to the host (e.g. 2001:db8:1c7::/64); gives each sandbox a no-NAT v6 address")
		proxyAddr    = fs.String("proxy-addr", ":8081", "HTTP proxy edge listen address for <sub>.<domain> (empty to disable)")
		proxyDomain  = fs.String("proxy-domain", "hivemind.tools", "base domain for sandbox web routes")
		proxyTLS     = fs.Bool("proxy-tls", false, "terminate TLS for the proxy edge (see --tls-provider)")
		consolePass  = fs.String("console-password", "", "password for the operator console at <console-subdomain>.<domain> (empty disables it)")
		consoleSub   = fs.String("console-subdomain", "console", "subdomain that serves the operator console")
		tlsProvider  = fs.String("tls-provider", "cloudflare", "TLS certs when --proxy-tls: cloudflare (DNS-01 wildcard, needs CLOUDFLARE_API_TOKEN) | autocert (per-host on-demand)")
		tlsEmail     = fs.String("tls-email", "", "ACME account email (recommended for cert-expiry notices)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *usersPath == "" {
		return errors.New("--users is required")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		return err
	}
	hostKey, err := sshgw.LoadOrCreateKey(*stateDir, "gateway_host_key")
	if err != nil {
		return fmt.Errorf("host key: %w", err)
	}
	upstreamKey, err := sshgw.LoadOrCreateKey(*stateDir, "gateway_upstream_key")
	if err != nil {
		return fmt.Errorf("upstream key: %w", err)
	}
	users, err := sshgw.LoadUsers(*usersPath)
	if err != nil {
		return fmt.Errorf("users: %w", err)
	}

	var driver vmm.Driver
	switch *driverName {
	case "mock":
		driver = mock.New(*stateDir, hostKey)
	case "firecracker":
		driver, err = fcdriver.New(fcdriver.Options{
			KernelPath: *kernelPath, ImageDir: *imageDir, StateDir: *stateDir,
			Subnet6: *subnet6,
		})
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown driver %q", *driverName)
	}
	defer driver.Close()

	routeStore, err := routes.Open(filepath.Join(*stateDir, "sparkbox.db"))
	if err != nil {
		return fmt.Errorf("route store: %w", err)
	}
	defer routeStore.Close()

	mgr, err := host.NewManager(host.Options{
		StateDir: *stateDir, Driver: driver,
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
		Routes: routeStore,
	})
	if err != nil {
		return err
	}

	gw := sshgw.New(sshgw.GatewayOptions{
		Manager: mgr, Users: users, HostKey: hostKey, UpstreamKey: upstreamKey,
		DefaultImage: *defaultImage, Logger: log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go mgr.RunReaper(ctx, *idleTimeout, time.Minute)

	apiSrv := &http.Server{Addr: *apiAddr, Handler: api.New(mgr, routeStore, *defaultImage, log).Handler()}
	sshSrv := gw.Server(*sshAddr)

	errCh := make(chan error, 3)
	go func() { errCh <- apiSrv.ListenAndServe() }()
	go func() { errCh <- sshSrv.ListenAndServe() }()

	var proxySrv *http.Server
	if *proxyAddr != "" {
		px := proxy.New(mgr, routeStore, *proxyDomain, log)
		// The password is a secret, so prefer an env var (kept out of ps/systemd
		// status) and fall back to the flag for local/dev use.
		consolePw := *consolePass
		if consolePw == "" {
			consolePw = os.Getenv("SPARKBOX_CONSOLE_PASSWORD")
		}
		if consolePw != "" {
			px.SetConsole(*consoleSub, console.New(mgr, consolePw, *proxyTLS, log).Handler())
			log.Info("operator console enabled", "url", *consoleSub+"."+*proxyDomain)
		}
		proxySrv = &http.Server{Addr: *proxyAddr, Handler: px}
		if *proxyTLS {
			log.Info("obtaining TLS certificate", "provider", *tlsProvider, "domain", *proxyDomain)
			if err := setupProxyTLS(ctx, proxySrv, tlsParams{
				provider: *tlsProvider, domain: *proxyDomain, email: *tlsEmail, stateDir: *stateDir,
			}); err != nil {
				return fmt.Errorf("proxy tls: %w", err)
			}
			go func() { errCh <- proxySrv.ListenAndServeTLS("", "") }()
		} else {
			go func() { errCh <- proxySrv.ListenAndServe() }()
		}
	}

	log.Info("sparkbox up", "driver", *driverName, "ssh", *sshAddr, "api", *apiAddr,
		"proxy", *proxyAddr, "domain", *proxyDomain, "proxy_tls", *proxyTLS)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	apiSrv.Shutdown(shutCtx) //nolint:errcheck
	if proxySrv != nil {
		proxySrv.Shutdown(shutCtx) //nolint:errcheck
	}
	sshSrv.Close() //nolint:errcheck
	return nil
}
