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
	"syscall"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/api"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
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
		})
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown driver %q", *driverName)
	}
	defer driver.Close()

	mgr, err := host.NewManager(host.Options{
		StateDir: *stateDir, Driver: driver,
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
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

	apiSrv := &http.Server{Addr: *apiAddr, Handler: api.New(mgr, *defaultImage, log).Handler()}
	sshSrv := gw.Server(*sshAddr)

	errCh := make(chan error, 2)
	go func() { errCh <- apiSrv.ListenAndServe() }()
	go func() { errCh <- sshSrv.ListenAndServe() }()
	log.Info("sparkbox up", "driver", *driverName, "ssh", *sshAddr, "api", *apiAddr)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	apiSrv.Shutdown(shutCtx) //nolint:errcheck
	sshSrv.Close()           //nolint:errcheck
	return nil
}
