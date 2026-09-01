package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// ---------------------------------------------------------------------------
// Runtime env for guest apps that ship a next-runtime-env bundle
// ---------------------------------------------------------------------------
//
// ClickHouse now embeds the ClickStack (HyperDX) UI in its own HTTP server, so
// a sandbox running ClickHouse serves a full observability console on :8123.
// It does not work behind this edge, and the reason is one line in HyperDX:
//
//	init.credentials = 'omit'
//
// HyperDX's "local mode" moves the ClickHouse username/password into query
// params (to dodge a CORS preflight on the Authorization header) and then
// sends the request with credentials omitted. Omitted credentials means the
// browser withholds the session cookie, so every query arrives here
// unauthenticated and the private-route gate answers 401. The UI loads — it is
// served from the same origin as a normal document request — and then every
// query inside it fails.
//
// HyperDX picks that code path with
//
//	const isLocalMode = this.username != null && this.password != null
//
// and its other path, standardModeFetch, sets no credentials at all — which
// defaults to same-origin, sends the cookie, and passes the gate. So a
// connection record with NO password field takes the working path. There is no
// flag for this; the connection record is the switch.
//
// Which is reachable, because the bundle is built with next-runtime-env: the
// page loads <script src="/__ENV.js"> (blocking, ahead of the main bundle) and
// HyperDX reads NEXT_PUBLIC_HDX_LOCAL_DEFAULT_CONNECTIONS out of it to seed a
// connection whenever the browser has none stored. ClickHouse does not serve
// that path — it answers 503 — so window.__ENV is undefined and the seed never
// happens.
//
// So the edge answers it. That is the whole fix: one synthesised path, no
// rewriting of any guest response body. Body rewriting was the alternative
// (inject a fetch shim, or register a service worker) and it would have meant
// giving up "the edge never edits what a guest serves" for every app on the
// fleet, to fix one.

// runtimeEnvPath is the only path the edge will ever answer on a guest's
// behalf. It is next-runtime-env's fixed filename, not a sparkbox invention.
const runtimeEnvPath = "/__ENV.js"

// clickStackConnections seeds HyperDX with a connection that authenticates the
// way this edge expects.
//
// Two fields are doing the work, and both are load-bearing:
//
//   - No "password" key at all. Not "", which is a non-null string and would
//     put HyperDX straight back into local mode and its credentials-omitting
//     fetch. The field is optional in HyperDX's own schema; leaving it out is
//     what selects the code path that sends the session cookie.
//   - host is "/" — a PATHNAME, not a URL. The non-local path computes
//     `window.origin + host`, so a full URL here would produce nonsense like
//     https://box.example.comhttps://box.example.com. "/" resolves to the
//     sandbox's own origin, which is exactly where ClickHouse is listening.
//
// It needs no per-sandbox substitution for the same reason: the origin comes
// from the browser, so one constant is correct on every box.
const clickStackConnections = `[{"id":"local","name":"Default","host":"/","username":"default"}]`

// DefaultRuntimeEnv is the payload New installs. Serving it is safe on a
// sandbox running anything else: nothing but a next-runtime-env bundle ever
// requests /__ENV.js, and a guest that serves its own is never overridden.
func DefaultRuntimeEnv() map[string]string {
	return map[string]string{
		"NEXT_PUBLIC_HDX_LOCAL_DEFAULT_CONNECTIONS": clickStackConnections,
	}
}

// SetRuntimeEnv replaces the runtime-env payload the edge serves when a guest
// does not answer /__ENV.js itself. Passing nil or an empty map turns the
// behaviour off, leaving the guest's own response (usually a 404 or 503) to
// reach the browser untouched. Call before serving.
func (s *Server) SetRuntimeEnv(vars map[string]string) {
	if len(vars) == 0 {
		s.runtimeEnv = nil
		return
	}
	// Marshalling a map sorts the keys, so the body is byte-stable across
	// restarts — it is a cacheable static asset and should not change identity
	// for no reason.
	blob, err := json.Marshal(vars)
	if err != nil {
		// Only unmarshalable types can fail here and this is a map of strings,
		// so this is unreachable; drop the payload rather than serve a broken
		// script that would throw before the app's own bundle runs.
		s.log.Error("runtime env payload is not marshalable; /__ENV.js disabled", "err", err)
		s.runtimeEnv = nil
		return
	}
	// window['__ENV'] rather than window.__ENV: this is next-runtime-env's own
	// emitted form, and matching it exactly keeps us honest if they ever start
	// caring about the shape.
	s.runtimeEnv = []byte("window['__ENV'] = " + string(blob) + ";\n")
}

// fillRuntimeEnv answers /__ENV.js on the guest's behalf when the guest itself
// could not, and leaves every other response alone. It runs as the reverse
// proxy's ModifyResponse, so it sees the upstream's real status before
// anything reaches the client.
func (s *Server) fillRuntimeEnv(resp *http.Response) error {
	if len(s.runtimeEnv) == 0 || resp.Request == nil || resp.Request.URL.Path != runtimeEnvPath {
		return nil
	}
	// A guest that serves its own runtime env owns it. Only a failure is
	// filled in, so a real HyperDX deployment inside a sandbox — which serves
	// this path properly — is never shadowed by ours.
	if resp.StatusCode < 400 {
		return nil
	}
	// The upstream body is discarded, so its framing headers must go with it:
	// a stale Content-Encoding from a gzipped error page would leave the
	// browser trying to inflate plain JavaScript.
	if resp.Body != nil {
		resp.Body.Close()
	}
	body := s.runtimeEnv
	resp.StatusCode = http.StatusOK
	resp.Status = strconv.Itoa(http.StatusOK) + " " + http.StatusText(http.StatusOK)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Uncompressed = false
	resp.Header = http.Header{
		"Content-Type":   []string{"text/javascript; charset=utf-8"},
		"Content-Length": []string{strconv.Itoa(len(body))},
		// The payload is per-deployment, not per-visitor, but it rides the same
		// private hostnames as everything else here and the gate has already
		// run. no-store keeps it consistent with every other authenticated
		// response the edge writes.
		"Cache-Control": []string{"no-store"},
	}
	return nil
}
