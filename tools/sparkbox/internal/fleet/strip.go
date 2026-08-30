package fleet

// Clearing the managed secret block from a sandbox on another machine, before
// that machine packs its rootfs into something durable.
//
// This is the third and sharpest instance of envsync.go's split. THE NODE HAS
// NO SECRETS STORE, so `host.Manager.stripEnvForPack` — which finds its
// rewriter by type-asserting `m.envSync` — finds nothing over there, returns
// nil, and the pack proceeds with plaintext secret values still in the guest's
// /etc/environment. Every fork of the resulting template copies them, and an
// archive carries them into object storage.
//
// Unlike the push (which a node merely never does, so a remote sandbox would
// get no secrets and somebody would notice within a minute) this one fails
// SILENTLY and in the safe-looking direction. Nothing is missing afterwards.
// The disk simply has more in it than anybody meant.
//
// It matters most exactly where it is least visible: on CKS the gateway is
// control-plane-only and every VM lives on the node, so every capture and every
// archive is a remote one. There is no local path there to be correct instead.

import (
	"context"
	"fmt"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// envStripBudget bounds the rewrite itself — not the wake that may precede it,
// which is a cold boot on another machine and has no business being measured
// against a file edit.
//
// Stated here for the reason toolRefreshBudget is: envsync.deliverBlock installs
// its own pushTimeout only when the context carries NO deadline, and a caller
// packing a rootfs always carries one (ctlops opens the whole capture on
// ArchiveTimeout, 15 minutes). Inheriting that would let one unreachable guest
// spend the entire budget of an operation whose real work has not started.
const envStripBudget = 3 * time.Minute

// beginPack marks name as being packed and returns the release.
//
// While it is set, the gateway will not push secrets into that sandbox. That is
// not tidiness: the strip below usually has to WAKE the guest first, the node
// announces the wake as a sandbox.changed, and the gateway's own ApplyChanged
// turns that announcement into a push (see envsync.go, moment 2). Without this
// gate the push lands between the strip and the capture and refills the very
// block the strip just cleared — and the pack that follows is exactly as
// compromised as if the strip had never run, with a log line saying it did.
//
// The existing envsync `quiesced` flag does not cover this. It stops
// CHANGE-time pushes (SyncOwner), while PushEnv — the lifecycle push, which is
// what ApplyChanged fires — clears it on purpose.
//
// A counter rather than a flag because Archive and Snapshot both use it and a
// second pack of the same box must not release the first one's hold.
func (f *Fleet) beginPack(name string) func() {
	f.mu.Lock()
	if f.packs == nil {
		f.packs = make(map[string]int)
	}
	f.packs[name]++
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.packs[name] <= 1 {
			delete(f.packs, name)
			return
		}
		f.packs[name]--
	}
}

// packing reports whether a pack is holding name against lifecycle pushes.
func (f *Fleet) packing(name string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, held := f.packs[name]
	return held
}

// stripEnvBefore clears the managed secret block from a sandbox on another
// machine so the rootfs that machine is about to pack carries no plaintext
// secret values.
//
// Called from Fleet.Snapshot and Fleet.Archive, inside a pack hold, before
// anything else touches the guest. Three deliberate differences from
// refreshToolsBefore, which sits beside it and looks similar:
//
//   - It RETURNS ITS ERROR, and both callers abort on it. A failed tool refresh
//     costs a slightly stale template; a failed strip costs credentials in a
//     disk every fork copies byte-for-byte. host.Manager.stripEnvForPack fails
//     the caller rather than packing an uncleared disk, and the machine a
//     sandbox happens to sit on must not change that answer.
//   - It WAKES a paused guest, because a sleeping box cannot be rewritten and
//     archiving a paused sandbox is the ordinary case, not the exception.
//   - It wakes it through the NODE (EnsureReady) rather than through
//     Fleet.EnsureRunning, whose whole job is to push the environment on a box
//     it made run — which would race the strip it is about to perform. This is
//     the fleet's version of the local path's resumeOrRecreate, and it is safe
//     over there for the reason this file opens with: the node's own push hook
//     is nil, so a node-side wake pushes nothing.
//
// A wake this function performed is undone on failure, so a refused pack leaves
// the sandbox as it found it.
func (f *Fleet) stripEnvBefore(ctx context.Context, n Node, name string) error {
	if n == nil || n.Name() == f.localName {
		// The manager does its own, inside its disk lock and with the wake it
		// alone can perform. Doing it here as well would put two writers on one
		// guest's /etc/environment.
		return nil
	}
	f.mu.RLock()
	p := f.envPush
	f.mu.RUnlock()
	stripper, ok := p.(host.EnvStripper)
	if !ok {
		// Every deployment that wired no secrets at all. Nothing was ever
		// delivered into this guest, so there is nothing to take back out —
		// the same answer a manager with no hook gives.
		return nil
	}

	b, ok := f.Get(name)
	if !ok {
		return fmt.Errorf("sandbox %q not found", name)
	}
	if b.State == vmm.StateArchived {
		// No local rootfs to strip or to pack; callers refuse archived boxes
		// before reaching here. Matches stripEnvForPack's wording.
		return fmt.Errorf("sandbox %q is archived", name)
	}
	woke := false
	if b.State != vmm.StateRunning {
		if _, err := n.EnsureReady(ctx, name); err != nil {
			return fmt.Errorf("wake %s to strip secret env: %w", name, err)
		}
		woke = true
		if b, ok = f.Get(name); !ok {
			return fmt.Errorf("sandbox %q not found", name)
		}
	}
	row, ok := f.rowFor(name)
	if !ok {
		return fmt.Errorf("sandbox %q has no placement row", name)
	}

	// WithTimeout keeps the earlier of the two deadlines, so a caller already on
	// a tighter budget still wins; see envStripBudget.
	stripCtx, cancel := context.WithTimeout(ctx, envStripBudget)
	defer cancel()
	// serve for the reason every other remote call in this package serves: the
	// addresses a node reports are its own, the dialer needs the synthetic fleet
	// address, and the ledger's owner is the only owner this package acts on.
	if err := stripper.StripEnv(stripCtx, f.serve(b, row, true)); err != nil {
		if woke {
			// The pack is about to be refused, so leave the sandbox as it was
			// rather than parked awake on somebody's node. Best-effort: the
			// error that matters is the strip's.
			if perr := n.Pause(context.WithoutCancel(ctx), name); perr != nil {
				f.log.Warn("could not re-pause a sandbox woken for a strip that then failed",
					"name", name, "node", row.Node, "err", perr)
			}
		}
		return fmt.Errorf("clear the secret environment of %s before packing it: %w", name, err)
	}
	return nil
}
