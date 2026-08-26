package restapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The repo surface is three routes over one ctlops call each, so what is worth
// testing here is not the calls — ctlops has its own tests for those — but the
// three things this package alone decides: that a host without a repo store
// says so rather than failing, that the check endpoint is its own operation and
// not a body-less POST landing on `repo.add`, and that a slug survives the trip
// through three path segments intact.
//
// The rig deliberately wires no repo store (newTestAPI's ctlops.Config names
// none), which is exactly the state every one of these tests wants.

// repoRoutes pulls the repo rows out of the table rather than restating them,
// so a route added later is covered here without anybody remembering to add it.
func repoRoutes(t *testing.T, h *Handler) []route {
	t.Helper()
	var out []route
	for _, rt := range h.routes() {
		if strings.HasPrefix(rt.opID, "repo.") {
			out = append(out, rt)
		}
	}
	if len(out) == 0 {
		t.Fatal("the route table carries no repo routes")
	}
	return out
}

// TestRepoRoutesAnswerDisabledWithoutAStore pins the answer a host that has not
// configured repo attachments gives: 501 with the typed envelope, not a 500 and
// not a nil-deref inside a handler. It also asserts the op string, which is the
// contract binding these routes to ctlops — the route table, the `const op` in
// each handler and the Op that ctlops stamps on the error are one string, and
// this is where a rename in only one of the three shows up.
func TestRepoRoutesAnswerDisabledWithoutAStore(t *testing.T) {
	ta := newTestAPI(t)
	for _, rt := range repoRoutes(t, ta.handler) {
		path, body := sample(rt)
		t.Run(rt.opID, func(t *testing.T) {
			rec := ta.do(t, rt.method, path, "alice", body)
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("status %d, want 501 (%s)", rec.Code, rec.Body)
			}
			e := decodeErr(t, rec)
			if e.Kind != "disabled" {
				t.Errorf("kind %q, want disabled", e.Kind)
			}
			if e.Op != rt.opID {
				t.Errorf("op %q, want %q — the handler, the table and ctlops must agree", e.Op, rt.opID)
			}
			// codeFor truncates at the first dot, so every repo verb shares one
			// machine-readable code and a client can switch on it once.
			if e.Code != "repo_disabled" {
				t.Errorf("code %q, want repo_disabled", e.Code)
			}
			if e.Message == "" {
				t.Error("a disabled answer with no sentence tells the user nothing")
			}
		})
	}
}

// TestRepoCheckIsItsOwnOperation guards the one routing accident this shape
// invites: "/v1/repos/check" is a POST under the same prefix as "/v1/repos", so
// a mistake in the table would land a check on the attach handler, which reads
// a body it was not sent and would attach nothing while answering 201.
func TestRepoCheckIsItsOwnOperation(t *testing.T) {
	ta := newTestAPI(t)

	check := ta.do(t, "POST", "/v1/repos/check", "alice", nil)
	if got := decodeErr(t, check).Op; got != "repo.check" {
		t.Fatalf("POST /v1/repos/check reached %q, want repo.check", got)
	}
	add := ta.do(t, "POST", "/v1/repos", "alice", repoRequest{Slug: "wandb/hivemind"})
	if got := decodeErr(t, add).Op; got != "repo.add" {
		t.Fatalf("POST /v1/repos reached %q, want repo.add", got)
	}
	// The check is a read, not a mutation: it must not demand the CSRF proof a
	// browser would have to be first-party to send.
	bare := ta.doRaw(t, "POST", "/v1/repos/check", "alice", nil, nil)
	if bare.Code == http.StatusForbidden {
		t.Fatal("the check endpoint is behind the mutation gate; it changes nothing and should not be")
	}
}

// TestRepoAddRefusesAnUnknownField: `tag` is the singular a user reaches for
// first, and silently ignoring it would attach the repo to `default` — which
// every untagged sandbox carries — while the caller believed they had scoped
// it. DisallowUnknownFields turns that into a 400 before ctlops is reached.
func TestRepoAddRefusesAnUnknownField(t *testing.T) {
	ta := newTestAPI(t)
	rec := ta.do(t, "POST", "/v1/repos", "alice",
		map[string]any{"slug": "wandb/hivemind", "tag": []string{"hm"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (%s)", rec.Code, rec.Body)
	}
	e := decodeErr(t, rec)
	if e.Op != "repo.add" || e.Code != "malformed_body" {
		t.Fatalf("op/code = %q/%q, want repo.add/malformed_body", e.Op, e.Code)
	}
}

// TestRepoTargetRebuildsTheSlug drives the real DELETE pattern — read out of
// the route table, so the wildcard names cannot drift away from what
// repoTarget reads — over a bare mux, which is the only way to see the path
// values a handler receives without also standing up the ctlops call behind it.
func TestRepoTargetRebuildsTheSlug(t *testing.T) {
	h := New(Config{})
	var pattern string
	for _, rt := range h.routes() {
		if rt.opID == "repo.rm" {
			pattern = rt.pattern
		}
	}
	if pattern == "" {
		t.Fatal("no repo.rm route to test")
	}

	var gotHost, gotSlug string
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE "+pattern, func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotSlug = repoTarget(r)
	})

	for _, tc := range []struct {
		name, url          string
		wantCode           int
		wantHost, wantSlug string
	}{
		{"plain", "/v1/repos/github.com/wandb/hivemind", 200, "github.com", "wandb/hivemind"},
		// A repository name may carry dots and underscores, and none of them are
		// separators here.
		{"dotted name", "/v1/repos/github.com/wandb/hive.mind_2", 200, "github.com", "wandb/hive.mind_2"},
		// The encoded-slash spelling is NOT an alternate address. Go's mux would
		// happily hand a single wildcard the decoded "wandb/hivemind" — which is
		// precisely the trap this three-segment path avoids, because the proxies
		// in front of this API rewrite %2F on the way in and the two spellings
		// would stop agreeing. Four segments cannot match five: it 404s.
		{"encoded slash", "/v1/repos/github.com/wandb%2Fhivemind", 404, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotHost, gotSlug = "", ""
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("DELETE", tc.url, nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d, want %d", rec.Code, tc.wantCode)
			}
			if gotHost != tc.wantHost || gotSlug != tc.wantSlug {
				t.Fatalf("host/slug = %q/%q, want %q/%q", gotHost, gotSlug, tc.wantHost, tc.wantSlug)
			}
		})
	}
}
