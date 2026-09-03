package dnsproxy

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/vanpelt/sparky/tools/sluice/internal/allowlist"
	"github.com/vanpelt/sparky/tools/sluice/internal/denials"
	"github.com/vanpelt/sparky/tools/sluice/internal/ipmap"
	"github.com/vanpelt/sparky/tools/sluice/internal/policy"
)

// fakeUpstream answers A queries for names in `zone`, else NXDOMAIN.
type fakeUpstream struct {
	zone  map[string][]string // name (fqdn) -> IPs
	ttl   uint32
	calls atomic.Uint64
}

func (f *fakeUpstream) Exchange(m *dns.Msg, _ string) (*dns.Msg, time.Duration, error) {
	f.calls.Add(1)
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
	if up.calls.Load() != 0 {
		t.Errorf("denied query must not reach upstream (calls=%d)", up.calls.Load())
	}
	if im.Allowed(netip.MustParseAddr("203.0.113.9")) {
		t.Error("denied name's IP must never enter the allow-set")
	}
	if p.StatsSnapshot().Denied != 1 {
		t.Errorf("Denied stat = %d, want 1", p.StatsSnapshot().Denied)
	}
}

func TestDeniedNameEntersActiveTapCapture(t *testing.T) {
	al, _ := allowlist.New([]string{"github.com"})
	pol := policy.New(al)
	recorder := denials.New()
	recorder.Start("sbtap320", "box-id")
	p, err := New(Config{
		Allow: pol, IPMap: ipmap.New(), Upstreams: []string{"upstream:53"},
		Client: &fakeUpstream{}, Denials: recorder,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	p.ServeDNS(&capWriter{}, query("Registry.NPMJS.org", dns.TypeAAAA))
	got, err := recorder.Snapshot("sbtap320", "box-id")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Domains) != 1 || got.Domains[0].Name != "registry.npmjs.org" || got.Domains[0].Queries != 1 {
		t.Fatalf("capture = %+v", got)
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
	if up.calls.Load() != 1 {
		t.Errorf("subdomain of wildcard should be forwarded once, calls=%d", up.calls.Load())
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

func TestConcurrentStatsNoRace(t *testing.T) {
	up := &fakeUpstream{zone: map[string][]string{"api.github.com.": {"140.82.112.5"}}, ttl: 300}
	p, _ := newTestProxy(t, up)

	const workers, each = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				p.ServeDNS(&capWriter{}, query("api.github.com", dns.TypeA)) // allow
				p.ServeDNS(&capWriter{}, query("evil.com", dns.TypeA))       // deny
			}
		}()
	}
	wg.Wait()

	s := p.StatsSnapshot()
	if want := uint64(workers * each * 2); s.Queries != want {
		t.Errorf("Queries = %d, want %d", s.Queries, want)
	}
	if want := uint64(workers * each); s.Allowed != want {
		t.Errorf("Allowed = %d, want %d", s.Allowed, want)
	}
	if want := uint64(workers * each); s.Denied != want {
		t.Errorf("Denied = %d, want %d", s.Denied, want)
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
	if up.calls.Load() != 0 {
		t.Error("non-IN class must not be forwarded")
	}
}

// clientAllower records the client it was asked about and allows only for a
// specific "policied" address. It implements ClientAllower, so the proxy must
// prefer AllowedFor over Allowed and thread the query's source address through.
type clientAllower struct {
	sawClient netip.Addr
	allowFor  netip.Addr // this client resolves anything; others resolve nothing
}

func (c *clientAllower) Allowed(string) (bool, string) { return false, "" } // union path must NOT be used
func (c *clientAllower) AllowedFor(client netip.Addr, _ string) (bool, string) {
	c.sawClient = client
	return client == c.allowFor, ""
}

func TestPerClientResolutionUsesAllowedFor(t *testing.T) {
	guest := netip.MustParseAddr("172.30.9.2")
	ca := &clientAllower{allowFor: guest}
	up := &fakeUpstream{zone: map[string][]string{"anything.example.": {"9.9.9.9"}}, ttl: 60}
	p, err := New(Config{
		Allow: ca, IPMap: ipmap.New(), Upstreams: []string{"x:53"},
		Client: up, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The policied guest resolves; the proxy saw its address, not the union path.
	w := &capWriter{from: &net.UDPAddr{IP: net.ParseIP("172.30.9.2"), Port: 5353}}
	p.ServeDNS(w, query("anything.example", dns.TypeA))
	if ca.sawClient != guest {
		t.Fatalf("AllowedFor client = %v, want %v", ca.sawClient, guest)
	}
	if w.msg.Rcode != dns.RcodeSuccess || len(w.msg.Answer) == 0 {
		t.Errorf("policied client should have resolved, got rcode %d / %d answers", w.msg.Rcode, len(w.msg.Answer))
	}

	// A different client (not the allowed one) is denied by the same per-client path.
	w2 := &capWriter{from: &net.UDPAddr{IP: net.ParseIP("172.30.4.2"), Port: 5353}}
	p.ServeDNS(w2, query("anything.example", dns.TypeA))
	if w2.msg.Rcode != dns.RcodeNameError {
		t.Errorf("non-policied client rcode = %d, want NXDOMAIN", w2.msg.Rcode)
	}
}
