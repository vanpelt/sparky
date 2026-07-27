package bootsecrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSM serves the AccessSecretVersionByPath envelope for a fixed set of
// secrets keyed by "<path>/<name>".
func fakeSM(t *testing.T, secrets map[string]struct{ payload, typ string }) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s, ok := secrets[q.Get("secret_path")+"/"+q.Get("secret_name")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": base64.StdEncoding.EncodeToString([]byte(s.payload)),
			"type": s.typ,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sshSecret(pem string) struct{ payload, typ string } {
	b, _ := json.Marshal(map[string]string{"ssh_private_key": pem})
	return struct{ payload, typ string }{string(b), "ssh_key"}
}

func cfg(t *testing.T, srv *httptest.Server, dir string) Config {
	t.Helper()
	src, err := NewScalewaySource(ScalewayConfig{
		BaseURL:   srv.URL,
		Region:    "fr-par",
		ProjectID: "proj",
		Token:     "k",
		Path:      "/sparkbox/fleet",
	})
	if err != nil {
		t.Fatalf("NewScalewaySource: %v", err)
	}
	return Config{
		Source: src,
		KeyDir: filepath.Join(dir, "keys"),
		EnvOut: filepath.Join(dir, "secrets.env"),
	}
}

func TestFetchWritesKeysAndEnv(t *testing.T) {
	const hostPEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nHOST\n-----END OPENSSH PRIVATE KEY-----\n"
	const upstreamPEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nUP\n-----END OPENSSH PRIVATE KEY-----\n"
	const oidcPEM = "-----BEGIN PRIVATE KEY-----\nOIDC\n-----END PRIVATE KEY-----\n"
	const caCertPEM = "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----\n"
	const caKeyPEM = "-----BEGIN PRIVATE KEY-----\nCAKEY\n-----END PRIVATE KEY-----\n"
	const controlKeyPEM = "-----BEGIN PRIVATE KEY-----\nCONTROL\n-----END PRIVATE KEY-----\n"
	srv := fakeSM(t, map[string]struct{ payload, typ string }{
		"/sparkbox/fleet/gateway-host-key":     sshSecret(hostPEM),
		"/sparkbox/fleet/gateway-upstream-key": sshSecret(upstreamPEM),
		"/sparkbox/fleet/oidc-signing-key":     {oidcPEM, "opaque"},
		"/sparkbox/fleet/node-control-ca-cert": {caCertPEM, "opaque"},
		"/sparkbox/fleet/node-control-ca-key":  {caKeyPEM, "opaque"},
		"/sparkbox/fleet/gateway-control-key":  {controlKeyPEM, "opaque"},
		"/sparkbox/fleet/cloudflare-api-token": {"cf-token", "opaque"},
		"/sparkbox/fleet/console-password":     {`p@ss "with" quotes`, "opaque"},
	})

	dir := t.TempDir()
	c := cfg(t, srv, dir)
	if err := Fetch(context.Background(), c); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// PEMs land unwrapped, with tight perms.
	for name, want := range map[string]string{
		"gateway_host_key.pem":     hostPEM,
		"gateway_upstream_key.pem": upstreamPEM,
		"oidc_signing_key.pem":     oidcPEM,
		"node_ca_cert.pem":         caCertPEM,
		"node_ca_key.pem":          caKeyPEM,
		"gateway_control_key.pem":  controlKeyPEM,
	} {
		p := filepath.Join(c.KeyDir, name)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s content = %q, want %q", name, got, want)
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s perms = %o, want 600", name, fi.Mode().Perm())
		}
	}

	// Env file carries both passwords, quoted, at 0600.
	envBytes, err := os.ReadFile(c.EnvOut)
	if err != nil {
		t.Fatal(err)
	}
	env := string(envBytes)
	if !strings.Contains(env, `CLOUDFLARE_API_TOKEN="cf-token"`) {
		t.Errorf("env missing cloudflare token:\n%s", env)
	}
	if !strings.Contains(env, `SPARKBOX_CONSOLE_PASSWORD="p@ss \"with\" quotes"`) {
		t.Errorf("env did not escape quotes in the password:\n%s", env)
	}
	if fi, _ := os.Stat(c.EnvOut); fi.Mode().Perm() != 0o600 {
		t.Errorf("env perms = %o, want 600", fi.Mode().Perm())
	}
}

func TestFetchOptionalMissing(t *testing.T) {
	// Only the three required keys exist; the two env secrets are absent.
	srv := fakeSM(t, map[string]struct{ payload, typ string }{
		"/sparkbox/fleet/gateway-host-key":     sshSecret("-----BEGIN OPENSSH PRIVATE KEY-----\nA\n-----END OPENSSH PRIVATE KEY-----\n"),
		"/sparkbox/fleet/gateway-upstream-key": sshSecret("-----BEGIN OPENSSH PRIVATE KEY-----\nB\n-----END OPENSSH PRIVATE KEY-----\n"),
		"/sparkbox/fleet/oidc-signing-key":     {"-----BEGIN PRIVATE KEY-----\nC\n-----END PRIVATE KEY-----\n", "opaque"},
	})
	dir := t.TempDir()
	if err := Fetch(context.Background(), cfg(t, srv, dir)); err != nil {
		t.Fatalf("optional secrets missing should not fail the boot: %v", err)
	}
	// Env file still exists (with no vars), so a `-` EnvironmentFile is happy.
	if _, err := os.Stat(filepath.Join(dir, "secrets.env")); err != nil {
		t.Errorf("env file not written: %v", err)
	}
}

func TestFetchRequiredMissingFails(t *testing.T) {
	srv := fakeSM(t, map[string]struct{ payload, typ string }{}) // nothing exists
	dir := t.TempDir()
	err := Fetch(context.Background(), cfg(t, srv, dir))
	if err == nil || !strings.Contains(err.Error(), "gateway-host-key") {
		t.Errorf("want a required-secret-missing error naming the secret, got %v", err)
	}
}

func TestScalewaySourceNeedsCredentials(t *testing.T) {
	if _, err := NewScalewaySource(ScalewayConfig{ProjectID: "proj"}); err == nil {
		t.Error("want an error when the token is empty")
	}
	if _, err := NewScalewaySource(ScalewayConfig{Token: "k"}); err == nil {
		t.Error("want an error when the project ID is empty")
	}
}

func TestFetchNeedsSource(t *testing.T) {
	dir := t.TempDir()
	err := Fetch(context.Background(), Config{
		KeyDir: filepath.Join(dir, "keys"),
		EnvOut: filepath.Join(dir, "secrets.env"),
	})
	if err == nil {
		t.Error("want an error when no source is configured")
	}
}

func TestFetchWrongTypeFails(t *testing.T) {
	// gateway-host-key stored as opaque instead of ssh_key: must fail loudly,
	// not write a mistyped payload where a PEM belongs.
	srv := fakeSM(t, map[string]struct{ payload, typ string }{
		"/sparkbox/fleet/gateway-host-key": {"not-a-wrapped-key", "opaque"},
	})
	dir := t.TempDir()
	err := Fetch(context.Background(), cfg(t, srv, dir))
	if err == nil || !strings.Contains(err.Error(), "ssh_key") {
		t.Errorf("want a type-mismatch error, got %v", err)
	}
}

// mapSource is a store with no notion of secret types — the shape 1Password
// presents, where an SSH key is a bare PEM rather than Scaleway's JSON envelope.
type mapSource map[string]string

func (m mapSource) Get(_ context.Context, name string) ([]byte, string, error) {
	v, ok := m[name]
	if !ok {
		return nil, "", ErrNotFound
	}
	return []byte(v), "", nil
}

func (m mapSource) Describe() string { return "test source" }

func TestFetchFromUntypedSource(t *testing.T) {
	const hostPEM = "-----BEGIN OPENSSH PRIVATE KEY-----\nHOST\n-----END OPENSSH PRIVATE KEY-----\n"
	const oidcPEM = "-----BEGIN PRIVATE KEY-----\nOIDC\n-----END PRIVATE KEY-----\n"
	dir := t.TempDir()
	c := Config{
		Source: mapSource{
			"gateway-host-key":     hostPEM,
			"gateway-upstream-key": hostPEM,
			"oidc-signing-key":     oidcPEM,
			// No trailing newline: a paste into a secret store often loses it.
			"node-control-ca-key": "-----BEGIN PRIVATE KEY-----\nCAKEY\n-----END PRIVATE KEY-----",
			// A trailing newline on an env value must not fail the boot.
			"console-password": "hunter2\n",
		},
		KeyDir: filepath.Join(dir, "keys"),
		EnvOut: filepath.Join(dir, "secrets.env"),
	}
	if err := Fetch(context.Background(), c); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// The SSH key is stored verbatim, NOT run through the JSON unwrap.
	got, err := os.ReadFile(filepath.Join(c.KeyDir, "gateway_host_key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != hostPEM {
		t.Errorf("host key = %q, want the raw PEM %q", got, hostPEM)
	}

	// A PEM that arrived without its trailing newline gains exactly one.
	got, err = os.ReadFile(filepath.Join(c.KeyDir, "node_ca_key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "-----BEGIN PRIVATE KEY-----\nCAKEY\n-----END PRIVATE KEY-----\n"; string(got) != want {
		t.Errorf("ca key = %q, want a single trailing newline %q", got, want)
	}

	envBytes, err := os.ReadFile(c.EnvOut)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envBytes), `SPARKBOX_CONSOLE_PASSWORD="hunter2"`) {
		t.Errorf("env did not trim the trailing newline:\n%s", envBytes)
	}
}
