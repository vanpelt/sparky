package restapi

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/webui"
)

//go:embed openapi.json
var openapiSource []byte

// specDomain is the placeholder zone the canonical document is authored
// against. Every occurrence — the server URL, every example host, every curl
// snippet — is rewritten to the real zone when a Handler is built, so the
// examples a reader copies out of /docs are examples that actually run against
// the host they are reading. openapi_test.go asserts the file names no other
// domain, so this substitution is total.
const specDomain = "example.com"

// The canonical document, parsed once at init. A malformed spec panics here
// rather than 500ing on the first request, the same bargain internal/webui
// makes: this is developer-controlled embedded content, and a broken build
// should refuse to start.
var canonicalSpec = mustParseSpec(openapiSource)

// specDoc is the slice of OpenAPI this package validates and the spec test
// walks. It is deliberately partial — nothing here re-implements a schema
// validator; it checks the things a hand-authored document actually gets wrong.
type specDoc struct {
	OpenAPI string `json:"openapi"`
	Info    struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	Servers []struct {
		URL string `json:"url"`
	} `json:"servers"`
	Paths      map[string]map[string]specOp `json:"paths"`
	Components struct {
		Schemas         map[string]json.RawMessage `json:"schemas"`
		SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
	} `json:"components"`
	Security []map[string][]string `json:"security"`
}

type specOp struct {
	OperationID string                  `json:"operationId"`
	Summary     string                  `json:"summary"`
	Tags        []string                `json:"tags"`
	Security    *[]map[string][]string  `json:"security"`
	Parameters  []specParam             `json:"parameters"`
	Responses   map[string]specResponse `json:"responses"`
}

// specParam is an inline parameter or a $ref into components/parameters.
type specParam struct {
	Ref  string `json:"$ref"`
	Name string `json:"name"`
	In   string `json:"in"`
}

// specResponse is either an inline response object or a $ref into
// components/responses — the shared 401/404/501 answers are declared once and
// referenced everywhere, so validation has to accept both shapes.
type specResponse struct {
	Ref         string `json:"$ref"`
	Description string `json:"description"`
}

func mustParseSpec(src []byte) *specDoc {
	doc, err := parseSpec(src)
	if err != nil {
		panic("restapi: openapi.json: " + err.Error())
	}
	return doc
}

// parseSpec unmarshals and sanity-checks the document. The checks are the ones
// that catch a hand-edit: a wrong version, an empty paths object, a duplicated
// operationId (legal JSON, illegal OpenAPI, and silently breaks generators), a
// response with no description (required by the spec), and an operation nobody
// gave an id — which would make the route/spec bijection unfalsifiable.
func parseSpec(src []byte) (*specDoc, error) {
	var doc specDoc
	dec := json.NewDecoder(bytes.NewReader(src))
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		return nil, fmt.Errorf("openapi version %q, want 3.1.x", doc.OpenAPI)
	}
	if doc.Info.Title == "" || doc.Info.Version == "" {
		return nil, fmt.Errorf("info.title and info.version are required")
	}
	if len(doc.Paths) == 0 {
		return nil, fmt.Errorf("paths is empty")
	}
	seen := map[string]string{}
	for path, methods := range doc.Paths {
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("path %q does not start with /", path)
		}
		for method, op := range methods {
			where := strings.ToUpper(method) + " " + path
			if op.OperationID == "" {
				return nil, fmt.Errorf("%s has no operationId", where)
			}
			if prev, dup := seen[op.OperationID]; dup {
				return nil, fmt.Errorf("operationId %q used by both %s and %s",
					op.OperationID, prev, where)
			}
			seen[op.OperationID] = where
			if op.Summary == "" {
				return nil, fmt.Errorf("%s has no summary", where)
			}
			if len(op.Responses) == 0 {
				return nil, fmt.Errorf("%s documents no responses", where)
			}
			for code, resp := range op.Responses {
				if resp.Description == "" && resp.Ref == "" {
					return nil, fmt.Errorf("%s response %s has neither a description nor a $ref", where, code)
				}
			}
		}
	}
	return &doc, nil
}

// blob is a pre-compressed static body. Same bargain as internal/webui.Page:
// compute both encodings once, then every request is a header write and a copy.
type blob struct {
	contentType string
	raw         []byte
	gzipped     []byte
}

func newBlob(contentType string, body []byte) *blob {
	var gz bytes.Buffer
	zw, err := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	if err != nil {
		panic("restapi: gzip writer: " + err.Error())
	}
	if _, err := zw.Write(body); err != nil {
		panic("restapi: gzip write: " + err.Error())
	}
	if err := zw.Close(); err != nil {
		panic("restapi: gzip close: " + err.Error())
	}
	return &blob{contentType: contentType, raw: body, gzipped: gz.Bytes()}
}

func (b *blob) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", b.contentType)
	h.Set("Vary", "Accept-Encoding")
	// No-store rather than a max-age: the document changes only on redeploy,
	// but a cached spec that disagrees with the running binary is exactly the
	// drift this package's test exists to prevent.
	h.Set("Cache-Control", "no-store")
	body := b.raw
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			h.Set("Content-Encoding", "gzip")
			body = b.gzipped
			break
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body) //nolint:errcheck
}

// specHosts are the three subtrees the canonical document names by label:
// api.<zone> in servers[0].url and every curl snippet built from it,
// xterm.<zone> in the terminal examples, login.<zone> in the auth prose. They
// are all relocatable at runtime (--api-subdomain, --xterm-subdomain,
// --login-subdomain), so substituting only the zone would leave a relabelled
// deployment's docs page pointing every one of its copy-paste examples at a
// host it does not serve.
type specHosts struct {
	API   string // "rest.catnip.sh"
	Xterm string // "shell.catnip.sh"
	Login string // "signin.catnip.sh"
}

// specFor renders this host's copy of the document. An empty domain leaves the
// placeholder in place, which is what a unit test and a host with no
// --proxy-domain both want: a document that is honest about being generic
// beats one claiming to describe "https://api.".
func specFor(domain string, hosts specHosts) (jsonBlob, yamlBlob *blob) {
	src := openapiSource
	if domain != "" && domain != specDomain {
		// Full hosts before the bare zone: once "example.com" has become
		// "catnip.sh", "api.example.com" no longer exists to be found.
		for _, sub := range []struct{ from, to string }{
			{"api." + specDomain, hosts.API},
			{"xterm." + specDomain, hosts.Xterm},
			{"login." + specDomain, hosts.Login},
		} {
			if sub.to == "" {
				continue
			}
			src = bytes.ReplaceAll(src, []byte(sub.from), []byte(sub.to))
		}
		src = bytes.ReplaceAll(src, []byte(specDomain), []byte(domain))
	}
	yaml, err := jsonToYAML(src)
	if err != nil {
		// The canonical document already parsed at init, and substitution only
		// replaces one hostname with another, so reaching this means the
		// emitter is broken rather than the input.
		panic("restapi: rendering openapi.yaml: " + err.Error())
	}
	return newBlob("application/json; charset=utf-8", src),
		newBlob("application/yaml; charset=utf-8", yaml)
}

func (h *Handler) openapiJSON(w http.ResponseWriter, r *http.Request) { h.specJSON.ServeHTTP(w, r) }
func (h *Handler) openapiYAML(w http.ResponseWriter, r *http.Request) { h.specYAML.ServeHTTP(w, r) }

//go:embed docs.html
var docsTemplate []byte

// docsPage is the reference UI composed against the shared design system,
// minified and pre-gzipped at package init — the same pipeline both consoles
// use, so the three pages stay siblings and a malformed template fails the
// build rather than one route.
var docsPage = webui.Build(docsTemplate)

// docs serves the reference. The CSP is the user console's, minus everything
// this page does not need: it fetches only /openapi.json, from its own origin,
// and loads no image, font or script from anywhere. 'unsafe-inline' is required
// because the page inlines its <style> and <script>, which is what makes it a
// single self-contained document with no CDN in the loop.
func (h *Handler) docs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
			"script-src 'self' 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'")
	docsPage.ServeHTTP(w, r)
}
