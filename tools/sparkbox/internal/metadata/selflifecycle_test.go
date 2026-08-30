package metadata

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// fakeLifecycle records WHEN each destructive method was entered, because the
// whole feature is an ordering property and a boolean cannot express it.
type fakeLifecycle struct {
	mu        sync.Mutex
	plan      ctlops.SelfSnapshotPlan
	planErr   error
	pauses    int
	snaps     int
	args      ctlops.SnapshotToTagArgs
	enteredAt time.Time
	entered   chan struct{}
}

func newFakeLifecycle() *fakeLifecycle {
	return &fakeLifecycle{
		entered: make(chan struct{}, 4),
		plan: ctlops.SelfSnapshotPlan{
			Sandbox: "alice-box", Tags: []string{"default", "web"}, Tag: "web",
			Snapshot: "web-260829-1412", DiskMB: 4300,
			CtlHint: "ssh ctl@catnip.sh", SSHHint: "ssh alice-box.catnip.sh",
			Carriers: []ctlops.TaggedSandbox{{Name: "alice-box", State: "running", Self: true}},
			Token:    "tok-abc",
		},
	}
}

func (f *fakeLifecycle) mark(pause bool, a ctlops.SnapshotToTagArgs) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enteredAt = time.Now()
	if pause {
		f.pauses++
	} else {
		f.snaps++
		f.args = a
	}
	select {
	case f.entered <- struct{}{}:
	default:
	}
}

func (f *fakeLifecycle) Pause(_ context.Context, _ *host.Sandbox) error {
	f.mark(true, ctlops.SnapshotToTagArgs{})
	return nil
}

func (f *fakeLifecycle) PlanSnapshot(_ context.Context, box *host.Sandbox, tag, name string) (ctlops.SelfSnapshotPlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.planErr != nil {
		return ctlops.SelfSnapshotPlan{}, f.planErr
	}
	p := f.plan
	p.Sandbox = box.Name
	if tag != "" {
		p.Tag = tag
	}
	if name != "" {
		p.Snapshot = name
	}
	return p, nil
}

func (f *fakeLifecycle) Snapshot(_ context.Context, _ *host.Sandbox, a ctlops.SnapshotToTagArgs) error {
	f.mark(false, a)
	return nil
}

func (f *fakeLifecycle) counts() (pauses, snaps int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pauses, f.snaps
}

func (f *fakeLifecycle) awaitEntered(t *testing.T, d time.Duration) time.Time {
	t.Helper()
	select {
	case <-f.entered:
	case <-time.After(d):
		t.Fatalf("the destructive half never started within %v", d)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enteredAt
}

// lifecycleFixture is the standard two-sandbox fixture with a lifecycle wired.
func lifecycleFixture(t *testing.T) (*Server, *fakeLifecycle) {
	t.Helper()
	s := fixture(t)
	life := newFakeLifecycle()
	s.lifecycle = life
	s.allowSelfSnapshot = true
	return s, life
}

// guestServer runs a real HTTP server in front of the handler, rewriting each
// connection's addresses into the slot the fixture's alice-box occupies.
//
// A real server rather than httptest.ResponseRecorder because the property
// under test is a TCP one: the handler waits for the client to CLOSE the
// connection, which a recorder has no way to express.
func guestServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = net.JoinHostPort("172.30.5.2", "40000")
		r = r.WithContext(context.WithValue(r.Context(), localAddrKey{},
			&net.TCPAddr{IP: net.ParseIP("172.30.5.1"), Port: DefaultPort}))
		s.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// oneShotClient closes its connection as soon as a response body is closed,
// which is what makes the read receipt observable at all: with keep-alives the
// connection outlives the request and the server never sees a close.
func oneShotClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

// TestTheGuestReadsTheAcceptanceBeforeAnythingStops is THE test for this
// feature. The box that issues the request is the box that stops, so the whole
// design rests on the destructive half not starting until the guest holds the
// complete response.
//
// It also pins the numeric Content-Length, and that is not a second assertion
// about the same thing: without it a Flush switches the connection to chunked
// encoding and the client waits for a terminating chunk net/http only writes
// when the handler RETURNS — after the wait — so every call would deadlock into
// the 2s cap and the receipt would silently degrade to a timer that still
// passes the ordering check above.
func TestTheGuestReadsTheAcceptanceBeforeAnythingStops(t *testing.T) {
	for _, tc := range []struct{ name, method, path string }{
		{"pause", http.MethodPost, "/self/pause"},
		{"snapshot commit", http.MethodPost, "/self/snapshot?tag=web&name=web-260829-1412&plan=tok-abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, life := lifecycleFixture(t)
			srv := guestServer(t, s)

			req, err := http.NewRequest(tc.method, srv.URL+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Accept", "text/plain")
			resp, err := oneShotClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("status = %d, want 202", resp.StatusCode)
			}
			if n, cerr := strconv.Atoi(resp.Header.Get("Content-Length")); cerr != nil || n <= 0 {
				t.Fatalf("Content-Length = %q — a flushed response without one goes chunked, and the "+
					"client then waits for a terminating chunk that only arrives when the handler returns",
					resp.Header.Get("Content-Length"))
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			readAt := time.Now()
			resp.Body.Close()
			if len(body) == 0 {
				t.Fatal("empty acceptance body")
			}

			entered := life.awaitEntered(t, 5*time.Second)
			if !entered.After(readAt) {
				t.Errorf("the destructive half started at %v, before the guest finished reading at %v — "+
					"the acceptance can be lost the moment the VM stops", entered, readAt)
			}
			// And it must be prompt: the receipt is the guest's own FIN, not the
			// fail-soft cap, so the gap is milliseconds rather than seconds.
			if gap := entered.Sub(readAt); gap > ackGrace {
				t.Errorf("waited %v after the read — the FIN was not seen and this fell through to the cap", gap)
			}
		})
	}
}

// TestTheCapReleasesAGuestThatNeverCloses: the receipt fails soft. A client
// that reads the body and holds the connection open still gets its capture
// started, within the cap.
func TestTheCapReleasesAGuestThatNeverCloses(t *testing.T) {
	s, life := lifecycleFixture(t)
	srv := guestServer(t, s)

	// A raw connection so nothing on this side can close it for us.
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "POST /self/pause HTTP/1.1\r\nHost: x\r\nAccept: text/plain\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	life.awaitEntered(t, 5*time.Second)
	if elapsed := time.Since(start); elapsed > 2*ackGrace {
		t.Errorf("the cap took %v to release; ackGrace is %v", elapsed, ackGrace)
	}
	if pauses, _ := life.counts(); pauses != 1 {
		t.Errorf("pauses = %d, want 1", pauses)
	}
}

// TestNothingMovesOnARefusedRequest is the second half of the contract: every
// refusal is answered while the VM is alive, and none of them reaches the half
// that stops it.
func TestNothingMovesOnARefusedRequest(t *testing.T) {
	refusals := []struct {
		name   string
		err    error
		status int
	}{
		{"default tag", &ctlops.Error{Kind: ctlops.KindDenied, Op: "snapshot.self",
			Code: "tag_is_default", Msg: "a template cannot be bound to the tag \"default\"", Verbatim: true}, 403},
		{"tag not carried", &ctlops.Error{Kind: ctlops.KindDenied, Op: "snapshot.self",
			Code: "tag_not_on_sandbox", Msg: "alice-box does not carry the tag `cuda`", Verbatim: true}, 403},
		{"bad grammar", ctlops.Invalid("snapshot.self", "bad_tag", "invalid tag %q", "Web_1"), 400},
		{"no candidate tag", ctlops.Invalid("snapshot.self", "no_candidate_tag", "no tag to capture into"), 400},
		{"name taken", &ctlops.Error{Kind: ctlops.KindConflict, Op: "snapshot.self",
			Code: "snapshot_name_taken", Msg: "a snapshot named \"web-260829-1412\" already exists", Verbatim: true}, 409},
		{"snapshots unsupported", ctlops.Disabled("snapshot.self", "this host cannot capture templates"), 501},
		{"bindings unavailable", ctlops.Disabled("snapshot.self", "this gateway cannot bind a snapshot to a tag"), 501},
		{"gateway unreachable", &ctlops.Error{Kind: ctlops.KindCapacity, Op: "snapshot.self",
			Code: ctlops.CodeNodeUnreachable, Msg: "the machine holding this sandbox is not answering", Verbatim: true}, 503},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			for _, route := range []struct{ method, path string }{
				{http.MethodGet, "/self/snapshot?tag=web"},
				{http.MethodPost, "/self/snapshot?tag=web&name=web-260829-1412&plan=tok-abc"},
			} {
				s, life := lifecycleFixture(t)
				life.planErr = tc.err
				rec := requestMethod(s, route.method, route.path, "172.30.5.2", "172.30.5.1")
				if rec.Code != tc.status {
					t.Errorf("%s %s = %d, want %d (%s)", route.method, route.path, rec.Code, tc.status, rec.Body.String())
				}
				if pauses, snaps := life.counts(); pauses != 0 || snaps != 0 {
					t.Errorf("%s %s: a refused request reached the destructive half (%d pauses, %d captures)",
						route.method, route.path, pauses, snaps)
				}
			}
		})
	}
}

// TestAStalePlanIsRefusedBeforeThePause: the user agreed to warnings about a
// world that has since moved. The commit re-plans and compares, so this is
// answered on a live connection rather than acted on and reported to nobody.
func TestAStalePlanIsRefusedBeforeThePause(t *testing.T) {
	s, life := lifecycleFixture(t)
	rec := requestMethod(s, http.MethodPost,
		"/self/snapshot?tag=web&name=web-260829-1412&plan=tok-from-an-older-plan", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if _, snaps := life.counts(); snaps != 0 {
		t.Errorf("a stale plan started %d captures", snaps)
	}
}

// TestTheCommitCapturesThePlansOwnNameAndTag: the guest sends back what it was
// shown, and the host acts on what IT re-derived. A commit that re-derived a
// name would capture under one nobody was shown — the derived name carries a
// minute in it, so a slow prompt is enough.
func TestTheCommitCapturesThePlansOwnNameAndTag(t *testing.T) {
	s, life := lifecycleFixture(t)
	srv := guestServer(t, s)
	req, _ := http.NewRequest(http.MethodPost,
		srv.URL+"/self/snapshot?tag=web&name=web-260829-1412&plan=tok-abc", nil)
	req.Header.Set("Accept", "text/plain")
	resp, err := oneShotClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body) //nolint:errcheck
	resp.Body.Close()
	life.awaitEntered(t, 5*time.Second)

	life.mu.Lock()
	defer life.mu.Unlock()
	want := ctlops.SnapshotToTagArgs{Sandbox: "alice-box", Name: "web-260829-1412", Tag: "web"}
	if life.args != want {
		t.Errorf("captured %+v, want %+v", life.args, want)
	}
}

// TestTheLifecycleRoutesAreGatedLikeEveryOtherSelfRoute. Three routes that can
// stop a VM must not be reachable from anywhere the pin route is not.
func TestTheLifecycleRoutesAreGatedLikeEveryOtherSelfRoute(t *testing.T) {
	routes := []struct{ method, path string }{
		{http.MethodPost, "/self/pause"},
		{http.MethodGet, "/self/snapshot"},
		{http.MethodPost, "/self/snapshot"},
	}
	for _, route := range routes {
		for _, bad := range []struct{ name, src, dst string }{
			// Not a guest address at all.
			{"outside the guest range", "10.0.0.5", "172.30.5.1"},
			// A guest reaching another slot's host address: the kernel would
			// deliver it, so this is the check that has to refuse it.
			{"another slot's gateway", "172.30.5.2", "172.30.9.1"},
			// A guest address with no sandbox behind it.
			{"no sandbox at that address", "172.30.7.2", "172.30.7.1"},
		} {
			s, life := lifecycleFixture(t)
			rec := requestMethod(s, route.method, route.path, bad.src, bad.dst)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s from %s: status = %d, want 403", route.method, route.path, bad.name, rec.Code)
			}
			if pauses, snaps := life.counts(); pauses != 0 || snaps != 0 {
				t.Errorf("%s %s from %s reached the destructive half", route.method, route.path, bad.name)
			}
		}
	}
}

// TestAHostWithoutTheFeatureAnswers501 — and answers it the same way whether it
// has no control plane at all or an operator turned the capture verb off, which
// is the same fact from a guest's point of view.
func TestAHostWithoutTheFeatureAnswers501(t *testing.T) {
	t.Run("no lifecycle", func(t *testing.T) {
		s := fixture(t)
		for _, route := range []struct{ method, path string }{
			{http.MethodPost, "/self/pause"},
			{http.MethodGet, "/self/snapshot"},
			{http.MethodPost, "/self/snapshot"},
		} {
			rec := requestMethod(s, route.method, route.path, "172.30.5.2", "172.30.5.1")
			if rec.Code != http.StatusNotImplemented {
				t.Errorf("%s %s = %d, want 501", route.method, route.path, rec.Code)
			}
		}
	})
	t.Run("capture switched off", func(t *testing.T) {
		s, life := lifecycleFixture(t)
		s.allowSelfSnapshot = false
		for _, route := range []struct{ method, path string }{
			{http.MethodGet, "/self/snapshot"},
			{http.MethodPost, "/self/snapshot"},
		} {
			rec := requestMethod(s, route.method, route.path, "172.30.5.2", "172.30.5.1")
			if rec.Code != http.StatusNotImplemented {
				t.Errorf("%s %s = %d, want 501", route.method, route.path, rec.Code)
			}
		}
		// pause is deliberately NOT behind the switch: a guest can already halt,
		// and this is a cleaner transition than the one it has.
		rec := requestMethod(s, http.MethodPost, "/self/pause", "172.30.5.2", "172.30.5.1")
		if rec.Code != http.StatusAccepted {
			t.Errorf("pause = %d with the capture switch off, want 202", rec.Code)
		}
		life.awaitEntered(t, 5*time.Second)
	})
}

// TestTheCaptureBudgetIsItsOwn: three an hour, per sandbox, and spending it does
// not cost the guest its identity. repos.go states the reason a shared window
// would — one workload starving the OIDC refresh — and this is the third budget
// in the same building.
func TestTheCaptureBudgetIsItsOwn(t *testing.T) {
	s, _ := lifecycleFixture(t)
	commit := "/self/snapshot?tag=web&name=web-260829-1412&plan=tok-abc"
	for i := range selfBurst {
		if rec := requestMethod(s, http.MethodPost, commit, "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusAccepted {
			t.Fatalf("capture %d = %d, want 202", i+1, rec.Code)
		}
	}
	if rec := requestMethod(s, http.MethodPost, commit, "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("capture %d = %d, want 429", selfBurst+1, rec.Code)
	}
	// Another sandbox is unaffected: the window is keyed per box.
	if rec := requestMethod(s, http.MethodPost, commit, "172.30.9.2", "172.30.9.1"); rec.Code != http.StatusAccepted {
		t.Errorf("a second sandbox's first capture = %d, want 202", rec.Code)
	}
	// And the mint still works, which is the whole reason this window is not
	// /token's.
	if rec := request(s, "/token", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
		t.Errorf("token after exhausting the capture budget = %d, want 200", rec.Code)
	}
	// The plan keeps its own, larger budget: a guest that used up its captures
	// can still read what it would have done.
	if rec := request(s, "/self/snapshot?tag=web", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
		t.Errorf("plan after exhausting the capture budget = %d, want 200", rec.Code)
	}
}

// TestThePlanRendersTheContract pins the copy byte-for-byte. These sentences are
// the whole user interface of a command that ends somebody's session, and the
// guest prints the body verbatim — so this test is the only thing standing
// between a careless edit and a warning that no longer says what it means.
func TestThePlanRendersTheContract(t *testing.T) {
	base := ctlops.SelfSnapshotPlan{
		Sandbox: "quiet-lake", Tags: []string{"default", "web"}, Tag: "web",
		Snapshot: "web-260829-1412", DiskMB: 4300,
		CtlHint: "ssh ctl@catnip.sh", SSHHint: "ssh quiet-lake.catnip.sh",
		Carriers: []ctlops.TaggedSandbox{{Name: "quiet-lake", State: "running", Self: true}},
	}

	wantFirst := `
  this sandbox   quiet-lake   tags: default, web
  capture as     web-260829-1412   (new)
  tag ` + "`web`" + `      boots the stock template
                 -> boots web-260829-1412

  ! quiet-lake is the only sandbox carrying ` + "`web`" + `. Binding does not re-base
    it: it keeps the rootfs it was created from. Only sandboxes created after
    this boot from web-260829-1412.

  This pauses quiet-lake and ends this session. 4.2 GB to compact, so the
  capture takes a minute or two and runs after you are gone. Nothing is lost —
  ` + "`ssh quiet-lake.catnip.sh`" + ` resumes it exactly where you left it.

`
	if got := renderPlan(base); got != wantFirst {
		t.Errorf("first capture of a tag:\n--- got ---\n%s\n--- want ---\n%s", got, wantFirst)
	}

	repoint := base
	repoint.Bound = "web-260812-0930"
	repoint.BoundFrom = "blue-meadow"
	repoint.BoundAt = time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC)
	repoint.Carriers = []ctlops.TaggedSandbox{
		{Name: "quiet-lake", State: "running", Self: true},
		{Name: "amber-hill", State: "running"},
		{Name: "still-fjord", State: "paused"},
	}
	wantRepoint := `
  this sandbox   quiet-lake   tags: default, web
  capture as     web-260829-1412   (new)
  tag ` + "`web`" + `      boots web-260812-0930 (captured 2026-08-12 from blue-meadow)
                 -> boots web-260829-1412

  ! ` + "`web`" + ` already points at web-260812-0930. This re-points it. The old
    snapshot is kept, and you can put it back with:
      ssh ctl@catnip.sh snapshot bind web-260812-0930 --tag web

  ! 3 of your sandboxes carry ` + "`web`" + `:
      quiet-lake    running   (this one)
      amber-hill    running
      still-fjord   paused
    Re-pointing does not re-base any of them. Running or paused, they keep the
    rootfs they were created from — this one included. Only sandboxes created
    after this boot from web-260829-1412.

  This pauses quiet-lake and ends this session. 4.2 GB to compact, so the
  capture takes a minute or two and runs after you are gone. Nothing is lost —
  ` + "`ssh quiet-lake.catnip.sh`" + ` resumes it exactly where you left it.

`
	if got := renderPlan(repoint); got != wantRepoint {
		t.Errorf("re-point with three carriers:\n--- got ---\n%s\n--- want ---\n%s", got, wantRepoint)
	}

	// The disk-busy note is a warning and never a gate, so it says the capture
	// WILL happen — later — rather than that it was refused.
	busy := base
	busy.Busy = "archive"
	if got := renderPlan(busy); !strings.Contains(got,
		"  ! another disk operation is running on this sandbox (archive). The capture\n"+
			"    waits for it to finish before it pauses, which can take several minutes.\n"+
			"    Until then this session stays open and nothing has changed.\n") {
		t.Errorf("disk-busy note:\n%s", got)
	}

	// The fleet note. Re-pointing a tag also moves where this owner's FUTURE
	// sandboxes are placed, which is the least obvious consequence of a command
	// that looks like it is about images.
	fleet := base
	fleet.Node = "sparky"
	fleet.Bound = "web-260812-0930"
	fleet.BoundNode = "laptop"
	if got := renderPlan(fleet); !strings.Contains(got, `  placement      web-260812-0930 lives on node "laptop"; this capture will`) ||
		!strings.Contains(got, "so they will move to sparky.") {
		t.Errorf("fleet placement note:\n%s", got)
	}
}

func TestTheAcceptanceNamesWhereTheOutcomeLands(t *testing.T) {
	p := ctlops.SelfSnapshotPlan{
		Sandbox: "quiet-lake", Tag: "web", Snapshot: "web-260829-1412",
		CtlHint: "ssh ctl@catnip.sh",
	}
	want := "accepted — capturing quiet-lake as web-260829-1412, then binding `web` to it.\n" +
		"Pausing now. The rest runs on the gateway and you will not see it here:\n" +
		"  ssh ctl@catnip.sh snapshot ls\n"
	if got := renderAccepted(p); got != want {
		t.Errorf("acceptance:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestThePauseTellsAPinnedBoxItWillComeBack. Pinning is the one fact that makes
// the outcome differ from what somebody typing `pause` expects.
func TestThePauseTellsAPinnedBoxItWillComeBack(t *testing.T) {
	plain := renderPauseAccepted(&host.Sandbox{Name: "quiet-lake"})
	want := "pausing quiet-lake — memory and processes are snapshotted, so\n" +
		"`ssh quiet-lake` picks up exactly here.\n"
	if plain != want {
		t.Errorf("pause:\n--- got ---\n%s\n--- want ---\n%s", plain, want)
	}
	pinned := renderPauseAccepted(&host.Sandbox{Name: "quiet-lake", Pinned: true})
	if !strings.HasPrefix(pinned, "note: quiet-lake is pinned, so a host restart will bring it back up on its own.\n"+
		"      Run `sparkbox unpin` first if you want it to stay down.\n\n") {
		t.Errorf("a pinned box was not warned:\n%s", pinned)
	}
	if !strings.HasSuffix(pinned, want) {
		t.Errorf("the pinned note replaced the pause line instead of preceding it:\n%s", pinned)
	}
}

// TestContentNegotiationCarriesTheSameFacts: the guest asks for text and prints
// it; anything else gets the document. Neither may be the only one that knows
// something.
func TestContentNegotiationCarriesTheSameFacts(t *testing.T) {
	s, _ := lifecycleFixture(t)

	text := requestAccept(s, http.MethodGet, "/self/snapshot?tag=web", "text/plain")
	if ct := text.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("text/plain request answered %q", ct)
	}
	for _, want := range []string{"alice-box", "web-260829-1412", "`web`"} {
		if !strings.Contains(text.Body.String(), want) {
			t.Errorf("rendered plan missing %q:\n%s", want, text.Body.String())
		}
	}

	doc := requestAccept(s, http.MethodGet, "/self/snapshot?tag=web", "")
	if ct := doc.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("default request answered %q, want application/json", ct)
	}
	var got ctlops.SelfSnapshotPlan
	if err := json.Unmarshal(doc.Body.Bytes(), &got); err != nil {
		t.Fatalf("plan JSON: %v (%s)", err, doc.Body.String())
	}
	if got.Sandbox != "alice-box" || got.Tag != "web" || got.Snapshot != "web-260829-1412" {
		t.Errorf("plan JSON = %+v", got)
	}

	// The four headers the POSIX-sh side reads instead of parsing anything.
	for header, want := range map[string]string{
		"Sparkbox-Tag": "web", "Sparkbox-Snapshot": "web-260829-1412",
		"Sparkbox-Plan": "tok-abc", "Sparkbox-Ctl": "ssh ctl@catnip.sh",
	} {
		if got := text.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// TestARefusalReachesTheGuestVerbatim. The host's sentence is the only
// explanation that exists — the shell deliberately drops `curl -f` so it can
// print this rather than curl's generic "returned error: 409".
func TestARefusalReachesTheGuestVerbatim(t *testing.T) {
	s, life := lifecycleFixture(t)
	life.planErr = &ctlops.Error{
		Kind: ctlops.KindDenied, Op: "snapshot.self", Code: "tag_not_on_sandbox",
		Msg:      "alice-box does not carry the tag `cuda`, and a sandbox may only re-point a tag it already carries.",
		Verbatim: true,
	}
	rec := requestAccept(s, http.MethodGet, "/self/snapshot?tag=cuda", "text/plain")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	want := "sparkbox: alice-box does not carry the tag `cuda`, and a sandbox may only re-point a tag it already carries.\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}

	// And as a document for a caller that wanted one, with the machine token
	// intact so a client never has to match on the sentence.
	rec = requestAccept(s, http.MethodGet, "/self/snapshot?tag=cuda", "application/json")
	var doc selfErrorDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("error JSON: %v (%s)", err, rec.Body.String())
	}
	if doc.Code != "tag_not_on_sandbox" || doc.Kind != "denied" {
		t.Errorf("error doc = %+v", doc)
	}
}

// TestTheRenderedPlanCarriesNoControlBytes. Every value in a plan is
// host-authored — a tag from tagRe, a snapshot name from snapNameRe, a sandbox
// name the manager minted — and this pins that the rendering cannot smuggle a
// carriage return or an escape into somebody's terminal even if one of those
// grammars is ever loosened.
func TestTheRenderedPlanCarriesNoControlBytes(t *testing.T) {
	p := ctlops.SelfSnapshotPlan{
		Sandbox: "quiet-lake", Tags: []string{"default", "web"}, Tag: "web",
		Snapshot: "web-260829-1412", Bound: "web-260812-0930", BoundFrom: "blue-meadow",
		Busy: "archive", Turbo: true, DiskMB: 4300, Node: "sparky", BoundNode: "laptop",
		CtlHint: "ssh ctl@catnip.sh", SSHHint: "ssh quiet-lake.catnip.sh",
		Carriers: []ctlops.TaggedSandbox{
			{Name: "quiet-lake", State: "running", Self: true},
			{Name: "amber-hill", State: "paused"},
		},
	}
	for _, text := range []string{renderPlan(p), renderAccepted(p)} {
		for i, r := range text {
			if r != '\n' && r < 0x20 || r == 0x7f {
				t.Errorf("control byte %#x at offset %d in:\n%s", r, i, text)
			}
		}
	}
}

// requestAccept drives a handler with an explicit Accept header.
func requestAccept(s *Server, method, path, accept string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.RemoteAddr = net.JoinHostPort("172.30.5.2", "40000")
	if accept != "" {
		r.Header.Set("Accept", accept)
	}
	r = r.WithContext(context.WithValue(r.Context(), localAddrKey{},
		&net.TCPAddr{IP: net.ParseIP("172.30.5.1"), Port: DefaultPort}))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}
