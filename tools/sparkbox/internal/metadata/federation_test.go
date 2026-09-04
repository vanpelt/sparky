package metadata

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/federation"
)

// federationFixture is fixture() minus the parts this endpoint has no opinion
// about: it neither mints nor reads an account, so a server that can name its
// caller is the whole dependency.
func federationFixture(cfg federation.Config) *Server {
	return New(Options{
		Manager: fakeBoxes{
			"172.30.5.2": {Name: "alice-box", Owner: "alice", Image: "universal", HostIP: "172.30.5.2"},
		},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Federation: cfg,
	})
}

func TestFederationListIsServedToTheCallingSandbox(t *testing.T) {
	cfg := federation.Config{Federators: []federation.Federator{
		federation.HiveMind("https://hivemind.wandb.tools"),
		federation.OpenAI("idp_abc123", "idpm_ghi789", "svc_def456"),
	}}
	s := federationFixture(cfg)
	rec := request(s, "/federation", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// What is served is exactly the one encoding the guest reads, and it is
	// the Config with its defaults filled in — a guest that had to default the
	// token path itself is a guest whose defaults can drift from the host's.
	if got, want := rec.Body.String(), cfg.WithDefaults().Guest(); got != want {
		t.Errorf("served:\n%s\nwant:\n%s", got, want)
	}
	back, err := federation.ParseGuest(strings.NewReader(rec.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Names(); len(got) != 2 || got[0] != "hivemind" || got[1] != "openai" {
		t.Errorf("federators = %v", got)
	}
	if hm, _ := back.Get("hivemind"); hm.TokenFile != "/var/run/secrets/hivemind/token" {
		t.Errorf("hivemind token file = %q, want the default filled in", hm.TokenFile)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q; a cached list is a corrected provider id nobody sees", cc)
	}
}

// An operator may list nothing, and then the guest mints nothing. That is
// served as an empty body rather than an error, so the guest can tell "this
// fleet federates with nobody" from "the host is having a bad minute" and
// erase its stale copy in the first case only.
func TestAnEmptyFederationListIsAnEmptyBody(t *testing.T) {
	s := federationFixture(federation.Config{})
	rec := request(s, "/federation", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("status = %d body = %q, want 200 and nothing", rec.Code, rec.Body.String())
	}
}

// Not because the list is secret — it is not — but because the tap-position
// authentication is only worth anything if every endpoint applies it. See
// caller().
func TestFederationListRefusesANonSandboxCaller(t *testing.T) {
	s := federationFixture(federation.Default("https://hivemind.wandb.tools"))
	rec := request(s, "/federation", "10.0.0.5", "172.30.5.1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}
