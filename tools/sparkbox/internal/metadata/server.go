// Package metadata is the per-sandbox identity endpoint: the unforgeable
// channel by which a guest learns who it is and gets an OIDC id token, with no
// secret material ever stored in the VM. Possession of the network position is
// the authentication — the same model as a cloud provider's IMDS.
//
// # How a caller is identified (and why this is safe)
//
// Each guest sits on a point-to-point /30 with the host: guest 172.30.<idx>.2,
// host 172.30.<idx>.1. A request is attributed to the sandbox whose recorded
// guest IP equals the connection's SOURCE address, and is refused unless the
// DESTINATION is that same slot's host address — a guest may only ask its own
// gateway.
//
// Source, not destination, is the identity. Destination is attacker-chosen:
// the host has IP forwarding on and Linux's weak host model accepts a packet
// for any local address on any interface, so a guest in slot 5 can simply
// connect to 172.30.9.1 and have the kernel deliver it — the accepted socket's
// local address would then read 172.30.9.1 and hand slot 5 a token minted for
// slot 9. Source cannot be forged the same way: this is TCP, and a SYN with a
// spoofed source is answered by a SYN-ACK routed to the *real* owner of that
// address (a different tap), so the spoofer never completes the handshake.
// The firecracker driver additionally sets rp_filter on each tap, which drops
// such packets outright rather than merely leaving them unanswered.
package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// DefaultPort is the metadata service's port on each tap host address. It is
// reachable only from the guest on the other end of that tap.
const DefaultPort = 8967

// guestNet bounds which source addresses may be attributed to a sandbox at
// all: the driver's per-VM /30 range.
var guestNet = net.IPNet{IP: net.IPv4(172, 30, 0, 0), Mask: net.CIDRMask(16, 32)}

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
}

// Accounts is the slice of the user store this service needs: the owner's
// verified external identity. *users.Store satisfies it.
type Accounts interface {
	Get(handle string) (users.User, error)
}

type Server struct {
	mgr      Sandboxes
	issuer   *oidc.Issuer
	users    Accounts
	log      *slog.Logger
	defAud   string
	nodeName string

	mu     sync.Mutex
	recent map[string][]time.Time // sandbox -> mint times inside the window
}

type Options struct {
	Manager Sandboxes
	Issuer  *oidc.Issuer
	Users   Accounts
	Logger  *slog.Logger
	// DefaultAudience is used when a caller passes no ?aud=.
	DefaultAudience string
	// NodeName identifies this host in the `box` claim.
	NodeName string
}

func New(opts Options) *Server {
	return &Server{
		mgr: opts.Manager, issuer: opts.Issuer, users: opts.Users, log: opts.Logger,
		defAud: opts.DefaultAudience, nodeName: opts.NodeName,
		recent: map[string][]time.Time{},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", s.token)
	mux.HandleFunc("GET /identity", s.identity)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// ListenAndServe runs the metadata service until ctx is done. It binds all
// interfaces because taps come and go with sandboxes; every request is then
// checked against the guest range, so a connection arriving on the public NIC
// is refused. Deployments additionally firewall the port to sbtap+ only.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
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
	src := net.ParseIP(srcStr)
	if src == nil || !guestNet.Contains(src) {
		return nil, fmt.Errorf("caller is not a sandbox")
	}
	local, _ := r.Context().Value(localAddrKey{}).(net.Addr)
	ta, ok := local.(*net.TCPAddr)
	if !ok {
		return nil, fmt.Errorf("unparseable local address")
	}
	// A guest may only ask its own gateway (172.30.<idx>.1 for source
	// 172.30.<idx>.2). Refusing cross-slot destinations keeps a guest from
	// even reaching another slot's endpoint.
	if !ta.IP.Equal(gatewayFor(src)) {
		return nil, fmt.Errorf("caller must use its own gateway address")
	}
	box, ok := s.mgr.GetByHostIP(src.String())
	if !ok {
		return nil, fmt.Errorf("no sandbox at %s", src)
	}
	return box, nil
}

// gatewayFor returns the host address on a guest's /30: 172.30.<idx>.2 ->
// 172.30.<idx>.1.
func gatewayFor(guest net.IP) net.IP {
	v4 := guest.To4()
	if v4 == nil {
		return nil
	}
	gw := make(net.IP, 4)
	copy(gw, v4)
	gw[3] = 1
	return gw
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
	if !s.issuer.AudienceAllowed(aud) {
		http.Error(w, fmt.Sprintf("sparkbox: audience %q is not allowed by this issuer", aud), http.StatusBadRequest)
		return
	}
	if !s.allow(box.Name) {
		http.Error(w, "sparkbox: too many token requests", http.StatusTooManyRequests)
		return
	}

	claims := s.claimsFor(box)
	claims.Audience = aud
	token, exp, err := s.issuer.Mint(claims)
	if err != nil {
		s.log.Error("mint failed", "sandbox", box.Name, "err", err)
		http.Error(w, "sparkbox: could not mint a token", http.StatusInternalServerError)
		return
	}
	s.log.Info("minted id token", "sandbox", box.Name, "owner", box.Owner, "aud", aud, "exp", exp)
	w.Header().Set("Content-Type", "application/jwt")
	// Every fetch mints a fresh token with a new jti; the exchange accepts each
	// exactly once, so a cached token is a token that no longer works.
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintln(w, token)
}

// identityDoc is what /run/sparkbox/identity.json holds in the guest: the same
// claims a token carries, minus the registered ones, so shells and tools can
// answer "who am I" without parsing a JWT.
type identityDoc struct {
	Issuer  string `json:"iss"`
	Subject string `json:"sub"`
	Owner   string `json:"owner"`
	GitHub  string `json:"github,omitempty"`
	KeyFP   string `json:"key_fp,omitempty"`
	Sandbox string `json:"sandbox"`
	Image   string `json:"image"`
	Box     string `json:"box"`
}

// identity returns the claims without minting a token, so a guest can answer
// "who am I" without burning a single-use jti.
func (s *Server) identity(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	c := s.claimsFor(box)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(identityDoc{ //nolint:errcheck
		Issuer: s.issuer.URL(), Subject: c.Subject, Owner: c.Owner,
		GitHub: c.GitHub, KeyFP: c.KeyFP,
		Sandbox: c.Sandbox, Image: c.Image, Box: c.Box,
	})
}

// claimsFor assembles the identity of a sandbox: its own facts plus its
// owner's. The `github` claim is present only when the owner verified it, so a
// policy matching on it fails closed for everyone else.
func (s *Server) claimsFor(box *host.Sandbox) oidc.Claims {
	c := oidc.Claims{
		Subject: oidc.SubjectFor(box.Owner),
		Owner:   box.Owner,
		KeyFP:   box.KeyFP,
		Sandbox: box.Name,
		Image:   box.Image,
		Box:     s.nodeName,
	}
	if s.users != nil {
		if u, err := s.users.Get(box.Owner); err == nil && u.GitHubVerifiedAt != nil {
			c.GitHub = u.GitHubLogin
		}
	}
	return c
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
