package ghuser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestManagerAuthorizesOneRepositoryAndRetainsTokensOnGateway(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var repositoryID string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/device/code", func(w http.ResponseWriter, r *http.Request) {
		assertForm(t, r, "client_id", "Iv23liTEST")
		fmt.Fprint(w, `{"device_code":"secret-device-code","user_code":"ABCD-EFGH","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`)
	})
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		repositoryID = r.Form.Get("repository_id")
		mu.Unlock()
		fmt.Fprint(w, `{"access_token":"ghu_user_secret","expires_in":28800,"refresh_token":"ghr_refresh_secret","refresh_token_expires_in":15897600}`)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ghu_user_secret" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, `{"id":7}`)
	})
	mux.HandleFunc("GET /user/installations/42/repositories", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":1,"repositories":[{"id":99}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := NewClient(Config{
		ClientID: "Iv23liTEST", HTTPClient: srv.Client(), CodeURL: srv.URL + "/login/device/code",
		TokenURL: srv.URL + "/login/oauth/access_token", APIURL: srv.URL, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(t.TempDir(), "grants.db"), DeriveKEK([]byte("test signing material")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mgr := NewManager(client, store, nil)
	subject := Subject{Owner: "alice", GitHubID: 7, InstallationID: 42, RepoID: 99, Slug: "wandb/hivemind"}

	started, err := mgr.Start(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	if started.UserCode != "ABCD-EFGH" || started.ID == "" {
		t.Fatalf("start = %+v", started)
	}
	status, err := mgr.Poll(context.Background(), "alice", started.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "authorized" {
		t.Fatalf("state = %q", status.State)
	}
	mu.Lock()
	gotRepositoryID := repositoryID
	mu.Unlock()
	if gotRepositoryID != "99" {
		t.Fatalf("repository_id = %q, want 99", gotRepositoryID)
	}

	// The credential path knows only the attached slug; the immutable id stays
	// in the encrypted gateway record and is not re-resolved from GitHub.
	tok, ok, err := mgr.Token(context.Background(), Subject{
		Owner: "alice", GitHubID: 7, InstallationID: 42, Slug: "WandB/HiveMind",
	})
	if err != nil || !ok || tok.AccessToken != "ghu_user_secret" {
		t.Fatalf("Token = (%+v, %v, %v)", tok, ok, err)
	}

	var access, refresh []byte
	if err := store.db.QueryRow(`SELECT access_token, refresh_token FROM github_user_grants`).Scan(&access, &refresh); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(access, []byte("ghu_user_secret")) || bytes.Contains(refresh, []byte("ghr_refresh_secret")) {
		t.Fatal("tokens were stored in plaintext")
	}
}

func TestManagerMissingGrantIsBotFallback(t *testing.T) {
	client, err := NewClient(Config{ClientID: "Iv23liTEST"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(t.TempDir(), "grants.db"), DeriveKEK([]byte("test signing material")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	_, ok, err := NewManager(client, store, nil).Token(context.Background(), Subject{
		Owner: "alice", GitHubID: 7, InstallationID: 42, Slug: "wandb/other",
	})
	if err != nil || ok {
		t.Fatalf("missing grant = (ok %v, err %v), want bot fallback", ok, err)
	}
}

func TestManagerDropsGrantWhenInstallationBindingChanges(t *testing.T) {
	client, err := NewClient(Config{ClientID: "Iv23liTEST"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(t.TempDir(), "grants.db"), DeriveKEK([]byte("test signing material")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Put(Grant{
		Owner: "alice", GitHubID: 7, InstallationID: 42, RepoID: 99, Slug: "wandb/hivemind",
		Token: Token{AccessToken: "ghu_old", RefreshToken: "ghr_old",
			AccessExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(24 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}
	_, ok, err := NewManager(client, store, nil).Token(context.Background(), Subject{
		Owner: "alice", GitHubID: 7, InstallationID: 43, Slug: "wandb/hivemind",
	})
	if err != nil || ok {
		t.Fatalf("changed installation = (ok %v, err %v), want bot fallback", ok, err)
	}
	if _, err := store.GetBySlug("alice", "wandb/hivemind"); !errors.Is(err, ErrNoGrant) {
		t.Fatalf("stale grant still present: %v", err)
	}
}

func TestVerifyRefusesAUserTokenThatExposesMoreThanRequestedRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"id":7}`) })
	mux.HandleFunc("GET /user/installations/42/repositories", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":2,"repositories":[{"id":99},{"id":100}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := NewClient(Config{ClientID: "Iv23liTEST", HTTPClient: srv.Client(), APIURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Verify(context.Background(), "ghu_secret", 7, 42, 99); err == nil || !errors.Is(err, ErrWrongScope) {
		t.Fatalf("Verify error = %v, want ErrWrongScope", err)
	}
}

func assertForm(t *testing.T, r *http.Request, key, want string) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatal(err)
	}
	if got := values.Get(key); got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}
