// Package hivemindpresence protects VMs whose HiveMind agent sessions are live.
package hivemindpresence

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

const (
	defaultInterval = time.Minute
	maxParallel     = 4
)

type Sandboxes interface {
	List() []*host.Sandbox
}

type Protector interface {
	ProtectUntil(sandboxID string, until time.Time)
}

type Identity interface {
	Issue(ctx context.Context, box *host.Sandbox, aud string) (metadata.Token, error)
}

type Options struct {
	APIBase    string
	Audience   string
	Sandboxes  Sandboxes
	Protector  Protector
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
	identity  Identity
	http      *http.Client
	log       *slog.Logger
	userAgent string

	mu     sync.Mutex
	tokens map[string]cachedToken
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
	ProtectUntil *time.Time `json:"protect_until"`
}

func New(opts Options) (*Monitor, error) {
	if strings.TrimSpace(opts.APIBase) == "" {
		return nil, fmt.Errorf("hivemind presence: API base is required")
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
		apiBase:   strings.TrimRight(opts.APIBase, "/"),
		audience:  opts.Audience,
		boxes:     opts.Sandboxes,
		protector: opts.Protector,
		identity:  opts.Identity,
		http:      client,
		log:       logger,
		userAgent: opts.UserAgent,
		tokens:    map[string]cachedToken{},
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
				m.log.Warn("HiveMind presence check failed",
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
	if err := m.post(ctx, "/v1/integrations/sparkbox/presence", token, []byte("{}"), &presence); err != nil {
		return fmt.Errorf("query: %w", err)
	}
	if presence.ProtectUntil != nil && presence.ProtectUntil.After(time.Now()) {
		m.protector.ProtectUntil(box.ID, presence.ProtectUntil.UTC())
	}
	return nil
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
