package ctlops

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// preM0Payload is the `list`/`info` wire form as it shipped before the fleet
// existed, for the rig's domain and xterm subdomain. It is a literal rather
// than a struct because it is the bytes clients parse: a field that reordered,
// or one that stopped being omitted, would pass a field-by-field check and
// break them.
const preM0Payload = `{"name":"alicebox","owner":"alice","state":"running","pinned":false,` +
	`"tags":[],"vcpus":2,"mem_mb":8192,"disk_mb":25600,` +
	`"created_at":"1970-01-01T00:00:00Z","last_active":"1970-01-01T00:00:00Z",` +
	`"url":"https://alicebox.example.test","terminal_url":"https://alicebox-xterm.example.test"}`

// TestInfoOnASingleBoxAddsOnlyTheNode pins what a single-box deployment
// actually pays for the fleet. The tempting claim — that a lone box's payload
// is byte-identical to the pre-fleet one — is false and cannot be tested
// honestly: NewManager defaults its node name to "local", backfills it onto
// every record it loads and stamps it onto every record it creates, so there
// is no such thing as a live sandbox with an empty Node. The record here comes
// out of a real Manager for exactly that reason; a hand-built one with Node ""
// would assert a shape the product never emits.
//
// What is true, and what clients depend on, is that the delta is exactly one
// key: `node` appears, `unreachable` does not (a box on the machine serving the
// request is reachable by construction), and nothing else moved or vanished.
func TestInfoOnASingleBoxAddsOnlyTheNode(t *testing.T) {
	r := newRig(t)
	b := singleBoxRecord(t)

	// A single-box record names its machine. If this ever stops holding, the
	// byte-comparison below is testing a fiction rather than the product.
	if b.Node == "" {
		t.Fatalf("manager emitted a record with no node: %+v", b)
	}
	if b.SSHAddr == "" || b.HostIP == "" {
		t.Fatalf("driver gave no topology to drop: %+v", b)
	}

	got, err := json.Marshal(r.ops.info(b))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	const want = `{"name":"alicebox","owner":"alice","state":"running","node":"local",` +
		`"pinned":false,"tags":[],"vcpus":2,"mem_mb":8192,"disk_mb":25600,` +
		`"created_at":"1970-01-01T00:00:00Z","last_active":"1970-01-01T00:00:00Z",` +
		`"url":"https://alicebox.example.test","terminal_url":"https://alicebox-xterm.example.test"}`
	if string(got) != want {
		t.Errorf("single-box payload changed\n got: %s\nwant: %s", got, want)
	}

	// And the delta against the shipped shape, checked rather than asserted by
	// eye: the two literals above differ by `node` and by nothing else.
	added, removed, changed := diffPayloads(t, preM0Payload, string(got))
	if len(added) != 1 || added[0] != "node" {
		t.Errorf("keys added to the single-box payload = %v, want [node]", added)
	}
	if len(removed) != 0 {
		t.Errorf("keys dropped from the single-box payload = %v, want none", removed)
	}
	if len(changed) != 0 {
		t.Errorf("values changed in the single-box payload = %v, want none", changed)
	}

	// A node name is not an address, and the record this came from carried real
	// ones. Nothing in the projection may hand a client something it could dial.
	var back map[string]any
	if err := json.Unmarshal(got, &back); err != nil {
		t.Fatalf("unmarshal %s: %v", got, err)
	}
	for _, k := range []string{"ssh_addr", "ssh_user", "host_ip", "guest_v6"} {
		if _, ok := back[k]; ok {
			t.Errorf("payload leaked %q: %s", k, got)
		}
	}
}

// TestInfoCarriesTheNode is the other half: once a record names a machine other
// than the one answering, both fields have to reach the client, because "which
// box is my sandbox on" and "is that box answering" are the only two questions
// a fleet adds to `list`.
func TestInfoCarriesTheNode(t *testing.T) {
	r := newRig(t)
	now := time.Unix(0, 0).UTC()

	si := r.ops.info(&host.Sandbox{
		Name: "bobbox", Owner: "bob", State: vmm.StateRunning,
		Node: "nodeb", Unreachable: true,
		CreatedAt: now, LastActive: now,
	})
	if si.Node != "nodeb" || !si.Unreachable {
		t.Fatalf("info = %+v, want node nodeb and unreachable", si)
	}

	raw, err := json.Marshal(si)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if back["node"] != "nodeb" || back["unreachable"] != true {
		t.Errorf("payload = %s, want node and unreachable set", raw)
	}
	// A node name is not an address. Nothing in the projection may hand a
	// client something it could dial.
	for _, k := range []string{"ssh_addr", "host_ip", "guest_v6"} {
		if _, ok := back[k]; ok {
			t.Errorf("payload leaked %q: %s", k, raw)
		}
	}
}

// singleBoxRecord returns the record a default, unnamed host produces for a
// freshly created sandbox — node name, driver topology and all — with the two
// clock fields and the disk ceiling frozen so the projection has stable bytes.
// Those three are the only values a caller may pin without lying about the
// record; everything the fleet work touches is left exactly as the Manager
// wrote it.
func singleBoxRecord(t *testing.T) *host.Sandbox {
	t.Helper()
	dir := t.TempDir()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	driver := mock.New(dir, signer)
	t.Cleanup(func() { driver.Close() })

	mgr, err := host.NewManager(host.Options{
		StateDir:         dir,
		Driver:           driver,
		GatewayPublicKey: string(xssh.MarshalAuthorizedKey(signer.PublicKey())),
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		// NodeName deliberately unset: this is the single-box deployment.
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := mgr.Create(context.Background(), "alicebox", "alice", "base", 2, 8192)
	if err != nil {
		t.Fatal(err)
	}
	rec := *b
	now := time.Unix(0, 0).UTC()
	rec.CreatedAt, rec.LastActive = now, now
	rec.DiskMB = 25600
	return &rec
}

// diffPayloads compares two JSON objects key by key, so a test can state which
// keys a change added or removed instead of eyeballing two long literals.
func diffPayloads(t *testing.T, before, after string) (added, removed, changed []string) {
	t.Helper()
	var a, b map[string]any
	if err := json.Unmarshal([]byte(before), &a); err != nil {
		t.Fatalf("unmarshal %s: %v", before, err)
	}
	if err := json.Unmarshal([]byte(after), &b); err != nil {
		t.Fatalf("unmarshal %s: %v", after, err)
	}
	for k, bv := range b {
		av, ok := a[k]
		switch {
		case !ok:
			added = append(added, k)
		case !sameJSON(av, bv):
			changed = append(changed, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return added, removed, changed
}

// sameJSON compares two decoded JSON values by re-encoding them: the values in
// these payloads are scalars and one string slice, and re-encoding is exact for
// both without dragging in reflect.
func sameJSON(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}
