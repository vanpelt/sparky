package envsync

// Triggering a sandbox's repo checkout from the outside.
//
// The env push and this one look alike and are not the same shape of thing. A
// secret environment is DATA the gateway holds and the guest cannot compute:
// the push carries the values. A repo checkout is the opposite — the guest
// already knows how to fetch its own manifest and mint its own credentials
// through the metadata service on its tap, and does exactly that at boot. The
// only thing missing when an owner retags a running box is the NUDGE, so that
// is all this sends: no manifest, no slug, no token, nothing that has to be
// kept in step with what the ledger says. The guest re-reads the truth itself.
//
// It rides envsync rather than getting a package of its own because what it
// needs is the guest exec channel — a dialer that reaches a sandbox on any
// machine in the fleet, and an upstream key to open it with — and that channel
// already lives here, wired once in cmd/sparkbox. A second copy would be a
// second thing to route, to test against the mock driver, and to get wrong.

import (
	"context"
	"errors"
	"fmt"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// ErrNoRepoSupport is host's, re-exported so a reader of this file can see what
// the guest's exit 3 becomes. It is a named error rather than a silent success
// because it is the exact confusion this feature shipped with — a box created
// before the payload existed accepts a tag, reports the tag, and then checks
// nothing out, which looks like a broken clone rather than an old machine.
var ErrNoRepoSupport = host.ErrNoRepoSupport

// repoSyncExitUnsupported is the guest's way of saying ErrNoRepoSupport. Chosen
// outside the range a shell hands back for its own failures (126/127 for exec
// problems, 128+n for signals) so it cannot be confused with the script dying.
const repoSyncExitUnsupported = 3

// repoSyncScript asks the guest to reconcile its checkouts and return
// immediately. Never synchronously: a clone of a large monorepo runs for
// minutes, and the operation that triggered this — `ctl tags set`, a `repo
// add` — is a control-plane call that must answer in the time a person waits
// for a prompt. The systemd unit is the one that owns the long half, with the
// timeout and the journal it already had for the boot pass.
//
// `restart` and not `start`: the unit is Type=oneshot RemainAfterExit=yes, so
// after the boot pass it is `active (exited)` and a start is a no-op. Restart
// re-runs it. The unit's own work is idempotent — a repository already checked
// out is reported present, not cloned again — so re-running it is safe however
// many times an owner edits their tags.
const repoSyncScript = `set -eu
[ -x /usr/local/sbin/sparkbox-repos ] || exit 3
if command -v systemctl >/dev/null 2>&1 &&
   systemctl restart --no-block sparkbox-repos.service 2>/dev/null; then
  exit 0
fi
# No systemd, or no unit: run it detached anyway. setsid so it survives this
# session closing, and every stream redirected so nothing keeps the channel
# open after the exec returns.
setsid /usr/local/sbin/sparkbox-repos sync </dev/null >/dev/null 2>&1 &
exit 0
`

// SyncRepos tells box's guest to reconcile its checkouts with the manifest its
// gateway holds, and returns as soon as the guest has accepted the job.
//
// Implements host.RepoSyncer. Paused, archived and quiesced sandboxes are
// skipped rather than woken, on the same rule the env push follows: waking
// somebody's machine is a bigger act than the one they asked for, and the boot
// pass checks out whatever is attached the next time it starts anyway.
func (s *Syncer) SyncRepos(ctx context.Context, box *host.Sandbox) error {
	if box == nil || box.State != vmm.StateRunning {
		return nil
	}
	st := s.boxState(box.Name)
	st.mu.Lock()
	quiesced := st.quiesced
	st.mu.Unlock()
	if quiesced {
		// Mid archive/snapshot. A clone landing between the pre-pack strip and
		// the pack would be packed into the image half-finished.
		return nil
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pushTimeout)
		defer cancel()
	}
	client, err := sshgw.DialUpstreamVia(ctx, s.dial, box.SSHAddr, box.SSHUser, s.upstreamKey)
	if err != nil {
		return fmt.Errorf("dial %s: %w", box.Name, err)
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	out, err := runScript(sess, s.shell, repoSyncScript)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("sync repos on %s: %w", box.Name, ctx.Err())
	}
	var exitErr *xssh.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitStatus() == repoSyncExitUnsupported {
		return fmt.Errorf("%w: %s", ErrNoRepoSupport, box.Name)
	}
	return fmt.Errorf("sync repos on %s: %w (%s)", box.Name, err, out)
}
