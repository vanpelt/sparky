package reserved

import "regexp"

// labelRe is the platform's one name charset: lowercase, digits and hyphens,
// starting with a letter or a digit, at most 63 characters. It is a DNS label's
// rules because every name it governs becomes one — a sandbox name is its
// default subdomain, and a node name is the middle label of the synthetic
// <sandbox>.<node>.sandbox.invalid addresses the router mints.
var labelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidLabel reports whether s is usable as one of those names.
//
// It lives beside the reserved list for the same reason that list does: this
// rule was written out three times — host's create-time check, the node
// roster's, and the browser terminal's hand-rolled loop over the same charset —
// and three copies of a validation rule drift into a name one door accepts and
// another rejects. Asking one predicate is what makes "the manager would accept
// this" and "the edge would parse this" the same sentence.
//
// It is a separate question from Name: this is whether the name is well formed,
// Name is whether the platform has already claimed it.
func ValidLabel(s string) bool { return labelRe.MatchString(s) }
