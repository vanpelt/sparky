package metadata

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// fakeEnvSetup is the gateway half of an environment build, recording what it
// was handed and when the recording started.
type fakeEnvSetup struct {
	mu sync.Mutex

	script string
	env    string
	has    bool
	err    error

	boxes     []string
	results   []SetupResult
	doneErr   error
	entered   chan struct{}
	enteredAt time.Time
	mode      string // "" means SetupModeScript
}

func newFakeEnvSetup() *fakeEnvSetup {
	return &fakeEnvSetup{entered: make(chan struct{}, 4)}
}

func (f *fakeEnvSetup) SetupFor(_ context.Context, box *host.Sandbox) (SetupJob, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return SetupJob{}, false, f.err
	}
	// Only the box the fixture nominated as the builder has a job; every other
	// sandbox in the fleet asks the same question and is told no.
	if !f.has || box.Name != "alice-box" {
		return SetupJob{}, false, nil
	}
	mode := f.mode
	if mode == "" {
		mode = SetupModeScript
	}
	return SetupJob{Env: f.env, Mode: mode, Payload: f.script}, true, nil
}

func (f *fakeEnvSetup) SetupDone(_ context.Context, box *host.Sandbox, r SetupResult) error {
	f.mu.Lock()
	f.boxes = append(f.boxes, box.Name)
	f.results = append(f.results, r)
	f.enteredAt = time.Now()
	err := f.doneErr
	f.mu.Unlock()
	select {
	case f.entered <- struct{}{}:
	default:
	}
	return err
}

func (f *fakeEnvSetup) reports() []SetupResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SetupResult(nil), f.results...)
}

func (f *fakeEnvSetup) awaitEntered(t *testing.T, d time.Duration) time.Time {
	t.Helper()
	select {
	case <-f.entered:
	case <-time.After(d):
		t.Fatalf("the result was never recorded within %v", d)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enteredAt
}

// envSetupFixture is the standard two-sandbox fixture with a build wired to
// alice-box.
func envSetupFixture(t *testing.T) (*Server, *fakeEnvSetup) {
	t.Helper()
	s := fixture(t)
	env := newFakeEnvSetup()
	env.has, env.env, env.script = true, "webapp", "#!/usr/bin/env bash\nset -euo pipefail\npnpm install\n"
	s.SetEnvSetup(env)
	return s, env
}

// postReport drives the report route through a recorder whose request context
// is already cancelled.
//
// That cancellation stands in for the guest's own FIN. ackThenAct waits for the
// client to close a connection it has fully read, and a ResponseRecorder can
// never deliver one, so every accepted report through a recorder would
// otherwise sit out the whole fail-soft cap. The response bytes still land in
// the recorder and the detached half still runs on the server's base context,
// so nothing under test is skipped — the receipt itself is pinned over a real
// TCP connection in TestTheGuestHoldsTheAcceptanceBeforeTheCaptureStarts.
func postReport(s *Server, body, src, dst string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/self/setup/result", strings.NewReader(body))
	r.RemoteAddr = net.JoinHostPort(src, "40000")
	ctx, cancel := context.WithCancel(context.WithValue(r.Context(), localAddrKey{},
		&net.TCPAddr{IP: net.ParseIP(dst), Port: DefaultPort}))
	cancel()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r.WithContext(ctx))
	return rec
}

// setupReport builds a well-formed body: three lines and then the log tail
// verbatim, exactly as sparkbox-env-setup writes it in the guest.
func setupReport(status string, code int, script, log string) string {
	return status + "\n" + strconv.Itoa(code) + "\n" +
		base64.StdEncoding.EncodeToString([]byte(script)) + "\n" + log
}

// TestASandboxWithNoBuildIsToldSoCheaply: 204 is the answer for nearly every VM
// in the fleet, so it must be an empty body and not a document that has to be
// parsed to discover it says nothing.
func TestASandboxWithNoBuildIsToldSoCheaply(t *testing.T) {
	s, _ := envSetupFixture(t)
	// bob-box is in slot 9 and is not the builder.
	rec := request(s, "/self/setup", "172.30.9.2", "172.30.9.1")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %q)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

// TestAnUnrenderableModeIsRefusedHereRatherThanShippedToAGuest.
//
// The check lives in the RENDERER because internal/metadata.Server is the one
// piece of this path that a gateway's own guest and a node's guest both run.
// Hoisting it into ctlops or fleet would validate it for one kind of guest and
// skip it for the other — which is the shape of the Phase B bug where /self/setup
// answered 501 on every node and 200 on the gateway.
//
// Failing here costs nothing. Shipping an unknown mode costs a builder boot to
// produce a refusal this host could have produced immediately, and shipping an
// EMPTY one is worse than either: it is the state a relay produces when it
// drops a field, and the guest cannot tell it from a gateway that meant it.
func TestAnUnrenderableModeIsRefusedHereRatherThanShippedToAGuest(t *testing.T) {
	for _, mode := range []string{"", "telepathy", "script\nagent", "SCRIPT"} {
		t.Run("mode "+strconv.Quote(mode), func(t *testing.T) {
			s, env := envSetupFixture(t)
			env.mode = mode
			// A mode of "" would take the fake's default, so it is forced.
			if mode == "" {
				env.mode = " "
			}
			rec := request(s, "/self/setup", "172.30.5.2", "172.30.5.1")
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 — an unrenderable job is a host bug, not a job (body %q)",
					rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), mode) && mode != "" {
				t.Errorf("the refusal echoed the bad mode back to the guest: %q", rec.Body.String())
			}
		})
	}
}

// TestTheBuilderIsHandedItsScriptInThreeLines pins the wire format the guest
// parses with sed and head, and the reason line 3 is base64: a setup script is
// arbitrary bytes containing the separator.
func TestTheBuilderIsHandedItsScriptInThreeLines(t *testing.T) {
	for _, tc := range []struct{ name, mode, script string }{
		{"multi-line", SetupModeScript, "#!/usr/bin/env bash\nset -e\n\necho 'hi there'\n"},
		{"empty", SetupModeScript, ""},
		{"non-ascii and control bytes", SetupModeScript, "echo \"café\"\nprintf '\\x1b[31m'\n"},
		// Agent mode rides the SAME three lines with the same base64 on line 3
		// — the payload is a prompt rather than a script, and the format does
		// not know the difference. Tabled here rather than asserted separately
		// so the two modes cannot drift into two formats.
		{"an agent prompt", SetupModeAgent, "You are configuring a fresh microVM.\nDo not ask questions.\n"},
		{"a prompt with shell metacharacters", SetupModeAgent, "write $(whoami) and `id` down\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, env := envSetupFixture(t)
			env.script = tc.script
			env.mode = tc.mode

			rec := request(s, "/self/setup", "172.30.5.2", "172.30.5.1")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			lines := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n")
			if len(lines) != 3 {
				t.Fatalf("got %d lines, want 3: %q", len(lines), rec.Body.String())
			}
			if lines[0] != "webapp" {
				t.Errorf("line 1 = %q, want the environment name", lines[0])
			}
			if lines[1] != tc.mode {
				t.Errorf("line 2 = %q, want the mode %q", lines[1], tc.mode)
			}
			got, err := base64.StdEncoding.DecodeString(lines[2])
			if err != nil {
				t.Fatalf("line 3 does not decode: %v", err)
			}
			if string(got) != tc.script {
				t.Errorf("script round-trip = %q, want %q", got, tc.script)
			}
			// Without a Content-Length the guest's curl cannot tell a complete
			// script from a truncated one.
			if n, cerr := strconv.Atoi(rec.Header().Get("Content-Length")); cerr != nil || n != rec.Body.Len() {
				t.Errorf("Content-Length = %q, body is %d bytes",
					rec.Header().Get("Content-Length"), rec.Body.Len())
			}
		})
	}
}

// TestAnOversizedSetupReportIsRefusedBeforeItIsRead: the cap is taken by
// MaxBytesReader, so the refusal costs the host the cap and not the body the
// guest chose, and nothing reaches the control plane.
func TestAnOversizedSetupReportIsRefusedBeforeItIsRead(t *testing.T) {
	s, env := envSetupFixture(t)
	huge := setupReport("ok", 0, strings.Repeat("x", maxSetupResultBody+1), "")
	rec := requestBody(s, http.MethodPost, "/self/setup/result", huge, "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %q)", rec.Code, rec.Body.String())
	}
	if got := env.reports(); len(got) != 0 {
		t.Errorf("SetupDone ran %d times on a body that was never read", len(got))
	}
}

// TestAMalformedSetupReportIsRefusedAndAppliesNothing. Each row is a body the
// host cannot read in full, and the rule is the same for all of them: a 400
// with a sentence, no panic, and nothing applied in part.
func TestAMalformedSetupReportIsRefusedAndAppliesNothing(t *testing.T) {
	oversizeScript := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", maxSetupScript+1)))
	for _, tc := range []struct{ name, body string }{
		{"empty", ""},
		{"one line", "ok\n"},
		{"unknown status word", setupReport("done", 0, "", "")},
		{"status is not a word at all", setupReport("", 0, "", "")},
		{"exit code is not a number", "ok\nzero\n\n"},
		{"exit code is negative", "failed\n-1\n\n"},
		{"exit code is out of range", "failed\n999\n\n"},
		{"script is not base64", "ok\n0\nnot base64!!\n"},
		{"script is too large", "ok\n0\n" + oversizeScript + "\n"},
		{"script is not text", "ok\n0\n" + base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, env := envSetupFixture(t)
			rec := requestBody(s, http.MethodPost, "/self/setup/result", tc.body, "172.30.5.2", "172.30.5.1")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			if !strings.HasPrefix(rec.Body.String(), "sparkbox: ") {
				t.Errorf("refusal = %q, want a plain sparkbox sentence", rec.Body.String())
			}
			if got := env.reports(); len(got) != 0 {
				t.Errorf("SetupDone ran %d times on a malformed report", len(got))
			}
		})
	}
}

// TestAReportWithNoLogAtAllIsStillAReport. The log is the last field and
// everything after the third newline is it, so a run that produced no output
// sends nothing there — and that is a complete report, not a truncated one.
func TestAReportWithNoLogAtAllIsStillAReport(t *testing.T) {
	for _, body := range []string{"ok\n0\n\n", "ok\n0\n"} {
		res, why := parseSetupResult(body)
		if why != "" {
			t.Fatalf("%q refused: %s", body, why)
		}
		if want := (SetupResult{OK: true}); res != want {
			t.Errorf("%q parsed as %#v, want %#v", body, res, want)
		}
	}
}

// TestALongLogKeepsItsTail: the field is a tail, so what survives the cap is
// the END of it — the part that says how the run stopped.
func TestALongLogKeepsItsTail(t *testing.T) {
	log := strings.Repeat("x", maxSetupLog) + "the last line\n"
	res, why := parseSetupResult(setupReport("failed", 1, "", log))
	if why != "" {
		t.Fatalf("refused a long log instead of trimming it: %s", why)
	}
	if len(res.Log) != maxSetupLog {
		t.Errorf("log kept %d bytes, want %d", len(res.Log), maxSetupLog)
	}
	if !strings.HasSuffix(res.Log, "the last line\n") {
		t.Errorf("log = %q..., want the tail kept", res.Log[:32])
	}
}

// TestALogOfInvalidBytesStillFitsTheCap is the node case. Sanitizing after
// trimming would grow the tail past maxSetupLog — each run of invalid bytes
// becomes a three-byte U+FFFD — and internal/nodelink refuses a relayed report
// whose log exceeds the same number. A build that finished on a gateway would
// then be lost on a node, which is the asymmetry the relay exists to remove.
func TestALogOfInvalidBytesStillFitsTheCap(t *testing.T) {
	// Alternating invalid byte and valid byte is the worst case: every invalid
	// byte is its own run, so every one of them triples.
	log := strings.Repeat("\xffx", maxSetupLog)
	res, why := parseSetupResult(setupReport("failed", 1, "", log))
	if why != "" {
		t.Fatalf("refused a log of invalid bytes instead of trimming it: %s", why)
	}
	if len(res.Log) > maxSetupLog {
		t.Errorf("log kept %d bytes, want at most %d — a node would refuse the whole report",
			len(res.Log), maxSetupLog)
	}
	if !utf8.ValidString(res.Log) {
		t.Error("the trimmed log is not valid UTF-8: the byte cut landed inside a rune")
	}
}

// TestAFoldedBase64ScriptStillDecodes: `base64` without GNU's -w0 wraps at 76
// columns, and the guest is not guaranteed that flag.
func TestAFoldedBase64ScriptStillDecodes(t *testing.T) {
	script := strings.Repeat("echo hello world\n", 20)
	folded := base64.StdEncoding.EncodeToString([]byte(script))
	var wrapped strings.Builder
	for i := 0; i < len(folded); i += 76 {
		wrapped.WriteString(folded[i:min(i+76, len(folded))])
		wrapped.WriteString(" ")
	}
	res, why := parseSetupResult("ok\n0\n" + wrapped.String() + "\n")
	if why != "" {
		t.Fatalf("refused a folded payload: %s", why)
	}
	if res.Script != script {
		t.Errorf("script = %q, want the folded payload decoded", res.Script)
	}
}

// TestTheSetupRoutesRefuseACallerTheTapDoesNotVouchFor. The identity is the
// connection, so a guest reaching another slot's gateway address — the one
// thing Linux's weak host model would otherwise deliver — is refused on both
// routes, and neither the store nor the control plane is touched.
func TestTheSetupRoutesRefuseACallerTheTapDoesNotVouchFor(t *testing.T) {
	for _, tc := range []struct{ name, method, path string }{
		{"fetch", http.MethodGet, "/self/setup"},
		{"report", http.MethodPost, "/self/setup/result"},
	} {
		for _, who := range []struct{ name, src, dst string }{
			// alice's guest asking bob's gateway address.
			{"cross-slot destination", "172.30.5.2", "172.30.9.1"},
			// Something off the guest range entirely.
			{"not a sandbox", "10.0.0.7", "172.30.5.1"},
			// A slot with no sandbox in it.
			{"empty slot", "172.30.7.2", "172.30.7.1"},
		} {
			t.Run(tc.name+" "+who.name, func(t *testing.T) {
				s, env := envSetupFixture(t)
				body := setupReport("ok", 0, "echo hi", "log")
				rec := requestBody(s, tc.method, tc.path, body, who.src, who.dst)
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
				}
				if got := env.reports(); len(got) != 0 {
					t.Errorf("SetupDone ran %d times for an unauthenticated caller", len(got))
				}
			})
		}
	}
}

// TestAHostWithoutEnvironmentsAnswers501 on both routes, and does it without
// dereferencing the nil.
func TestAHostWithoutEnvironmentsAnswers501(t *testing.T) {
	for _, tc := range []struct{ name, method, path string }{
		{"fetch", http.MethodGet, "/self/setup"},
		{"report", http.MethodPost, "/self/setup/result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t) // no EnvSetup wired at all
			rec := requestBody(s, tc.method, tc.path,
				setupReport("ok", 0, "", ""), "172.30.5.2", "172.30.5.1")
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501 (body %q)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestTheSetupBudgetsAreTheirOwn. The whole point of a fourth pair of windows
// is that neither of them can starve the guest's identity, and that a fetch
// loop cannot spend the report's budget.
func TestTheSetupBudgetsAreTheirOwn(t *testing.T) {
	s, _ := envSetupFixture(t)
	for i := 0; i < setupBurst; i++ {
		if rec := request(s, "/self/setup", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
			t.Fatalf("fetch %d: status = %d, want 200", i, rec.Code)
		}
	}
	if rec := request(s, "/self/setup", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fetch %d: status = %d, want 429", setupBurst, rec.Code)
	}
	// The other sandbox is unaffected: the budget is per sandbox.
	if rec := request(s, "/self/setup", "172.30.9.2", "172.30.9.1"); rec.Code != http.StatusNoContent {
		t.Errorf("bob's fetch = %d, want 204 — the budget is per sandbox", rec.Code)
	}
	// And so is this sandbox's identity, which is the reason these windows are
	// separate from the mint's at all.
	if rec := request(s, "/token", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
		t.Errorf("token = %d, want 200 — a setup loop must not cost a guest its identity", rec.Code)
	}
	// The report's budget was never touched by any of that.
	rec := postReport(s, setupReport("failed", 2, "", "boom"), "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("report = %d, want 202 — the fetch loop spent the report's budget", rec.Code)
	}
}

// TestTheReportBudgetIsSpentSeparately drains the report window and checks the
// fetch still answers.
func TestTheReportBudgetIsSpentSeparately(t *testing.T) {
	s, _ := envSetupFixture(t)
	body := setupReport("failed", 1, "", "")
	for i := 0; i < doneBurst; i++ {
		rec := postReport(s, body, "172.30.5.2", "172.30.5.1")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("report %d: status = %d, want 202", i, rec.Code)
		}
	}
	rec := postReport(s, body, "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("report %d: status = %d, want 429", doneBurst, rec.Code)
	}
	if rec := request(s, "/self/setup", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
		t.Errorf("fetch = %d, want 200 — the report loop spent the fetch's budget", rec.Code)
	}
}

// TestTheGuestHoldsTheAcceptanceBeforeTheCaptureStarts is THE test for the
// report route, and it is the same property selflifecycle.go's tests pin for
// the same reason: recording a result pauses this very VM to capture its disk,
// so nothing may start until the guest has the whole response.
func TestTheGuestHoldsTheAcceptanceBeforeTheCaptureStarts(t *testing.T) {
	s, env := envSetupFixture(t)
	srv := guestServer(t, s)

	body := setupReport("ok", 0, "#!/usr/bin/env bash\npnpm install\n", "installed 412 packages\n")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/self/setup/result", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := oneShotClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if n, cerr := strconv.Atoi(resp.Header.Get("Content-Length")); cerr != nil || n <= 0 {
		t.Fatalf("Content-Length = %q — a flushed response without one goes chunked, and the guest "+
			"then waits for a terminating chunk that only arrives after the wait",
			resp.Header.Get("Content-Length"))
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	readAt := time.Now()
	resp.Body.Close()
	if len(got) == 0 {
		t.Fatal("empty acceptance body")
	}

	entered := env.awaitEntered(t, 5*time.Second)
	if !entered.After(readAt) {
		t.Errorf("the capture half started at %v, before the guest finished reading at %v — "+
			"the acceptance can be lost the moment the VM stops", entered, readAt)
	}
	if gap := entered.Sub(readAt); gap > ackGrace {
		t.Errorf("waited %v after the read — the FIN was not seen and this fell through to the cap", gap)
	}

	reports := env.reports()
	if len(reports) != 1 {
		t.Fatalf("SetupDone ran %d times, want 1", len(reports))
	}
	want := SetupResult{
		OK: true, ExitCode: 0,
		Script: "#!/usr/bin/env bash\npnpm install\n",
		Log:    "installed 412 packages\n",
	}
	if reports[0] != want {
		t.Errorf("recorded %#v, want %#v", reports[0], want)
	}
	// And it was attributed to the sandbox on the tap, which no field of the
	// body could have named.
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.boxes) != 1 || env.boxes[0] != "alice-box" {
		t.Errorf("attributed to %v, want [alice-box]", env.boxes)
	}
}

// TestAFailedRunIsCarriedVerbatim: the report of a build that did not work is
// the only account anybody gets, so nothing about it is normalised away.
func TestAFailedRunIsCarriedVerbatim(t *testing.T) {
	s, env := envSetupFixture(t)
	rec := postReport(s, setupReport("failed", 127, "", "bash: pnpm: command not found\n"),
		"172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	env.awaitEntered(t, 5*time.Second)
	reports := env.reports()
	if len(reports) != 1 {
		t.Fatalf("SetupDone ran %d times, want 1", len(reports))
	}
	want := SetupResult{OK: false, ExitCode: 127, Script: "", Log: "bash: pnpm: command not found\n"}
	if reports[0] != want {
		t.Errorf("recorded %#v, want %#v", reports[0], want)
	}
}

// TestAStoreThatCannotAnswerIsNotAJob: a failing lookup must not be reported to
// the guest as "you have nothing to do", because the oneshot would then exit
// successfully having built nothing.
func TestAStoreThatCannotAnswerIsNotAJob(t *testing.T) {
	s, env := envSetupFixture(t)
	env.err = errNoSetupStore
	rec := request(s, "/self/setup", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %q)", rec.Code, rec.Body.String())
	}
}

// TestAnUnrenderableEnvironmentNameIsNotServed. Line 1 of a line-oriented
// format cannot contain the separator, and the host checks that on its own
// output rather than trusting whatever wrote the row.
func TestAnUnrenderableEnvironmentNameIsNotServed(t *testing.T) {
	for _, name := range []string{"", "web\napp", "web\tapp", strings.Repeat("a", maxEnvNameLen+1)} {
		s, env := envSetupFixture(t)
		env.env = name
		rec := request(s, "/self/setup", "172.30.5.2", "172.30.5.1")
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("environment %q: status = %d, want 500", name, rec.Code)
		}
	}
}

// TestTheReportIsToleratedWhenTheStatusAndExitDisagree. Structure is strict;
// the redundancy between the word and the number is not, because refusing here
// would throw away the only account of a build that already ran.
func TestTheReportIsToleratedWhenTheStatusAndExitDisagree(t *testing.T) {
	s, env := envSetupFixture(t)
	rec := postReport(s, setupReport("failed", 0, "", "killed by a timeout\n"),
		"172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %q)", rec.Code, rec.Body.String())
	}
	env.awaitEntered(t, 5*time.Second)
	reports := env.reports()
	if len(reports) != 1 || reports[0].OK {
		t.Fatalf("recorded %#v, want a single not-OK result", reports)
	}
}

// TestAnInvalidByteInTheLogTailIsRepairedNotRefused: a log is diagnostic
// output, and a stray byte in it must not cost the build its record.
func TestAnInvalidByteInTheLogTailIsRepairedNotRefused(t *testing.T) {
	res, why := parseSetupResult("failed\n1\n\nboom \xff\n")
	if why != "" {
		t.Fatalf("refused: %s", why)
	}
	if res.Log != "boom \uFFFD\n" {
		t.Errorf("log = %q, want the invalid byte replaced", res.Log)
	}
}

// errNoSetupStore stands in for a store that cannot be read.
var errNoSetupStore = errSetup("environment store is unavailable")

type errSetup string

func (e errSetup) Error() string { return string(e) }
