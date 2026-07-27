package xterm

// The instrument strip's data source: one same-origin JSON reading of the
// sandbox this page is already attached to.
//
// It is a separate route rather than a new frame type on /ws for two reasons.
// The WebSocket's wire protocol is published — openapi.json documents it, and
// internal/restapi hands the same Bridge to `exe-ssh` style clients that are
// not this page — so unsolicited frames would change a contract every client
// reads in order to decorate one page's chrome. And the meters are most useful
// exactly when the socket is not: a reading still lands while the terminal is
// between reconnects.
//
// Reading vitals is watching, not working. This route resolves through Get and
// never through EnsureRunning, and never calls Touch: a tab left open overnight
// must not keep a sandbox awake, which is the same promise the page's own
// refusal to auto-reconnect after a pause (index.html) is making.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/webui"
)

// VitalsReader reads live resource counters for a sandbox running on THIS
// machine. Satisfied by *host.Manager.
//
// Every method answers ok=false for a name it does not hold, which is what
// makes the fleet case correct without this package knowing what a fleet is: a
// balloon and a VMM process can only be asked of the host running them, so a
// sandbox placed on another node simply misses in the local manager's maps and
// reports nothing. The page then draws the meters it has and hides the rest —
// the same degradation the user console has always had (userconsole.machines).
type VitalsReader interface {
	// CPUSeconds is cumulative host CPU time of the VM process, in seconds.
	CPUSeconds(ctx context.Context, name string) (float64, bool)
	// MemStats is the guest's real memory use in MiB, from balloon statistics.
	MemStats(ctx context.Context, name string) (int64, bool)
	// NetCounters are the raw tap counters in bytes, guest's point of view.
	// They reset to zero whenever the sandbox is paused, resumed or cold-booted.
	NetCounters(ctx context.Context, name string) (rx, tx uint64, ok bool)
}

// vitals is the wire shape of one reading.
//
// The counters go out raw and the page derives the rates, matching what the
// user console already does with cpu_seconds. That keeps the server stateless —
// no per-viewer sample to keep, nothing to expire — and it puts the delta in
// the one place that knows whether its own samples are contiguous, which the
// server cannot know for a client that was backgrounded for ten minutes.
//
// AtMS is the server's clock at the moment of the reading, and it is the
// divisor the page must use. Timing the interval on the browser's clock instead
// would fold request latency into every rate: a response held up 300ms would
// show as a CPU spike that never happened.
type vitals struct {
	// State is the sandbox's lifecycle state ("running", "paused", ...). The
	// counters below are only ever present while it is running.
	State vmm.State `json:"state"`
	// AtMS is when the readings were taken, in Unix milliseconds.
	AtMS int64 `json:"at_ms"`

	// VCPUs and MemMB are the sandbox's ceilings — the denominators the page
	// needs to turn a counter into a percentage. Always present.
	VCPUs int64 `json:"vcpus"`
	MemMB int64 `json:"mem_mb"`
	// Ballooned means the idle reaper has reclaimed some of this guest's RAM to
	// the host, so MemMB is a ceiling it cannot currently reach.
	Ballooned bool `json:"ballooned,omitempty"`

	// Pointers, not zero values: a missing reading and a genuine zero are
	// different things, and the page draws them differently — one hides the
	// meter, the other shows an idle one.
	CPUSeconds *float64 `json:"cpu_seconds,omitempty"`
	MemUsedMB  *int64   `json:"mem_used_mb,omitempty"`
	NetRxBytes *uint64  `json:"net_rx_bytes,omitempty"`
	NetTxBytes *uint64  `json:"net_tx_bytes,omitempty"`

	// LifeRxBytes and LifeTxBytes are the sandbox's lifetime network totals,
	// which unlike the counters above survive a pause and a restart. They are
	// what the network readout names on hover; the plot never uses them.
	LifeRxBytes uint64 `json:"life_rx_bytes,omitempty"`
	LifeTxBytes uint64 `json:"life_tx_bytes,omitempty"`
}

// vitalsHandler answers GET /vitals for the sandbox this host names.
//
// It always answers 200 with the ceilings, even when no reading is available —
// a sandbox that is paused, on another node, or served by a build with no
// VitalsReader wired all produce the same shape with the counters absent. The
// alternative, a 404 or a 501, would make the page carry a second code path for
// a case it renders identically: no plot, just the state.
func (h *Handler) vitals(w http.ResponseWriter, r *http.Request) {
	box, ok := h.resolve(w, r)
	if !ok {
		return
	}
	out := vitals{
		State:       box.State,
		AtMS:        time.Now().UnixMilli(),
		VCPUs:       box.VCPUs,
		MemMB:       box.MemMB,
		Ballooned:   box.Ballooned,
		LifeRxBytes: box.NetRxBytes,
		LifeTxBytes: box.NetTxBytes,
	}
	h.readVitals(r.Context(), box, &out)

	// edgeauth.Require already sets no-store on everything behind the gate;
	// repeating it here means the guarantee does not depend on how this handler
	// happens to be mounted. A cached reading would freeze the meters at
	// whatever an intermediary last saw, which looks exactly like a hung guest.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	json.NewEncoder(w).Encode(out) //nolint:errcheck
}

// readVitals fills in whatever the local machine can answer about box.
//
// The three reads run concurrently under one budget, mirroring the user
// console's dashboard: they touch different things — /proc for CPU, sysfs for
// the tap, the VMM's API socket for the balloon — and only the last of those
// can be slow. Sequentially, one wedged VMM would cost the page its CPU plot
// too, and at a reading a second the latency would compound into a visibly
// stuttering chart.
func (h *Handler) readVitals(ctx context.Context, box *host.Sandbox, out *vitals) {
	if h.vitalsOf == nil || box.State != vmm.StateRunning {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, webui.ProbeTimeout)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if secs, ok := h.vitalsOf.CPUSeconds(ctx, box.Name); ok {
			out.CPUSeconds = &secs
		}
	}()
	go func() {
		defer wg.Done()
		if used, ok := h.vitalsOf.MemStats(ctx, box.Name); ok {
			out.MemUsedMB = &used
		}
	}()
	go func() {
		defer wg.Done()
		if rx, tx, ok := h.vitalsOf.NetCounters(ctx, box.Name); ok {
			out.NetRxBytes, out.NetTxBytes = &rx, &tx
		}
	}()
	wg.Wait()
}
