package envsync

// Starting an environment build's setup run inside the builder guest.
//
// Third of the family that rides this package's guest exec channel — after the
// repo nudge (repos.go) and the pre-capture tool refresh (tools.go) — and it is
// here for the reason both of those are: what it needs is a dialer that reaches
// a sandbox on any machine in the fleet plus an upstream key to open it with,
// and that channel is wired exactly once, in cmd/sparkbox. A second copy would
// be a second thing to route, to test against the mock driver, and to get
// wrong.
//
// It is the repo nudge's shape, not the tool refresh's, and deliberately so.
// Nothing about the job travels down this wire: no script, no environment name,
// no owner. The guest fetches its own work from the metadata service on its tap
// (GET /self/setup), runs it, and reports back (POST /self/setup/result), so the
// gateway decides everything — including whether this sandbox has a job at all —
// from the sandbox identity it reads off the tap rather than from anything the
// guest said. Sending the script here would put the same secret in two places
// and give a guest a second, weaker way to ask for one.

import (
	"context"
	"errors"
	"fmt"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// ErrNoEnvSetup is host's, re-exported so a reader of this file can see what
// the guest's exit 3 becomes — the courtesy repos.go does for ErrNoRepoSupport
// and tools.go for ErrNoToolRefresh, and for the same reason. A builder booted
// from a template that predates environment support would otherwise accept the
// nudge, report nothing, and sit in `building` until the build timeout, whose
// sentence ("the builder never reported") names the wrong cause entirely.
var ErrNoEnvSetup = host.ErrNoEnvSetup

// envSetupExitUnsupported is the guest's way of saying ErrNoEnvSetup. Chosen
// outside the range a shell hands back for its own failures (126/127 for exec
// problems, 128+n for signals) so it cannot be confused with the script dying,
// and the same 3 the two sibling nudges use so the guest payloads stay one
// convention.
const envSetupExitUnsupported = 3

// envSetupUnit is the guest oneshot that owns the long half of the build, and
// envSetupPath is the program it runs. A package var only so the tests can
// point it at something the mock driver's unprivileged /bin/sh can actually
// run, exactly as toolUpdaterPath is; neither is ever derived from a request,
// so both are interpolated into the script below without quoting.
const envSetupUnit = "sparkbox-env-setup.service"

var envSetupPath = "/usr/local/sbin/sparkbox-env-setup"

// envSetupScript asks the guest to start its setup run and return immediately.
// Never synchronously: a setup script installs a toolchain and builds a
// dependency tree, which runs for minutes, while the operation that triggered
// it — `ctl env build` — is a control-plane call that must answer in the time a
// person waits for a prompt. The systemd unit owns the long half, with the
// bounded TimeoutStartSec and the journal it already has for its boot pass, and
// the gateway learns the outcome from POST /self/setup/result rather than from
// this exec's exit status.
//
// `restart` and not `start`: the unit is Type=oneshot RemainAfterExit=yes, so
// once a pass has run it is `active (exited)` and a start is a no-op. Restart
// re-runs it, which is what a second `env build` of the same environment on a
// re-used builder needs.
func envSetupScript(program, unit string) string {
	return `set -eu
[ -x ` + program + ` ] || exit ` + fmt.Sprint(envSetupExitUnsupported) + `
if command -v systemctl >/dev/null 2>&1 &&
   systemctl restart --no-block ` + unit + ` 2>/dev/null; then
  exit 0
fi
# No systemd, or no unit: run it detached anyway. setsid so it survives this
# session closing, and every stream redirected so nothing keeps the channel
# open after the exec returns.
setsid ` + program + ` </dev/null >/dev/null 2>&1 &
exit 0
`
}

// StartSetup tells box's guest to run the environment setup its gateway is
// holding for it, and returns as soon as the guest has accepted the job.
//
// Paused, archived and quiesced sandboxes are skipped rather than woken, on the
// rule the env push and the repo nudge both follow: waking somebody's machine
// is a bigger act than the one they asked for. The two guards are not the same
// guard said twice.
//
//   - Not running: for a BUILDER this cannot normally happen (BuildEnvironment
//     creates the box and nudges it in the same breath), so the case that
//     reaches here is a reconciler sweeping a build whose builder was paused by
//     the idle reaper or a host restart. Resuming it from here would put an
//     unattended agent-shaped script back on somebody's credentials with nobody
//     watching; the sweep's own decision — resume deliberately, or fail the row
//     — belongs to the caller that knows the build's age.
//   - Quiesced: the box is mid archive/snapshot. A setup run landing between
//     the pre-pack strip and the pack would be packed into the image
//     half-finished, which is precisely the artifact every fork of that
//     template would then copy.
//
// A nil box is the same silent no-op rather than a panic: a caller holds a
// record read a moment earlier and a row can go away in between.
func (s *Syncer) StartSetup(ctx context.Context, box *host.Sandbox) error {
	if box == nil || box.State != vmm.StateRunning || box.SSHAddr == "" {
		return nil
	}
	st := s.boxState(box.Name)
	st.mu.Lock()
	quiesced := st.quiesced
	st.mu.Unlock()
	if quiesced {
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
	// The ctx bound must cover the exec and not just the dial: a guest whose
	// command never exits would otherwise block Run forever. Closing the client
	// tears down the transport, which unblocks Run.
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	out, err := runScript(sess, s.shell, envSetupScript(envSetupPath, envSetupUnit))
	if err == nil {
		s.log.Debug("started environment setup", "sandbox", box.Name)
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("start environment setup on %s: %w", box.Name, ctx.Err())
	}
	var exitErr *xssh.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitStatus() == envSetupExitUnsupported {
		return fmt.Errorf("%w: %s", ErrNoEnvSetup, box.Name)
	}
	return fmt.Errorf("start environment setup on %s: %w (%s)", box.Name, err, out)
}
