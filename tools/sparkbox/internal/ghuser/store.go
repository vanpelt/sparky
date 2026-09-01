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
	"strconv"
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
		CREATE TABLE IF NOT EXISTS github_user_grants (
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
		CREATE INDEX IF NOT EXISTS github_user_grants_github_id ON github_user_grants(github_id);
	`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, aead: aead}, nil
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
	_, err = s.db.Exec(`INSERT OR REPLACE INTO github_user_grants
		(owner, github_id, installation_id, repo_id, slug, access_token, refresh_token, access_expires_at, refresh_expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Owner, g.GitHubID, g.InstallationID, g.RepoID, g.Slug, access, refresh,
		g.Token.AccessExpiresAt, g.Token.RefreshExpiresAt, time.Now().UTC())
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
	err := s.db.QueryRow(`SELECT owner, github_id, installation_id, repo_id, slug, access_token, refresh_token,
		access_expires_at, refresh_expires_at FROM github_user_grants WHERE `+where, args...).
		Scan(&g.Owner, &g.GitHubID, &g.InstallationID, &g.RepoID, &g.Slug, &access, &refresh,
			&g.Token.AccessExpiresAt, &g.Token.RefreshExpiresAt)
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
	return g, nil
}

func (s *Store) Delete(owner string, repoID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM github_user_grants WHERE owner=? AND repo_id=?`, owner, repoID)
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
	return []byte(fmt.Sprintf("sparkbox-github-user-grant/v1|%s|%s|%s|%s", g.Owner,
		strconv.FormatInt(g.GitHubID, 10), strconv.FormatInt(g.RepoID, 10), kind))
}
