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
	if _, err := m.EnsureRunning(ctx, "warm"); err != nil {
		t.Fatal(err)
	}
	if m.boxes["warm"].Ballooned {
		t.Fatal("EnsureRunning should have deflated the balloon")
	}
	st, _ = m.balloon.BalloonStats(ctx, "warm")
	if st.TargetMiB != 0 {
		t.Fatalf("balloon should be fully deflated, target = %d", st.TargetMiB)
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
