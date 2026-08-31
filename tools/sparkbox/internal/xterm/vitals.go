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
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/webui"
)

// VitalsReader reads live resource counters for a sandbox, wherever it runs.
// It is webui's, shared with both consoles, because all three surfaces draw the
// same meters from the same numbers under the same budget — see webui.Probe.
// Satisfied by *fleet.Fleet, and by *host.Manager for a build with no fleet.
//
// It is one method rather than the three readers underneath it because the
// fleet made "all three at once" the unit that crosses a machine boundary: a
// balloon and a VMM process can only be asked of the host running them, so a
// sandbox on another node is asked over the link, and three round trips a
// second per open tab is not a wire protocol. The reading that comes back is
// all-pointers for the same reason the wire shape below is — a missing counter
// and a zero counter are different facts, and the page draws them differently.
type VitalsReader = webui.VitalsReader

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
	// Name is the sandbox this page is attached to, resolved by the server.
	//
	// The page cannot work it out for itself and used to try: the host is
	// `<name>-<subdomain>.<zone>`, one label, so the obvious
	// `hostname.split(".")[0]` yields `demo-xterm` — which is what the header
	// and the turbo dialog both said. The subdomain is configurable, so there
	// is no suffix the page can safely strip either. It is one field on a
	// response the page already makes, and it is the only authority.
	Name string `json:"name"`
	// SSH is the whole command that opens this same shell from a terminal,
	// ready to paste. Absent when this host advertises no SSH endpoint.
	//
	// It is composed here and not in the page because only the server knows the
	// ADVERTISED host and port — an edge DNAT means the port people type is not
	// the port the gateway binds, and the page has no way to learn either.
	SSH string `json:"ssh,omitempty"`
	// Console is the user console's URL, for the menu's one link off this page.
	// Absent when the host runs no console.
	Console string `json:"console,omitempty"`
	// Proxy is the sandbox's default HTTPS endpoint. Its portless URL follows
	// the route store's current default port, including changes made while this
	// terminal is open.
	Proxy string `json:"proxy,omitempty"`

	// State is the sandbox's lifecycle state ("running", "paused", ...). The
	// counters below are only ever present while it is running.
	State vmm.State `json:"state"`
	// AtMS is when the readings were taken, in Unix milliseconds.
	AtMS int64 `json:"at_ms"`

	// VCPUs and MemMB are the sandbox's ceilings — the denominators the page
	// needs to turn a counter into a percentage. Always present.
	VCPUs int64 `json:"vcpus"`
	MemMB int64 `json:"mem_mb"`
	// Turbo says the two above are a doubled allocation borrowed for this run.
	// It rides the poll the page already makes rather than getting a route of
	// its own: the lamp has to go dark the moment the idle reaper hands the
	// resources back, which is a thing that happens to the sandbox and not a
	// thing this page did.
	Turbo bool `json:"turbo,omitempty"`
	// TurboAvailable says this host will accept POST /turbo at all. The page
	// hides the button rather than offering one that answers 501.
	TurboAvailable bool `json:"turbo_available,omitempty"`
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
		Name:           box.Name,
		Console:        h.consoleURL,
		State:          box.State,
		AtMS:           time.Now().UnixMilli(),
		VCPUs:          box.VCPUs,
		MemMB:          box.MemMB,
		Turbo:          box.Turbo,
		TurboAvailable: h.turbocharger != nil,
		Ballooned:      box.Ballooned,
		LifeRxBytes:    box.NetRxBytes,
		LifeTxBytes:    box.NetTxBytes,
	}
	if h.sshCommand != nil {
		out.SSH = h.sshCommand(box.Name)
	}
	if h.proxyURL != nil {
		out.Proxy = h.proxyURL(box.Name)
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

// readVitals fills in whatever the machine holding box can answer about it.
//
// The reading, its budget and what counts as "no reading" are all webui's, so
// that this page and the two dashboards cannot drift on any of them. What is
// left here is the one thing this surface does differently: an error is logged
// at debug rather than dropped silently, because a terminal is attached to
// exactly one sandbox and "the meters went blank" has a cause worth naming when
// somebody goes looking.
func (h *Handler) readVitals(ctx context.Context, box *host.Sandbox, out *vitals) {
	v, err := h.probe.Vitals(ctx, h.vitalsOf, box)
	if err != nil {
		// Debug, not warn: on a fleet with a node that has gone away this fires
		// once a second per open tab, and the fact is already visible where it
		// is actionable (`ctl node ls`, and the console's own offline badge).
		h.log.Debug("vitals unavailable", "sandbox", box.Name, "node", box.Node, "err", err)
		return
	}
	out.CPUSeconds, out.MemUsedMB = v.CPUSeconds, v.MemUsedMB
	out.NetRxBytes, out.NetTxBytes = v.NetRxBytes, v.NetTxBytes
}
