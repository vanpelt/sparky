package restapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// A deterministic JSON-to-YAML emitter, ~150 lines, so that /openapi.yaml is a
// faithful rendering of the same bytes /openapi.json serves rather than a
// second document to keep in sync — and so that no YAML library joins the
// dependency set for one endpoint.
//
// Determinism means KEY ORDER: encoding/json's map decoding sorts keys, which
// would scramble an OpenAPI document into alphabetical soup (`components`
// before `openapi`, `delete` before `get`). So the JSON is parsed through a
// token stream into an ordered tree first, and emitted in the order it was
// authored.

// ynode is an ordered JSON value.
type ynode struct {
	kind  ykind
	keys  []string // kind == yobject
	vals  []*ynode // kind == yobject, parallel to keys
	items []*ynode // kind == yarray
	lit   string   // kind == ystring (raw), ynumber, ybool, ynull (rendered)
}

type ykind int

const (
	yobject ykind = iota
	yarray
	ystring
	ynumber
	ybool
	ynull
)

// jsonToYAML converts a JSON document, preserving key order.
func jsonToYAML(src []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	// UseNumber keeps 1.0 from becoming 1 and large integers from becoming
	// floats — a document that round-trips is the whole point.
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("read root token: %w", err)
	}
	root, err := parseNode(dec, tok)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString("# Generated from openapi.json — do not edit; edit the JSON.\n")
	if err := emit(&buf, root, 0); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func parseNode(dec *json.Decoder, tok json.Token) (*ynode, error) {
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &ynode{kind: yobject}
			for {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				if d, ok := kt.(json.Delim); ok && d == '}' {
					return n, nil
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("object key is %T, not a string", kt)
				}
				vt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				val, err := parseNode(dec, vt)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, key)
				n.vals = append(n.vals, val)
			}
		case '[':
			n := &ynode{kind: yarray}
			for {
				it, err := dec.Token()
				if err != nil {
					return nil, err
				}
				if d, ok := it.(json.Delim); ok && d == ']' {
					return n, nil
				}
				item, err := parseNode(dec, it)
				if err != nil {
					return nil, err
				}
				n.items = append(n.items, item)
			}
		default:
			return nil, fmt.Errorf("unexpected delimiter %q", t)
		}
	case string:
		return &ynode{kind: ystring, lit: t}, nil
	case json.Number:
		return &ynode{kind: ynumber, lit: t.String()}, nil
	case bool:
		return &ynode{kind: ybool, lit: fmt.Sprint(t)}, nil
	case nil:
		return &ynode{kind: ynull, lit: "null"}, nil
	}
	return nil, fmt.Errorf("unsupported token %T", tok)
}

func emit(buf *bytes.Buffer, n *ynode, indent int) error {
	pad := strings.Repeat(" ", indent)
	switch n.kind {
	case yobject:
		for i, k := range n.keys {
			buf.WriteString(pad)
			buf.WriteString(yamlKey(k))
			buf.WriteString(":")
			if err := emitChild(buf, n.vals[i], indent); err != nil {
				return err
			}
		}
	case yarray:
		for _, item := range n.items {
			buf.WriteString(pad)
			buf.WriteString("-")
			if err := emitChild(buf, item, indent); err != nil {
				return err
			}
		}
	default:
		buf.WriteString(pad)
		buf.WriteString(scalar(n))
		buf.WriteString("\n")
	}
	return nil
}

// emitChild writes the value that follows a "key:" or a "-" on the same line —
// inline when it is a scalar or empty, on the next lines when it has contents.
func emitChild(buf *bytes.Buffer, v *ynode, indent int) error {
	switch {
	case v.kind == yobject && len(v.keys) == 0:
		buf.WriteString(" {}\n")
	case v.kind == yarray && len(v.items) == 0:
		buf.WriteString(" []\n")
	case v.kind == yobject || v.kind == yarray:
		buf.WriteString("\n")
		return emit(buf, v, indent+2)
	default:
		buf.WriteString(" ")
		buf.WriteString(scalar(v))
		buf.WriteString("\n")
	}
	return nil
}

// scalar renders a leaf. Strings are ALWAYS double-quoted: an OpenAPI document
// is full of values YAML would otherwise reinterpret — "true", "no", "3.0.0",
// "on", ":port" — and quoting unconditionally is both correct and free of the
// judgement calls that make emitters subtly wrong.
func scalar(n *ynode) string {
	if n.kind == ystring {
		return quote(n.lit)
	}
	return n.lit
}

// yamlKey quotes a mapping key only when it needs it. Most keys here are plain
// identifiers, and quoting `paths` would make the document harder to read for
// no gain; but the paths themselves ("/v1/sandboxes/{name}") start with a
// character YAML would otherwise have to interpret.
func yamlKey(k string) string {
	if k == "" {
		return `""`
	}
	// A bare key that YAML would read as a boolean, a null, or a number.
	switch strings.ToLower(k) {
	case "true", "false", "null", "yes", "no", "on", "off", "y", "n", "~":
		return quote(k)
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		plain := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '-'
		if !plain {
			return quote(k)
		}
	}
	if k[0] >= '0' && k[0] <= '9' {
		return quote(k)
	}
	return k
}

// quote renders a double-quoted YAML scalar. YAML's double-quoted style uses
// the same escapes JSON does for everything this document contains, so the JSON
// encoder is the escaper — with HTML escaping off, since &, < and > are literal
// in descriptions and & would be noise.
func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s) //nolint:errcheck // encoding a string cannot fail
	return strings.TrimRight(b.String(), "\n")
}
