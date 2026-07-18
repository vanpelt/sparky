// Package users is the sqlite-backed identity store: who may connect, which
// SSH keys are theirs, and which invite codes are still live.
//
// SSH public keys are the sole credential — there are no passwords and no
// cookies for users. A key is looked up by its exact wire bytes, so
// authentication is a single indexed point lookup, the same strength as the
// flat users.conf map it replaces. Everything downstream keeps using the
// handle string.
//
// State lives in the same sqlite file as the proxy routes (internal/routes),
// on its own connection — WAL makes that safe and keeps the packages
// decoupled.
package users

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	xssh "golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite"
)

var (
	// ErrNoSuchUser is returned when a handle has no record.
	ErrNoSuchUser = errors.New("no such user")
	// ErrHandleTaken is returned when a handle is already claimed.
	ErrHandleTaken = errors.New("handle already taken")
	// ErrKeyLinked is returned when a key already belongs to some user.
	ErrKeyLinked = errors.New("key is already linked to an account")
	// ErrBadInvite covers unknown, spent, and expired codes alike — the caller
	// should not learn which.
	ErrBadInvite = errors.New("invite code is not valid")
	// ErrLastKey is returned when removing a key would lock the user out.
	ErrLastKey = errors.New("refusing to remove the last key on the account")
)

// handleRe bounds handles to what is safe in an OIDC `sub` claim. Handles are
// immutable once claimed: they appear in `sub`, and a rename would silently
// break every CEL policy written against it.
var handleRe = regexp.MustCompile(`^[a-z0-9-]{2,32}$`)

// reserved handles would collide with the gateway's own doors and subdomains.
var reserved = map[string]bool{
	"new": true, "ctl": true, "signup": true, "console": true, "oidc": true,
	"login": true, "admin": true, "root": true, "sparkbox": true, "www": true,
	"my": true, // user console subdomain
}

// ValidHandle reports whether h is a claimable handle.
func ValidHandle(h string) bool { return handleRe.MatchString(h) && !reserved[h] }

// Key is one SSH public key linked to a user.
type Key struct {
	FP            string    `json:"fp"`    // "SHA256:..."
	Label         string    `json:"label"` // free-text comment from the key line
	AuthorizedKey string    `json:"authorized_key"`
	AddedAt       time.Time `json:"added_at"`
	Via           string    `json:"via"` // seed | signup | ctl | github-import
}

// User is an account. GitHub is populated only when verified.
type User struct {
	Handle           string     `json:"handle"`
	CreatedAt        time.Time  `json:"created_at"`
	Status           string     `json:"status"` // active | disabled
	InvitedBy        string     `json:"invited_by,omitempty"`
	Email            string     `json:"email,omitempty"`
	GitHubLogin      string     `json:"github_login,omitempty"`
	GitHubVerifiedAt *time.Time `json:"github_verified_at,omitempty"`
}

// OperatorInviter marks the accounts seeded from users.conf. That file is
// written by whoever provisions the host, so its entries are the operators —
// which is what lets `ctl invite` work on a fresh box with no extra config.
// Accounts created through signup carry their inviter's handle instead.
const OperatorInviter = "operator"

// IsOperator reports whether the account was operator-blessed via users.conf
// rather than invited by another user.
func (u User) IsOperator() bool { return u.InvitedBy == OperatorInviter }

type Store struct {
	mu sync.Mutex // serialises writes (sqlite is single-writer)
	db *sql.DB
}

// Open opens (creating if needed) the sqlite database at path and applies the
// schema.
func Open(path string) (*Store, error) {
	// DSN pragmas run on every pooled connection (a db.Exec pragma binds to
	// just one), and _txlock=immediate takes the write lock at Begin, where
	// busy_timeout applies — every transaction in this store writes. See the
	// fuller rationale in secrets.Open; the stores share sparkbox.db.
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// Redundant with the DSN, but an unsupported pragma fails Open loudly.
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close() //nolint:errcheck
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			handle             TEXT PRIMARY KEY,
			created_at         TIMESTAMP NOT NULL,
			status             TEXT NOT NULL DEFAULT 'active',
			invited_by         TEXT,
			github_login       TEXT,
			github_verified_at TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS user_keys (
			fp             TEXT PRIMARY KEY,
			handle         TEXT NOT NULL REFERENCES users(handle) ON DELETE CASCADE,
			wire           TEXT NOT NULL,
			authorized_key TEXT NOT NULL,
			label          TEXT NOT NULL DEFAULT '',
			added_at       TIMESTAMP NOT NULL,
			via            TEXT NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS user_keys_wire ON user_keys(wire);
		CREATE INDEX IF NOT EXISTS user_keys_handle ON user_keys(handle);
		CREATE TABLE IF NOT EXISTS invites (
			code_hash  TEXT PRIMARY KEY,
			created_by TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			used_at    TIMESTAMP,
			used_by    TEXT
		);
	`); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	// Migration: the email column postdates the authenticated proxy, which
	// forwards it upstream as X-Forwarded-Email. Nullable — an account without a
	// set email simply has the header omitted.
	if err := addColumnIfMissing(db, "users", "email", "TEXT"); err != nil {
		db.Close() //nolint:errcheck
		return nil, err
	}
	return &Store{db: db}, nil
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN unless the column already
// exists (sqlite has no ADD COLUMN IF NOT EXISTS, and a bare ALTER errors on
// the second boot).
func addColumnIfMissing(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// wireOf is the exact-match lookup key for a public key: its SSH wire
// encoding, which is what the client proves possession of.
func wireOf(key xssh.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key.Marshal())
}

// Lookup returns the active user owning the presented key. Disabled accounts
// do not authenticate.
func (s *Store) Lookup(key xssh.PublicKey) (string, bool) {
	var handle, status string
	err := s.db.QueryRow(
		`SELECT u.handle, u.status FROM user_keys k
		 JOIN users u ON u.handle = k.handle WHERE k.wire = ?`,
		wireOf(key),
	).Scan(&handle, &status)
	if err != nil || status != "active" {
		return "", false
	}
	return handle, true
}

// Get returns one user record.
func (s *Store) Get(handle string) (User, error) {
	var u User
	var invitedBy, ghLogin, email sql.NullString
	var ghAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT handle, created_at, status, invited_by, email, github_login, github_verified_at
		 FROM users WHERE handle = ?`, handle,
	).Scan(&u.Handle, &u.CreatedAt, &u.Status, &invitedBy, &email, &ghLogin, &ghAt)
	if err == sql.ErrNoRows {
		return User{}, ErrNoSuchUser
	}
	if err != nil {
		return User{}, err
	}
	u.InvitedBy = invitedBy.String
	u.Email = email.String
	u.GitHubLogin = ghLogin.String
	if ghAt.Valid {
		t := ghAt.Time
		u.GitHubVerifiedAt = &t
	}
	return u, nil
}

// List returns every user, handle-sorted.
func (s *Store) List() ([]User, error) {
	rows, err := s.db.Query(
		`SELECT handle, created_at, status, invited_by, email, github_login, github_verified_at
		 FROM users ORDER BY handle`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var invitedBy, ghLogin, email sql.NullString
		var ghAt sql.NullTime
		if err := rows.Scan(&u.Handle, &u.CreatedAt, &u.Status, &invitedBy, &email, &ghLogin, &ghAt); err != nil {
			return nil, err
		}
		u.InvitedBy = invitedBy.String
		u.Email = email.String
		u.GitHubLogin = ghLogin.String
		if ghAt.Valid {
			t := ghAt.Time
			u.GitHubVerifiedAt = &t
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Create registers a new user with one initial key. invitedBy records who
// authorised the account ("operator" for seeded entries).
func (s *Store) Create(handle string, key xssh.PublicKey, label, via, invitedBy string) error {
	if !ValidHandle(handle) {
		return fmt.Errorf("invalid handle %q (2-32 chars of a-z, 0-9, dash; some names are reserved)", handle)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM users WHERE handle = ?`, handle).Scan(&exists); err == nil {
		return ErrHandleTaken
	} else if err != sql.ErrNoRows {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(
		`INSERT INTO users (handle, created_at, status, invited_by) VALUES (?, ?, 'active', ?)`,
		handle, now, invitedBy); err != nil {
		return err
	}
	if err := addKeyTx(tx, handle, key, label, via, now); err != nil {
		return err
	}
	return tx.Commit()
}

// AddKey links another key to an existing user.
func (s *Store) AddKey(handle string, key xssh.PublicKey, label, via string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM users WHERE handle = ?`, handle).Scan(&exists); err == sql.ErrNoRows {
		return ErrNoSuchUser
	} else if err != nil {
		return err
	}
	if err := addKeyTx(tx, handle, key, label, via, time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// addKeyTx inserts one key, refusing to move a key that another account
// already claims. Re-adding a key the same user already has is a no-op, so
// `keys import-github` is idempotent.
func addKeyTx(tx *sql.Tx, handle string, key xssh.PublicKey, label, via string, now time.Time) error {
	wire := wireOf(key)
	var owner string
	err := tx.QueryRow(`SELECT handle FROM user_keys WHERE wire = ?`, wire).Scan(&owner)
	switch {
	case err == nil && owner == handle:
		return nil
	case err == nil:
		return ErrKeyLinked
	case err != sql.ErrNoRows:
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO user_keys (fp, handle, wire, authorized_key, label, added_at, via)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		xssh.FingerprintSHA256(key), handle, wire,
		strings.TrimSpace(string(xssh.MarshalAuthorizedKey(key))), label, now, via)
	return err
}

// Keys lists a user's keys, oldest first.
func (s *Store) Keys(handle string) ([]Key, error) {
	rows, err := s.db.Query(
		`SELECT fp, label, authorized_key, added_at, via FROM user_keys
		 WHERE handle = ? ORDER BY added_at, fp`, handle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.FP, &k.Label, &k.AuthorizedKey, &k.AddedAt, &k.Via); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RemoveKey unlinks a key by fingerprint, refusing to remove the account's
// last key (which would lock the user out for good).
func (s *Store) RemoveKey(handle, fp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	// Check the key exists before the last-key rule, so a typo'd fingerprint is
	// reported as such rather than as "refusing to remove the last key".
	var owned int
	err = tx.QueryRow(`SELECT 1 FROM user_keys WHERE handle = ? AND fp = ?`, handle, fp).Scan(&owned)
	if err == sql.ErrNoRows {
		return fmt.Errorf("no key %s on this account", fp)
	} else if err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM user_keys WHERE handle = ?`, handle).Scan(&n); err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastKey
	}
	if _, err := tx.Exec(`DELETE FROM user_keys WHERE handle = ? AND fp = ?`, handle, fp); err != nil {
		return err
	}
	return tx.Commit()
}

// emailRe is a deliberately loose sanity check: a local part, an @, and a
// dotted domain. The edge only forwards this string as a header; it is not an
// address we send mail to, so we reject the obviously-malformed and no more.
var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// ValidEmail reports whether e passes the loose header-safety check.
func ValidEmail(e string) bool { return len(e) <= 254 && emailRe.MatchString(e) }

// SetEmail records (or clears, with "") the account's email. The edge forwards
// it upstream as X-Forwarded-Email once set.
func (s *Store) SetEmail(handle, email string) error {
	email = strings.TrimSpace(email)
	if email != "" && !ValidEmail(email) {
		return fmt.Errorf("that doesn't look like an email address")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var value any
	if email != "" {
		value = email
	}
	res, err := s.db.Exec(`UPDATE users SET email = ? WHERE handle = ?`, value, handle)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchUser
	}
	return nil
}

// LinkGitHub records a verified GitHub login. Callers must have verified the
// linkage first (see VerifyGitHubKey) — this only writes the claim.
func (s *Store) LinkGitHub(handle, login string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`UPDATE users SET github_login = ?, github_verified_at = ? WHERE handle = ?`,
		login, time.Now().UTC(), handle)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchUser
	}
	return nil
}

// ---- invites ---------------------------------------------------------------

// InviteTTL is how long a fresh invite code stays usable.
const InviteTTL = 7 * 24 * time.Hour

// NewInvite mints a single-use code and stores only its hash, so a database
// read never yields a usable code.
func (s *Store) NewInvite(createdBy string) (string, error) {
	code, err := randomCode()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if _, err := s.db.Exec(
		`INSERT INTO invites (code_hash, created_by, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hashCode(code), createdBy, now, now.Add(InviteTTL)); err != nil {
		return "", err
	}
	return code, nil
}

// InviteCount is how many invites a user has minted that are still live or
// already spent — i.e. what counts against their quota. Expired-unused codes
// are not charged: the invite was never taken up.
func (s *Store) InviteCount(handle string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT count(*) FROM invites
		 WHERE created_by = ? AND (used_at IS NOT NULL OR expires_at > ?)`,
		handle, time.Now().UTC()).Scan(&n)
	return n, err
}

// RedeemInvite spends a code for handle. It is atomic: the UPDATE's WHERE
// clause is the check, so two racing signups cannot both claim one code.
func (s *Store) RedeemInvite(code, handle string) (createdBy string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck
	now := time.Now().UTC()
	res, err := tx.Exec(
		`UPDATE invites SET used_at = ?, used_by = ?
		 WHERE code_hash = ? AND used_at IS NULL AND expires_at > ?`,
		now, handle, hashCode(code), now)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", ErrBadInvite
	}
	if err := tx.QueryRow(`SELECT created_by FROM invites WHERE code_hash = ?`,
		hashCode(code)).Scan(&createdBy); err != nil {
		return "", err
	}
	return createdBy, tx.Commit()
}

// AttributeInvite records which handle spent a code, once signup knows it.
// RedeemInvite reserves the code before the account exists, so used_by is
// filled in here; this is bookkeeping, not part of the race.
func (s *Store) AttributeInvite(code, handle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE invites SET used_by = ? WHERE code_hash = ? AND used_at IS NOT NULL`,
		handle, hashCode(code))
	return err
}

// ReleaseInvite un-spends a code. Signup redeems before creating the account
// (so the race is decided by the database); if the create then fails, the code
// must not be burned.
func (s *Store) ReleaseInvite(code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`UPDATE invites SET used_at = NULL, used_by = NULL WHERE code_hash = ?`, hashCode(code))
	return err
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(normalizeCode(code)))
	return hex.EncodeToString(sum[:])
}

// normalizeCode makes codes forgiving to type: case and dashes don't matter.
func normalizeCode(code string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(code), "-", ""))
}

// codeAlphabet omits vowels (no accidental words) and the 0/o/1/l/i lookalikes.
const codeAlphabet = "23456789bcdfghjkmnpqrstvwxz"

// randomCode returns a "xxxx-xxxx" code with ~38 bits of entropy — plenty for
// a single-use, expiring, operator-issued invite. Rejection sampling keeps the
// alphabet uniform (256 isn't a multiple of 27).
func randomCode() (string, error) {
	const max = 256 - 256%len(codeAlphabet)
	out := make([]byte, 0, 9)
	buf := make([]byte, 1)
	for len(out) < 9 {
		if len(out) == 4 {
			out = append(out, '-')
			continue
		}
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		if int(buf[0]) >= max {
			continue
		}
		out = append(out, codeAlphabet[int(buf[0])%len(codeAlphabet)])
	}
	return string(out), nil
}
