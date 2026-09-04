// Package denials keeps bounded, build-scoped summaries of DNS queries that
// sluice refused for policy. It is deliberately in memory: sparkbox drains a
// capture into its durable environment row when a build finishes, while an
// abandoned capture expires without turning domain history into a host log.
package denials

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxDomainsPerCapture = 256
	maxCaptures          = 4096
	captureTTL           = 2 * time.Hour
)

var ErrCaptureMismatch = errors.New("denials: capture is not active")

// Domain is one distinct policy-denied name in a capture.
type Domain struct {
	Name          string   `json:"domain"`
	Queries       uint64   `json:"queries"`
	QTypes        []string `json:"qtypes"`
	FirstSeenUnix int64    `json:"first_seen_unix"`
	LastSeenUnix  int64    `json:"last_seen_unix"`
}

// Capture is the bounded summary returned to sparkbox.
type Capture struct {
	ID              string   `json:"capture_id"`
	Domains         []Domain `json:"domains"`
	OverflowQueries uint64   `json:"overflow_queries,omitempty"`
}

type domainState struct {
	domain Domain
	qtypes map[string]struct{}
}

type captureState struct {
	id       string
	touched  time.Time
	domains  map[string]*domainState
	overflow uint64
}

// Recorder holds at most one capture per tap. Start is idempotent for a
// capture ID, which makes a retried control request safe; a different ID means
// a different sandbox incarnation and replaces the old capture.
type Recorder struct {
	mu       sync.Mutex
	captures map[string]*captureState
	now      func() time.Time
}

func New() *Recorder {
	return &Recorder{captures: map[string]*captureState{}, now: time.Now}
}

// Start arms a capture for tap. Repeating the same ID does not clear events.
func (r *Recorder) Start(tap, id string) {
	if tap == "" || id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	r.expireLocked(now)
	if current := r.captures[tap]; current != nil && current.id == id {
		current.touched = now
		return
	}
	if r.captures[tap] == nil && len(r.captures) >= maxCaptures {
		r.evictOldestLocked()
	}
	r.captures[tap] = &captureState{
		id: id, touched: now, domains: map[string]*domainState{},
	}
}

// Record adds one denied DNS query when tap has an active capture.
func (r *Recorder) Record(tap, name, qtype string) {
	name = canonical(name)
	if tap == "" || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	r.expireLocked(now)
	c := r.captures[tap]
	if c == nil {
		return
	}
	c.touched = now
	d := c.domains[name]
	if d == nil {
		if len(c.domains) >= maxDomainsPerCapture {
			c.overflow++
			return
		}
		d = &domainState{
			domain: Domain{Name: name, FirstSeenUnix: now.Unix(), LastSeenUnix: now.Unix()},
			qtypes: map[string]struct{}{},
		}
		c.domains[name] = d
	}
	d.domain.Queries++
	d.domain.LastSeenUnix = now.Unix()
	if qtype != "" {
		d.qtypes[qtype] = struct{}{}
	}
}

// Snapshot returns a capture without deleting it. Keeping the completed value
// makes retries safe; a later Start with a new sandbox ID replaces it.
func (r *Recorder) Snapshot(tap, id string) (Capture, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now().UTC()
	r.expireLocked(now)
	c := r.captures[tap]
	if c == nil || c.id != id {
		return Capture{}, ErrCaptureMismatch
	}
	c.touched = now
	out := Capture{ID: c.id, Domains: make([]Domain, 0, len(c.domains)), OverflowQueries: c.overflow}
	for _, state := range c.domains {
		d := state.domain
		d.QTypes = make([]string, 0, len(state.qtypes))
		for qtype := range state.qtypes {
			d.QTypes = append(d.QTypes, qtype)
		}
		sort.Strings(d.QTypes)
		out.Domains = append(out.Domains, d)
	}
	sort.Slice(out.Domains, func(i, j int) bool {
		if out.Domains[i].Queries != out.Domains[j].Queries {
			return out.Domains[i].Queries > out.Domains[j].Queries
		}
		return out.Domains[i].Name < out.Domains[j].Name
	})
	return out, nil
}

func (r *Recorder) expireLocked(now time.Time) {
	for tap, capture := range r.captures {
		if now.Sub(capture.touched) > captureTTL {
			delete(r.captures, tap)
		}
	}
}

func (r *Recorder) evictOldestLocked() {
	var oldestTap string
	var oldestTime time.Time
	for tap, capture := range r.captures {
		if oldestTap == "" || capture.touched.Before(oldestTime) ||
			(capture.touched.Equal(oldestTime) && tap < oldestTap) {
			oldestTap = tap
			oldestTime = capture.touched
		}
	}
	delete(r.captures, oldestTap)
}

func canonical(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}
