package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"
)

// snapNameRe bounds a user-facing snapshot name (the part a user types). The
// on-disk template it maps to is snap-<owner>-<name>, which the driver
// re-validates before touching the filesystem.
var snapNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// Snapshot is a fork-able template captured from a customized sandbox: the
// compacted, identity-stripped rootfs saved as an image the driver can reflink.
// New sandboxes fork it via Manager.Fork (Create with Image = the template).
type Snapshot struct {
	Name      string    `json:"name"`       // user-facing name, unique per owner
	Owner     string    `json:"owner"`      // who may fork/delete it
	Image     string    `json:"image"`      // template basename in the image dir (snap-<owner>-<name>)
	FromBox   string    `json:"from_box"`   // the sandbox it was taken from
	CreatedAt time.Time `json:"created_at"` //nolint:tagliatelle
}

// templateImage derives the reflink-able template name for an owner's snapshot.
func templateImage(owner, name string) string { return "snap-" + owner + "-" + name }

// Snapshotter reports whether snapshot/fork is available (needs a driver that
// implements vmm.Archivable).
func (m *Manager) Snapshotter() bool { return m.archiver != nil }

// Snapshot captures the named sandbox's current rootfs as a reusable template
// owned by `owner`, so `owner` can Fork fresh sandboxes from it. The sandbox is
// paused first (a consistent, unmounted rootfs), then the driver compacts +
// sanitizes it into the image dir. The heavy driver work runs without m.mu held.
func (m *Manager) Snapshot(ctx context.Context, box, snapName, owner string) (*Snapshot, error) {
	if !m.Snapshotter() {
		return nil, fmt.Errorf("snapshots are not supported by this driver")
	}
	if !snapNameRe.MatchString(snapName) {
		return nil, fmt.Errorf("invalid snapshot name %q (lowercase alphanumerics and dashes)", snapName)
	}
	image := templateImage(owner, snapName)

	m.mu.Lock()
	b, ok := m.boxes[box]
	if !ok || b.Owner != owner {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox %q not found", box)
	}
	if _, exists := m.snaps[image]; exists {
		m.mu.Unlock()
		return nil, fmt.Errorf("snapshot %q already exists", snapName)
	}
	m.mu.Unlock()

	// Every fork copies this rootfs byte-for-byte, so the managed secret block
	// must be cleared before capture — the fork's create-time push writes the
	// then-current set.
	if err := m.stripEnvForPack(ctx, box); err != nil {
		return nil, fmt.Errorf("snapshot %q: %w", snapName, err)
	}
	// Pause so the guest has flushed + unmounted its rootfs before we fsck/mount
	// it. Idempotent if already paused.
	if err := m.Pause(ctx, box); err != nil {
		return nil, fmt.Errorf("snapshot %q: pause %s: %w", snapName, box, err)
	}
	if err := m.archiver.Snapshot(ctx, box, image); err != nil {
		return nil, fmt.Errorf("snapshot %q: %w", snapName, err)
	}

	snap := &Snapshot{Name: snapName, Owner: owner, Image: image, FromBox: box, CreatedAt: time.Now().UTC()}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snaps[image] = snap
	m.log.Info("sandbox snapshot created", "snapshot", snapName, "owner", owner, "from", box, "image", image)
	if err := m.saveSnapshots(); err != nil {
		return nil, err
	}
	return copyOfSnap(snap), nil
}

// Fork creates a new sandbox from one of the owner's snapshots. It's a thin
// wrapper over Create with the snapshot's template as the image, so it inherits
// admission, routing, and front-door setup unchanged.
func (m *Manager) Fork(ctx context.Context, snapName, newName, owner string, vcpus, memMB int64) (*Sandbox, error) {
	image := templateImage(owner, snapName)
	m.mu.Lock()
	snap, ok := m.snaps[image]
	owns := ok && snap.Owner == owner
	m.mu.Unlock()
	if !owns {
		return nil, fmt.Errorf("snapshot %q not found", snapName)
	}
	return m.Create(ctx, newName, owner, image, vcpus, memMB)
}

// Snapshots lists an owner's snapshots, newest first.
func (m *Manager) Snapshots(owner string) []*Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Snapshot
	for _, s := range m.snaps {
		if s.Owner == owner {
			out = append(out, copyOfSnap(s))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// AllSnapshots lists every snapshot across owners, newest first — for the
// operator console (which acts across all owners, unlike owner-scoped ctl@).
func (m *Manager) AllSnapshots() []*Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Snapshot, 0, len(m.snaps))
	for _, s := range m.snaps {
		out = append(out, copyOfSnap(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// SnapshotByName returns an owner's snapshot record.
func (m *Manager) SnapshotByName(owner, name string) (*Snapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.snaps[templateImage(owner, name)]
	if !ok {
		return nil, false
	}
	return copyOfSnap(s), true
}

// DeleteSnapshot removes an owner's snapshot: the template file and its registry
// entry. Sandboxes already forked from it are unaffected (they hold their own
// reflink copy).
func (m *Manager) DeleteSnapshot(ctx context.Context, snapName, owner string) error {
	image := templateImage(owner, snapName)
	m.mu.Lock()
	snap, ok := m.snaps[image]
	if !ok || snap.Owner != owner {
		m.mu.Unlock()
		return fmt.Errorf("snapshot %q not found", snapName)
	}
	m.mu.Unlock()

	if m.archiver != nil {
		if err := m.archiver.RemoveTemplate(ctx, image); err != nil {
			return fmt.Errorf("delete snapshot %q: %w", snapName, err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.snaps, image)
	m.log.Info("snapshot deleted", "snapshot", snapName, "owner", owner)
	return m.saveSnapshots()
}

func (m *Manager) loadSnapshots() error {
	data, err := os.ReadFile(m.snapsPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &m.snaps)
}

// saveSnapshots persists the registry; callers must hold m.mu.
func (m *Manager) saveSnapshots() error {
	data, err := json.MarshalIndent(m.snaps, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.snapsPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.snapsPath)
}

func copyOfSnap(s *Snapshot) *Snapshot {
	c := *s
	return &c
}
