// Package dnsedge serves wildcard DNS for the sandbox web domain. It answers
// A/AAAA for the domain apex and every *.<domain> with the edge's address(es),
// so a Tailscale split-DNS entry can point the whole zone at this responder and
// tailnet members resolve <name>.<domain> to the edge with no per-name DNS and
// no external dnsmasq. It is deliberately dumb: one wildcard answer for the
// zone, nothing else — routing to the right sandbox happens at the proxy edge,
// not in DNS.
package dnsedge

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
)

// Server answers the configured zone with a fixed set of edge addresses.
type Server struct {
	zone string // canonical fqdn with trailing dot, lower-case (e.g. "catnip.sh.")
	v4   []net.IP
	v6   []net.IP
	ttl  uint32
	log  *slog.Logger
}

// New builds a responder for domain that answers every name in the zone with the
// given addresses (v4 -> A, v6 -> AAAA). ttl is fixed low so the edge IP can move.
func New(domain string, answers []netip.Addr, log *slog.Logger) *Server {
	s := &Server{
		zone: dns.Fqdn(strings.ToLower(domain)),
		ttl:  60,
		log:  log,
	}
	for _, a := range answers {
		switch {
		case a.Is4():
			s.v4 = append(s.v4, net.IP(a.AsSlice()))
		case a.Is6():
			s.v6 = append(s.v6, net.IP(a.AsSlice()))
		}
	}
	return s
}

func (s *Server) handle(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	// The mux only dispatches in-zone queries here, so every question is a name
	// we own; answer A/AAAA and leave everything else as an empty NOERROR (a
	// wildcard host, not a full zone). Answering the wrong family with an empty
	// NOERROR — rather than NXDOMAIN — keeps dual-stack clients from failing.
	for _, q := range r.Question {
		hdr := dns.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: s.ttl}
		switch q.Qtype {
		case dns.TypeA:
			for _, ip := range s.v4 {
				m.Answer = append(m.Answer, &dns.A{Hdr: hdr, A: ip})
			}
		case dns.TypeAAAA:
			for _, ip := range s.v6 {
				m.Answer = append(m.Answer, &dns.AAAA{Hdr: hdr, AAAA: ip})
			}
		}
	}
	_ = w.WriteMsg(m)
}

// handler dispatches only the responder's zone (apex + whole subtree) to handle.
func (s *Server) handler() dns.Handler {
	mux := dns.NewServeMux()
	mux.HandleFunc(s.zone, s.handle)
	return mux
}

// ListenAndServe serves the responder on UDP and TCP at addr until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if len(s.v4) == 0 && len(s.v6) == 0 {
		return errors.New("dnsedge: no answer addresses configured")
	}
	h := s.handler()
	udp := &dns.Server{Addr: addr, Net: "udp", Handler: h}
	tcp := &dns.Server{Addr: addr, Net: "tcp", Handler: h}
	errc := make(chan error, 2)
	go func() { errc <- udp.ListenAndServe() }()
	go func() { errc <- tcp.ListenAndServe() }()
	s.log.Info("wildcard DNS responder enabled", "addr", addr, "zone", s.zone, "v4", s.v4, "v6", s.v6)

	defer func() { _ = udp.Shutdown(); _ = tcp.Shutdown() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errc:
		return err
	}
}
