package metadata

// The environment-build door: the two routes a BUILDER sandbox uses to be told
// what to run and to say what happened.
//
// One fact shapes every line of this file, and it is the same one that shapes
// selflifecycle.go: THE BOX THAT POSTS THE RESULT IS THE BOX THAT STOPS. The
// gateway answers a result by stripping the managed secret block, refreshing
// the agent CLIs, PAUSING the guest, and capturing its disk as the
// environment's template. A response written after that reaches a kernel that
// is not running. So the result handler acks first and acts second, through the
// same ackThenAct that the pause and capture verbs use, and for the identical
// reason.
//
// The second fact is the security model, and it is a subtraction rather than an
// addition: NOTHING IN EITHER REQUEST NAMES ANYTHING. There is no sandbox
// parameter, no environment parameter, no build-id parameter. The caller is the
// tap (see caller in server.go), and the job that caller has — if any — is
// decided entirely by the host from its own environment rows. A guest cannot
// ask for another sandbox's script, cannot report a result against another
// sandbox's build, and cannot name the environment its result lands on. Adding
// any such parameter would hand a compromised guest the ability to re-point
// another owner's template, which is the whole thing this port is arranged not
// to permit.
//
// TWO MODES RIDE ONE FORMAT. Line 2 of the fetch is the mode and line 3 is its
// payload, base64 either way: in `script` mode the payload is the setup script,
// in `agent` mode it is the prompt an agent is run with. The guest switches on
// the mode BEFORE it decodes, and refuses a mode it does not know by name — so
// a gateway ahead of its guests produces one clear sentence rather than a run
// of the wrong thing. Adding a third mode is a new value here, still not a new
// format.

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// EnvSetup is the gateway-owned half of an environment build. It is a SIBLING
// of SelfLifecycle, RouteControl and RepoAccess and follows the rule that
// matters most about all of them: every method takes the sandbox RECORD this
// host resolved from the tap, never a name from the request. The owner, the
// environment, and whether there is a job at all are read off that record's
// row by the implementation, and none of the three can be influenced by the
// caller.
//
// Nil answers both routes 501, which is what a host with no environment store
// is.
type EnvSetup interface {
	// SetupFor returns the job this sandbox should run, or ok=false when it has
	// no job. The sandbox identity comes from the tap, never from the request.
	SetupFor(ctx context.Context, box *host.Sandbox) (job SetupJob, ok bool, err error)
	// SetupDone hands back what the guest reported. It returns immediately; the
	// snapshot it triggers happens on its own goroutine, because the caller is
	// the guest that is about to be paused.
	SetupDone(ctx context.Context, box *host.Sandbox, r SetupResult) error
}

// SetupJob is what a builder is told to do: which environment it is building,
// which of the two ways, and the bytes that way needs.
//
// A STRUCT AND NOT THREE STRINGS, deliberately. The previous shape returned
// (script, env string, ok bool, err error), and a mode makes that three
// same-typed positional returns in a row — where transposing two of them
// compiles cleanly and serves the mode as the environment name. Every field
// here is host-authored and none carries a secret; Payload is the one that
// varies by mode, which is why it is not called Script any more.
type SetupJob struct {
	Env     string // the environment's name, which the guest prints
	Mode    string // SetupModeScript or SetupModeAgent
	Payload string // the setup script, or the agent's prompt
}

// The modes, named here because this package owns the wire format that carries
// them. ctlops mirrors these constants rather than importing them, for the same
// reason SetupResult is mirrored as ctlops.SetupReport: this package imports
// ctlops, so ctlops cannot import back.
const (
	SetupModeScript = "script"
	SetupModeAgent  = "agent"
)

// SetupResult is what the guest says happened. Every field is guest-authored
// and every field is bounded before it gets here.
type SetupResult struct {
	OK       bool
	ExitCode int
	Script   string // the .sparkbox/setup.sh the run ended with; "" when unchanged
	Log      string // bounded tail
}

// SetEnvSetup installs the environment-build collaborator after construction.
// It exists for the same reason fleet.SetRepos does — construction order: the
// control plane that implements this is built with a sandbox store that is
// itself built with this server. Call it before ListenAndServe; nothing reads
// the field until a request arrives.
func (s *Server) SetEnvSetup(e EnvSetup) { s.envSetup = e }

// The environment endpoints' own sliding windows, deliberately neither the
// mint's, nor the repo pair's, nor the lifecycle trio's. repos.go states the
// rule and the reason: a shared window lets one workload starve the guest's
// identity, and these two are polled by a systemd oneshot that nobody is
// watching.
//
// Both are limited generously, and the report deliberately more so than
// selfBurst's three captures an hour, even though it is also what triggers a
// capture. The difference is where the ceiling actually lives: a report only
// moves an environment that is still `building` with THIS box as its builder,
// so the control plane already bounds how many captures a spin loop can cause,
// and all this window has left to stop is the spin itself. Refusing an honest
// report, meanwhile, leaves a row in `building` with nobody to say why — which
// is much the worse of the two failures to court.
//
// Both windows are at most an hour, which is not incidental: allowSelfCall
// sweeps its map on selfWindow, so a longer window here would have its entries
// dropped early and its budget quietly refilled.
const (
	setupWindow = time.Minute
	setupBurst  = 10
	doneWindow  = time.Hour
	doneBurst   = 10
)

// setupBudget bounds the host-side read, mirroring selfBudget: a handler has a
// 10s WriteTimeout and the guest calls with a short --max-time, so a store that
// is slow must produce a sentence rather than a truncated response.
const setupBudget = 6 * time.Second

// The caps on the report. The body cap is taken BEFORE anything is parsed —
// http.MaxBytesReader, exactly as maxRepoStatusBody is used — and the field
// caps are taken before either field is kept.
//
// The guest bounds itself too, at 48 KiB of script and 8 KiB of log
// (`sparkbox-env-setup` in deploy/install-guest-identity.sh), and these sit
// deliberately at or above those: a report refused for size is an environment
// left in `building` with nobody to say why, so the host's job here is to stop
// an ABUSIVE body, not to second-guess an honest one. 48 KiB of script is
// exactly 64 KiB once base64 has it, and the body cap covers that plus the log
// with room to spare.
const (
	maxSetupResultBody = 128 << 10
	maxSetupScript     = 64 << 10
	maxSetupLog        = 8 << 10
	maxEnvNameLen      = 200
)

// ---------------------------------------------------------------------------
// GET /self/setup — "do I have a job, and what is it?"
// ---------------------------------------------------------------------------

// selfSetup answers the builder's fetch.
//
// The wire format is three lines because the caller is POSIX sh with curl and
// base64 and no JSON encoder at all — the same constraint that made
// /repos/status line-oriented. A guest reads it with sed and head:
//
//	env=$(sed -n 1p resp)
//	mode=$(sed -n 2p resp)
//	sed -n '3,$p' resp | tr -d ' \t\r\n' | base64 -d > setup.sh
//
// Line 3 is base64 because a setup script is arbitrary bytes containing
// newlines, and a format whose payload can contain its own separator is a
// format with a parsing bug waiting in it. The host writes it unfolded on one
// line; the guest strips whitespace across everything from line 3 on anyway, so
// a future host that folds it does not break a guest that is already deployed.
//
// 204 is the answer for every VM that is not currently a builder, which is
// nearly all of them, so that path allocates a rate-limiter key and nothing
// else.
func (s *Server) selfSetup(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		s.log.Warn("metadata setup fetch refused", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	if s.envSetup == nil {
		http.Error(w, "sparkbox: environment builds are not enabled on this host",
			http.StatusNotImplemented)
		return
	}
	// Taken BEFORE the work, like /token's and the plan's: past here the call
	// reads control-plane stores, and on a node it may leave the machine.
	if !s.allowSelfCall(box.Name+" setup", setupWindow, setupBurst) {
		http.Error(w, "sparkbox: too many setup requests from this sandbox",
			http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), setupBudget)
	defer cancel()
	job, ok, err := s.envSetup.SetupFor(ctx, box)
	if err != nil {
		s.log.Error("could not resolve a sandbox's setup job", "sandbox", box.Name, "err", err)
		http.Error(w, "sparkbox: could not read this sandbox's setup job",
			http.StatusInternalServerError)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Both are host-authored, but both are about to become lines 1 and 2 of a
	// line-oriented format the guest parses, so they are checked HERE rather
	// than trusted to have been checked wherever they were stored. A row that
	// cannot be rendered safely is a host bug, not a guest's problem.
	//
	// The mode is checked against the known set and not merely for control
	// characters: an empty or unknown mode reaching a guest costs a whole
	// builder boot to produce a refusal this host could have produced in a
	// nanosecond, and a mode that is silently wrong is worse than either.
	//
	// This check lives in the RENDERER, which is the one piece of this path
	// that a gateway guest and a node guest both run — internal/metadata.Server
	// serves both. Hoisting it into ctlops or fleet would validate for one kind
	// of guest and skip it for the other, which is the Phase B asymmetry in
	// mirror image.
	if job.Env == "" || len(job.Env) > maxEnvNameLen || hasControl(job.Env) {
		s.log.Error("refusing to serve a setup job with an unrenderable environment name",
			"sandbox", box.Name)
		http.Error(w, "sparkbox: could not read this sandbox's setup job",
			http.StatusInternalServerError)
		return
	}
	if job.Mode != SetupModeScript && job.Mode != SetupModeAgent {
		s.log.Error("refusing to serve a setup job with an unknown mode",
			"sandbox", box.Name, "env", job.Env, "mode", job.Mode)
		http.Error(w, "sparkbox: could not read this sandbox's setup job",
			http.StatusInternalServerError)
		return
	}

	var b strings.Builder
	b.WriteString(job.Env)
	b.WriteString("\n")
	b.WriteString(job.Mode)
	b.WriteString("\n")
	// Base64 in BOTH modes, and in agent mode that is load-bearing rather than
	// uniform-for-its-own-sake: the payload is a prompt written by this host,
	// the guest feeds it to a shell, and a backtick or a $( in it would be code
	// if it travelled as plain text.
	b.WriteString(base64.StdEncoding.EncodeToString([]byte(job.Payload)))
	b.WriteString("\n")
	body := b.String()

	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(body)) //nolint:errcheck
}

// ---------------------------------------------------------------------------
// POST /self/setup/result — "here is what happened"
// ---------------------------------------------------------------------------

// selfSetupResult accepts the builder's report and then, once the guest is
// holding the whole response, hands it to the control plane — which pauses this
// very VM and captures its disk. See ackThenAct for why the order is not
// negotiable.
//
// The body is four fields:
//
//	line 1:  "ok" or "failed"
//	line 2:  the script's exit status, 0-255
//	line 3:  base64 of the .sparkbox/setup.sh the run ended with ("" when unchanged)
//	line 4+: the tail of the log, verbatim
//
// The script is base64 for the reason the fetch's is: it is arbitrary bytes
// containing the separator. The log is NOT, because it is the last field and
// everything after the third newline is it — which costs no ambiguity and
// saves the guest from having to encode the one thing it may be producing
// under memory pressure. A guest writes the whole report with printf, base64
// and tail:
//
//	{ printf '%s\n%s\n%s\n' "$status" "$rc" "$b64_script"
//	  tail -c 8192 "$log"
//	} | curl --data-binary @- .../self/setup/result
func (s *Server) selfSetupResult(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		s.log.Warn("metadata setup report refused", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	if s.envSetup == nil {
		http.Error(w, "sparkbox: environment builds are not enabled on this host",
			http.StatusNotImplemented)
		return
	}
	if !s.allowSelfCall(box.Name+" setup-result", doneWindow, doneBurst) {
		http.Error(w, "sparkbox: too many setup reports from this sandbox",
			http.StatusTooManyRequests)
		return
	}
	// Bounded before it is parsed, not after: MaxBytesReader stops the read at
	// the cap instead of buffering a body the guest chose the size of.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSetupResultBody))
	if err != nil {
		http.Error(w, "sparkbox: setup report is too large", http.StatusRequestEntityTooLarge)
		return
	}
	res, why := parseSetupResult(string(raw))
	if why != "" {
		// Nothing has been applied at this point and nothing will be: a report
		// this host cannot read in full is not a report it acts on in part.
		http.Error(w, "sparkbox: "+why, http.StatusBadRequest)
		return
	}

	s.log.Info("sandbox reported its environment setup",
		"sandbox", box.Name, "owner", box.Owner, "ok", res.OK, "exit", res.ExitCode)
	body := []byte("sparkbox: setup result recorded\n")
	s.ackThenAct(w, r, http.StatusAccepted, "text/plain; charset=utf-8", body, nil,
		func(ctx context.Context) {
			if err := s.envSetup.SetupDone(ctx, box, res); err != nil {
				// Nowhere else for this to go. The guest has been answered and
				// is on its way down; this line is the only record that the
				// build did not land. The reconciler is what a person sees.
				s.log.Error("a sandbox reported its setup and the result could not be recorded",
					"sandbox", box.Name, "owner", box.Owner, "err", err)
			}
		})
}

// parseSetupResult reads the four-field body. It returns a plain sentence
// rather than an error because every refusal here is a 400 the guest prints,
// and it applies nothing on any path that returns one.
//
// It is deliberately strict about STRUCTURE and lenient about the redundancy
// between "failed" and a non-zero exit status. The host decides from OK; the
// status is detail. Refusing a report because those two disagree would throw
// away the only account of a build that already happened, and would do it at
// the last step.
func parseSetupResult(body string) (SetupResult, string) {
	// SplitN, not Split: everything past the third newline is the log, and a
	// log that happens to contain a newline is not a fifth field.
	fields := strings.SplitN(body, "\n", 4)
	if len(fields) < 3 {
		return SetupResult{}, "malformed setup report: expected status, exit code, " +
			"base64 script and log tail on separate lines"
	}
	var res SetupResult
	switch fields[0] {
	case "ok":
		res.OK = true
	case "failed":
	default:
		return SetupResult{}, "malformed setup report: the first line must be ok or failed"
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil || code < 0 || code > 255 {
		return SetupResult{}, "malformed setup report: the second line must be an exit status from 0 to 255"
	}
	res.ExitCode = code

	// Whitespace-tolerant, because `base64` without -w0 folds at 76 columns and
	// the guest is not guaranteed the GNU flag. Nothing else about the field is
	// forgiven: bytes that are not base64 are a report this host cannot read.
	script, err := base64.StdEncoding.DecodeString(stripSpace(fields[2]))
	if err != nil {
		return SetupResult{}, "malformed setup report: the third line is not base64"
	}
	if len(script) > maxSetupScript {
		// Refused rather than truncated, and that asymmetry with the log below
		// is the point: half a script is what every future fork of this
		// environment would then run.
		return SetupResult{}, "setup script is too large to record"
	}
	if !utf8.Valid(script) {
		// The script is kept verbatim on the environment row and shown back to
		// people; bytes that are not text are a sign the wrong file was sent.
		return SetupResult{}, "malformed setup report: the setup script is not valid UTF-8"
	}
	res.Script = string(script)

	var log string
	if len(fields) == 4 {
		log = fields[3]
	}
	// A tail is trimmed rather than refused. It is diagnostic output and not a
	// record, and refusing the report over it would throw away the only account
	// of a build that already ran — leaving the row in `building` with nothing
	// to explain it. Keeping the END is what makes it a tail.
	//
	// SANITIZE FIRST, THEN TRIM, and that order is the whole of this paragraph.
	// ToValidUTF8 replaces each run of invalid bytes with a three-byte U+FFFD,
	// so a tail cut to exactly maxSetupLog and sanitized afterwards can come
	// back LONGER than the cap it was just cut to. On a gateway nobody would
	// notice. On a node the report is relayed through internal/nodelink, whose
	// MaxSelfSetupLogBytes is the same number and refuses the whole message —
	// so a build that finished would be lost, and lost differently depending on
	// which machine it landed on. That asymmetry is exactly what the relay
	// exists to remove.
	log = strings.ToValidUTF8(log, "�")
	if len(log) > maxSetupLog {
		log = log[len(log)-maxSetupLog:]
		// Cutting by bytes can land inside a rune, which would put invalid
		// bytes back at the head of a string this function just promised was
		// valid. Drop the partial one; it is at most three bytes of a line
		// nobody is reading the start of anyway.
		for len(log) > 0 && !utf8.RuneStart(log[0]) {
			log = log[1:]
		}
	}
	res.Log = log
	return res, ""
}

// stripSpace removes the whitespace a folded base64 payload carries. It is a
// byte filter rather than strings.Fields+Join so a payload that is already one
// line costs one pass and no allocation beyond the result.
func stripSpace(s string) string {
	if !strings.ContainsAny(s, " \t\r\n\v\f") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n', '\v', '\f':
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
