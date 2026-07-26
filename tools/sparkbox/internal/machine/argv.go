package machine

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// EncodeArgv packs an argument vector into one opaque, shell-inert token.
//
// This is how `sparkbox setup`'s own flags reach the copy of sparkbox running
// inside the machine. They cannot travel as argv: the transport joins argv with
// spaces and re-parses it with bash, so a proxy domain, a TLS email or an
// operator handle would be word-split, glob-expanded and command-substituted on
// the way in. Base64 of the NUL-joined vector has no character bash reacts to,
// and the guest reassembles it with bash's NUL-delimited mapfile — the same
// discipline /proc/<pid>/cmdline uses, and for the same reason.
func EncodeArgv(argv []string) string {
	var b strings.Builder
	for _, a := range argv {
		b.WriteString(a)
		b.WriteByte(0)
	}
	return base64.StdEncoding.EncodeToString([]byte(b.String()))
}

// DecodeArgv is EncodeArgv's inverse. Nothing in production calls it — the
// guest's bash does the decoding — but a round-trip test is the cheapest
// possible proof that the encoder and DecodeArgvSnippet agree about the format.
func DecodeArgv(s string) ([]string, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode argv: %w", err)
	}
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[len(raw)-1] != 0 {
		return nil, fmt.Errorf("decode argv: payload does not end with NUL")
	}
	parts := strings.Split(string(raw[:len(raw)-1]), "\x00")
	return parts, nil
}

// DecodeArgvSnippet is the guest-side half, kept beside the encoder as a const
// so the two cannot drift.
//
// It goes through a FILE rather than a process substitution deliberately: under
// `set -o pipefail` a failed `base64 -d` in a pipeline is caught, whereas a
// failure inside <(...) is not, and silently decoding to an empty vector would
// run `sparkbox setup` with no flags at all. SPARKBOX_ARGV_N is a cheap,
// decisive integrity check on the length — one integer that proves the whole
// vector survived.
const DecodeArgvSnippet = `printf '%s' "$SPARKBOX_ARGV_B64" | base64 -d > /run/sparkbox-inner-argv
mapfile -d '' -t args < /run/sparkbox-inner-argv
rm -f /run/sparkbox-inner-argv
if [ "${#args[@]}" -ne "$SPARKBOX_ARGV_N" ]; then
  echo "argv did not survive the transport: expected $SPARKBOX_ARGV_N words, got ${#args[@]}" >&2
  exit 90
fi
`
