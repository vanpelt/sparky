package fleet

// Refreshing the agent CLIs in a sandbox on another machine, before it is
// captured as a template.
//
// This is envsync.go's split applied to one more thing a node cannot do for
// itself, and here the reason is structural rather than a matter of which table
// lives where: a node caches the gateway's upstream PUBLIC key so it can
// AUTHENTICATE the gateway's connections into its guests. It holds no signer,
// so it cannot open a session into one. cmd/sparkbox's node mode therefore
// never calls Manager.SetToolSync, host.Manager.refreshToolsForPack finds a nil
// hook over there, and a template captured on a node would freeze whatever tool
// versions that sandbox happened to have — for every fork anybody ever makes of
// it — unless the gateway does the refresh itself.
//
// It can, because the refresh is an SSH exec and the fleet dialer reaches a
// remote guest exactly as it reaches a local one. The split is the usual one:
// the gateway does the half a node cannot, and ONLY for a sandbox on another
// machine, because two installers racing over one guest's /usr/local/bin is a
// worse failure than the one this fixes.

import (
	"context"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// toolRefreshBudget bounds the remote refresh, and it has to be stated HERE
// rather than inherited from the caller's context.
//
// host.Manager.refreshToolsForPack wraps its own call in the same 5 minutes, so
// the local path is capped. The remote path is not, and the difference is not
// academic: envsync.RefreshTools only installs its own 10-minute fallback when
// the context carries NO deadline, and this one always does — ctlops
// .CreateSnapshot opens the whole capture on ArchiveTimeout, 15 minutes. So an
// unbudgeted remote refresh can spend every one of those minutes and leave
// nothing for the e2fsck + zerofree + reflink of a 25 GiB rootfs that is the
// actual point of the operation, and the symptom reads as "snapshots started
// timing out", which is the wrong end to pull on.
//
// Same number as the manager's on purpose: the two paths differ in who runs the
// refresh, not in how long a sandbox is worth waiting for.
const toolRefreshBudget = 5 * time.Minute

// SetToolSync installs the pre-capture tool refresh the gateway fires for
// sandboxes on other machines. cmd/sparkbox passes the one *envsync.Syncer here
// and to Manager.SetToolSync both, exactly as it does for the env push and the
// checkout nudge: one guest exec channel, wired once, reaching every machine in
// the fleet.
//
// Nil — the default, and every deployment that wired no syncer — simply never
// refreshes, which is what a manager with no hook does too.
func (f *Fleet) SetToolSync(t host.ToolRefresher) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.toolSync = t
}

// refreshToolsBefore installs this fleet's cached agent CLIs into a sandbox on
// another machine, so the template about to be captured there starts current
// instead of frozen at whatever versions that guest happens to hold.
//
// Called from Fleet.Snapshot at the last moment the guest is still up: the node
// pauses it the instant the capture begins. Synchronous for the reason
// host.Manager.refreshToolsForPack documents — a pause landing mid-install
// freezes a truncated executable into a disk every fork copies byte-for-byte —
// and best-effort, because a failed tool download is never a reason to refuse
// somebody a template.
//
// Two early returns, both deliberate:
//
//   - The local machine. Its manager runs this itself, from inside its disk
//     lock and after the pre-pack strip has safely woken the guest — neither of
//     which this side can do. Doing it here as well would put two installers on
//     one /usr/local/bin.
//   - A remote sandbox that is not running. Nothing here wakes one, and after
//     stripEnvBefore nothing needs to: that step runs first, wakes a paused
//     guest to clear its secrets, and leaves it running for this one. So
//     reaching here on a paused box means the strip decided there was nothing
//     to strip — a deployment with no secrets wired at all — and the box is
//     captured with whatever tools it has. Waking it here instead would mean
//     two callers deciding independently to resume the same guest.
func (f *Fleet) refreshToolsBefore(ctx context.Context, n Node, name string) {
	if n == nil || n.Name() == f.localName {
		return
	}
	f.mu.RLock()
	t := f.toolSync
	f.mu.RUnlock()
	if t == nil {
		return
	}
	b, ok := f.Get(name)
	if !ok || b.State != vmm.StateRunning {
		return
	}
	row, ok := f.rowFor(name)
	if !ok {
		return
	}
	// serve for the reason every other remote call in this package serves: the
	// addresses a node reports are its own and the owner it reports is its
	// claim, while the dialer needs the synthetic fleet address and the ledger
	// is the only owner this package acts on.
	//
	// The neighbouring hole this hook was once only the SHAPE of a fix for — a
	// remote pack keeping its plaintext secrets, because a node never calls
	// SetEnvSync — is closed in strip.go, which runs immediately before this.
	// WithTimeout takes the earlier of the two deadlines, so a caller already on
	// a tighter budget still wins; see toolRefreshBudget for why inheriting the
	// caller's is not good enough.
	ctx, cancel := context.WithTimeout(ctx, toolRefreshBudget)
	defer cancel()
	if err := t.RefreshTools(ctx, f.serve(b, row, true)); err != nil {
		f.log.Warn("could not refresh the agent tools of a sandbox on another machine before capturing it; capturing what it has",
			"name", name, "node", row.Node, "err", err)
	}
}
