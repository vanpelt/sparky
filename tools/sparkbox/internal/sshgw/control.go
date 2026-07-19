package sshgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/schedule"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

const controlUsage = "usage: ssh ctl@<gateway> <command>\r\n" +
	"\r\n" +
	" creating a sandbox\r\n" +
	"  (new sandbox)            ssh new@<gateway> [<tag>…]   — creates one and connects\r\n" +
	"  fork <snapshot> <name> [--tag <t>]…  create one from a snapshot you saved\r\n" +
	"\r\n" +
	" sandboxes\r\n" +
	"  list                     list your sandboxes and their state\r\n" +
	"  pause <name>             pause a running sandbox to free a slot\r\n" +
	"  archive <name>           park a sandbox in object storage (frees host disk)\r\n" +
	"  restore <name>           bring an archived sandbox back and start it\r\n" +
	"  resize <name> <size>     grow a sandbox's root disk, e.g. 25G (cold-boots it)\r\n" +
	"  rm <name>                delete a sandbox and its disk — permanent, see archive\r\n" +
	"  tags <name> [<tag>…]     show or set tags (they select which secrets it gets)\r\n" +
	"  pin <name>               keep a sandbox always-on (in-VM cron/daemons run)\r\n" +
	"  unpin <name>             let a sandbox pause when idle again\r\n" +
	"\r\n" +
	" snapshots (fork-able disk templates)\r\n" +
	"  snapshot list            list your snapshots\r\n" +
	"  snapshot create <box> <name>  save <box>'s current disk as a template\r\n" +
	"  snapshot rm <name>       delete a snapshot template\r\n" +
	"\r\n" +
	" other\r\n" +
	"  schedule list            list your platform-scheduled jobs\r\n" +
	"  schedule add <box> \"<cron>\" <cmd>  wake <box> on a cron schedule to run <cmd>\r\n" +
	"  schedule rm <id>         remove a scheduled job\r\n" +
	"  whoami                   show your account and linked identities\r\n" +
	"  keys list                list the SSH keys on your account\r\n" +
	"  keys add \"<key line>\"    link another key\r\n" +
	"  keys rm <SHA256:...>     unlink a key (never the last one)\r\n" +
	"  keys import-github       adopt every key github.com lists for your login\r\n" +
	"  keys verify-github       re-check your GitHub link\r\n" +
	"  email [set <addr>|clear] show or set the email forwarded to private apps\r\n" +
	"  share <name> [public|private]  show or set who can reach a sandbox's URLs\r\n" +
	"  session-token [--ttl <dur>]    mint a browser/API token for private URLs\r\n" +
	"  invite                   mint a single-use invite code\r\n" +
	"  help                     print this list\r\n"

// handleControl serves the `ctl@` out-of-band channel: managing sandboxes and
// your own account without dialing into a VM. It only ever touches the
// caller's own sandboxes and keys.
func (g *Gateway) handleControl(s gssh.Session, user string, log *slog.Logger) {
	args := s.Command()
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), controlUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	switch args[0] {
	case "list":
		n := 0
		for _, b := range g.mgr.List() {
			if b.Owner != user {
				continue
			}
			tier := "scale-to-zero"
			if b.Pinned {
				tier = "pinned"
			}
			fmt.Fprintf(s, "%-24s %-8s %s\r\n", b.Name, b.State, tier)
			n++
		}
		if n == 0 {
			fmt.Fprint(s, "no sandboxes yet — create one with: ssh new@<gateway>\r\n")
		}
		s.Exit(0) //nolint:errcheck
	case "pause":
		name, ok := g.ownedBoxArg(s, user, args)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(s.Context(), pauseTimeout)
		defer cancel()
		if err := g.mgr.Pause(ctx, name); err != nil {
			fail(s, log, "pause", err)
			return
		}
		fmt.Fprintf(s, "paused %s\r\n", name)
		s.Exit(0) //nolint:errcheck
	case "archive":
		name, ok := g.ownedBoxArg(s, user, args)
		if !ok {
			return
		}
		fmt.Fprintf(s, "archiving %s (fsck + compress + upload; this can take a minute)…\r\n", name)
		ctx, cancel := context.WithTimeout(s.Context(), archiveTimeout)
		defer cancel()
		if err := g.mgr.Archive(ctx, name); err != nil {
			fail(s, log, "archive", err)
			return
		}
		fmt.Fprintf(s, "archived %s — host disk freed; `restore %s` (or just connect) brings it back\r\n", name, name)
		s.Exit(0) //nolint:errcheck
	case "restore":
		name, ok := g.ownedBoxArg(s, user, args)
		if !ok {
			return
		}
		fmt.Fprintf(s, "restoring %s (download + boot)…\r\n", name)
		ctx, cancel := context.WithTimeout(s.Context(), archiveTimeout)
		defer cancel()
		if _, err := g.mgr.EnsureRunning(ctx, name); err != nil {
			fail(s, log, "restore", err)
			return
		}
		fmt.Fprintf(s, "restored %s — it's running\r\n", name)
		s.Exit(0) //nolint:errcheck
	case "resize":
		name, ok := g.ownedBoxArg(s, user, args)
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
		ctx, cancel := context.WithTimeout(s.Context(), resizeTimeout)
		defer cancel()
		if err := g.mgr.Resize(ctx, name, sizeMB); err != nil {
			fail(s, log, "resize", err)
			return
		}
		fmt.Fprintf(s, "resized %s — it's running again with a %d MB disk\r\n", name, sizeMB)
		s.Exit(0) //nolint:errcheck
	case "rm", "remove", "destroy":
		name, ok := g.ownedBoxArg(s, user, args)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(s.Context(), pauseTimeout)
		defer cancel()
		if err := g.mgr.Destroy(ctx, name); err != nil {
			fail(s, log, "rm", err)
			return
		}
		fmt.Fprintf(s, "removed %s — its disk is gone\r\n", name)
		s.Exit(0) //nolint:errcheck
	case "tags":
		g.controlTags(s, user, args, log)
	case "pin":
		name, ok := g.ownedBoxArg(s, user, args)
		if !ok {
			return
		}
		if err := g.mgr.SetPinned(name, true); err != nil {
			fail(s, log, "pin", err)
			return
		}
		// Pinning implies "keep it running now", so resume it immediately.
		ctx, cancel := context.WithTimeout(s.Context(), pauseTimeout)
		defer cancel()
		if _, err := g.mgr.EnsureRunning(ctx, name); err != nil {
			// The pin flag is set; it just isn't warm yet. Report but don't fail.
			fmt.Fprintf(s.Stderr(), "sparkbox: pinned %s, but couldn't resume it now: %v\r\n", name, err)
			s.Exit(1) //nolint:errcheck
			return
		}
		fmt.Fprintf(s, "pinned %s — it stays always-on (in-VM cron & daemons keep running)\r\n", name)
		s.Exit(0) //nolint:errcheck
	case "unpin":
		name, ok := g.ownedBoxArg(s, user, args)
		if !ok {
			return
		}
		if err := g.mgr.SetPinned(name, false); err != nil {
			fail(s, log, "unpin", err)
			return
		}
		fmt.Fprintf(s, "unpinned %s — it will pause when idle\r\n", name)
		s.Exit(0) //nolint:errcheck
	case "snapshot":
		g.controlSnapshot(s, user, args[1:], log)
	case "fork":
		g.controlFork(s, user, args[1:], log)
	case "schedule":
		g.controlSchedule(s, user, args[1:], log)
	case "whoami":
		g.controlWhoami(s, user)
	case "keys":
		g.controlKeys(s, user, args[1:], log)
	case "email":
		g.controlEmail(s, user, args[1:], log)
	case "share":
		g.controlShare(s, user, args[1:], log)
	case "session-token":
		g.controlSessionToken(s, user, args[1:], log)
	case "invite":
		g.controlInvite(s, user, log)
	case "help", "-h", "--help":
		// Asked for, so it goes to stdout and exits 0 — unlike the same text
		// printed as an error for a bad command.
		fmt.Fprint(s, controlUsage)
		s.Exit(0) //nolint:errcheck
	default:
		fmt.Fprintf(s.Stderr(), "unknown command %q\r\n%s", args[0], controlUsage)
		s.Exit(2) //nolint:errcheck
	}
}

// parseSizeMB reads a human disk size into MiB: "25G"/"25GB"/"25g" and
// "512M"/"512MB", or a bare number, which is taken as GB because that is the
// unit anyone naming a sandbox disk is thinking in — "resize box 25" meaning
// 25 MB would be a surprising way to lose an afternoon.
func parseSizeMB(arg string) (int64, error) {
	t := strings.TrimSpace(strings.ToUpper(arg))
	t = strings.TrimSuffix(t, "B") // GB -> G, MB -> M
	mult := int64(1024)            // bare number: GB
	switch {
	case strings.HasSuffix(t, "G"):
		t, mult = strings.TrimSuffix(t, "G"), 1024
	case strings.HasSuffix(t, "M"):
		t, mult = strings.TrimSuffix(t, "M"), 1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad size %q — use e.g. 25G or 512M", arg)
	}
	if n <= 0 {
		return 0, fmt.Errorf("size must be positive, got %q", arg)
	}
	mb := n * mult
	if mb > maxDiskMB {
		return 0, fmt.Errorf("size %q exceeds the %d GB per-sandbox limit", arg, maxDiskMB/1024)
	}
	return mb, nil
}

// ownedBoxArg validates that args[1] names a sandbox the caller owns, printing
// the usage/not-found error and exiting the session on any failure. It returns
// the sandbox name and ok=true only when the caller may act on it. The
// not-found and not-owned cases share one message so we never leak whether
// another user's sandbox exists.
func (g *Gateway) ownedBoxArg(s gssh.Session, user string, args []string) (string, bool) {
	if len(args) < 2 {
		fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> %s <name>\r\n", ControlUser, args[0])
		s.Exit(2) //nolint:errcheck
		return "", false
	}
	name := args[1]
	box, ok := g.mgr.Get(name)
	if !ok || box.Owner != user {
		fmt.Fprintf(s.Stderr(), "sparkbox: no sandbox named %q\r\n", name)
		s.Exit(1) //nolint:errcheck
		return "", false
	}
	return name, true
}

func (g *Gateway) controlWhoami(s gssh.Session, user string) {
	u, err := g.users.Get(user)
	if err != nil {
		fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
		s.Exit(1) //nolint:errcheck
		return
	}
	fmt.Fprintf(s, "handle:  %s\r\n", u.Handle)
	fmt.Fprintf(s, "status:  %s\r\n", u.Status)
	if u.GitHubVerifiedAt != nil {
		fmt.Fprintf(s, "github:  %s (verified %s)\r\n", u.GitHubLogin, u.GitHubVerifiedAt.Format("2006-01-02"))
	} else {
		fmt.Fprintf(s, "github:  not linked — link it with: ssh %s@%s keys verify-github\r\n",
			ControlUser, g.domainHint())
	}
	fmt.Fprintf(s, "subject: %s\r\n", oidc.SubjectFor(user))
	fmt.Fprintf(s, "key:     %s\r\n", sessionKeyFP(s))
	s.Exit(0) //nolint:errcheck
}

func (g *Gateway) controlKeys(s gssh.Session, user string, args []string, log *slog.Logger) {
	if len(args) == 0 {
		fmt.Fprint(s.Stderr(), controlUsage)
		s.Exit(2) //nolint:errcheck
		return
	}
	switch args[0] {
	case "list":
		keys, err := g.users.Keys(user)
		if err != nil {
			fail(s, log, "keys list", err)
			return
		}
		thisFP := sessionKeyFP(s)
		for _, k := range keys {
			marker := " "
			if k.FP == thisFP {
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
		key, comment, _, _, err := xssh.ParseAuthorizedKey([]byte(strings.Join(args[1:], " ")))
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: that isn't a valid authorized_keys line: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		if err := g.users.AddKey(user, key, comment, "ctl"); err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		log.Info("key added", "user", user, "fp", xssh.FingerprintSHA256(key))
		fmt.Fprintf(s, "added %s\r\n", xssh.FingerprintSHA256(key))
		s.Exit(0) //nolint:errcheck

	case "rm":
		if len(args) < 2 {
			fmt.Fprint(s.Stderr(), "usage: ssh ctl@<gateway> keys rm <SHA256:...>\r\n")
			s.Exit(2) //nolint:errcheck
			return
		}
		if err := g.users.RemoveKey(user, args[1]); err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		log.Info("key removed", "user", user, "fp", args[1])
		fmt.Fprintf(s, "removed %s\r\n", args[1])
		s.Exit(0) //nolint:errcheck

	case "import-github":
		u, err := g.users.Get(user)
		if err != nil {
			fail(s, log, "keys import-github", err)
			return
		}
		if u.GitHubLogin == "" {
			fmt.Fprintf(s.Stderr(), "sparkbox: no GitHub account linked — link one with: ssh %s@%s keys verify-github\r\n",
				ControlUser, g.domainHint())
			s.Exit(1) //nolint:errcheck
			return
		}
		keys, err := users.FetchGitHubKeys(s.Context(), u.GitHubLogin)
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		added := 0
		for _, k := range keys {
			switch err := g.users.AddKey(user, k, "github:"+u.GitHubLogin, "github-import"); {
			case err == nil:
				added++
			case errors.Is(err, users.ErrKeyLinked):
				fmt.Fprintf(s.Stderr(), "skipped %s (linked to another account)\r\n", xssh.FingerprintSHA256(k))
			default:
				fail(s, log, "keys import-github", err)
				return
			}
		}
		// AddKey is a no-op for keys already on the account, so `added` counts
		// only genuinely new ones and re-running this is free.
		fmt.Fprintf(s, "imported %d new key(s) from github.com/%s (%d listed)\r\n", added, u.GitHubLogin, len(keys))
		s.Exit(0) //nolint:errcheck

	case "verify-github":
		var login string
		if len(args) >= 2 {
			login = args[1]
		} else if u, err := g.users.Get(user); err == nil {
			login = u.GitHubLogin
		}
		if login == "" {
			fmt.Fprint(s.Stderr(), "usage: ssh ctl@<gateway> keys verify-github <login>\r\n")
			s.Exit(2) //nolint:errcheck
			return
		}
		ok, err := users.VerifyGitHubKey(s.Context(), login, sessionKey(s))
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		if !ok {
			fmt.Fprintf(s.Stderr(),
				"sparkbox: %s isn't listed on github.com/%s.keys — add it there, then retry.\r\n",
				sessionKeyFP(s), login)
			s.Exit(1) //nolint:errcheck
			return
		}
		if err := g.users.LinkGitHub(user, login); err != nil {
			fail(s, log, "keys verify-github", err)
			return
		}
		log.Info("github verified", "user", user, "login", login)
		fmt.Fprintf(s, "verified: %s is listed on github.com/%s\r\n", sessionKeyFP(s), login)
		s.Exit(0) //nolint:errcheck

	default:
		fmt.Fprintf(s.Stderr(), "unknown keys command %q\r\n%s", args[0], controlUsage)
		s.Exit(2) //nolint:errcheck
	}
}

// controlInvite mints a single-use invite. Operators (the users seeded from
// users.conf) may always invite; everyone else is capped by --invites-per-user,
// which defaults to 0 — operator-only.
func (g *Gateway) controlInvite(s gssh.Session, user string, log *slog.Logger) {
	u, err := g.users.Get(user)
	if err != nil {
		fail(s, log, "invite", err)
		return
	}
	if !u.IsOperator() {
		if g.invitesPerUser <= 0 {
			fmt.Fprint(s.Stderr(), "sparkbox: only operators can mint invite codes here.\r\n")
			s.Exit(1) //nolint:errcheck
			return
		}
		n, err := g.users.InviteCount(user)
		if err != nil {
			fail(s, log, "invite", err)
			return
		}
		if n >= g.invitesPerUser {
			fmt.Fprintf(s.Stderr(), "sparkbox: you've used all %d of your invites.\r\n", g.invitesPerUser)
			s.Exit(1) //nolint:errcheck
			return
		}
	}
	code, err := g.users.NewInvite(user)
	if err != nil {
		fail(s, log, "invite", err)
		return
	}
	log.Info("invite minted", "by", user)
	fmt.Fprintf(s, "invite code: %s   (single use, expires in %d days)\r\n",
		code, int(users.InviteTTL.Hours()/24))
	fmt.Fprintf(s, "they run:    ssh %s@%s\r\n", SignupUser, g.domainHint())
	s.Exit(0) //nolint:errcheck
}

// controlSchedule manages the caller's platform-scheduler entries: cron jobs
// the host fires by waking the sandbox, so periodic work survives scale-to-zero
// (resource-model design, Part 3). It only ever touches the caller's own
// sandboxes and schedules.
func (g *Gateway) controlSchedule(s gssh.Session, user string, args []string, log *slog.Logger) {
	if g.schedules == nil {
		fmt.Fprint(s.Stderr(), "sparkbox: platform scheduling isn't enabled on this host.\r\n")
		s.Exit(1) //nolint:errcheck
		return
	}
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		entries, err := g.schedules.ListByOwner(user)
		if err != nil {
			fail(s, log, "schedule list", err)
			return
		}
		if len(entries) == 0 {
			fmt.Fprintf(s, "no scheduled jobs — add one with:\r\n  ssh %s@%s schedule add <box> \"*/30 * * * *\" <cmd>\r\n",
				ControlUser, g.domainHint())
			s.Exit(0) //nolint:errcheck
			return
		}
		now := time.Now()
		for _, e := range entries {
			next := "unparseable"
			if t, err := schedule.NextRun(e.Spec, now); err == nil {
				next = t.Local().Format("2006-01-02 15:04 MST")
			}
			last := "never"
			if !e.LastRun.IsZero() {
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
		sandbox := args[1]
		box, ok := g.mgr.Get(sandbox)
		if !ok || box.Owner != user {
			fmt.Fprintf(s.Stderr(), "sparkbox: no sandbox named %q\r\n", sandbox)
			s.Exit(1) //nolint:errcheck
			return
		}
		spec := args[2]
		cmd := strings.Join(args[3:], " ")
		e, err := g.schedules.Add(schedule.Entry{Sandbox: sandbox, Owner: user, Spec: spec, Command: cmd})
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(1) //nolint:errcheck
			return
		}
		log.Info("schedule added", "user", user, "sandbox", sandbox, "id", e.ID, "spec", spec)
		next, _ := schedule.NextRun(spec, time.Now())
		fmt.Fprintf(s, "added %s — waking %s on %q; next fire %s\r\n",
			e.ID, sandbox, spec, next.Local().Format("2006-01-02 15:04 MST"))
		s.Exit(0) //nolint:errcheck
	case "rm":
		if len(args) < 2 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> schedule rm <id>\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		id := args[1]
		e, err := g.schedules.Get(id)
		if err != nil || e.Owner != user {
			// Same message either way: don't leak another user's schedule ids.
			fmt.Fprintf(s.Stderr(), "sparkbox: no schedule %q\r\n", id)
			s.Exit(1) //nolint:errcheck
			return
		}
		if err := g.schedules.Delete(id); err != nil {
			fail(s, log, "schedule rm", err)
			return
		}
		log.Info("schedule removed", "user", user, "id", id)
		fmt.Fprintf(s, "removed %s\r\n", id)
		s.Exit(0) //nolint:errcheck
	default:
		fmt.Fprintf(s.Stderr(), "unknown schedule command %q\r\n%s", sub, controlUsage)
		s.Exit(2) //nolint:errcheck
	}
}

// controlSnapshot manages the caller's fork-able templates: snapshot list |
// create <box> <name> | rm <name>. Only ever touches the caller's own sandboxes
// and snapshots.
func (g *Gateway) controlSnapshot(s gssh.Session, user string, args []string, log *slog.Logger) {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		snaps := g.mgr.Snapshots(user)
		if len(snaps) == 0 {
			fmt.Fprintf(s, "no snapshots — create one with:\r\n  ssh %s@%s snapshot create <box> <name>\r\n",
				ControlUser, g.domainHint())
			s.Exit(0) //nolint:errcheck
			return
		}
		for _, sn := range snaps {
			fmt.Fprintf(s, "%-24s from %-20s %s\r\n", sn.Name, sn.FromBox, sn.CreatedAt.Local().Format("2006-01-02 15:04"))
		}
		s.Exit(0) //nolint:errcheck
	case "create":
		if len(args) < 3 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> snapshot create <box> <name>\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		box, ok := g.mgr.Get(args[1])
		if !ok || box.Owner != user {
			fmt.Fprintf(s.Stderr(), "sparkbox: no sandbox named %q\r\n", args[1])
			s.Exit(1) //nolint:errcheck
			return
		}
		fmt.Fprintf(s, "snapshotting %s → %q (pause + compact; this can take a minute)…\r\n", args[1], args[2])
		ctx, cancel := context.WithTimeout(s.Context(), archiveTimeout)
		defer cancel()
		if _, err := g.mgr.Snapshot(ctx, args[1], args[2], user); err != nil {
			fail(s, log, "snapshot create", err)
			return
		}
		fmt.Fprintf(s, "created snapshot %q — fork it with: ssh %s@%s fork %s <new-name>\r\n",
			args[2], ControlUser, g.domainHint(), args[2])
		s.Exit(0) //nolint:errcheck
	case "rm":
		if len(args) < 2 {
			fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> snapshot rm <name>\r\n", ControlUser)
			s.Exit(2) //nolint:errcheck
			return
		}
		if err := g.mgr.DeleteSnapshot(s.Context(), args[1], user); err != nil {
			fail(s, log, "snapshot rm", err)
			return
		}
		fmt.Fprintf(s, "deleted snapshot %q\r\n", args[1])
		s.Exit(0) //nolint:errcheck
	default:
		fmt.Fprintf(s.Stderr(), "unknown snapshot command %q\r\n%s", sub, controlUsage)
		s.Exit(2) //nolint:errcheck
	}
}

// controlFork creates a new sandbox from one of the caller's snapshots.
func (g *Gateway) controlFork(s gssh.Session, user string, args []string, log *slog.Logger) {
	tags, rest, err := parseTags(args)
	if err != nil {
		fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
		s.Exit(2) //nolint:errcheck
		return
	}
	if len(rest) < 2 {
		fmt.Fprintf(s.Stderr(), "usage: ssh %s@<gateway> fork <snapshot> <new-name> [--tag <t>]…\r\n"+
			"       list your snapshots with: ssh %s@<gateway> snapshot list\r\n", ControlUser, ControlUser)
		s.Exit(2) //nolint:errcheck
		return
	}
	snapshot, name := rest[0], rest[1]
	ctx, cancel := context.WithTimeout(s.Context(), pauseTimeout)
	defer cancel()
	// Tags before Fork, for the same reason as create: the secret-env push is
	// kicked off by the fork and tags are what decide its contents.
	if err := g.applyTags(name, user, tags); err != nil {
		fail(s, log, "fork", err)
		return
	}
	if _, err := g.mgr.Fork(ctx, snapshot, name, user, 0, 0); err != nil {
		if len(tags) > 0 && g.tags != nil {
			g.tags.SetTags(name, user, nil) //nolint:errcheck // best-effort cleanup
		}
		fail(s, log, "fork", err)
		return
	}
	tagNote := ""
	if len(tags) > 0 {
		tagNote = fmt.Sprintf(" [tags: %s]", strings.Join(tags, ", "))
	}
	fmt.Fprintf(s, "created %s from snapshot %q%s — connect with: ssh %s@%s\r\n",
		name, snapshot, tagNote, name, g.domainHint())
	s.Exit(0) //nolint:errcheck
}

// controlTags shows or replaces a sandbox's tags. Tags select which of the
// owner's secrets are pushed into the sandbox's environment, so setting them
// re-syncs the box rather than waiting for its next resume.
func (g *Gateway) controlTags(s gssh.Session, user string, args []string, log *slog.Logger) {
	name, ok := g.ownedBoxArg(s, user, args)
	if !ok {
		return
	}
	if g.tags == nil {
		fail(s, log, "tags", errors.New("tagging is not enabled on this host"))
		return
	}
	// `tags <name>` reads; `tags <name> a b c` replaces the whole set, and
	// `tags <name> --clear` empties it (an empty list can't be typed otherwise).
	if len(args) == 2 {
		tags, err := g.tags.TagsFor(name)
		if err != nil {
			fail(s, log, "tags", err)
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
		parsed, rest, err := parseTags(args[2:])
		if err != nil {
			fmt.Fprintf(s.Stderr(), "sparkbox: %v\r\n", err)
			s.Exit(2) //nolint:errcheck
			return
		}
		// Bare words are tags too, so `tags box ml prod` works alongside --tag.
		want = dedupeTags(append(parsed, normalizeTags(rest)...))
	}
	if err := g.tags.SetTags(name, user, want); err != nil {
		fail(s, log, "tags", err)
		return
	}
	if len(want) == 0 {
		fmt.Fprintf(s, "cleared tags on %s\r\n", name)
	} else {
		fmt.Fprintf(s, "%s: %s\r\n", name, strings.Join(want, ", "))
	}
	// Secrets follow tags, so the box needs a re-push to match its new set.
	g.mgr.ResyncEnv(s.Context(), name)
	s.Exit(0) //nolint:errcheck
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
