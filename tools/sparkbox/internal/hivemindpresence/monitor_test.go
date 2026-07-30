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

func TestPollExchangesOnceAndRefreshesLease(t *testing.T) {
	var mu sync.Mutex
	exchanges := 0
	queries := 0
	protectUntil := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
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
		case "/v1/integrations/sparkbox/presence":
			mu.Lock()
			queries++
			mu.Unlock()
			if got := r.Header.Get("Authorization"); got != "Bearer hivemind-token" {
				t.Errorf("authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"protect_until": protectUntil,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	identity := &fakeIdentity{}
	protector := &fakeProtector{leases: map[string]time.Time{}}
	monitor, err := New(Options{
		APIBase: server.URL, Audience: "hivemind",
		Sandboxes: fakeBoxes{boxes: []*host.Sandbox{
			{ID: "box-running", Name: "dev", State: vmm.StateRunning},
			{ID: "box-paused", Name: "sleeping", State: vmm.StatePaused},
		}},
		Protector: protector, Identity: identity,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	monitor.Poll(context.Background())
	monitor.Poll(context.Background())

	mu.Lock()
	gotExchanges, gotQueries := exchanges, queries
	mu.Unlock()
	if gotExchanges != 1 || identity.calls != 1 {
		t.Fatalf("exchange/identity calls = %d/%d, want 1/1", gotExchanges, identity.calls)
	}
	if gotQueries != 2 {
		t.Fatalf("presence queries = %d, want 2", gotQueries)
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
