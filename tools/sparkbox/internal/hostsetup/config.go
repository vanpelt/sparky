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
	"time"
)

// DefaultArtifactBase is the GitHub Releases endpoint the release workflow
// publishes to. Assets are a flat, arch-suffixed namespace under
// <base>/download/<tag>/ (or <base>/latest/download/ for the newest
// non-prerelease): manifest-<arch>.env, vmlinux-<arch>, firecracker-<arch>,
// <rootfs>-<arch>.ext4.zst, sparkbox-linux-<arch>.
const DefaultArtifactBase = "https://github.com/vanpelt/sparky/releases"

// proxyPort is the HTTP edge port. The standalone unit's --proxy-addr and
// sparkbox.env's PROXY_PORT (which sparkbox-net.sh DNATs any-port traffic to)
// must agree, so both renders read this one constant.
const proxyPort = 8081

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
	// Force overwrites a destination binary that reports a NEWER version than
	// the one running setup. Without it a downgrade is refused: re-running an
	// old installer on an upgraded host would otherwise quietly roll it back.
	Force bool
	// Version is the release tag of the binary executing this command
	// (main.version, "dev" for a hand build). It is plumbed in rather than read
	// here because it is stamped into package main at link time; doctor compares
	// it against the binary at BinPath and the running service.
	Version string

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
