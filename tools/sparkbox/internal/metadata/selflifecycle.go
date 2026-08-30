package metadata

// The two lifecycle verbs a sandbox can aim at itself: `sparkbox pause` and
// `sparkbox snapshot <tag>`.
//
// Everything about this file falls out of one fact: THE BOX THAT ISSUES THE
// REQUEST IS THE BOX THAT STOPS. So nothing refusable may happen after the
// response, and nothing destructive may happen before the guest has read it.
//
// That gives the snapshot verb two routes rather than one. GET /self/snapshot
// is the PLAN — a pure read that answers every refusal a user can act on, and
// prints the warnings, while the VM is fully alive and the session is still
// open. POST /self/snapshot is the commit, which re-runs the plan (so a world
// that moved while the user was deciding is refused, not acted on), writes an
// acceptance, waits for the guest to read it, and only then starts the work.
//
// See ackThenAct for the three mechanics that make the read receipt real.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/host"
)

// SelfLifecycle is the gateway-owned half of the two verbs. A gateway
// implementation reaches its own control plane; a node relays over its
// authenticated fleet link, and BOTH go through the fleet — including for a
// gateway's own guests — so there is one authorization path rather than two
// that can drift.
//
// It is a SIBLING of RouteControl and RepoAccess and follows them exactly,
// including the rule that matters most here: every method takes the sandbox
// RECORD this host resolved from the tap, never a name from the request. The
// implementation reads the owner off that record, which is the documented
// elevation this feature rests on — from "a machine on a tap" to "the person
// who owns it" — and it is not one a request can influence.
//
// Nil answers all three routes 501, which is what a host with no control plane
// on its fleet is.
type SelfLifecycle interface {
	// Pause returns when the pause has been ACCEPTED, not when the VM has
	// stopped. Its error is therefore a host log line and never a guest's: by
	// the time it can be known, the acceptance has already been written and the
	// box it would be reported to is on its way down.
	Pause(ctx context.Context, box *host.Sandbox) error
	// PlanSnapshot mutates nothing and wakes nothing. tag and name may both be
	// empty, meaning "derive them"; the plan reports what was derived and the
	// commit sends those exact values back.
	PlanSnapshot(ctx context.Context, box *host.Sandbox, tag, name string) (ctlops.SelfSnapshotPlan, error)
	// Snapshot starts the capture and returns as soon as it is under way. The
	// outcome lands minutes later with the guest paused, which is why the
	// acceptance names where to read it from instead.
	Snapshot(ctx context.Context, box *host.Sandbox, a ctlops.SnapshotToTagArgs) error
}

// The lifecycle endpoints' own sliding windows, deliberately neither the mint's
// nor the repo pair's. repos.go states the rule and the reason: a shared window
// lets one workload starve the guest's identity.
//
// The commit budget is small on purpose. A capture is an unmetered write to the
// image volume every VM on this machine reflinks from — nothing anywhere counts
// snapshots, by number or by bytes — and it loop-mounts guest-authored ext4 on
// the host. Three an hour bounds the rate of both. It does not bound the total;
// that is owed work and not fixed here.
//
// The plan is limited separately and generously: it is a read, it is where
// people discover what they may do, and a tight budget on it would push callers
// into guessing at the commit instead.
const (
	planWindow = time.Minute
	planBurst  = 10
	selfWindow = time.Hour
	selfBurst  = 3
)

// selfBudget bounds the plan, mirroring githubBudget and for the same reason:
// ListenAndServe gives a handler a 10s WriteTimeout, the guest calls with
// `curl --max-time 20`, and on a node the plan is a fleet round trip. Left to
// the request's own deadline a slow gateway produces a truncated response at
// the write timeout, which reads to the guest as a broken host; cut short here
// it produces a sentence the guest can print.
const selfBudget = 6 * time.Second

// ackGrace caps how long a handler waits for the guest to finish reading its
// acceptance before the destructive half begins.
//
// The wait normally ends far sooner, on the guest's own FIN — see ackThenAct.
// This is the fail-soft floor for the day that signal degrades, and it is the
// design most people would have shipped anyway.
const ackGrace = 2 * time.Second

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// selfPause answers POST /self/pause.
//
// Respond-then-act, exactly like the commit below, and for a reason that is
// easy to miss: a pause freezes the guest's vCPUs, so a response written after
// it would be delivered to a kernel that is not running, and the connection
// would be dead by the time the box came back. Without the receipt the happy
// path prints a curl transport error.
func (s *Server) selfPause(w http.ResponseWriter, r *http.Request) {
	box, err := s.caller(r)
	if err != nil {
		s.log.Warn("metadata self-service refused", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return
	}
	if s.lifecycle == nil {
		http.Error(w, "sparkbox: pausing from inside a sandbox is not enabled on this host",
			http.StatusNotImplemented)
		return
	}
	text := renderPauseAccepted(box)
	body, contentType := s.negotiate(r, text, selfPauseDoc{
		Sandbox: box.Name, Pinned: box.Pinned, Accepted: true,
	})
	s.log.Info("sandbox paused itself", "sandbox", box.Name, "owner", box.Owner)
	s.ackThenAct(w, r, http.StatusAccepted, contentType, body, nil, func(ctx context.Context) {
		if err := s.lifecycle.Pause(ctx, box); err != nil {
			// Nowhere else for this to go. The guest asked to be stopped, was
			// told yes, and is either stopped or gone; this line is the only
			// record that it was neither.
			s.log.Error("a sandbox asked to pause itself and the pause failed",
				"sandbox", box.Name, "owner", box.Owner, "err", err)
		}
	})
}

// selfSnapshotPlan answers GET /self/snapshot: what would a capture do, and may
// this sandbox do it. Mutates nothing.
func (s *Server) selfSnapshotPlan(w http.ResponseWriter, r *http.Request) {
	_, plan, ok := s.plan(w, r, planKind)
	if !ok {
		return
	}
	text := renderPlan(plan)
	body, contentType := s.negotiate(r, text, plan)
	h := w.Header()
	// The four the shell reads with sed. They exist so the POSIX-sh side parses
	// no JSON at all: the body is printed verbatim and these carry the machine-
	// readable bits. Sparkbox-Ctl is here because a guest is never told its own
	// domain, so every hint that names the gateway has to be host-authored.
	h.Set("Sparkbox-Tag", plan.Tag)
	h.Set("Sparkbox-Snapshot", plan.Snapshot)
	h.Set("Sparkbox-Plan", plan.Token)
	h.Set("Sparkbox-Ctl", plan.CtlHint)
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	w.Write(body) //nolint:errcheck
}

// selfSnapshotCommit answers POST /self/snapshot.
//
// It re-runs the plan on this same healthy connection, so every refusal is
// still delivered while the VM is alive — nothing that can say no is discovered
// after the acceptance is written. Only then does it write, wait for the guest
// to read, and start the capture.
func (s *Server) selfSnapshotCommit(w http.ResponseWriter, r *http.Request) {
	box, plan, ok := s.plan(w, r, commitKind)
	if !ok {
		return
	}
	if token := r.URL.Query().Get("plan"); token != plan.Token {
		// The warnings the user agreed to described a world that has changed.
		// Refused rather than acted on, and refused HERE, before the pause.
		s.failSelf(w, r, box, &ctlops.Error{
			Kind: ctlops.KindConflict, Op: "snapshot.self", Code: "plan_stale",
			Msg: fmt.Sprintf("`%s` changed while you were deciding, so the warnings above are out of date — "+
				"%s Nothing was captured and this sandbox is still running. "+
				"Run the command again to see the new plan.", plan.Tag, bootsNow(plan)),
			Verbatim: true,
		})
		return
	}

	args := ctlops.SnapshotToTagArgs{Sandbox: box.Name, Name: plan.Snapshot, Tag: plan.Tag}
	text := renderAccepted(plan)
	body, contentType := s.negotiate(r, text, selfSnapshotDoc{
		Sandbox: box.Name, Snapshot: plan.Snapshot, Tag: plan.Tag, Accepted: true,
	})
	s.log.Info("sandbox asked to be captured as a template",
		"sandbox", box.Name, "owner", box.Owner, "snapshot", plan.Snapshot, "tag", plan.Tag)
	s.ackThenAct(w, r, http.StatusAccepted, contentType, body, map[string]string{
		"Sparkbox-Tag": plan.Tag, "Sparkbox-Snapshot": plan.Snapshot, "Sparkbox-Ctl": plan.CtlHint,
	}, func(ctx context.Context) {
		if err := s.lifecycle.Snapshot(ctx, box, args); err != nil {
			s.log.Error("a sandbox asked to be captured and the capture could not be started",
				"sandbox", box.Name, "owner", box.Owner, "snapshot", args.Name, "tag", args.Tag, "err", err)
		}
	})
}

// planKind and commitKind pick which budget the shared preamble spends. They
// are not two behaviours: the commit runs the same plan the GET does, which is
// the whole reason a refusal can never arrive after an acceptance.
type planPurpose int

const (
	planKind planPurpose = iota
	commitKind
)

// plan is the preamble both snapshot routes share: authenticate, refuse a host
// that cannot do this at all, spend a budget, and resolve the plan.
//
// It returns ok=false having already written the response.
func (s *Server) plan(w http.ResponseWriter, r *http.Request, purpose planPurpose) (*host.Sandbox, ctlops.SelfSnapshotPlan, bool) {
	box, err := s.caller(r)
	if err != nil {
		s.log.Warn("metadata self-service refused", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "sparkbox: "+err.Error(), http.StatusForbidden)
		return nil, ctlops.SelfSnapshotPlan{}, false
	}
	if s.lifecycle == nil || !s.allowSelfSnapshot {
		// One sentence for both, deliberately: "this host has no control plane"
		// and "the operator turned this off" are the same fact to a guest, and
		// the door that still works is the same door in either case.
		http.Error(w, "sparkbox: capturing from inside a sandbox is not enabled on this host. "+
			"Your owner can still do it from outside:\n"+
			"  ssh ctl@<gateway> snapshot create "+box.Name+" <name> --tag <tag>",
			http.StatusNotImplemented)
		return nil, ctlops.SelfSnapshotPlan{}, false
	}
	// Taken BEFORE the work, like /token's and the credential mint's: past here
	// the call leaves this machine on a node, and on either it makes the
	// gateway read stores. A commit spends the small budget even when the plan
	// it re-runs refuses — the GET is where refusals are meant to be found, and
	// a commit is only ever issued after one succeeded.
	window, burst, what := planWindow, planBurst, "plan"
	if purpose == commitKind {
		window, burst, what = selfWindow, selfBurst, "capture"
	}
	if !s.allowSelfCall(box.Name+" "+what, window, burst) {
		s.failSelf(w, r, box, &ctlops.Error{
			Kind: ctlops.KindLimit, Op: "snapshot.self", Code: "self_snapshot_rate_limited",
			Msg: fmt.Sprintf("too many %ss from this sandbox (%d per %s). Wait, or capture from outside:\n"+
				"  ssh ctl@<gateway> snapshot create %s <name> --tag <tag>",
				what, burst, humanWindow(window), box.Name),
			Verbatim: true,
		})
		return nil, ctlops.SelfSnapshotPlan{}, false
	}

	ctx, cancel := context.WithTimeout(r.Context(), selfBudget)
	defer cancel()
	plan, err := s.lifecycle.PlanSnapshot(ctx, box,
		strings.TrimSpace(r.URL.Query().Get("tag")), strings.TrimSpace(r.URL.Query().Get("name")))
	if err != nil {
		s.failSelf(w, r, box, err)
		return nil, ctlops.SelfSnapshotPlan{}, false
	}
	return box, plan, true
}

// humanWindow renders a budget's period the way the refusal reads it.
func humanWindow(d time.Duration) string {
	switch d {
	case time.Hour:
		return "hour"
	case time.Minute:
		return "minute"
	}
	return d.String()
}

// ---------------------------------------------------------------------------
// The read receipt
// ---------------------------------------------------------------------------

// ackThenAct writes the acceptance, waits for the guest to have read it, and
// only then starts the half that stops the VM.
//
// THREE THINGS HERE ARE LOAD-BEARING AND EACH IS EASY TO GET WRONG.
//
//  1. Content-Length is mandatory. A Flush without it switches the connection
//     to chunked encoding, and curl does not consider a body complete until the
//     terminating zero-length chunk — which net/http writes when the handler
//     RETURNS, i.e. after the wait below. That deadlocks into ackGrace on every
//     single call and silently degrades the receipt to a timer.
//
//  2. The wait must be inside the handler. ServeHTTP cancels the request
//     context on return, so a goroutine selecting on r.Context().Done() fires
//     immediately, always — and would start the destructive half at once.
//
//  3. The work must start AFTER the wait, not before it with a release. That is
//     what makes this identical on a gateway and on a node, where the chain is
//     guest tap → node metadata → uplink → gateway control plane → back down to
//     the node's manager: every hop happens after the guest already holds the
//     complete response.
//
// net/http cancels the request context when an HTTP/1 client closes a
// connection whose body it has fully read, so that close is a genuine
// application-level receipt: a guest cannot FIN a response it did not read.
// That is observed behaviour rather than a documented contract, which is what
// ackGrace is for — the degraded mode is a timer, not a hang.
//
// One residual remains and cannot be closed from here: the response written and
// lost in transit. The guest answers that with a message that claims nothing
// and an exit code that is not success.
func (s *Server) ackThenAct(w http.ResponseWriter, r *http.Request, status int,
	contentType string, body []byte, headers map[string]string, act func(context.Context)) {
	h := w.Header()
	for k, v := range headers {
		h.Set(k, v)
	}
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body) //nolint:errcheck
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	t := time.NewTimer(ackGrace)
	defer t.Stop()
	select {
	case <-r.Context().Done():
	case <-t.C:
	}

	// Detached from the request — the handler is about to return and cancel it —
	// and bounded by the archive budget, which is the ceiling on the longest
	// thing this can start.
	go func() {
		ctx, cancel := context.WithTimeout(s.baseContext(), ctlops.ArchiveTimeout)
		defer cancel()
		act(ctx)
	}()
}

// baseContext is the process's own lifetime, so a detached worker dies with the
// service rather than outliving it.
func (s *Server) baseContext() context.Context {
	s.baseMu.Lock()
	defer s.baseMu.Unlock()
	if s.base == nil {
		return context.Background()
	}
	return s.base
}

func (s *Server) setBaseContext(ctx context.Context) {
	s.baseMu.Lock()
	defer s.baseMu.Unlock()
	s.base = ctx
}

// ---------------------------------------------------------------------------
// Wire shapes and content negotiation
// ---------------------------------------------------------------------------

type selfPauseDoc struct {
	Sandbox  string `json:"sandbox"`
	Pinned   bool   `json:"pinned"`
	Accepted bool   `json:"accepted"`
}

type selfSnapshotDoc struct {
	Sandbox  string `json:"sandbox"`
	Snapshot string `json:"snapshot"`
	Tag      string `json:"tag"`
	Accepted bool   `json:"accepted"`
}

// selfErrorDoc is a refusal for a caller that asked for JSON. The sentence is
// the same one the text renderer prints; nothing is rephrased, because a shell
// or a client that paraphrased a refusal would be paraphrasing the only
// explanation that exists.
type selfErrorDoc struct {
	Op      string         `json:"op"`
	Kind    string         `json:"kind"`
	Code    string         `json:"code"`
	Error   string         `json:"error"`
	Hint    string         `json:"hint,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// negotiate renders text for a caller that asked for text/plain and JSON for
// everyone else. The guest CLI asks for text and prints the body verbatim,
// which is what keeps every user-facing sentence in Go where a golden test can
// pin it, and leaves the shell no opportunity to paraphrase a refusal.
func (s *Server) negotiate(r *http.Request, text string, doc any) ([]byte, string) {
	if !wantsText(r) {
		body, err := json.Marshal(doc)
		if err != nil {
			// A doc this package built and cannot marshal is a bug, not a
			// caller's problem; the text form says the same thing.
			s.log.Error("could not marshal a self-service document", "err", err)
			return []byte(text), "text/plain; charset=utf-8"
		}
		return append(body, '\n'), "application/json"
	}
	return []byte(text), "text/plain; charset=utf-8"
}

func wantsText(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/plain")
}

// failSelf maps a control-plane refusal onto a status and a sentence.
//
// The status comes from the error's own Kind, which is what keeps the guest's
// exit-code table honest: invalid is 400 (exit 2), denied 403 (exit 3),
// conflict and limit 409/429 (exit 4), disabled 501 (exit 5), and a node that
// is not answering 503 (exit 75, "try again"). A failure this package cannot
// classify is a 502, because from a guest's point of view the thing that did
// not answer is the gateway.
func (s *Server) failSelf(w http.ResponseWriter, r *http.Request, box *host.Sandbox, err error) {
	var e *ctlops.Error
	if !errors.As(err, &e) {
		s.log.Error("guest self-service failed", "sandbox", box.Name, "err", err)
		http.Error(w, "sparkbox: the gateway that owns your tags is not reachable right now. "+
			"Nothing was changed — try again in a minute.", http.StatusBadGateway)
		return
	}
	status := e.HTTPStatus()
	if status >= 500 {
		s.log.Error("guest self-service failed", "sandbox", box.Name, "op", e.Op, "code", e.Code, "err", err)
	} else {
		s.log.Warn("guest self-service refused", "sandbox", box.Name, "owner", box.Owner,
			"op", e.Op, "code", e.Code, "err", err)
	}
	if !wantsText(r) {
		body, merr := json.Marshal(selfErrorDoc{
			Op: e.Op, Kind: e.Kind.String(), Code: e.Code, Error: e.Msg,
			Hint: e.Hint, Details: e.Details,
		})
		if merr == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(status)
			w.Write(append(body, '\n')) //nolint:errcheck
			return
		}
	}
	http.Error(w, "sparkbox: "+e.Msg, status)
}

// ---------------------------------------------------------------------------
// The prose
//
// Every user-facing sentence for these verbs lives here, in Go, where a golden
// test pins it. It is deliberately not in the shell: the guest CLI is frozen
// into every template this feature captures, so a sentence that lived there
// would be un-fixable in every box forked from one.
// ---------------------------------------------------------------------------

// planWidth is where prose wraps. 78 leaves room in an 80-column terminal for
// the two characters somebody's terminal will add.
const planWidth = 78

func renderPlan(p ctlops.SelfSnapshotPlan) string {
	var b strings.Builder
	b.WriteString("\n")
	// The first two rows are written straight rather than wrapped: their
	// alignment is data (a name, then a tag list), and wrap() normalizes runs of
	// spaces, which is exactly what holds those two columns apart.
	fmt.Fprintf(&b, "  %-15s%s   tags: %s\n", "this sandbox", p.Sandbox, strings.Join(p.Tags, ", "))
	fmt.Fprintf(&b, "  %-15s%s   (new)\n", "capture as", p.Snapshot)
	field(&b, "tag `"+p.Tag+"`", boots(p))
	fieldCont(&b, "-> boots "+p.Snapshot)
	if p.BoundNode != "" {
		field(&b, "placement", fmt.Sprintf(
			"%s lives on node %q; this capture will live on %q. New `%s` sandboxes are placed on the "+
				"machine that holds the template, so they will move to %s.",
			p.Bound, p.BoundNode, p.Node, p.Tag, p.Node))
	}

	for _, warning := range planWarnings(p) {
		b.WriteString("\n")
		b.WriteString(warning)
	}

	b.WriteString("\n")
	b.WriteString(wrap("  ", "  ", planWidth, closingParagraph(p)))
	b.WriteString("\n")
	return b.String()
}

// boots describes what the tag boots TODAY. "the stock template" rather than a
// name, because an unbound tag does not point at anything a user could go and
// look at.
func boots(p ctlops.SelfSnapshotPlan) string {
	if p.Bound == "" {
		return "boots the stock template"
	}
	detail := ""
	switch {
	case !p.BoundAt.IsZero() && p.BoundFrom != "":
		detail = fmt.Sprintf(" (captured %s from %s)", p.BoundAt.UTC().Format("2006-01-02"), p.BoundFrom)
	case p.BoundFrom != "":
		detail = " (captured from " + p.BoundFrom + ")"
	}
	return "boots " + p.Bound + detail
}

// bootsNow is the plan_stale sentence's middle clause, which has to name what
// the tag boots from NOW — the fact that changed.
func bootsNow(p ctlops.SelfSnapshotPlan) string {
	if p.Bound == "" {
		return "it now boots the stock template."
	}
	return "it now boots " + p.Bound + "."
}

func planWarnings(p ctlops.SelfSnapshotPlan) []string {
	var out []string
	if p.Bound != "" {
		out = append(out, warning(fmt.Sprintf(
			"`%s` already points at %s. This re-points it. The old snapshot is kept, and you can put it "+
				"back with:", p.Tag, p.Bound))+
			"      "+p.CtlHint+" snapshot bind "+p.Bound+" --tag "+p.Tag+"\n")
	}
	switch len(p.Carriers) {
	case 0:
	case 1:
		out = append(out, warning(fmt.Sprintf(
			"%s is the only sandbox carrying `%s`. Binding does not re-base it: it keeps the rootfs it was "+
				"created from. Only sandboxes created after this boot from %s.",
			p.Carriers[0].Name, p.Tag, p.Snapshot)))
	default:
		var b strings.Builder
		b.WriteString(fmt.Sprintf("  ! %d of your sandboxes carry `%s`:\n", len(p.Carriers), p.Tag))
		width := 0
		for _, c := range p.Carriers {
			if len(c.Name) > width {
				width = len(c.Name)
			}
		}
		for _, c := range p.Carriers {
			self := ""
			if c.Self {
				self = "   (this one)"
			}
			b.WriteString(fmt.Sprintf("      %-*s   %s%s\n", width, c.Name, c.State, self))
		}
		b.WriteString(wrap("    ", "    ", planWidth, fmt.Sprintf(
			"Re-pointing does not re-base any of them. Running or paused, they keep the rootfs they were "+
				"created from — this one included. Only sandboxes created after this boot from %s.", p.Snapshot)))
		out = append(out, b.String())
	}
	if p.Busy != "" {
		out = append(out, warning(fmt.Sprintf(
			"another disk operation is running on this sandbox (%s). The capture waits for it to finish "+
				"before it pauses, which can take several minutes. Until then this session stays open and "+
				"nothing has changed.", p.Busy)))
	}
	if p.Turbo {
		out = append(out, warning(fmt.Sprintf(
			"%s is running in turbo, and the pause ends it. It comes back at its ordinary size.", p.Sandbox)))
	}
	return out
}

// closingParagraph is the one every plan ends with: what is about to happen,
// how long it takes, and the fact that nothing is lost.
//
// "Nothing is lost" is said without hedging because it is true by construction:
// the driver reflink-copies the disk before it sanitizes anything and never
// touches the source VM's, and the pause writes a full memory snapshot — so the
// captured box resumes with its processes intact.
func closingParagraph(p ctlops.SelfSnapshotPlan) string {
	size := ""
	if p.DiskMB > 0 {
		size = fmt.Sprintf("%.1f GB to compact, so the capture takes a minute or two and runs after you are "+
			"gone. ", float64(p.DiskMB)/1024)
	} else {
		size = "The capture takes a minute or two and runs after you are gone. "
	}
	return fmt.Sprintf("This pauses %s and ends this session. %sNothing is lost — `%s` resumes it exactly "+
		"where you left it.", p.Sandbox, size, p.SSHHint)
}

func renderAccepted(p ctlops.SelfSnapshotPlan) string {
	return fmt.Sprintf("accepted — capturing %s as %s, then binding `%s` to it.\n"+
		"Pausing now. The rest runs on the gateway and you will not see it here:\n"+
		"  %s snapshot ls\n", p.Sandbox, p.Snapshot, p.Tag, p.CtlHint)
}

// renderPauseAccepted is the pause verb's whole output. The pinned note comes
// FIRST because it is the one thing that makes the outcome differ from what the
// person typing expects: a pinned box comes back on its own at the next host
// restart, so "I paused it to stop it costing anything" is not what happened.
func renderPauseAccepted(box *host.Sandbox) string {
	var b strings.Builder
	if box.Pinned {
		b.WriteString(fmt.Sprintf("note: %s is pinned, so a host restart will bring it back up on its own.\n"+
			"      Run `sparkbox unpin` first if you want it to stay down.\n\n", box.Name))
	}
	b.WriteString(fmt.Sprintf("pausing %s — memory and processes are snapshotted, so\n"+
		"`ssh %s` picks up exactly here.\n", box.Name, box.Name))
	return b.String()
}

// field writes one "  label   value" row of the plan's header block, wrapping
// the value into the same column the label ends at.
func field(b *strings.Builder, label, value string) {
	head := fmt.Sprintf("  %-15s", label)
	b.WriteString(wrap(head, strings.Repeat(" ", len(head)), planWidth, value))
}

// fieldCont writes a row with no label, aligned under the values above it.
func fieldCont(b *strings.Builder, value string) {
	pad := strings.Repeat(" ", 17)
	b.WriteString(wrap(pad, pad, planWidth, value))
}

// warning writes a "  ! …" block, continuation lines aligned under the text
// rather than under the bang.
func warning(text string) string { return wrap("  ! ", "    ", planWidth, text) }

// wrap breaks text onto lines of at most width COLUMNS, prefixing the first
// with first and the rest with rest.
//
// Columns, not bytes: this prose has em dashes in it, and measuring their three
// bytes as three columns wraps the lines around them short enough to look like
// a bug. A word longer than the budget goes on its own line rather than being
// cut — every long token here is a sandbox name, a tag or a command, and a
// hyphenated one would be un-pasteable.
func wrap(first, rest string, width int, text string) string {
	var b strings.Builder
	prefix, line := first, ""
	flush := func() {
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteString("\n")
		prefix, line = rest, ""
	}
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case cols(prefix)+cols(line)+1+cols(word) <= width:
			line += " " + word
		default:
			flush()
			line = word
		}
	}
	flush()
	return b.String()
}

func cols(s string) int { return utf8.RuneCountInString(s) }

// ---------------------------------------------------------------------------
// The self endpoints' own rate window
// ---------------------------------------------------------------------------

// allowSelfCall is a third copy of allow's shape, and a third copy on purpose:
// the point of these budgets is that they cannot touch, and a shared helper
// over a shared map would be one refactor away from being one budget again. The
// key carries the operation as well as the sandbox, so a plan loop cannot spend
// the capture budget.
func (s *Server) allowSelfCall(key string, window time.Duration, burst int) bool {
	s.selfMu.Lock()
	defer s.selfMu.Unlock()
	now := time.Now()
	cutoff := now.Add(-window)

	if s.selfRecent == nil {
		s.selfRecent = map[string][]time.Time{}
	}
	// Sweep callers that have stopped asking, for the reason allow does: a
	// sandbox that was destroyed never returns, and nothing else would drop it.
	// The cutoff is this call's own window, so an entry from the other budget is
	// only ever swept early — it is refilled by its next request, and both are
	// per-sandbox anyway.
	for k, times := range s.selfRecent {
		if len(times) == 0 || times[len(times)-1].Before(now.Add(-selfWindow)) {
			delete(s.selfRecent, k)
		}
	}
	kept := s.selfRecent[key][:0]
	for _, t := range s.selfRecent[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= burst {
		s.selfRecent[key] = kept
		return false
	}
	s.selfRecent[key] = append(kept, now)
	return true
}
