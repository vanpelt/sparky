package deploy

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/federation"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/metadata"
)

// The federation payload is what makes `codex`, the OpenAI SDKs and whatever a
// relying party added next work in a sandbox with no API key anywhere in it,
// and every property asserted here is one that fails silently if it regresses:
// a variable a client does not find is a login prompt, and a variable it finds
// pointing at nothing is a hard failure with no fallback to that prompt.
//
// The list is rendered by the real federation.Config.Guest, for the reason
// TestGuestGitIdentityWritesAnAttributableAuthor spells out: the service
// encodes, the guest reads with awk and cut, and a reader written against a
// hand-typed list matched nothing at all on a live box. The identity snapshot
// is built in BOTH JSON encodings for the same reason.
func TestGuestFederationEnvBlock(t *testing.T) {
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)
	writer := filepath.Join(root, "usr/local/sbin/sparkbox-federation-env")

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

	// A guest tree with an environment file and a profile.d, so a test can
	// run the script twice against the same box.
	type box struct{ env, profileDir, fed, id string }
	newBox := func(t *testing.T, envFile string) box {
		t.Helper()
		dir := t.TempDir()
		b := box{
			env:        filepath.Join(dir, "environment"),
			profileDir: filepath.Join(dir, "profile.d"),
			fed:        filepath.Join(dir, "federation"),
			id:         filepath.Join(dir, "identity.json"),
		}
		if err := os.WriteFile(b.env, []byte(envFile), 0o644); err != nil {
			t.Fatal(err)
		}
		return b
	}
	// render writes the list and identity (an empty string means no file at
	// all, which is what a host answering nothing leaves behind), runs the real
	// script, and returns the environment file and the snippets it may have
	// written, keyed by file name.
	render := func(t *testing.T, b box, list, identity string) (env string, snippets map[string]string) {
		t.Helper()
		for path, body := range map[string]string{b.fed: list, b.id: identity} {
			os.Remove(path)
			if body != "" {
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		cmd := exec.Command("sh", writer, b.fed, b.id)
		cmd.Env = append(os.Environ(),
			"SPARKBOX_ENVIRONMENT="+b.env,
			"SPARKBOX_PROFILE_D="+b.profileDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sparkbox-federation-env: %v\n%s", err, out)
		}
		body, err := os.ReadFile(b.env)
		if err != nil {
			t.Fatal(err)
		}
		snippets = map[string]string{}
		entries, _ := os.ReadDir(b.profileDir)
		for _, e := range entries {
			snippets[e.Name()] = guestFile(t, b.profileDir, e.Name())
		}
		return string(body), snippets
	}

	// The PATH the image bakes in, and envsync's own managed block. Neither is
	// this script's to touch, and losing either is worse than losing the
	// feature: a rewritten PATH breaks the SSH push channel itself.
	const existing = "PATH=\"/usr/local/bin:/usr/bin:/bin\"\n" +
		"# --- sparkbox-managed secrets (do not edit below) ---\n" +
		"ANTHROPIC_API_KEY=\"sk-test\"\n" +
		"# --- end sparkbox-managed ---\n"

	openai := federation.OpenAI("idp_abc123", "idpm_ghi789", "svc_def456")
	both := federation.Config{Federators: []federation.Federator{
		federation.HiveMind("https://hivemind.wandb.tools"), openai,
	}}.Guest()
	hivemindOnly := federation.Default("https://hivemind.wandb.tools").Guest()
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
			id := encode(t, identity, enc.indent)

			t.Run("a configured party exports what its client reads", func(t *testing.T) {
				got, _ := render(t, newBox(t, existing), both, id)
				for _, want := range []string{
					"OPENAI_IDENTITY_TOKEN_FILE=" + openai.TokenFile + "\n",
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
				// HiveMind's daemon already looks at its path, so its entry
				// names no variable and exports nothing: a block that invented
				// one would be a block that drifts from the daemon's default.
				if strings.Contains(got, "HIVEMIND") {
					t.Errorf("hivemind exported a variable nothing reads:\n%s", got)
				}
			})

			// The reason the attribution context is not in /etc/environment at
			// all. A shell sourcing that file performs quote removal, so a bare
			// JSON value arrives as `{instance_id:box}` — not JSON any more —
			// and a quoted one depends on pam_env stripping the pair back off.
			t.Run("no JSON reaches the environment file", func(t *testing.T) {
				got, _ := render(t, newBox(t, existing), both, id)
				if strings.Contains(got, "OPENAI_WORKLOAD_IDENTITY_CONTEXT") || strings.Contains(got, "{") {
					t.Errorf("the attribution context moved into /etc/environment, where quote removal destroys it:\n%s", got)
				}
			})

			// The snippet is shell, so the test that matters is whether a shell
			// sourcing it hands the relying party back the JSON we meant to send.
			t.Run("attribution survives being sourced by a shell", func(t *testing.T) {
				_, snippets := render(t, newBox(t, existing), both, id)
				snippet := snippets["sparkbox-openai.sh"]
				if snippet == "" {
					t.Fatalf("no profile snippet was written for openai, so nothing attributes a request to a sandbox (got %v)", snippets)
				}
				if _, stray := snippets["sparkbox-hivemind.sh"]; stray {
					t.Error("a snippet was written for a party that names no context variable")
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
				b := newBox(t, existing)
				render(t, b, both, id)
				twice, _ := render(t, b, both, id)
				if strings.Count(twice, "OPENAI_FEDERATION_RULE_ID=") != 1 {
					t.Errorf("re-running stacked a second block:\n%s", twice)
				}
			})
		})
	}
	id := encode(t, identity, true)

	// The token unit calls this on every refresh whatever the list says, so
	// these are the paths that actually run on most boxes.
	t.Run("the default list exports nothing", func(t *testing.T) {
		got, snippets := render(t, newBox(t, existing), hivemindOnly, id)
		if strings.Contains(got, ">>> sparkbox federation") {
			t.Errorf("a list with nothing to export still wrote a block:\n%s", got)
		}
		if len(snippets) != 0 {
			t.Errorf("wrote snippets for a list with no context variable: %v", snippets)
		}
		if !strings.Contains(got, "ANTHROPIC_API_KEY=\"sk-test\"") {
			t.Errorf("lost the secrets block:\n%s", got)
		}
	})

	// A party dropped from the list, or a fork template carried across fleets:
	// its variables and its snippet have to go, or codex keeps federating
	// against a rule that no longer exists instead of falling back to its own
	// login.
	t.Run("dropping a party removes what it exported", func(t *testing.T) {
		b := newBox(t, existing)
		_, before := render(t, b, both, id)
		if _, ok := before["sparkbox-openai.sh"]; !ok {
			t.Fatal("setup: no openai snippet to remove")
		}
		got, after := render(t, b, hivemindOnly, id)
		if strings.Contains(got, "OPENAI_") {
			t.Errorf("stale OpenAI block survived the party leaving the list:\n%s", got)
		}
		if _, still := after["sparkbox-openai.sh"]; still {
			t.Error("stale openai attribution snippet survived the party leaving the list")
		}
		if !strings.Contains(got, "PATH=") {
			t.Errorf("removing the block took the PATH with it:\n%s", got)
		}
	})

	t.Run("an absent list removes everything too", func(t *testing.T) {
		b := newBox(t, existing)
		render(t, b, both, id)
		got, after := render(t, b, "", "")
		if strings.Contains(got, "OPENAI_") || len(after) != 0 {
			t.Errorf("an absent list left federation state behind:\n%s\n%v", got, after)
		}
	})

	// The stamp is the test for what is ours to remove, not the name.
	t.Run("a snippet somebody else wrote is left alone", func(t *testing.T) {
		b := newBox(t, existing)
		if err := os.MkdirAll(b.profileDir, 0o755); err != nil {
			t.Fatal(err)
		}
		foreign := filepath.Join(b.profileDir, "sparkbox-custom.sh")
		if err := os.WriteFile(foreign, []byte("export MINE=1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, after := render(t, b, hivemindOnly, id)
		if after["sparkbox-custom.sh"] != "export MINE=1\n" {
			t.Errorf("a file this script did not write was removed or rewritten: %v", after)
		}
	})

	// Attribution is optional to a relying party, so it is never the reason a
	// sandbox fails to authenticate at all.
	t.Run("a missing identity snapshot still exports the credentials", func(t *testing.T) {
		got, snippets := render(t, newBox(t, existing), both, "")
		if !strings.Contains(got, "OPENAI_FEDERATION_RULE_ID=idpm_ghi789") {
			t.Errorf("lost the federation rule when attribution was unavailable:\n%s", got)
		}
		if len(snippets) != 0 {
			t.Errorf("wrote an attribution context with no sandbox name to put in it: %v", snippets)
		}
	})

	// An operator's own party, invented for this test: nothing in the guest
	// knows its name, which is the whole point.
	t.Run("a party nobody has heard of works the same way", func(t *testing.T) {
		list := federation.Config{Federators: []federation.Federator{{
			Name: "vault", Audience: "api://vault-prod", TokenFileEnv: "VAULT_JWT_FILE",
			Env: map[string]string{"VAULT_ROLE": "sparkbox", "VAULT_ADDR": "https://vault.example.com:8200"},
		}}}.Guest()
		got, _ := render(t, newBox(t, existing), list, id)
		for _, want := range []string{
			"VAULT_JWT_FILE=/var/run/secrets/vault/token\n",
			"VAULT_ADDR=https://vault.example.com:8200\n",
			"VAULT_ROLE=sparkbox\n",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q in:\n%s", want, got)
			}
		}
	})
}

// The token unit walks the list the host serves, and the properties that
// matter are the ones no party-specific test would catch: that it asks the
// host which parties exist rather than knowing, that every assertion lands in
// the shape OpenAI's guidance is specific about (a 0700 directory owned by the
// account that runs the agent), and that one party's bad minute costs no other
// party its token.
func TestGuestTokenWalksTheFederationList(t *testing.T) {
	root := fakeGuestTree(t, false)
	installGuestPayload(t, root)
	token := guestFile(t, root, "usr/local/sbin/sparkbox-token")

	for _, want := range []struct {
		fragment string
		why      string
	}{
		{"$META/federation", "nothing asks the host which relying parties this fleet has"},
		{`--data-urlencode "aud=$1"`,
			"the audience is pasted into the query instead of being encoded; it is a URL and carries a ':' and two '/' of its own"},
		{`chmod 0700 "$dir"`,
			"the assertion directory is not 0700, which is what OpenAI asks for"},
		{`chown "$SANDBOX_USER" "$dir"`,
			"the assertion directory is not owned by the account that runs the agent, so a 0700 directory locks that account out of its own credential"},
		{"/usr/local/sbin/sparkbox-federation-env",
			"nothing renders the environment block, so codex never learns the token file exists"},
		{`printf 'hivemind\ttoken_file\t/var/run/secrets/hivemind/token\n'`,
			"a host that predates /federation would leave a new template with no token at all"},
		{`exit "$failed"`,
			"a failed mint no longer fails the unit, so systemd never retries it"},
	} {
		if !strings.Contains(token, want.fragment) {
			t.Errorf("sparkbox-token: %s (missing %q)", want.why, want.fragment)
		}
	}

	// No party by name. The day one appears here is the day adding the next
	// one is a template rebuild again.
	for _, party := range []string{"openai", "OPENAI", "HIVEMIND_OIDC_TOKEN_FILE"} {
		if strings.Contains(token, party) {
			t.Errorf("sparkbox-token names %q; parties come from the host's list, not the script", party)
		}
	}

	// The exit status is decided LAST. Everything after the mint loop — the
	// identity snapshot, the environment block — runs whether or not a mint
	// failed, so one party's bad minute never leaves another's variables stale.
	loop := strings.Index(token, "for name in")
	env := strings.Index(token, "sparkbox-federation-env")
	exit := strings.LastIndex(token, `exit "$failed"`)
	if loop < 0 || env < loop || exit < env {
		t.Error("the environment renderer or the exit moved ahead of the mint loop")
	}
}

// The example beside deploy.sh is what an operator copies, so it has to be a
// list the binary accepts — with the placeholders an OpenAI admin replaces
// still in it, since those are identifiers and the loader has no opinion about
// their spelling.
func TestFederationExampleIsALoadableList(t *testing.T) {
	cfg, err := federation.Load("kubernetes/federation.example.json", federation.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Names(); len(got) != 2 || got[0] != "hivemind" || got[1] != "openai" {
		t.Errorf("example lists %v, want hivemind then openai", got)
	}
	// The OpenAI entry in the example is exactly what federation.OpenAI
	// builds, placeholders aside: the two must not drift, because one is what
	// the docs show and the other is what the tests exercise.
	want := federation.OpenAI("idp_REPLACE_ME", "idpm_REPLACE_ME", "svc_REPLACE_ME")
	got, _ := cfg.Get("openai")
	if got.Audience != want.Audience || got.TokenFile != want.TokenFile ||
		got.TokenFileEnv != want.TokenFileEnv || got.ContextEnv != want.ContextEnv ||
		len(got.Env) != len(want.Env) {
		t.Errorf("example openai entry:\n%+v\nwant federation.OpenAI's:\n%+v", got, want)
	}
	for k, v := range want.Env {
		if got.Env[k] != v {
			t.Errorf("example env %s = %q, want %q", k, got.Env[k], v)
		}
	}
}
