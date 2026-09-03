package userconsole

// The Environments panel's server half. What is worth testing here is the seam
// rather than the composition: ctlops owns the five-store ordering rule and has
// its own tests for it, and what this file must prove is that the console hands
// that control plane the right caller, the right shape, and answers the browser
// with what came back.

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
)

// fakeEnvOps is the control plane as this panel sees it: it records the caller
// of every call, because owner scoping in this package is not a WHERE clause
// here — it is the Caller, and a handler that built one from the wrong place
// would be a cross-owner read that no store could catch.
type fakeEnvOps struct {
	mu      sync.Mutex
	callers []string
	calls   []string
	rows    map[string]ctlops.EnvironmentInfo
	scripts map[string]string
	origins map[string]string
	// lastArgs is what PutEnvironment was handed most recently, and putErr is
	// what it fails with. Both exist for the adoption seam: the panel's whole
	// confirm-and-retry rests on a flag arriving and a code coming back.
	lastArgs ctlops.EnvArgs
	putErr   error
}

func newFakeEnvOps() *fakeEnvOps {
	return &fakeEnvOps{
		rows:    map[string]ctlops.EnvironmentInfo{},
		scripts: map[string]string{},
		origins: map[string]string{},
	}
}

func (f *fakeEnvOps) note(c ctlops.Caller, call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callers = append(f.callers, c.Handle)
	f.calls = append(f.calls, call)
}

func (f *fakeEnvOps) ListEnvironments(c ctlops.Caller) ([]ctlops.EnvironmentInfo, error) {
	f.note(c, "list")
	var out []ctlops.EnvironmentInfo
	for _, e := range f.rows {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeEnvOps) GetEnvironment(c ctlops.Caller, name string) (ctlops.EnvironmentInfo, error) {
	f.note(c, "get "+name)
	e, ok := f.rows[name]
	if !ok {
		return ctlops.EnvironmentInfo{}, ctlops.NotFound("env.show", "environment", name)
	}
	return e, nil
}

func (f *fakeEnvOps) PutEnvironment(_ context.Context, c ctlops.Caller, a ctlops.EnvArgs) (ctlops.EnvironmentInfo, error) {
	f.note(c, "put "+a.Name)
	f.lastArgs = a
	if f.putErr != nil {
		return ctlops.EnvironmentInfo{}, f.putErr
	}
	e := f.rows[a.Name]
	e.Name = a.Name
	if a.Description != nil {
		e.Description = *a.Description
	}
	for _, r := range a.Repos {
		e.Repos = append(e.Repos, r.Slug)
	}
	e.Secrets = append(e.Secrets, a.Secrets...)
	e.Rules = append(e.Rules, a.Rules...)
	for _, v := range a.Vars {
		set := false
		for i := range e.Vars {
			if e.Vars[i].Name == v.Name {
				e.Vars[i] = v
				set = true
			}
		}
		if !set {
			e.Vars = append(e.Vars, v)
		}
	}
	if e.State == "" {
		e.State = string(envs.StateDraft)
	}
	f.rows[a.Name] = e
	return e, nil
}

func (f *fakeEnvOps) DeleteEnvironment(_ context.Context, c ctlops.Caller, name string) (ctlops.EnvDeleteResult, error) {
	f.note(c, "rm "+name)
	e, ok := f.rows[name]
	if !ok {
		return ctlops.EnvDeleteResult{}, ctlops.NotFound("env.rm", "environment", name)
	}
	delete(f.rows, name)
	return ctlops.EnvDeleteResult{
		Name: name, Repos: e.Repos, Secrets: e.Secrets, Rules: []string{},
		RemovedRule: name, Resynced: []string{},
	}, nil
}

func (f *fakeEnvOps) UnsetEnvVar(_ context.Context, c ctlops.Caller, env, name string) error {
	f.note(c, "unset "+env+"."+name)
	e := f.rows[env]
	kept := e.Vars[:0]
	for _, v := range e.Vars {
		if v.Name != name {
			kept = append(kept, v)
		}
	}
	e.Vars = kept
	f.rows[env] = e
	return nil
}

func (f *fakeEnvOps) EnvScript(c ctlops.Caller, name string) (string, string, error) {
	f.note(c, "script "+name)
	if _, ok := f.rows[name]; !ok {
		return "", "", ctlops.NotFound("env.script", "environment", name)
	}
	return f.scripts[name], f.origins[name], nil
}

func (f *fakeEnvOps) SetEnvScript(c ctlops.Caller, name, script, from string) error {
	f.note(c, "script.set "+name+" from="+from)
	f.scripts[name], f.origins[name] = script, from
	e := f.rows[name]
	e.HasSetup, e.SetupBytes, e.SetupFrom = script != "", len(script), from
	f.rows[name] = e
	return nil
}

// AdoptRepoScript is the "Use <repo>'s" button. The fake records the call and
// clears the drift verdict, because that is what the real one does — the row
// and the repository now hold the same bytes.
func (f *fakeEnvOps) AdoptRepoScript(_ context.Context, c ctlops.Caller, name string) (ctlops.EnvironmentInfo, error) {
	f.note(c, "script.from_repo "+name)
	e, ok := f.rows[name]
	if !ok {
		return ctlops.EnvironmentInfo{}, envs.ErrNoSuchEnvironment
	}
	e.ScriptDrift = ctlops.ScriptDriftMatch
	e.HasSetup = true
	f.rows[name] = e
	return e, nil
}

func (f *fakeEnvOps) BuildEnvironment(_ context.Context, c ctlops.Caller, name string) (ctlops.EnvironmentInfo, error) {
	f.note(c, "build "+name)
	e := f.rows[name]
	e.State, e.BuildBox = string(envs.StateBuilding), name+"-build"
	f.rows[name] = e
	return e, nil
}

func (f *fakeEnvOps) CaptureEnvironment(_ context.Context, c ctlops.Caller, name string) (ctlops.EnvironmentInfo, error) {
	f.note(c, "capture "+name)
	e := f.rows[name]
	e.State, e.Snapshot = string(envs.StateReady), name+"-260902"
	f.rows[name] = e
	return e, nil
}

func (f *fakeEnvOps) saw(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func withEnvOps(t *testing.T, tc *testConsole) *fakeEnvOps {
	t.Helper()
	f := newFakeEnvOps()
	tc.handler.SetEnvironments(f)
	return f
}

// Without the seam every route answers 501 and the tab says so, exactly as the
// Network and Repos tabs do on a host without their stores. A nil interface
// answering 500 — or panicking — would take the whole page down with it.
func TestEnvironmentRoutesAreDisabledWithoutTheControlPlane(t *testing.T) {
	tc := newTestConsole(t)
	for _, ep := range []struct{ method, path string }{
		{"GET", "/api/environments"},
		{"PUT", "/api/environments/web"},
		{"DELETE", "/api/environments/web"},
		{"GET", "/api/environments/web/script"},
		{"PUT", "/api/environments/web/script"},
		{"POST", "/api/environments/web/build"},
		{"POST", "/api/environments/web/capture"},
	} {
		rec := tc.do(t, ep.method, ep.path, "alice", map[string]any{})
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s = %d, want 501", ep.method, ep.path, rec.Code)
		}
	}
}

// The listing serialises as [] and never null: the SPA's Array.isArray guard
// renders null as an empty table with nothing anywhere to explain it.
func TestEmptyEnvironmentListIsAnArray(t *testing.T) {
	tc := newTestConsole(t)
	withEnvOps(t, tc)
	rec := tc.do(t, "GET", "/api/environments", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body = %q, want []", got)
	}
}

// The caller ctlops sees is the session's handle, on every route. This is the
// whole of owner scoping in this panel: there is no WHERE clause here to get
// wrong, only a Caller to build from the wrong place.
func TestEveryEnvironmentRouteCarriesTheSessionHandle(t *testing.T) {
	tc := newTestConsole(t)
	f := withEnvOps(t, tc)
	tc.do(t, "PUT", "/api/environments/web", "alice", map[string]any{})
	tc.do(t, "GET", "/api/environments", "mallory", nil)
	tc.do(t, "POST", "/api/environments/web/build", "alice", nil)
	f.mu.Lock()
	defer f.mu.Unlock()
	want := []string{"alice", "mallory", "alice"}
	if len(f.callers) != len(want) {
		t.Fatalf("callers = %v, want %v", f.callers, want)
	}
	for i := range want {
		if f.callers[i] != want[i] {
			t.Errorf("call %d (%s) came from %q, want %q", i, f.calls[i], f.callers[i], want[i])
		}
	}
}

// The panel's PUT is a form, so the variable list it sends is the WHOLE set and
// a line somebody deleted has to be unset. Everything else ADDS, because a
// secret can belong to three environments at once and a console that detached
// the ones this form did not list would be editing other environments from
// here.
func TestSavingAnEnvironmentReplacesVarsAndAddsEverythingElse(t *testing.T) {
	tc := newTestConsole(t)
	f := withEnvOps(t, tc)
	f.rows["web"] = ctlops.EnvironmentInfo{
		Name: "web", Description: "the web box",
		Secrets: []string{"GITHUB_TOKEN"},
		Vars: []ctlops.EnvVar{
			{Name: "NODE_ENV", Value: "development"},
			{Name: "GOING_AWAY", Value: "1"},
		},
	}
	rec := tc.do(t, "PUT", "/api/environments/web", "alice", map[string]any{
		"description": "the web box",
		"secrets":     []string{"DATABASE_URL"},
		"vars":        []map[string]string{{"name": "NODE_ENV", "value": "test"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !f.saw("unset web.GOING_AWAY") {
		t.Errorf("a variable the form dropped was not unset: %v", f.calls)
	}
	var got ctlops.EnvironmentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// The answer is the state AFTER the unsets, not the state between the two
	// halves of the write.
	if len(got.Vars) != 1 || got.Vars[0].Value != "test" {
		t.Errorf("vars = %+v, want only NODE_ENV=test", got.Vars)
	}
	if len(got.Secrets) != 2 {
		t.Errorf("secrets = %v, want the new one added to the old", got.Secrets)
	}
}

// An omitted description is "leave it alone" and an empty one is "clear it",
// and the JSON pointer is what carries the difference. A blank create form must
// not be able to wipe the description of an environment somebody named a second
// time to add a secret to it.
func TestAnOmittedDescriptionIsNotAnEmptyOne(t *testing.T) {
	tc := newTestConsole(t)
	f := withEnvOps(t, tc)
	f.rows["web"] = ctlops.EnvironmentInfo{Name: "web", Description: "the web box"}

	tc.do(t, "PUT", "/api/environments/web", "alice", map[string]any{"secrets": []string{"X"}})
	if f.rows["web"].Description != "the web box" {
		t.Errorf("description = %q, want it untouched by a save that named none", f.rows["web"].Description)
	}
	tc.do(t, "PUT", "/api/environments/web", "alice", map[string]any{"description": ""})
	if f.rows["web"].Description != "" {
		t.Errorf("description = %q, want an explicit empty one to clear it", f.rows["web"].Description)
	}
}

// A script typed into the editor came from a person, whatever the row said
// before. Recording it as `repo` or `agent` would tell the next build to look
// for a source that did not write it.
func TestAPastedScriptIsRecordedAsManual(t *testing.T) {
	tc := newTestConsole(t)
	f := withEnvOps(t, tc)
	f.rows["web"] = ctlops.EnvironmentInfo{Name: "web"}
	f.origins["web"] = envs.SetupFromAgent

	rec := tc.do(t, "PUT", "/api/environments/web/script", "alice",
		map[string]string{"script": "#!/usr/bin/env bash\npnpm install\n"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if f.origins["web"] != envs.SetupFromManual {
		t.Errorf("origin = %q, want %q", f.origins["web"], envs.SetupFromManual)
	}
	rec = tc.do(t, "GET", "/api/environments/web/script", "alice", nil)
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got["script"], "pnpm install") || got["from"] != envs.SetupFromManual {
		t.Errorf("read back %+v", got)
	}
}

// `rm` on a grouping must never destroy the things grouped, and the panel says
// so out loud — so what survived has to reach the browser.
func TestDeletingAnEnvironmentReportsWhatSurvived(t *testing.T) {
	tc := newTestConsole(t)
	f := withEnvOps(t, tc)
	f.rows["web"] = ctlops.EnvironmentInfo{
		Name: "web", Repos: []string{"wandb/hivemind"}, Secrets: []string{"GITHUB_TOKEN"},
	}
	rec := tc.do(t, "DELETE", "/api/environments/web", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var res ctlops.EnvDeleteResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Secrets) != 1 || res.Secrets[0] != "GITHUB_TOKEN" {
		t.Errorf("secrets = %v, want the surviving one named", res.Secrets)
	}
	if res.RemovedRule != "web" {
		t.Errorf("removed_rule = %q, want the auto-created rule-set named", res.RemovedRule)
	}
}

// A typo in a field name must not be a request that silently does nothing.
func TestAnUnknownFieldIsRefused(t *testing.T) {
	tc := newTestConsole(t)
	withEnvOps(t, tc)
	rec := tc.do(t, "PUT", "/api/environments/web", "alice", map[string]any{"descripton": "typo"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown field: %s", rec.Code, rec.Body)
	}
}

func TestBuildAndCaptureReachTheControlPlane(t *testing.T) {
	tc := newTestConsole(t)
	f := withEnvOps(t, tc)
	f.rows["web"] = ctlops.EnvironmentInfo{Name: "web"}
	if rec := tc.do(t, "POST", "/api/environments/web/build", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("build = %d: %s", rec.Code, rec.Body)
	}
	if f.rows["web"].State != string(envs.StateBuilding) {
		t.Errorf("state = %q, want building", f.rows["web"].State)
	}
	if rec := tc.do(t, "POST", "/api/environments/web/capture", "alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("capture = %d: %s", rec.Code, rec.Body)
	}
	if f.rows["web"].Snapshot == "" {
		t.Error("capture bound no base image")
	}
}

// TestTheAdoptFlagReachesTheControlPlane is the seam this package is
// responsible for: the panel's confirm-and-retry is worthless if the retry's
// `adopt` never arrives.
func TestTheAdoptFlagReachesTheControlPlane(t *testing.T) {
	tc := newTestConsole(t)
	f := withEnvOps(t, tc)

	tc.do(t, "PUT", "/api/environments/web", "alice", map[string]any{"secrets": []string{"X"}})
	if f.lastArgs.Adopt {
		t.Error("a save that did not ask to adopt arrived with Adopt set")
	}
	tc.do(t, "PUT", "/api/environments/web", "alice", map[string]any{
		"secrets": []string{"X"}, "adopt": true,
	})
	if !f.lastArgs.Adopt {
		t.Error("adopt:true did not reach the control plane, so the retry would be refused again")
	}
}

// TestAConflictCarriesItsCode pins the one thing the console's error body could
// not previously say.
//
// The body has always been {"error": "<sentence>"}, and the page cannot tell an
// adoption conflict from any other 409 by reading prose — so it now carries the
// stable machine token beside the sentence. Matching on the wording would be
// exactly the coupling Code exists to prevent, and it would break the first time
// somebody improved the sentence.
func TestAConflictCarriesItsCode(t *testing.T) {
	tc := newTestConsole(t)
	f := withEnvOps(t, tc)
	f.putErr = &ctlops.Error{
		Kind: ctlops.KindConflict, Op: "env.set", Code: "env_tag_in_use",
		Msg: "the tag \"web\" is already carrying 2 repositories.", Verbatim: true,
	}

	rec := tc.do(t, "PUT", "/api/environments/web", "alice", map[string]any{"secrets": []string{"X"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "env_tag_in_use" {
		t.Errorf("code = %q, want env_tag_in_use", body.Code)
	}
	// The sentence still rides in `error`, where every other caller reads it.
	if !strings.Contains(body.Error, "already carrying") {
		t.Errorf("error = %q, want the control plane's own sentence", body.Error)
	}
}
