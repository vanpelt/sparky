// Package hivemindpresence protects VMs whose HiveMind agent sessions are live.
package hivemindpresence

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
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
	// Client, when set, is used instead of building one from the fields above.
	// A gateway that also answers `ctl sessions` passes the same client to both
	// so the two share one exchange cache; a node leaves it nil.
	Client *Client
}

type Monitor struct {
	client    *Client
	boxes     Sandboxes
	protector Protector
	observer  Observer
	log       *slog.Logger

	mu sync.Mutex
	// sessionsAt is the last successful catalog refresh per sandbox. Protection
	// is polled every minute, but the expensive paginated history/count query is
	// deliberately much less frequent.
	sessionsAt map[string]time.Time
}

func New(opts Options) (*Monitor, error) {
	if opts.Sandboxes == nil || opts.Protector == nil {
		return nil, fmt.Errorf("hivemind presence: sandboxes and protector are required")
	}
	client := opts.Client
	if client == nil {
		var err error
		client, err = NewClient(ClientOptions{
			APIBase: opts.APIBase, Audience: opts.Audience, Identity: opts.Identity,
			HTTPClient: opts.HTTPClient, UserAgent: opts.UserAgent,
		})
		if err != nil {
			return nil, err
		}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Monitor{
		client:     client,
		boxes:      opts.Sandboxes,
		protector:  opts.Protector,
		observer:   opts.Observer,
		log:        logger,
		sessionsAt: map[string]time.Time{},
	}, nil
}

// Client is the authenticated HiveMind client this monitor polls with, so a
// gateway can hand the same one to its control plane.
func (m *Monitor) Client() *Client { return m.client }

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
	m.client.Retain(liveIDs)
	m.mu.Lock()
	for sandboxID := range m.sessionsAt {
		if _, exists := liveIDs[sandboxID]; !exists {
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
	presence, err := m.client.Presence(ctx, box)
	if err != nil {
		return err
	}
	if presence.ProtectUntil != nil && presence.ProtectUntil.After(time.Now()) {
		m.protector.ProtectUntil(box.ID, presence.ProtectUntil.UTC())
	}
	if m.observer == nil || !m.sessionsDue(box.ID, time.Now()) {
		return nil
	}

	snapshot, err := m.client.Sessions(ctx, box, maxPageSize)
	if err != nil {
		return err
	}
	m.observer.ObserveHiveMindSessions(box.ID, snapshot)
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
