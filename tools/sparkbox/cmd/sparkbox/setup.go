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
	sshAddr := fs.String("ssh-addr", "", "SSH gateway listen address, host:port (default :2222, or :22 with --move-admin-ssh). Give the gateway an address of its own (e.g. 10.66.0.1:2222) so it cannot collide with host services")
	proxyAddr := fs.String("proxy-addr", cfg.ProxyAddr, "HTTP edge listen address, host:port. sparkbox.env's PROXY_PORT is derived from its port, so this moves the any-port forwarding target with it")
	apiAddr := fs.String("api-addr", cfg.APIAddr, "control API listen address, host:port (no auth — keep it private)")
	dnsAddr := fs.String("dns-addr", "", "wildcard DNS responder listen address, host:port (e.g. 10.66.0.1:53) for a split-DNS entry pointing *.<domain> at this edge; empty disables it")
	dnsAnswer := fs.String("dns-answer", "", "comma-separated IPs the wildcard responder answers *.<domain> with (default: the IP host of --proxy-addr)")
	// The optional subsystems (F2). Every one of these was previously reachable
	// only by hand-writing it into a sparkbox.env flag bundle, which is how the
	// live DGX gateway came to run a configuration no `setup` could reproduce.
	// Each is omitted from the unit entirely when left unset.
	edgeIP := fs.String("edge-ip", "", "give the edge its own address on a dummy interface (e.g. a dedicated tailnet /32 like 10.66.0.1) and DNAT any-port traffic by destination; also turns the uplink REDIRECT off, so the edge cannot collide with host services")
	sshAdvertise := fs.Int("ssh-advertise-port", 0, "SSH port shown in user-facing instructions when a DNAT forwards it to the gateway (e.g. 22 while the gateway binds :2222); 0 advertises the real listen port")
	proxyTLS := fs.Bool("proxy-tls", false, "terminate TLS at the edge (see --tls-provider)")
	tlsProvider := fs.String("tls-provider", "", "certificates when --proxy-tls: cloudflare (DNS-01 wildcard, needs CLOUDFLARE_API_TOKEN in the unit's environment) | autocert (per-host, on demand)")
	tlsEmail := fs.String("tls-email", "", "ACME account email for certificate-expiry notices (with --proxy-tls)")
	guestDNS := fs.String("guest-dns", "", "resolver handed to guests: an IP (e.g. 172.30.0.53, where sluice listens) or the literal \"gateway\". Empty leaves guests on public DNS, which bypasses egress filtering entirely")
	sluiceSocket := fs.String("sluice-socket", "", "sluice control socket (e.g. /run/sluice.sock) the gateway pushes per-tag egress policy to; empty disables egress control. NOTE: setup does not install sluice itself yet — see the egress check in 'sparkbox doctor'")
	archiveRemote := fs.String("archive-remote", "", "rclone remote holding sandbox archives (needs S3 write creds in the host's rclone.conf); requires --archive-bucket")
	archiveBucket := fs.String("archive-bucket", "", "bucket within --archive-remote for sandbox archives")
	binPath := fs.String("bin-path", cfg.BinPath, "where to install this sparkbox binary; the systemd unit's ExecStart runs it (empty skips the install)")
	force := fs.Bool("force", false, "overwrite a --bin-path binary that reports a NEWER version than this one")
	adoptLegacy := fs.Bool("adopt-legacy", false, "use a sparkbox state directory that is already live on this host (e.g. the pre-setup flat <root>/state) instead of <root>/data/state; without it, setup refuses rather than provisioning a second data root beside the live one")
	dryRun := fs.Bool("dry-run", false, "print the plan and change nothing")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sparkbox setup [flags]\n\nProvisions this Linux host into a running Sparkbox gateway or fleet node.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg = applyPaths(cfg, *root, *stateDir, "", *kernel, *imageDir, "", *users)
	// Which flags the operator actually TYPED, before any of the assignments
	// below erase the difference between "asked for" and "defaulted to". setup
	// now reconciles managed keys in a live host's sparkbox.env on every run, and
	// several of those keys (PROXY_DOMAIN above all) have a compiled-in default
	// that would otherwise overwrite a working host's value on an upgrade run
	// that never mentioned them.
	cfg.FlagsGiven = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { cfg.FlagsGiven[f.Name] = true })
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
	// Same trap as BinPath: applyPaths rebuilds cfg from DefaultConfigAt when
	// --root moves, and a listen address is not derived from the sparkbox home,
	// so it has to be assigned after that call or it is silently reset to the
	// default. hostsetup.Provision validates these (parseable host:port, no
	// whitespace, no --ssh-addr/--move-admin-ssh contradiction) before it
	// touches the host.
	cfg.SSHAddr = *sshAddr
	cfg.ProxyAddr = *proxyAddr
	cfg.APIAddr = *apiAddr
	cfg.DNSAddr = *dnsAddr
	cfg.DNSAnswer = *dnsAnswer
	// The optional subsystems, assigned here for the same reason as the
	// addresses above: applyPaths rebuilds cfg from DefaultConfigAt when --root
	// moves, and none of these is derived from the sparkbox home.
	// hostsetup.Provision validates them (whitespace, IP literals, and the pairs
	// `serve` would silently ignore when given in half) before touching the host.
	cfg.EdgeIP = *edgeIP
	cfg.SSHAdvertisePort = *sshAdvertise
	cfg.ProxyTLS = *proxyTLS
	cfg.TLSProvider = *tlsProvider
	cfg.TLSEmail = *tlsEmail
	cfg.GuestDNS = *guestDNS
	cfg.SluiceSocket = *sluiceSocket
	cfg.ArchiveRemote = *archiveRemote
	cfg.ArchiveBucket = *archiveBucket
	cfg.Force = *force
	cfg.AdoptLegacy = *adoptLegacy
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
