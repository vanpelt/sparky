package host

// One reading of a running sandbox's live resource counters, gathered in one
// call.
//
// The three readers below it — CPUSeconds, MemStats, NetCounters — stay exactly
// as they were and are still the honest shape for a caller that wants one
// number. This exists because every surface that draws a meter wants all three
// at once, and because the fleet made "all three at once" the unit that has to
// cross a machine boundary: a balloon and a VMM process can only be asked of the
// host running them, so a sandbox on another node is asked over the link, and
// three round trips a second per open terminal tab is not a wire protocol, it is
// a mistake with a schema.

import (
	"context"
	"sync"
)

// Vitals is what one machine can currently say about one sandbox.
//
// Every field is a pointer because a missing reading and a genuine zero are
// different facts and every surface draws them differently — a guest that has
// used no CPU since it booted shows an idle meter, a guest whose driver has no
// CPU stats shows no meter at all. A zero value of this struct is therefore the
// correct answer for a paused sandbox, a sandbox on an unreachable machine, and
// a build with no readers wired: nobody has said.
type Vitals struct {
	// CPUSeconds is cumulative host CPU time of the VM process, in seconds.
	CPUSeconds *float64
	// MemUsedMB is the guest's real memory use in MiB, from balloon statistics.
	MemUsedMB *int64
	// NetRxBytes and NetTxBytes are the raw tap counters in bytes, from the
	// guest's point of view. They reset to zero on every pause, resume and cold
	// boot — see NetCounters — so a caller deriving a rate must treat a reading
	// below the previous one as that reset rather than a negative rate.
	NetRxBytes *uint64
	NetTxBytes *uint64
}

// Empty reports whether this reading carries nothing at all. It is what tells
// "the machine answered and has no counters for that sandbox" apart from "the
// machine answered with numbers", which are rendered identically but logged
// differently.
func (v Vitals) Empty() bool {
	return v.CPUSeconds == nil && v.MemUsedMB == nil && v.NetRxBytes == nil && v.NetTxBytes == nil
}

// Vitals reads all three counters for one sandbox on THIS machine.
//
// The reads run concurrently under the caller's context because they touch
// different things — /proc for CPU, sysfs for the tap, the VMM's API socket for
// the balloon — and only the last of those can be slow. Sequentially, one wedged
// VMM would cost a caller its CPU number too, and at a reading a second the
// latency would compound into a visibly stuttering chart.
//
// The error is always nil here and exists for the interface's sake: the same
// method on a sandbox that lives on another machine can fail at the link, and a
// caller that has to switch on where a sandbox is placed is a caller the fleet
// has failed to hide anything from. A sandbox this manager does not hold, or
// does not have running, is not an error — it is the zero reading.
func (m *Manager) Vitals(ctx context.Context, name string) (Vitals, error) {
	var (
		out Vitals
		wg  sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		if secs, ok := m.CPUSeconds(ctx, name); ok {
			out.CPUSeconds = &secs
		}
	}()
	go func() {
		defer wg.Done()
		if used, ok := m.MemStats(ctx, name); ok {
			out.MemUsedMB = &used
		}
	}()
	go func() {
		defer wg.Done()
		if rx, tx, ok := m.NetCounters(ctx, name); ok {
			out.NetRxBytes, out.NetTxBytes = &rx, &tx
		}
	}()
	wg.Wait()
	return out, nil
}
