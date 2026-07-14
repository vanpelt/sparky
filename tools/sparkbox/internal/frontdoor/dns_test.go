package frontdoor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/libdns/libdns"
)

// recordingDNS captures libdns calls; fail makes every call error.
type recordingDNS struct {
	mu   sync.Mutex
	sets []libdns.Record
	dels []libdns.Record
	fail bool
}

func (r *recordingDNS) SetRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return nil, errors.New("cloudflare says no")
	}
	if zone != "hivemind.tools." {
		return nil, errors.New("wrong zone " + zone)
	}
	r.sets = append(r.sets, recs...)
	return recs, nil
}

func (r *recordingDNS) DeleteRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return nil, errors.New("cloudflare says no")
	}
	if zone != "hivemind.tools." {
		return nil, errors.New("wrong zone " + zone)
	}
	r.dels = append(r.dels, recs...)
	return recs, nil
}

func newTestPublisher(t *testing.T) (*Publisher, *recordingDNS, *Mapper) {
	t.Helper()
	m := mustMapper(t)
	dns := &recordingDNS{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newPublisher(m, "hivemind.tools", dns, log), dns, m
}

func TestPublisherEnsurePublishesAAAA(t *testing.T) {
	p, dns, m := newTestPublisher(t)
	ctx := context.Background()

	p.Ensure(ctx, "fuzzy-otter")
	p.flush()

	if len(dns.sets) != 1 {
		t.Fatalf("SetRecords calls = %d, want 1", len(dns.sets))
	}
	rr := dns.sets[0].RR()
	if rr.Type != "AAAA" || rr.Name != "fuzzy-otter" {
		t.Fatalf("unexpected record %+v", rr)
	}
	if want := m.Addr("fuzzy-otter").String(); rr.Data != want {
		t.Fatalf("record data = %s, want %s", rr.Data, want)
	}
	if rr.TTL != recordTTL {
		t.Fatalf("record ttl = %s, want %s", rr.TTL, recordTTL)
	}
}

func TestPublisherRemoveDeletesRecord(t *testing.T) {
	p, dns, m := newTestPublisher(t)
	ctx := context.Background()

	p.Ensure(ctx, "fuzzy-otter")
	p.Remove(ctx, "fuzzy-otter")
	p.flush()

	// Ordered worker: the publish always lands before the delete.
	if len(dns.sets) != 1 || len(dns.dels) != 1 {
		t.Fatalf("sets=%d dels=%d, want 1/1", len(dns.sets), len(dns.dels))
	}
	rr := dns.dels[0].RR()
	if rr.Type != "AAAA" || rr.Name != "fuzzy-otter" || rr.Data != m.Addr("fuzzy-otter").String() {
		t.Fatalf("unexpected delete %+v", rr)
	}
}

func TestPublisherSurvivesAPIFailure(t *testing.T) {
	p, dns, _ := newTestPublisher(t)
	dns.fail = true

	// Must not block or panic — failures are logged and retried by the next
	// Ensure (e.g. the startup reconcile pass).
	p.Ensure(context.Background(), "fuzzy-otter")
	p.flush()

	dns.fail = false
	p.Ensure(context.Background(), "fuzzy-otter")
	p.flush()
	if len(dns.sets) != 1 {
		t.Fatalf("recovery publish missing: sets=%d", len(dns.sets))
	}
}

func TestMultiFansOut(t *testing.T) {
	var a, b recordingHook
	m := Multi{&a, &b}
	m.Ensure(context.Background(), "x")
	m.Remove(context.Background(), "x")
	for _, h := range []*recordingHook{&a, &b} {
		if h.ensured != 1 || h.removed != 1 {
			t.Fatalf("hook calls = %+v", h)
		}
	}
}

type recordingHook struct{ ensured, removed int }

func (h *recordingHook) Ensure(context.Context, string) { h.ensured++ }
func (h *recordingHook) Remove(context.Context, string) { h.removed++ }
