package federation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The list is what a guest walks, and every property here is one that fails
// quietly if it regresses: a federator that did not survive the round trip is
// a client asking a person to log in, not an error anywhere.

func TestDefaultIsHiveMindAlone(t *testing.T) {
	cfg, err := Load("", Default("https://hivemind.wandb.tools"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Names(); !slices.Equal(got, []string{"hivemind"}) {
		t.Fatalf("federators = %v, want hivemind alone", got)
	}
	hm, _ := cfg.Get("hivemind")
	// The path `hivemind start` reads with no configuration at all. Moving it
	// is a change to every sandbox on every fleet that has not written a file.
	if hm.TokenFile != "/var/run/secrets/hivemind/token" {
		t.Errorf("hivemind token file = %q, want the path the daemon already reads", hm.TokenFile)
	}
	if hm.Audience != "https://hivemind.wandb.tools" {
		t.Errorf("hivemind audience = %q", hm.Audience)
	}
}

func TestLoadReadsAnOperatorsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "federation.json")
	if err := os.WriteFile(path, []byte(`{
  "federators": [
    {"name": "hivemind", "audience": "https://hivemind.wandb.tools"},
    {
      "name": "openai",
      "audience": "https://api.openai.com/v1",
      "token_file": "/var/run/secrets/openai.com/identity-token",
      "token_file_env": "OPENAI_IDENTITY_TOKEN_FILE",
      "context_env": "OPENAI_WORKLOAD_IDENTITY_CONTEXT",
      "env": {"OPENAI_FEDERATION_RULE_ID": "idpm_ghi789", "OPENAI_IDENTITY_PROVIDER_ID": "idp_abc123"}
    }
  ]
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, Default("unused"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Names(); !slices.Equal(got, []string{"hivemind", "openai"}) {
		t.Fatalf("federators = %v", got)
	}
	// The file replaces the default rather than adding to it: an operator who
	// wrote the list wrote the whole list.
	if got := cfg.Audiences(); !slices.Equal(got, []string{"https://hivemind.wandb.tools", "https://api.openai.com/v1"}) {
		t.Errorf("audiences = %v", got)
	}
	oa, _ := cfg.Get("openai")
	if oa.Env["OPENAI_FEDERATION_RULE_ID"] != "idpm_ghi789" || oa.TokenFileEnv != "OPENAI_IDENTITY_TOKEN_FILE" {
		t.Errorf("openai did not survive the file: %+v", oa)
	}
}

// A misspelt key is a federation that half-works, discovered in a sandbox, so
// the file is refused rather than read around.
func TestLoadRefusesWhatItDoesNotUnderstand(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":     `{"federators": [{"name": "x", "audience": "a", "token_fil": "/x"}]}`,
		"duplicate name":    `{"federators": [{"name": "x", "audience": "a"}, {"name": "x", "audience": "b"}]}`,
		"shared token file": `{"federators": [{"name": "x", "audience": "a", "token_file": "/t"}, {"name": "y", "audience": "b", "token_file": "/t"}]}`,
		"no audience":       `{"federators": [{"name": "x"}]}`,
		"uppercase name":    `{"federators": [{"name": "OpenAI", "audience": "a"}]}`,
		"relative path":     `{"federators": [{"name": "x", "audience": "a", "token_file": "secrets/t"}]}`,
		"directory path":    `{"federators": [{"name": "x", "audience": "a", "token_file": "/run/secrets/"}]}`,
		"lowercase env":     `{"federators": [{"name": "x", "audience": "a", "env": {"rule": "r"}}]}`,
		"bad env name":      `{"federators": [{"name": "x", "audience": "a", "token_file_env": "a-b"}]}`,
		// The guest exports these bare into /etc/environment and greps a
		// tab-separated copy. A quote, a space, a `$` or a tab in a value is a
		// value that reads differently to pam_env, a shell and cut.
		"quoted value":    `{"federators": [{"name": "x", "audience": "a", "env": {"K": "\"v\""}}]}`,
		"spaced value":    `{"federators": [{"name": "x", "audience": "a", "env": {"K": "a b"}}]}`,
		"expanding value": `{"federators": [{"name": "x", "audience": "a", "env": {"K": "$HOME"}}]}`,
		"tabbed audience": `{"federators": [{"name": "x", "audience": "a\tb"}]}`,
		"not json":        `federators: []`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(body)); err == nil {
				t.Errorf("accepted %s", body)
			}
		})
	}
}

// Every value a relying party actually issues fits the character set.
func TestValidateAcceptsRealIdentifiers(t *testing.T) {
	cfg := Config{Federators: []Federator{
		HiveMind("https://hivemind.wandb.tools"),
		OpenAI("idp_abc123", "idpm_ghi789", "svc_def456"),
		{Name: "vault.example", Audience: "api://vault-prod", Env: map[string]string{
			"VAULT_ROLE": "sparkbox/user,ns=eng", "VAULT_ADDR": "https://vault.example.com:8200/v1/jwt%2Flogin",
		}},
	}}.WithDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIExportsWhatCodexAndTheSDKsRead(t *testing.T) {
	f := OpenAI("idp_abc123", "idpm_ghi789", "svc_def456")
	if f.Audience != "https://api.openai.com/v1" {
		t.Errorf("audience = %q, want the one OpenAI's own guide projects", f.Audience)
	}
	if f.TokenFile != "/var/run/secrets/openai.com/identity-token" || f.TokenFileEnv != "OPENAI_IDENTITY_TOKEN_FILE" {
		t.Errorf("token file %q via %q, want the path OpenAI documents and the variable Codex reads", f.TokenFile, f.TokenFileEnv)
	}
	want := map[string]string{
		"OPENAI_FEDERATION_RULE_ID":   "idpm_ghi789",
		"OPENAI_IDENTITY_PROVIDER_ID": "idp_abc123",
		"OPENAI_SERVICE_ACCOUNT_ID":   "svc_def456",
	}
	for k, v := range want {
		if f.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, f.Env[k], v)
		}
	}
	// An id the admin did not hand back is not exported as an empty string,
	// which the SDKs would read as "set, and wrong".
	if _, set := OpenAI("", "idpm_ghi789", "").Env["OPENAI_IDENTITY_PROVIDER_ID"]; set {
		t.Error("an empty provider id was exported")
	}
}

// The guest reads this encoding with grep and cut and nothing else, so the
// shape is pinned exactly: one fact per line, tab-separated, mint order, env
// sorted so two hosts serve identical bytes.
func TestGuestEncodingIsFlatAndOrdered(t *testing.T) {
	cfg := Config{Federators: []Federator{
		HiveMind("https://hivemind.wandb.tools"),
		OpenAI("idp_abc123", "idpm_ghi789", ""),
	}}
	got := cfg.Guest()
	want := strings.Join([]string{
		"hivemind\taudience\thttps://hivemind.wandb.tools",
		"hivemind\ttoken_file\t/var/run/secrets/hivemind/token",
		"openai\taudience\thttps://api.openai.com/v1",
		"openai\ttoken_file\t/var/run/secrets/openai.com/identity-token",
		"openai\ttoken_file_env\tOPENAI_IDENTITY_TOKEN_FILE",
		"openai\tcontext_env\tOPENAI_WORKLOAD_IDENTITY_CONTEXT",
		"openai\tenv\tOPENAI_FEDERATION_RULE_ID=idpm_ghi789",
		"openai\tenv\tOPENAI_IDENTITY_PROVIDER_ID=idp_abc123",
		"",
	}, "\n")
	if got != want {
		t.Errorf("guest encoding:\n%s\nwant:\n%s", got, want)
	}
	back, err := ParseGuest(strings.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if !equal(back, cfg.WithDefaults()) {
		t.Errorf("round trip lost something:\n%+v\nwant\n%+v", back, cfg.WithDefaults())
	}
	if Default("x").Guest() == "" || (Config{}).Guest() != "" {
		t.Error("an empty list must encode as an empty body and a non-empty one must not")
	}
}

func TestWithAudiencesKeepsTheIssuerInStepWithTheGuests(t *testing.T) {
	const hivemind = "https://hivemind.wandb.tools"
	cfg := Config{Federators: []Federator{HiveMind(hivemind), OpenAI("idp_x", "idpm_y", "")}}

	t.Run("every federator's audience is added", func(t *testing.T) {
		got := WithAudiences([]string{hivemind}, cfg)
		if !slices.Equal(got, []string{hivemind, "https://api.openai.com/v1"}) {
			t.Errorf("allowlist = %v", got)
		}
	})
	t.Run("an operator's own audience is the one allowlisted", func(t *testing.T) {
		own := Config{Federators: []Federator{{Name: "openai", Audience: "api://sparkbox-codex"}}}
		got := WithAudiences([]string{hivemind}, own)
		if !slices.Contains(got, "api://sparkbox-codex") || slices.Contains(got, "https://api.openai.com/v1") {
			t.Errorf("allowlist = %v, want the configured audience and no default beside it", got)
		}
	})
	t.Run("nothing is added twice", func(t *testing.T) {
		got := WithAudiences([]string{hivemind, "https://api.openai.com/v1"}, cfg)
		if len(got) != 2 {
			t.Errorf("allowlist = %v, want each audience once", got)
		}
	})
	// Empty means "any audience". Appending to it would silently NARROW the
	// issuer to the listed values, turning a permissive development host into
	// one that refuses the mint it was performing yesterday.
	t.Run("an empty allowlist still means any audience", func(t *testing.T) {
		if got := WithAudiences(nil, cfg); len(got) != 0 {
			t.Errorf("allowlist = %v, want it left empty rather than narrowed", got)
		}
	})
	t.Run("the caller's list is not mutated", func(t *testing.T) {
		in := make([]string, 1, 4)
		in[0] = hivemind
		out := WithAudiences(in, cfg)
		if &in[0] == &out[0] {
			t.Error("the allowlist was appended to in place")
		}
	})
}

func equal(a, b Config) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
