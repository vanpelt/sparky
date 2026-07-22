// Package report joins the eBPF per-remote-IP byte counters to the domains the
// DNS proxy resolved, producing a per-domain bandwidth breakdown. Addresses
// with no known domain (raw-IP connections, or names resolved before the meter
// started) are bucketed under their literal IP so nothing is silently dropped.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"text/tabwriter"
)

// Flow is the byte/packet tally the eBPF meter keeps for one remote address.
// Tx is guest→remote (upload), Rx is remote→guest (download).
type Flow struct {
	TxBytes uint64
	RxBytes uint64
	TxPkts  uint64
	RxPkts  uint64
}

// Total bytes in both directions.
func (f Flow) Total() uint64 { return f.TxBytes + f.RxBytes }

// Resolver maps an address to a domain label. *ipmap.Map satisfies it.
type Resolver interface {
	Domain(netip.Addr) (string, bool)
}

// DomainUsage is bandwidth aggregated to a single domain (or a raw-IP bucket).
type DomainUsage struct {
	Domain    string `json:"domain"`
	Resolved  bool   `json:"resolved"` // false ⇒ Domain is a literal IP bucket
	TxBytes   uint64 `json:"tx_bytes"`
	RxBytes   uint64 `json:"rx_bytes"`
	TxPkts    uint64 `json:"tx_pkts"`
	RxPkts    uint64 `json:"rx_pkts"`
	Addresses int    `json:"addresses"` // distinct remote IPs folded in
}

// Total bytes both directions.
func (d DomainUsage) Total() uint64 { return d.TxBytes + d.RxBytes }

// Aggregate folds per-IP flows into per-domain usage using r for the IP→domain
// join. Results are sorted by total bytes descending (ties broken by name) so
// the busiest destinations lead.
func Aggregate(flows map[netip.Addr]Flow, r Resolver) []DomainUsage {
	type acc struct {
		DomainUsage
		seen map[netip.Addr]struct{}
	}
	byKey := map[string]*acc{}
	get := func(key string, resolved bool) *acc {
		a := byKey[key]
		if a == nil {
			a = &acc{seen: map[netip.Addr]struct{}{}}
			a.Domain = key
			a.Resolved = resolved
			byKey[key] = a
		}
		return a
	}

	for addr, f := range flows {
		addr = addr.Unmap()
		key, resolved := addr.String(), false
		if r != nil {
			if d, ok := r.Domain(addr); ok {
				key, resolved = d, true
			}
		}
		a := get(key, resolved)
		a.TxBytes += f.TxBytes
		a.RxBytes += f.RxBytes
		a.TxPkts += f.TxPkts
		a.RxPkts += f.RxPkts
		if _, dup := a.seen[addr]; !dup {
			a.seen[addr] = struct{}{}
			a.Addresses++
		}
	}

	out := make([]DomainUsage, 0, len(byKey))
	for _, a := range byKey {
		out = append(out, a.DomainUsage)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total() != out[j].Total() {
			return out[i].Total() > out[j].Total()
		}
		return out[i].Domain < out[j].Domain
	})
	return out
}

// WriteTable renders usage as an aligned text table.
func WriteTable(w io.Writer, usage []DomainUsage) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DOMAIN\tIPS\tUP\tDOWN\tTOTAL")
	var tx, rx uint64
	for _, u := range usage {
		name := u.Domain
		if !u.Resolved {
			name += " (ip)"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", name, u.Addresses,
			humanBytes(u.TxBytes), humanBytes(u.RxBytes), humanBytes(u.Total()))
		tx += u.TxBytes
		rx += u.RxBytes
	}
	fmt.Fprintf(tw, "TOTAL\t\t%s\t%s\t%s\n", humanBytes(tx), humanBytes(rx), humanBytes(tx+rx))
	tw.Flush()
}

// WriteJSON renders usage as a JSON array.
func WriteJSON(w io.Writer, usage []DomainUsage) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(usage)
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
