package ghuser

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

type webFlow struct {
	subject     Subject
	verifier    string
	redirectURI string
	expiresAt   time.Time
}

type Manager struct {
	client    *Client
	store     *Store
	log       *slog.Logger
	now       func() time.Time
	mu        sync.Mutex
	flows     map[string]flow
	webFlows  map[string]webFlow
	refreshMu sync.Mutex
}

func NewManager(client *Client, store *Store, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{client: client, store: store, log: log, now: client.now,
		flows: map[string]flow{}, webFlows: map[string]webFlow{}}
}

func (m *Manager) WebEnabled() bool { return m != nil && m.client.WebEnabled() }

// StartWeb begins the browser OAuth flow. The verifier remains gateway-only;
// the opaque state is one-time, expires quickly, and is bound to the Sparkbox
// owner who initiated it.
func (m *Manager) StartWeb(subject Subject, redirectURI string) (string, error) {
	if !validSubject(subject) {
		return "", errors.New("incomplete github authorization subject")
	}
	if !m.WebEnabled() {
		return "", errors.New("github web authorization is not enabled")
	}
	stateRaw := make([]byte, 32)
	verifierRaw := make([]byte, 32)
	if _, err := rand.Read(stateRaw); err != nil {
		return "", err
	}
	if _, err := rand.Read(verifierRaw); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(stateRaw)
	verifier := base64.RawURLEncoding.EncodeToString(verifierRaw)
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	location, err := m.client.AuthorizationURL(redirectURI, state, challenge)
	if err != nil {
		return "", err
	}
	now := m.now()
	m.mu.Lock()
	for key, existing := range m.webFlows {
		// Keep only the newest browser attempt for one owner/repository. This
		// also bounds repeated clicks without making the callback state global
		// or guessable.
		if !now.Before(existing.expiresAt) ||
			(existing.subject.Owner == subject.Owner && strings.EqualFold(existing.subject.Slug, subject.Slug)) {
			delete(m.webFlows, key)
		}
	}
	m.webFlows[state] = webFlow{subject: subject, verifier: verifier, redirectURI: redirectURI, expiresAt: now.Add(10 * time.Minute)}
	m.mu.Unlock()
	return location, nil
}

// CancelWeb consumes a declined browser flow without letting another account
// invalidate it. GitHub returns state alongside access_denied but no code.
func (m *Manager) CancelWeb(owner, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.webFlows[state]; ok && f.subject.Owner == owner {
		delete(m.webFlows, state)
	}
}

// FinishWeb consumes a browser OAuth state before exchanging the code. A
// failed exchange cannot be replayed; the user starts a fresh authorization.
func (m *Manager) FinishWeb(ctx context.Context, owner, state, code string) (AuthorizationStatus, error) {
	m.mu.Lock()
	f, ok := m.webFlows[state]
	if ok {
		delete(m.webFlows, state)
	}
	m.mu.Unlock()
	if !ok || f.subject.Owner != owner || !m.now().Before(f.expiresAt) {
		return AuthorizationStatus{}, ErrExpired
	}
	tok, err := m.client.ExchangeCode(ctx, code, f.redirectURI, f.verifier, f.subject.RepoID)
	if err != nil {
		return AuthorizationStatus{}, err
	}
	if err := m.saveVerified(ctx, f.subject, tok); err != nil {
		return AuthorizationStatus{}, err
	}
	return AuthorizationStatus{State: "authorized", Slug: f.subject.Slug}, nil
}

// Authorized reports a usable stored grant without refreshing it or calling
// GitHub. It exists for the console's four-second list poll, not the credential
// path; an expired access token still counts while its refresh grant is alive.
func (m *Manager) Authorized(owner, slug string, githubID int64) bool {
	if m == nil {
		return false
	}
	g, err := m.store.GetBySlug(owner, slug)
	if err != nil || g.GitHubID != githubID || !strings.EqualFold(g.Slug, slug) {
		return false
	}
	return m.now().Before(g.Token.RefreshExpiresAt)
}

func validSubject(subject Subject) bool {
	return subject.Owner != "" && subject.GitHubID > 0 && subject.InstallationID > 0 && subject.RepoID > 0 && subject.Slug != ""
}

func (m *Manager) Start(ctx context.Context, subject Subject) (AuthorizationStart, error) {
	if !validSubject(subject) {
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
	if err := m.saveVerified(ctx, f.subject, tok); err != nil {
		m.mu.Lock()
		delete(m.flows, id)
		m.mu.Unlock()
		return AuthorizationStatus{}, err
	}
	m.mu.Lock()
	delete(m.flows, id)
	m.mu.Unlock()
	return AuthorizationStatus{State: "authorized", Slug: f.subject.Slug}, nil
}

func (m *Manager) saveVerified(ctx context.Context, subject Subject, tok Token) error {
	if err := m.client.Verify(ctx, tok.AccessToken, subject.GitHubID, subject.InstallationID, subject.RepoID); err != nil {
		return err
	}
	grant := Grant{Owner: subject.Owner, GitHubID: subject.GitHubID,
		InstallationID: subject.InstallationID, RepoID: subject.RepoID, Slug: subject.Slug, Token: tok}
	if err := m.store.Put(grant); err != nil {
		return err
	}
	m.log.Info("github repository authorized by user", "owner", subject.Owner,
		"github_id", subject.GitHubID, "repo", subject.Slug, "expires_at", tok.AccessExpiresAt)
	return nil
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
