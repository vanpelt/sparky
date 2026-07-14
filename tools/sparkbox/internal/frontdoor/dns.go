package frontdoor

import (
	"context"
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
	log    *slog.Logger
	queue  chan func()
}

// NewPublisher builds a Cloudflare-backed publisher for <name>.<domain>
// records. apiToken needs Zone.DNS:Edit on the domain's zone (the same scope
// the DNS-01 TLS provider uses).
func NewPublisher(m *Mapper, domain, apiToken string, log *slog.Logger) *Publisher {
	return newPublisher(m, domain, &cloudflare.Provider{APIToken: apiToken}, log)
}

func newPublisher(m *Mapper, domain string, dns dnsProvider, log *slog.Logger) *Publisher {
	p := &Publisher{
		mapper: m,
		zone:   domain + ".",
		dns:    dns,
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

// Ensure upserts name's AAAA record. The caller's ctx is deliberately not
// used: it belongs to a create request that will be gone long before DNS
// needs it.
func (p *Publisher) Ensure(_ context.Context, name string) {
	addr, ok := netip.AddrFromSlice(p.mapper.Addr(name))
	if !ok {
		p.log.Error("front-door address unrepresentable", "name", name)
		return
	}
	p.enqueue("publish", name, func(ctx context.Context) error {
		_, err := p.dns.SetRecords(ctx, p.zone, []libdns.Record{
			libdns.Address{Name: name, TTL: recordTTL, IP: addr},
		})
		return err
	})
}

// Remove deletes name's AAAA record (sandbox destroyed).
func (p *Publisher) Remove(_ context.Context, name string) {
	addr, _ := netip.AddrFromSlice(p.mapper.Addr(name))
	p.enqueue("delete", name, func(ctx context.Context) error {
		_, err := p.dns.DeleteRecords(ctx, p.zone, []libdns.Record{
			libdns.Address{Name: name, TTL: recordTTL, IP: addr},
		})
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
