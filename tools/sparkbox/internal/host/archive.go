package host

import (
	"context"
	"fmt"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// EnvStripper is the optional pre-pack counterpart to EnvPusher: rewrite a
// sandbox's managed /etc/environment block to empty (see internal/envsync) so
// a packed rootfs — an archive bound for object storage, or a snapshot
// template every fork copies byte-for-byte — never carries plaintext secret
// values. Detected on the env-sync hook with a type assertion, so a pusher
// that predates the method is simply skipped.
type EnvStripper interface {
	StripEnv(ctx context.Context, box *Sandbox) error
}

// stripEnvForPack clears name's managed env block before Archive/Snapshot hand
// the rootfs to the driver's pack. The strip rides the same SSH channel as the
// push, so the guest must be reachable: an already-paused sandbox is woken at
// the driver level — never via EnsureRunning, whose env-push hook would race
// the strip by rewriting the secrets — and the caller pauses it again
// immediately after, which is also when the settled state is save()d. A box the
// strip itself woke is re-paused when the strip fails, so a failed pack never
// leaves it running. Unlike pushes, a failed strip fails the caller: packing a
// rootfs whose block could not be cleared would put plaintext secrets in
// object storage.
func (m *Manager) stripEnvForPack(ctx context.Context, name string) error {
	m.mu.Lock()
	stripper, ok := m.envSync.(EnvStripper)
	if !ok {
		m.mu.Unlock()
		return nil
	}
	b, found := m.boxes[name]
	if !found {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q not found", name)
	}
	if b.State == vmm.StateArchived {
		// No local rootfs to strip (or pack); callers refuse archived boxes.
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q is archived", name)
	}
	woke := false
	if b.State != vmm.StateRunning {
		inst, err := m.resumeOrRecreate(ctx, b)
		if err != nil {
			m.mu.Unlock()
			return fmt.Errorf("wake %s to strip secret env: %w", name, err)
		}
		woke = true
		b.State = inst.State
		b.SSHAddr = inst.SSHAddr
		b.SSHUser = inst.SSHUser
		b.HostIP = inst.HostIP
		b.GuestV6 = inst.GuestV6
	}
	// The strip's SSH work runs with m.mu released, and a box being packed is
	// by definition long-idle: restart the idle clock so a reaper tick can't
	// pause it mid-strip.
	b.LastActive = time.Now().UTC()
	box := copyOf(b)
	m.mu.Unlock()
	if err := stripper.StripEnv(ctx, box); err != nil {
		if woke {
			if perr := m.pause(ctx, name, "was re-paused after a failed disk pack"); perr != nil {
				m.log.Warn("re-pause after failed strip", "name", name, "err", perr)
			}
		}
		return fmt.Errorf("strip secret env from %s: %w", name, err)
	}
	return nil
}

// toolRefreshBudget bounds the pre-snapshot tool refresh. It is deliberately
// NOT envsync's push budget (3 minutes, sized for one exec rewriting one file):
// this downloads ~150MB of agent CLIs over the guest's tap and installs them.
//
// The arithmetic it has to fit inside: ctlops.ArchiveTimeout is 15 minutes for
// the WHOLE capture, so five here leaves ten for the e2fsck + zerofree +
// reflink of a 25 GiB rootfs. If snapshots start timing out, the fix is to
// raise ArchiveTimeout — the symptom will read as "snapshots got slower after
// the tool refresh landed", which is the wrong end to pull on.
const toolRefreshBudget = 5 * time.Minute

// refreshToolsForPack installs the host's cached agent CLIs into name's guest
// so the template about to be captured starts current rather than frozen at
// whatever versions the sandbox happened to have. Reports whether it ran.
//
// Four ordering constraints, all of them load-bearing:
//
//   - AFTER stripEnvForPack. That is the only step that wakes a paused guest
//     safely — via resumeOrRecreate rather than EnsureRunning, whose async env
//     push would land after the strip and write the secrets back. This function
//     must never be the one that wakes a box, so on a host with no EnvStripper
//     a paused sandbox is simply captured with the tools it has.
//   - BEFORE the pause, which is the last moment the guest is reachable.
//   - SYNCHRONOUS, unlike RepoSyncer's nudge. A pause landing halfway through
//     writing /usr/local/bin/claude freezes a truncated executable into a
//     template that every fork then copies byte-for-byte.
//   - INSIDE lockDiskOperation, which is what keeps the reaper from pausing the
//     box out from under a download in flight.
//
// Best-effort by design: a failure is a WARN and the capture proceeds. A
// snapshot that failed because a tool download failed is worse than a slightly
// stale snapshot.
func (m *Manager) refreshToolsForPack(ctx context.Context, name string) bool {
	m.mu.Lock()
	refresher := m.toolSync
	if refresher == nil {
		m.mu.Unlock()
		return false
	}
	b, found := m.boxes[name]
	if !found {
		m.mu.Unlock()
		return false
	}
	if b.State != vmm.StateRunning {
		// Nothing woke it — see the ordering note above — so there is no
		// channel into this guest and never will be before the pause. Debug,
		// not Warn: the capture is correct, just no fresher than the disk.
		m.mu.Unlock()
		m.log.Debug("skipped the pre-snapshot agent-tool refresh", "name", name, "state", b.State)
		return false
	}
	// The install runs with m.mu released and takes minutes; restart the idle
	// clock for the same reason stripEnvForPack does, so a reaper tick cannot
	// pause the box mid-download.
	b.LastActive = time.Now().UTC()
	box := copyOf(b)
	m.mu.Unlock()

	// WithTimeout takes the earlier of the two deadlines, so a caller on a
	// tighter budget still wins.
	ctx, cancel := context.WithTimeout(ctx, toolRefreshBudget)
	defer cancel()
	if err := refresher.RefreshTools(ctx, box); err != nil {
		m.log.Warn("could not refresh the agent tools before capturing a template; capturing what the sandbox has",
			"name", name, "err", err)
		return false
	}
	m.log.Info("refreshed the agent tools before capturing a template", "name", name)
	return true
}
