package frontdoor

// The one-shot *.<label> wildcard publish. What is worth pinning here is record
// SHAPE (a wrong name or an AAAA holding an IPv4 literal is a silent NXDOMAIN
// nobody can debug from the logs) and the clobber policy (a tunnel deployment's
// CNAME must survive contact with a box that has a Cloudflare token).

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/libdns/libdns"
)

// listingDNS is a recordingDNS that can also be asked what is already in the
// zone, which is the half PublishWildcard needs and Publisher never does.
type listingDNS struct {
	mu       sync.Mutex
	existing []libdns.Record
	sets     []libdns.Record
	getErr   error
	setErr   error
}

func (l *listingDNS) GetRecords(_ context.Context, zone string) ([]libdns.Record, error) {
	if zone != "hivemind.tools." {
		return nil, errors.New("wrong zone " + zone)
	}
	return l.existing, l.getErr
}

func (l *listingDNS) SetRecords(_ context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	if zone != "hivemind.tools." {
		return nil, errors.New("wrong zone " + zone)
	}
	if l.setErr != nil {
		return nil, l.setErr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sets = append(l.sets, recs...)
	return recs, nil
}

// capturingLog returns a logger and a func reading everything written to it, so
// a test can assert that a destructive-looking action was actually announced.
func capturingLog() (*slog.Logger, func() string) {
	var mu sync.Mutex
	var sb strings.Builder
	h := slog.NewTextHandler(&lockedWriter{mu: &mu, sb: &sb}, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h), func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	sb *strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sb.Write(p)
}

func TestPublishWildcardShapesRecords(t *testing.T) {
	dns := &listingDNS{}
	log, _ := capturingLog()
	addrs := []netip.Addr{
		netip.MustParseAddr("62.210.142.210"),
		netip.MustParseAddr("2001:db8::1"),
		// A 4-in-6 address must come out as an A, not an AAAA carrying an IPv4
		// literal — Cloudflare would reject it, or worse, accept it.
		netip.MustParseAddr("::ffff:203.0.113.9"),
	}
	if err := publishWildcard(context.Background(), dns, "hivemind.tools", "xterm", addrs, log); err != nil {
		t.Fatal(err)
	}
	if len(dns.sets) != 3 {
		t.Fatalf("published %d records, want 3", len(dns.sets))
	}
	want := map[string]string{
		"62.210.142.210": "A",
		"2001:db8::1":    "AAAA",
		"203.0.113.9":    "A",
	}
	for _, rec := range dns.sets {
		rr := rec.RR()
		// Relative to the zone: the provider absolutises it to
		// *.xterm.hivemind.tools. Publishing the FQDN here would double the zone.
		if rr.Name != "*.xterm" {
			t.Errorf("record name = %q, want %q", rr.Name, "*.xterm")
		}
		if rr.TTL != wildcardTTL {
			t.Errorf("record ttl = %s, want %s", rr.TTL, wildcardTTL)
		}
		if want[rr.Data] != rr.Type {
			t.Errorf("record %s has type %s, want %s", rr.Data, rr.Type, want[rr.Data])
		}
		delete(want, rr.Data)
	}
	if len(want) != 0 {
		t.Errorf("records missing for %v", want)
	}
}

func TestPublishWildcardNeedsAnAddress(t *testing.T) {
	dns := &listingDNS{}
	log, _ := capturingLog()
	// An invalid address is not an address: publishing zero records would ask
	// Cloudflare to delete the name's whole record set.
	err := publishWildcard(context.Background(), dns, "hivemind.tools", "xterm", []netip.Addr{{}}, log)
	if err == nil {
		t.Fatal("expected an error with no usable address")
	}
	if len(dns.sets) != 0 {
		t.Fatalf("published %d records despite having no address", len(dns.sets))
	}
}

func TestPublishWildcardLeavesATunnelCNAMEAlone(t *testing.T) {
	dns := &listingDNS{existing: []libdns.Record{
		libdns.CNAME{Name: "*.xterm", Target: "abc-123.cfargotunnel.com."},
	}}
	log, logged := capturingLog()
	if err := publishWildcard(context.Background(), dns, "hivemind.tools", "xterm",
		[]netip.Addr{netip.MustParseAddr("62.210.142.210")}, log); err != nil {
		t.Fatal(err)
	}
	if len(dns.sets) != 0 {
		t.Fatalf("overwrote a tunnel CNAME with %d records", len(dns.sets))
	}
	if out := logged(); !strings.Contains(out, "cfargotunnel.com") {
		t.Fatalf("declining to overwrite the CNAME was not logged: %s", out)
	}
}

func TestPublishWildcardAnnouncesAReplacement(t *testing.T) {
	dns := &listingDNS{existing: []libdns.Record{
		// The record we are about to move, plus two that must not be mistaken
		// for it: a different name, and the same name at the address we want.
		libdns.Address{Name: "*.xterm", IP: netip.MustParseAddr("198.51.100.1")},
		libdns.Address{Name: "*.xterm", IP: netip.MustParseAddr("2001:db8::1")},
		libdns.CNAME{Name: "*", Target: "abc-123.cfargotunnel.com."},
	}}
	log, logged := capturingLog()
	addrs := []netip.Addr{netip.MustParseAddr("62.210.142.210"), netip.MustParseAddr("2001:db8::1")}
	if err := publishWildcard(context.Background(), dns, "hivemind.tools", "xterm", addrs, log); err != nil {
		t.Fatal(err)
	}
	if len(dns.sets) != 2 {
		t.Fatalf("published %d records, want 2", len(dns.sets))
	}
	out := logged()
	if !strings.Contains(out, "198.51.100.1") {
		t.Fatalf("replacing an existing A was not logged: %s", out)
	}
	// The AAAA is unchanged and the CNAME is at another name — neither is a
	// replacement, and crying wolf about them would train operators to ignore
	// the warning that matters.
	if strings.Contains(out, "2001:db8::1") || strings.Contains(out, "cfargotunnel") {
		t.Fatalf("warned about records that were not being replaced: %s", out)
	}
}

func TestPublishWildcardPublishesBlindWhenListingFails(t *testing.T) {
	// Zone:Read is a separate token scope from Zone.DNS:Edit, so a token that
	// can write may not be able to read. That must not cost us the record.
	dns := &listingDNS{getErr: errors.New("insufficient scope")}
	log, logged := capturingLog()
	if err := publishWildcard(context.Background(), dns, "hivemind.tools", "xterm",
		[]netip.Addr{netip.MustParseAddr("62.210.142.210")}, log); err != nil {
		t.Fatal(err)
	}
	if len(dns.sets) != 1 {
		t.Fatalf("published %d records, want 1", len(dns.sets))
	}
	if out := logged(); !strings.Contains(out, "clobber check") {
		t.Fatalf("skipping the clobber check was not logged: %s", out)
	}
}

func TestPublishWildcardReportsWriteFailure(t *testing.T) {
	dns := &listingDNS{setErr: errors.New("cloudflare says no")}
	log, _ := capturingLog()
	err := publishWildcard(context.Background(), dns, "hivemind.tools", "xterm",
		[]netip.Addr{netip.MustParseAddr("62.210.142.210")}, log)
	if err == nil || !strings.Contains(err.Error(), "*.xterm.hivemind.tools") {
		t.Fatalf("error = %v, want one naming the record", err)
	}
}
