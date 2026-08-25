package hivemindpresence

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// clientServer is a stand-in HiveMind that records what it was asked.
type clientServer struct {
	mu        sync.Mutex
	exchanges int
	queries   []string
	bodies    []string
	server    *httptest.Server
}

func newClientServer(t *testing.T) *clientServer {
	t.Helper()
	s := &clientServer{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 512)
		n, _ := r.Body.Read(body)
		s.mu.Lock()
		if r.URL.Path == "/v1/auth/actions/exchange" {
			s.exchanges++
		} else {
			s.queries = append(s.queries, r.URL.RequestURI())
			s.bodies = append(s.bodies, string(body[:n]))
		}
		s.mu.Unlock()

		switch r.URL.Path {
		case "/v1/auth/actions/exchange":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "hm-jwt", "expires_at": time.Now().Add(time.Hour).Unix(),
			})
		case "/v1/integrations/runtime/sessions":
			if got := r.Header.Get("Authorization"); got != "Bearer hm-jwt" {
				t.Errorf("authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"observed_at": time.Now().UTC(),
				"sessions":    []map[string]any{{"id": "s1", "title": "one", "state": "ended"}},
				"total_count": 3, "has_more": true,
			})
		default:
			http.Error(w, "no route", http.StatusNotFound)
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func testClient(t *testing.T, base string) *Client {
	t.Helper()
	c, err := NewClient(ClientOptions{
		APIBase: base, Audience: "https://hivemind.example", Identity: &fakeIdentity{},
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// TestSessionsClampsPageSizeAndReusesTheExchange. The clamp matters because the
// API rejects a page_size above 100 outright; the reuse matters because every
// exchange burns a single-use id token.
func TestSessionsClampsPageSizeAndReusesTheExchange(t *testing.T) {
	srv := newClientServer(t)
	c := testClient(t, srv.server.URL)
	box := &host.Sandbox{ID: "box-1", Name: "alicebox"}

	got, err := c.Sessions(context.Background(), box, 5000)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if got.TotalCount != 3 || !got.HasMore || len(got.Sessions) != 1 {
		t.Errorf("snapshot = %+v", got)
	}
	if _, err := c.Sessions(context.Background(), box, 0); err != nil {
		t.Fatalf("second Sessions: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.exchanges != 1 {
		t.Errorf("exchanges = %d, want 1 — the cached JWT was not reused", srv.exchanges)
	}
	if len(srv.queries) != 2 {
		t.Fatalf("queries = %v", srv.queries)
	}
	if !strings.HasSuffix(srv.queries[0], "page_size=100") {
		t.Errorf("query[0] = %q, want page_size clamped to 100", srv.queries[0])
	}
	if !strings.HasSuffix(srv.queries[1], "page_size=50") {
		t.Errorf("query[1] = %q, want the default page size", srv.queries[1])
	}
	// The request body carries no device selector, and must not grow one: the
	// binding is the token, and a body field would be a second, forgeable one.
	for _, b := range srv.bodies {
		if strings.TrimSpace(b) != "{}" {
			t.Errorf("request body = %q, want an empty object", b)
		}
	}
}

// TestRetainDropsCredentialsForVanishedSandboxes: a destroyed VM's exchanged
// JWT must not sit in memory for the life of the process.
func TestRetainDropsCredentialsForVanishedSandboxes(t *testing.T) {
	srv := newClientServer(t)
	c := testClient(t, srv.server.URL)
	box := &host.Sandbox{ID: "box-1", Name: "alicebox"}

	if _, err := c.Sessions(context.Background(), box, 10); err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	c.Retain(map[string]struct{}{"someone-else": {}})
	if _, err := c.Sessions(context.Background(), box, 10); err != nil {
		t.Fatalf("Sessions after Retain: %v", err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.exchanges != 2 {
		t.Errorf("exchanges = %d, want 2 — the dropped credential was reused", srv.exchanges)
	}
}

func TestNewClientRequiresAnIdentity(t *testing.T) {
	if _, err := NewClient(ClientOptions{APIBase: "https://hivemind.example"}); err == nil {
		t.Fatal("NewClient accepted a nil identity")
	}
}
