package fleet

// The secret environment of a sandbox on another machine.
//
// This is sidestores.go's rule applied to the one gateway-owned thing that is
// not a table: THE NODE HAS NO SECRETS STORE. cmd/sparkbox's node mode opens no
// secrets.Store and never calls Manager.SetEnvSync, so over there
// Manager.pushEnv returns immediately and the whole change-time and
// lifecycle-time propagation channel is a no-op. That is deliberate — an
// owner's decrypted secrets must not be sitting on every machine that happens
// to run one of their sandboxes, and the KEK is derived from a key only the
// gateway holds — but it means a sandbox placed on a node would receive NO
// secrets at all, ever, unless the gateway does the push itself.
//
// It can, because a push is just an SSH exec into the guest and the fleet
// dialer reaches a remote guest exactly as it reaches a local one. So the split
// is the same one sidestores.go draws: the gateway does the half a node cannot,
// and ONLY for a sandbox on another machine. A local sandbox's push stays the
// manager's, because doing it twice would mean two writers racing over one
// guest's /etc/environment.
//
// There are three moments a remote sandbox needs its environment, and they are
// not the same moment:
//
//  1. The gateway made it run — Create, EnsureRunning. Fired from the call
//     itself, not from the change event, because the caller is usually about to
//     connect and a delivery that waited on an event would be a race the user
//     loses. "Made it run" and not "asked it to run": EnsureRunning pushes only
//     when the sandbox was not already running, because it is called on every
//     proxied request, session, terminal attach and job, and on a box that is
//     already up it does nothing. See Fleet.EnsureRunning.
//  2. The MACHINE made it run — a node rebooting and resuming its pinned
//     sandboxes, a restore, a reaper's box being woken by something node-side.
//     The gateway learns about these only from sandbox.changed, so ApplyChanged
//     fires the push (see link.go). Without this a node restart silently strips
//     the secrets from every box it brings back.
//  3. The secrets or the tags changed — ResyncEnv, and the console's SyncOwner
//     fan-out. The fan-out is handled by giving the syncer the fleet as its
//     Lister rather than the local manager (cmd/sparkbox); ResyncEnv is handled
//     below.
//
// 1 and 2 overlap on the paths the gateway drives, and the duplicate push is
// accepted rather than engineered away: envsync serializes deliveries to one
// guest on a per-box mutex and each delivery rewrites the same block from the
// same store, so the second is a no-op that costs one SSH exec. Suppressing it
// would mean the gateway deciding which of its own events to believe, which is
// a great deal more machinery than an idempotent write.

import (
	"context"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// envPushBudget bounds one gateway-initiated push. It is host.Manager.pushEnv's
// own budget, restated: the call dials a guest across another machine's tunnel
// and must not be able to outlive the reason anyone wanted it.
const envPushBudget = 3 * time.Minute

// SetEnvPusher installs the secret-env push the gateway fires for sandboxes on
// other machines. It takes the same object host.Manager.SetEnvSync is given —
// cmd/sparkbox passes the one *envsync.Syncer to both — because there is one
// secrets store per deployment and one channel into a guest.
//
// Nil, the default, is every deployment that has not wired secrets at all, and
// a fleet with no pusher simply never pushes: the same thing a manager with no
// hook does.
func (f *Fleet) SetEnvPusher(p host.EnvPusher) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envPush = p
}

// pushEnv delivers b's owner's secret environment into b, if b is a running
// sandbox on another machine.
//
// b is re-served through the ledger before it is used, and that is the security
// half of this function rather than tidiness. Which secrets a sandbox receives
// is decided by its OWNER, and the record a node hands back carries the owner
// the node claims (remoteNode.record; EnsureRunning does not even overwrite it,
// because resume is not an ownership-changing operation). Pushing on that
// string would let a machine name any handle it liked and be sent that user's
// decrypted secrets. The ledger's owner column is the only authorization input
// anywhere else in this package, and it is the only one here.
//
// Everything else matches host.Manager.pushEnv exactly: a detached goroutine on
// a context that cannot be cancelled by the operation that triggered it, a hard
// budget, and a failure that is logged and never fatal. A sandbox is never
// failed over its environment — the next transition to running pushes again.
func (f *Fleet) pushEnv(ctx context.Context, b *host.Sandbox) {
	if b == nil || b.State != vmm.StateRunning {
		return
	}
	f.mu.RLock()
	p := f.envPush
	f.mu.RUnlock()
	if p == nil {
		return
	}
	row, ok := f.rowFor(b.Name)
	if !ok || row.Node == f.localName {
		// No row means the gateway did not place it and does not know whose it
		// is; a local row means the manager's own hook has it. Either way this
		// is not ours to push.
		return
	}
	box := f.serve(b, row, true)
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), envPushBudget)
		defer cancel()
		if err := p.PushEnv(ctx, box); err != nil {
			f.log.Warn("could not push the secret environment of a sandbox on another machine",
				"name", box.Name, "node", box.Node, "err", err)
		}
	}()
}

// pushEnvByName is pushEnv for the callers that hold a name rather than a
// record — the ones reacting to something that has already happened, where the
// current state is whatever the link's cache now says.
func (f *Fleet) pushEnvByName(ctx context.Context, name string) {
	if b, ok := f.Get(name); ok {
		f.pushEnv(ctx, b)
	}
}
