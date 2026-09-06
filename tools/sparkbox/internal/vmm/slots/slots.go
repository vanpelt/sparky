// Package slots owns a guest-network slot namespace: which indices exist,
// which are spoken for by a host service, and which VM holds each one.
//
// A slot is not an implementation detail of one driver. Everything a guest is
// reachable by is derived from it — the tap device, the per-slot uid the
// privileged helper confines the VMM to, the /30's host and guest IPv4, the
// guest IPv6, the jail directory — and the formulas are the same in both
// drivers because they have to be: two guests on one host cannot both be
// 172.30.0.2.
//
// It exists because the two drivers used to answer "which slot is free" from
// byte-identical private copies that each scanned only their OWN live VMs.
// That is correct for a node running one VMM and silently wrong for a node
// running two: both would answer 0. What stopped that being a cross-tenant
// disaster rather than a failed create is the privileged helper, which is
// keyed by slot alone and refuses a slot that is taken — so the second driver
// got a refusal instead of the first driver's uid. A Pool shared between the
// two makes the refusal unnecessary rather than load-bearing.
//
// A Pool is safe for concurrent use and holds no lock while calling anything,
// which is what lets two drivers share one without an ordering rule between
// their own mutexes.
package slots

import (
	"fmt"
	"sync"
)

// Pool hands out and takes back slots within one namespace.
type Pool struct {
	mu       sync.Mutex
	capacity int
	label    string
	reserved map[int]bool
	byName   map[string]int
	byIndex  map[int]string
}

// New builds a pool over capacity indices. Reserved indices are inside the
// prefix but belong to a host service rather than a guest — today the sluice
// DNS listener — so they count against capacity and are never handed out.
//
// label is what a caller sees when the pool is empty; it is the subnet, because
// "no free network slots in 172.30.0.0/20" tells an operator both what ran out
// and which knob makes more of it.
func New(label string, capacity int, reserved ...int) *Pool {
	p := &Pool{
		capacity: capacity,
		label:    label,
		reserved: make(map[int]bool, len(reserved)),
		byName:   map[string]int{},
		byIndex:  map[int]string{},
	}
	for _, idx := range reserved {
		if idx >= 0 && idx < capacity {
			p.reserved[idx] = true
		}
	}
	return p
}

// Claim gives name the lowest free slot and records it.
//
// Lowest-free rather than next-unused is deliberate and is the behaviour both
// drivers already had: a node that creates and destroys sandboxes all day
// otherwise walks off the end of its subnet while almost every slot in it is
// idle.
func (p *Pool) Claim(name string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx, ok := p.byName[name]; ok {
		return idx, nil
	}
	for idx := 0; idx < p.capacity; idx++ {
		if p.reserved[idx] {
			continue
		}
		if _, taken := p.byIndex[idx]; taken {
			continue
		}
		p.byName[name] = idx
		p.byIndex[idx] = name
		return idx, nil
	}
	return 0, fmt.Errorf("no free network slots in %s (max %d concurrent VMs)", p.label, p.capacity)
}

// Hold records a slot a caller already knows about, for a VM whose index was
// decided elsewhere — a restart re-adopting what it found, or a test. It
// refuses an index another name holds rather than overwriting, because the two
// callers would then derive the same tap and the same uid from it.
func (p *Pool) Hold(name string, idx int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= p.capacity {
		return fmt.Errorf("slot %d is outside %s (capacity %d)", idx, p.label, p.capacity)
	}
	if owner, taken := p.byIndex[idx]; taken && owner != name {
		return fmt.Errorf("slot %d already belongs to %q", idx, owner)
	}
	if held, ok := p.byName[name]; ok && held != idx {
		delete(p.byIndex, held)
	}
	p.byName[name] = idx
	p.byIndex[idx] = name
	return nil
}

// Release returns name's slot. It is idempotent: every path that drops a VM
// record calls it, several of them can run for one VM, and a second call must
// not free a slot a later create has already been given.
func (p *Pool) Release(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.byName[name]
	if !ok {
		return
	}
	delete(p.byName, name)
	if p.byIndex[idx] == name {
		delete(p.byIndex, idx)
	}
}

// Rename moves a held slot to a new name without releasing it, which is what a
// VM rename is: the same guest, the same tap, the same addresses, a different
// record. Renaming something that holds nothing is not an error — a paused VM
// whose record was dropped still gets renamed.
func (p *Pool) Rename(old, updated string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, ok := p.byName[old]
	if !ok {
		return nil
	}
	if _, taken := p.byName[updated]; taken {
		return fmt.Errorf("%q already holds a slot", updated)
	}
	delete(p.byName, old)
	p.byName[updated] = idx
	p.byIndex[idx] = updated
	return nil
}

// Capacity is how many slots the namespace has, reserved ones included.
func (p *Pool) Capacity() int { return p.capacity }

// Held is every (name, slot) this pool has handed out. It exists for the
// invariant tests that check a driver's own records and its pool agree; a
// disagreement is a slot leak, which shows up much later as a subnet that has
// mysteriously run out.
func (p *Pool) Held() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.byName))
	for name, idx := range p.byName {
		out[name] = idx
	}
	return out
}
