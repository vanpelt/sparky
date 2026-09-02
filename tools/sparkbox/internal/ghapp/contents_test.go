package ghapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// contentsStub registers the two endpoints every ReadFile call touches — the
// mint and the contents read — and hands back the raw query the read carried,
// which the recorded `call` does not keep.
type contentsStub struct {
	*stub
	rawQuery string
	accept   string
}

func newContentsStub(t *testing.T, h http.HandlerFunc) *contentsStub {
	t.Helper()
	cs := &contentsStub{stub: newStub(t)}
	cs.json("POST /app/installations/42/access_tokens", 200, tokenBody)
	cs.handle("GET /repos/wandb/hivemind/contents/", func(w http.ResponseWriter, r *http.Request) {
		cs.rawQuery = r.URL.RawQuery
		cs.accept = r.Header.Get("Accept")
		h(w, r)
	})
	return cs
}

// raw answers the way github answers a file under the raw media type: the body
// IS the file, and the content type is not the JSON envelope.
func raw(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(200)
		w.Write(body) //nolint:errcheck
	}
}

var testInst = Installation{ID: 42, AccountID: 800, AccountLogin: "wandb", AccountType: "Organization",
	Permissions: map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write"}}

const setupScript = "#!/usr/bin/env bash\nset -euo pipefail\npnpm install\n"

// The happy path, and the security posture around it: the bytes come back
// exactly as sent, the read presents the MINTED token rather than the app
// assertion, and the credential it presents was asked for read-only on one
// repository even though this installation holds write on three permissions.
func TestReadFileReturnsTheBytesUnderAOneRepoReadOnlyToken(t *testing.T) {
	s := newContentsStub(t, raw([]byte(setupScript)))
	app := newApp(t, s.stub, nil)

	got, err := app.ReadFile(context.Background(), testInst, "wandb", "hivemind", "trunk", ".sparkbox/setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != setupScript {
		t.Fatalf("contents = %q, want %q", got, setupScript)
	}

	read := s.last("/contents/")
	if read.path != "/repos/wandb/hivemind/contents/.sparkbox/setup.sh" {
		t.Errorf("path = %q", read.path)
	}
	if read.auth != "Bearer ghs_supersecret" {
		t.Errorf("Authorization = %q, want the minted installation token", read.auth)
	}
	if s.accept != rawMediaType {
		t.Errorf("Accept = %q, want %q", s.accept, rawMediaType)
	}
	if read.version != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", read.version)
	}
	if s.rawQuery != "ref=trunk" {
		t.Errorf("query = %q, want ref=trunk", s.rawQuery)
	}

	var mint struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(s.last("/access_tokens").body), &mint); err != nil {
		t.Fatal(err)
	}
	if len(mint.Repositories) != 1 || mint.Repositories[0] != "hivemind" {
		t.Errorf("repositories = %v, want just the one being read", mint.Repositories)
	}
	// Read-only, and nothing but contents — an installation that holds write on
	// three permissions must not lend any of that to a file read.
	want := map[string]string{"contents": "read"}
	if len(mint.Permissions) != 1 || mint.Permissions["contents"] != want["contents"] {
		t.Errorf("permissions = %v, want %v", mint.Permissions, want)
	}
}

// An empty ref means "whatever the default branch is", which is github's
// question to answer: guessing "main" here is wrong on every repository that
// never renamed master.
func TestReadFileLeavesTheDefaultBranchToGitHub(t *testing.T) {
	s := newContentsStub(t, raw([]byte("ok\n")))
	app := newApp(t, s.stub, nil)

	if _, err := app.ReadFile(context.Background(), testInst, "wandb", "hivemind", "", ".sparkbox/setup.sh"); err != nil {
		t.Fatal(err)
	}
	if s.rawQuery != "" {
		t.Fatalf("query = %q, want no ref at all", s.rawQuery)
	}
}

// The common case. Most repositories have no .sparkbox/setup.sh, so this must
// be one cheap request, one matchable sentinel, and no prose that reads like a
// fault in a log.
func TestReadFileMissingFileIsASentinelNotAFault(t *testing.T) {
	s := newContentsStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		fmt.Fprint(w, `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`)
	})
	app := newApp(t, s.stub, nil)

	_, err := app.ReadFile(context.Background(), testInst, "wandb", "hivemind", "", ".sparkbox/setup.sh")
	if !errors.Is(err, ErrNoSuchFile) {
		t.Fatalf("err = %v, want ErrNoSuchFile", err)
	}
	for _, bad := range []string{"Not Found", "github answered 404"} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("a missing file reported %q: %v", bad, err)
		}
	}
	if !strings.Contains(err.Error(), ".sparkbox/setup.sh") || !strings.Contains(err.Error(), "wandb/hivemind") {
		t.Errorf("the refusal does not say what was missing where: %v", err)
	}
	if n := s.count("/contents/"); n != 1 {
		t.Errorf("made %d content requests, want 1", n)
	}
}

// A truncated shell script is not a smaller shell script, it is a different
// one. So the cap refuses rather than cuts — and the byte exactly on the cap is
// still a file this reads.
func TestReadFileBoundsTheRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{"at the cap", maxFileBytes, false},
		{"one byte over", maxFileBytes + 1, true},
		{"far over", maxFileBytes * 4, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Repeat([]byte("x"), tc.size)
			s := newContentsStub(t, raw(body))
			app := newApp(t, s.stub, nil)

			got, err := app.ReadFile(context.Background(), testInst, "wandb", "hivemind", "", "big.sh")
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("a %d byte file was accepted as %d bytes", tc.size, len(got))
			case tc.wantErr:
				if len(got) != 0 {
					t.Errorf("a refused read still returned %d bytes", len(got))
				}
				if errors.Is(err, ErrNoSuchFile) {
					t.Errorf("an oversized file reported as missing: %v", err)
				}
				if !strings.Contains(err.Error(), "larger than") {
					t.Errorf("err = %v, want it to say why", err)
				}
			case err != nil:
				t.Fatalf("a file exactly at the cap was refused: %v", err)
			case len(got) != tc.size:
				t.Fatalf("read %d bytes, want %d", len(got), tc.size)
			}
		})
	}
}

// A directory, a symlink and a submodule have no raw form, so github abandons
// the media type and answers with its JSON envelope. Returning that to a caller
// expecting a shell script would be worse than any error.
func TestReadFileRefusesWhatIsNotAFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"directory", `[{"name":"setup.sh","path":".sparkbox/setup.sh","type":"file"}]`},
		{"symlink", `{"type":"symlink","target":"../elsewhere/setup.sh"}`},
		{"submodule", `{"type":"submodule","submodule_git_url":"https://github.com/wandb/other"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newContentsStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				fmt.Fprint(w, tc.body)
			})
			app := newApp(t, s.stub, nil)

			got, err := app.ReadFile(context.Background(), testInst, "wandb", "hivemind", "", ".sparkbox")
			if err == nil {
				t.Fatalf("a %s was returned as file contents: %q", tc.name, got)
			}
			if len(got) != 0 {
				t.Errorf("a refused read still returned %d bytes", len(got))
			}
			if !strings.Contains(err.Error(), "not a regular file") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// The status map. 403 is a refusal an operator fixes on github.com; 5xx and a
// dead transport are the one class worth retrying, which is why they are
// separated rather than folded together.
func TestReadFileMapsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"forbidden", 403, `{"message":"Resource not accessible by integration"}`, ErrForbidden},
		{"unauthorized", 401, `{"message":"Bad credentials"}`, ErrForbidden},
		{"server error", 500, `{"message":"Server Error"}`, ErrUpstream},
		{"rate limited", 429, `{"message":"You have exceeded a secondary rate limit"}`, ErrUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newContentsStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			app := newApp(t, s.stub, nil)

			_, err := app.ReadFile(context.Background(), testInst, "wandb", "hivemind", "", ".sparkbox/setup.sh")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if errors.Is(err, ErrNoSuchFile) {
				t.Errorf("a %d was reported as a missing file: %v", tc.status, err)
			}
		})
	}
}

// A transport that never answers is upstream being down, not the App being
// refused: the caller is allowed to retry it.
func TestReadFileMapsTransportFailure(t *testing.T) {
	s := newContentsStub(t, raw([]byte("ok")))
	app := newApp(t, s.stub, nil)
	// Mint first so the token is cached, then take the server away: the failure
	// under test has to be the content read, not the mint.
	if _, err := app.MintToken(context.Background(), testInst, []string{"hivemind"}, map[string]string{"contents": PermRead}); err != nil {
		t.Fatal(err)
	}
	s.srv.Close()

	_, err := app.ReadFile(context.Background(), testInst, "wandb", "hivemind", "", ".sparkbox/setup.sh")
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v, want ErrUpstream", err)
	}
}

// Arguments that could not mean what the caller intended are refused before a
// credential is minted, let alone spent.
func TestReadFileRefusesUnusableArguments(t *testing.T) {
	for _, tc := range []struct {
		name             string
		owner, repo, ref string
		path             string
	}{
		{"bad owner", "wan db", "hivemind", "", "setup.sh"},
		{"bad repo", "wandb", "hive mind", "", "setup.sh"},
		{"empty path", "wandb", "hivemind", "", ""},
		{"absolute path", "wandb", "hivemind", "", "/etc/passwd"},
		{"climbing path", "wandb", "hivemind", "", "../../etc/passwd"},
		{"dot segment", "wandb", "hivemind", "", ".sparkbox/./setup.sh"},
		{"empty segment", "wandb", "hivemind", "", ".sparkbox//setup.sh"},
		{"trailing slash", "wandb", "hivemind", "", ".sparkbox/"},
		{"control character in path", "wandb", "hivemind", "", "setup\n.sh"},
		{"overlong path", "wandb", "hivemind", "", strings.Repeat("a/", 400) + "setup.sh"},
		{"ref with a space", "wandb", "hivemind", "my branch", "setup.sh"},
		{"ref with a newline", "wandb", "hivemind", "trunk\nHost: evil", "setup.sh"},
		{"overlong ref", "wandb", "hivemind", strings.Repeat("b", 256), "setup.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newContentsStub(t, raw([]byte("must not be reached")))
			app := newApp(t, s.stub, nil)

			if _, err := app.ReadFile(context.Background(), testInst, tc.owner, tc.repo, tc.ref, tc.path); err == nil {
				t.Fatal("the argument was accepted")
			}
			if n := len(s.seen()); n != 0 {
				t.Errorf("made %d requests, want 0 — nothing should be minted for an unusable argument", n)
			}
		})
	}
}

// Path segments that are legal in a repository but not in a URL still address
// the file they name, rather than being pasted through as structure.
func TestReadFileEscapesPathSegments(t *testing.T) {
	s := newContentsStub(t, raw([]byte("ok")))
	app := newApp(t, s.stub, nil)

	if _, err := app.ReadFile(context.Background(), testInst, "wandb", "hivemind", "", "dir/a b?c#d.sh"); err != nil {
		t.Fatal(err)
	}
	// The mux decodes before it records, so what is asserted is that the escaped
	// form round-tripped to the same path — a '?' pasted through unescaped would
	// have arrived as a query instead.
	if got := s.last("/contents/").path; got != "/repos/wandb/hivemind/contents/dir/a b?c#d.sh" {
		t.Fatalf("path = %q", got)
	}
	if s.rawQuery != "" {
		t.Fatalf("query = %q, want none", s.rawQuery)
	}
}

// The rule the whole package is built around: a token may be spent but never
// spoken. The file's own bytes are held to the same rule — a setup script is
// somebody's private repository content and has no business in a log line.
func TestReadFileLeaksNeitherTheTokenNorTheContents(t *testing.T) {
	var logged bytes.Buffer
	const secretContents = "#!/bin/sh\nexport DEPLOY_KEY=hunter2\n"
	s := newContentsStub(t, raw([]byte(secretContents)))
	app, err := New(Config{
		ClientID: "Iv23liTESTCLIENT", Key: testKey(), BaseURL: s.srv.URL,
		Logger: slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:    func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := app.ReadFile(context.Background(), testInst, "wandb", "hivemind", "", ".sparkbox/setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != secretContents {
		t.Fatalf("contents = %q", got)
	}
	for _, bad := range []string{"ghs_", "hunter2", "DEPLOY_KEY"} {
		if strings.Contains(logged.String(), bad) {
			t.Errorf("a log line carried %q: %q", bad, logged.String())
		}
	}
}
