package launch

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/repos"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

func readyEnvironment(name string) map[string]ctlops.EnvironmentInfo {
	return map[string]ctlops.EnvironmentInfo{name: {Name: name, State: "ready", Repos: []string{testSlug}}}
}

func TestParseTargetNormalizesEnvironmentName(t *testing.T) {
	got, err := parseTarget("wandb", "hivemind", "", "  HiveMind  ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Env != "hivemind" {
		t.Fatalf("Env = %q, want hivemind", got.Env)
	}
}

func TestEnvironmentLaunchAlwaysConfirmsAndNamesTheEnvironment(t *testing.T) {
	store := attached(testHandle, attachment("main", "hivemind"), map[string][]repos.Repo{
		"existing": {{Host: gitHubHost, Slug: testSlug, Ref: "feat/x"}},
	})
	ops := &fakeOps{t: t, environments: readyEnvironment("hivemind"), boxes: []ctlops.SandboxInfo{
		func() ctlops.SandboxInfo {
			b := box("existing", string(vmm.StateRunning), false, time.Now())
			b.Tags = []string{"hivemind"}
			return b
		}(),
	}}
	h := newHandler(t, ops, store)

	rec := serveLaunch(t, h, http.MethodGet,
		"https://go.example.test/wandb/hivemind?ref=feat/x&env=Hivemind",
		asBrowser, signedIn(t, testHandle))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want confirmation page", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"Environment", "hivemind", "Open existing", `action="/wandb/hivemind?env=hivemind&amp;ref=feat%2Fx"`} {
		if !strings.Contains(body, want) {
			t.Errorf("confirmation page does not contain %q", want)
		}
	}
	if got := rec.Header().Get("Location"); got != "" {
		t.Fatalf("Location = %q; an environment selection must never silently redirect", got)
	}
}

func TestEnvironmentLaunchRequiresReadyEnvironmentAttachedToRepo(t *testing.T) {
	for _, tc := range []struct {
		name string
		tags []string
		env  ctlops.EnvironmentInfo
		code int
	}{
		{"repo is not in environment", []string{"other"}, ctlops.EnvironmentInfo{Name: "hivemind", State: "ready"}, http.StatusBadRequest},
		{"environment is not ready", []string{"hivemind"}, ctlops.EnvironmentInfo{Name: "hivemind", State: "failed"}, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := &fakeOps{t: t, environments: map[string]ctlops.EnvironmentInfo{"hivemind": tc.env}}
			h := newHandler(t, ops, &fakeRepos{attachments: map[string][]repos.Repo{
				testHandle: {attachment("main", tc.tags...)},
			}})
			rec := serveLaunch(t, h, http.MethodGet,
				"https://go.example.test/wandb/hivemind?env=hivemind",
				asBrowser, signedIn(t, testHandle))
			if rec.Code != tc.code {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body, tc.code)
			}
		})
	}
}

func TestEnvironmentLaunchPostCreatesOnlySelectedEnvironment(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true, environments: readyEnvironment("hivemind")}
	h := newHandler(t, ops, &fakeRepos{attachments: map[string][]repos.Repo{
		testHandle: {attachment("main", "hivemind", "production")},
	}})

	rec := serveLaunch(t, h, http.MethodPost,
		"https://go.example.test/wandb/hivemind?ref=feat/x&env=hivemind",
		signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303", rec.Code, rec.Body)
	}
	if len(ops.created) != 1 {
		t.Fatalf("created args = %+v, want one create", ops.created)
	}
	got := ops.created[0]
	if got.Env != "hivemind" || len(got.Tags) != 0 {
		t.Fatalf("create = %+v, want Env hivemind and no attachment tag union", got)
	}
	if len(got.Refs) != 1 || got.Refs[0].Slug != testSlug || got.Refs[0].Ref != "feat/x" {
		t.Fatalf("refs = %+v, want scoped feat/x override", got.Refs)
	}
}

func TestEnvironmentLaunchDoesNotReuseAnotherEnvironment(t *testing.T) {
	store := attached(testHandle, attachment("main", "hivemind", "production"), map[string][]repos.Repo{
		"prod-box": {{Host: gitHubHost, Slug: testSlug, Ref: "main"}},
	})
	prod := box("prod-box", string(vmm.StateRunning), false, time.Now())
	prod.Tags = []string{"production"}
	ops := &fakeOps{t: t, allowCreate: true, environments: readyEnvironment("hivemind"), boxes: []ctlops.SandboxInfo{prod}}
	h := newHandler(t, ops, store)

	rec := serveLaunch(t, h, http.MethodPost,
		"https://go.example.test/wandb/hivemind?env=hivemind",
		signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d (%s), want 303", rec.Code, rec.Body)
	}
	if ops.createCount() != 1 {
		t.Fatalf("created %d sandboxes, want one; prod-box must not satisfy the hivemind override", ops.createCount())
	}
	if got := rec.Header().Get("Location"); strings.Contains(got, "prod-box") {
		t.Fatalf("Location = %q, reused a sandbox from another environment", got)
	}
}

func TestEnvironmentLaunchPostRevalidatesAuthorization(t *testing.T) {
	ops := &fakeOps{t: t, allowCreate: true}
	h := newHandler(t, ops, &fakeRepos{attachments: map[string][]repos.Repo{
		testHandle: {attachment("main", "hivemind")},
	}})
	rec := serveLaunch(t, h, http.MethodPost,
		"https://go.example.test/wandb/hivemind?env=hivemind",
		signedIn(t, testHandle), fromThePage)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d (%s), want masked 404", rec.Code, rec.Body)
	}
	if ops.createCount() != 0 {
		t.Fatal("POST created after the environment disappeared")
	}
}
