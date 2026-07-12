package sshgw

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"
)

// Users maps SSH public keys to usernames. The backing file uses one entry
// per line: "<username> <authorized_keys line>", e.g.
//
//	vanpelt ssh-ed25519 AAAAC3... laptop
//
// Comments (#) and blank lines are ignored. This is the MVP identity store;
// the exe.dev model likewise identifies users purely by presented public key.
type Users struct {
	byKey map[string]string // marshaled wire key -> username
}

func LoadUsers(path string) (*Users, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	u := &Users{byKey: map[string]string{}}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, keyText, ok := strings.Cut(text, " ")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected '<user> <authorized_keys line>'", path, line)
		}
		key, _, _, _, err := xssh.ParseAuthorizedKey([]byte(keyText))
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		u.byKey[string(key.Marshal())] = name
	}
	return u, sc.Err()
}

// Lookup returns the username owning the presented key.
func (u *Users) Lookup(key gssh.PublicKey) (string, bool) {
	name, ok := u.byKey[string(key.Marshal())]
	return name, ok
}
