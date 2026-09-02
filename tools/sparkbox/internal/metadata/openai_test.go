package metadata

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
)

// openAIFixture is fixture() minus the parts an OpenAI config test has no
// opinion about: this endpoint neither mints nor reads an account, so a server
// that can name its caller is the whole dependency.
func openAIFixture(cfg OpenAI) *Server {
	return New(Options{
		Manager: fakeBoxes{
			"172.30.5.2": {Name: "alice-box", Owner: "alice", Image: "universal", HostIP: "172.30.5.2"},
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OpenAI: cfg,
	})
}

func TestOpenAIConfigIsServedToTheCallingSandbox(t *testing.T) {
	s := openAIFixture(OpenAI{
		IdentityProvider: "idp_abc123",
		ServiceAccount:   "svc_def456",
		FederationRule:   "idpm_ghi789",
	})
	rec := request(s, "/openai", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got OpenAI
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	// The two an operator should never have to type are filled in here, once,
	// so every guest on the fleet reads the same answer — a guest that had to
	// default them itself is a guest whose defaults can drift from the
	// audience the issuer actually allowlisted.
	if got.Audience != DefaultOpenAIAudience {
		t.Errorf("audience = %q, want the default %q", got.Audience, DefaultOpenAIAudience)
	}
	if got.TokenFile != DefaultOpenAITokenFile {
		t.Errorf("token file = %q, want the default %q", got.TokenFile, DefaultOpenAITokenFile)
	}
	if got.FederationRule != "idpm_ghi789" || got.IdentityProvider != "idp_abc123" ||
		got.ServiceAccount != "svc_def456" {
		t.Errorf("identifiers did not survive the round trip: %+v", got)
	}
}

func TestOpenAIConfigHonoursExplicitOverrides(t *testing.T) {
	s := openAIFixture(OpenAI{
		Audience:       "api://sparkbox-codex",
		FederationRule: "idpm_ghi789",
		TokenFile:      "/var/run/secrets/openai/assertion",
	})
	rec := request(s, "/openai", "172.30.5.2", "172.30.5.1")
	var got OpenAI
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Audience != "api://sparkbox-codex" {
		t.Errorf("audience = %q, want the operator's own", got.Audience)
	}
	if got.TokenFile != "/var/run/secrets/openai/assertion" {
		t.Errorf("token file = %q, want the operator's own", got.TokenFile)
	}
}

// 501 is the answer that turns the guest side off, and it is the answer on
// every fleet that has not been through an OpenAI admin — which is all of them
// until someone passes the flags.
func TestOpenAIConfigIsAbsentUntilConfigured(t *testing.T) {
	s := openAIFixture(OpenAI{})
	rec := request(s, "/openai", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body.String())
	}
}

// An audience alone is not configuration. A guest that acted on it would export
// OPENAI_IDENTITY_TOKEN_FILE with no rule to present the assertion against, and
// Codex would federate and fail instead of offering the login the user could
// have completed.
func TestOpenAIAudienceAloneDoesNotEnableFederation(t *testing.T) {
	s := openAIFixture(OpenAI{Audience: DefaultOpenAIAudience})
	rec := request(s, "/openai", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 for an audience with no rule or provider: %s",
			rec.Code, rec.Body.String())
	}
}

// Not because these identifiers are secret — they are not — but because the
// tap-position authentication is only worth anything if every endpoint applies
// it. See caller().
func TestOpenAIConfigRefusesANonSandboxCaller(t *testing.T) {
	s := openAIFixture(OpenAI{FederationRule: "idpm_ghi789"})
	rec := request(s, "/openai", "10.0.0.5", "172.30.5.1")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
}
