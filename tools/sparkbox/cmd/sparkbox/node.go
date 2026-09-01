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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/grpcidentity"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/hivemindpresence"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodepki"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

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
	vmStateDir string
	keyDir     string

	kernelPath              string
	imageDir                string
	templateDir             string
	jailerBin               string
	jailerChrootBase        string
	jailerUIDBase           int
	chrootJailer            bool
	privilegedHelperSocket  string
	privilegedHelperBin     string
	helperControllerGID     int
	disableHostRootfsMounts bool
	defaultLogin            string
	guestSubnet             string
	subnet6                 string
	guestDNS                string
	sluiceSocket            string
	// metaAddr is where the guest metadata service binds. Empty disables it,
	// which is what a node that should hand out no workload identity wants —
	// but the default is the same port a gateway uses, because a guest asks its
	// own default gateway on a fixed port and has no way to be told otherwise.
	metaAddr string
	// toolsDir is this node's OWN agent-CLI cache, served to its own guests and
	// NEVER relayed to the gateway. Identity and repo access relay because a
	// fleet has one signing key and one attachment ledger; tools are neither.
	// This machine runs the same refresher on the same timer, so the artifacts
	// are already here, and pulling a ~92MB tarball over the fleet link to hand
	// a guest bytes off the local disk would defeat the entire point.
	toolsDir string
	// guestSelfSnapshot lets a guest capture itself into one of its own tags.
	// The same operator switch the gateway carries, threaded here so a fleet
	// answers the same way on every machine in it.
	guestSelfSnapshot bool

	idleBalloon      time.Duration
	idleTimeout      time.Duration
	activityCPU      float64
	activityNetKB    int64
	maxPerOwner      int
	maxBoxesPerOwner int
	memAdmitPct      int
	hostMemMB        int64
	memReserve       int64
	ownerMemPool     int64
	ownerMemBurst    int64
	diskPool         int64
	hivemindAPI      string
	hivemindAudience string
	hivemindInterval time.Duration

	controlTransport   string
	grpcAddr           string
	guestDataTransport string
	metricsAddr        string
	metrics            *fleetmetrics.Registry
	log                *slog.Logger
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
	if opts.metrics == nil {
		opts.metrics = fleetmetrics.New()
	}
	if err := os.MkdirAll(opts.stateDir, 0o700); err != nil {
		return err
	}
	opts.vmStateDir = effectiveVMStateDir(opts.vmStateDir, opts.stateDir)
	if err := os.MkdirAll(opts.vmStateDir, 0o700); err != nil {
		return err
	}
	guestNetwork, err := guestnet.Parse(opts.guestSubnet)
	if err != nil {
		return err
	}
	opts.guestSubnet = guestNetwork.String()
	if opts.metricsAddr != "" {
		listener, err := net.Listen("tcp", opts.metricsAddr)
		if err != nil {
			return fmt.Errorf("node metrics listen: %w", err)
		}
		metricsServer := &http.Server{
			Handler:           opts.metrics.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
		}
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = metricsServer.Shutdown(shutdownCtx)
		}()
		go func() {
			if err := metricsServer.Serve(listener); err != nil &&
				!errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
				log.Error("node metrics listener stopped", "err", err)
			}
		}()
		log.Info("node metrics enabled", "addr", listener.Addr())
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
		md := mock.New(opts.vmStateDir, nodeKey)
		md.LoginUser = opts.defaultLogin
		driver = md
	case "firecracker":
		driver, err = newFirecrackerDriver(
			opts.kernelPath, opts.imageDir, opts.templateDir, opts.vmStateDir,
			opts.jailerBin, opts.jailerChrootBase, opts.jailerUIDBase,
			opts.chrootJailer, opts.privilegedHelperSocket, opts.privilegedHelperBin,
			opts.helperControllerGID, opts.disableHostRootfsMounts,
			opts.guestSubnet, opts.subnet6, opts.defaultLogin, opts.guestDNS,
		)
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
	var controlState *nodeControlState
	if opts.controlTransport == "grpc" && opts.grpcAddr == "" {
		return errors.New("--node-control-transport=grpc requires --node-grpc-addr")
	}
	if opts.grpcAddr != "" && opts.controlTransport != "ssh" {
		controlState, err = openNodeControlState(opts.stateDir, log)
		if err != nil {
			return err
		}
		defer controlState.Close(log)
	}
	var observer host.Observer = emitter
	if controlState != nil {
		observer = host.Observers{emitter, controlState.observer}
	}

	hostMem := opts.hostMemMB
	if hostMem == 0 {
		hostMem = detectHostMemMB()
	}
	// Routes, Schedules, Tags, Archive and FrontDoor are all nil and stay nil:
	// each is backed by a store or a DNS zone the gateway owns. A node keeps
	// only what describes the VMs on this machine.
	mgr, err := host.NewManager(host.Options{
		Context: ctx, StateDir: opts.stateDir, Driver: driver,
		GatewayPublicKey: gwPub, Logger: log,
		MaxRunningPerOwner:   opts.maxPerOwner,
		MaxSandboxesPerOwner: opts.maxBoxesPerOwner,
		MemAdmissionPct:      opts.memAdmitPct,
		HostMemMB:            hostMem,
		MemReserveMB:         opts.memReserve,
		OwnerMemoryPoolMB:    opts.ownerMemPool,
		OwnerMemoryBurstMB:   opts.ownerMemBurst,
		DiskPoolMBPerOwner:   opts.diskPool,
		ActivityCPUPct:       opts.activityCPU,
		ActivityNetBytes:     uint64(opts.activityNetKB) * 1024,
		NodeName:             opts.nodeName,
		Arch:                 opts.arch,
		Release:              version,
		HostVCPUs:            int64(runtime.NumCPU()),
		Observer:             observer,
		Metrics:              opts.metrics,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := mgr.FlushActivity(); err != nil {
			log.Warn("final activity flush failed", "err", err)
		}
	}()
	// Sessions terminate at the gateway, so there is nothing here to hang up.
	// The emitter is installed anyway: what the gateway needs is the news that a
	// pause is coming, early enough to hang up its own.
	mgr.SetSessions(emitter)

	// Egress policy runs per machine — sluice enforces against this host's own
	// taps and can see no others — so the syncer is wired exactly as on a
	// gateway, minus the rules store, which lives up there.
	//
	// The rules ARRIVE now rather than being resolved here: the gateway resolves
	// each of this machine's sandboxes against the ledger's owner column and
	// pushes the whole snapshot down (nodelink.TypeNetPolicy), and the syncer's
	// Apply turns those names into this machine's own taps. So the syncer is
	// given no rule source at all — a node that resolved its own would be a
	// second, tagless answer racing the one being sent to it.
	var sluiceClient *netpush.Client
	if opts.sluiceSocket != "" {
		sluiceClient = netpush.NewClient(opts.sluiceSocket)
		log.Info("sluice egress policy enabled", "socket", opts.sluiceSocket)
	} else {
		log.Info("egress control not enabled on this node",
			"detail", "no --sluice-socket, so nothing here is filtered or metered")
	}
	netSyncer, err := netpush.NewSyncerForSubnet(sluiceClient, netpushFleet{mgr}, ungovernedRules{}, opts.guestSubnet, log)
	if err != nil {
		return fmt.Errorf("guest subnet for network accounting: %w", err)
	}

	// The guest metadata service runs here exactly as it does on a gateway. What
	// differs is only who signs: a node holds no OIDC key and must not, so its
	// Identity is a relay that names the sandbox and lets the gateway resolve
	// everything about it. The uplink is what carries that request, and it is
	// created before the link exists because a guest can boot and ask before
	// this machine has ever reached its gateway — it is answered 503 and its own
	// timer retries, which is the whole of the degradation.
	uplink := nodelink.NewUplink()
	identityRelay := newRelayIdentity(uplink, opts.controlTransport, log)
	defer identityRelay.Close()
	// Repo attachments relay over the same control connection the identity
	// relay owns: currentGRPC is that relay's accessor, so configureGRPC and
	// setGRPC keep being the only places the transport's lifecycle is decided.
	// A second dialer here would mean two connections to one gateway and two
	// opinions about when mTLS is healthy.
	reposRelay := newRelayRepos(uplink, identityRelay.currentGRPC, log)
	if err := startNodeControl(ctx, opts, mgr, netSyncer, uplink, controlState, identityRelay, log); err != nil {
		return err
	}

	// A process restart marks every sandbox paused; bring the pinned ones back
	// up, exactly as a gateway does, and — the point of doing it here, before
	// the link is up — without waiting on a gateway that may be down.
	mgr.ResumePinned(ctx)
	if opts.hivemindAPI != "" {
		monitor, err := hivemindpresence.New(hivemindpresence.Options{
			APIBase: opts.hivemindAPI, Audience: opts.hivemindAudience,
			Sandboxes: mgr, Protector: mgr, Identity: identityRelay,
			Observer: mgr,
			Logger:   log, UserAgent: "sparkbox/" + version,
		})
		if err != nil {
			return err
		}
		go monitor.Run(ctx, opts.hivemindInterval)
		log.Info("HiveMind session presence enabled",
			"api", opts.hivemindAPI, "interval", opts.hivemindInterval)
	}
	go mgr.RunReaper(ctx, opts.idleBalloon, opts.idleTimeout, time.Minute)
	go mgr.RunMemoryPressureController(ctx, time.Minute)
	// No push loop here. On a gateway that ticker is what reconciles rule
	// changes the console did not push; on a node there is nothing local to
	// reconcile FROM — the policy is whatever the gateway last sent — and a
	// second loop re-applying a stale snapshot would fight the one being pushed.
	// The gateway's own loop covers this machine on the same cadence.
	if opts.metaAddr != "" {
		meta, err := metadata.NewChecked(metadata.Options{
			Manager: mgr, Logger: log,
			Identity:     identityRelay,
			RouteControl: relayRouteControl{up: uplink},
			Repos:        reposRelay,
			Tools:        localTools(opts.toolsDir),
			// Wired on the node in the same release as on the gateway, and that
			// is not a scheduling nicety. metadata's own rule (repos.go's
			// githubError) is that a guest must not be able to tell which
			// machine its sandbox landed on from the status it got, and a 501
			// here beside a 202 on the gateway is exactly that leak.
			SelfLifecycle:     relaySelfLifecycle{up: uplink},
			AllowSelfSnapshot: opts.guestSelfSnapshot,
			GuestSubnet:       opts.guestSubnet,
			// No default audience here: the gateway substitutes its own, which
			// is the only one that could be right — the allowlist that decides
			// whether an audience is permitted lives with the issuer.
		})
		if err != nil {
			return fmt.Errorf("guest metadata subnet: %w", err)
		}
		go func() {
			if err := meta.ListenAndServe(ctx, opts.metaAddr); err != nil && ctx.Err() == nil {
				log.Error("guest metadata service stopped", "err", err)
			}
		}()
		log.Info("guest metadata service enabled", "addr", opts.metaAddr,
			"signing", "relayed to the gateway", "tools_dir", opts.toolsDir)
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
			Uplink:    uplink,
			Net:       netSyncer,
			Hello:     func() nodelink.Hello { return nodeHello(opts, mgr, netSyncer) },
			OnWelcome: func(w nodelink.Welcome) error { return acceptWelcome(w, mgr, gwPubPath) },
			Log:       log,
			Metrics:   opts.metrics,
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

// relayIdentity is a node's metadata.Identity: it names the sandbox and lets
// the gateway decide everything else. A negotiated mTLS gRPC client is
// preferred; the old SSH uplink remains the compatibility path when the
// gateway omitted an identity endpoint or that transport is temporarily down.
//
// Deliberately the thinnest possible object. It sends the name and the audience
// and nothing more — no owner, no image, no claims — because a relay that
// asserted any of those would be a machine describing its own guests to the
// thing that signs for them. The gateway resolves all of it from its ledger;
// see internal/fleet/identity.go for the check that makes that binding.
type relayIdentity struct {
	up   *nodelink.Uplink
	mode string
	log  *slog.Logger

	mu   sync.RWMutex
	grpc gatewayIdentityClient
}

type gatewayRouteControl struct {
	fleet *fleet.Fleet
	node  string
}

func (c gatewayRouteControl) SetVisibility(ctx context.Context, box *host.Sandbox, visibility string) (metadata.RouteVisibility, error) {
	resp, err := c.fleet.SelfVisibility(ctx, c.node, nodelink.SelfVisibilityReq{Sandbox: box.Name, Visibility: visibility})
	return metadata.RouteVisibility{Sandbox: resp.Sandbox, Visibility: resp.Visibility, Routes: resp.Routes}, err
}

func (c gatewayRouteControl) SetPort(ctx context.Context, box *host.Sandbox, port int) (metadata.RoutePort, error) {
	resp, err := c.fleet.SelfPort(ctx, c.node, nodelink.SelfPortReq{Sandbox: box.Name, Port: port})
	return metadata.RoutePort{Sandbox: resp.Sandbox, Port: resp.Port}, err
}

type relayRouteControl struct{ up *nodelink.Uplink }

func (c relayRouteControl) SetVisibility(ctx context.Context, box *host.Sandbox, visibility string) (metadata.RouteVisibility, error) {
	var resp nodelink.SelfVisibilityResp
	err := c.up.Request(ctx, nodelink.TypeSelfVisibility,
		nodelink.SelfVisibilityReq{Sandbox: box.Name, Visibility: visibility}, &resp)
	return metadata.RouteVisibility{Sandbox: resp.Sandbox, Visibility: resp.Visibility, Routes: resp.Routes}, err
}

func (c relayRouteControl) SetPort(ctx context.Context, box *host.Sandbox, port int) (metadata.RoutePort, error) {
	var resp nodelink.SelfPortResp
	err := c.up.Request(ctx, nodelink.TypeSelfPort,
		nodelink.SelfPortReq{Sandbox: box.Name, Port: port}, &resp)
	return metadata.RoutePort{Sandbox: resp.Sandbox, Port: resp.Port}, err
}

// gatewaySelfLifecycle is the guest lifecycle path on a GATEWAY — and it goes
// through the fleet, with this machine's own node name, exactly as
// gatewayRouteControl does.
//
// That is the point rather than an accident of reuse: the gateway's own guests
// and a node's guests then take one authorization path (fleet.selfServiceBox,
// the placement-ledger check) instead of two that agree today and can drift
// tomorrow.
type gatewaySelfLifecycle struct {
	fleet *fleet.Fleet
	node  string
}

func (c gatewaySelfLifecycle) Pause(ctx context.Context, box *host.Sandbox) error {
	_, err := c.fleet.SelfPause(ctx, c.node, nodelink.SelfPauseReq{Sandbox: box.Name})
	return err
}

func (c gatewaySelfLifecycle) PlanSnapshot(ctx context.Context, box *host.Sandbox, tag, name string) (ctlops.SelfSnapshotPlan, error) {
	resp, err := c.fleet.SelfSnapshotPlan(ctx, c.node,
		nodelink.SelfSnapshotPlanReq{Sandbox: box.Name, Tag: tag, Name: name})
	if err != nil {
		return ctlops.SelfSnapshotPlan{}, err
	}
	return resp.Plan(), nil
}

func (c gatewaySelfLifecycle) Snapshot(ctx context.Context, box *host.Sandbox, a ctlops.SnapshotToTagArgs) error {
	_, err := c.fleet.SelfSnapshot(ctx, c.node,
		nodelink.SelfSnapshotReq{Sandbox: box.Name, Tag: a.Tag, Name: a.Name})
	return err
}

// relaySelfLifecycle is the same three verbs on a NODE: the name of the sandbox
// and the operation's own arguments, and nothing else. The owner, the tags, the
// bindings and every refusal are the gateway's to decide from its ledger.
type relaySelfLifecycle struct{ up *nodelink.Uplink }

func (c relaySelfLifecycle) Pause(ctx context.Context, box *host.Sandbox) error {
	var resp nodelink.SelfPauseResp
	return c.up.Request(ctx, nodelink.TypeSelfPause, nodelink.SelfPauseReq{Sandbox: box.Name}, &resp)
}

func (c relaySelfLifecycle) PlanSnapshot(ctx context.Context, box *host.Sandbox, tag, name string) (ctlops.SelfSnapshotPlan, error) {
	var resp nodelink.SelfSnapshotPlanResp
	err := c.up.Request(ctx, nodelink.TypeSelfSnapshotPlan,
		nodelink.SelfSnapshotPlanReq{Sandbox: box.Name, Tag: tag, Name: name}, &resp)
	if err != nil {
		return ctlops.SelfSnapshotPlan{}, err
	}
	return resp.Plan(), nil
}

func (c relaySelfLifecycle) Snapshot(ctx context.Context, box *host.Sandbox, a ctlops.SnapshotToTagArgs) error {
	var resp nodelink.SelfSnapshotResp
	return c.up.Request(ctx, nodelink.TypeSelfSnapshot,
		nodelink.SelfSnapshotReq{Sandbox: box.Name, Tag: a.Tag, Name: a.Name}, &resp)
}

type gatewayIdentityClient interface {
	metadata.Identity
	Close() error
}

func newRelayIdentity(up *nodelink.Uplink, mode string, log *slog.Logger) *relayIdentity {
	return &relayIdentity{up: up, mode: mode, log: log}
}

func (r *relayIdentity) configureGRPC(ctx context.Context, stateDir, nodeName string) error {
	if r == nil || r.mode == "ssh" {
		return nil
	}
	address, err := nodepki.LoadGatewayGRPCAddr(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		r.setGRPC(nil)
		return nil
	}
	if err != nil {
		r.setGRPC(nil)
		return err
	}
	leaf, roots, err := nodepki.LoadNodeCertificate(stateDir, nodeName)
	if err != nil {
		r.setGRPC(nil)
		return err
	}
	gateway, err := nodepki.LoadGatewayIdentity(stateDir)
	if err != nil {
		r.setGRPC(nil)
		return err
	}
	tlsConfig, err := nodecert.ClientTLSConfig(leaf, roots, gateway, nil)
	if err != nil {
		r.setGRPC(nil)
		return err
	}
	client, err := grpcidentity.DialTLS(ctx, address, tlsConfig)
	if err != nil {
		r.setGRPC(nil)
		return err
	}
	r.setGRPC(client)
	r.log.Info("gateway identity gRPC configured", "addr", address)
	return nil
}

func (r *relayIdentity) setGRPC(client gatewayIdentityClient) {
	r.mu.Lock()
	old := r.grpc
	r.grpc = client
	r.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

func (r *relayIdentity) currentGRPC() gatewayIdentityClient {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.grpc
}

func (r *relayIdentity) Close() {
	if r != nil {
		r.setGRPC(nil)
	}
}

func (r *relayIdentity) Issue(ctx context.Context, box *host.Sandbox, aud string) (metadata.Token, error) {
	if client := r.currentGRPC(); client != nil {
		token, err := client.Issue(ctx, box, aud)
		if err == nil || !errors.Is(err, grpcidentity.ErrUnavailable) {
			return token, err
		}
		r.log.Debug("gateway identity gRPC unavailable; falling back to SSH", "err", err)
	}
	var resp nodelink.IdentityTokenResp
	req := nodelink.IdentityReq{Sandbox: box.Name, Aud: aud}
	if err := r.up.Request(ctx, nodelink.TypeIdentityToken, req, &resp); err != nil {
		return metadata.Token{}, relayError(err)
	}
	return metadata.Token{JWT: resp.Token, ExpiresAt: resp.ExpiresAt}, nil
}

func (r *relayIdentity) Describe(ctx context.Context, box *host.Sandbox) (metadata.Doc, error) {
	if client := r.currentGRPC(); client != nil {
		doc, err := client.Describe(ctx, box)
		if err == nil || !errors.Is(err, grpcidentity.ErrUnavailable) {
			return doc, err
		}
		r.log.Debug("gateway identity gRPC unavailable; falling back to SSH", "err", err)
	}
	var resp nodelink.IdentityDocResp
	req := nodelink.IdentityReq{Sandbox: box.Name}
	if err := r.up.Request(ctx, nodelink.TypeIdentityDoc, req, &resp); err != nil {
		return metadata.Doc{}, relayError(err)
	}
	return docFromRelay(resp), nil
}

// docFromRelay converts the SSH fallback's wire struct into what the metadata
// service hands a guest.
//
// A named function rather than a literal inline above so it can be tested
// exhaustively: this conversion fails SILENTLY. A field the wire carries and
// this does not copy produces a document that is well-formed, a guest that
// carries on, and a symptom somewhere else entirely — a dropped GitHubID, for
// instance, leaves the guest writing the legacy noreply address that github.com
// declines to attribute, with nothing anywhere reporting an error.
func docFromRelay(resp nodelink.IdentityDocResp) metadata.Doc {
	return metadata.Doc{
		Issuer: resp.Issuer, Subject: resp.Subject, Owner: resp.Owner, GitHub: resp.GitHub,
		GitHubID: resp.GitHubID, KeyFP: resp.KeyFP, Sandbox: resp.Sandbox,
		SandboxID: resp.SandboxID, Image: resp.Image, Box: resp.Box,
	}
}

// relayError turns what came back off the link into what metadata classifies
// on, so a guest is told 400, 503 or 500 for the right reason.
//
// The two that matter are the ones that are NOT faults of this machine. A wrong
// `?aud=` is the guest's own mistake and must stay a 400 however many hops it
// crossed. Everything transport-shaped — no link yet, a gateway mid-restart, a
// deadline — is ErrNoIssuer, which is a 503 the guest's timer retries out of;
// reporting those as 500 would be true and useless, since the repair is to wait.
//
// A refusal this machine should never have provoked (CodeNotYours) is left as a
// 500 on purpose: the guest cannot fix it and must not retry it into a loop, and
// the sentence an operator needs is already in the gateway's log.
func relayError(err error) error {
	if errors.Is(err, nodelink.ErrNoLink) || errors.Is(err, nodelink.ErrLinkClosed) ||
		errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", metadata.ErrNoIssuer, err)
	}
	var typed *ctlops.Error
	if errors.As(err, &typed) {
		switch typed.Code {
		case nodelink.CodeIdentityAudience:
			return fmt.Errorf("%w: %s", metadata.ErrAudience, typed.Msg)
		case nodelink.CodeNoIssuer:
			// A permanent configuration answer, not an outage. Reported as
			// unavailable anyway: it is out of this machine's hands either way,
			// and the retry costs one request every 45 minutes.
			return fmt.Errorf("%w: %s", metadata.ErrNoIssuer, typed.Msg)
		case nodelink.CodeNoRepos:
			// The gateway attaches no repositories at all. A 501, which is what
			// a guest on the gateway's own machine is told, so the answer does
			// not depend on which machine the sandbox landed on.
			return fmt.Errorf("%w: %s", metadata.ErrNotEnabled, typed.Msg)
		case nodelink.CodeNoSuchRepo:
			// The guest asked about a repository it has no attachment to. A 404
			// and not a refusal: git consults the credential helper about every
			// host it touches, and a miss is the ordinary answer.
			return fmt.Errorf("%w: %s", metadata.ErrNoSuchRepo, typed.Msg)
		case nodelink.CodeRepoDenied:
			// Attached, and still refused: an owner with no verified GitHub
			// link, an inactive account, an App that is not installed there. A
			// 403 carrying the sentence, because each of those has its own fix
			// and none of them is a retry.
			return fmt.Errorf("%w: %s", metadata.ErrRepoDenied, typed.Msg)
		case nodelink.CodeRepoUpstream:
			// github.com, not this fleet. 503, because waiting is the repair.
			return fmt.Errorf("%w: %s", metadata.ErrUpstream, typed.Msg)
		}
		return err
	}
	// An untyped error off a link is the link having died mid-request.
	return fmt.Errorf("%w: %w", metadata.ErrNoIssuer, err)
}

// nodeHello is what this machine tells a gateway about itself. Everything in it
// is a fact the gateway cannot observe from the outside and needs in order to
// decide whether a sandbox can be placed here at all.
func nodeHello(opts nodeOptions, mgr *host.Manager, net *netpush.Syncer) nodelink.Hello {
	var capabilities []string
	grpcAddr := ""
	if opts.grpcAddr != "" && opts.controlTransport != "ssh" {
		grpcAddr = opts.grpcAddr
		capabilities = append(capabilities, nodelink.CapabilityGRPCControlV1)
	}
	if opts.guestDataTransport != "ssh" {
		capabilities = append(capabilities, nodelink.CapabilityRoutedGuestV1)
	}
	return nodelink.Hello{
		Arch: opts.arch,
		// Whether this machine meters and filters at all. Stated rather than
		// left for the gateway to discover on its first refused push, so an
		// operator reading `ctl node ls` sees it before a rule is ever written.
		Sluice: net.Enabled(),
		// Release and Version are the same string on purpose: hack/stage-artifacts.sh
		// stamps the release tag straight into the binary, so this build's version
		// IS the release it shipped in. A hand-built binary says "dev" for both,
		// which is also the honest answer.
		Release: version, Version: version,
		Driver:       opts.driverName,
		Images:       imageNames(opts.imageDir, opts.templateDir),
		Archiving:    mgr.ArchivingEnabled(),
		Snapshots:    mgr.Snapshotter(),
		GuestSubnet:  opts.guestSubnet,
		GRPCAddr:     grpcAddr,
		Capabilities: capabilities,
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
// imageNames is what this machine tells the gateway it can boot, across every
// directory it reads templates from — the operator's base images and, where the
// two are split, the captures written beside them. A machine that omitted its
// captures would advertise itself as unable to boot disks it is holding.
//
// Deduplicated because the dirs may be the same one (they are on every
// single-machine host, where TemplateDir is empty).
func imageNames(dirs ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			name, ok := strings.CutSuffix(e.Name(), ".ext4")
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
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
