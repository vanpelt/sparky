// Package control exposes sluice's host-local control plane over a Unix domain
// socket: the sparkbox user console reads live per-tap (per-VM) bandwidth from
// it and pushes per-tap egress policy to it. The socket is root-owned and never
// leaves the box, so there is no auth or TLS — reachability is the permission.
//
// Two endpoints:
//
//	GET  /report.json  -> per-tap per-domain bandwidth (report.DomainUsage)
//	PUT  /policy       -> replace the per-tap allowlists (sbtap<idx> -> patterns)
//
// The report joins the eBPF per-tap byte counters to domains through the same
// IP→domain table the DNS proxy fills, and labels each bucket by tap name so
// the caller can map it to a VM.
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"time"

	"github.com/vanpelt/sparky/tools/sluice/internal/allowlist"
	"github.com/vanpelt/sparky/tools/sluice/internal/policy"
	"github.com/vanpelt/sparky/tools/sluice/internal/report"
)

// Meter is the subset of *meter.Meter the control plane reads.
type Meter interface {
	// FlowsByIface returns byte counters broken out per tap ifindex.
	FlowsByIface() (map[uint32]map[netip.Addr]report.Flow, error)
	// Ifaces maps each attached tap's ifindex to its name.
	Ifaces() map[uint32]string
}

// Server answers the control socket. A nil Meter degrades /report.json to an
// empty body (metering unavailable) but /policy still works.
type Server struct {
	meter    Meter
	resolver report.Resolver // ipmap: joins an address to its domain
	pol      *policy.Policy
	poke     func() // request an immediate reconcile after a policy change
	log      *slog.Logger
}

// New builds a control server. poke may be nil.
func New(m Meter, resolver report.Resolver, pol *policy.Policy, poke func(), log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if poke == nil {
		poke = func() {}
	}
	return &Server{meter: m, resolver: resolver, pol: pol, poke: poke, log: log}
}

// Handler returns the HTTP mux, exposed for testing without a real socket.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /report.json", s.getReport)
	mux.HandleFunc("PUT /policy", s.putPolicy)
	return mux
}

// Serve listens on the Unix socket at path until ctx is done. It removes a stale
// socket file first and restricts the socket to the owner + group.
func (s *Server) Serve(ctx context.Context, path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil {
		s.log.Warn("chmod control socket", "path", path, "err", err)
	}
	srv := &http.Server{Handler: s.Handler()}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	s.log.Info("control socket listening", "path", path)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// sortTaps orders taps by descending total bytes, ties broken by name, so the
// busiest VMs lead and the output is stable.
func sortTaps(taps []TapUsage) {
	sort.Slice(taps, func(i, j int) bool {
		ti, tj := taps[i].TxBytes+taps[i].RxBytes, taps[j].TxBytes+taps[j].RxBytes
		if ti != tj {
			return ti > tj
		}
		return taps[i].Tap < taps[j].Tap
	})
}

// TapUsage is one tap's per-domain bandwidth breakdown.
type TapUsage struct {
	Tap     string               `json:"tap"`
	TxBytes uint64               `json:"tx_bytes"`
	RxBytes uint64               `json:"rx_bytes"`
	Domains []report.DomainUsage `json:"domains"`
}

// Report is the /report.json body: one entry per attached tap, plus a timestamp.
type Report struct {
	GeneratedAtUnix int64      `json:"generated_at_unix"`
	Taps            []TapUsage `json:"taps"`
}

func (s *Server) getReport(w http.ResponseWriter, r *http.Request) {
	rep := Report{GeneratedAtUnix: time.Now().Unix(), Taps: []TapUsage{}}
	if s.meter != nil {
		byIf, err := s.meter.FlowsByIface()
		if err != nil {
			s.log.Warn("read flows", "err", err)
			http.Error(w, "read flows: "+err.Error(), http.StatusInternalServerError)
			return
		}
		names := s.meter.Ifaces()
		for ifindex, flows := range byIf {
			name := names[ifindex]
			if name == "" {
				name = fmt.Sprintf("if%d", ifindex)
			}
			usage := report.Aggregate(flows, s.resolver)
			tu := TapUsage{Tap: name, Domains: usage}
			for _, d := range usage {
				tu.TxBytes += d.TxBytes
				tu.RxBytes += d.RxBytes
			}
			rep.Taps = append(rep.Taps, tu)
		}
	}
	sortTaps(rep.Taps)
	writeJSON(w, rep)
}

// policyRequest is the PUT /policy body: per-tap allow patterns. A tap present
// here becomes a policied (enforced) tap — even with an empty pattern list,
// which is a deny-all. A tap the caller omits reverts to base-only and, in the
// daemon's open-untagged mode, to unrestricted egress. Callers therefore send
// only the taps they want enforced.
type policyRequest struct {
	Taps map[string][]string `json:"taps"`
}

func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request) {
	var req policyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	taps := make(map[string]*allowlist.List, len(req.Taps))
	for tap, patterns := range req.Taps {
		l, err := allowlist.New(patterns)
		if err != nil {
			http.Error(w, fmt.Sprintf("tap %q: %v", tap, err), http.StatusBadRequest)
			return
		}
		taps[tap] = l
	}
	s.pol.ReplaceTaps(taps)
	s.poke() // apply the new grants without waiting for the next sync tick
	s.log.Info("policy updated", "taps", len(taps))
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
