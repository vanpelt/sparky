package users

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	xssh "golang.org/x/crypto/ssh"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// newKey returns a fresh ed25519 public key and its authorized_keys line.
func newKey(t *testing.T, comment string) (xssh.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := xssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(xssh.MarshalAuthorizedKey(key)))
	if comment != "" {
		line += " " + comment
	}
	return key, line
}

func TestCreateAndLookup(t *testing.T) {
	s := testStore(t)
	key, _ := newKey(t, "laptop")
	if err := s.Create("vanpelt", key, "laptop", "signup", "operator"); err != nil {
		t.Fatal(err)
	}
	handle, ok := s.Lookup(key)
	if !ok || handle != "vanpelt" {
		t.Fatalf("Lookup = %q, %v; want vanpelt, true", handle, ok)
	}
	// An unrelated key authenticates as nobody.
	other, _ := newKey(t, "")
	if _, ok := s.Lookup(other); ok {
		t.Error("an unregistered key authenticated")
	}
}

func TestHandlesAreClaimedOnce(t *testing.T) {
	s := testStore(t)
	a, _ := newKey(t, "")
	b, _ := newKey(t, "")
	if err := s.Create("vanpelt", a, "", "signup", "operator"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create("vanpelt", b, "", "signup", "operator"); !errors.Is(err, ErrHandleTaken) {
		t.Errorf("second claim of a handle: %v, want ErrHandleTaken", err)
	}
}

func TestValidHandle(t *testing.T) {
	for _, h := range []string{"cvp", "vanpelt", "a1", "with-dash", strings.Repeat("a", 32)} {
		if !ValidHandle(h) {
			t.Errorf("ValidHandle(%q) = false, want true", h)
		}
	}
	for _, h := range []string{
		"a", "", "UPPER", "has space", "under_score", strings.Repeat("a", 33),
		// Reserved: these would collide with the gateway's own doors.
		"new", "ctl", "signup", "console", "oidc",
	} {
		if ValidHandle(h) {
			t.Errorf("ValidHandle(%q) = true, want false", h)
		}
	}
}

// A key proves who you are, so it must never be claimable by two accounts.
func TestKeyCannotBeLinkedToTwoAccounts(t *testing.T) {
	s := testStore(t)
	key, _ := newKey(t, "")
	other, _ := newKey(t, "")
	if err := s.Create("alice", key, "", "signup", "operator"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create("bob", other, "", "signup", "operator"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey("bob", key, "", "ctl"); !errors.Is(err, ErrKeyLinked) {
		t.Errorf("stealing alice's key: %v, want ErrKeyLinked", err)
	}
	if h, _ := s.Lookup(key); h != "alice" {
		t.Errorf("key now authenticates as %q, want alice", h)
	}
}

// import-github re-adds keys the account already has, so AddKey must be a
// no-op rather than an error.
func TestAddingAnOwnedKeyAgainIsANoOp(t *testing.T) {
	s := testStore(t)
	key, _ := newKey(t, "")
	if err := s.Create("alice", key, "", "signup", "operator"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddKey("alice", key, "", "github-import"); err != nil {
		t.Errorf("re-adding an owned key: %v, want nil", err)
	}
	keys, err := s.Keys("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Errorf("got %d keys, want 1 (re-adding must not duplicate)", len(keys))
	}
}

func TestRemoveKeyRefusesTheLastOne(t *testing.T) {
	s := testStore(t)
	first, _ := newKey(t, "laptop")
	second, _ := newKey(t, "desktop")
	if err := s.Create("alice", first, "laptop", "signup", "operator"); err != nil {
		t.Fatal(err)
	}
	fp := xssh.FingerprintSHA256(first)
	if err := s.RemoveKey("alice", fp); !errors.Is(err, ErrLastKey) {
		t.Errorf("removing the only key: %v, want ErrLastKey", err)
	}
	if err := s.AddKey("alice", second, "desktop", "ctl"); err != nil {
		t.Fatal(err)
	}
	// With a spare key, the lost laptop can go.
	if err := s.RemoveKey("alice", fp); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(first); ok {
		t.Error("the removed key still authenticates")
	}
	if h, ok := s.Lookup(second); !ok || h != "alice" {
		t.Error("the remaining key stopped working")
	}
}

// A typo'd fingerprint should say so, not blame the last-key rule.
func TestRemoveUnknownKeyReportsItAsUnknown(t *testing.T) {
	s := testStore(t)
	first, _ := newKey(t, "laptop")
	second, _ := newKey(t, "desktop")
	if err := s.Create("alice", first, "laptop", "signup", "operator"); err != nil {
		t.Fatal(err)
	}
	// One key on the account: the last-key rule must not mask a bad fingerprint.
	err := s.RemoveKey("alice", "SHA256:nonsense")
	if errors.Is(err, ErrLastKey) {
		t.Error("a bogus fingerprint was reported as ErrLastKey")
	}
	if err == nil || !strings.Contains(err.Error(), "no key") {
		t.Errorf("RemoveKey(bogus) = %v, want a 'no key' error", err)
	}
	// And with several keys on the account.
	if err := s.AddKey("alice", second, "desktop", "ctl"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveKey("alice", "SHA256:nonsense"); err == nil || !strings.Contains(err.Error(), "no key") {
		t.Errorf("RemoveKey(bogus) = %v, want a 'no key' error", err)
	}
	if keys, _ := s.Keys("alice"); len(keys) != 2 {
		t.Errorf("a failed removal changed the keyring: %d keys", len(keys))
	}
}

func TestDisabledAccountsDoNotAuthenticate(t *testing.T) {
	s := testStore(t)
	key, _ := newKey(t, "")
	if err := s.Create("alice", key, "", "signup", "operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET status = 'disabled' WHERE handle = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(key); ok {
		t.Error("a disabled account authenticated")
	}
}

func TestGitHubLinkIsRecordedWithATimestamp(t *testing.T) {
	s := testStore(t)
	key, _ := newKey(t, "")
	if err := s.Create("alice", key, "", "signup", "operator"); err != nil {
		t.Fatal(err)
	}
	u, err := s.Get("alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.GitHubVerifiedAt != nil {
		t.Error("a fresh account is github-verified")
	}
	if err := s.LinkGitHub("alice", "alice-gh"); err != nil {
		t.Fatal(err)
	}
	if u, err = s.Get("alice"); err != nil {
		t.Fatal(err)
	}
	if u.GitHubLogin != "alice-gh" || u.GitHubVerifiedAt == nil {
		t.Errorf("github link = %q, verified_at = %v", u.GitHubLogin, u.GitHubVerifiedAt)
	}
}

func TestOperatorsAreTheSeededUsers(t *testing.T) {
	s := testStore(t)
	op, _ := newKey(t, "")
	guest, _ := newKey(t, "")
	if err := s.Create("op", op, "", "seed", OperatorInviter); err != nil {
		t.Fatal(err)
	}
	if err := s.Create("guest", guest, "", "signup", "op"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Get("op"); !u.IsOperator() {
		t.Error("a users.conf-seeded account is not an operator")
	}
	if u, _ := s.Get("guest"); u.IsOperator() {
		t.Error("an invited account is an operator")
	}
}

func TestInviteIsSingleUse(t *testing.T) {
	s := testStore(t)
	code, err := s.NewInvite("op")
	if err != nil {
		t.Fatal(err)
	}
	by, err := s.RedeemInvite(code, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if by != "op" {
		t.Errorf("invited_by = %q, want op", by)
	}
	if _, err := s.RedeemInvite(code, "bob"); !errors.Is(err, ErrBadInvite) {
		t.Errorf("reusing a code: %v, want ErrBadInvite", err)
	}
	if _, err := s.RedeemInvite("nope-nope", "bob"); !errors.Is(err, ErrBadInvite) {
		t.Errorf("unknown code: %v, want ErrBadInvite", err)
	}
}

// Codes should be forgiving to type: case and dashes are cosmetic.
func TestInviteCodesAreCaseAndDashInsensitive(t *testing.T) {
	s := testStore(t)
	code, err := s.NewInvite("op")
	if err != nil {
		t.Fatal(err)
	}
	messy := strings.ToUpper(strings.ReplaceAll(code, "-", ""))
	if _, err := s.RedeemInvite("  "+messy+"  ", "alice"); err != nil {
		t.Errorf("redeeming %q (from %q): %v", messy, code, err)
	}
}

// Signup redeems before creating the account, so an abandoned dialog must not
// burn the code.
func TestReleasedInviteCanBeRedeemedAgain(t *testing.T) {
	s := testStore(t)
	code, err := s.NewInvite("op")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(code, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseInvite(code); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(code, "alice"); err != nil {
		t.Errorf("a released code should work again: %v", err)
	}
}

// Two people racing the same code: exactly one may win. The database's
// conditional update is what decides it.
func TestConcurrentRedeemHasExactlyOneWinner(t *testing.T) {
	s := testStore(t)
	code, err := s.NewInvite("op")
	if err != nil {
		t.Fatal(err)
	}
	const racers = 8
	var wg sync.WaitGroup
	results := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = s.RedeemInvite(code, "")
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range results {
		if err == nil {
			won++
		} else if !errors.Is(err, ErrBadInvite) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Errorf("%d racers redeemed one single-use code, want exactly 1", won)
	}
}

func TestInviteCountIgnoresExpiredUnusedCodes(t *testing.T) {
	s := testStore(t)
	if _, err := s.NewInvite("alice"); err != nil {
		t.Fatal(err)
	}
	n, err := s.InviteCount("alice")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("InviteCount = %d, want 1", n)
	}
	// Age the code out: nobody took it up, so it shouldn't be charged.
	if _, err := s.db.Exec(`UPDATE invites SET expires_at = '2000-01-01 00:00:00+00:00'`); err != nil {
		t.Fatal(err)
	}
	if n, err = s.InviteCount("alice"); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("InviteCount = %d after expiry, want 0", n)
	}
}

func TestSeedFileImportsAndIsIdempotent(t *testing.T) {
	s := testStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	k1, line1 := newKey(t, "laptop")
	k2, line2 := newKey(t, "desktop")
	other, line3 := newKey(t, "")

	path := filepath.Join(t.TempDir(), "users.conf")
	conf := "# a comment\n\nvanpelt " + line1 + "\nvanpelt " + line2 + "\ncvp " + line3 + "\n"
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // seeding twice must not duplicate or fail
		if err := SeedFile(path, s, log); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	// Both of vanpelt's machines authenticate as one account.
	for _, k := range []xssh.PublicKey{k1, k2} {
		if h, ok := s.Lookup(k); !ok || h != "vanpelt" {
			t.Errorf("Lookup = %q, %v; want vanpelt", h, ok)
		}
	}
	if h, _ := s.Lookup(other); h != "cvp" {
		t.Errorf("Lookup = %q, want cvp", h)
	}
	keys, err := s.Keys("vanpelt")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Errorf("vanpelt has %d keys, want 2", len(keys))
	}
	// Seeded users are the operators — that's what lets `ctl invite` work on a
	// freshly provisioned box with no extra configuration.
	if u, _ := s.Get("vanpelt"); !u.IsOperator() {
		t.Error("a seeded user is not an operator")
	}
}

// A key comment that is exactly one address becomes the account email — the
// seed file is the only place a comment is visible (the SSH wire protocol
// never carries it). Host-style comments and accounts with an email already
// set are left alone.
func TestSeedFileBackfillsEmailFromComment(t *testing.T) {
	s := testStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, emailLine := newKey(t, "cvp@example.com")
	_, hostLine := newKey(t, "vanpelt@Chris-Van-Pelt-M3 (NVIDIA Sync)")

	path := filepath.Join(t.TempDir(), "users.conf")
	conf := "vanpelt " + emailLine + "\ncvp " + hostLine + "\n"
	if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedFile(path, s, log); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Get("vanpelt"); u.Email != "cvp@example.com" {
		t.Errorf("email = %q, want the key comment cvp@example.com", u.Email)
	}
	if u, _ := s.Get("cvp"); u.Email != "" {
		t.Errorf("a host-style comment was adopted as an email: %q", u.Email)
	}

	// An address the user set themselves survives re-seeding.
	if err := s.SetEmail("vanpelt", "chosen@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := SeedFile(path, s, log); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Get("vanpelt"); u.Email != "chosen@example.com" {
		t.Errorf("re-seeding overwrote the email: %q", u.Email)
	}
}

func TestSeedFileRejectsBadInput(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, line := newKey(t, "")
	for name, conf := range map[string]string{
		"no key":          "vanpelt\n",
		"bad key":         "vanpelt ssh-ed25519 not-base64\n",
		"invalid handle":  "Not-A-Handle " + line + "\n",
		"reserved handle": "ctl " + line + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "users.conf")
			if err := os.WriteFile(path, []byte(conf), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := SeedFile(path, testStore(t), log); err == nil {
				t.Error("bad users.conf was accepted")
			}
		})
	}
}

// A key can't be moved between accounts by editing users.conf; that would let
// a file edit silently hand over an identity.
func TestSeedFileRefusesToStealAKey(t *testing.T) {
	s := testStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	key, line := newKey(t, "")
	if err := s.Create("alice", key, "", "signup", "operator"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "users.conf")
	if err := os.WriteFile(path, []byte("bob "+line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SeedFile(path, s, log); err == nil {
		t.Error("users.conf reassigned an existing key to another handle")
	}
	if h, _ := s.Lookup(key); h != "alice" {
		t.Errorf("key now authenticates as %q, want alice", h)
	}
}

func TestGitHubLoginValidation(t *testing.T) {
	for _, ok := range []string{"vanpelt", "a", "a-b", "user123", strings.Repeat("a", 39)} {
		if !githubLoginOK(ok) {
			t.Errorf("githubLoginOK(%q) = false, want true", ok)
		}
	}
	// Anything that could escape the URL we fetch, or that GitHub itself would
	// never issue.
	for _, bad := range []string{
		"", "-lead", "trail-", "a--b", "has space", "a/b", "../etc", "a?b", strings.Repeat("a", 40),
	} {
		if githubLoginOK(bad) {
			t.Errorf("githubLoginOK(%q) = true, want false", bad)
		}
	}
}

func TestParseAuthorizedKeysSkipsJunk(t *testing.T) {
	_, line := newKey(t, "real")
	body := "# comment\n\nnot a key at all\n" + line + "\n"
	keys, err := parseAuthorizedKeys(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Errorf("got %d keys, want 1", len(keys))
	}
}
