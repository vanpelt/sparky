// Package dnsproxy is the enforcement half of sluice: a DNS resolver that a
// sandbox is pointed at (via its gateway) and that will only resolve names on
// the allowlist. A denied name gets an empty NXDOMAIN/REFUSED answer and is
// logged; an allowed name is forwarded to a real upstream, its A/AAAA answers
// are recorded into the shared IP→domain table, and only then returned.
//
// The DNS gate alone is soft — a guest could hard-code an IP or use DoH. It is
// paired with the eBPF enforcer, which drops egress to any address not in the
// table. Because the only way an address enters the table is by being the
// answer to an allowlisted query here, the two together make the allowlist the
// sole path to the internet.
package dnsproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/miekg/dns"

	"github.com/vanpelt/sparky/tools/sluice/internal/allowlist"
	"github.com/vanpelt/sparky/tools/sluice/internal/ipmap"
)

// Exchanger performs a single upstream DNS round-trip. Extracted so tests can
// inject a fake upstream; *dns.Client satisfies it.
type Exchanger interface {
	Exchange(m *dns.Msg, addr string) (*dns.Msg, time.Duration, error)
}

// DenyMode controls the reply given to a blocked query.
type DenyMode int

const (
	// DenyNXDOMAIN answers "no such name" — the record simply doesn't exist,
	// which resolvers and clients cache and handle gracefully.
	DenyNXDOMAIN DenyMode = iota
	// DenyREFUSED answers "refused" — more honest about the policy, but some
	// stub resolvers retry it against every configured server.
	DenyREFUSED
)

// Config configures a Proxy.
type Config struct {
	Allow     *allowlist.List
	IPMap     *ipmap.Map
	Upstreams []string      // host:port of real resolvers, tried in order
	Client    Exchanger     // defaults to a UDP *dns.Client
	Deny      DenyMode      // reply for blocked names
	Timeout   time.Duration // per-upstream exchange timeout
	Logger    *slog.Logger
}

// Proxy is a dns.Handler implementing the allowlist gate.
type Proxy struct {
	cfg   Config
	log   *slog.Logger
	stats Stats
}

// Stats counts decisions for observability.
type Stats struct {
	Queries uint64
	Allowed uint64
	Denied  uint64
	Errors  uint64
}

// New validates cfg and returns a Proxy.
func New(cfg Config) (*Proxy, error) {
	if cfg.Allow == nil {
		return nil, errors.New("dnsproxy: nil allowlist")
	}
	if cfg.IPMap == nil {
		return nil, errors.New("dnsproxy: nil ipmap")
	}
	if len(cfg.Upstreams) == 0 {
		return nil, errors.New("dnsproxy: no upstreams")
	}
	if cfg.Client == nil {
		cfg.Client = &dns.Client{Net: "udp"}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Proxy{cfg: cfg, log: cfg.Logger}, nil
}

// ServeDNS implements dns.Handler.
func (p *Proxy) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	p.stats.Queries++
	resp := p.handle(w, req)
	if resp == nil {
		return
	}
	_ = w.WriteMsg(resp)
}

func (p *Proxy) handle(w dns.ResponseWriter, req *dns.Msg) *dns.Msg {
	if len(req.Question) == 0 {
		return errorReply(req, dns.RcodeFormatError)
	}
	q := req.Question[0]
	client := clientIP(w)

	// Only IN-class A/AAAA/CNAME etc. are meaningful for egress. Other classes
	// are refused outright rather than forwarded.
	if q.Qclass != dns.ClassINET {
		p.stats.Denied++
		p.log.Info("dns", "decision", "deny", "reason", "class", "client", client,
			"name", q.Name, "type", dns.TypeToString[q.Qtype])
		return p.denyReply(req)
	}

	name := q.Name
	allowed, pattern := p.cfg.Allow.Allowed(name)
	if !allowed {
		p.stats.Denied++
		p.log.Info("dns", "decision", "deny", "client", client,
			"name", name, "type", dns.TypeToString[q.Qtype])
		return p.denyReply(req)
	}

	resp, rtt, err := p.forward(req)
	if err != nil {
		p.stats.Errors++
		p.log.Warn("dns", "decision", "error", "client", client, "name", name, "err", err.Error())
		return errorReply(req, dns.RcodeServerFailure)
	}

	ips, minTTL := recordAnswers(resp)
	if len(ips) > 0 {
		p.cfg.IPMap.Record(canonicalName(pattern, name), ips, time.Duration(minTTL)*time.Second)
	}
	p.stats.Allowed++
	p.log.Info("dns", "decision", "allow", "client", client, "name", name,
		"type", dns.TypeToString[q.Qtype], "pattern", pattern,
		"answers", len(ips), "rtt_ms", rtt.Milliseconds())
	return resp
}

// forward tries each upstream until one answers.
func (p *Proxy) forward(req *dns.Msg) (*dns.Msg, time.Duration, error) {
	m := req.Copy()
	var lastErr error
	for _, up := range p.cfg.Upstreams {
		ctx, cancel := context.WithTimeout(context.Background(), p.cfg.Timeout)
		resp, rtt, err := p.exchangeCtx(ctx, m, up)
		cancel()
		if err == nil {
			return resp, rtt, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no upstream answered")
	}
	return nil, 0, lastErr
}

// exchangeCtx runs one exchange, respecting ctx cancellation even if the
// injected Exchanger has no context-aware method.
func (p *Proxy) exchangeCtx(ctx context.Context, m *dns.Msg, up string) (*dns.Msg, time.Duration, error) {
	type result struct {
		msg *dns.Msg
		rtt time.Duration
		err error
	}
	ch := make(chan result, 1)
	go func() {
		r, rtt, err := p.cfg.Client.Exchange(m, up)
		ch <- result{r, rtt, err}
	}()
	select {
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	case r := <-ch:
		return r.msg, r.rtt, r.err
	}
}

func (p *Proxy) denyReply(req *dns.Msg) *dns.Msg {
	if p.cfg.Deny == DenyREFUSED {
		return errorReply(req, dns.RcodeRefused)
	}
	return errorReply(req, dns.RcodeNameError)
}

// StatsSnapshot returns a copy of the running counters.
func (p *Proxy) StatsSnapshot() Stats { return p.stats }

func errorReply(req *dns.Msg, rcode int) *dns.Msg {
	m := new(dns.Msg)
	m.SetRcode(req, rcode)
	m.RecursionAvailable = true
	return m
}

// recordAnswers pulls A/AAAA addresses and the smallest TTL out of a response.
func recordAnswers(resp *dns.Msg) ([]netip.Addr, uint32) {
	if resp == nil {
		return nil, 0
	}
	var ips []netip.Addr
	var minTTL uint32
	consider := func(ip net.IP, ttl uint32) {
		a, ok := netip.AddrFromSlice(ip)
		if !ok {
			return
		}
		ips = append(ips, a.Unmap())
		if minTTL == 0 || ttl < minTTL {
			minTTL = ttl
		}
	}
	for _, rr := range resp.Answer {
		switch v := rr.(type) {
		case *dns.A:
			consider(v.A, v.Hdr.Ttl)
		case *dns.AAAA:
			consider(v.AAAA, v.Hdr.Ttl)
		}
	}
	return ips, minTTL
}

// canonicalName labels a recorded IP with the allowlist pattern when it is a
// bare domain (so github.com and api.github.com both report as "github.com"),
// otherwise with the queried name minus its trailing dot.
func canonicalName(pattern, queried string) string {
	if pattern != "" && pattern[0] != '*' {
		return pattern
	}
	return trimDot(queried)
}

func trimDot(s string) string {
	if n := len(s); n > 0 && s[n-1] == '.' {
		return s[:n-1]
	}
	return s
}

func clientIP(w dns.ResponseWriter) string {
	if w == nil || w.RemoteAddr() == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(w.RemoteAddr().String()); err == nil {
		return host
	}
	return w.RemoteAddr().String()
}

// Servers builds the udp+tcp dns.Server pair bound to addr. Start each with
// ListenAndServe; both share this Proxy as their handler.
func (p *Proxy) Servers(addr string) []*dns.Server {
	return []*dns.Server{
		{Addr: addr, Net: "udp", Handler: p},
		{Addr: addr, Net: "tcp", Handler: p},
	}
}

var _ fmt.Stringer = DenyMode(0)

// String renders a DenyMode for logging/flags.
func (d DenyMode) String() string {
	if d == DenyREFUSED {
		return "refused"
	}
	return "nxdomain"
}
