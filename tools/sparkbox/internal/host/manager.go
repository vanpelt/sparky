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
	// Pinned exempts a sandbox from the idle reaper: a pinned sandbox stays
	// resident so in-guest cron, daemons, and queue workers keep running (the
	// "always-on" escape hatch for work with no inbound trigger — see the
	// resource-model design, Part 3). It costs a permanent RAM slot, so it's a
	// bounded, paid capability. Pinned sandboxes are also resumed on host boot
	// (ResumePinned) so a restart doesn't silently kill the daemon.
	Pinned bool `json:"pinned,omitempty"`
	// Ballooned means the idle reaper has inflated this (still-running) sandbox's
	// memory balloon to hand its unused RAM back to the host — the live-overcommit
	// "Warm" tier. The guest keeps running (cron/daemons fire); it just has a
	// smaller resident footprint until the next activity deflates it.
	Ballooned bool `json:"ballooned,omitempty"`
	// KeyFP is the fingerprint of the SSH key whose session last created or
	// resumed this sandbox. It rides along into the id token's `key_fp` claim
	// for auditing — which of a user's machines started this thing — and is
	// deliberately not meant for authorization policy.
	KeyFP string `json:"key_fp,omitempty"`
}

// ScheduleCleaner drops a sandbox's platform-scheduler entries when it is
// destroyed, so a deleted sandbox leaves no jobs that would wake a ghost
// forever. Satisfied structurally by *schedule.Store (avoids importing it).
type ScheduleCleaner interface {
	DeleteBySandbox(sandbox string) error
}

// FrontDoor is an optional hook for per-sandbox public-address plumbing (see
// internal/frontdoor): Ensure is called when a sandbox is created, Remove when
// it is destroyed. Implementations are expected to be best-effort — a sandbox
// is never failed over front-door plumbing.
type FrontDoor interface {
	Ensure(ctx context.Context, name string)
	Remove(ctx context.Context, name string)
}

type Manager struct {
	mu          sync.Mutex
	driver      vmm.Driver
	balloon     vmm.Ballooner // driver's balloon capability, if it has one; else nil
	log         *slog.Logger
	path        string // JSON state file
	boxes       map[string]*Sandbox
	gwPubKey    string
	routes      *routes.Store   // optional: proxy route bookkeeping
	schedules   ScheduleCleaner // optional: platform-scheduler cleanup on destroy
	frontDoor   FrontDoor       // optional: per-sandbox address plumbing
	maxPerOwner int             // max running sandboxes per owner; 0 = unlimited
	memAdmitPct int             // RAM admission threshold as % of host; 0 = disabled
	hostMemMB   int64           // host RAM in MB for admission; 0 = disabled
	reserveMB   int64           // per-VM working-set floor for admission + balloon; 0 = off (count full ceiling)
	nodeName    string          // this host's name in capacity reports
	hostVCPUs   int64           // host logical CPUs for capacity reports; 0 = unknown
}

type Options struct {
	StateDir         string
	Driver           vmm.Driver
	GatewayPublicKey string
	Logger           *slog.Logger
	// Routes, if set, gets a default route per sandbox on create and is cleaned
	// up on destroy. Nil disables proxy-route bookkeeping (used by unit tests).
	Routes *routes.Store
	// Schedules, if set, has a sandbox's schedules deleted when it is destroyed.
	Schedules ScheduleCleaner
	// MaxRunningPerOwner caps how many sandboxes one owner may have running at
	// once (0 = unlimited). Enforced on create and resume-on-connect.
	MaxRunningPerOwner int
	// MemAdmissionPct + HostMemMB gate starting a sandbox on host RAM: a start
	// is refused if running sandboxes' allocated RAM would exceed
	// HostMemMB*MemAdmissionPct/100. Either being 0 disables the check.
	MemAdmissionPct int
	HostMemMB       int64
	// MemReserveMB is the per-VM working-set floor that turns on live overcommit:
	// when > 0, admission counts this (not the full memory ceiling) per running
	// sandbox, and the idle reaper balloons a warm sandbox down to leave only
	// this much resident. 0 (the default) keeps the old behaviour — count the
	// full ceiling, never balloon — so overcommit is opt-in and measurement-set.
	MemReserveMB int64
	// NodeName identifies this host in capacity reports (defaults to "local").
	NodeName string
	// HostVCPUs is the host's logical CPU count for capacity reports (0 = unknown).
	HostVCPUs int64
	// FrontDoor, if set, gets Ensure/Remove calls as sandboxes come and go.
	FrontDoor FrontDoor
}

func NewManager(opts Options) (*Manager, error) {
	m := &Manager{
		driver:      opts.Driver,
		log:         opts.Logger,
		path:        filepath.Join(opts.StateDir, "sandboxes.json"),
		boxes:       map[string]*Sandbox{},
		gwPubKey:    opts.GatewayPublicKey,
		routes:      opts.Routes,
		schedules:   opts.Schedules,
		maxPerOwner: opts.MaxRunningPerOwner,
		memAdmitPct: opts.MemAdmissionPct,
		hostMemMB:   opts.HostMemMB,
		reserveMB:   opts.MemReserveMB,
		nodeName:    opts.NodeName,
		hostVCPUs:   opts.HostVCPUs,
		frontDoor:   opts.FrontDoor,
	}
	// The balloon reclaim path is optional — only firecracker (and the mock)
	// implement it. Detect it once so the reaper and resume paths can use it.
	if bl, ok := opts.Driver.(vmm.Ballooner); ok {
		m.balloon = bl
	}
	if m.nodeName == "" {
		m.nodeName = "local"
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
	if m.frontDoor != nil {
		m.frontDoor.Ensure(ctx, name)
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
				used += m.effectiveMemMB(b.MemMB)
			}
		}
		budget := m.hostMemMB * int64(m.memAdmitPct) / 100
		cost := m.effectiveMemMB(memMB)
		if used+cost > budget {
			return &CapacityError{RequestedMB: cost, UsedMB: used, BudgetMB: budget}
		}
	}
	return nil
}

// effectiveMemMB is what admission charges for a running sandbox. With live
// overcommit on (reserveMB > 0) we charge the working-set floor instead of the
// full ceiling, since idle guests are ballooned down to ~reserveMB and the
// balloon's deflate-on-oom + host swap absorb the rare spike. Off, we charge
// the full ceiling (the old, pessimistic Firecracker-reservation accounting).
func (m *Manager) effectiveMemMB(memMB int64) int64 {
	if m.reserveMB > 0 && m.reserveMB < memMB {
		return m.reserveMB
	}
	return memMB
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

// GetByHostIP returns the running sandbox whose guest IP is ip. This is how
// the metadata service attributes a connection to a sandbox: the guest IP is
// the source address of a request that reached the host's tap, which TCP makes
// unforgeable (see internal/metadata). Paused sandboxes have no address and
// never match.
func (m *Manager) GetByHostIP(ip string) (*Sandbox, bool) {
	if ip == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.boxes {
		if b.HostIP == ip && b.State == vmm.StateRunning {
			return copyOf(b), true
		}
	}
	return nil, false
}

// RecordKey stamps the SSH key fingerprint that authenticated a session for
// this sandbox. Best-effort bookkeeping for the `key_fp` claim: a sandbox is
// never failed over it.
func (m *Manager) RecordKey(name, fp string) {
	if fp == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.boxes[name]; ok && b.KeyFP != fp {
		b.KeyFP = fp
		m.save() //nolint:errcheck
	}
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

// NodeCapacity is one host's resource picture: what the box has, what the
// admission controller will hand out, and what running sandboxes have claimed.
// Sparkbox is single-host today, but capacity is reported per node so a
// multi-box control plane can aggregate a []NodeCapacity without a schema
// change. Total* fields are 0 when the host couldn't be inspected (non-Linux
// dev machines); BudgetMemMB is 0 when admission control is disabled.
type NodeCapacity struct {
	Node        string `json:"node"`
	TotalVCPUs  int64  `json:"total_vcpus"`
	TotalMemMB  int64  `json:"total_mem_mb"`
	BudgetMemMB int64  `json:"budget_mem_mb"` // TotalMemMB * MemAdmissionPct/100
	UsedVCPUs   int64  `json:"used_vcpus"`    // allocated to running sandboxes
	UsedMemMB   int64  `json:"used_mem_mb"`   // allocated ceiling of running sandboxes (sum of MemMB)
	// EffectiveMemMB is what admission actually charges running sandboxes: the
	// working-set reserve under live overcommit, or the full ceiling when off.
	// This — not UsedMemMB — is what to compare against BudgetMemMB.
	EffectiveMemMB int64 `json:"effective_mem_mb"`
	// ReserveMemMB is the per-VM working-set floor; 0 means overcommit is off.
	ReserveMemMB int64 `json:"reserve_mem_mb"`
	Running      int   `json:"running"`
	Sandboxes    int   `json:"sandboxes"`
}

// Capacity reports this node's resources. Used* counts only running sandboxes,
// mirroring the admission check: paused sandboxes cost disk, not RAM/CPU.
func (m *Manager) Capacity() NodeCapacity {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := NodeCapacity{
		Node:         m.nodeName,
		TotalVCPUs:   m.hostVCPUs,
		TotalMemMB:   m.hostMemMB,
		ReserveMemMB: m.reserveMB,
		Sandboxes:    len(m.boxes),
	}
	if m.memAdmitPct > 0 {
		c.BudgetMemMB = m.hostMemMB * int64(m.memAdmitPct) / 100
	}
	for _, b := range m.boxes {
		if b.State == vmm.StateRunning {
			c.Running++
			c.UsedVCPUs += b.VCPUs
			c.UsedMemMB += b.MemMB
			c.EffectiveMemMB += m.effectiveMemMB(b.MemMB)
		}
	}
	return c
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
	// Activity returns a ballooned-down sandbox to full RAM (whether it was
	// just resumed or was warm-but-ballooned).
	m.deflate(ctx, b)
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

// MemStats reports a running sandbox's real memory use in MiB, read from its
// balloon statistics — the live-overcommit signal: what the guest actually
// touches versus its configured ceiling. ok=false when unavailable (the driver
// has no balloon, the sandbox isn't running, or it predates the balloon device,
// e.g. an old snapshot). The driver call is made without m.mu held.
func (m *Manager) MemStats(ctx context.Context, name string) (usedMiB int64, ok bool) {
	m.mu.Lock()
	b, exists := m.boxes[name]
	if !exists || m.balloon == nil || b.State != vmm.StateRunning {
		m.mu.Unlock()
		return 0, false
	}
	memMB, bl := b.MemMB, m.balloon
	m.mu.Unlock()

	st, err := bl.BalloonStats(ctx, name)
	if err != nil {
		return 0, false
	}
	// The guest sees (ceiling − ballooned) RAM; what it's actually using is that
	// minus what it reports free. This is roughly the host RAM the VM costs.
	used := memMB - st.ActualMiB - st.FreeMiB
	if used < 0 {
		used = 0
	}
	return used, true
}

// SetPinned marks a sandbox pinned (exempt from the idle reaper) or clears the
// flag. Pinning does not itself resume the sandbox — callers that want it warm
// immediately follow with EnsureRunning.
func (m *Manager) SetPinned(name string, pinned bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok {
		return fmt.Errorf("sandbox %q not found", name)
	}
	if b.Pinned == pinned {
		return nil
	}
	b.Pinned = pinned
	m.log.Info("sandbox pin changed", "name", name, "pinned", pinned)
	return m.save()
}

// ResumePinned brings every pinned sandbox back to running. Called once at
// startup (after load), since a process restart marks all sandboxes paused —
// without this a host reboot would silently freeze a pinned daemon until
// someone next connected. Best-effort: a pinned sandbox that can't be admitted
// or resumed is logged and left paused rather than failing boot.
func (m *Manager) ResumePinned(ctx context.Context) {
	for _, b := range m.List() {
		if !b.Pinned || b.State == vmm.StateRunning {
			continue
		}
		if _, err := m.EnsureRunning(ctx, b.Name); err != nil {
			m.log.Warn("resume pinned sandbox failed", "name", b.Name, "err", err)
			continue
		}
		m.log.Info("resumed pinned sandbox on boot", "name", b.Name)
	}
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
	if m.schedules != nil {
		if err := m.schedules.DeleteBySandbox(name); err != nil {
			m.log.Warn("schedule cleanup failed", "name", name, "err", err)
		}
	}
	if m.frontDoor != nil {
		m.frontDoor.Remove(ctx, name)
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

// RunReaper walks idle sandboxes down the activity gradient. Blocks until ctx
// is done. Two thresholds: a running sandbox idle past balloonAfter is
// *ballooned down* (RAM reclaimed to the host, VM still running); idle past
// pauseAfter it is *paused* (snapshotted, RAM fully freed). balloonAfter <= 0
// or no reserve configured skips the balloon stage — straight to pause, the
// old behaviour.
func (m *Manager) RunReaper(ctx context.Context, balloonAfter, pauseAfter, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reapOnce(ctx, balloonAfter, pauseAfter)
		}
	}
}

// reapOnce applies one pass of the idle policy. Split out from RunReaper's loop
// so the two-stage gradient is unit-testable without a ticker.
func (m *Manager) reapOnce(ctx context.Context, balloonAfter, pauseAfter time.Duration) {
	for _, b := range m.List() {
		// Pinned sandboxes hold their full RAM on purpose so in-guest
		// timers/daemons keep firing; the reaper never touches them.
		if b.Pinned || b.State != vmm.StateRunning {
			continue
		}
		idle := time.Since(b.LastActive)
		switch {
		case idle > pauseAfter:
			if err := m.Pause(ctx, b.Name); err != nil {
				m.log.Error("reaper pause failed", "name", b.Name, "err", err)
			} else {
				m.log.Info("reaper paused idle sandbox", "name", b.Name)
			}
		case m.reserveMB > 0 && balloonAfter > 0 && !b.Ballooned && idle > balloonAfter:
			if err := m.balloonDown(ctx, b.Name); err != nil {
				m.log.Warn("reaper balloon-down failed", "name", b.Name, "err", err)
			}
		}
	}
}

// balloonDown reclaims a running sandbox's idle RAM to the host by inflating
// its balloon to leave only the working-set reserve, without pausing it. A
// no-op if overcommit is off, the driver has no balloon, or there's nothing to
// reclaim above the reserve.
func (m *Manager) balloonDown(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok || b.State != vmm.StateRunning || b.Ballooned || m.balloon == nil || m.reserveMB <= 0 {
		return nil
	}
	target := b.MemMB - m.reserveMB
	if target <= 0 {
		return nil
	}
	if err := m.balloon.SetBalloonTarget(ctx, name, target); err != nil {
		return err
	}
	b.Ballooned = true
	m.log.Info("reaper ballooned down idle sandbox", "name", name, "reclaim_mb", target)
	return m.save()
}

// deflate returns a ballooned sandbox's RAM on reactivation. Callers hold m.mu.
func (m *Manager) deflate(ctx context.Context, b *Sandbox) {
	if !b.Ballooned || m.balloon == nil {
		return
	}
	if err := m.balloon.SetBalloonTarget(ctx, b.Name, 0); err != nil {
		m.log.Warn("balloon deflate failed", "name", b.Name, "err", err)
		return
	}
	b.Ballooned = false
	m.log.Info("deflated balloon on activity", "name", b.Name)
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
