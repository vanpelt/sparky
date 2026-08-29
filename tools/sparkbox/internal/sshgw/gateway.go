// Package sshgw is the smart SSH proxy: clients run `ssh <sandbox>@gateway`,
// are identified by public key, and the gateway resumes the target sandbox if
// suspended (resume-on-connect) before piping the session through.
//
// SSH has no Host header, so the sandbox name travels in the SSH username —
// the simplest of the two routing schemes exe.dev's design allows (the other
// keys off destination IP from a shared pool; see docs/agentic-sandbox-design.md).
package sshgw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/frontdoor"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/nodes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// NewSandboxUser is the reserved SSH username that creates a fresh sandbox on
// connect: `ssh new@gateway`.
const NewSandboxUser = "new"

// ControlUser is the reserved SSH username for the out-of-band control channel:
// `ssh ctl@gateway ls` / `ssh ctl@gateway pause <name>`. It never routes to a
// VM, so no sandbox may be named "ctl".
const ControlUser = "ctl"

// SignupUser is the reserved SSH username that registers a new account:
// `ssh signup@gateway`. It is the one door an *unregistered* key may open —
// turning that key into an account is its entire job — so the public-key
// check lets a stranger through here and nowhere else.
const SignupUser = "signup"

// NodeUser is the reserved SSH username a fleet node connects as:
// `ssh node@gateway sparkbox-link/1`. It is deliberately absent from
// ReservedUsers: cmd/sparkbox iterates that slice to mint a front-door IPv6 and
// publish a public DNS record per entry, and resolveDoor matches a connection
// back to a door by destination IP — so joining it would publish
// node.<domain> and let anyone reach the fleet control door by address.
const NodeUser = "node"

// ReservedUsers are the gateway's own doors. No sandbox may be named one of
// these (they'd be unreachable), and each gets a front-door address before any
// sandbox exists.
var ReservedUsers = []string{NewSandboxUser, ControlUser, SignupUser}

// authedUserKey holds the handle the presented key belongs to; it is unset for
// an unregistered key at the signup door. authedKeyKey holds the key itself,
// which signup needs to register and every session needs for `key_fp`.
// authedNodeKey holds the roster row a node door's key belongs to, kept apart
// from the user keys so a node identity can never satisfy a user lookup.
// rawConnKey holds the connection's own net.Conn, which is the only handle
// anything here has on a peer that has not opened a session yet — see the node
// door's admission watchdog. probationKey holds that watchdog's disarm.
const (
	authedUserKey = "sparkbox-user"
	authedKeyKey  = "sparkbox-key"
	authedNodeKey = "sparkbox-node"
	rawConnKey    = "sparkbox-rawconn"
	probationKey  = "sparkbox-probation"
)

// The per-command timeout budgets and the resize ceiling are ctlops' —
// PauseTimeout, ArchiveTimeout, ResizeTimeout, MaxDiskMB. They are not
// restated here because every transport must apply the same numbers, and two
// answers to "how long may an archive take" is one answer too many.

// Sandboxes is the slice of the sandbox store the interactive `ssh
// <name>@gateway` path drives directly. It deliberately does not go through
// ctlops, because this path must also record the connecting key and dial the
// guest, neither of which is a control-plane operation. *host.Manager
// satisfies it, and so does a fleet router that answers for sandboxes living
// on another machine.
type Sandboxes interface {
	Get(name string) (*host.Sandbox, bool)
	List() []*host.Sandbox
	EnsureReady(ctx context.Context, name string) (*host.Sandbox, error)
	MarkActive(name string)
	RecordKey(name, fp string)
}

// FleetSandboxes is everything a fleet router has to be before it can stand in
// for the local manager: the interactive slice above, plus the two the control
// plane reads.
//
// All three are demanded here, in the field type, rather than asserted for at
// wiring time. A gateway whose sandbox lookups go through the fleet while its
// control plane reads the local manager has two answers to "who owns this box
// and where does it live": `ssh <name>@` would resume a sandbox on whichever
// machine holds it, and `ctl@ pause <name>` would look for it here and report it
// gone. Nothing can reconcile that afterwards, so a router too narrow to back
// the control plane must fail to compile rather than produce the split.
// *fleet.Fleet satisfies it.
type FleetSandboxes interface {
	Sandboxes
	ctlops.Sandboxes
	ctlops.Templates
}

// Dialer opens the raw connection to a guest port; it is
// net.Dialer.DialContext's shape. Nil means the host network, which is what a
// single-box deployment has always done.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

type Gateway struct {
	mgr  Sandboxes
	dial Dialer
	// ops is the control plane itself: every `ctl@` command is a call into it,
	// so the ownership check, the timeout budget and the error taxonomy this
	// channel applies are the same ones api.<domain> and the browser terminal do.
	ops         *ctlops.Ops
	users       *users.Store
	log         *slog.Logger
	hostKey     xssh.Signer
	upstreamKey xssh.Signer // authenticates the gateway into VMs
	dialTimeout time.Duration
	doors       *frontdoor.Mapper // optional: hostname (dest-IP) routing
	domain      string            // base domain for user-facing hints, e.g. "hivemind.tools"
	sshHost     string            // public gateway hostname when the zone apex is not routable
	// xtermSubdomain is the label the browser terminal is served under, used only
	// to tell a hung-up browser tab where to come back. Empty falls back to the
	// SSH reconnect wording.
	xtermSubdomain string
	openSignup     bool // signup without an invite code
	// nodes and joiner are the fleet door: the roster the connecting key is
	// resolved against, and whatever owns a link once it is admitted. Both nil
	// on a single-box deployment, which is what keeps that door shut.
	nodes  NodeRoster
	joiner NodeJoiner
	// nodeEnrol lets an unknown key at the node door record itself as pending.
	// It grants nothing on its own — see nodedoor.go.
	nodeEnrol bool
	// admissionBudget is how long a machine the gateway has not approved may
	// hold a connection open. New sets nodeAdmissionBudget; a test shortens it
	// before the listener starts, so nothing ever reads it concurrently.
	admissionBudget time.Duration
	// live tracks the interactive sessions attached to each sandbox so they can
	// be hung up cleanly when it is paused — see livesessions.go.
	liveMu sync.Mutex
	live   map[string]map[*liveSession]struct{}
}

type GatewayOptions struct {
	// Manager is the local machine's sandbox store. It stays a concrete type
	// because opsConfig builds the control plane out of it, and it remains the
	// sandbox lookup unless Fleet is set.
	Manager *host.Manager
	// Fleet, if set, replaces Manager on every sandbox lookup and resume this
	// channel performs, so a sandbox held by another machine is
	// indistinguishable from a local one.
	Fleet FleetSandboxes
	// Dial, if set, opens upstream connections to guests through it rather than
	// the host network — the seam a fleet's reverse tunnel plugs into.
	Dial         Dialer
	Users        *users.Store
	HostKey      xssh.Signer
	UpstreamKey  xssh.Signer
	DefaultImage string
	Logger       *slog.Logger
	// Doors, if set, enables hostname routing: connections whose destination
	// address is a front-door IPv6 are routed to the matching sandbox, and the
	// SSH username no longer has to carry the sandbox name.
	Doors *frontdoor.Mapper
	// Domain is the base DNS domain used in user-facing hints (optional).
	Domain string
	// SSHHost is the public gateway hostname used in user@host hints. Empty
	// falls back to Domain. It differs on wildcard-only edges, where the zone
	// apex is not a DNS record but a label such as ssh.<Domain> is.
	SSHHost string
	// OpenSignup drops the invite requirement, letting anyone who can reach the
	// gateway register a key. Useful for a demo box; off by default.
	OpenSignup bool
	// InvitesPerUser is how many invites a non-operator active user may mint.
	// 0 (the default) means only operators can invite.
	InvitesPerUser int
	// Schedules, if set, enables the `ctl@ schedule` commands for managing
	// platform-side cron entries. Nil disables the feature (unit tests).
	Schedules *schedule.Store
	// Routes, if set, enables `ctl@ share` to flip a sandbox's web routes
	// between public and private. Nil disables it.
	Routes *routes.Store
	// Session, if set, enables `ctl@ session-token` to mint an edge session
	// token from the caller's (already key-authenticated) ctl channel. Nil
	// disables it.
	Session *edgeauth.Signer
	// Tags, if set, enables tagging a sandbox at creation (`ssh new@<gateway>
	// --tag ml`) and the `ctl@ tags` command. Satisfied by *secrets.Store; nil
	// disables both.
	Tags SandboxTagger
	// XtermSubdomain is the label the browser terminal is served under
	// ("xterm"), used only so a hung-up terminal tab is told a URL rather than
	// an ssh command. Empty is fine on a host that serves no terminal.
	XtermSubdomain string
	// Nodes, if set, is the fleet's node roster and opens the `node@` door.
	// Nil keeps it shut, which is a single-box deployment.
	Nodes NodeRoster
	// NodeJoiner is what owns an admitted link. Satisfied by *fleet.Fleet; the
	// door stays shut without it, since a link nothing serves is worse than no
	// door at all.
	NodeJoiner NodeJoiner
	// NodeEnrol lets an unknown key at the node door record itself as pending,
	// the way an unknown key at the signup door may register. It is on by
	// default in cmd/sparkbox because an operator still has to approve the row
	// before it can carry anything.
	NodeEnrol bool
	// Ops, if set, is the control-plane core this gateway drives; nil builds one
	// from the stores above. The integrator passes its own so that the REST API,
	// the browser terminal and this channel share a single Ops — one job
	// registry, one audit logger, one lifetime to Close.
	Ops *ctlops.Ops
}

// SandboxTagger is the slice of the secrets store the gateway needs to read and
// stamp a sandbox's tags. Tags select which of an owner's secrets get pushed
// into that sandbox's environment, so setting them is a privileged operation
// the gateway only ever performs for the caller's own boxes.
type SandboxTagger interface {
	TagsFor(sandbox string) ([]string, error)
	SetTags(sandbox, owner string, tags []string) error
}

func New(opts GatewayOptions) *Gateway {
	g := &Gateway{
		ops: opts.Ops, users: opts.Users, log: opts.Logger,
		hostKey: opts.HostKey, upstreamKey: opts.UpstreamKey,
		dial:        opts.Dial,
		dialTimeout: 15 * time.Second,
		doors:       opts.Doors, domain: opts.Domain, sshHost: opts.SSHHost,
		xtermSubdomain:  opts.XtermSubdomain,
		openSignup:      opts.OpenSignup,
		nodes:           opts.Nodes,
		joiner:          opts.NodeJoiner,
		nodeEnrol:       opts.NodeEnrol,
		admissionBudget: nodeAdmissionBudget,
	}
	// Both assignments go through a nil check for the reason opsConfig
	// documents below: a nil *host.Manager stored in an interface field is not
	// a nil interface, so an unset store would arrive as something that looks
	// present and panics on first use.
	if opts.Manager != nil {
		g.mgr = opts.Manager
	}
	if opts.Fleet != nil {
		g.mgr = opts.Fleet
	}
	if g.ops == nil {
		g.ops = ctlops.New(opsConfig(opts))
	}
	return g
}

// opsConfig builds a control plane from the gateway's own stores, for callers
// that have not built one themselves.
//
// Every optional store is checked against nil before it is assigned, which is
// not paperwork: assigning a nil *schedule.Store to an interface field yields a
// non-nil interface holding a nil pointer, and ctlops decides whether a feature
// exists by comparing that field to nil. Skipping the check would turn "this
// host has no scheduler" from a polite refusal into a panic on the first
// `schedule list`.
func opsConfig(opts GatewayOptions) ctlops.Config {
	cfg := ctlops.Config{
		DefaultImage:   opts.DefaultImage,
		Domain:         opts.Domain,
		XtermSubdomain: opts.XtermSubdomain,
		InvitesPerUser: opts.InvitesPerUser,
		Log:            opts.Logger,
	}
	if opts.Manager != nil {
		cfg.Sandboxes, cfg.Templates = opts.Manager, opts.Manager
	}
	// The fleet wins wherever both are set, for the same reason it wins in New
	// and the reason FleetSandboxes asks for the control-plane methods at all:
	// a control plane reading the local manager while lookups go through the
	// fleet is a sandbox that resumes on one machine and cannot be paused from
	// the other.
	if opts.Fleet != nil {
		cfg.Sandboxes, cfg.Templates = opts.Fleet, opts.Fleet
	}
	if opts.Users != nil {
		cfg.Accounts = opts.Users
	}
	if opts.Tags != nil {
		cfg.Tags = opts.Tags
	}
	if opts.Schedules != nil {
		cfg.Schedules = opts.Schedules
	}
	if opts.Routes != nil {
		cfg.Routes = opts.Routes
	}
	if opts.Session != nil {
		cfg.Sessions = opts.Session
	}
	// cfg.Nodes is deliberately not filled in from opts.Nodes. The `ctl@ node`
	// commands need the roster joined to the live fleet — which machine is
	// answering, and what removing it would strand — and no store can produce
	// that join alone. Whoever builds it (cmd/sparkbox) passes its own Ops.
	return cfg
}

// Server builds the gliderlabs/ssh server; callers run Serve/ListenAndServe.
//
// It sets no IdleTimeout and no MaxTimeout. That absence is what lets a node's
// link live for weeks — gliderlabs enforces both per session, so either one
// would cut every linked machine loose on a schedule — and a test pins it. The
// bound a machine the gateway has *not* approved lives under is armed
// per-connection instead, by g.probate below, so it can never reach a link the
// operator blessed.
func (g *Gateway) Server(addr string) *gssh.Server {
	srv := &gssh.Server{
		Addr:    addr,
		Handler: g.handle,
		// The raw connection is stashed before the handshake because it is the
		// only thing that can hang up on a peer which authenticates and then
		// does nothing at all. Nothing reads it unless the node door arms the
		// watchdog below, so this costs a map entry and changes no behaviour.
		ConnCallback: func(ctx gssh.Context, conn net.Conn) net.Conn {
			ctx.SetValue(rawConnKey, conn)
			return conn
		},
		PublicKeyHandler: func(ctx gssh.Context, key gssh.PublicKey) bool {
			ctx.SetValue(authedKeyKey, key)
			if user, ok := g.users.Lookup(key); ok {
				ctx.SetValue(authedUserKey, user)
				return true
			}
			// A machine's key is resolved after a user's, so a key that is both
			// is its user everywhere except at this door. Status is not checked
			// here but in handleNodeLink: a pending node has to get far enough
			// to be told that it is pending.
			if g.nodeDoorOpen() && g.isNodeDoor(ctx.User(), ctx.LocalAddr()) {
				if n, ok := g.nodes.Lookup(key); ok {
					ctx.SetValue(authedNodeKey, n)
					// Approval is what buys an unbounded connection. Anything
					// else here is a machine that is about to be refused, and a
					// refused machine must not be able to hold the door open by
					// simply never letting go of it.
					if n.Status != nodes.StatusApproved {
						g.probate(ctx)
					}
					return true
				}
				// An unknown key at the node door may enrol and do nothing
				// else, exactly as an unknown key at the signup door may
				// register and nothing else. With enrolment refused it falls
				// through to the same rejection, and the same log line, as an
				// unknown key anywhere else.
				if g.nodeEnrol {
					g.probate(ctx)
					return true
				}
			}
			// An unregistered key gets through to the signup door and nowhere
			// else: registering it is precisely what that door is for. Every
			// other path still requires a key the store already knows.
			if g.isSignupDoor(ctx.User(), ctx.LocalAddr()) {
				return true
			}
			g.log.Warn("rejected unknown key", "remote", ctx.RemoteAddr(),
				"fp", xssh.FingerprintSHA256(key))
			return false
		},
	}
	srv.AddHostKey(g.hostKey)
	return srv
}

func (g *Gateway) handle(s gssh.Session) {
	user, _ := s.Context().Value(authedUserKey).(string)
	sandboxName := s.User()

	// Hostname routing: when the client dialed a sandbox's front-door IPv6
	// (ssh myvm.<domain>), the destination address names the target and the
	// SSH username is not consulted for routing. Outside the front-door range
	// (or when doors are disabled) the username carries the name, as ever.
	viaDoor := false
	if name, ok, inRange := g.resolveDoor(s.LocalAddr()); inRange {
		if !ok {
			log := g.log.With("user", user, "dest", s.LocalAddr().String(), "remote", s.RemoteAddr())
			fail(s, log, "lookup", fmt.Errorf("no sandbox at this address — create one with: ssh new.%s", g.domainHint()))
			return
		}
		sandboxName = name
		viaDoor = true
	}

	sandboxName, requestedName := splitNewName(sandboxName)
	log := g.log.With("user", user, "sandbox", sandboxName, "remote", s.RemoteAddr())

	if sandboxName == SignupUser {
		g.handleSignup(s, user, log)
		return
	}
	if sandboxName == ControlUser {
		g.handleControl(s, user, log)
		return
	}
	// Before the handle guard below, deliberately: a node is a machine and has
	// no account, so requiring one here would refuse every link.
	if sandboxName == NodeUser && g.nodeDoorOpen() {
		g.handleNodeLink(s, log)
		return
	}
	// Only the signup door admits an unregistered key; anything else reaching
	// here without a handle is a bug rather than a user error.
	if user == "" {
		fail(s, log, "authenticate", fmt.Errorf("this key isn't registered — run: ssh %s@%s", SignupUser, g.sshHint()))
		return
	}

	// The budget for STARTING the sandbox, and only that. The env delivery and
	// the shell dial take their own below, off the session context rather than
	// off this one: three sequential waits sharing a single 15-second deadline
	// means whichever runs first can spend the whole thing, and the ones after
	// it fail instantly with a deadline they were never given a chance to use.
	// A cold create leaves little of it, so the shell dial — the thing the user
	// actually asked for — was the step being funded last and out of change.
	// The browser terminal has always given its three steps three budgets
	// (internal/xterm.Handler.serve); this is that, at the door that carries
	// the traffic.
	ctx, cancel := context.WithTimeout(s.Context(), g.dialTimeout)
	defer cancel()

	viaNewDoor := sandboxName == NewSandboxUser
	if viaNewDoor {
		// `ssh new@<gateway> ml prod` stamps the box before it exists. Bare words
		// are taken as tags, not just --tag: the local ssh client swallows
		// leading-dash arguments as its own options, so `ssh new@host --tag ml`
		// never reaches us without an `ssh new@host -- --tag ml` incantation.
		// This door takes no positional arguments — a chosen name arrives in the
		// username instead (splitNewName), precisely so it can't be mistaken for
		// a tag — so every bare word here is unambiguously a tag.
		//
		// Which is exactly why --node has to be understood HERE and cannot be
		// left to fall through: an unrecognised flag is not refused at this door,
		// it is quietly turned into two tags. See parseCreateArgs.
		flagged, node, bare, err := parseCreateArgs(s.Command())
		if err != nil {
			failUsage(s, log, err)
			return
		}
		tags, err := ctlops.NormalizeTags(append(flagged, bare...))
		if err != nil {
			failUsage(s, log, err)
			return
		}
		// ctlops.Create is the whole creation: it names the box when the caller
		// didn't, stamps the tags before Create (the secret-env push is fired
		// asynchronously and the tags decide its contents) and clears them again
		// if the create fails.
		box, err := g.ops.Create(ctx, caller(s, user), ctlops.CreateArgs{Name: requestedName, Tags: tags, Node: node})
		if err != nil {
			g.failStart(s, log, "create sandbox", err)
			return
		}
		tagNote := ""
		if len(tags) > 0 {
			tagNote = fmt.Sprintf(" [tags: %s]", strings.Join(tags, ", "))
		}
		// Where it landed, and only when the caller asked: on a single-box
		// deployment there is one answer and printing it every time would be
		// noise, but a user who named a machine is owed confirmation from the
		// record rather than from their own request.
		if node != "" && box.Node != "" {
			tagNote = " on " + box.Node + tagNote
		}
		via := viaGateway
		if viaDoor {
			via = viaFrontDoor
		}
		fmt.Fprintf(s.Stderr(), "sparkbox: created sandbox %q%s — reconnect with: %s\r\n",
			box.Name, tagNote, g.reconnectHint(box.Name, via))
		// The tags-mean-no-PTY caveat, said at the one moment it can be acted
		// on. See noTerminalNotice.
		_, _, isPty := s.Pty()
		fmt.Fprint(s.Stderr(), noTerminalNotice(s.RawCommand(), isPty, g.reconnectHint(box.Name, via)))
		// One line, once, to an account that has never linked GitHub. This door
		// is where the traffic is — it is the only one a user is guaranteed to
		// come back to — and a first sandbox is the moment the offer is worth
		// something, since a linked account is what lets a workload identity
		// token carry `github` and what makes `keys import-github` work. It
		// goes on stderr beside the create banner and never in front of the
		// shell that follows.
		g.nudgeGitHub(s, s.Stderr(), caller(s, user))
		// And the other thing a first sandbox is missing. A secret set here
		// reaches this box immediately: it was just created with the default
		// tag, which is the tag an untagged secret gets.
		g.nudgeAgentToken(s, s.Stderr(), caller(s, user))
		sandboxName = box.Name
	}

	box, ok := g.mgr.Get(sandboxName)
	if !ok {
		fail(s, log, "lookup", fmt.Errorf("no sandbox named %q", sandboxName))
		return
	}
	if box.Owner != user {
		// Same message as not-found: don't leak other users' sandbox names.
		fail(s, log, "lookup", fmt.Errorf("no sandbox named %q", sandboxName))
		return
	}

	// Whether this connection is about to START the VM, read before Prepare
	// resumes it and thereby destroys the evidence. Both of the paths it covers —
	// a create through the new door, a resume of anything not already running —
	// fire an asynchronous secret-env push that the session below must not
	// out-run. A warm box already has its environment; making every reconnect
	// wait on a redundant SSH exec into the guest would be a cost for nothing.
	starting := viaNewDoor || box.State != vmm.StateRunning

	// Resume-on-connect: the user perceives an always-on machine; suspended
	// sandboxes cost only disk.
	box, err := host.Prepare(ctx, g.mgr, sandboxName)
	if err != nil {
		g.failStart(s, log, "resume", err)
		return
	}
	defer g.mgr.MarkActive(sandboxName)
	// Record which of the owner's machines is driving this sandbox; it rides
	// into the id token as `key_fp` for auditing.
	g.mgr.RecordKey(sandboxName, sessionKeyFP(s))

	// Register before dialling so a pause racing this connection still finds the
	// session and closes it, rather than leaving a terminal attached to a VM
	// that is no longer there.
	_, _, isPTY := s.Pty()
	defer g.trackSession(sandboxName, s, isPTY)()

	// The secret environment goes in BEFORE the shell. pam_env reads
	// /etc/environment once, at session setup, so a push that lands a second
	// later lands in a file this session will never read again — which is how
	// `ctl secret set` followed immediately by `ssh new@<gateway>` produced a
	// shell holding none of the secrets it had just been given, and an agent
	// asking the user to log in on the box they had just tokenised.
	if starting {
		g.awaitEnv(s.Context(), s, caller(s, user), sandboxName, log)
	}

	dialCtx, stopDial := context.WithTimeout(s.Context(), g.dialTimeout)
	client, err := g.dialUpstream(dialCtx, box.SSHAddr, box.SSHUser)
	stopDial()
	if err != nil {
		failDial(s, log, sandboxName, err)
		return
	}
	defer client.Close()

	exitCode, err := g.pipeSession(s, client, viaNewDoor)
	if err != nil {
		log.Warn("session ended with error", "err", err)
	}
	s.Exit(exitCode) //nolint:errcheck
}

// envAwaitBudget bounds the wait for a sandbox's secret environment. The push
// dials the guest's sshd, which the session is about to dial anyway, so this
// mostly overlaps a wait the connection was already going to make; the bound is
// there so a guest that never answers costs a delayed shell rather than a hung
// one.
const envAwaitBudget = 30 * time.Second

// awaitEnv delivers the caller's secrets into the sandbox before a session is
// opened on it. Never fatal: a box worth attaching to is worth attaching to
// without its environment, and the next transition to running pushes again.
//
// The failure is reported to the terminal only when the user actually has
// secrets to miss. Everyone else's push writes an empty block, and telling them
// their secrets did not arrive would be inventing a problem they do not have —
// while staying silent for someone who does have them is the exact failure this
// whole path exists to stop being silent about.
func (g *Gateway) awaitEnv(ctx context.Context, s gssh.Session, c ctlops.Caller, name string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, envAwaitBudget)
	defer cancel()
	if err := g.ops.AwaitEnv(ctx, name); err != nil {
		log.Warn("secrets not delivered before the session opened", "name", name, "err", err)
		if metas, lerr := g.ops.ListSecrets(c); lerr == nil && len(metas) > 0 {
			fmt.Fprintf(s.Stderr(), "sparkbox: your secrets have not reached %s yet — reconnect to pick them up.\r\n", name)
		}
	}
}

// resolveDoor maps the connection's destination address to a sandbox name.
// inRange reports whether the address is a front-door address at all; ok
// whether it named a reserved user or an existing sandbox. When the address
// is out of range (or doors are disabled) the caller falls back to routing by
// the SSH username.
func (g *Gateway) resolveDoor(addr net.Addr) (name string, ok, inRange bool) {
	if g.doors == nil {
		return "", false, false
	}
	ta, isTCP := addr.(*net.TCPAddr)
	if !isTCP || !g.doors.Contains(ta.IP) {
		return "", false, false
	}
	// Reserved names first: their doors exist before any sandbox does (and no
	// sandbox may claim them for routing).
	for _, r := range ReservedUsers {
		if g.doors.Addr(r).Equal(ta.IP) {
			return r, true, true
		}
	}
	names := make([]string, 0, 16)
	for _, b := range g.mgr.List() {
		names = append(names, b.Name)
	}
	if n, found := g.doors.Resolve(ta.IP, names); found {
		return n, true, true
	}
	return "", false, true
}

// isSignupDoor reports whether a connection is aimed at the signup door,
// under either routing scheme. It runs during the public-key check, before a
// session exists, which is why it takes the username and destination directly.
func (g *Gateway) isSignupDoor(user string, local net.Addr) bool {
	if name, ok, inRange := g.resolveDoor(local); inRange {
		return ok && name == SignupUser
	}
	return user == SignupUser
}

// sessionKey returns the public key that authenticated this session.
func sessionKey(s gssh.Session) gssh.PublicKey {
	k, _ := s.Context().Value(authedKeyKey).(gssh.PublicKey)
	return k
}

// sessionKeyFP is the fingerprint of the session's key, for the `key_fp` claim.
func sessionKeyFP(s gssh.Session) string {
	if k := sessionKey(s); k != nil {
		return xssh.FingerprintSHA256(k)
	}
	return ""
}

// domainHint is the base domain for user-facing "ssh <x>.<domain>" hints.
func (g *Gateway) domainHint() string {
	if g.domain != "" {
		return g.domain
	}
	return "<domain>"
}

func (g *Gateway) sshHint() string {
	if g.sshHost != "" {
		return g.sshHost
	}
	return g.domainHint()
}

// dialUpstream connects to the VM's sshd with the gateway's upstream key,
// through the fleet dialer when one was supplied.
func (g *Gateway) dialUpstream(ctx context.Context, addr, user string) (*xssh.Client, error) {
	return DialUpstreamVia(ctx, g.dial, addr, user, g.upstreamKey)
}

// handshakeTimeout bounds the SSH handshake on an established connection.
// ClientConfig.Timeout does not: it is handed to ssh.Dial's own net dial and
// is never consulted again, so a peer that accepts the TCP connection and then
// says nothing would otherwise block the handshake read forever.
const handshakeTimeout = 10 * time.Second

// localDialTimeout is one connect attempt's budget for a guest on this
// machine's own network. It is the number the gateway has always used, and the
// retry loop below is built around it: three seconds per try is what lets a
// guest whose sshd is still coming up be re-probed several times inside the
// caller's 15s budget rather than once.
const localDialTimeout = 3 * time.Second

// fleetDialTimeout is one connect attempt's budget for an address the supplied
// dialer resolves itself, which in this tree means opening a stream over
// another machine's reverse tunnel. That crosses the internet before any guest
// is touched, so it gets the room a tap device three hops away does not need.
const fleetDialTimeout = 10 * time.Second

// dialAttemptTimeout picks the per-attempt connect budget from the address.
// RFC 6761 reserves the .invalid TLD precisely so it can never name a real
// host, which makes it the reliable tell: an address in it resolves to nothing
// on this network and only the caller's own dialer knows what it means —
// internal/fleet mints "<sandbox>.<node>.sandbox.invalid" for a guest held by
// another machine. Anything else is an address this machine can dial itself.
//
// The classification is by address rather than by "was a dialer supplied"
// because the fleet dialer is installed on every deployment, single-box
// included, where it falls through to the host network for all but these names.
func dialAttemptTimeout(addr string) time.Duration {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		h = addr
	}
	if strings.HasSuffix(h, ".invalid") {
		return fleetDialTimeout
	}
	return localDialTimeout
}

// DialUpstream connects to a VM's sshd as user, authenticating with key, and
// retries briefly since a freshly resumed/booted VM may not be accepting
// connections yet. It keeps retrying until ctx expires, so callers must pass
// a bounded context. Exported for internal/envsync, whose pushes must dial
// the guest directly rather than ride RunInSandbox (whose first act is
// EnsureRunning, which would wake a paused box).
func DialUpstream(ctx context.Context, addr, user string, key xssh.Signer) (*xssh.Client, error) {
	return DialUpstreamVia(ctx, nil, addr, user, key)
}

// DialUpstreamVia is DialUpstream with the TCP dial supplied by the caller, so
// a sandbox held by another machine can be reached through that machine's
// tunnel instead of the host network. dial is called afresh on every retry;
// nil dials the host network directly.
//
// The gateway used to provision every VM and own the only route to it, which
// is why the host key is ignored. Once another machine sits in the path it can
// impersonate its own guests — accepted, because a machine that lies can only
// lie about the sandboxes it already runs — but the old premise that there is
// no other route no longer holds.
func DialUpstreamVia(ctx context.Context, dial Dialer, addr, user string, key xssh.Signer) (*xssh.Client, error) {
	cfg := &xssh.ClientConfig{
		User: user,
		Auth: []xssh.AuthMethod{xssh.PublicKeys(key)},
		// The gateway provisions the VM and owns the only route to it; there
		// is no prior host key to verify against on first boot.
		HostKeyCallback: xssh.InsecureIgnoreHostKey(), //nolint:gosec
		// Timeout is left unset on purpose. It bounds ssh.Dial's own net dial,
		// and we dial ourselves so the caller's ctx reaches the connect — an
		// improvement over the net.DialTimeout this used to call, which no
		// cancellation could interrupt. The two budgets that do apply are
		// dialAttemptTimeout on the connect and handshakeTimeout on the
		// handshake, neither of which ClientConfig can express.
	}
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	// The connect budget belongs to one attempt, not to the loop: the caller's
	// ctx bounds how long we keep retrying a booting guest, so arming the whole
	// thing with it would let a single wedged connect eat the lot. Cancelling
	// right after the call is safe because a Dialer's ctx governs the dial and
	// not the connection it returns, which is net.Dialer.DialContext's contract
	// and the one the fleet dialer documents keeping.
	attempt := dialAttemptTimeout(addr)
	var lastErr error
	for {
		dialCtx, cancelDial := context.WithTimeout(ctx, attempt)
		conn, err := dial(dialCtx, "tcp", addr)
		cancelDial()
		if err == nil {
			_ = conn.SetDeadline(handshakeDeadline(ctx))
			c, chans, reqs, err := xssh.NewClientConn(conn, addr, cfg)
			if err == nil {
				_ = conn.SetDeadline(time.Time{})
				return xssh.NewClient(c, chans, reqs), nil
			}
			conn.Close()
			lastErr = err
		} else {
			lastErr = err
			// Retrying exists because a freshly booted guest's sshd is not up
			// yet, which is something only the machine holding the guest can
			// tell us. A refusal that is a final ANSWER must not be retried,
			// so hammering it for the rest of the dial budget only delays the
			// message the user is going to get anyway — but a refusal that
			// merely means "not yet" is the very condition this loop exists
			// for, whichever machine it comes from. See retryableRefusal.
			//
			// A typed *ctlops.Error is a final answer said in this tree's own
			// vocabulary: the fleet dialer answers one when the machine holding
			// the sandbox is offline, or when this build cannot carry a
			// connection to another machine at all. Neither becomes true again
			// inside a 15-second dial budget, and the sentence it carries is the
			// one the user needs — so it is delivered at once rather than after
			// a quarter-minute of silence.
			var typed *ctlops.Error
			if errors.As(err, &typed) {
				return nil, fmt.Errorf("vm ssh not reachable: %w", err)
			}
			var oce *xssh.OpenChannelError
			if errors.As(err, &oce) && !RetryableRefusal(oce) {
				return nil, fmt.Errorf("vm ssh not reachable: %w", err)
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("vm ssh not reachable: %w", lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// RetryableRefusal reports whether a node's channel-open rejection means "not
// yet" rather than "no".
//
// Every caller of DialUpstreamVia is dialing ONE port — the guest's sshd — on a
// VM it has just started, so the two are not the same answer and the difference
// is the whole reason the retry loop exists. On a single box that distinction
// arrives as a plain ECONNREFUSED from the host's own network stack and the
// loop handles it without being told anything. In a fleet the same refusal is
// tunnelled: the node dials the guest, gets ECONNREFUSED because sshd has not
// finished starting, and reports it as an SSH channel rejection with reason
// ConnectionFailed (internal/nodelink.serveStream). Reading every rejection as
// final made a remote sandbox's FIRST attach fail roughly one second after its
// create returned — and the same misreading silently cost it the secret-env
// push, which is fired once per transition to running and would not be tried
// again until the box next paused and resumed. Observed live on the CKS
// deployment: box created 16:03:22.2, refused 16:03:23.2, guest reachable
// 16:03:33.5; the user's shell came up nine seconds before it had to, without
// the token it had been given, and nothing said why.
//
// The reasons the node uses for a genuine answer stay final:
//   - Prohibited — "unknown sandbox" (a placement fault) or "sandbox not
//     running" (a state the caller must resume, not outwait).
//   - UnknownChannelType, ResourceShortage — this link cannot carry the stream
//     at all, or the node is already at its stream limit; neither is a boot
//     that has yet to finish.
//
// ConnectionFailed also covers a malformed stream request, which retrying
// cannot fix. That is a gateway bug that has never fired, it costs the dial
// budget rather than correctness, and reason is a far sturdier thing to switch
// on than another package's message strings.
func RetryableRefusal(oce *xssh.OpenChannelError) bool {
	return oce.Reason == xssh.ConnectionFailed
}

// handshakeDeadline is the earlier of the handshake budget and whatever the
// caller has left, so a stalled peer never outlives the request that wanted it.
func handshakeDeadline(ctx context.Context) time.Time {
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		return d
	}
	return deadline
}

// splitNewName reads `new+myvm` as "the new@ door, and call it myvm", returning
// the routing name and the requested sandbox name ("" when none was asked for).
//
// The name rides in the username rather than the arguments because names and
// tags are character-identical (both `[a-z0-9-]`), so a positional word could
// never be told apart from a tag — `ssh new@host ml` is genuinely ambiguous. Nor
// can a flag carry it: ssh(1) rejects `--name` as its own bad option, and
// silently reads `-name` as `-n -a -m -e`, quietly doing something else entirely.
// A front-door name comes from DNS and cannot contain '+', so this only ever
// fires on a username.
func splitNewName(user string) (routeTo, requested string) {
	if rest, found := strings.CutPrefix(user, NewSandboxUser+"+"); found {
		return NewSandboxUser, rest
	}
	return user, ""
}

// execsCommand reports whether a session should run the client's command inside
// the guest rather than opening a shell.
//
// tagsOnly is the `new@` door, whose arguments parseTags already consumed as
// tags. Running them a second time as a guest command gives every tag an
// accidental second meaning: `ssh new@host claude` both tagged the box and
// launched claude, and `ssh new@host ml` would tag it and then die on `ml:
// command not found`. A freshly created sandbox always gets a shell.
//
// Caveat we document rather than paper over: passing tags means the client's
// ssh saw a command word and allocated no PTY, so the shell we open here runs
// non-interactively — no prompt, no line editing. Callers want `ssh -t`.
func execsCommand(raw string, tagsOnly bool) bool { return raw != "" && !tagsOnly }

// noTerminalNotice is the sentence for a `new@` session that carried tag words
// and therefore has no PTY, or "" when there is nothing to say.
//
// execsCommand documents why a tag word is not run as a command. What it cannot
// fix is that the client's ssh decided, before it ever reached this server, that
// a command word means no terminal — PTY allocation is the CLIENT's request, and
// a server cannot ask for one after the fact. So the shell opened below is live
// and does exactly what is typed into it; it just prints no prompt and echoes
// nothing, which reads as a hang. The last line on screen before that silence is
// whatever the banner said, so users conclude the thing named there is stuck —
// a repo checkout, most often, since that is the slowest line the banner has.
//
// One line, and it turns a mute terminal into a typo.
func noTerminalNotice(raw string, isPty bool, hint string) string {
	if isPty || raw == "" {
		return ""
	}
	// reconnectHint renders a whole ssh(1) invocation, `ssh ` included, so -t
	// is spliced into the command it already built rather than prefixed to it —
	// prefixing produced `ssh -t ssh name@host`, which is a hint that does not
	// run. Anything not shaped like an ssh command (the browser URL form) is
	// printed as it came: a URL with -t bolted on would be worse than no hint.
	fix := hint
	if rest, ok := strings.CutPrefix(hint, "ssh "); ok {
		fix = "ssh -t " + rest
	}
	return fmt.Sprintf(
		"sparkbox: no terminal — your ssh client read %q as a command, so it allocated no PTY.\r\n"+
			"          The shell below is live but has no prompt and no echo. For an interactive one:\r\n"+
			"              %s\r\n", raw, fix)
}

// pipeSession mirrors the client's session (PTY, env, command, window
// changes, streams) onto a session inside the VM and returns the exit code.
// tagsOnly suppresses command execution for callers whose arguments carry a
// different meaning than "run this in the guest" — see the `new@` door.
func (g *Gateway) pipeSession(s gssh.Session, client *xssh.Client, tagsOnly bool) (int, error) {
	up, err := client.NewSession()
	if err != nil {
		return 1, err
	}
	defer up.Close()

	for _, kv := range s.Environ() {
		if k, v, ok := splitEnv(kv); ok {
			up.Setenv(k, v) //nolint:errcheck // VM sshd may restrict AcceptEnv
		}
	}

	ptyReq, winCh, isPty := s.Pty()
	if isPty {
		modes := xssh.TerminalModes{xssh.ECHO: 1}
		if err := up.RequestPty(ptyReq.Term, ptyReq.Window.Height, ptyReq.Window.Width, modes); err != nil {
			return 1, err
		}
		go func() {
			for win := range winCh {
				up.WindowChange(win.Height, win.Width) //nolint:errcheck
			}
		}()
	}

	stdin, err := up.StdinPipe()
	if err != nil {
		return 1, err
	}
	stdout, err := up.StdoutPipe()
	if err != nil {
		return 1, err
	}
	stderr, err := up.StderrPipe()
	if err != nil {
		return 1, err
	}
	go func() {
		io.Copy(stdin, s) //nolint:errcheck
		stdin.Close()
	}()
	done := make(chan struct{}, 2)
	go func() { io.Copy(s, stdout); done <- struct{}{} }()          //nolint:errcheck
	go func() { io.Copy(s.Stderr(), stderr); done <- struct{}{} }() //nolint:errcheck

	if raw := s.RawCommand(); execsCommand(raw, tagsOnly) {
		err = up.Start(raw)
	} else {
		err = up.Shell()
	}
	if err != nil {
		return 1, err
	}
	err = up.Wait()
	<-done
	<-done
	if err == nil {
		return 0, nil
	}
	if exitErr, ok := err.(*xssh.ExitError); ok {
		return exitErr.ExitStatus(), nil
	}
	return 1, err
}

func fail(s gssh.Session, log *slog.Logger, what string, err error) {
	log.Error(what+" failed", "err", err)
	fmt.Fprintf(s.Stderr(), "sparkbox: %s failed: %v\r\n", what, err)
	s.Exit(1) //nolint:errcheck
}

// ---------------------------------------------------------------------------
// A failed dial is the one error on this path that is never printed as it came
// ---------------------------------------------------------------------------
//
// Every other failure here is a sentence somebody wrote. A dial error is
// composed by whatever did the dialing, and what it names is an ADDRESS: on
// this machine `dial tcp 172.30.5.2:22: connect: connection refused`, and for a
// sandbox held by another machine the synthetic
// `<sandbox>.<node>.sandbox.invalid` the fleet dialer resolves. Neither is
// something a user can act on — the first is a bridge address on a host they
// have no shell on, the second resolves nowhere by design — and the second is
// fleet topology besides.
//
// It matters more than the wording suggests, because this path does not only
// write to a terminal. RunInSandbox is the scheduler's exec runner, and the
// error it returns is stored verbatim in the job's `last_error` column and
// rendered by `ctl schedule list` and by both consoles. An address that reaches
// there is persisted, not just glimpsed.
//
// A TYPED refusal is the exception and is printed in full: fleet's node-offline
// sentence is curated, names no address, and is the only useful thing anybody
// can say about a machine that is not answering. Telling an owner "your sandbox
// is on a machine that is offline" is exactly the point of having the type.

// dialError is a failed guest dial rendered for a reader, with the cause still
// reachable for errors.As and for a log line that wants it.
type dialError struct {
	sandbox string
	msg     string
	cause   error
}

func (e *dialError) Error() string { return "dial " + e.sandbox + ": " + e.msg }
func (e *dialError) Unwrap() error { return e.cause }

// dialFailure wraps a dial error for a caller that will store or display the
// sentence. See the section comment above.
func dialFailure(sandbox string, err error) error {
	var typed *ctlops.Error
	if errors.As(err, &typed) {
		return &dialError{sandbox: sandbox, msg: typed.Msg, cause: err}
	}
	return &dialError{sandbox: sandbox, msg: unreachableShell, cause: err}
}

// unreachableShell is what a user is told when the dial failed for a reason
// that only an operator's log can usefully carry.
const unreachableShell = "could not reach the sandbox's shell; it may still be starting"

// failDial answers an interactive session whose sandbox's sshd could not be
// reached. The cause reaches the log; the terminal gets a sentence.
func failDial(s gssh.Session, log *slog.Logger, sandbox string, err error) {
	var typed *ctlops.Error
	if errors.As(err, &typed) {
		// A curated refusal, rendered by the one function that knows whether a
		// ctlops sentence stands alone or is wrapped — the same rendering the
		// ctl@ door gives the identical error, so an offline machine reads the
		// same way whichever door you knocked on.
		failCtl(s, log, "dial vm", typed)
		return
	}
	log.Error("dial vm failed", "sandbox", sandbox, "err", err)
	fmt.Fprintf(s.Stderr(), "sparkbox: %s\r\n", unreachableShell)
	s.Exit(1) //nolint:errcheck
}

// failUsage answers a malformed invocation: the command never ran, so this is
// not a failure of anything and does not read as one.
//
// Exit 2 rather than 1, which is what the ctl@ door has always used for a
// mistyped command (see control.go) and what ctlops.KindInvalid renders as
// everywhere else. This door used to exit 1 for a bad --tag, which was the odd
// one out; nothing pinned it, and a user who cannot tell "you typed it wrong"
// from "it broke" retries instead of reading.
func failUsage(s gssh.Session, log *slog.Logger, err error) {
	log.Info("bad arguments", "err", err)
	fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
	s.Exit(2) //nolint:errcheck
}

// failStart is fail plus friendly, actionable guidance for the resource-limit
// errors, which are expected user-facing conditions rather than faults.
func (g *Gateway) failStart(s gssh.Session, log *slog.Logger, what string, err error) {
	var limit *host.LimitError
	if errors.As(err, &limit) {
		log.Info("start refused: per-owner limit", "running", limit.Running)
		fmt.Fprintf(s.Stderr(),
			"sparkbox: you already have %d running sandboxes (max %d)%s\r\n",
			len(limit.Running), limit.Max, namesOrNothing(limit.Running))
		// The example only exists when there is a name to put in it. Nothing
		// reaches here with an empty set today: host.Manager.admit only mints a
		// LimitError once it has collected at least one running name, and
		// ctlops.hostFromWire drops the typed cause entirely when a node's
		// refusal arrives with no name that survives scrubbing. This is defence
		// in depth behind those two, not the thing standing between a node and a
		// panicked session goroutine — that was closed at the wire.
		if len(limit.Running) > 0 {
			fmt.Fprintf(s.Stderr(), "Pause one to free a slot, e.g.:  ssh %s@%s pause %s\r\n",
				ControlUser, g.sshHint(), limit.Running[0])
		} else {
			fmt.Fprintf(s.Stderr(), "Pause one to free a slot:  ssh %s@%s list\r\n",
				ControlUser, g.sshHint())
		}
		s.Exit(1) //nolint:errcheck
		return
	}
	var capacity *host.CapacityError
	if errors.As(err, &capacity) {
		log.Info("start refused: host at capacity", "used_mb", capacity.UsedMB, "budget_mb", capacity.BudgetMB)
		fmt.Fprintf(s.Stderr(),
			"sparkbox: host is at capacity (%d/%d MB allocated). Try again shortly, or pause a sandbox:  ssh %s@%s list\r\n",
			capacity.UsedMB, capacity.BudgetMB, ControlUser, g.sshHint())
		s.Exit(1) //nolint:errcheck
		return
	}
	// A curated sentence the router already wrote — chiefly "sandbox %q lives
	// on node %q, which is offline" — is printed as it stands rather than
	// wrapped in "resume failed: …". The Verbatim flag is the error's own claim
	// that it is already the whole sentence, and honouring it here is what
	// makes an offline machine read identically at this door and at ctl@.
	//
	// Only Verbatim errors are diverted, so nothing a single-box deployment can
	// produce takes this branch: the manager raises host.* error types, none of
	// which is a *ctlops.Error at all.
	var typed *ctlops.Error
	if errors.As(err, &typed) && typed.Verbatim {
		failCtl(s, log, what, typed)
		return
	}
	fail(s, log, what, err)
}

// namesOrNothing renders a sandbox list as ": a, b" or as nothing at all, so
// the sentence above reads properly either way rather than trailing a colon
// into empty space.
func namesOrNothing(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return ": " + strings.Join(names, ", ")
}

func splitEnv(kv string) (string, string, bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}
	return "", "", false
}
