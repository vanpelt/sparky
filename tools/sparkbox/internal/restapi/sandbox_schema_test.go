package restapi

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/vanpelt/sparky/tools/sparkbox/internal/ctlops"
)

// TestSandboxSchemaMatchesTheStruct is the drift guard for the one schema this
// package serves verbatim: handlers marshal ctlops.SandboxInfo straight onto the
// wire, so a field added to the struct and not to the document is an undocumented
// field, and a property described here and not in the struct is a promise no
// response keeps. It is the schema-level twin of TestSpecDescribesExactlyTheRoutes.
func TestSandboxSchemaMatchesTheStruct(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas struct {
				Sandbox struct {
					Required   []string                   `json:"required"`
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"Sandbox"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(openapiSource, &doc); err != nil {
		t.Fatal(err)
	}
	schema := doc.Components.Schemas.Sandbox

	// omitempty is what keeps `node` and `unreachable` out of a single-box
	// payload, so a required list naming either would document a field that
	// simply is not there.
	optional := map[string]bool{}
	fields := map[string]bool{}
	rt := reflect.TypeFor[ctlops.SandboxInfo]()
	for i := range rt.NumField() {
		tag := rt.Field(i).Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		fields[name] = true
		optional[name] = strings.Contains(opts, "omitempty")
	}

	for name := range fields {
		if _, ok := schema.Properties[name]; !ok {
			t.Errorf("SandboxInfo.%s is undocumented in components.schemas.Sandbox", name)
		}
	}
	for name := range schema.Properties {
		if !fields[name] {
			t.Errorf("components.schemas.Sandbox describes %q, which no response carries", name)
		}
	}
	for _, name := range schema.Required {
		if optional[name] {
			t.Errorf("%q is listed as required but is omitempty on SandboxInfo", name)
		}
	}
}
