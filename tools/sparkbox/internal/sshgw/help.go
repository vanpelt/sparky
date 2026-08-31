package sshgw

// The `ctl@` channel's help. It used to be one 90-line constant printed for
// every mistake anybody made, which meant the listing that teaches the channel
// was also the punishment for a typo, and neither job was done well.
//
// It is two things now: a compact index of topics that fits in a terminal
// without scrolling, and one detailed page per topic reached with
// `help <topic>`. Every wrong-subcommand path prints the page for the group it
// is in rather than the whole surface, so `snapshot wat` answers with
// snapshots.
//
// The operator commands are filtered out of both for everyone else. That is
// presentation, never policy: ctlops resolves the operator bit from the account
// store on every one of those calls (see ctlops.operatorOnly), so hiding a line
// here changes what a user reads and nothing about what they may do.

import (
	"fmt"
	"strings"

	gssh "github.com/gliderlabs/ssh"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// helpTopic is one group of commands: the row that names it in the index, and
// the page `help <topic>` prints.
//
// verbs is deliberately a hand-written string rather than a list joined at
// render time — the index is a teaser, so which four of a group's nine verbs
// earn a place there is an editorial decision, not a slice.
type helpTopic struct {
	name    string   // the word `help <topic>` takes
	aliases []string // other words that reach this page — command names, mostly
	verbs   string   // the index's second column
	blurb   string   // the index's third column; empty is fine
	page    string   // what `help <topic>` prints
	// operator hides the row and the page from non-operators. See the file
	// header: this is what a user reads, not what they may run.
	operator bool
}

// helpTopics is the whole surface, in the order the index lists them. It is a
// function rather than a package var because the pages interpolate ControlUser,
// and a var would fix that at init in a package that also defines it.
func helpTopics() []helpTopic {
	return []helpTopic{{
		name:    "sandboxes",
		aliases: []string{"sandbox", "ls", "list", "new", "pause", "rename", "mv", "rm", "tags", "pin", "unpin", "sessions", "archive", "restore", "checkpoint", "resize"},
		verbs:   "ls · pause · rename · rm · tags",
		blurb:   "plus pin and the disk verbs",
		page:    sandboxHelp,
	}, {
		name:    "secrets",
		aliases: []string{"secret"},
		verbs:   "secret ls · set · rm",
		blurb:   "env vars your sandboxes get",
		page:    secretUsage,
	}, {
		name:    "repos",
		aliases: []string{"repo"},
		verbs:   "repo ls · add · rm · check",
		blurb:   "cloned in before you arrive",
		page:    repoUsage,
	}, {
		// A topic of its own, which it was not: `help badge` used to land on the
		// repos page and every mistyped `badge` printed it — a page about
		// attaching repositories, forks and ref overrides, for somebody who
		// forgot a slug. The row costs the index its tenth line and stays
		// inside 80 columns, which is what
		// TestControlHelpHidesOperatorTopics holds it to.
		name:  "badge",
		verbs: "badge <owner>/<name>",
		blurb: "a button for a PR comment",
		page:  badgeUsage,
	}, {
		name:    "snapshots",
		aliases: []string{"snapshot", "fork", "bind", "unbind"},
		// 34 runes, exactly helpVerbWidth, so the row still lands inside 80 once
		// its blurb is appended — TestControlHelpHidesOperatorTopics is what
		// says so, and it is why `rm` gave up its place to `bind`.
		verbs: "snapshot ls · create · bind · fork",
		blurb: "disk templates to boot from",
		page:  snapshotHelp,
	}, {
		name:    "schedule",
		aliases: []string{"cron"},
		verbs:   "schedule ls · add · rm",
		blurb:   "cron that wakes a sandbox",
		page:    scheduleHelp,
	}, {
		name:    "sharing",
		aliases: []string{"share", "session-token", "url", "urls"},
		verbs:   "share · session-token",
		blurb:   "who can reach a sandbox's URLs",
		page:    sharingHelp,
	}, {
		name:    "account",
		aliases: []string{"whoami", "keys", "key", "github", "passkey", "passkeys", "email", "invite"},
		verbs:   "whoami · keys · github · passkey",
		blurb:   "you, and the keys that are you",
		page:    accountHelp,
	}, {
		name:     "users",
		aliases:  []string{"user"},
		verbs:    "user ls · add · sync-github-org",
		blurb:    "admit people to this host",
		page:     userUsage,
		operator: true,
	}, {
		name:     "nodes",
		aliases:  []string{"node", "fleet"},
		verbs:    "node ls · approve · rm",
		blurb:    "the machines this fleet runs",
		page:     nodeHelp,
		operator: true,
	}}
}

// ---------------------------------------------------------------------------
// The index
// ---------------------------------------------------------------------------

// helpIndexWidth is the column the verbs start in and helpVerbWidth the one the
// blurbs do. Together with the two-space indent they leave a 30-column blurb
// inside 80, which is what TestControlHelpHidesOperatorTopics holds them to:
// this listing is read in a terminal somebody has not resized for it.
const (
	helpIndexWidth = 12
	helpVerbWidth  = 34
)

// controlHelp renders the index. operator adds the rows only an operator can
// act on.
func controlHelp(operator bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "usage: ssh %s@<gateway> <command>\r\n", ControlUser)
	b.WriteString("\r\n")
	// Creating a sandbox is the one thing that is not a ctl command at all —
	// it is a different user at the same door — so it leads, outside the index.
	b.WriteString("  ssh new@<gateway> [<tag>…]      a new sandbox, named for you, connected\r\n")
	b.WriteString("  ssh new+<name>@<gateway>        the same, named by you\r\n")
	b.WriteString("\r\n")
	for _, t := range helpTopics() {
		if t.operator && !operator {
			continue
		}
		line := fmt.Sprintf("  %-*s %s", helpIndexWidth, t.name, t.verbs)
		if t.blurb != "" {
			// Padded to a third column so the blurbs line up; a row whose verbs
			// overflow it just pushes its own blurb right rather than wrapping,
			// which a terminal handles better than we would.
			line = fmt.Sprintf("  %-*s %-*s %s", helpIndexWidth, t.name, helpVerbWidth, t.verbs, t.blurb)
		}
		b.WriteString(strings.TrimRight(line, " ") + "\r\n")
	}
	b.WriteString("\r\n")
	fmt.Fprintf(&b, "  %-*s ssh %s@<gateway> help <topic>, e.g. `help secrets`\r\n",
		helpIndexWidth, "help", ControlUser)
	b.WriteString("\r\n")
	b.WriteString("  the same sandboxes without ssh:\r\n")
	b.WriteString("    a shell in a browser tab     https://<name>-xterm.<domain>\r\n")
	b.WriteString("    these commands over HTTP     https://api.<domain>  — docs at /docs\r\n")
	return b.String()
}

// helpPage resolves a topic word — its name, or any of its aliases, or a bare
// command name — to the page that documents it.
func helpPage(topic string, operator bool) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(topic))
	for _, t := range helpTopics() {
		if t.operator && !operator {
			continue
		}
		if t.name == want {
			return t.page, true
		}
		for _, a := range t.aliases {
			if a == want {
				return t.page, true
			}
		}
	}
	return "", false
}

// pageFor is helpPage for the wrong-subcommand paths, which know their group by
// name and have nothing sensible to do if it is missing. The operator pages are
// reached unconditionally here: somebody who just typed `node wat` is reading
// the node listing's refusal either way, and answering a typo with a smaller
// help page than the command itself would print is a puzzle, not a kindness.
func pageFor(topic string) string {
	page, ok := helpPage(topic, true)
	if !ok {
		return controlHelp(false)
	}
	return page
}

// ---------------------------------------------------------------------------
// The pages
// ---------------------------------------------------------------------------

const sandboxHelp = "usage: ssh ctl@<gateway> <command> <name>\r\n" +
	"\r\n" +
	" creating one\r\n" +
	"  ssh new@<gateway> [<tag>…]     create one and connect; the words are tags\r\n" +
	"  ssh new+<name>@<gateway>       the same, but you choose the name\r\n" +
	"     the words after new@ are tags, never a command to run — a fresh sandbox\r\n" +
	"     always gets a shell. Passing any word makes ssh skip the terminal, so use\r\n" +
	"     `ssh -t new+<name>@<gateway> <tag>…` or you get a shell with no prompt.\r\n" +
	"  fork <snapshot> <name> [--tag <t>]… [--ref <branch>]…\r\n" +
	"                                 create one from a snapshot you saved\r\n" +
	"     a tag you have bound a snapshot to boots from it, so `ssh new@<gateway>\r\n" +
	"     <tag>` is a fork you do not have to remember — see `help snapshots`.\r\n" +
	"     --ref names the branch this ONE sandbox's checkout starts on, whatever\r\n" +
	"     the attachment says: `--ref feat/x`, or `--ref <owner>/<repo>=feat/x`\r\n" +
	"     when more than one repository is attached. see `help repos`.\r\n" +
	"\r\n" +
	" living with one\r\n" +
	"  ls                       list your sandboxes and their state\r\n" +
	"  rename <name> <new>      rename it — the URLs and the ssh name move with it\r\n" +
	"  pause <name>             pause a running sandbox to free a slot\r\n" +
	"  rm <name>                delete a sandbox and its disk — permanent, see archive\r\n" +
	"  tags <name> [<tag>…]     show or set tags (they select secrets and repos)\r\n" +
	"  sessions <name>          the HiveMind agent sessions recorded from a sandbox\r\n" +
	"  pin <name>               keep a sandbox always-on (in-VM cron/daemons run)\r\n" +
	"  unpin <name>             let a sandbox pause when idle again\r\n" +
	"\r\n" +
	" its disk\r\n" +
	"  archive <name>           park a sandbox in object storage (frees host disk)\r\n" +
	"  restore <name>           bring an archived sandbox back and start it\r\n" +
	"  checkpoint <name>        save a durable disk checkpoint (cold-boots it)\r\n" +
	"  checkpoint restore <name>  replace the local disk with its latest checkpoint\r\n" +
	"  resize <name> <size>     grow a sandbox's root disk, e.g. 25G (cold-boots it)\r\n" +
	"\r\n" +
	"renaming and resizing pause the sandbox first and it cold-boots after, so\r\n" +
	"processes running inside do not survive either one.\r\n"

const snapshotHelp = "usage: ssh ctl@<gateway> snapshot ls\r\n" +
	"       ssh ctl@<gateway> snapshot create <box> <name> [--tag <tag>]\r\n" +
	"       ssh ctl@<gateway> snapshot rm <name>\r\n" +
	"       ssh ctl@<gateway> snapshot bind <name> --tag <tag>\r\n" +
	"       ssh ctl@<gateway> snapshot unbind --tag <tag>\r\n" +
	"       ssh ctl@<gateway> fork <snapshot> <new-name> [--tag <t>]…\r\n" +
	"\r\n" +
	"a snapshot is a disk template: `create` pauses a sandbox and compacts its disk\r\n" +
	"into one, `fork` boots a brand-new sandbox from it. the fork happens on the\r\n" +
	"machine holding the template, so it takes no --node.\r\n" +
	"\r\n" +
	"the fork gets the tags you give it, not the ones the original had — tags are\r\n" +
	"what select the secrets and repos it will be handed.\r\n" +
	"\r\n" +
	"`bind` makes a snapshot the base disk of a tag, so `ssh new@<gateway> <tag>`\r\n" +
	"boots from it instead of the stock image — a fork you don't have to remember.\r\n" +
	"a tag has exactly one base image, so binding again re-points it and says what\r\n" +
	"it replaced, and a create whose tags bind two different snapshots is refused\r\n" +
	"rather than guessed at: a sandbox has one disk. `default` cannot be bound —\r\n" +
	"every sandbox you make carries it, so the binding would reach all of them.\r\n" +
	"`snapshot ls` shows which of your snapshots are bound, and `unbind` takes the\r\n" +
	"binding away without deleting anything.\r\n" +
	"\r\n" +
	"`create --tag <t>` does both halves as one operation, which is also what\r\n" +
	"`sparkbox snapshot <t>` runs from INSIDE a sandbox: it prints what it is about\r\n" +
	"to re-point, asks, then pauses that box and captures it. a sandbox may only\r\n" +
	"re-point a tag it already carries. if the capture succeeds and the binding\r\n" +
	"fails, the snapshot is kept and the message carries the one `bind` that\r\n" +
	"finishes it — a re-run would capture a different disk.\r\n" +
	"\r\n" +
	"a capture also carries the port the sandbox was serving on, so a box created\r\n" +
	"from the template answers on its own URL with nothing to set up. `snapshot ls`\r\n" +
	"prints that port on the rows that have one; a template captured from a box on\r\n" +
	"the stock port prints none, and its sandboxes get the stock port. `sparkbox\r\n" +
	"port <n>` from inside a sandbox moves it either way.\r\n" +
	"\r\n" +
	"`create` brings the agent CLIs up to date before it captures, so a template\r\n" +
	"does not start life frozen at the versions of the day you took it — inside a\r\n" +
	"sandbox, `sparkbox update-tools` does the same on demand. a sandbox that is\r\n" +
	"already paused on a multi-machine deployment is captured as it is: waking it\r\n" +
	"is a bigger act than the one you asked for.\r\n"

const scheduleHelp = "usage: ssh ctl@<gateway> schedule ls\r\n" +
	"       ssh ctl@<gateway> schedule add <box> \"<cron>\" <command>\r\n" +
	"       ssh ctl@<gateway> schedule rm <id>\r\n" +
	"\r\n" +
	"the host fires the job by waking the sandbox first, so periodic work still runs\r\n" +
	"on a box that pauses when idle — which in-VM cron cannot do.\r\n" +
	"\r\n" +
	"  ssh ctl@<gateway> schedule add mybox \"*/30 * * * *\" /usr/local/bin/sync\r\n"

const sharingHelp = "usage: ssh ctl@<gateway> share <name> [public|private]\r\n" +
	"       ssh ctl@<gateway> session-token [--ttl <dur>]\r\n" +
	"\r\n" +
	"  share <name>                   who can reach this sandbox's URLs today\r\n" +
	"  share <name> public            anyone with the URL can reach it\r\n" +
	"  share <name> private           visitors must sign in and own it (the default)\r\n" +
	"  session-token [--ttl <dur>]    mint a browser/API token for private URLs\r\n" +
	"\r\n" +
	"visibility is per sandbox: every route pointing at it flips together.\r\n" +
	"\r\n" +
	"  TOKEN=$(ssh ctl@<gateway> session-token | tr -d '\\r\\n')\r\n" +
	"  curl -H \"Authorization: Bearer $TOKEN\" https://api.<domain>/v1/sandboxes\r\n"

const accountHelp = "usage: ssh ctl@<gateway> <command>\r\n" +
	"\r\n" +
	"  whoami                   show your account and linked identities\r\n" +
	"  keys ls                  list the SSH keys on your account\r\n" +
	"  keys add \"<key line>\"    link another key\r\n" +
	"  keys rm <SHA256:...>     unlink a key (never the last one)\r\n" +
	"  keys import-github       adopt every key github.com lists for your login\r\n" +
	"  keys verify-github       link by proving this key is published on GitHub\r\n" +
	"  github link [<login>]    link your GitHub account\r\n" +
	"  github install           install the GitHub App on the repos you want\r\n" +
	"  passkey ls               list the passkeys enrolled from your browsers\r\n" +
	"  passkey rm <id>          remove a passkey (id or unique prefix from the list)\r\n" +
	"  email [set <addr>|clear] show or set the email forwarded to private apps\r\n" +
	"  invite                   mint a single-use invite code\r\n" +
	"\r\n" +
	"linking GitHub is what lets a sandbox clone your private repositories — see\r\n" +
	"`help repos` — and what `keys import-github` reads to save you pasting keys.\r\n"

const nodeHelp = "usage: ssh ctl@<gateway> node ls\r\n" +
	"       ssh ctl@<gateway> node approve <SHA256:...> --guest-subnet <CIDR> [--grpc-addr <host:port>]\r\n" +
	"       ssh ctl@<gateway> node rm <name>\r\n" +
	"\r\n" +
	"a machine enrols itself and then waits. read its fingerprint off `node ls`,\r\n" +
	"compare it out of band, and approve it — approval reserves the guest network\r\n" +
	"that machine may address, which is why it is not optional.\r\n" +
	"\r\n" +
	"`rm` revokes the approval rather than banning the key: the machine may enrol\r\n" +
	"again and wait to be approved a second time.\r\n"

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

// isOperator resolves the caller's operator bit for presentation only. An
// unreadable account answers false, which shows the smaller listing rather than
// failing the command: help that half-works beats help that errors.
func (g *Gateway) isOperator(s gssh.Session, c ctlops.Caller) bool {
	me, err := g.ops.Whoami(s.Context(), c)
	return err == nil && me.Operator
}

// controlHelpCmd serves `help` and `help <topic>`.
func (g *Gateway) controlHelpCmd(s gssh.Session, c ctlops.Caller, args []string) {
	operator := g.isOperator(s, c)
	if len(args) == 0 {
		fmt.Fprint(s, controlHelp(operator))
		s.Exit(0) //nolint:errcheck
		return
	}
	page, ok := helpPage(args[0], operator)
	if !ok {
		// The index, not the missing page: whoever typed a topic that is not
		// one needs to see the topics.
		fmt.Fprintf(s.Stderr(), "no help topic %q\r\n%s", args[0], controlHelp(operator))
		s.Exit(2) //nolint:errcheck
		return
	}
	fmt.Fprint(s, page)
	s.Exit(0) //nolint:errcheck
}
