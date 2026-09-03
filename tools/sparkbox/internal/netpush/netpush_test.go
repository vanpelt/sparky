package netpush

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestTapName(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"172.30.5.2":  {"sbtap320", true},
		"172.30.0.2":  {"sbtap0", true},
		"172.30.5.1":  {"", false}, // gateway, not guest
		"127.0.0.1":   {"", false}, // mock driver
		"":            {"", false}, // paused
		"10.0.0.2":    {"", false},
		"172.30.5.2.": {"", false},
	}
	for in, want := range cases {
		got, ok := TapName(in)
		if got != want.want || ok != want.ok {
			t.Errorf("TapName(%q) = (%q,%v), want (%q,%v)", in, got, ok, want.want, want.ok)
		}
	}
}

func TestTapNameForConfiguredSubnet(t *testing.T) {
	tests := []struct {
		subnet, addr string
		want         string
		ok           bool
	}{
		{"10.44.16.0/20", "10.44.16.2", "sbtap0", true},
		{"10.44.16.0/20", "10.44.17.6", "sbtap65", true},
		{"10.44.16.0/20", "10.44.17.5", "", false},
		{"10.44.16.0/20", "172.30.0.2", "", false},
		{"bad", "10.44.16.2", "", false},
	}
	for _, test := range tests {
		got, ok := TapNameForSubnet(test.subnet, test.addr)
		if got != test.want || ok != test.ok {
			t.Errorf("TapNameForSubnet(%q, %q) = (%q,%v), want (%q,%v)",
				test.subnet, test.addr, got, ok, test.want, test.ok)
		}
	}
}

func TestSyncerUsesConfiguredSubnet(t *testing.T) {
	var got policyBody
	client := unixSluice(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got) //nolint:errcheck
		w.WriteHeader(http.StatusNoContent)
	}))
	syncer, err := NewSyncerForSubnet(
		client,
		fakeFleet{boxes: []Sandbox{{Name: "ci-box", Owner: "alice", HostIP: "10.44.17.6"}}},
		fakeRules{"alice/ci-box": {"github.com"}},
		"10.44.16.9/20",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"sbtap65": {"github.com"}}
	if !reflect.DeepEqual(got.Taps, want) {
		t.Fatalf("configured-subnet taps = %#v, want %#v", got.Taps, want)
	}
}

type fakeFleet struct{ boxes []Sandbox }

func (f fakeFleet) List() []Sandbox { return f.boxes }

type fakeRules map[string][]string // "owner/sandbox" -> allow

// A sandbox is governed iff it has an entry here — even an empty slice (a
// deliberate deny-all). Absent keys are ungoverned and Push must omit them.
func (r fakeRules) AllowForSandbox(sandbox, owner string) ([]string, bool, error) {
	allow, ok := r[owner+"/"+sandbox]
	return allow, ok, nil
}

// unixSluice starts an httptest-style server bound to a Unix socket and returns
// a Client for it plus a handle to the last policy body pushed.
func unixSluice(t *testing.T, h http.Handler) *Client {
	t.Helper()
	// A short base dir: macOS caps unix socket paths at ~104 bytes, and
	// t.TempDir() bakes the (long) test name into the path.
	dir, err := os.MkdirTemp("", "sl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln) //nolint:errcheck
	t.Cleanup(func() { srv.Close() })
	return NewClient(sock)
}

func TestPushSendsFullFleetPolicy(t *testing.T) {
	var mu sync.Mutex
	var got policyBody
	client := unixSluice(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/policy" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		mu.Lock()
		json.NewDecoder(r.Body).Decode(&got) //nolint:errcheck
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))

	fleet := fakeFleet{boxes: []Sandbox{
		{Name: "ci-box", Owner: "alice", HostIP: "172.30.5.2"},
		{Name: "gpu-box", Owner: "alice", HostIP: "172.30.6.2"},
		{Name: "paused", Owner: "bob", HostIP: ""}, // no tap -> skipped
	}}
	rules := fakeRules{
		"alice/ci-box":  {"github.com"},
		"alice/gpu-box": {"huggingface.co"},
	}
	s := NewSyncer(client, fleet, rules, nil)
	if err := s.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string][]string{
		"sbtap320": {"github.com"},
		"sbtap384": {"huggingface.co"},
	}
	if !reflect.DeepEqual(got.Taps, want) {
		t.Errorf("pushed taps = %v, want %v", got.Taps, want)
	}
}

func TestPushOmitsUngovernedButSendsGovernedDenyAll(t *testing.T) {
	var mu sync.Mutex
	var got policyBody
	client := unixSluice(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		json.NewDecoder(r.Body).Decode(&got) //nolint:errcheck
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	fleet := fakeFleet{boxes: []Sandbox{
		{Name: "ci-box", Owner: "alice", HostIP: "172.30.5.2"},   // governed, has rules
		{Name: "locked", Owner: "alice", HostIP: "172.30.6.2"},   // governed deny-all
		{Name: "untagged", Owner: "alice", HostIP: "172.30.7.2"}, // ungoverned -> omitted
	}}
	rules := fakeRules{
		"alice/ci-box": {"github.com"},
		"alice/locked": {}, // present but empty: governed deny-all
	}
	s := NewSyncer(client, fleet, rules, nil)
	if err := s.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string][]string{
		"sbtap320": {"github.com"},
		"sbtap384": {}, // deny-all still pushed, so sluice enforces (drops) it
	}
	if !reflect.DeepEqual(got.Taps, want) {
		t.Errorf("pushed taps = %#v, want %#v (untagged VM must be omitted)", got.Taps, want)
	}
	if _, ok := got.Taps["sbtap448"]; ok {
		t.Error("ungoverned VM sbtap448 must not appear in the policy")
	}
}

func TestUsageAttributesTapsToVMsAndFiltersByOwner(t *testing.T) {
	rep := Report{Taps: []TapUsage{
		{Tap: "sbtap320", TxBytes: 100, RxBytes: 900, Domains: []DomainUsage{
			{Domain: "github.com", TxBytes: 10, RxBytes: 90},
			{Domain: "youtube.com", TxBytes: 90, RxBytes: 810},
		}},
		{Tap: "sbtap576", TxBytes: 5, RxBytes: 5, Domains: nil}, // bob's box
		{Tap: "sbtap42", TxBytes: 1, RxBytes: 1},                // no current VM
	}}
	client := unixSluice(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(rep) //nolint:errcheck
	}))
	fleet := fakeFleet{boxes: []Sandbox{
		{Name: "ci-box", Owner: "alice", HostIP: "172.30.5.2"},
		{Name: "bob-box", Owner: "bob", HostIP: "172.30.9.2"},
	}}
	s := NewSyncer(client, fleet, fakeRules{}, nil)

	usage, err := s.Usage(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 {
		t.Fatalf("owner filter failed: %v", usage)
	}
	u, ok := usage["ci-box"]
	if !ok {
		t.Fatalf("ci-box missing: %v", usage)
	}
	// Domains sorted by total bytes desc: youtube (900) before github (100).
	if len(u.Domains) != 2 || u.Domains[0].Domain != "youtube.com" {
		t.Errorf("domains not sorted desc: %+v", u.Domains)
	}
}

func TestDenialCaptureUsesTapAndImmutableSandboxID(t *testing.T) {
	var paths []string
	client := unixSluice(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if body["capture_id"] != "box-immutable-id" {
			t.Errorf("capture_id = %q", body["capture_id"])
		}
		if strings.HasSuffix(r.URL.Path, "/start") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode(DenialCapture{CaptureID: body["capture_id"], Domains: []DeniedDomain{
			{Domain: "registry.npmjs.org", Queries: 2, QTypes: []string{"A", "AAAA"}},
		}}) //nolint:errcheck
	}))
	s := NewSyncer(client, fakeFleet{boxes: []Sandbox{
		{ID: "box-immutable-id", Name: "web-build", Owner: "alice", HostIP: "172.30.5.2"},
	}}, fakeRules{}, nil)
	if err := s.StartDenialCapture(context.Background(), "web-build"); err != nil {
		t.Fatal(err)
	}
	got, err := s.FinishDenialCapture(context.Background(), "web-build")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Domains) != 1 || got.Domains[0].Domain != "registry.npmjs.org" {
		t.Fatalf("capture = %+v", got)
	}
	want := []string{"/denials/sbtap320/start", "/denials/sbtap320/finish"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestNilClientIsNoOp(t *testing.T) {
	s := NewSyncer(nil, fakeFleet{}, fakeRules{}, nil)
	if s.Enabled() {
		t.Error("nil client should report disabled")
	}
	if err := s.Push(context.Background()); err != nil {
		t.Errorf("Push no-op errored: %v", err)
	}
	u, err := s.Usage(context.Background(), "alice")
	if err != nil || len(u) != 0 {
		t.Errorf("Usage no-op = %v, %v", u, err)
	}
}

// ---------------------------------------------------------------------------
// The Resolve/Apply split
// ---------------------------------------------------------------------------

// Resolve is the half a GATEWAY runs for a sandbox it does not hold: it must
// answer in names, because tap indices are assigned per machine and sbtap320 on
// one is a different person's VM on another.
func TestResolveKeysByNameAndKeepsTheGovernedDistinction(t *testing.T) {
	fleet := fakeFleet{boxes: []Sandbox{
		{Name: "ci-box", Owner: "alice", HostIP: "172.30.5.2"},
		{Name: "vault", Owner: "alice", HostIP: "172.30.6.2"},
		{Name: "free", Owner: "bob", HostIP: "172.30.7.2"},
		// Paused: no tap here, but the gateway resolving for another machine
		// cannot see taps at all, so this must still resolve on its rules.
		{Name: "resting", Owner: "alice", HostIP: ""},
	}}
	rules := fakeRules{
		"alice/ci-box":  {"github.com"},
		"alice/vault":   nil, // governed deny-all
		"alice/resting": {"pypi.org"},
		// bob/free absent: ungoverned
	}
	got := NewSyncer(nil, fleet, rules, nil).Resolve()
	want := map[string][]string{
		"ci-box":  {"github.com"},
		"vault":   {},
		"resting": {"pypi.org"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve() = %v, want %v", got, want)
	}
	if _, present := got["free"]; present {
		t.Error("an ungoverned sandbox appeared in the resolved set; it must be omitted so it stays unrestricted")
	}
}

// Apply is the half a NODE runs on what arrives: names in, its own taps out.
func TestApplyResolvesNamesToThisMachinesOwnTaps(t *testing.T) {
	var mu sync.Mutex
	var got policyBody
	client := unixSluice(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		json.NewDecoder(r.Body).Decode(&got) //nolint:errcheck
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))

	// This machine's slot assignment is its own: the gateway that sent these
	// names has no idea which index any of them landed in.
	fleet := fakeFleet{boxes: []Sandbox{
		{Name: "ci-box", Owner: "alice", HostIP: "172.30.9.2"},
		{Name: "vault", Owner: "alice", HostIP: "172.30.3.2"},
		{Name: "resting", Owner: "alice", HostIP: ""}, // paused: no tap
	}}
	s := NewSyncer(client, fleet, fakeRules{}, nil)
	err := s.Apply(context.Background(), map[string][]string{
		"ci-box":  {"github.com"},
		"vault":   {},
		"resting": {"pypi.org"},    // no live tap here
		"gone":    {"example.com"}, // destroyed between resolve and apply
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string][]string{"sbtap576": {"github.com"}, "sbtap192": {}}
	if !reflect.DeepEqual(got.Taps, want) {
		t.Errorf("applied taps = %v, want %v", got.Taps, want)
	}
}

// Push is still both halves back to back, so a single-box deployment is
// unchanged by the split existing.
func TestPushIsResolveThenApply(t *testing.T) {
	var mu sync.Mutex
	var got policyBody
	client := unixSluice(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		json.NewDecoder(r.Body).Decode(&got) //nolint:errcheck
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	fleet := fakeFleet{boxes: []Sandbox{{Name: "ci-box", Owner: "alice", HostIP: "172.30.5.2"}}}
	rules := fakeRules{"alice/ci-box": {"github.com"}}
	s := NewSyncer(client, fleet, rules, nil)
	if err := s.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := (map[string][]string{"sbtap320": {"github.com"}}); !reflect.DeepEqual(got.Taps, want) {
		t.Errorf("pushed taps = %v, want %v", got.Taps, want)
	}
}
