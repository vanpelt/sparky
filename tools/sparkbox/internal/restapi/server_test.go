package restapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

const testDomain = "hivemind.tools"

// stubGitHub keeps the GitHub-linked endpoints off the network. Verify always
// says "that key isn't listed", which is the interesting answer: it is the one
// that must render as a 403 rather than a 500.
type stubGitHub struct{}

func (stubGitHub) Fetch(context.Context, string) ([]xssh.PublicKey, error) { return nil, nil }
func (stubGitHub) Verify(context.Context, string, xssh.PublicKey) (bool, error) {
	return false, nil
}
func (stubGitHub) Profile(context.Context, string) (users.GitHubProfile, error) {
	return users.GitHubProfile{}, nil
}

type testAPI struct {
	h       http.Handler
	handler *Handler
	mgr     *host.Manager
	driver  *mock.Driver
	users   *users.Store
	secrets *secrets.Store
	routes  *routes.Store
	sched   *schedule.Store
	signer  *edgeauth.Signer
}

func newTestAPI(t *testing.T) *testAPI {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	hostKey, err := sshgw.LoadOrCreateKey(dir, "gateway_host_key")
	if err != nil {
		t.Fatal(err)
	}
	upstreamKey, err := sshgw.LoadOrCreateKey(dir, "gateway_upstream_key")
	if err != nil {
		t.Fatal(err)
	}
	driver := mock.New(dir, hostKey)
	t.Cleanup(func() { driver.Close() })

	db := filepath.Join(dir, "sparkbox.db")
	routeStore, err := routes.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { routeStore.Close() })
	userStore, err := users.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { userStore.Close() })
	schedStore, err := schedule.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { schedStore.Close() })
	secretStore, err := secrets.Open(filepath.Join(dir, "secrets.db"),
		secrets.DeriveKEK([]byte("test-ikm")), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { secretStore.Close() })

	mgr, err := host.NewManager(host.Options{
		StateDir: dir, Driver: driver,
		GatewayPublicKey: sshgw.PublicKeyLine(upstreamKey), Logger: log,
		Routes: routeStore, Tags: secretStore,
	})
	if err != nil {
		t.Fatal(err)
	}

	// alice owns things, mallory tries to reach them, opsy is seeded (and so an
	// operator, the only distinction this API makes).
	for _, u := range []struct{ handle, invitedBy string }{
		{"alice", "bob"}, {"mallory", "bob"}, {"opsy", users.OperatorInviter},
	} {
		if err := userStore.Create(u.handle, genKey(t), u.handle+"-laptop", "seed", u.invitedBy); err != nil {
			t.Fatal(err)
		}
	}

	signer := edgeauth.NewSigner([]byte("test-oidc-ikm"))
	ops := ctlops.New(ctlops.Config{
		Sandboxes: mgr, Templates: mgr, Accounts: userStore,
		Tags: secretStore, Schedules: schedStore, Routes: routeStore,
		Sessions: signer, GitHub: stubGitHub{},
		DefaultImage: "ubuntu", Domain: testDomain, XtermSubdomain: "xterm",
		Log: log,
	})
	t.Cleanup(ops.Close)

	h := New(Config{
		Ops: ops, Accounts: userStore, Signer: signer,
		Subdomain: "api", Domain: testDomain, Log: log,
	})
	return &testAPI{
		h: h.Handler(), handler: h, mgr: mgr, driver: driver, users: userStore,
		secrets: secretStore, routes: routeStore, sched: schedStore, signer: signer,
	}
}

func genKey(t *testing.T) xssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := xssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func (ta *testAPI) session(t *testing.T, handle string) *http.Cookie {
	t.Helper()
	tok, _, err := ta.signer.Mint(edgeauth.Identity{Handle: handle}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: edgeauth.CookieName, Value: tok}
}

// do issues a request as handle ("" = unauthenticated), carrying the CSRF
// header a first-party client would send.
func (ta *testAPI) do(t *testing.T, method, path, handle string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return ta.doRaw(t, method, path, handle, body, func(r *http.Request) {
		r.Header.Set(edgeauth.MutationHeader, "1")
	})
}

func (ta *testAPI) doRaw(t *testing.T, method, path, handle string, body any,
	tweak func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if handle != "" {
		req.AddCookie(ta.session(t, handle))
		if tweak != nil {
			tweak(req)
		}
	}
	rec := httptest.NewRecorder()
	ta.h.ServeHTTP(rec, req)
	return rec
}

func (ta *testAPI) create(t *testing.T, name, owner string) *host.Sandbox {
	t.Helper()
	box, err := ta.mgr.Create(context.Background(), name, owner, "ubuntu", 1, 512)
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// decodeErr reads the error envelope, failing the test if the body is not one.
func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) apiError {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %q is not an error envelope: %v", rec.Body.String(), err)
	}
	return env.Error
}

// ---------------------------------------------------------------------------
// Representative requests, derived from the route table so they cannot drift
// ---------------------------------------------------------------------------

// sampleKey is a syntactically valid authorized_keys line for the add-key path.
const sampleKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGb9ECWmEzf6FQbrBZ9w7mvBB3BGooJU9UjBt2gyVJXo demo"

// sample returns a request that reaches rt's handler. Sandbox-scoped routes
// deliberately name a sandbox nobody owns: the point of driving every route is
// to exercise routing, decoding and error rendering, not to sequence a
// lifecycle — which the lifecycle tests do explicitly.
func sample(rt route) (path string, body any) {
	// specPath, not the raw pattern: "/{$}" is a mux wildcard, and requesting it
	// literally would exercise the catch-all instead of the root route.
	path = specPath(rt.pattern)
	path = strings.ReplaceAll(path, "{name}", "ghost")
	path = strings.ReplaceAll(path, "{id}", "ghost")
	switch rt.opID {
	case "keys.rm":
		path += "?fingerprint=SHA256:none"
	case "resize":
		body = resizeRequest{Size: "25G"}
	case "rename":
		body = renameRequest{Name: "ghost-two"}
	case "tags.set":
		body = tagsRequest{Tags: []string{"x"}}
	case "share.set":
		body = visibilityRequest{Visibility: "public"}
	case "snapshot.create":
		body = snapshotRequest{Sandbox: "ghost", Name: "ghost-snap"}
	case "fork":
		body = forkRequest{Name: "ghost-fork"}
	case "schedule.add":
		body = scheduleRequest{Sandbox: "ghost", Spec: "@daily", Command: "echo hi"}
	case "keys.add":
		body = addKeyRequest{Key: sampleKey}
	case "keys.verify-github":
		body = githubRequest{Login: "nobody", Fingerprint: "SHA256:none"}
	case "email.set":
		body = emailRequest{Email: ""}
	case "session-token":
		body = tokenRequest{TTL: "1h"}
	}
	return path, body
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func TestEveryV1EndpointRequiresAuth(t *testing.T) {
	ta := newTestAPI(t)
	for _, rt := range ta.handler.routes() {
		if rt.auth == authPublic {
			continue
		}
		path, body := sample(rt)
		rec := ta.do(t, rt.method, path, "", body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated: status %d, want 401", rt.method, path, rec.Code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s: missing Cache-Control: no-store", rt.method, path)
		}
	}
}

func TestPublicEndpointsNeedNoSession(t *testing.T) {
	ta := newTestAPI(t)
	for _, tc := range []struct {
		path string
		want int
		ct   string
	}{
		{"/", http.StatusSeeOther, ""},
		{"/docs", http.StatusOK, "text/html"},
		{"/openapi.json", http.StatusOK, "application/json"},
		{"/openapi.yaml", http.StatusOK, "application/yaml"},
	} {
		rec := ta.do(t, "GET", tc.path, "", nil)
		if rec.Code != tc.want {
			t.Errorf("GET %s: status %d, want %d", tc.path, rec.Code, tc.want)
		}
		if tc.ct != "" && !strings.Contains(rec.Header().Get("Content-Type"), tc.ct) {
			t.Errorf("GET %s: content-type %q, want %q", tc.path, rec.Header().Get("Content-Type"), tc.ct)
		}
	}
}

// TestMutationCSRFGate is the reason RequireMutation exists: the session cookie
// is scoped to the whole zone, so every sandbox's own web page is same-site with
// this API and SameSite=Lax fences off nothing.
func TestMutationCSRFGate(t *testing.T) {
	ta := newTestAPI(t)
	ta.create(t, "alices-box", "alice")

	// Cookie alone — what a cross-site form post would carry.
	rec := ta.doRaw(t, "POST", "/v1/sandboxes/alices-box/pin", "alice", nil, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cookie-only mutation: status %d, want 403", rec.Code)
	}
	// A foreign Origin is no better.
	rec = ta.doRaw(t, "POST", "/v1/sandboxes/alices-box/pin", "alice", nil, func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign Origin: status %d, want 403", rec.Code)
	}
	if box, _ := ta.mgr.Get("alices-box"); box.Pinned {
		t.Fatal("a refused mutation still changed state")
	}

	// The three ways through: first-party Origin, the console header, Bearer.
	rec = ta.doRaw(t, "POST", "/v1/sandboxes/alices-box/pin", "alice", nil, func(r *http.Request) {
		r.Header.Set("Origin", "https://api."+testDomain)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("first-party Origin: status %d, want 200 (%s)", rec.Code, rec.Body)
	}
	rec = ta.do(t, "POST", "/v1/sandboxes/alices-box/unpin", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("mutation header: status %d, want 200 (%s)", rec.Code, rec.Body)
	}

	tok, _, err := ta.signer.Mint(edgeauth.Identity{Handle: "alice"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/sandboxes/alices-box/pin", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	bearer := httptest.NewRecorder()
	ta.h.ServeHTTP(bearer, req)
	if bearer.Code != http.StatusOK {
		t.Fatalf("bearer mutation: status %d, want 200 (%s)", bearer.Code, bearer.Body)
	}
}

// ---------------------------------------------------------------------------
// Owner scoping
// ---------------------------------------------------------------------------

// TestCrossOwnerIsIndistinguishable is the load-bearing security test: acting on
// alice's resource as mallory must produce byte-identical output to acting on a
// name that was never created. A 403, a different message, or even a different
// error code would confirm the name exists.
func TestCrossOwnerIsIndistinguishable(t *testing.T) {
	ta := newTestAPI(t)
	ctx := context.Background()
	ta.create(t, "alices-box", "alice")
	if _, err := ta.mgr.Snapshot(ctx, "alices-box", "alices-snap", "alice"); err != nil {
		t.Fatal(err)
	}
	entry, err := ta.sched.Add(schedule.Entry{
		Sandbox: "alices-box", Owner: "alice", Spec: "@daily", Command: "echo hi",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name          string
		method        string
		hers, nobodys string
		body          any
	}{
		{"get", "GET", "/v1/sandboxes/alices-box", "/v1/sandboxes/nobodys-box", nil},
		{"pause", "POST", "/v1/sandboxes/alices-box/pause", "/v1/sandboxes/nobodys-box/pause", nil},
		{"resume", "POST", "/v1/sandboxes/alices-box/resume", "/v1/sandboxes/nobodys-box/resume", nil},
		{"destroy", "DELETE", "/v1/sandboxes/alices-box", "/v1/sandboxes/nobodys-box", nil},
		{"tags", "GET", "/v1/sandboxes/alices-box/tags", "/v1/sandboxes/nobodys-box/tags", nil},
		{"settags", "PUT", "/v1/sandboxes/alices-box/tags", "/v1/sandboxes/nobodys-box/tags",
			tagsRequest{Tags: []string{"pwned"}}},
		{"visibility", "GET", "/v1/sandboxes/alices-box/visibility",
			"/v1/sandboxes/nobodys-box/visibility", nil},
		{"snapshot", "DELETE", "/v1/snapshots/alices-snap", "/v1/snapshots/nobodys-snap", nil},
		{"fork", "POST", "/v1/snapshots/alices-snap/fork", "/v1/snapshots/nobodys-snap/fork",
			forkRequest{Name: "stolen"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hers := ta.do(t, tc.method, tc.hers, "mallory", tc.body)
			nobodys := ta.do(t, tc.method, tc.nobodys, "mallory", tc.body)
			if hers.Code != http.StatusNotFound {
				t.Fatalf("alice's resource as mallory: status %d, want 404 (%s)", hers.Code, hers.Body)
			}
			if nobodys.Code != hers.Code {
				t.Fatalf("nonexistent: status %d, alice's: %d — distinguishable",
					nobodys.Code, hers.Code)
			}
			a, b := decodeErr(t, hers), decodeErr(t, nobodys)
			if a.Kind != b.Kind || a.Code != b.Code {
				t.Fatalf("errors differ: %+v vs %+v", a, b)
			}
			// The message differs only by the name the caller typed.
			if strings.ReplaceAll(a.Message, "alices", "nobodys") != b.Message {
				t.Fatalf("messages differ beyond the name: %q vs %q", a.Message, b.Message)
			}
		})
	}

	// Nothing was touched.
	if box, ok := ta.mgr.Get("alices-box"); !ok || box.Owner != "alice" {
		t.Fatal("alice's sandbox did not survive mallory")
	}
	if tags, err := ta.secrets.TagsFor("alices-box"); err != nil || len(tags) != 0 {
		t.Fatalf("tags were written: %v %v", tags, err)
	}
	if _, err := ta.sched.Get(entry.ID); err != nil {
		t.Fatalf("alice's schedule vanished: %v", err)
	}

	// Listings answer empty rather than leaking a count.
	for _, path := range []string{"/v1/sandboxes", "/v1/snapshots", "/v1/schedules"} {
		rec := ta.do(t, "GET", path, "mallory", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s as mallory: status %d", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "alice") {
			t.Fatalf("GET %s leaked alice: %s", path, rec.Body)
		}
	}

	// A foreign schedule id is masked exactly like a foreign sandbox name.
	rec := ta.do(t, "DELETE", "/v1/schedules/"+entry.ID, "mallory", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign schedule: status %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Status mapping
// ---------------------------------------------------------------------------

// TestStatusComesFromKindNotText pins the mapping to ctlops's typed Kind. The
// point is that rewording a message can never move a status code, which is
// exactly what the substring-matching statusFor() functions elsewhere allow.
func TestStatusComesFromKindNotText(t *testing.T) {
	ta := newTestAPI(t)
	ta.create(t, "alices-box", "alice")

	for _, tc := range []struct {
		name       string
		method     string
		path       string
		handle     string
		body       any
		wantStatus int
		wantKind   string
		wantCode   string
	}{
		{"missing sandbox", "GET", "/v1/sandboxes/ghost", "alice", nil,
			404, "not_found", "sandbox_not_found"},
		{"bad size", "POST", "/v1/sandboxes/alices-box/resize", "alice",
			resizeRequest{Size: "25T"}, 400, "invalid", "bad_size"},
		{"no size at all", "POST", "/v1/sandboxes/alices-box/resize", "alice",
			resizeRequest{}, 400, "invalid", "bad_size"},
		{"bad cron", "POST", "/v1/schedules", "alice",
			scheduleRequest{Sandbox: "alices-box", Spec: "not a cron", Command: "x"},
			400, "invalid", "bad_cron"},
		{"bad visibility", "PUT", "/v1/sandboxes/alices-box/visibility", "alice",
			visibilityRequest{Visibility: "sortof"}, 400, "invalid", "bad_visibility"},
		{"archiving off", "POST", "/v1/sandboxes/alices-box/archive", "alice", nil,
			501, "disabled", "archive_disabled"},
		{"invite without operator", "POST", "/v1/account/invites", "alice", nil,
			403, "denied", "not_operator"},
		{"github key not listed", "POST", "/v1/account/github", "alice",
			githubRequest{Login: "nobody", Fingerprint: "SHA256:none"},
			404, "not_found", "key_not_found"},
		{"bad ttl", "POST", "/v1/account/tokens", "alice", tokenRequest{TTL: "soon"},
			400, "invalid", "bad_ttl"},
		{"malformed key", "POST", "/v1/account/keys", "alice",
			addKeyRequest{Key: "not a key"}, 400, "invalid", "bad_key"},
		{"unknown job", "GET", "/v1/jobs/nope", "alice", nil,
			404, "not_found", "job_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := ta.do(t, tc.method, tc.path, tc.handle, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body)
			}
			e := decodeErr(t, rec)
			if e.Kind != tc.wantKind || e.Code != tc.wantCode {
				t.Fatalf("kind/code = %q/%q, want %q/%q", e.Kind, e.Code, tc.wantKind, tc.wantCode)
			}
			if e.Message == "" || e.Op == "" {
				t.Fatalf("error is missing message or op: %+v", e)
			}
		})
	}

	// An operator gets through the one gate that exists.
	rec := ta.do(t, "POST", "/v1/account/invites", "opsy", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("operator invite: status %d, want 201 (%s)", rec.Code, rec.Body)
	}
}

// TestNameCollisionsAreAlwaysConflicts: the same condition must not answer 409
// through one door and 500 through another. A 500 is the one status a client is
// invited to retry, and no amount of retrying frees a taken name — it also
// spends the error budget on user typos.
func TestNameCollisionsAreAlwaysConflicts(t *testing.T) {
	ta := newTestAPI(t)
	ta.create(t, "a", "alice")
	ta.create(t, "b", "alice")
	if _, err := ta.mgr.Snapshot(context.Background(), "a", "base", "alice"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"create", "POST", "/v1/sandboxes", createRequest{Name: "b"}},
		{"rename", "POST", "/v1/sandboxes/a/rename", renameRequest{Name: "b"}},
		{"snapshot", "POST", "/v1/snapshots", snapshotRequest{Sandbox: "a", Name: "base"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := ta.do(t, tc.method, tc.path, "alice", tc.body)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status %d, want 409 (%s)", rec.Code, rec.Body)
			}
			if e := decodeErr(t, rec); e.Kind != "conflict" || e.Code != "name_taken" {
				t.Fatalf("kind/code = %q/%q, want conflict/name_taken", e.Kind, e.Code)
			}
		})
	}
}

// TestJobsDoNotCollapseDifferentArguments: the de-duplicator answers a
// collapsed request with the FIRST job's result under an ordinary 2xx, so
// matching on (owner, op, resource) alone reported a resize the caller never
// asked for as a success and never ran the second one at all.
func TestJobsDoNotCollapseDifferentArguments(t *testing.T) {
	ta := newTestAPI(t)
	ta.create(t, "alices-box", "alice")
	c := ctlops.Caller{Handle: "alice"}
	ran := make(chan int64, 2)
	block := make(chan struct{})

	start := func(sizeMB int64) *ctlops.Job {
		ref := ctlops.Ref{Type: "sandbox", Name: "alices-box",
			Args: strconv.FormatInt(sizeMB, 10)}
		return ta.handler.ops.Go(c, "resize", ref, time.Minute,
			func(context.Context) (any, error) {
				ran <- sizeMB
				<-block
				return map[string]int64{"disk_mb": sizeMB}, nil
			})
	}
	first := start(25600)
	<-ran // the first closure is running, so the second can only collapse or start
	second := start(102400)
	if first.ID == second.ID {
		t.Fatal("a 100G resize collapsed into the in-flight 25G one")
	}
	if got := <-ran; got != 102400 {
		t.Fatalf("second closure ran with %d MB", got)
	}
	// The argument-less case still collapses, which is the property that makes a
	// retried pause or archive free.
	same := ctlops.Ref{Type: "sandbox", Name: "alices-box"}
	a := ta.handler.ops.Go(c, "pause", same, time.Minute,
		func(context.Context) (any, error) { <-block; return nil, nil })
	b := ta.handler.ops.Go(c, "pause", same, time.Minute,
		func(context.Context) (any, error) { t.Error("a duplicate pause ran twice"); return nil, nil })
	if a.ID != b.ID {
		t.Errorf("an identical pause started a second job")
	}
	close(block)
}

// TestUnroutedRequestsUseTheEnvelope: "GET /" used to be the mux's
// least-specific pattern and therefore a catch-all, so every mistyped path 303'd
// to the HTML docs page — into a JSON parser, from a route that never reaches
// the auth gate.
func TestUnroutedRequestsUseTheEnvelope(t *testing.T) {
	ta := newTestAPI(t)

	for _, tc := range []struct {
		name, method, path string
		want               int
		code               string
	}{
		{"singular collection", "GET", "/v1/sandbox/demo", 404, "unknown_endpoint"},
		{"typo'd sub-resource", "GET", "/v1/sandboxes/x/typo", 404, "unknown_endpoint"},
		{"unversioned", "GET", "/nonsense", 404, "unknown_endpoint"},
		{"post to a typo", "POST", "/v1/sandbox/demo", 404, "unknown_endpoint"},
		{"wrong verb on a real path", "POST", "/v1/whoami", 405, "method_not_allowed"},
		{"wrong verb on the root", "POST", "/", 405, "method_not_allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := ta.do(t, tc.method, tc.path, "alice", nil)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tc.want, rec.Body)
			}
			if e := decodeErr(t, rec); e.Code != tc.code {
				t.Fatalf("code %q, want %q", e.Code, tc.code)
			}
		})
	}
	// The root itself still redirects — that is the route "/{$}" now names
	// exactly, rather than the whole subtree.
	if rec := ta.do(t, "GET", "/", "", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("GET / = %d, want 303", rec.Code)
	}
}

// TestErrorOpIsTheOperationID pins what both openapi.json's Error schema and
// ctlops.Error.Op promise: the envelope's "op" IS the operationId. ctlops names
// one op per verb-pair where the route table splits them, so tags.get/tags.set,
// share.get/share.set and email.get all used to answer with the pair's name and
// miss in any client table keyed on operationIds.
func TestErrorOpIsTheOperationID(t *testing.T) {
	ta := newTestAPI(t)
	for _, rt := range ta.handler.routes() {
		if rt.auth == authPublic {
			continue
		}
		path, body := sample(rt)
		rec := ta.do(t, rt.method, path, "alice", body)
		if rec.Code < 400 {
			continue // this route succeeded; nothing to check
		}
		e := decodeErr(t, rec)
		if e.Op != rt.opID {
			t.Errorf("%s %s answered op %q, want the operationId %q",
				rt.method, rt.pattern, e.Op, rt.opID)
		}
	}
}

// TestIdempotencyKeyIsReleasedByAPanic: net/http recovers a panicking handler
// at the connection level, so a straight-line settle never ran and the key sat
// in-flight — answering the client's own retry 409 for a full day.
func TestIdempotencyKeyIsReleasedByAPanic(t *testing.T) {
	rc := newReplayCache()
	boom := rc.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("handler bug")
	}))
	call := func(h http.Handler) (rec *httptest.ResponseRecorder, panicked bool) {
		defer func() { panicked = recover() != nil }()
		req := httptest.NewRequest("POST", "/v1/account/tokens", strings.NewReader("{}"))
		req.Header.Set(idempotencyHeader, "k")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec, false
	}
	if _, panicked := call(boom); !panicked {
		t.Fatal("the panic did not propagate; the test proves nothing")
	}
	ok := rc.wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]string{"ok": "yes"})
	}))
	rec, _ := call(ok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry after a panic = %d, want 201 (%s)", rec.Code, rec.Body)
	}
}

func TestMalformedBodyIsFourHundred(t *testing.T) {
	ta := newTestAPI(t)
	ta.create(t, "alices-box", "alice")

	req := httptest.NewRequest("PUT", "/v1/sandboxes/alices-box/tags",
		strings.NewReader("{not json"))
	req.AddCookie(ta.session(t, "alice"))
	req.Header.Set(edgeauth.MutationHeader, "1")
	rec := httptest.NewRecorder()
	ta.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	if e := decodeErr(t, rec); e.Code != "malformed_body" {
		t.Fatalf("code %q, want malformed_body", e.Code)
	}

	// An unknown field is refused rather than silently dropped: a misspelled
	// "vcpu" would otherwise hand the caller a sandbox they did not ask for.
	rec = ta.do(t, "POST", "/v1/sandboxes", "alice", map[string]any{"vcpu": 4})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: status %d, want 400 (%s)", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------------------
// Smoke: every route
// ---------------------------------------------------------------------------

// TestEveryRouteAnswersCleanly drives the whole table as a real user. It asserts
// the shape of the answer, not its content: no route may panic, return HTML, or
// produce an unclassified 500 — the three failures a hand-written mux quietly
// grows.
func TestEveryRouteAnswersCleanly(t *testing.T) {
	ta := newTestAPI(t)
	ta.create(t, "alices-box", "alice")

	allowed := map[int]bool{
		200: true, 201: true, 202: true, 303: true,
		400: true, 403: true, 404: true, 409: true, 501: true,
	}
	for _, rt := range ta.handler.routes() {
		path, body := sample(rt)
		rec := ta.do(t, rt.method, path, "alice", body)
		if !allowed[rec.Code] {
			t.Errorf("%s %s: status %d (%s)", rt.method, path, rec.Code, rec.Body)
			continue
		}
		if rt.auth == authPublic {
			continue
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "application/json") {
			t.Errorf("%s %s: content-type %q, want JSON", rt.method, path, ct)
			continue
		}
		var any map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &any); err != nil {
			t.Errorf("%s %s: body is not a JSON object: %v", rt.method, path, err)
			continue
		}
		if rec.Code >= 400 {
			if e := decodeErr(t, rec); e.Code == "" || e.Message == "" {
				t.Errorf("%s %s: %d with an empty error envelope: %s",
					rt.method, path, rec.Code, rec.Body)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func TestSandboxLifecycle(t *testing.T) {
	ta := newTestAPI(t)

	rec := ta.do(t, "POST", "/v1/sandboxes", "alice", createRequest{Name: "demo", Tags: []string{"B", "a", "a"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status %d, want 201 (%s)", rec.Code, rec.Body)
	}
	var box ctlops.SandboxInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &box); err != nil {
		t.Fatal(err)
	}
	if box.Name != "demo" || box.Owner != "alice" {
		t.Fatalf("created %+v", box)
	}
	// Tags are normalized on the way in, and the URLs are derived from the
	// configured zone rather than the request's Host.
	if len(box.Tags) != 2 || box.Tags[0] != "a" || box.Tags[1] != "b" {
		t.Fatalf("tags %v, want [a b]", box.Tags)
	}
	if box.URL != "https://demo."+testDomain {
		t.Fatalf("url %q", box.URL)
	}
	if box.TerminalURL != "https://demo-xterm."+testDomain {
		t.Fatalf("terminal_url %q", box.TerminalURL)
	}

	// The tags really landed before the VM did — the whole reason ctlops owns
	// the ordering.
	if got, err := ta.secrets.TagsFor("demo"); err != nil || len(got) != 2 {
		t.Fatalf("stored tags %v (%v)", got, err)
	}

	rec = ta.do(t, "GET", "/v1/sandboxes", "alice", nil)
	var list sandboxList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sandboxes) != 1 {
		t.Fatalf("list returned %d sandboxes", len(list.Sandboxes))
	}

	rec = ta.do(t, "PUT", "/v1/sandboxes/demo/tags", "alice", tagsRequest{Tags: []string{"prod"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("set tags: status %d (%s)", rec.Code, rec.Body)
	}
	var tags tagsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tags); err != nil {
		t.Fatal(err)
	}
	if len(tags.Tags) != 1 || tags.Tags[0] != "prod" {
		t.Fatalf("tags %v", tags.Tags)
	}

	rec = ta.do(t, "POST", "/v1/sandboxes/demo/pause", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: status %d (%s)", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &box); err != nil {
		t.Fatal(err)
	}
	if box.State != "paused" {
		t.Fatalf("state after pause: %q", box.State)
	}

	rec = ta.do(t, "POST", "/v1/sandboxes/demo/resume", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: status %d (%s)", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &box); err != nil {
		t.Fatal(err)
	}
	if box.State != "running" {
		t.Fatalf("state after resume: %q", box.State)
	}

	rec = ta.do(t, "DELETE", "/v1/sandboxes/demo", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("destroy: status %d (%s)", rec.Code, rec.Body)
	}
	var gone deleted
	if err := json.Unmarshal(rec.Body.Bytes(), &gone); err != nil {
		t.Fatal(err)
	}
	if !gone.Deleted || gone.Name != "demo" {
		t.Fatalf("delete result %+v", gone)
	}
	if _, ok := ta.mgr.Get("demo"); ok {
		t.Fatal("sandbox survived its own destruction")
	}
}

// ---------------------------------------------------------------------------
// Async
// ---------------------------------------------------------------------------

// TestRespondAsyncEscalatesToAJob covers the 202 path end to end: the client
// asks not to wait, gets a job and a Location, and can then poll that job to
// completion and read the result the inline call would have returned.
func TestRespondAsyncEscalatesToAJob(t *testing.T) {
	ta := newTestAPI(t)
	ta.create(t, "demo", "alice")

	// respond-async waits out a zero-length window and then reports whichever
	// outcome happened, so a pause that finishes inside the two statements
	// between starting it and reading it answers 200 with the result — correct,
	// and not the path under test here. This driver's pause is fast enough for
	// that to come down to how the runner felt: it passed locally and on one CI
	// runner while failing on another, same commit. The delay makes the job
	// unambiguously still running, which is the state 202 exists to describe.
	ta.driver.PauseDelay = 250 * time.Millisecond

	rec := ta.doRaw(t, "POST", "/v1/sandboxes/demo/pause", "alice", nil, func(r *http.Request) {
		r.Header.Set(edgeauth.MutationHeader, "1")
		r.Header.Set("Prefer", "respond-async")
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202 (%s)", rec.Code, rec.Body)
	}
	var job jobBody
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if job.Op != "pause" || job.Resource.Name != "demo" {
		t.Fatalf("job %+v", job)
	}
	if got := rec.Header().Get("Location"); got != "/v1/jobs/"+job.ID {
		t.Fatalf("Location %q", got)
	}
	if got := rec.Header().Get("Preference-Applied"); got != "respond-async" {
		t.Fatalf("Preference-Applied %q", got)
	}

	// Poll to completion.
	deadline := time.Now().Add(30 * time.Second)
	for {
		rec = ta.do(t, "GET", "/v1/jobs/"+job.ID, "alice", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("poll: status %d (%s)", rec.Code, rec.Body)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
			t.Fatal(err)
		}
		if job.State != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.State != "succeeded" {
		t.Fatalf("job state %q, error %+v", job.State, job.Error)
	}
	var box ctlops.SandboxInfo
	if err := json.Unmarshal(job.Result, &box); err != nil {
		t.Fatalf("job result is not a sandbox: %v (%s)", err, job.Result)
	}
	if box.State != "paused" {
		t.Fatalf("result state %q", box.State)
	}

	// The job is listed, and only for its owner.
	rec = ta.do(t, "GET", "/v1/jobs", "alice", nil)
	var mine jobList
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil {
		t.Fatal(err)
	}
	if len(mine.Jobs) == 0 {
		t.Fatal("alice sees no jobs")
	}
	rec = ta.do(t, "GET", "/v1/jobs", "mallory", nil)
	var theirs jobList
	if err := json.Unmarshal(rec.Body.Bytes(), &theirs); err != nil {
		t.Fatal(err)
	}
	if len(theirs.Jobs) != 0 {
		t.Fatalf("mallory sees alice's jobs: %+v", theirs.Jobs)
	}
	rec = ta.do(t, "GET", "/v1/jobs/"+job.ID, "mallory", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("mallory reading alice's job: status %d, want 404", rec.Code)
	}

	// Canceling something that already finished is a conflict, not a silent no-op.
	rec = ta.do(t, "DELETE", "/v1/jobs/"+job.ID, "alice", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel finished job: status %d, want 409 (%s)", rec.Code, rec.Body)
	}
}

func TestPreferWaitIsHonoured(t *testing.T) {
	ta := newTestAPI(t)
	ta.create(t, "demo", "alice")

	rec := ta.doRaw(t, "POST", "/v1/sandboxes/demo/pause", "alice", nil, func(r *http.Request) {
		r.Header.Set(edgeauth.MutationHeader, "1")
		r.Header.Set("Prefer", "wait=30")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Preference-Applied"); got != "wait=30" {
		t.Fatalf("Preference-Applied %q, want wait=30", got)
	}
}

func TestParsePrefer(t *testing.T) {
	for _, tc := range []struct {
		header    string
		wantAsync bool
		wantWait  time.Duration
	}{
		{"", false, defaultWait},
		{"respond-async", true, 0},
		{"wait=5", false, 5 * time.Second},
		{"  Respond-Async ", true, 0},
		{"wait=999", false, maxWait},
		{"wait=nonsense", false, defaultWait},
		{"handling=lenient, wait=3", false, 3 * time.Second},
	} {
		r := httptest.NewRequest("POST", "/", nil)
		if tc.header != "" {
			r.Header.Set("Prefer", tc.header)
		}
		got := parsePrefer(r)
		if got.async != tc.wantAsync || got.wait != tc.wantWait {
			t.Errorf("Prefer %q: async=%v wait=%v, want %v/%v",
				tc.header, got.async, got.wait, tc.wantAsync, tc.wantWait)
		}
	}
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

// TestIdempotencyKeyReplaysTheAnswer uses token minting, the one call with no
// resource ref for the job de-duplicator to catch: without the header, two
// requests really do mint two credentials.
func TestIdempotencyKeyReplaysTheAnswer(t *testing.T) {
	ta := newTestAPI(t)
	key := "0123456789abcdef"

	mint := func(body any) *httptest.ResponseRecorder {
		return ta.doRaw(t, "POST", "/v1/account/tokens", "alice", body, func(r *http.Request) {
			r.Header.Set(edgeauth.MutationHeader, "1")
			r.Header.Set(idempotencyHeader, key)
		})
	}

	first := mint(tokenRequest{TTL: "1h"})
	if first.Code != http.StatusCreated {
		t.Fatalf("first mint: status %d (%s)", first.Code, first.Body)
	}
	if first.Header().Get(replayedHeader) != "" {
		t.Fatal("the first call was marked as a replay")
	}

	second := mint(tokenRequest{TTL: "1h"})
	if second.Code != first.Code || second.Body.String() != first.Body.String() {
		t.Fatalf("replay differs: %d %s vs %d %s",
			second.Code, second.Body, first.Code, first.Body)
	}
	if second.Header().Get(replayedHeader) != "true" {
		t.Fatal("the replay was not marked")
	}

	// The same key with different arguments is the client bug that must not be
	// answered with the first call's response.
	reused := mint(tokenRequest{TTL: "24h"})
	if reused.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reused key: status %d, want 422 (%s)", reused.Code, reused.Body)
	}
	if e := decodeErr(t, reused); e.Code != "idempotency_key_reused" {
		t.Fatalf("code %q", e.Code)
	}

	// Keys are scoped per account: mallory's identical key is not alice's.
	mallory := ta.doRaw(t, "POST", "/v1/account/tokens", "mallory", tokenRequest{TTL: "1h"},
		func(r *http.Request) {
			r.Header.Set(edgeauth.MutationHeader, "1")
			r.Header.Set(idempotencyHeader, key)
		})
	if mallory.Code != http.StatusCreated {
		t.Fatalf("mallory's key collided with alice's: status %d (%s)", mallory.Code, mallory.Body)
	}
	if mallory.Body.String() == first.Body.String() {
		t.Fatal("mallory received alice's token")
	}
}

func TestIdempotencyIgnoresUnkeyedRequests(t *testing.T) {
	ta := newTestAPI(t)
	a := ta.do(t, "POST", "/v1/account/tokens", "alice", tokenRequest{TTL: "1h"})
	b := ta.do(t, "POST", "/v1/account/tokens", "alice", tokenRequest{TTL: "1h"})
	if a.Code != http.StatusCreated || b.Code != http.StatusCreated {
		t.Fatalf("statuses %d/%d", a.Code, b.Code)
	}
	if a.Body.String() == b.Body.String() {
		t.Fatal("two unkeyed mints returned the same token — the cache is over-reaching")
	}
}

// TestServerErrorsAreNotCached: a 5xx must stay retryable with the same key, or
// a transient store fault gets pinned for a day.
func TestServerErrorsAreNotCached(t *testing.T) {
	rc := newReplayCache()
	calls := 0
	h := rc.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			writeJSON(w, http.StatusInternalServerError, errorEnvelope{})
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"calls": calls})
	}))
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("POST", "/v1/account/tokens", strings.NewReader("{}"))
		req.Header.Set(idempotencyHeader, "same")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if i == 1 && rec.Code != http.StatusOK {
			t.Fatalf("retry after 5xx: status %d, want the handler to run again", rec.Code)
		}
	}
	if calls != 2 {
		t.Fatalf("handler ran %d times, want 2", calls)
	}
}

// ---------------------------------------------------------------------------
// Account
// ---------------------------------------------------------------------------

func TestAccountEndpoints(t *testing.T) {
	ta := newTestAPI(t)

	rec := ta.do(t, "GET", "/v1/whoami", "alice", nil)
	var me ctlops.Whoami
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Handle != "alice" || me.Operator {
		t.Fatalf("whoami %+v", me)
	}
	// The one field an HTTP session can never have.
	if me.KeyFP != "" {
		t.Fatalf("key_fp leaked over HTTP: %q", me.KeyFP)
	}

	rec = ta.do(t, "GET", "/v1/capabilities", "alice", nil)
	var caps ctlops.Capabilities
	if err := json.Unmarshal(rec.Body.Bytes(), &caps); err != nil {
		t.Fatal(err)
	}
	if caps.Archiving {
		t.Fatal("archiving reported enabled without object storage")
	}
	if !caps.Tags || !caps.Scheduling || !caps.Routes || !caps.SessionTokens || !caps.Terminal {
		t.Fatalf("capabilities %+v", caps)
	}

	rec = ta.do(t, "POST", "/v1/account/keys", "alice", addKeyRequest{Key: sampleKey})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add key: status %d (%s)", rec.Code, rec.Body)
	}
	var added ctlops.KeyInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if added.FP == "" || added.Label != "demo" {
		t.Fatalf("key %+v", added)
	}

	// The fingerprint's base64 alphabet includes '+' and '/', so it has to be
	// percent-encoded — a bare '+' would arrive as a space and match nothing.
	rec = ta.do(t, "DELETE", "/v1/account/keys?fingerprint="+url.QueryEscape(added.FP), "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove key: status %d (%s)", rec.Code, rec.Body)
	}
	rec = ta.do(t, "GET", "/v1/account/keys", "alice", nil)
	var keys keyList
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys.Keys) != 1 {
		t.Fatalf("after removing the added key alice has %d keys", len(keys.Keys))
	}

	rec = ta.do(t, "PUT", "/v1/account/email", "alice", emailRequest{Email: "alice@example.com"})
	if rec.Code != http.StatusOK {
		t.Fatalf("set email: status %d (%s)", rec.Code, rec.Body)
	}
	rec = ta.do(t, "GET", "/v1/account/email", "alice", nil)
	var em emailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &em); err != nil {
		t.Fatal(err)
	}
	if em.Email != "alice@example.com" {
		t.Fatalf("email %q", em.Email)
	}

	// The minted token really authenticates this API — the SSH-to-HTTP bridge,
	// closed here without SSH.
	rec = ta.do(t, "POST", "/v1/account/tokens", "alice", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: status %d (%s)", rec.Code, rec.Body)
	}
	var tok ctlops.TokenResult
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	viaToken := httptest.NewRecorder()
	ta.h.ServeHTTP(viaToken, req)
	if viaToken.Code != http.StatusOK {
		t.Fatalf("minted token did not authenticate: status %d (%s)", viaToken.Code, viaToken.Body)
	}
}

// ---------------------------------------------------------------------------
// Terminal
// ---------------------------------------------------------------------------

type recordingTerminal struct {
	handle  string
	sandbox string
	called  bool
}

func (rt *recordingTerminal) ServeTerminal(w http.ResponseWriter, r *http.Request,
	c ctlops.Caller, sandbox string) {
	rt.called, rt.handle, rt.sandbox = true, c.Handle, sandbox
	w.WriteHeader(http.StatusSwitchingProtocols)
}

func TestTerminalEndpoint(t *testing.T) {
	ta := newTestAPI(t)
	ta.create(t, "demo", "alice")

	// Not configured: a clear 501, not a panic.
	rec := ta.do(t, "GET", "/v1/sandboxes/demo/terminal", "alice", nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("without a bridge: status %d, want 501 (%s)", rec.Code, rec.Body)
	}

	// Configured: the handler proves who is asking and hands over the name.
	bridge := &recordingTerminal{}
	ta.handler.terminal = bridge
	ta.h = ta.handler.Handler()
	rec = ta.do(t, "GET", "/v1/sandboxes/demo/terminal", "alice", nil)
	if !bridge.called {
		t.Fatal("the bridge was never reached")
	}
	if bridge.handle != "alice" || bridge.sandbox != "demo" {
		t.Fatalf("bridge saw handle=%q sandbox=%q", bridge.handle, bridge.sandbox)
	}
	if rec.Code != http.StatusSwitchingProtocols {
		t.Fatalf("status %d", rec.Code)
	}

	// Still gated: an unauthenticated handshake never reaches the bridge.
	bridge.called = false
	rec = ta.do(t, "GET", "/v1/sandboxes/demo/terminal", "", nil)
	if rec.Code != http.StatusUnauthorized || bridge.called {
		t.Fatalf("unauthenticated: status %d, bridge called %v", rec.Code, bridge.called)
	}

	// Owner-gated before the upgrade, and masked: a stranger gets the 404 an
	// absent sandbox gets, with no socket ever opened. Were this check left to
	// the bridge it could only be reported as a close code, which is both
	// unreadable to a client and one more place to forget it.
	bridge.called = false
	rec = ta.do(t, "GET", "/v1/sandboxes/demo/terminal", "mallory", nil)
	ghost := ta.do(t, "GET", "/v1/sandboxes/ghost/terminal", "mallory", nil)
	if rec.Code != http.StatusNotFound || bridge.called {
		t.Fatalf("cross-owner: status %d, bridge called %v (%s)", rec.Code, bridge.called, rec.Body)
	}
	// Identical modulo the name the caller typed, which is the whole point:
	// nothing in the answer says whether "demo" exists.
	if got, want := rec.Body.String(), strings.ReplaceAll(ghost.Body.String(), "ghost", "demo"); got != want {
		t.Fatalf("cross-owner leaks existence:\n someone else's: %s absent: %s", got, ghost.Body)
	}
}

// ---------------------------------------------------------------------------
// Pages
// ---------------------------------------------------------------------------

func TestDocsPageIsSelfContained(t *testing.T) {
	ta := newTestAPI(t)
	rec := ta.do(t, "GET", "/docs", "", nil)
	body := rec.Body.String()

	if !strings.Contains(body, "sparkbox") {
		t.Fatal("the docs page does not mention sparkbox")
	}
	// The shared design system was composed in: the markers are gone and the
	// tokens they carry are present.
	for _, marker := range []string{"/*SHARED_CSS*/", "/*SHARED_JS*/"} {
		if strings.Contains(body, marker) {
			t.Fatalf("%s was never replaced — webui.Build did not run", marker)
		}
	}
	if !strings.Contains(body, "--muted-foreground") {
		t.Fatal("the shared CSS tokens are missing")
	}
	// Nothing may be fetched from anywhere but this origin.
	for _, external := range []string{"https://cdn", "http://cdn", "unpkg", "jsdelivr", "//fonts."} {
		if strings.Contains(body, external) {
			t.Fatalf("the docs page references %q", external)
		}
	}
	if !strings.Contains(body, "/openapi.json") {
		t.Fatal("the docs page never loads the spec")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Fatalf("CSP %q", csp)
	}

	// The preview tooling injects around these literal tags, so the minifier
	// must keep them.
	for _, tag := range []string{"</head>", "<body>"} {
		if !strings.Contains(body, tag) {
			t.Fatalf("minification ate %s", tag)
		}
	}
}

func TestSpecIsServedAndNamesThisHost(t *testing.T) {
	ta := newTestAPI(t)

	rec := ta.do(t, "GET", "/openapi.json", "", nil)
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served spec is not JSON: %v", err)
	}
	if strings.Contains(rec.Body.String(), specDomain) {
		t.Fatalf("the served spec still names %s", specDomain)
	}
	if !strings.Contains(rec.Body.String(), "https://api."+testDomain) {
		t.Fatal("the served spec does not name this host's API")
	}

	// gzip is pre-computed, not per-request.
	req := httptest.NewRequest("GET", "/openapi.json", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	gz := httptest.NewRecorder()
	ta.h.ServeHTTP(gz, req)
	if gz.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("gzip was not offered")
	}
	if gz.Body.Len() >= rec.Body.Len() {
		t.Fatal("the gzipped spec is not smaller")
	}

	yml := ta.do(t, "GET", "/openapi.yaml", "", nil)
	if !strings.Contains(yml.Body.String(), "openapi: \"3.1.1\"") {
		t.Fatalf("YAML does not start with the version: %.120s", yml.Body.String())
	}
}

// A create that names a machine this host does not have must be refused rather
// than quietly built here. The rig wires a bare *host.Manager as its sandbox
// store — a single box, exactly as most deployments are — so the honest answer
// is that placing by name is not a thing this host can do.
//
// The regression it guards is the silent one: a `node` field that never reached
// CreateArgs would come back 201, with a sandbox on the wrong machine and
// nothing said about it.
func TestCreateOnANamedNodeWithoutAFleet(t *testing.T) {
	ta := newTestAPI(t)

	rec := ta.do(t, "POST", "/v1/sandboxes", "alice", createRequest{Name: "demo", Node: "dgx"})
	if rec.Code == http.StatusCreated {
		t.Fatalf("a single-box host accepted a placement: %s", rec.Body)
	}
	var env errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Kind != ctlops.KindDisabled.String() {
		t.Fatalf("kind %q, want %q (%s)", env.Error.Kind, ctlops.KindDisabled, rec.Body)
	}
	if _, ok := ta.mgr.Get("demo"); ok {
		t.Error("the refused create built the sandbox here anyway")
	}
}
