package machine

import (
	"slices"
	"strings"
	"testing"
)

func TestEncodeArgvRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		argv []string
	}{
		{"empty", nil},
		{"simple", []string{"setup", "--release", "v0.4.0"}},
		// Every one of these is a word bash would have mangled on the joined
		// command line, which is the entire reason this codec exists.
		{"spaces", []string{"--tls-email", "a b@example.com"}},
		{"dollar", []string{"--operator-handle", "$HOME"}},
		{"semicolon", []string{"--proxy-domain", "x;echo INJECT"}},
		{"glob", []string{"--kernel", "/srv/*"}},
		{"newline", []string{"--dns-answer", "1.2.3.4\n5.6.7.8"}},
		{"quotes", []string{"--node-name", `a'b"c`}},
		{"empty string element", []string{"--x", "", "--y"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			enc := EncodeArgv(tc.argv)
			if strings.ContainsAny(enc, "'\"$;|*\\ \n") {
				t.Fatalf("encoded form is not shell-inert: %q", enc)
			}
			got, err := DecodeArgv(enc)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.argv) {
				t.Errorf("round trip = %q, want %q", got, tc.argv)
			}
		})
	}
}

func TestDecodeArgvRejectsGarbage(t *testing.T) {
	if _, err := DecodeArgv("not base64!!"); err == nil {
		t.Error("expected an error for non-base64 input")
	}
	// A payload not ending in NUL means the blob was truncated in transit; the
	// guest catches the same thing with SPARKBOX_ARGV_N.
	if _, err := DecodeArgv("YWJj"); err == nil {
		t.Error("expected an error for a payload with no trailing NUL")
	}
}

// The guest snippet and the encoder have to agree about the format, and the
// only cheap way to assert that is to pin the pieces the agreement rests on.
func TestDecodeArgvSnippetMatchesTheFormat(t *testing.T) {
	for _, want := range []string{
		"base64 -d",                // the encoding
		"mapfile -d '' -t args",    // NUL-separated, matching EncodeArgv's separator
		"$SPARKBOX_ARGV_B64",       // the env key the payload rides in
		"$SPARKBOX_ARGV_N",         // the length integrity check
		"/run/sparkbox-inner-argv", // a file, so pipefail can catch a failed decode
	} {
		if !strings.Contains(DecodeArgvSnippet, want) {
			t.Errorf("DecodeArgvSnippet is missing %q:\n%s", want, DecodeArgvSnippet)
		}
	}
}
