package hivemindsignin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubHiveMind stands in for the API, recording the one request it gets so a
// test can assert the wire shape both codebases have to agree on.
type stubHiveMind struct {
	server *httptest.Server
	paths  []string
	codes  []string
}

func newStub(t *testing.T, handler http.HandlerFunc) *stubHiveMind {
	t.Helper()
	s := &stubHiveMind{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		var in struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &in)
		s.paths = append(s.paths, r.URL.Path)
		s.codes = append(s.codes, in.Code)
		handler(w, r)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stubHiveMind) redeemer(t *testing.T) *Redeemer {
	t.Helper()
	r, err := New(Options{APIBase: s.server.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func okClaims(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sub": "hivemind:user:42", "github": "vanpelt", "github_id": 7,
		"orgs":  []string{"wandb", "some-other-org"},
		"email": "vanpelt@wandb.com", "dest": "https://my.catnip.sh/",
	})
}

func TestRedeemReadsTheClaims(t *testing.T) {
	stub := newStub(t, okClaims)

	got, err := stub.redeemer(t).Redeem(context.Background(), "hmh_abc")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got.GitHub != "vanpelt" || got.GitHubID != 7 {
		t.Fatalf("claims = %+v", got)
	}
	if got.Email != "vanpelt@wandb.com" || got.Dest != "https://my.catnip.sh/" {
		t.Fatalf("claims = %+v", got)
	}
	// The path is half of a contract with another repository.
	if len(stub.paths) != 1 || stub.paths[0] != Path {
		t.Fatalf("posted to %v, want %q", stub.paths, Path)
	}
	if len(stub.codes) != 1 || stub.codes[0] != "hmh_abc" {
		t.Fatalf("sent codes %v", stub.codes)
	}
}

func TestHasOrgIsCaseInsensitive(t *testing.T) {
	c := Claims{Orgs: []string{"WandB", " acme "}}
	for _, org := range []string{"wandb", "WANDB", "acme"} {
		if !c.HasOrg(org) {
			t.Errorf("HasOrg(%q) = false", org)
		}
	}
	if c.HasOrg("wand") {
		t.Error("HasOrg matched a prefix")
	}
	if (Claims{}).HasOrg("wandb") {
		t.Error("an empty membership list matched")
	}
}

// Unknown, spent and expired are one outcome, because the browser holding the
// code cannot tell them apart and the remedy is identical.
func TestRedeemRefusalsAreOneError(t *testing.T) {
	for _, status := range []int{
		http.StatusNotFound, http.StatusGone,
		http.StatusUnauthorized, http.StatusForbidden,
	} {
		stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no", status)
		})
		_, err := stub.redeemer(t).Redeem(context.Background(), "hmh_abc")
		if !errors.Is(err, ErrRefused) {
			t.Errorf("status %d gave err %v, want ErrRefused", status, err)
		}
	}
}

// A code that could not possibly be one never becomes an outbound request.
func TestRedeemRefusesAnImpossibleCodeWithoutAsking(t *testing.T) {
	stub := newStub(t, okClaims)
	r := stub.redeemer(t)
	for _, code := range []string{"", "   ", strings.Repeat("x", maxCodeLen+1)} {
		if _, err := r.Redeem(context.Background(), code); !errors.Is(err, ErrRefused) {
			t.Errorf("code %q gave %v, want ErrRefused", code, err)
		}
	}
	if len(stub.paths) != 0 {
		t.Fatalf("an impossible code reached the network: %v", stub.paths)
	}
}

// A 500 is not a refusal: the code may still be good, and telling somebody to
// start again would spend it for nothing.
func TestRedeemDistinguishesAnUpstreamFault(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := stub.redeemer(t).Redeem(context.Background(), "hmh_abc")
	if err == nil || errors.Is(err, ErrRefused) {
		t.Fatalf("err = %v, want an upstream failure that is not ErrRefused", err)
	}
}

// The github login is the anchor, so an answer without one fails closed rather
// than falling back to anything.
func TestRedeemFailsClosedWithoutAGitHubLogin(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": "hivemind:user:42", "orgs": []string{"wandb"}})
	})
	if _, err := stub.redeemer(t).Redeem(context.Background(), "hmh_abc"); err == nil {
		t.Fatal("a claims document with no github login was accepted")
	}
}

// A login carrying anything that could break out of a log line, a URL or a
// handle is refused before it travels.
func TestRedeemRefusesAMisshapenLogin(t *testing.T) {
	for _, login := range []string{
		"vanpelt\nX-Injected: 1",
		"vanpelt/../root",
		"vanpelt@example.com",
		"has space",
		strings.Repeat("a", 40),
	} {
		stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"github": login, "orgs": []string{"wandb"}})
		})
		if _, err := stub.redeemer(t).Redeem(context.Background(), "hmh_abc"); err == nil {
			t.Errorf("login %q was accepted", login)
		}
	}
}

// The optional fields are dropped rather than refused when they carry something
// that would not survive being rendered on one line — losing a display string
// must not fail a sign-in.
func TestRedeemDropsUnsafeOptionalFields(t *testing.T) {
	stub := newStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"github": "vanpelt",
			"email":  "vanpelt@wandb.com\r\nX-Injected: 1",
			"dest":   "https://my.catnip.sh/\nLocation: https://evil.example",
			"orgs":   []string{"wandb", "bad\norg", "   "},
		})
	})
	got, err := stub.redeemer(t).Redeem(context.Background(), "hmh_abc")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if got.Email != "" || got.Dest != "" {
		t.Fatalf("unsafe fields survived: %+v", got)
	}
	if len(got.Orgs) != 1 || got.Orgs[0] != "wandb" {
		t.Fatalf("orgs = %v", got.Orgs)
	}
}

// The API base is the same rule hivemindpresence applies to the same host.
func TestNewRefusesANonHTTPSBase(t *testing.T) {
	for _, base := range []string{"", "http://hivemind.tools", "not a url", "ftp://x.test"} {
		if _, err := New(Options{APIBase: base}); err == nil {
			t.Errorf("APIBase %q was accepted", base)
		}
	}
	for _, base := range []string{"https://api.hivemind.tools", "http://127.0.0.1:8080", "http://localhost:9"} {
		if _, err := New(Options{APIBase: base}); err != nil {
			t.Errorf("APIBase %q was refused: %v", base, err)
		}
	}
}
