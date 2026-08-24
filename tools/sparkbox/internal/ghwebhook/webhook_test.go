package ghwebhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSecret = "s3cr3t-webhook-string"

// sign is the sender's half of the protocol, written out independently of
// Receiver.verify so a change to either side has to be made twice on purpose.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// newReceiver builds a receiver logging into logged, which every test inspects
// for what did and did not end up in it.
func newReceiver(t *testing.T, logged *bytes.Buffer) *Receiver {
	t.Helper()
	rc, err := New(Config{
		Secret: testSecret,
		Logger: slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return rc
}

// padded returns syntactically valid JSON of exactly n bytes, for the two cases
// that only differ by one byte either side of the cap.
func padded(n int) []byte {
	const head, tail = `{"action":"push","pad":"`, `"}`
	return []byte(head + strings.Repeat("a", n-len(head)-len(tail)) + tail)
}

// A receiver with no secret verifies nothing, so it must not be constructible:
// the whole fail-closed story downstream rests on there being no such object to
// mount. See the package doc.
func TestNewRefusesAnEmptySecret(t *testing.T) {
	rc, err := New(Config{Secret: ""})
	if err != ErrNoSecret {
		t.Fatalf("New(no secret) err = %v, want ErrNoSecret", err)
	}
	if rc != nil {
		t.Error("New refused and still returned a receiver")
	}
}

func TestReceive(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
		event  string
		body   []byte
		// sig produces the X-Hub-Signature-256 header value for the request
		// body. nil sends no signature header at all.
		sig  func(body []byte) string
		want int
	}{
		{
			name: "a valid signature is accepted",
			body: []byte(`{"action":"opened"}`),
			sig:  func(b []byte) string { return sign(testSecret, b) },
			want: http.StatusNoContent,
		},
		{
			// GitHub's one-shot delivery when the webhook is saved: the
			// operator's first and only confirmation that the URL and the
			// secret agree, so it must not be the request that produces noise.
			name:  "ping is answered like any other event",
			event: "ping",
			body:  []byte(`{"zen":"Non-blocking is better than blocking.","hook_id":1}`),
			sig:   func(b []byte) string { return sign(testSecret, b) },
			want:  http.StatusNoContent,
		},
		{
			// The App's event list is edited on github.com, not here. An event
			// this build has never heard of must still be a clean 204, or
			// checking a new box in the App settings starts failing deliveries
			// on a host nobody has redeployed.
			name:  "an unknown event is still accepted",
			event: "deployment_protection_rule",
			body:  []byte(`{"action":"requested"}`),
			sig:   func(b []byte) string { return sign(testSecret, b) },
			want:  http.StatusNoContent,
		},
		{
			name: "a signature under the wrong secret is refused",
			body: []byte(`{"action":"opened"}`),
			sig:  func(b []byte) string { return sign("not-the-secret", b) },
			want: http.StatusUnauthorized,
		},
		{
			// The signature covers the exact bytes, which is the only reason
			// the body is read before it is parsed. Sign one payload, send
			// another.
			name: "a valid signature over different bytes is refused",
			body: []byte(`{"action":"opened"}`),
			sig:  func([]byte) string { return sign(testSecret, []byte(`{"action":"closed"}`)) },
			want: http.StatusUnauthorized,
		},
		{
			name: "an unsigned delivery is refused",
			body: []byte(`{"action":"opened"}`),
			sig:  nil,
			want: http.StatusUnauthorized,
		},
		{
			name: "a signature in another scheme is refused",
			body: []byte(`{"action":"opened"}`),
			sig:  func(b []byte) string { return strings.TrimPrefix(sign(testSecret, b), "sha256=") },
			want: http.StatusUnauthorized,
		},
		{
			name: "a malformed hex digest is refused",
			body: []byte(`{"action":"opened"}`),
			sig:  func([]byte) string { return "sha256=zz" },
			want: http.StatusUnauthorized,
		},
		{
			// Under the cap by one byte, correctly signed: the boundary belongs
			// to the sender, so a delivery that is exactly the documented size
			// must not be refused.
			name: "a body exactly at the cap is accepted",
			body: padded(MaxBodyBytes),
			sig:  func(b []byte) string { return sign(testSecret, b) },
			want: http.StatusNoContent,
		},
		{
			// Signed correctly on purpose: the cap has to be enforced before
			// the HMAC runs, or an unauthenticated caller decides how much this
			// process buffers and hashes.
			name: "a body over the cap is refused before it is verified",
			body: padded(MaxBodyBytes + 1),
			sig:  func(b []byte) string { return sign(testSecret, b) },
			want: http.StatusRequestEntityTooLarge,
		},
		{
			// A signed GET is still a GET. GitHub only ever POSTs, and the
			// method check costs nothing.
			name:   "a signed GET is refused as a bad method",
			method: http.MethodGet,
			body:   nil,
			sig:    func(b []byte) string { return sign(testSecret, b) },
			want:   http.StatusMethodNotAllowed,
		},
		{
			name: "another path on the same host is not found",
			path: "/github/extra",
			body: []byte(`{"action":"opened"}`),
			sig:  func(b []byte) string { return sign(testSecret, b) },
			want: http.StatusNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			srv := httptest.NewServer(newReceiver(t, &logged).Handler())
			defer srv.Close()

			method := tc.method
			if method == "" {
				method = http.MethodPost
			}
			path := tc.path
			if path == "" {
				path = Path
			}
			req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(DeliveryHeader, "11111111-2222-3333-4444-555555555555")
			if tc.event != "" {
				req.Header.Set(EventHeader, tc.event)
			}
			if tc.sig != nil {
				req.Header.Set(SignatureHeader, tc.sig(tc.body))
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (log: %s)", resp.StatusCode, tc.want, logged.String())
			}
			// Nothing this package answers with may quote the delivery: an
			// error body is the one place a poke at the endpoint gets an echo.
			if body := readAll(t, resp); strings.Contains(body, "action") {
				t.Errorf("the response echoed the payload: %q", body)
			}
		})
	}
}

// The log line is the entire product of this package today, so what it does and
// does not carry is the behaviour under test. Everything nameable in the
// payload — the private repository, the branch, the commit subject — is other
// people's data that arrived over an unauthenticated port, and none of it has
// any business sitting in a host log.
func TestTheLogLineNamesTheDeliveryAndNotThePayload(t *testing.T) {
	var logged bytes.Buffer
	srv := httptest.NewServer(newReceiver(t, &logged).Handler())
	defer srv.Close()

	body := []byte(`{
	  "action": "synchronize",
	  "installation": {"id": 4242},
	  "sender": {"login": "vanpelt"},
	  "repository": {"full_name": "wandb/very-private-thing", "private": true},
	  "pull_request": {"head": {"ref": "secret-branch-name"}, "title": "do not log me"}
	}`)
	req, err := http.NewRequest(http.MethodPost, srv.URL+Path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(EventHeader, "pull_request")
	req.Header.Set(DeliveryHeader, "d3l1very-id")
	req.Header.Set(SignatureHeader, sign(testSecret, body))
	req.Header.Set("X-Real-Secret-Ish-Header", "should-never-be-logged")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	got := logged.String()
	for _, want := range []string{
		"event=pull_request", "action=synchronize", "delivery=d3l1very-id",
		"installation=4242", "sender=vanpelt", "repositories=1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log line is missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"very-private-thing", "secret-branch-name", "do not log me",
		"should-never-be-logged", "full_name",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("log line carried %q, which is the payload:\n%s", forbidden, got)
		}
	}
	// One line per delivery, not one per field. A receiver that logs twice is
	// a receiver somebody will later "fix" by dropping the wrong one.
	if n := strings.Count(got, `msg="github webhook delivery"`); n != 1 {
		t.Errorf("logged the delivery %d times, want 1:\n%s", n, got)
	}
	if strings.Contains(got, "level=WARN") {
		t.Errorf("a verified delivery produced a warning:\n%s", got)
	}
}

// An unverified delivery is either a secret that disagrees with the App's or a
// stranger, and an operator has to be able to see both. It must still not carry
// the rejected bytes into the log.
func TestARefusalWarnsWithoutQuotingTheBody(t *testing.T) {
	var logged bytes.Buffer
	srv := httptest.NewServer(newReceiver(t, &logged).Handler())
	defer srv.Close()

	body := []byte(`{"action":"opened","repository":{"full_name":"wandb/very-private-thing"}}`)
	req, err := http.NewRequest(http.MethodPost, srv.URL+Path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(EventHeader, "pull_request")
	req.Header.Set(DeliveryHeader, "d3l1very-id")
	req.Header.Set(SignatureHeader, sign("yesterdays-secret", body))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	got := logged.String()
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("a refused delivery was not warned about:\n%s", got)
	}
	if !strings.Contains(got, "delivery=d3l1very-id") {
		t.Errorf("the refusal cannot be matched to a delivery:\n%s", got)
	}
	if strings.Contains(got, "very-private-thing") {
		t.Errorf("the refusal carried the body:\n%s", got)
	}
	if strings.Contains(got, `msg="github webhook delivery"`) {
		t.Errorf("a refused delivery was also logged as accepted:\n%s", got)
	}
}

// The installation events carry a list where the push events carry one object,
// and the count is the only thing about either that gets logged.
func TestRepositoryCountCoversBothPayloadShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"a list", `{"action":"added","repositories":[{"id":1},{"id":2},{"id":3}]}`, "repositories=3"},
		{"one object", `{"repository":{"id":1}}`, "repositories=1"},
		{"neither", `{"action":"created"}`, "repositories=0"},
		{"an explicit null", `{"repository":null}`, "repositories=0"},
		// A signed body this struct cannot parse is GitHub sending a shape we
		// do not model, not an attack — refusing it would fail the delivery and
		// eventually disable the webhook.
		{"unparseable json", `not json at all`, "repositories=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			srv := httptest.NewServer(newReceiver(t, &logged).Handler())
			defer srv.Close()

			body := []byte(tc.body)
			req, err := http.NewRequest(http.MethodPost, srv.URL+Path, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(EventHeader, "installation_repositories")
			req.Header.Set(SignatureHeader, sign(testSecret, body))

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", resp.StatusCode)
			}
			if got := logged.String(); !strings.Contains(got, tc.want) {
				t.Errorf("log is missing %q:\n%s", tc.want, got)
			}
		})
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	var b bytes.Buffer
	if _, err := b.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
