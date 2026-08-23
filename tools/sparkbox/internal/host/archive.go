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
