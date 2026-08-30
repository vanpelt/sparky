package sshgw

// Sandbox tagging: the CLI grammar for `--tag` at creation.
//
// Tags decide which of an owner's secrets get pushed into a sandbox's
// environment, so stamping them is a create-time concern rather than a cosmetic
// one — and that stamping now lives in ctlops, which writes the rows before it
// calls Create so the asynchronous secret-env push sees them. What stays here is
// the part that is genuinely about ssh(1): pulling `--tag` out of an argument
// list nobody else will ever see.

import (
	"fmt"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// maxTagsPerSandbox bounds one create's tag list. It is ctlops' cap rather than
// a second opinion: a transport that parses flags must not be able to accept
// what the store layer will refuse, nor refuse what it would have taken.
const maxTagsPerSandbox = ctlops.MaxTagsPerSandbox

// parseTags pulls `--tag x` / `-t x` / `--tag=x` out of an argument list,
// returning the tags and whatever arguments were left. Comma-separated values
// are split, so `--tag ml,prod` and `--tag ml --tag prod` are the same thing;
// people write both. Tags are lowercased and de-duplicated because they are
// matched against secret tags, where "ML" and "ml" meaning different things
// would be a trap rather than a feature — all of which ctlops.NormalizeTags
// does, so the flag grammar is the only thing this function decides.
func parseTags(args []string) (tags, rest []string, err error) {
	var raw []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--tag" || a == "-t":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s needs a value, e.g. %s ml", a, a)
			}
			raw = append(raw, args[i+1])
			i++
		case strings.HasPrefix(a, "--tag="):
			raw = append(raw, strings.TrimPrefix(a, "--tag="))
		case strings.HasPrefix(a, "-t="):
			raw = append(raw, strings.TrimPrefix(a, "-t="))
		default:
			rest = append(rest, a)
		}
	}
	tags, err = ctlops.NormalizeTags(raw)
	if err != nil {
		return nil, nil, err
	}
	return tags, rest, nil
}

// parseCreateArgs is the whole argument grammar of a create: the tags, the
// machine to build on, and whatever is left.
//
// The fourth value is the point of the function. The new@ door reads every bare
// word as a tag (see Gateway.handle), so a flag this grammar does not recognise
// is not rejected — it is silently absorbed. `ssh new@gw -- --node dgx ml` with
// no --node case would create a sandbox HERE, tagged `--node` and `dgx`, and
// say nothing at all about the machine the user asked for. That is the failure
// this function exists to make impossible, and it is why the ctl@ doors call it
// too even though they take no --node: a flag that means something at one door
// and nothing at another has to say so.
//
// It is a wrapper rather than a fourth return value bolted onto parseTags
// because parseTags is the tag grammar and has exactly one job — the tests that
// pin it are a tripwire for that grammar changing shape, and they should not
// have to move for a flag that is not a tag.
func parseCreateArgs(args []string) (created, error) {
	node, rest, err := splitNodeFlag(args)
	if err != nil {
		return created{}, err
	}
	refs, rest, err := splitRefFlag(rest)
	if err != nil {
		return created{}, err
	}
	tags, rest, err := parseTags(rest)
	if err != nil {
		return created{}, err
	}
	return created{Tags: tags, Node: node, Refs: refs, Rest: rest}, nil
}

// created is what the grammar above resolved. A struct rather than four
// returns because the two []string are easy to swap at a call site and the
// compiler would not notice — `rest` becoming the tag list is a sandbox coming
// up tagged `--ref`.
type created struct {
	Tags []string
	Node string
	Refs []ctlops.RepoRef
	Rest []string
}

// splitRefFlag pulls `--ref` out, in the same two spellings and with the same
// consume-the-next-argument rule as the other two.
//
// Unlike --node it ACCUMULATES, because it can name a different branch for each
// attached repository: `--ref wandb/hivemind=feat/x --ref wandb/other=main`.
// The bare form `--ref feat/x` names no repository and is only unambiguous when
// the tags select exactly one — which this grammar cannot know and does not
// try to. ctlops.resolveRepoRefs is where that is refused, because that is
// where the attachments are.
func splitRefFlag(args []string) (refs []ctlops.RepoRef, rest []string, err error) {
	add := func(value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("--ref needs a value, e.g. --ref main or --ref owner/repo=main")
		}
		r := ctlops.ParseRepoRef(value)
		if strings.TrimSpace(r.Ref) == "" {
			return fmt.Errorf("--ref %s names no branch; write it as owner/repo=branch", value)
		}
		refs = append(refs, r)
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--ref":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("--ref needs a value, e.g. --ref main")
			}
			if err := add(args[i+1]); err != nil {
				return nil, nil, err
			}
			i++
		case strings.HasPrefix(a, "--ref="):
			if err := add(strings.TrimPrefix(a, "--ref=")); err != nil {
				return nil, nil, err
			}
		default:
			rest = append(rest, a)
		}
	}
	return refs, rest, nil
}

// splitNodeFlag pulls `--node x` / `--node=x` out of an argument list. Its shape
// deliberately mirrors parseTags': the same two spellings, the same
// consume-the-next-argument rule, and the same sentence when the value is
// missing — a door's flags should not each have their own dialect.
//
// The last --node wins, as the last --tag does not (tags accumulate). Repeating
// a flag that names one thing is a correction, not a list.
func splitNodeFlag(args []string) (node string, rest []string, err error) {
	seen := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--node":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s needs a value, e.g. %s dgx", a, a)
			}
			node, seen = args[i+1], true
			i++
		case strings.HasPrefix(a, "--node="):
			node, seen = strings.TrimPrefix(a, "--node="), true
		default:
			rest = append(rest, a)
		}
	}
	// `--node=` and `--node "  "` are the same mistake as leaving the value off
	// the end, and reading them as "no preference" would build the sandbox here
	// while the user waited to be told which machine it went to.
	if node = strings.TrimSpace(node); seen && node == "" {
		return "", nil, fmt.Errorf("--node needs a value, e.g. --node dgx")
	}
	return node, rest, nil
}
