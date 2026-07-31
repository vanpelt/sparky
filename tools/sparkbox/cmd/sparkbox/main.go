// sparkbox is a single-host MVP of an exe.dev-style agentic sandbox service:
// on-demand sandbox VMs behind a smart SSH gateway with resume-on-connect.
//
//	sparkbox serve --driver mock  --state-dir ./state --users users.conf
//	sparkbox serve --driver firecracker --kernel vmlinux --image-dir ./images ...
//
// Then: ssh -p 2222 new@localhost (creates a sandbox), ssh -p 2222 <name>@localhost.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/api"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/console"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/dnsedge"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/domainmeta"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envsync"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/frontdoor"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/hivemindpresence"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/hostsetup"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netpush"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/netrules"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodecert"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodeenroll"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodelink"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodepki"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/objstore"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/placement"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/proxy"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/restapi"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/userconsole"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/xterm"
)

// version is the artifact release tag this binary was built from, stamped by
// hack/stage-artifacts.sh (-X main.version). A hand-built binary says "dev", so
// `sparkbox version` on a host answers "which release is this box running?".
var version = "dev"

func main() {
	const usage = "usage: sparkbox <serve|setup|doctor|fetch-secrets|version> [flags]"
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "setup":
		err = setup(os.Args[2:])
	case "doctor":
		err = doctor(os.Args[2:])
	case "fetch-secrets":
		err = fetchSecrets(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("sparkbox %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sparkbox:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var (
		driverName           = fs.String("driver", "mock", "vm driver: mock | firecracker")
		stateDir             = fs.String("state-dir", "./state", "directory for control state: sqlite stores, certificates, and sandbox metadata")
		vmStateDir           = fs.String("vm-state-dir", "", "directory for per-VM disks, sockets, and memory snapshots (default: --state-dir)")
		keyDir               = fs.String("key-dir", "", "directory holding fleet private key PEMs, including node-control CA/gateway keys (default: --state-dir); point at tmpfs on a fleet host fed by `sparkbox fetch-secrets`")
		requireKeys          = fs.Bool("require-keys", false, "fail if any required fleet private key is missing instead of generating one — set on fleet hosts, where a missing key means secret hydration failed and generating a fresh identity would lock the fleet out")
		usersPath            = fs.String("users", "", "users file: '<user> <authorized_keys line>' per line (required)")
		sshAddr              = fs.String("ssh-addr", ":2222", "SSH gateway listen address")
		sshAdvertise         = fs.Int("ssh-advertise-port", 0, "gateway port shown in user-facing instructions when it differs from the listen port (e.g. 22 when an edge DNAT forwards :22 to the gateway); 0 uses the listen port")
		sshAdvertiseHost     = fs.String("ssh-advertise-host", "", "gateway hostname shown in user-facing instructions when it differs from --proxy-domain (e.g. ssh.example.com); empty uses --proxy-domain")
		apiAddr              = fs.String("api-addr", "127.0.0.1:8080", "control API listen address (no auth — keep private)")
		metricsAddr          = fs.String("metrics-addr", "127.0.0.1:9090", "node mode: private Prometheus metrics listen address (empty to disable; gateways expose /metrics on --api-addr)")
		defaultImage         = fs.String("default-image", "universal", "rootfs template for new sandboxes")
		defaultLogin         = fs.String("default-login-user", "sparky", "guest account the gateway SSHes in as; must match the template's baked authorized_keys (our images declare it via the sparkbox.login-user label, published as ROOTFS_LOGIN_USER in the release manifest)")
		idleTimeout          = fs.Duration("idle-timeout", 30*time.Minute, "pause sandboxes idle longer than this")
		idleBalloon          = fs.Duration("idle-balloon", 2*time.Minute, "balloon a warm sandbox down to --mem-reserve-mb after this much idle, reclaiming its RAM while it keeps running (0 disables; needs --mem-reserve-mb)")
		activityCPU          = fs.Float64("activity-cpu-pct", 2, "treat a sandbox as active while it burns at least this % of one host core (0 disables); keeps an unattended build or agent from being reaped. Idle boxes measure ~0.4%")
		activityNetKB        = fs.Int64("activity-net-kb", 64, "treat a sandbox as active while it moves at least this many KiB per reaper tick in either direction (0 disables). Idle boxes measure ~3 KB/min, a working agent 400 KB+")
		maxPerOwner          = fs.Int("max-running-per-owner", 2, "max concurrently running sandboxes per owner (0 = unlimited); pause with `ssh ctl@host pause <name>`")
		memAdmitPct          = fs.Int("mem-admission-pct", 85, "refuse to start a sandbox if running sandboxes' RAM cost would exceed this % of host RAM (0 = disabled)")
		hostMemMB            = fs.Int64("host-mem-mb", 0, "host RAM in MB for admission control (0 = auto-detect from /proc/meminfo)")
		memReserve           = fs.Int64("mem-reserve-mb", 0, "per-VM working-set floor (MB) enabling live overcommit: admission counts this instead of the full memory ceiling, and idle VMs balloon down to it (0 = off; count the full ceiling, never balloon)")
		diskPool             = fs.Int64("disk-pool-mb-per-owner", 0, "cap an owner's pooled on-disk usage across all their sandboxes + archives (0 = unlimited); soft accounting enforced at create/restore")
		archiveRemote        = fs.String("archive-remote", "", "rclone remote name for sandbox archives (e.g. sparkbox-artifacts); empty disables archive/restore. Needs S3 WRITE creds in the host's rclone.conf")
		archiveBucket        = fs.String("archive-bucket", "", "bucket within --archive-remote for archives (required to enable archiving)")
		archivePrefix        = fs.String("archive-prefix", "archives", "object-key prefix archives are written under: <prefix>/<owner>/<name>.ext4.zst")
		checkpointDir        = fs.String("checkpoint-dir", "", "durable mounted directory for immutable manual checkpoints; empty disables checkpoint/restore")
		checkpointPrefix     = fs.String("checkpoint-prefix", "checkpoints", "object-key prefix beneath --checkpoint-dir")
		kernelPath           = fs.String("kernel", "", "firecracker: vmlinux path")
		imageDir             = fs.String("image-dir", "", "firecracker: directory of <image>.ext4 templates")
		guestSubnet          = fs.String("guest-subnet", guestnet.DefaultPrefix, "IPv4 prefix divided into per-sandbox /30s; fleet nodes must set an explicit unique prefix (a /20 provides 1,024 slots)")
		subnet6              = fs.String("subnet6", "", "routable IPv6 /64 delegated to the host (e.g. 2001:db8:1c7::/64); gives each sandbox a no-NAT v6 address and a front-door address for hostname SSH routing")
		guestDNS             = fs.String("guest-dns", "", "resolver to hand guests via the sparkbox_dns kernel arg; \"gateway\" points each guest at its own gateway (172.30.<idx>.1), where the sluice allowlist resolver listens. Empty leaves guests on public DNS")
		sluiceSocket         = fs.String("sluice-socket", "", "path to the sluice control socket (e.g. /run/sluice.sock); enables per-tag egress rules + per-VM bandwidth in the user console. Empty disables both")
		proxyAddr            = fs.String("proxy-addr", ":8081", "HTTP proxy edge listen address for <sub>.<domain> (empty to disable)")
		proxyAdvertise       = fs.Int("proxy-advertise-port", 0, "public HTTP(S) port used for browser origins when it differs from --proxy-addr (e.g. 443 when a load balancer forwards to :8081); 0 uses the listen port")
		proxyDomain          = fs.String("proxy-domain", "hivemind.tools", "base domain for sandbox web routes")
		edgeV4               = fs.String("edge-v4", "", "public IPv4 of the proxy edge; when set, each sandbox also gets an A record here so <name>.<domain> resolves over IPv4 (the per-name front-door AAAA otherwise shadows the wildcard A). Point it at the same address the wildcard *.<domain> A does")
		proxyTLS             = fs.Bool("proxy-tls", false, "terminate TLS for the proxy edge (see --tls-provider)")
		consolePass          = fs.String("console-password", "", "password for the operator console at <console-subdomain>.<domain> (empty disables it)")
		consoleSub           = fs.String("console-subdomain", "console", "subdomain that serves the operator console")
		loginSub             = fs.String("login-subdomain", "login", "subdomain serving the browser sign-in for private (authenticated) routes")
		userConsoleSub       = fs.String("user-console-subdomain", "my", "subdomain serving the per-user console (empty disables it)")
		apiSub               = fs.String("api-subdomain", "api", "subdomain serving the authenticated REST API and its OpenAPI docs at <api-subdomain>.<domain>/docs (empty disables it); authenticate with a token from 'ssh ctl@<domain> session-token'")
		xtermSub             = fs.String("xterm-subdomain", xterm.DefaultSubdomain, "name suffix serving browser terminals at <name>-<xterm-subdomain>.<domain> (empty disables them); it is one label on purpose, so the zone's existing *.<domain> wildcard covers it in both DNS and TLS with nothing further to publish. Sandbox and route names ending in -<xterm-subdomain> are refused, since the edge answers them here")
		sessionTTL           = fs.Duration("session-ttl", 12*time.Hour, "lifetime of a browser session cookie minted for private routes")
		tlsProvider          = fs.String("tls-provider", "cloudflare", "TLS certs when --proxy-tls: cloudflare (DNS-01 wildcard, needs CLOUDFLARE_API_TOKEN) | autocert (per-host on-demand)")
		tlsEmail             = fs.String("tls-email", "", "ACME account email (recommended for cert-expiry notices)")
		oidcSub              = fs.String("oidc-subdomain", "oidc", "subdomain serving the OIDC discovery document and JWKS")
		oidcAud              = fs.String("oidc-audiences", defaultAudience, "comma-separated allowlist of `aud` values id tokens may be minted for (empty = any)")
		hivemindAPI          = fs.String("hivemind-api", "", "HiveMind API origin used to protect VMs with live agent sessions (empty disables)")
		hivemindAudience     = fs.String("hivemind-audience", defaultAudience, "OIDC audience used for HiveMind workload-token exchange")
		hivemindInterval     = fs.Duration("hivemind-presence-interval", time.Minute, "how often to refresh HiveMind session-presence leases")
		metaAddr             = fs.String("metadata-addr", fmt.Sprintf(":%d", metadata.DefaultPort), "guest metadata/token service listen address (reachable only from sandbox taps)")
		dnsAddr              = fs.String("dns-addr", "", "wildcard DNS responder listen address (e.g. <edge-ip>:53); serves *.<domain> -> the edge for a Tailscale split-DNS entry. Empty disables it")
		dnsAnswer            = fs.String("dns-answer", "", "comma-separated IPs the wildcard DNS answers with (default: the IP host of --proxy-addr)")
		openSignup           = fs.Bool("open-signup", false, "let anyone with an SSH key register at signup@ without an invite code")
		invitesPer           = fs.Int("invites-per-user", 0, "how many invite codes a non-operator user may mint (0 = operators only)")
		githubClientID       = fs.String("github-client-id", defaultGitHubClientID, "public client id of the GitHub app used to link accounts via the OAuth device flow (`ssh ctl@<domain> github link`). It is an identifier, not a secret. Empty disables the flow, leaving `keys verify-github` — which needs the user's key published on GitHub — as the only way to link")
		nodeNameFlag         = fs.String("node-name", "", "name this machine reports to the fleet and stamps on the sandboxes it holds (default: hostname). Also the `box` claim in every id token it issues, so changing it on a live host is externally visible")
		archFlag             = fs.String("arch", runtime.GOARCH, "CPU architecture this machine reports to the fleet")
		noNodeEnrol          = fs.Bool("no-node-enrol", false, "refuse unknown keys at the node@ door, so a machine cannot record itself as awaiting approval. Enrolment grants nothing on its own — an operator must approve its verified fingerprint and network configuration — so this is only worth setting on a gateway that will never gain another node")
		gatewayAddr          = fs.String("gateway", "", "run this machine as a fleet NODE linked to the gateway at host:port, instead of as a gateway itself. A provisioned node passes it through the unit's GATEWAY_FLAG line, which lands here as an ordinary flag")
		gatewayPub           = fs.String("gateway-pubkey", "", "node mode: the gateway's PUBLIC upstream authorized_keys line, or a path to a file holding it. Omitted, it is learned from the first welcome and cached under --state-dir")
		gatewayHostK         = fs.String("gateway-host-key", "", "node mode: pin the gateway's SSH host key (an authorized_keys line or a path to one). Empty trusts the first key offered, remembers it, and refuses any later change")
		nodeControlTransport = fs.String("node-control-transport", "auto", "fleet control transport: auto | ssh | grpc (auto prefers healthy mTLS gRPC and retains SSH fallback)")
		nodeControlRollout   = fs.String("node-control-rollout", "inherit", "gateway control cutover: inherit | shadow | read-only | idempotent | grpc")
		nodeGRPCAddr         = fs.String("node-grpc-addr", "", "node mode: tailnet listen address for the mTLS NodeControl server, such as 100.64.0.12:9443 (empty disables it)")
		gatewayGRPCAddr      = fs.String("gateway-grpc-addr", "", "gateway mode: tailnet host:port for the mTLS GatewayIdentity server; enrolled nodes learn this endpoint automatically (empty keeps SSH identity relay)")
		guestDataTransport   = fs.String("guest-data-transport", "auto", "remote guest data transport: auto | ssh | routed (auto falls back to the SSH data pool on route failure)")
		routedGuestCanary    = fs.Int("routed-guest-canary-percent", 100, "gateway share of sandboxes using routed data in auto mode: 0..100")
		clusterID            = fs.String("cluster-id", "", "gateway mTLS identity name (default: --node-name/hostname; persist this across gateway moves)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	*vmStateDir = effectiveVMStateDir(*vmStateDir, *stateDir)
	guestSubnetSet := false
	transportFlagsGiven := map[string]bool{}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "guest-subnet" {
			guestSubnetSet = true
		}
		transportFlagsGiven[f.Name] = true
	})
	guestNetwork, err := guestnet.Parse(*guestSubnet)
	if err != nil {
		return err
	}
	*guestSubnet = guestNetwork.String()
	if err := validateTransportFlag("--node-control-transport", *nodeControlTransport, "auto", "ssh", "grpc"); err != nil {
		return err
	}
	if err := validateTransportFlag("--guest-data-transport", *guestDataTransport, "auto", "ssh", "routed"); err != nil {
		return err
	}
	// Runtime and setup must accept exactly the same transport combinations.
	// Keeping normalization in hostsetup also makes direct `sparkbox serve`
	// invocations fail closed on role-only flags instead of silently ignoring
	// (for example) a node listener configured on a gateway.
	transport, err := hostsetup.NormalizeTransportConfig(hostsetup.Config{
		Gateway:                  *gatewayAddr,
		NodeControlTransport:     *nodeControlTransport,
		NodeControlRollout:       *nodeControlRollout,
		NodeGRPCAddr:             *nodeGRPCAddr,
		GatewayGRPCAddr:          *gatewayGRPCAddr,
		GuestDataTransport:       *guestDataTransport,
		RoutedGuestCanaryPercent: *routedGuestCanary,
		ClusterID:                *clusterID,
		FlagsGiven:               transportFlagsGiven,
	})
	if err != nil {
		return err
	}
	*nodeControlTransport = transport.NodeControlTransport
	*nodeControlRollout = transport.NodeControlRollout
	*nodeGRPCAddr = transport.NodeGRPCAddr
	*gatewayGRPCAddr = transport.GatewayGRPCAddr
	*guestDataTransport = transport.GuestDataTransport
	*routedGuestCanary = transport.RoutedGuestCanaryPercent
	*clusterID = transport.ClusterID
	controlRollout, shadowInventory, err := controlRolloutStageConfig(*nodeControlRollout)
	if err != nil {
		return err
	}
	if *gatewayAddr != "" && !guestSubnetSet {
		return errors.New("fleet nodes require an explicit unique --guest-subnet (use a non-overlapping /20 for 1,024 sandbox slots)")
	}
	// A node has no accounts of its own — every identity in a fleet lives on the
	// gateway — so the seed file is required of everything except a node.
	if *usersPath == "" && *gatewayAddr == "" {
		return errors.New("--users is required")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	metricsRegistry := fleetmetrics.New()

	// Node mode forks here, before a single store is opened. Nothing below this
	// line runs on a node: the users, secrets, routes, schedules, netrules,
	// placement and roster stores, the OIDC issuer, the SSH gateway, the HTTP
	// edge and both consoles are all fleet-wide surfaces, and a fleet has exactly
	// one of each. The surest way for a node never to hold one is never to reach
	// the line that opens it.
	if *gatewayAddr != "" {
		// Said out loud, because "this host serves nothing" is otherwise
		// indistinguishable from a crash to whoever is looking at it.
		log.Info("running as a fleet node, not a gateway", "gateway", *gatewayAddr)
		return serveNode(nodeOptions{
			gateway: *gatewayAddr, gatewayPub: *gatewayPub, gatewayHostKey: *gatewayHostK,
			nodeName: nodeNameOr(*nodeNameFlag), arch: *archFlag,
			driverName: *driverName, stateDir: *stateDir, vmStateDir: *vmStateDir, keyDir: *keyDir,
			kernelPath: *kernelPath, imageDir: *imageDir,
			defaultLogin: *defaultLogin, guestSubnet: *guestSubnet, subnet6: *subnet6, guestDNS: *guestDNS,
			sluiceSocket: *sluiceSocket, metaAddr: *metaAddr,
			idleBalloon: *idleBalloon, idleTimeout: *idleTimeout,
			activityCPU: *activityCPU, activityNetKB: *activityNetKB,
			maxPerOwner: *maxPerOwner, memAdmitPct: *memAdmitPct, hostMemMB: *hostMemMB,
			memReserve: *memReserve, diskPool: *diskPool,
			controlTransport: *nodeControlTransport, grpcAddr: *nodeGRPCAddr,
			guestDataTransport: *guestDataTransport,
			hivemindAPI:        *hivemindAPI, hivemindAudience: *hivemindAudience,
			hivemindInterval: *hivemindInterval,
			metricsAddr:      *metricsAddr, metrics: metricsRegistry, log: log,
		})
	}

	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(*vmStateDir, 0o700); err != nil {
		return err
	}
	// Fleet private keys live in --key-dir (default --state-dir). On a fleet
	// host that's tmpfs, hydrated from Secret Manager by `sparkbox fetch-secrets`
	// before this starts, and --require-keys turns a missing file into a hard
	// failure rather than a silently-minted new fleet identity.
	keysIn := *keyDir
	if keysIn == "" {
		keysIn = *stateDir
	}
	loadSSH := sshgw.LoadOrCreateKey
	loadOIDC := oidc.LoadOrCreateKey
	if *requireKeys {
		loadSSH = sshgw.LoadKey
		loadOIDC = oidc.LoadKey
	}
	hostKey, err := loadSSH(keysIn, "gateway_host_key")
	if err != nil {
		return fmt.Errorf("host key: %w", err)
	}
	upstreamKey, err := loadSSH(keysIn, "gateway_upstream_key")
	if err != nil {
		return fmt.Errorf("upstream key: %w", err)
	}
	// The identity store lives in the same sqlite file as the proxy routes.
	// users.conf stays the bootstrap seed: it is how a freshly provisioned host
	// knows its first (operator) user before anyone can run `ssh signup@`.
	userStore, err := users.Open(filepath.Join(*stateDir, "sparkbox.db"))
	if err != nil {
		return fmt.Errorf("user store: %w", err)
	}
	defer userStore.Close()
	if err := users.SeedFile(*usersPath, userStore, log); err != nil {
		return fmt.Errorf("users: %w", err)
	}

	// ES256 signing key for the OIDC issuer. It cannot be the ed25519 gateway
	// key: verifiers (hivemind among them) allowlist RS256/ES256 only.
	oidcKey, err := loadOIDC(keysIn, "oidc_signing_key")
	if err != nil {
		return fmt.Errorf("oidc signing key: %w", err)
	}
	prevKey, err := oidc.LoadKeyIfPresent(keysIn, "oidc_signing_key_prev")
	if err != nil {
		return fmt.Errorf("previous oidc signing key: %w", err)
	}
	issuer, err := oidc.New(oidc.Options{
		IssuerURL: "https://" + *oidcSub + "." + *proxyDomain,
		Signer:    oidcKey, Previous: prevKey,
		Audiences: splitList(*oidcAud),
	})
	if err != nil {
		return fmt.Errorf("oidc issuer: %w", err)
	}

	// The edge session signer is keyed off the OIDC signing material (HKDF), so
	// authenticated forwarding adds no new fleet secret. It signs the browser/API
	// tokens `ctl@ session-token` mints and the proxy edge verifies.
	sessionSigner := edgeauth.NewSigner(oidcKey.D.Bytes())

	// The secrets KEK is derived from the same OIDC signing material (own HKDF
	// info string), so encrypted-at-rest secrets add no new fleet secret; on a
	// fleet host the ikm lives in tmpfs, so a stolen sparkbox.db alone is
	// unreadable. Rotating the OIDC key orphans stored values — the store's
	// keycheck sentinel detects that loudly and disables secret ops while tag
	// bookkeeping keeps working (see internal/secrets).
	secretsStore, err := secrets.Open(filepath.Join(*stateDir, "sparkbox.db"),
		secrets.DeriveKEK(oidcKey.D.Bytes()), log)
	if err != nil {
		return fmt.Errorf("secrets store: %w", err)
	}
	defer secretsStore.Close()

	// Network rule-sets (per-tag egress allowlists) live in the same DB, on their
	// own connection. Independent of sluice: rules can be authored even when the
	// data plane is down; they take effect once pushed.
	netrulesStore, err := netrules.Open(filepath.Join(*stateDir, "sparkbox.db"), log)
	if err != nil {
		return fmt.Errorf("netrules store: %w", err)
	}
	defer netrulesStore.Close()

	var driver vmm.Driver
	switch *driverName {
	case "mock":
		md := mock.New(*vmStateDir, hostKey)
		md.LoginUser = *defaultLogin
		driver = md
	case "firecracker":
		driver, err = newFirecrackerDriver(
			*kernelPath, *imageDir, *vmStateDir, *guestSubnet, *subnet6, *defaultLogin, *guestDNS,
		)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown driver %q", *driverName)
	}
	defer driver.Close()

	routeStore, err := routes.Open(filepath.Join(*stateDir, "sparkbox.db"))
	if err != nil {
		return fmt.Errorf("route store: %w", err)
	}
	defer routeStore.Close()

	scheduleStore, err := schedule.Open(filepath.Join(*stateDir, "schedules.db"))
	if err != nil {
		return fmt.Errorf("schedule store: %w", err)
	}
	defer scheduleStore.Close()

	hostMem := *hostMemMB
	if hostMem == 0 {
		hostMem = detectHostMemMB()
	}
	nodeName := nodeNameOr(*nodeNameFlag)
	if *clusterID == "" {
		*clusterID = nodeName
	}

	// The placement ledger: which machine holds which sandbox name. It is a
	// sixth writer on the same sqlite file, and on a single-box deployment its
	// only job is being the name allocator — the local manager stays the truth
	// for everything it holds.
	placeStore, err := placement.Open(filepath.Join(*stateDir, "sparkbox.db"))
	if err != nil {
		return fmt.Errorf("placement store: %w", err)
	}
	defer placeStore.Close()

	// The node roster: which machines may link to this gateway. It is separate
	// from the users store on purpose — a node is a machine, not an account —
	// and it is opened unconditionally because the `node@` door is what a new
	// machine knocks on, and a gateway with no roster could not even tell it to
	// come back once an operator has approved it.
	nodeStore, err := nodes.Open(filepath.Join(*stateDir, "sparkbox.db"))
	if err != nil {
		return fmt.Errorf("node roster: %w", err)
	}
	defer nodeStore.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	controlMode := fleet.ControlTransport(*nodeControlTransport)
	var (
		nodeAuthority  *nodepki.Authority
		nodeIssuer     *nodeenroll.Issuer
		gatewayLeaf    *gatewayCertificateSource
		gatewayControl *gatewayNodeControl
	)
	if controlMode != fleet.ControlTransportSSH {
		nodeAuthority, err = nodepki.LoadOrCreateAuthorityFrom(
			*stateDir, keysIn, *clusterID, *requireKeys,
		)
		if err != nil {
			return fmt.Errorf("node control certificate authority: %w", err)
		}
		leaf, err := nodeAuthority.GatewayCertificateFrom(
			*stateDir, keysIn, *clusterID, nodecert.DefaultTTL, *requireKeys,
		)
		if err != nil {
			return fmt.Errorf("gateway node control certificate: %w", err)
		}
		gatewayLeaf = newGatewayCertificateSource(leaf)
		nodeIssuer, err = nodeenroll.New(
			nodeAuthority.CA, nodeStore, *clusterID, nodecert.DefaultTTL,
		)
		if err != nil {
			return fmt.Errorf("node certificate issuer: %w", err)
		}
		go gatewayLeaf.run(
			ctx, nodeAuthority, *stateDir, keysIn, *clusterID, *requireKeys, log,
		)
	}

	// --edge-v4 feeds the per-name front-door A records below. Parsed here, once,
	// so a malformed value is reported once too.
	var edgeAddrs []netip.Addr
	if *edgeV4 != "" {
		if a, perr := netip.ParseAddr(*edgeV4); perr == nil && a.Is4() {
			edgeAddrs = append(edgeAddrs, a)
		} else {
			log.Warn("ignoring --edge-v4: not an IPv4 address", "value", *edgeV4)
		}
	}

	// Front doors: with a delegated IPv6 prefix, every sandbox name maps to a
	// deterministic public address, so `ssh <name>.<domain>` can route by the
	// dialed address instead of the SSH username. Username routing keeps
	// working regardless (v4 clients, no per-name DNS yet).
	var doors *frontdoor.Mapper
	var plumber *frontdoor.Plumber
	var doorHooks frontdoor.Multi
	if *subnet6 != "" {
		if doors, err = frontdoor.New(*subnet6); err != nil {
			// A prefix narrower than /64 still works for guest addressing, so
			// don't fail the server — just run without hostname SSH routing.
			log.Warn("front doors disabled", "err", err)
			doors = nil
		} else {
			plumber = frontdoor.NewPlumber(doors, log)
			doorHooks = frontdoor.Multi{plumber}
			// With a Cloudflare token, each sandbox also gets an AAAA record so
			// `ssh <name>.<domain>` resolves to its front door. Same token scope
			// as the DNS-01 TLS provider (Zone.DNS:Edit).
			if token := os.Getenv("CLOUDFLARE_API_TOKEN"); token != "" {
				// Publish an A at the shared edge alongside the per-name AAAA, or
				// the AAAA shadows the wildcard A and the name dies over IPv4.
				var edge netip.Addr
				if len(edgeAddrs) > 0 {
					edge = edgeAddrs[0]
				} else {
					log.Warn("front-door names will not resolve over IPv4", "reason", "no --edge-v4 (per-name AAAA shadows the wildcard A)")
				}
				doorHooks = append(doorHooks, frontdoor.NewPublisher(doors, *proxyDomain, token, edge, log))
				log.Info("front-door DNS publishing enabled", "zone", *proxyDomain, "edge_v4", *edgeV4)
			} else {
				log.Info("front-door DNS publishing disabled", "reason", "no CLOUDFLARE_API_TOKEN")
			}
		}
	}

	mgrOpts := host.Options{
		Context: ctx, StateDir: *stateDir, Driver: driver,
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
		Routes:               routeStore,
		Schedules:            scheduleStore,
		Tags:                 secretsStore,
		MaxRunningPerOwner:   *maxPerOwner,
		MemAdmissionPct:      *memAdmitPct,
		HostMemMB:            hostMem,
		MemReserveMB:         *memReserve,
		DiskPoolMBPerOwner:   *diskPool,
		ActivityCPUPct:       *activityCPU,
		ActivityNetBytes:     uint64(*activityNetKB) * 1024,
		ArchivePrefix:        *archivePrefix,
		CheckpointPrefix:     *checkpointPrefix,
		CheckpointStagingDir: *vmStateDir,
		NodeName:             nodeName,
		Arch:                 *archFlag,
		Release:              version,
		HostVCPUs:            int64(runtime.NumCPU()),
		Metrics:              metricsRegistry,
	}
	if doorHooks != nil {
		mgrOpts.FrontDoor = doorHooks
	}
	// Archiving is opt-in: only wire the object store when a remote+bucket are
	// configured. Assign through the local (non-nil) check so a nil *Store never
	// becomes a non-nil ObjectStore interface (the classic typed-nil trap).
	if arch := objstore.New(*archiveRemote, *archiveBucket); arch != nil {
		mgrOpts.Archive = arch
		log.Info("sandbox archiving enabled", "remote", *archiveRemote, "bucket", *archiveBucket, "prefix", *archivePrefix)
	}
	if *checkpointDir != "" {
		checkpoints, err := objstore.NewFilesystem(*checkpointDir)
		if err != nil {
			return fmt.Errorf("checkpoint store: %w", err)
		}
		mgrOpts.Checkpoint = checkpoints
		log.Info("sandbox checkpointing enabled", "dir", *checkpointDir, "prefix", *checkpointPrefix)
	}
	mgr, err := host.NewManager(mgrOpts)
	if err != nil {
		return err
	}
	defer func() {
		if err := mgr.FlushActivity(); err != nil {
			log.Warn("final activity flush failed", "err", err)
		}
	}()

	// The fleet router stands between every control-plane surface and the
	// machine that actually holds a sandbox. With no other machine linked it is
	// this manager with a ledger bolted on: the local answer is authoritative
	// for everything local, so a single-box deployment behaves exactly as it did
	// before this existed.
	//
	// It is given the SAME four name-keyed side stores the manager was given
	// (mgrOpts above), and it must be: they live here, a node has none of them,
	// and they are keyed by a sandbox's name. For a local placement the manager
	// still does all of that work itself; for one on another machine the fleet is
	// the only thing that can. See internal/fleet/sidestores.go.
	// Egress policy + per-VM bandwidth bridge to sluice. The client is nil unless
	// --sluice-socket is set, which makes the syncer a no-op (rules can still be
	// authored, just not enforced).
	//
	// Built BEFORE the fleet because the fleet holds it: egress is enforced and
	// metered per machine, so the gateway's own sluice is the gateway's node
	// adapter's business exactly as a node's own is that node's. The rules it
	// resolves against are the whole fleet's, and go in separately below.
	var sluiceClient *netpush.Client
	if *sluiceSocket != "" {
		sluiceClient = netpush.NewClient(*sluiceSocket)
		log.Info("sluice egress policy enabled", "socket", *sluiceSocket)
	}
	netSyncer, err := netpush.NewSyncerForSubnet(sluiceClient, netpushFleet{mgr}, netrulesStore, *guestSubnet, log)
	if err != nil {
		return fmt.Errorf("guest subnet for network accounting: %w", err)
	}

	fleetOpts := fleet.Options{
		Context: ctx, Local: mgr, LocalName: nodeName, LocalArch: *archFlag,
		Index: placeStore, Log: log, Metrics: metricsRegistry,
		Routes: routeStore, Schedules: scheduleStore, Tags: secretsStore,
		LocalNet: netSyncer,
	}
	if nodeIssuer != nil {
		fleetOpts.OnCertificateEnroll = func(
			enrollCtx context.Context,
			node string,
			request nodelink.CertificateEnrollRequest,
		) (nodelink.CertificateEnrollResponse, error) {
			if gatewayControl == nil {
				return nodeIssuer.Issue(enrollCtx, node, request)
			}
			return gatewayControl.Enroll(enrollCtx, node, request)
		}
	}
	if doorHooks != nil {
		fleetOpts.FrontDoor = doorHooks
	}
	flt, err := fleet.New(fleetOpts)
	if err != nil {
		return fmt.Errorf("fleet: %w", err)
	}
	defer flt.Close()
	nodeStore.SetRevocationHook(func(name, reason string) {
		flt.EvictNode(name, reason)
	})
	if nodeIssuer != nil {
		gatewayControl = newGatewayNodeControl(
			ctx, flt, nodeStore, nodeIssuer, nodeAuthority, gatewayLeaf,
			controlMode, fleet.GuestTransport(*guestDataTransport),
			controlRollout, shadowInventory, *routedGuestCanary,
			*gatewayGRPCAddr,
			metricsRegistry, log,
		)
		if err := gatewayControl.StartExisting(); err != nil {
			return fmt.Errorf("restore node gRPC controls: %w", err)
		}
	}

	// Secret-env propagation: the syncer rewrites a sandbox's managed
	// /etc/environment block over SSH when it reaches running (create, resume,
	// restore, fork) and when the console changes a tag or secret. Installed
	// post-construction so the manager never depends on it at build time.
	//
	// The lister is the FLEET, not the manager. SyncOwner's fan-out — what runs
	// when the user console changes a tag or a secret — walks whatever it is
	// given, so a manager here would quietly restrict every secret change to
	// the sandboxes on this machine while reporting success for all of them.
	// The dialer is what then lets a delivery reach a guest on another machine;
	// the two together are the whole of it, because a node has no secrets store
	// and its own push hook is nil (see internal/fleet/envsync.go).
	syncer := envsync.New(secretsStore, flt, upstreamKey, log)
	// Environment delivery is explicitly never a wake-up source. A delayed
	// push can race a pause; the request-facing dialer would interpret the
	// resulting typed not-running refusal as stale cache and resume the box.
	// This variant preserves the delivery contract: skip it now and reconcile
	// on the next real transition to running.
	syncer.SetDialer(flt.DialContextNoResume)
	mgr.SetEnvSync(syncer)
	// The same syncer on both sides of the split: the manager fires it for a
	// sandbox on this machine, the fleet for one on any other. There is exactly
	// one channel into a guest's /etc/environment, and two would race over it.
	flt.SetEnvPusher(syncer)

	// The rules half of the egress plane. It goes on the FLEET rather than only
	// into the local syncer because a tag binding is a gateway-wide fact and the
	// sandbox it governs may be on any machine: the fleet resolves each one
	// against the ledger's owner column and hands every machine its own share.
	// Favicons are cached under the state dir.
	flt.SetRules(netrulesStore)
	faviconCache := domainmeta.NewFaviconCache(filepath.Join(*stateDir, "favicons"), nil)

	log.Info("resource limits", "max_running_per_owner", *maxPerOwner,
		"mem_admission_pct", *memAdmitPct, "host_mem_mb", hostMem,
		"mem_reserve_mb", *memReserve, "overcommit", *memReserve > 0)

	// Browser terminals only exist behind the HTTPS edge, so with no proxy there
	// is no terminal subdomain to advertise. Threading the empty label through
	// keeps `ctl whoami`, the REST capabilities and every sandbox's terminal_url
	// honest rather than pointing at a host that answers nothing.
	xtermLabel := *xtermSub
	if *proxyAddr == "" {
		xtermLabel = ""
	}

	// One control plane, shared by all three transports. Built here rather than
	// left to sshgw's nil-Ops fallback because a second Ops would mean a second
	// job registry and a second reaper goroutine: a job started over REST would
	// then be invisible to the SSH channel, and only the Ops that owns a reaper
	// can stop it.
	ops := newGatewayOps(gatewayStores{
		Fleet: flt, Placement: placeStore, Roster: nodeStore,
		Checkpoints: localCheckpointOps{mgr: mgr},
		Users:       userStore, Secrets: secretsStore,
		Schedules: scheduleStore, Routes: routeStore, Sessions: sessionSigner,
		DefaultImage: *defaultImage, Domain: *proxyDomain,
		GatewayGuestSubnet: *guestSubnet,
		XtermSubdomain:     xtermLabel, InvitesPerUser: *invitesPer,
		GitHubClientID: *githubClientID,
		Log:            log,
	})
	defer ops.Close()

	gw := sshgw.New(sshgw.GatewayOptions{
		Manager: mgr, Fleet: flt, Dial: flt.DialContext,
		Users: userStore, HostKey: hostKey, UpstreamKey: upstreamKey,
		DefaultImage: *defaultImage, Logger: log,
		Doors: doors, Domain: *proxyDomain, SSHHost: advertisedHost(*sshAdvertiseHost, *proxyDomain),
		OpenSignup: *openSignup, InvitesPerUser: *invitesPer,
		Schedules: scheduleStore,
		Routes:    routeStore, Session: sessionSigner, Tags: secretsStore,
		Ops: ops, XtermSubdomain: xtermLabel,
		Nodes: nodeStore, NodeJoiner: flt, NodeEnrol: !*noNodeEnrol,
	})
	// The gateway knows which terminals are attached to which sandbox, so it is
	// what the manager calls to release them when a sandbox is paused. The
	// fleet gets the SAME registry, not one of its own: a sandbox on another
	// machine has its sessions here like any other, and two registries would
	// mean a pause hanging up only the half of them that happened to land in
	// the one it could see.
	mgr.SetSessions(gw)
	flt.SetSessions(gw)

	warnDoorNameCollision(sshgw.NodeUser, mgr, log)

	// Claim the front-door range (AnyIP), then run every hook (NDP plumbing +
	// DNS records) for the reserved names and each existing sandbox. This is
	// the reconcile pass: new sandboxes are handled on create, and anything
	// that failed or drifted since the last run is repaired here.
	//
	// It walks the FLEET rather than the local manager, and has to: a front
	// door is a name plumbed on THIS host — an address in this host's range and
	// a DNS record pointing at it — and the gateway mints one for a sandbox
	// built on another machine too (fleet.mint). Reconciling only the local
	// manager's names would quietly drop every remote sandbox's front door at
	// the next deploy, and `ssh <name>.<domain>` would stop resolving for its
	// owner with nothing in the logs about it. Nothing here touches a VM: the
	// hooks take a name and plumb this host, so a row for a machine that is not
	// answering yet costs an idempotent repair and no more.
	if plumber != nil {
		plumber.EnsureRange(ctx)
		for _, r := range sshgw.ReservedUsers {
			doorHooks.Ensure(ctx, r)
		}
		for _, b := range flt.List() {
			doorHooks.Ensure(ctx, b.Name)
		}
		log.Info("front doors enabled", "range", doors.Range(),
			"new", doors.Addr(sshgw.NewSandboxUser).String())
	}

	// A process restart marks every sandbox paused; bring the pinned ones back
	// up so their in-guest daemons keep running across a host reboot.
	//
	// The manager, deliberately, and NOT the fleet. Both of the boot-time
	// reconciliations below are true of the machine this process is on and
	// false of every other one: this process dying took its own VMs with it, so
	// "everything is paused now" is an observation here and an invention about
	// a node — whose VMs kept running throughout, and whose own sparkbox is the
	// one that resumes its pinned ones and reaps its idle ones. A gateway that
	// ran either of these fleet-wide would stop somebody's work with a deploy
	// they were told was invisible to them. See internal/fleet/reconcile.go's
	// second prohibition, which is the same rule at ingest time.
	mgr.ResumePinned(ctx)

	if *hivemindAPI != "" {
		monitor, err := hivemindpresence.New(hivemindpresence.Options{
			APIBase: *hivemindAPI, Audience: *hivemindAudience,
			Sandboxes: mgr, Protector: mgr,
			Observer: mgr,
			Identity: metadata.Local{Issuer: issuer, Users: userStore, NodeName: nodeName},
			Logger:   log, UserAgent: "sparkbox/" + version,
		})
		if err != nil {
			return err
		}
		go monitor.Run(ctx, *hivemindInterval)
		log.Info("HiveMind session presence enabled",
			"api", *hivemindAPI, "interval", *hivemindInterval)
	}
	go mgr.RunReaper(ctx, *idleBalloon, *idleTimeout, time.Minute)
	// Reconcile egress policy periodically so VM churn (create, resume, destroy)
	// that bypasses the console's change-time push still converges. The console
	// also pushes on every rule/tag mutation for immediacy.
	//
	// Driven off the FLEET, not the local syncer, and unconditionally: a gateway
	// with no sluice of its own may still have nodes that have one, and gating
	// this on the local socket would leave every one of them permanently
	// unpoliced. PushNet refuses per machine and says nothing about the ones
	// that meter nothing.
	go pushLoop(ctx, flt, log)

	// The platform scheduler wakes sandboxes to run due cron jobs (the honest
	// answer to background work in a scale-to-zero world). It ticks every 30s so
	// minute-granularity crons fire promptly; the gateway is its exec runner.
	go schedule.NewScheduler(scheduleStore, gw, log).Run(ctx, 30*time.Second)

	// The legacy loopback API goes through the fleet like every other surface.
	// Not to reach other machines — it is unauthenticated and stays bound to
	// 127.0.0.1 — but because its create and destroy allocate and release
	// sandbox names, and the placement ledger is where names are allocated. A
	// create that skipped it would take a name nothing recorded; a destroy that
	// skipped it would reserve one forever.
	apiSrv := &http.Server{
		Addr: *apiAddr,
		Handler: privateAPIHandler(
			api.New(flt, routeStore, *defaultImage, log).Handler(),
			metricsRegistry.Handler(),
		),
	}
	sshSrv := gw.Server(*sshAddr)

	errCh := make(chan error, 6)

	// Guest metadata service: hands each sandbox an id token over its own tap.
	// It binds every interface because taps come and go, and identifies the
	// caller by source address — see internal/metadata for why that's the safe
	// end of the connection to trust.
	//
	// The same signing path is handed to the FLEET, which is what lets a node
	// run this service too: its guests reach their own tap here exactly as these
	// do, and the mint it cannot perform itself is relayed up and answered by
	// fleetIdentity below. Installed whether or not this gateway serves its own
	// metadata endpoint, because the two are independent — a gateway holding no
	// VMs of its own still signs for the ones on its nodes.
	flt.SetIdentity(fleetIdentity{issuer: issuer, users: userStore,
		defAud: firstOr(splitList(*oidcAud), defaultAudience)})
	if *gatewayGRPCAddr != "" {
		identityServer, identityListener, err := newGatewayIdentityServer(
			ctx, *gatewayGRPCAddr, flt, nodeStore,
			nodeAuthority, gatewayLeaf,
		)
		if err != nil {
			return fmt.Errorf("gateway identity gRPC: %w", err)
		}
		defer identityListener.Close() //nolint:errcheck
		go func() { errCh <- identityServer.Serve(identityListener) }()
		log.Info("mTLS gateway identity enabled", "addr", *gatewayGRPCAddr)
	}
	// Do not admit an enrolling node until the advertised identity endpoint has
	// bound successfully. Older nodes ignore it; new nodes may dial immediately
	// after the certificate response arrives.
	go func() { errCh <- apiSrv.ListenAndServe() }()
	go func() { errCh <- sshSrv.ListenAndServe() }()
	if *metaAddr != "" {
		meta, err := metadata.NewChecked(metadata.Options{
			Manager: mgr, Logger: log,
			Identity:        metadata.Local{Issuer: issuer, Users: userStore, NodeName: nodeName},
			DefaultAudience: firstOr(splitList(*oidcAud), defaultAudience),
			GuestSubnet:     *guestSubnet,
		})
		if err != nil {
			return fmt.Errorf("guest metadata subnet: %w", err)
		}
		go func() { errCh <- meta.ListenAndServe(ctx, *metaAddr) }()
		log.Info("guest metadata service enabled", "addr", *metaAddr, "issuer", issuer.URL())
	}

	// Wildcard DNS responder for the tailnet edge: answers *.<domain> with the
	// edge IP so a Tailscale split-DNS entry resolves every sandbox name to this
	// box, no per-name DNS. Answers default to the concrete IP in --proxy-addr.
	if *dnsAddr != "" {
		var answers []netip.Addr
		for _, s := range splitList(*dnsAnswer) {
			a, perr := netip.ParseAddr(s)
			if perr != nil {
				return fmt.Errorf("--dns-answer %q: %w", s, perr)
			}
			answers = append(answers, a)
		}
		if len(answers) == 0 {
			if host, _, herr := net.SplitHostPort(*proxyAddr); herr == nil {
				if a, perr := netip.ParseAddr(host); perr == nil {
					answers = append(answers, a)
				}
			}
		}
		if len(answers) == 0 {
			return errors.New("--dns-addr set but no answer IP: pass --dns-answer or use a concrete IP host in --proxy-addr")
		}
		dnsSrv := dnsedge.New(*proxyDomain, answers, log)
		go func() { errCh <- dnsSrv.ListenAndServe(ctx, *dnsAddr) }()
		log.Info("wildcard DNS responder enabled", "addr", *dnsAddr, "domain", *proxyDomain, "answers", answers)
	}

	var proxySrv *http.Server
	if *proxyAddr != "" {
		px := proxy.New(flt, routeStore, *proxyDomain, log)
		px.SetDialer(flt.DialContext)
		px.SetMetrics(metricsRegistry)
		// The issuer rides on the existing proxy edge: wildcard DNS already
		// resolves oidc.<domain> to this host and autocert already issues a cert
		// per SNI, so serving it is two GET handlers and no new listener.
		px.SetIssuer(*oidcSub, issuer.Handler())
		log.Info("oidc issuer enabled", "url", issuer.URL(), "audiences", *oidcAud)

		// Authenticated forwarding: private routes are gated behind a session the
		// visitor mints from their SSH key, and the browser sign-in rides the edge
		// at login.<domain> like the console and issuer do.
		loginH, lerr := edgeauth.NewLoginHandler(edgeauth.LoginConfig{
			Signer: sessionSigner, Domain: *proxyDomain, Secure: *proxyTLS,
			TTL: *sessionTTL, Logger: log, Gateway: advertisedHost(*sshAdvertiseHost, *proxyDomain),
			GatewayPort: advertisedPort(*sshAdvertise, *sshAddr),
			Passkeys:    userStore, Subdomain: *loginSub, Port: advertisedPort(*proxyAdvertise, *proxyAddr),
			HomeSub: *userConsoleSub,
		})
		if lerr != nil {
			return fmt.Errorf("login handler: %w", lerr)
		}
		px.SetAuth(*loginSub, loginH.Handler(), sessionSigner, userStore)
		px.SetListenPort(portOf(*proxyAddr))
		log.Info("authenticated forwarding enabled", "login", *loginSub+"."+*proxyDomain, "session_ttl", *sessionTTL)
		if !*proxyTLS {
			// The `iss` claim must be an https URL a verifier can actually
			// fetch: they follow it to the discovery document and JWKS over
			// public https, and refuse anything else. Fine for a local mock run,
			// fatal to federation on a real host — so say so plainly.
			log.Warn("oidc issuer advertises https but --proxy-tls is off; "+
				"relying parties will not be able to verify these tokens",
				"issuer", issuer.URL())
		}
		// The password is a secret, so prefer an env var (kept out of ps/systemd
		// status) and fall back to the flag for local/dev use.
		consolePw := *consolePass
		if consolePw == "" {
			consolePw = os.Getenv("SPARKBOX_CONSOLE_PASSWORD")
		}
		if consolePw != "" {
			consoleH := console.New(mgr, routeStore, *proxyDomain, consolePw, *proxyTLS, log)
			consoleH.SetSchedules(scheduleStore)
			// Everything goes through the fleet: the listing and every
			// lifecycle action, so the placement ledger sees every name the
			// console takes or frees — and the balloon read too, which only the
			// machine running a VM can answer and which the fleet routes there.
			consoleH.SetSandboxes(flt)
			consoleH.SetVitals(flt)
			consoleH.SetCapacities(flt.Capacities)
			consoleH.SetDialer(flt.DialContext)
			px.SetConsole(*consoleSub, consoleH.Handler())
			log.Info("operator console enabled", "url", *consoleSub+"."+*proxyDomain)
		}
		// Per-user console: self-service dashboard authenticated by the same
		// edge session as private routes.
		if *userConsoleSub != "" {
			warnSubdomainCollision("user console", *userConsoleSub, mgr, routeStore, log)
			// xtermLabel, not *xtermSub: it is already emptied when there is no
			// proxy edge, and the console's Terminal button must not link to a
			// host nothing serves.
			uc := userconsole.New(mgr, routeStore, secretsStore, netrulesStore, flt, faviconCache, userStore, sessionSigner, syncer, *userConsoleSub, *proxyDomain, xtermLabel, *proxyTLS, log)
			// Same as the operator console: the fleet answers everything an
			// owner can act on, and routes the balloon and CPU reads to the
			// machine holding each sandbox.
			uc.SetSandboxes(flt)
			uc.SetVitals(flt)
			uc.SetDialer(flt.DialContext)
			px.SetReserved(*userConsoleSub, uc.Handler())
			// The apex has no other job, and the user console is the only page a
			// visitor who typed the bare domain could have wanted. Set alongside
			// the mount so the redirect cannot outlive its target.
			px.SetHome(*userConsoleSub)
			log.Info("user console enabled", "url", *userConsoleSub+"."+*proxyDomain,
				"apex_redirects_here", *proxyDomain)
		}

		// Browser terminals: one host per sandbox, so the browser's own origin
		// isolation keeps one sandbox's page from scripting another's socket.
		// The host is <name>-xterm.<domain> — one label, so it costs no wildcard
		// of its own in either DNS or TLS. It buys that with a name suffix the
		// platform has to reserve; see proxy.SetReservedSuffix.
		var xt *xterm.Handler
		if xtermLabel != "" {
			warnXtermSuffixCollision(xtermLabel, mgr, routeStore, log)
			xt = xterm.New(xterm.Config{
				Sandboxes: flt, Accounts: userStore, Sessions: sessionSigner,
				UpstreamKey: upstreamKey, Dial: flt.DialContext,
				// The fleet, not the manager: a balloon and a VMM process can
				// only be asked of the machine running them, and the fleet is
				// what knows which machine that is. Node lets the handler give
				// a remote reading the longer budget it needs.
				Vitals: flt, Node: mgr.NodeName(),
				// Same routing argument as Vitals: the restart has to happen on
				// the machine holding the VM, and the fleet is what knows which.
				Turbo:  flt,
				Domain: *proxyDomain, Subdomain: xtermLabel,
				LoginURL: "https://" + *loginSub + "." + *proxyDomain + "/",
				// The gateway owns the one live-session registry the manager
				// closes on pause; a browser terminal that kept its own would be
				// silently stranded when the reaper pauses its sandbox.
				Track: func(sandbox string, s xterm.SessionConn, _ bool) func() {
					return gw.TrackTerminal(sandbox, s)
				},
				Log: log,
			})
			px.SetReservedSuffix(xtermLabel, xt.Handler())
			log.Info("browser terminals enabled", "url", "https://<name>-"+xtermLabel+"."+*proxyDomain)
		}

		// The REST API mirrors the ctl@ command surface for callers that have a
		// session token but no SSH client. It shares `ops` with the gateway, so
		// there is exactly one place where ownership, timeout budgets and the
		// tags-before-create ordering are decided.
		if *apiSub != "" {
			warnSubdomainCollision("rest api", *apiSub, mgr, routeStore, log)
			// A nil Terminal is a typed-nil trap waiting to happen: assigning a
			// nil *xterm.Handler to the interface would make restapi's own
			// "not configured" check see a non-nil value and 501 turn into a
			// panic. Assign only through the non-nil branch.
			apiCfg := restapi.Config{
				Ops: ops, Accounts: userStore, Signer: sessionSigner,
				Subdomain: *apiSub, Domain: *proxyDomain,
				// The configured labels, not the spec's defaults: every example
				// in the served document is rewritten from them, so a host that
				// moved either subtree still hands out copy-paste that works.
				XtermSubdomain: xtermLabel,
				LoginURL:       "https://" + *loginSub + "." + *proxyDomain + "/",
				Log:            log,
			}
			if xt != nil {
				apiCfg.Terminal = terminalBridge{xt}
			}
			px.SetReserved(*apiSub, restapi.New(apiCfg).Handler())
			log.Info("rest api enabled", "url", "https://"+*apiSub+"."+*proxyDomain,
				"docs", "https://"+*apiSub+"."+*proxyDomain+"/docs")
		}
		proxySrv = &http.Server{
			Addr:    *proxyAddr,
			Handler: px,
			// Recover the pre-DNAT port below TLS: iptables REDIRECTs the private
			// port range to this one listener, and this is how a request learns it
			// was dialed on e.g. :4444 so it can forward to that guest port.
			ConnContext: func(ctx context.Context, c net.Conn) context.Context {
				if p, ok := proxy.OriginalDstPort(c); ok {
					return proxy.WithOriginalPort(ctx, p)
				}
				return ctx
			},
		}
		if *proxyTLS {
			log.Info("obtaining TLS certificate", "provider", *tlsProvider, "domain", *proxyDomain)
			names, terr := setupProxyTLS(ctx, proxySrv, tlsParams{
				provider: *tlsProvider, domain: *proxyDomain, email: *tlsEmail,
				stateDir: *stateDir, log: log,
			})
			if terr != nil {
				return fmt.Errorf("proxy tls: %w", terr)
			}
			// Report what was actually obtained, not what was asked for.
			// autocert reports nothing — it issues per-SNI on first request.
			log.Info("tls certificates managed", "names", names)
			// Sniff each connection below TLS: a cleartext-HTTP client that
			// dialed the HTTPS edge (e.g. http://myvm.hivemind.tools:4444) is
			// answered with a 308 to the https:// URL instead of the TLS stack's
			// bare "Client sent an HTTP request to an HTTPS server.".
			ln, lerr := net.Listen("tcp", proxySrv.Addr)
			if lerr != nil {
				return fmt.Errorf("proxy edge listen %s: %w", proxySrv.Addr, lerr)
			}
			ln = proxy.RedirectPlainHTTP(ln, log)
			go func() { errCh <- proxySrv.ServeTLS(ln, "", "") }()
		} else {
			go func() { errCh <- proxySrv.ListenAndServe() }()
		}
		// No DNS half to publish: <name>-xterm.<domain> is one label, so the
		// same wildcard record that already answers for every sandbox front door
		// answers for its terminal too.
	}

	log.Info("sparkbox up", "driver", *driverName, "ssh", *sshAddr, "api", *apiAddr,
		"proxy", *proxyAddr, "domain", *proxyDomain, "proxy_tls", *proxyTLS)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	log.Info("shutting down")
	// Release attached terminals BEFORE tearing anything down. sshSrv.Close()
	// drops live connections abruptly, which leaves a full-screen TUI's modes
	// (mouse reporting, alternate screen) latched in the user's terminal with
	// nothing left to undo them — the redeploy-wedged-my-terminal case. This
	// blocks briefly so the restore sequence actually reaches the wire before
	// the process exits; systemd's stop timeout is far longer.
	gw.CloseAllSessions("was interrupted by a control-plane restart", 3*time.Second)
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	apiSrv.Shutdown(shutCtx) //nolint:errcheck
	if proxySrv != nil {
		proxySrv.Shutdown(shutCtx) //nolint:errcheck
	}
	sshSrv.Close() //nolint:errcheck
	return nil
}

// privateAPIHandler adds process observability to the existing private control
// listener. It must never be mounted on the public proxy edge.
func privateAPIHandler(control, metrics http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	mux.Handle("/", control)
	return mux
}

// terminalBridge lets the REST API serve GET /v1/sandboxes/{name}/terminal from
// the same WebSocket code path as the browser page, instead of a second copy of
// the framing, the origin gate and the close codes.
//
// The ctlops.Caller is deliberately unused: internal/restapi has already run the
// owner gate — where the JSON error envelope lives, so a stranger gets the same
// masked 404 every other endpoint returns rather than an opaque close code — and
// checking twice in two packages is how the two eventually disagree.
type terminalBridge struct{ xt *xterm.Handler }

func (t terminalBridge) ServeTerminal(w http.ResponseWriter, r *http.Request, _ ctlops.Caller, sandbox string) {
	sess, _ := edgeauth.From(r.Context())
	t.xt.Bridge(w, r, sandbox, sess)
}

// nodeNameOr resolves this machine's name in the fleet from --node-name.
//
// The hostname stays the default because it is already the `box` claim in every
// id token this host has ever issued — externally observable, so a fleet-wide
// rename to something tidier like "local" would break relying parties that
// pinned it. Both modes resolve it through here so a machine cannot introduce
// itself to a gateway under one name and stamp another into its own tokens.
func nodeNameOr(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if h, _ := os.Hostname(); h != "" {
		return h
	}
	return "local"
}

// effectiveVMStateDir preserves the historical one-directory layout unless an
// operator explicitly places the hot VM data elsewhere.
func effectiveVMStateDir(configured, stateDir string) string {
	if configured == "" {
		return stateDir
	}
	return configured
}

func validateTransportFlag(flagName, value string, allowed ...string) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("%s=%q is invalid (expected %s)", flagName, value, strings.Join(allowed, "|"))
}

// warnSubdomainCollision reports a reserved subdomain something already answers
// for. Reserved dispatch runs before route lookup, so the squatter goes dark
// rather than winning — but a sandbox or custom route may predate the
// reservation, and which one moves is the operator's call, not ours.
func warnSubdomainCollision(what, sub string, mgr *host.Manager, rs *routes.Store, log *slog.Logger) {
	if _, exists := mgr.Get(sub); exists {
		log.Warn(what+" subdomain collides with an existing sandbox, which is now unreachable over HTTP",
			"subdomain", sub)
		return
	}
	if rt, found, err := rs.GetBySubdomain(sub); err == nil && found {
		log.Warn(what+" subdomain collides with an existing route, which is now unreachable",
			"subdomain", sub, "sandbox", rt.Sandbox)
	}
}

// warnDoorNameCollision reports a sandbox holding the name of an SSH gateway
// door. Username routing resolves the gateway's own doors before it looks for a
// sandbox, so the squatter goes dark rather than winning — and unlike the HTTP
// surfaces this one has no subdomain flag to move it to. A sandbox may predate
// the reservation (names are validated at create and rename time only), so
// renaming it is the operator's call.
func warnDoorNameCollision(door string, mgr *host.Manager, log *slog.Logger) {
	if _, exists := mgr.Get(door); exists {
		log.Warn("a sandbox is named after an ssh gateway door, so it is unreachable as ssh "+door+"@<domain>",
			"sandbox", door)
	}
}

// warnXtermSuffixCollision is the same warning for a family of names rather
// than one name. Every subdomain ending in "-<label>" now reaches the terminal
// handler before any route lookup, so a sandbox or route that predates the
// reservation — or one created under a different --xterm-subdomain, which the
// stores' own hardcoded "-xterm" cannot know about — goes dark with nothing in
// the request to explain it. The stores refuse new such names; this reports the
// ones already on disk.
func warnXtermSuffixCollision(label string, mgr *host.Manager, rs *routes.Store, log *slog.Logger) {
	suffix := xterm.ReservedSuffix(strings.ToLower(label))
	for _, b := range mgr.List() {
		if strings.HasSuffix(strings.ToLower(b.Name), suffix) {
			log.Warn("sandbox name ends in the browser-terminal suffix, so its web front door is now unreachable",
				"sandbox", b.Name, "suffix", suffix)
		}
	}
	all, err := rs.List()
	if err != nil {
		return // a startup courtesy, not a precondition
	}
	for _, rt := range all {
		if strings.HasSuffix(strings.ToLower(rt.Subdomain), suffix) {
			log.Warn("route subdomain ends in the browser-terminal suffix, so it is now unreachable",
				"subdomain", rt.Subdomain, "sandbox", rt.Sandbox)
		}
	}
}

// defaultAudience is the hivemind SaaS URL: the relying party sparkbox exists
// to federate with today. The audience allowlist defaults closed to it — a
// relying party enforces its own `aud`, but minting only what we mean to mint
// keeps a stolen token from being replayable anywhere else.
const defaultAudience = "https://hivemind.wandb.tools"

// ---------------------------------------------------------------------------
// The control plane a gateway runs
// ---------------------------------------------------------------------------

// gatewayStores are the fleet-wide stores the control plane is assembled from.
// They are a struct rather than a dozen parameters so the assembly below reads
// as the wiring diagram it is.
type gatewayStores struct {
	Fleet       *fleet.Fleet
	Checkpoints ctlops.Checkpoints
	Placement   *placement.Store
	Roster      *nodes.Store
	Users       *users.Store
	Secrets     *secrets.Store
	Schedules   *schedule.Store
	Routes      *routes.Store
	Sessions    *edgeauth.Signer

	DefaultImage       string
	Domain             string
	GatewayGuestSubnet string
	XtermSubdomain     string
	InvitesPerUser     int
	// GitHubClientID turns the OAuth device flow on. Empty leaves it off, which
	// is not a degraded state so much as the state every host was in before it
	// existed: `keys verify-github` still links an account whose key is
	// published on GitHub.
	GitHubClientID string
	Log            *slog.Logger
}

// newGatewayOps builds the one ctlops.Ops every transport on this host shares.
//
// It is a function rather than a literal inside serve() because the wiring *is*
// the feature, and a missing field here is silent: ctlops decides whether a
// capability exists by comparing a store against nil, so dropping Nodes turns
// `ssh ctl@<gw> node ls` into "this host is not a fleet gateway" on a machine
// that is plainly running one — with nothing else in the process noticing, and
// therefore no node ever approvable. A test builds exactly this value and asks
// the Ops whether it is a fleet, so the field cannot go missing again.
func newGatewayOps(s gatewayStores) *ctlops.Ops {
	return ctlops.New(ctlops.Config{
		Sandboxes: s.Fleet, Templates: s.Fleet, Accounts: s.Users,
		Checkpoints: s.Checkpoints,
		Tags:        s.Secrets, Schedules: s.Schedules, Routes: s.Routes,
		Sessions: s.Sessions,
		// The roster reaches the control plane joined to the live fleet, which
		// is what ctlops.NodeRoster asks of whoever wires it: the roster alone
		// cannot say whether a machine is answering, and the fleet alone cannot
		// say which fingerprint an operator compared before approving it.
		Nodes: fleetRoster{roster: s.Roster, index: s.Placement, flt: s.Fleet},
		// nil rather than a client with an empty id: ctlops decides whether the
		// flow exists by comparing this against nil, and a client that would
		// answer every start with "invalid client" is worse than a host that
		// says plainly it cannot do this.
		GitHubDevice: githubDevice(s.GitHubClientID),
		DefaultImage: s.DefaultImage, Domain: s.Domain,
		GatewayGuestSubnet: s.GatewayGuestSubnet,
		XtermSubdomain:     s.XtermSubdomain, InvitesPerUser: s.InvitesPerUser,
		Log: s.Log,
	})
}

// localCheckpointOps keeps the manual checkpoint v1 explicitly node-local.
// Enabled is target-aware, so a gateway with a mounted checkpoint directory
// does not advertise the operation for a sandbox placed on another node.
type localCheckpointOps struct {
	mgr *host.Manager
}

func (c localCheckpointOps) Enabled(name string) bool {
	if c.mgr == nil || !c.mgr.CheckpointEnabled() {
		return false
	}
	_, ok := c.mgr.Get(name)
	return ok
}

func (c localCheckpointOps) Checkpoint(ctx context.Context, name string) error {
	return c.mgr.Checkpoint(ctx, name)
}

func (c localCheckpointOps) RestoreCheckpoint(ctx context.Context, name string) error {
	return c.mgr.RestoreCheckpoint(ctx, name)
}

// defaultGitHubClientID is the app a stock sparkbox links accounts through.
//
// A client id is a public identifier — it travels in the request that mints a
// device code and is meant to be read by the user authorizing it — so shipping
// one costs nothing and saves every operator the app registration. It does mean
// the consent screen names that app rather than the host's own; an operator who
// would rather it named theirs registers one and passes --github-client-id,
// and one who wants no GitHub linking at all passes the empty string.
//
// Whatever app it names must have the device flow enabled, or every attempt
// fails with the same message. See docs/github-linking-design.md.
const defaultGitHubClientID = "Iv23liV6n9amGfGY20Js"

// githubDevice builds the device-flow client, or nil when no app is configured.
func githubDevice(clientID string) ctlops.GitHubDeviceFlow {
	if clientID == "" {
		return nil
	}
	return users.NewGitHubDevice(clientID)
}

// fleetRoster answers the operator's two questions about a machine that neither
// store can answer alone: is it answering right now (the fleet knows), and what
// would removing it strand (the placement ledger knows). The roster row carries
// the rest — the fingerprint, who approved it and when.
type fleetRoster struct {
	roster *nodes.Store
	index  *placement.Store
	flt    *fleet.Fleet
}

func (r fleetRoster) ListNodes() ([]ctlops.NodeInfo, error) {
	rows, err := r.roster.List()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]nodes.Node, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}

	var out []ctlops.NodeInfo
	linked := map[string]bool{}
	for _, st := range r.flt.Nodes() {
		info, err := r.join(st, byName[st.Name])
		if err != nil {
			return nil, err
		}
		linked[st.Name] = true
		out = append(out, info)
	}
	// A roster row with no link is a machine waiting for approval, or one that
	// was approved and is not answering. Both are exactly what an operator
	// opened this listing to see.
	for _, row := range rows {
		if linked[row.Name] {
			continue
		}
		held, err := r.held(row.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, ctlops.NodeInfo{
			Name: row.Name, Status: row.Status, FP: row.FP,
			Arch: row.Arch, Release: row.Release, Sandboxes: held,
			ApprovedBy: row.ApprovedBy, ApprovedAt: row.ApprovedAt, LastSeen: row.LastSeen,
			GuestSubnet: row.ApprovedGuestSubnet, GRPCAddr: row.GRPCAddr,
			CertSerial: row.CertSerial, CertExpiresAt: row.CertExpiresAt,
			CertRevokedAt: row.CertRevokedAt,
		})
	}
	return out, nil
}

func (r fleetRoster) join(st fleet.NodeStatus, row nodes.Node) (ctlops.NodeInfo, error) {
	held, err := r.held(st.Name)
	if err != nil {
		return ctlops.NodeInfo{}, err
	}
	// held is the ledger's count, and today the ledger only ever has rows for
	// this machine: a linked node's inventory is cached on its link but nothing
	// durable is written from it (see fleet.linkInventory), so a sandbox living
	// on another machine has no placement here at all. RemoveNode refuses on
	// this number, and refusing on the ledger alone would make that guard say
	// "holds nothing" about every remote machine in the fleet — which is the one
	// case it exists for. So the machine's own report counts until the gateway
	// places sandboxes on nodes itself, and taking the larger of the two means a
	// node that under-reports still cannot hide a placement the ledger knows of.
	if st.Sandboxes > held {
		held = st.Sandboxes
	}
	info := ctlops.NodeInfo{
		Name: st.Name, Status: st.Status, Online: st.Online, Local: st.Local,
		Arch: st.Arch, Release: st.Release, Sandboxes: held, LastSeen: st.LastSeen,
		Egress: st.Egress,
	}
	if row.Name != "" {
		info.Status, info.FP = row.Status, row.FP
		info.ApprovedBy, info.ApprovedAt = row.ApprovedBy, row.ApprovedAt
		info.GuestSubnet, info.GRPCAddr = row.ApprovedGuestSubnet, row.GRPCAddr
		info.CertSerial, info.CertExpiresAt = row.CertSerial, row.CertExpiresAt
		info.CertRevokedAt = row.CertRevokedAt
	}
	return info, nil
}

// held is how many names the ledger still places on a machine — the number that
// decides whether removing it would strand anything.
func (r fleetRoster) held(node string) (int, error) {
	rows, err := r.index.ByNode(node)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// ApproveNode blesses the machine holding the key with this fingerprint. It is
// keyed on the fingerprint rather than the name for the reason ctlops.ApproveNode
// documents: a node names itself, so a name cannot carry an approval.
func (r fleetRoster) ApproveNode(fp, by string) (ctlops.NodeInfo, error) {
	if err := r.roster.ApproveFP(fp, by); err != nil {
		return ctlops.NodeInfo{}, err
	}
	list, err := r.ListNodes()
	if err != nil {
		return ctlops.NodeInfo{}, err
	}
	for _, n := range list {
		if n.FP != "" && n.FP == fp {
			return n, nil
		}
	}
	return ctlops.NodeInfo{}, nodes.ErrNoSuchNode
}

func (r fleetRoster) ApproveFPWithConfig(fp, by string, cfg nodes.ApprovalConfig) error {
	return r.roster.ApproveFPWithConfig(fp, by, cfg)
}

func (r fleetRoster) RemoveNode(name string) error { return r.roster.Remove(name) }

// EvictNode closes the link a machine is holding right now. It is the other
// half of taking an approval away, and it is only half a line here because the
// fleet this adapter is already joined to is the thing that owns the link.
//
// Without it the durable half of a revocation happens and the live half does
// not: approval is read once, at the door (sshgw.admitNode), so a machine that
// is already connected keeps its control channel, keeps reporting capacity into
// the fleet's totals and keeps its data channels for as long as it cares to —
// on a gateway whose operator has been told it is gone.
func (r fleetRoster) EvictNode(name, reason string) bool { return r.flt.EvictNode(name, reason) }

// The interfaces this adapter must satisfy, checked by the compiler.
//
// NodeEvicter is the one that matters here. ctlops discovers it with a runtime
// type assertion, because a roster that cannot reach a link is still a perfectly
// good roster — which means a gateway that stopped satisfying it would keep
// building, keep passing its tests and silently stop closing links. This line is
// what turns that into a compile error. Every optional interface the control
// plane asserts for against a value this package supplies belongs on this list,
// and it is the whole list today: NodeEvicter is the only one ctlops has.
var (
	_ ctlops.NodeRoster             = fleetRoster{}
	_ ctlops.NodeEvicter            = fleetRoster{}
	_ ctlops.NodeConfiguredApprover = fleetRoster{}
)

// netpushFleet adapts the host manager to netpush.Fleet: only running sandboxes
// have a live tap, so paused/archived ones are filtered out here.
//
// It stays on the local manager rather than the fleet router on purpose: sluice
// runs once per machine, enforces against that machine's own taps, and its PUT
// /policy replaces the whole set — feeding it another machine's sandboxes would
// describe taps that do not exist here.
type netpushFleet struct{ mgr *host.Manager }

func (f netpushFleet) List() []netpush.Sandbox {
	var out []netpush.Sandbox
	for _, b := range f.mgr.List() {
		if b.State != vmm.StateRunning {
			continue
		}
		out = append(out, netpush.Sandbox{Name: b.Name, Owner: b.Owner, HostIP: b.HostIP})
	}
	return out
}

// pushLoop reconciles egress policy to sluice on a ticker (and once at start),
// so fleet changes that skip the console's change-time push still converge.
//
// The pusher is an interface of one method so the two callers can differ: a
// gateway hands it the FLEET, which resolves every machine's share and pushes
// each one its own, and a node hands it that node's local syncer, which has no
// rules of its own and is driven by what the gateway sends it. Both converge on
// the same cadence, which is what makes a missed push — an offline node, a
// restarted daemon — cost at most one interval.
type netPusher interface {
	PushNet(ctx context.Context) error
}

func pushLoop(ctx context.Context, p netPusher, log *slog.Logger) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		if err := p.PushNet(ctx); err != nil && ctx.Err() == nil {
			log.Warn("periodic egress policy push", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// fleetIdentity is the gateway's signing path, presented to the fleet.
//
// It is an adapter rather than the issuer itself for one reason worth stating:
// every claim it assembles comes from the sandbox record and the node name the
// FLEET resolved, never from anything the asking machine sent. metadata.Local
// already does exactly that assembly for this host's own guests, so this reuses
// it with the node name substituted — which keeps a token minted for a sandbox
// on `laptop` identical to one minted for the same sandbox had it been created
// here, but for the `box` claim, which is the one thing that genuinely differs.
type fleetIdentity struct {
	issuer *oidc.Issuer
	users  *users.Store
	defAud string
}

func (f fleetIdentity) local(node string) metadata.Local {
	return metadata.Local{Issuer: f.issuer, Users: f.users, NodeName: node}
}

func (f fleetIdentity) Issue(ctx context.Context, box *host.Sandbox, node, aud string) (string, time.Time, error) {
	if aud == "" {
		aud = f.defAud
	}
	tok, err := f.local(node).Issue(ctx, box, aud)
	if err != nil {
		return "", time.Time{}, err
	}
	return tok.JWT, tok.ExpiresAt, nil
}

func (f fleetIdentity) Describe(ctx context.Context, box *host.Sandbox, node string) (string, fleet.Claims, error) {
	doc, err := f.local(node).Describe(ctx, box)
	if err != nil {
		return "", fleet.Claims{}, err
	}
	return doc.Issuer, fleet.Claims{
		Subject: doc.Subject, Owner: doc.Owner, GitHub: doc.GitHub, KeyFP: doc.KeyFP,
		Sandbox: doc.Sandbox, SandboxID: doc.SandboxID, Image: doc.Image, Box: doc.Box,
	}, nil
}

// splitList parses a comma-separated flag into a trimmed, non-empty list.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstOr(list []string, def string) string {
	if len(list) > 0 {
		return list[0]
	}
	return def
}

// portOf extracts the numeric port from a listen address like ":443" or
// "0.0.0.0:8081". Returns 0 when there is no parseable port, which just
// disables the edge's "dialed me directly" check in the proxy.
// advertisedPort picks the public port user-facing instructions and browser
// origins should use: the advertised override when set (an edge or load
// balancer can expose a different port than the process binds), else the
// listen port.
func advertisedPort(advertised int, listenAddr string) int {
	if advertised != 0 {
		return advertised
	}
	return portOf(listenAddr)
}

func advertisedHost(advertised, fallback string) string {
	if advertised != "" {
		return advertised
	}
	return fallback
}

func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// detectHostMemMB reads total RAM from /proc/meminfo (Linux). Returns 0 when it
// can't be determined (e.g. non-Linux dev machines), which disables admission.
func detectHostMemMB() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line) // ["MemTotal:", "<kB>", "kB"]
		if len(fields) >= 2 {
			if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return kb / 1024
			}
		}
	}
	return 0
}
