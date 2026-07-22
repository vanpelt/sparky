// Package allowlist matches DNS names against an operator-supplied allowlist.
//
// Two pattern forms are supported, chosen to match how people actually think
// about an egress allowlist:
//
//	github.com      matches github.com AND any subdomain (api.github.com, …)
//	*.github.com    matches subdomains only, NOT the apex github.com
//
// A bare domain covering its subdomains is the common case ("let the VM reach
// GitHub"); the explicit wildcard is there for the rarer "subdomains but not
// the apex". Lines starting with '#' and blank lines are ignored, so an
// allowlist file reads like a hosts file.
package allowlist

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// List is an immutable, concurrency-safe set of allow rules. The zero value
// allows nothing; build one with New or Load.
type List struct {
	// exact[d] means d and *.d are allowed (bare-domain rule).
	exact map[string]struct{}
	// subOnly[d] means *.d is allowed but d itself is not (wildcard rule).
	subOnly map[string]struct{}
}

// New builds a List from a slice of patterns. Patterns are normalised
// (lower-cased, trailing dot stripped); an empty or comment pattern is skipped.
// A malformed pattern (e.g. a bare "*") is reported with its index.
func New(patterns []string) (*List, error) {
	l := &List{exact: map[string]struct{}{}, subOnly: map[string]struct{}{}}
	for i, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		if err := l.add(p); err != nil {
			return nil, fmt.Errorf("pattern %d (%q): %w", i, raw, err)
		}
	}
	return l, nil
}

func (l *List) add(p string) error {
	p = strings.ToLower(strings.TrimSuffix(p, "."))
	if strings.HasPrefix(p, "*.") {
		suffix := p[2:]
		if suffix == "" || strings.ContainsRune(suffix, '*') {
			return fmt.Errorf("wildcard must be of the form *.example.com")
		}
		l.subOnly[suffix] = struct{}{}
		return nil
	}
	if strings.ContainsRune(p, '*') {
		return fmt.Errorf("'*' is only allowed as a leading *. label")
	}
	if !strings.Contains(p, ".") {
		return fmt.Errorf("not a domain")
	}
	l.exact[p] = struct{}{}
	return nil
}

// Allowed reports whether name is permitted, and if so the pattern that
// matched (useful for logging). name is normalised before matching, so a
// trailing dot or mixed case does not matter.
func (l *List) Allowed(name string) (bool, string) {
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if name == "" {
		return false, ""
	}
	// Exact bare-domain hit.
	if _, ok := l.exact[name]; ok {
		return true, name
	}
	// Walk parent domains: a.b.example.com -> b.example.com -> example.com.
	// The first label is dropped each step; a match in `exact` covers the
	// bare-domain-plus-subdomains rule, a match in `subOnly` covers *.parent.
	for rest := name; ; {
		dot := strings.IndexByte(rest, '.')
		if dot < 0 {
			break
		}
		parent := rest[dot+1:]
		if _, ok := l.exact[parent]; ok {
			return true, parent
		}
		if _, ok := l.subOnly[parent]; ok {
			return true, "*." + parent
		}
		rest = parent
	}
	return false, ""
}

// Patterns returns the canonical patterns in the list, sorted, for display.
func (l *List) Patterns() []string {
	out := make([]string, 0, len(l.exact)+len(l.subOnly))
	for d := range l.exact {
		out = append(out, d)
	}
	for d := range l.subOnly {
		out = append(out, "*."+d)
	}
	sort.Strings(out)
	return out
}

// Len is the number of rules in the list.
func (l *List) Len() int { return len(l.exact) + len(l.subOnly) }

// Load reads patterns from an allowlist file, one per line.
func Load(path string) (*List, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f)
}

// Read parses an allowlist from r.
func Read(r io.Reader) (*List, error) {
	var lines []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return New(lines)
}
