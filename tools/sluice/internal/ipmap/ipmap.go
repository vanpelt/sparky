// Package ipmap keeps the live association between IP addresses and the domain
// the DNS proxy resolved them from. It serves two consumers at once:
//
//   - the reporter joins eBPF per-IP byte counters back to a domain name, so
//     "bandwidth to storage.googleapis.com" is meaningful; and
//   - the enforcer treats membership as the egress allow-set — an address is
//     only reachable while some allowlisted domain still resolves to it.
//
// Entries expire on the DNS TTL, clamped to [MinTTL, MaxTTL] plus a grace
// window so that a connection opened just before a record's TTL lapses is not
// severed mid-flight. All methods are safe for concurrent use.
package ipmap

import (
	"net/netip"
	"sort"
	"sync"
	"time"
)

// Defaults for TTL clamping. DNS TTLs are frequently a few seconds, which is
// far too churny for a firewall allow-set; MinTTL keeps a resolved address
// reachable long enough to actually be used.
const (
	DefaultMinTTL = 5 * time.Minute
	DefaultMaxTTL = 1 * time.Hour
	DefaultGrace  = 30 * time.Second
)

type entry struct {
	domain string
	expire time.Time
}

// Map is a TTL-expiring IP→domain association.
type Map struct {
	mu  sync.RWMutex
	m   map[netip.Addr]entry
	now func() time.Time

	MinTTL time.Duration
	MaxTTL time.Duration
	Grace  time.Duration
}

// New returns an empty Map with default TTL clamping.
func New() *Map {
	return &Map{
		m:      map[netip.Addr]entry{},
		now:    time.Now,
		MinTTL: DefaultMinTTL,
		MaxTTL: DefaultMaxTTL,
		Grace:  DefaultGrace,
	}
}

// Record associates each addr with domain, expiring after ttl (clamped). A
// later Record for the same addr extends the expiry and, if the domain differs,
// takes the newer domain — CDNs share IPs across names, and the most recent
// resolver answer is the most useful label.
func (m *Map) Record(domain string, addrs []netip.Addr, ttl time.Duration) {
	if ttl < m.MinTTL {
		ttl = m.MinTTL
	}
	if ttl > m.MaxTTL {
		ttl = m.MaxTTL
	}
	exp := m.now().Add(ttl + m.Grace)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range addrs {
		a = a.Unmap()
		cur, ok := m.m[a]
		if !ok || exp.After(cur.expire) || cur.domain != domain {
			m.m[a] = entry{domain: domain, expire: exp}
		}
	}
}

// Pin records addr→domain with no expiry (expiry in the far future). Used to
// seed always-reachable infrastructure such as the guest's own gateway, whose
// address is never handed out by DNS but must stay in the allow-set.
func (m *Map) Pin(domain string, addr netip.Addr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.m[addr.Unmap()] = entry{domain: domain, expire: time.Time{}}
}

// Domain returns the domain associated with addr, if any and unexpired.
func (m *Map) Domain(addr netip.Addr) (string, bool) {
	addr = addr.Unmap()
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.m[addr]
	if !ok {
		return "", false
	}
	if !e.expire.IsZero() && !m.now().Before(e.expire) {
		return "", false
	}
	return e.domain, true
}

// Allowed reports whether addr is currently in the allow-set.
func (m *Map) Allowed(addr netip.Addr) bool {
	_, ok := m.Domain(addr)
	return ok
}

// Sweep drops expired entries and returns the addresses removed, so a caller
// (the enforcer) can mirror the deletion into its own state, e.g. a BPF map.
func (m *Map) Sweep() []netip.Addr {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	var dropped []netip.Addr
	for a, e := range m.m {
		if !e.expire.IsZero() && !now.Before(e.expire) {
			delete(m.m, a)
			dropped = append(dropped, a)
		}
	}
	return dropped
}

// Entry is a point-in-time view of one association.
type Entry struct {
	Addr   netip.Addr
	Domain string
	Expire time.Time // zero means pinned
}

// Snapshot returns all unexpired entries, sorted by address, for syncing the
// allow-set or for display.
func (m *Map) Snapshot() []Entry {
	now := m.now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Entry, 0, len(m.m))
	for a, e := range m.m {
		if !e.expire.IsZero() && !now.Before(e.expire) {
			continue
		}
		out = append(out, Entry{Addr: a, Domain: e.domain, Expire: e.expire})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr.Less(out[j].Addr) })
	return out
}

// Len is the number of live entries (including expired-but-not-yet-swept).
func (m *Map) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.m)
}
