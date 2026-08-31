// Package hivemindsignin is the back channel of the HiveMind sign-in handshake:
// one POST that trades a single-use code, handed to us through the visitor's
// browser, for the facts HiveMind holds about them.
//
// # Why a code and a callback rather than a signed assertion
//
// The obvious design is the SAML/OIDC one: HiveMind signs a short-lived
// assertion, the browser POSTs it here, and the edge verifies the signature
// offline against a pinned key or a JWKS. It works, and docs/hivemind-signin
// -design.md keeps it as the escape hatch. It is not what shipped, because
// everything it needs is machinery this handshake can simply not have:
//
//   - a signing key on HiveMind's side to mint, distribute, pin and rotate
//   - a JWKS fetch with its own cache, failure mode and rotation window
//   - a replay cache, because a self-contained bearer credential is valid until
//     it expires no matter how many times it is presented
//   - a tolerance for clock skew between two independently operated systems
//
// A code redeemed over the back channel needs none of them. Single use is
// enforced by the one party that can enforce it — the minter — so a stolen code
// is worthless the moment the real visitor's browser arrives, and a code that
// is never redeemed simply expires in a Redis key. Revocation, which for a
// signed assertion is a denylist nobody wants to build, is a delete.
//
// The cost is honest and it is one dependency: signing in through this door
// needs the gateway to reach HiveMind. That is not a new dependency — the same
// binary already dials the same API every minute (internal/hivemindpresence,
// --hivemind-api) — and it is contained: a HiveMind outage closes THIS door
// only. The session token minted from `ssh ctl@<gateway> session-token` and the
// passkey path are untouched, so nobody is locked out of sparkbox by it.
//
// # What this package deliberately does not do
//
// It does not decide anything. It performs no membership check, applies no
// allowlist, resolves no account and mints no session. It reads a body off the
// wire and hands back what HiveMind said, with the shapes validated to the
// point where a downstream string can be trusted not to be a smuggled newline.
// The policy lives at the door (internal/edgeauth/handoff.go) where an operator
// flag can be read beside it.
package hivemindsignin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Path is the redeem endpoint on the HiveMind API. It is a constant rather than
// a flag because the two halves of one handshake are released together, and an
// operator who could point this at an arbitrary path could point it at one that
// answers 200 with an empty body — which this package would then have to treat
// as a failure anyway.
const Path = "/v1/handoff/redeem"

// maxCodeLen bounds what is accepted as a code before any network call.
// HiveMind mints 32 random bytes base64url'd; the ceiling is loose enough for
// that to grow a prefix and tight enough that a megabyte of form field never
// reaches an HTTP request.
const maxCodeLen = 256

// ErrRefused is what a code HiveMind will not honour comes back as: unknown,
// already redeemed, or expired. One error for all three on purpose — the
// browser holding it can tell them apart only by guessing, so the door has one
// sentence and there is nothing to leak by choosing it.
var ErrRefused = errors.New("hivemind refused this sign-in code")

// Claims is what HiveMind says about the person who started the handshake.
//
// Every field is untrusted input until this package has checked it, and the
// checks are not hygiene: GitHub and Email reach a sqlite write and an HTTP
// header, Dest reaches a Location, and Orgs reaches an allowlist comparison
// whose whole value is that both sides mean the same string.
type Claims struct {
	// Subject is HiveMind's own immutable id for the account, carried for the
	// audit line only. Nothing here resolves on it — sparkbox has no column for
	// a HiveMind user — and it must never become an identity: the anchor is
	// GitHub, which is a fact about the world rather than about HiveMind.
	Subject string `json:"sub"`
	// GitHub is the login HiveMind's own GitHub OAuth login established. This
	// is the anchor and the only field the account half reads.
	GitHub string `json:"github"`
	// GitHubID is GitHub's immutable account number, 0 when HiveMind has none.
	GitHubID int64 `json:"github_id"`
	// Orgs is the GitHub organisations HiveMind currently believes the visitor
	// belongs to, refreshed on its side by a worker that also REMOVES orgs
	// somebody has left. It is a snapshot at sign-in time and nothing here
	// pretends otherwise.
	Orgs []string `json:"orgs"`
	// Email is optional; the edge forwards it upstream as X-Forwarded-Email.
	Email string `json:"email"`
	// Dest is where the handshake was aimed, fixed by HiveMind when the code was
	// minted rather than read from the browser's POST. That is the half of the
	// login-CSRF mitigation this package owns: a stolen code cannot be re-aimed.
	// It is still run through the edge's own return-URL guard before it reaches
	// a Location header — see internal/edgeauth's safeReturn — because "HiveMind
	// said so" is not a reason to redirect off our own zone.
	Dest string `json:"dest"`
}

// HasOrg reports whether the visitor is in org, compared the way GitHub itself
// compares account names: case-insensitively. A door configured for `wandb`
// must admit somebody HiveMind spells `WandB`.
func (c Claims) HasOrg(org string) bool {
	for _, have := range c.Orgs {
		if strings.EqualFold(strings.TrimSpace(have), strings.TrimSpace(org)) {
			return true
		}
	}
	return false
}

// Options configures a Redeemer. APIBase is required.
type Options struct {
	// APIBase is the HiveMind API origin, e.g. https://api.hivemind.tools. The
	// same value --hivemind-api already carries.
	APIBase string
	// HTTPClient is optional; the default carries a timeout deliberately
	// shorter than any browser's patience, because a visitor is watching a
	// blank page while this runs.
	HTTPClient *http.Client
	UserAgent  string
}

// Redeemer trades codes for claims. Safe for concurrent use; it holds no state
// beyond its configuration, which is itself the point — there is no cache here,
// because a code is single-use and caching one would be caching a credential
// that is already spent.
type Redeemer struct {
	endpoint  string
	http      *http.Client
	userAgent string
}

// New validates the API base and returns a Redeemer.
//
// The https requirement, and the loopback exception to it, are lifted from
// hivemindpresence.NewClient deliberately rather than imported: they are the
// same rule about the same host, and the two packages disagreeing about which
// schemes are acceptable would be a way to configure one door insecurely while
// the other refused.
func New(opts Options) (*Redeemer, error) {
	base := strings.TrimSpace(opts.APIBase)
	if base == "" {
		return nil, fmt.Errorf("hivemind sign-in: API base is required")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("hivemind sign-in: API base must be an absolute URL")
	}
	if parsed.Scheme != "https" {
		hostname := parsed.Hostname()
		ip := net.ParseIP(hostname)
		loopbackHTTP := parsed.Scheme == "http" &&
			(hostname == "localhost" || (ip != nil && ip.IsLoopback()))
		if !loopbackHTTP {
			return nil, fmt.Errorf("hivemind sign-in: API base must use HTTPS (HTTP is allowed only for loopback testing)")
		}
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &Redeemer{
		endpoint:  strings.TrimRight(base, "/") + Path,
		http:      client,
		userAgent: opts.UserAgent,
	}, nil
}

// Redeem spends a code and returns what HiveMind says about its holder.
//
// A refused code is ErrRefused and nothing else; every other failure is
// wrapped with enough context for an operator's log and nothing that a browser
// should see, because the handler renders one sentence either way.
func (r *Redeemer) Redeem(ctx context.Context, code string) (Claims, error) {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > maxCodeLen {
		// Refused rather than invalid: a malformed code and an expired one are
		// the same event to the person holding it, and the door says the same
		// sentence for both. Checking it here also means a pasted essay never
		// becomes an outbound request.
		return Claims{}, ErrRefused
	}
	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return Claims{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return Claims{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if r.userAgent != "" {
		request.Header.Set("User-Agent", r.userAgent)
	}
	response, err := r.http.Do(request)
	if err != nil {
		return Claims{}, fmt.Errorf("redeem: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck

	switch {
	case response.StatusCode == http.StatusOK:
	case response.StatusCode == http.StatusNotFound,
		response.StatusCode == http.StatusGone,
		response.StatusCode == http.StatusUnauthorized,
		response.StatusCode == http.StatusForbidden:
		// The four ways HiveMind can say "not a code I will honour". A 404 is
		// in the list because an unknown code and an unmounted endpoint answer
		// the same status, and the visitor-facing outcome is identical: this
		// link did not work, start again.
		return Claims{}, ErrRefused
	default:
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return Claims{}, fmt.Errorf("redeem: hivemind returned %s: %s",
			response.Status, strings.TrimSpace(string(message)))
	}

	var claims Claims
	// 64 KiB is orders of magnitude more than a claims document and still small
	// enough that a misbehaving upstream cannot make this allocate a browser
	// request's worth of memory per click.
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&claims); err != nil {
		return Claims{}, fmt.Errorf("redeem: could not read hivemind's answer: %w", err)
	}
	if err := claims.validate(); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

// validate refuses a claims document that cannot be acted on safely.
//
// The GitHub login is the one field with no tolerance at all: it becomes an
// account name, a key-adoption target and a `github` claim, so a login this
// process cannot vouch for the shape of must not travel further. Email and
// Subject are cleaned rather than refused — neither authorizes anything, and
// losing a display string is not worth failing a sign-in over.
func (c *Claims) validate() error {
	c.GitHub = strings.TrimSpace(c.GitHub)
	if c.GitHub == "" {
		// Fails closed, and this is the same rule HiveMind applies to us in the
		// other direction: partner_federation refuses a sparkbox token with no
		// `github` claim rather than falling back to the routing handle.
		return fmt.Errorf("redeem: hivemind returned no github login for this code")
	}
	if len(c.GitHub) > 39 || strings.ContainsAny(c.GitHub, " \t\r\n/@:") {
		// A cheap shape check, not the grammar: users.ValidGitHubLogin owns
		// that and runs at the door. What this stops is the class where a value
		// with a newline or a slash in it reaches a log line or a URL before
		// anybody has looked at it.
		return fmt.Errorf("redeem: hivemind returned a github login this host will not accept")
	}
	c.Subject = sanitizeLine(c.Subject, 200)
	c.Email = sanitizeLine(c.Email, 254)
	c.Dest = sanitizeLine(c.Dest, 2048)
	orgs := make([]string, 0, len(c.Orgs))
	for _, org := range c.Orgs {
		if org = sanitizeLine(org, 39); org != "" {
			orgs = append(orgs, org)
		}
	}
	c.Orgs = orgs
	return nil
}

// sanitizeLine trims a value and drops it entirely if it carries anything that
// would let it break out of the single line it is going to be rendered on — a
// header, a log field, a Location. Dropping rather than escaping is right here
// because every field it guards is optional: an absent email costs a header,
// where a smuggled CR costs a response split.
func sanitizeLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > max {
		return ""
	}
	if strings.ContainsAny(s, "\r\n\x00") {
		return ""
	}
	return s
}
