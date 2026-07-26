package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/hostsetup"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
)

// setupOpts holds every `sparkbox setup` flag pointer.
//
// The flags are declared in newSetupFlags rather than inline in setup() so that
// a test can walk the REAL FlagSet: TestEverySetupFlagHasAFate asserts that
// every flag declared here is classified at the darwin boundary (always
// emitted, forwardable to the inner setup, refused, or Mac-only). A flag added
// without a fate is a flag silently dropped on macOS, which is F2's shape.
type setupOpts struct {
	root, stateDir, kernel, imageDir, users              *string
	domain, artifactBase, release                        *string
	operatorKey, operatorHandle                          *string
	dataGB, swapGB                                       *int
	gateway, nodeName                                    *string
	moveAdminSSH                                         *bool
	sshAddr, proxyAddr, apiAddr, dnsAddr, dnsAnswer      *string
	edgeIP                                               *string
	sshAdvertise                                         *int
	proxyTLS                                             *bool
	tlsProvider, tlsEmail, guestDNS                      *string
	sluice, agentTools                                   *bool
	sluiceDNSAddr, sluiceSocket                          *string
	archiveRemote, archiveBucket                         *string
	binPath                                              *string
	force, adoptLegacy, dryRun                           *bool
	machineName, machineImage, outerKernel, containerBin *string
	machineCPUs, machineMemGB                            *int
}

func newSetupFlags(cfg hostsetup.Config) (*flag.FlagSet, *setupOpts) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	o := &setupOpts{}
	o.root = fs.String("root", cfg.Root, "sparkbox home directory")
	o.stateDir = fs.String("state-dir", "", "state dir (default <root>/data/state)")
	o.kernel = fs.String("kernel", "", "guest vmlinux path (default <root>/vmlinux)")
	o.imageDir = fs.String("image-dir", "", "rootfs template dir (default <root>/data/images)")
	o.users = fs.String("users", "", "users.conf path (default <root>/users.conf)")
	o.domain = fs.String("proxy-domain", cfg.ProxyDomain, "base domain for sandbox web routes")
	o.artifactBase = fs.String("artifact-base", cfg.ArtifactBase, "release artifact base URL")
	o.release = fs.String("release", cfg.Release, "release tag, or 'latest'")
	o.operatorKey = fs.String("operator-key", "", "operator SSH public key: a path, or literal 'ssh-... key' text (default: auto-detect ~/.ssh/*.pub)")
	o.operatorHandle = fs.String("operator-handle", cfg.OperatorHandle, "handle for the operator account in users.conf")
	o.dataGB = fs.Int("data-volume-gb", cfg.DataVolumeGB, "size of the XFS reflink data volume, GiB")
	o.swapGB = fs.Int("swap-gb", cfg.SwapGB, "overcommit safety-valve swapfile size, GiB (0 disables)")
	o.gateway = fs.String("gateway", cfg.Gateway, "fleet gateway host:port; provision this machine as a node instead of a gateway")
	o.nodeName = fs.String("node-name", cfg.NodeName, "fleet node name (default: hostname; only used with --gateway)")
	o.moveAdminSSH = fs.Bool("move-admin-ssh", false, "relocate the host's own sshd to :2222 so the gateway can own :22 (DANGEROUS over an SSH session — keep another shell open)")
	o.sshAddr = fs.String("ssh-addr", "", "SSH gateway listen address, host:port (default :2222, or :22 with --move-admin-ssh). Give the gateway an address of its own (e.g. 10.66.0.1:2222) so it cannot collide with host services")
	o.proxyAddr = fs.String("proxy-addr", cfg.ProxyAddr, "HTTP edge listen address, host:port. sparkbox.env's PROXY_PORT is derived from its port, so this moves the any-port forwarding target with it")
	o.apiAddr = fs.String("api-addr", cfg.APIAddr, "control API listen address, host:port (no auth — keep it private)")
	o.dnsAddr = fs.String("dns-addr", "", "wildcard DNS responder listen address, host:port (e.g. 10.66.0.1:53) for a split-DNS entry pointing *.<domain> at this edge; empty disables it")
	o.dnsAnswer = fs.String("dns-answer", "", "comma-separated IPs the wildcard responder answers *.<domain> with (default: the IP host of --proxy-addr)")
	// The optional subsystems (F2). Every one of these was previously reachable
	// only by hand-writing it into a sparkbox.env flag bundle, which is how the
	// live DGX gateway came to run a configuration no `setup` could reproduce.
	// Each is omitted from the unit entirely when left unset.
	o.edgeIP = fs.String("edge-ip", "", "give the edge its own address on a dummy interface (e.g. a dedicated tailnet /32 like 10.66.0.1) and DNAT any-port traffic by destination; also turns the uplink REDIRECT off, so the edge cannot collide with host services")
	o.sshAdvertise = fs.Int("ssh-advertise-port", 0, "SSH port shown in user-facing instructions when a DNAT forwards it to the gateway (e.g. 22 while the gateway binds :2222); 0 advertises the real listen port")
	o.proxyTLS = fs.Bool("proxy-tls", false, "terminate TLS at the edge (see --tls-provider)")
	o.tlsProvider = fs.String("tls-provider", "", "certificates when --proxy-tls: cloudflare (DNS-01 wildcard, needs CLOUDFLARE_API_TOKEN in the unit's environment) | autocert (per-host, on demand)")
	o.tlsEmail = fs.String("tls-email", "", "ACME account email for certificate-expiry notices (with --proxy-tls)")
	o.guestDNS = fs.String("guest-dns", "", "resolver handed to guests: an IP (e.g. 172.30.0.53, where sluice listens) or the literal \"gateway\". Empty leaves guests on public DNS, which bypasses egress filtering entirely")
	o.sluice = fs.Bool("sluice", false, "install and enable the sluice egress gateway from this release: fetch sluice-linux-<arch>, seed an allowlist, write its unit and start it. Implies --sluice-socket and --guest-dns unless you set them. Needs kernel >= 6.6. Untagged sandboxes keep unrestricted egress; only tagged ones are filtered")
	o.agentTools = fs.Bool("agent-tools", cfg.AgentTools, "bake the agent CLIs (claude, codex, hivemind) + guest workload identity into the rootfs templates and install the daily refresher. On by default — a sandbox with no agent in it is rarely what anyone wants; --agent-tools=false leaves the templates bare")
	o.sluiceDNSAddr = fs.String("sluice-dns-addr", "", "where sluice's allowlist resolver binds, host:port (default :53 with --sluice). Give it an address of its own (e.g. 172.30.0.53:53) on a host that also runs --dns-addr; guests are then pointed at that literal")
	o.sluiceSocket = fs.String("sluice-socket", "", "sluice control socket (e.g. /run/sluice.sock) the gateway pushes per-tag egress policy to; empty disables egress control. --sluice supplies /run/sluice.sock; set this explicitly only to talk to a sluice you installed yourself")
	o.archiveRemote = fs.String("archive-remote", "", "rclone remote holding sandbox archives (needs S3 write creds in the host's rclone.conf); requires --archive-bucket")
	o.archiveBucket = fs.String("archive-bucket", "", "bucket within --archive-remote for sandbox archives")
	o.binPath = fs.String("bin-path", cfg.BinPath, "where to install this sparkbox binary; the systemd unit's ExecStart runs it (empty skips the install)")
	o.force = fs.Bool("force", false, "overwrite a --bin-path binary that reports a NEWER version than this one")
	o.adoptLegacy = fs.Bool("adopt-legacy", false, "use a sparkbox state directory that is already live on this host (e.g. the pre-setup flat <root>/state) instead of <root>/data/state; without it, setup refuses rather than provisioning a second data root beside the live one")
	o.dryRun = fs.Bool("dry-run", false, "print the plan and change nothing")
	// macOS only. On a Mac the gateway runs in a nested linux machine, and every
	// flag above describes THAT machine (its layout, its listeners, its
	// subsystems) and is forwarded to the `sparkbox setup` that runs inside it.
	// These five are the only ones that describe the Mac.
	o.machineName = fs.String("machine-name", cfg.MachineName, "macOS only: the nested linux machine to create and provision")
	o.machineCPUs = fs.Int("machine-cpus", cfg.MachineCPUs, "macOS only: CPUs for the nested machine")
	o.machineMemGB = fs.Int("machine-memory-gb", cfg.MachineMemGB, "macOS only: memory for the nested machine, GiB")
	o.machineImage = fs.String("machine-image", "", "macOS only: gateway image reference (default: local/sparkbox-gateway:<hash of the embedded build context>)")
	o.outerKernel = fs.String("outer-kernel", "", "macOS only: path to the KVM-capable outer kernel the machine boots (default: ~/Library/Application Support/sparkbox/vmlinux-macos-arm64, downloaded from the release)")
	o.containerBin = fs.String("container-bin", cfg.ContainerBin, "macOS only: Apple's container CLI")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sparkbox setup [flags]\n\n"+
			"Provisions this host into a running Sparkbox gateway or fleet node.\n"+
			"On Linux that is this machine; on macOS it is a nested linux machine this command creates.")
		fs.PrintDefaults()
	}
	return fs, o
}

// setup provisions a host into a running sparkbox service: preflight, fetch a
// prebuilt artifact release, lay down an XFS reflink volume, wire systemd, and
// start. It is idempotent, and --dry-run prints the plan without touching
// anything.
//
// On macOS the same command creates a nested linux machine (Apple's `container
// machine --virtualization`) and runs this same provisioning inside it.
func setup(args []string) error {
	cfg := hostsetup.DefaultConfig()
	fs, o := newSetupFlags(cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg = applyPaths(cfg, *o.root, *o.stateDir, "", *o.kernel, *o.imageDir, "", *o.users)
	// Which flags the operator actually TYPED, before any of the assignments
	// below erase the difference between "asked for" and "defaulted to". setup
	// now reconciles managed keys in a live host's sparkbox.env on every run, and
	// several of those keys (PROXY_DOMAIN above all) have a compiled-in default
	// that would otherwise overwrite a working host's value on an upgrade run
	// that never mentioned them. On macOS it is also what decides which flags
	// are forwarded to the inner setup.
	cfg.FlagsGiven = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { cfg.FlagsGiven[f.Name] = true })
	cfg.ProxyDomain = *o.domain
	cfg.ArtifactBase = *o.artifactBase
	cfg.Release = *o.release
	cfg.OperatorKey = *o.operatorKey
	cfg.OperatorHandle = *o.operatorHandle
	cfg.DataVolumeGB = *o.dataGB
	cfg.SwapGB = *o.swapGB
	cfg.Gateway = *o.gateway
	cfg.NodeName = *o.nodeName
	cfg.MoveAdminSSH = *o.moveAdminSSH
	// Assigned after applyPaths, which rebuilds cfg from DefaultConfigAt when
	// --root moves: the install path is absolute and does not hang off the
	// sparkbox home, so it must not be re-derived away.
	cfg.BinPath = *o.binPath
	// Same trap as BinPath: applyPaths rebuilds cfg from DefaultConfigAt when
	// --root moves, and a listen address is not derived from the sparkbox home,
	// so it has to be assigned after that call or it is silently reset to the
	// default. hostsetup.Provision validates these (parseable host:port, no
	// whitespace, no --ssh-addr/--move-admin-ssh contradiction) before it
	// touches the host.
	cfg.SSHAddr = *o.sshAddr
	cfg.ProxyAddr = *o.proxyAddr
	cfg.APIAddr = *o.apiAddr
	cfg.DNSAddr = *o.dnsAddr
	cfg.DNSAnswer = *o.dnsAnswer
	// The optional subsystems, assigned here for the same reason as the
	// addresses above: applyPaths rebuilds cfg from DefaultConfigAt when --root
	// moves, and none of these is derived from the sparkbox home.
	// hostsetup.Provision validates them (whitespace, IP literals, and the pairs
	// `serve` would silently ignore when given in half) before touching the host.
	cfg.EdgeIP = *o.edgeIP
	cfg.SSHAdvertisePort = *o.sshAdvertise
	cfg.ProxyTLS = *o.proxyTLS
	cfg.TLSProvider = *o.tlsProvider
	cfg.TLSEmail = *o.tlsEmail
	cfg.GuestDNS = *o.guestDNS
	cfg.AgentTools = *o.agentTools
	cfg.Sluice = *o.sluice
	cfg.SluiceDNSAddr = *o.sluiceDNSAddr
	cfg.SluiceSocket = *o.sluiceSocket
	cfg.ArchiveRemote = *o.archiveRemote
	cfg.ArchiveBucket = *o.archiveBucket
	cfg.Force = *o.force
	cfg.AdoptLegacy = *o.adoptLegacy
	// The macOS machine fields, assigned here for the same applyPaths reason.
	cfg.MachineName = *o.machineName
	cfg.MachineCPUs = *o.machineCPUs
	cfg.MachineMemGB = *o.machineMemGB
	cfg.MachineImage = *o.machineImage
	cfg.OuterKernel = *o.outerKernel
	cfg.ContainerBin = *o.containerBin
	// The release tag this binary was linked with (main.version): setup installs
	// *itself*, so this is the version that ends up on the host, and doctor
	// compares it with the running service and the requested release.
	cfg.Version = version
	cfg.DryRun = *o.dryRun
	if cfg.Gateway != "" && cfg.MoveAdminSSH {
		return fmt.Errorf("--move-admin-ssh cannot be used with --gateway; a fleet node has no inbound Sparkbox SSH gateway")
	}
	if err := validateNodeFlags(cfg.Gateway, cfg.NodeName); err != nil {
		return err
	}
	if err := validatePlatformFlags(runtime.GOOS, cfg.FlagsGiven); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env := hostsetup.NewEnv(ctx, cfg, hostsetup.NewExecRunner(), hostsetup.NewHTTPFetcher(), os.Stdout)
	return hostsetup.Provision(env)
}

// validatePlatformFlags refuses a flag that cannot mean anything on this host.
//
// Both directions, and both loudly. A --machine-name on linux would be quietly
// ignored (there is no machine), and a --move-admin-ssh on darwin would leave
// an operator expecting a relocated sshd that never moved. Silently dropping
// either is how a host ends up running a configuration nobody asked for.
func validatePlatformFlags(goos string, given map[string]bool) error {
	if goos == "darwin" {
		for name := range given {
			if hostsetup.FlagFate(name) == "refused" {
				return fmt.Errorf("--%s cannot be used on macOS; run `sparkbox setup -h` for what the macOS flags do", name)
			}
		}
		return nil
	}
	return rejectMacOnlyFlags(goos, given)
}

// rejectMacOnlyFlags is the linux half, shared with `doctor` — which declares
// the same machine flags for the same reason and must refuse them in the same
// words. One policy, one registry (hostsetup.FlagFate), two commands.
func rejectMacOnlyFlags(goos string, given map[string]bool) error {
	if goos == "darwin" {
		return nil
	}
	var macOnly []string
	for name := range given {
		if hostsetup.FlagFate(name) == "darwin-only" && name != "dry-run" {
			macOnly = append(macOnly, "--"+name)
		}
	}
	if len(macOnly) > 0 {
		sort.Strings(macOnly) // a stable message: map iteration is not
		return fmt.Errorf("%s only mean something on macOS, where the gateway runs in a nested linux machine; "+
			"on %s it runs on this host directly", strings.Join(macOnly, ", "), goos)
	}
	return nil
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
