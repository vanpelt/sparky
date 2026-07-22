package restapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"sync"
	"time"
)

// The Idempotency-Key replay cache.
//
// Jobs already make the LONG operations idempotent — ctlops.Go returns the
// in-flight job when the same owner asks for the same work again — but that
// only covers work that has a resource ref. `POST /v1/account/tokens` mints a
// new credential every call, and `POST /v1/account/invites` burns a quota slot;
// for those, a client that retries after a network timeout must not spend
// twice. The header is the standard answer, so this is the standard shape:
// remember the response, replay it verbatim, and refuse a key that arrives with
// a different request.
//
// It is in memory and unreplicated, which matches the jobs registry and is
// honest for a single-process control plane: a restart forgets both, and a
// restart also killed whatever the key was protecting.
const (
	// idempotencyRetain is how long a key is remembered. 24 hours is the
	// interval the IETF draft suggests and comfortably outlives any client's
	// retry budget.
	idempotencyRetain = 24 * time.Hour
	// idempotencyMax bounds the cache. Keys are supplied by authenticated
	// callers, so this is a memory guard rather than an abuse control.
	idempotencyMax = 4096
	// idempotencyHeader is the request header clients set.
	idempotencyHeader = "Idempotency-Key"
	// replayedHeader tells a client its answer came from the cache, so a retry
	// that looks successful is distinguishable from one that did the work.
	replayedHeader = "Idempotency-Replayed"
)

type replayEntry struct {
	// fingerprint is a hash of method, path and body. A key reused with
	// different arguments is a client bug — usually a key generated once and
	// reused for a loop — and answering it with the FIRST call's response would
	// silently drop work.
	fingerprint string
	at          time.Time
	done        bool
	status      int
	header      http.Header
	body        []byte
}

type replayCache struct {
	mu        sync.Mutex
	entries   map[string]*replayEntry
	lastSweep time.Time
	now       func() time.Time
}

func newReplayCache() *replayCache {
	return &replayCache{entries: map[string]*replayEntry{}, now: time.Now}
}

// wrap is the middleware. It sits inside the auth gate so the cache key is
// scoped to the authenticated handle: two users may pick the same key string
// without colliding, and an unauthenticated caller cannot probe for one.
func (rc *replayCache) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(idempotencyHeader)
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			// The body limit is enforced by limitBody, so this is a truncated
			// upload rather than an oversized one; either way there is nothing
			// to fingerprint.
			writeJSON(w, http.StatusBadRequest, errorEnvelope{Error: apiError{
				Kind: "invalid", Op: "idempotency", Code: "unreadable_body",
				Message: "could not read the request body",
			}})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))

		id := cacheKey(caller(r).Handle, key)
		fp := fingerprint(r.Method, r.URL.Path, r.URL.RawQuery, body)

		entry, verdict := rc.claim(id, fp)
		switch verdict {
		case replayHit:
			for k, vs := range entry.header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.Header().Set(replayedHeader, "true")
			w.WriteHeader(entry.status)
			w.Write(entry.body) //nolint:errcheck
			return
		case replayInFlight:
			writeJSON(w, http.StatusConflict, errorEnvelope{Error: apiError{
				Kind: "conflict", Op: "idempotency", Code: "idempotency_key_in_flight",
				Message: "a request with this Idempotency-Key is still running",
				Hint:    "Retry once it finishes, or poll the job it created.",
			}})
			return
		case replayMismatch:
			// 422 is what the IETF idempotency draft specifies for a key reused
			// with a different payload, and it is the right shape: the request
			// is well-formed, it is the KEY that cannot be honoured.
			writeJSON(w, http.StatusUnprocessableEntity, errorEnvelope{Error: apiError{
				Kind: "conflict", Op: "idempotency", Code: "idempotency_key_reused",
				Message: "this Idempotency-Key was already used for a different request",
				Hint:    "Use a fresh key for each distinct request.",
			}})
			return
		}

		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		// Deferred, not called after ServeHTTP: net/http recovers a panicking
		// handler at the connection level, so a straight-line settle would never
		// run and the key would sit done:false — answering every later request
		// with that key, including a plain retry of the same call, 409
		// idempotency_key_in_flight until it expires a day later.
		defer rc.settle(id, rec)
		next.ServeHTTP(rec, r)
	})
}

type verdict int

const (
	replayNew verdict = iota
	replayHit
	replayInFlight
	replayMismatch
)

// claim records an in-flight request or reports why it cannot.
func (rc *replayCache) claim(id, fp string) (*replayEntry, verdict) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.sweepLocked()

	if e, ok := rc.entries[id]; ok {
		switch {
		case e.fingerprint != fp:
			return nil, replayMismatch
		case !e.done:
			return nil, replayInFlight
		default:
			return e, replayHit
		}
	}
	rc.entries[id] = &replayEntry{fingerprint: fp, at: rc.now()}
	return nil, replayNew
}

// settle stores the response, or forgets the key entirely when the handler
// answered 5xx. A server fault is the one case where the client SHOULD be able
// to retry with the same key and have it actually run again; caching it would
// pin a transient failure for 24 hours.
//
// A handler that wrote nothing at all — it panicked — is treated the same way,
// and deliberately not as the 200 the recorder's zero status would imply: the
// work is not known to have happened, so the client must be allowed to retry.
func (rc *replayCache) settle(id string, rec *recorder) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	e, ok := rc.entries[id]
	if !ok {
		return
	}
	if !rec.wrote || rec.status >= 500 {
		delete(rc.entries, id)
		return
	}
	e.done = true
	e.status = rec.status
	e.body = rec.body.Bytes()
	e.header = http.Header{}
	// Only the headers that describe THIS answer are replayed. Copying
	// everything would replay Cache-Control and Date from a day-old response.
	for _, k := range []string{"Content-Type", "Location", "Retry-After", "Preference-Applied"} {
		if v := rec.Header().Get(k); v != "" {
			e.header.Set(k, v)
		}
	}
}

// sweepLocked drops expired entries, at most once a minute, and hard-trims the
// map if a client somehow outruns expiry.
func (rc *replayCache) sweepLocked() {
	now := rc.now()
	if now.Sub(rc.lastSweep) < time.Minute && len(rc.entries) < idempotencyMax {
		return
	}
	rc.lastSweep = now
	cutoff := now.Add(-idempotencyRetain)
	for id, e := range rc.entries {
		if e.at.Before(cutoff) {
			delete(rc.entries, id)
		}
	}
	// Over budget even after expiry: drop finished entries oldest-first. An
	// in-flight entry is never dropped, because losing it would let a duplicate
	// run concurrently — the exact thing the key was sent to prevent.
	for len(rc.entries) > idempotencyMax {
		var oldestID string
		var oldest time.Time
		for id, e := range rc.entries {
			if !e.done {
				continue
			}
			if oldestID == "" || e.at.Before(oldest) {
				oldestID, oldest = id, e.at
			}
		}
		if oldestID == "" {
			return
		}
		delete(rc.entries, oldestID)
	}
}

func cacheKey(handle, key string) string { return handle + "\x00" + key }

func fingerprint(method, path, query string, body []byte) string {
	sum := sha256.New()
	sum.Write([]byte(method))
	sum.Write([]byte{0})
	sum.Write([]byte(path))
	sum.Write([]byte{0})
	sum.Write([]byte(query))
	sum.Write([]byte{0})
	sum.Write(body)
	return hex.EncodeToString(sum.Sum(nil))
}

// recorder captures a handler's answer so it can be replayed. It buffers the
// body, which is safe here because every response this API writes is a small
// JSON document — the one streaming endpoint is the WebSocket, and that is a
// GET which never reaches this middleware.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	wrote  bool
}

func (rec *recorder) WriteHeader(code int) {
	if rec.wrote {
		return
	}
	rec.wrote = true
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *recorder) Write(p []byte) (int, error) {
	if !rec.wrote {
		rec.WriteHeader(http.StatusOK)
	}
	rec.body.Write(p) //nolint:errcheck
	return rec.ResponseWriter.Write(p)
}

// The recorder deliberately does not implement http.Hijacker. It only ever
// wraps mutations, and the one endpoint that upgrades a connection is a GET —
// so a recorder in that path would be a bug, and failing to hijack is how it
// would announce itself rather than silently buffering a shell session.
var _ http.ResponseWriter = (*recorder)(nil)
