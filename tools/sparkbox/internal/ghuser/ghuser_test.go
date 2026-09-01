package ghuser

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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
	var tokenForm url.Values
	var scopeRequest struct {
		AccessToken   string            `json:"access_token"`
		Target        string            `json:"target"`
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}
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
		tokenForm, _ = url.ParseQuery(r.Form.Encode())
		mu.Unlock()
		fmt.Fprint(w, `{"access_token":"ghu_broad_secret","expires_in":28800,"refresh_token":"ghr_refresh_secret","refresh_token_expires_in":15897600}`)
	})
	mux.HandleFunc("POST /applications/Iv23liTEST/token/scoped", func(w http.ResponseWriter, r *http.Request) {
		clientID, clientSecret, ok := r.BasicAuth()
		if !ok || clientID != "Iv23liTEST" || clientSecret != "client-secret" {
			t.Errorf("scoped token basic auth = (%q, %q, %v)", clientID, clientSecret, ok)
		}
		mu.Lock()
		defer mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&scopeRequest); err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(w, `{"token":"ghu_scoped_secret","expires_at":"2026-09-01T20:00:00Z"}`)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		if got != "Bearer ghu_broad_secret" && got != "Bearer ghu_scoped_secret" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, `{"id":7}`)
	})
	mux.HandleFunc("GET /user/installations/42/repositories", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ghu_scoped_secret" {
			t.Errorf("repository listing Authorization = %q", got)
		}
		fmt.Fprint(w, `{"total_count":1,"repositories":[{"id":99}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client, err := NewClient(Config{
		ClientID: "Iv23liTEST", ClientSecret: "client-secret", HTTPClient: srv.Client(), CodeURL: srv.URL + "/login/device/code",
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
	subject := Subject{Owner: "alice", GitHubID: 7, InstallationID: 42, RepoID: 99, Slug: "wandb/hivemind",
		Target: "wandb", Permissions: map[string]string{"contents": "write", "pull_requests": "write"}}

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
	gotTokenForm := tokenForm
	gotScope := scopeRequest
	mu.Unlock()
	if gotTokenForm.Get("repository_id") != "" {
		t.Fatalf("broad token exchange unexpectedly requested repository_id: %v", gotTokenForm)
	}
	if gotScope.AccessToken != "ghu_broad_secret" || gotScope.Target != "wandb" ||
		len(gotScope.RepositoryIDs) != 1 || gotScope.RepositoryIDs[0] != 99 ||
		gotScope.Permissions["contents"] != "write" || gotScope.Permissions["pull_requests"] != "write" {
		t.Fatalf("scoped token request = %+v", gotScope)
	}

	// The credential path knows only the attached slug; the immutable id stays
	// in the encrypted gateway record and is not re-resolved from GitHub.
	tok, ok, err := mgr.Token(context.Background(), Subject{
		Owner: "alice", GitHubID: 7, InstallationID: 42, Slug: "WandB/HiveMind",
	})
	if err != nil || !ok || tok.AccessToken != "ghu_scoped_secret" {
		t.Fatalf("Token = (%+v, %v, %v)", tok, ok, err)
	}

	var access, refresh []byte
	if err := store.db.QueryRow(`SELECT access_token, refresh_token FROM github_scoped_user_grants`).Scan(&access, &refresh); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(access, []byte("ghu_scoped_secret")) || bytes.Contains(access, []byte("ghu_broad_secret")) ||
		bytes.Contains(refresh, []byte("ghr_refresh_secret")) {
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

func TestManagerWebFlowUsesPKCEAndBindsStateToOwner(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	var authorizeQuery url.Values
	var tokenForm url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		tokenForm, _ = url.ParseQuery(r.Form.Encode())
		fmt.Fprint(w, `{"access_token":"ghu_web_broad","expires_in":28800,"refresh_token":"ghr_web_secret","refresh_token_expires_in":15897600}`)
	})
	mux.HandleFunc("POST /applications/Iv23liTEST/token/scoped", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"ghu_web_scoped","expires_at":"2026-09-01T20:00:00Z"}`)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"id":7}`) })
	mux.HandleFunc("GET /user/installations/42/repositories", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":1,"repositories":[{"id":99}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := NewClient(Config{
		ClientID: "Iv23liTEST", ClientSecret: "client-secret", HTTPClient: srv.Client(),
		AuthorizeURL: srv.URL + "/login/oauth/authorize", TokenURL: srv.URL + "/login/oauth/access_token",
		APIURL: srv.URL, Now: func() time.Time { return now },
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
	subject := Subject{Owner: "alice", GitHubID: 7, InstallationID: 42, RepoID: 99, Slug: "wandb/hivemind",
		Target: "wandb", Permissions: map[string]string{"contents": "write"}}
	redirectURI := "https://my.example.test/github/repo/callback"
	location, err := mgr.StartWeb(subject, redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	authorize, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	authorizeQuery = authorize.Query()
	if authorizeQuery.Get("client_id") != "Iv23liTEST" || authorizeQuery.Get("redirect_uri") != redirectURI ||
		authorizeQuery.Get("state") == "" || authorizeQuery.Get("code_challenge") == "" ||
		authorizeQuery.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorize query = %v", authorizeQuery)
	}
	state := authorizeQuery.Get("state")
	if _, err := mgr.FinishWeb(context.Background(), "mallory", state, "oauth-code"); !errors.Is(err, ErrExpired) {
		t.Fatalf("cross-owner callback error = %v, want ErrExpired", err)
	}
	// The mismatched callback consumed the one-time state, so begin again.
	location, err = mgr.StartWeb(subject, redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	authorize, _ = url.Parse(location)
	status, err := mgr.FinishWeb(context.Background(), "alice", authorize.Query().Get("state"), "oauth-code")
	if err != nil || status.State != "authorized" {
		t.Fatalf("FinishWeb = (%+v, %v)", status, err)
	}
	if tokenForm.Get("client_secret") != "client-secret" || tokenForm.Get("code") != "oauth-code" ||
		tokenForm.Get("redirect_uri") != redirectURI || tokenForm.Get("repository_id") != "" ||
		tokenForm.Get("code_verifier") == "" {
		t.Fatalf("token exchange form = %v", tokenForm)
	}
	digest := sha256.Sum256([]byte(tokenForm.Get("code_verifier")))
	wantChallenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if got := authorize.Query().Get("code_challenge"); got != wantChallenge {
		t.Fatalf("PKCE challenge = %q, want %q", got, wantChallenge)
	}
	if !mgr.Authorized("alice", "WandB/HiveMind", 7) {
		t.Fatal("stored web grant was not reported as authorized")
	}
	if _, err := mgr.FinishWeb(context.Background(), "alice", authorize.Query().Get("state"), "oauth-code"); !errors.Is(err, ErrExpired) {
		t.Fatalf("replayed callback error = %v, want ErrExpired", err)
	}
	location, err = mgr.StartWeb(subject, redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	authorize, _ = url.Parse(location)
	state = authorize.Query().Get("state")
	mgr.CancelWeb("alice", state)
	if _, err := mgr.FinishWeb(context.Background(), "alice", state, "oauth-code"); !errors.Is(err, ErrExpired) {
		t.Fatalf("declined callback state error = %v, want ErrExpired", err)
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

func TestManagerRefreshesBroadGrantAndRescopesBeforeReturning(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "ghr_old" {
			t.Fatalf("refresh form = %v", r.Form)
		}
		fmt.Fprint(w, `{"access_token":"ghu_refreshed_broad","expires_in":28800,"refresh_token":"ghr_rotated","refresh_token_expires_in":15897600}`)
	})
	mux.HandleFunc("POST /applications/Iv23liTEST/token/scoped", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AccessToken   string  `json:"access_token"`
			Target        string  `json:"target"`
			RepositoryIDs []int64 `json:"repository_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Fatal(err)
		}
		if in.AccessToken != "ghu_refreshed_broad" || in.Target != "wandb" || len(in.RepositoryIDs) != 1 || in.RepositoryIDs[0] != 99 {
			t.Fatalf("scope request = %+v", in)
		}
		fmt.Fprint(w, `{"token":"ghu_refreshed_scoped","expires_at":"2026-09-01T20:00:00Z"}`)
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"id":7}`) })
	mux.HandleFunc("GET /user/installations/42/repositories", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"total_count":1,"repositories":[{"id":99}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := NewClient(Config{ClientID: "Iv23liTEST", ClientSecret: "client-secret", HTTPClient: srv.Client(),
		TokenURL: srv.URL + "/login/oauth/access_token", APIURL: srv.URL, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(t.TempDir(), "grants.db"), DeriveKEK([]byte("test signing material")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Put(Grant{Owner: "alice", GitHubID: 7, InstallationID: 42, RepoID: 99, Slug: "wandb/hivemind",
		Token: Token{AccessToken: "ghu_expired_scoped", RefreshToken: "ghr_old",
			AccessExpiresAt: now.Add(-time.Minute), RefreshExpiresAt: now.Add(24 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	subject := Subject{Owner: "alice", GitHubID: 7, InstallationID: 42, Slug: "wandb/hivemind",
		Target: "wandb", Permissions: map[string]string{"contents": "write"}}
	tok, ok, err := NewManager(client, store, nil).Token(context.Background(), subject)
	if err != nil || !ok || tok.AccessToken != "ghu_refreshed_scoped" || tok.RefreshToken != "ghr_rotated" {
		t.Fatalf("Token = (%+v, %v, %v)", tok, ok, err)
	}
	stored, err := store.GetBySlug("alice", "wandb/hivemind")
	if err != nil || stored.Token.AccessToken != "ghu_refreshed_scoped" || stored.Token.RefreshToken != "ghr_rotated" {
		t.Fatalf("stored grant = (%+v, %v)", stored, err)
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
