package screenshots

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

type schemaNode struct {
	ID          string                `json:"$id"`
	Description string                `json:"description"`
	Const       any                   `json:"const"`
	Enum        []string              `json:"enum"`
	Required    []string              `json:"required"`
	MinItems    int                   `json:"minItems"`
	MaxItems    int                   `json:"maxItems"`
	MaxLength   int                   `json:"maxLength"`
	Properties  map[string]schemaNode `json:"properties"`
	Defs        map[string]schemaNode `json:"$defs"`
}

func loadSchema(t *testing.T) schemaNode {
	t.Helper()
	return parseSchema(t, Schema())
}

func parseSchema(t *testing.T, data []byte) schemaNode {
	t.Helper()
	var schema schemaNode
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestVersionIdentifiersArePinned(t *testing.T) {
	spectest.Proves(t, feature, "the-identifiers-are-pinned", "the-version-identifiers-are-pinned")
	if ProtocolVersion != 2 || ProtocolVersionV1 != 1 {
		t.Fatalf("ProtocolVersion = %d, ProtocolVersionV1 = %d; a wire change needs a new version, a new schema URL and a new corpus",
			ProtocolVersion, ProtocolVersionV1)
	}
	if SchemaURL != "https://putnami.dev/schemas/putnami-screenshots-v2.json" {
		t.Fatalf("SchemaURL = %q, want the pinned v2 URL", SchemaURL)
	}
	if SchemaURLV1 != "https://putnami.dev/schemas/putnami-screenshots-v1.json" {
		t.Fatalf("SchemaURLV1 = %q, want the pinned v1 URL", SchemaURLV1)
	}
	if ManifestFile != "screenshots.json" {
		t.Fatalf("ManifestFile = %q", ManifestFile)
	}
	for _, published := range []struct {
		schema  func() []byte
		url     string
		version int
	}{{Schema, SchemaURL, ProtocolVersion}, {SchemaV1, SchemaURLV1, ProtocolVersionV1}} {
		schema := parseSchema(t, published.schema())
		if schema.ID != published.url {
			t.Errorf("schema $id = %q, want %q", schema.ID, published.url)
		}
		if version, _ := schema.Properties["protocolVersion"].Const.(float64); int(version) != published.version {
			t.Errorf("schema %s protocolVersion const = %v, want %d", published.url, schema.Properties["protocolVersion"].Const, published.version)
		}
		if first, second := published.schema(), published.schema(); &first[0] == &second[0] {
			t.Errorf("the schema %s is returned as the embedded bytes, which a caller could change", published.url)
		}
	}
}

func TestSchemaMembersMatchTheGoTypes(t *testing.T) {
	spectest.Proves(t, feature, "the-identifiers-are-pinned", "the-schema-members-match-the-go-types")
	schema := loadSchema(t)
	v1 := parseSchema(t, SchemaV1())
	// The optional members of each object: a page proves nothing when it
	// omits proves.
	optional := map[string][]string{"Page": {"proves"}}
	for name, pair := range map[string]struct {
		node  schemaNode
		value any
	}{
		"Manifest": {schema, Manifest{}},
		"Page":     {schema.Defs["page"], Page{}},
		"Proof":    {schema.Defs["proof"], Proof{}},
		"Capture":  {schema.Defs["capture"], Capture{}},
	} {
		var want []string
		for member := range pair.node.Properties {
			want = append(want, member)
		}
		sort.Strings(want)
		typeOf := reflect.TypeOf(pair.value)
		var got []string
		for i := 0; i < typeOf.NumField(); i++ {
			got = append(got, strings.Split(typeOf.Field(i).Tag.Get("json"), ",")[0])
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s members differ: Go=%v schema=%v", name, got, want)
		}
		// Every other member is required: the decoder refuses a manifest
		// without one.
		var mandatory []string
		for _, member := range want {
			if !slices.Contains(optional[name], member) {
				mandatory = append(mandatory, member)
			}
		}
		required := append([]string(nil), pair.node.Required...)
		sort.Strings(required)
		if !reflect.DeepEqual(required, mandatory) {
			t.Errorf("%s required members = %v, want every member but %v: %v", name, required, optional[name], mandatory)
		}
	}
	// Version 1 holds the same members, but a page that proves nothing.
	for name, pair := range map[string][2]schemaNode{
		"Manifest": {schema, v1},
		"Page":     {schema.Defs["page"], v1.Defs["page"]},
		"Capture":  {schema.Defs["capture"], v1.Defs["capture"]},
	} {
		for member := range pair[0].Properties {
			if _, held := pair[1].Properties[member]; !held && !(name == "Page" && member == "proves") {
				t.Errorf("the version 1 %s lacks the member %s", name, member)
			}
		}
		if len(pair[1].Properties) != len(pair[0].Properties)-len(optional[name]) {
			t.Errorf("the version 1 %s holds %d members, want those of version 2 but %v", name, len(pair[1].Properties), optional[name])
		}
	}
}

func TestSchemaClosedSetsMatchTheGoSets(t *testing.T) {
	spectest.Proves(t, feature, "the-identifiers-are-pinned", "the-schema-closed-sets-match-the-go-sets")
	schema := loadSchema(t)
	capture := schema.Defs["capture"].Properties
	var viewports []string
	for _, viewport := range Viewports() {
		viewports = append(viewports, viewport.Name)
	}
	for member, want := range map[string][]string{"side": Sides(), "viewport": viewports, "scheme": Schemes()} {
		if got := capture[member].Enum; !reflect.DeepEqual(got, want) {
			t.Errorf("schema %s set = %v, want %v", member, got, want)
		}
	}
	want := []Viewport{{"desktop", 1440, 900, 1}, {"mobile", 390, 844, 2}}
	if !reflect.DeepEqual(Viewports(), want) {
		t.Errorf("Viewports() = %v, want %v", Viewports(), want)
	}
	if !strings.Contains(capture["viewport"].Description, "1440x900 CSS pixels at scale 1") ||
		!strings.Contains(capture["viewport"].Description, "390x844 at scale 2") {
		t.Errorf("the schema does not describe the viewports: %q", capture["viewport"].Description)
	}
	if !reflect.DeepEqual(Sides(), []string{"before", "after"}) || !reflect.DeepEqual(Schemes(), []string{"light", "dark"}) {
		t.Errorf("Sides() = %v, Schemes() = %v", Sides(), Schemes())
	}
	// The bounds the schema states are the bounds of the Go contract.
	bounds := map[string][2]int{
		"pages":         {schema.Properties["pages"].MaxItems, MaxPages},
		"captures":      {schema.Properties["captures"].MaxItems, MaxCaptures},
		"relativePath":  {schema.Defs["relativePath"].MaxLength, MaxTextBytes},
		"page.route":    {schema.Defs["page"].Properties["route"].MaxLength, MaxTextBytes},
		"page.path":     {schema.Defs["page"].Properties["path"].MaxLength, MaxTextBytes},
		"capture.route": {capture["route"].MaxLength, MaxTextBytes},
		"capture.url":   {capture["url"].MaxLength, MaxTextBytes},
		"capture.file":  {capture["file"].MaxLength, MaxFileNameBytes},
		"page.proves":   {schema.Defs["page"].Properties["proves"].MaxItems, MaxProofs},
		"proves.min":    {schema.Defs["page"].Properties["proves"].MinItems, 1},
		"proof.feature": {schema.Defs["proof"].Properties["feature"].MaxLength, MaxTextBytes},
		"proof.req":     {schema.Defs["proof"].Properties["requirement"].MaxLength, MaxTextBytes},
	}
	for name, bound := range bounds {
		if bound[0] != bound[1] {
			t.Errorf("schema bound of %s = %d, want %d", name, bound[0], bound[1])
		}
	}
	for _, text := range []string{"at most 4194304 bytes", "at most 2048 bytes", "at most 67108864 bytes"} {
		if !strings.Contains(schema.Description, text) {
			t.Errorf("the schema description does not state %q", text)
		}
	}
}
