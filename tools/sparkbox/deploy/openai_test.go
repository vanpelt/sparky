package deploy

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
)

// The OpenAI federation payload is what makes `codex` and the OpenAI SDKs work
// in a sandbox with no API key anywhere in it, and every property asserted here
// is one that fails silently if it regresses: a variable Codex does not find is
// a login prompt, and a variable it finds pointing at nothing is a hard failure
// with no fallback to that prompt.
//
// The configuration is built from the real metadata.OpenAI in BOTH encodings,
// for the reason TestGuestGitIdentityWritesAnAttributableAuthor spells out: the
// service encodes with SetIndent, the guest reads with sed, and a reader
// written against hand-typed compact JSON matched nothing at all on a live box.
func TestGuestOpenAIEnvBlock(t *testing.T) {
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)
	writer := filepath.Join(root, "usr/local/sbin/sparkbox-openai-env")

	encode := func(t *testing.T, v any, indent bool) string {
		t.Helper()
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		if indent {
			enc.SetIndent("", "  ")
		}
		if err := enc.Encode(v); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	// render writes conf and identity (an empty string means no file at all,
	// which is what a fleet answering 501 leaves behind), runs the real script,
	// and returns both files it may write. The profile snippet is "" when the
	// script chose not to write one.
	render := func(t *testing.T, conf, identity, envFile string) (env, profile string) {
		t.Helper()
		dir := t.TempDir()
		confPath := filepath.Join(dir, "openai.json")
		if conf != "" {
			if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		idPath := filepath.Join(dir, "identity.json")
		if identity != "" {
			if err := os.WriteFile(idPath, []byte(identity), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		envPath := filepath.Join(dir, "environment")
		if err := os.WriteFile(envPath, []byte(envFile), 0o644); err != nil {
			t.Fatal(err)
		}
		profileDir := filepath.Join(dir, "profile.d")
		cmd := exec.Command("sh", writer, confPath, idPath)
		cmd.Env = append(os.Environ(),
			"SPARKBOX_ENVIRONMENT="+envPath,
			"SPARKBOX_PROFILE_D="+profileDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sparkbox-openai-env: %v\n%s", err, out)
		}
		body, err := os.ReadFile(envPath)
		if err != nil {
			t.Fatal(err)
		}
		snippet, err := os.ReadFile(filepath.Join(profileDir, "sparkbox-openai.sh"))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		return string(body), string(snippet)
	}

	// The PATH the image bakes in, and envsync's own managed block. Neither is
	// this script's to touch, and losing either is worse than losing the
	// feature: a rewritten PATH breaks the SSH push channel itself.
	const existing = "PATH=\"/usr/local/bin:/usr/bin:/bin\"\n" +
		"# --- sparkbox-managed secrets (do not edit below) ---\n" +
		"ANTHROPIC_API_KEY=\"sk-test\"\n" +
		"# --- end sparkbox-managed ---\n"

	configured := metadata.OpenAI{
		Audience:         metadata.DefaultOpenAIAudience,
		IdentityProvider: "idp_abc123",
		ServiceAccount:   "svc_def456",
		FederationRule:   "idpm_ghi789",
		TokenFile:        metadata.DefaultOpenAITokenFile,
	}
	identity := metadata.Doc{
		Issuer: "https://oidc.catnip.sh", Subject: "sparkbox:user:vanpelt", Owner: "vanpelt",
		Sandbox: "dazzling-canyon", SandboxID: "sb_01", Image: "ubuntu", Box: "sparky",
	}

	for _, enc := range []struct {
		name   string
		indent bool
	}{
		// The one the metadata service actually serves goes first.
		{name: "indented", indent: true},
		{name: "compact"},
	} {
		t.Run(enc.name, func(t *testing.T) {
			conf := encode(t, configured, enc.indent)
			id := encode(t, identity, enc.indent)

			t.Run("a configured fleet exports what codex and the SDKs read", func(t *testing.T) {
				got, _ := render(t, conf, id, existing)
				for _, want := range []string{
					"OPENAI_IDENTITY_TOKEN_FILE=" + metadata.DefaultOpenAITokenFile + "\n",
					// Codex needs this one and nothing else.
					"OPENAI_FEDERATION_RULE_ID=idpm_ghi789\n",
					// The SDKs perform the exchange themselves and need both.
					"OPENAI_IDENTITY_PROVIDER_ID=idp_abc123\n",
					"OPENAI_SERVICE_ACCOUNT_ID=svc_def456\n",
					// Everything that was already in the file.
					"PATH=\"/usr/local/bin:/usr/bin:/bin\"\n",
					"ANTHROPIC_API_KEY=\"sk-test\"\n",
				} {
					if !strings.Contains(got, want) {
						t.Errorf("missing %q in:\n%s", want, got)
					}
				}
			})

			// The reason the attribution context is not in /etc/environment at
			// all. A shell sourcing that file performs quote removal, so a bare
			// JSON value arrives as `{instance_id:box}` — not JSON any more —
			// and a quoted one depends on pam_env stripping the pair back off.
			t.Run("no JSON reaches the environment file", func(t *testing.T) {
				got, _ := render(t, conf, id, existing)
				if strings.Contains(got, "OPENAI_WORKLOAD_IDENTITY_CONTEXT") {
					t.Errorf("the attribution context moved into /etc/environment, where quote removal destroys it:\n%s", got)
				}
			})

			// The snippet is shell, so the test that matters is whether a shell
			// sourcing it hands OpenAI back the JSON we meant to send.
			t.Run("attribution survives being sourced by a shell", func(t *testing.T) {
				_, snippet := render(t, conf, id, existing)
				if snippet == "" {
					t.Fatal("no profile snippet was written, so nothing attributes a request to a sandbox")
				}
				dir := t.TempDir()
				path := filepath.Join(dir, "sparkbox-openai.sh")
				if err := os.WriteFile(path, []byte(snippet), 0o644); err != nil {
					t.Fatal(err)
				}
				out, err := exec.Command("bash", "-c",
					". "+path+`; printf '%s' "$OPENAI_WORKLOAD_IDENTITY_CONTEXT"`).CombinedOutput()
				if err != nil {
					t.Fatalf("sourcing the snippet: %v\n%s", err, out)
				}
				var probe struct {
					InstanceID string            `json:"instance_id"`
					Labels     map[string]string `json:"labels"`
				}
				if err := json.Unmarshal(out, &probe); err != nil {
					t.Fatalf("attribution context is not JSON OpenAI will accept: %v (%q)", err, out)
				}
				if probe.InstanceID != "dazzling-canyon" {
					t.Errorf("instance_id = %q, want the sandbox name", probe.InstanceID)
				}
				if probe.Labels["owner"] != "vanpelt" || probe.Labels["box"] != "sparky" {
					t.Errorf("labels = %v, want the owner and the node holding the sandbox", probe.Labels)
				}
				// OpenAI caps the whole object at 1,024 bytes.
				if len(out) > 1024 {
					t.Errorf("attribution context is %d bytes, over OpenAI's 1,024-byte cap", len(out))
				}
			})

			t.Run("a rewrite replaces the block instead of stacking copies", func(t *testing.T) {
				once, _ := render(t, conf, id, existing)
				twice, _ := render(t, conf, id, once)
				if strings.Count(twice, "OPENAI_FEDERATION_RULE_ID=") != 1 {
					t.Errorf("re-running stacked a second block:\n%s", twice)
				}
			})
		})
	}

	// The token unit calls this on every refresh whether or not the fleet
	// federates, so these are the paths that actually run on most boxes.
	t.Run("a fleet that does not federate gets no block", func(t *testing.T) {
		got, snippet := render(t, "", "", existing)
		if strings.Contains(got, "OPENAI_") {
			t.Errorf("unconfigured fleet exported OpenAI variables:\n%s", got)
		}
		if snippet != "" {
			t.Errorf("unconfigured fleet wrote a profile snippet:\n%s", snippet)
		}
		if !strings.Contains(got, "ANTHROPIC_API_KEY=\"sk-test\"") {
			t.Errorf("unconfigured fleet lost the secrets block:\n%s", got)
		}
	})

	// Federation turned off on a fleet, or a fork template carried across
	// fleets: the stale block has to go, or codex keeps federating against a
	// rule that no longer exists instead of falling back to its own login.
	t.Run("turning federation off removes a block already on disk", func(t *testing.T) {
		configuredEnv, _ := render(t, encode(t, configured, true), encode(t, identity, true), existing)
		got, _ := render(t, "", "", configuredEnv)
		if strings.Contains(got, "OPENAI_") {
			t.Errorf("stale OpenAI block survived a fleet turning federation off:\n%s", got)
		}
		if !strings.Contains(got, "PATH=") {
			t.Errorf("removing the block took the PATH with it:\n%s", got)
		}
	})

	// Half-configured is worse than absent: a Codex that finds a token file
	// federates rather than offering the login the user could have completed.
	t.Run("a token file with nothing to present it against writes no block", func(t *testing.T) {
		conf := encode(t, metadata.OpenAI{
			Audience:  metadata.DefaultOpenAIAudience,
			TokenFile: metadata.DefaultOpenAITokenFile,
		}, true)
		got, snippet := render(t, conf, encode(t, identity, true), existing)
		if strings.Contains(got, "OPENAI_IDENTITY_TOKEN_FILE") {
			t.Errorf("exported a token file with no federation rule or provider:\n%s", got)
		}
		if snippet != "" {
			t.Errorf("attributed requests a sandbox cannot make:\n%s", snippet)
		}
	})

	// Attribution is optional to OpenAI, so it is never the reason a sandbox
	// fails to authenticate at all.
	t.Run("a missing identity snapshot still exports the credentials", func(t *testing.T) {
		got, snippet := render(t, encode(t, configured, true), "", existing)
		if !strings.Contains(got, "OPENAI_FEDERATION_RULE_ID=idpm_ghi789") {
			t.Errorf("lost the federation rule when attribution was unavailable:\n%s", got)
		}
		if snippet != "" {
			t.Errorf("wrote an attribution context with no sandbox name to put in it:\n%s", snippet)
		}
	})
}

// The assertion is a second mint at a second audience, and the properties that
// matter are the ones OpenAI's own guidance is specific about: an absolute path
// in a dedicated 0700 directory owned by the account that runs the agent, and a
// refresh that never blocks on the fleet having configured any of this.
func TestGuestTokenFetchesTheOpenAIAssertion(t *testing.T) {
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)
	token := guestFile(t, root, "usr/local/sbin/sparkbox-token")

	for _, want := range []struct {
		fragment string
		why      string
	}{
		{"$META/openai", "nothing asks the host whether this fleet federates with OpenAI"},
		{`--data-urlencode "aud=$OPENAI_AUD"`,
			"the OpenAI audience is pasted into the query instead of being encoded; it is a URL and carries a ':' and two '/' of its own"},
		{`chmod 0700 "$OPENAI_DIR"`,
			"the assertion directory is not 0700, which is what OpenAI asks for"},
		{`chown "$SANDBOX_USER" "$OPENAI_DIR"`,
			"the assertion directory is not owned by the account that runs the agent, so a 0700 directory locks that account out of its own credential"},
		{"/usr/local/sbin/sparkbox-openai-env",
			"nothing renders the environment block, so codex never learns the token file exists"},
	} {
		if !strings.Contains(token, want.fragment) {
			t.Errorf("sparkbox-token: %s (missing %q)", want.why, want.fragment)
		}
	}

	// Ordering is the load-bearing part. The hivemind fetch above exits 1 on
	// failure and is what a sandbox cannot work without; an OpenAI integration
	// the fleet may not even have configured must never be able to reach that
	// exit, and must never run before it.
	hivemind := strings.Index(token, "HIVEMIND_OIDC_TOKEN_FILE")
	openai := strings.Index(token, "$META/openai")
	if hivemind < 0 || openai < 0 || openai < hivemind {
		t.Error("the OpenAI block no longer runs after the hivemind token fetch, so a bad minute at OpenAI can cost a sandbox its identity")
	}

	// The env renderer runs unconditionally — that call is what removes a stale
	// block from a box whose fleet turned federation off — so it must sit
	// outside the `if [ -s "$OPENAI_CONF" ]` guard.
	if !strings.Contains(token, "fi\n\n# Unconditional") {
		t.Error("the environment renderer moved inside the configured-fleet guard, so turning federation off no longer reaches running boxes")
	}
}
