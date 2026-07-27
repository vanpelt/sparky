package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/hostsetup"
)

// doctor runs the preflight/health battery against this host and exits non-zero
// if any hard check fails. Its flags mirror the subset of `serve` flags that
// determine where artifacts and keys live, so it diagnoses the exact layout the
// service uses.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfg := hostsetup.DefaultConfig()
	root := fs.String("root", cfg.Root, "sparkbox home directory")
	stateDir := fs.String("state-dir", "", "state dir (default <root>/data/state)")
	keyDir := fs.String("key-dir", "", "fleet key dir (default: state dir)")
	kernel := fs.String("kernel", "", "guest vmlinux path (default <root>/vmlinux)")
	imageDir := fs.String("image-dir", "", "rootfs template dir (default <root>/data/images)")
	defaultImage := fs.String("default-image", cfg.DefaultImage, "rootfs template basename")
	users := fs.String("users", "", "users.conf path (default <root>/users.conf)")
	gateway := fs.String("gateway", cfg.Gateway, "fleet gateway host:port; diagnose this machine as a node")
	nodeName := fs.String("node-name", cfg.NodeName, "fleet node name expected in this node's exact mTLS SPIFFE identity")
	guestSubnet := fs.String("guest-subnet", cfg.GuestSubnet, "IPv4 guest prefix expected in daemon, NAT, and Tailscale route state")
	nodeControlTransport := fs.String("node-control-transport", cfg.NodeControlTransport, "fleet control transport: auto | ssh | grpc")
	nodeControlRollout := fs.String("node-control-rollout", cfg.NodeControlRollout, "gateway control cutover: inherit | shadow | read-only | idempotent | grpc")
	nodeGRPCAddr := fs.String("node-grpc-addr", cfg.NodeGRPCAddr, "fleet node tailnet listen address for the mTLS NodeControl server")
	gatewayGRPCAddr := fs.String("gateway-grpc-addr", cfg.GatewayGRPCAddr, "fleet gateway tailnet listen address for mTLS workload-identity RPCs")
	guestDataTransport := fs.String("guest-data-transport", cfg.GuestDataTransport, "remote guest data transport: auto | ssh | routed")
	routedGuestCanaryPercent := fs.Int("routed-guest-canary-percent", cfg.RoutedGuestCanaryPercent, "gateway share of sandboxes using routed data in auto mode: 0..100")
	clusterID := fs.String("cluster-id", cfg.ClusterID, "stable gateway mTLS identity name")
	binPath := fs.String("bin-path", cfg.BinPath, "sparkbox binary the systemd unit runs (checked for version skew against the live service)")
	release := fs.String("release", "", "release tag this host is supposed to be running; skew from the installed binary is reported (default: no assertion)")
	// macOS only, and the same spellings `sparkbox setup` uses. Without these a
	// Mac whose gateway lives in `--machine-name other` would be diagnosed
	// against the default name and told, wrongly and confidently, that it has no
	// machine at all — a new way to be untruthful in the command whose whole job
	// is not being that.
	machineName := fs.String("machine-name", cfg.MachineName, "macOS only: the nested linux machine holding the gateway")
	machineImage := fs.String("machine-image", "", "macOS only: gateway image reference (default: local/sparkbox-gateway:<hash of the embedded build context>)")
	outerKernel := fs.String("outer-kernel", "", "macOS only: path to the KVM-capable outer kernel the machine boots (default: ~/Library/Application Support/sparkbox/vmlinux-macos-arm64)")
	containerBin := fs.String("container-bin", cfg.ContainerBin, "macOS only: Apple's container CLI")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sparkbox doctor [flags]\n\n"+
			"Reports whether this host is ready to run sparkbox.\n"+
			"On macOS it reports on two hosts: this Mac (`mac:` lines) and the nested linux\n"+
			"machine the gateway runs in (`machine:` lines, ending with the gateway's own doctor).")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if err := rejectMacOnlyFlags(runtime.GOOS, given); err != nil {
		return err
	}

	cfg = applyPaths(cfg, *root, *stateDir, *keyDir, *kernel, *imageDir, *defaultImage, *users)
	cfg.Gateway = *gateway
	cfg.NodeName = *nodeName
	cfg.GuestSubnet = *guestSubnet
	cfg.NodeControlTransport = *nodeControlTransport
	cfg.NodeControlRollout = *nodeControlRollout
	cfg.NodeGRPCAddr = *nodeGRPCAddr
	cfg.GatewayGRPCAddr = *gatewayGRPCAddr
	cfg.GuestDataTransport = *guestDataTransport
	cfg.RoutedGuestCanaryPercent = *routedGuestCanaryPercent
	cfg.ClusterID = *clusterID
	cfg.FlagsGiven = given
	normalizedGuestSubnet, err := hostsetup.NormalizeGuestSubnet(cfg.GuestSubnet)
	if err != nil {
		return fmt.Errorf("invalid --guest-subnet: %w", err)
	}
	cfg.GuestSubnet = normalizedGuestSubnet
	// Set after applyPaths: --root rebuilds cfg from the default layout, and the
	// install path is absolute rather than derived from the sparkbox home.
	cfg.BinPath = *binPath
	cfg.Release = *release // empty (or "latest") asserts nothing
	cfg.Version = version
	// Also after applyPaths, for the same reason: --root rebuilds cfg from the
	// default layout and would otherwise silently reset these to the defaults.
	// Inert on linux, where nothing reads them.
	cfg.MachineName = *machineName
	cfg.MachineImage = *machineImage
	cfg.OuterKernel = *outerKernel
	cfg.ContainerBin = *containerBin

	// An Env, not just a Config: on macOS the battery has to be able to talk to
	// the nested machine (that is where the gateway is), and the Env is what
	// carries the driver. On Linux nothing in the battery reads it and the
	// result is identical to what doctor always ran.
	env := hostsetup.NewEnv(context.Background(), cfg, hostsetup.NewExecRunner(), hostsetup.NewHTTPFetcher(), os.Stdout)
	if err := hostsetup.AttachMachineDriver(env); err != nil {
		return err
	}
	// On Linux, diagnose what systemd actually starts. TRANSPORT_FLAGS is
	// managed by setup, while the later compatibility/operator bundles retain
	// Go flag's last-value-wins precedence. The macOS outer doctor deliberately
	// leaves this to the inner Linux doctor running inside the machine.
	if runtime.GOOS != "darwin" {
		cfg, err = hostsetup.NormalizeTransportConfig(hostsetup.EffectiveFleetConfig(env.Probe, cfg))
		if err != nil {
			return fmt.Errorf("effective transport configuration: %w", err)
		}
		if err := validateNodeFlags(cfg.Gateway, cfg.NodeName); err != nil {
			return err
		}
		env.Cfg = cfg
		check := nodeControlHealthCheck(env.Ctx, cfg)
		env.NodeControlHealth = &check
	}
	results := hostsetup.RunChecks(env.Probe, cfg, hostsetup.DoctorChecksFor(env))
	// Unchanged on linux ("sparkbox doctor — /srv/sparkbox"); on a Mac it names
	// the machine as well, because cfg.Root is a path inside it.
	fmt.Println(hostsetup.DoctorHeader(env))
	hostsetup.PrintResults(os.Stdout, results)
	if hostsetup.AnyFail(results) {
		return errors.New("one or more checks FAILED (see above)")
	}
	return nil
}

// applyPaths layers explicit path flags over the root-derived defaults, so
// passing only --root shifts the whole layout while individual flags still win.
func applyPaths(cfg hostsetup.Config, root, stateDir, keyDir, kernel, imageDir, defaultImage, users string) hostsetup.Config {
	if root != "" && root != cfg.Root {
		cfg = hostsetup.DefaultConfigAt(root)
	}
	if stateDir != "" {
		cfg.StateDir = stateDir
	}
	if keyDir != "" {
		cfg.KeyDir = keyDir
	}
	if kernel != "" {
		cfg.KernelPath = kernel
	}
	if imageDir != "" {
		cfg.ImageDir = imageDir
	}
	if defaultImage != "" {
		cfg.DefaultImage = defaultImage
	}
	if users != "" {
		cfg.UsersPath = users
	}
	return cfg
}
