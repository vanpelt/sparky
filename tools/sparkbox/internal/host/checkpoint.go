package host

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

func (m *Manager) lockDiskOperation(name string) func() {
	value, _ := m.diskOps.LoadOrStore(name, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// CheckpointEnabled reports whether this host can create and restore durable
// rootfs checkpoints. A rebooter is required because PackRootfs discards the
// Firecracker memory snapshot; DropSnapshots also forgets the paused driver
// record so the preserved rootfs can cold-boot cleanly.
func (m *Manager) CheckpointEnabled() bool {
	_, canCheckPresence := m.driver.(vmm.RootfsPresencer)
	return m.checkpoint != nil && m.archiver != nil && m.rebooter != nil && canCheckPresence
}

func (m *Manager) checkpointKey(owner, sandboxID string) string {
	return path.Join(m.checkpointPfx, owner, sandboxID, uuid.NewString()+".ext4.zst")
}

// Checkpoint creates an immutable durable copy of a sandbox's rootfs while
// retaining its local disk. This deliberately reuses the conservative archive
// pack path: the guest stays paused through fsck, zeroing, compression, and
// upload, then cold-boots if it was running when the operation began.
//
// The record's pointer moves only after Put succeeds, so an interrupted upload
// leaves the previous committed checkpoint selected.
func (m *Manager) Checkpoint(ctx context.Context, name string) (retErr error) {
	unlock := m.lockDiskOperation(name)
	defer unlock()
	if !m.CheckpointEnabled() {
		return &DisabledError{Code: "checkpoint_disabled",
			Msg: "checkpointing is not enabled on this host"}
	}
	m.mu.Lock()
	b, ok := m.boxes[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q not found", name)
	}
	if b.State == vmm.StateArchived {
		m.mu.Unlock()
		return &StateError{Code: "sandbox_archived",
			Msg: fmt.Sprintf("sandbox %q is archived; restore it before checkpointing", name)}
	}
	owner, sandboxID := b.Owner, b.ID
	wasRunning := b.State == vmm.StateRunning
	m.mu.Unlock()

	if err := m.stripEnvForPack(ctx, name); err != nil {
		return fmt.Errorf("checkpoint %s: %w", name, err)
	}
	if err := m.revalidateCheckpointTarget(name, sandboxID, owner, "checkpoint"); err != nil {
		return err
	}
	if err := m.pause(ctx, name, "was paused for a durable checkpoint"); err != nil {
		return fmt.Errorf("checkpoint %s: pause: %w", name, err)
	}
	// Once paused, restore the caller's running state on every exit path. Use a
	// fresh bounded context because a canceled upload context should not leave a
	// previously-running sandbox down.
	defer func() {
		if !wasRunning {
			return
		}
		resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
		defer cancel()
		if _, err := m.ensureReady(resumeCtx, name); err != nil {
			retErr = errors.Join(retErr,
				fmt.Errorf("checkpoint %s: checkpoint finished but cold boot failed: %w", name, err))
		}
	}()

	packPath, packErr := m.archiver.PackRootfs(ctx, name)
	// Always forget the paused driver record after a pack attempt. PackRootfs
	// can remove the memory snapshot before compression fails; without this a
	// later Resume sees a paused record with no state.snap and Create then
	// refuses the still-registered name.
	dropErr := m.rebooter.DropSnapshots(name)
	if packPath != "" {
		defer os.Remove(packPath) //nolint:errcheck
	}
	if packErr != nil || dropErr != nil {
		return errors.Join(
			wrapCheckpointErr(name, "pack", packErr),
			wrapCheckpointErr(name, "drop memory snapshot", dropErr),
		)
	}

	key := m.checkpointKey(owner, sandboxID)
	if err := m.checkpoint.Put(ctx, key, packPath); err != nil {
		return fmt.Errorf("checkpoint %s: upload: %w", name, err)
	}

	m.mu.Lock()
	b, ok = m.boxes[name]
	if !ok || b.ID != sandboxID || b.Owner != owner {
		m.mu.Unlock()
		// The upload is immutable but no record can reference it. Best effort
		// cleanup avoids leaking an object when the sandbox was destroyed or
		// replaced while the long upload ran.
		_ = m.checkpoint.Delete(context.WithoutCancel(ctx), key)
		if !ok {
			return fmt.Errorf("sandbox %q not found", name)
		}
		return fmt.Errorf("sandbox %q changed identity during checkpoint", name)
	}
	previousKey, previousAt := b.CheckpointKey, b.CheckpointAt
	b.CheckpointKey = key
	b.CheckpointAt = time.Now().UTC()
	m.log.Info("sandbox checkpoint committed", "name", name, "key", key)
	m.observe(b, "checkpointed")
	err := m.save()
	if err != nil {
		b.CheckpointKey, b.CheckpointAt = previousKey, previousAt
	}
	m.mu.Unlock()
	if err != nil {
		_ = m.checkpoint.Delete(context.WithoutCancel(ctx), key)
		return fmt.Errorf("checkpoint %s: commit pointer: %w", name, err)
	}
	return nil
}

// RestoreCheckpoint replaces the local rootfs with the latest committed
// checkpoint. The durable object and its metadata pointer remain intact, so a
// restore is a copy rather than the move semantics used by archive restore.
func (m *Manager) RestoreCheckpoint(ctx context.Context, name string) (retErr error) {
	unlock := m.lockDiskOperation(name)
	defer unlock()
	if !m.CheckpointEnabled() {
		return &DisabledError{Code: "checkpoint_disabled",
			Msg: "checkpointing is not enabled on this host"}
	}
	m.mu.Lock()
	b, ok := m.boxes[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox %q not found", name)
	}
	if b.State == vmm.StateArchived {
		m.mu.Unlock()
		return &StateError{Code: "sandbox_archived",
			Msg: fmt.Sprintf("sandbox %q is archived; restore it before restoring a checkpoint", name)}
	}
	key, owner, sandboxID := b.CheckpointKey, b.Owner, b.ID
	wasRunning := b.State == vmm.StateRunning
	m.mu.Unlock()
	if key == "" {
		return fmt.Errorf("sandbox %q has no checkpoint", name)
	}

	tmp, err := os.CreateTemp(m.checkpointStageDir, "."+name+".checkpoint-restore-*.ext4.zst")
	if err != nil {
		return fmt.Errorf("restore checkpoint %s: staging: %w", name, err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath) //nolint:errcheck
		return fmt.Errorf("restore checkpoint %s: staging: %w", name, err)
	}
	os.Remove(tmpPath)       //nolint:errcheck // Get publishes the complete destination.
	defer os.Remove(tmpPath) //nolint:errcheck
	if err := m.checkpoint.Get(ctx, key, tmpPath); err != nil {
		return fmt.Errorf("restore checkpoint %s: download: %w", name, err)
	}

	if err := m.revalidateCheckpointTarget(name, sandboxID, owner, "restore checkpoint"); err != nil {
		return err
	}
	if err := m.pause(ctx, name, "was paused to restore a durable checkpoint"); err != nil {
		return fmt.Errorf("restore checkpoint %s: pause: %w", name, err)
	}
	if err := m.rebooter.DropSnapshots(name); err != nil {
		return fmt.Errorf("restore checkpoint %s: drop memory snapshot: %w", name, err)
	}
	if err := m.revalidateCheckpointTarget(name, sandboxID, owner, "restore checkpoint"); err != nil {
		return err
	}
	if err := m.archiver.UnpackRootfs(ctx, name, tmpPath); err != nil {
		return fmt.Errorf("restore checkpoint %s: unpack: %w", name, err)
	}

	m.mu.Lock()
	b, ok = m.boxes[name]
	if !ok || b.ID != sandboxID || b.Owner != owner || b.CheckpointKey != key {
		m.mu.Unlock()
		return fmt.Errorf("restore checkpoint %s: sandbox identity or checkpoint pointer changed", name)
	}
	b.State = vmm.StatePaused
	b.SSHAddr, b.HostIP, b.GuestV6 = "", "", ""
	m.log.Info("sandbox restored from checkpoint", "name", name, "key", key)
	m.observe(b, "checkpoint-restored")
	err = m.save()
	m.mu.Unlock()
	if err != nil {
		return fmt.Errorf("restore checkpoint %s: save state: %w", name, err)
	}
	if wasRunning {
		if _, err := m.ensureReady(ctx, name); err != nil {
			return fmt.Errorf("restore checkpoint %s: cold boot: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) revalidateCheckpointTarget(name, sandboxID, owner, op string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.boxes[name]
	if !ok || b.ID != sandboxID || b.Owner != owner {
		return fmt.Errorf("%s %s: sandbox identity changed during operation", op, name)
	}
	return nil
}

func wrapCheckpointErr(name, step string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("checkpoint %s: %s: %w", name, step, err)
}
