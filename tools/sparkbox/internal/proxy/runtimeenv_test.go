package proxy

// The edge answers /__ENV.js for a guest that cannot. What these pin is the
// narrowness of it: a failure on exactly that path is filled in, and nothing
// else is touched — not a guest that serves its own, not another path, not a
// deployment that turned the behaviour off.

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
)

func envServer(t *testing.T) *Server {
	t.Helper()
	return New(placedManager{}, nil, "hivemind.tools", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// response builds an upstream reply for path with the given status, standing
// in for what ClickHouse (or any guest) handed back.
func response(path string, status int, body string) *http.Response {
	req := httptest.NewRequest(http.MethodGet, "http://box.hivemind.tools"+path, nil)
	return &http.Response{
		StatusCode:    status,
		Request:       req,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Header: http.Header{
			"Content-Type":     []string{"text/plain"},
			"Content-Encoding": []string{"gzip"},
		},
	}
}

func TestRuntimeEnvFillsGuestFailure(t *testing.T) {
	s := envServer(t)
	resp := response(runtimeEnvPath, http.StatusServiceUnavailable, "upstream said no")
	if err := s.fillRuntimeEnv(resp); err != nil {
		t.Fatalf("fillRuntimeEnv: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), "window['__ENV'] = {") {
		t.Fatalf("body is not a next-runtime-env payload: %q", body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Errorf("Content-Type = %q, want text/javascript", got)
	}
	// The discarded upstream body's framing must not survive onto ours, or the
	// browser tries to gunzip plain JavaScript.
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want it dropped", got)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Errorf("ContentLength = %d, want %d", resp.ContentLength, len(body))
	}
}

func TestRuntimeEnvNeverShadowsAGuestThatServesIt(t *testing.T) {
	s := envServer(t)
	const own = "window['__ENV'] = {\"NEXT_PUBLIC_MINE\":\"1\"};"
	resp := response(runtimeEnvPath, http.StatusOK, own)
	if err := s.fillRuntimeEnv(resp); err != nil {
		t.Fatalf("fillRuntimeEnv: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != own {
		t.Fatalf("guest's own payload was replaced: %q", body)
	}
}

func TestRuntimeEnvLeavesOtherPathsAlone(t *testing.T) {
	s := envServer(t)
	resp := response("/clickstack/search", http.StatusServiceUnavailable, "upstream said no")
	if err := s.fillRuntimeEnv(resp); err != nil {
		t.Fatalf("fillRuntimeEnv: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the upstream's 503 untouched", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream said no" {
		t.Fatalf("body = %q, want the upstream's untouched", body)
	}
}

func TestSetRuntimeEnvNilDisables(t *testing.T) {
	s := envServer(t)
	s.SetRuntimeEnv(nil)
	resp := response(runtimeEnvPath, http.StatusServiceUnavailable, "upstream said no")
	if err := s.fillRuntimeEnv(resp); err != nil {
		t.Fatalf("fillRuntimeEnv: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the upstream's 503 untouched", resp.StatusCode)
	}
}

// The seeded connection is the entire fix, and it works only because of what
// it does NOT contain: a password key. HyperDX selects its
// credentials-omitting fetch with `username != null && password != null`, so a
// password of "" would put the console straight back to 401ing on every query.
func TestClickStackSeedOmitsPassword(t *testing.T) {
	var conns []map[string]any
	if err := json.Unmarshal([]byte(clickStackConnections), &conns); err != nil {
		t.Fatalf("seed is not valid JSON: %v", err)
	}
	if len(conns) != 1 {
		t.Fatalf("got %d connections, want exactly 1 (a second would re-open the onboarding modal)", len(conns))
	}
	if _, ok := conns[0]["password"]; ok {
		t.Error("seed carries a password key; that selects HyperDX local mode and the 401 comes back")
	}
	if got := conns[0]["username"]; got != "default" {
		t.Errorf("username = %v, want %q", got, "default")
	}
	// host is a pathname because the non-local path computes window.origin+host.
	if got := conns[0]["host"]; got != "/" {
		t.Errorf("host = %v, want %q — a full URL would be concatenated onto the origin", got, "/")
	}
}

func TestRuntimeEnvPayloadIsValidJS(t *testing.T) {
	s := envServer(t)
	s.SetRuntimeEnv(map[string]string{"NEXT_PUBLIC_B": "2", "NEXT_PUBLIC_A": "1"})
	got := string(s.runtimeEnv)
	// Keys sorted, so the asset is byte-stable across restarts.
	want := "window['__ENV'] = {\"NEXT_PUBLIC_A\":\"1\",\"NEXT_PUBLIC_B\":\"2\"};\n"
	if got != want {
		t.Errorf("payload =\n%q\nwant\n%q", got, want)
	}
}

// TestRuntimeEnvEndToEnd pins that the hook is installed on the reverse proxy
// itself, not merely correct in isolation: a guest that 503s the path the way
// ClickHouse does must still hand the browser a usable script.
func TestRuntimeEnvEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == runtimeEnvPath {
			// What ClickHouse actually does with an unknown path.
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "guest app")
	}))
	t.Cleanup(upstream.Close)

	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	store, err := routes.Open(filepath.Join(t.TempDir(), "sparkbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Upsert(routes.Route{
		Subdomain: "box", Sandbox: "box", Owner: "alice", Port: port,
	}); err != nil {
		t.Fatal(err)
	}
	s := New(localManager(host), store, "hivemind.tools", slog.New(slog.NewTextHandler(io.Discard, nil)))

	get := func(path string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://box.hivemind.tools"+path, nil)
		req.Host = "box.hivemind.tools"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		body, _ := io.ReadAll(rec.Result().Body)
		return rec.Code, string(body)
	}

	code, body := get(runtimeEnvPath)
	if code != http.StatusOK {
		t.Fatalf("/__ENV.js answered %d, want 200 (the hook is not wired into the proxy)", code)
	}
	if !strings.Contains(body, "NEXT_PUBLIC_HDX_LOCAL_DEFAULT_CONNECTIONS") {
		t.Errorf("payload missing the connection seed: %q", body)
	}

	// The rest of the guest is untouched.
	if code, body := get("/anything-else"); code != http.StatusOK || body != "guest app" {
		t.Errorf("guest path answered %d %q, want 200 %q", code, body, "guest app")
	}
}
