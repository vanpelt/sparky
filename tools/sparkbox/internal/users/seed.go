package users

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	xssh "golang.org/x/crypto/ssh"
)

// SeedFile imports operator-blessed entries from a users.conf into the store.
// The file keeps its original format — one "<handle> <authorized_keys line>"
// per line — and remains the bootstrap path: it is how a freshly provisioned
// host knows its first user before anyone can run `ssh signup@`.
//
// Seeding is additive and idempotent: entries already in the store are left
// alone, so the store (not the file) is the source of truth once running. A
// key the file assigns to one handle but the store has under another is
// reported rather than moved.
func SeedFile(path string, s *Store, log *slog.Logger) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		handle, keyText, ok := strings.Cut(text, " ")
		if !ok {
			return fmt.Errorf("%s:%d: expected '<handle> <authorized_keys line>'", path, line)
		}
		key, comment, _, _, err := xssh.ParseAuthorizedKey([]byte(keyText))
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if !ValidHandle(handle) {
			return fmt.Errorf("%s:%d: invalid handle %q (2-32 chars of a-z, 0-9, dash; some names are reserved)", path, line, handle)
		}
		if err := seedOne(s, handle, key, comment); err != nil {
			return fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if log != nil {
			log.Debug("seeded user from users.conf", "handle", handle, "fp", xssh.FingerprintSHA256(key))
		}
	}
	return sc.Err()
}

func seedOne(s *Store, handle string, key xssh.PublicKey, comment string) error {
	if owner, ok := s.Lookup(key); ok {
		if owner != handle {
			return fmt.Errorf("key %s is already linked to user %q", xssh.FingerprintSHA256(key), owner)
		}
	} else if _, err := s.Get(handle); err == ErrNoSuchUser {
		if err := s.Create(handle, key, comment, "seed", OperatorInviter); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		// The user exists (from an earlier seed or a signup) but this key is new:
		// a second machine added to users.conf.
		if err := s.AddKey(handle, key, comment, "seed"); err != nil {
			return err
		}
	}
	return backfillEmail(s, handle, comment)
}

// backfillEmail adopts a key comment as the account email when the whole
// comment is one address (the ssh-keygen default of user@host rarely qualifies
// — no dot — but a deliberate "you@example.com" comment does) and the account
// has none. users.conf is the only place a comment survives: the SSH wire
// protocol never carries it, so seeding is the one chance to pick the address
// up for free. Never overwrites — the file is a bootstrap, not the owner of
// the field.
func backfillEmail(s *Store, handle, comment string) error {
	email := strings.TrimSpace(comment)
	if !ValidEmail(email) {
		return nil
	}
	u, err := s.Get(handle)
	if err != nil || u.Email != "" {
		return err
	}
	return s.SetEmail(handle, email)
}

// githubKeysURL is where GitHub serves an account's public SSH keys. The login
// and ".keys" are one path segment: github.com/<login>.keys, not
// github.com/<login>/.keys — the latter 404s for every account.
//
// A var, not a const, so tests can point it at a local server: this URL was
// wrong (the extra slash) from the first commit until 2026-07-15, and nothing
// caught it because there was no way to exercise it without reaching GitHub.
var githubKeysURL = "https://github.com/%s.keys"

// githubLoginOK bounds logins to GitHub's own rules (alphanumerics and single
// dashes, 39 max) so nothing exotic reaches the URL we fetch.
func githubLoginOK(login string) bool {
	if len(login) == 0 || len(login) > 39 || strings.HasPrefix(login, "-") || strings.HasSuffix(login, "-") {
		return false
	}
	for i := 0; i < len(login); i++ {
		c := login[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !alnum && !(c == '-' && login[i-1] != '-') {
			return false
		}
	}
	return true
}

// VerifyGitHubKey reports whether key is among the public keys github.com
// serves for login. Possession of a key GitHub publishes for an account proves
// control of that account to the same strength GitHub itself accepts for git
// push — which is why this needs no OAuth app, browser, or client secret.
func VerifyGitHubKey(ctx context.Context, login string, key xssh.PublicKey) (bool, error) {
	if !githubLoginOK(login) {
		return false, fmt.Errorf("invalid github login %q", login)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(githubKeysURL, login), nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, fmt.Errorf("no such github user %q", login)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("github returned %s", resp.Status)
	}
	keys, err := parseAuthorizedKeys(resp.Body)
	if err != nil {
		return false, err
	}
	want := wireOf(key)
	for _, k := range keys {
		if wireOf(k) == want {
			return true, nil
		}
	}
	return false, nil
}

// githubUserAPIURL is where GitHub serves an account's public profile. A var
// for the same reason as githubKeysURL: so tests can exercise it locally.
var githubUserAPIURL = "https://api.github.com/users/%s"

// FetchGitHubEmail returns login's public profile email, or "" when the
// profile doesn't show one — the common case, since the field is opt-in. It
// only prefills the signup email prompt, so callers treat "" and most errors
// alike: no default, just ask.
func FetchGitHubEmail(ctx context.Context, login string) (string, error) {
	if !githubLoginOK(login) {
		return "", fmt.Errorf("invalid github login %q", login)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(githubUserAPIURL, login), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github returned %s for %q", resp.Status, login)
	}
	var profile struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&profile); err != nil {
		return "", err
	}
	email := strings.TrimSpace(profile.Email)
	if !ValidEmail(email) {
		return "", nil
	}
	return email, nil
}

// FetchGitHubKeys returns every public key github.com serves for login. Used
// by `ctl keys import-github` to adopt a user's whole GitHub keyring.
func FetchGitHubKeys(ctx context.Context, login string) ([]xssh.PublicKey, error) {
	if !githubLoginOK(login) {
		return nil, fmt.Errorf("invalid github login %q", login)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(githubKeysURL, login), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github returned %s for %q", resp.Status, login)
	}
	return parseAuthorizedKeys(resp.Body)
}

// parseAuthorizedKeys reads an authorized_keys-style stream, capped so a
// misbehaving upstream can't feed us an unbounded body.
func parseAuthorizedKeys(r io.Reader) ([]xssh.PublicKey, error) {
	var out []xssh.PublicKey
	sc := bufio.NewScanner(io.LimitReader(r, 1<<20))
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, _, _, _, err := xssh.ParseAuthorizedKey([]byte(text))
		if err != nil {
			continue // skip key types we don't understand rather than failing the set
		}
		out = append(out, key)
	}
	return out, sc.Err()
}
