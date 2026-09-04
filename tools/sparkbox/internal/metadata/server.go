// Package metadata is the per-sandbox identity and self-service endpoint: the
// unforgeable channel by which a guest learns who it is, gets an OIDC id token,
// and changes its own lifecycle policy without a credential stored in the VM.
// Possession of the network position is the authentication — the same model as
// a cloud provider's IMDS.
//
// # How a caller is identified (and why this is safe)
//
// Each guest sits on a point-to-point /30 with the host at offset 1 and the
// guest at offset 2. A request is attributed to the sandbox whose recorded
// guest IP equals the connection's SOURCE address, and is refused unless the
// DESTINATION is that same slot's host address — a guest may only ask its own
// gateway.
//
// Source, not destination, is the identity. Destination is attacker-chosen:
// the host has IP forwarding on and Linux's weak host model accepts a packet
// for any local address on any interface, so a guest in slot 5 can simply
// connect to another slot's host address and have the kernel deliver it — the
// accepted socket's local address would then name the other slot. Source
// cannot be forged the same way: this is TCP, and a SYN with a
// spoofed source is answered by a SYN-ACK routed to the *real* owner of that
// address (a different tap), so the spoofer never completes the handshake.
// The firecracker driver additionally sets rp_filter on each tap, which drops
// such packets outright rather than merely leaving them unanswered.
//
// # Where the signing happens (and why that is a separate question)
//
// Everything above is about the HOST end of a tap, and every sparkbox host has
// one: a fleet node holds VMs exactly as a gateway does, and a guest on it
// reaches its own gateway address the same way. What a node does NOT hold is
// the fleet's OIDC signing key, which lives on the gateway and must stay there
// — one key, one issuer, one JWKS.
//
// So this package answers "which sandbox is asking" locally, on every host, and
// asks Identity to turn that sandbox into a token. On a gateway Identity is
// Local, which signs in-process. On a node it is a relay over the fleet link:
// the node names the sandbox, and the gateway decides everything about it from
// its own placement ledger — see internal/fleet's identity handling for the one
// check that makes that safe.
package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestdocs"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// DefaultPort is the metadata service's port on each tap host address. It is
// reachable only from the guest on the other end of that tap.
const DefaultPort = 8967

// rateWindow / rateBurst bound how often one sandbox may mint tokens. The
// guest refreshes every 45 minutes; this leaves ample room for retries and
// manual pokes while capping a compromised guest's ability to farm tokens.
const (
	rateWindow = time.Minute
	rateBurst  = 10
)

// Sandboxes is the slice of the host manager this service needs: resolving a
// guest address to the sandbox that holds it. *host.Manager satisfies it.
type Sandboxes interface {
	GetByHostIP(ip string) (*host.Sandbox, bool)
	SetPinned(name string, pinned bool) error
}

// RepoStatusSink accepts an advisory checkout snapshot from the guest. It is
// separate from Sandboxes so small identity-only test doubles and deployments
// do not acquire a mutation they do not need.
type RepoStatusSink interface {
	SetRepoStatus(name string, repos []host.RepoStatus, at time.Time) error
}

// VitalsReader supplies the same live counters used by the xterm instrument
// strip. It is optional so identity-only metadata deployments remain small.
type VitalsReader interface {
	Vitals(context.Context, string) (host.Vitals, error)
}

// Accounts is the slice of the user store this service needs: the owner's
// verified external identity. *users.Store satisfies it.
type Accounts interface {
	Get(handle string) (users.User, error)
}

// Token is one minted id token and when it stops being valid.
type Token struct {
	JWT       string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Doc is what /run/sparkbox/identity.json holds in the guest: the same claims a
// token carries, minus the registered ones, so shells and tools can answer "who
// am I" without parsing a JWT.
type Doc struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Owner     string `json:"owner"`
	GitHub    string `json:"github,omitempty"`
	KeyFP     string `json:"key_fp,omitempty"`
	Sandbox   string `json:"sandbox"`
	SandboxID string `json:"sandbox_id"`
	Image     string `json:"image"`
	Box       string `json:"box"`
	// GitHubID is GitHub's immutable account number for the owner, on the same
	// terms as GitHub above and 0 when that is empty. It is here and not in the
	// token because nothing federates on it: the guest needs it to write the
	// `<id>+<login>@users.noreply.github.com` address that attributes a commit,
	// and a relying party matching on an account number instead of the login
	// this platform actually proved would be reading the wrong fact.
	GitHubID int64 `json:"github_id,omitempty"`
}

// Identity turns an authenticated sandbox into credentials. It is the whole of
// what differs between a gateway and a node: the caller-identification above is
// identical on both, and only this is signed somewhere else.
//
// Both methods take a context because a node's implementation is a network
// call. Both take the sandbox rather than its name because the local
// implementation needs no lookup — the record is already in hand — and the
// relay deliberately sends only the name, so that nothing a node asserts about
// a sandbox reaches the claims.
type Identity interface {
	Issue(ctx context.Context, box *host.Sandbox, aud string) (Token, error)
	Describe(ctx context.Context, box *host.Sandbox) (Doc, error)
}

// RouteControl changes gateway-owned route state for the already-authenticated
// calling sandbox. A gateway implementation writes locally; a node relays over
// its authenticated fleet link.
type RouteControl interface {
	SetVisibility(ctx context.Context, box *host.Sandbox, visibility string, port int) (RouteVisibility, error)
	SetPort(ctx context.Context, box *host.Sandbox, port int) (RoutePort, error)
}

type RouteVisibility struct {
	Sandbox    string `json:"sandbox"`
	Visibility string `json:"visibility"`
	// Port is the single guest port that changed, or zero when the change was
	// the whole sandbox's.
	Port   int `json:"port,omitempty"`
	Routes int `json:"routes"`
}

type RoutePort struct {
	Sandbox string `json:"sandbox"`
	Port    int    `json:"port"`
}

// ErrAudience is what an Identity returns for an audience its issuer will not
// mint for. It is a distinct error because it is the caller's mistake — a
// guest asked for `?aud=` something this fleet does not federate with — and is
// answered 400, where every other failure here is the host's and is answered
// 500 or 503.
var ErrAudience = errors.New("audience is not allowed by this issuer")

// ErrNoIssuer is what a relay returns when it cannot reach the machine that
// signs. It is answered 503: the guest's own retry — the sparkbox-token timer
// comes back every 45 minutes against a token that lives an hour — is the
// repair, and a 500 would invite it to give up instead.
var ErrNoIssuer = errors.New("the machine that signs id tokens is not reachable")

// Local is the Identity of a host that holds the signing key: it mints in
// process, with no network anywhere on the path.
type Local struct {
	Issuer *oidc.Issuer
	// Users is optional. Nil omits the `github` claim for everyone, which is
	// the same thing an unverified owner gets.
	Users Accounts
	// NodeName is the machine the sandbox runs on, for the `box` claim.
	NodeName string
}

func (l Local) Issue(_ context.Context, box *host.Sandbox, aud string) (Token, error) {
	if !l.Issuer.AudienceAllowed(aud) {
		return Token{}, fmt.Errorf("%w: %q", ErrAudience, aud)
	}
	c := l.claims(box)
	c.Audience = aud
	jwt, exp, err := l.Issuer.Mint(c)
	if err != nil {
		return Token{}, err
	}
	return Token{JWT: jwt, ExpiresAt: exp}, nil
}

func (l Local) Describe(_ context.Context, box *host.Sandbox) (Doc, error) {
	c, id := l.claimsWithGitHubID(box)
	return Doc{
		Issuer: l.Issuer.URL(), Subject: c.Subject, Owner: c.Owner,
		GitHub: c.GitHub, GitHubID: id, KeyFP: c.KeyFP,
		Sandbox: c.Sandbox, SandboxID: c.SandboxID, Image: c.Image, Box: c.Box,
	}, nil
}

// claims assembles the identity of a sandbox: its own facts plus its owner's.
// The `github` claim is present only when the owner verified it, so a policy
// matching on it fails closed for everyone else.
func (l Local) claims(box *host.Sandbox) oidc.Claims {
	c, _ := l.claimsWithGitHubID(box)
	return c
}

// claimsWithGitHubID assembles the claims and the owner's GitHub account number
// from ONE account snapshot.
//
// The single read is the point, not an optimisation. Two reads — one for the
// login, one for the number — can straddle a concurrent relink or rename and
// hand back a document pairing one account's login with another's number, or a
// login with the 0 of an account that no longer has it. Both values are the
// same statement about the same account, so they have to be read as one.
func (l Local) claimsWithGitHubID(box *host.Sandbox) (oidc.Claims, int64) {
	c := oidc.Claims{
		Subject:   oidc.SubjectFor(box.Owner),
		Owner:     box.Owner,
		KeyFP:     box.KeyFP,
		Sandbox:   box.Name,
		SandboxID: box.ID,
		Image:     box.Image,
		Box:       l.NodeName,
	}
	login, id := l.githubOwner(box.Owner)
	c.GitHub = login
	return c, id
}

// githubOwner is the owner's GitHub login and immutable account number, or
// ("", 0) when this host cannot vouch for either. Both come from one account
// read, so a caller that needs both gets a consistent pair.
//
// Strong provenance only, and this is the load-bearing half of the condition
// rather than a refinement of it.
//
// docs/identity-federation-design.md tells relying parties that `github` is a
// strong external anchor — hivemind is invited to write `claims.github ==
// "vanpelt"` in a policy — and that is a promise about where the fact came
// from, not merely that somebody recorded one. A link established by a third
// party's signed word would make this claim mean "somebody said so", and if
// that third party is also the one reading the policy it would be authorizing
// against a fact it asserted. So an `assertion` link is a fine thing for a
// console to display and not a thing an id token may carry. See
// docs/github-linking-design.md.
//
// The number rides the same gate rather than a looser one, because it is the
// same statement: an account number recorded beside an unproved login is no
// better evidence than the login was.
func (l Local) githubOwner(owner string) (login string, id int64) {
	if l.Users == nil {
		return "", 0
	}
	u, err := l.Users.Get(owner)
	if err != nil || u.GitHubVerifiedAt == nil || !users.StrongGitHubLink(u.GitHubVia) {
		return "", 0
	}
	return u.GitHubLogin, u.GitHubID
}

type Server struct {
	mgr            Sandboxes
	id             Identity
	routes         RouteControl
	repoAccess     RepoAccess
	repoAuthorizer RepoAuthorizer
	repoStatus     RepoStatusSink
	vitals         VitalsReader
	tools          ToolCache
	lifecycle      SelfLifecycle
	// envSetup is the environment-build door. Nil answers both /self/setup
	// routes 501, which is what a host with no environment store is. See
	// envsetup.go and SetEnvSetup.
	envSetup EnvSetup
	// allowSelfSnapshot is the operator's kill switch for capture-from-inside.
	// Default on, because the carried-tag restriction already bounds it and a
	// self-service feature nobody is told about does not exist; the flag is for
	// an operator handing boxes to third parties. `pause` is not behind it —
	// see SelfLifecycle.
	allowSelfSnapshot bool
	log               *slog.Logger
	defAud            string
	openAI            OpenAI
	guestNet          guestnet.Network

	mu     sync.Mutex
	recent map[string][]time.Time // sandbox -> mint times inside the window

	// The repo endpoints keep their own window. See credWindow in repos.go for
	// why sharing the one above would cost the guest its identity.
	credMu     sync.Mutex
	credRecent map[string][]time.Time // sandbox+op -> request times inside the window

	// And the lifecycle endpoints keep a third. See allowSelfCall.
	selfMu     sync.Mutex
	selfRecent map[string][]time.Time // sandbox+op -> request times inside the window

	// base is the process's lifetime, stashed by ListenAndServe so a capture
	// that outlives the request that accepted it is still bounded by the life
	// of the service. See ackThenAct.
	baseMu sync.Mutex
	base   context.Context
}

type Options struct {
	Manager Sandboxes
	// Identity signs. Local on a gateway, a relay to one on a node.
	Identity     Identity
	RouteControl RouteControl
	// Repos serves the two repository endpoints. Nil answers both 501, which
	// is what a host with no repos store or no GitHub App is.
	Repos RepoAccess
	// RepoAuthorizer serves the interactive per-repository GitHub user flow.
	// It is separate from Repos so older/minimal hosts retain bot credentials.
	RepoAuthorizer RepoAuthorizer
	// RepoStatus receives the guest's bounded read-only git survey. Nil answers
	// the publish route 501 while leaving manifest and credential reads intact.
	RepoStatus RepoStatusSink
	// Vitals lets sparkbox status render the resource snapshot also exposed by
	// the xterm UI. Nil leaves the stable fields present and live counters absent.
	Vitals VitalsReader
	// Tools serves this machine's own agent-CLI cache. Nil answers both /tools
	// routes 501, which is what a host started without --tools-dir is. It is
	// never a relay to another machine — see ToolCache.
	Tools ToolCache
	// SelfLifecycle runs `sparkbox pause` and `sparkbox snapshot <tag>` for the
	// calling sandbox. Nil answers all three lifecycle routes 501, which is
	// what a host with no control plane on its fleet is.
	SelfLifecycle SelfLifecycle
	// EnvSetup serves the environment-build pair. Nil answers both 501, which
	// is what a host with no environment store is. A deployment whose
	// construction order cannot supply it here uses SetEnvSetup instead.
	EnvSetup EnvSetup
	// AllowSelfSnapshot is the operator's switch for the capture verb alone.
	// The gateway and node wiring both default it to true; `pause` ignores it.
	AllowSelfSnapshot bool
	Logger            *slog.Logger
	// DefaultAudience is used when a caller passes no ?aud=.
	DefaultAudience string
	// OpenAI is this fleet's OpenAI workload-identity federation config, served
	// at /openai. A zero value answers 501 there and leaves guests alone.
	OpenAI OpenAI
	// GuestSubnet must match the VM driver's IPv4 prefix. Empty uses the
	// standalone compatibility default.
	GuestSubnet string
}

func New(opts Options) *Server {
	server, err := NewChecked(opts)
	if err != nil {
		panic(err)
	}
	return server
}

// NewChecked constructs a server and reports an invalid guest subnet. New is
// retained for existing callers that use the compatibility default.
func NewChecked(opts Options) (*Server, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	guestNetwork, err := guestnet.Parse(opts.GuestSubnet)
	if err != nil {
		return nil, err
	}
	return &Server{
		mgr: opts.Manager, id: opts.Identity, routes: opts.RouteControl,
		repoAccess: opts.Repos, repoAuthorizer: opts.RepoAuthorizer, repoStatus: opts.RepoStatus,
		vitals: opts.Vitals,
		tools:  opts.Tools, log: log,
		lifecycle:         opts.SelfLifecycle,
		envSetup:          opts.EnvSetup,
		allowSelfSnapshot: opts.AllowSelfSnapshot,
		defAud:            opts.DefaultAudience,
		openAI:            opts.OpenAI.withDefaults(),
		guestNet:          guestNetwork,
		recent:            map[string][]time.Time{},
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", s.token)
	mux.HandleFunc("GET /identity", s.identity)
	mux.HandleFunc("GET /openai", s.openai)
	mux.HandleFunc("GET /repos", s.repoManifest)
	mux.HandleFunc("POST /repos/status", s.publishRepoStatus)
	mux.HandleFunc("GET /github/credential", s.githubCredential)
	mux.HandleFunc("POST /github/authorization", s.startGithubAuthorization)
	mux.HandleFunc("GET /github/authorization/{id}", s.pollGithubAuthorization)
	// The literal pattern is more specific than the wildcard one, so
	// /tools/manifest can never be routed as a tool named "manifest" — which is
	// also why parseToolManifest refuses that name outright rather than
	// publishing something unreachable.
	//
	// {name} matches exactly one path segment, but a percent-encoded separator
	// still arrives decoded in PathValue. That is not what keeps the cache
	// directory safe: toolFile checks the shape and LocalTools.Open looks the
	// name up among the manifest's own entries, opening the basename that entry
	// stored, so no request ever reaches a filepath.Join.
	mux.HandleFunc("GET /tools/manifest", s.toolManifest)
	mux.HandleFunc("GET /tools/{name}", s.toolFile)
	mux.HandleFunc("GET /self", s.self)
	mux.HandleFunc("POST /self/pin", s.pin)
	mux.HandleFunc("POST /self/unpin", s.unpin)
	mux.HandleFunc("POST /self/visibility/{visibility}", s.visibility)
	mux.HandleFunc("POST /self/port/{port}", s.port)
	// The lifecycle trio. GET and POST on one path is the whole design: the GET
	// is the plan — a pure read carrying every refusal and both warnings — and
	// the POST is the commit that acts on one. See selflifecycle.go.
	mux.HandleFunc("POST /self/pause", s.selfPause)
	mux.HandleFunc("GET /self/snapshot", s.selfSnapshotPlan)
	mux.HandleFunc("POST /self/snapshot", s.selfSnapshotCommit)
	// The environment-build pair. Neither request names a sandbox or an
	// environment: the tap is the identity and the host decides the rest. See
	// envsetup.go.
	mux.HandleFunc("GET /self/setup", s.selfSetup)
	mux.HandleFunc("POST /self/setup/result", s.selfSetupResult)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Same static, unauthenticated content docs.<domain> serves at the edge,
	// mounted here too: docs.<domain> is a public DNS name that can resolve to
	// this fleet's own edge, which a guest's own tap firewall then has no route
	// to reach directly (see sparkbox-net.sh's SPARKBOX_GUEST_HOST — only 53 and
	// this port are open guest-to-host). No caller() check, same as /healthz:
	// the content carries no per-sandbox secret, so anything that can already
	// reach this port may read it.
	mux.Handle("GET /docs/", http.StripPrefix("/docs", guestdocs.Handler()))
	return mux
}

const (
	maxRepoStatusBody = 64 << 10
	maxRepoStatusRows = 64
	maxRepoStatusText = 512
)

// publishRepoStatus accepts the checkout survey produced inside the calling
// guest. The source tap authenticates the sandbox exactly as it does for every
// other metadata route; the body is only advisory state and is bounded before
// any of it reaches durable inventory or a fleet link.
//
// Each line is eight ASCII-US-separated fields:
// slug, path, branch, upstream, ahead, behind, dirty (0/1), state.
func (s *Server) publishRepoStatus(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	if s.repoStatus == nil {
		http.Error(w, "sparkbox: repo status reporting is not enabled", http.StatusNotImplemented)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRepoStatusBody))
	if err != nil {
		http.Error(w, "sparkbox: repo status report is too large", http.StatusRequestEntityTooLarge)
		return
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if len(lines) > maxRepoStatusRows {
		http.Error(w, "sparkbox: repo status report has too many rows", http.StatusBadRequest)
		return
	}
	rows := make([]host.RepoStatus, 0, len(lines))
	for _, line := range lines {
		fields := strings.Split(line, "\x1f")
		if len(fields) != 8 {
			http.Error(w, "sparkbox: malformed repo status row", http.StatusBadRequest)
			return
		}
		for _, field := range fields {
			if len(field) > maxRepoStatusText || hasControl(field) {
				http.Error(w, "sparkbox: invalid repo status field", http.StatusBadRequest)
				return
			}
		}
		if !repos.ValidSlug(fields[0]) || !strings.HasPrefix(fields[1], "/") {
			http.Error(w, "sparkbox: invalid repository or path in status report", http.StatusBadRequest)
			return
		}
		ahead, e1 := strconv.ParseInt(fields[4], 10, 64)
		behind, e2 := strconv.ParseInt(fields[5], 10, 64)
		if e1 != nil || e2 != nil || ahead < 0 || behind < 0 ||
			(fields[6] != "0" && fields[6] != "1") {
			http.Error(w, "sparkbox: invalid repository counters in status report", http.StatusBadRequest)
			return
		}
		switch fields[7] {
		case "ready", "stale", "missing", "failed":
		default:
			http.Error(w, "sparkbox: invalid repository state in status report", http.StatusBadRequest)
			return
		}
		rows = append(rows, host.RepoStatus{
			Slug: fields[0], Path: fields[1], Branch: fields[2], Upstream: fields[3],
			Ahead: ahead, Behind: behind, Dirty: fields[6] == "1", State: fields[7],
		})
	}
	if err := s.repoStatus.SetRepoStatus(box.Name, rows, time.Now()); err != nil {
		s.log.Error("repo status publish failed", "sandbox", box.Name, "err", err)
		http.Error(w, "sparkbox: could not record repo status", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

type selfDoc struct {
	Sandbox        string                     `json:"sandbox"`
	State          vmm.State                  `json:"state"`
	Pinned         bool                       `json:"pinned"`
	AtMS           int64                      `json:"at_ms"`
	VCPUs          int64                      `json:"vcpus"`
	MemMB          int64                      `json:"mem_mb"`
	Turbo          bool                       `json:"turbo,omitempty"`
	Ballooned      bool                       `json:"ballooned,omitempty"`
	CPUSeconds     *float64                   `json:"cpu_seconds,omitempty"`
	MemUsedMB      *int64                     `json:"mem_used_mb,omitempty"`
	NetRxBytes     *uint64                    `json:"net_rx_bytes,omitempty"`
	NetTxBytes     *uint64                    `json:"net_tx_bytes,omitempty"`
	LifeRxBytes    uint64                     `json:"life_rx_bytes,omitempty"`
	LifeTxBytes    uint64                     `json:"life_tx_bytes,omitempty"`
	ListeningPorts []int                      `json:"listening_ports,omitempty"`
	PortServices   []host.PortService         `json:"port_services,omitempty"`
	PortsChecked   bool                       `json:"ports_checked,omitempty"`
	Repositories   map[string]host.RepoStatus `json:"repositories"`
	RepoStatusAt   *time.Time                 `json:"repo_status_at,omitempty"`
	// HiveMind is the host's latest cached session catalog for this VM. It is
	// absent until the optional presence monitor has completed a history query;
	// a non-nil snapshot with no sessions is an authoritative empty result.
	HiveMind *host.HiveMindSessionSnapshot `json:"hivemind,omitempty"`
}

func (s *Server) self(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	if r.URL.Query().Get("format") == "text" {
		s.writeSelfText(w, s.selfStatus(r.Context(), box))
		return
	}
	s.writeSelf(w, s.selfStatus(r.Context(), box))
}

func (s *Server) pin(w http.ResponseWriter, r *http.Request)   { s.setPinned(w, r, true) }
func (s *Server) unpin(w http.ResponseWriter, r *http.Request) { s.setPinned(w, r, false) }

func (s *Server) setPinned(w http.ResponseWriter, r *http.Request, pinned bool) {
	box, err := s.caller(r)
	if err != nil {
		s.log.Warn("metadata self-service refused", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	if err := s.mgr.SetPinned(box.Name, pinned); err != nil {
		s.log.Error("metadata self-service pin failed", "sandbox", box.Name, "pinned", pinned, "err", err)
		http.Error(w, "sparkbox: could not update this sandbox", http.StatusInternalServerError)
		return
	}
	box.Pinned = pinned
	s.log.Info("sandbox changed its own pin", "sandbox", box.Name, "owner", box.Owner, "pinned", pinned)
	s.writeSelf(w, s.selfStatus(r.Context(), box))
}

func (s *Server) selfStatus(ctx context.Context, box *host.Sandbox) selfDoc {
	repositories := make(map[string]host.RepoStatus, len(box.Repos))
	for _, repo := range box.Repos {
		repositories[repo.Slug] = repo
	}
	doc := selfDoc{
		Sandbox: box.Name, State: box.State, Pinned: box.Pinned,
		AtMS: time.Now().UnixMilli(), VCPUs: box.VCPUs, MemMB: box.MemMB,
		Turbo: box.Turbo, Ballooned: box.Ballooned,
		LifeRxBytes: box.NetRxBytes, LifeTxBytes: box.NetTxBytes,
		Repositories: repositories, HiveMind: box.HiveMind,
	}
	if !box.RepoStatusAt.IsZero() {
		at := box.RepoStatusAt
		doc.RepoStatusAt = &at
	}
	if s.vitals != nil && box.State == vmm.StateRunning {
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if live, err := s.vitals.Vitals(readCtx, box.Name); err == nil {
			doc.CPUSeconds, doc.MemUsedMB = live.CPUSeconds, live.MemUsedMB
			doc.NetRxBytes, doc.NetTxBytes = live.NetRxBytes, live.NetTxBytes
			doc.ListeningPorts = append([]int(nil), live.ListeningPorts...)
			doc.PortServices = append([]host.PortService(nil), live.PortServices...)
			doc.PortsChecked = live.PortsChecked
		} else if err != nil {
			s.log.Debug("guest status vitals unavailable", "sandbox", box.Name, "err", err)
		}
	}
	return doc
}

func (s *Server) writeSelf(w http.ResponseWriter, doc selfDoc) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(doc) //nolint:errcheck
}

func (s *Server) writeSelfText(w http.ResponseWriter, doc selfDoc) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, "sandbox: %s\nstate: %s\npinned: %t\n", doc.Sandbox, doc.State, doc.Pinned)
	fmt.Fprintf(w, "allocation: %d vCPU, %d MiB memory\n", doc.VCPUs, doc.MemMB)
	if doc.CPUSeconds != nil {
		fmt.Fprintf(w, "cpu time: %.1f seconds\n", *doc.CPUSeconds)
	}
	if doc.MemUsedMB != nil {
		if doc.MemMB > 0 {
			fmt.Fprintf(w, "memory: %d MiB used (%.0f%%)\n", *doc.MemUsedMB, 100*float64(*doc.MemUsedMB)/float64(doc.MemMB))
		} else {
			fmt.Fprintf(w, "memory: %d MiB used\n", *doc.MemUsedMB)
		}
	}
	if doc.NetRxBytes != nil && doc.NetTxBytes != nil {
		fmt.Fprintf(w, "network: %d bytes received, %d bytes sent\n", *doc.NetRxBytes, *doc.NetTxBytes)
	}
	if doc.PortsChecked {
		if len(doc.ListeningPorts) == 0 {
			fmt.Fprintln(w, "listening ports: none")
		} else {
			ports := make([]string, 0, len(doc.ListeningPorts))
			for _, port := range doc.ListeningPorts {
				ports = append(ports, strconv.Itoa(port))
			}
			fmt.Fprintf(w, "listening ports: %s\n", strings.Join(ports, ", "))
		}
	}
	if len(doc.Repositories) == 0 {
		if doc.RepoStatusAt == nil {
			fmt.Fprintln(w, "repos: none reported")
		} else {
			fmt.Fprintln(w, "repos: none attached")
		}
	} else {
		fmt.Fprintln(w, "repos:")
		slugs := make([]string, 0, len(doc.Repositories))
		for slug := range doc.Repositories {
			slugs = append(slugs, slug)
		}
		sort.Strings(slugs)
		for _, slug := range slugs {
			repo := doc.Repositories[slug]
			branch := repo.Branch
			if branch == "" {
				branch = "not cloned"
			}
			flags := make([]string, 0, 3)
			if repo.Ahead > 0 {
				flags = append(flags, fmt.Sprintf("%d unpushed", repo.Ahead))
			}
			if repo.Behind > 0 {
				flags = append(flags, fmt.Sprintf("%d behind", repo.Behind))
			}
			if repo.Dirty {
				flags = append(flags, "dirty")
			}
			note := ""
			if len(flags) > 0 {
				note = " [" + strings.Join(flags, ", ") + "]"
			}
			fmt.Fprintf(w, "  %-28s %-34s %s%s\n", repo.Slug, repo.Path, branch, note)
		}
	}
	if doc.RepoStatusAt != nil {
		fmt.Fprintf(w, "repo status checked: %s\n", doc.RepoStatusAt.UTC().Format(time.RFC3339))
	}
	writeHiveMindSessions(w, doc.HiveMind)
}

func writeHiveMindSessions(w io.Writer, snapshot *host.HiveMindSessionSnapshot) {
	if snapshot == nil {
		return
	}
	checked := ""
	if !snapshot.ObservedAt.IsZero() {
		checked = ", cached at " + snapshot.ObservedAt.UTC().Format(time.RFC3339)
	}
	if len(snapshot.Sessions) == 0 {
		fmt.Fprintf(w, "HiveMind sessions: none recorded%s\n", checked)
		return
	}

	shown, total := len(snapshot.Sessions), snapshot.TotalCount
	if total < shown {
		total = shown
	}
	count := fmt.Sprintf("%d", total)
	if snapshot.HasMore || shown < total {
		count = fmt.Sprintf("%d of %d shown", shown, total)
	}
	fmt.Fprintf(w, "HiveMind sessions (%s%s):\n", count, checked)
	for _, session := range snapshot.Sessions {
		title := ctlops.SafeText(session.Title, 80)
		if title == "" {
			title = ctlops.SafeText(session.ID, 80)
		}
		if title == "" {
			title = "untitled"
		}
		state := ctlops.SafeText(session.State, 24)
		if state != "" {
			fmt.Fprintf(w, "  %s [%s]\n", title, state)
		} else {
			fmt.Fprintf(w, "  %s\n", title)
		}
		detail := make([]string, 0, 2)
		if agent := ctlops.SafeText(session.AgentType, 32); agent != "" {
			detail = append(detail, agent)
		}
		if model := ctlops.SafeText(session.Model, 48); model != "" {
			detail = append(detail, model)
		}
		if len(detail) > 0 {
			fmt.Fprintf(w, "    %s\n", strings.Join(detail, " · "))
		}
		if link := ctlops.SafeText(session.URL, 500); link != "" {
			fmt.Fprintf(w, "    %s\n", link)
		}
	}
}

func (s *Server) visibility(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	visibility := r.PathValue("visibility")
	if visibility != "public" && visibility != "private" {
		http.Error(w, "sparkbox: visibility must be public or private", http.StatusBadRequest)
		return
	}
	// ?port= narrows the change to one guest port of this sandbox's hostname.
	// Absent means the whole sandbox, which is asymmetric on purpose: private
	// closes every port, public opens only the default one. See
	// ctlops.SetVisibility for why.
	port := 0
	if raw := r.URL.Query().Get("port"); raw != "" {
		p, err := strconv.Atoi(raw)
		if err != nil || p < 1 || p > 65535 {
			http.Error(w, "sparkbox: port must be from 1 through 65535", http.StatusBadRequest)
			return
		}
		port = p
	}
	if s.routes == nil {
		http.Error(w, "sparkbox: route self-service is not enabled", http.StatusNotImplemented)
		return
	}
	result, err := s.routes.SetVisibility(r.Context(), box, visibility, port)
	if err != nil {
		s.failRouteControl(w, box, err)
		return
	}
	s.writeJSON(w, result)
}

func (s *Server) port(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "sparkbox: port must be from 1 through 65535", http.StatusBadRequest)
		return
	}
	if s.routes == nil {
		http.Error(w, "sparkbox: route self-service is not enabled", http.StatusNotImplemented)
		return
	}
	result, err := s.routes.SetPort(r.Context(), box, port)
	if err != nil {
		s.failRouteControl(w, box, err)
		return
	}
	s.writeJSON(w, result)
}

func (s *Server) failRouteControl(w http.ResponseWriter, box *host.Sandbox, err error) {
	s.log.Error("metadata route self-service failed", "sandbox", box.Name, "err", err)
	http.Error(w, "sparkbox: gateway could not update this sandbox's route", http.StatusBadGateway)
}

func (s *Server) writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(value) //nolint:errcheck
}

// ListenAndServe runs the metadata service until ctx is done. It binds all
// interfaces because taps come and go with sandboxes; every request is then
// checked against the guest range, so a connection arriving on the public NIC
// is refused. Deployments additionally firewall the port to sbtap+ only.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	// Stashed before anything can be served: a capture accepted on one request
	// outlives it by minutes, and this is what bounds that worker by the life of
	// the process rather than by a request that has already been answered.
	s.setBaseContext(ctx)
	srv := &http.Server{
		Addr:         addr,
		Handler:      s.Handler(),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		// Stash the accepted connection's addresses on the request context:
		// net/http exposes the remote address as a string but not the local one,
		// and we check both.
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, localAddrKey{}, c.LocalAddr())
		},
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx) //nolint:errcheck
	}()
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type localAddrKey struct{}

// caller resolves a request to the sandbox that made it, or an error if the
// connection isn't a guest talking to its own gateway.
func (s *Server) caller(r *http.Request) (*host.Sandbox, error) {
	srcStr, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil, fmt.Errorf("unparseable remote address")
	}
	src, err := netip.ParseAddr(srcStr)
	if err != nil {
		return nil, fmt.Errorf("caller is not a sandbox")
	}
	src = src.Unmap()
	slotIndex, ok := s.guestNet.SlotForGuest(src)
	if !ok {
		return nil, fmt.Errorf("caller is not a sandbox")
	}
	local, _ := r.Context().Value(localAddrKey{}).(net.Addr)
	ta, ok := local.(*net.TCPAddr)
	if !ok {
		return nil, fmt.Errorf("unparseable local address")
	}
	localAddr, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return nil, fmt.Errorf("unparseable local address")
	}
	slot, err := s.guestNet.Slot(slotIndex)
	if err != nil {
		return nil, fmt.Errorf("caller is not a sandbox")
	}
	// A guest may only ask the host offset in its own /30. Refusing cross-slot
	// destinations keeps a guest from reaching another slot's endpoint.
	if localAddr.Unmap() != slot.Host {
		return nil, fmt.Errorf("caller must use its own gateway address")
	}
	box, ok := s.mgr.GetByHostIP(src.String())
	if !ok {
		return nil, fmt.Errorf("no sandbox at %s", src)
	}
	return box, nil
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		s.log.Warn("metadata token refused", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	aud := r.URL.Query().Get("aud")
	if aud == "" {
		aud = s.defAud
	}
	// The rate limit is taken BEFORE the mint, and on a node that matters more
	// than it does on a gateway: past here the call leaves the machine, so this
	// is also what bounds how much of the gateway's link one guest can occupy.
	if !s.allow(box.Name) {
		http.Error(w, "sparkbox: too many token requests", http.StatusTooManyRequests)
		return
	}

	tok, err := s.id.Issue(r.Context(), box, aud)
	if err != nil {
		s.fail(w, "mint", box, err)
		return
	}
	s.log.Info("minted id token", "sandbox", box.Name, "owner", box.Owner, "aud", aud, "exp", tok.ExpiresAt)
	w.Header().Set("Content-Type", "application/jwt")
	// Every fetch mints a fresh token with a new jti; the exchange accepts each
	// exactly once, so a cached token is a token that no longer works.
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintln(w, tok.JWT)
}

// identity returns the claims without minting a token, so a guest can answer
// "who am I" without burning a single-use jti.
func (s *Server) identity(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	doc, err := s.id.Describe(r.Context(), box)
	if err != nil {
		s.fail(w, "describe", box, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(doc) //nolint:errcheck
}

// fail maps an Identity's error onto a status. The 503 is the one that earns
// its place: a node whose gateway is down is a host that will be able to answer
// this shortly, and the guest's timer is already the retry — telling it 500
// instead invites the systemd unit to burn its StartLimitBurst against an
// outage that fixes itself.
func (s *Server) fail(w http.ResponseWriter, what string, box *host.Sandbox, err error) {
	switch {
	case errors.Is(err, ErrAudience):
		http.Error(w, "sparkbox: "+err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrNoIssuer):
		s.log.Warn("identity unavailable", "op", what, "sandbox", box.Name, "err", err)
		http.Error(w, "sparkbox: "+err.Error(), http.StatusServiceUnavailable)
	default:
		s.log.Error("identity failed", "op", what, "sandbox", box.Name, "err", err)
		http.Error(w, "sparkbox: could not establish this sandbox's identity", http.StatusInternalServerError)
	}
}

// allow is a per-sandbox sliding-window rate limit on minting.
func (s *Server) allow(sandbox string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-rateWindow)

	// Sweep sandboxes that have stopped asking — paused, destroyed, or just
	// idle. They never call again, so nothing else would ever drop them and the
	// map would grow for the life of the process. Cheap: this runs at most once
	// per mint, and a sandbox mints about once every 45 minutes.
	for name, times := range s.recent {
		if len(times) == 0 || times[len(times)-1].Before(cutoff) {
			delete(s.recent, name)
		}
	}
	// A surviving entry can still hold timestamps that have aged out, so filter
	// this caller's own window too.
	kept := s.recent[sandbox][:0]
	for _, t := range s.recent[sandbox] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rateBurst {
		s.recent[sandbox] = kept
		return false
	}
	s.recent[sandbox] = append(kept, now)
	return true
}
