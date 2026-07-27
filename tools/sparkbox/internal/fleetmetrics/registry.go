// Package fleetmetrics owns Sparkbox's transport-neutral fleet metrics.
//
// The package deliberately exposes operations rather than Prometheus vectors.
// Callers cannot add labels, so request IDs, sandbox names, owners, and other
// unbounded values cannot accidentally become part of the fleet's time series.
package fleetmetrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry is an isolated fleet metrics registry. It does not register with
// prometheus.DefaultRegisterer, which keeps tests independent and lets one
// process explicitly decide which private listener exposes these metrics.
type Registry struct {
	reg *prometheus.Registry

	controlRPC      *prometheus.HistogramVec
	controlPending  *prometheus.GaugeVec
	controlInFlight *prometheus.GaugeVec
	controlQueue    *prometheus.GaugeVec
	dropped         *prometheus.CounterVec
	reconnects      *prometheus.CounterVec
	liveness        *prometheus.CounterVec
	disconnects     *prometheus.CounterVec
	liveStreams     *prometheus.GaugeVec
	streamOpen      *prometheus.HistogramVec
	streamBytes     *prometheus.CounterVec
	routeFallback   *prometheus.CounterVec
	shadowInventory *prometheus.CounterVec
	ensureReady     *prometheus.CounterVec
	proxyTTFB       *prometheus.HistogramVec
	managerSave     *prometheus.HistogramVec
	activityFlush   *prometheus.CounterVec
	activityMarks   *prometheus.CounterVec
}

// New returns an empty registry containing the fleet transport collectors.
func New() *Registry {
	r := &Registry{reg: prometheus.NewRegistry()}
	r.controlRPC = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sparkbox_fleet_control_rpc_duration_seconds",
		Help:    "Control-plane request duration by operation, transport, and bounded outcome.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 21),
	}, []string{"node", "transport", "operation", "outcome"})
	r.controlPending = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sparkbox_fleet_control_pending_requests",
		Help: "Control requests sent and awaiting replies.",
	}, []string{"node", "transport"})
	r.controlInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sparkbox_fleet_control_in_flight_requests",
		Help: "Control requests currently executing on the receiving peer.",
	}, []string{"node", "transport", "operation"})
	r.controlQueue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sparkbox_fleet_control_write_queue_depth",
		Help: "Frames waiting in the bounded control transport write queue.",
	}, []string{"node", "transport"})
	r.dropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sparkbox_fleet_control_dropped_total",
		Help: "Control replies or events dropped at a bounded queue.",
	}, []string{"node", "transport", "kind"})
	r.reconnects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sparkbox_fleet_reconnects_total",
		Help: "Node control links that had been established and then reconnected.",
	}, []string{"node", "transport"})
	r.liveness = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sparkbox_fleet_liveness_failures_total",
		Help: "Failed active liveness probes by the side which detected them.",
	}, []string{"node", "transport", "side"})
	r.disconnects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sparkbox_fleet_disconnects_total",
		Help: "Ended control links classified into a bounded reason taxonomy.",
	}, []string{"node", "transport", "reason"})
	r.liveStreams = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sparkbox_fleet_guest_streams",
		Help: "Currently live guest streams by node, transport, and stream kind.",
	}, []string{"node", "transport", "kind"})
	r.streamOpen = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sparkbox_fleet_guest_stream_open_duration_seconds",
		Help:    "Guest stream-open duration and bounded outcome.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 16),
	}, []string{"node", "transport", "kind", "outcome"})
	r.streamBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sparkbox_fleet_guest_stream_bytes_total",
		Help: "Guest stream bytes from the gateway's point of view; never labelled by sandbox.",
	}, []string{"node", "transport", "kind", "direction"})
	r.routeFallback = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sparkbox_fleet_guest_route_fallback_total",
		Help: "Routed guest attempts that reached the SSH fallback decision, by bounded outcome.",
	}, []string{"node", "outcome"})
	r.shadowInventory = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sparkbox_fleet_control_shadow_inventory_total",
		Help: "SSH/gRPC inventory shadow comparisons by bounded outcome.",
	}, []string{"node", "outcome"})
	r.ensureReady = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sparkbox_fleet_ensure_ready_total",
		Help: "Sandbox readiness decisions classified into warm, resume, restore, and stale-state retry paths.",
	}, []string{"node", "classification"})
	r.proxyTTFB = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sparkbox_proxy_time_to_first_byte_seconds",
		Help:    "Proxy time to the first upstream response byte for warm and cold sandbox requests.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 21),
	}, []string{"node", "temperature"})
	r.managerSave = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sparkbox_manager_save_duration_seconds",
		Help:    "Duration of atomic sandbox-state saves by node and bounded outcome.",
		Buckets: prometheus.ExponentialBuckets(0.0005, 2, 18),
	}, []string{"node", "outcome"})
	r.activityFlush = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sparkbox_manager_activity_flush_total",
		Help: "Activity flush attempts by node and bounded outcome.",
	}, []string{"node", "outcome"})
	r.activityMarks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sparkbox_manager_activity_flushed_marks_total",
		Help: "Dirty activity timestamps included in successful flush attempts.",
	}, []string{"node"})

	r.reg.MustRegister(
		r.controlRPC,
		r.controlPending,
		r.controlInFlight,
		r.controlQueue,
		r.dropped,
		r.reconnects,
		r.liveness,
		r.disconnects,
		r.liveStreams,
		r.streamOpen,
		r.streamBytes,
		r.routeFallback,
		r.shadowInventory,
		r.ensureReady,
		r.proxyTTFB,
		r.managerSave,
		r.activityFlush,
		r.activityMarks,
	)
	return r
}

// Handler returns the Prometheus-compatible private metrics endpoint.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{})
}

func (r *Registry) ObserveControlRPC(node, transport, operation, outcome string, d time.Duration) {
	if r == nil {
		return
	}
	r.controlRPC.WithLabelValues(node, transport, operation, outcome).Observe(d.Seconds())
}

func (r *Registry) AddPending(node, transport string, delta float64) {
	if r == nil {
		return
	}
	r.controlPending.WithLabelValues(node, transport).Add(delta)
}

func (r *Registry) AddInFlight(node, transport, operation string, delta float64) {
	if r == nil {
		return
	}
	r.controlInFlight.WithLabelValues(node, transport, operation).Add(delta)
}

func (r *Registry) SetWriteQueueDepth(node, transport string, depth int) {
	if r == nil {
		return
	}
	r.controlQueue.WithLabelValues(node, transport).Set(float64(depth))
}

func (r *Registry) IncDropped(node, transport, kind string) {
	if r == nil {
		return
	}
	r.dropped.WithLabelValues(node, transport, kind).Inc()
}

func (r *Registry) IncReconnect(node, transport string) {
	if r == nil {
		return
	}
	r.reconnects.WithLabelValues(node, transport).Inc()
}

func (r *Registry) IncLivenessFailure(node, transport, side string) {
	if r == nil {
		return
	}
	r.liveness.WithLabelValues(node, transport, side).Inc()
}

func (r *Registry) IncDisconnect(node, transport, reason string) {
	if r == nil {
		return
	}
	r.disconnects.WithLabelValues(node, transport, reason).Inc()
}

func (r *Registry) ObserveStreamOpen(node, transport, kind, outcome string, d time.Duration) {
	if r == nil {
		return
	}
	r.streamOpen.WithLabelValues(node, transport, kind, outcome).Observe(d.Seconds())
}

func (r *Registry) AddLiveStreams(node, transport, kind string, delta float64) {
	if r == nil {
		return
	}
	r.liveStreams.WithLabelValues(node, transport, kind).Add(delta)
}

func (r *Registry) AddStreamBytes(node, transport, kind, direction string, n int) {
	if r == nil || n <= 0 {
		return
	}
	r.streamBytes.WithLabelValues(node, transport, kind, direction).Add(float64(n))
}

func (r *Registry) IncRouteFallback(node, outcome string) {
	if r == nil {
		return
	}
	r.routeFallback.WithLabelValues(node, outcome).Inc()
}

func (r *Registry) IncShadowInventory(node, outcome string) {
	if r == nil {
		return
	}
	r.shadowInventory.WithLabelValues(node, outcome).Inc()
}

func (r *Registry) IncEnsureReady(node, classification string) {
	if r == nil {
		return
	}
	r.ensureReady.WithLabelValues(node, classification).Inc()
}

func (r *Registry) ObserveProxyTTFB(node, temperature string, d time.Duration) {
	if r == nil {
		return
	}
	r.proxyTTFB.WithLabelValues(node, temperature).Observe(d.Seconds())
}

func (r *Registry) ObserveManagerSave(node, outcome string, d time.Duration) {
	if r == nil {
		return
	}
	r.managerSave.WithLabelValues(node, outcome).Observe(d.Seconds())
}

func (r *Registry) ObserveActivityFlush(node, outcome string, marks int) {
	if r == nil {
		return
	}
	r.activityFlush.WithLabelValues(node, outcome).Inc()
	if outcome == "ok" && marks > 0 {
		r.activityMarks.WithLabelValues(node).Add(float64(marks))
	}
}
