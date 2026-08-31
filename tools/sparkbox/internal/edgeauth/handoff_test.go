package edgeauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/hivemindsignin"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/users"
)

// fakeRedeemer answers with one canned claims document, or one canned error,
// and records every code it was asked about — a code spent twice is the bug
// this door must never have.
type fakeRedeemer struct {
	claims hivemindsignin.Claims
	err    error
	codes  []string
}

func (f *fakeRedeemer) Redeem(_ context.Context, code string) (hivemindsignin.Claims, error) {
	f.codes = append(f.codes, code)
	if f.err != nil {
		return hivemindsignin.Claims{}, f.err
	}
	return f.claims, nil
}

// fakeAdmitter stands in for *ctlops.Ops.
type fakeAdmitter struct {
	out    Admission
	err    error
	logins []string
	emails []string
}

func (f *fakeAdmitter) AdmitGitHubLogin(_ context.Context, login, email string) (Admission, error) {
	f.logins = append(f.logins, login)
	f.emails = append(f.emails, email)
	if f.err != nil {
		return Admission{}, f.err
	}
	out := f.out
	if out.Handle == "" {
		out.Handle = users.HandleForGitHubLogin(login)
	}
	return out, nil
}

type handoffFixture struct {
	h        *LoginHandler
	signer   *Signer
	redeem   *fakeRedeemer
	admit    *fakeAdmitter
	accounts fakeAccounts
}

func newHandoff(t *testing.T, existing ...string) handoffFixture {
	t.Helper()
	accounts := fakeAccounts{}
	for _, h := range existing {
		accounts[h] = users.User{Handle: h, Status: users.StatusActive}
	}
	redeem := &fakeRedeemer{claims: hivemindsignin.Claims{
		Subject: "hivemind:user:1", GitHub: "vanpelt", GitHubID: 7,
		Orgs: []string{"wandb"}, Email: "vanpelt@wandb.com",
		Dest: "https://my.hivemind.tools/",
	}}
	admit := &fakeAdmitter{}
	signer := NewSigner([]byte("k"))
	h, err := NewLoginHandler(LoginConfig{
		Signer: signer, Domain: "hivemind.tools", Gateway: "hivemind.tools", Secure: true,
		HomeSub: "my",
		Handoff: &HandoffConfig{Redeem: redeem, Admit: admit, Accounts: accounts, Orgs: []string{"wandb"}},
	})
	if err != nil {
		t.Fatalf("NewLoginHandler: %v", err)
	}
	return handoffFixture{h: h, signer: signer, redeem: redeem, admit: admit, accounts: accounts}
}

// post drives the door the way a browser does, optionally carrying a session.
func (f handoffFixture) post(path string, form url.Values, session string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "https://login.hivemind.tools"+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Deliberately cross-site, because that is what HiveMind's form POST is.
	// Nothing in the door reads it; the test carries it so a future Origin
	// check cannot be added without this failing.
	req.Header.Set("Origin", "https://wandb.hivemind.tools")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: CookieName, Value: session})
	}
	rec := httptest.NewRecorder()
	f.h.Handler().ServeHTTP(rec, req)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	return nil
}

// An existing account with nobody signed in is the zero-touch path: one POST
// in, a session and a redirect out, with no page in between.
func TestHandoffSignsInExistingAccount(t *testing.T) {
	f := newHandoff(t, "vanpelt")
	rec := f.post(HandoffPath, url.Values{"code": {"hmh_abc"}}, "")

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "https://my.hivemind.tools/" {
		t.Fatalf("landed at %q", loc)
	}
	c := sessionCookie(rec)
	if c == nil {
		t.Fatal("no session cookie set")
	}
	id, ok := f.signer.Verify(c.Value)
	if !ok || id.Handle != "vanpelt" {
		t.Fatalf("cookie carries %+v (ok=%v)", id, ok)
	}
	if id.Email != "vanpelt@wandb.com" {
		t.Fatalf("email not carried into the session: %q", id.Email)
	}
	if len(f.redeem.codes) != 1 || f.redeem.codes[0] != "hmh_abc" {
		t.Fatalf("codes redeemed: %v", f.redeem.codes)
	}
	if len(f.admit.emails) != 1 || f.admit.emails[0] != "vanpelt@wandb.com" {
		t.Fatalf("admitter saw emails %v", f.admit.emails)
	}
}

// A visitor with no account is asked before one is made for them. The account
// half must not have run yet — the page is a question, not a receipt.
func TestHandoffAsksBeforeCreatingAnAccount(t *testing.T) {
	f := newHandoff(t)
	rec := f.post(HandoffPath, url.Values{"code": {"hmh_abc"}}, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Create a sparkbox account?") {
		t.Fatalf("not the create page: %s", body)
	}
	if !strings.Contains(body, "vanpelt") {
		t.Fatal("the page does not name the handle being created")
	}
	if sessionCookie(rec) != nil {
		t.Fatal("a question page must not establish a session")
	}
	if len(f.admit.logins) != 0 {
		t.Fatalf("the account was created before the visitor answered: %v", f.admit.logins)
	}
}

// The interstitial's ticket completes what the first request declined to do,
// without a second code — the first one is spent.
func TestHandoffConfirmCreatesAndSignsIn(t *testing.T) {
	f := newHandoff(t)
	f.admit.out = Admission{Created: true, Strong: true, Keys: 2}

	ask := f.post(HandoffPath, url.Values{"code": {"hmh_abc"}}, "")
	ticket := ticketFrom(t, ask.Body.String())

	rec := f.post(HandoffPath+"/confirm", url.Values{"ticket": {ticket}}, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
	if sessionCookie(rec) == nil {
		t.Fatal("no session cookie set")
	}
	if len(f.redeem.codes) != 1 {
		t.Fatalf("confirm redeemed a second code: %v", f.redeem.codes)
	}
	if len(f.admit.logins) != 1 || f.admit.logins[0] != "vanpelt" {
		t.Fatalf("admitter saw %v", f.admit.logins)
	}
}

// A fresh account github.com publishes no key for is browser-only, and the
// door says so at the moment it becomes true rather than leaving it to be
// discovered at the launch page.
func TestHandoffTellsAKeylessAccountWhatItCannotDo(t *testing.T) {
	f := newHandoff(t)
	f.admit.out = Admission{Created: true, Strong: false}

	ask := f.post(HandoffPath, url.Values{"code": {"hmh_abc"}}, "")
	rec := f.post(HandoffPath+"/confirm", url.Values{"ticket": {ticketFrom(t, ask.Body.String())}}, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("expected the notice page, got %d", rec.Code)
	}
	if sessionCookie(rec) == nil {
		t.Fatal("the notice page still signs them in")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "browser-only") || !strings.Contains(body, "github link") {
		t.Fatalf("notice does not say what is missing or how to fix it: %s", body)
	}
	if !strings.Contains(body, "https://my.hivemind.tools/") {
		t.Fatalf("notice has no way forward: %s", body)
	}
}

// The login-CSRF mitigation: a handoff for somebody else never silently
// replaces the session in the browser.
func TestHandoffAsksBeforeSwappingASession(t *testing.T) {
	f := newHandoff(t, "vanpelt", "someone")
	other, _, _ := f.signer.Mint(Identity{Handle: "someone"}, time.Hour)

	rec := f.post(HandoffPath, url.Values{"code": {"hmh_abc"}}, other)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the interstitial, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Switch to vanpelt?") {
		t.Fatalf("not the switch page: %s", body)
	}
	if !strings.Contains(body, "someone") {
		t.Fatal("the page does not name the account already signed in")
	}
	if !strings.Contains(body, "Stay signed in as someone") {
		t.Fatal("no way out of a switch the visitor did not ask for")
	}
	if c := sessionCookie(rec); c != nil {
		t.Fatalf("the session was swapped without asking: %q", c.Value)
	}
}

// Landing on a session that is already the right one is the common re-click,
// and it costs nothing: no admission, no github.com round trip, just the hop.
func TestHandoffWithMatchingSessionJustRedirects(t *testing.T) {
	f := newHandoff(t, "vanpelt")
	mine, _, _ := f.signer.Mint(Identity{Handle: "vanpelt"}, time.Hour)

	rec := f.post(HandoffPath, url.Values{"code": {"hmh_abc"}}, mine)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://my.hivemind.tools/" {
		t.Fatalf("landed at %q", loc)
	}
	if len(f.admit.logins) != 0 {
		t.Fatalf("a re-click spent an admission: %v", f.admit.logins)
	}
}

// The org gate is the whole authorization story, and it must not leak the
// visitor's own memberships back onto a page an unauthenticated POST reached.
func TestHandoffRefusesOutsideTheOrgAllowlist(t *testing.T) {
	f := newHandoff(t, "vanpelt")
	f.redeem.claims.Orgs = []string{"acme", "someone-elses-secret-org"}

	rec := f.post(HandoffPath, url.Values{"code": {"hmh_abc"}}, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if sessionCookie(rec) != nil {
		t.Fatal("a refused visitor got a session")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "wandb") {
		t.Fatalf("the refusal does not name what is required: %s", body)
	}
	if strings.Contains(body, "someone-elses-secret-org") {
		t.Fatalf("the refusal echoed the visitor's memberships: %s", body)
	}
	if len(f.admit.logins) != 0 {
		t.Fatal("a refused visitor reached the account half")
	}
}

// Org membership is compared the way GitHub compares account names.
func TestHandoffOrgMatchIsCaseInsensitive(t *testing.T) {
	f := newHandoff(t, "vanpelt")
	f.redeem.claims.Orgs = []string{"WandB"}

	if rec := f.post(HandoffPath, url.Values{"code": {"hmh_abc"}}, ""); rec.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A spent, unknown or expired code is one sentence and no session.
func TestHandoffRefusedCode(t *testing.T) {
	f := newHandoff(t, "vanpelt")
	f.redeem.err = hivemindsignin.ErrRefused

	rec := f.post(HandoffPath, url.Values{"code": {"hmh_stale"}}, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if sessionCookie(rec) != nil {
		t.Fatal("a refused code produced a session")
	}
	if !strings.Contains(rec.Body.String(), "already been used or has expired") {
		t.Fatalf("unhelpful refusal: %s", rec.Body.String())
	}
}

// HiveMind being unreachable is a different sentence from a bad code: one is
// "try again", the other is "start again".
func TestHandoffUpstreamFailure(t *testing.T) {
	f := newHandoff(t, "vanpelt")
	f.redeem.err = errors.New("dial tcp: no route to host")

	rec := f.post(HandoffPath, url.Values{"code": {"hmh_abc"}}, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "could not be reached") {
		t.Fatalf("unhelpful refusal: %s", body)
	}
	if strings.Contains(body, "no route to host") {
		t.Fatalf("the page leaked the upstream error: %s", body)
	}
}

// A refusal from the account half is shown as written: ctlops sentences are
// meant for a person and the door must not invent a second vocabulary.
func TestHandoffShowsAdmissionRefusal(t *testing.T) {
	f := newHandoff(t, "vanpelt")
	f.admit.err = errors.New("the sparkbox account \"vanpelt\" already belongs to somebody else")

	rec := f.post(HandoffPath, url.Values{"code": {"hmh_abc"}}, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "already belongs to somebody else") {
		t.Fatalf("refusal not shown: %s", rec.Body.String())
	}
	if sessionCookie(rec) != nil {
		t.Fatal("a refused admission produced a session")
	}
}

// HiveMind chooses the destination and does not get to choose it off our zone.
func TestHandoffSanitisesTheDestination(t *testing.T) {
	for _, dest := range []string{
		"https://evil.example/steal",
		"http://my.hivemind.tools/",           // no cleartext on a --proxy-tls host
		"https://my.hivemind.tools.evil.test", // the suffix trap
		"javascript:alert(1)",
		"",
	} {
		f := newHandoff(t, "vanpelt")
		f.redeem.claims.Dest = dest
		rec := f.post(HandoffPath, url.Values{"code": {"hmh_abc"}}, "")
		if got := rec.Header().Get("Location"); got != "https://my.hivemind.tools/" {
			t.Errorf("dest %q landed at %q, want the home fallback", dest, got)
		}
	}
}

// The ticket is not a session, and a session is not a ticket. The separation
// is in the MAC, so neither can be presented as the other.
func TestTicketAndSessionAreNotInterchangeable(t *testing.T) {
	s := NewSigner([]byte("k"))
	session, _, err := s.Mint(Identity{Handle: "vanpelt"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := s.MintTicket(Ticket{Handle: "vanpelt", Login: "vanpelt", Dest: "https://my.hivemind.tools/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(ticket); ok {
		t.Fatal("a handoff ticket verified as a session token")
	}
	if _, ok := s.VerifyTicket(session); ok {
		t.Fatal("a session token verified as a handoff ticket")
	}
	if got, ok := s.VerifyTicket(ticket); !ok || got.Handle != "vanpelt" {
		t.Fatalf("VerifyTicket(%q) = %+v, %v", ticket, got, ok)
	}
	if _, ok := s.VerifyTicket(TicketPrefix + "tampered.payload"); ok {
		t.Fatal("a forged ticket verified")
	}
}

func TestTicketExpires(t *testing.T) {
	s := NewSigner([]byte("k"))
	ticket, err := s.MintTicket(Ticket{Handle: "vanpelt", Login: "vanpelt"})
	if err != nil {
		t.Fatal(err)
	}
	// The ticket's expiry is stamped at mint; nothing here can travel in time,
	// so assert the window is the documented one rather than sleeping through
	// it. VerifyTicket's expiry branch is exercised by the zero-value ticket
	// below, whose Expiry is 0 and therefore already past.
	got, ok := s.VerifyTicket(ticket)
	if !ok {
		t.Fatal("a fresh ticket did not verify")
	}
	if d := time.Until(time.Unix(got.Expiry, 0)); d <= 0 || d > TicketTTL+time.Second {
		t.Fatalf("ticket lives %v, want <= %v", d, TicketTTL)
	}
	stale, err := s.MintTicket(Ticket{Handle: "vanpelt"})
	if err != nil {
		t.Fatal(err)
	}
	// Re-sign a payload whose expiry has passed, the way an old ticket would
	// look: the MAC is valid and the clock is not.
	if _, ok := s.VerifyTicket(rewriteExpiry(t, s, stale, time.Now().Add(-time.Second).Unix())); ok {
		t.Fatal("an expired ticket verified")
	}
}

// A door with no allowlist is a door that admits everybody, so the handler
// refuses to exist rather than defaulting.
func TestHandoffNeedsAnOrgAllowlist(t *testing.T) {
	_, err := NewLoginHandler(LoginConfig{
		Signer: NewSigner([]byte("k")), Domain: "hivemind.tools", Secure: true,
		Handoff: &HandoffConfig{
			Redeem: &fakeRedeemer{}, Admit: &fakeAdmitter{}, Accounts: fakeAccounts{},
		},
	})
	if err == nil {
		t.Fatal("a handoff with no allowed orgs was accepted")
	}
	if !strings.Contains(err.Error(), "GitHub org") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// A rendered page must carry its own content type and no-store, which is only
// true if the status is written after them — the ordering trap that costs a
// Set-Cookie on the notice page when it is got wrong.
func TestHandoffPagesSetTheirHeaders(t *testing.T) {
	f := newHandoff(t, "vanpelt")
	f.redeem.err = hivemindsignin.ErrRefused
	rec := f.post(HandoffPath, url.Values{"code": {"hmh_stale"}}, "")

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("content type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control = %q", got)
	}
}

// A host that has not configured the door serves nothing there — not an
// endpoint that accepts anything.
func TestHandoffNotMountedWhenUnconfigured(t *testing.T) {
	h, _ := newTestLogin()
	req := httptest.NewRequest("POST", "https://login.hivemind.tools"+HandoffPath, strings.NewReader("code=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || rec.Code == http.StatusSeeOther {
		t.Fatalf("an unconfigured host answered the handoff with %d", rec.Code)
	}
}

// ticketFrom pulls the hidden field out of a rendered interstitial, which is
// how a browser would carry it to the confirm step.
func ticketFrom(t *testing.T, body string) string {
	t.Helper()
	const marker = `name="ticket" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no ticket in the page: %s", body)
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatal("unterminated ticket field")
	}
	return rest[:j]
}

// rewriteExpiry re-signs a ticket with a different expiry, so a test can make
// one that is genuinely stale rather than waiting out TicketTTL.
func rewriteExpiry(t *testing.T, s *Signer, ticket string, exp int64) string {
	t.Helper()
	got, ok := s.VerifyTicket(ticket)
	if !ok {
		t.Fatal("fixture ticket did not verify")
	}
	got.Expiry = exp
	body, err := marshalTicket(got)
	if err != nil {
		t.Fatal(err)
	}
	return TicketPrefix + body + "." + s.mac(TicketPrefix, body)
}
