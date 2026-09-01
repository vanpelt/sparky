package ghuser

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type Subject struct {
	Owner          string
	GitHubID       int64
	InstallationID int64
	RepoID         int64
	Slug           string
}

type AuthorizationStart struct {
	ID              string    `json:"id"`
	UserCode        string    `json:"user_code"`
	VerificationURI string    `json:"verification_uri"`
	IntervalSeconds int       `json:"interval_seconds"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type AuthorizationStatus struct {
	State string `json:"state"`
	Slug  string `json:"slug,omitempty"`
}

type flow struct {
	subject Subject
	code    DeviceCode
	next    time.Time
}

type Manager struct {
	client    *Client
	store     *Store
	log       *slog.Logger
	now       func() time.Time
	mu        sync.Mutex
	flows     map[string]flow
	refreshMu sync.Mutex
}

func NewManager(client *Client, store *Store, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{client: client, store: store, log: log, now: client.now, flows: map[string]flow{}}
}

func (m *Manager) Start(ctx context.Context, subject Subject) (AuthorizationStart, error) {
	if subject.Owner == "" || subject.GitHubID <= 0 || subject.InstallationID <= 0 || subject.RepoID <= 0 || subject.Slug == "" {
		return AuthorizationStart{}, errors.New("incomplete github authorization subject")
	}
	dc, err := m.client.Start(ctx)
	if err != nil {
		return AuthorizationStart{}, err
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return AuthorizationStart{}, err
	}
	id := base64.RawURLEncoding.EncodeToString(raw)
	m.mu.Lock()
	for key, existing := range m.flows {
		if m.now().After(existing.code.ExpiresAt) {
			delete(m.flows, key)
		}
	}
	m.flows[id] = flow{subject: subject, code: dc, next: m.now()}
	m.mu.Unlock()
	return AuthorizationStart{ID: id, UserCode: dc.UserCode, VerificationURI: dc.VerificationURI,
		IntervalSeconds: int(dc.Interval / time.Second), ExpiresAt: dc.ExpiresAt}, nil
}

func (m *Manager) Poll(ctx context.Context, owner, id string) (AuthorizationStatus, error) {
	m.mu.Lock()
	f, ok := m.flows[id]
	if !ok || f.subject.Owner != owner {
		m.mu.Unlock()
		return AuthorizationStatus{}, ErrExpired
	}
	now := m.now()
	if now.After(f.code.ExpiresAt) {
		delete(m.flows, id)
		m.mu.Unlock()
		return AuthorizationStatus{}, ErrExpired
	}
	if now.Before(f.next) {
		m.mu.Unlock()
		return AuthorizationStatus{State: "pending", Slug: f.subject.Slug}, nil
	}
	f.next = now.Add(f.code.Interval)
	m.flows[id] = f
	m.mu.Unlock()
	tok, err := m.client.Poll(ctx, f.code, f.subject.RepoID)
	if errors.Is(err, ErrPending) {
		return AuthorizationStatus{State: "pending", Slug: f.subject.Slug}, nil
	}
	if errors.Is(err, ErrSlowDown) {
		m.mu.Lock()
		current := m.flows[id]
		current.code.Interval += 5 * time.Second
		current.next = now.Add(current.code.Interval)
		m.flows[id] = current
		m.mu.Unlock()
		return AuthorizationStatus{State: "pending", Slug: f.subject.Slug}, nil
	}
	if err != nil {
		m.mu.Lock()
		delete(m.flows, id)
		m.mu.Unlock()
		return AuthorizationStatus{}, err
	}
	if err := m.client.Verify(ctx, tok.AccessToken, f.subject.GitHubID, f.subject.InstallationID, f.subject.RepoID); err != nil {
		m.mu.Lock()
		delete(m.flows, id)
		m.mu.Unlock()
		return AuthorizationStatus{}, err
	}
	grant := Grant{Owner: f.subject.Owner, GitHubID: f.subject.GitHubID,
		InstallationID: f.subject.InstallationID, RepoID: f.subject.RepoID, Slug: f.subject.Slug, Token: tok}
	if err := m.store.Put(grant); err != nil {
		m.mu.Lock()
		delete(m.flows, id)
		m.mu.Unlock()
		return AuthorizationStatus{}, err
	}
	m.mu.Lock()
	delete(m.flows, id)
	m.mu.Unlock()
	m.log.Info("github repository authorized by user", "owner", f.subject.Owner,
		"github_id", f.subject.GitHubID, "repo", f.subject.Slug, "expires_at", tok.AccessExpiresAt)
	return AuthorizationStatus{State: "authorized", Slug: f.subject.Slug}, nil
}

// Token returns false for an absent grant so callers can deliberately fall
// back to an installation token. Invalid or expired refresh grants are deleted
// and also become a bot fallback rather than breaking repository access.
func (m *Manager) Token(ctx context.Context, subject Subject) (Token, bool, error) {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	var g Grant
	var err error
	if subject.RepoID > 0 {
		g, err = m.store.Get(subject.Owner, subject.RepoID)
	} else {
		g, err = m.store.GetBySlug(subject.Owner, subject.Slug)
	}
	if errors.Is(err, ErrNoGrant) {
		return Token{}, false, nil
	}
	if err != nil {
		m.log.Warn("github user grant unavailable; using installation token",
			"owner", subject.Owner, "repo", subject.Slug, "err", err)
		return Token{}, false, err
	}
	if g.GitHubID != subject.GitHubID || g.InstallationID != subject.InstallationID || !strings.EqualFold(g.Slug, subject.Slug) {
		_ = m.store.Delete(subject.Owner, g.RepoID)
		return Token{}, false, nil
	}
	if m.now().Add(5 * time.Minute).Before(g.Token.AccessExpiresAt) {
		return g.Token, true, nil
	}
	if !m.now().Before(g.Token.RefreshExpiresAt) {
		_ = m.store.Delete(subject.Owner, g.RepoID)
		return Token{}, false, nil
	}
	tok, err := m.client.Refresh(ctx, g.Token.RefreshToken)
	if err != nil {
		if errors.Is(err, ErrBadRefresh) {
			_ = m.store.Delete(subject.Owner, g.RepoID)
			return Token{}, false, nil
		}
		m.log.Warn("github user grant could not be refreshed; using installation token",
			"owner", subject.Owner, "repo", subject.Slug, "err", err)
		return Token{}, false, fmt.Errorf("refresh github user token: %w", err)
	}
	if err := m.client.Verify(ctx, tok.AccessToken, subject.GitHubID, subject.InstallationID, g.RepoID); err != nil {
		_ = m.store.Delete(subject.Owner, g.RepoID)
		m.log.Warn("refreshed github user grant failed verification; using installation token",
			"owner", subject.Owner, "repo", subject.Slug, "err", err)
		return Token{}, false, err
	}
	g.Token = tok
	if err := m.store.Put(g); err != nil {
		return Token{}, false, err
	}
	return tok, true, nil
}
