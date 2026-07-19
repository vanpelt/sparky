package users

import (
	"errors"
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
)

func testCred(id string) webauthn.Credential {
	return webauthn.Credential{
		ID:        []byte(id),
		PublicKey: []byte("pk-" + id),
		Authenticator: webauthn.Authenticator{
			AAGUID:    []byte("aaguid-0000000"),
			SignCount: 1,
		},
	}
}

func TestPasskeyLifecycle(t *testing.T) {
	s := testStore(t)
	key, _ := newKey(t, "laptop")
	if err := s.Create("vanpelt", key, "laptop", "signup", "operator"); err != nil {
		t.Fatal(err)
	}

	if has, err := s.HasPasskeys("vanpelt"); err != nil || has {
		t.Fatalf("HasPasskeys before add = %v, %v; want false, nil", has, err)
	}
	if err := s.AddPasskey("vanpelt", "MacBook", testCred("credential-one")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPasskey("ghost", "x", testCred("credential-two")); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("AddPasskey for missing user = %v; want ErrNoSuchUser", err)
	}
	if has, err := s.HasPasskeys("vanpelt"); err != nil || !has {
		t.Fatalf("HasPasskeys after add = %v, %v; want true, nil", has, err)
	}

	pks, err := s.Passkeys("vanpelt")
	if err != nil || len(pks) != 1 {
		t.Fatalf("Passkeys = %v, %v; want one", pks, err)
	}
	pk := pks[0]
	if pk.Label != "MacBook" || string(pk.Credential.ID) != "credential-one" ||
		string(pk.Credential.PublicKey) != "pk-credential-one" || pk.LastUsedAt != nil {
		t.Fatalf("round-tripped passkey mismatch: %+v", pk)
	}

	// A successful assertion bumps the counter and stamps last_used_at.
	cred := pk.Credential
	cred.Authenticator.SignCount = 7
	if err := s.UpdatePasskey("vanpelt", cred); err != nil {
		t.Fatal(err)
	}
	pks, _ = s.Passkeys("vanpelt")
	if pks[0].Credential.Authenticator.SignCount != 7 || pks[0].LastUsedAt == nil {
		t.Fatalf("update not persisted: %+v", pks[0])
	}
	if err := s.UpdatePasskey("someoneelse", cred); !errors.Is(err, ErrNoSuchPasskey) {
		t.Fatalf("UpdatePasskey under wrong handle = %v; want ErrNoSuchPasskey", err)
	}
}

func TestPasskeyRemovalByPrefix(t *testing.T) {
	s := testStore(t)
	key, _ := newKey(t, "laptop")
	if err := s.Create("vanpelt", key, "laptop", "signup", "operator"); err != nil {
		t.Fatal(err)
	}
	// "credA"/"credB" share the b64url prefix of "cred" ("Y3JlZ..."), so a
	// short prefix is ambiguous and a longer one selects exactly one.
	if err := s.AddPasskey("vanpelt", "a", testCred("credA")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPasskey("vanpelt", "b", testCred("credB")); err != nil {
		t.Fatal(err)
	}
	pks, _ := s.Passkeys("vanpelt")
	if len(pks) != 2 {
		t.Fatalf("want 2 passkeys, got %d", len(pks))
	}
	common := pks[0].ID[:4]
	if err := s.RemovePasskey("vanpelt", common); !errors.Is(err, ErrAmbiguousPasskey) {
		t.Fatalf("ambiguous prefix = %v; want ErrAmbiguousPasskey", err)
	}
	if err := s.RemovePasskey("vanpelt", pks[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePasskey("vanpelt", "zzzz"); !errors.Is(err, ErrNoSuchPasskey) {
		t.Fatalf("missing id = %v; want ErrNoSuchPasskey", err)
	}
	// Removing the last passkey is allowed — SSH keys remain the root credential.
	if err := s.RemovePasskey("vanpelt", pks[1].ID); err != nil {
		t.Fatal(err)
	}
	if has, _ := s.HasPasskeys("vanpelt"); has {
		t.Error("passkeys remain after removing both")
	}
}
