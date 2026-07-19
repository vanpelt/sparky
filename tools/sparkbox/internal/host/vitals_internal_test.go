package host

// White-box tests for vitals-based activity detection: they reach reapOnce and
// the vitals/boxes maps directly so a sandbox's resource counters can be
// stepped by an exact amount over a controlled interval, without a live ticker.

import (
	"context"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm/mock"
)

// primeVitals installs a baseline reading `ago` in the past, standing in for a
// previous reaper tick, so the next reapOnce measures against a known interval.
func primeVitals(m *Manager, name string, ago time.Duration, cpuNanos, rx, tx uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vitals[name] = vitalsSample{
		at:       time.Now().Add(-ago),
		cpuNanos: cpuNanos,
		rx:       rx,
		tx:       tx,
	}
}

// TestVitalsNetworkActivityDefersPause is the case the whole feature exists
// for: a sandbox nobody has connected to in an hour, but which is streaming
// network traffic (an agent talking to a model API), must not be paused.
func TestVitalsNetworkActivityDefersPause(t *testing.T) {
	m := internalManager(t, Options{ActivityNetBytes: 64 * 1024})
	ctx := context.Background()
	if _, err := m.Create(ctx, "busy", "alice", "ubuntu", 1, 2048); err != nil {
		t.Fatal(err)
	}
	driver := m.driver.(*mock.Driver)

	// Idle by the traffic clock for an hour, but 1 MB moved since the last tick.
	m.boxes["busy"].LastActive = time.Now().Add(-time.Hour)
	primeVitals(m, "busy", time.Minute, 0, 0, 0)
	if err := driver.SetNetBytes("busy", 700_000, 300_000); err != nil {
		t.Fatal(err)
	}

	m.reapOnce(ctx, 0, 30*time.Minute)

	if got := m.boxes["busy"].State; got != vmm.StateRunning {
		t.Fatalf("network-active sandbox was reaped: state = %v, want running", got)
	}
	if idle := time.Since(m.boxes["busy"].LastActive); idle > time.Minute {
		t.Fatalf("LastActive not reset by network activity: idle = %v", idle)
	}
	// The traffic is also charged to the lifetime meters, guest-perspective.
	if rx, tx := m.boxes["busy"].NetRxBytes, m.boxes["busy"].NetTxBytes; rx != 700_000 || tx != 300_000 {
		t.Fatalf("lifetime totals = rx %d/tx %d, want rx 700000/tx 300000", rx, tx)
	}
}

// TestVitalsQuietSandboxStillPauses is the other half: activity detection must
// not become a blanket reprieve. A sandbox below both floors still gets reaped.
func TestVitalsQuietSandboxStillPauses(t *testing.T) {
	m := internalManager(t, Options{ActivityCPUPct: 2, ActivityNetBytes: 64 * 1024})
	ctx := context.Background()
	if _, err := m.Create(ctx, "quiet", "alice", "ubuntu", 1, 2048); err != nil {
		t.Fatal(err)
	}
	driver := m.driver.(*mock.Driver)

	m.boxes["quiet"].LastActive = time.Now().Add(-time.Hour)
	// A minute's baseline, and only background chatter since: 3 KB of traffic
	// (the measured idle rate) and a CPU counter that barely moved. The mock
	// accrues 50ms of CPU per read, so prime high enough that one read's worth
	// over a minute stays under the 2%-of-a-core floor.
	primeVitals(m, "quiet", time.Minute, 0, 2000, 1000)
	if err := driver.SetNetBytes("quiet", 4000, 2000); err != nil {
		t.Fatal(err)
	}

	m.reapOnce(ctx, 0, 30*time.Minute)

	if got := m.boxes["quiet"].State; got != vmm.StatePaused {
		t.Fatalf("idle sandbox not paused: state = %v, want paused", got)
	}
	// Even the trickle is metered.
	if rx := m.boxes["quiet"].NetRxBytes; rx != 2000 {
		t.Fatalf("NetRxBytes = %d, want 2000", rx)
	}
}

// TestVitalsCPUActivityDefersPause covers the signal network misses: a quiet
// but compute-heavy guest (a build, a training run) with no traffic at all.
func TestVitalsCPUActivityDefersPause(t *testing.T) {
	m := internalManager(t, Options{ActivityCPUPct: 2})
	ctx := context.Background()
	if _, err := m.Create(ctx, "compute", "alice", "ubuntu", 1, 2048); err != nil {
		t.Fatal(err)
	}

	m.boxes["compute"].LastActive = time.Now().Add(-time.Hour)
	// Baseline 10s back with the CPU counter 9s of core-time behind where the
	// mock's next read lands: ~90% of one core, far over the 2% floor. No
	// network movement at all.
	m.mu.Lock()
	m.vitals["compute"] = vitalsSample{at: time.Now().Add(-10 * time.Second)}
	m.mu.Unlock()
	driver := m.driver.(*mock.Driver)
	for i := 0; i < 180; i++ { // 180 * 50ms = 9s of accrued CPU
		if _, err := driver.CPUTimeNanos(ctx, "compute"); err != nil {
			t.Fatal(err)
		}
	}

	m.reapOnce(ctx, 0, 30*time.Minute)

	if got := m.boxes["compute"].State; got != vmm.StateRunning {
		t.Fatalf("CPU-active sandbox was reaped: state = %v, want running", got)
	}
}

// TestVitalsCounterResetNotCharged guards the accumulator against the tap
// teardown every pause/resume causes: counters restart at zero, and a naive
// subtraction would underflow into a ~16 exabyte "delta" that both corrupts the
// lifetime meter and pins the sandbox active forever.
func TestVitalsCounterResetNotCharged(t *testing.T) {
	m := internalManager(t, Options{ActivityNetBytes: 64 * 1024})
	ctx := context.Background()
	if _, err := m.Create(ctx, "cycled", "alice", "ubuntu", 1, 2048); err != nil {
		t.Fatal(err)
	}
	driver := m.driver.(*mock.Driver)
	m.boxes["cycled"].NetRxBytes = 5_000_000
	m.boxes["cycled"].NetTxBytes = 5_000_000

	// Baseline high, current reading low: the device was recreated between ticks.
	primeVitals(m, "cycled", time.Minute, 0, 900_000, 900_000)
	if err := driver.SetNetBytes("cycled", 1000, 500); err != nil {
		t.Fatal(err)
	}
	m.boxes["cycled"].LastActive = time.Now().Add(-time.Hour)

	m.reapOnce(ctx, 0, 30*time.Minute)

	// Post-reset reading is charged at face value, not prev-minus-cur.
	if rx, tx := m.boxes["cycled"].NetRxBytes, m.boxes["cycled"].NetTxBytes; rx != 5_001_000 || tx != 5_000_500 {
		t.Fatalf("reset mischarged: rx = %d, tx = %d; want 5001000/5000500", rx, tx)
	}
	// 1.5 KB is under the floor, so the sandbox is still idle and gets reaped.
	if got := m.boxes["cycled"].State; got != vmm.StatePaused {
		t.Fatalf("counter reset masqueraded as activity: state = %v, want paused", got)
	}
}

// TestVitalsFirstReadingOnlyPrimes: with no baseline there is nothing to
// subtract, so the first tick must record and judge nothing. Charging the raw
// counter would read a long-running sandbox's whole history as one interval's
// traffic and grant it a reprieve on every control-plane restart.
func TestVitalsFirstReadingOnlyPrimes(t *testing.T) {
	m := internalManager(t, Options{ActivityNetBytes: 64 * 1024})
	ctx := context.Background()
	if _, err := m.Create(ctx, "fresh", "alice", "ubuntu", 1, 2048); err != nil {
		t.Fatal(err)
	}
	driver := m.driver.(*mock.Driver)
	if err := driver.SetNetBytes("fresh", 50_000_000, 50_000_000); err != nil {
		t.Fatal(err)
	}
	m.boxes["fresh"].LastActive = time.Now().Add(-time.Hour)

	m.reapOnce(ctx, 0, 30*time.Minute)

	if rx := m.boxes["fresh"].NetRxBytes; rx != 0 {
		t.Fatalf("priming read charged %d bytes, want 0", rx)
	}
	if got := m.boxes["fresh"].State; got != vmm.StatePaused {
		t.Fatalf("priming read granted a reprieve: state = %v, want paused", got)
	}
}

// TestVitalsPauseDropsBaseline: a resumed sandbox must re-prime rather than
// measure across the pause, whose interval would dilute any rate to nothing.
func TestVitalsPauseDropsBaseline(t *testing.T) {
	m := internalManager(t, Options{ActivityNetBytes: 64 * 1024})
	ctx := context.Background()
	if _, err := m.Create(ctx, "cycled", "alice", "ubuntu", 1, 2048); err != nil {
		t.Fatal(err)
	}
	primeVitals(m, "cycled", time.Minute, 0, 1, 1)

	if err := m.Pause(ctx, "cycled"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	_, ok := m.vitals["cycled"]
	m.mu.Unlock()
	if ok {
		t.Fatal("pause left a stale vitals baseline behind")
	}
}

// TestVitalsDisabledLeavesReaperAlone: with both floors at zero the old
// traffic-only policy must hold exactly, so the feature is opt-in.
func TestVitalsDisabledLeavesReaperAlone(t *testing.T) {
	m := internalManager(t, Options{})
	ctx := context.Background()
	if _, err := m.Create(ctx, "loud", "alice", "ubuntu", 1, 2048); err != nil {
		t.Fatal(err)
	}
	driver := m.driver.(*mock.Driver)
	primeVitals(m, "loud", time.Minute, 0, 0, 0)
	if err := driver.SetNetBytes("loud", 900_000_000, 900_000_000); err != nil {
		t.Fatal(err)
	}
	m.boxes["loud"].LastActive = time.Now().Add(-time.Hour)

	m.reapOnce(ctx, 0, 30*time.Minute)

	if got := m.boxes["loud"].State; got != vmm.StatePaused {
		t.Fatalf("activity floors are off; sandbox should still pause, got %v", got)
	}
	// Metering runs regardless: the bytes are recorded even with the floors off.
	if rx := m.boxes["loud"].NetRxBytes; rx != 900_000_000 {
		t.Fatalf("NetRxBytes = %d, want 900000000 (metering is independent of the floors)", rx)
	}
}
