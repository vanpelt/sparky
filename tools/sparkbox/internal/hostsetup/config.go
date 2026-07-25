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
	"path/filepath"
	"strings"
	"time"
)

// DefaultArtifactBase is the GitHub Releases endpoint the release workflow
// publishes to. Assets are a flat, arch-suffixed namespace under
// <base>/download/<tag>/ (or <base>/latest/download/ for the newest
// non-prerelease): manifest-<arch>.env, vmlinux-<arch>, firecracker-<arch>,
// <rootfs>-<arch>.ext4.zst, sparkbox-linux-<arch>.
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
	// the gateway's egress syncer a silent no-op. See checkEgress: setup cannot
	// install sluice itself yet.
	SluiceSocket string
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
		FirecrackerBin: "/usr/local/bin/firecracker",
		BinPath:        "/usr/local/bin/sparkbox",
		ServiceSettle:  DefaultServiceSettle,
	}
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
