// Command rename-user is a one-off migration: rename a user handle across
// sparkbox.db. Handles are immutable through the normal surface, so this works
// directly on the database with the daemon stopped.
//
// Secrets need real work, not just an UPDATE: each ciphertext's GCM AAD binds
// owner|env|id, so every row is unsealed under the old owner and re-sealed
// under the new one using the same KEK (derived from the OIDC signing key).
//
// Passkeys cannot be migrated: the authenticator stored the old handle as the
// WebAuthn user handle at enrollment and login resolves the account from that
// value, so the rows are deleted and the user re-enrolls.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"

	_ "modernc.org/sqlite"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/oidc"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/secrets"
)

func main() {
	db := flag.String("db", "", "path to sparkbox.db")
	keys := flag.String("keys", "", "dir holding oidc_signing_key.pem")
	from := flag.String("from", "", "current handle")
	to := flag.String("to", "", "new handle")
	flag.Parse()
	if *db == "" || *keys == "" || *from == "" || *to == "" {
		log.Fatal("usage: rename-user -db sparkbox.db -keys <dir> -from old -to new")
	}

	key, err := oidc.LoadKey(*keys, "oidc_signing_key")
	if err != nil {
		log.Fatalf("load oidc key: %v", err)
	}
	resealer, err := secrets.NewResealer(secrets.DeriveKEK(key.D.Bytes()))
	if err != nil {
		log.Fatalf("aead: %v", err)
	}

	conn, err := sql.Open("sqlite", "file:"+*db+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	tx, err := conn.Begin()
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM users WHERE handle = ?`, *to).Scan(&exists); err != sql.ErrNoRows {
		log.Fatalf("target handle %q already exists (or: %v)", *to, err)
	}
	if err := tx.QueryRow(`SELECT 1 FROM users WHERE handle = ?`, *from).Scan(&exists); err != nil {
		log.Fatalf("no user %q: %v", *from, err)
	}

	// Re-seal secrets first, while both AADs are unambiguous.
	rows, err := tx.Query(`SELECT id, env_name, ciphertext FROM secrets WHERE owner = ?`, *from)
	if err != nil {
		log.Fatal(err)
	}
	type resealed struct {
		id   string
		blob []byte
	}
	var todo []resealed
	for rows.Next() {
		var id, env string
		var blob []byte
		if err := rows.Scan(&id, &env, &blob); err != nil {
			log.Fatal(err)
		}
		ct, err := resealer.Reseal(*from, *to, env, id, blob)
		if err != nil {
			log.Fatalf("reseal secret %s (%s): %v", id, env, err)
		}
		todo = append(todo, resealed{id, ct})
		fmt.Printf("resealed secret %s %s\n", id, env)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
	rows.Close()
	for _, r := range todo {
		if _, err := tx.Exec(`UPDATE secrets SET owner = ?, ciphertext = ? WHERE id = ?`, *to, r.blob, r.id); err != nil {
			log.Fatal(err)
		}
	}

	// Foreign keys are off by default on this connection, so parent-then-child
	// order doesn't matter.
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE users SET handle = ? WHERE handle = ?`, []any{*to, *from}},
		{`UPDATE user_keys SET handle = ? WHERE handle = ?`, []any{*to, *from}},
		{`UPDATE routes SET owner = ? WHERE owner = ?`, []any{*to, *from}},
		{`UPDATE sandbox_tags SET owner = ? WHERE owner = ?`, []any{*to, *from}},
		{`UPDATE invites SET created_by = ? WHERE created_by = ?`, []any{*to, *from}},
		{`UPDATE invites SET used_by = ? WHERE used_by = ?`, []any{*to, *from}},
		{`DELETE FROM user_passkeys WHERE handle = ?`, []any{*from}},
	} {
		res, err := tx.Exec(stmt.sql, stmt.args...)
		if err != nil {
			log.Fatalf("%s: %v", stmt.sql, err)
		}
		n, _ := res.RowsAffected()
		fmt.Printf("%-55s %d row(s)\n", stmt.sql, n)
	}

	if err := tx.Commit(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("renamed %q -> %q. passkeys were deleted: re-enroll via the login page.\n", *from, *to)
}
