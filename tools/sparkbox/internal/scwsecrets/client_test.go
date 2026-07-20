package scwsecrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeSM stands in for Secret Manager's AccessSecretVersionByPath endpoint,
// keyed by "<path>/<name>", returning the same {data: base64, type} envelope the
// real API does.
func fakeSM(t *testing.T, token string, secrets map[string]struct{ payload, typ string }) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Auth-Token"); got != token {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"denied","reason":"condition request.ip"}`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/secrets-by-path/versions/latest_enabled/access") {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		key := q.Get("secret_path") + "/" + q.Get("secret_name")
		s, ok := secrets[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"secret not found"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"secret_id": "00000000-0000-0000-0000-000000000000",
			"revision":  1,
			"data":      base64.StdEncoding.EncodeToString([]byte(s.payload)),
			"type":      s.typ,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAccessByPath(t *testing.T) {
	const tok = "test-secret-key"
	sshWrapped, _ := json.Marshal(SSHKeyPayload{SSHPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----\n"})
	srv := fakeSM(t, tok, map[string]struct{ payload, typ string }{
		"/sparkbox/fleet/gateway-host-key":     {string(sshWrapped), "ssh_key"},
		"/sparkbox/fleet/cloudflare-api-token": {"cf-token-value", "opaque"},
	})
	c := New(srv.URL, "fr-par", "proj-1", tok)

	t.Run("ssh_key round-trips through unwrap", func(t *testing.T) {
		data, typ, err := c.AccessByPath(context.Background(), "/sparkbox/fleet", "gateway-host-key")
		if err != nil {
			t.Fatal(err)
		}
		if typ != "ssh_key" {
			t.Errorf("type = %q, want ssh_key", typ)
		}
		pem, err := UnwrapSSHKey(data)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(pem), "-----BEGIN OPENSSH PRIVATE KEY-----") {
			t.Errorf("unwrapped payload is not a PEM: %q", pem)
		}
	})

	t.Run("opaque returns raw bytes", func(t *testing.T) {
		data, typ, err := c.AccessByPath(context.Background(), "/sparkbox/fleet", "cloudflare-api-token")
		if err != nil {
			t.Fatal(err)
		}
		if typ != "opaque" || string(data) != "cf-token-value" {
			t.Errorf("got type=%q data=%q", typ, data)
		}
	})

	t.Run("missing secret is ErrNotFound", func(t *testing.T) {
		_, _, err := c.AccessByPath(context.Background(), "/sparkbox/fleet", "nope")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestAccessByPathDeniedSurfacesReason(t *testing.T) {
	srv := fakeSM(t, "the-real-token", nil)
	c := New(srv.URL, "fr-par", "proj-1", "wrong-token") // wrong key → 403

	_, _, err := c.AccessByPath(context.Background(), "/sparkbox/fleet", "gateway-host-key")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("want a denial error, got %v", err)
	}
	// The IP-condition failure mode is the one we most need to diagnose, so the
	// Scaleway-provided reason must reach the operator.
	if !strings.Contains(err.Error(), "request.ip") {
		t.Errorf("denial error dropped the reason body: %v", err)
	}
}

func TestUnwrapSSHKeyRejectsNonJSON(t *testing.T) {
	if _, err := UnwrapSSHKey([]byte("-----BEGIN OPENSSH PRIVATE KEY-----")); err == nil {
		t.Error("want an error unwrapping a bare PEM as an ssh_key payload")
	}
	empty, _ := json.Marshal(SSHKeyPayload{})
	if _, err := UnwrapSSHKey(empty); err == nil {
		t.Error("want an error for an empty ssh_private_key")
	}
}

func TestQueryParamsAreSet(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		json.NewEncoder(w).Encode(map[string]any{"data": base64.StdEncoding.EncodeToString([]byte("x")), "type": "opaque"})
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "nl-ams", "proj-xyz", "t")
	if _, _, err := c.AccessByPath(context.Background(), "/a/b", "c"); err != nil {
		t.Fatal(err)
	}
	if got.Get("project_id") != "proj-xyz" || got.Get("secret_path") != "/a/b" || got.Get("secret_name") != "c" {
		t.Errorf("query params wrong: %v", got)
	}
}
