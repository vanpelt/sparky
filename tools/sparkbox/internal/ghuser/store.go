package ghuser

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/hkdf"
	_ "modernc.org/sqlite"
)

var ErrNoGrant = errors.New("repository has no user authorization")

type Grant struct {
	Owner          string
	GitHubID       int64
	InstallationID int64
	RepoID         int64
	Slug           string
	Token          Token
	// Permissions is what the scoped token GitHub actually issued carries — not
	// what was asked for. The two differ whenever the platform's minted set is
	// widened: an existing user authorization was consented to under the old
	// set and cannot be re-scoped past it, so the refresh falls back and this
	// records the narrower reality.
	//
	// It is stored so the console can say "this grant predates the permissions
	// this host now mints, re-authorize it" without a round trip to github.com
	// on a four-second poll, and so a refresh knows what it is allowed to ask
	// for a second time. Not secret — a permission name is configuration — so
	// it is a plain column and not part of the sealed AAD, which also means
	// widening it later cannot orphan a stored ciphertext.
	Permissions map[string]string
}

type Store struct {
	mu   sync.Mutex
	db   *sql.DB
	aead cipher.AEAD
}

func DeriveKEK(ikm []byte) []byte {
	r := hkdf.New(sha256.New, ikm, nil, []byte("sparkbox-github-user-grants-kek/v1"))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		panic("ghuser: hkdf: " + err.Error())
	}
	return key
}

func Open(path string, kek []byte) (*Store, error) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS github_scoped_user_grants (
			owner TEXT NOT NULL,
			github_id INTEGER NOT NULL,
			installation_id INTEGER NOT NULL,
			repo_id INTEGER NOT NULL,
			slug TEXT NOT NULL COLLATE NOCASE,
			access_token BLOB NOT NULL,
			refresh_token BLOB NOT NULL,
			access_expires_at TIMESTAMP NOT NULL,
			refresh_expires_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (owner, slug),
			UNIQUE (owner, repo_id)
		);
		CREATE INDEX IF NOT EXISTS github_scoped_user_grants_github_id ON github_scoped_user_grants(github_id);
	`); err != nil {
		db.Close()
		return nil, err
	}
	// Grants written before the minted permission set became data carry no
	// record of what they were consented to under. They are left empty rather
	// than backfilled with a guess: the old authorize path hardcoded `write` on
	// all three core permissions but Narrow could have downgraded any of them,
	// so a backfill would be wrong in exactly the direction that makes a
	// refresh ask for more than the grant holds. An empty column reads as
	// "unknown", and Manager treats unknown as "try the full set, fall back".
	if err := addColumnIfMissing(db, "github_scoped_user_grants", "permissions", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, aead: aead}, nil
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN unless the column already
// exists. sqlite has no ADD COLUMN IF NOT EXISTS, and a bare ALTER errors on
// the second boot, so we consult table_info first. This package's own copy, per
// the deliberate per-package duplication convention.
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

// encodePermissions renders a permission set as a stable, sorted, greppable
// string: "actions=read,contents=write". Sorted so the same set never produces
// two different rows, and text rather than JSON so a human reading the table
// with sqlite3 can see what a grant holds.
func encodePermissions(perms map[string]string) string {
	if len(perms) == 0 {
		return ""
	}
	parts := make([]string, 0, len(perms))
	for name, level := range perms {
		parts = append(parts, name+"="+level)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// decodePermissions is encodePermissions' inverse. A malformed entry is
// dropped rather than failing the read: this column is advisory — it decides
// whether a nudge is shown and which set a refresh retries with — and a grant
// that still holds a working token must not become unreadable because one
// field was written by something else.
func decodePermissions(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		name, level, ok := strings.Cut(part, "=")
		if !ok || name == "" || level == "" {
			continue
		}
		out[name] = level
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Put(g Grant) error {
	if g.Owner == "" || g.GitHubID <= 0 || g.InstallationID <= 0 || g.RepoID <= 0 || g.Slug == "" ||
		g.Token.AccessToken == "" || g.Token.RefreshToken == "" {
		return errors.New("incomplete github user grant")
	}
	access, err := s.seal(g, "access", g.Token.AccessToken)
	if err != nil {
		return err
	}
	refresh, err := s.seal(g, "refresh", g.Token.RefreshToken)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.db.Exec(`INSERT OR REPLACE INTO github_scoped_user_grants
		(owner, github_id, installation_id, repo_id, slug, access_token, refresh_token, access_expires_at, refresh_expires_at, updated_at, permissions)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Owner, g.GitHubID, g.InstallationID, g.RepoID, g.Slug, access, refresh,
		g.Token.AccessExpiresAt, g.Token.RefreshExpiresAt, time.Now().UTC(), encodePermissions(g.Permissions))
	return err
}

func (s *Store) Get(owner string, repoID int64) (Grant, error) {
	return s.get(`owner=? AND repo_id=?`, owner, repoID)
}

// GetBySlug is the hot credential path. The immutable repository id was
// established and stored during authorization; resolving it from GitHub again
// on every git credential request would make the bot fallback depend on an
// extra network round trip.
func (s *Store) GetBySlug(owner, slug string) (Grant, error) {
	return s.get(`owner=? AND slug=? COLLATE NOCASE`, owner, slug)
}

func (s *Store) get(where string, args ...any) (Grant, error) {
	var g Grant
	var access, refresh []byte
	var perms string
	err := s.db.QueryRow(`SELECT owner, github_id, installation_id, repo_id, slug, access_token, refresh_token,
		access_expires_at, refresh_expires_at, permissions FROM github_scoped_user_grants WHERE `+where, args...).
		Scan(&g.Owner, &g.GitHubID, &g.InstallationID, &g.RepoID, &g.Slug, &access, &refresh,
			&g.Token.AccessExpiresAt, &g.Token.RefreshExpiresAt, &perms)
	if errors.Is(err, sql.ErrNoRows) {
		return Grant{}, ErrNoGrant
	}
	if err != nil {
		return Grant{}, err
	}
	g.Token.AccessToken, err = s.open(g, "access", access)
	if err != nil {
		return Grant{}, err
	}
	g.Token.RefreshToken, err = s.open(g, "refresh", refresh)
	if err != nil {
		return Grant{}, err
	}
	g.Permissions = decodePermissions(perms)
	return g, nil
}

func (s *Store) Delete(owner string, repoID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM github_scoped_user_grants WHERE owner=? AND repo_id=?`, owner, repoID)
	return err
}

func (s *Store) seal(g Grant, kind, value string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(value), aad(g, kind)), nil
}

func (s *Store) open(g Grant, kind string, blob []byte) (string, error) {
	if len(blob) < s.aead.NonceSize() {
		return "", errors.New("github user grant cannot be decrypted")
	}
	plain, err := s.aead.Open(nil, blob[:s.aead.NonceSize()], blob[s.aead.NonceSize():], aad(g, kind))
	if err != nil {
		return "", errors.New("github user grant cannot be decrypted")
	}
	return string(plain), nil
}

func aad(g Grant, kind string) []byte {
	return []byte(fmt.Sprintf("sparkbox-github-user-grant/v2|%s|%s|%s|%s", g.Owner,
		strconv.FormatInt(g.GitHubID, 10), strconv.FormatInt(g.RepoID, 10), kind))
}
