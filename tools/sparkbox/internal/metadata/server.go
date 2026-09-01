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
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/guestnet"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
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
	SetVisibility(ctx context.Context, box *host.Sandbox, visibility string) (RouteVisibility, error)
	SetPort(ctx context.Context, box *host.Sandbox, port int) (RoutePort, error)
}

type RouteVisibility struct {
	Sandbox    string `json:"sandbox"`
	Visibility string `json:"visibility"`
	Routes     int    `json:"routes"`
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
	tools          ToolCache
	lifecycle      SelfLifecycle
	// allowSelfSnapshot is the operator's kill switch for capture-from-inside.
	// Default on, because the carried-tag restriction already bounds it and a
	// self-service feature nobody is told about does not exist; the flag is for
	// an operator handing boxes to third parties. `pause` is not behind it —
	// see SelfLifecycle.
	allowSelfSnapshot bool
	log               *slog.Logger
	defAud            string
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
	// Tools serves this machine's own agent-CLI cache. Nil answers both /tools
	// routes 501, which is what a host started without --tools-dir is. It is
	// never a relay to another machine — see ToolCache.
	Tools ToolCache
	// SelfLifecycle runs `sparkbox pause` and `sparkbox snapshot <tag>` for the
	// calling sandbox. Nil answers all three lifecycle routes 501, which is
	// what a host with no control plane on its fleet is.
	SelfLifecycle SelfLifecycle
	// AllowSelfSnapshot is the operator's switch for the capture verb alone.
	// The gateway and node wiring both default it to true; `pause` ignores it.
	AllowSelfSnapshot bool
	Logger            *slog.Logger
	// DefaultAudience is used when a caller passes no ?aud=.
	DefaultAudience string
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
		repoAccess: opts.Repos, repoAuthorizer: opts.RepoAuthorizer, tools: opts.Tools, log: log,
		lifecycle:         opts.SelfLifecycle,
		allowSelfSnapshot: opts.AllowSelfSnapshot,
		defAud:            opts.DefaultAudience,
		guestNet:          guestNetwork,
		recent:            map[string][]time.Time{},
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", s.token)
	mux.HandleFunc("GET /identity", s.identity)
	mux.HandleFunc("GET /repos", s.repoManifest)
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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

type selfDoc struct {
	Sandbox string `json:"sandbox"`
	Pinned  bool   `json:"pinned"`
}

func (s *Server) self(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	s.writeSelf(w, box)
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
	s.writeSelf(w, box)
}

func (s *Server) writeSelf(w http.ResponseWriter, box *host.Sandbox) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(selfDoc{Sandbox: box.Name, Pinned: box.Pinned}) //nolint:errcheck
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
	if s.routes == nil {
		http.Error(w, "sparkbox: route self-service is not enabled", http.StatusNotImplemented)
		return
	}
	result, err := s.routes.SetVisibility(r.Context(), box, visibility)
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
