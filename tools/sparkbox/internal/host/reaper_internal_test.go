package host

// White-box tests for the idle gradient: they reach reapOnce and the boxes map
// directly so the balloon→pause transitions can be driven deterministically
// (setting LastActive into the past) without a live ticker or sleeps.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"testing"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

func internalManager(t *testing.T, opts Options) *Manager {
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
	opts.StateDir = dir
	opts.Driver = driver
	opts.GatewayPublicKey = string(xssh.MarshalAuthorizedKey(signer.PublicKey()))
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := NewManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestReaperGradientBalloonThenPause(t *testing.T) {
	m := internalManager(t, Options{MemReserveMB: 1024})
	ctx := context.Background()
	if _, err := m.Create(ctx, "warm", "alice", "ubuntu", 1, 8192); err != nil {
		t.Fatal(err)
	}

	// Idle 5m: past the 1m balloon floor, short of the 1h pause floor → balloon
	// down (RAM reclaimed to the balloon) but stay running.
	m.boxes["warm"].LastActive = time.Now().Add(-5 * time.Minute)
	m.reapOnce(ctx, time.Minute, time.Hour)

	if !m.boxes["warm"].Ballooned {
		t.Fatal("idle-past-balloon sandbox should be ballooned")
	}
	if m.boxes["warm"].State != vmm.StateRunning {
		t.Fatalf("ballooned sandbox must keep running, got %s", m.boxes["warm"].State)
	}
	// The driver was told to reclaim everything above the 1024 MB reserve.
	st, err := m.balloon.BalloonStats(ctx, "warm")
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(8192 - 1024); st.TargetMiB != want {
		t.Fatalf("balloon target = %d MiB, want %d", st.TargetMiB, want)
	}

	// A second pass at the same idle must not re-balloon (idempotent).
	m.reapOnce(ctx, time.Minute, time.Hour)
	if !m.boxes["warm"].Ballooned {
		t.Fatal("second pass unexpectedly changed balloon state")
	}

	// Activity deflates it and clears the flag.
	if _, err := m.EnsureReady(ctx, "warm"); err != nil {
		t.Fatal(err)
	}
	if m.boxes["warm"].Ballooned {
		t.Fatal("EnsureRunning should have deflated the balloon")
	}
	st, _ = m.balloon.BalloonStats(ctx, "warm")
	if st.TargetMiB != 0 {
		t.Fatalf("balloon should be fully deflated, target = %d", st.TargetMiB)
	}
	if err := m.FlushActivity(); err != nil {
		t.Fatal(err)
	}

	// Idle past the pause floor → paused (RAM fully freed).
	m.boxes["warm"].LastActive = time.Now().Add(-2 * time.Hour)
	m.reapOnce(ctx, time.Minute, time.Hour)
	if m.boxes["warm"].State != vmm.StatePaused {
		t.Fatalf("idle-past-pause sandbox should be paused, got %s", m.boxes["warm"].State)
	}
}

func TestReaperNoBalloonWithoutReserve(t *testing.T) {
	// Reserve off (0): the balloon stage is skipped entirely — straight to pause,
	// the pre-overcommit behaviour.
	m := internalManager(t, Options{}) // MemReserveMB defaults to 0
	ctx := context.Background()
	if _, err := m.Create(ctx, "box", "alice", "ubuntu", 1, 8192); err != nil {
		t.Fatal(err)
	}
	m.boxes["box"].LastActive = time.Now().Add(-5 * time.Minute)
	m.reapOnce(ctx, time.Minute, time.Hour)
	if m.boxes["box"].Ballooned {
		t.Fatal("no reserve configured: should never balloon")
	}
	if m.boxes["box"].State != vmm.StateRunning {
		t.Fatal("short of the pause floor: should still be running")
	}
}

func TestPresenceLeasePreventsPauseButAllowsBalloon(t *testing.T) {
	m := internalManager(t, Options{MemReserveMB: 1024})
	ctx := context.Background()
	box, err := m.Create(ctx, "working", "alice", "ubuntu", 1, 8192)
	if err != nil {
		t.Fatal(err)
	}
	m.boxes["working"].LastActive = time.Now().Add(-2 * time.Hour)
	m.ProtectUntil(box.ID, time.Now().Add(10*time.Minute))

	m.reapOnce(ctx, time.Minute, time.Hour)

	if m.boxes["working"].State != vmm.StateRunning {
		t.Fatalf("presence-protected sandbox was paused: %s", m.boxes["working"].State)
	}
	if !m.boxes["working"].Ballooned {
		t.Fatal("presence lease should still allow idle memory ballooning")
	}

	// Once the lease expires, the ordinary idle policy resumes.
	m.protectUntil[box.ID] = time.Now().Add(-time.Second)
	m.reapOnce(ctx, time.Minute, time.Hour)
	if m.boxes["working"].State != vmm.StatePaused {
		t.Fatalf("sandbox with expired lease stayed %s, want paused", m.boxes["working"].State)
	}
}

func TestProtectUntilNeverShortensLease(t *testing.T) {
	m := internalManager(t, Options{})
	later := time.Now().Add(time.Hour)
	m.ProtectUntil("sandbox-id", later)
	m.ProtectUntil("sandbox-id", time.Now().Add(time.Minute))
	if got := m.protectUntil["sandbox-id"]; !got.Equal(later) {
		t.Fatalf("lease shortened to %v, want %v", got, later)
	}
}

func TestOwnerPressureBalloonsColdestWorkingSets(t *testing.T) {
	m := internalManager(t, Options{MemReserveMB: 128, OwnerMemoryPoolMB: 512})
	ctx := context.Background()
	for i, name := range []string{"coldest", "middle", "newest"} {
		if _, err := m.Create(ctx, name, "alice", "ubuntu", 1, 512); err != nil {
			t.Fatal(err)
		}
		m.boxes[name].LastActive = time.Now().Add(time.Duration(i-3) * time.Hour)
	}

	// The mock reports a 256 MiB working set for each VM: 768 MiB actual against
	// a 512 MiB owner pool, despite only 384 MiB of admission charges.
	m.reconcileMemoryPressure(ctx)
	if !m.boxes["coldest"].Ballooned || !m.boxes["middle"].Ballooned {
		t.Fatalf("pressure did not reclaim the two coldest VMs: cold=%v middle=%v newest=%v",
			m.boxes["coldest"].Ballooned, m.boxes["middle"].Ballooned, m.boxes["newest"].Ballooned)
	}
	if m.boxes["newest"].Ballooned {
		t.Fatal("pressure reclaimed a newer VM after enough cold memory was available")
	}
}

func TestPressureReclaimsTurboBeforeNormalVM(t *testing.T) {
	m := internalManager(t, Options{
		MemReserveMB: 128, OwnerMemoryPoolMB: 512, OwnerMemoryBurstMB: 1024,
	})
	ctx := context.Background()
	for _, name := range []string{"normal", "fast"} {
		if _, err := m.Create(ctx, name, "alice", "ubuntu", 1, 512); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.SetTurbo(ctx, "fast", true); err != nil {
		t.Fatal(err)
	}
	// Make normal older to prove the turbo-first rule wins over LRU.
	m.boxes["normal"].LastActive = time.Now().Add(-2 * time.Hour)
	m.boxes["fast"].LastActive = time.Now()
	m.memUsed["normal"], m.memUsed["fast"] = 400, 800
	m.reclaimMemory(ctx, "alice", 100)
	if !m.boxes["fast"].Ballooned || m.boxes["normal"].Ballooned {
		t.Fatalf("turbo was not reclaimed first: normal=%v turbo=%v",
			m.boxes["normal"].Ballooned, m.boxes["fast"].Ballooned)
	}
	stats, err := m.balloon.BalloonStats(ctx, "fast")
	if err != nil {
		t.Fatal(err)
	}
	// Turbo retains twice the 128 MiB baseline floor.
	if want := int64(1024 - 256); stats.TargetMiB != want {
		t.Fatalf("turbo balloon target = %d, want %d", stats.TargetMiB, want)
	}
}
