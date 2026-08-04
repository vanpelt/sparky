// Package hivemindpresence protects VMs whose HiveMind agent sessions are live.
package hivemindpresence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

const (
	defaultInterval        = time.Minute
	sessionRefreshInterval = 10 * time.Minute
	maxParallel            = 4
)

type Sandboxes interface {
	List() []*host.Sandbox
}

type Protector interface {
	ProtectUntil(sandboxID string, until time.Time)
}

type Observer interface {
	ObserveHiveMindSessions(sandboxID string, snapshot host.HiveMindSessionSnapshot)
}

type Identity interface {
	Issue(ctx context.Context, box *host.Sandbox, aud string) (metadata.Token, error)
}

type Options struct {
	APIBase    string
	Audience   string
	Sandboxes  Sandboxes
	Protector  Protector
	Observer   Observer
	Identity   Identity
	HTTPClient *http.Client
	Logger     *slog.Logger
	UserAgent  string
}

type Monitor struct {
	apiBase   string
	audience  string
	boxes     Sandboxes
	protector Protector
	observer  Observer
	identity  Identity
	http      *http.Client
	log       *slog.Logger
	userAgent string

	mu     sync.Mutex
	tokens map[string]cachedToken
	// sessionsAt is the last successful catalog refresh per sandbox. Protection
	// is polled every minute, but the expensive paginated history/count query is
	// deliberately much less frequent.
	sessionsAt map[string]time.Time
}

type cachedToken struct {
	value     string
	expiresAt time.Time
}

type exchangeResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
}

type presenceResponse struct {
	ObservedAt   time.Time  `json:"observed_at"`
	ProtectUntil *time.Time `json:"protect_until"`
}

type sessionsResponse struct {
	ObservedAt time.Time              `json:"observed_at"`
	Sessions   []host.HiveMindSession `json:"sessions"`
	TotalCount int                    `json:"total_count"`
	HasMore    bool                   `json:"has_more"`
}

func New(opts Options) (*Monitor, error) {
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
	if opts.Sandboxes == nil || opts.Protector == nil || opts.Identity == nil {
		return nil, fmt.Errorf("hivemind presence: sandboxes, protector, and identity are required")
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Monitor{
		apiBase:    strings.TrimRight(apiBase, "/"),
		audience:   opts.Audience,
		boxes:      opts.Sandboxes,
		protector:  opts.Protector,
		observer:   opts.Observer,
		identity:   opts.Identity,
		http:       client,
		log:        logger,
		userAgent:  opts.UserAgent,
		tokens:     map[string]cachedToken{},
		sessionsAt: map[string]time.Time{},
	}, nil
}

// Run checks immediately, then on interval until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultInterval
	}
	m.Poll(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Poll(ctx)
		}
	}
}

// Poll refreshes protection leases for every locally-running sandbox. Failures
// are fail-open for scale-to-zero: an old lease is never extended from stale
// state, and the normal idle policy resumes when it expires.
func (m *Monitor) Poll(ctx context.Context) {
	boxes := m.boxes.List()
	liveIDs := make(map[string]struct{}, len(boxes))
	for _, box := range boxes {
		if box.ID != "" {
			liveIDs[box.ID] = struct{}{}
		}
	}
	m.mu.Lock()
	for sandboxID := range m.tokens {
		if _, exists := liveIDs[sandboxID]; !exists {
			delete(m.tokens, sandboxID)
			delete(m.sessionsAt, sandboxID)
		}
	}
	m.mu.Unlock()

	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for _, box := range boxes {
		if box.State != vmm.StateRunning || box.ID == "" {
			continue
		}
		box := box
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if err := m.pollSandbox(ctx, box); err != nil {
				m.log.Warn("HiveMind monitor poll failed",
					"sandbox", box.Name, "sandbox_id", box.ID, "err", err)
			}
		}()
	}
	wg.Wait()
}

func (m *Monitor) pollSandbox(ctx context.Context, box *host.Sandbox) error {
	token, err := m.token(ctx, box)
	if err != nil {
		return err
	}
	var presence presenceResponse
	if err := m.post(
		ctx,
		"/v1/integrations/runtime/presence",
		token,
		[]byte("{}"),
		&presence,
	); err != nil {
		return fmt.Errorf("query: %w", err)
	}
	if presence.ProtectUntil != nil && presence.ProtectUntil.After(time.Now()) {
		m.protector.ProtectUntil(box.ID, presence.ProtectUntil.UTC())
	}
	if m.observer == nil || !m.sessionsDue(box.ID, time.Now()) {
		return nil
	}

	var sessions sessionsResponse
	if err := m.post(
		ctx,
		"/v1/integrations/runtime/sessions?page_size=100",
		token,
		[]byte("{}"),
		&sessions,
	); err != nil {
		return fmt.Errorf("sessions: %w", err)
	}
	m.observer.ObserveHiveMindSessions(box.ID, host.HiveMindSessionSnapshot{
		ObservedAt: sessions.ObservedAt,
		Sessions:   sessions.Sessions,
		TotalCount: sessions.TotalCount,
		HasMore:    sessions.HasMore,
	})
	m.mu.Lock()
	m.sessionsAt[box.ID] = time.Now()
	m.mu.Unlock()
	return nil
}

func (m *Monitor) sessionsDue(sandboxID string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.sessionsAt[sandboxID].Add(sessionRefreshInterval).After(now)
}

func (m *Monitor) token(ctx context.Context, box *host.Sandbox) (string, error) {
	now := time.Now()
	m.mu.Lock()
	cached := m.tokens[box.ID]
	m.mu.Unlock()
	if cached.value != "" && cached.expiresAt.After(now.Add(5*time.Minute)) {
		return cached.value, nil
	}

	idToken, err := m.identity.Issue(ctx, box, m.audience)
	if err != nil {
		return "", fmt.Errorf("mint identity: %w", err)
	}
	body, err := json.Marshal(map[string]string{"id_token": idToken.JWT})
	if err != nil {
		return "", err
	}
	var exchange exchangeResponse
	if err := m.post(ctx, "/v1/auth/actions/exchange", "", body, &exchange); err != nil {
		return "", fmt.Errorf("exchange identity: %w", err)
	}
	if exchange.Token == "" || exchange.ExpiresAt <= now.Unix() {
		return "", fmt.Errorf("exchange identity: HiveMind returned an invalid token")
	}
	cached = cachedToken{
		value:     exchange.Token,
		expiresAt: time.Unix(exchange.ExpiresAt, 0),
	}
	m.mu.Lock()
	m.tokens[box.ID] = cached
	m.mu.Unlock()
	return cached.value, nil
}

func (m *Monitor) post(
	ctx context.Context,
	path string,
	bearer string,
	body []byte,
	out any,
) error {
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, m.apiBase+path, bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if m.userAgent != "" {
		request.Header.Set("User-Agent", m.userAgent)
	}
	response, err := m.http.Do(request)
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
