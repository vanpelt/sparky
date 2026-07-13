// Package host is the single-host control plane: it owns sandbox records,
// persists them to a JSON state file, drives the vmm.Driver, and pauses idle
// sandboxes (the suspend-to-snapshot cost lever).
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Default per-sandbox resources, applied when the caller passes <= 0 (the SSH
// `new@` path always does; the HTTP API may override). Bounded only by host
// capacity — an 8c/16t/64GB box fits ~8 of these before overcommit.
const (
	defaultVCPUs int64 = 2
	defaultMemMB int64 = 8192
)

// LimitError is returned when a sandbox can't be brought to running because the
// owner already has too many running. It carries the running set so callers
// (the SSH gateway) can tell the user exactly what to pause.
type LimitError struct {
	Max     int
	Running []string // names of the owner's currently-running sandboxes
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("running-sandbox limit reached (%d/%d): %s",
		len(e.Running), e.Max, strings.Join(e.Running, ", "))
}

// CapacityError is returned when starting a sandbox would push the host's
// allocated RAM past the admission budget.
type CapacityError struct {
	RequestedMB, UsedMB, BudgetMB int64
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("host at capacity: %d MB running + %d MB requested exceeds the %d MB budget",
		e.UsedMB, e.RequestedMB, e.BudgetMB)
}

type Sandbox struct {
	Name       string    `json:"name"`
	Owner      string    `json:"owner"`
	Image      string    `json:"image"`
	VCPUs      int64     `json:"vcpus"`
	MemMB      int64     `json:"mem_mb"`
	State      vmm.State `json:"state"`
	SSHAddr    string    `json:"ssh_addr,omitempty"`
	SSHUser    string    `json:"ssh_user,omitempty"`
	HostIP     string    `json:"host_ip,omitempty"`  // guest IP for the HTTP proxy; empty when paused
	GuestV6    string    `json:"guest_v6,omitempty"` // routable IPv6 identity; empty when paused
	CreatedAt  time.Time `json:"created_at"`
	LastActive time.Time `json:"last_active"`
}

type Manager struct {
	mu          sync.Mutex
	driver      vmm.Driver
	log         *slog.Logger
	path        string // JSON state file
	boxes       map[string]*Sandbox
	gwPubKey    string
	routes      *routes.Store // optional: proxy route bookkeeping
	maxPerOwner int           // max running sandboxes per owner; 0 = unlimited
	memAdmitPct int           // RAM admission threshold as % of host; 0 = disabled
	hostMemMB   int64         // host RAM in MB for admission; 0 = disabled
}

type Options struct {
	StateDir         string
	Driver           vmm.Driver
	GatewayPublicKey string
	Logger           *slog.Logger
	// Routes, if set, gets a default route per sandbox on create and is cleaned
	// up on destroy. Nil disables proxy-route bookkeeping (used by unit tests).
	Routes *routes.Store
	// MaxRunningPerOwner caps how many sandboxes one owner may have running at
	// once (0 = unlimited). Enforced on create and resume-on-connect.
	MaxRunningPerOwner int
	// MemAdmissionPct + HostMemMB gate starting a sandbox on host RAM: a start
	// is refused if running sandboxes' allocated RAM would exceed
	// HostMemMB*MemAdmissionPct/100. Either being 0 disables the check.
	MemAdmissionPct int
	HostMemMB       int64
}

func NewManager(opts Options) (*Manager, error) {
	m := &Manager{
		driver:      opts.Driver,
		log:         opts.Logger,
		path:        filepath.Join(opts.StateDir, "sandboxes.json"),
		boxes:       map[string]*Sandbox{},
		gwPubKey:    opts.GatewayPublicKey,
		routes:      opts.Routes,
		maxPerOwner: opts.MaxRunningPerOwner,
		memAdmitPct: opts.MemAdmissionPct,
		hostMemMB:   opts.HostMemMB,
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	// Driver state does not survive process restarts in the mock driver, and
	// firecracker VMs died with the previous process too. Mark everything
	// paused; Resume recreates on demand.
	for _, b := range m.boxes {
		if b.State == vmm.StateRunning {
			b.State = vmm.StatePaused
			b.SSHAddr = ""
			b.HostIP = ""
			b.GuestV6 = ""
		}
	}
	return m, m.save()
}

func (m *Manager) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*Sandbox, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("invalid sandbox name %q (lowercase alphanumerics and dashes)", name)
	}
	if vcpus <= 0 {
		vcpus = defaultVCPUs
	}
	if memMB <= 0 {
		memMB = defaultMemMB
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.boxes[name]; ok {
		return nil, fmt.Errorf("sandbox %q already exists", name)
	}
	if err := m.admit(owner, memMB, ""); err != nil {
		return nil, err
	}
	inst, err := m.driver.Create(ctx, vmm.Config{
		Name: name, Image: image, VCPUs: vcpus, MemMB: memMB,
		GatewayPublicKey: m.gwPubKey,
	})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	b := &Sandbox{
		Name: name, Owner: owner, Image: image, VCPUs: vcpus, MemMB: memMB,
		State: inst.State, SSHAddr: inst.SSHAddr, SSHUser: inst.SSHUser,
		HostIP: inst.HostIP, GuestV6: inst.GuestV6, CreatedAt: now, LastActive: now,
	}
	m.boxes[name] = b
	// Default web route: <name>.<domain> -> :8000, so every sandbox is
	// reachable over HTTP with no extra setup.
	if m.routes != nil {
		if err := m.routes.Upsert(routes.Route{
			Subdomain: name, Sandbox: name, Owner: owner, Port: routes.DefaultPort,
		}); err != nil {
			m.log.Warn("default route creation failed", "name", name, "err", err)
		}
	}
	m.log.Info("sandbox created", "name", name, "owner", owner)
	return copyOf(b), m.save()
}

// admit enforces the running-sandbox limits before a start (create or resume).
// Callers must hold m.mu. exclude is the name of the sandbox being resumed (so
// it isn't counted against its own start), or "" for a create.
func (m *Manager) admit(owner string, memMB int64, exclude string) error {
	if m.maxPerOwner > 0 {
		var running []string
		for _, b := range m.boxes {
			if b.Name != exclude && b.Owner == owner && b.State == vmm.StateRunning {
				running = append(running, b.Name)
			}
		}
		if len(running) >= m.maxPerOwner {
			sort.Strings(running)
			return &LimitError{Max: m.maxPerOwner, Running: running}
		}
	}
	if m.memAdmitPct > 0 && m.hostMemMB > 0 {
		var used int64
		for _, b := range m.boxes {
			if b.Name != exclude && b.State == vmm.StateRunning {
				used += b.MemMB
			}
		}
		budget := m.hostMemMB * int64(m.memAdmitPct) / 100
		if used+memMB > budget {
			return &CapacityError{RequestedMB: memMB, UsedMB: used, BudgetMB: budget}
		}
	}
	return nil
}

func (m *Manager) Get(name string) (*Sandbox, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok {
		return nil, false
	}
	return copyOf(b), true
}

func (m *Manager) List() []*Sandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Sandbox, 0, len(m.boxes))
	for _, b := range m.boxes {
		out = append(out, copyOf(b))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// EnsureRunning resumes the sandbox if paused and returns its SSH endpoint.
// This is the gateway's resume-on-connect entry point.
func (m *Manager) EnsureRunning(ctx context.Context, name string) (*Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok {
		return nil, fmt.Errorf("sandbox %q not found", name)
	}
	if b.State != vmm.StateRunning {
		// Resuming brings this sandbox back to running, so it's subject to the
		// same limits as a fresh create (exclude itself — it isn't running yet).
		if err := m.admit(b.Owner, b.MemMB, b.Name); err != nil {
			return nil, err
		}
		inst, err := m.resumeOrRecreate(ctx, b)
		if err != nil {
			return nil, err
		}
		b.State = inst.State
		b.SSHAddr = inst.SSHAddr
		b.SSHUser = inst.SSHUser
		b.HostIP = inst.HostIP
		b.GuestV6 = inst.GuestV6
		m.log.Info("sandbox resumed", "name", name)
	}
	b.LastActive = time.Now().UTC()
	return copyOf(b), m.save()
}

// resumeOrRecreate handles the post-restart case where the driver has no
// record of the sandbox: Resume fails, so recreate it from the stored spec
// (mock loses nothing since the workdir persists; firecracker cold-boots the
// still-present per-VM disk).
func (m *Manager) resumeOrRecreate(ctx context.Context, b *Sandbox) (*vmm.Instance, error) {
	inst, err := m.driver.Resume(ctx, b.Name)
	if err == nil {
		return inst, nil
	}
	m.log.Warn("resume failed, recreating", "name", b.Name, "err", err)
	return m.driver.Create(ctx, vmm.Config{
		Name: b.Name, Image: b.Image, VCPUs: b.VCPUs, MemMB: b.MemMB,
		GatewayPublicKey: m.gwPubKey,
	})
}

func (m *Manager) Pause(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok {
		return fmt.Errorf("sandbox %q not found", name)
	}
	if b.State == vmm.StatePaused {
		return nil
	}
	if err := m.driver.Pause(ctx, name); err != nil {
		return err
	}
	b.State = vmm.StatePaused
	b.SSHAddr = ""
	b.HostIP = ""
	b.GuestV6 = ""
	m.log.Info("sandbox paused", "name", name)
	return m.save()
}

func (m *Manager) Destroy(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.boxes[name]; !ok {
		return fmt.Errorf("sandbox %q not found", name)
	}
	if err := m.driver.Destroy(ctx, name); err != nil {
		return err
	}
	delete(m.boxes, name)
	if m.routes != nil {
		if err := m.routes.DeleteBySandbox(name); err != nil {
			m.log.Warn("route cleanup failed", "name", name, "err", err)
		}
	}
	m.log.Info("sandbox destroyed", "name", name)
	return m.save()
}

// Touch records sandbox activity (an SSH session) for the idle reaper.
func (m *Manager) Touch(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.boxes[name]; ok {
		b.LastActive = time.Now().UTC()
		m.save() //nolint:errcheck
	}
}

// RunReaper pauses running sandboxes idle longer than timeout. Blocks until
// ctx is done.
func (m *Manager) RunReaper(ctx context.Context, timeout, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, b := range m.List() {
				if b.State == vmm.StateRunning && time.Since(b.LastActive) > timeout {
					if err := m.Pause(ctx, b.Name); err != nil {
						m.log.Error("reaper pause failed", "name", b.Name, "err", err)
					} else {
						m.log.Info("reaper paused idle sandbox", "name", b.Name)
					}
				}
			}
		}
	}
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &m.boxes)
}

// save persists state; callers must hold m.mu.
func (m *Manager) save() error {
	data, err := json.MarshalIndent(m.boxes, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

func copyOf(b *Sandbox) *Sandbox {
	c := *b
	return &c
}
