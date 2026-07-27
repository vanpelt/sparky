// Package netpush is sparkbox's bridge to the sluice control socket
// (/run/sluice.sock). It translates the console's tag-scoped network rule-sets
// into the per-tap egress policy sluice enforces, and reads sluice's per-tap
// bandwidth back, re-labelled from tap name to VM name.
//
// The mapping between a VM and its tap is implicit in addressing: a running
// sandbox's HostIP is its guest address 172.30.<idx>.2, and its tap is
// sbtap<idx> (see internal/vmm/firecracker). So the console never needs to
// learn tap names — this package derives them.
//
// sluice's PUT /policy replaces the entire per-tap set, so every push sends the
// whole running fleet; a VM that has gone away simply drops out of the map and
// reverts to the base allowlist. Pushes are therefore always full snapshots.
package netpush

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"time"
)

// Sandbox is the slice of a running VM netpush needs: its name, owner, and the
// guest IP that identifies its tap. Satisfied by *host.Sandbox via an adapter
// in the caller.
type Sandbox struct {
	Name  string
	Owner string
	// HostIP is the guest address (172.30.<idx>.2). Empty for a paused VM,
	// which has no live tap and is skipped.
	HostIP string
}

// Fleet enumerates the running sandboxes across all owners.
type Fleet interface {
	List() []Sandbox
}

// Rules resolves a sandbox's merged egress allow-set from its tags, and whether
// it is governed at all (has a tag bound to a rule). Satisfied by
// *netrules.Store.AllowForSandbox.
type Rules interface {
	AllowForSandbox(sandbox, owner string) (allow []string, governed bool, err error)
}

// Client talks HTTP over the sluice Unix socket.
type Client struct {
	http *http.Client
}

// NewClient dials the socket at path for every request. The path itself is
// fixed per process; the dialer ignores the URL host.
func NewClient(path string) *Client {
	return &Client{http: &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
		},
	}}
}

// PutPolicy replaces sluice's per-tap allowlists. taps maps tap name
// (sbtap<idx>) to that tap's allow patterns.
func (c *Client) PutPolicy(ctx context.Context, taps map[string][]string) error {
	body, err := json.Marshal(policyBody{Taps: taps})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://sluice/policy", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("put policy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("put policy: sluice returned %s", resp.Status)
	}
	return nil
}

// Report fetches the raw per-tap bandwidth report from sluice.
func (c *Client) Report(ctx context.Context) (*Report, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sluice/report.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get report: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get report: sluice returned %s", resp.Status)
	}
	var rep Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		return nil, fmt.Errorf("decode report: %w", err)
	}
	return &rep, nil
}

type policyBody struct {
	Taps map[string][]string `json:"taps"`
}

// DomainUsage mirrors sluice's report.DomainUsage.
type DomainUsage struct {
	Domain    string `json:"domain"`
	Resolved  bool   `json:"resolved"`
	TxBytes   uint64 `json:"tx_bytes"`
	RxBytes   uint64 `json:"rx_bytes"`
	TxPkts    uint64 `json:"tx_pkts"`
	RxPkts    uint64 `json:"rx_pkts"`
	Addresses int    `json:"addresses"`
}

// TapUsage mirrors sluice's control.TapUsage.
type TapUsage struct {
	Tap     string        `json:"tap"`
	TxBytes uint64        `json:"tx_bytes"`
	RxBytes uint64        `json:"rx_bytes"`
	Domains []DomainUsage `json:"domains"`
}

// Report mirrors sluice's control.Report.
type Report struct {
	GeneratedAtUnix int64      `json:"generated_at_unix"`
	Taps            []TapUsage `json:"taps"`
}

// VMUsage is one VM's per-domain bandwidth, re-labelled from its tap.
type VMUsage struct {
	Name    string        `json:"name"`
	TxBytes uint64        `json:"tx_bytes"`
	RxBytes uint64        `json:"rx_bytes"`
	Domains []DomainUsage `json:"domains"`
}

// Syncer computes and pushes the full-fleet policy and attributes bandwidth back
// to VMs. It is safe for concurrent use; each method call is independent.
type Syncer struct {
	client *Client
	fleet  Fleet
	rules  Rules
	log    *slog.Logger
}

// NewSyncer wires a syncer. A nil client disables the socket calls (Push/Usage
// become no-ops returning nil), so the console still runs when sluice is absent.
func NewSyncer(client *Client, fleet Fleet, rules Rules, log *slog.Logger) *Syncer {
	if log == nil {
		log = slog.Default()
	}
	return &Syncer{client: client, fleet: fleet, rules: rules, log: log}
}

// Enabled reports whether a sluice socket is configured.
func (s *Syncer) Enabled() bool { return s.client != nil }

// Push recomputes every running VM's merged allow-set from its tags and pushes
// the whole fleet's per-tap policy to sluice in one call. Only governed VMs —
// those with a tag bound to a rule-set — are included; sluice enforces exactly
// the taps it receives. A VM with no tap (paused) or no governing rule is
// omitted, which in sluice's open-untagged mode leaves its egress unrestricted.
// A governed VM whose rules resolve to an empty allow-set is still sent (as a
// deliberate deny-all), distinct from an ungoverned one.
func (s *Syncer) Push(ctx context.Context) error {
	if s.client == nil {
		return nil
	}
	return s.Apply(ctx, s.Resolve())
}

// Resolve computes the allow-sets for the sandboxes this syncer's fleet holds,
// keyed by SANDBOX NAME.
//
// Split out of Push because in a fleet the two halves happen on different
// machines: the rules live in the gateway's store and the taps live wherever
// the VM does. The gateway calls Resolve for a node's sandboxes and sends the
// result; the node calls Apply on what arrives. On one machine Push is still
// both, back to back, which is the single-box deployment paying nothing for the
// split.
//
// Only governed sandboxes appear. An absent name means no rule governs it,
// which downstream is "leave it unrestricted"; a name present with an empty
// list is a deliberate deny-all. Keeping those two distinguishable is why the
// map is name-keyed rather than a list of names.
func (s *Syncer) Resolve() map[string][]string {
	allow := map[string][]string{}
	for _, b := range s.fleet.List() {
		set, governed, err := s.rules.AllowForSandbox(b.Name, b.Owner)
		if err != nil {
			s.log.Warn("resolve allow-set", "sandbox", b.Name, "err", err)
			continue
		}
		if !governed {
			continue // untagged / unpolicied VM → sluice leaves it unrestricted
		}
		if set == nil {
			set = []string{} // governed deny-all: send an explicit empty list
		}
		allow[b.Name] = set
	}
	return allow
}

// Apply turns a name-keyed allow map into the tap-keyed policy sluice enforces
// and PUTs the whole thing.
//
// The name -> tap resolution happens HERE, on the machine that assigned the
// slot, and that is the point of the split: sbtap3 is a different sandbox on
// every machine in the fleet, so a tap name is meaningless anywhere but where
// it was minted.
//
// A name this machine does not have running is dropped rather than refused. It
// is the ordinary race — a VM paused between the gateway resolving the policy
// and this call — and the correct handling is the same as for a VM that was
// never here: it has no tap, so there is nothing to enforce against, and the
// next full snapshot will agree.
func (s *Syncer) Apply(ctx context.Context, allow map[string][]string) error {
	if s.client == nil {
		return nil
	}
	byName := map[string]Sandbox{}
	for _, b := range s.fleet.List() {
		byName[b.Name] = b
	}
	taps := map[string][]string{}
	var unplaced int
	for name, set := range allow {
		b, ok := byName[name]
		if !ok {
			unplaced++
			continue
		}
		tap, ok := TapName(b.HostIP)
		if !ok {
			continue // paused or non-firecracker addressing: no live tap
		}
		if set == nil {
			set = []string{}
		}
		taps[tap] = set
	}
	if err := s.client.PutPolicy(ctx, taps); err != nil {
		return err
	}
	s.log.Info("pushed egress policy", "taps", len(taps), "not_running_here", unplaced)
	return nil
}

// Usage fetches the fleet bandwidth report and returns it keyed by VM name,
// filtered to owner (empty owner returns every VM). Tap names with no matching
// running VM are dropped.
func (s *Syncer) Usage(ctx context.Context, owner string) (map[string]VMUsage, error) {
	if s.client == nil {
		return map[string]VMUsage{}, nil
	}
	rep, err := s.client.Report(ctx)
	if err != nil {
		return nil, err
	}
	// tap name -> VM (name, owner), for the running fleet.
	byTap := map[string]Sandbox{}
	for _, b := range s.fleet.List() {
		if tap, ok := TapName(b.HostIP); ok {
			byTap[tap] = b
		}
	}
	out := map[string]VMUsage{}
	for _, tu := range rep.Taps {
		b, ok := byTap[tu.Tap]
		if !ok {
			continue // a tap we can't attribute to a current VM
		}
		if owner != "" && b.Owner != owner {
			continue
		}
		domains := tu.Domains
		sort.SliceStable(domains, func(i, j int) bool {
			return domains[i].TxBytes+domains[i].RxBytes > domains[j].TxBytes+domains[j].RxBytes
		})
		out[b.Name] = VMUsage{Name: b.Name, TxBytes: tu.TxBytes, RxBytes: tu.RxBytes, Domains: domains}
	}
	return out, nil
}

// TapName derives the tap device name from a guest HostIP of the form
// 172.30.<idx>.2. It returns false for any other address (a paused VM's empty
// HostIP, or a mock-driver 127.0.0.1), so callers skip VMs with no real tap.
func TapName(hostIP string) (string, bool) {
	addr, err := netip.ParseAddr(hostIP)
	if err != nil || !addr.Is4() {
		return "", false
	}
	o := addr.As4()
	if o[0] != 172 || o[1] != 30 || o[3] != 2 {
		return "", false
	}
	return fmt.Sprintf("sbtap%d", o[2]), true
}
