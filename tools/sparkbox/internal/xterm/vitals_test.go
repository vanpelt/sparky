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
	"strings"
	"sync"
	"testing"
	"time"

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
	mu           sync.Mutex
	cpu          map[string]float64
	mem          map[string]int64
	net          map[string][2]uint64
	ports        map[string][]int
	services     map[string][]host.PortService
	portsChecked map[string]bool
	err          error
	calls        int
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
	if ports, ok := f.ports[name]; ok {
		v.ListeningPorts = append([]int(nil), ports...)
	}
	if services, ok := f.services[name]; ok {
		v.PortServices = append([]host.PortService(nil), services...)
	}
	v.PortsChecked = f.portsChecked[name]
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

func TestVitalsCarriesListeningPortsAndCurrentDefault(t *testing.T) {
	fv := &fakeVitals{
		ports:        map[string][]int{"demo": {3000, 8000}},
		services:     map[string][]host.PortService{"demo": {{Port: 3000, Name: "Vite"}, {Port: 8000, Name: "JSON API"}}},
		portsChecked: map[string]bool{"demo": true},
	}
	hz := withVitals(t, fv, runningBox("demo", "alice"))
	hz.h.proxyPort = func(string) (int, bool) { return 3000, true }

	m := decode(t, hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice"))
	if m["proxy_port"] != float64(3000) || m["ports_checked"] != true {
		t.Fatalf("proxy availability = %v, want port 3000 checked", m)
	}
	ports, ok := m["listening_ports"].([]any)
	if !ok || len(ports) != 2 || ports[0] != float64(3000) || ports[1] != float64(8000) {
		t.Fatalf("listening_ports = %v, want [3000 8000]", m["listening_ports"])
	}
	services, ok := m["port_services"].([]any)
	if !ok || len(services) != 2 || services[1].(map[string]any)["name"] != "JSON API" {
		t.Fatalf("port_services = %v, want named metadata", m["port_services"])
	}
}

func TestVitalsDistinguishesCheckedWithNoListeners(t *testing.T) {
	fv := &fakeVitals{portsChecked: map[string]bool{"demo": true}}
	hz := withVitals(t, fv, runningBox("demo", "alice"))
	m := decode(t, hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice"))
	if m["ports_checked"] != true {
		t.Fatalf("ports_checked = %v, want true", m["ports_checked"])
	}
	if _, present := m["listening_ports"]; present {
		t.Fatalf("empty listening_ports should be omitted: %v", m)
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

// ---------------------------------------------------------------------------
// What the page cannot work out for itself
// ---------------------------------------------------------------------------

// The name, the ssh line, the proxy link and the console link ride this poll
// because the page has no other way to learn any of them.
//
// The name is the one that was actually wrong: the host is
// `<name>-<subdomain>.<zone>` — one label — so the page's
// `hostname.split(".")[0]` yields "demo-xterm", which is what its header and
// its turbo dialog used to say. The subdomain is configurable, so there is no
// suffix a page can safely strip either, which is why this is a field and not a
// client-side fix.
func TestVitalsCarriesTheNameSSHAndConsole(t *testing.T) {
	hz := newHarness(t, runningBox("demo", "alice"))
	hz.h.sshCommand = sshCommand("catnip.sh", 22)
	hz.h.consoleURL = "https://my.catnip.sh/"

	m := decode(t, hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice"))
	for field, want := range map[string]any{
		"name":    "demo",
		"ssh":     "ssh demo@catnip.sh",
		"proxy":   "https://demo.hivemind.tools/",
		"console": "https://my.catnip.sh/",
	} {
		if got := m[field]; got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
	if got, _ := m["name"].(string); strings.Contains(got, "-xterm") {
		t.Errorf("name = %q — that is the host label, not the sandbox", got)
	}
}

func TestVitalsCarriesRepositoryStateMap(t *testing.T) {
	box := runningBox("demo", "alice")
	box.RepoStatusAt = time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	box.Repos = []host.RepoStatus{{
		Slug: "wandb/agentstream", Path: "/home/sparky/src/wandb/agentstream",
		Branch: "feat/x", Upstream: "origin/feat/x", Ahead: 2, Dirty: true, State: "stale",
	}}
	hz := newHarness(t, box)

	m := decode(t, hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice"))
	repositories, ok := m["repositories"].(map[string]any)
	if !ok {
		t.Fatalf("repositories = %#v, want an object keyed by slug", m["repositories"])
	}
	repo, ok := repositories["wandb/agentstream"].(map[string]any)
	if !ok || repo["ahead"] != float64(2) || repo["dirty"] != true {
		t.Errorf("repository state = %#v", repositories["wandb/agentstream"])
	}
	if m["repo_status_at"] != "2026-09-01T12:00:00Z" {
		t.Errorf("repo_status_at = %v", m["repo_status_at"])
	}
}

// A host that advertises no SSH endpoint and runs no console omits both fields
// rather than sending empty strings. The page keys its menu rows off their
// presence, so an empty string would render "Copy the ssh command" over a blank
// line and a link to nowhere.
func TestVitalsOmitsWhatThisHostDoesNotHave(t *testing.T) {
	hz := newHarness(t, runningBox("demo", "alice"))

	m := decode(t, hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice"))
	for _, field := range []string{"ssh", "console"} {
		if _, present := m[field]; present {
			t.Errorf("%s was sent as %v on a host that has none", field, m[field])
		}
	}
	// The name is not optional: every host knows it.
	if m["name"] != "demo" {
		t.Errorf("name = %v, want demo", m["name"])
	}
}

func TestVitalsCarriesTheMostRecentHiveMindSession(t *testing.T) {
	base := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	box := runningBox("demo", "alice")
	box.HiveMind = &host.HiveMindSessionSnapshot{Sessions: []host.HiveMindSession{
		{Title: "Older work", URL: "https://hivemind.example/sessions/old", StartedAt: base, LastActivityAt: base.Add(time.Minute)},
		{Title: "  Fix terminal chrome  ", URL: "https://hivemind.example/sessions/new", StartedAt: base.Add(time.Minute), LastActivityAt: base.Add(2 * time.Minute)},
	}}
	hz := newHarness(t, box)

	m := decode(t, hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice"))
	if got := m["hivemind_session_title"]; got != "Fix terminal chrome" {
		t.Errorf("hivemind_session_title = %v, want newest session title", got)
	}
	if got := m["hivemind_session_url"]; got != "https://hivemind.example/sessions/new" {
		t.Errorf("hivemind_session_url = %v, want newest session URL", got)
	}
}

func TestVitalsOmitsUnsafeHiveMindSessionLinks(t *testing.T) {
	box := runningBox("demo", "alice")
	box.HiveMind = &host.HiveMindSessionSnapshot{Sessions: []host.HiveMindSession{{
		Title: "Not a dashboard", URL: "javascript:alert(1)", LastActivityAt: time.Now(),
	}}}
	hz := newHarness(t, box)

	m := decode(t, hz.getJSON(t, "demo-xterm."+testDomain, "/vitals", "alice"))
	for _, field := range []string{"hivemind_session_title", "hivemind_session_url"} {
		if value, present := m[field]; present {
			t.Errorf("unsafe session produced %s=%v", field, value)
		}
	}
}

func TestSandboxProxyURL(t *testing.T) {
	for _, tc := range []struct {
		domain string
		want   string
	}{
		{"Catnip.SH.", "https://demo.catnip.sh/"},
		{"", ""},
	} {
		build := sandboxProxyURL(tc.domain)
		got := ""
		if build != nil {
			got = build("demo")
		}
		if got != tc.want {
			t.Errorf("sandboxProxyURL(%q) = %q, want %q", tc.domain, got, tc.want)
		}
	}
}

// The command has to be the one a person can paste. That means the ADVERTISED
// port — the one an edge DNAT sends to the gateway, not the one it binds — and
// no `-p` at all when it is the port ssh would have used anyway.
func TestSSHCommandSpellsTheAdvertisedEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		port int
		want string
	}{
		{"the default port is left unsaid", "catnip.sh", 22, "ssh demo@catnip.sh"},
		{"an unparsed port is not a port to type", "catnip.sh", 0, "ssh demo@catnip.sh"},
		{"anything else is spelled out", "catnip.sh", 2222, "ssh -p 2222 demo@catnip.sh"},
		{"a gateway on its own hostname", "gw.catnip.sh", 22, "ssh demo@gw.catnip.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := sshCommand(tc.host, tc.port)
			if cmd == nil {
				t.Fatal("no command for a configured host")
			}
			if got := cmd("demo"); got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
	// No host is not a command with a hole in it. A `ssh demo@` line pasted
	// into a terminal is worse than no row at all.
	if sshCommand("", 22) != nil {
		t.Error("a host with no advertised SSH endpoint still produced a command")
	}
}
