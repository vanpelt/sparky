package allowlist

import (
	"strings"
	"testing"
)

func TestAllowed(t *testing.T) {
	l, err := New([]string{
		"# comment",
		"",
		"github.com",
		"*.googleapis.com",
		"Example.COM.", // mixed case + trailing dot, should normalise
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		want bool
	}{
		{"github.com", true},             // bare apex
		{"api.github.com", true},         // subdomain of bare rule
		{"a.b.github.com", true},         // deep subdomain
		{"GitHub.com", true},             // case-insensitive
		{"github.com.", true},            // trailing dot
		{"notgithub.com", false},         // suffix but not a subdomain boundary
		{"github.com.evil.com", false},   // apex appears as a label, not the parent
		{"example.com", true},            // normalised apex
		{"www.example.com", true},        // subdomain of normalised apex
		{"googleapis.com", false},        // wildcard does NOT match apex
		{"storage.googleapis.com", true}, // wildcard matches subdomain
		{"x.storage.googleapis.com", true},
		{"", false},
		{"com", false},
	}
	for _, c := range cases {
		got, pat := l.Allowed(c.name)
		if got != c.want {
			t.Errorf("Allowed(%q) = %v (pattern %q); want %v", c.name, got, pat, c.want)
		}
	}
}

func TestMatchedPattern(t *testing.T) {
	l, _ := New([]string{"github.com", "*.googleapis.com"})
	if _, pat := l.Allowed("api.github.com"); pat != "github.com" {
		t.Errorf("subdomain of bare rule: pattern = %q, want github.com", pat)
	}
	if _, pat := l.Allowed("storage.googleapis.com"); pat != "*.googleapis.com" {
		t.Errorf("wildcard: pattern = %q, want *.googleapis.com", pat)
	}
}

func TestNewErrors(t *testing.T) {
	for _, p := range []string{"*", "*.", "foo*bar.com", "localhost"} {
		if _, err := New([]string{p}); err == nil {
			t.Errorf("New(%q) expected error, got nil", p)
		}
	}
}

func TestReadAndPatterns(t *testing.T) {
	l, err := Read(strings.NewReader("github.com\n# note\n*.internal.example\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if l.Len() != 2 {
		t.Fatalf("Len = %d, want 2", l.Len())
	}
	got := strings.Join(l.Patterns(), ",")
	if got != "*.internal.example,github.com" {
		t.Errorf("Patterns = %q", got)
	}
}
