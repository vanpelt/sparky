package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/hostsetup"
)

// setup provisions an arbitrary Linux host into a running sparkbox service:
// preflight, fetch a prebuilt artifact release, lay down an XFS reflink volume,
// seed users.conf, install systemd units, and start. It is idempotent, and
// --dry-run prints the plan without touching the host.
func setup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	cfg := hostsetup.DefaultConfig()
	root := fs.String("root", cfg.Root, "sparkbox home directory")
	stateDir := fs.String("state-dir", "", "state dir (default <root>/data/state)")
	kernel := fs.String("kernel", "", "guest vmlinux path (default <root>/vmlinux)")
	imageDir := fs.String("image-dir", "", "rootfs template dir (default <root>/data/images)")
	users := fs.String("users", "", "users.conf path (default <root>/users.conf)")
	domain := fs.String("proxy-domain", cfg.ProxyDomain, "base domain for sandbox web routes")
	artifactBase := fs.String("artifact-base", cfg.ArtifactBase, "release artifact base URL")
	release := fs.String("release", cfg.Release, "release tag, or 'latest'")
	operatorKey := fs.String("operator-key", "", "operator SSH public key: a path, or literal 'ssh-... key' text (default: auto-detect ~/.ssh/*.pub)")
	operatorHandle := fs.String("operator-handle", cfg.OperatorHandle, "handle for the operator account in users.conf")
	dataGB := fs.Int("data-volume-gb", cfg.DataVolumeGB, "size of the XFS reflink data volume, GiB")
	swapGB := fs.Int("swap-gb", cfg.SwapGB, "overcommit safety-valve swapfile size, GiB (0 disables)")
	moveAdminSSH := fs.Bool("move-admin-ssh", false, "relocate the host's own sshd to :2222 so the gateway can own :22 (DANGEROUS over an SSH session — keep another shell open)")
	dryRun := fs.Bool("dry-run", false, "print the plan and change nothing")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sparkbox setup [flags]\n\nProvisions this Linux host into a running sparkbox gateway.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg = applyPaths(cfg, *root, *stateDir, "", *kernel, *imageDir, "", *users)
	cfg.ProxyDomain = *domain
	cfg.ArtifactBase = *artifactBase
	cfg.Release = *release
	cfg.OperatorKey = *operatorKey
	cfg.OperatorHandle = *operatorHandle
	cfg.DataVolumeGB = *dataGB
	cfg.SwapGB = *swapGB
	cfg.MoveAdminSSH = *moveAdminSSH
	cfg.DryRun = *dryRun

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env := hostsetup.NewEnv(ctx, cfg, hostsetup.NewExecRunner(), hostsetup.NewHTTPFetcher(), os.Stdout)
	return hostsetup.Provision(env)
}
