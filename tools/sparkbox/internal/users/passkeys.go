package users

// Passkeys are the browser-native credential alongside SSH keys: a WebAuthn
// credential enrolled from the login page after a session-token sign-in. The
// store keeps the library's own Credential struct as JSON — the schema only
// indexes what it queries (credential id, owning handle), so a library upgrade
// that grows the struct is a no-op here.

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// ErrNoSuchPasskey is returned when an id (or prefix) matches nothing.
var ErrNoSuchPasskey = errors.New("no such passkey")

// ErrAmbiguousPasskey is returned when an id prefix matches more than one
// credential on the account.
var ErrAmbiguousPasskey = errors.New("passkey id prefix is ambiguous")

// Passkey is one WebAuthn credential linked to a user.
type Passkey struct {
	ID         string     `json:"id"` // base64url credential id
	Handle     string     `json:"handle"`
	Label      string     `json:"label"` // e.g. "MacBook Pro" — client-supplied hint
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	Credential webauthn.Credential
}

func passkeyID(credID []byte) string {
	return base64.RawURLEncoding.EncodeToString(credID)
}

// AddPasskey links a new WebAuthn credential to an existing user.
func (s *Store) AddPasskey(handle, label string, cred webauthn.Credential) error {
	blob, err := json.Marshal(cred)
	if err != nil {
		return err
	}
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
	if _, err := tx.Exec(
		`INSERT INTO user_passkeys (cred_id, handle, label, credential, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		passkeyID(cred.ID), handle, label, string(blob), time.Now().UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

// Passkeys lists a user's passkeys, oldest first.
func (s *Store) Passkeys(handle string) ([]Passkey, error) {
	rows, err := s.db.Query(
		`SELECT cred_id, handle, label, credential, created_at, last_used_at
		 FROM user_passkeys WHERE handle = ? ORDER BY created_at, cred_id`, handle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var p Passkey
		var blob string
		var lastUsed sql.NullTime
		if err := rows.Scan(&p.ID, &p.Handle, &p.Label, &blob, &p.CreatedAt, &lastUsed); err != nil {
			return nil, err
		}
		if lastUsed.Valid {
			t := lastUsed.Time
			p.LastUsedAt = &t
		}
		if err := json.Unmarshal([]byte(blob), &p.Credential); err != nil {
			return nil, fmt.Errorf("passkey %s: corrupt credential: %w", p.ID, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// HasPasskeys reports whether the account has at least one passkey — the login
// flow uses it to decide whether to offer enrollment after a token sign-in.
func (s *Store) HasPasskeys(handle string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM user_passkeys WHERE handle = ?`, handle).Scan(&n)
	return n > 0, err
}

// UpdatePasskey rewrites a credential after a successful assertion (the sign
// counter moved) and stamps last_used_at.
func (s *Store) UpdatePasskey(handle string, cred webauthn.Credential) error {
	blob, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(
		`UPDATE user_passkeys SET credential = ?, last_used_at = ? WHERE cred_id = ? AND handle = ?`,
		string(blob), time.Now().UTC(), passkeyID(cred.ID), handle)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchPasskey
	}
	return nil
}

// RemovePasskey unlinks a credential by id, accepting an unambiguous prefix so
// the ctl command doesn't demand a full 40-plus-character paste. Unlike SSH
// keys there is no last-passkey rule: the SSH key is the root credential, so
// removing every passkey merely reverts the account to token sign-in.
func (s *Store) RemovePasskey(handle, idPrefix string) error {
	idPrefix = strings.TrimSpace(idPrefix)
	if idPrefix == "" {
		return ErrNoSuchPasskey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT cred_id FROM user_passkeys WHERE handle = ? AND cred_id LIKE ? ESCAPE '\'`,
		handle, likePrefix(idPrefix))
	if err != nil {
		return err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		matches = append(matches, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	switch len(matches) {
	case 0:
		return ErrNoSuchPasskey
	case 1:
	default:
		return ErrAmbiguousPasskey
	}
	_, err = s.db.Exec(`DELETE FROM user_passkeys WHERE handle = ? AND cred_id = ?`, handle, matches[0])
	return err
}

// likePrefix escapes LIKE metacharacters in p and appends the wildcard, so a
// credential id containing % or _ (possible in base64url? only -_ — _ is a
// LIKE wildcard) matches literally.
func likePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(p) + "%"
}
