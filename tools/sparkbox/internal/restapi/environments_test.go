package restapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
	"github.com/vanpelt/sparky/tools/sparkbox/internal/envs"
)

// The round trip a client actually makes: name an environment, add to it, read
// it back, record a script, and remove it.
//
// What this pins that the spec tests cannot is the SHAPE crossing the wire —
// the JSON tags on ctlops.EnvironmentInfo are the contract openapi.json
// describes, and a renamed field would sail through a compile.
func TestEnvironmentRoundTrip(t *testing.T) {
	ta := newTestAPI(t)

	rec := ta.do(t, "POST", "/v1/environments", "alice", map[string]any{
		"name":        "web",
		"description": "the marketing site",
		"vars":        []map[string]string{{"name": "NODE_ENV", "value": "development"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body)
	}
	var info ctlops.EnvironmentInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Name != "web" || info.State != string(envs.StateDraft) {
		t.Fatalf("created %+v, want a draft named web", info)
	}
	if len(info.Vars) != 1 || info.Vars[0].Name != "NODE_ENV" {
		t.Errorf("vars = %+v", info.Vars)
	}
	// Never nil: a client that renders `environments[0].repos.length` must not
	// have to guard against null.
	if info.Repos == nil || info.Secrets == nil || info.Rules == nil {
		t.Errorf("a composition field serialised as null: %+v", info)
	}

	// The second call ADDS, and leaves the description alone because it names
	// none — the difference between "" and absent, which is why the field is a
	// pointer.
	rec = ta.do(t, "POST", "/v1/environments", "alice", map[string]any{
		"name": "web",
		"vars": []map[string]string{{"name": "LOG_LEVEL", "value": "debug"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", rec.Code, rec.Body)
	}
	rec = ta.do(t, "GET", "/v1/environments/web", "alice", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Description != "the marketing site" {
		t.Errorf("description = %q, want it untouched by a call that named none", info.Description)
	}
	if len(info.Vars) != 2 {
		t.Errorf("vars = %+v, want both", info.Vars)
	}

	// The listing is an object with a named array, so a cursor can be added
	// later without breaking a client's parser.
	rec = ta.do(t, "GET", "/v1/environments", "alice", nil)
	var list environmentList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Environments) != 1 || list.Environments[0].Name != "web" {
		t.Fatalf("list = %+v", list.Environments)
	}

	// A script sent here is `manual`, whatever the row said before.
	const script = "#!/usr/bin/env bash\npnpm install\n"
	rec = ta.do(t, "PUT", "/v1/environments/web/script", "alice",
		map[string]string{"script": script})
	if rec.Code != http.StatusOK {
		t.Fatalf("script.set = %d: %s", rec.Code, rec.Body)
	}
	rec = ta.do(t, "GET", "/v1/environments/web/script", "alice", nil)
	var got environmentScript
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Script != script || got.From != envs.SetupFromManual {
		t.Errorf("script = %+v, want it recorded as manual", got)
	}

	rec = ta.do(t, "DELETE", "/v1/environments/web", "alice", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rm = %d: %s", rec.Code, rec.Body)
	}
	var res ctlops.EnvDeleteResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	// The survivors are the point of the verb, and they are never null even
	// when there are none.
	if res.Repos == nil || res.Secrets == nil || res.Rules == nil {
		t.Errorf("a survivor list serialised as null: %+v", res)
	}
	if rec := ta.do(t, "GET", "/v1/environments/web", "alice", nil); rec.Code != http.StatusNotFound {
		t.Errorf("after rm, get = %d, want 404", rec.Code)
	}
}

// Somebody else's environment answers exactly like one that does not exist, on
// every route — the rule this whole API is built on, applied to a new family.
func TestEnvironmentsAreOwnerScoped(t *testing.T) {
	ta := newTestAPI(t)
	if rec := ta.do(t, "POST", "/v1/environments", "alice",
		map[string]any{"name": "web"}); rec.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body)
	}
	for _, ep := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/environments/web", nil},
		{"DELETE", "/v1/environments/web", nil},
		{"GET", "/v1/environments/web/script", nil},
		{"PUT", "/v1/environments/web/script", map[string]string{"script": "echo hi"}},
		{"POST", "/v1/environments/web/build", nil},
		{"POST", "/v1/environments/web/capture", nil},
	} {
		rec := ta.do(t, ep.method, ep.path, "mallory", ep.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as mallory = %d, want 404", ep.method, ep.path, rec.Code)
		}
	}
	// And mallory's own listing does not mention it.
	rec := ta.do(t, "GET", "/v1/environments", "mallory", nil)
	var list environmentList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Environments) != 0 {
		t.Errorf("mallory sees %+v", list.Environments)
	}
}

// `default` is refused, and the sentence says why rather than lecturing about
// the character set: an environment binds a base image, so naming it `default`
// would silently re-base every sandbox its owner ever makes.
func TestEnvironmentCannotBeNamedDefault(t *testing.T) {
	ta := newTestAPI(t)
	rec := ta.do(t, "POST", "/v1/environments", "alice", map[string]any{"name": "default"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	// The store's own sentence reaches the caller unrewritten, under this
	// package's stable code — a client switching on `code` must not have to
	// read prose, and a person reading the prose must get the argument.
	var body struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "reserved_env_name" {
		t.Errorf("code = %q", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "name IS its tag") {
		t.Errorf("message = %q, want the reason rather than a spelling lecture", body.Error.Message)
	}
}
