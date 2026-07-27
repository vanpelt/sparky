// Package hostsetup provides the host-provisioning and preflight-diagnostic
// logic behind the `sparkbox setup` and `sparkbox doctor` subcommands. It turns
// a bare Linux host into a running sparkbox gateway (fetch a prebuilt artifact
// release, lay down an XFS reflink volume, wire systemd) and reports whether a
// host is ready to run one.
//
// The logic lives here (rather than in package main) so it is unit-testable
// without a real KVM host: every environment interaction goes through the Probe
// / Runner / Fetcher interfaces, which tests replace with in-memory fakes. This
// mirrors the canonical provisioning shell (sparkbox-provision.sh embedded in
// deploy/cloud-init.yaml), minus the Scaleway Secret Manager and flexible-IP
// steps: a standalone host generates its own fleet keys on first `serve`.
package hostsetup

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/machine"
	macosassets "github.com/vanpelt/sparky/tools/sparkbox/macos"
)

// DefaultArtifactBase is the GitHub Releases endpoint the release workflow
// publishes to. Assets are a flat, arch-suffixed namespace under
// <base>/download/<tag>/ (or <base>/latest/download/ for the newest
// non-prerelease): manifest-<arch>.env, vmlinux-<arch>, firecracker-<arch>,
// <rootfs>-<arch>.ext4.zst, sparkbox-linux-<arch> — plus, for Apple Silicon,
// sparkbox-darwin-arm64 and its own manifest-darwin-arm64.env (see
// Manifest for why the Mac reads a different file rather than this one).
const DefaultArtifactBase = "https://github.com/vanpelt/sparky/releases"

// The gateway's default listen addresses. Every one of them is settable now
// (Config.SSHAddr/ProxyAddr/APIAddr/DNSAddr, from setup's --ssh-addr etc.);
// these are only what an operator gets who names none of them.
//
// The DGX gateway is why the flags exist: it binds a dedicated tailnet /32
// (10.66.0.1) for both the SSH gateway and the edge, so the any-port DNATs key
// off the destination IP and cannot collide with host services. There was no
// flag for that, so the addresses were smuggled into the TLS_FLAGS bundle,
// which the unit appends after the hardcoded ones (Go's flag package lets a
// repeated flag win last). It worked, and it read like a bug.
const (
	// defaultProxyAddr is the HTTP edge. The unit's --proxy-addr and
	// sparkbox.env's PROXY_PORT (which sparkbox-net.sh REDIRECTs/DNATs any-port
	// traffic to) MUST agree, so renderEnvFile derives PROXY_PORT from whatever
	// address the unit gets rather than from a second constant — see
	// Config.proxyPortNum.
	defaultProxyAddr = ":8081"
	// defaultAPIAddr deliberately is NOT :8080. That port is taken on any
	// workstation-class host worth the name — on the DGX by an unrelated python
	// process — so a fresh `setup` there produced a gateway that lost a port
	// race at boot and crash-looped forever on
	// "listen tcp 127.0.0.1:8080: bind: address already in use" (F1/F7).
	defaultAPIAddr = "127.0.0.1:8079"
	// defaultSSHAddr is where the gateway's SSH door binds when the host's own
	// sshd still owns :22; --move-admin-ssh relocates that sshd and hands the
	// gateway adminSSHAddr instead.
	defaultSSHAddr = ":2222"
	adminSSHAddr   = ":22"
	// defaultSluiceDNSAddr is where sluice's allowlist resolver binds when
	// --sluice is asked for and --sluice-dns-addr is not. The wildcard is right
	// for the common case: guests are handed their own tap's host-side address
	// as a resolver, and there is one of those per VM, so the resolver has to
	// answer on all of them. A host that ALSO runs sparkbox's own wildcard DNS
	// responder cannot leave it here — see validateSluice.
	defaultSluiceDNSAddr = ":53"
	// defaultSluiceSocket is the control socket the gateway pushes per-tag
	// egress policy to and reads per-VM byte counters from. /run is tmpfs, so it
	// is recreated every boot and never leaves the box.
	defaultSluiceSocket = "/run/sluice.sock"
)

// Sluice's on-host paths. Not flags: unlike the sparkbox binary (whose
// --bin-path exists because setup installs the file it is *running*, which an
// operator may keep anywhere), these are files only setup ever writes, only the
// sluice unit ever reads, and both renderings come from here.
const (
	sluiceBinPath   = "/usr/local/bin/sluice"
	sluiceUnit      = "sluice.service"
	sluiceTapPrefix = "sbtap"
	// minSluiceKernel is the host kernel sluice's meter needs. It attaches its
	// TC programs with a TCX link (link.AttachTCX), which landed in 6.6; on
	// anything older Load succeeds and Attach fails, so the daemon comes up,
	// meters nothing, enforces nothing and stays "active".
	//
	// The unit carries ConditionKernelVersion=>=6.6 as well, and that is NOT
	// enough on its own: systemd SKIPS a unit whose condition fails and
	// `systemctl start` on a skipped unit exits 0. Installing on a 6.1 host
	// would therefore have printed a clean provisioning report over an egress
	// filter that never ran — F7 with a different cause. So setup refuses.
	minSluiceKernelMajor = 6
	minSluiceKernelMinor = 6
)

// Config is the shared configuration for both doctor and setup. Its zero value
// is not useful; callers build it from flags (see cmd/sparkbox) and DefaultConfig
// supplies the paths the systemd unit and cloud-init agree on.
type Config struct {
	// Root is the sparkbox home on the host (/srv/sparkbox). Data, images, and
	// state hang off it, matching the layout the systemd units expect.
	Root string
	// StateDir holds the sqlite store, certs, and — on a standalone host — the
	// generated fleet keys. Defaults to <Root>/data/state.
	StateDir string
	// KeyDir holds the three fleet key PEMs. Empty means "same as StateDir"
	// (the standalone default); a fleet host points it at tmpfs.
	KeyDir string
	// ImageDir holds the <DefaultImage>.ext4 rootfs templates.
	ImageDir string
	// KernelPath is the guest vmlinux.
	KernelPath string
	// DefaultImage is the rootfs template basename new sandboxes clone.
	DefaultImage string
	// UsersPath is the users.conf bootstrap seed.
	UsersPath string
	// ProxyDomain is the base domain for web routes (hivemind.tools).
	ProxyDomain string
	// Gateway, when set, provisions this host as a fleet NODE linked to the
	// gateway at that host:port rather than as a gateway of its own. It changes
	// what gets laid down, not just what gets passed: a node has no accounts, so
	// the users.conf seed — which hard-fails without an operator key — is not
	// merely unnecessary but wrong to demand of a machine that will never
	// authenticate anyone.
	Gateway string
	// NodeName is the stable fleet node name written alongside Gateway. Empty
	// lets sparkbox serve use the machine hostname.
	NodeName string

	// --- listen addresses (all templated into the unit's ExecStart) ---
	// SSHAddr is the gateway's SSH door. Empty keeps the historical behaviour:
	// :2222, or :22 when MoveAdminSSH freed it. Setting it explicitly wins, so a
	// host with a dedicated edge IP can bind 10.66.0.1:2222 and stop competing
	// with whatever else listens on the wildcard address.
	SSHAddr string
	// ProxyAddr is the HTTP edge. Empty means defaultProxyAddr. Whatever it
	// ends up being, sparkbox.env's PROXY_PORT is derived from its port half —
	// the packet-filter script forwards any-port traffic there, so the two
	// disagreeing silently breaks every sandbox web route.
	ProxyAddr string
	// APIAddr is the control API (no auth — keep it on loopback or a private
	// address). Empty means defaultAPIAddr.
	APIAddr string
	// DNSAddr is the built-in wildcard DNS responder (dnsedge), e.g.
	// 10.66.0.1:53 for a Tailscale split-DNS entry. Empty means the responder
	// is not enabled at all, and the flag is then omitted from the unit rather
	// than passed empty.
	DNSAddr string
	// DNSAnswer is what that responder answers *.<ProxyDomain> with: one or
	// more comma-separated IPs. Empty makes `serve` fall back to the IP host of
	// ProxyAddr, which is why an edge on a concrete address needs neither.
	DNSAnswer string

	// --- optional subsystems -------------------------------------------------
	//
	// Every field below is a `sparkbox serve` flag the unit carries only when
	// this config asks for it. Each was previously reachable ONLY by hand-writing
	// it into sparkbox.env's flag bundles (F2), which is how the DGX gateway came
	// to run a configuration no `sparkbox setup` invocation could reproduce.
	//
	// "Omitted when unset" is load-bearing, not tidiness: a flag rendered with an
	// empty value becomes an empty argv word, Go's flag package stops parsing at
	// the first non-flag argument, and `serve` never inspects fs.Args() — so one
	// stray "" silently drops every flag after it and the gateway comes up
	// missing half its configuration without a word.

	// SSHAdvertisePort is the port shown in user-facing instructions when a DNAT
	// forwards it to the gateway's real listen port. The DGX advertises 22 while
	// the gateway binds :2222, so `ssh ctl@catnip.sh` works bare. 0 means "use
	// the listen port".
	SSHAdvertisePort int
	// ProxyTLS terminates TLS at the edge (see TLSProvider).
	ProxyTLS bool
	// TLSProvider is how certificates are obtained when ProxyTLS is on:
	// "cloudflare" (DNS-01 wildcard, needs CLOUDFLARE_API_TOKEN in the unit's
	// environment) or "autocert" (per-host on demand). Empty leaves `serve` on
	// its own default.
	TLSProvider string
	// TLSEmail is the ACME account address certificate-expiry notices go to.
	TLSEmail string
	// GuestDNS is the resolver handed to guests via the sparkbox_dns kernel arg:
	// the literal "gateway" (each guest's own 172.30.<idx>.1) or an IP. It is
	// half of egress control — sluice can be running and enforcing nothing if
	// guests are left on public DNS.
	GuestDNS string
	// SluiceSocket is the sluice control socket (/run/sluice.sock). Empty leaves
	// the gateway's egress syncer a silent no-op — unless Sluice is set, which
	// defaults it (see Config.sluiceSocket).
	SluiceSocket string
	// AgentTools bakes the agent CLIs (claude, codex, hivemind) plus the guest
	// workload-identity payload into the rootfs templates, and installs the
	// daily refresher that keeps them current. ON by default, unlike every other
	// field in this block: it does not change what a sandbox is allowed to do,
	// it is what makes one worth creating, and no host provisioned by the binary
	// could get it any other way. See agenttools.go.
	AgentTools bool
	// Sluice installs and enables the sluice egress gateway itself: fetch
	// sluice-linux-<arch> from the release, seed an allowlist, write the unit,
	// start it. Off by default, because turning egress filtering on is a change
	// to what a host's sandboxes can reach and must be asked for.
	//
	// It is the flag `--sluice-socket` was waiting for. Until a release
	// published a sluice binary there was no honest way to install one, so setup
	// could only offer to *talk* to a daemon somebody else had put there, and
	// doctor could only report the resulting silence (checkEgress).
	//
	// Setting it also supplies SluiceSocket and GuestDNS when those are unset:
	// a gateway with the daemon running and neither flag pushes no policy and
	// leaves guests on public DNS, which is an egress filter that filters
	// nothing. See Config.sluiceSocket and Config.guestDNS.
	Sluice bool
	// SluiceDNSAddr is where sluice's allowlist resolver binds. Empty means
	// defaultSluiceDNSAddr (:53) when Sluice is set, and nothing at all when it
	// is not. A gateway that also runs sparkbox's own wildcard responder
	// (--dns-addr) needs a concrete address here — the DGX gave it a dedicated
	// 172.30.0.53 on a dummy interface for exactly that reason.
	SluiceDNSAddr string
	// ArchiveRemote / ArchiveBucket enable sandbox archiving to object storage
	// through the host's rclone.conf. Both are required — objstore.New returns a
	// nil store when either is empty, so half a configuration silently disables
	// archiving rather than failing.
	ArchiveRemote string
	ArchiveBucket string

	// EdgeIP is the edge's own address on a dummy interface — the dedicated
	// tailnet /32 (10.66.0.1) the DGX runs. It is the one setting here that is
	// NOT a `serve` flag: sparkbox-net.sh reads it from sparkbox.env as
	// SPARKBOX_EDGE_IP, creates the `sparkedge` interface, and DNATs any-port
	// traffic by DESTINATION rather than REDIRECTing everything that arrives on
	// the uplink. Setting it also writes SPARKBOX_EDGE_REDIRECT=0, because the
	// two modes answer the same question and the uplink REDIRECT would otherwise
	// keep hijacking every inbound TCP port above 1024.
	EdgeIP string

	// --- setup only ---
	// ArtifactBase overrides DefaultArtifactBase.
	ArtifactBase string
	// Release is the artifact release tag, or "latest" for the newest
	// non-prerelease release (GitHub's /releases/latest redirect).
	Release string
	// OperatorKey is a path to (or the literal text of) the operator's SSH
	// public key seeded into users.conf. Empty auto-detects ~/.ssh/id_ed25519.pub.
	OperatorKey string
	// OperatorHandle is the users.conf handle for the operator key.
	OperatorHandle string
	// DataVolumeGB sizes the XFS reflink volume image.
	DataVolumeGB int
	// SwapGB sizes the overcommit safety-valve swapfile (0 disables).
	SwapGB int
	// MoveAdminSSH relocates the host's own sshd to :2222 so the gateway can
	// own :22. Off by default: it is the one step that can lock an operator out.
	MoveAdminSSH bool
	// DryRun prints the plan and mutates nothing.
	DryRun bool
	// FirecrackerBin is where the firecracker binary is installed.
	FirecrackerBin string
	// BinPath is where `setup` installs the sparkbox binary it is running from,
	// and the path the systemd unit's ExecStart names. The unit used to hardcode
	// /usr/local/bin/sparkbox while nothing ever put a binary there, so a
	// textbook install produced a service that could not start (and, on a host
	// with a stale binary already at that path, a service silently running the
	// *previous* release). Empty disables the install step.
	BinPath string
	// AdoptLegacy tells setup to use a sparkbox state directory that is already
	// live on this host instead of the layout DefaultConfig describes.
	//
	// The DGX is why it exists: it predates `sparkbox setup` and keeps its
	// state, images and kernel flat under /srv/sparkbox, while setup's layout is
	// <root>/data/{state,images} on an XFS volume. Without this flag setup
	// refuses such a host rather than building a second, empty data root beside
	// the live one and pointing the unit at it (F4). With it, Cfg.StateDir and
	// Cfg.ImageDir are repointed at what is already there and no volume is
	// built.
	AdoptLegacy bool
	// Force overwrites a destination binary that reports a NEWER version than
	// the one running setup. Without it a downgrade is refused: re-running an
	// old installer on an upgraded host would otherwise quietly roll it back.
	Force bool
	// --- darwin: the nested linux machine ------------------------------------
	//
	// On darwin, EVERY field above continues to describe the MACHINE's layout —
	// Root is /srv/sparkbox inside it, ProxyDomain is the domain its gateway
	// serves, --sluice installs sluice in it — and is forwarded verbatim to the
	// inner `sparkbox setup`. The five fields below are the only ones that
	// describe the Mac itself.

	// MachineName is the nested VM `setup` creates and provisions.
	//
	// It is "sparkbox", NOT poc.sh's "sparkbox-poc": the two coexist during the
	// transition and the operator retires the PoC when they are ready. Validated
	// against machine.ValidName — both because the name is the one
	// caller-supplied word that reaches a command line the guest's bash
	// re-parses, and because it must be non-empty: `container machine
	// inspect`/`stop` with no id silently operate on the DEFAULT machine.
	MachineName string
	// MachineCPUs / MachineMemGB size the machine. These are the machine's
	// whole budget, not a per-sandbox one.
	MachineCPUs  int
	MachineMemGB int
	// MachineImage overrides the gateway image reference. Empty means the
	// content-addressed default, local/sparkbox-gateway:<12 hex of the embedded
	// build context> — so an edit to any of the three embedded files produces a
	// new tag and a rebuild, which an existence check on a constant tag never
	// would.
	MachineImage string
	// OuterKernel is where the macOS OUTER kernel (the KVM-capable arm64 Image
	// Apple's `container machine` boots) lands on the Mac. Empty means
	// <Env.MacDir>/vmlinux-macos-arm64. Not to be confused with KernelPath,
	// which is the GUEST kernel a firecracker microVM boots, inside the machine.
	OuterKernel string
	// ContainerBin is Apple's container CLI. A field so a test can point it at
	// a stub and an operator at a non-standard install.
	ContainerBin string

	// Version is the release tag of the binary executing this command
	// (main.version, "dev" for a hand build). It is plumbed in rather than read
	// here because it is stamped into package main at link time; doctor compares
	// it against the binary at BinPath and the running service.
	Version string

	// FlagsGiven names the flags the operator ACTUALLY passed this run, as the
	// command line spells them ("proxy-domain"), filled from flag.FlagSet.Visit.
	//
	// It exists because a Go flag with a default is indistinguishable from one
	// the operator typed: cfg.ProxyDomain is "hivemind.tools" both when that was
	// asked for and when nothing was asked at all. That was harmless while
	// sparkbox.env was written once and never touched again, and became a live
	// hazard the moment stepEnvFile started RECONCILING managed keys — an
	// upgrade run with no --proxy-domain would have rewritten the DGX's
	// PROXY_DOMAIN=catnip.sh to hivemind.tools, moved every web route, and sent
	// certmagic off to order a DNS-01 wildcard in a zone its token cannot touch.
	//
	// Nil (a hand-built Config, or doctor) means "nothing was explicitly set",
	// which is the conservative reading: setup then corrects only what it can
	// derive rather than what it merely defaulted to.
	FlagsGiven map[string]bool

	// --- diagnostics ---
	// ServiceSettle is how long the service liveness check waits between its two
	// `systemctl show` samples. The unit sets Restart=always with no start-rate
	// limit, so `systemctl is-active` reads "active" at almost any instant of a
	// crash loop; only a second sample can tell a running gateway from one that
	// is being restarted every two seconds. It lives on Config (rather than a
	// package global or a new Probe method) because a Check is a pure function
	// of a Probe and a Config, so this is the one seam a check can already see —
	// and a zero value means "sample twice, but do not wait", which is what
	// keeps the tests instantaneous.
	ServiceSettle time.Duration
}

// DefaultConfig returns a Config with the on-host paths the systemd units and
// cloud-init already agree on, so doctor and setup describe the same layout.
func DefaultConfig() Config { return DefaultConfigAt("/srv/sparkbox") }

// DefaultConfigAt returns the standard layout rooted at root, so an operator's
// --root flag shifts every derived path together.
func DefaultConfigAt(root string) Config {
	return Config{
		Root:           root,
		StateDir:       filepath.Join(root, "data", "state"),
		ImageDir:       filepath.Join(root, "data", "images"),
		KernelPath:     filepath.Join(root, "vmlinux"),
		DefaultImage:   "universal",
		UsersPath:      filepath.Join(root, "users.conf"),
		ProxyDomain:    "hivemind.tools",
		ProxyAddr:      defaultProxyAddr,
		APIAddr:        defaultAPIAddr,
		ArtifactBase:   DefaultArtifactBase,
		Release:        "latest",
		OperatorHandle: "operator",
		DataVolumeGB:   300,
		SwapGB:         16,
		// On by default — see the header of agenttools.go. A sandbox host whose
		// sandboxes hold no agent is not a useful default, and this was
		// unreachable from the released binary at all until stepAgentTools.
		AgentTools:     true,
		FirecrackerBin: "/usr/local/bin/firecracker",
		BinPath:        "/usr/local/bin/sparkbox",
		ServiceSettle:  DefaultServiceSettle,
		// darwin only; inert on linux, where nothing reads them.
		MachineName:  defaultMachineName,
		MachineCPUs:  8,
		MachineMemGB: 24,
		ContainerBin: "container",
	}
}

// darwin machine defaults. Named here rather than inline for the same reason
// sluiceBinPath and serviceUnit are: the plan line, the create call, the
// ownership predicate and the connect banner all have to agree on one string.
const (
	defaultMachineName = "sparkbox"
	// machineImageRepo is the repository half of the gateway image reference.
	// The ownership predicate compares THIS, not the full tag — see
	// machineIsOurs.
	machineImageRepo = "local/sparkbox-gateway"
	// outerKernelName is the outer kernel's filename on the Mac. It matches the
	// release asset name so an operator who downloaded one by hand can drop it
	// in place.
	outerKernelName = "vmlinux-macos-arm64"
	// minMacOSVersion / minContainerVersion / minAppleGeneration are the host
	// floors for a nested KVM machine: Apple's --virtualization documents
	// "Apple Silicon M3+ and macOS 15+", and every transport behaviour sparkbox
	// relies on was measured on container 1.1.0 only.
	minMacOSVersion     = "15.0"
	minContainerVersion = "1.1.0"
	minAppleGeneration  = 3
)

// machineImageRef is the gateway image this build of sparkbox wants.
//
// Content-addressed by default: the tag is the first 12 hex of a hash over the
// three embedded context files and the pinned base image, so "is the image
// current?" is a content comparison rather than an existence check — the same
// lesson wantedUnits and wantedNetAssets already encode. poc.sh tags a constant
// and would keep a stale image forever after an asset edit.
func machineImageRef(c Config) string {
	if r := strings.TrimSpace(c.MachineImage); r != "" {
		return r
	}
	return machineImageRepo + ":" + macosassets.ContextSHA()[:12]
}

// outerKernelPath is where the macOS outer kernel lands on the Mac.
func (c Config) outerKernelPath(macDir string) string {
	if p := strings.TrimSpace(c.OuterKernel); p != "" {
		return p
	}
	return filepath.Join(macDir, outerKernelName)
}

// machineName is the nested VM's name, defaulted so a hand-built Config in a
// test cannot accidentally address the DEFAULT machine.
func (c Config) machineName() string {
	if n := strings.TrimSpace(c.MachineName); n != "" {
		return n
	}
	return defaultMachineName
}

// containerBin is Apple's container CLI.
func (c Config) containerBin() string {
	if b := strings.TrimSpace(c.ContainerBin); b != "" {
		return b
	}
	return "container"
}

// validateMachine rejects darwin machine settings setup cannot act on. Called
// from Provision before anything is described, so a bad name costs nothing.
func validateMachine(c Config) error {
	if !machine.ValidName(c.machineName()) {
		return fmt.Errorf("invalid --machine-name %q: lowercase letters, digits and hyphens only, "+
			"starting with a letter or digit, at most 63 characters "+
			"(the name reaches a command line the machine's own bash re-parses)", c.machineName())
	}
	if c.MachineCPUs < 0 || c.MachineMemGB < 0 {
		return fmt.Errorf("--machine-cpus and --machine-memory-gb must not be negative")
	}
	return nil
}

// DefaultServiceSettle is the production settle window for the service liveness
// check: long enough to catch the unit's RestartSec=2 loop several times over,
// short enough that nobody kills `sparkbox doctor` waiting for it.
const DefaultServiceSettle = 10 * time.Second

// flagGiven reports whether the operator named this flag on the command line,
// as opposed to it carrying a default. See Config.FlagsGiven.
func (c Config) flagGiven(name string) bool { return c.FlagsGiven[name] }

// keyDir resolves the directory holding the fleet key PEMs, defaulting to the
// state dir like `sparkbox serve` does.
func (c Config) keyDir() string {
	if c.KeyDir != "" {
		return c.KeyDir
	}
	return c.StateDir
}

// dataDir is the XFS reflink mount that holds state + images.
func (c Config) dataDir() string { return filepath.Join(c.Root, "data") }

// rootfsPath is where the default rootfs template lands on disk.
func (c Config) rootfsPath() string {
	return filepath.Join(c.ImageDir, c.DefaultImage+".ext4")
}

// envPath is the non-secret host config the systemd units source.
func (c Config) envPath() string { return filepath.Join(c.Root, "sparkbox.env") }

// --- listen addresses -------------------------------------------------------
//
// These resolvers (rather than the raw fields) are what every renderer, plan
// line and preflight probe reads, so a Config built by hand in a test and one
// built by DefaultConfig can never describe different listeners.

// sshAddr is where the gateway's SSH door binds.
//
// An explicit --ssh-addr wins over the --move-admin-ssh default. The two are
// rejected together unless the explicit address is on port 22 (validateAddrs):
// --move-admin-ssh exists solely to evict the host's sshd from :22 so the
// gateway can have it, and it parks that sshd on :2222 — so asking for both a
// moved admin sshd and a gateway anywhere other than :22 either collides with
// the sshd we just moved or leaves :22 unclaimed by anyone. Refusing is
// cheaper to explain than either outcome.
func (c Config) sshAddr() string {
	if a := strings.TrimSpace(c.SSHAddr); a != "" {
		return a
	}
	if c.MoveAdminSSH {
		return adminSSHAddr
	}
	return defaultSSHAddr
}

func (c Config) proxyAddr() string {
	if a := strings.TrimSpace(c.ProxyAddr); a != "" {
		return a
	}
	return defaultProxyAddr
}

func (c Config) apiAddr() string {
	if a := strings.TrimSpace(c.APIAddr); a != "" {
		return a
	}
	return defaultAPIAddr
}

// dnsAddr is empty when the wildcard DNS responder is not wanted, and the unit
// then omits --dns-addr entirely rather than passing it empty.
func (c Config) dnsAddr() string { return strings.TrimSpace(c.DNSAddr) }

// --- sluice ------------------------------------------------------------------
//
// The three resolvers below are the ONE place --sluice's implications are
// worked out, and everything downstream reads them: the rendered sluice unit,
// the gateway's --guest-dns/--sluice-socket flags, the port preflight, the
// plan text and the validator. Deriving in a step (or in cmd/sparkbox) instead
// would have meant the unit and the preflight disagreeing about which address
// is about to be bound, which is the class of bug F1 was.

// sluiceDNSAddr is where sluice's allowlist resolver binds, and "" when sluice
// is not being installed — so validateAddrs and the port preflight skip it by
// the same "empty means off" rule --dns-addr already uses.
func (c Config) sluiceDNSAddr() string {
	if !c.Sluice {
		return ""
	}
	if a := strings.TrimSpace(c.SluiceDNSAddr); a != "" {
		return a
	}
	return defaultSluiceDNSAddr
}

// sluiceSocket is the control socket path the gateway pushes policy to. An
// explicit --sluice-socket wins (a host may already run sluice from somewhere
// else); --sluice supplies the default; otherwise there is none and the
// gateway's egress syncer stays a no-op.
func (c Config) sluiceSocket() string {
	if s := strings.TrimSpace(c.SluiceSocket); s != "" {
		return s
	}
	if c.Sluice {
		return defaultSluiceSocket
	}
	return ""
}

// guestDNS is the resolver handed to guests through the sparkbox_dns kernel
// arg. --sluice implies one, because a filter guests do not resolve through is
// not a filter: they keep using whatever public resolver the image ships with
// and never meet the allowlist at all. checkEgress reports precisely that state
// on hosts that got there by hand.
//
// The derivation follows the address sluice will bind. A concrete one
// (172.30.0.53) is what guests must be pointed at literally; a wildcard means
// the resolver answers on every interface — including the host end of each
// guest's own tap — so "gateway" is right, and it has the advantage of needing
// no extra address to exist on the box at all.
func (c Config) guestDNS() string {
	if d := strings.TrimSpace(c.GuestDNS); d != "" {
		return d
	}
	if !c.Sluice {
		return ""
	}
	if host, _, err := splitAddr(c.sluiceDNSAddr()); err == nil && !isWildcardHost(host) {
		return host
	}
	return "gateway"
}

// sluiceResolverIP is the concrete IP sluice's resolver binds, when it has one
// this host must actually hold — and "" for every other shape.
//
// It exists because recommending an address is not the same as creating it.
// validateSluice tells an operator whose gateway already runs the wildcard DNS
// responder to give sluice `--sluice-dns-addr 172.30.0.53:53`, guestDNS then
// hands guests that literal, and NOTHING put 172.30.0.53 on the box: the bind
// failed with EADDRNOTAVAIL, the port preflight steps over that error by
// design, systemd's `enable --now` returned 0 at the fork, and Restart=always
// looped forever while `setup` printed a green report. So sparkbox-net.sh now
// creates the address on a dummy interface (SLUICE_DNS_IP), exactly as it
// already does for a dedicated --edge-ip, and this is the derivation both the
// fresh-host render and the reconcile read.
//
// A wildcard (:53) needs no address of its own — it binds everything the host
// already has — and a loopback one is refused outright by validateSluice, so
// the only case left is the concrete one.
func (c Config) sluiceResolverIP() string {
	if !c.Sluice {
		return ""
	}
	host, _, err := splitAddr(c.sluiceDNSAddr())
	if err != nil || isWildcardHost(host) {
		return ""
	}
	if net.ParseIP(host) == nil {
		// A hostname would have to be resolved to be added to an interface, and
		// resolving it here would bake today's answer into a boot script.
		return ""
	}
	return host
}

// sluiceEnvPath is the EnvironmentFile the sluice unit sources for SLUICE_ARGS.
func (c Config) sluiceEnvPath() string { return filepath.Join(c.Root, "sluice.env") }

// sluiceAllowlistPath is the allowlist sluice resolves against. Mandatory, and
// the exit code says which kind of "missing" it was: sluice exits 1 when the
// path its --allowlist names cannot be opened (the case this seed prevents) and
// 2 only when the FLAG itself is empty, which the rendered unit cannot produce
// because it always templates this path. So a host that never got the seed
// crash-loops with status=1/FAILURE, not 2 — worth knowing when reading its
// journal.
func (c Config) sluiceAllowlistPath() string { return filepath.Join(c.Root, "allowlist.txt") }

// proxyPortNum is the port half of the edge address, and the single derivation
// point for sparkbox.env's PROXY_PORT.
//
// This is the coupling the old `const proxyPort = 8081` was trying to hold
// together and could not: the moment an operator put `--proxy-addr :443` in a
// flag bundle, the unit listened on 443 while PROXY_PORT still said 8081 and
// sparkbox-net.sh went on REDIRECTing every any-port connection to a port
// nothing was bound to. Deriving both renders from one address makes that
// desync unrepresentable.
func (c Config) proxyPortNum() (int, error) {
	_, port, err := splitAddr(c.proxyAddr())
	return port, err
}
