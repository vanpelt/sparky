// Package envsync pushes an owner's secret environment (internal/secrets)
// into their sandboxes' /etc/environment over SSH — the single propagation
// channel for tag and secret changes. The env file lives in the rootfs, and
// the rootfs survives every lifecycle transition (pause/resume, archive/
// restore, host-restart recreate, forks), so change-time pushes to running
// boxes plus the manager's push-on-Create/EnsureRunning hook cover everything
// with no boot-time channel of their own.
//
// Only a sentinel-delimited block (BlockBegin/BlockEnd) is ever rewritten, so
// the toolchain PATH the image bakes into /etc/environment survives. The
// rewrite script — and, nested inside it, the block itself — travels
// base64-encoded, so no secret value ever meets a shell-quoting context. The
// push dials the guest's sshd directly with the fleet upstream key, never via
// EnsureRunning, so a pushing goroutine can never wake a paused box.
//
// Deliveries to one guest are serialized on a per-box mutex, and the env
// snapshot is read under it, so the last delivery to a box always carries the
// newest store state. StripEnv additionally quiesces the box: change-time
// pushes skip it until the manager's next lifecycle PushEnv (fired only on a
// transition to running, i.e. after any pack window has closed), so an
// in-flight push can never rewrite secrets into a rootfs between the pre-pack
// strip and the pack itself.
package envsync

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/sshgw"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/vmm"
)

// BlockBegin and BlockEnd delimit the managed block in /etc/environment.
// Everything between them is sparkbox's to rewrite; everything outside is
// never touched. The image-hygiene pass (packed archives and snapshot
// templates) strips the same markers, so they are part of the contract, not
// an implementation detail.
const (
	BlockBegin = "# --- sparkbox-managed secrets (do not edit below) ---"
	BlockEnd   = "# --- end sparkbox-managed ---"
)

// pushTimeout bounds a single push when the caller's context carries no
// deadline: DialUpstream retries until the context expires, and a hook fired
// with context.Background must not spin forever on a wedged guest. Sized like
// the gateway's pause budget — generous for one SSH exec, tiny for a leak.
const pushTimeout = 3 * time.Minute

// maxExecOutput caps how much guest stdout/stderr a delivery retains. The
// bytes are guest-controlled and only ever quoted in an error message, so an
// uncapped read would let a hostile guest balloon the control plane's memory.
const maxExecOutput = 4096

// reservedEnvNames are never delivered regardless of what the store holds: a
// pushed PATH overrides sshd's default via pam_env and breaks the push
// channel itself (the delivery pipeline's command lookup), and LD_PRELOAD /
// LD_LIBRARY_PATH poison every process in every guest session. The store is
// expected to reject these at write time; dropping them here is defense in
// depth so one bad row cannot brick a box's push channel.
var reservedEnvNames = map[string]bool{
	"PATH":            true,
	"LD_PRELOAD":      true,
	"LD_LIBRARY_PATH": true,
}

// Lister is the one manager method the syncer drives: the fan-out in
// SyncOwner. It is an interface rather than *host.Manager so a fleet router
// can stand in and an owner's sandboxes on other machines get their secrets
// too; *host.Manager satisfies it, so a single-box deployment is unchanged.
type Lister interface {
	List() []*host.Sandbox
}

// Syncer delivers secret environments into guests. It implements
// host.EnvPusher, so the manager fires it after Create and EnsureRunning;
// the console fires SyncOwner after tag/secret mutations.
type Syncer struct {
	store       *secrets.Store
	mgr         Lister
	upstreamKey xssh.Signer
	dial        sshgw.Dialer // nil dials the guest over the host network
	log         *slog.Logger
	wg          sync.WaitGroup // in-flight SyncOwner pushes (tests wait on it)

	mu    sync.Mutex           // guards boxes
	boxes map[string]*boxState // per-box delivery state; entries are never removed

	// Test seams: the mock driver's fake VMs run /bin/sh unprivileged with
	// cwd = the per-sandbox workdir, so tests point envPath at a relative
	// file and drop the sudo.
	envPath string // guest path of the env file
	shell   string // guest command the decoded rewrite script is piped into
}

// boxState serializes deliveries to one guest. quiesced is set by StripEnv
// and cleared by the manager's lifecycle PushEnv; while set, change-time
// (SyncOwner) pushes skip the box so nothing can rewrite secrets between the
// pre-pack strip and the pack.
type boxState struct {
	mu       sync.Mutex
	quiesced bool
}

var (
	_ host.EnvPusher   = (*Syncer)(nil)
	_ host.EnvStripper = (*Syncer)(nil)
)

func New(store *secrets.Store, mgr Lister, upstreamKey xssh.Signer, log *slog.Logger) *Syncer {
	return &Syncer{
		store: store, mgr: mgr, upstreamKey: upstreamKey, log: log,
		boxes: make(map[string]*boxState),
		// Absolute paths so the delivery works even if a poisoned PATH ever
		// reaches pam_env; the inner script runs under sudo's secure_path.
		envPath: "/etc/environment",
		shell:   "/usr/bin/sudo -n /bin/sh", // login user has NOPASSWD sudo; -n fails loud instead of hanging
	}
}

// SetDialer routes delivery through d instead of a plain TCP dial to the
// guest, so a sandbox on another machine still receives its environment. It is
// post-construction, matching the mgr.SetEnvSync idiom on the other side of
// the same wiring, because the syncer is built before the thing that dials for
// it. Call it before the first push; nothing here is guarded, since main wires
// it once at startup.
func (s *Syncer) SetDialer(d sshgw.Dialer) { s.dial = d }

// boxState returns name's delivery state, creating it on first use.
func (s *Syncer) boxState(name string) *boxState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.boxes[name]
	if !ok {
		st = &boxState{}
		s.boxes[name] = st
	}
	return st
}

// PushEnv rewrites box's managed /etc/environment block to the owner's
// current secret set. It never wakes a box: anything not running is skipped
// (the manager's push-on-EnsureRunning hook reconciles it on next resume),
// and the dial goes straight to the guest's sshd with the upstream key. An
// empty secret set still writes an empty block — tag removal must remove
// secrets. On ErrUndecryptable nothing is pushed at all: a partial or empty
// environment must never masquerade as the real one. On ErrNotEnabled
// (keycheck failed after a key rotation) the guest keeps its last-known-good
// values rather than having them erased.
//
// Only the manager calls PushEnv, and only on a transition to running
// (create, resume, restore, recreate, fork) — the box is out of any pack
// window by then, so a strip-time quiesce lifts here.
func (s *Syncer) PushEnv(ctx context.Context, box *host.Sandbox) error {
	if box.State != vmm.StateRunning || box.SSHAddr == "" {
		return nil
	}
	st := s.boxState(box.Name)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.quiesced = false
	return s.pushEnvLocked(ctx, box)
}

// pushEnvLocked reads the store and delivers. The caller holds box's per-box
// mutex, so the env snapshot and the delivery are one atomic step: serialized
// deliveries each read the store after all earlier ones for the box complete,
// making the last delivered block always reflect the newest store state.
func (s *Syncer) pushEnvLocked(ctx context.Context, box *host.Sandbox) error {
	env, err := s.store.EnvForSandbox(box.Name, box.Owner)
	if err != nil {
		if errors.Is(err, secrets.ErrNotEnabled) {
			return nil
		}
		return fmt.Errorf("env for %s: %w", box.Name, err)
	}
	env, err = s.sanitizeEnv(box.Name, env)
	if err != nil {
		return err
	}
	if err := s.deliverBlock(ctx, box, renderBlock(env), false); err != nil {
		return err
	}
	s.log.Debug("pushed env", "sandbox", box.Name, "vars", len(env))
	return nil
}

// sanitizeEnv is the render-time gate on what reaches the guest. Reserved
// names are dropped and logged, never fatal — the rest of the env must still
// flow. Values containing '#' fail the whole push: pam_env truncates an
// /etc/environment line at the first '#' before any quote handling, so such a
// value cannot be delivered faithfully, and — like ErrUndecryptable — a
// silently truncated environment must never masquerade as the real one.
func (s *Syncer) sanitizeEnv(boxName string, env map[string]string) (map[string]string, error) {
	for name, value := range env {
		if reservedEnvNames[name] {
			delete(env, name)
			s.log.Warn("refusing to push reserved env name", "sandbox", boxName, "name", name)
			continue
		}
		if strings.Contains(value, "#") {
			return nil, fmt.Errorf("secret %s for %s contains '#', which pam_env treats as a comment even inside quotes; not pushing", name, boxName)
		}
	}
	return env, nil
}

// StripEnv rewrites box's managed block to empty. It is the pre-pack hygiene
// pass (host.EnvStripper) that Archive and Snapshot fire before handing the
// rootfs to the driver's pack, so a packed artifact never carries plaintext
// secret values. Unlike PushEnv it never consults the store — the block must
// come out even (especially) when the store is undecryptable or disabled —
// and an unreachable box is an error rather than a skip: the manager only
// calls it with a running box copy, and a silent skip here would let an
// uncleared rootfs into object storage. The strip runs strict: if the guest
// file's markers are unbalanced in a way the rewrite cannot prove clean, it
// fails, and so does the pack.
//
// A successful strip quiesces the box (see boxState) so a change-time push
// cannot rewrite secrets before the pack; a failed strip lifts the quiesce
// again, since the caller aborts the pack.
func (s *Syncer) StripEnv(ctx context.Context, box *host.Sandbox) error {
	if box.State != vmm.StateRunning || box.SSHAddr == "" {
		return fmt.Errorf("sandbox %s not reachable for env strip (state %s)", box.Name, box.State)
	}
	st := s.boxState(box.Name)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.quiesced = true
	if err := s.deliverBlock(ctx, box, renderBlock(nil), true); err != nil {
		st.quiesced = false
		return err
	}
	s.log.Debug("stripped env", "sandbox", box.Name)
	return nil
}

// cappedWriter retains at most max bytes; overflow is reported as written and
// dropped so the SSH channel never backpressures on a hostile guest.
type cappedWriter struct {
	buf []byte
	max int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if room := w.max - len(w.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.buf = append(w.buf, p[:room]...)
	}
	return len(p), nil
}

// deliverBlock runs the rewrite script on box's guest over SSH, replacing the
// managed block with block. Shared delivery channel for PushEnv and StripEnv;
// callers hold the box's per-box mutex.
func (s *Syncer) deliverBlock(ctx context.Context, box *host.Sandbox, block string, strict bool) error {
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
	// The ctx bound must cover the exec, not just the dial: a guest whose
	// command never exits would otherwise block Run forever. Closing the
	// client tears down the transport, which unblocks Run.
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()

	script := rewriteScript(s.envPath, block, strict)
	// printf is a shell builtin and the decoder path is absolute, so the
	// outer pipeline works even under a guest-poisoned PATH.
	cmd := fmt.Sprintf("printf '%%s' %s | /usr/bin/base64 -d | %s",
		base64.StdEncoding.EncodeToString([]byte(script)), s.shell)
	out := &cappedWriter{max: maxExecOutput}
	sess.Stdout, sess.Stderr = out, out
	if err := sess.Run(cmd); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("rewrite %s on %s: %w", s.envPath, box.Name, ctx.Err())
		}
		return fmt.Errorf("rewrite %s on %s: %w (%s)", s.envPath, box.Name, err, strings.TrimSpace(string(out.buf)))
	}
	return nil
}

// SyncOwner re-pushes the owner's environment after a tag or secret change.
// Running boxes get an async push bounded by pushTimeout; paused and archived
// boxes are skipped — never woken — because the manager's
// push-on-EnsureRunning hook catches them up on next resume. A box pausing
// mid-fanout misses this push, and a quiesced box (mid archive/snapshot)
// deliberately skips it; both are corrected the same way: eventual
// consistency with a one-cycle window.
func (s *Syncer) SyncOwner(ctx context.Context, owner string) {
	for _, box := range s.mgr.List() {
		if box.Owner != owner || box.State != vmm.StateRunning {
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			st := s.boxState(box.Name)
			st.mu.Lock()
			defer st.mu.Unlock()
			if st.quiesced {
				return
			}
			// The caller's ctx is typically an HTTP request about to return;
			// the push must outlive it, bounded by its own deadline — whose
			// clock starts after the lock, so a queued push isn't consumed
			// by waiting on an earlier delivery.
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
			defer cancel()
			if err := s.pushEnvLocked(pctx, box); err != nil {
				s.log.Warn("env push failed", "sandbox", box.Name, "err", err)
			}
		}()
	}
}

// renderBlock renders the managed block: BlockBegin, one NAME="value" line
// per secret in sorted order, BlockEnd. Values are written verbatim inside
// double quotes — pam_env strips one surrounding quote pair and does no
// escape processing, and the store plus sanitizeEnv already reject everything
// pam_env would corrupt (newlines, NULs, '#') — so no escaping is needed
// (and any would be wrong). An empty env still renders the marker pair.
func renderBlock(env map[string]string) string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(BlockBegin + "\n")
	for _, name := range names {
		b.WriteString(name + `="` + env[name] + `"` + "\n")
	}
	b.WriteString(BlockEnd + "\n")
	return b.String()
}

// rewriteScript builds the POSIX-sh script that swaps the managed block in
// envPath. The new block rides inside it base64-encoded — base64's alphabet
// is shell-inert, so hostile values ($(...), quotes, backslashes) never meet
// a quoting context on either hop. awk strips every marker pair; a begin
// marker with no end (a hand-edited file) strips to EOF — over-removal beats
// leaking a secret line whose end marker was deleted, and the file heals: the
// appended fresh block leaves it balanced. An end marker with no begin is
// ambiguous — the lines before it may be orphaned secret values — so it is
// kept as-is (push mode: the file neither grows unboundedly nor loses user
// lines) or, in strict mode (StripEnv), fails the script with exit 3: a strip
// that cannot prove the block is gone must fail so the pack aborts. The
// rewrite is tmp-write + rename, never in-place; a trap removes the tmp file
// on any failure and a sweep clears strays from crashed past runs.
//
// The file lands 0644, root:root when run under sudo. pam_env reads it as
// root during session setup (sshd's privileged monitor runs the PAM session
// stack), so 0600 would work for ssh — but stock Ubuntu ships
// /etc/environment 0644, images/Dockerfile bakes it 0644 with no sshd/PAM
// config of its own, and the single guest login user has passwordless sudo,
// so 0600 would hide nothing while risking any non-root reader. Keep the
// stock mode.
func rewriteScript(envPath, block string, strict bool) string {
	strictFlag := 0
	if strict {
		strictFlag = 1
	}
	return fmt.Sprintf(`set -eu
f='%s'
tmp="$f.sparkbox.$$"
trap 'rm -f "$tmp"' EXIT HUP INT TERM
rm -f "$f".sparkbox.* 2>/dev/null || true
umask 022
if [ -f "$f" ]; then
	awk -v b='%s' -v e='%s' -v strict=%d '
		$0 == b { inb = 1; next }
		$0 == e && inb { inb = 0; next }
		$0 == e { bad = 1 }
		!inb { print }
		END { if (strict && bad) { print "unbalanced sparkbox markers" | "cat >&2"; exit 3 } }
	' "$f" > "$tmp"
else
	: > "$tmp"
fi
printf '%%s' %s | /usr/bin/base64 -d >> "$tmp"
chmod 0644 "$tmp"
mv "$tmp" "$f"
`, envPath, BlockBegin, BlockEnd, strictFlag, base64.StdEncoding.EncodeToString([]byte(block)))
}
