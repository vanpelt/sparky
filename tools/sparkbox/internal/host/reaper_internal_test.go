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

// TestBalloonReclaimIsNotReadAsActivity pins the fix for a loop that kept a
// completely idle sandbox running for fifteen hours.
//
// vmm.CPUStatser reads the VMM process's own utime+stime, so inflating a
// balloon is charged to the very counter the reaper reads for evidence of work:
// the host releases gigabytes of guest pages and the guest kernel then faults
// its working set back in. Read as activity, that deflated the balloon and
// reset the idle clock; two minutes later the reaper ballooned it down again,
// and the idle clock could never reach the pause threshold.
func TestBalloonReclaimIsNotReadAsActivity(t *testing.T) {
	m := internalManager(t, Options{MemReserveMB: 1024, ActivityCPUPct: 2})
	ctx := context.Background()
	if _, err := m.Create(ctx, "warm", "alice", "ubuntu", 1, 8192); err != nil {
		t.Fatal(err)
	}
	idleSince := time.Now().Add(-5 * time.Minute)
	m.boxes["warm"].LastActive = idleSince
	if err := m.balloonDown(ctx, "warm"); err != nil {
		t.Fatal(err)
	}
	if !m.boxes["warm"].Ballooned {
		t.Fatal("setup: the sandbox should be ballooned")
	}

	// One minute's interval carrying thirty seconds of VMM CPU: half a core,
	// more than an order of magnitude past the 2% floor. This is the shape of
	// the reclaim itself, not of anything the user asked for.
	spike := func() {
		now := time.Now()
		m.vitals["warm"] = vitalsSample{at: now.Add(-time.Minute)}
		m.applyVitals(ctx, "warm", vitalsSample{at: now, cpuNanos: uint64(30 * time.Second)}, true, false)
	}
	spike()

	if !m.boxes["warm"].Ballooned {
		t.Error("the balloon's own reclaim deflated it — the loop is back")
	}
	if got := m.boxes["warm"].LastActive; !got.Equal(idleSince) {
		t.Errorf("the reclaim reset the idle clock to %s; a sandbox that cannot accumulate idle can never be paused", got)
	}

	// Once the balloon has settled the same reading means what it says.
	m.balloonedAt["warm"] = time.Now().Add(-2 * balloonSettle)
	spike()
	if m.boxes["warm"].Ballooned {
		t.Error("a settled sandbox that is genuinely busy must get its RAM back")
	}
	if !m.boxes["warm"].LastActive.After(idleSince) {
		t.Error("a settled sandbox that is genuinely busy must reset the idle clock")
	}
}

// TestBalloonedSandboxStillTrustsItsNetwork is the other half: only CPU is
// compromised by ballooning. Traffic is not, so it stays a valid activity
// signal in both states — which is what keeps the settle window above from
// costing a busy sandbox its RAM for three minutes.
func TestBalloonedSandboxStillTrustsItsNetwork(t *testing.T) {
	m := internalManager(t, Options{MemReserveMB: 1024, ActivityCPUPct: 2, ActivityNetBytes: 64 * 1024})
	ctx := context.Background()
	if _, err := m.Create(ctx, "warm", "alice", "ubuntu", 1, 8192); err != nil {
		t.Fatal(err)
	}
	idleSince := time.Now().Add(-5 * time.Minute)
	m.boxes["warm"].LastActive = idleSince
	if err := m.balloonDown(ctx, "warm"); err != nil {
		t.Fatal(err)
	}

	// No CPU reading at all, a megabyte of traffic: unambiguously the guest.
	now := time.Now()
	m.vitals["warm"] = vitalsSample{at: now.Add(-time.Minute)}
	m.applyVitals(ctx, "warm", vitalsSample{at: now, rx: 1 << 20}, false, true)

	if m.boxes["warm"].Ballooned {
		t.Error("traffic on a ballooned sandbox must still return its RAM")
	}
	if !m.boxes["warm"].LastActive.After(idleSince) {
		t.Error("traffic on a ballooned sandbox must still reset the idle clock")
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

// A cold boot commonly crosses the steady-state owner pool while its page
// cache and dev stack are coming up. That burst is exactly when balloon reclaim
// is most harmful, and the vitals sampler needs a full interval before it can
// distinguish the startup from an idle VM.
func TestOwnerPressureDoesNotBalloonRecentActivity(t *testing.T) {
	m := internalManager(t, Options{MemReserveMB: 128, OwnerMemoryPoolMB: 256})
	ctx := context.Background()
	for _, name := range []string{"cold", "starting"} {
		if _, err := m.Create(ctx, name, "alice", "ubuntu", 1, 512); err != nil {
			t.Fatal(err)
		}
	}
	m.boxes["cold"].LastActive = time.Now().Add(-time.Hour)
	m.boxes["starting"].LastActive = time.Now().Add(-time.Hour)
	if err := m.balloonDown(ctx, "starting"); err != nil {
		t.Fatal(err)
	}
	// This is the interactive loop that caused the live failure: activity
	// deflates a previously reclaimed VM immediately before pressure runs again.
	m.applyActivity("starting", time.Now())

	m.reconcileMemoryPressure(ctx)

	if !m.boxes["cold"].Ballooned {
		t.Fatal("pressure did not reclaim the inactive sandbox")
	}
	if m.boxes["starting"].Ballooned {
		t.Fatal("pressure reclaimed a freshly active sandbox before vitals could sample it")
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
	m.boxes["fast"].LastActive = time.Now().Add(-time.Hour)
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
	// Pressure was only 100 MiB over budget. Reclaiming the entire 768 MiB above
	// the turbo reserve would make a small correction needlessly destructive.
	if want := int64(100); stats.TargetMiB != want {
		t.Fatalf("turbo pressure balloon target = %d, want only the %d MiB excess", stats.TargetMiB, want)
	}
}

// TestIdleBalloonNeverSqueezesBelowTheLiveWorkingSet pins the fix for a guest
// that burned four vCPUs for the better part of an hour and could not be
// rescued by anything the control plane knew how to do.
//
// A 4026 MiB sandbox was ballooned to its 256 MiB reserve two minutes into a
// boot that had not finished. containerd then failed eleven times, docker took
// fourteen minutes, and the in-guest agent never started at all — so no traffic
// ever moved, the activity floor never fired, and nothing deflated the balloon.
// The reserve is a guess made before the guest exists; what the guest is
// measurably touching is not, and the balloon device already reports it.
func TestIdleBalloonNeverSqueezesBelowTheLiveWorkingSet(t *testing.T) {
	m := internalManager(t, Options{MemReserveMB: 256})
	ctx := context.Background()
	if _, err := m.Create(ctx, "booting", "alice", "ubuntu", 4, 4026); err != nil {
		t.Fatal(err)
	}
	// Mid-boot with docker coming up: 2 GiB of the 4026 in real use.
	m.driver.(*mock.Driver).SetInUseMiB("booting", 2048)

	m.boxes["booting"].LastActive = time.Now().Add(-5 * time.Minute)
	m.reapOnce(ctx, time.Minute, time.Hour)

	st, err := m.balloon.BalloonStats(ctx, "booting")
	if err != nil {
		t.Fatal(err)
	}
	// Squeezing to the 256 MiB reserve would reclaim 3770 MiB — the exact
	// inflation that wedged the real guest.
	if st.TargetMiB > 4026-2048 {
		t.Fatalf("balloon reclaimed %d MiB, leaving the guest %d MiB while it was using 2048 MiB",
			st.TargetMiB, 4026-st.TargetMiB)
	}
}

// TestIdleBalloonWaitsForActualMemoryDemand is the "only when it makes sense"
// half. Inflating a balloon makes the guest fault its whole working set back in
// later; on a host with RAM to spare that is pure churn, and it is what turns
// an idle fleet into a CPU-spike generator. Genuine overage is the pressure
// controller's job.
func TestIdleBalloonWaitsForActualMemoryDemand(t *testing.T) {
	// A node budget of 90% of 64 GiB, with one idle guest nowhere near it.
	m := internalManager(t, Options{MemReserveMB: 1024, HostMemMB: 65536, MemAdmissionPct: 90})
	ctx := context.Background()
	if _, err := m.Create(ctx, "idle", "alice", "ubuntu", 1, 8192); err != nil {
		t.Fatal(err)
	}
	// A working set well clear of the reserve, so that when the balloon does
	// fire it is the demand gate being tested and not the live-working-set
	// floor: 1.5 x 3000 MiB still leaves 3692 MiB worth reclaiming.
	m.driver.(*mock.Driver).SetInUseMiB("idle", 3000)
	m.boxes["idle"].LastActive = time.Now().Add(-5 * time.Minute)
	m.reapOnce(ctx, time.Minute, time.Hour)

	if m.boxes["idle"].Ballooned {
		t.Error("an idle guest on a host with 64 GiB to spare must not be squeezed")
	}

	// Same guest, same idleness — but now it is most of the node's budget, so
	// reclaiming it actually buys something.
	m.mu.Lock()
	m.hostMemMB = 3000
	m.mu.Unlock()
	m.reapOnce(ctx, time.Minute, time.Hour)
	if !m.boxes["idle"].Ballooned {
		t.Error("a guest whose node is over its memory watermark must still be reclaimed")
	}
}
