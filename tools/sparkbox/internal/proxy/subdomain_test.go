package proxy

// Host parsing and dispatch precedence. These are unit tests on the Server's
// routing decisions alone: no manager, no route store, no VM — the cases that
// matter here are the ones that must be decided before any of that is
// consulted. The end-to-end counterpart (a real route row losing to a reserved
// handler) lives in proxy_test.go at the repo root.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testServer() *Server {
	return &Server{domain: "hivemind.tools", log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestSubdomainOf(t *testing.T) {
	s := testServer()
	cases := []struct {
		host string
		want string
		ok   bool
	}{
		// One label: the ordinary sandbox route.
		{"myvm.hivemind.tools", "myvm", true},
		{"MyVM.Hivemind.Tools", "myvm", true},
		{"myvm.hivemind.tools:8081", "myvm", true},
		// Two labels: the whole prefix comes back as one string, which is why
		// the reserved map can never match these and suffix dispatch exists.
		{"demo-xterm.hivemind.tools", "demo-xterm", true},
		{"api.myvm.hivemind.tools", "api.myvm", true},
		// Three labels are not special-cased either.
		{"a.b-xterm.hivemind.tools", "a.b-xterm", true},
		// The apex has no subdomain, and neither does a foreign host.
		{"hivemind.tools", "", false},
		{"hivemind.tools:443", "", false},
		{"evil.com", "", false},
		{"nothivemind.tools", "", false},
		// A suffix match that isn't a label boundary must not count.
		{"xhivemind.tools", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := s.subdomainOf(c.host)
		if got != c.want || ok != c.ok {
			t.Errorf("subdomainOf(%q) = (%q, %v), want (%q, %v)", c.host, got, ok, c.want, c.ok)
		}
	}
}

func TestSuffixDispatch(t *testing.T) {
	s := testServer()
	s.SetReserved("console", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "console") //nolint:errcheck
	}))
	s.SetReservedSuffix("xterm", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, ok := SuffixName(r)
		if !ok {
			t.Error("SuffixName reported no dispatch inside a suffix handler")
		}
		io.WriteString(w, "xterm:"+name) //nolint:errcheck
	}))

	cases := []struct {
		host string
		want string // "" means "not dispatched to a built-in handler"
	}{
		{"demo-xterm.hivemind.tools", "xterm:demo"},
		{"demo-XTERM.hivemind.tools", "xterm:demo"},
		{"demo-xterm.hivemind.tools:8443", "xterm:demo"},
		// The whole subtree belongs to the handler, however deep. Falling
		// through here would leave a "a.b-xterm" route row able to squat it.
		{"a.b-xterm.hivemind.tools", "xterm:a.b"},
		// Exact reservations still win, and unrelated hosts are untouched.
		{"console.hivemind.tools", "console"},
		{"myvm.hivemind.tools", ""},
		// The bare label is not claimed by SetReservedSuffix.
		{"xterm.hivemind.tools", ""},
		// Neither is a different suffix that merely contains the label, nor a
		// name the label is only a prefix of — the split is on the last hyphen,
		// so "demoxterm" and "demo-xterm2" are ordinary route lookups.
		{"demo-notxterm.hivemind.tools", ""},
		{"demo-xterm2.hivemind.tools", ""},
		{"demoxterm.hivemind.tools", ""},
	}
	for _, c := range cases {
		sub, ok := s.subdomainOf(c.host)
		if !ok {
			t.Fatalf("subdomainOf(%q) rejected the host", c.host)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		// The same two lookups ServeHTTP performs, in the same order, before it
		// ever consults the route store.
		if h, hit := s.reserved[sub]; hit {
			h.ServeHTTP(rec, req)
		} else if h, name, hit := s.suffixHandler(sub); hit {
			h.ServeHTTP(rec, req.WithContext(context.WithValue(req.Context(), suffixKey, name)))
		}
		if got := rec.Body.String(); got != c.want {
			t.Errorf("%s dispatched to %q, want %q", c.host, got, c.want)
		}
	}
}

func TestSuffixNameAbsentWithoutDispatch(t *testing.T) {
	// A handler mounted anywhere but the edge's suffix dispatcher must be able
	// to tell: SuffixName reporting "" for both cases would make an unrouted
	// request look like a request for the empty sandbox name.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if name, ok := SuffixName(req); ok {
		t.Fatalf("SuffixName on an undispatched request = (%q, true), want ok=false", name)
	}
}

// The apex is not a subdomain, so it falls out of subdomainOf and would 404
// with "request host is not under <domain>" — true, and useless for the one
// hostname a person types from memory.
func TestApexRedirectsHome(t *testing.T) {
	s := testServer()
	s.SetHome("my")

	cases := []struct {
		host, target, want string
	}{
		{"hivemind.tools", "/", "http://my.hivemind.tools/"},
		{"HIVEMIND.TOOLS", "/", "http://my.hivemind.tools/"},
		// The trailing dot of a fully-qualified name is still the apex.
		{"hivemind.tools.", "/", "http://my.hivemind.tools/"},
		// Path and query survive, so a link trimmed back to the apex still
		// lands where it was going.
		{"hivemind.tools", "/machines?tag=prod", "http://my.hivemind.tools/machines?tag=prod"},
		// A non-default edge port has to come along or the redirect leaves the
		// server entirely.
		{"hivemind.tools:8081", "/", "http://my.hivemind.tools:8081/"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, c.target, nil)
		req.Host = c.host
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Errorf("%s%s = %d, want 302", c.host, c.target, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != c.want {
			t.Errorf("%s%s -> %q, want %q", c.host, c.target, got, c.want)
		}
	}
}

// Behind a TLS-terminating front end the connection to us is plain, so the
// scheme has to come from the header or the redirect downgrades every visitor.
func TestApexRedirectHonoursForwardedProto(t *testing.T) {
	s := testServer()
	s.SetHome("my")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "hivemind.tools"
	req.Header.Set("X-Forwarded-Proto", "https")
	s.ServeHTTP(rec, req)
	if got := rec.Header().Get("Location"); got != "https://my.hivemind.tools/" {
		t.Errorf("Location = %q, want https", got)
	}
}

// Only the apex, and only when a home is configured. A foreign host must not be
// bounced into our zone, and an unconfigured edge keeps its honest 404.
func TestApexRedirectIsNarrow(t *testing.T) {
	s := testServer()
	s.SetHome("my")
	for _, host := range []string{"evil.com", "nothivemind.tools", "xhivemind.tools"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", host, rec.Code)
		}
	}

	unset := testServer()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "hivemind.tools"
	unset.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("apex with no home = %d, want 404", rec.Code)
	}
}
