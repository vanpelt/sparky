package metadata

import (
	"encoding/json"
	"net/http"
)

// OpenAI workload identity federation.
//
// OpenAI verifies an OIDC assertion from a trusted issuer and exchanges it for
// a short-lived OpenAI access token (RFC 8693 token exchange at
// auth.openai.com/oauth/token, one hour maximum, no refresh token). Nothing
// long-lived is stored in the guest, which is the same bargain sparkbox
// already struck with HiveMind — so the whole integration on our side is one
// more audience out of the issuer we already run, plus telling the guest which
// federation rule to present the assertion against.
//
// The three identifiers below are NOT secrets. They name an OpenAI Workload
// Identity Provider, a federation rule ("mapping"), and a service account;
// possession of them grants nothing without an assertion this fleet's key
// signed. They are fleet configuration, so they arrive as flags on the host
// and are served here rather than being baked into a rootfs template — a
// template patched with the wrong provider id would have to be rebuilt, and a
// running sandbox picks a corrected one up on its next 45-minute refresh.

const (
	// DefaultOpenAIAudience is the `aud` sparkbox mints OpenAI assertions for.
	// It matches the audience OpenAI's own Kubernetes guide tells operators to
	// project, so an admin configuring the provider sees a value they have
	// seen before. It is only ever an opaque string to both ends — it is not
	// fetched — but it must match the provider's configured audience exactly.
	DefaultOpenAIAudience = "https://api.openai.com/v1"

	// DefaultOpenAITokenFile is where the guest keeps the assertion. OpenAI
	// documents this exact directory ("use an absolute path in a dedicated
	// directory, such as /var/run/secrets/openai.com"), and Codex reads the
	// path from OPENAI_IDENTITY_TOKEN_FILE.
	DefaultOpenAITokenFile = "/var/run/secrets/openai.com/identity-token"
)

// OpenAI is the fleet's OpenAI federation configuration, served to guests at
// GET /openai. A zero value is a fleet that has not configured federation, and
// answers 501 — which is what turns the guest side off, since the guest asks
// this endpoint before it fetches anything.
type OpenAI struct {
	// Audience is the `aud` claim of the assertion the guest should request.
	// The issuer's own allowlist is what decides whether it may be minted; this
	// only says which one to ask for.
	Audience string `json:"audience"`
	// IdentityProvider is the OpenAI Workload Identity Provider id (idp_...),
	// and ServiceAccount the service account the rule maps to (svc_...). The
	// OpenAI SDKs take both when they perform the exchange themselves.
	IdentityProvider string `json:"identity_provider_id,omitempty"`
	ServiceAccount   string `json:"service_account_id,omitempty"`
	// FederationRule is the federation rule / mapping id (idpm_...). Codex
	// wants this one and only this one, as OPENAI_FEDERATION_RULE_ID.
	FederationRule string `json:"federation_rule_id,omitempty"`
	// TokenFile is the absolute path the guest must write the assertion to and
	// advertise as OPENAI_IDENTITY_TOKEN_FILE.
	TokenFile string `json:"token_file"`
}

// Configured reports whether this fleet federates with OpenAI at all.
//
// An audience alone is deliberately not enough. A guest that exported
// OPENAI_IDENTITY_TOKEN_FILE with no rule to present it against would leave
// Codex worse off than untouched: it would try workload identity, fail, and
// never fall back to the ordinary login the user could have completed. So the
// switch is an identifier that could only have come from an OpenAI admin.
func (o OpenAI) Configured() bool {
	return o.FederationRule != "" || o.IdentityProvider != ""
}

// withDefaults fills the two fields an operator should not have to think
// about. Called once at construction so every guest reads the same answer.
func (o OpenAI) withDefaults() OpenAI {
	if o.Audience == "" {
		o.Audience = DefaultOpenAIAudience
	}
	if o.TokenFile == "" {
		o.TokenFile = DefaultOpenAITokenFile
	}
	return o
}

// AudienceOrDefault is the `aud` this configuration mints for, with the
// default filled in when the operator left the flag empty. The host uses it to
// keep the issuer's allowlist in step with what guests will ask for.
func (o OpenAI) AudienceOrDefault() string { return o.withDefaults().Audience }

// openai serves the federation configuration to the calling sandbox.
//
// caller() runs first for the reason every other endpoint runs it: not because
// these identifiers are secret — they are not — but because answering anything
// to a source this service cannot name as a sandbox is how the tap-position
// authentication gets eroded one convenience at a time.
func (s *Server) openai(w http.ResponseWriter, r *http.Request) {
	if _, err := s.caller(r); err != nil {
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	if !s.openAI.Configured() {
		http.Error(w, "sparkbox: this fleet does not federate with OpenAI", http.StatusNotImplemented)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(s.openAI) //nolint:errcheck
}
