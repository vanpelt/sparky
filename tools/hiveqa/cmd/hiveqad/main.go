// hiveqad is the hiveqa daemon. It manages docker-compose stacks,
// giving each one a unique Tailscale identity via tsnet.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/vanpelt/sparky/tools/hiveqa/internal/certs"
	"github.com/vanpelt/sparky/tools/hiveqa/internal/config"
	"github.com/vanpelt/sparky/tools/hiveqa/internal/daemon"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	cfg, err := config.Parse()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	d, err := daemon.New(cfg)
	if err != nil {
		log.Fatalf("failed to create daemon: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize ACME cert manager if needed.
	if cfg.TLS == config.TLSACME {
		mgr := &certs.Manager{
			ParentDomain:    cfg.ACMEDomain,
			Email:           cfg.ACMEEmail,
			DNSProviderName: certs.DNSProvider(cfg.ACMEDNS01Provider),
			CertStorage:     filepath.Join(cfg.StateDir, "certs"),
		}
		if err := mgr.Init(ctx); err != nil {
			log.Fatalf("ACME init: %v", err)
		}
		d.SetTLSConfig(mgr.TLSConfig())
		log.Printf("ACME TLS enabled for *.%s (dns-01: %s)", cfg.ACMEDomain, cfg.ACMEDNS01Provider)
	}

	// Handle shutdown signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("received %v, shutting down...", sig)
		d.Shutdown()
		cancel()
	}()

	socketPath := filepath.Join(cfg.StateDir, "hiveqad.sock")
	log.Printf("hiveqad starting (state: %s, control: %s, tls: %s)",
		cfg.StateDir, cfg.ControlURL, cfg.TLS)

	if err := d.ServeControl(ctx, socketPath); err != nil {
		if ctx.Err() == nil {
			log.Fatalf("control server error: %v", err)
		}
	}
}
