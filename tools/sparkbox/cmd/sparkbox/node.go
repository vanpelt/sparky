package main

// Node mode: what `sparkbox serve --gateway <host:port>` runs instead of a
// gateway.
//
// A node is a machine that holds VMs and nothing else. Every fleet-wide surface
// — accounts, secrets, routes, schedules, the OIDC issuer, the SSH gateway, the
// HTTP edge, both consoles, the REST API — belongs to the one gateway, and a
// node that ran a second copy of any of them would be a second source of truth
// for something that must have exactly one. What is left is the part that
// cannot be moved off the machine holding the VMs: a driver, a host manager,
// and one outbound link that reports what is here.
//
// The single property everything in this file is arranged around is that the
// link's health never reaches the VMs. serveNode has no error channel: a
// gateway is a machine that gets restarted, and a node whose serve loop
// returned when its link died would, under systemd's Restart=always, cold-boot
// every sandbox on this machine every time the gateway was redeployed.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// guestSubnet is the /16 every sparkbox host carves its per-VM tap pairs out of
// (internal/vmm/firecracker). It is reported in the hello so a gateway can say
// out loud what it already assumes: every node mints the same guest addresses,
// which is exactly why no guest address ever crosses the link.
const guestSubnet = "172.30.0.0/16"

// nodeOptions is the slice of `serve`'s flags a node actually uses. It is a
// struct rather than a longer parameter list because most of these are limits
// that must mean the same thing on a node as on a gateway — the reaper's idle
// clocks, the admission budgets, the disk pool — and a positional list of
// fifteen of them is how two of them quietly swap places.
type nodeOptions struct {
	// gateway is host:port. gatewayPub and gatewayHostKey are both optional
	// pins: the first is learned from the welcome, the second on first use.
	gateway        string
	gatewayPub     string
	gatewayHostKey string

	nodeName   string
	arch       string
	driverName string
	stateDir   string
	keyDir     string

	kernelPath   string
	imageDir     string
	defaultLogin string
	subnet6      string
	guestDNS     string
	sluiceSocket string

	idleBalloon   time.Duration
	idleTimeout   time.Duration
	activityCPU   float64
	activityNetKB int64
	maxPerOwner   int
	memAdmitPct   int
	hostMemMB     int64
	memReserve    int64
	diskPool      int64

	log *slog.Logger
}

// serveNode runs this machine as a fleet node until it is signalled, mirroring
// serve()'s own shutdown contract so systemd stops both the same way.
func serveNode(opts nodeOptions) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runNode(ctx, opts)
}

// runNode is serveNode with its shutdown signal supplied, so a test can end it
// without raising one at the whole process.
func runNode(ctx context.Context, opts nodeOptions) error {
	log := opts.log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if err := os.MkdirAll(opts.stateDir, 0o700); err != nil {
		return err
	}
	keysIn := opts.keyDir
	if keysIn == "" {
		keysIn = opts.stateDir
	}
	// LoadOrCreateKey unconditionally, never the --require-keys LoadKey swap.
	// That switch exists so a missing *fleet* key is a hard failure instead of a
	// silently-minted new fleet identity; a node's own key is neither. It is
	// minted once, on the first boot of a machine that has never linked, and a
	// node that refused to mint it could never enrol in the first place.
	nodeKey, err := sshgw.LoadOrCreateKey(keysIn, "node_key")
	if err != nil {
		return fmt.Errorf("node key: %w", err)
	}
	log.Info("node identity", "node", opts.nodeName,
		"fingerprint", xssh.FingerprintSHA256(nodeKey.PublicKey()))

	var driver vmm.Driver
	switch opts.driverName {
	case "mock":
		// The node key doubles as the fake guest's host key. A node holds no
		// gateway host key, and minting one here would leave a gateway identity
		// lying on a machine that must never be a gateway.
		md := mock.New(opts.stateDir, nodeKey)
		md.LoginUser = opts.defaultLogin
		driver = md
	case "firecracker":
		driver, err = newFirecrackerDriver(opts.kernelPath, opts.imageDir, opts.stateDir, opts.subnet6, opts.defaultLogin, opts.guestDNS)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown driver %q", opts.driverName)
	}
	defer driver.Close()

	// The gateway's upstream public key is the only gateway material a node ever
	// holds, and it arrives over the link. Preloading the cached copy is what
	// lets a node that reboots during a gateway outage resume its pinned VMs:
	// Create refuses outright while this is unknown, since a sandbox booted
	// without it is one nobody can log into.
	gwPubPath := filepath.Join(opts.stateDir, "gateway_upstream_key.pub")
	gwPub, err := gatewayUpstreamKey(opts.gatewayPub, gwPubPath)
	if err != nil {
		return err
	}
	if gwPub == "" {
		log.Info("waiting to learn the gateway's upstream key", "detail", "sandboxes cannot be created here until the first link completes")
	}

	// An out-of-band pin, when the operator has one. Without it the link trusts
	// the first host key offered and refuses every later change — weaker than a
	// seeded pin, and much stronger than accepting whatever answers on that
	// address today, which is all a node with no operator at the keyboard could
	// otherwise do.
	var hostKeyPin xssh.PublicKey
	if opts.gatewayHostKey != "" {
		if hostKeyPin, err = readPublicKey(opts.gatewayHostKey); err != nil {
			return fmt.Errorf("--gateway-host-key: %w", err)
		}
	}

	// The emitter is this node's whole view of the gateway: it is installed as
	// both the manager's Observer and its SessionCloser, and it drops rather
	// than blocks, because both hooks fire inside the manager's lock.
	emitter := nodelink.NewEmitter(log)

	hostMem := opts.hostMemMB
	if hostMem == 0 {
		hostMem = detectHostMemMB()
	}
	// Routes, Schedules, Tags, Archive and FrontDoor are all nil and stay nil:
	// each is backed by a store or a DNS zone the gateway owns. A node keeps
	// only what describes the VMs on this machine.
	mgr, err := host.NewManager(host.Options{
		StateDir: opts.stateDir, Driver: driver,
		GatewayPublicKey: gwPub, Logger: log,
		MaxRunningPerOwner: opts.maxPerOwner,
		MemAdmissionPct:    opts.memAdmitPct,
		HostMemMB:          hostMem,
		MemReserveMB:       opts.memReserve,
		DiskPoolMBPerOwner: opts.diskPool,
		ActivityCPUPct:     opts.activityCPU,
		ActivityNetBytes:   uint64(opts.activityNetKB) * 1024,
		NodeName:           opts.nodeName,
		Arch:               opts.arch,
		Release:            version,
		HostVCPUs:          int64(runtime.NumCPU()),
		Observer:           emitter,
	})
	if err != nil {
		return err
	}
	// Sessions terminate at the gateway, so there is nothing here to hang up.
	// The emitter is installed anyway: what the gateway needs is the news that a
	// pause is coming, early enough to hang up its own.
	mgr.SetSessions(emitter)

	// Egress policy still runs per machine — sluice enforces against this host's
	// own taps — so the syncer is wired exactly as on a gateway, minus the rules
	// store, which lives up there. Nothing is governed here yet: no path carries
	// tag bindings across the link, and an ungoverned VM is left unrestricted
	// rather than denied.
	var sluiceClient *netpush.Client
	if opts.sluiceSocket != "" {
		sluiceClient = netpush.NewClient(opts.sluiceSocket)
		log.Info("sluice egress policy enabled", "socket", opts.sluiceSocket)
	}
	netSyncer := netpush.NewSyncer(sluiceClient, netpushFleet{mgr}, ungovernedRules{}, log)

	// The guest metadata service is a gateway surface: it answers a guest with
	// an id token signed by the fleet's OIDC key, which a node does not hold and
	// must not. A sandbox placed here therefore gets no id token, and one line
	// at boot beats a guest discovering it at runtime.
	log.Warn("guest metadata service disabled on this node",
		"reason", "id tokens are signed by the gateway's OIDC key, which a node never holds")

	// A process restart marks every sandbox paused; bring the pinned ones back
	// up, exactly as a gateway does, and — the point of doing it here, before
	// the link is up — without waiting on a gateway that may be down.
	mgr.ResumePinned(ctx)
	go mgr.RunReaper(ctx, opts.idleBalloon, opts.idleTimeout, time.Minute)
	if netSyncer.Enabled() {
		go pushLoop(ctx, netSyncer, log)
	}

	log.Info("sparkbox node up", "node", opts.nodeName, "gateway", opts.gateway,
		"driver", opts.driverName, "state_dir", opts.stateDir, "arch", opts.arch)

	// The supervisor gets a goroutine and no error channel. RunClient returns
	// only when ctx is cancelled or when it was configured impossibly, so the
	// only value that can arrive here before shutdown is a startup mistake worth
	// exiting on — never a transport failure, which is the whole contract.
	linked := make(chan error, 1)
	go func() {
		linked <- nodelink.RunClient(ctx, nodelink.ClientOptions{
			Gateway: opts.gateway, NodeName: opts.nodeName, Key: nodeKey,
			HostKey:     hostKeyPin,
			HostKeyPath: filepath.Join(opts.stateDir, "gateway_host_key.pub"),
			Manager:     mgr, Emitter: emitter,
			Hello:     func() nodelink.Hello { return nodeHello(opts, mgr) },
			OnWelcome: func(w nodelink.Welcome) error { return acceptWelcome(w, mgr, gwPubPath) },
			Log:       log,
		})
	}()

	select {
	case <-ctx.Done():
	case err := <-linked:
		return err
	}
	log.Info("shutting down")
	// Give the supervisor its moment to deliver the goodbye, so the gateway
	// marks this machine offline now rather than waiting out the grace period
	// wondering. Bounded: a gateway that is already gone owes us nothing.
	select {
	case <-linked:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// nodeHello is what this machine tells a gateway about itself. Everything in it
// is a fact the gateway cannot observe from the outside and needs in order to
// decide whether a sandbox can be placed here at all.
func nodeHello(opts nodeOptions, mgr *host.Manager) nodelink.Hello {
	return nodelink.Hello{
		Arch: opts.arch,
		// Release and Version are the same string on purpose: hack/stage-artifacts.sh
		// stamps the release tag straight into the binary, so this build's version
		// IS the release it shipped in. A hand-built binary says "dev" for both,
		// which is also the honest answer.
		Release: version, Version: version,
		Driver:      opts.driverName,
		Images:      imageNames(opts.imageDir),
		Archiving:   mgr.ArchivingEnabled(),
		Snapshots:   mgr.Snapshotter(),
		GuestSubnet: guestSubnet,
	}
}

// acceptWelcome installs the key this node's guests will trust and remembers it.
//
// A failure here fails the link by design: the only thing a node does with a
// welcome is learn that key, and a machine that could not learn it must not go
// on to boot VMs nobody can log into.
func acceptWelcome(w nodelink.Welcome, mgr *host.Manager, cachePath string) error {
	line := strings.TrimSpace(w.GatewayUpstreamPub)
	if line == "" {
		return errors.New("the gateway sent no upstream public key")
	}
	if _, _, _, _, err := xssh.ParseAuthorizedKey([]byte(line)); err != nil {
		return fmt.Errorf("the gateway's upstream key is not a public key: %w", err)
	}
	mgr.SetGatewayPublicKey(line + "\n")
	// Cached, not just held: a node that reboots while the gateway is down has
	// to be able to resume its pinned VMs without asking anyone.
	if err := os.WriteFile(cachePath, []byte(line+"\n"), 0o644); err != nil { //nolint:gosec // a public key
		return fmt.Errorf("caching the gateway's upstream key: %w", err)
	}
	return nil
}

// gatewayUpstreamKey resolves the authorized_keys line new guests will trust:
// the operator's --gateway-pubkey when given, else the copy cached from the
// last welcome. Empty when neither exists, which is a first boot — the link
// will supply it, and Create refuses until then.
func gatewayUpstreamKey(flagValue, cachePath string) (string, error) {
	if flagValue != "" {
		key, err := readPublicKey(flagValue)
		if err != nil {
			return "", fmt.Errorf("--gateway-pubkey: %w", err)
		}
		return string(xssh.MarshalAuthorizedKey(key)), nil
	}
	data, err := os.ReadFile(cachePath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// readPublicKey resolves a flag that carries either an authorized_keys line or
// the path of a file holding one.
//
// The value is tried as a key first and only then as a path, which needs no
// prefix convention to disambiguate: a filesystem path never parses as a public
// key, and a public key line is never a usable path.
func readPublicKey(value string) (xssh.PublicKey, error) {
	text := []byte(value)
	if _, _, _, _, err := xssh.ParseAuthorizedKey(text); err != nil {
		data, rerr := os.ReadFile(value)
		if rerr != nil {
			return nil, fmt.Errorf("%q is neither an authorized_keys line nor a readable file: %w", value, rerr)
		}
		text = data
	}
	key, _, _, _, err := xssh.ParseAuthorizedKey(text)
	if err != nil {
		return nil, err
	}
	return key, nil
}

// imageNames lists the rootfs templates this machine can boot, so a gateway can
// decline to place a sandbox on a node that lacks its image.
//
// An unset or unreadable directory reports nothing rather than failing: the
// mock driver has no templates at all, and a node with no images is a node
// nothing gets placed on — not a node that refuses to start. os.ReadDir returns
// entries sorted by name, and stripping a shared suffix preserves that.
func imageNames(dir string) []string {
	if dir == "" {
		return nil
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if name, ok := strings.CutSuffix(e.Name(), ".ext4"); ok {
			out = append(out, name)
		}
	}
	return out
}

// ungovernedRules is a node's egress-rule source: nothing on this machine is
// governed, because tag-to-rule bindings live in the gateway's store and no
// path yet carries them across the link.
//
// The syncer is wired to it anyway rather than left nil, because "governed by
// nothing" and "not wired at all" are different policies with different
// failures: the first pushes an empty policy set, which sluice reads as leaving
// these taps unrestricted, and the second would be a nil interface for the
// push loop to trip over.
type ungovernedRules struct{}

func (ungovernedRules) AllowForSandbox(string, string) ([]string, bool, error) {
	return nil, false, nil
}
