package xterm

// Tests for the instrument strip's data source. The theme running through them
// is that reading a sandbox's vitals is a strictly passive act: it must not
// wake a sandbox, must not count as activity against the idle reaper, and must
// tell a stranger nothing a stranger could not already learn.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/edgeauth"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// fakeVitals is a VitalsReader over a fixed table. A name it does not hold
// answers the empty reading, which is exactly how the real *host.Manager
// behaves for a sandbox it does not run — so "no reading" needs no special case
// here, it is just an absent row. calls counts every read so a test can assert
// the reader was not consulted at all; err makes it fail the way a machine that
// has stopped answering does.
type fakeVitals struct {
	mu    sync.Mutex
	cpu   map[string]float64
	mem   map[string]int64
	net   map[string][2]uint64
	err   error
	calls int
}

func (f *fakeVitals) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeVitals) Vitals(_ context.Context, name string) (host.Vitals, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return host.Vitals{}, f.err
	}
	var v host.Vitals
	if secs, ok := f.cpu[name]; ok {
		v.CPUSeconds = &secs
	}
	if used, ok := f.mem[name]; ok {
		v.MemUsedMB = &used
	}
	if n, ok := f.net[name]; ok {
		rx, tx := n[0], n[1]
		v.NetRxBytes, v.NetTxBytes = &rx, &tx
	}
	return v, nil
}

// getJSON issues the request the page actually makes: no text/html in Accept,
// so an expired session is answered 401 rather than redirected to a login page
// the fetch could not follow anyway.
func (hz *harness) getJSON(t *testing.T, host, path, handle string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
	req.Host = host
	req.Header.Set("Accept", "*/*")
	if handle != "" {
		req.AddCookie(&http.Cookie{Name: edgeauth.CookieName, Value: hz.token(t, handle)})
	}
	rec := httptest.NewRecorder()
	hz.h.ServeHTTP(rec, req)
	return rec
}

// decode reads the body as a generic map, which is the only way to tell a
// field that was omitted from one that was sent as its zero value — the
// distinction the page relies on to know whether it has a reading at all.
func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return m
}

func withVitals(t *testing.T, fv *fakeVitals, boxes ...*host.Sandbox) *harness {
	t.Helper()
	hz := newHarness(t, boxes...)
	hz.h.vitalsOf = fv
	return hz
}

func runningBox(name, owner string) *host.Sandbox {
	b := newBox(name, owner, vmm.StateRunning)
	b.VCPUs, b.MemMB = 4, 8192
	b.NetRxBytes, b.NetTxBytes = 900, 300
	return b
}

func TestVitalsServesLiveCounters(t *testing.T) {
	fv := &fakeVitals{
		cpu: map[string]float64{"demo": 1234.5},
		mem: map[string]int64{"demo": 2048},
		net: map[string][2]uint64{"demo": {7000, 4000}},
	}
	hz := withVitals(t, fv, runningBox("demo", "alice"))

	rec := hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	// A cached reading would freeze the meters at whatever an intermediary last
	// saw, which looks exactly like a hung sandbox.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	m := decode(t, rec)
	for field, want := range map[string]any{
		"state":         "running",
		"vcpus":         float64(4),
		"mem_mb":        float64(8192),
		"cpu_seconds":   1234.5,
		"mem_used_mb":   float64(2048),
		"net_rx_bytes":  float64(7000),
		"net_tx_bytes":  float64(4000),
		"life_rx_bytes": float64(900),
		"life_tx_bytes": float64(300),
	} {
		if got := m[field]; got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
	// at_ms is the divisor the page derives every rate from; without it the
	// browser would have to time the interval on its own clock and fold request
	// latency into the numbers.
	if at, ok := m["at_ms"].(float64); !ok || at <= 0 {
		t.Errorf("at_ms = %v, want a positive timestamp", m["at_ms"])
	}
}

// The load-bearing test. A tab left open overnight polls this once a second;
// if any of it counted as activity, the terminal would keep resurrecting a
// sandbox the idle reaper is trying to put away — and the page's deliberate
// refusal to auto-reconnect after a pause would be pointless.
func TestVitalsNeverResumesOrTouches(t *testing.T) {
	fv := &fakeVitals{cpu: map[string]float64{"demo": 1}}
	hz := withVitals(t, fv, newBox("demo", "alice", vmm.StatePaused))

	for i := 0; i < 3; i++ {
		if rec := hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice"); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}
	resumes, touches := hz.mgr.counts()
	if resumes != 0 {
		t.Errorf("EnsureRunning called %d times; reading vitals must never wake a sandbox", resumes)
	}
	if touches != 0 {
		t.Errorf("Touch called %d times; watching a meter is not activity", touches)
	}
}

// Missing and not-yours are the same answer everywhere else in this package,
// and they have to be here too: a 200 with ceilings would confirm that a name
// exists and hand over its shape.
func TestVitalsCrossOwnerIs404AndReadsNothing(t *testing.T) {
	fv := &fakeVitals{
		cpu: map[string]float64{"demo": 1234.5},
		mem: map[string]int64{"demo": 2048},
	}
	hz := withVitals(t, fv, runningBox("demo", "alice"))

	rec := hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "mallory")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if n := fv.count(); n != 0 {
		t.Errorf("vitals reader consulted %d times for a sandbox the caller does not own", n)
	}
	// Operators get no bypass here for the same reason they get none on the
	// PTY: this is the owner's machine, and the strip is a view of their work.
	if rec := hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "opsy"); rec.Code != http.StatusNotFound {
		t.Errorf("operator status = %d, want 404", rec.Code)
	}
}

// A paused sandbox has no VMM process to read /proc for, no tap to count and no
// balloon to ask. The ceilings still go out so the page can keep the strip's
// layout instead of collapsing the header every time a box goes to sleep.
func TestVitalsPausedOmitsCounters(t *testing.T) {
	fv := &fakeVitals{
		cpu: map[string]float64{"demo": 1234.5},
		mem: map[string]int64{"demo": 2048},
		net: map[string][2]uint64{"demo": {7000, 4000}},
	}
	box := runningBox("demo", "alice")
	box.State = vmm.StatePaused
	hz := withVitals(t, fv, box)

	m := decode(t, hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice"))
	if m["state"] != string(vmm.StatePaused) {
		t.Errorf("state = %v, want %q", m["state"], vmm.StatePaused)
	}
	if m["vcpus"] != float64(4) || m["mem_mb"] != float64(8192) {
		t.Errorf("ceilings dropped for a paused sandbox: %v", m)
	}
	for _, absent := range []string{"cpu_seconds", "mem_used_mb", "net_rx_bytes", "net_tx_bytes"} {
		if _, present := m[absent]; present {
			t.Errorf("%s present for a paused sandbox; a stale counter reads as a live one", absent)
		}
	}
	if n := fv.count(); n != 0 {
		t.Errorf("driver read %d times for a sandbox that is not running", n)
	}
}

// Two cases with one shape: a build with no VitalsReader wired, and a sandbox
// this machine does not hold (the fleet case — a balloon and a VMM process can
// only be asked of the host running them). Both answer 200 with the ceilings
// and no readings, so the page has one code path for "no plot".
func TestVitalsWithoutReadingsStillReportsCeilings(t *testing.T) {
	for name, hz := range map[string]*harness{
		"reader not configured": newHarness(t, runningBox("demo", "alice")),
		"sandbox on another node": withVitals(t,
			&fakeVitals{cpu: map[string]float64{"elsewhere": 1}}, runningBox("demo", "alice")),
	} {
		t.Run(name, func(t *testing.T) {
			rec := hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			m := decode(t, rec)
			if m["state"] != string(vmm.StateRunning) || m["vcpus"] != float64(4) {
				t.Errorf("ceilings dropped: %v", m)
			}
			for _, absent := range []string{"cpu_seconds", "mem_used_mb", "net_rx_bytes"} {
				if _, present := m[absent]; present {
					t.Errorf("%s present with nothing to read it from", absent)
				}
			}
		})
	}
}

// The page stops polling on a 401 and lets the WebSocket's own recovery reload
// it, so this status is load-bearing rather than cosmetic: a redirect here
// would be followed by fetch into an HTML login page and parsed as garbage.
func TestVitalsUnauthenticatedIs401(t *testing.T) {
	hz := withVitals(t, &fakeVitals{}, runningBox("demo", "alice"))
	if rec := hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// The host names the sandbox, so a host this server does not answer for has no
// sandbox to report on — the same rule the page and the WebSocket apply.
func TestVitalsForeignHostIs404(t *testing.T) {
	hz := withVitals(t, &fakeVitals{}, runningBox("demo", "alice"))
	if rec := hz.getJSON(t, "demo-xterm.evil.example", "/vitals", "alice"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestVitalsSurviveAMachineThatIsNotAnswering is the fleet's failure mode seen
// from the page.
//
// A sandbox on another node is now asked of that node, which means the read can
// fail in a way a local one never could. When it does, the terminal must keep
// working: the header loses its meters and nothing else changes. A 500 here
// would be the terminal telling somebody their session is broken because a stat
// read timed out — and the page polls this once a second, so it would say so
// sixty times a minute.
func TestVitalsSurviveAMachineThatIsNotAnswering(t *testing.T) {
	fv := &fakeVitals{err: errors.New("node boxb is offline")}
	hz := withVitals(t, fv, runningBox("demo", "alice"))

	rec := hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unreachable machine is not a broken terminal", rec.Code)
	}
	got := decode(t, rec)
	if got["state"] != string(vmm.StateRunning) {
		t.Errorf("state = %v, want the record's own running", got["state"])
	}
	if _, ok := got["cpu_seconds"]; ok {
		t.Errorf("a failed read produced a cpu_seconds field: %v", got)
	}
	// The ceilings still ship, because they come from the gateway's own record
	// rather than from the machine that went quiet. It is what lets the page
	// draw the empty instrument at the right scale.
	if got["vcpus"] != float64(4) || got["mem_mb"] != float64(8192) {
		t.Errorf("ceilings = %v/%v, want the record's 4/8192", got["vcpus"], got["mem_mb"])
	}
}
