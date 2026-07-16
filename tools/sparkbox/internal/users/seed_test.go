package users

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serveGitHubKeys points githubKeysURL at a local server that mimics GitHub's
// /<login>.keys endpoint, and returns the paths it was asked for. GitHub serves
// keys at a path with no leading directory — "/vanpelt.keys" — so a handler
// registered at that exact path only fires if we format the URL correctly.
func serveGitHubKeys(t *testing.T, keysFor map[string]string) *[]string {
	t.Helper()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path)
		login, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".keys")
		if !ok {
			http.NotFound(w, r) // any other shape is a 404, exactly as github.com replies
			return
		}
		keys, ok := keysFor[login]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, keys)
	}))
	t.Cleanup(srv.Close)

	prev := githubKeysURL
	githubKeysURL = srv.URL + "/%s.keys"
	t.Cleanup(func() { githubKeysURL = prev })
	return &got
}

// TestGitHubKeysURLShape is the regression test for the bug this file's seam
// exists for: the URL read github.com/%s/.keys, which 404s for every real
// account, so GitHub verification silently never worked and no CEL policy on
// claims.github could ever match.
func TestGitHubKeysURLShape(t *testing.T) {
	if want := "https://github.com/%s.keys"; githubKeysURL != want {
		t.Fatalf("githubKeysURL = %q, want %q — the login and .keys are one path segment", githubKeysURL, want)
	}
	if url := fmt.Sprintf(githubKeysURL, "vanpelt"); url != "https://github.com/vanpelt.keys" {
		t.Errorf("formatted URL = %q, want https://github.com/vanpelt.keys", url)
	}
}

func TestVerifyGitHubKey(t *testing.T) {
	mine, mineLine := newKey(t, "mine@laptop")
	theirs, _ := newKey(t, "someone-else")
	other, otherLine := newKey(t, "mine@desktop")

	paths := serveGitHubKeys(t, map[string]string{"vanpelt": mineLine + "\n" + otherLine + "\n"})

	t.Run("listed key verifies", func(t *testing.T) {
		ok, err := VerifyGitHubKey(context.Background(), "vanpelt", mine)
		if err != nil {
			t.Fatalf("VerifyGitHubKey: %v", err)
		}
		if !ok {
			t.Error("a key GitHub lists for the account did not verify")
		}
	})

	t.Run("second listed key verifies", func(t *testing.T) {
		ok, err := VerifyGitHubKey(context.Background(), "vanpelt", other)
		if err != nil {
			t.Fatalf("VerifyGitHubKey: %v", err)
		}
		if !ok {
			t.Error("only the first key in the list verified")
		}
	})

	t.Run("unlisted key does not verify", func(t *testing.T) {
		ok, err := VerifyGitHubKey(context.Background(), "vanpelt", theirs)
		if err != nil {
			t.Fatalf("VerifyGitHubKey: %v", err)
		}
		if ok {
			t.Error("a key GitHub does not list verified — this would let anyone claim any account")
		}
	})

	t.Run("requests the bare .keys path", func(t *testing.T) {
		for _, p := range *paths {
			if p != "/vanpelt.keys" {
				t.Errorf("requested %q, want /vanpelt.keys", p)
			}
		}
	})

	t.Run("unknown user is an error, not a false negative", func(t *testing.T) {
		_, err := VerifyGitHubKey(context.Background(), "nosuchuser", mine)
		if err == nil {
			t.Fatal("want an error for a 404, got nil")
		}
		if !strings.Contains(err.Error(), "no such github user") {
			t.Errorf("err = %v, want it to name the missing user", err)
		}
	})

	t.Run("invalid login is rejected before any request", func(t *testing.T) {
		if _, err := VerifyGitHubKey(context.Background(), "bad/../login", mine); err == nil {
			t.Error("want an error for a login that would escape the URL path")
		}
	})
}

func TestFetchGitHubKeys(t *testing.T) {
	_, oneLine := newKey(t, "laptop")
	_, twoLine := newKey(t, "desktop")
	serveGitHubKeys(t, map[string]string{"vanpelt": oneLine + "\n" + twoLine + "\n"})

	keys, err := FetchGitHubKeys(context.Background(), "vanpelt")
	if err != nil {
		t.Fatalf("FetchGitHubKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys, want 2", len(keys))
	}

	if _, err := FetchGitHubKeys(context.Background(), "nosuchuser"); err == nil {
		t.Error("want an error for an account GitHub does not serve")
	}
}
