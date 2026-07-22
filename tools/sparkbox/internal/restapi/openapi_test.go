package restapi

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// A specification that has quietly drifted from the router is worse than none:
// it is a document people trust. These tests make drift a build failure — every
// route must be described, every description must correspond to a route, and
// the security each operation advertises must be the gate it actually passes
// through.

func specRoutes(t *testing.T) map[string]route {
	t.Helper()
	h := New(Config{})
	out := map[string]route{}
	for _, rt := range h.routes() {
		out[rt.method+" "+specPath(rt.pattern)] = rt
	}
	return out
}

func TestSpecDescribesExactlyTheRoutes(t *testing.T) {
	registered := specRoutes(t)

	documented := map[string]specOp{}
	for path, methods := range canonicalSpec.Paths {
		for method, op := range methods {
			documented[strings.ToUpper(method)+" "+path] = op
		}
	}

	for key, rt := range registered {
		op, ok := documented[key]
		if !ok {
			t.Errorf("%s is served but not documented in openapi.json", key)
			continue
		}
		// The operationId is the same string ctlops stamps on an error's Op, so
		// a client can correlate a failure with the operation that produced it
		// without parsing prose.
		if op.OperationID != rt.opID {
			t.Errorf("%s: operationId %q in the spec, %q in the route table",
				key, op.OperationID, rt.opID)
		}
	}
	for key := range documented {
		if _, ok := registered[key]; !ok {
			t.Errorf("%s is documented in openapi.json but not served", key)
		}
	}
}

// TestSpecSecurityMatchesTheGate: a public route must say so explicitly
// (`"security": []`), and an authenticated one must inherit or restate the
// document-level requirement. Getting this backwards is how a reader concludes
// they need a token for the docs page, or that they do not need one for a
// destroy.
func TestSpecSecurityMatchesTheGate(t *testing.T) {
	for key, rt := range specRoutes(t) {
		op := specOpFor(t, key)
		switch rt.auth {
		case authPublic:
			if op.Security == nil || len(*op.Security) != 0 {
				t.Errorf("%s is served without auth but does not declare \"security\": []", key)
			}
		default:
			if op.Security != nil && len(*op.Security) == 0 {
				t.Errorf("%s requires a session but declares itself open", key)
			}
			if _, ok := op.Responses["401"]; !ok {
				t.Errorf("%s requires a session but documents no 401", key)
			}
		}
	}
	// The document-level default is what the authenticated operations inherit.
	if len(canonicalSpec.Security) == 0 {
		t.Fatal("the document declares no default security requirement")
	}
	for _, want := range []string{"bearerAuth", "sessionCookie"} {
		if _, ok := canonicalSpec.Components.SecuritySchemes[want]; !ok {
			t.Errorf("securityScheme %q is not defined", want)
		}
	}
}

// TestEveryMutationDocumentsIdempotency pins the middleware to the document:
// every route behind RequireMutation passes through the replay cache, so every
// one of them accepts the header, and a client reading the spec must be able to
// see that without trying it.
func TestEveryMutationDocumentsIdempotency(t *testing.T) {
	for key, rt := range specRoutes(t) {
		if rt.auth != authMutate {
			continue
		}
		op := specOpFor(t, key)
		found := false
		for _, p := range op.Parameters {
			if p.Ref == "#/components/parameters/IdempotencyKey" ||
				strings.EqualFold(p.Name, idempotencyHeader) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s accepts %s but does not document it", key, idempotencyHeader)
		}
	}
}

// TestAsyncOperationsDocumentPrefer is an internal-consistency check: an
// operation that can answer 202 must tell the reader how to control the wait,
// and one that cannot must not pretend to.
func TestAsyncOperationsDocumentPrefer(t *testing.T) {
	for path, methods := range canonicalSpec.Paths {
		for method, op := range methods {
			where := strings.ToUpper(method) + " " + path
			_, has202 := op.Responses["202"]
			prefer := false
			for _, p := range op.Parameters {
				if p.Ref == "#/components/parameters/Prefer" || p.Name == "Prefer" {
					prefer = true
				}
			}
			if has202 != prefer {
				t.Errorf("%s: documents 202 = %v but Prefer = %v — one of them is wrong",
					where, has202, prefer)
			}
		}
	}
}

func specOpFor(t *testing.T, key string) specOp {
	t.Helper()
	for path, methods := range canonicalSpec.Paths {
		for method, op := range methods {
			if strings.ToUpper(method)+" "+path == key {
				return op
			}
		}
	}
	t.Fatalf("%s is not in the spec", key)
	return specOp{}
}

// TestEveryRefResolves walks the whole document. A dangling $ref renders as a
// blank row on the docs page rather than an error, which is exactly the kind of
// rot that survives review.
func TestEveryRefResolves(t *testing.T) {
	var doc any
	if err := json.Unmarshal(openapiSource, &doc); err != nil {
		t.Fatal(err)
	}
	var walk func(node any, where string)
	walk = func(node any, where string) {
		switch v := node.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				if !resolves(doc, ref) {
					t.Errorf("%s: $ref %q does not resolve", where, ref)
				}
			}
			for k, child := range v {
				walk(child, where+"/"+k)
			}
		case []any:
			for i, child := range v {
				walk(child, where+"/"+itoa(i))
			}
		}
	}
	walk(doc, "")
}

func resolves(doc any, ref string) bool {
	if !strings.HasPrefix(ref, "#/") {
		return false // this document has no external references, by design
	}
	cur := doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[part]
		if !ok {
			return false
		}
	}
	return true
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

var hostRe = regexp.MustCompile(`https?://([a-zA-Z0-9.-]+)`)

// TestSpecOnlyNamesThePlaceholderDomain guards the substitution in specFor: it
// rewrites the placeholder zone and nothing else, so an example accidentally
// written against a real deployment would survive into every host's copy and
// tell readers to curl somebody else's box.
func TestSpecOnlyNamesThePlaceholderDomain(t *testing.T) {
	for _, m := range hostRe.FindAllStringSubmatch(string(openapiSource), -1) {
		host := m[1]
		switch {
		case host == specDomain, strings.HasSuffix(host, "."+specDomain):
		case host == "github.com": // the one genuinely external service
		default:
			t.Errorf("the spec names %q; every example host must be under %s", host, specDomain)
		}
	}
}

func TestSpecForRewritesEveryExample(t *testing.T) {
	defaults := specHosts{API: "api.catnip.sh", Xterm: "xterm.catnip.sh", Login: "login.catnip.sh"}
	jsonBlob, yamlBlob := specFor("catnip.sh", defaults)
	for _, b := range []*blob{jsonBlob, yamlBlob} {
		body := string(b.raw)
		if strings.Contains(body, specDomain) {
			t.Errorf("%s still names %s", b.contentType, specDomain)
		}
		if !strings.Contains(body, "https://api.catnip.sh") {
			t.Errorf("%s does not name the configured zone", b.contentType)
		}
	}
	// The rendered copy is still a valid document.
	if _, err := parseSpec(jsonBlob.raw); err != nil {
		t.Fatalf("the substituted document no longer parses: %v", err)
	}

	// An unconfigured host keeps the honest placeholder rather than claiming to
	// describe "https://api.".
	generic, _ := specFor("", specHosts{})
	if !strings.Contains(string(generic.raw), "https://api."+specDomain) {
		t.Fatal("an empty domain did not leave the placeholder in place")
	}
}

// TestSpecForRewritesRelocatedSubtrees is the case the zone-only substitution
// got wrong: all three labels are configurable, and the docs page builds every
// one of its curl snippets from servers[0].url, so a host that moved a subtree
// used to publish 44 examples aimed at a subtree it does not serve.
func TestSpecForRewritesRelocatedSubtrees(t *testing.T) {
	moved := specHosts{API: "rest.catnip.sh", Xterm: "shell.catnip.sh", Login: "signin.catnip.sh"}
	jsonBlob, yamlBlob := specFor("catnip.sh", moved)
	for _, b := range []*blob{jsonBlob, yamlBlob} {
		body := string(b.raw)
		for _, stale := range []string{"api.catnip.sh", "xterm.catnip.sh", "login.catnip.sh", specDomain} {
			if strings.Contains(body, stale) {
				t.Errorf("%s still names %q after relabelling", b.contentType, stale)
			}
		}
		for _, want := range []string{
			"https://rest.catnip.sh",   // servers[0].url, and every snippet built from it
			"<name>.shell.catnip.sh",   // the terminal examples
			"https://signin.catnip.sh", // the sign-in prose
			"demo.shell.catnip.sh",     // the terminal_url example value
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not name %q", b.contentType, want)
			}
		}
	}
	if _, err := parseSpec(jsonBlob.raw); err != nil {
		t.Fatalf("the relabelled document no longer parses: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The YAML emitter
// ---------------------------------------------------------------------------

func TestYAMLPreservesKeyOrder(t *testing.T) {
	src := []byte(`{"openapi":"3.1.1","info":{"title":"x","version":"1"},"paths":{}}`)
	out, err := jsonToYAML(src)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	want := "# Generated from openapi.json — do not edit; edit the JSON.\n" +
		"openapi: \"3.1.1\"\n" +
		"info:\n" +
		"  title: \"x\"\n" +
		"  version: \"1\"\n" +
		"paths: {}\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestYAMLQuotesAmbiguousScalars(t *testing.T) {
	src := []byte(`{"a":"true","b":"3.0","c":"yes","d":true,"e":3,"f":null,` +
		`"g":"line one\nline two","h":"say \"hi\"","i":[],"j":["x"],"on":"k"}`)
	out, err := jsonToYAML(src)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		`a: "true"`,               // a string that YAML would otherwise read as a boolean
		`b: "3.0"`,                // …or as a number
		`c: "yes"`,                // …or as YAML 1.1's other boolean
		"d: true",                 // a real boolean stays bare
		"e: 3",                    // as does a real number
		"f: null",                 //
		`g: "line one\nline two"`, // newlines escape rather than break the shape
		`h: "say \"hi\""`,
		"i: []",
		`"on": "k"`, // a key YAML would read as a boolean gets quoted too
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "j:\n  - \"x\"") {
		t.Errorf("array rendering wrong in:\n%s", got)
	}
}

// TestYAMLEmitsTheWholeDocument is the cheap end-to-end check: the emitter must
// not silently drop a branch of a 2000-line document. Comparing the leaf count
// catches that without re-implementing a parser to compare trees.
func TestYAMLEmitsTheWholeDocument(t *testing.T) {
	out, err := jsonToYAML(openapiSource)
	if err != nil {
		t.Fatal(err)
	}
	yaml := string(out)
	for _, marker := range []string{
		"openapi: \"3.1.1\"",
		"\"/v1/sandboxes/{name}/terminal\":",
		"operationId: \"attach\"",
		"securitySchemes:",
		"bearerAuth:",
	} {
		if !strings.Contains(yaml, marker) {
			t.Errorf("the YAML is missing %q", marker)
		}
	}
	if lines := strings.Count(yaml, "\n"); lines < 500 {
		t.Fatalf("the YAML has only %d lines — a branch went missing", lines)
	}
}
