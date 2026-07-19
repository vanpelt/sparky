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
	"github.com/vanpelt/sparky/tools/sparkbox/internal/dnsedge"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envsync"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/frontdoor"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/objstore"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/proxy"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/userconsole"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	fcdriver "github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/firecracker"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sparkbox <serve|fetch-secrets> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "fetch-secrets":
		err = fetchSecrets(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: sparkbox <serve|fetch-secrets> [flags]")
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
		driverName     = fs.String("driver", "mock", "vm driver: mock | firecracker")
		stateDir       = fs.String("state-dir", "./state", "directory for the sqlite store, certs, and VM data")
		keyDir         = fs.String("key-dir", "", "directory holding the three fleet key PEMs (default: --state-dir); point at tmpfs on a fleet host fed by `sparkbox fetch-secrets`")
		requireKeys    = fs.Bool("require-keys", false, "fail if a fleet key is missing instead of generating one — set on fleet hosts, where a missing key means the Secret Manager fetch failed and generating a fresh identity would lock the fleet out")
		usersPath      = fs.String("users", "", "users file: '<user> <authorized_keys line>' per line (required)")
		sshAddr        = fs.String("ssh-addr", ":2222", "SSH gateway listen address")
		sshAdvertise   = fs.Int("ssh-advertise-port", 0, "gateway port shown in user-facing instructions when it differs from the listen port (e.g. 22 when an edge DNAT forwards :22 to the gateway); 0 uses the listen port")
		apiAddr        = fs.String("api-addr", "127.0.0.1:8080", "control API listen address (no auth — keep private)")
		defaultImage   = fs.String("default-image", "universal", "rootfs template for new sandboxes")
		defaultLogin   = fs.String("default-login-user", "sparky", "guest account the gateway SSHes in as; must match the template's baked authorized_keys (our images declare it via the sparkbox.login-user label, published as ROOTFS_LOGIN_USER in the release manifest)")
		idleTimeout    = fs.Duration("idle-timeout", 30*time.Minute, "pause sandboxes idle longer than this")
		idleBalloon    = fs.Duration("idle-balloon", 2*time.Minute, "balloon a warm sandbox down to --mem-reserve-mb after this much idle, reclaiming its RAM while it keeps running (0 disables; needs --mem-reserve-mb)")
		activityCPU    = fs.Float64("activity-cpu-pct", 2, "treat a sandbox as active while it burns at least this % of one host core (0 disables); keeps an unattended build or agent from being reaped. Idle boxes measure ~0.4%")
		activityNetKB  = fs.Int64("activity-net-kb", 64, "treat a sandbox as active while it moves at least this many KiB per reaper tick in either direction (0 disables). Idle boxes measure ~3 KB/min, a working agent 400 KB+")
		maxPerOwner    = fs.Int("max-running-per-owner", 2, "max concurrently running sandboxes per owner (0 = unlimited); pause with `ssh ctl@host pause <name>`")
		memAdmitPct    = fs.Int("mem-admission-pct", 85, "refuse to start a sandbox if running sandboxes' RAM cost would exceed this % of host RAM (0 = disabled)")
		hostMemMB      = fs.Int64("host-mem-mb", 0, "host RAM in MB for admission control (0 = auto-detect from /proc/meminfo)")
		memReserve     = fs.Int64("mem-reserve-mb", 0, "per-VM working-set floor (MB) enabling live overcommit: admission counts this instead of the full memory ceiling, and idle VMs balloon down to it (0 = off; count the full ceiling, never balloon)")
		diskPool       = fs.Int64("disk-pool-mb-per-owner", 0, "cap an owner's pooled on-disk usage across all their sandboxes + archives (0 = unlimited); soft accounting enforced at create/restore")
		archiveRemote  = fs.String("archive-remote", "", "rclone remote name for sandbox archives (e.g. sparkbox-artifacts); empty disables archive/restore. Needs S3 WRITE creds in the host's rclone.conf")
		archiveBucket  = fs.String("archive-bucket", "", "bucket within --archive-remote for archives (required to enable archiving)")
		archivePrefix  = fs.String("archive-prefix", "archives", "object-key prefix archives are written under: <prefix>/<owner>/<name>.ext4.zst")
		kernelPath     = fs.String("kernel", "", "firecracker: vmlinux path")
		imageDir       = fs.String("image-dir", "", "firecracker: directory of <image>.ext4 templates")
		subnet6        = fs.String("subnet6", "", "routable IPv6 /64 delegated to the host (e.g. 2001:db8:1c7::/64); gives each sandbox a no-NAT v6 address and a front-door address for hostname SSH routing")
		proxyAddr      = fs.String("proxy-addr", ":8081", "HTTP proxy edge listen address for <sub>.<domain> (empty to disable)")
		proxyDomain    = fs.String("proxy-domain", "hivemind.tools", "base domain for sandbox web routes")
		edgeV4         = fs.String("edge-v4", "", "public IPv4 of the proxy edge; when set, each sandbox also gets an A record here so <name>.<domain> resolves over IPv4 (the per-name front-door AAAA otherwise shadows the wildcard A). Point it at the same address the wildcard *.<domain> A does")
		proxyTLS       = fs.Bool("proxy-tls", false, "terminate TLS for the proxy edge (see --tls-provider)")
		consolePass    = fs.String("console-password", "", "password for the operator console at <console-subdomain>.<domain> (empty disables it)")
		consoleSub     = fs.String("console-subdomain", "console", "subdomain that serves the operator console")
		loginSub       = fs.String("login-subdomain", "login", "subdomain serving the browser sign-in for private (authenticated) routes")
		userConsoleSub = fs.String("user-console-subdomain", "my", "subdomain serving the per-user console (empty disables it)")
		sessionTTL     = fs.Duration("session-ttl", 12*time.Hour, "lifetime of a browser session cookie minted for private routes")
		tlsProvider    = fs.String("tls-provider", "cloudflare", "TLS certs when --proxy-tls: cloudflare (DNS-01 wildcard, needs CLOUDFLARE_API_TOKEN) | autocert (per-host on-demand)")
		tlsEmail       = fs.String("tls-email", "", "ACME account email (recommended for cert-expiry notices)")
		oidcSub        = fs.String("oidc-subdomain", "oidc", "subdomain serving the OIDC discovery document and JWKS")
		oidcAud        = fs.String("oidc-audiences", defaultAudience, "comma-separated allowlist of `aud` values id tokens may be minted for (empty = any)")
		metaAddr       = fs.String("metadata-addr", fmt.Sprintf(":%d", metadata.DefaultPort), "guest metadata/token service listen address (reachable only from sandbox taps)")
		dnsAddr        = fs.String("dns-addr", "", "wildcard DNS responder listen address (e.g. <edge-ip>:53); serves *.<domain> -> the edge for a Tailscale split-DNS entry. Empty disables it")
		dnsAnswer      = fs.String("dns-answer", "", "comma-separated IPs the wildcard DNS answers with (default: the IP host of --proxy-addr)")
		openSignup     = fs.Bool("open-signup", false, "let anyone with an SSH key register at signup@ without an invite code")
		invitesPer     = fs.Int("invites-per-user", 0, "how many invite codes a non-operator user may mint (0 = operators only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *usersPath == "" {
		return errors.New("--users is required")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		return err
	}
	// The three fleet keys live in --key-dir (default --state-dir). On a fleet
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

	var driver vmm.Driver
	switch *driverName {
	case "mock":
		md := mock.New(*stateDir, hostKey)
		md.LoginUser = *defaultLogin
		driver = md
	case "firecracker":
		driver, err = fcdriver.New(fcdriver.Options{
			KernelPath: *kernelPath, ImageDir: *imageDir, StateDir: *stateDir,
			Subnet6: *subnet6, LoginUser: *defaultLogin,
		})
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
	nodeName, _ := os.Hostname()

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
				if *edgeV4 != "" {
					if a, perr := netip.ParseAddr(*edgeV4); perr == nil && a.Is4() {
						edge = a
					} else {
						log.Warn("ignoring --edge-v4: not an IPv4 address", "value", *edgeV4)
					}
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
		StateDir: *stateDir, Driver: driver,
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
		Routes:             routeStore,
		Schedules:          scheduleStore,
		Tags:               secretsStore,
		MaxRunningPerOwner: *maxPerOwner,
		MemAdmissionPct:    *memAdmitPct,
		HostMemMB:          hostMem,
		MemReserveMB:       *memReserve,
		DiskPoolMBPerOwner: *diskPool,
		ActivityCPUPct:     *activityCPU,
		ActivityNetBytes:   uint64(*activityNetKB) * 1024,
		ArchivePrefix:      *archivePrefix,
		NodeName:           nodeName,
		HostVCPUs:          int64(runtime.NumCPU()),
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
	mgr, err := host.NewManager(mgrOpts)
	if err != nil {
		return err
	}
	// Secret-env propagation: the syncer rewrites a sandbox's managed
	// /etc/environment block over SSH when it reaches running (create, resume,
	// restore, fork) and when the console changes a tag or secret. Installed
	// post-construction so the manager never depends on it at build time.
	syncer := envsync.New(secretsStore, mgr, upstreamKey, log)
	mgr.SetEnvSync(syncer)
	log.Info("resource limits", "max_running_per_owner", *maxPerOwner,
		"mem_admission_pct", *memAdmitPct, "host_mem_mb", hostMem,
		"mem_reserve_mb", *memReserve, "overcommit", *memReserve > 0)

	gw := sshgw.New(sshgw.GatewayOptions{
		Manager: mgr, Users: userStore, HostKey: hostKey, UpstreamKey: upstreamKey,
		DefaultImage: *defaultImage, Logger: log,
		Doors: doors, Domain: *proxyDomain,
		OpenSignup: *openSignup, InvitesPerUser: *invitesPer,
		Schedules: scheduleStore,
		Routes:    routeStore, Session: sessionSigner,
	})
	// The gateway knows which terminals are attached to which sandbox, so it is
	// what the manager calls to release them when a sandbox is paused.
	mgr.SetSessions(gw)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Claim the front-door range (AnyIP), then run every hook (NDP plumbing +
	// DNS records) for the reserved names and each existing sandbox. This is
	// the reconcile pass: new sandboxes are handled on create, and anything
	// that failed or drifted since the last run is repaired here.
	if plumber != nil {
		plumber.EnsureRange(ctx)
		for _, r := range sshgw.ReservedUsers {
			doorHooks.Ensure(ctx, r)
		}
		for _, b := range mgr.List() {
			doorHooks.Ensure(ctx, b.Name)
		}
		log.Info("front doors enabled", "range", doors.Range(),
			"new", doors.Addr(sshgw.NewSandboxUser).String())
	}

	// A process restart marks every sandbox paused; bring the pinned ones back
	// up so their in-guest daemons keep running across a host reboot.
	mgr.ResumePinned(ctx)

	go mgr.RunReaper(ctx, *idleBalloon, *idleTimeout, time.Minute)

	// The platform scheduler wakes sandboxes to run due cron jobs (the honest
	// answer to background work in a scale-to-zero world). It ticks every 30s so
	// minute-granularity crons fire promptly; the gateway is its exec runner.
	go schedule.NewScheduler(scheduleStore, gw, log).Run(ctx, 30*time.Second)

	apiSrv := &http.Server{Addr: *apiAddr, Handler: api.New(mgr, routeStore, *defaultImage, log).Handler()}
	sshSrv := gw.Server(*sshAddr)

	errCh := make(chan error, 5)
	go func() { errCh <- apiSrv.ListenAndServe() }()
	go func() { errCh <- sshSrv.ListenAndServe() }()

	// Guest metadata service: hands each sandbox an id token over its own tap.
	// It binds every interface because taps come and go, and identifies the
	// caller by source address — see internal/metadata for why that's the safe
	// end of the connection to trust.
	if *metaAddr != "" {
		meta := metadata.New(metadata.Options{
			Manager: mgr, Issuer: issuer, Users: userStore, Logger: log,
			DefaultAudience: firstOr(splitList(*oidcAud), defaultAudience),
			NodeName:        nodeName,
		})
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
		px := proxy.New(mgr, routeStore, *proxyDomain, log)
		// The issuer rides on the existing proxy edge: wildcard DNS already
		// resolves oidc.<domain> to this host and autocert already issues a cert
		// per SNI, so serving it is two GET handlers and no new listener.
		px.SetIssuer(*oidcSub, issuer.Handler())
		log.Info("oidc issuer enabled", "url", issuer.URL(), "audiences", *oidcAud)

		// Authenticated forwarding: private routes are gated behind a session the
		// visitor mints from their SSH key, and the browser sign-in rides the edge
		// at login.<domain> like the console and issuer do.
		loginH := edgeauth.NewLoginHandler(edgeauth.LoginConfig{
			Signer: sessionSigner, Domain: *proxyDomain, Secure: *proxyTLS,
			TTL: *sessionTTL, Logger: log, Gateway: *proxyDomain, GatewayPort: gatewayPort(*sshAdvertise, *sshAddr),
		})
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
			px.SetConsole(*consoleSub, consoleH.Handler())
			log.Info("operator console enabled", "url", *consoleSub+"."+*proxyDomain)
		}
		// Per-user console: self-service dashboard authenticated by the same
		// edge session as private routes. Reserved subdomains dispatch before
		// route lookup, so anything already claiming the name goes dark — the
		// handle is reserved for new signups, but a sandbox or custom route may
		// predate the reservation. Warn rather than fail: the operator picks
		// whether to rename the squatter or move the console.
		if *userConsoleSub != "" {
			if _, exists := mgr.Get(*userConsoleSub); exists {
				log.Warn("user console subdomain collides with an existing sandbox, which is now unreachable over HTTP",
					"subdomain", *userConsoleSub)
			} else if rt, found, rerr := routeStore.GetBySubdomain(*userConsoleSub); rerr == nil && found {
				log.Warn("user console subdomain collides with an existing route, which is now unreachable",
					"subdomain", *userConsoleSub, "sandbox", rt.Sandbox)
			}
			uc := userconsole.New(mgr, routeStore, secretsStore, userStore, sessionSigner, syncer, *userConsoleSub, *proxyDomain, *proxyTLS, log)
			px.SetReserved(*userConsoleSub, uc.Handler())
			log.Info("user console enabled", "url", *userConsoleSub+"."+*proxyDomain)
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
			if err := setupProxyTLS(ctx, proxySrv, tlsParams{
				provider: *tlsProvider, domain: *proxyDomain, email: *tlsEmail, stateDir: *stateDir,
			}); err != nil {
				return fmt.Errorf("proxy tls: %w", err)
			}
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
	}

	log.Info("sparkbox up", "driver", *driverName, "ssh", *sshAddr, "api", *apiAddr,
		"proxy", *proxyAddr, "domain", *proxyDomain, "proxy_tls", *proxyTLS)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	apiSrv.Shutdown(shutCtx) //nolint:errcheck
	if proxySrv != nil {
		proxySrv.Shutdown(shutCtx) //nolint:errcheck
	}
	sshSrv.Close() //nolint:errcheck
	return nil
}

// defaultAudience is the hivemind SaaS URL: the relying party sparkbox exists
// to federate with today. The audience allowlist defaults closed to it — a
// relying party enforces its own `aud`, but minting only what we mean to mint
// keeps a stolen token from being replayable anywhere else.
const defaultAudience = "https://hivemind.wandb.tools"

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
// gatewayPort picks the port user-facing instructions should show for the SSH
// gateway: the advertised override when set (an edge DNAT can expose the
// gateway on a different port than it binds), else the listen port.
func gatewayPort(advertised int, listenAddr string) int {
	if advertised != 0 {
		return advertised
	}
	return portOf(listenAddr)
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
