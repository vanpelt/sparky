package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// The script runner: where a script build's script runs from, and what happens
// when it does not run. Both halves are exercised through the envWorld harness
// in assets_test.go, which runs the REAL guest worker over the REAL string the
// gateway ships — the gateway writes the payload and a guest executes it, they
// live in packages with no compiler relationship, and this seam is where both
// of the bugs these tests are named after were found.

// selfLocatingSetupScript opens the way most real setup scripts open, and the
// way the platform's own guidance produces: find the repository from the
// script's own path, then work relative to that.
//
// It is correct for a file at .sparkbox/setup.sh, and it is what died on the
// live cluster. A script build used to be handed the script bare, and the guest
// worker stages whatever it is handed at /run/sparkbox/env-setup.sh — so
// `dirname "$0"/..` resolved to /run and the next line failed on a directory
// that was sitting right there in the checkout. An AGENT build never saw it,
// because an agent build verifies by running .sparkbox/setup.sh in the
// checkout: the script passed the moment it was written and failed the first
// time the same environment was rebuilt from it.
const selfLocatingSetupScript = `#!/usr/bin/env bash
set -e
cd "$(dirname "${BASH_SOURCE[0]}")/.."
echo "self=$0"
cd backend
echo "cwd=$PWD"
`

// agentCredential puts an agent token in the environment the worker sources, so
// the repair pass is reachable. Its ABSENCE is this harness's default on
// purpose: a script build is never refused for want of a credential, so the
// ordinary world is one where the repair pass may or may not be available.
func (w *envWorld) agentCredential() {
	w.t.Helper()
	w.write(filepath.Join(w.root, "etc/environment"),
		"PATH=\""+w.stub+":/usr/bin:/bin:/usr/local/bin\"\nSETUP_SECRET=\"s3cr3t\"\n"+
			"CLAUDE_CODE_OAUTH_TOKEN=\"tok\"\n")
}

// called reports how many times the stub agent was invoked and, unlike read,
// does not fail when the answer is none — which is itself the assertion several
// of these tests are making.
func (w *envWorld) called() string {
	body, err := os.ReadFile(filepath.Join(w.fix, "claude-calls"))
	if err != nil {
		return "0"
	}
	return strings.TrimSpace(string(body))
}

// TestAScriptBuildRunsTheSetupScriptFromItsOwnPathInTheCheckout is the hardware
// bug in a test: an environment whose setup script locates the project from its
// own path builds, instead of dying on a directory that is present.
func TestAScriptBuildRunsTheSetupScriptFromItsOwnPathInTheCheckout(t *testing.T) {
	w := newEnvWorld(t)
	if err := os.MkdirAll(filepath.Join(w.work, "backend"), 0o755); err != nil {
		t.Fatal(err)
	}
	w.job("webapp", "script", ctlops.ScriptRunner("webapp", selfLocatingSetupScript))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	state, exit, script, log := w.report()
	if state != "ok" || exit != "0" {
		t.Fatalf("state/exit = %q/%q — a script that finds its project from its own path did not run:\n%s",
			state, exit, log)
	}
	// The path it ran AS, which is the whole fix: `$0` has to be the setup
	// script's conventional home inside the checkout, not a staging copy.
	if !strings.Contains(log, "self="+ctlops.SetupScriptPath) {
		t.Errorf("the script did not run as %s:\n%s", ctlops.SetupScriptPath, log)
	}
	if want := "cwd=" + filepath.Join(w.work, "backend"); !strings.Contains(log, want) {
		t.Errorf("the script never reached %q:\n%s", want, log)
	}
	// This environment's checkout had no copy of the file, so the runner placed
	// one — and the readback reports it unchanged, which is what keeps the row
	// and the checkout saying the same thing.
	if got := decodeScript(t, script); got != selfLocatingSetupScript {
		t.Errorf("reported script = %q, want the environment's own", got)
	}
	// And no agent, because nothing failed. A script build that quietly spent
	// an agent on a working script would be the expensive kind of regression.
	if got := w.called(); got != "0" {
		t.Errorf("agent calls = %s, want 0 — a script that works needs no agent", got)
	}
}

// TestAScriptBuildRunsTheCheckoutsOwnFileWhenItMatches is the ordinary case,
// and the assertion is that the runner writes NOTHING: the stored script was
// seeded out of this very repository, so the file is already there and already
// right, and rewriting it would leave every builder with a dirty tree.
func TestAScriptBuildRunsTheCheckoutsOwnFileWhenItMatches(t *testing.T) {
	w := newEnvWorld(t)
	for _, dir := range []string{".sparkbox", "backend"} {
		if err := os.MkdirAll(filepath.Join(w.work, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(w.work, ctlops.SetupScriptPath)
	w.write(path, selfLocatingSetupScript)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	w.job("webapp", "script", ctlops.ScriptRunner("webapp", selfLocatingSetupScript))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	state, _, _, log := w.report()
	if state != "ok" {
		t.Fatalf("state = %q:\n%s", state, log)
	}
	if !strings.Contains(log, "running "+ctlops.SetupScriptPath+" from this checkout") {
		t.Errorf("the runner did not say it was running the checkout's own file:\n%s", log)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("the checkout's %s was rewritten with the bytes it already had",
			ctlops.SetupScriptPath)
	}
}

// TestAFailedScriptBuildGetsOneRepairAgent is the feature: a setup script that
// worked on the machine it was written on and does not work on a fresh microVM
// gets one agent pass at making it run, instead of a paused builder and a
// person who has to go and look.
func TestAFailedScriptBuildGetsOneRepairAgent(t *testing.T) {
	w := newEnvWorld(t)
	w.agentCredential()
	w.write(filepath.Join(w.fix, "claude-exit"), "0")
	w.write(filepath.Join(w.fix, "claude-writes"), goodSetupScript)
	w.job("webapp", "script", ctlops.ScriptRunner("webapp", brokenSetupScript))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	state, exit, script, log := w.report()
	if state != "ok" || exit != "0" {
		t.Fatalf("state/exit = %q/%q — the repaired script was not run and accepted:\n%s",
			state, exit, log)
	}
	// The REPAIRED file is what the row keeps, because it is what every later
	// build of this environment runs.
	if got := decodeScript(t, script); got != goodSetupScript {
		t.Errorf("reported script = %q, want the repaired one", got)
	}
	if got := w.called(); got != "1" {
		t.Errorf("agent calls = %s, want exactly 1", got)
	}
	prompt := w.read("claude-prompt-1")
	// The failure itself, not a summary of it: an agent asked to fix a script
	// without being shown what it printed is being asked to guess.
	for _, want := range []string{
		"cd: selfhost", ctlops.SetupScriptPath,
		"sparkbox docs dev-environment", "webapp",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the repair prompt does not mention %q:\n%s", want, prompt)
		}
	}
	if !strings.Contains(prompt, "do not ask") && !strings.Contains(prompt, "Do not ask") {
		t.Errorf("the repair prompt does not tell the agent nobody can answer it:\n%s", prompt)
	}
}

// TestAFailedScriptBuildWithNoAgentCredentialKeepsItsOwnError.
//
// A script build is never refused for want of an agent credential — the script
// is expected to work and usually does — so the absence can only be discovered
// at the moment it would have been used. What must survive that discovery is
// the script's own error: summarizeBuildLog takes the last non-empty line of
// the log as the environment's recorded failure, so a bare note about the agent
// that did not run would replace the one sentence the owner can act on.
func TestAFailedScriptBuildWithNoAgentCredentialKeepsItsOwnError(t *testing.T) {
	w := newEnvWorld(t)
	w.write(filepath.Join(w.fix, "claude-exit"), "0")
	w.write(filepath.Join(w.fix, "claude-writes"), goodSetupScript)
	w.job("webapp", "script", ctlops.ScriptRunner("webapp", "echo the-real-failure\nexit 7\n"))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0 — the worker always reports", code)
	}
	state, exit, _, log := w.report()
	if state != "failed" || exit != "7" {
		t.Fatalf("state/exit = %q/%q, want failed/7 — the script's own status is the answer:\n%s",
			state, exit, log)
	}
	if got := w.called(); got != "0" {
		t.Errorf("agent calls = %s, want 0 — there was no credential to run one with", got)
	}
	last := lastNonEmptyLine(log)
	if !strings.Contains(last, "the-real-failure") {
		t.Errorf("the script's own error is not the last line of the log:\n%s", last)
	}
	if !strings.Contains(last, "secret set "+ctlops.AgentCredential+" --tag webapp") {
		t.Errorf("the last line does not name the way to turn repair on:\n%s", last)
	}
}

// TestAFailedScriptBuildSpendsOnlyOneAgent. A second agent that could not make
// a script run is not usually one round away from making it run, the build
// shares one wall-clock budget with the run that got here, and the builder is
// kept paused either way — so a person opening the box beats a third guess.
func TestAFailedScriptBuildSpendsOnlyOneAgent(t *testing.T) {
	w := newEnvWorld(t)
	w.agentCredential()
	w.write(filepath.Join(w.fix, "claude-exit"), "0")
	w.write(filepath.Join(w.fix, "claude-writes"), brokenSetupScript)
	w.job("webapp", "script", ctlops.ScriptRunner("webapp", brokenSetupScript))

	if _, _, code := w.run(); code != 0 {
		t.Fatalf("exit = %d, want 0 — the worker always reports", code)
	}
	state, _, script, log := w.report()
	if state != "failed" {
		t.Fatalf("state = %q, want failed:\n%s", state, log)
	}
	if got := w.called(); got != "1" {
		t.Errorf("agent calls = %s, want exactly 1", got)
	}
	// Kept anyway, on the same terms as an agent build: the file is the record
	// of what happened, and `env capture` adopts the box a person then fixes.
	if got := decodeScript(t, script); got != brokenSetupScript {
		t.Errorf("the script the repair pass left behind was not reported: %q", got)
	}
	if !strings.Contains(lastNonEmptyLine(log), "still does not run here") {
		t.Errorf("the log does not end by saying the repair did not take:\n%s", log)
	}
}

// lastNonEmptyLine is what summarizeBuildLog does to a build log, and these
// tests assert on it for that reason: it is the sentence that reaches the row.
func lastNonEmptyLine(log string) string {
	last := ""
	for _, line := range strings.Split(log, "\n") {
		if strings.TrimSpace(line) != "" {
			last = line
		}
	}
	return last
}
