package netpush

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestTapName(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"172.30.5.2":  {"sbtap5", true},
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
		"sbtap5": {"github.com"},
		"sbtap6": {"huggingface.co"},
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
		"sbtap5": {"github.com"},
		"sbtap6": {}, // deny-all still pushed, so sluice enforces (drops) it
	}
	if !reflect.DeepEqual(got.Taps, want) {
		t.Errorf("pushed taps = %#v, want %#v (untagged VM must be omitted)", got.Taps, want)
	}
	if _, ok := got.Taps["sbtap7"]; ok {
		t.Error("ungoverned VM sbtap7 must not appear in the policy")
	}
}

func TestUsageAttributesTapsToVMsAndFiltersByOwner(t *testing.T) {
	rep := Report{Taps: []TapUsage{
		{Tap: "sbtap5", TxBytes: 100, RxBytes: 900, Domains: []DomainUsage{
			{Domain: "github.com", TxBytes: 10, RxBytes: 90},
			{Domain: "youtube.com", TxBytes: 90, RxBytes: 810},
		}},
		{Tap: "sbtap9", TxBytes: 5, RxBytes: 5, Domains: nil}, // bob's box
		{Tap: "sbtap42", TxBytes: 1, RxBytes: 1},              // no current VM
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
