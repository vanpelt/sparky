package frontdoor

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/libdns/cloudflare"
	"github.com/libdns/libdns"
)

// recordTTL is the AAAA TTL. Short, so a freshly created sandbox's name
// resolves to its front door quickly — the wildcard record answers in the
// meantime (pointing at the shared edge), so there is never an NXDOMAIN to
// negative-cache, just a briefly stale target.
const recordTTL = 60 * time.Second

// dnsTimeout bounds one publish/delete round-trip to the DNS API.
const dnsTimeout = 30 * time.Second

// dnsProvider is the slice of libdns the publisher uses; *cloudflare.Provider
// satisfies it, and tests substitute a recorder.
type dnsProvider interface {
	SetRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error)
	DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error)
}

// Publisher keeps a per-name AAAA record (<name>.<domain> -> front door) in
// sync as sandboxes come and go, so `ssh <name>.<domain>` reaches the right
// address. It implements host.FrontDoor, same as Plumber.
//
// All DNS work is asynchronous through a single ordered worker: the manager
// invokes front-door hooks under its lock, and a Cloudflare round-trip (or
// outage) must never stall sandbox operations. Ordering per name is preserved
// (a create's publish always lands before a later destroy's delete), failures
// are logged and left for the next Ensure — main re-Ensures every sandbox at
// startup, which doubles as drift repair.
type Publisher struct {
	mapper *Mapper
	zone   string // libdns zone, e.g. "hivemind.tools."
	dns    dnsProvider
	edgeV4 netip.Addr // shared proxy-edge IPv4; zero value = don't publish an A
	log    *slog.Logger
	queue  chan func()
}

// NewPublisher builds a Cloudflare-backed publisher for <name>.<domain>
// records. apiToken needs Zone.DNS:Edit on the domain's zone (the same scope
// the DNS-01 TLS provider uses). edgeV4, when valid, is the shared proxy-edge
// IPv4 published as a per-name A record (see Ensure); pass the zero Addr to
// publish AAAA only.
func NewPublisher(m *Mapper, domain, apiToken string, edgeV4 netip.Addr, log *slog.Logger) *Publisher {
	return newPublisher(m, domain, &cloudflare.Provider{APIToken: apiToken}, edgeV4, log)
}

func newPublisher(m *Mapper, domain string, dns dnsProvider, edgeV4 netip.Addr, log *slog.Logger) *Publisher {
	p := &Publisher{
		mapper: m,
		zone:   domain + ".",
		dns:    dns,
		edgeV4: edgeV4,
		log:    log,
		queue:  make(chan func(), 256),
	}
	go func() {
		for job := range p.queue {
			job()
		}
	}()
	return p
}

// records is the DNS record set for name: always the per-name front-door AAAA,
// plus an A at the shared proxy edge when edgeV4 is configured.
//
// The A matters because a per-name AAAA *shadows the wildcard A*: in Cloudflare
// (and per RFC 4592) an explicit record of any type at a name disables the
// wildcard for that name across all types, so once we publish the AAAA the name
// has no A at all and is unreachable over IPv4 — HTTP included. Republishing an
// A at the same edge the wildcard points to restores v4: the proxy routes by
// Host header, so one shared edge address serves every name. SSH-by-address is
// unaffected (it only ever uses the v6 front door). Returns ok=false if the
// front-door address is unrepresentable.
func (p *Publisher) records(name string) ([]libdns.Record, bool) {
	addr, ok := netip.AddrFromSlice(p.mapper.Addr(name))
	if !ok {
		return nil, false
	}
	recs := []libdns.Record{libdns.Address{Name: name, TTL: recordTTL, IP: addr}}
	if p.edgeV4.IsValid() {
		recs = append(recs, libdns.Address{Name: name, TTL: recordTTL, IP: p.edgeV4})
	}
	return recs, true
}

// Ensure upserts name's front-door records (AAAA, plus A when edgeV4 is set).
// The caller's ctx is deliberately not used: it belongs to a create request
// that will be gone long before DNS needs it.
func (p *Publisher) Ensure(_ context.Context, name string) {
	recs, ok := p.records(name)
	if !ok {
		p.log.Error("front-door address unrepresentable", "name", name)
		return
	}
	p.enqueue("publish", name, func(ctx context.Context) error {
		_, err := p.dns.SetRecords(ctx, p.zone, recs)
		return err
	})
}

// Remove deletes name's front-door records (sandbox destroyed).
func (p *Publisher) Remove(_ context.Context, name string) {
	recs, ok := p.records(name)
	if !ok {
		return
	}
	p.enqueue("delete", name, func(ctx context.Context) error {
		_, err := p.dns.DeleteRecords(ctx, p.zone, recs)
		return err
	})
}

func (p *Publisher) enqueue(op, name string, job func(context.Context) error) {
	select {
	case p.queue <- func() {
		ctx, cancel := context.WithTimeout(context.Background(), dnsTimeout)
		defer cancel()
		if err := job(ctx); err != nil {
			p.log.Warn("front-door dns "+op+" failed", "name", name, "err", err)
			return
		}
		p.log.Info("front-door dns "+op, "name", name, "zone", p.zone)
	}:
	default:
		// A full queue means the DNS API has been failing/slow for hundreds of
		// ops; dropping is safe (startup re-Ensure repairs) — blocking is not.
		p.log.Warn("front-door dns queue full, dropping", "op", op, "name", name)
	}
}

// flush blocks until every queued job has run (test synchronization).
func (p *Publisher) flush() {
	done := make(chan struct{})
	p.queue <- func() { close(done) }
	<-done
}

// wildcardTTL is the TTL on a *.<label> wildcard. Longer than recordTTL on
// purpose: this record is published once at startup and points at a fixed edge
// address, so a short TTL would buy no agility and cost a query per lookup.
const wildcardTTL = 5 * time.Minute

// wildcardDNS is the libdns slice PublishWildcard needs. It reads before it
// writes, which the per-name Publisher never has to do: per-name records are
// sparkbox's own bookkeeping, while a zone-wide wildcard is scenery an operator
// may have configured by hand and must not lose silently.
type wildcardDNS interface {
	GetRecords(ctx context.Context, zone string) ([]libdns.Record, error)
	SetRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error)
}

// PublishWildcard upserts "*.<label>.<domain>" as A/AAAA records pointing at
// the shared proxy edge, so every <name>.<label>.<domain> resolves without a
// per-sandbox record. It is a one-shot startup publish: the record describes
// the host, not any sandbox, so there is deliberately no Remove — and nothing
// under the label may ever get a per-name record either, because an explicit
// record at a name disables the wildcard for that name across all types
// (RFC 4592), which is the same trap the front-door AAAA already fell into.
//
// It handles the direct-IP edge shape only. In a Cloudflare Tunnel deployment
// the wildcard must instead be a PROXIED CNAME to <tunnel-uuid>.cfargotunnel.com
// — sparkbox does not know the tunnel's UUID, and the tunnel deploy path
// deliberately withholds CLOUDFLARE_API_TOKEN from it — so there the record is
// the operator's to create with `cloudflared tunnel route dns`. An existing
// CNAME at the name is recognised as exactly that case and left alone.
func PublishWildcard(ctx context.Context, domain, label, apiToken string, addrs []netip.Addr, log *slog.Logger) error {
	return publishWildcard(ctx, &cloudflare.Provider{APIToken: apiToken}, domain, label, addrs, log)
}

func publishWildcard(ctx context.Context, dns wildcardDNS, domain, label string, addrs []netip.Addr, log *slog.Logger) error {
	name, zone := "*."+label, domain+"."
	fqdn := name + "." + domain

	recs := make([]libdns.Record, 0, len(addrs))
	for _, a := range addrs {
		// Unmap first: a 4-in-6 address reports Is6() and would be published as
		// an AAAA holding an IPv4 literal, which resolves to nothing.
		if a = a.Unmap(); a.IsValid() {
			recs = append(recs, libdns.Address{Name: name, TTL: wildcardTTL, IP: a})
		}
	}
	if len(recs) == 0 {
		return fmt.Errorf("no usable edge address to publish %s at", fqdn)
	}

	if existing, err := dns.GetRecords(ctx, zone); err != nil {
		// Listing is a courtesy check, and Zone:Read is a separate scope from
		// the Zone.DNS:Edit this feature actually needs — so a failure here
		// downgrades to publishing blind rather than refusing to publish.
		log.Warn("could not list zone records; publishing wildcard without a clobber check",
			"zone", zone, "name", fqdn, "err", err)
	} else {
		for _, rec := range existing {
			rr := rec.RR()
			if rr.Name != name {
				continue
			}
			if rr.Type == "CNAME" {
				// The Cloudflare Tunnel shape. Replacing it with an A at our
				// own edge would take the terminal offline in exactly the
				// deployment where it was working, and libdns could not even
				// put it back (it sets Proxied only for a .cfargotunnel.com
				// target). Leave it and say who owns it.
				log.Warn("wildcard already exists as a CNAME; leaving it alone",
					"name", fqdn, "target", rr.Data,
					"note", "tunnel deployments publish this with `cloudflared tunnel route dns`")
				return nil
			}
			if (rr.Type == "A" || rr.Type == "AAAA") && !publishes(recs, rr) {
				log.Warn("replacing an existing wildcard record that points elsewhere",
					"name", fqdn, "type", rr.Type, "was", rr.Data)
			}
		}
	}

	if _, err := dns.SetRecords(ctx, zone, recs); err != nil {
		return fmt.Errorf("publish %s: %w", fqdn, err)
	}
	log.Info("wildcard dns published", "name", fqdn, "records", len(recs), "ttl", wildcardTTL)
	return nil
}

// publishes reports whether recs already carries rr's type and data, i.e.
// whether writing recs leaves that record where it is.
func publishes(recs []libdns.Record, rr libdns.RR) bool {
	for _, rec := range recs {
		if got := rec.RR(); got.Type == rr.Type && got.Data == rr.Data {
			return true
		}
	}
	return false
}

// Hook is the per-sandbox lifecycle contract shared by Plumber and Publisher
// (structurally identical to host.FrontDoor, restated here to avoid a
// dependency on the host package).
type Hook interface {
	Ensure(ctx context.Context, name string)
	Remove(ctx context.Context, name string)
}

// Multi fans one lifecycle event out to several hooks (plumbing + DNS).
type Multi []Hook

func (m Multi) Ensure(ctx context.Context, name string) {
	for _, h := range m {
		h.Ensure(ctx, name)
	}
}

func (m Multi) Remove(ctx context.Context, name string) {
	for _, h := range m {
		h.Remove(ctx, name)
	}
}
