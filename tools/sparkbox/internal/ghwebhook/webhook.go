// Package ghwebhook receives GitHub App webhook deliveries on the gateway.
//
// It is deliberately a stub. It proves a delivery came from GitHub, writes one
// line saying what arrived, and answers 204. Nothing else in the platform
// changes because a webhook showed up — no repo is re-checked, no installation
// cache is invalidated, no token is minted.
//
// Landing the empty receiver first is not ceremony. This is the one surface in
// the codebase a stranger on the public internet can reach with a POST body of
// their choosing, and every property that makes that acceptable — the shared
// secret, the constant-time compare, the size cap, the refusal to log the
// payload, the hostname it answers on — is settled here, once, while there is
// still nothing behind it to get wrong. The alternative is settling them in the
// same change that first *acts* on a delivery, where the interesting part of
// the diff is the action and the verification is the part reviewed last.
//
// # Fail closed, in both places
//
// New refuses to build a receiver with no secret, and cmd/sparkbox refuses to
// mount one it could not build. Both, because the failure they prevent is the
// same one and it is silent: a receiver with an empty secret verifies nothing,
// so an unauthenticated POST would be indistinguishable from a real delivery
// for as long as nobody looked. The point of the secret is that no code
// downstream of this package ever has to reason about that case.
//
// # Why it gets a hostname of its own
//
// The receiver is mounted at its own reserved subdomain rather than under the
// REST API, which is otherwise the obvious home for an HTTP endpoint here.
// api.<domain> is uniformly authenticated — every route on it demands a session
// token — and that uniformity is worth more than the tidiness of one origin. An
// unauthenticated exception inside it is a shape the next endpoint copies
// without re-deriving why this one was allowed to differ. (It differs because
// the signature *is* the authentication, which is a property of this endpoint
// and not of its neighbours.)
package ghwebhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const (
	// DefaultSubdomain is the label the receiver expects to be mounted at, so
	// the App's webhook URL is https://hooks.<domain>/github. It is named here
	// and reserved in internal/reserved, which cannot import this package —
	// keep the two in step, since a sandbox allowed to take this name would
	// shadow nothing and simply never be served.
	DefaultSubdomain = "hooks"

	// Path is the one route this package serves. GitHub sends every event of
	// every type to a single configured URL, so the path carries no routing
	// information; it exists to leave room beside it for a second forge later
	// without moving the first one's URL out from under an installed App.
	Path = "/github"
)

// The three headers a delivery is identified by. Only the signature is
// load-bearing; the other two are what makes a log line useful when an operator
// is staring at the App's "Recent Deliveries" page trying to match one up.
const (
	SignatureHeader = "X-Hub-Signature-256"
	EventHeader     = "X-GitHub-Event"
	DeliveryHeader  = "X-GitHub-Delivery"
)

// MaxBodyBytes caps how much of a delivery is read.
//
// GitHub documents 25MB as the maximum payload it will send, so this cap is
// well under what a legitimate delivery can be — and that is on purpose. The
// body has to be buffered whole before it can be verified, because the
// signature covers the exact bytes, so the cap is the only thing standing
// between an unauthenticated POST and this process allocating whatever the
// caller felt like sending. 8MB is far above every event this platform will
// ever act on (a push, an installation change) and far below a memory problem.
// The payloads that approach 25MB are the ones nobody wants anyway: a mass
// import, a repository-transfer batch. If one is ever needed, GitHub's own
// answer is to re-fetch the object from the API rather than to raise this.
const MaxBodyBytes = 8 << 20

// ErrNoSecret is why New refused. See the package doc: a receiver without a
// secret is not a degraded receiver, it is an open one.
var ErrNoSecret = errors.New("a github webhook receiver needs a secret")

// Config describes the receiver.
type Config struct {
	// Secret is the webhook secret configured in the GitHub App's settings,
	// verbatim. Required — see ErrNoSecret. Unlike the App's private key this
	// one is rotatable from both ends in a click, because GitHub never
	// generates it: it is a random string an operator pastes into the App and
	// into this fleet's secret store, so replacing it costs one edit on each
	// side and no re-consent by anyone.
	Secret string
	// Logger receives one line per delivery. It never receives the payload.
	Logger *slog.Logger
}

// Receiver verifies and logs deliveries for one GitHub App. It holds no state
// beyond its secret and is safe for concurrent use.
type Receiver struct {
	secret []byte
	log    *slog.Logger
}

// New builds a receiver, or refuses. The refusal is the point: every caller
// then has to decide out loud what to do without a secret, and the only correct
// answer — do not serve this endpoint at all — is the one that falls out of
// having no receiver to mount.
func New(cfg Config) (*Receiver, error) {
	if cfg.Secret == "" {
		return nil, ErrNoSecret
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Receiver{secret: []byte(cfg.Secret), log: log}, nil
}

// Handler serves the receiver. Mount it at the webhook subdomain; every other
// path 404s and every other method on Path answers 405, both from the mux's own
// pattern matching rather than from a hand-written check that could drift from
// the registration beside it.
func (rc *Receiver) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Path, rc.receive)
	return mux
}

// payload is the sliver of a delivery this package will look at.
//
// It is a struct with six fields and not a map[string]any, because the reason
// to name them is to be unable to reach the rest. A delivery carries private
// repository names, org member logins, branch names, commit messages and issue
// bodies. None of that is secret the way a token is, but it is other people's
// data arriving over an unauthenticated port, and a host log is a place it
// would sit for months, get shipped to a collector, and be read by everyone who
// can read logs. Parsing narrowly makes leaking it take an edit rather than an
// oversight.
type payload struct {
	Action       string `json:"action"`
	Installation *struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Sender *struct {
		Login string `json:"login"`
	} `json:"sender"`
	// The two shapes GitHub uses to name repositories: one object on the
	// per-repository events (push, pull_request), a list on the installation
	// ones. Both are decoded into empty structs — a count is enough to tell a
	// one-repo push from a 200-repo installation change, which is the only
	// thing anyone reads this field for, and it is all that can be logged
	// without logging what the repositories are called.
	Repository   *struct{}  `json:"repository"`
	Repositories []struct{} `json:"repositories"`
}

func (rc *Receiver) receive(w http.ResponseWriter, r *http.Request) {
	// The raw bytes first, before anything parses them: the signature covers
	// exactly what was sent, so a body that has been through a decoder and back
	// is a different body. Read one byte past the cap so "exactly at the limit"
	// stays a success and an over-limit body is detectable rather than silently
	// truncated into a signature mismatch.
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		rc.log.Warn("could not read a github webhook delivery",
			"delivery", r.Header.Get(DeliveryHeader), "error", err)
		http.Error(w, "sparkbox: could not read the delivery", http.StatusBadRequest)
		return
	}
	if len(body) > MaxBodyBytes {
		// Before verification, deliberately. Verifying first would mean HMACing
		// an unbounded body to decide whether to refuse it, which hands the cap
		// back to the caller.
		rc.log.Warn("refused an oversized github webhook delivery",
			"delivery", r.Header.Get(DeliveryHeader), "limit_bytes", MaxBodyBytes)
		http.Error(w, "sparkbox: delivery is too large", http.StatusRequestEntityTooLarge)
		return
	}

	if !rc.verify(r.Header.Get(SignatureHeader), body) {
		// Warn, not Info: on a correctly configured host this never happens, so
		// when it does it is either a secret that disagrees with the App's or
		// somebody poking the endpoint, and both deserve to be visible. The
		// answer to the caller says nothing about which — there is no version
		// of this message worth the hint it gives.
		rc.log.Warn("refused an unverified github webhook delivery",
			"event", r.Header.Get(EventHeader),
			"delivery", r.Header.Get(DeliveryHeader),
			"bytes", len(body),
			"signed", r.Header.Get(SignatureHeader) != "")
		http.Error(w, "sparkbox: signature verification failed", http.StatusUnauthorized)
		return
	}

	// Malformed JSON under a valid signature is GitHub sending something this
	// struct does not model, not an attack — the bytes were signed with a
	// secret only GitHub and this host hold. Log what was recoverable and still
	// answer 204, because refusing would mark the delivery failed and teach
	// GitHub to disable the webhook over a field name we did not know about.
	var p payload
	_ = json.Unmarshal(body, &p)

	event := r.Header.Get(EventHeader)
	switch event {
	case "ping":
		// GitHub sends exactly one of these when the webhook is first saved,
		// and it is the operator's only confirmation that the URL, the TLS
		// chain and the secret all line up. Named here so that first test is a
		// clean 204 on a path somebody chose, rather than falling through to a
		// default arm that will one day want to warn about events it does not
		// handle.
	default:
		// Any future handling goes on a goroutine or a queue — never inline.
		// GitHub gives a delivery 10 seconds; past that the delivery is marked
		// failed, and enough failures disable the webhook silently. An API call
		// to github.com from this function would blow that budget on the first
		// slow response, and the symptom (deliveries stop, days later) names
		// none of this. Answer first, work afterwards.
	}

	rc.log.Info("github webhook delivery",
		"event", event,
		"action", p.Action,
		"delivery", r.Header.Get(DeliveryHeader),
		"installation", p.installationID(),
		"sender", p.senderLogin(),
		"repositories", p.repoCount(),
		"bytes", len(body))

	w.WriteHeader(http.StatusNoContent)
}

// verify reports whether header is a valid X-Hub-Signature-256 over body.
//
// Every rejection path returns the same false: a missing header, a header in
// some other scheme, a truncated hex digest and a signature computed under the
// wrong secret are one answer to the caller and one status code, because the
// differences between them are only ever useful to somebody trying to find out
// which of the four they hit.
func (rc *Receiver) verify(header string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	// hex.DecodeString rejects odd lengths and non-hex bytes, so a malformed
	// digest never reaches the compare. It is case-insensitive; GitHub sends
	// lowercase.
	sent, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, rc.secret)
	mac.Write(body)
	// hmac.Equal, never ==. String or slice equality on a MAC returns early at
	// the first differing byte, which leaks how many leading bytes a guess got
	// right and turns forging a signature into 32 sequential searches.
	return hmac.Equal(sent, mac.Sum(nil))
}

// The three accessors below exist so the log call site stays a flat list of
// key/value pairs: every field is present on every line, with a zero value
// where the event does not carry it, so the lines stay greppable and a missing
// `installation` never means "the parse gave up halfway".

func (p payload) installationID() int64 {
	if p.Installation == nil {
		return 0
	}
	return p.Installation.ID
}

func (p payload) senderLogin() string {
	if p.Sender == nil {
		return ""
	}
	return p.Sender.Login
}

func (p payload) repoCount() int {
	n := len(p.Repositories)
	if p.Repository != nil {
		n++
	}
	return n
}
