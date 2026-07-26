package machine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxScriptBytes caps the wrapped script we hand to the transport's stdin.
//
// The number is measured, not chosen for tidiness: on Apple Container 1.1.0 a
// single stdin payload round-trips byte-exact at 64 KiB and 128 KiB and
// DEADLOCKS at 192 KiB and 1 MiB — the CLI stops draining, the guest process
// never sees EOF, and the hang survives a SIGTERM aimed at the pipeline. So
// this is refused BEFORE the process starts. Context cancellation is not a
// substitute for the cap: a wedged `container` child is manual cleanup for the
// user, and cancelling a pipe nobody is reading changes nothing.
//
// Nothing sparkbox sends is close to this. The largest payload is the inner
// setup invocation, a few hundred bytes; binaries are fetched by the machine
// itself from a URL and checksum the Mac pinned, never pushed.
const MaxScriptBytes = 64 << 10

// DefaultExecTimeout bounds a guest call that names no timeout of its own.
const DefaultExecTimeout = 30 * time.Minute

// envKeyRe is the env-name rule. `-e NAME` inherit-by-name is the safe channel
// for values (measured byte-exact against a hostile value), but the NAME half
// still lands on the joined command line, so it is restricted to characters
// bash cannot interpret.
var envKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// NewNonce mints a per-call receipt marker: 16 random bytes, hex.
func NewNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice, and a receipt that is merely
		// predictable is still a receipt — it defends against a script that did
		// not run, not against an adversary inside the machine.
		return "sparkboxreceiptfallback000000000"
	}
	return hex.EncodeToString(b[:])
}

// WrapScript builds the payload actually sent to the guest shell.
//
// # Why every script gets a receipt
//
// The transport hands back a SHELL's exit status, and there are at least three
// measured ways for that status to be 0 over work that did not happen:
//
//  1. `-i` omitted: stdin is silently discarded, the guest shell reads EOF
//     immediately, and the call exits 0 having run nothing.
//  2. argv mangled by the bash -c join (see doc.go): a fragment runs instead of
//     the intended command, and the fragment often succeeds.
//  3. a multi-statement body without `set -e`: the status is the LAST
//     statement's. Measured: the identical two-line script returns 2 with
//     `set -euo pipefail` and 0 without, printing the same failure both times.
//
// So the library — not the call site — prepends `set -euo pipefail`, prints a
// `begin` line the moment the shell is actually reading our bytes, and installs
// an EXIT trap that prints the real status last. The trap fires for a `set -e`
// abort as well as for a normal exit, so its ABSENCE means truncation or a
// signal, which is a transport fault rather than a script failure.
func WrapScript(body, nonce string) string {
	var b strings.Builder
	b.WriteString("set -euo pipefail\n")
	// Single-quoted trap body so nothing expands until the trap fires; the
	// nonce is hex, so it can never contain a quote.
	b.WriteString("trap 'rc=$?; printf \"\\n" + nonce + " rc=%d\\n\" \"$rc\"' EXIT\n")
	b.WriteString("printf '%s begin\\n' '" + nonce + "'\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// Receipt is what StripReceipt found in a guest's stdout.
type Receipt struct {
	Begin   bool // the shell was reading our bytes
	Trailer bool // the EXIT trap fired
	RC      int  // the status the trap reported
}

// StripReceipt removes the receipt lines from stdout and reports what it found.
// Pure function, and the most heavily unit-tested thing in this package.
func StripReceipt(stdout []byte, nonce string) (clean []byte, r Receipt) {
	beginLine := nonce + " begin"
	rcPrefix := nonce + " rc="
	lines := strings.Split(string(stdout), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		switch {
		case t == beginLine:
			r.Begin = true
			continue
		case strings.HasPrefix(t, rcPrefix):
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(t, rcPrefix))); err == nil {
				r.RC = n
				r.Trailer = true
				continue
			}
		}
		kept = append(kept, line)
	}
	// The trap prints a leading newline so its marker always starts a line even
	// when the body's last write had no trailing newline; that blank line is
	// ours, not the script's, so drop one trailing empty line beyond the usual
	// final-newline artefact.
	for len(kept) > 0 && kept[len(kept)-1] == "" {
		kept = kept[:len(kept)-1]
	}
	out := strings.Join(kept, "\n")
	if out != "" {
		out += "\n"
	}
	return []byte(out), r
}

// ValidateExec rejects a spec the transport cannot carry faithfully. Called
// before anything is spawned, so a bad name or env key costs nothing.
func ValidateExec(s ExecSpec) error {
	if !ValidName(s.Machine) {
		// Not pedantry: `container machine inspect`/`stop`/`logs` take the id as
		// an OPTIONAL argument and silently fall back to the DEFAULT machine, so
		// an empty name here would operate on somebody else's VM.
		return fmt.Errorf("invalid machine name %q: lowercase letters, digits and hyphens only, "+
			"starting with a letter or digit, at most 63 characters", s.Machine)
	}
	if strings.TrimSpace(s.Op) == "" {
		return fmt.Errorf("exec spec has no Op (it names the log line, the evidence file and the test fixture)")
	}
	for k := range s.Env {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("invalid env key %q: must match %s (the NAME half rides the guest command line, "+
				"which bash re-parses)", k, envKeyRe)
		}
	}
	return nil
}

// Verdict turns a raw transport result into an ExecResult, applying the four
// conditions that together mean "the machine really ran our script and this is
// really what happened":
//
//  1. the process exited 0;
//  2. the `begin` line appeared — the ONLY signal separating a real run from
//     the measured "stdin discarded, exit 0" mode and from an argv-mangled
//     fragment;
//  3. the `rc=` trailer appeared — the EXIT trap fires for `set -e` aborts and
//     normal exits alike, so its absence means truncation or a signal;
//  4. the trailer's rc equals the process exit code. These agreed on CLI 1.1.0
//     for 0/1/2/7/127/137/255; the point is that correctness no longer DEPENDS
//     on them agreeing.
//
// A violation of 2, 3 or 4 is ErrTransport even at exit 0 — reported as "the
// machine did not run the script", never as success and never as an inner
// failure. A clean receipt with a non-zero code is an *ExitError.
func Verdict(op string, nonce string, procExit int, stdout, stderr []byte) (ExecResult, error) {
	clean, r := StripReceipt(stdout, nonce)
	res := ExecResult{ExitCode: procExit, Stdout: clean, Stderr: stderr}
	switch {
	case !r.Begin:
		return res, fmt.Errorf("%s: %w — the guest shell never acknowledged the script (exit %d). "+
			"The usual cause is stdin not reaching it: `container machine run` DISCARDS stdin unless -i is passed, "+
			"and then exits 0 having run nothing", op, ErrTransport, procExit)
	case !r.Trailer:
		return res, fmt.Errorf("%s: %w — the script started but its exit trap never fired (exit %d), "+
			"so the output above is truncated and the real status is unknown "+
			"(the guest process was probably signalled, or the stream was cut)", op, ErrTransport, procExit)
	case r.RC != procExit:
		return res, fmt.Errorf("%s: %w — the guest reported status %d but the transport reported %d; "+
			"one of the two is lying, so neither is trusted", op, ErrTransport, r.RC, procExit)
	case procExit != 0:
		return res, &ExitError{Op: op, Code: procExit, Stdout: clean, Stderr: stderr}
	}
	return res, nil
}
