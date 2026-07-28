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
//
// Those two consumers want DIFFERENT lifetimes, which is why there are two
// maps. The allow-set must forget on schedule — that is the whole security
// property. Labels must not, because the meter's counters are cumulative and
// the reporter joins them at report time: if the name were gone by then, a
// sandbox's entire lifetime total would redraw itself under a bare IP the
// moment one DNS entry lapsed. That hit the busiest destinations hardest —
// a long-lived keep-alive connection re-resolves least often, so the biggest
// row on the panel was the likeliest to be anonymous. Label keeps the last
// name we ever saw for an address, long after the address stopped being
// reachable; it is display metadata and deliberately not consulted by
// Allowed.
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

	// DefaultLabelTTL is how long a name survives for display after the
	// address it named left the allow-set. It is generous because it buys
	// nothing but a readable panel: a sandbox that ran for a day should still
	// be able to say who it talked to at the start of the day.
	DefaultLabelTTL = 24 * time.Hour

	// DefaultMaxLabels caps the label memory so a sandbox resolving endless
	// unique addresses cannot grow it without bound. On overflow the oldest
	// labels go first; losing one costs a row its name, nothing more.
	DefaultMaxLabels = 8192
)

type entry struct {
	domain string
	expire time.Time
}

type label struct {
	domain string
	seen   time.Time // last time this name was recorded for the address
}

// Map is a TTL-expiring IP→domain association.
type Map struct {
	mu     sync.RWMutex
	m      map[netip.Addr]entry
	labels map[netip.Addr]label
	now    func() time.Time

	MinTTL time.Duration
	MaxTTL time.Duration
	Grace  time.Duration

	LabelTTL  time.Duration
	MaxLabels int
}

// New returns an empty Map with default TTL clamping.
func New() *Map {
	return &Map{
		m:         map[netip.Addr]entry{},
		labels:    map[netip.Addr]label{},
		now:       time.Now,
		MinTTL:    DefaultMinTTL,
		MaxTTL:    DefaultMaxTTL,
		Grace:     DefaultGrace,
		LabelTTL:  DefaultLabelTTL,
		MaxLabels: DefaultMaxLabels,
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
	now := m.now()
	exp := now.Add(ttl + m.Grace)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range addrs {
		a = a.Unmap()
		cur, ok := m.m[a]
		if !ok || exp.After(cur.expire) || cur.domain != domain {
			m.m[a] = entry{domain: domain, expire: exp}
		}
		m.rememberLocked(a, domain, now)
	}
}

// rememberLocked records the display label for addr. Callers hold m.mu.
func (m *Map) rememberLocked(addr netip.Addr, domain string, now time.Time) {
	if m.labels == nil {
		m.labels = map[netip.Addr]label{}
	}
	m.labels[addr] = label{domain: domain, seen: now}
	m.trimLabelsLocked(now)
}

// trimLabelsLocked drops labels past LabelTTL, then evicts the least recently
// seen until the map is back under MaxLabels.
func (m *Map) trimLabelsLocked(now time.Time) {
	if m.LabelTTL > 0 {
		for a, l := range m.labels {
			if now.Sub(l.seen) > m.LabelTTL {
				delete(m.labels, a)
			}
		}
	}
	max := m.MaxLabels
	if max <= 0 || len(m.labels) <= max {
		return
	}
	type aged struct {
		addr netip.Addr
		seen time.Time
	}
	all := make([]aged, 0, len(m.labels))
	for a, l := range m.labels {
		all = append(all, aged{a, l.seen})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].seen.Before(all[j].seen) })
	for _, a := range all[:len(all)-max] {
		delete(m.labels, a.addr)
	}
}

// Pin records addr→domain with no expiry (expiry in the far future). Used to
// seed always-reachable infrastructure such as the guest's own gateway, whose
// address is never handed out by DNS but must stay in the allow-set.
func (m *Map) Pin(domain string, addr netip.Addr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	addr = addr.Unmap()
	m.m[addr] = entry{domain: domain, expire: time.Time{}}
	m.rememberLocked(addr, domain, m.now())
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
//
// Deliberately built on Domain, not Label: an address is reachable only while
// a live DNS answer says so. Label remembers names for far longer, and reading
// it here would quietly turn "we once saw this name" into "you may talk to it".
func (m *Map) Allowed(addr netip.Addr) bool {
	_, ok := m.Domain(addr)
	return ok
}

// Label returns the last domain ever recorded for addr, whether or not the
// address is still in the allow-set. Reporting-only — see Allowed.
func (m *Map) Label(addr netip.Addr) (string, bool) {
	addr = addr.Unmap()
	m.mu.RLock()
	defer m.mu.RUnlock()
	// The live association is authoritative when there is one: a CDN address
	// can be re-Recorded under a newer name, and Domain has already applied
	// that preference.
	if e, ok := m.m[addr]; ok {
		if e.expire.IsZero() || m.now().Before(e.expire) {
			return e.domain, true
		}
	}
	l, ok := m.labels[addr]
	if !ok {
		return "", false
	}
	if m.LabelTTL > 0 && m.now().Sub(l.seen) > m.LabelTTL {
		return "", false
	}
	return l.domain, true
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
