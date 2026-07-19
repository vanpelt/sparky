package sshgw

// Sandbox tagging: parsing `--tag` at creation and stamping the rows.
//
// Tags decide which of an owner's secrets get pushed into a sandbox's
// environment, so stamping them is a create-time concern rather than a
// cosmetic one — see applyTags for the ordering that makes that work.

import (
	"fmt"
	"sort"
	"strings"
)

// maxTagsPerSandbox bounds one create's tag list. Generous for real use and
// small enough that a runaway argument list can't write unbounded rows.
const maxTagsPerSandbox = 32

// parseTags pulls `--tag x` / `-t x` / `--tag=x` out of an argument list,
// returning the tags and whatever arguments were left. Comma-separated values
// are split, so `--tag ml,prod` and `--tag ml --tag prod` are the same thing;
// people write both. Tags are lowercased and de-duplicated because they are
// matched against secret tags, where "ML" and "ml" meaning different things
// would be a trap rather than a feature.
func parseTags(args []string) (tags, rest []string, err error) {
	add := func(v string) {
		for _, t := range strings.Split(v, ",") {
			if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
				tags = append(tags, t)
			}
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--tag" || a == "-t":
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s needs a value, e.g. %s ml", a, a)
			}
			add(args[i+1])
			i++
		case strings.HasPrefix(a, "--tag="):
			add(strings.TrimPrefix(a, "--tag="))
		case strings.HasPrefix(a, "-t="):
			add(strings.TrimPrefix(a, "-t="))
		default:
			rest = append(rest, a)
		}
	}
	tags = dedupeTags(tags)
	if len(tags) > maxTagsPerSandbox {
		return nil, nil, fmt.Errorf("too many tags (%d); limit is %d", len(tags), maxTagsPerSandbox)
	}
	return tags, rest, nil
}

// dedupeTags sorts and uniques a tag list so the stored set is stable
// regardless of the order they were typed in.
func dedupeTags(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// applyTags stamps a sandbox's tags. It is called with the name the sandbox is
// ABOUT to be created under, before Create — deliberately, because Create kicks
// off the secret-env push asynchronously. Setting tags afterwards would race
// that push and, more often than not, lose: the box would come up without the
// secrets its tags select, and only pick them up on some later resume.
//
// Best-effort by design: a tagging failure must not cost the user the sandbox
// they asked for, so callers log and carry on.
func (g *Gateway) applyTags(sandbox, owner string, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	if g.tags == nil {
		return fmt.Errorf("tagging is not enabled on this host")
	}
	return g.tags.SetTags(sandbox, owner, tags)
}

// normalizeTags lowercases, splits commas, and drops blanks from bare tag
// words, so `tags box ML,prod` and `tags box ml prod` agree.
func normalizeTags(in []string) []string {
	var out []string
	for _, v := range in {
		for _, t := range strings.Split(v, ",") {
			if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}
