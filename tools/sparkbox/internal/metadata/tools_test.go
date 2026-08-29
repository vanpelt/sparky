package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudeBody and browserBody stand in for the two shapes the refresher caches:
// a bare binary and a bundle tarball. Their exact bytes are what the download
// test compares, so they must not be all one value.
var (
	claudeBody  = []byte("#!/bin/sh\necho claude 2.1.2\n")
	browserBody = bytes.Repeat([]byte("agent-browser tarball\n"), 64)
)

// toolCacheDir builds a directory in the shape refresh-agent-tools.sh leaves
// behind: the artifacts, plus a manifest.json describing them. The manifest is
// marshalled from the real wire types, so a test that reads it back is also
// asserting the producer and consumer agree about the encoding.
func toolCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeArtifact(t, dir, "claude-2.1.2-linux-x64", claudeBody)
	writeArtifact(t, dir, "agent-browser-1.4.0.tgz", browserBody)
	writeToolManifest(t, dir, ToolManifest{
		Arch: "x86_64", Rev: "claude=2.1.2 agentbrowser=1.4.0",
		GeneratedAt: "2026-08-29T00:00:00Z",
		Tools: []ToolEntry{
			{
				Name: "claude", Key: "claude", Version: "2.1.2",
				File: "claude-2.1.2-linux-x64", SHA256: strings.Repeat("a", 64),
				Size: int64(len(claudeBody)), Kind: "binary", Bin: "/usr/local/bin/claude",
			},
			{
				Name: "agent-browser", Key: "agentbrowser", Version: "1.4.0",
				File: "agent-browser-1.4.0.tgz", SHA256: strings.Repeat("b", 64),
				Size: int64(len(browserBody)), Kind: "bundle",
				Bin: "/usr/local/bin/agent-browser", Dir: "/usr/local/lib/agent-browser",
				Exec: "bin/agent-browser-linux-x64",
				Link: "../lib/agent-browser/bin/agent-browser-linux-x64",
			},
		},
	})
	return dir
}

func writeArtifact(t *testing.T, dir, name string, body []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeToolManifest(t *testing.T, dir string, m ToolManifest) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeArtifact(t, dir, "manifest.json", raw)
}

// guestRequest stamps the accepted connection's local address onto a request
// the way ConnContext does in ListenAndServe, for the cases request() cannot
// build because they carry a header.
func guestRequest(r *http.Request, dst string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), localAddrKey{},
		&net.TCPAddr{IP: net.ParseIP(dst), Port: DefaultPort}))
}

// withTools is fixture plus a tool cache, over the same two sandboxes (alice in
// slot 5, bob in slot 9) and the same working issuer the token tests use — the
// budget test below needs /token to actually mint.
func withTools(t *testing.T, cache ToolCache) *Server {
	t.Helper()
	s := fixture(t)
	s.tools = cache
	return s
}

func TestToolManifestIsServedToTheCallingSandbox(t *testing.T) {
	s := withTools(t, &LocalTools{Dir: toolCacheDir(t)})

	rec := request(s, "/tools/manifest", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tools/manifest = %d: %s", rec.Code, rec.Body)
	}
	var got ToolManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("manifest = %+v", got.Tools)
	}
	if got.Tools[0].Name != "claude" || got.Tools[0].Size != int64(len(claudeBody)) {
		t.Errorf("claude entry = %+v", got.Tools[0])
	}
	if got.Tools[1].Kind != "bundle" || got.Tools[1].Link == "" {
		t.Errorf("agent-browser must carry its bundle layout: %+v", got.Tools[1])
	}
	// The guest parses this with tr+awk and no JSON library, and its pair-walk
	// only recognises `"key": "value"` — an unquoted number reads back empty and
	// costs it the length check that tells a truncated download from a tampered
	// one. So size must be QUOTED on the wire, not merely decodable.
	if want := fmt.Sprintf(`"size":"%d"`, len(claudeBody)); !strings.Contains(rec.Body.String(), want) {
		t.Errorf("size must be a quoted string on the wire (%s):\n%s", want, rec.Body)
	}
	if store := rec.Header().Get("Cache-Control"); store != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", store)
	}
}

// A host built without --tools-dir keeps minting tokens and simply has no tool
// cache. 501 is the status the guest installer reads as "this host does not
// serve one" and stops on, rather than retrying forever.
func TestToolRoutesAreNotImplementedWithoutACache(t *testing.T) {
	s := fixture(t)
	for _, path := range []string{"/tools/manifest", "/tools/claude"} {
		if rec := request(s, path, "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusNotImplemented {
			t.Errorf("GET %s with no cache = %d, want 501: %s", path, rec.Code, rec.Body)
		}
	}
	// And the rest of the port is unaffected.
	if rec := request(s, "/token", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
		t.Errorf("/token = %d, want 200 — the tool routes must not perturb the mint", rec.Code)
	}
}

// The failure this must never produce is an empty 200. To the guest an empty
// tool list reads as "you are already current", which is exactly the lie
// refresh-agent-tools.sh rewrote itself to stop telling.
func TestAnUnpublishedCacheIsAnErrorNotAnEmptyList(t *testing.T) {
	s := withTools(t, &LocalTools{Dir: t.TempDir()})
	rec := request(s, "/tools/manifest", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("GET /tools/manifest over an empty dir = %d, want 501: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"tools":[]`) {
		t.Errorf("an empty tool list must never be served: %s", rec.Body)
	}

	// Same rule when the document exists but names nothing this host holds.
	dir := t.TempDir()
	writeToolManifest(t, dir, ToolManifest{Arch: "x86_64", Rev: "claude=2.1.2", Tools: []ToolEntry{{
		Name: "claude", Key: "claude", Version: "2.1.2", File: "claude-2.1.2-linux-x64",
		SHA256: strings.Repeat("a", 64), Size: 12, Kind: "binary", Bin: "/usr/local/bin/claude",
	}}})
	rec = request(withTools(t, &LocalTools{Dir: dir}), "/tools/manifest", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("manifest naming no artifact this host has = %d, want 500: %s", rec.Code, rec.Body)
	}
}

// One artifact half-way through a refresh must not take the other four with it:
// a missing or short file drops that entry and the rest stay installable.
func TestEntriesWithoutTheirArtifactAreDropped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, dir string)
	}{
		{"missing", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "agent-browser-1.4.0.tgz")); err != nil {
				t.Fatal(err)
			}
		}},
		{"short", func(t *testing.T, dir string) {
			writeArtifact(t, dir, "agent-browser-1.4.0.tgz", browserBody[:len(browserBody)/2])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := toolCacheDir(t)
			tc.prepare(t, dir)
			s := withTools(t, &LocalTools{Dir: dir})

			rec := request(s, "/tools/manifest", "172.30.5.2", "172.30.5.1")
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /tools/manifest = %d: %s", rec.Code, rec.Body)
			}
			var got ToolManifest
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Tools) != 1 || got.Tools[0].Name != "claude" {
				t.Fatalf("manifest = %+v, want claude alone", got.Tools)
			}
			// And the dropped one is not quietly downloadable either.
			if rec := request(s, "/tools/agent-browser", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusNotFound {
				t.Errorf("GET /tools/agent-browser = %d, want 404: %s", rec.Code, rec.Body)
			}
		})
	}
}

// A malformed entry is a different severity from a missing file: it means the
// producer and this reader disagree about the contract, so the WHOLE document
// is refused rather than the half of it that happened to parse.
func TestAMalformedEntryRefusesTheWholeManifest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(e *ToolEntry)
	}{
		{"traversal", func(e *ToolEntry) { e.File = "../../etc/passwd" }},
		{"absolute", func(e *ToolEntry) { e.File = "/etc/passwd" }},
		{"digest", func(e *ToolEntry) { e.SHA256 = "nope" }},
		{"size", func(e *ToolEntry) { e.Size = 0 }},
		{"kind", func(e *ToolEntry) { e.Kind = "script" }},
		{"relative install path", func(e *ToolEntry) { e.Bin = "claude" }},
		{"name is the route", func(e *ToolEntry) { e.Name = "manifest" }},
		{"name is a path", func(e *ToolEntry) { e.Name = "../claude" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := toolCacheDir(t)
			var m ToolManifest
			raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&m.Tools[1])
			writeToolManifest(t, dir, m)
			s := withTools(t, &LocalTools{Dir: dir})

			if rec := request(s, "/tools/manifest", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusInternalServerError {
				t.Errorf("GET /tools/manifest = %d, want 500: %s", rec.Code, rec.Body)
			}
			// Including the entry that was fine: a document this reader does not
			// understand serves nothing at all.
			if rec := request(s, "/tools/claude", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusInternalServerError {
				t.Errorf("GET /tools/claude = %d, want 500: %s", rec.Code, rec.Body)
			}
		})
	}
}

// A bundle whose layout is incomplete is the pi/agent-browser breakage the
// layout fields exist to prevent, so it is refused at the manifest and never
// reaches a guest that would install an executable with no skill data beside it.
func TestABundleMustNameItsDirectoryExecutableAndLink(t *testing.T) {
	for _, drop := range []string{"dir", "exec", "link"} {
		t.Run(drop, func(t *testing.T) {
			dir := toolCacheDir(t)
			var m ToolManifest
			raw, _ := os.ReadFile(filepath.Join(dir, "manifest.json")) //nolint:errcheck
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			switch drop {
			case "dir":
				m.Tools[1].Dir = ""
			case "exec":
				m.Tools[1].Exec = ""
			case "link":
				m.Tools[1].Link = ""
			}
			writeToolManifest(t, dir, m)
			s := withTools(t, &LocalTools{Dir: dir})
			if rec := request(s, "/tools/manifest", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusInternalServerError {
				t.Errorf("bundle missing %s = %d, want 500: %s", drop, rec.Code, rec.Body)
			}
		})
	}
}

func TestToolBodyIsServedByteForByte(t *testing.T) {
	s := withTools(t, &LocalTools{Dir: toolCacheDir(t)})

	// Every request in this package runs through httptest.ResponseRecorder,
	// which answers http.ErrNotSupported to SetWriteDeadline. If toolFile ever
	// stops ignoring that error, this test is the one that turns 500.
	rec := request(s, "/tools/agent-browser", "172.30.5.2", "172.30.5.1")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tools/agent-browser = %d: %s", rec.Code, rec.Body)
	}
	if !bytes.Equal(rec.Body.Bytes(), browserBody) {
		t.Errorf("body = %d bytes, want the artifact's %d", rec.Body.Len(), len(browserBody))
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len(browserBody)) {
		t.Errorf("Content-Length = %q, want %d — the guest checks the length before the digest", got, len(browserBody))
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// A 92MB pull over a metered tap does die part-way. ServeContent's Range
// support is what lets the guest resume instead of spending the whole tarball
// again.
func TestARangeRequestResumesADownload(t *testing.T) {
	s := withTools(t, &LocalTools{Dir: toolCacheDir(t)})
	r := httptest.NewRequest(http.MethodGet, "/tools/agent-browser", nil)
	r.Header.Set("Range", "bytes=100-199")
	r.RemoteAddr = "172.30.5.2:40000"
	r = guestRequest(r, "172.30.5.1")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("ranged GET = %d, want 206: %s", rec.Code, rec.Body)
	}
	if !bytes.Equal(rec.Body.Bytes(), browserBody[100:200]) {
		t.Errorf("ranged body = %q", rec.Body.String())
	}
}

func TestUnknownToolsAndTraversalsAre404(t *testing.T) {
	s := withTools(t, &LocalTools{Dir: toolCacheDir(t)})
	for _, path := range []string{
		"/tools/codex",                  // not in this manifest
		"/tools/manifest.json",          // the file itself is not a tool
		"/tools/%2e%2e%2fmanifest.json", // percent-encoded traversal
		"/tools/..%2f..%2fetc%2fpasswd",
		"/tools/claude%00",
	} {
		rec := request(s, path, "172.30.5.2", "172.30.5.1")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404: %s", path, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "root:") {
			t.Fatalf("GET %s served /etc/passwd", path)
		}
	}
}

// The same caller gate as every other route on this port: source must be a
// guest, and destination must be that guest's own /30 host address. See the
// package comment for why source is the identity and destination is only a
// cross-slot refusal.
func TestToolRoutesRefuseNonGuestsAndCrossSlotDestinations(t *testing.T) {
	s := withTools(t, &LocalTools{Dir: toolCacheDir(t)})
	for _, tc := range []struct{ what, src, dst string }{
		{"public source", "203.0.113.7", "172.30.5.1"},
		{"cross-slot destination", "172.30.5.2", "172.30.9.1"},
	} {
		for _, path := range []string{"/tools/manifest", "/tools/claude"} {
			if rec := request(s, path, tc.src, tc.dst); rec.Code != http.StatusForbidden {
				t.Errorf("%s GET %s = %d, want 403: %s", tc.what, path, rec.Code, rec.Body)
			}
		}
	}
}

// The tool pull is bulk guest-initiated traffic like a clone, so it takes the
// repo window and not the mint's. A box refreshing its CLIs must not lose its
// identity over it — sparkbox-token.service carries StartLimitBurst=10/300s.
func TestToolBudgetIsSeparateFromTheTokenBudget(t *testing.T) {
	s := withTools(t, &LocalTools{Dir: toolCacheDir(t)})
	for i := 0; i < credBurst; i++ {
		if rec := request(s, "/tools/manifest", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
			t.Fatalf("manifest %d = %d: %s", i, rec.Code, rec.Body)
		}
	}
	if rec := request(s, "/tools/manifest", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("over-budget manifest = %d, want 429", rec.Code)
	}
	// Downloads share the key with the manifest — one pull is one listing plus
	// five bodies, and they are the same class of traffic.
	if rec := request(s, "/tools/claude", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("over-budget download = %d, want 429", rec.Code)
	}
	if rec := request(s, "/token", "172.30.5.2", "172.30.5.1"); rec.Code != http.StatusOK {
		t.Errorf("/token after a tool pull = %d, want 200 — the two windows must not be one", rec.Code)
	}
	// Per sandbox, like the mint's and the credential's.
	if rec := request(s, "/tools/manifest", "172.30.9.2", "172.30.9.1"); rec.Code != http.StatusOK {
		t.Errorf("bob = %d, want 200 (the limit must be per-sandbox)", rec.Code)
	}
}

// A fleet-wide update is N guests asking within seconds of each other, so the
// document is parsed once and re-read only when the refresher renames a new one
// into place — which it detects by (size, mtime), the two things a rename moves.
func TestTheManifestIsReparsedOnlyWhenTheFileMoves(t *testing.T) {
	dir := toolCacheDir(t)
	cache := &LocalTools{Dir: dir}
	path := filepath.Join(dir, "manifest.json")

	first, err := cache.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if first.Rev == "" {
		t.Fatal("manifest carries no rev")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Rewrite the document to exactly the same length and put its timestamps
	// back: neither number moved, so the memo must still be believed.
	rewritten := bytes.Replace(mustRead(t, path),
		[]byte("claude=2.1.2 agentbrowser=1.4.0"), []byte("claude=2.1.9 agentbrowser=1.4.0"), 1)
	if len(rewritten) != int(st.Size()) {
		t.Fatalf("the stale-read probe must not change the file's length (%d -> %d)", st.Size(), len(rewritten))
	}
	writeArtifact(t, dir, "manifest.json", rewritten)
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	again, err := cache.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if again.Rev != first.Rev {
		t.Errorf("rev = %q after an in-place rewrite, want the memoised %q", again.Rev, first.Rev)
	}

	// Move the mtime the way the refresher's rename does, and the new document
	// is picked up.
	if err := os.Chtimes(path, st.ModTime().Add(time.Second), st.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	moved, err := cache.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(moved.Rev, "claude=2.1.9") {
		t.Errorf("rev = %q after the mtime moved, want the rewritten one", moved.Rev)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
