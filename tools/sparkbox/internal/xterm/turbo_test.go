package xterm

// The turbo endpoint's gates. Turbo restarts somebody's sandbox, so it is the
// only mutation this package has, and it has to clear the same three bars as
// the WebSocket does — owner, first-party intent, and the host actually having
// the capability wired — before the manager is touched at all.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// fakeTurbo records what it was asked for, so a test can prove a refused
// request never reached it rather than merely that the status was right.
type fakeTurbo struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeTurbo) SetTurbo(_ context.Context, name string, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+":"+map[bool]string{true: "on", false: "off"}[on])
	return nil
}

func (f *fakeTurbo) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// post issues POST /turbo. csrf controls the custom header the page sends;
// without it (and without a matching Origin) the request must be refused.
func (hz *harness) postTurbo(t *testing.T, host, handle string, on, csrf bool) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]bool{"on": on})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://"+host+"/turbo", strings.NewReader(string(body)))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	if csrf {
		req.Header.Set(edgeauth.MutationHeader, "1")
	}
	if handle != "" {
		req.AddCookie(&http.Cookie{Name: edgeauth.CookieName, Value: hz.token(t, handle)})
	}
	rec := httptest.NewRecorder()
	hz.h.ServeHTTP(rec, req)
	return rec
}

func TestTurboRequiresOwnerIntentAndCapability(t *testing.T) {
	const host = "demo-xterm." + testDomain

	// Unwired: the host serves no turbo, so the page is told so rather than
	// being handed a button that fails.
	hz := newHarness(t, newBox("demo", "alice", vmm.StateRunning))
	if rec := hz.postTurbo(t, host, "alice", true, true); rec.Code != http.StatusNotImplemented {
		t.Fatalf("turbo with no Turbocharger: status %d, want 501", rec.Code)
	}

	turbo := &fakeTurbo{}
	hz = newHarnessWith(t, turbo, newBox("demo", "alice", vmm.StateRunning))

	// A stranger gets the answer a missing sandbox gets, and the manager is
	// never asked — the ownership check strictly precedes it, exactly as it
	// does for the WebSocket.
	if rec := hz.postTurbo(t, host, "mallory", true, true); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner turbo: status %d, want 404", rec.Code)
	}
	// Operators do not get a bypass here either: turbo restarts an owner's
	// guest, and this package's rule is owner-only for everything past resolve.
	if rec := hz.postTurbo(t, host, "opsy", true, true); rec.Code != http.StatusNotFound {
		t.Fatalf("operator turbo: status %d, want 404", rec.Code)
	}
	// A same-site page with the owner's cookie but no proof of first-party
	// intent — the CSRF shape the session cookie alone cannot fence off.
	if rec := hz.postTurbo(t, host, "alice", true, false); rec.Code != http.StatusForbidden {
		t.Fatalf("turbo with no CSRF proof: status %d, want 403", rec.Code)
	}
	if got := turbo.recorded(); len(got) != 0 {
		t.Fatalf("a refused request reached the manager: %v", got)
	}

	if rec := hz.postTurbo(t, host, "alice", true, true); rec.Code != http.StatusOK {
		t.Fatalf("owner turbo: status %d (%s)", rec.Code, rec.Body)
	}
	if got := turbo.recorded(); len(got) != 1 || got[0] != "demo:on" {
		t.Fatalf("manager saw %v, want [demo:on]", got)
	}
}

// An Origin matching the host this page was served on is the other accepted
// proof, since that is what a real browser puts on a same-origin POST.
func TestTurboAcceptsItsOwnOrigin(t *testing.T) {
	const host = "demo-xterm." + testDomain
	turbo := &fakeTurbo{}
	hz := newHarnessWith(t, turbo, newBox("demo", "alice", vmm.StateRunning))

	req := httptest.NewRequest(http.MethodPost, "https://"+host+"/turbo", strings.NewReader(`{"on":false}`))
	req.Host = host
	req.Header.Set("Origin", "https://"+host)
	req.AddCookie(&http.Cookie{Name: edgeauth.CookieName, Value: hz.token(t, "alice")})
	rec := httptest.NewRecorder()
	hz.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin turbo: status %d (%s)", rec.Code, rec.Body)
	}

	// A sandbox's own web route is same-SITE with this page and carries the
	// same cookie, so its Origin must not be good enough.
	req = httptest.NewRequest(http.MethodPost, "https://"+host+"/turbo", strings.NewReader(`{"on":true}`))
	req.Host = host
	req.Header.Set("Origin", "https://demo."+testDomain)
	req.AddCookie(&http.Cookie{Name: edgeauth.CookieName, Value: hz.token(t, "alice")})
	rec = httptest.NewRecorder()
	hz.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("foreign-origin turbo: status %d, want 403", rec.Code)
	}
	if got := turbo.recorded(); len(got) != 1 || got[0] != "demo:off" {
		t.Fatalf("manager saw %v, want just the same-origin call", got)
	}
}

// The vitals poll is where the page learns whether to draw the switch at all,
// and whether its lamp should be lit — so both facts have to ride it.
func TestVitalsCarriesTurboState(t *testing.T) {
	const host = "demo-xterm." + testDomain
	box := newBox("demo", "alice", vmm.StateRunning)
	box.VCPUs, box.MemMB = 4, 16384
	box.Turbo, box.BaseVCPUs, box.BaseMemMB = true, 2, 8192

	hz := newHarness(t, box)
	var off vitals
	if err := json.Unmarshal(hz.get(t, host, "/vitals", "alice").Body.Bytes(), &off); err != nil {
		t.Fatal(err)
	}
	if off.TurboAvailable {
		t.Fatal("a host with no Turbocharger advertised turbo")
	}
	if !off.Turbo || off.VCPUs != 4 || off.MemMB != 16384 {
		t.Fatalf("vitals = %+v, want the sandbox's current doubled ceilings", off)
	}

	hz = newHarnessWith(t, &fakeTurbo{}, box)
	var on vitals
	if err := json.Unmarshal(hz.get(t, host, "/vitals", "alice").Body.Bytes(), &on); err != nil {
		t.Fatal(err)
	}
	if !on.TurboAvailable {
		t.Fatal("a host with a Turbocharger did not advertise turbo")
	}
}
