package hivemindpresence

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

type fakeBoxes struct {
	boxes []*host.Sandbox
}

func (f fakeBoxes) List() []*host.Sandbox { return f.boxes }

type fakeIdentity struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeIdentity) Issue(
	_ context.Context,
	box *host.Sandbox,
	audience string,
) (metadata.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return metadata.Token{
		JWT:       "oidc-" + box.ID + "-" + audience,
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

type fakeProtector struct {
	mu     sync.Mutex
	leases map[string]time.Time
}

func (f *fakeProtector) ProtectUntil(id string, until time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases[id] = until
}

type fakeObserver struct {
	mu        sync.Mutex
	snapshots map[string]host.HiveMindSessionSnapshot
}

func (f *fakeObserver) ObserveHiveMindSessions(
	id string,
	snapshot host.HiveMindSessionSnapshot,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots[id] = snapshot
}

func TestNewRejectsInsecureRemoteAPIBase(t *testing.T) {
	_, err := New(Options{
		APIBase:   "http://hivemind.example",
		Sandboxes: fakeBoxes{}, Protector: &fakeProtector{}, Identity: &fakeIdentity{},
	})
	if err == nil {
		t.Fatal("New accepted a remote plaintext API base")
	}
}

func TestNewAllowsLoopbackHTTPForTests(t *testing.T) {
	_, err := New(Options{
		APIBase:   "http://127.0.0.1:8080",
		Sandboxes: fakeBoxes{}, Protector: &fakeProtector{}, Identity: &fakeIdentity{},
	})
	if err != nil {
		t.Fatalf("New rejected loopback HTTP: %v", err)
	}
}

func TestPollExchangesOnceAndRefreshesLease(t *testing.T) {
	var mu sync.Mutex
	exchanges := 0
	presenceQueries := 0
	sessionQueries := 0
	protectUntil := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	observedAt := time.Now().UTC().Truncate(time.Second)
	startedAt := observedAt.Add(-time.Hour)
	endedAt := observedAt.Add(-5 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/actions/exchange":
			mu.Lock()
			exchanges++
			mu.Unlock()
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["id_token"] == "" {
				t.Error("exchange received no id_token")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "hivemind-token", "expires_at": time.Now().Add(time.Hour).Unix(),
			})
		case "/v1/integrations/runtime/presence":
			mu.Lock()
			presenceQueries++
			mu.Unlock()
			if got := r.Header.Get("Authorization"); got != "Bearer hivemind-token" {
				t.Errorf("authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"observed_at":   observedAt,
				"protect_until": protectUntil,
			})
		case "/v1/integrations/runtime/sessions":
			mu.Lock()
			sessionQueries++
			mu.Unlock()
			if got := r.URL.Query().Get("page_size"); got != "100" {
				t.Errorf("page_size = %q, want 100", got)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer hivemind-token" {
				t.Errorf("authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"observed_at": observedAt,
				"total_count": 1,
				"has_more":    true,
				"sessions": []map[string]any{{
					"id": "session-1", "title": "Fix session listing",
					"url":   "https://hivemind.example/sessions/session-1",
					"state": "ended", "agent_type": "codex", "model": "gpt-5",
					"started_at":       startedAt,
					"ended_at":         endedAt,
					"last_activity_at": observedAt,
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	identity := &fakeIdentity{}
	protector := &fakeProtector{leases: map[string]time.Time{}}
	observer := &fakeObserver{snapshots: map[string]host.HiveMindSessionSnapshot{}}
	monitor, err := New(Options{
		APIBase: server.URL, Audience: "hivemind",
		Sandboxes: fakeBoxes{boxes: []*host.Sandbox{
			{ID: "box-running", Name: "dev", State: vmm.StateRunning},
			{ID: "box-paused", Name: "sleeping", State: vmm.StatePaused},
		}},
		Protector: protector, Observer: observer, Identity: identity,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	monitor.Poll(context.Background())
	monitor.Poll(context.Background())

	mu.Lock()
	gotExchanges := exchanges
	gotPresenceQueries, gotSessionQueries := presenceQueries, sessionQueries
	mu.Unlock()
	if gotExchanges != 1 || identity.calls != 1 {
		t.Fatalf("exchange/identity calls = %d/%d, want 1/1", gotExchanges, identity.calls)
	}
	if gotPresenceQueries != 2 {
		t.Fatalf("presence queries = %d, want 2", gotPresenceQueries)
	}
	if gotSessionQueries != 1 {
		t.Fatalf("session queries = %d, want 1", gotSessionQueries)
	}
	protector.mu.Lock()
	lease, ok := protector.leases["box-running"]
	_, pausedProtected := protector.leases["box-paused"]
	protector.mu.Unlock()
	if !ok || !lease.Equal(protectUntil) {
		t.Fatalf("running lease = %v, want %v", lease, protectUntil)
	}
	if pausedProtected {
		t.Fatal("paused sandbox was queried or protected")
	}
	observer.mu.Lock()
	snapshot, observed := observer.snapshots["box-running"]
	observer.mu.Unlock()
	if !observed || len(snapshot.Sessions) != 1 {
		t.Fatalf("session snapshot = %+v, observed %v", snapshot, observed)
	}
	got := snapshot.Sessions[0]
	if got.ID != "session-1" ||
		got.Title != "Fix session listing" ||
		got.URL != "https://hivemind.example/sessions/session-1" ||
		got.State != "ended" ||
		got.AgentType != "codex" ||
		got.Model != "gpt-5" ||
		!got.StartedAt.Equal(startedAt) ||
		got.EndedAt == nil ||
		!got.EndedAt.Equal(endedAt) ||
		!got.LastActivityAt.Equal(observedAt) {
		t.Fatalf("session = %+v", got)
	}
	if !snapshot.ObservedAt.Equal(observedAt) || snapshot.TotalCount != 1 || !snapshot.HasMore {
		t.Fatalf("snapshot metadata = %+v", snapshot)
	}

	monitor.mu.Lock()
	monitor.sessionsAt["box-running"] = time.Now().Add(-sessionRefreshInterval)
	monitor.mu.Unlock()
	monitor.Poll(context.Background())
	mu.Lock()
	gotPresenceQueries, gotSessionQueries = presenceQueries, sessionQueries
	mu.Unlock()
	if gotPresenceQueries != 3 || gotSessionQueries != 2 {
		t.Fatalf("queries after catalog expiry = presence %d, sessions %d, want 3/2",
			gotPresenceQueries, gotSessionQueries)
	}
}

func TestPollDoesNotProtectOnQueryFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/actions/exchange" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "hivemind-token", "expires_at": time.Now().Add(time.Hour).Unix(),
			})
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	protector := &fakeProtector{leases: map[string]time.Time{}}
	monitor, err := New(Options{
		APIBase: server.URL, Audience: "hivemind",
		Sandboxes: fakeBoxes{boxes: []*host.Sandbox{
			{ID: "box-running", Name: "dev", State: vmm.StateRunning},
		}},
		Protector: protector, Identity: &fakeIdentity{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	monitor.Poll(context.Background())
	if len(protector.leases) != 0 {
		t.Fatalf("failed query installed leases: %v", protector.leases)
	}
}

func TestSessionCatalogFailureDoesNotDiscardPresenceLease(t *testing.T) {
	protectUntil := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/actions/exchange":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "hivemind-token", "expires_at": time.Now().Add(time.Hour).Unix(),
			})
		case "/v1/integrations/runtime/presence":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"observed_at": time.Now().UTC(), "protect_until": protectUntil,
			})
		case "/v1/integrations/runtime/sessions":
			http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	protector := &fakeProtector{leases: map[string]time.Time{}}
	monitor, err := New(Options{
		APIBase: server.URL, Audience: "hivemind",
		Sandboxes: fakeBoxes{boxes: []*host.Sandbox{
			{ID: "box-running", Name: "dev", State: vmm.StateRunning},
		}},
		Protector: protector, Observer: &fakeObserver{snapshots: map[string]host.HiveMindSessionSnapshot{}},
		Identity: &fakeIdentity{},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	monitor.Poll(context.Background())

	protector.mu.Lock()
	lease, ok := protector.leases["box-running"]
	protector.mu.Unlock()
	if !ok || !lease.Equal(protectUntil) {
		t.Fatalf("presence lease = %v, want %v", lease, protectUntil)
	}
}
