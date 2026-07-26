package machine

import (
	"errors"
	"strings"
	"testing"
)

func TestWrapScriptAlwaysPrependsSetEuoPipefail(t *testing.T) {
	got := WrapScript("echo hi", "abc123")
	want := "set -euo pipefail\n" +
		"trap 'rc=$?; printf \"\\nabc123 rc=%d\\n\" \"$rc\"' EXIT\n" +
		"printf '%s begin\\n' 'abc123'\n" +
		"echo hi\n"
	if got != want {
		t.Errorf("WrapScript:\n got %q\nwant %q", got, want)
	}
	// A body that already ends in a newline must not gain a second one — the
	// golden script files compare byte for byte.
	if g := WrapScript("echo hi\n", "abc123"); g != want {
		t.Errorf("trailing newline not normalised: %q", g)
	}
}

func TestStripReceipt(t *testing.T) {
	const n = "deadbeef"
	tests := []struct {
		name        string
		stdout      string
		wantClean   string
		wantBegin   bool
		wantTrailer bool
		wantRC      int
	}{
		{
			name:      "clean run",
			stdout:    n + " begin\nhello\nworld\n\n" + n + " rc=0\n",
			wantClean: "hello\nworld\n", wantBegin: true, wantTrailer: true, wantRC: 0,
		},
		{
			// The measured "no -i" shape: stdin discarded, shell exits 0 with
			// nothing to show for it.
			name:   "missing begin",
			stdout: "",
		},
		{
			name:      "missing trailer (truncated / signalled)",
			stdout:    n + " begin\npartial output\n",
			wantClean: "partial output\n", wantBegin: true,
		},
		{
			name:      "non-zero rc",
			stdout:    n + " begin\nboom\n\n" + n + " rc=2\n",
			wantClean: "boom\n", wantBegin: true, wantTrailer: true, wantRC: 2,
		},
		{
			// A body that PRINTS receipt-looking text with a different nonce
			// must survive untouched; only our own marker is stripped.
			name:      "marker-looking text in the body",
			stdout:    n + " begin\ncafebabe begin\ncafebabe rc=99\n\n" + n + " rc=0\n",
			wantClean: "cafebabe begin\ncafebabe rc=99\n", wantBegin: true, wantTrailer: true,
		},
		{
			name:      "no trailing newline from the body",
			stdout:    n + " begin\nno-newline\n" + n + " rc=7\n",
			wantClean: "no-newline\n", wantBegin: true, wantTrailer: true, wantRC: 7,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clean, r := StripReceipt([]byte(tc.stdout), n)
			if string(clean) != tc.wantClean {
				t.Errorf("clean = %q, want %q", clean, tc.wantClean)
			}
			if r.Begin != tc.wantBegin || r.Trailer != tc.wantTrailer || r.RC != tc.wantRC {
				t.Errorf("receipt = %+v, want begin=%v trailer=%v rc=%d", r, tc.wantBegin, tc.wantTrailer, tc.wantRC)
			}
		})
	}
}

// TestVerdict is the regression test this whole workstream exists for: every
// way the transport can report success over work that did not happen must come
// back as an error.
func TestVerdict(t *testing.T) {
	const n = "deadbeef"
	tests := []struct {
		name     string
		exit     int
		stdout   string
		wantErr  error
		wantCode int // for the ExitError rows
	}{
		{name: "clean success", exit: 0, stdout: n + " begin\nok\n\n" + n + " rc=0\n"},
		{
			name: "exit 0 with no receipt at all", exit: 0, stdout: "",
			wantErr: ErrTransport,
		},
		{
			name: "exit 0 with output but no begin (argv mangled)", exit: 0, stdout: "some fragment ran\n",
			wantErr: ErrTransport,
		},
		{
			name: "began but no trailer", exit: 0, stdout: n + " begin\nhalf\n",
			wantErr: ErrTransport,
		},
		{
			name: "trailer disagrees with the process exit", exit: 0,
			stdout:  n + " begin\n\n" + n + " rc=7\n",
			wantErr: ErrTransport,
		},
		{
			name: "honest inner failure", exit: 2, stdout: n + " begin\nboom\n\n" + n + " rc=2\n",
			wantCode: 2,
		},
		{
			name: "inner binary missing (127)", exit: 127, stdout: n + " begin\n\n" + n + " rc=127\n",
			wantCode: 127,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verdict("inner-setup", n, tc.exit, []byte(tc.stdout), nil)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantCode != 0:
				var ee *ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("err = %v, want *ExitError", err)
				}
				if ee.Code != tc.wantCode {
					t.Errorf("code = %d, want %d", ee.Code, tc.wantCode)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestValidateExec(t *testing.T) {
	tests := []struct {
		name string
		spec ExecSpec
		ok   bool
	}{
		{"good", ExecSpec{Machine: "sparkbox", Op: "probe", Env: map[string]string{"SPARKBOX_X": "v"}}, true},
		// An empty name is not merely invalid, it is dangerous: `machine
		// inspect`/`stop` with no id silently target the DEFAULT machine.
		{"empty name", ExecSpec{Machine: "", Op: "probe"}, false},
		{"upper-case name", ExecSpec{Machine: "Sparkbox", Op: "probe"}, false},
		{"name with a shell metacharacter", ExecSpec{Machine: "spark;box", Op: "probe"}, false},
		{"no op", ExecSpec{Machine: "sparkbox", Op: ""}, false},
		{"lower-case env key", ExecSpec{Machine: "sparkbox", Op: "p", Env: map[string]string{"x": "v"}}, false},
		{"env key with a space", ExecSpec{Machine: "sparkbox", Op: "p", Env: map[string]string{"A B": "v"}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateExec(tc.spec)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateExec = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestNonceIsUniquePerCall(t *testing.T) {
	a, b := NewNonce(), NewNonce()
	if a == b {
		t.Fatalf("two nonces are equal: %q", a)
	}
	if len(a) != 32 || strings.ContainsAny(a, "'\"$;|*\\ ") {
		t.Errorf("nonce %q must be shell-inert hex", a)
	}
}
