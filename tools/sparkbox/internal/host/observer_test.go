package host_test

// The Observer is how a process that mirrors this host's inventory learns that
// anything happened — it never polls — so the property under test is coverage:
// every lifecycle transition emits exactly one event, named by the transition
// rather than by the sentence a human reads.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// observed is one recorded SandboxChanged call.
type observed struct {
	reason string
	box    *host.Sandbox
}

// recorder is a host.Observer that just remembers. It takes its own lock
// because events arrive under the manager's, on whichever goroutine drove the
// change — and it returns immediately, as the interface demands.
type recorder struct {
	mu      sync.Mutex
	changed []observed
	gone    []string
}

func (r *recorder) SandboxChanged(b *host.Sandbox, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changed = append(r.changed, observed{reason: reason, box: b})
}

func (r *recorder) SandboxGone(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gone = append(r.gone, name)
}

// take drains what has been recorded so far, so each step of a sequence judges
// only its own transition. Compound operations emit several events — a rename
// pauses first, an archive pauses and a restore resumes — which is why the
// assertions below count one reason rather than the whole slice.
func (r *recorder) take() ([]observed, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed, gone := r.changed, r.gone
	r.changed, r.gone = nil, nil
	return changed, gone
}

// only returns the single event carrying reason, failing if there is not
// exactly one: a transition reported twice is a duplicate somewhere downstream,
// and one reported never is a lifecycle path nobody will hear about.
func only(t *testing.T, events []observed, reason string) observed {
	t.Helper()
	var hits []observed
	for _, e := range events {
		if e.reason == reason {
			hits = append(hits, e)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("got %d %q events, want 1; the whole batch was %s", len(hits), reason, summarize(events))
	}
	return hits[0]
}

func summarize(events []observed) string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, fmt.Sprintf("%s/%s", e.box.Name, e.reason))
	}
	return fmt.Sprint(out)
}

// TestObserverSeesEveryTransition walks one sandbox through its whole life on a
// manager built with an Observer, checking each step reports itself once.
func TestObserverSeesEveryTransition(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	m := newTestManager(t, host.Options{Observer: rec, Archive: newMemStore()})

	steps := []struct {
		name      string
		run       func() error
		reason    string
		wantBox   string
		wantState vmm.State
		wantGone  string
	}{
		{
			name:   "create",
			run:    func() error { _, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 512); return err },
			reason: "created", wantBox: "box", wantState: vmm.StateRunning,
		},
		{
			name: "pause", run: func() error { return m.Pause(ctx, "box") },
			reason: "paused", wantBox: "box", wantState: vmm.StatePaused,
		},
		{
			name: "resume", run: func() error { _, err := m.EnsureRunning(ctx, "box"); return err },
			reason: "resumed", wantBox: "box", wantState: vmm.StateRunning,
		},
		{
			name: "pin", run: func() error { return m.SetPinned("box", true) },
			reason: "pinned", wantBox: "box", wantState: vmm.StateRunning,
		},
		{
			name: "unpin", run: func() error { return m.SetPinned("box", false) },
			reason: "unpinned", wantBox: "box", wantState: vmm.StateRunning,
		},
		{
			name: "resize", run: func() error { return m.Resize(ctx, "box", 51200) },
			reason: "resized", wantBox: "box", wantState: vmm.StateRunning,
		},
		{
			name: "reboot", run: func() error { return m.Reboot(ctx, "box") },
			reason: "rebooted", wantBox: "box", wantState: vmm.StateRunning,
		},
		{
			// The record arrives under its new name: a mirror keyed by the old
			// one would otherwise carry a ghost forever.
			name: "rename", run: func() error { return m.Rename(ctx, "box", "boxy", "alice") },
			reason: "renamed", wantBox: "boxy", wantState: vmm.StatePaused,
		},
		{
			name: "archive", run: func() error { return m.Archive(ctx, "boxy") },
			reason: "archived", wantBox: "boxy", wantState: vmm.StateArchived,
		},
		{
			// Resume-on-connect restores first, so this step emits the restore
			// and the resume that rides on it.
			name: "restore", run: func() error { _, err := m.EnsureRunning(ctx, "boxy"); return err },
			reason: "restored", wantBox: "boxy", wantState: vmm.StatePaused,
		},
		{
			name: "destroy", run: func() error { return m.Destroy(ctx, "boxy") },
			wantGone: "boxy",
		},
	}

	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			if err := step.run(); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			changed, gone := rec.take()
			if step.reason != "" {
				e := only(t, changed, step.reason)
				if e.box.Name != step.wantBox {
					t.Errorf("%s event carried sandbox %q, want %q", step.reason, e.box.Name, step.wantBox)
				}
				if e.box.State != step.wantState {
					t.Errorf("%s event carried state %q, want %q", step.reason, e.box.State, step.wantState)
				}
				if e.box.Owner != "alice" {
					t.Errorf("%s event carried owner %q, want alice", step.reason, e.box.Owner)
				}
			}
			if step.wantGone == "" {
				if len(gone) != 0 {
					t.Errorf("%s reported %v gone; only a destroy does that", step.name, gone)
				}
				return
			}
			if len(gone) != 1 || gone[0] != step.wantGone {
				t.Errorf("gone = %v, want [%s]", gone, step.wantGone)
			}
			if len(changed) != 0 {
				t.Errorf("destroy also emitted %s; a record that is gone has not changed", summarize(changed))
			}
		})
	}
}

// TestObserverGetsACopy covers the hand-off: an observer holding a record must
// not be holding the manager's, or anything it does with it — stamping a node
// name, say — rewrites live state.
func TestObserverGetsACopy(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	m := newTestManager(t, host.Options{Observer: rec})

	if _, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	changed, _ := rec.take()
	e := only(t, changed, "created")
	e.box.Owner = "mallory"
	e.box.Pinned = true

	got, ok := m.Get("box")
	if !ok {
		t.Fatal("sandbox missing after create")
	}
	if got.Owner != "alice" || got.Pinned {
		t.Errorf("observer mutated the manager's record: owner %q pinned %v", got.Owner, got.Pinned)
	}
}

// TestNoObserverIsFine pins that the hook stays optional: every single-box
// deployment runs without one.
func TestNoObserverIsFine(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, host.Options{})
	if _, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatal(err)
	}
	if err := m.Destroy(ctx, "box"); err != nil {
		t.Fatal(err)
	}
}

// keylessManager wires a manager that has not been told the gateway's public
// key, alongside the authorized_keys line its guests will accept once it has.
func keylessManager(t *testing.T) (*host.Manager, string) {
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
	m, err := host.NewManager(host.Options{
		StateDir: dir,
		Driver:   driver,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, string(xssh.MarshalAuthorizedKey(signer.PublicKey()))
}

// TestCreateWaitsForTheGatewayKey covers the bootstrap window of a host that is
// told the gateway's key rather than started with it: booting a guest in that
// window spends a rootfs on a VM nobody can ever log into, so the create is
// refused as a capability this host does not have *yet*.
func TestCreateWaitsForTheGatewayKey(t *testing.T) {
	ctx := context.Background()
	m, keyLine := keylessManager(t)

	_, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 512)
	var disabled *host.DisabledError
	if !errors.As(err, &disabled) {
		t.Fatalf("create without a gateway key: want *host.DisabledError, got %v", err)
	}
	if disabled.Code != "no_gateway_key" {
		t.Errorf("code = %q, want no_gateway_key", disabled.Code)
	}
	if _, ok := m.Get("box"); ok {
		t.Error("a refused create left a record behind")
	}

	m.SetGatewayPublicKey(keyLine)
	if _, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 512); err != nil {
		t.Fatalf("create after the key arrived: %v", err)
	}
}

// TestSetGatewayPublicKeyIsRacyByDesign runs the setter against live creates:
// the key lands mid-flight in production (the link comes up while a caller is
// already asking for a sandbox), so the only acceptable outcomes are a clean
// create and the typed refusal — never a torn read, and never a booted guest
// carrying a half-written key. Meaningful under -race.
func TestSetGatewayPublicKeyIsRacyByDesign(t *testing.T) {
	ctx := context.Background()
	m, keyLine := keylessManager(t)

	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = m.Create(ctx, fmt.Sprintf("box%d", i), "alice", "ubuntu", 1, 512)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.SetGatewayPublicKey(keyLine)
	}()
	wg.Wait()

	for i, err := range errs {
		var disabled *host.DisabledError
		if err != nil && !errors.As(err, &disabled) {
			t.Errorf("box%d: %v", i, err)
			continue
		}
		_, created := m.Get(fmt.Sprintf("box%d", i))
		if created != (err == nil) {
			t.Errorf("box%d: err %v but record present = %v", i, err, created)
		}
	}
}
