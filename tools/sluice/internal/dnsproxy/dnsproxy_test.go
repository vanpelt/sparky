package dnsproxy

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/vanpelt/sparky/tools/sluice/internal/allowlist"
	"github.com/vanpelt/sparky/tools/sluice/internal/ipmap"
)

// fakeUpstream answers A queries for names in `zone`, else NXDOMAIN.
type fakeUpstream struct {
	zone  map[string][]string // name (fqdn) -> IPs
	ttl   uint32
	calls int
}

func (f *fakeUpstream) Exchange(m *dns.Msg, _ string) (*dns.Msg, time.Duration, error) {
	f.calls++
	resp := new(dns.Msg)
	resp.SetReply(m)
	q := m.Question[0]
	if ips, ok := f.zone[q.Name]; ok && q.Qtype == dns.TypeA {
		for _, ip := range ips {
			resp.Answer = append(resp.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: f.ttl},
				A:   net.ParseIP(ip),
			})
		}
	} else {
		resp.Rcode = dns.RcodeNameError
	}
	return resp, 5 * time.Millisecond, nil
}

// capWriter captures the written reply.
type capWriter struct {
	dns.ResponseWriter
	msg  *dns.Msg
	from net.Addr
}

func (c *capWriter) WriteMsg(m *dns.Msg) error { c.msg = m; return nil }
func (c *capWriter) RemoteAddr() net.Addr {
	if c.from != nil {
		return c.from
	}
	return &net.UDPAddr{IP: net.ParseIP("172.30.5.2"), Port: 5353}
}

func newTestProxy(t *testing.T, up Exchanger) (*Proxy, *ipmap.Map) {
	t.Helper()
	al, err := allowlist.New([]string{"github.com", "*.googleapis.com"})
	if err != nil {
		t.Fatal(err)
	}
	im := ipmap.New()
	p, err := New(Config{
		Allow:     al,
		IPMap:     im,
		Upstreams: []string{"upstream:53"},
		Client:    up,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p, im
}

func query(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

func TestAllowedForwardsAndRecords(t *testing.T) {
	up := &fakeUpstream{zone: map[string][]string{
		"api.github.com.": {"140.82.112.5", "140.82.112.6"},
	}, ttl: 300}
	p, im := newTestProxy(t, up)

	w := &capWriter{}
	p.ServeDNS(w, query("api.github.com", dns.TypeA))

	if w.msg == nil || w.msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected success reply, got %+v", w.msg)
	}
	if len(w.msg.Answer) != 2 {
		t.Fatalf("expected 2 answers, got %d", len(w.msg.Answer))
	}
	// Recorded under the bare-domain pattern, reachable in the allow-set.
	if d, ok := im.Domain(netip.MustParseAddr("140.82.112.5")); !ok || d != "github.com" {
		t.Fatalf("ipmap join = %q,%v; want github.com,true", d, ok)
	}
	if !im.Allowed(netip.MustParseAddr("140.82.112.6")) {
		t.Fatal("second answer IP should be in allow-set")
	}
	if p.StatsSnapshot().Allowed != 1 {
		t.Errorf("Allowed stat = %d, want 1", p.StatsSnapshot().Allowed)
	}
}

func TestDeniedNameNotForwarded(t *testing.T) {
	up := &fakeUpstream{zone: map[string][]string{"evil.com.": {"203.0.113.9"}}, ttl: 300}
	p, im := newTestProxy(t, up)

	w := &capWriter{}
	p.ServeDNS(w, query("evil.com", dns.TypeA))

	if w.msg.Rcode != dns.RcodeNameError {
		t.Fatalf("expected NXDOMAIN, got rcode %d", w.msg.Rcode)
	}
	if up.calls != 0 {
		t.Errorf("denied query must not reach upstream (calls=%d)", up.calls)
	}
	if im.Allowed(netip.MustParseAddr("203.0.113.9")) {
		t.Error("denied name's IP must never enter the allow-set")
	}
	if p.StatsSnapshot().Denied != 1 {
		t.Errorf("Denied stat = %d, want 1", p.StatsSnapshot().Denied)
	}
}

func TestWildcardApexDenied(t *testing.T) {
	// *.googleapis.com allows subdomains but not the apex.
	up := &fakeUpstream{zone: map[string][]string{"googleapis.com.": {"1.2.3.4"}}, ttl: 60}
	p, _ := newTestProxy(t, up)

	w := &capWriter{}
	p.ServeDNS(w, query("googleapis.com", dns.TypeA))
	if w.msg.Rcode != dns.RcodeNameError {
		t.Errorf("apex of wildcard should be denied, got rcode %d", w.msg.Rcode)
	}

	w2 := &capWriter{}
	p.ServeDNS(w2, query("storage.googleapis.com", dns.TypeA))
	// Not in the fake zone, so upstream returns NXDOMAIN — but it WAS forwarded.
	if up.calls != 1 {
		t.Errorf("subdomain of wildcard should be forwarded once, calls=%d", up.calls)
	}
}

func TestDenyModeRefused(t *testing.T) {
	al, _ := allowlist.New([]string{"github.com"})
	p, _ := New(Config{
		Allow: al, IPMap: ipmap.New(), Upstreams: []string{"x:53"},
		Client: &fakeUpstream{}, Deny: DenyREFUSED,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	w := &capWriter{}
	p.ServeDNS(w, query("blocked.example", dns.TypeA))
	if w.msg.Rcode != dns.RcodeRefused {
		t.Errorf("DenyREFUSED rcode = %d, want %d", w.msg.Rcode, dns.RcodeRefused)
	}
}

func TestNonINClassDenied(t *testing.T) {
	up := &fakeUpstream{}
	p, _ := newTestProxy(t, up)
	m := new(dns.Msg)
	m.Question = []dns.Question{{Name: "github.com.", Qtype: dns.TypeA, Qclass: dns.ClassCHAOS}}
	w := &capWriter{}
	p.ServeDNS(w, m)
	if w.msg.Rcode != dns.RcodeNameError {
		t.Errorf("CHAOS class should be denied, got %d", w.msg.Rcode)
	}
	if up.calls != 0 {
		t.Error("non-IN class must not be forwarded")
	}
}
