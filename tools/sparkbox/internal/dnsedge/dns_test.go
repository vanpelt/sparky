package dnsedge

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"

	"github.com/miekg/dns"
)

// serve spins the responder on an ephemeral loopback UDP port and returns its
// address plus a cleanup. It uses PacketConn so there's no port guessing.
func serve(t *testing.T, answers ...netip.Addr) string {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s := New("catnip.sh", answers, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := &dns.Server{PacketConn: pc, Handler: s.handler()}
	go srv.ActivateAndServe() //nolint:errcheck
	t.Cleanup(func() { _ = srv.Shutdown() })
	return pc.LocalAddr().String()
}

func query(t *testing.T, addr, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	resp, _, err := (&dns.Client{}).Exchange(m, addr)
	if err != nil {
		t.Fatalf("exchange %s: %v", name, err)
	}
	return resp
}

func TestWildcardAnswersApexAndAnyDepth(t *testing.T) {
	v4 := netip.MustParseAddr("100.65.150.80")
	addr := serve(t, v4)

	for _, name := range []string{"catnip.sh", "dazzling-canyon.catnip.sh", "a.b.c.catnip.sh"} {
		resp := query(t, addr, name, dns.TypeA)
		if resp.Rcode != dns.RcodeSuccess {
			t.Fatalf("%s: rcode = %v, want success", name, dns.RcodeToString[resp.Rcode])
		}
		if len(resp.Answer) != 1 {
			t.Fatalf("%s: %d answers, want 1", name, len(resp.Answer))
		}
		a, ok := resp.Answer[0].(*dns.A)
		if !ok || !a.A.Equal(net.IP(v4.AsSlice())) {
			t.Fatalf("%s: answer = %v, want A %v", name, resp.Answer[0], v4)
		}
	}
}

func TestAAAAEmptyWhenNoV6(t *testing.T) {
	addr := serve(t, netip.MustParseAddr("100.65.150.80")) // v4 only
	resp := query(t, addr, "dazzling-canyon.catnip.sh", dns.TypeAAAA)
	// A dual-stack client asking AAAA must get NOERROR with no records, not
	// NXDOMAIN — otherwise it can give up on the name entirely.
	if resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("rcode = %v, want success", dns.RcodeToString[resp.Rcode])
	}
	if len(resp.Answer) != 0 {
		t.Fatalf("got %d AAAA answers, want 0", len(resp.Answer))
	}
}

func TestDualStack(t *testing.T) {
	v4 := netip.MustParseAddr("100.65.150.80")
	v6 := netip.MustParseAddr("fd7a:115c:a1e0::8332:9650")
	addr := serve(t, v4, v6)

	if got := query(t, addr, "x.catnip.sh", dns.TypeA); len(got.Answer) != 1 {
		t.Fatalf("A: %d answers, want 1", len(got.Answer))
	}
	resp := query(t, addr, "x.catnip.sh", dns.TypeAAAA)
	if len(resp.Answer) != 1 {
		t.Fatalf("AAAA: %d answers, want 1", len(resp.Answer))
	}
	if aaaa, ok := resp.Answer[0].(*dns.AAAA); !ok || !aaaa.AAAA.Equal(net.IP(v6.AsSlice())) {
		t.Fatalf("AAAA answer = %v, want %v", resp.Answer[0], v6)
	}
}

func TestOutOfZoneRefused(t *testing.T) {
	addr := serve(t, netip.MustParseAddr("100.65.150.80"))
	// The mux only owns catnip.sh.; a query for another zone must not get our
	// wildcard answer.
	resp := query(t, addr, "example.com", dns.TypeA)
	for _, rr := range resp.Answer {
		if a, ok := rr.(*dns.A); ok && a.A.String() == "100.65.150.80" {
			t.Fatalf("out-of-zone example.com got our edge answer")
		}
	}
}
