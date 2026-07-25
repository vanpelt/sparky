package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/hostsetup"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
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
	gateway := fs.String("gateway", cfg.Gateway, "fleet gateway host:port; provision this machine as a node instead of a gateway")
	nodeName := fs.String("node-name", cfg.NodeName, "fleet node name (default: hostname; only used with --gateway)")
	moveAdminSSH := fs.Bool("move-admin-ssh", false, "relocate the host's own sshd to :2222 so the gateway can own :22 (DANGEROUS over an SSH session — keep another shell open)")
	binPath := fs.String("bin-path", cfg.BinPath, "where to install this sparkbox binary; the systemd unit's ExecStart runs it (empty skips the install)")
	force := fs.Bool("force", false, "overwrite a --bin-path binary that reports a NEWER version than this one")
	dryRun := fs.Bool("dry-run", false, "print the plan and change nothing")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sparkbox setup [flags]\n\nProvisions this Linux host into a running Sparkbox gateway or fleet node.")
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
	cfg.Gateway = *gateway
	cfg.NodeName = *nodeName
	cfg.MoveAdminSSH = *moveAdminSSH
	// Assigned after applyPaths, which rebuilds cfg from DefaultConfigAt when
	// --root moves: the install path is absolute and does not hang off the
	// sparkbox home, so it must not be re-derived away.
	cfg.BinPath = *binPath
	cfg.Force = *force
	// The release tag this binary was linked with (main.version): setup installs
	// *itself*, so this is the version that ends up on the host, and doctor
	// compares it with the running service and the requested release.
	cfg.Version = version
	cfg.DryRun = *dryRun
	if cfg.Gateway != "" && cfg.MoveAdminSSH {
		return fmt.Errorf("--move-admin-ssh cannot be used with --gateway; a fleet node has no inbound Sparkbox SSH gateway")
	}
	if err := validateNodeFlags(cfg.Gateway, cfg.NodeName); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env := hostsetup.NewEnv(ctx, cfg, hostsetup.NewExecRunner(), hostsetup.NewHTTPFetcher(), os.Stdout)
	return hostsetup.Provision(env)
}

// validateNodeFlags rejects fleet-node settings that setup cannot faithfully
// write down. Both land in sparkbox.env's GATEWAY_FLAG bundle, which the units
// reference unquoted so systemd word-splits it into argv — so a value carrying
// whitespace does not reach the daemon as one argument, it becomes extra
// arguments and the service dies on an opaque flag-parse error at boot rather
// than here. The gateway itself already refuses a malformed node name when the
// link opens (nodelink.CodeBadNodeName); this is the same rule applied before
// the host is provisioned around it.
func validateNodeFlags(gateway, nodeName string) error {
	if gateway != "" && strings.ContainsAny(gateway, " \t\n") {
		return fmt.Errorf("invalid --gateway %q: expected host:port with no whitespace", gateway)
	}
	if nodeName == "" {
		return nil
	}
	if gateway == "" {
		return fmt.Errorf("--node-name %q needs --gateway; without it this host is provisioned as a gateway, which has no node name", nodeName)
	}
	if !nodes.ValidName(nodeName) {
		return fmt.Errorf("invalid --node-name %q: lowercase letters, digits and hyphens only, starting with a letter or digit, at most 63 characters", nodeName)
	}
	return nil
}
