package sshgw

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	gssh "github.com/gliderlabs/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// handleControl serves the `ctl@` out-of-band channel: managing sandboxes and
// your own account without dialing into a VM. It only ever touches the
// caller's own sandboxes and keys.
//
// Every command here is parse → call ctlops → format. The ownership check, the
// timeout budget and the error taxonomy live in ctlops so that the REST API and
// the browser terminal make the same decisions this channel does; what stays is
// the argument grammar ssh(1) forces on us and the exact sentences this channel
// has always printed.
func (g *Gateway) handleControl(s gssh.Session, user string, log *slog.Logger) {
	args := s.Command()
	c := caller(s, user)
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), controlHelp(g.isOperator(s, c)))
		s.Exit(2) //nolint:errcheck
		return
	}
	switch args[0] {
	// `ls` is what the help documents; `list` is what shipped, what the docs
	// and other people's scripts say, and what the REST path is called. Both,
	// forever — the alias costs a word and an alias nobody has to remember is
	// the whole point.
	case "ls", "list":
		boxes, err := g.ops.List(s.Context(), c)
		if err != nil {
			failCtl(s, log, "list", err)
			return
		}
		for _, b := range boxes {
			tier := "scale-to-zero"
			if b.Pinned {
				tier = "pinned"
			}
			fmt.Fprintf(s, "%-24s %-8s %s\r\n", b.Name, b.State, tier)
		}
		if len(boxes) == 0 {
			fmt.Fprint(s, "no sandboxes yet — create one with: ssh new@<gateway>\r\n")
		}
		s.Exit(0) //nolint:errcheck
	case "pause":
		name, ok := g.ownedBoxArg(s, c, args, log)
		if !ok {
			return
		}
		if _, err := g.ops.Pause(s.Context(), c, name); err != nil {
			failCtl(s, log, "pause", err)
			return
		}
		fmt.Fprintf(s, "paused %s\r\n", name)
		s.Exit(0) //nolint:errcheck
	case "archive":
		name, ok := g.ownedBoxArg(s, c, args, log)
		if !ok {
			return
		}
		fmt.Fprintf(s, "archiving %s (fsck + compress + upload; this can take a minute)…\r\n", name)
		// A host with no object storage used to be the manager's error and so was
		// printed through fail()'s wrapper; ctlops raises it as a typed
		// KindDisabled, which would otherwise reword a shipped sentence.
		if _, err := g.ops.Archive(s.Context(), c, name); err != nil {
			failCtl(s, log, "archive", wrapVerbatim(err, ctlops.KindDisabled))
			return
		}
		fmt.Fprintf(s, "archived %s — host disk freed; `restore %s` (or just connect) brings it back\r\n", name, name)
		s.Exit(0) //nolint:errcheck
	case "restore":
		name, ok := g.ownedBoxArg(s, c, args, log)
		if !ok {
			return
		}
		fmt.Fprintf(s, "restoring %s (download + boot)…\r\n", name)
		if _, err := g.ops.Resume(s.Context(), c, name); err != nil {
			failCtl(s, log, "restore", err)
			return
		}
		fmt.Fprintf(s, "restored %s — it's running\r\n", name)
		s.Exit(0) //nolint:errcheck
	case "checkpoint":
		restore := len(args) >= 2 && args[1] == "restore"
		if (restore && len(args) != 3) || (!restore && len(args) != 2) {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> checkpoint <name>\r\n"+
				"       ssh %s@<gateway> checkpoint restore <name>\r\n", ControlUser, ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		name := args[1]
		op := "checkpoint"
		if restore {
			name = args[2]
			op = "checkpoint restore"
		}
		if restore {
			fmt.Fprintf(s, "restoring %s from its latest checkpoint (local disk will be replaced)…\r\n", name)
			if _, err := g.ops.RestoreCheckpoint(s.Context(), c, name); err != nil {
				failCtl(s, log, op, wrapVerbatim(err, ctlops.KindDisabled))
				return
			}
			fmt.Fprintf(s, "restored %s from its latest checkpoint\r\n", name)
		} else {
			fmt.Fprintf(s, "checkpointing %s (pause + fsck + compress + copy; this can take a minute)…\r\n", name)
			if _, err := g.ops.Checkpoint(s.Context(), c, name); err != nil {
				failCtl(s, log, op, wrapVerbatim(err, ctlops.KindDisabled))
				return
			}
			fmt.Fprintf(s, "checkpointed %s — the local disk was retained\r\n", name)
		}
		s.Exit(0) //nolint:errcheck
	case "resize":
		name, ok := g.ownedBoxArg(s, c, args, log)
		if !ok {
			return
		}
		if len(args) < 3 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> resize <name> <size>   e.g. 25G\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		sizeMB, err := parseSizeMB(args[2])
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(2) //nolint:errcheck
			return
		}
		// Resizing cold-boots the sandbox, so say so before the session goes
		// quiet for the fsck — and warn that in-guest processes do not survive.
		fmt.Fprintf(s, "resizing %s to %d MB (pause + cold boot; running processes restart)…\r\n", name, sizeMB)
		if _, err := g.ops.Resize(s.Context(), c, name, sizeMB); err != nil {
			failCtl(s, log, "resize", err)
			return
		}
		fmt.Fprintf(s, "resized %s — it's running again with a %d MB disk\r\n", name, sizeMB)
		s.Exit(0) //nolint:errcheck
	case "rm", "remove", "destroy":
		name, ok := g.ownedBoxArg(s, c, args, log)
		if !ok {
			return
		}
		if err := g.ops.Destroy(s.Context(), c, name); err != nil {
			failCtl(s, log, "rm", err)
			return
		}
		fmt.Fprintf(s, "removed %s — its disk is gone\r\n", name)
		s.Exit(0) //nolint:errcheck
	case "rename", "mv":
		g.controlRename(s, c, args, log)
	case "tags":
		g.controlTags(s, c, args, log)
	case "sessions":
		g.controlSessions(s, c, args, log)
	case "pin":
		name, ok := g.ownedBoxArg(s, c, args, log)
		if !ok {
			return
		}
		if _, err := g.ops.SetPinned(s.Context(), c, name, true); err != nil {
			failCtl(s, log, "pin", err)
			return
		}
		// Pinning implies "keep it running now", so resume it immediately.
		if _, err := g.ops.Resume(s.Context(), c, name); err != nil {
			// The pin flag is set; it just isn't warm yet. Report but don't fail.
			// The manager's own words go in the sentence, as they always have.
			fmt.Fprintf(s.Stderr(), "sparkbox: pinned %s, but couldn't resume it now: %v\r\n", name, cause(err))
			s.Exit(1) //nolint:errcheck
			return
		}
		fmt.Fprintf(s, "pinned %s — it stays always-on (in-VM cron & daemons keep running)\r\n", name)
		s.Exit(0) //nolint:errcheck
	case "unpin":
		name, ok := g.ownedBoxArg(s, c, args, log)
		if !ok {
			return
		}
		if _, err := g.ops.SetPinned(s.Context(), c, name, false); err != nil {
			failCtl(s, log, "unpin", err)
			return
		}
		fmt.Fprintf(s, "unpinned %s — it will pause when idle\r\n", name)
		s.Exit(0) //nolint:errcheck
	case "snapshot":
		g.controlSnapshot(s, c, args[1:], log)
	case "fork":
		g.controlFork(s, c, args[1:], log)
	case "schedule":
		g.controlSchedule(s, c, args[1:], log)
	case "whoami":
		g.controlWhoami(s, c, log)
	case "keys":
		g.controlKeys(s, c, args[1:], log)
	case "github":
		g.controlGitHub(s, c, args[1:], log)
	case "passkey", "passkeys":
		g.controlPasskey(s, c, args[1:], log)
	case "email":
		g.controlEmail(s, c, args[1:], log)
	case "share":
		g.controlShare(s, c, args[1:], log)
	case "session-token":
		g.controlSessionToken(s, c, args[1:], log)
	case "invite":
		g.controlInvite(s, c, log)
	case "secret", "secrets":
		g.controlSecret(s, c, args[1:], log)
	case "repo", "repos":
		g.controlRepo(s, c, args[1:], log)
	// `badge` sits beside the repo verbs rather than under them because what it
	// prints is about a repository the way `repo add` is, and because it is the
	// one command on this channel whose output is meant to be copied out of the
	// terminal — burying it as `repo badge` would hide it behind a verb people
	// only type when they are attaching something.
	case "badge":
		g.controlBadge(s, c, args[1:], log)
	case "user", "users":
		g.controlUser(s, c, args[1:], log)
	case "node":
		g.controlNode(s, c, args[1:], log)
	case "help", "-h", "--help":
		// Asked for, so it goes to stdout and exits 0 — unlike the index
		// printed at somebody who mistyped.
		g.controlHelpCmd(s, c, args[1:])
	default:
		fmt.Fprintf(s.Stderr(), "unknown command %q\r\n%s", args[0], controlHelp(g.isOperator(s, c)))
		s.Exit(2) //nolint:errcheck
	}
}

// caller is who ctlops should act for: the handle the public-key check already
// resolved, plus the fingerprint of the key on this session — which whoami
// echoes, `keys list` marks, and `keys verify-github` offers github.com as
// proof. ctlops takes no operator flag, so this channel cannot assert one.
func caller(s gssh.Session, user string) ctlops.Caller {
	return ctlops.Caller{Handle: user, KeyFP: sessionKeyFP(s)}
}

// failCtl renders a ctlops error on the ctl channel and ends the session.
//
// Whether the sentence stands alone or is wrapped in fail()'s
// "sparkbox: <what> failed: …" shape is the error's own Verbatim flag rather
// than this function's guess — that is exactly what keeps `no sandbox named "x"`
// and `pause failed: …` byte-identical now that the logic producing them lives
// in another package. The exit code comes from the error's Kind, so the
// 2-means-you-typed-it-wrong / 1-means-it-failed contract is applied in one
// place instead of at thirty call sites.
func failCtl(s gssh.Session, log *slog.Logger, what string, err error) {
	e := ctlops.AsError(what, err)
	if !e.Verbatim {
		fail(s, log, what, e)
		return
	}
	// A verbatim sentence is a refusal the user is already reading — a typo, a
	// name they don't own, a feature this host doesn't run — so it stays out of
	// the operator's log unless something actually broke.
	switch e.Kind {
	case ctlops.KindInternal, ctlops.KindUpstream:
		log.Error(what+" failed", "err", err)
	default:
		log.Debug(what+" refused", "err", err, "kind", e.Kind.String())
	}
	fmt.Fprintf(s.Stderr(), "sparkbox: %s\r\n", e.Msg)
	s.Exit(e.ExitCode()) //nolint:errcheck
}

// wrapVerbatim renders a ctlops error through fail()'s "<what> failed: …"
// wrapper instead of as a bare sentence. It exists for the handful of failures
// ctlops now classifies (a host with no object storage, a driver that cannot
// snapshot) that this channel has always printed wrapped, because the check used
// to live in the manager. Only the named kinds are rewritten, so a masked
// `no sandbox named …` can never be reshaped by accident.
func wrapVerbatim(err error, kinds ...ctlops.Kind) error {
	var e *ctlops.Error
	if !errors.As(err, &e) || !e.Verbatim {
		return err
	}
	for _, k := range kinds {
		if e.Kind == k {
			w := *e
			w.Verbatim = false
			return &w
		}
	}
	return err
}

// exitAs forces the exit code this channel has always used for a class of
// failure. `schedule add` with an unparseable cron is the one case: ctlops
// rejects the spec itself and so calls it a malformed invocation (exit 2), but
// the shipped CLI let the store reject it and reported exit 1, and other tooling
// may key off that.
func exitAs(err error, kind ctlops.Kind, code int) error {
	var e *ctlops.Error
	if !errors.As(err, &e) || e.Kind != kind {
		return err
	}
	w := *e
	w.Exit = code
	return &w
}

// cause unwraps a ctlops error to the failure underneath. It is for the one
// message that interpolates an error with %v rather than rendering it: `pin`
// reports a half-success in a sentence of its own, and the manager's own words
// are what has always appeared there.
func cause(err error) error {
	var e *ctlops.Error
	if errors.As(err, &e) && e.Err != nil {
		return e.Err
	}
	return err
}

// parseSizeMB reads a human disk size into MiB. It is a one-line wrapper over
// ctlops.ParseSize so `ctl resize` and the REST API's `size` field cannot drift
// apart, error text included.
func parseSizeMB(arg string) (int64, error) { return ctlops.ParseSize(arg) }

// ownedBoxArg reads args[1] and confirms the caller may act on it, printing the
// usage or not-found error and ending the session on either failure.
//
// The lookup itself is ctlops.Get, so the "does not exist" and "is not yours"
// answers come from the same line of code the REST API and the browser terminal
// use, and neither can drift into leaking which sandboxes exist. It still runs
// before anything is printed: a command that announces what it is about to do
// must not announce it about a sandbox that isn't there.
func (g *Gateway) ownedBoxArg(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) (string, bool) {
	if len(args) < 2 {
		fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> %s <name>\r\n", ControlUser, args[0])
		s.Exit(2) //nolint:errcheck
		return "", false
	}
	name := args[1]
	if _, err := g.ops.Get(s.Context(), c, name); err != nil {
		failCtl(s, log, args[0], err)
		return "", false
	}
	return name, true
}

func (g *Gateway) controlWhoami(s gssh.Session, c ctlops.Caller, log *slog.Logger) {
	me, err := g.ops.Whoami(s.Context(), c)
	if err != nil {
		failCtl(s, log, "whoami", err)
		return
	}
	fmt.Fprintf(s, "handle:  %s\r\n", me.Handle)
	fmt.Fprintf(s, "status:  %s\r\n", me.Status)
	if me.GitHubVerifiedAt != nil {
		// The provenance is shown because the three ways of proving a link are
		// not interchangeable — only two of them may adopt keys — and somebody
		// wondering why `keys import-github` refused should read the answer
		// here rather than guess.
		fmt.Fprintf(s, "github:  %s (verified %s via %s)\r\n",
			me.GitHubLogin, me.GitHubVerifiedAt.Format("2006-01-02"), me.GitHubVia)
	} else {
		fmt.Fprintf(s, "github:  not linked — link it with: ssh %s@%s github link\r\n",
			ControlUser, g.sshHint())
	}
	fmt.Fprintf(s, "subject: %s\r\n", me.Subject)
	fmt.Fprintf(s, "key:     %s\r\n", me.KeyFP)
	s.Exit(0) //nolint:errcheck
}

func (g *Gateway) controlKeys(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), pageFor("account"))
		s.Exit(2) //nolint:errcheck
		return
	}
	switch args[0] {
	case "ls", "list":
		keys, err := g.ops.ListKeys(s.Context(), c)
		if err != nil {
			failCtl(s, log, "keys list", err)
			return
		}
		for _, k := range keys {
			marker := " "
			if k.Current {
				marker = "*" // the key you're connected with right now
			}
			fmt.Fprintf(s, "%s %s  %-16s %-14s %s\r\n",
				marker, k.FP, truncate(k.Label, 16), k.Via, k.AddedAt.Format("2006-01-02"))
		}
		s.Exit(0) //nolint:errcheck

	case "add":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), "usage: ssh ctl@<gateway> keys add \"ssh-ed25519 AAAA... label\"\r\n")
			s.Exit(2) //nolint:errcheck
			return
		}
		// Joined with spaces first, so an unquoted key line still works.
		k, err := g.ops.AddKey(s.Context(), c, strings.Join(args[1:], " "))
		if err != nil {
			failCtl(s, log, "keys add", err)
			return
		}
		fmt.Fprintf(s, "added %s\r\n", k.FP)
		s.Exit(0) //nolint:errcheck

	case "rm":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), "usage: ssh ctl@<gateway> keys rm <SHA256:...>\r\n")
			s.Exit(2) //nolint:errcheck
			return
		}
		if err := g.ops.RemoveKey(s.Context(), c, args[1]); err != nil {
			failCtl(s, log, "keys rm", err)
			return
		}
		fmt.Fprintf(s, "removed %s\r\n", args[1])
		s.Exit(0) //nolint:errcheck

	case "import-github":
		res, err := g.ops.ImportGitHubKeys(s.Context(), c)
		if err != nil {
			// The "link one first" hint names an ssh incantation, which is this
			// channel's syntax rather than something ctlops should know; it hands
			// back the transport-free version, so the CLI wording is rebuilt here.
			if e := ctlops.AsError("keys import-github", err); e.Code == "github_not_linked" {
				fmt.Fprintf(s.Stderr(), "sparkbox: no GitHub account linked — link one with: ssh %s@%s github link\r\n",
					ControlUser, g.sshHint())
				s.Exit(1) //nolint:errcheck
				return
			}
			failCtl(s, log, "keys import-github", err)
			return
		}
		for _, fp := range res.Skipped {
			fmt.Fprintf(s.Stderr(), "skipped %s (linked to another account)\r\n", fp)
		}
		// AddKey is a no-op for keys already on the account, so `Imported` counts
		// only genuinely new ones and re-running this is free.
		fmt.Fprintf(s, "imported %d new key(s) from github.com/%s (%d listed)\r\n",
			res.Imported, res.Login, res.Listed)
		s.Exit(0) //nolint:errcheck

	case "verify-github":
		// Resolving the login here rather than letting ctlops fall back to the
		// stored one keeps the "you have to name it" answer a usage line, which is
		// what a CLI owes a caller who typed too little.
		login := ""
		if len(args) >= 2 {
			login = args[1]
		} else if me, err := g.ops.Whoami(s.Context(), c); err == nil {
			login = me.GitHubLogin
		}
		if login == "" {
			fmt.Fprint(s.Stderr(), "usage: ssh ctl@<gateway> keys verify-github <login>\r\n")
			s.Exit(2) //nolint:errcheck
			return
		}
		// The session's own key is the proof: ctlops checks it belongs to the
		// caller before offering it to github.com.
		if _, err := g.ops.VerifyGitHub(s.Context(), c, login, c.KeyFP); err != nil {
			failCtl(s, log, "keys verify-github", err)
			return
		}
		fmt.Fprintf(s, "verified: %s is listed on github.com/%s\r\n", c.KeyFP, login)
		s.Exit(0) //nolint:errcheck

	default:
		fmt.Fprintf(s.Stderr(), "unknown keys command %q\r\n%s", args[0], pageFor("account"))
		s.Exit(2) //nolint:errcheck
	}
}

// controlInvite mints a single-use invite. Operators (the users seeded from
// users.conf) may always invite; everyone else is capped by --invites-per-user,
// which defaults to 0 — operator-only. ctlops resolves operator status from the
// account store, so this channel cannot assert it on the caller's behalf.
func (g *Gateway) controlInvite(s gssh.Session, c ctlops.Caller, log *slog.Logger) {
	inv, err := g.ops.Invite(s.Context(), c)
	if err != nil {
		failCtl(s, log, "invite", err)
		return
	}
	fmt.Fprintf(s, "invite code: %s   (single use, expires in %d days)\r\n",
		inv.Code, int(users.InviteTTL.Hours()/24))
	fmt.Fprintf(s, "they run:    ssh %s@%s\r\n", SignupUser, g.sshHint())
	s.Exit(0) //nolint:errcheck
}

// controlSchedule manages the caller's platform-scheduler entries: cron jobs
// the host fires by waking the sandbox, so periodic work survives scale-to-zero
// (resource-model design, Part 3). It only ever touches the caller's own
// sandboxes and schedules.
func (g *Gateway) controlSchedule(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	// Asked up front rather than per-subcommand, so an unknown subcommand on a
	// host without the scheduler still says the honest thing.
	if !g.ops.Capabilities().Scheduling {
		fmt.Fprint(s.Stderr(), "sparkbox: platform scheduling isn't enabled on this host.\r\n")
		s.Exit(1) //nolint:errcheck
		return
	}
	sub := "ls"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "ls", "list":
		entries, err := g.ops.ListSchedules(s.Context(), c)
		if err != nil {
			failCtl(s, log, "schedule list", err)
			return
		}
		if len(entries) == 0 {
			fmt.Fprintf(s, "no scheduled jobs — add one with:\r\n  ssh %s@%s schedule add <box> \"*/30 * * * *\" <cmd>\r\n",
				ControlUser, g.sshHint())
			s.Exit(0) //nolint:errcheck
			return
		}
		for _, e := range entries {
			next := "unparseable"
			if e.NextRun != nil {
				next = e.NextRun.Local().Format("2006-01-02 15:04 MST")
			}
			last := "never"
			if e.LastRun != nil {
				mark := "✓"
				if e.LastError != "" {
					mark = "✗"
				}
				last = e.LastRun.Local().Format("01-02 15:04") + " " + mark
			}
			fmt.Fprintf(s, "%s  %-20s %-15s  next %s  last %s\r\n      $ %s\r\n",
				e.ID, e.Sandbox, e.Spec, next, last, e.Command)
			if e.LastError != "" {
				fmt.Fprintf(s, "      ! %s\r\n", e.LastError)
			}
		}
		s.Exit(0) //nolint:errcheck
	case "add":
		// schedule add <sandbox> "<cron>" <command...>
		if len(args) < 4 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> schedule add <sandbox> \"<cron>\" <command>\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		e, err := g.ops.AddSchedule(s.Context(), c, ctlops.ScheduleArgs{
			Sandbox: args[1], Spec: args[2], Command: strings.Join(args[3:], " "),
		})
		if err != nil {
			failCtl(s, log, "schedule add", exitAs(err, ctlops.KindInvalid, 1))
			return
		}
		next := ""
		if e.NextRun != nil {
			next = e.NextRun.Local().Format("2006-01-02 15:04 MST")
		}
		fmt.Fprintf(s, "added %s — waking %s on %q; next fire %s\r\n", e.ID, e.Sandbox, e.Spec, next)
		s.Exit(0) //nolint:errcheck
	case "rm":
		if len(args) < 2 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> schedule rm <id>\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		if err := g.ops.DeleteSchedule(s.Context(), c, args[1]); err != nil {
			failCtl(s, log, "schedule rm", err)
			return
		}
		fmt.Fprintf(s, "removed %s\r\n", args[1])
		s.Exit(0) //nolint:errcheck
	default:
		fmt.Fprintf(s.Stderr(), "unknown schedule command %q\r\n%s", sub, pageFor("schedule"))
		s.Exit(2) //nolint:errcheck
	}
}

// controlSnapshot manages the caller's fork-able templates: snapshot list |
// create <box> <name> | rm <name> | bind <name> --tag <t> | unbind --tag <t>.
// Only ever touches the caller's own sandboxes and snapshots.
func (g *Gateway) controlSnapshot(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	sub := "ls"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "ls", "list":
		snaps, err := g.ops.ListSnapshots(s.Context(), c)
		if err != nil {
			failCtl(s, log, "snapshot list", err)
			return
		}
		if len(snaps) == 0 {
			fmt.Fprintf(s, "no snapshots — create one with:\r\n  ssh %s@%s snapshot create <box> <name>\r\n",
				ControlUser, g.sshHint())
			s.Exit(0) //nolint:errcheck
			return
		}
		for _, sn := range snaps {
			// The three columns are unchanged and the two additions are
			// SUFFIXES, both omitted when they are empty. A host with no
			// bindings and one machine therefore prints exactly the row it
			// printed before this feature existed — which is what keeps the
			// listing inside 80 columns for everybody who is not using either
			// of them, and keeps every shipped golden row where it was.
			extra := ""
			if len(sn.BoundTags) > 0 {
				extra += "  tags: " + strings.Join(sn.BoundTags, ",")
			}
			if sn.Node != "" {
				extra += "  on " + sn.Node
			}
			fmt.Fprintf(s, "%-24s from %-20s %s%s\r\n",
				sn.Name, sn.FromBox, sn.CreatedAt.Local().Format("2006-01-02 15:04"), extra)
		}
		s.Exit(0) //nolint:errcheck
	case "create":
		// parseTags, so that `--tag web` captures and binds in one gesture —
		// the same ctlops.SnapshotToTag the in-guest `sparkbox snapshot web`
		// goes through, rather than a second implementation that agrees today.
		// Not parseCreateArgs: a capture names no machine, because the sandbox
		// it comes from already does.
		tags, rest, err := parseTags(args[1:])
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(2) //nolint:errcheck
			return
		}
		if len(rest) != 2 || len(tags) > 1 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> snapshot create <box> <name> [--tag <tag>]\r\n"+
				"       at most one tag: a tag has exactly one base image\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		box, name := rest[0], rest[1]
		tag := ""
		if len(tags) == 1 {
			tag = tags[0]
		}
		// Resolve before promising: the progress line below must not name a
		// sandbox the caller cannot act on.
		if _, err := g.ops.Get(s.Context(), c, box); err != nil {
			failCtl(s, log, "snapshot create", err)
			return
		}
		// The refresh is named because it is why this now takes longer than it
		// used to, and the estimate is widened for the same reason. No line
		// reports its OUTCOME: it is best-effort, and a failure is a gateway
		// WARN by design — telling a person their template is a few days
		// behind on tools would invite them to retry a capture that worked.
		fmt.Fprintf(s, "snapshotting %s → %q (refresh agent tools + pause + compact; this can take a few minutes)…\r\n", box, name)
		res, err := g.ops.SnapshotToTag(s.Context(), c, ctlops.SnapshotToTagArgs{
			Sandbox: box, Name: name, Tag: tag,
		})
		if err != nil {
			// The half-failure — captured, not bound — comes through here with
			// its repair line already in the sentence, which is why this stays
			// one call site rather than growing a special case.
			failCtl(s, log, "snapshot create", wrapVerbatim(err, ctlops.KindDisabled))
			return
		}
		if res.Bound {
			fmt.Fprintf(s, "created snapshot %q and bound tag %q to it — your next\r\n  ssh %s@%s -- --tag %s\r\nboots from it.\r\n",
				name, res.Tag, NewSandboxUser, g.sshHint(), res.Tag)
			if res.Previous != "" {
				fmt.Fprintf(s, "note: that tag used to boot from %q — sandboxes already created from it are unaffected.\r\n",
					res.Previous)
			}
		} else {
			fmt.Fprintf(s, "created snapshot %q — fork it with: ssh %s@%s fork %s <new-name>\r\n",
				name, ControlUser, g.sshHint(), name)
		}
		// Stated as the POLICY, not as an outcome. The refresh is best-effort and
		// silently skipped for a guest the gateway cannot reach — a paused remote
		// sandbox is the ordinary case — and refreshToolsForPack's bool that says
		// which happened is dropped long before it could reach this session. A
		// sentence that claimed it every time would be false on exactly the
		// captures whose staleness a reader most needs to suspect.
		fmt.Fprintf(s, "a capture refreshes the agent CLIs first when it can reach the guest; inside a sandbox, `sparkbox update-tools` does it on demand\r\n")
		s.Exit(0) //nolint:errcheck
	case "rm":
		if len(args) < 2 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> snapshot rm <name>\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		if err := g.ops.DeleteSnapshot(s.Context(), c, args[1]); err != nil {
			failCtl(s, log, "snapshot rm", err)
			return
		}
		fmt.Fprintf(s, "deleted snapshot %q\r\n", args[1])
		s.Exit(0) //nolint:errcheck
	case "bind":
		// parseTags, not parseCreateArgs: a bind names no machine, because the
		// snapshot already does — it is a file in one machine's image directory
		// — so `--node` is not a flag this door silently absorbs, it is a bare
		// word and therefore a usage error below.
		tags, rest, err := parseTags(args[1:])
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(2) //nolint:errcheck
			return
		}
		if len(rest) != 1 || len(tags) != 1 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> snapshot bind <snapshot> --tag <tag>\r\n"+
				"       one tag, one snapshot: a tag has exactly one base image\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		res, err := g.ops.BindTemplate(s.Context(), c, rest[0], tags[0])
		if err != nil {
			failCtl(s, log, "snapshot bind", wrapVerbatim(err, ctlops.KindDisabled))
			return
		}
		fmt.Fprintf(s, "tag %q now boots from snapshot %q — create one with:\r\n  ssh %s@%s -- %s\r\n",
			res.Binding.Tag, res.Binding.Snapshot, NewSandboxUser, g.sshHint(), res.Binding.Tag)
		if res.Previous != "" {
			// A re-point is the one outcome that looks identical to a first
			// bind and is not: sandboxes made from here on boot from something
			// else. ctlops reports what it replaced precisely so this line can
			// exist — see TemplateBindResult.Previous.
			fmt.Fprintf(s, "note: that tag used to boot from %q — sandboxes already created from it are unaffected.\r\n",
				res.Previous)
		}
		s.Exit(0) //nolint:errcheck
	case "unbind":
		tags, rest, err := parseTags(args[1:])
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(2) //nolint:errcheck
			return
		}
		if len(rest) != 0 || len(tags) != 1 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> snapshot unbind --tag <tag>\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		b, err := g.ops.UnbindTemplate(s.Context(), c, tags[0])
		if err != nil {
			failCtl(s, log, "snapshot unbind", wrapVerbatim(err, ctlops.KindDisabled))
			return
		}
		// The snapshot is named on the way out because an unbind that only said
		// "ok" reads the same whether the right tag was dropped or the wrong
		// one, and nothing is deleted here for the user to check against.
		fmt.Fprintf(s, "tag %q no longer boots from snapshot %q — new sandboxes on it take the default image again.\r\n",
			b.Tag, b.Snapshot)
		s.Exit(0) //nolint:errcheck
	default:
		fmt.Fprintf(s.Stderr(), "unknown snapshot command %q\r\n%s", sub, pageFor("snapshots"))
		s.Exit(2) //nolint:errcheck
	}
}

// controlFork creates a new sandbox from one of the caller's snapshots.
func (g *Gateway) controlFork(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	parsed, err := parseCreateArgs(args)
	if err == nil && parsed.Node != "" {
		// A fork has no --node to give: a snapshot is a file in one machine's
		// image directory, so the fork happens where the template is or not at
		// all. Saying so is the whole reason this door parses a flag it does not
		// accept — the alternative is the bare word `--node` becoming a tag.
		err = fmt.Errorf("fork has no --node: a snapshot can only be forked on the machine that holds it")
	}
	if err != nil {
		fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
		s.Exit(2) //nolint:errcheck
		return
	}
	if len(parsed.Rest) < 2 {
		fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> fork <snapshot> <new-name> [--tag <t>]… [--ref <branch>]…\r\n"+
			"       list your snapshots with: ssh %s@<gateway> snapshot list\r\n", ControlUser, ControlUser)
		s.Exit(2) //nolint:errcheck
		return
	}
	snapshot, name := parsed.Rest[0], parsed.Rest[1]
	// ctlops stamps the tags before the fork and clears them again if it fails,
	// for the same reason the new@ door does: a fork is a create, and the
	// secret-env push it kicks off asynchronously is decided by the tags.
	if _, err := g.ops.Fork(s.Context(), c, ctlops.ForkArgs{
		Snapshot: snapshot, Name: name, Tags: parsed.Tags, Refs: parsed.Refs,
	}); err != nil {
		failCtl(s, log, "fork", wrapVerbatim(err, ctlops.KindDisabled))
		return
	}
	tagNote := ""
	if len(parsed.Tags) > 0 {
		tagNote = fmt.Sprintf(" [tags: %s]", strings.Join(parsed.Tags, ", "))
	}
	fmt.Fprintf(s, "created %s from snapshot %q%s — connect with: ssh %s@%s\r\n",
		name, snapshot, tagNote, name, g.sshHint())
	s.Exit(0) //nolint:errcheck
}

// controlRename gives a sandbox a new name — and with it a new default
// subdomain, a new browser-terminal host and a new `ssh <name>@<gateway>`.
//
// The manager pauses the box before it moves the VM directory and drops the
// memory snapshot (a firecracker state.snap embeds the old absolute paths), so
// the next start is a cold boot. That is the same bargain `resize` makes, and
// it is announced the same way and for the same reason: the session goes quiet
// for the pause, and processes running inside do not come back.
func (g *Gateway) controlRename(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	// Ordered exactly like `resize`, and for its two reasons. The name is
	// resolved before the missing-destination complaint, so the usage line can
	// never confirm that a stranger's sandbox is real; and both arity checks
	// print this command's real grammar rather than letting ownedBoxArg's
	// one-argument "rename <name>" stand in for it.
	usage := func() {
		fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> rename <name> <new-name>\r\n", ControlUser)
		s.Exit(2) //nolint:errcheck
	}
	if len(args) < 2 {
		usage()
		return
	}
	name, ok := g.ownedBoxArg(s, c, args, log)
	if !ok {
		return
	}
	if len(args) < 3 {
		usage()
		return
	}
	newName := args[2]
	fmt.Fprintf(s, "renaming %s to %s (pause + cold boot; running processes restart)…\r\n", name, newName)
	if _, err := g.ops.Rename(s.Context(), c, name, newName); err != nil {
		failCtl(s, log, "rename", wrapVerbatim(err, ctlops.KindDisabled))
		return
	}
	// The new address is the point of the command, so it is the sentence. The
	// guest's own hostname is the one thing that does not move until it reboots,
	// which it is about to do anyway.
	fmt.Fprintf(s, "renamed %s → %s — connect with: ssh %s@%s\r\n", name, newName, newName, g.sshHint())
	s.Exit(0) //nolint:errcheck
}

// controlTags shows or replaces a sandbox's tags. Tags select which of the
// owner's secrets are pushed into the sandbox's environment, so setting them
// re-syncs the box rather than waiting for its next resume — ctlops does that
// re-push, because every transport that writes tags owes it.
func (g *Gateway) controlTags(s gssh.Session, c ctlops.Caller, args []string, log *slog.Logger) {
	name, ok := g.ownedBoxArg(s, c, args, log)
	if !ok {
		return
	}
	// `tags <name>` reads; `tags <name> a b c` replaces the whole set, and
	// `tags <name> --clear` empties it (an empty list can't be typed otherwise).
	if len(args) == 2 {
		tags, err := g.ops.Tags(s.Context(), c, name)
		if err != nil {
			failCtl(s, log, "tags", wrapVerbatim(err, ctlops.KindDisabled))
			return
		}
		if len(tags) == 0 {
			fmt.Fprintf(s, "%s has no tags — set them with: ssh %s@<gateway> tags %s <tag>…\r\n",
				name, ControlUser, name)
		} else {
			fmt.Fprintf(s, "%s: %s\r\n", name, strings.Join(tags, ", "))
		}
		s.Exit(0) //nolint:errcheck
		return
	}
	var want []string
	if !(len(args) == 3 && args[2] == "--clear") {
		parsed, err := parseCreateArgs(args[2:])
		if err == nil && parsed.Node != "" {
			// Tags do not move a sandbox, and a --node here would be read as
			// two tags if this door did not know the flag at all. See
			// parseCreateArgs.
			err = fmt.Errorf("tags has no --node: a sandbox is placed when it is created")
		}
		if err == nil && len(parsed.Refs) > 0 {
			// Same shape of refusal, and the reason is the one --ref rests on:
			// it says which branch a sandbox STARTS on, decided once when it is
			// created. Retagging a box that already exists cannot re-run that,
			// and a --ref quietly accepted here would look like it had.
			err = fmt.Errorf("tags has no --ref: it decides where a checkout starts, so it is a create-time choice — use `git switch` in the box")
		}
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(2) //nolint:errcheck
			return
		}
		// Bare words are tags too, so `tags box ml prod` works alongside --tag.
		want = append(parsed.Tags, parsed.Rest...)
	}
	set, note, err := g.ops.SetTags(s.Context(), c, name, want)
	if err != nil {
		failCtl(s, log, "tags", wrapVerbatim(err, ctlops.KindDisabled))
		return
	}
	if len(set) == 0 {
		fmt.Fprintf(s, "cleared tags on %s\r\n", name)
	} else {
		fmt.Fprintf(s, "%s: %s\r\n", name, strings.Join(set, ", "))
	}
	// Repos follow tags. The note says whether the box took the checkout job,
	// and it goes to stderr because the line above is the answer to what was
	// asked — a script reading tags out of stdout should not have to parse
	// around advice about a machine.
	if note != "" {
		fmt.Fprintf(s.Stderr(), "sparkbox: %s\r\n", note)
	}
	s.Exit(0) //nolint:errcheck
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
