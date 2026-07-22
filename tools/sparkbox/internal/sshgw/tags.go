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
