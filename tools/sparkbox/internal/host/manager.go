// Package host is the single-host control plane: it owns sandbox records,
// persists them to a JSON state file, drives the vmm.Driver, and pauses idle
// sandboxes (the suspend-to-snapshot cost lever).
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/fleetmetrics"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/reserved"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/routes"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
	"golang.org/x/sync/singleflight"
)

// validName reports whether a sandbox name is well formed. The charset is the
// platform's one label rule and lives in internal/reserved with the claimed
// list, because the node roster and the browser terminal each used to carry
// their own copy of exactly this expression.
func validName(name string) bool { return reserved.ValidLabel(name) }

// reservedName reports whether a sandbox may not take this name. A sandbox's
// name is its default subdomain, so the answer is the platform-wide one:
// internal/reserved owns the list, and the routes store and the user store ask
// it the same question. This used to be a local map kept in deliberate sync
// with users.reserved, which is exactly the arrangement that drifted.
func reservedName(name string) bool { return reserved.Name(name) }

// Default per-sandbox resources, applied when the caller passes <= 0 (the SSH
// `new@` path always does; the HTTP API may override). Bounded only by host
// capacity — an 8c/16t/64GB box fits ~5 of these before overcommit.
const (
	// DefaultMemMB is exported because a caller that has to decide whether a
	// sandbox fits a given machine — internal/devpod, sizing a node for a
	// laptop's container machine — must read the real number rather than
	// restate it. defaultVCPUs has no such caller.
	defaultVCPUs int64 = 4
	DefaultMemMB int64 = 12288

	// activityInterval is the shortest gap between activity marks we retain for
	// one sandbox. A mark is deliberately approximate: the idle thresholds are
	// measured in minutes, while keeping one timestamp per request would turn a
	// page load into a lock storm for no additional scheduling information.
	activityInterval = 10 * time.Second
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
	// Owner is set when the exhausted budget is an owner's pooled entitlement;
	// empty means the node-wide safety budget.
	Owner                         string
	RequestedMB, UsedMB, BudgetMB int64
}

func (e *CapacityError) Error() string {
	if e.Owner != "" {
		return fmt.Sprintf("memory pool full for %s: %d MB running + %d MB requested exceeds the %d MB pool",
			e.Owner, e.UsedMB, e.RequestedMB, e.BudgetMB)
	}
	return fmt.Sprintf("host at capacity: %d MB running + %d MB requested exceeds the %d MB budget",
		e.UsedMB, e.RequestedMB, e.BudgetMB)
}

// NameProblem says why a name was refused, so a transport can answer "you typed
// it wrong" (400) apart from "somebody already has it" (409).
type NameProblem int

const (
	NameInvalid  NameProblem = iota // fails the charset rule
	NameReserved                    // collides with a subdomain the edge owns
	NameTaken                       // already in use by an existing object
)

// NameError reports a name the caller may not use. These three conditions are
// caller mistakes, not faults, but they used to be bare fmt.Errorf values that
// every transport reported as a 500 — a client cannot retry its way out of a
// typo, and the noise hid real faults. The message strings are reproduced
// exactly as they were so the SSH channel's wording is unchanged.
type NameError struct {
	Problem NameProblem
	Noun    string // "sandbox" or "snapshot"
	Name    string
}

func (e *NameError) Error() string {
	switch e.Problem {
	case NameReserved:
		return fmt.Sprintf("%s name %q is reserved", e.Noun, e.Name)
	case NameTaken:
		return fmt.Sprintf("%s %q already exists", e.Noun, e.Name)
	default:
		return fmt.Sprintf("invalid %s name %q (lowercase alphanumerics and dashes)", e.Noun, e.Name)
	}
}

// DiskQuotaError is returned when a create or restore would push an owner's
// pooled on-disk usage past their per-owner disk budget.
type DiskQuotaError struct {
	Owner                       string
	RequestedMB, UsedMB, PoolMB int64
}

func (e *DiskQuotaError) Error() string {
	return fmt.Sprintf("disk pool full for %s: %d MB used + %d MB requested exceeds the %d MB pool",
		e.Owner, e.UsedMB, e.RequestedMB, e.PoolMB)
}

// MissingError, StateError and DisabledError are the other three shapes a
// caller's mistake takes, alongside NameError. They exist for the same reason it
// does: a bare fmt.Errorf reaches every transport as an unclassified fault, so a
// 500 is what a client gets for a name collision it can never retry its way out
// of, and the noise buries real faults. The message strings are reproduced
// exactly as they were, so the SSH channel's wording is unchanged — only the
// HTTP status and the log level move.

// MissingError reports an object that is not there (or not the caller's; the
// manager's own checks conflate the two on purpose).
type MissingError struct {
	Noun string // "sandbox" or "snapshot"
	Name string
}

func (e *MissingError) Error() string { return fmt.Sprintf("%s %q not found", e.Noun, e.Name) }

// StateError reports a well-formed request the current state refuses: renaming
// an archived sandbox, a subdomain another route already holds, a sandbox that
// was resumed underneath a rename. Code is the stable token a client switches
// on, since these have no other machine-readable distinction.
type StateError struct {
	Code string
	Msg  string
}

func (e *StateError) Error() string { return e.Msg }

// DisabledError reports a capability this host has not configured, as opposed to
// one that failed. The distinction is what lets a client hide a button rather
// than discover the gap by watching an operation fail.
type DisabledError struct {
	Code string
	Msg  string
}

func (e *DisabledError) Error() string { return e.Msg }

type Sandbox struct {
	ID         string    `json:"id"`
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
	// NetRxBytes/NetTxBytes are lifetime network totals from the guest's point
	// of view: bytes it received and bytes it sent. The driver's underlying
	// counters die with the host-side tap on every pause/resume, so these are
	// accumulated from per-sample deltas (sampleVitals) rather than read
	// straight through — they only ever grow, and they survive a restart
	// because they ride in the state file. Metering for future egress limits.
	NetRxBytes uint64 `json:"net_rx_bytes,omitempty"`
	NetTxBytes uint64 `json:"net_tx_bytes,omitempty"`
	// Repos is the latest git state reported from inside this guest. The host
	// cannot safely mount or inspect a running guest filesystem, so the
	// read-only in-guest survey publishes this bounded snapshot through the
	// tap-authenticated metadata service. It is advisory UI/safety data, never
	// an authorization input. RepoStatusAt says how fresh the snapshot is.
	Repos        []RepoStatus `json:"repos,omitempty"`
	RepoStatusAt time.Time    `json:"repo_status_at,omitempty"`
	// ArchiveKey is the object-storage key holding this sandbox's rootfs when
	// State is archived (empty otherwise). ArchivedAt is when it was parked.
	// Resume-on-connect downloads the archive and cold-boots it (Manager.restore).
	ArchiveKey string    `json:"archive_key,omitempty"`
	ArchivedAt time.Time `json:"archived_at,omitempty"`
	// CheckpointKey identifies the latest immutable durable copy of this
	// sandbox's rootfs. Unlike an archive it remains set while the local disk is
	// present, and restoring it never consumes the durable object.
	CheckpointKey string    `json:"checkpoint_key,omitempty"`
	CheckpointAt  time.Time `json:"checkpoint_at,omitempty"`
	// DiskMB is this sandbox's durable root-filesystem usage. Host representation
	// details (sparse holes and shared reflink extents) and the regenerable
	// memory snapshot are deliberately excluded. For an archived box it is the
	// stored archive size because there is no live filesystem to measure.
	// Refreshed by the reaper and summed per owner for the pooled-disk admission
	// check.
	DiskMB int64 `json:"disk_mb,omitempty"`
	// DiskTotalMB is the guest's hard disk ceiling — the size of its rootfs
	// filesystem, which it cannot grow past. Discovered from the image rather
	// than configured, so boxes built from different templates report their own.
	// 0 when the driver can't say, which the consoles render as a bare figure
	// with no meter.
	DiskTotalMB int64 `json:"disk_total_mb,omitempty"`
	// BaseDiskMB is the used-blocks figure of the template named by Image, as
	// last measured by the driver. Every one of those blocks physically exists
	// once on the host and is shared by reflink with every other sandbox forked
	// from the same image, so pooled accounting subtracts it (pooledDiskMB) and
	// charges an owner only for what their sandboxes wrote. 0 when the driver
	// can't measure templates, which simply restores the old raw charging.
	//
	// It is measured against this machine's image dir but it does cross the node
	// link (nodelink/frame.go, grpccontrol/convert.go), because a gateway that
	// cannot see it has to charge every remote sandbox raw — a 25 GB fork that
	// physically cost 400 MB. A node too old to send it, or one whose driver
	// cannot measure templates, sends 0, which restores exactly that raw
	// charging rather than breaking anything.
	//
	// The PER-SANDBOX meters in both consoles stay raw on purpose: a person
	// looking at one machine wants the disk that machine sees. Only the pooled
	// per-owner rollup (CapacityForOwner) subtracts it, which is what makes the
	// user console's footprint card able to say how much sharing saved.
	BaseDiskMB int64 `json:"base_disk_mb,omitempty"`
	// RenamedFrom journals an in-flight rename (see Rename): the record is
	// saved under its new name with this set to the old name before the VM dir
	// moves on disk, so a crash between the two converges at the next load
	// (see NewManager) instead of stranding the rootfs under the other name.
	// Empty except inside that window.
	RenamedFrom string `json:"renamed_from,omitempty"`
	// Node names the machine whose driver runs this VM. A node's own manager
	// writes its own name here; a gateway routing across a fleet overwrites it
	// from its placement ledger, which is the only authorization input.
	Node string `json:"node,omitempty"`
	// Unreachable is set only by a gateway routing across a fleet, never by a
	// node's own manager: it means the machine holding this sandbox is not
	// answering the control plane. There is deliberately no fourth vmm.State —
	// every `b.State ==` switch in host, envsync, netpush and both consoles
	// treats "not running" as "safe to ignore", which is right, and a fourth
	// value would have to be handled in all of them.
	Unreachable bool `json:"unreachable,omitempty"`
	// Turbo means this sandbox is currently booted with doubled CPU and RAM —
	// VCPUs and MemMB above hold the doubled figures, and BaseVCPUs/BaseMemMB
	// remember what to go back to.
	//
	// The doubling lives in VCPUs/MemMB rather than beside them so that nothing
	// downstream has to know about turbo at all: admission, the balloon target,
	// the vmm.Config a cold boot is built from and every meter's denominator all
	// keep reading the one pair of fields that has always meant "what this VM
	// has". Turbo is the bit that says those two are borrowed.
	//
	// It lasts exactly one run. Every path that stops the VM goes through
	// Manager.pause, which hands the resources back — so an idle reap, an
	// explicit pause, a reboot or a rename all end turbo, and the next resume
	// comes up at the sandbox's own size. See SetTurbo.
	Turbo bool `json:"turbo,omitempty"`
	// BaseVCPUs and BaseMemMB are the sandbox's own allocation, held aside while
	// Turbo is on. Zero when it is off — the fields have no meaning then, and an
	// omitted pair is what every record written before turbo existed looks like.
	BaseVCPUs int64 `json:"base_vcpus,omitempty"`
	BaseMemMB int64 `json:"base_mem_mb,omitempty"`
	// HiveMind is an ephemeral view of sessions attributed to this VM. It is
	// refreshed from a token-bound API response and deliberately never written
	// to the sandbox state file.
	HiveMind *HiveMindSessionSnapshot `json:"-"`
	// HiveMindPresence is the live half of the same view, and it is separate
	// from HiveMind because the two are refreshed on very different clocks: the
	// presence endpoint is cheap and polled every minute, the session catalog is
	// paginated and polled far less often. Folding them into one field would
	// force the expensive query onto the cheap one's schedule.
	//
	// It is also the only trustworthy answer to "is an agent working here".
	// A catalog row's State reaches "ended" at a turn boundary while the session
	// is still open and the device is still live, so a surface that reports
	// activity must read this and not HiveMind.Sessions[i].State.
	HiveMindPresence *HiveMindPresence `json:"-"`
}

// RepoStatus is one checkout as observed by the guest. Ahead and Behind are
// relative to Upstream after the reporting pass fetched its remote; Dirty
// includes untracked files. Missing checkouts retain their configured path and
// report State "missing" with the git-specific fields empty.
type RepoStatus struct {
	Slug     string `json:"slug"`
	Path     string `json:"path"`
	Branch   string `json:"branch,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Ahead    int64  `json:"ahead,omitempty"`
	Behind   int64  `json:"behind,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
	State    string `json:"state"`
}

type HiveMindSession struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	URL            string     `json:"url"`
	State          string     `json:"state"`
	AgentType      string     `json:"agent_type"`
	Model          string     `json:"model"`
	StartedAt      time.Time  `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at"`
	LastActivityAt time.Time  `json:"last_activity_at"`
}

type HiveMindSessionSnapshot struct {
	ObservedAt time.Time         `json:"observed_at"`
	Sessions   []HiveMindSession `json:"sessions"`
	TotalCount int               `json:"total_count"`
	HasMore    bool              `json:"has_more"`
}

// HiveMindPresence is what the cheap presence endpoint last said about a VM.
//
// State is HiveMind's own word for what the device is doing — "idle",
// "waiting", and whatever else it grows — carried through as an opaque string
// rather than mapped onto an enum here. A new value HiveMind starts sending
// should reach a person unchanged, not become "unknown" because this end of the
// federation has not been redeployed.
//
// ProtectUntil is nil when nothing is live. When it is set, something is:
// it is the same field the reaper honours, so it is the one signal that is
// authoritative in both directions rather than only descriptive.
type HiveMindPresence struct {
	ObservedAt   time.Time  `json:"observed_at"`
	State        string     `json:"state,omitempty"`
	ProtectUntil *time.Time `json:"protect_until,omitempty"`
}

// Recent is the most recently active session in the snapshot, or nil for one
// holding none. It selects by LastActivityAt rather than trusting the order the
// API returned, falling back to StartedAt to break a tie.
//
// It deliberately does not filter on State. A session reaches "ended" at a turn
// boundary and is still the one a person is working in — see the note on
// Sandbox.HiveMindPresence — so filtering here would routinely hide the only
// session worth naming.
func (s *HiveMindSessionSnapshot) Recent() *HiveMindSession {
	if s == nil {
		return nil
	}
	var best *HiveMindSession
	for i := range s.Sessions {
		session := &s.Sessions[i]
		if best == nil || session.LastActivityAt.After(best.LastActivityAt) ||
			(session.LastActivityAt.Equal(best.LastActivityAt) &&
				session.StartedAt.After(best.StartedAt)) {
			best = session
		}
	}
	return best
}

// Live reports whether HiveMind currently considers this VM's agent to be
// working. It reads ProtectUntil rather than State because that is the field
// with a defined meaning on both sides of the federation, and because an
// expired lease must stop counting as live without anyone re-observing it.
func (p *HiveMindPresence) Live() bool {
	return p != nil && p.ProtectUntil != nil && p.ProtectUntil.After(time.Now())
}

// TurboFactor is how much a turbo boot multiplies a sandbox's CPU and RAM by.
// One constant, because the console renders the multiplier it promises ("2×")
// from the same number the manager allocates with.
const TurboFactor = 2

// ScheduleCleaner drops a sandbox's platform-scheduler entries when it is
// destroyed, so a deleted sandbox leaves no jobs that would wake a ghost
// forever. Satisfied structurally by *schedule.Store (avoids importing it).
type ScheduleCleaner interface {
	DeleteBySandbox(sandbox string) error
}

// TagCleaner keeps sandbox tag rows (see internal/secrets) in step with the
// sandbox lifecycle: rows are dropped when a sandbox is destroyed and follow a
// rename. Satisfied structurally by *secrets.Store (avoids importing it).
type TagCleaner interface {
	DeleteBySandbox(sandbox string) error
	RenameSandbox(old, new string) error
}

// RepoRefCleaner keeps per-sandbox repo ref overrides (see internal/repos,
// SetSandboxRefs) in step with the sandbox lifecycle. Same two methods as
// TagCleaner and deliberately a separate name, because it guards a different
// thing and its absence is a different bug.
//
// It is not optional the way the others are pleasant-to-have. Every other table
// in internal/repos is keyed by a TAG, which outlives sandboxes on purpose;
// this one is keyed by a sandbox NAME, and names are reusable. A row left
// behind by a destroyed sandbox silently decides what its name-successor checks
// out — a different box, possibly a different person's. Satisfied structurally
// by *repos.Store (avoids importing it).
type RepoRefCleaner interface {
	DeleteBySandbox(sandbox string) error
	RenameSandbox(old, new string) error
}

// sandboxRenamer is the rename hook side stores (routes, schedules) grow so
// their sandbox-keyed rows follow a rename. Detected with a type assertion at
// call time, so a store that predates the method is simply skipped.
type sandboxRenamer interface {
	RenameSandbox(old, new string) error
}

// EnvPusher pushes an owner's secret environment into a sandbox's
// /etc/environment over SSH (see internal/envsync). Optional and always
// best-effort: a lifecycle operation is never failed over a push.
type EnvPusher interface {
	PushEnv(ctx context.Context, box *Sandbox) error
}

// RepoSyncer nudges a sandbox into reconciling its GitHub checkouts with the
// attachments its owner's tags imply (see internal/envsync). Optional, and —
// unlike EnvPusher — never fired from the lifecycle: the guest checks out at
// boot on its own, so the only moment anything needs to push it is when an
// owner changes what is attached to a box that is ALREADY running. Errors are
// returned rather than swallowed, because the caller is a person who just
// retagged something and is owed the answer.
type RepoSyncer interface {
	SyncRepos(ctx context.Context, box *Sandbox) error
}

// ErrNoRepoSupport is a sandbox whose rootfs predates the repo payload: no
// checkout binary in the guest, so there is nothing to nudge and never will be
// until the box is recreated from a current template.
//
// It lives here rather than in the package that detects it because it is part
// of the RepoSyncer contract, and because the surface that must PRINT it —
// internal/ctlops — cannot import the implementation: envsync reaches ctlops
// through sshgw, so the dependency only runs one way.
var ErrNoRepoSupport = errors.New("this sandbox was created before repo support and cannot check repositories out")

// ToolRefresher installs the agent CLIs this host has cached into a running
// sandbox (see internal/envsync). Optional, and fired from exactly one place:
// immediately before a snapshot capture, so a template starts current instead
// of frozen at the tool versions of the day it was taken. Every fork copies
// that disk byte-for-byte, so a template captured stale stays stale for every
// sandbox anybody ever makes from it.
//
// Unlike RepoSyncer's nudge this must be SYNCHRONOUS: the caller is about to
// pause the guest, and a pause landing halfway through writing /usr/local/bin/
// claude would freeze a truncated executable into the template.
type ToolRefresher interface {
	RefreshTools(ctx context.Context, box *Sandbox) error
}

// ErrNoToolRefresh is a guest with no updater in it — a sandbox created before
// the tool payload existed. Named rather than silent for the same reason
// ErrNoRepoSupport is: the box otherwise reports a successful snapshot of tools
// nothing could have updated.
//
// It lives here, beside the interface it belongs to, because internal/ctlops
// may have to print it and cannot import envsync (envsync reaches ctlops
// through sshgw, so that dependency only runs one way).
var ErrNoToolRefresh = errors.New("this sandbox was created before agent-tool updates and cannot refresh them")

// ErrNoEnvSetup is a guest with no environment-setup payload in it — a builder
// booted from a template that predates environment builds. Named rather than
// silent for the reason the two sentinels above are: the nudge otherwise
// succeeds, nothing ever runs, and the build sits in `building` until its
// timeout reports "the builder never reported", which names the wrong cause.
//
// It lives here beside them, and for their reason: internal/ctlops has to print
// it and cannot import envsync (envsync reaches ctlops through sshgw, so that
// dependency only runs one way).
var ErrNoEnvSetup = errors.New("this sandbox was created before environment builds and cannot run a setup script")

// FrontDoor is an optional hook for per-sandbox public-address plumbing (see
// internal/frontdoor): Ensure is called when a sandbox is created, Remove when
// it is destroyed. Implementations are expected to be best-effort — a sandbox
// is never failed over front-door plumbing.
type FrontDoor interface {
	Ensure(ctx context.Context, name string)
	Remove(ctx context.Context, name string)
}

// SessionCloser hangs up the interactive sessions attached to a sandbox that is
// about to stop being reachable, so an attached terminal is released instead of
// being left pointing at a VM that is no longer running. Satisfied by
// *sshgw.Gateway; nil simply skips the courtesy.
//
// Implementations MUST return without waiting for those sessions to finish
// unwinding: this is called with the manager's lock held, and a session's
// teardown path takes that same lock.
type SessionCloser interface {
	// CloseSandboxSessions closes every session attached to sandbox, telling the
	// user why (reason is a fragment like "paused after 30m idle"), and returns
	// how many it closed.
	CloseSandboxSessions(sandbox, reason string) int
}

// SessionDrainer is a SessionCloser for a caller that is NOT holding the
// manager's lock and therefore can afford to wait for the goodbye to actually
// reach the client before it lets the VM stop answering.
//
// The distinction exists because the local pause path and the fleet's remote
// one have opposite constraints, not because one of them is fussier. Locally
// the ordering that makes the goodbye land is free: Manager.pause closes the
// sessions and then calls the driver, in one process, so a hang-up goroutine
// that has merely been *started* still gets to write before the guest's sshd
// goes away — and it must not be waited on, because the manager holds a lock
// the teardown takes. When the sandbox is on another machine there is no such
// luck. The gateway holds the terminals, the machine holds the sshd, and
// "started the hang-up" no longer implies "wrote the goodbye first": the pause
// travels over a link, the guest dies over there, and the two race with the
// wire in between. So the fleet — which holds no lock at all when it dispatches
// — asks for the stronger guarantee and waits.
//
// Optional: nothing requires a SessionCloser to implement it, and a registry
// that does not simply keeps the weaker, racy ordering.
type SessionDrainer interface {
	SessionCloser
	// DrainSandboxSessions closes every session attached to sandbox and blocks
	// until their goodbyes have been written, or wait elapses, whichever comes
	// first. It returns how many it closed.
	DrainSandboxSessions(sandbox, reason string, wait time.Duration) int
}

// Observer is told about every change to a sandbox record, so a process that
// mirrors this host's inventory somewhere else can follow along instead of
// polling. Optional: nil on a host nobody is mirroring, which is every
// single-box deployment.
//
// It is a separate hook from SessionCloser rather than more methods on it
// because CloseSandboxSessions fires from exactly one place — the pause path —
// so a SessionCloser could only ever report pauses.
//
// Implementations MUST return without blocking: these fire from lifecycle
// methods that hold the manager's lock, and a slow observer would stall every
// other sandbox on the host.
type Observer interface {
	// SandboxChanged carries a copy of the record after the change. reason is
	// the transition ("created", "paused", "renamed", …), not the sentence a
	// human reads.
	SandboxChanged(b *Sandbox, reason string)
	// SandboxGone reports a record that no longer exists.
	SandboxGone(name string)
}

// Observers fans lifecycle notifications out to several independently
// nonblocking mirrors (for example, the legacy SSH emitter and the durable
// gRPC event journal).
type Observers []Observer

func (all Observers) SandboxChanged(b *Sandbox, reason string) {
	for _, observer := range all {
		if observer != nil {
			observer.SandboxChanged(b, reason)
		}
	}
}

func (all Observers) SandboxGone(name string) {
	for _, observer := range all {
		if observer != nil {
			observer.SandboxGone(name)
		}
	}
}

// ObjectStore is where archived sandbox rootfs artifacts are parked and fetched
// back (see internal/objstore). Optional — nil disables archiving, and the
// manager also needs the driver to implement vmm.Archivable. Keys are
// bucket-relative paths the manager lays out under <prefix>/<owner>/.
type ObjectStore interface {
	Put(ctx context.Context, key, localPath string) error
	Get(ctx context.Context, key, localPath string) error
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
}

type Manager struct {
	mu                 sync.Mutex
	ready              singleflight.Group // one restore/resume per sandbox at a time
	ctx                context.Context    // process lifetime for shared restore/resume work
	driver             vmm.Driver
	balloon            vmm.Ballooner        // driver's balloon capability, if it has one; else nil
	archiver           vmm.Archivable       // driver's pack/unpack/snapshot capability; else nil
	diskReport         vmm.DiskReporter     // driver's disk-usage capability; else nil
	templateReport     vmm.TemplateReporter // driver's template-measurement capability; else nil
	renamer            vmm.Renamer          // driver's VM-rename capability; else nil
	rebooter           vmm.Rebooter         // driver's snapshot-discard capability; else nil
	diskResize         vmm.DiskResizer      // driver's disk-grow capability; else nil
	cpuStats           vmm.CPUStatser       // driver's CPU-time capability; else nil
	netStats           vmm.NetStatser       // driver's network-counter capability; else nil
	archive            ObjectStore          // object store for archives; nil disables archiving
	checkpoint         ObjectStore          // immutable durable checkpoints; nil disables checkpointing
	log                *slog.Logger
	stateDir           string // dir holding sandboxes.json + transient archive staging
	checkpointStageDir string // local directory for checkpoint pack/download staging
	path               string // JSON state file
	boxes              map[string]*Sandbox
	snaps              map[string]*Snapshot // fork-able templates, keyed by template image name
	snapsPath          string               // snapshots.json
	gwPubKey           string
	routes             *routes.Store           // optional: proxy route bookkeeping
	schedules          ScheduleCleaner         // optional: platform-scheduler cleanup on destroy
	frontDoor          FrontDoor               // optional: per-sandbox address plumbing
	tags               TagCleaner              // optional: sandbox tag-row cleanup on destroy/rename
	repoRefs           RepoRefCleaner          // optional: per-sandbox repo ref overrides, cleaned on destroy and moved on rename
	envSync            EnvPusher               // optional: secret-env push when a sandbox reaches running
	repoSync           RepoSyncer              // optional: repo checkout nudge when tags or attachments change
	toolSync           ToolRefresher           // optional: agent-CLI refresh run once, just before a snapshot capture
	sessions           SessionCloser           // optional: hang up attached sessions when a sandbox pauses
	observer           Observer                // optional: relay record changes to whoever mirrors this host
	maxPerOwner        int                     // max running sandboxes per owner; 0 = unlimited
	maxBoxesPerOwner   int                     // max total sandbox identities per owner; 0 = unlimited
	memAdmitPct        int                     // RAM admission threshold as % of host; 0 = disabled
	hostMemMB          int64                   // host RAM in MB for admission; 0 = disabled
	reserveMB          int64                   // per-VM working-set floor for admission + balloon; 0 = off (count full ceiling)
	ownerMemPoolMB     int64                   // per-owner effective-memory budget; 0 = disabled
	ownerMemBurstMB    int64                   // temporary per-owner ceiling for turbo; 0 = baseline pool
	diskPoolMB         int64                   // per-owner pooled-disk budget in MB; 0 = disabled
	defVCPUs           int64                   // vCPUs for a sandbox created without a size; 0 = DefaultVCPUs
	defMemMB           int64                   // MB for a sandbox created without a size; 0 = DefaultMemMB
	archivePfx         string                  // object-key prefix for archives (default "archives")
	checkpointPfx      string                  // object-key prefix for checkpoints (default "checkpoints")
	nodeName           string                  // this host's name in capacity reports
	nodeArch           string                  // this host's CPU architecture in capacity reports
	nodeRelease        string                  // this host's sparkbox release tag in capacity reports
	hostVCPUs          int64                   // host logical CPUs for capacity reports; 0 = unknown
	actCPUPct          float64                 // activity floor: % of one core over a sample; 0 = off
	actNetBytes        uint64                  // activity floor: bytes per sample; 0 = off
	vitals             map[string]vitalsSample // last CPU/net reading per sandbox, for deltas
	memUsed            map[string]int64        // latest observed guest working set per running sandbox
	memUsedAt          time.Time               // when memUsed was last refreshed, so two controllers on one cadence sample once
	// balloonedAt is when the reaper last inflated a sandbox's balloon, for the
	// settle window applyVitals judges CPU against. Cleared by deflate and by
	// every path that drops the vitals baseline.
	balloonedAt map[string]time.Time
	metrics     *fleetmetrics.Registry // optional node-local persistence/readiness metrics
	diskOps     sync.Map               // sandbox name -> *sync.Mutex; serializes rootfs lifecycle work
	portScans   singleflight.Group     // one supported-port scan per sandbox at a time
	portScanMu  sync.Mutex
	portScan    map[string]portScanSample // short-lived results keyed by sandbox name

	// Activity is intentionally kept off mu on the offer path. Lifecycle
	// operations hold mu across driver calls, which can take seconds; a web
	// request marking another sandbox active must not wait behind one. The
	// asynchronous applier updates the live record and deflates a balloon,
	// while save/FlushActivity merge the pending timestamps durably.
	activityMu sync.Mutex
	activity   map[string]time.Time // dirty timestamps not yet persisted
	markedAt   map[string]time.Time // last accepted mark, for coalescing

	// protectUntil holds short, external activity leases keyed by immutable
	// sandbox ID. They are intentionally memory-only: a HiveMind outage or a
	// process restart must not turn yesterday's observation into a permanent
	// exemption from scale-to-zero.
	protectUntil map[string]time.Time
}

// vitalsSample is the previous reaper-tick reading of a sandbox's resource
// counters, kept in memory only: it exists to turn the drivers' cumulative
// counters into per-interval rates. It is deliberately not persisted — after a
// restart the first tick just re-primes rather than charging a bogus delta
// against a counter that reset while we were down.
type vitalsSample struct {
	at       time.Time
	cpuNanos uint64
	rx, tx   uint64 // raw driver counters, which reset on every tap teardown
}

type Options struct {
	StateDir         string
	Driver           vmm.Driver
	GatewayPublicKey string
	Logger           *slog.Logger
	// Context owns shared restore/resume work. Request cancellation stops that
	// caller waiting but must not abort the one operation concurrent callers
	// share. Nil uses Background for tests and embedders without a lifecycle.
	Context context.Context
	// Routes, if set, gets a default route per sandbox on create and is cleaned
	// up on destroy. Nil disables proxy-route bookkeeping (used by unit tests).
	Routes *routes.Store
	// Schedules, if set, has a sandbox's schedules deleted when it is destroyed.
	Schedules ScheduleCleaner
	// Tags, if set, has a sandbox's tag rows deleted on destroy and moved on
	// rename. Satisfied structurally by *secrets.Store.
	Tags TagCleaner
	// RepoRefs, if set, has a sandbox's per-instance repo ref overrides deleted
	// on destroy and moved on rename. Satisfied structurally by *repos.Store.
	// See RepoRefCleaner for why leaving this nil on a host that HAS the
	// overrides is a correctness bug rather than a missing feature.
	RepoRefs RepoRefCleaner
	// MaxRunningPerOwner caps how many sandboxes one owner may have running at
	// once (0 = unlimited). Enforced on create and resume-on-connect.
	MaxRunningPerOwner int
	// MaxSandboxesPerOwner caps every non-deleted sandbox identity belonging to
	// one owner, regardless of whether it is running, paused, or archived.
	MaxSandboxesPerOwner int
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
	// OwnerMemoryPoolMB caps the aggregate effective memory charged to one
	// owner's running sandboxes. VM MemMB values remain ceilings; the owner pool
	// is the product entitlement they share. Zero disables the owner-level cap.
	OwnerMemoryPoolMB int64
	// OwnerMemoryBurstMB is the temporary aggregate effective-memory ceiling an
	// owner may reach through turbo. It does not reserve node RAM: different
	// owners' burst ceilings overlap, and node admission remains authoritative.
	// Zero uses OwnerMemoryPoolMB (no borrowing above the baseline).
	OwnerMemoryBurstMB int64
	// DefaultVCPUs and DefaultMemMB size a sandbox whose caller named no size —
	// which is every `new@` sandbox. Zero keeps the package defaults below,
	// which are sized for a CKS node.
	//
	// They exist because those defaults are not universally safe: a node runs
	// them whatever it is, and a dev box inside a 12 GB Linux VM would hand a
	// single sandbox 12288 MB — a guest larger than the machine hosting it.
	// Admission control cannot catch that (it compares against HostMemMB, and
	// one VM at the ceiling still "fits" a host claiming to be a cluster node),
	// so the size has to be settable per node rather than inferred from one.
	DefaultVCPUs int64
	DefaultMemMB int64
	// NodeName identifies this host in capacity reports (defaults to "local").
	NodeName string
	// Arch and Release describe this host in capacity reports: the CPU
	// architecture its sandboxes will run on and the sparkbox release it boots
	// them from. Both are empty when unknown — a fleet aggregating capacities
	// needs them to tell an arm64 box from an amd64 one, and a single-box
	// deployment has no use for either.
	Arch    string
	Release string
	// HostVCPUs is the host's logical CPU count for capacity reports (0 = unknown).
	HostVCPUs int64
	// FrontDoor, if set, gets Ensure/Remove calls as sandboxes come and go.
	FrontDoor FrontDoor
	// Observer, if set, is told about every sandbox record change. This is the
	// only way to install one: whatever mirrors this host is built before the
	// manager is, and a second wiring path would be a manager that emits
	// nothing because the field was set on the wrong side of a boot.
	Observer Observer
	// Archive is the object store for archived rootfs artifacts. Nil (or a driver
	// without vmm.Archivable) disables the archive/restore lifecycle.
	Archive ObjectStore
	// ArchivePrefix is the object-key prefix archives are written under
	// (default "archives"): <prefix>/<owner>/<name>.ext4.zst.
	ArchivePrefix string
	// Checkpoint is the store for immutable durable rootfs checkpoints. Nil
	// disables manual checkpoint/restore.
	Checkpoint ObjectStore
	// CheckpointPrefix is the object-key prefix for checkpoints.
	CheckpointPrefix string
	// CheckpointStagingDir holds temporary packed/downloaded artifacts. It
	// should be on the local hot tier; empty defaults to StateDir.
	CheckpointStagingDir string
	// DiskPoolMBPerOwner caps an owner's pooled durable usage across all their
	// sandboxes + archives (0 = unlimited). Soft accounting, enforced at
	// create/restore — see admit.
	DiskPoolMBPerOwner int64
	// ActivityCPUPct and ActivityNetBytes turn on in-guest activity detection:
	// each reaper tick samples every running sandbox's host CPU time and
	// network counters, and a sandbox busier than either threshold has its
	// idle clock reset. This is what keeps a long-running agent (or build, or
	// training job) with no inbound traffic from being reaped mid-flight —
	// without it the only activity signal is control-plane traffic.
	//
	// ActivityCPUPct is percent of ONE host core averaged across the sample
	// interval; ActivityNetBytes is bytes moved in either direction over that
	// interval. Either being 0 disables that half; both 0 keeps the old
	// traffic-only behaviour. Measured baselines on an idle box: ~0.4% CPU and
	// ~3 KB/min, against 3.6-14% CPU and 400 KB-4 MB/min for a working agent.
	ActivityCPUPct   float64
	ActivityNetBytes uint64
	// Metrics records bounded node-local lifecycle and persistence observations.
	// Nil preserves the historical no-instrumentation path.
	Metrics *fleetmetrics.Registry
}

func NewManager(opts Options) (*Manager, error) {
	if opts.OwnerMemoryBurstMB > 0 && opts.OwnerMemoryPoolMB <= 0 {
		return nil, errors.New("owner memory burst requires an owner memory pool")
	}
	if opts.OwnerMemoryBurstMB > 0 && opts.OwnerMemoryBurstMB < opts.OwnerMemoryPoolMB {
		return nil, errors.New("owner memory burst must be at least the owner memory pool")
	}
	lifecycle := opts.Context
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	m := &Manager{
		ctx:                lifecycle,
		driver:             opts.Driver,
		log:                opts.Logger,
		stateDir:           opts.StateDir,
		path:               filepath.Join(opts.StateDir, "sandboxes.json"),
		snapsPath:          filepath.Join(opts.StateDir, "snapshots.json"),
		boxes:              map[string]*Sandbox{},
		snaps:              map[string]*Snapshot{},
		vitals:             map[string]vitalsSample{},
		balloonedAt:        map[string]time.Time{},
		memUsed:            map[string]int64{},
		activity:           map[string]time.Time{},
		markedAt:           map[string]time.Time{},
		protectUntil:       map[string]time.Time{},
		actCPUPct:          opts.ActivityCPUPct,
		actNetBytes:        opts.ActivityNetBytes,
		gwPubKey:           opts.GatewayPublicKey,
		routes:             opts.Routes,
		schedules:          opts.Schedules,
		tags:               opts.Tags,
		repoRefs:           opts.RepoRefs,
		archive:            opts.Archive,
		checkpoint:         opts.Checkpoint,
		archivePfx:         opts.ArchivePrefix,
		checkpointPfx:      opts.CheckpointPrefix,
		checkpointStageDir: opts.CheckpointStagingDir,
		maxPerOwner:        opts.MaxRunningPerOwner,
		maxBoxesPerOwner:   opts.MaxSandboxesPerOwner,
		defVCPUs:           opts.DefaultVCPUs,
		defMemMB:           opts.DefaultMemMB,
		memAdmitPct:        opts.MemAdmissionPct,
		hostMemMB:          opts.HostMemMB,
		reserveMB:          opts.MemReserveMB,
		ownerMemPoolMB:     opts.OwnerMemoryPoolMB,
		ownerMemBurstMB:    opts.OwnerMemoryBurstMB,
		diskPoolMB:         opts.DiskPoolMBPerOwner,
		nodeName:           opts.NodeName,
		nodeArch:           opts.Arch,
		nodeRelease:        opts.Release,
		hostVCPUs:          opts.HostVCPUs,
		frontDoor:          opts.FrontDoor,
		observer:           opts.Observer,
		metrics:            opts.Metrics,
	}
	if m.archivePfx == "" {
		m.archivePfx = "archives"
	}
	if m.checkpointPfx == "" {
		m.checkpointPfx = "checkpoints"
	}
	if m.checkpointStageDir == "" {
		m.checkpointStageDir = opts.StateDir
	}
	if err := os.MkdirAll(m.checkpointStageDir, 0o700); err != nil {
		return nil, fmt.Errorf("create checkpoint staging directory: %w", err)
	}
	// The balloon reclaim path is optional — only firecracker (and the mock)
	// implement it. Detect it once so the reaper and resume paths can use it.
	if bl, ok := opts.Driver.(vmm.Ballooner); ok {
		m.balloon = bl
	}
	// Same for the disk-lifecycle capabilities: archive/restore/snapshot needs
	// vmm.Archivable; pooled-disk accounting needs vmm.DiskReporter.
	if ar, ok := opts.Driver.(vmm.Archivable); ok {
		m.archiver = ar
	}
	if dr, ok := opts.Driver.(vmm.DiskReporter); ok {
		m.diskReport = dr
	}
	// Template measurement is asserted separately from DiskReporter so a driver
	// that can read a live rootfs but not an image dir keeps its accounting and
	// loses only the reflink discount (every sandbox then charges its raw usage,
	// which is what happened before this existed).
	if tr, ok := opts.Driver.(vmm.TemplateReporter); ok {
		m.templateReport = tr
	}
	// And the user-console capabilities: rename, cold-boot reboot, CPU stats.
	if rn, ok := opts.Driver.(vmm.Renamer); ok {
		m.renamer = rn
	}
	if rb, ok := opts.Driver.(vmm.Rebooter); ok {
		m.rebooter = rb
	}
	if dr, ok := opts.Driver.(vmm.DiskResizer); ok {
		m.diskResize = dr
	}
	if cs, ok := opts.Driver.(vmm.CPUStatser); ok {
		m.cpuStats = cs
	}
	if ns, ok := opts.Driver.(vmm.NetStatser); ok {
		m.netStats = ns
	}
	if m.nodeName == "" {
		m.nodeName = "local"
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	if err := m.loadSnapshots(); err != nil {
		return nil, err
	}
	// Complete any rename that crashed between its journal save and its final
	// save (see Rename): the record already carries the new name, so re-run the
	// dir move — it succeeds when the crash preceded the move, and fails
	// harmlessly when the dir already moved (old gone, new present).
	for name, b := range m.boxes {
		if b.RenamedFrom == "" {
			continue
		}
		if m.renamer != nil {
			if err := m.renamer.RenameVM(b.RenamedFrom, name); err != nil {
				m.log.Info("rename reconcile: vm dir already moved",
					"name", name, "from", b.RenamedFrom, "err", err)
			}
		}
		b.RenamedFrom = ""
	}
	// Every record in this state dir is this machine's own — it is the machine
	// holding the rootfs — so the node name is stamped rather than filled in
	// only when missing. Records written before hosts had a name carry none;
	// records written before the host was renamed (--node-name, or a hostname
	// that moved) carry the old one, and a name nothing answers to reads as
	// another machine to a gateway, which then routes every one of them over a
	// link instead of into this process. Unreachable is a routing verdict some
	// other process makes about this one, so it is never loaded off disk.
	for _, b := range m.boxes {
		if b.ID == "" {
			b.ID = uuid.NewString()
		}
		b.Node = m.nodeName
		b.Unreachable = false
	}
	for _, s := range m.snaps {
		s.Node = m.nodeName
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

// defaultVCPUs and defaultMemMB are the size a sandbox gets when its creator
// named none. Options.DefaultVCPUs / DefaultMemMB override the package
// constants so a node can be sized for the machine it is actually on; see
// Options for why that is node configuration rather than something admission
// control can be trusted to catch.
func (m *Manager) defaultVCPUs() int64 {
	if m.defVCPUs > 0 {
		return m.defVCPUs
	}
	return defaultVCPUs
}

func (m *Manager) defaultMemMB() int64 {
	if m.defMemMB > 0 {
		return m.defMemMB
	}
	return DefaultMemMB
}

func (m *Manager) Create(ctx context.Context, name, owner, image string, vcpus, memMB int64) (*Sandbox, error) {
	if !validName(name) {
		return nil, &NameError{Problem: NameInvalid, Noun: "sandbox", Name: name}
	}
	if reservedName(name) {
		return nil, &NameError{Problem: NameReserved, Noun: "sandbox", Name: name}
	}
	if vcpus <= 0 {
		vcpus = m.defaultVCPUs()
	}
	if memMB <= 0 {
		memMB = m.defaultMemMB()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// A guest whose authorized_keys is empty is a VM nobody can log into, and
	// nothing later in the lifecycle can repair it — so refuse the boot instead
	// of spending a rootfs on it. Checked here rather than up with the name
	// rules because the key can be replaced at any time (SetGatewayPublicKey)
	// and so must be read under the lock.
	if m.gwPubKey == "" {
		return nil, &DisabledError{Code: "no_gateway_key",
			Msg: "this node has not yet learned the gateway's key; it will once the link is up"}
	}
	if _, ok := m.boxes[name]; ok {
		return nil, &NameError{Problem: NameTaken, Noun: "sandbox", Name: name}
	}
	if m.maxBoxesPerOwner > 0 {
		owned := 0
		for _, b := range m.boxes {
			if b.Owner == owner {
				owned++
			}
		}
		if owned >= m.maxBoxesPerOwner {
			return nil, &StateError{Code: "sandbox_limit", Msg: fmt.Sprintf(
				"sandbox limit reached for %s (%d/%d)", owner, owned, m.maxBoxesPerOwner)}
		}
	}
	if err := m.admit(owner, memMB, 0, ""); err != nil {
		return nil, err
	}
	inst, err := m.driver.Create(ctx, vmm.Config{
		Name: name, Image: image, VCPUs: vcpus, MemMB: memMB,
		GatewayPublicKey: m.gwPubKey,
		// The name-taken check above just proved this sandbox is new to the
		// ledger, so a disk already under the name is not its state — see
		// vmm.Config.NewSandbox. The other Create call site (resumeOrRecreate)
		// cold-boots a sandbox we already know, where that disk is the point.
		NewSandbox: true,
	})
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	b := &Sandbox{
		ID:   uuid.NewString(),
		Name: name, Owner: owner, Image: image, VCPUs: vcpus, MemMB: memMB,
		State: inst.State, SSHAddr: inst.SSHAddr, SSHUser: inst.SSHUser,
		HostIP: inst.HostIP, GuestV6: inst.GuestV6, CreatedAt: now, LastActive: now,
		Node: m.nodeName,
	}
	m.boxes[name] = b
	// Fresh tap and VMM process, so the counters start at zero and the first
	// tick can charge a real delta instead of only priming (see EnsureRunning).
	m.vitals[name] = vitalsSample{at: time.Now()}
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
	// Unconditional push on create also covers forks: a forked rootfs carries
	// the template's managed env block, and this rewrite is what replaces it.
	m.pushEnv(ctx, copyOf(b))
	m.observe(b, "created")
	return copyOf(b), m.save()
}

// pooledDiskMB is what this sandbox costs its owner's pooled-disk budget: the
// blocks it wrote, not the template blocks it shares. A fork's rootfs reports
// the template's contents as used (DiskUsageMB reads the guest's own filesystem
// counters, which know nothing of reflink), so ten forks of one image would each
// be charged the full template while the host holds a single copy. Subtracting
// BaseDiskMB makes the pool measure the thing the operator is actually buying.
//
// Floored at zero: the refresher replaces base templates by atomic rename, so a
// baseline re-measured against a newer template can exceed an older clone's
// usage, and a negative charge would credit an owner for other sandboxes' bytes.
//
// An archived sandbox pays full freight. Archive replaces DiskMB with the
// compressed artifact's size (see Archive), which lives in object storage and
// gets no dedup at all — subtracting a local template's baseline from it would
// discount bytes the owner genuinely occupies. (Sharing across archives is the
// deferred delta-archive design, not this.)
//
// Only the pooled-budget call sites use this. The per-sandbox ceiling, both
// consoles' meters and every wire projection keep the raw DiskMB, which is the
// number a user can reconcile with `df` inside their guest.
func (b *Sandbox) pooledDiskMB() int64 {
	if b.State == vmm.StateArchived {
		return b.DiskMB
	}
	if b.BaseDiskMB >= b.DiskMB {
		return 0
	}
	return b.DiskMB - b.BaseDiskMB
}

// admit enforces the per-owner limits before a start (create or resume/restore).
// Callers must hold m.mu. exclude is the name of the sandbox being started (so
// it isn't counted against its own start), or "" for a create. reqDiskMB is the
// disk that start will occupy on the pooled budget (0 for a create — a fresh
// reflink is ~free until written; the box's own DiskMB for a resume/restore).
func (m *Manager) admit(owner string, memMB, reqDiskMB int64, exclude string) error {
	return m.admitCost(owner, reqDiskMB, exclude, m.effectiveMemMB(memMB), m.ownerMemPoolMB)
}

// admitTurbo is admission for the one operation allowed to borrow above an
// owner's baseline. Doubling a VM must also double its working-set charge;
// otherwise MemReserveMB would make normal and turbo VMs cost exactly the same.
func (m *Manager) admitTurbo(b *Sandbox) error {
	budget := m.ownerMemBurstMB
	if budget <= 0 {
		budget = m.ownerMemPoolMB
	}
	baseMemMB := b.MemMB
	ceiling := b.MemMB * TurboFactor
	if b.Turbo && b.BaseMemMB > 0 {
		baseMemMB = b.BaseMemMB
		ceiling = b.MemMB
	}
	cost := m.effectiveMemMB(baseMemMB) * TurboFactor
	if cost > ceiling {
		cost = ceiling
	}
	return m.admitCost(b.Owner, b.pooledDiskMB(), b.Name, cost, budget)
}

func (m *Manager) admitCost(owner string, reqDiskMB int64, exclude string, cost, ownerBudget int64) error {
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
	// The plan belongs to the owner, not to each VM. Charge every running child
	// against one pooled memory entitlement before considering node capacity.
	// effectiveMemMB deliberately matches node admission: with ballooning on it
	// is the measured working-set floor; without it an 8 GiB VM consumes the
	// whole 8 GiB pool.
	if ownerBudget > 0 {
		var used int64
		for _, b := range m.boxes {
			if b.Name != exclude && b.Owner == owner && b.State == vmm.StateRunning {
				used += m.sandboxEffectiveMemMB(b)
			}
		}
		if used+cost > ownerBudget {
			return &CapacityError{Owner: owner, RequestedMB: cost, UsedMB: used, BudgetMB: ownerBudget}
		}
	}
	if m.memAdmitPct > 0 && m.hostMemMB > 0 {
		var used int64
		for _, b := range m.boxes {
			if b.Name != exclude && b.State == vmm.StateRunning {
				used += m.sandboxEffectiveMemMB(b)
			}
		}
		budget := m.hostMemMB * int64(m.memAdmitPct) / 100
		if used+cost > budget {
			return &CapacityError{RequestedMB: cost, UsedMB: used, BudgetMB: budget}
		}
	}
	// Pooled per-owner disk (soft accounting): the sum of each running/paused
	// root filesystem's used blocks plus archived boxes' object-storage size
	// must stay under the owner's pool. Each live box is charged its pooled
	// figure — what it wrote on top of its template — because template blocks
	// exist once on the host however many sandboxes were forked from them.
	if m.diskPoolMB > 0 {
		var used int64
		for _, b := range m.boxes {
			if b.Name != exclude && b.Owner == owner {
				used += b.pooledDiskMB()
			}
		}
		if used+reqDiskMB > m.diskPoolMB {
			return &DiskQuotaError{Owner: owner, RequestedMB: reqDiskMB, UsedMB: used, PoolMB: m.diskPoolMB}
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

// sandboxEffectiveMemMB is the node and owner charge for an already-running
// sandbox. Turbo doubles the base working-set charge, not merely the guest
// ceiling: under overcommit both ceilings would otherwise collapse to the same
// MemReserveMB and borrowed capacity would be invisible.
func (m *Manager) sandboxEffectiveMemMB(b *Sandbox) int64 {
	return effectiveMemFor(b, m.reserveMB)
}

// effectiveMemFor is sandboxEffectiveMemMB with the node's working-set floor
// passed in rather than read off a Manager, so a gateway folding records for
// sandboxes on other machines charges them the way their own node does.
func effectiveMemFor(b *Sandbox, reserveMB int64) int64 {
	reserved := func(memMB int64) int64 {
		if reserveMB > 0 && reserveMB < memMB {
			return reserveMB
		}
		return memMB
	}
	if b.Turbo && b.BaseMemMB > 0 {
		cost := reserved(b.BaseMemMB) * TurboFactor
		if cost > b.MemMB {
			return b.MemMB
		}
		return cost
	}
	return reserved(b.MemMB)
}

// observedMemMB returns the last balloon-stat working set, falling back to the
// conservative admission charge until the first sample arrives. Callers hold
// m.mu.
func (m *Manager) observedMemMB(b *Sandbox) int64 {
	if used, ok := m.memUsed[b.Name]; ok {
		return used
	}
	return m.sandboxEffectiveMemMB(b)
}

func (m *Manager) Get(name string) (*Sandbox, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok {
		return nil, false
	}
	out := copyOf(b)
	out.LastActive = m.latestActivity(name, out.LastActive)
	return out, true
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
		c := copyOf(b)
		c.LastActive = m.latestActivity(c.Name, c.LastActive)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (m *Manager) ObserveHiveMindSessions(sandboxID string, snapshot HiveMindSessionSnapshot) {
	if sandboxID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, box := range m.boxes {
		if box.ID != sandboxID {
			continue
		}
		snapshot.Sessions = cloneHiveMindSessions(snapshot.Sessions)
		box.HiveMind = &snapshot
		return
	}
}

// ObserveHiveMindPresence records the live reading taken on every poll. It is
// separate from ObserveHiveMindSessions so the minute-by-minute signal is not
// held hostage to the catalog's much slower refresh — see Sandbox.HiveMind.
func (m *Manager) ObserveHiveMindPresence(sandboxID string, presence HiveMindPresence) {
	if sandboxID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, box := range m.boxes {
		if box.ID != sandboxID {
			continue
		}
		box.HiveMindPresence = &presence
		return
	}
}

func (m *Manager) HiveMindSessions(sandboxID string) (HiveMindSessionSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, box := range m.boxes {
		if box.ID != sandboxID || box.HiveMind == nil {
			continue
		}
		snapshot := *box.HiveMind
		snapshot.Sessions = cloneHiveMindSessions(snapshot.Sessions)
		return snapshot, true
	}
	return HiveMindSessionSnapshot{}, false
}

// ListByOwner returns one owner's sandboxes, sorted by name.
func (m *Manager) ListByOwner(owner string) []*Sandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Sandbox
	for _, b := range m.boxes {
		if b.Owner == owner {
			c := copyOf(b)
			c.LastActive = m.latestActivity(c.Name, c.LastActive)
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NodeName is what this host calls itself — the name it stamps on the sandboxes
// it creates and reports in its capacity. Lock-free: it is written once, in
// NewManager.
func (m *Manager) NodeName() string { return m.nodeName }

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
	ResidentMemMB  int64 `json:"resident_mem_mb"`
	// EntitledMemMB is the sum of one baseline owner pool for every owner with a
	// running sandbox. It may exceed BudgetMemMB by design: owner pools overlap;
	// EffectiveMemMB is the physical admission signal.
	EntitledMemMB      int64 `json:"entitled_mem_mb"`
	ActiveOwners       int   `json:"active_owners"`
	OwnerMemoryPoolMB  int64 `json:"owner_memory_pool_mb"`
	OwnerMemoryBurstMB int64 `json:"owner_memory_burst_mb"`
	// ReserveMemMB is the per-VM working-set floor; 0 means overcommit is off.
	ReserveMemMB int64 `json:"reserve_mem_mb"`
	// UsedDiskMB is the summed durable usage of all sandboxes on this node
	// (used root-filesystem blocks + archived boxes' object-storage size), on
	// the same basis pooled admission charges: net of the template blocks forks
	// share, so it is a lower bound on physical bytes rather than a `du`.
	// DiskPoolMBPerOwner is the per-owner pooled budget (0 = unlimited).
	UsedDiskMB         int64 `json:"used_disk_mb"`
	DiskPoolMBPerOwner int64 `json:"disk_pool_mb_per_owner"`
	Running            int   `json:"running"`
	Sandboxes          int   `json:"sandboxes"`
	// Arch and Release say what kind of machine this is: an aggregator showing
	// several nodes at once needs them to explain why a sandbox landed where it
	// did. Empty when the host didn't say.
	Arch    string `json:"arch,omitempty"`
	Release string `json:"release,omitempty"`
	// Online and LastSeenAt are filled in by whoever aggregates capacities from
	// several machines; a manager reporting on itself is online by definition
	// and leaves LastSeenAt nil, since "now" carries no information.
	Online     bool       `json:"online"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
}

// OwnerCapacity is one subscriber's pooled entitlement and current allocation
// on this node. Configured VM memory remains a per-VM ceiling; EffectiveMemMB is
// the amount admission charges to the shared owner pool.
type OwnerCapacity struct {
	Owner          string `json:"owner"`
	MemoryPoolMB   int64  `json:"memory_pool_mb"`
	MemoryBurstMB  int64  `json:"memory_burst_mb"`
	EffectiveMemMB int64  `json:"effective_memory_mb"`
	ResidentMemMB  int64  `json:"resident_memory_mb"`
	BorrowedMemMB  int64  `json:"borrowed_memory_mb"`
	AllocatedMemMB int64  `json:"allocated_memory_mb"`
	DiskPoolMB     int64  `json:"disk_pool_mb"`
	// UsedDiskMB is what this owner's sandboxes charge the pool: their written
	// blocks, net of the template blocks every clone shares. It is therefore a
	// lower bound on the physical bytes they occupy, and it is smaller than the
	// per-sandbox figures the consoles show, which stay raw.
	UsedDiskMB int64 `json:"used_disk_mb"`
	// RawDiskMB is the same sandboxes' disk WITHOUT the reflink subtraction —
	// what they would occupy if every fork had been a copy. RawDiskMB minus
	// UsedDiskMB is therefore the sharing dividend, which is the number worth
	// showing an owner: it is the difference between "you have six 25 GB disks"
	// and "you have written 3 GB".
	RawDiskMB int64 `json:"raw_disk_mb"`
	// CapacityDiskMB is the summed hard ceiling of those sandboxes' filesystems
	// — the space they could grow into, not space anyone is holding. 0 for
	// sandboxes whose driver cannot report a ceiling, so it is a floor.
	CapacityDiskMB    int64 `json:"capacity_disk_mb"`
	AllocatedVCPUs    int64 `json:"allocated_vcpus"`
	RunningSandboxes  int   `json:"running_sandboxes"`
	TotalSandboxes    int   `json:"total_sandboxes"`
	ArchivedSandboxes int   `json:"archived_sandboxes"`
	MaxRunning        int   `json:"max_running"`
	MaxSandboxes      int   `json:"max_sandboxes"`
	TurboSandboxes    int   `json:"turbo_sandboxes"`
	// Nodes is how many machines these sandboxes are spread over — 1 for a
	// node reporting on itself, and what a gateway fills in when it merges. It
	// is the caveat on the pool figures below: those are one node's
	// configuration, not a fleet-wide entitlement (see Merge).
	Nodes int `json:"nodes,omitempty"`
}

// OwnerPolicy is the node configuration an owner's charge is computed against.
// It is separated from the Manager so a gateway can fold the sandboxes it holds
// records for — which live on other machines — with exactly the arithmetic the
// node itself would use, rather than a second copy of it that drifts.
type OwnerPolicy struct {
	MemoryPoolMB  int64
	MemoryBurstMB int64
	DiskPoolMB    int64
	// ReserveMemMB is the per-VM working-set floor under overcommit; 0 means
	// overcommit is off and a VM is charged its full ceiling.
	ReserveMemMB int64
	MaxRunning   int
	MaxSandboxes int
}

// RollUpOwner folds boxes into one owner's capacity picture under policy.
//
// resident supplies a running sandbox's live balloon working set and may be
// nil, which is the honest answer away from the machine holding the VM: the
// conservative admission charge stands in, exactly as it does on a node that
// has not sampled a guest yet. Callers with a live reading of their own — the
// user console has one per running sandbox already — should prefer it over
// this field.
//
// boxes is filtered by owner here rather than by the caller so that every
// caller filters it the same way.
func RollUpOwner(owner string, boxes []*Sandbox, policy OwnerPolicy, resident func(*Sandbox) int64) OwnerCapacity {
	c := OwnerCapacity{
		Owner: owner, MemoryPoolMB: policy.MemoryPoolMB, MemoryBurstMB: policy.MemoryBurstMB,
		DiskPoolMB: policy.DiskPoolMB,
		MaxRunning: policy.MaxRunning, MaxSandboxes: policy.MaxSandboxes,
	}
	for _, b := range boxes {
		if b == nil || b.Owner != owner {
			continue
		}
		c.TotalSandboxes++
		c.UsedDiskMB += b.pooledDiskMB()
		c.RawDiskMB += b.DiskMB
		c.CapacityDiskMB += b.DiskTotalMB
		switch b.State {
		case vmm.StateArchived:
			c.ArchivedSandboxes++
		case vmm.StateRunning:
			c.RunningSandboxes++
			c.AllocatedMemMB += b.MemMB
			c.AllocatedVCPUs += b.VCPUs
			effective := effectiveMemFor(b, policy.ReserveMemMB)
			c.EffectiveMemMB += effective
			if resident != nil {
				c.ResidentMemMB += resident(b)
			} else {
				c.ResidentMemMB += effective
			}
			if b.Turbo {
				c.TurboSandboxes++
			}
		}
	}
	if c.TotalSandboxes > 0 {
		c.Nodes = 1
	}
	c.recomputeBorrowed()
	return c
}

// Merge folds another machine's rollup for the same owner into this one.
//
// Usage adds; the pool figures do NOT. MemoryPoolMB, MemoryBurstMB, DiskPoolMB
// and the two Max counts are one machine's configuration, and an owner who
// straddles two machines has not thereby been granted two pools — every node in
// a fleet is configured with the same numbers, so the larger of two identical
// values is that value, and the larger of a configured one and an unset 0 is
// the configured one. Summing them would quietly double an owner's apparent
// entitlement the first time a sandbox was placed elsewhere.
func (c OwnerCapacity) Merge(other OwnerCapacity) OwnerCapacity {
	if c.Owner == "" {
		c.Owner = other.Owner
	}
	c.EffectiveMemMB += other.EffectiveMemMB
	c.ResidentMemMB += other.ResidentMemMB
	c.AllocatedMemMB += other.AllocatedMemMB
	c.AllocatedVCPUs += other.AllocatedVCPUs
	c.UsedDiskMB += other.UsedDiskMB
	c.RawDiskMB += other.RawDiskMB
	c.CapacityDiskMB += other.CapacityDiskMB
	c.RunningSandboxes += other.RunningSandboxes
	c.TotalSandboxes += other.TotalSandboxes
	c.ArchivedSandboxes += other.ArchivedSandboxes
	c.TurboSandboxes += other.TurboSandboxes
	c.Nodes += other.Nodes
	c.MemoryPoolMB = max(c.MemoryPoolMB, other.MemoryPoolMB)
	c.MemoryBurstMB = max(c.MemoryBurstMB, other.MemoryBurstMB)
	c.DiskPoolMB = max(c.DiskPoolMB, other.DiskPoolMB)
	c.MaxRunning = max(c.MaxRunning, other.MaxRunning)
	c.MaxSandboxes = max(c.MaxSandboxes, other.MaxSandboxes)
	c.recomputeBorrowed()
	return c
}

// recomputeBorrowed restates how far past the baseline pool this owner is
// charged. It is derived, so a Merge that added two halves has to redo it
// rather than add two borrowings computed against the same pool twice.
func (c *OwnerCapacity) recomputeBorrowed() {
	c.BorrowedMemMB = 0
	if c.MemoryPoolMB > 0 && c.EffectiveMemMB > c.MemoryPoolMB {
		c.BorrowedMemMB = c.EffectiveMemMB - c.MemoryPoolMB
	}
}

// CapacityForOwner reports the local portion of an owner's resource envelope.
// Owners are initially pinned to one node, making this the authoritative pool
// view; a future distributed owner scheduler can sum these records.
func (m *Manager) CapacityForOwner(owner string) OwnerCapacity {
	m.mu.Lock()
	defer m.mu.Unlock()
	boxes := make([]*Sandbox, 0, len(m.boxes))
	for _, b := range m.boxes {
		boxes = append(boxes, b)
	}
	// observedMemMB wants m.mu, which is held for the whole fold — the reason
	// the balloon reading is passed in rather than read inside the rollup.
	return RollUpOwner(owner, boxes, m.ownerPolicy(), m.observedMemMB)
}

// ownerPolicy is this node's configured owner envelope. Callers hold m.mu.
func (m *Manager) ownerPolicy() OwnerPolicy {
	return OwnerPolicy{
		MemoryPoolMB: m.ownerMemPoolMB, MemoryBurstMB: m.ownerMemBurstMB,
		DiskPoolMB: m.diskPoolMB, ReserveMemMB: m.reserveMB,
		MaxRunning: m.maxPerOwner, MaxSandboxes: m.maxBoxesPerOwner,
	}
}

// Capacity reports this node's resources. Used* counts only running sandboxes,
// mirroring the admission check: paused sandboxes cost disk, not RAM/CPU.
func (m *Manager) Capacity() NodeCapacity {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := NodeCapacity{
		Node:               m.nodeName,
		Arch:               m.nodeArch,
		Release:            m.nodeRelease,
		Online:             true,
		TotalVCPUs:         m.hostVCPUs,
		TotalMemMB:         m.hostMemMB,
		ReserveMemMB:       m.reserveMB,
		OwnerMemoryPoolMB:  m.ownerMemPoolMB,
		OwnerMemoryBurstMB: m.ownerMemBurstMB,
		DiskPoolMBPerOwner: m.diskPoolMB,
		Sandboxes:          len(m.boxes),
	}
	if m.memAdmitPct > 0 {
		c.BudgetMemMB = m.hostMemMB * int64(m.memAdmitPct) / 100
	}
	activeOwners := make(map[string]struct{})
	for _, b := range m.boxes {
		c.UsedDiskMB += b.pooledDiskMB()
		if b.State == vmm.StateRunning {
			activeOwners[b.Owner] = struct{}{}
			c.Running++
			c.UsedVCPUs += b.VCPUs
			c.UsedMemMB += b.MemMB
			c.EffectiveMemMB += m.sandboxEffectiveMemMB(b)
			c.ResidentMemMB += m.observedMemMB(b)
		}
	}
	c.ActiveOwners = len(activeOwners)
	if m.ownerMemPoolMB > 0 {
		c.EntitledMemMB = int64(c.ActiveOwners) * m.ownerMemPoolMB
	}
	return c
}

// Accessor is the lifecycle slice shared by every warm access path. Both a
// Manager and a fleet router satisfy it.
type Accessor interface {
	Get(name string) (*Sandbox, bool)
	EnsureReady(ctx context.Context, name string) (*Sandbox, error)
	MarkActive(name string)
}

// Prepare is the common resume-on-access decision. A running cached record is
// returned immediately and merely marked active; only a stopped or unknown
// record crosses the potentially expensive EnsureReady boundary.
//
// Unknown is deliberately handed to EnsureReady rather than answered here.
// The authoritative store owns the exact not-found/offline/orphaned error, and
// callers rely on that taxonomy for both masking and user-facing status codes.
func Prepare(ctx context.Context, boxes Accessor, name string) (*Sandbox, error) {
	b, ok := boxes.Get(name)
	if ok && b.State == vmm.StateRunning {
		boxes.MarkActive(name)
		return b, nil
	}
	return boxes.EnsureReady(ctx, name)
}

// EnsureReady resumes the sandbox if paused and returns its SSH endpoint.
// Concurrent callers for one sandbox share the complete restore/resume. Warm
// request paths call Prepare and do not reach this method.
func (m *Manager) EnsureReady(ctx context.Context, name string) (*Sandbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := m.ready.DoChan(name, func() (any, error) {
		unlock := m.lockDiskOperation(name)
		defer unlock()
		operationCtx, cancel := sharedOperationContext(ctx, m.ctx)
		defer cancel()
		return m.ensureReady(operationCtx, name)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resolved := <-result:
		if resolved.Err != nil {
			return nil, resolved.Err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return copyOf(resolved.Val.(*Sandbox)), nil
	}
}

func sharedOperationContext(values, lifecycle context.Context) (context.Context, context.CancelFunc) {
	operation, cancel := context.WithCancel(context.WithoutCancel(values))
	if lifecycle.Err() != nil {
		cancel()
		return operation, cancel
	}
	stop := context.AfterFunc(lifecycle, cancel)
	return operation, func() {
		stop()
		cancel()
	}
}

func (m *Manager) ensureReady(ctx context.Context, name string) (*Sandbox, error) {
	// An archived sandbox must first be pulled back onto local disk. That's a
	// multi-GB download, so restore runs without m.mu held and flips the record
	// to Paused; the resume path below then cold-boots the restored rootfs.
	m.mu.Lock()
	b, ok := m.boxes[name]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox %q not found", name)
	}
	archived, archKey := b.State == vmm.StateArchived, b.ArchiveKey
	classification := "resume"
	switch b.State {
	case vmm.StateRunning:
		classification = "warm"
	case vmm.StateArchived:
		classification = "restore"
	}
	m.metrics.IncEnsureReady(m.nodeName, classification)
	m.mu.Unlock()
	if archived {
		if err := m.restore(ctx, name, archKey); err != nil {
			return nil, err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok = m.boxes[name]
	if !ok {
		return nil, fmt.Errorf("sandbox %q not found", name)
	}
	resumed := false
	if b.State != vmm.StateRunning {
		// Resuming brings this sandbox back to running, so it's subject to the
		// same limits as a fresh create (exclude itself — it isn't running yet).
		// Its own footprint (rootfs, or the just-restored size) is the disk it
		// reclaims against the pool — net of the template blocks it shares, on
		// the same basis the pooled sum above is computed.
		var admitErr error
		if b.Turbo {
			admitErr = m.admitTurbo(b)
		} else {
			admitErr = m.admit(b.Owner, b.MemMB, b.pooledDiskMB(), b.Name)
		}
		if admitErr != nil {
			return nil, admitErr
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
		// Seed a zero baseline: this start built a fresh tap and a fresh VMM
		// process, so both counter sets demonstrably begin at zero. Without it
		// the next tick would only *prime*, and every byte moved between resume
		// and that tick would go unmetered — which is most of the traffic on a
		// box that resumes, does one burst of work, and goes quiet.
		m.vitals[name] = vitalsSample{at: time.Now()}
		// The rootfs survived pause/archive/host-restart, but the env may have
		// changed while the box was down — reconcile on every return to running.
		m.pushEnv(ctx, copyOf(b))
		m.observe(b, "resumed")
		resumed = true
	}
	// Activity returns a ballooned-down sandbox to full RAM (whether it was
	// just resumed or was warm-but-ballooned).
	m.deflate(ctx, b)
	b.LastActive = time.Now().UTC()
	if resumed {
		return copyOf(b), m.save()
	}
	// Explicit resume of an already-running sandbox is activity, but not a
	// lifecycle transition. Keep it off the synchronous persistence path.
	out := copyOf(b)
	m.activityMu.Lock()
	m.activity[name] = b.LastActive
	m.markedAt[name] = b.LastActive
	m.activityMu.Unlock()
	return out, nil
}

// SetEnvSync installs the env-push hook after construction — the syncer needs
// the gateway's upstream SSH key, which only exists once the manager does.
// Once set, every sandbox that reaches running (create, resume, restore,
// recreate, fork) gets a best-effort push, so the guest's env file self-heals
// on the next start no matter how a change-time push was missed.
func (m *Manager) SetEnvSync(p EnvPusher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.envSync = p
}

// SetRepoSync installs the repo-checkout nudge, post-construction for the same
// reason SetEnvSync is: it needs the gateway's upstream SSH key.
func (m *Manager) SetRepoSync(r RepoSyncer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repoSync = r
}

// SetToolSync installs the pre-capture agent-tool refresh, post-construction
// for the same reason SetRepoSync is: it needs the gateway's upstream SSH key.
//
// Only a GATEWAY wires this. A node caches the gateway's upstream PUBLIC key
// and holds no signer, so it cannot open a session into its own guests; the
// refresh for a sandbox on another machine is done from the gateway side
// instead (fleet.Fleet.refreshToolsBefore). This hook covers the sandboxes on
// THIS machine, which on a single-box deployment is all of them.
func (m *Manager) SetToolSync(t ToolRefresher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.toolSync = t
}

// ResyncRepos asks name's guest to reconcile its checkouts, for a caller that
// changed which repositories the sandbox is entitled to — its tags, or an
// attachment on a tag it already carries.
//
// Synchronous, unlike ResyncEnv, and that difference is the point. The env push
// is fire-and-forget because no lifecycle operation may fail on an SSH exec;
// this one has no lifecycle operation behind it at all — it is a person typing
// `ctl tags set` and reading the reply — so the one thing it can usefully do is
// tell them whether the machine took the job. The guest returns the moment it
// has, and the clone itself runs on the guest's own clock. See
// envsync.SyncRepos.
//
// A sandbox that is not running is not an error: it checks out at its next
// boot, from whatever is attached then.
func (m *Manager) ResyncRepos(ctx context.Context, name string) error {
	m.mu.Lock()
	r := m.repoSync
	b, ok := m.boxes[name]
	if r == nil || !ok || b.State != vmm.StateRunning {
		m.mu.Unlock()
		return nil
	}
	cp := copyOf(b)
	m.mu.Unlock()
	return r.SyncRepos(ctx, cp)
}

// SetSessions installs the hook that hangs up sessions attached to a sandbox
// being paused. Installed post-construction because the gateway is built with
// the manager, so it cannot be passed in at construction time.
func (m *Manager) SetSessions(c SessionCloser) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = c
}

// SetGatewayPublicKey installs the authorized_keys line new guests trust. A
// host that knows it at startup passes Options.GatewayPublicKey; a host that
// learns it from somewhere else — a machine whose gateway is another process,
// which cannot be asked before it is reachable — sets it here, and Create
// refuses until it is set rather than booting a VM nobody can log into.
func (m *Manager) SetGatewayPublicKey(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.gwPubKey = line
}

// observe hands the Observer, if one is installed, a copy of a record that just
// changed. Callers hold m.mu — both the hook and the record are read under it,
// so an observer never sees a half-written record — which is also why the
// contract says implementations must not block.
func (m *Manager) observe(b *Sandbox, reason string) {
	if m.observer == nil {
		return
	}
	m.observer.SandboxChanged(copyOf(b), reason)
}

// ReachedRunning reports whether an observed change is a transition INTO
// running, given the reason the manager stamped on it.
//
// It exists for the fleet, and it lives here because the vocabulary is this
// file's: the reasons are the literals passed to observe/observeName a few
// lines up, and a predicate stated anywhere else would silently stop matching
// the day one of them is added or renamed.
//
// Why a caller needs it at all: on a single box the manager fires its own
// env-push hook the moment a sandbox reaches running. On a node that hook is
// nil by construction — a node has no secrets store and must not — so the
// gateway has to fire the push for a sandbox on another machine, and its only
// notice that one has come up on the machine's own initiative (a node reboot
// resuming its pinned sandboxes, a restore) is the change event. Every running
// sandbox also emits "touched" on every reaper tick, so "state is running" is
// not a usable trigger; the reason is what separates a transition from a
// heartbeat.
//
// ADD A REASON HERE when you add one that ends with a VM running, or a sandbox
// on another machine will come up without its secrets.
func ReachedRunning(reason string) bool {
	switch reason {
	case "created", "resumed", "restored", "rebooted", "resized":
		return true
	}
	return false
}

// observeName is observe for the paths that finish with the lock released:
// Resize and Reboot both end in EnsureRunning, which takes it.
func (m *Manager) observeName(name, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.boxes[name]; ok {
		m.observe(b, reason)
	}
}

// pushEnv fires the env-sync hook for a sandbox that just reached running.
// Callers hold m.mu; the push dials the guest's sshd, so it runs on its own
// goroutine (bounded, detached from the caller's ctx) and can never fail or
// slow the lifecycle operation it rides on.
// ResyncEnv re-pushes a sandbox's secret environment, for callers that changed
// something the push depends on — chiefly its tags, which decide which of the
// owner's secrets it receives. No-op for a sandbox that isn't running: the
// EnsureRunning hook pushes on its next start anyway.
func (m *Manager) ResyncEnv(ctx context.Context, name string) {
	m.mu.Lock()
	b, ok := m.boxes[name]
	if !ok || b.State != vmm.StateRunning {
		m.mu.Unlock()
		return
	}
	cp := copyOf(b)
	m.mu.Unlock()
	m.pushEnv(ctx, cp)
}

// AwaitEnv delivers a sandbox's secret environment and returns only once it has
// actually been written — the synchronous counterpart of pushEnv, for the one
// caller that cannot use a fire-and-forget push.
//
// pushEnv is asynchronous on purpose: no lifecycle operation should be able to
// stall or fail on an SSH exec into a guest. But an interactive attach both
// starts a VM and opens a session on it in the same breath, and pam_env reads
// /etc/environment exactly once, at session setup. The push and the session are
// then two goroutines racing for the same second, and the session wins — it was
// begun by the call that already returned, while the push still has to dial. The
// user's first shell on the box they just saved a token for comes up without it,
// and, as ever with this feature, nothing anywhere says why.
//
// Overlapping an in-flight push is safe and is the normal case: the syncer
// serializes deliveries per box and each one rewrites the same block from the
// same store, so this either waits behind the async push and rewrites an
// identical block, or goes first and makes that one the cheap duplicate. The
// same idempotence the fleet already leans on for its own overlapping pushes.
func (m *Manager) AwaitEnv(ctx context.Context, name string) error {
	m.mu.Lock()
	p := m.envSync
	b, ok := m.boxes[name]
	if p == nil || !ok || b.State != vmm.StateRunning {
		m.mu.Unlock()
		return nil
	}
	cp := copyOf(b)
	m.mu.Unlock()
	return p.PushEnv(ctx, cp)
}

func (m *Manager) pushEnv(ctx context.Context, b *Sandbox) {
	if m.envSync == nil {
		return
	}
	p := m.envSync
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if err := p.PushEnv(ctx, b); err != nil {
			m.log.Warn("env push failed", "name", b.Name, "err", err)
		}
	}()
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
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	presence, canCheckRootfs := m.driver.(vmm.RootfsPresencer)
	if canCheckRootfs {
		present, presenceErr := presence.RootfsPresent(b.Name)
		if presenceErr != nil {
			return nil, fmt.Errorf("check local rootfs for %s: %w", b.Name, presenceErr)
		}
		if !present {
			if b.CheckpointKey != "" {
				return nil, &StateError{Code: "checkpoint_restore_required",
					Msg: fmt.Sprintf("sandbox %q lost its local disk; restore its checkpoint", b.Name)}
			}
			return nil, &StateError{Code: "sandbox_unrecoverable",
				Msg: fmt.Sprintf("sandbox %q lost its local disk and has no checkpoint to restore", b.Name)}
		}
	} else if b.CheckpointKey != "" {
		return nil, &StateError{Code: "checkpoint_restore_required",
			Msg: fmt.Sprintf("sandbox %q has a checkpoint but the driver cannot verify its local disk; restore the checkpoint", b.Name)}
	}
	m.log.Warn("resume failed, recreating", "name", b.Name, "err", err)
	return m.driver.Create(ctx, vmm.Config{
		Name: b.Name, Image: b.Image, VCPUs: b.VCPUs, MemMB: b.MemMB,
		GatewayPublicKey: m.gwPubKey,
	})
}

func (m *Manager) Pause(ctx context.Context, name string) error {
	unlock := m.lockDiskOperation(name)
	defer unlock()
	return m.pause(ctx, name, "was paused")
}

// pause is Pause with the explanation shown to anyone attached to the sandbox.
// The reaper passes its own wording ("went idle for 30m") so a user whose
// terminal just got closed learns why without having to go looking.
func (m *Manager) pause(ctx context.Context, name, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok {
		return fmt.Errorf("sandbox %q not found", name)
	}
	if b.State == vmm.StatePaused {
		return nil
	}
	// Release attached terminals before the VM stops answering. Done first so
	// the goodbye reaches the client while the session is still healthy; the
	// hook returns immediately, so it cannot deadlock against the lock we hold.
	if m.sessions != nil {
		m.sessions.CloseSandboxSessions(name, reason)
	}
	if err := m.driver.Pause(ctx, name); err != nil {
		return err
	}
	b.State = vmm.StatePaused
	b.SSHAddr = ""
	b.HostIP = ""
	b.GuestV6 = ""
	// Drop the baseline: the tap goes away with the VM, so the next reading
	// after a resume starts from zero over an interval spanning the whole pause.
	// Re-priming costs one tick and keeps that from reading as a rate.
	delete(m.vitals, name)
	delete(m.memUsed, name)
	delete(m.balloonedAt, name)
	// Turbo is borrowed for exactly one run, and this is where it is handed
	// back — every path that stops a VM arrives here, so there is one place
	// that has to remember rather than four that have to agree.
	m.endTurbo(b)
	m.log.Info("sandbox paused", "name", name)
	m.observe(b, "paused")
	return m.save()
}

// endTurbo returns a paused sandbox to its own CPU and RAM. Callers hold m.mu
// and have already stopped the VM.
//
// The memory snapshot the driver just wrote goes with the allocation. A
// firecracker guest's shape is baked into its snapshot — the same reason Resize
// must never resume onto a grown disk — so a snapshot of a doubled machine
// resumed under a record that says otherwise gives a VM twice the size the
// control plane is accounting for, invisibly. Dropping it forces the next
// resume through a cold boot at the size the record now claims.
func (m *Manager) endTurbo(b *Sandbox) {
	if !b.Turbo {
		return
	}
	// Nothing should be able to reach this: SetTurbo refuses on a host whose
	// driver cannot drop snapshots, so no sandbox there is ever turbo. If it
	// somehow is, the flag stays on — a record that overstates what the guest
	// has is recoverable, one that understates it is a silent overcommit.
	if m.rebooter == nil || b.BaseVCPUs <= 0 || b.BaseMemMB <= 0 {
		m.log.Warn("turbo cannot be released", "name", b.Name)
		return
	}
	if err := m.rebooter.DropSnapshots(b.Name); err != nil {
		m.log.Warn("turbo release: drop snapshots", "name", b.Name, "err", err)
		return
	}
	m.log.Info("turbo released", "name", b.Name, "vcpus", b.BaseVCPUs, "mem_mb", b.BaseMemMB)
	b.VCPUs, b.MemMB = b.BaseVCPUs, b.BaseMemMB
	b.BaseVCPUs, b.BaseMemMB = 0, 0
	b.Turbo = false
}

// SetTurbo restarts a sandbox with TurboFactor times its CPU and RAM (on), or
// back at its own size (off), and leaves it running either way.
//
// It is a cold boot, not a resize, and it cannot be anything else: firecracker
// has no CPU hotplug, and the balloon can only hand memory back to the host —
// never borrow more than the machine was configured with. So the guest's
// processes stop. Every caller says so before asking.
//
// The extra allocation lasts one run. Manager.pause hands it back, which means
// an idle reap, an explicit pause, a reboot and a rename all end it — turbo is
// a thing you do to the session you are in, not a size you set.
func (m *Manager) SetTurbo(ctx context.Context, name string, on bool) error {
	unlock := m.lockDiskOperation(name)
	defer unlock()
	if m.rebooter == nil {
		return errors.New("turbo is not enabled on this host (driver cannot drop snapshots)")
	}
	m.mu.Lock()
	b, ok := m.boxes[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q not found", name)
	}
	if b.State == vmm.StateArchived {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q is archived; restore it first", name)
	}
	if b.Turbo == on {
		// Already the size being asked for. Restarting the guest to tell the
		// caller nothing changed would be the most expensive possible no-op.
		m.mu.Unlock()
		return nil
	}
	if on {
		// Check what the boot will cost before anything is torn down. EnsureReady
		// checks it again for real, but failing here leaves the sandbox running at
		// its own size rather than paused with an apology.
		if err := m.admitTurbo(b); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	m.mu.Unlock()

	reason := "is restarting at its normal size"
	if on {
		reason = "is restarting in turbo mode"
	}
	// The same pause/drop dance as Reboot, including its re-check: a client
	// whose session we just hung up reconnects immediately, and reconnecting
	// resumes the box out from under us.
	for attempt := 0; ; attempt++ {
		if err := m.pause(ctx, name, reason); err != nil {
			return fmt.Errorf("turbo %s: pause: %w", name, err)
		}
		m.mu.Lock()
		b, ok = m.boxes[name]
		if !ok {
			m.mu.Unlock()
			return fmt.Errorf("sandbox %q not found", name)
		}
		if b.State != vmm.StatePaused {
			m.mu.Unlock()
			if attempt > 0 {
				return fmt.Errorf("turbo %s: sandbox keeps being resumed mid-change; try again", name)
			}
			continue
		}
		// Turning turbo off is already done: the pause above handed the
		// allocation back and dropped the snapshot with it, so all that is left
		// is the cold boot below. Turning it on needs the same snapshot drop for
		// the same reason, and then the new size written down.
		var err error
		if on {
			if err = m.rebooter.DropSnapshots(name); err == nil {
				b.BaseVCPUs, b.BaseMemMB = b.VCPUs, b.MemMB
				b.VCPUs, b.MemMB = b.VCPUs*TurboFactor, b.MemMB*TurboFactor
				b.Turbo = true
				err = m.save()
			}
		} else if b.Turbo {
			// endTurbo could not let go — its snapshot drop failed. Say so
			// instead of cold-booting a guest that is about to come back at the
			// size the caller just asked to leave.
			err = fmt.Errorf("could not release the turbo allocation; try again")
		}
		m.mu.Unlock()
		if err != nil {
			return fmt.Errorf("turbo %s: %w", name, err)
		}
		break
	}

	if _, err := m.ensureReady(ctx, name); err != nil {
		// The size is committed but the guest did not come up. Put the record
		// back, so that the next resume — quite possibly an automatic one, from
		// a web request or a reconnecting terminal — asks for an allocation this
		// host has already served.
		m.revertTurbo(name)
		return fmt.Errorf("turbo %s: %w", name, err)
	}
	m.log.Info("turbo set", "name", name, "on", on)
	m.observeName(name, "turbo")
	return nil
}

// revertTurbo undoes an uncommitted turbo allocation after a failed boot. The
// sandbox is paused and its snapshot already gone, so only the record moves.
func (m *Manager) revertTurbo(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok || !b.Turbo || b.BaseVCPUs <= 0 || b.BaseMemMB <= 0 {
		return
	}
	b.VCPUs, b.MemMB = b.BaseVCPUs, b.BaseMemMB
	b.BaseVCPUs, b.BaseMemMB = 0, 0
	b.Turbo = false
	if err := m.save(); err != nil {
		m.log.Warn("turbo revert: save", "name", name, "err", err)
	}
}

// Reboot cold-restarts a sandbox's guest: pause, discard the memory snapshot,
// then EnsureRunning falls through resumeOrRecreate to a cold boot of the
// preserved rootfs. This is the only way already-running guest processes pick
// up a changed /etc/environment (new SSH sessions see it immediately).
// Resize grows a sandbox's root disk to sizeMB and brings it back up.
//
// The snapshot drop is not an implementation detail, it is the whole reason
// this is a manager operation rather than a driver call. A guest's block-device
// geometry is baked into its memory snapshot, so resuming one onto a grown
// filesystem gives a guest whose ext4 superblock claims more blocks than its
// virtio-blk device reports — writes past the old boundary land nowhere. Resize
// therefore always pauses, DISCARDS the snapshot, resizes, and cold boots.
// Never resize-then-resume.
//
// Grow only (see vmm.DiskResizer). The new ceiling costs no disk up front: the
// image is sparse, so it fills in as the guest writes.
func (m *Manager) Resize(ctx context.Context, name string, sizeMB int64) error {
	unlock := m.lockDiskOperation(name)
	defer unlock()
	if m.diskResize == nil {
		return errors.New("resize is not enabled on this host (driver cannot resize disks)")
	}
	if m.rebooter == nil {
		// Without snapshot-dropping we cannot guarantee the cold boot that makes
		// a resize safe, so refuse rather than risk a stale-geometry resume.
		return errors.New("resize is not enabled on this host (driver cannot drop snapshots)")
	}
	m.mu.Lock()
	b, ok := m.boxes[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q not found", name)
	}
	if b.State == vmm.StateArchived {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q is archived; restore it first", name)
	}
	m.mu.Unlock()

	// Same pause/drop dance as Reboot: a client whose session we just killed
	// reconnects immediately, so re-check under the lock and re-pause once if
	// the box got resumed out from under us.
	for attempt := 0; ; attempt++ {
		if err := m.pause(ctx, name, "was paused for a disk resize"); err != nil {
			return fmt.Errorf("resize %s: pause: %w", name, err)
		}
		m.mu.Lock()
		b, ok = m.boxes[name]
		if !ok {
			m.mu.Unlock()
			return fmt.Errorf("sandbox %q not found", name)
		}
		if b.State == vmm.StatePaused {
			err := m.rebooter.DropSnapshots(name)
			m.mu.Unlock()
			if err != nil {
				return fmt.Errorf("resize %s: drop snapshots: %w", name, err)
			}
			break
		}
		m.mu.Unlock()
		if attempt > 0 {
			return fmt.Errorf("resize %s: sandbox keeps being resumed mid-resize; try again", name)
		}
	}

	// Deliberately outside the lock: fsck + resize2fs take seconds, and holding
	// m.mu would stall every proxied request on the host. The driver refuses a
	// running VM, so the worst a racing resume can do is fail this cleanly —
	// leaving a sandbox that is simply still its old size.
	if err := m.diskResize.ResizeDisk(ctx, name, sizeMB); err != nil {
		return fmt.Errorf("resize %s: %w", name, err)
	}
	m.refreshDiskUsage(ctx, name)

	if _, err := m.ensureReady(ctx, name); err != nil {
		return fmt.Errorf("resize %s: %w", name, err)
	}
	m.log.Info("sandbox disk resized", "name", name, "size_mb", sizeMB)
	m.observeName(name, "resized")
	return nil
}

func (m *Manager) Reboot(ctx context.Context, name string) error {
	unlock := m.lockDiskOperation(name)
	defer unlock()
	if m.rebooter == nil {
		return errors.New("reboot is not enabled on this host (driver cannot drop snapshots)")
	}
	m.mu.Lock()
	b, ok := m.boxes[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q not found", name)
	}
	if b.State == vmm.StateArchived {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q is archived; resume it instead", name)
	}
	m.mu.Unlock()

	// Pause first so the guest has flushed its rootfs; idempotent if paused.
	// A resume-on-connect can slip in between the pause and the snapshot drop
	// (clients auto-reconnect the moment the pause kills their session), so
	// re-check under the lock — held across the drop, like Rename — and
	// re-pause once if the box was resumed out from under us.
	for attempt := 0; ; attempt++ {
		if err := m.pause(ctx, name, "was paused for a cold reboot"); err != nil {
			return fmt.Errorf("reboot %s: pause: %w", name, err)
		}
		m.mu.Lock()
		b, ok = m.boxes[name]
		if !ok {
			m.mu.Unlock()
			return fmt.Errorf("sandbox %q not found", name)
		}
		if b.State == vmm.StatePaused {
			err := m.rebooter.DropSnapshots(name)
			m.mu.Unlock()
			if err != nil {
				return fmt.Errorf("reboot %s: drop snapshots: %w", name, err)
			}
			break
		}
		m.mu.Unlock()
		if attempt > 0 {
			return fmt.Errorf("reboot %s: sandbox keeps being resumed mid-reboot; try again", name)
		}
	}
	if _, err := m.ensureReady(ctx, name); err != nil {
		return fmt.Errorf("reboot %s: %w", name, err)
	}
	m.log.Info("sandbox rebooted", "name", name)
	m.observeName(name, "rebooted")
	return nil
}

// Rename gives a sandbox a new name — and with it a new default subdomain and
// SSH address. A running sandbox is auto-paused first (its VM dir moves on
// disk), and its memory snapshot is dropped before the move because a
// firecracker state.snap embeds absolute paths into the old dir — so the next
// start cold-boots the moved rootfs. Archived sandboxes are refused: their
// archive object is keyed by name (archiveKey), so the flow is restore, then
// rename. The record is journaled under the new name (RenamedFrom) before the
// irreversible dir move, so a crash on either side of the move converges at
// the next load. The routes store moves first and fatally — its rows carry
// ownership for private-route auth, so an orphaned sandbox=old row is a
// security hole, not a cosmetic one — and is rolled back (idempotently) if
// the record commit or dir move then fails; a crash between the hook and the
// commit is repaired by renaming again, which renameChecks and the store's
// idempotent RenameSandbox both tolerate. The remaining side stores
// (schedules, tags, front door) follow best-effort and idempotently.
func (m *Manager) Rename(ctx context.Context, oldName, newName, owner string) error {
	unlock := m.lockDiskOperation(oldName)
	defer unlock()
	if m.renamer == nil {
		return &DisabledError{Code: "rename_disabled", Msg: "rename is not enabled on this host"}
	}
	if !validName(newName) {
		return &NameError{Problem: NameInvalid, Noun: "sandbox", Name: newName}
	}
	if reservedName(newName) {
		return &NameError{Problem: NameReserved, Noun: "sandbox", Name: newName}
	}
	m.mu.Lock()
	if err := m.renameChecks(oldName, newName, owner); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	// Pause first so the rootfs is flushed and the VM dir is movable.
	// Idempotent if already paused.
	if err := m.pause(ctx, oldName, "was paused for rename"); err != nil {
		return fmt.Errorf("rename %s: pause: %w", oldName, err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-validate: the world may have moved between the pause and the lock.
	if err := m.renameChecks(oldName, newName, owner); err != nil {
		return err
	}
	b := m.boxes[oldName]
	if b.State != vmm.StatePaused {
		return &StateError{Code: "sandbox_resumed",
			Msg: fmt.Sprintf("sandbox %q was resumed mid-rename; try again", oldName)}
	}
	// Snapshots must go before the dir moves (see the doc comment); dropping
	// them alone is safe — the next start simply cold-boots.
	if m.rebooter != nil {
		if err := m.rebooter.DropSnapshots(oldName); err != nil {
			return fmt.Errorf("rename %s: drop snapshots: %w", oldName, err)
		}
	}
	// Move the routes first, and fatally: once the record commits under the
	// new name, a failed hook could never be re-fired (Rename(old,new) would
	// no longer find old), permanently orphaning rows whose Owner column
	// gates private-route auth. Before the commit the failure is clean — the
	// record and VM dir are untouched — and the store's RenameSandbox is
	// idempotent, so the crash-repair story ("rename again") holds on either
	// side of it.
	var routesRenamer sandboxRenamer
	if m.routes != nil {
		routesRenamer, _ = any(m.routes).(sandboxRenamer)
	}
	if routesRenamer != nil {
		if err := routesRenamer.RenameSandbox(oldName, newName); err != nil {
			return fmt.Errorf("rename %s: routes: %w", oldName, err)
		}
	}
	undoRoutes := func() {
		if routesRenamer == nil {
			return
		}
		if err := routesRenamer.RenameSandbox(newName, oldName); err != nil {
			// Recoverable: renaming old→new again completes the half-moved
			// state (renameChecks tolerates the box's own rows at newName).
			m.log.Warn("route rename rollback failed; rename again to repair",
				"old", oldName, "new", newName, "err", err)
		}
	}
	// Journal the rename before the irreversible dir move: flip the record to
	// the new name with RenamedFrom set and make it durable, so a crash on
	// either side of the move is reconciled at the next load (see NewManager)
	// instead of leaving a record whose VM dir lives under the other name.
	delete(m.boxes, oldName)
	b.Name = newName
	b.RenamedFrom = oldName
	m.boxes[newName] = b
	undo := func() {
		delete(m.boxes, newName)
		b.Name = oldName
		b.RenamedFrom = ""
		m.boxes[oldName] = b
	}
	if err := m.save(); err != nil {
		undo()
		undoRoutes()
		return err
	}
	if err := m.renamer.RenameVM(oldName, newName); err != nil {
		// The dir move is a single rename(2), so on error nothing moved and
		// the record can safely return to the old name.
		undo()
		m.save() //nolint:errcheck
		undoRoutes()
		return fmt.Errorf("rename %s: %w", oldName, err)
	}
	b.RenamedFrom = ""
	if err := m.save(); err != nil {
		return err
	}
	m.activityMu.Lock()
	if at, ok := m.activity[oldName]; ok {
		m.activity[newName] = at
		delete(m.activity, oldName)
	}
	if at, ok := m.markedAt[oldName]; ok {
		m.markedAt[newName] = at
		delete(m.markedAt, oldName)
	}
	m.activityMu.Unlock()
	// The remaining side plumbing is best-effort per convention: the sandbox
	// record is the source of truth and each hook is idempotent under re-run.
	if sr, ok := m.schedules.(sandboxRenamer); ok {
		if err := sr.RenameSandbox(oldName, newName); err != nil {
			m.log.Warn("schedule rename failed", "old", oldName, "new", newName, "err", err)
		}
	}
	if m.tags != nil {
		if err := m.tags.RenameSandbox(oldName, newName); err != nil {
			m.log.Warn("tag rename failed", "old", oldName, "new", newName, "err", err)
		}
	}
	if m.repoRefs != nil {
		if err := m.repoRefs.RenameSandbox(oldName, newName); err != nil {
			m.log.Warn("repo ref-override rename failed", "old", oldName, "new", newName, "err", err)
		}
	}
	if m.frontDoor != nil {
		m.frontDoor.Remove(ctx, oldName)
		m.frontDoor.Ensure(ctx, newName)
	}
	m.log.Info("sandbox renamed", "old", oldName, "new", newName, "owner", b.Owner)
	m.observe(b, "renamed")
	return nil
}

// renameChecks validates a rename against current state. Callers hold m.mu.
// Run twice — once before the auto-pause and again before committing — since
// the pause happens with the lock released.
func (m *Manager) renameChecks(oldName, newName, owner string) error {
	b, ok := m.boxes[oldName]
	if !ok || b.Owner != owner {
		return &MissingError{Noun: "sandbox", Name: oldName}
	}
	if b.State == vmm.StateArchived {
		return &StateError{Code: "sandbox_archived",
			Msg: fmt.Sprintf("sandbox %q is archived; restore it first, then rename", oldName)}
	}
	if _, exists := m.boxes[newName]; exists {
		return &NameError{Problem: NameTaken, Noun: "sandbox", Name: newName}
	}
	if m.routes != nil {
		r, found, err := m.routes.GetBySubdomain(newName)
		if err != nil {
			return fmt.Errorf("rename %s: route check: %w", oldName, err)
		}
		// The subdomain is free if its route already belongs to this box: as
		// a custom route (sandbox == oldName), or half-moved by a rename that
		// crashed between the routes hook and the record commit (sandbox ==
		// newName, same owner — the owner guard keeps a stale row from a
		// destroyed box, which carries someone else's auth, blocking here
		// rather than being silently adopted). Renaming again completes the
		// crashed move: the routes store's RenameSandbox is idempotent.
		if found && r.Sandbox != oldName && !(r.Sandbox == newName && r.Owner == owner) {
			return &StateError{Code: "subdomain_taken",
				Msg: fmt.Sprintf("subdomain %q is already taken", newName)}
		}
	}
	return nil
}

// archiveKey is where an owner's sandbox archive lives in the object store.
func (m *Manager) archiveKey(owner, name string) string {
	return path.Join(m.archivePfx, owner, name+".ext4.zst")
}

// ArchivingEnabled reports whether this host can archive (an object store and a
// capable driver are both configured). Surfaces use it to hide the action.
func (m *Manager) ArchivingEnabled() bool {
	return m.archive != nil && m.archiver != nil
}

// Archive parks a sandbox's rootfs in object storage and frees its host disk:
// the deepest idle tier below Paused. It pauses the VM (flush + unmount), packs
// the rootfs (fsck + zero free space + zstd), uploads it, then destroys the
// local VM — leaving only a small control-plane record in the Archived state.
// Resume-on-connect brings it back transparently (see EnsureRunning/restore).
//
// The heavy pack/upload runs without m.mu held so it doesn't stall the whole
// host; the record is only flipped to Archived once the upload is durable.
func (m *Manager) Archive(ctx context.Context, name string) error {
	unlock := m.lockDiskOperation(name)
	defer unlock()
	if !m.ArchivingEnabled() {
		return errors.New("archiving is not enabled on this host")
	}
	m.mu.Lock()
	b, ok := m.boxes[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q not found", name)
	}
	if b.State == vmm.StateArchived {
		m.mu.Unlock()
		return nil
	}
	owner := b.Owner
	m.mu.Unlock()

	// The packed rootfs is uploaded verbatim, so the managed secret block must
	// be cleared while the guest is reachable — restore re-pushes it via the
	// EnsureRunning hook, so nothing is lost.
	if err := m.stripEnvForPack(ctx, name); err != nil {
		return fmt.Errorf("archive %s: %w", name, err)
	}
	// Pause so the guest has flushed and unmounted its rootfs. Idempotent
	// if already paused; uses the manager Pause so state/teardown are consistent.
	if err := m.pause(ctx, name, "was paused for archive"); err != nil {
		return fmt.Errorf("archive %s: pause: %w", name, err)
	}

	packPath, err := m.archiver.PackRootfs(ctx, name)
	if err != nil {
		return fmt.Errorf("archive %s: pack: %w", name, err)
	}
	defer os.Remove(packPath) //nolint:errcheck
	var archiveMB int64
	if fi, serr := os.Stat(packPath); serr == nil {
		archiveMB = fi.Size() / (1024 * 1024)
	}
	key := m.archiveKey(owner, name)
	if err := m.archive.Put(ctx, key, packPath); err != nil {
		return fmt.Errorf("archive %s: upload: %w", name, err)
	}
	// The archive is durable now, so reclaim the local VM dir (rootfs + any
	// leftover snapshot). Use the driver directly, NOT Manager.Destroy: the
	// sandbox is coming back, so its routes, schedules, and front door must stay.
	if err := m.driver.Destroy(ctx, name); err != nil {
		m.log.Warn("archive: local destroy failed (archive is safe)", "name", name, "err", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok = m.boxes[name]
	if !ok {
		// Destroyed out from under us mid-archive; don't leave an orphan object.
		go m.archive.Delete(context.WithoutCancel(ctx), key) //nolint:errcheck
		return nil
	}
	b.State = vmm.StateArchived
	b.ArchiveKey = key
	b.ArchivedAt = time.Now().UTC()
	b.SSHAddr, b.HostIP, b.GuestV6 = "", "", ""
	// Local disk is freed, but the archive still occupies the owner's pooled
	// budget (in object storage) — count its compressed size, not 0.
	b.DiskMB = archiveMB
	m.log.Info("sandbox archived", "name", name, "key", key, "archive_mb", archiveMB)
	m.observe(b, "archived")
	return m.save()
}

// restore pulls an archived sandbox's rootfs back onto local disk and marks it
// Paused, so the normal resume path (resumeOrRecreate → cold boot from the
// present rootfs) brings it up. The multi-GB download + unpack run without m.mu
// held. The archive object is dropped once we hold a local copy — archive/
// restore is a move, not a copy, so a re-archive later rewrites a fresh one.
func (m *Manager) restore(ctx context.Context, name, key string) error {
	if !m.ArchivingEnabled() {
		return errors.New("sandbox is archived but archiving is not enabled on this host")
	}
	if key == "" {
		return fmt.Errorf("sandbox %q has no archive key", name)
	}
	tmp := filepath.Join(m.stateDir, name+".restore.ext4.zst")
	defer os.Remove(tmp) //nolint:errcheck
	if err := m.archive.Get(ctx, key, tmp); err != nil {
		return fmt.Errorf("restore %s: download: %w", name, err)
	}
	if err := m.archiver.UnpackRootfs(ctx, name, tmp); err != nil {
		return fmt.Errorf("restore %s: unpack: %w", name, err)
	}
	m.mu.Lock()
	b, ok := m.boxes[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q not found", name)
	}
	b.State = vmm.StatePaused // rootfs present, no snapshot → resumeOrRecreate cold-boots it
	b.ArchiveKey = ""
	b.ArchivedAt = time.Time{}
	// UnpackRootfs decompressed a full image, so this rootfs shares no extents
	// with any template and the reflink discount no longer applies. The next
	// refresh tick re-measures and may hand the discount back even though every
	// block was written fresh — a bounded over-credit in the user's favour on a
	// soft budget, deliberately preferred over a persisted "was cloned" bit that
	// create, both restore paths and resumeOrRecreate would all have to maintain.
	b.BaseDiskMB = 0
	m.observe(b, "restored")
	err := m.save()
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if derr := m.archive.Delete(ctx, key); derr != nil {
		m.log.Warn("restore: archive cleanup failed", "name", name, "key", key, "err", derr)
	}
	m.log.Info("sandbox restored from archive", "name", name)
	return nil
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
	// Linux deliberately uses otherwise-idle RAM for reclaimable page cache.
	// Treating only MemFree as unused makes a healthy cached guest look nearly
	// full (and paints the consoles red) even though that memory is immediately
	// available to new work. Firecracker exposes the guest's MemAvailable
	// estimate, which is the same honest denominator `free` prints. Older
	// balloon implementations may leave it zero, so retain FreeMiB as a
	// compatibility fallback.
	unused := st.AvailableMiB
	if unused <= 0 {
		unused = st.FreeMiB
	}
	used := memMB - st.ActualMiB - unused
	if used < 0 {
		used = 0
	}
	return used, true
}

// CPUSeconds reports a running sandbox's cumulative host CPU time in seconds —
// the VMM process's vCPU + overhead time, so surfaces should label it "host
// CPU". Callers derive a utilization percentage from deltas between polls
// (÷ vcpus); the counter resets to zero on a cold boot. ok=false when
// unavailable (the driver has no CPU stats, or the sandbox isn't running).
// The driver call is made without m.mu held.
func (m *Manager) CPUSeconds(ctx context.Context, name string) (seconds float64, ok bool) {
	m.mu.Lock()
	b, exists := m.boxes[name]
	if !exists || m.cpuStats == nil || b.State != vmm.StateRunning {
		m.mu.Unlock()
		return 0, false
	}
	cs := m.cpuStats
	m.mu.Unlock()

	nanos, err := cs.CPUTimeNanos(ctx, name)
	if err != nil {
		return 0, false
	}
	return float64(nanos) / 1e9, true
}

// NetCounters reports a running sandbox's network counters straight from the
// device, in bytes, from the guest's point of view — rx received, tx sent.
//
// These are NOT Sandbox.NetRxBytes/NetTxBytes. Those are lifetime totals the
// reaper accumulates a minute at a time and which only ever grow; these are the
// raw tap counters, which die and restart at zero with the host-side device on
// every pause, resume and cold boot. A caller deriving a rate from two readings
// must treat a reading BELOW the previous one as that reset rather than as a
// negative rate — the same rule counterDelta encodes for the totals.
//
// It exists because a rate and a total want different resolutions: the reaper's
// once-a-minute sample is the right cadence for metering and the wrong one for
// a live meter, and reading through to the device costs two sysfs reads.
// ok=false when unavailable (the driver has no counters, the sandbox is not on
// this machine, or it isn't running). The driver call is made without m.mu held.
func (m *Manager) NetCounters(ctx context.Context, name string) (rx, tx uint64, ok bool) {
	m.mu.Lock()
	b, exists := m.boxes[name]
	if !exists || m.netStats == nil || b.State != vmm.StateRunning {
		m.mu.Unlock()
		return 0, 0, false
	}
	ns := m.netStats
	m.mu.Unlock()

	rx, tx, err := ns.NetBytes(ctx, name)
	if err != nil {
		return 0, 0, false
	}
	return rx, tx, true
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
	if pinned {
		m.observe(b, "pinned")
	} else {
		m.observe(b, "unpinned")
	}
	return m.save()
}

// SetRepoStatus records the latest bounded, read-only survey published by a
// running guest. The metadata service has already authenticated the caller by
// tap address and validated every field. Keeping the write on Manager makes it
// durable on a standalone host and lets the ordinary inventory observer carry
// the same advisory state from a fleet node to its gateway.
func (m *Manager) SetRepoStatus(name string, repos []RepoStatus, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok {
		return fmt.Errorf("sandbox %q not found", name)
	}
	b.Repos = append([]RepoStatus(nil), repos...)
	b.RepoStatusAt = at.UTC()
	m.observe(b, "repo-status")
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
		if _, err := m.EnsureReady(ctx, b.Name); err != nil {
			m.log.Warn("resume pinned sandbox failed", "name", b.Name, "err", err)
			continue
		}
		m.log.Info("resumed pinned sandbox on boot", "name", b.Name)
	}
}

func (m *Manager) Destroy(ctx context.Context, name string) error {
	unlock := m.lockDiskOperation(name)
	defer unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok {
		return fmt.Errorf("sandbox %q not found", name)
	}
	if err := m.driver.Destroy(ctx, name); err != nil {
		return err
	}
	// An archived sandbox has no local VM (already destroyed at archive time) but
	// its rootfs lives in object storage — drop it so a delete leaves nothing.
	if b.State == vmm.StateArchived && m.archive != nil && b.ArchiveKey != "" {
		if err := m.archive.Delete(ctx, b.ArchiveKey); err != nil {
			m.log.Warn("archive object cleanup failed", "name", name, "key", b.ArchiveKey, "err", err)
		}
	}
	delete(m.boxes, name)
	delete(m.vitals, name)
	delete(m.memUsed, name)
	delete(m.balloonedAt, name)
	delete(m.protectUntil, b.ID)
	m.activityMu.Lock()
	delete(m.activity, name)
	delete(m.markedAt, name)
	m.activityMu.Unlock()
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
	if m.tags != nil {
		if err := m.tags.DeleteBySandbox(name); err != nil {
			m.log.Warn("tag cleanup failed", "name", name, "err", err)
		}
	}
	// Not merely tidiness: this name can be created again, by anybody, and a
	// surviving override would decide what THAT sandbox checks out.
	if m.repoRefs != nil {
		if err := m.repoRefs.DeleteBySandbox(name); err != nil {
			m.log.Warn("repo ref-override cleanup failed", "name", name, "err", err)
		}
	}
	if m.frontDoor != nil {
		m.frontDoor.Remove(ctx, name)
	}
	m.log.Info("sandbox destroyed", "name", name)
	if m.observer != nil {
		m.observer.SandboxGone(name)
	}
	return m.save()
}

// MarkActive records sandbox activity without waiting for a lifecycle lock,
// driver call, or disk write. Marks are coalesced per sandbox; the accepted
// timestamp is immediately visible through Get/List and is applied to the live
// record (including balloon deflation) asynchronously.
func (m *Manager) MarkActive(name string) {
	now := time.Now().UTC()
	m.activityMu.Lock()
	if last := m.markedAt[name]; !last.IsZero() && now.Sub(last) < activityInterval {
		m.activityMu.Unlock()
		return
	}
	m.markedAt[name] = now
	m.activity[name] = now
	m.activityMu.Unlock()

	go m.applyActivity(name, now)
}

// latestActivity returns the newest in-memory activity timestamp. Callers may
// hold m.mu; MarkActive never takes it, so the lock order is always
// m.mu -> activityMu.
func (m *Manager) latestActivity(name string, recorded time.Time) time.Time {
	m.activityMu.Lock()
	defer m.activityMu.Unlock()
	if at := m.activity[name]; at.After(recorded) {
		return at
	}
	return recorded
}

func (m *Manager) applyActivity(name string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok {
		m.activityMu.Lock()
		delete(m.activity, name)
		delete(m.markedAt, name)
		m.activityMu.Unlock()
		return
	}
	if at.After(b.LastActive) {
		b.LastActive = at
	}
	if b.State == vmm.StateRunning {
		// MarkActive itself stays nonblocking; returning reclaimed RAM belongs
		// on this asynchronous node-local path, not on the gateway request.
		m.deflate(context.Background(), b)
	}
}

// FlushActivity durably persists accepted activity marks. Lifecycle saves also
// merge dirty activity, so this is only needed by the periodic and graceful
// shutdown paths.
func (m *Manager) FlushActivity() error {
	m.activityMu.Lock()
	marks := len(m.activity)
	m.activityMu.Unlock()
	if marks == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.save()
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	m.metrics.ObserveActivityFlush(m.nodeName, outcome, marks)
	return err
}

// minVitalsInterval is the shortest gap between two readings we'll turn into a
// rate. The reaper ticks a minute apart so this only guards pathological cases
// (a test looping reapOnce, a tick storm after clock adjustment) where the tiny
// divisor would turn counter noise into a huge apparent rate.
const minVitalsInterval = 5 * time.Second

// memoryPressureInterval is deliberately no faster than the vitals sampler.
// Ballooning is expensive guest work, not a cheap accounting adjustment: a
// tight controller can otherwise act repeatedly before CPU/network activity has
// had a chance to prove that a freshly-started workload is busy.
const memoryPressureInterval = time.Minute

// memoryPressureIdle is the minimum quiet period before pooled-memory pressure
// may reclaim a sandbox. Admission leaves host headroom for short bursts; this
// grace period lets the minute-cadence vitals sampler distinguish a cold page
// cache being populated from genuinely cold memory. In particular, a new VM
// must never be ballooned on the controller's first tick merely because its
// owner is above the steady-state pool while the dev stack is starting.
const memoryPressureIdle = 2 * time.Minute

// balloonSettle is how long after a balloon-down a sandbox's CPU counter is
// ignored for activity purposes. Long enough to cover the reclaim burst at the
// reaper's one-minute cadence; short enough that unattended CPU-only work gets
// its RAM back promptly. Interactive use never waits on it — MarkActive
// deflates directly, without consulting a counter at all.
const balloonSettle = 3 * time.Minute

// balloonHeadroomPct is the room the idle reaper leaves above a guest's
// *measured* working set when it squeezes one, on top of --mem-reserve-mb.
//
// The reserve alone is a guess made before the guest existed, and squeezing
// below what the guest is demonstrably touching does not reclaim idle RAM — it
// starts a reclaim storm. Measured on the Mac dev box: a 4026 MiB guest was
// ballooned to its 256 MiB reserve two minutes into a boot that had not
// finished, whereupon containerd failed eleven times, docker took fourteen
// minutes to start, and all four vCPUs sat at 100% system time in page reclaim
// for the better part of an hour. Nothing deflated it, because a guest that
// starved before its agent came up can never produce the traffic that the
// activity floor reads as life. Scaling the headroom with the working set
// rather than adding a flat number keeps it honest for both a 200 MiB idle box
// and a 2 GiB build.
const balloonHeadroomPct = 50

// minBalloonReclaimMiB is the smallest reclaim worth the guest's page faults.
// Every inflation costs the guest a reclaim pass and the host a fault storm as
// the working set comes back, so shaving a few dozen megabytes off an idle box
// is a straight loss.
const minBalloonReclaimMiB = 512

// refreshVitals samples every running sandbox's CPU and network counters, adds
// the network delta to its lifetime totals, and resets the idle clock of any
// sandbox busier than the configured activity floors.
//
// This is the signal that makes the reaper safe for unattended work. Without
// it the only evidence of life is control-plane traffic — an SSH connect, a
// proxied request — so a box running a coding agent, a build, or a training
// job for an hour with nobody watching looks exactly as idle as an abandoned
// one, and gets paused mid-flight. Deltas are per-interval rates, so a sandbox
// has to be *continuously* quiet across the whole idle timeout to be reaped.
//
// Accumulating byte totals happens whenever the driver can report them, even
// with the activity floors off, because that accounting is also the basis for
// egress metering.
func (m *Manager) refreshVitals(ctx context.Context) {
	if m.cpuStats == nil && m.netStats == nil {
		return
	}
	now := time.Now()
	for _, b := range m.List() {
		if b.State != vmm.StateRunning {
			continue
		}
		// Driver calls go out without m.mu held: they read sysfs and /proc, and
		// the manager lock guards a lot more than this sandbox.
		cur := vitalsSample{at: now}
		var cpuOK, netOK bool
		if m.cpuStats != nil {
			if n, err := m.cpuStats.CPUTimeNanos(ctx, b.Name); err == nil {
				cur.cpuNanos, cpuOK = n, true
			}
		}
		if m.netStats != nil {
			if rx, tx, err := m.netStats.NetBytes(ctx, b.Name); err == nil {
				cur.rx, cur.tx, netOK = rx, tx, true
			}
		}
		if !cpuOK && !netOK {
			continue // sandbox went away or lost its tap between List and here
		}
		m.applyVitals(ctx, b.Name, cur, cpuOK, netOK)
	}
}

// applyVitals folds one reading into a sandbox's state under m.mu: it charges
// the network delta to the lifetime totals and decides whether the sandbox was
// busy enough over the interval to count as active. Split from refreshVitals so
// the lock is held only for bookkeeping, never across a driver call.
func (m *Manager) applyVitals(ctx context.Context, name string, cur vitalsSample, cpuOK, netOK bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok {
		return
	}
	prev, primed := m.vitals[name]
	if !primed {
		// First reading for this sandbox (fresh boot, resume, or a control-plane
		// restart). There is no baseline to subtract, and the counters may have
		// just reset, so record it and judge activity from the next tick.
		m.vitals[name] = cur
		return
	}
	elapsed := cur.at.Sub(prev.at)
	if elapsed < minVitalsInterval {
		// Keep the older baseline rather than replacing it: a too-short gap
		// should defer the measurement, not discard the interval entirely.
		return
	}
	m.vitals[name] = cur

	var netDelta uint64
	if netOK {
		rxDelta, txDelta := counterDelta(prev.rx, cur.rx), counterDelta(prev.tx, cur.tx)
		netDelta = rxDelta + txDelta
		b.NetRxBytes += rxDelta
		b.NetTxBytes += txDelta
	}
	var cpuPct float64
	if cpuOK {
		cpuPct = float64(counterDelta(prev.cpuNanos, cur.cpuNanos)) / float64(elapsed.Nanoseconds()) * 100
	}

	// CPU is not evidence of work while a fresh balloon is still settling.
	// vmm.CPUStatser reads the VMM process's own utime+stime, so inflating a
	// balloon is charged to the very counter this reads: the host releases
	// gigabytes of guest pages and the guest kernel then faults its working set
	// back in, and that by itself clears the activity floor. Believing it closed
	// a loop — balloon down, read the reclaim as work, deflate and reset the
	// idle clock, balloon down again a couple of minutes later — under which a
	// sandbox can never accumulate enough idle to be paused, and one sat warm
	// for fifteen hours doing nothing. Network is untouched by ballooning, so it
	// stays a valid signal in both states.
	cpuBusy := m.actCPUPct > 0 && cpuOK && cpuPct >= m.actCPUPct && m.balloonSettled(name, cur.at)
	netBusy := m.actNetBytes > 0 && netOK && netDelta >= m.actNetBytes
	busy := cpuBusy || netBusy
	if busy {
		b.LastActive = time.Now().UTC()
		// Work is happening, so give back the RAM the warm tier reclaimed.
		m.deflate(ctx, b)
		m.log.Debug("sandbox active by vitals", "name", name,
			"cpu_pct", cpuPct, "net_bytes", netDelta, "elapsed", elapsed)
		m.observe(b, "touched")
	}
	m.save() //nolint:errcheck
}

// balloonSettled reports whether enough time has passed since the reaper last
// inflated this sandbox's balloon for its CPU counter to mean what it usually
// means. A sandbox that was never ballooned is settled by definition. Callers
// hold m.mu.
func (m *Manager) balloonSettled(name string, at time.Time) bool {
	inflated, ok := m.balloonedAt[name]
	return !ok || at.Sub(inflated) >= balloonSettle
}

// counterDelta subtracts two readings of a cumulative counter that restarts at
// zero whenever its backing device is recreated — which for the tap counters is
// every pause/resume. A reading below the previous one is that reset, not a
// 64-bit rollover, so the new value *is* the delta.
func counterDelta(prev, cur uint64) uint64 {
	if cur < prev {
		return cur
	}
	return cur - prev
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

// RunMemoryPressureController keeps intentional overcommit inside its safety
// rails. Owner pools overlap and admission charges working-set floors, so the
// controller periodically compares actual balloon-stat working sets with both
// owner and node budgets. It reclaims cold memory without pausing guests.
func (m *Manager) RunMemoryPressureController(ctx context.Context, interval time.Duration) {
	if m.balloon == nil || m.reserveMB <= 0 {
		return
	}
	if interval <= 0 {
		interval = memoryPressureInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reconcileMemoryPressure(ctx)
		}
	}
}

func (m *Manager) reconcileMemoryPressure(ctx context.Context) {
	m.refreshMemoryUsage(ctx)

	type ownerUse struct {
		used     int64
		hasTurbo bool
	}
	m.mu.Lock()
	owners := make(map[string]ownerUse)
	var nodeUsed int64
	for _, b := range m.boxes {
		if b.State != vmm.StateRunning {
			continue
		}
		used := m.observedMemMB(b)
		nodeUsed += used
		o := owners[b.Owner]
		o.used += used
		o.hasTurbo = o.hasTurbo || b.Turbo
		owners[b.Owner] = o
	}
	ownerPool, ownerBurst := m.ownerMemPoolMB, m.ownerMemBurstMB
	nodeBudget := int64(0)
	if m.memAdmitPct > 0 && m.hostMemMB > 0 {
		nodeBudget = m.hostMemMB * int64(m.memAdmitPct) / 100
	}
	m.mu.Unlock()

	for owner, usage := range owners {
		budget := ownerPool
		if usage.hasTurbo && ownerBurst > 0 {
			budget = ownerBurst
		}
		if budget > 0 && usage.used > budget {
			m.reclaimMemory(ctx, owner, usage.used-budget)
		}
	}
	if nodeBudget > 0 && nodeUsed > nodeBudget {
		m.reclaimMemory(ctx, "", nodeUsed-nodeBudget)
	}
}

// memSampleTTL is how long a working-set sample stays good enough to reuse.
// The reaper and the memory-pressure controller both need one and both run on
// the same one-minute cadence, so their tickers land within microseconds of
// each other; without this, every running guest is asked for balloon stats
// twice per minute for identical data.
const memSampleTTL = 5 * time.Second

func (m *Manager) refreshMemoryUsage(ctx context.Context) {
	m.mu.Lock()
	fresh := !m.memUsedAt.IsZero() && time.Since(m.memUsedAt) < memSampleTTL
	m.mu.Unlock()
	if fresh {
		return
	}
	sampleCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, b := range m.List() {
		if b.State != vmm.StateRunning {
			continue
		}
		name := b.Name
		wg.Add(1)
		go func() {
			defer wg.Done()
			used, ok := m.MemStats(sampleCtx, name)
			if !ok {
				return
			}
			m.mu.Lock()
			if current, exists := m.boxes[name]; exists && current.State == vmm.StateRunning {
				m.memUsed[name] = used
			}
			m.mu.Unlock()
		}()
	}
	wg.Wait()
	m.mu.Lock()
	m.memUsedAt = time.Now()
	m.mu.Unlock()
}

// reclaimMemory balloons cold, unpinned VMs until their projected reclaim
// covers excessMiB. Turbo is borrowed capacity and is reclaimed first. Recent
// activity is an eligibility boundary, not merely an ordering hint: reclaiming
// a freshly-started working set makes the balloon and the workload fight over
// the same pages, and interactive activity can then deflate/reinflate it on
// successive controller ticks.
func (m *Manager) reclaimMemory(ctx context.Context, owner string, excessMiB int64) {
	type candidate struct {
		box         *Sandbox
		reclaimable int64
	}
	var candidates []candidate
	m.mu.Lock()
	now := time.Now()
	for _, b := range m.boxes {
		if b.State != vmm.StateRunning || b.Pinned || b.Ballooned || (owner != "" && b.Owner != owner) {
			continue
		}
		lastActive := m.latestActivity(b.Name, b.LastActive)
		if now.Sub(lastActive) < memoryPressureIdle {
			continue
		}
		floor := m.workingSetFloor(b)
		reclaimable := m.observedMemMB(b) - floor
		if reclaimable > 0 {
			candidates = append(candidates, candidate{box: copyOf(b), reclaimable: reclaimable})
		}
	}
	m.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].box.Turbo != candidates[j].box.Turbo {
			return candidates[i].box.Turbo
		}
		return candidates[i].box.LastActive.Before(candidates[j].box.LastActive)
	})
	remaining := excessMiB
	for _, c := range candidates {
		if remaining <= 0 {
			break
		}
		reclaim := min(remaining, c.reclaimable)
		if err := m.balloonForPressure(ctx, c.box.Name, reclaim); err != nil {
			m.log.Warn("memory pressure balloon failed", "name", c.box.Name, "err", err)
			continue
		}
		remaining -= reclaim
	}
	if remaining > 0 {
		m.log.Warn("memory pressure could not be fully reclaimed", "owner", owner,
			"requested_mb", excessMiB, "remaining_mb", remaining)
	}
}

// ProtectUntil prevents the idle reaper from pausing one sandbox before until.
// It does not change LastActive and it does not prevent ballooning: the lease
// says an external session still needs the VM reachable, not that the workload
// is consuming memory. Older or expired observations never shorten a lease.
func (m *Manager) ProtectUntil(sandboxID string, until time.Time) {
	if sandboxID == "" || !until.After(time.Now()) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if current := m.protectUntil[sandboxID]; until.After(current) {
		m.protectUntil[sandboxID] = until
	}
}

func (m *Manager) isProtected(sandboxID string, now time.Time) bool {
	if sandboxID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	until := m.protectUntil[sandboxID]
	if !until.After(now) {
		delete(m.protectUntil, sandboxID)
		return false
	}
	return true
}

// reapOnce applies one pass of the idle policy. Split out from RunReaper's loop
// so the two-stage gradient is unit-testable without a ticker.
func (m *Manager) reapOnce(ctx context.Context, balloonAfter, pauseAfter time.Duration) {
	// The reaper is already the manager's periodic maintenance loop. Piggyback
	// activity persistence on its cadence so warm traffic costs no extra
	// ticker or request-path write.
	defer func() {
		if err := m.FlushActivity(); err != nil {
			m.log.Warn("activity flush failed", "err", err)
		}
	}()
	// Keep disk accounting fresh while we're already ticking: a running/paused
	// sandbox's rootfs (and snapshot) grow over time.
	//
	// Measurement is deliberately NOT gated on the pooled quota. It used to be,
	// which meant a host running without --disk-pool-mb-per-owner never measured
	// anything and reported every sandbox as 0 GB — the number the consoles show.
	// The pool governs *admission*, not whether we are allowed to look.
	if m.diskReport != nil {
		m.RefreshDiskUsage(ctx)
	}
	// Fold in-guest resource use into LastActive before judging idleness, so a
	// sandbox that is working but receiving no inbound traffic isn't reaped.
	m.refreshVitals(ctx)
	boxes := m.List()
	// workingSetFloor reads m.memUsed, so the balloon stage needs working sets
	// measured now rather than whenever the pressure controller last ran. Paid
	// only on a node that can balloon at all; refreshMemoryUsage itself skips a
	// pass whose sample is still fresh, so the two controllers on the same
	// cadence do not each pay for one.
	if m.balloon != nil && m.reserveMB > 0 && balloonAfter > 0 {
		m.refreshMemoryUsage(ctx)
	}
	for _, b := range boxes {
		// Pinned sandboxes hold their full RAM on purpose so in-guest
		// timers/daemons keep firing; the reaper never touches them.
		if b.Pinned || b.State != vmm.StateRunning {
			continue
		}
		idle := time.Since(b.LastActive)
		protected := m.isProtected(b.ID, time.Now())
		switch {
		case idle > pauseAfter && !protected:
			unlock := m.lockDiskOperation(b.Name)
			err := m.pause(ctx, b.Name, fmt.Sprintf("went idle for %s", pauseAfter))
			unlock()
			if err != nil {
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

// RefreshDiskUsage re-measures every non-archived sandbox's durable filesystem
// usage for pooled accounting. Called each reaper tick and
// available for on-demand refresh. Archived boxes keep their fixed archive size.
func (m *Manager) RefreshDiskUsage(ctx context.Context) {
	if m.diskReport == nil {
		return
	}
	for _, b := range m.List() {
		if b.State != vmm.StateArchived {
			m.refreshDiskUsage(ctx, b.Name)
		}
	}
}

// refreshDiskUsage measures a sandbox's current durable filesystem usage via
// the driver and updates its DiskMB for pooled accounting. The driver call is
// made without m.mu held; the brief locked section only stores the result.
//
// The template baseline is re-measured here rather than stamped at create time
// because the refresher replaces base templates by atomic rename: a create-time
// figure would describe an image that no longer exists. Re-reading each tick
// keeps it within one template revision of the truth.
func (m *Manager) refreshDiskUsage(ctx context.Context, name string) {
	if m.diskReport == nil {
		return
	}
	mb, err := m.diskReport.DiskUsageMB(ctx, name)
	if err != nil {
		return
	}
	// Ceiling is best-effort and independent: a driver that can't report it just
	// leaves the consoles showing a bare usage figure with no meter.
	capMB, capErr := m.diskReport.DiskCapacityMB(ctx, name)
	baseMB, baseErr := m.templateUsageMB(ctx, name)
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok || b.State == vmm.StateArchived {
		return
	}
	changed := b.DiskMB != mb
	b.DiskMB = mb
	if capErr == nil && capMB > 0 && b.DiskTotalMB != capMB {
		b.DiskTotalMB = capMB
		changed = true
	}
	// A failed measurement KEEPS the stored baseline. Deleting a snapshot makes
	// its template unreadable, and treating that as "baseline 0" would spike the
	// pooled charge of every sandbox forked from it by the whole template size —
	// on a tick, with nobody having written a byte. Same for a transient read
	// error.
	//
	// A move in the baseline alone still wakes the observer. It used not to,
	// because BaseDiskMB was node-local; it now rides the inventory so a gateway
	// can charge an owner for the blocks their sandboxes wrote rather than for
	// the reflinked copy that was never made, and a figure that never broadcasts
	// is one the gateway holds a stale copy of forever. The churn this adds is
	// nil in practice: a baseline only moves when the refresher replaces the
	// template a sandbox was forked from, not on the tick that re-measures it.
	if baseErr == nil && b.BaseDiskMB != baseMB {
		b.BaseDiskMB = baseMB
		changed = true
	}
	if changed {
		m.observe(b, "disk")
		m.save() //nolint:errcheck
	}
}

// templateUsageMB measures the used blocks of the template a sandbox was
// created from, for the pooled-disk baseline. The record is read under a brief
// lock and the driver call made outside it, matching refreshDiskUsage's shape:
// reading an image dir can touch a cold disk and must not hold up admission.
//
// An error (no such capability, no such image, unreadable superblock) leaves
// the caller's stored baseline alone — see refreshDiskUsage.
func (m *Manager) templateUsageMB(ctx context.Context, name string) (int64, error) {
	if m.templateReport == nil {
		return 0, errNoTemplateReport
	}
	m.mu.Lock()
	b, ok := m.boxes[name]
	var image string
	if ok {
		image = b.Image
	}
	m.mu.Unlock()
	if !ok || image == "" {
		return 0, fmt.Errorf("sandbox %q has no image to measure", name)
	}
	return m.templateReport.TemplateUsageMB(ctx, image)
}

// errNoTemplateReport is the "driver can't measure templates" answer. It is an
// error rather than a zero so callers keep whatever baseline they already had
// instead of re-charging shared blocks — see the vmm.TemplateReporter contract.
var errNoTemplateReport = errors.New("driver cannot measure template usage")

// balloonDown reclaims a running sandbox's idle RAM to the host by inflating
// its balloon to leave only the working-set reserve, without pausing it. A
// no-op if overcommit is off, the driver has no balloon, or there's nothing to
// reclaim above the reserve.
func (m *Manager) balloonDown(ctx context.Context, name string) error {
	return m.setBalloonTarget(ctx, name, 0)
}

// balloonForPressure reclaims no more than the measured pool excess. The idle
// path intentionally takes a cold VM all the way to its reserve, but doing that
// for a small transient overage turns a 100 MiB correction into several GiB of
// needless guest reclaim.
func (m *Manager) balloonForPressure(ctx context.Context, name string, reclaimMiB int64) error {
	if reclaimMiB <= 0 {
		return nil
	}
	return m.setBalloonTarget(ctx, name, reclaimMiB)
}

// setBalloonTarget inflates a running sandbox's balloon. reclaimMiB <= 0 means
// the idle policy's full target; a positive value bounds pressure reclaim.
func (m *Manager) setBalloonTarget(ctx context.Context, name string, reclaimMiB int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok || b.State != vmm.StateRunning || b.Ballooned || m.balloon == nil || m.reserveMB <= 0 {
		return nil
	}
	floor := m.workingSetFloor(b)
	// Never squeeze a guest below what the balloon device says it is actually
	// touching. The configured reserve is a guess made before the guest
	// existed, and going under the live set is self-reinforcing: the guest has
	// to fault the whole set back in, which pegs its vCPUs, and a guest that
	// cannot run never produces the activity that would deflate the balloon.
	//
	// Both paths get that floor; only the idle path also gets headroom. The
	// idle path is opportunistic — nobody is waiting for this memory, so
	// leaving room to grow costs nothing. The pressure path is closing a real
	// overage, where being too conservative means an admission failure instead
	// of a thrashing guest, so it reclaims right down to the live set.
	//
	// An unmeasured sandbox reads as 0 and keeps the configured reserve, which
	// is what the pooled accounting already assumes for it. Deliberately not
	// observedMemMB, whose fall back to the configured ceiling would read as
	// "using everything" and stop the reaper ballooning at all.
	if live := m.memUsed[name]; live > 0 {
		want := live
		if reclaimMiB <= 0 {
			want = live + live*balloonHeadroomPct/100
		}
		if want > floor {
			floor = want
		}
	}
	if floor > b.MemMB {
		floor = b.MemMB
	}
	target := b.MemMB - floor
	if reclaimMiB > 0 && reclaimMiB < target {
		target = reclaimMiB
	}
	// Below the minimum the inflation costs the guest more than the host gains.
	// The pressure path is exempt: it was asked for a specific number of
	// megabytes to close a real overage, and a partial answer still helps.
	if reclaimMiB <= 0 && target < minBalloonReclaimMiB {
		m.log.Debug("idle balloon declined: too little to reclaim above the live working set",
			"name", name, "mem_mb", b.MemMB, "floor_mb", floor, "would_reclaim_mb", target)
		return nil
	}
	if target <= 0 {
		return nil
	}
	if err := m.balloon.SetBalloonTarget(ctx, name, target); err != nil {
		return err
	}
	b.Ballooned = true
	m.balloonedAt[name] = time.Now()
	if reclaimMiB > 0 {
		m.log.Info("memory pressure ballooned cold sandbox", "name", name, "reclaim_mb", target)
	} else {
		// floor_mb is the number that matters when this goes wrong: it is what
		// the guest is left with, and a guest left below its working set will
		// thrash instead of idling.
		m.log.Info("reaper ballooned down idle sandbox", "name", name,
			"reclaim_mb", target, "floor_mb", floor, "measured_mb", m.memUsed[name])
	}
	m.observe(b, "ballooned")
	return m.save()
}

// workingSetFloor is the configured least-RAM for a sandbox: the admission
// reserve, doubled for turbo. It is the floor before anything is known about
// what the guest is actually touching, which is why it is only a ranking
// quantity here — setBalloonTarget raises it to the measured working set
// before any balloon is actually inflated. Callers hold m.mu.
func (m *Manager) workingSetFloor(b *Sandbox) int64 {
	floor := m.reserveMB
	if b.Turbo {
		floor *= TurboFactor
	}
	if floor > b.MemMB {
		return b.MemMB
	}
	return floor
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
	delete(m.balloonedAt, b.Name)
	m.log.Info("deflated balloon on activity", "name", b.Name)
	m.observe(b, "deflated")
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

// save persists state; callers must hold m.mu. Dirty activity is folded into
// the same atomic state-file replacement. Clear only the timestamps included
// in this write: a newer MarkActive may arrive while the file is being written.
func (m *Manager) save() error {
	started := time.Now()
	outcome := "ok"
	defer func() {
		m.metrics.ObserveManagerSave(m.nodeName, outcome, time.Since(started))
	}()
	pending := m.mergeActivityLocked()
	data, err := json.MarshalIndent(m.boxes, "", "  ")
	if err != nil {
		outcome = "error"
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		outcome = "error"
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		outcome = "error"
		return err
	}
	m.activityMu.Lock()
	for name, at := range pending {
		if current, ok := m.activity[name]; ok && !current.After(at) {
			delete(m.activity, name)
		}
	}
	m.activityMu.Unlock()
	return nil
}

// mergeActivityLocked applies a stable snapshot of dirty activity to records.
// Callers hold m.mu.
func (m *Manager) mergeActivityLocked() map[string]time.Time {
	m.activityMu.Lock()
	pending := make(map[string]time.Time, len(m.activity))
	for name, at := range m.activity {
		pending[name] = at
	}
	m.activityMu.Unlock()
	for name, at := range pending {
		if b, ok := m.boxes[name]; ok && at.After(b.LastActive) {
			b.LastActive = at
		}
	}
	return pending
}

func copyOf(b *Sandbox) *Sandbox {
	c := *b
	c.Repos = append([]RepoStatus(nil), b.Repos...)
	if b.HiveMind != nil {
		snapshot := *b.HiveMind
		snapshot.Sessions = cloneHiveMindSessions(b.HiveMind.Sessions)
		c.HiveMind = &snapshot
	}
	return &c
}

func cloneHiveMindSessions(sessions []HiveMindSession) []HiveMindSession {
	out := append([]HiveMindSession(nil), sessions...)
	for i := range out {
		if out[i].EndedAt == nil {
			continue
		}
		endedAt := *out[i].EndedAt
		out[i].EndedAt = &endedAt
	}
	return out
}

// Public copies a record for anything outside the control plane with its three
// addresses dropped: SSHAddr, HostIP and GuestV6. nil in, nil out.
//
// It lives on the type that declares those fields, and not in each surface that
// serializes one, so that a field added here is dropped by everything at once.
// Three surfaces call it — both consoles (via webui.Public) and the legacy
// loopback API — and internal/ctlops does the same thing by projecting onto a
// shape that never had the fields at all.
//
// Why they must not escape. On a single box they are a guest address on a
// bridge no client can route to: useless, and a needless statement of internal
// layout. In a fleet they are worse than useless. Every machine mints its
// guests the same 172.30.<idx>.2, so an address relayed from another machine
// names one of THIS machine's sandboxes — which is why the fleet replaces them
// with a synthetic <sandbox>.<node>.sandbox.invalid that resolves nowhere on
// purpose. Emitting either is an invitation for something to dial it, and the
// synthetic form additionally spells out which machine holds whose work.
func (b *Sandbox) Public() *Sandbox {
	if b == nil {
		return nil
	}
	c := *b
	c.Repos = append([]RepoStatus(nil), b.Repos...)
	c.SSHAddr, c.HostIP, c.GuestV6 = "", "", ""
	return &c
}
