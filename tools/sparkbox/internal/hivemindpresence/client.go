package hivemindpresence

// The HiveMind half of the federation, as one authenticated client.
//
// It is a type of its own rather than Monitor's private methods because two
// callers need the same three steps — mint an id token for a sandbox, exchange
// it for a HiveMind JWT, ask a question that JWT is bound to — for different
// reasons. The monitor asks every minute so the reaper does not stop a VM whose
// agent is mid-conversation. `ctl sessions` asks once, because a person wants
// to know what has run there. Sharing one client means they also share the
// exchange cache, so the human-triggered query usually costs no exchange.
//
// Nothing here takes a sandbox ID as an argument. Every request is bound to the
// sandbox by the token it carries: exchange projects the id token's `sandbox_id`
// claim into the signed `partner_device_id`, and both runtime endpoints answer
// only for that tuple. That is the whole authorization story, and it is why a
// gateway may ask these questions about a VM running on somebody else's
// hardware without any node agreeing to it — the claims come from the fleet's
// own ledger and its own signing key, never from the machine holding the VM.
// See docs/partner-federation.md in wandb/agentstream.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
)

// defaultPageSize is one catalog page. The API caps page_size at 100; a VM with
// more sessions than that has a history, not a status, and the caller that
// wants it can page.
const defaultPageSize = 50

const maxPageSize = 100

// Identity mints a sandbox's workload id token. On a gateway this is the local
// signing path; on a node it is the relay that asks the gateway to sign. Either
// way the claims are assembled from the fleet's record of the sandbox.
type Identity interface {
	Issue(ctx context.Context, box *host.Sandbox, aud string) (metadata.Token, error)
}

// ClientOptions configures a Client. APIBase and Identity are required.
type ClientOptions struct {
	APIBase    string
	Audience   string
	Identity   Identity
	HTTPClient *http.Client
	UserAgent  string
}

// Client is one process's authenticated view of the HiveMind API. Safe for
// concurrent use.
type Client struct {
	apiBase   string
	audience  string
	identity  Identity
	http      *http.Client
	userAgent string

	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

// Presence is what the lightweight endpoint answers: whether this sandbox has
// live HiveMind activity, and until when that buys it protection from the
// reaper. ProtectUntil is nil when nothing is live.
type Presence struct {
	ObservedAt time.Time `json:"observed_at"`
	// State is HiveMind's own word for what the device is doing — "idle" and
	// "waiting" are the two seen so far. It is carried as an opaque string on
	// purpose: this end of the federation should not have to ship a release to
	// pass through a value HiveMind has started sending.
	//
	// It is display only. ProtectUntil is what the reaper honours, and what
	// Live() answers from, because that field has an agreed meaning on both
	// sides and expires on its own.
	State        string     `json:"presence"`
	ProtectUntil *time.Time `json:"protect_until"`
}

// Live reports whether this reading says an agent is working.
func (p Presence) Live() bool {
	return p.ProtectUntil != nil && p.ProtectUntil.After(time.Now())
}

type exchangeResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

type sessionsResponse struct {
	ObservedAt time.Time              `json:"observed_at"`
	Sessions   []host.HiveMindSession `json:"sessions"`
	TotalCount int                    `json:"total_count"`
	HasMore    bool                   `json:"has_more"`
}

func NewClient(opts ClientOptions) (*Client, error) {
	apiBase := strings.TrimSpace(opts.APIBase)
	if apiBase == "" {
		return nil, fmt.Errorf("hivemind presence: API base is required")
	}
	parsedBase, err := url.Parse(apiBase)
	if err != nil || parsedBase.Host == "" {
		return nil, fmt.Errorf("hivemind presence: API base must be an absolute URL")
	}
	if parsedBase.Scheme != "https" {
		hostname := parsedBase.Hostname()
		ip := net.ParseIP(hostname)
		loopbackHTTP := parsedBase.Scheme == "http" &&
			(hostname == "localhost" || (ip != nil && ip.IsLoopback()))
		if !loopbackHTTP {
			return nil, fmt.Errorf("hivemind presence: API base must use HTTPS (HTTP is allowed only for loopback testing)")
		}
	}
	if opts.Identity == nil {
		return nil, fmt.Errorf("hivemind presence: an identity source is required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{
		apiBase:   strings.TrimRight(apiBase, "/"),
		audience:  opts.Audience,
		identity:  opts.Identity,
		http:      client,
		userAgent: opts.UserAgent,
		tokens:    map[string]cachedToken{},
	}, nil
}

// Presence asks whether this sandbox has live sessions right now.
func (c *Client) Presence(ctx context.Context, box *host.Sandbox) (Presence, error) {
	token, err := c.token(ctx, box)
	if err != nil {
		return Presence{}, err
	}
	var out Presence
	if err := c.post(ctx, "/v1/integrations/runtime/presence", token, []byte("{}"), &out); err != nil {
		return Presence{}, fmt.Errorf("query: %w", err)
	}
	return out, nil
}

// Sessions reads one page of the sandbox's session catalog, newest activity
// first. pageSize <= 0 takes the default; anything above the API's cap is
// clamped here rather than left for the server to reject.
func (c *Client) Sessions(
	ctx context.Context,
	box *host.Sandbox,
	pageSize int,
) (host.HiveMindSessionSnapshot, error) {
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	token, err := c.token(ctx, box)
	if err != nil {
		return host.HiveMindSessionSnapshot{}, err
	}
	var out sessionsResponse
	path := fmt.Sprintf("/v1/integrations/runtime/sessions?page_size=%d", pageSize)
	if err := c.post(ctx, path, token, []byte("{}"), &out); err != nil {
		return host.HiveMindSessionSnapshot{}, fmt.Errorf("sessions: %w", err)
	}
	return host.HiveMindSessionSnapshot{
		ObservedAt: out.ObservedAt,
		Sessions:   out.Sessions,
		TotalCount: out.TotalCount,
		HasMore:    out.HasMore,
	}, nil
}

// Retain drops the cached exchange for every sandbox not in live. The monitor
// calls it each poll so a destroyed VM's credential does not sit in memory for
// the rest of the process's life.
func (c *Client) Retain(live map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for sandboxID := range c.tokens {
		if _, exists := live[sandboxID]; !exists {
			delete(c.tokens, sandboxID)
		}
	}
}

// token returns a HiveMind JWT for this sandbox, exchanging a fresh id token
// when the cached one is gone or within five minutes of expiring.
//
// The five-minute floor is not politeness. Exchanged tokens are single-use —
// HiveMind remembers the id token's jti for 24 hours — so a token presented
// after expiry cannot be retried, only re-minted, and re-minting is the cheap
// half. Caching by sandbox ID rather than by name matters for the same reason a
// rename is not an identity change: the ID is what the claim carries.
func (c *Client) token(ctx context.Context, box *host.Sandbox) (string, error) {
	now := time.Now()
	c.mu.Lock()
	cached := c.tokens[box.ID]
	c.mu.Unlock()
	if cached.value != "" && cached.expiresAt.After(now.Add(5*time.Minute)) {
		return cached.value, nil
	}

	idToken, err := c.identity.Issue(ctx, box, c.audience)
	if err != nil {
		return "", fmt.Errorf("mint identity: %w", err)
	}
	body, err := json.Marshal(map[string]string{"id_token": idToken.JWT})
	if err != nil {
		return "", err
	}
	var exchange exchangeResponse
	if err := c.post(ctx, "/v1/auth/actions/exchange", "", body, &exchange); err != nil {
		return "", fmt.Errorf("exchange identity: %w", err)
	}
	if exchange.Token == "" || exchange.ExpiresAt <= now.Unix() {
		return "", fmt.Errorf("exchange identity: HiveMind returned an invalid token")
	}
	cached = cachedToken{
		value:     exchange.Token,
		expiresAt: time.Unix(exchange.ExpiresAt, 0),
	}
	c.mu.Lock()
	c.tokens[box.ID] = cached
	c.mu.Unlock()
	return cached.value, nil
}

func (c *Client) post(
	ctx context.Context,
	path string,
	bearer string,
	body []byte,
	out any,
) error {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.apiBase+path, bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if c.userAgent != "" {
		request.Header.Set("User-Agent", c.userAgent)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
