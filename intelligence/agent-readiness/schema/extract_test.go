package schema

import (
	"encoding/json"
	"strings"
	"testing"
)

const sampleOpenAPI = `{
  "openapi": "3.0.3",
  "components": {"schemas": {
    "Root": {"type": "object", "additionalProperties": false, "required": ["child"], "properties": {
      "child": {"$ref": "#/components/schemas/Child"},
      "maybe": {"$ref": "#/components/schemas/Child", "nullable": true},
      "note": {"type": "string", "nullable": true},
      "mode": {"type": "string", "enum": ["a", "b"], "nullable": true},
      "list": {"type": "array", "items": {"$ref": "#/components/schemas/Leaf"}}
    }},
    "Child": {"type": "object", "properties": {"leaf": {"$ref": "#/components/schemas/Leaf"}}},
    "Leaf": {"type": "string"},
    "Unrelated": {"type": "integer"},
    "Broken": {"type": "object", "properties": {"x": {"$ref": "https://example.com/x.json"}}},
    "Dangling": {"$ref": "#/components/schemas/Missing"}
  }}
}`

func TestExtractBuildsAStandaloneDocumentFromTheReferenceClosure(t *testing.T) {
	out, err := Extract([]byte(sampleOpenAPI), "Root", "root.v1.json", "Root v1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(out), "}\n") {
		t.Fatal("document does not end with a newline")
	}
	var document struct {
		ID    string                    `json:"$id"`
		Ref   string                    `json:"$ref"`
		Title string                    `json:"title"`
		Defs  map[string]map[string]any `json:"$defs"`
	}
	if err := json.Unmarshal(out, &document); err != nil {
		t.Fatal(err)
	}
	if document.ID != IDBase+"root.v1.json" || document.Ref != "#/$defs/Root" || document.Title != "Root v1" {
		t.Fatalf("header = %q %q %q", document.ID, document.Ref, document.Title)
	}
	if len(document.Defs) != 3 || document.Defs["Unrelated"] != nil {
		t.Fatalf("defs = %v, want Root, Child and Leaf only", document.Defs)
	}
	properties := document.Defs["Root"]["properties"].(map[string]any)
	if got := properties["child"].(map[string]any)["$ref"]; got != "#/$defs/Child" {
		t.Fatalf("child ref = %v", got)
	}
	maybe, _ := json.Marshal(properties["maybe"])
	if string(maybe) != `{"anyOf":[{"$ref":"#/$defs/Child"},{"type":"null"}]}` {
		t.Fatalf("nullable reference = %s", maybe)
	}
	note, _ := json.Marshal(properties["note"])
	if string(note) != `{"type":["string","null"]}` {
		t.Fatalf("nullable string = %s", note)
	}
	mode, _ := json.Marshal(properties["mode"])
	if string(mode) != `{"enum":["a","b",null],"type":["string","null"]}` {
		t.Fatalf("nullable enum = %s", mode)
	}

	again, err := Extract([]byte(sampleOpenAPI), "Root", "root.v1.json", "Root v1")
	if err != nil || string(again) != string(out) {
		t.Fatal("extraction is not deterministic")
	}
}

func TestExtractRefusesWhatItCannotResolve(t *testing.T) {
	for _, tc := range []struct {
		name, openapi, root, want string
	}{
		{"invalid document", "{", "Root", "decode OpenAPI document"},
		{"unknown root", sampleOpenAPI, "Nope", `component "Nope" not found`},
		{"external reference", sampleOpenAPI, "Broken", "unsupported reference"},
		{"dangling reference", sampleOpenAPI, "Dangling", `component "Missing" referenced from "Dangling" not found`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Extract([]byte(tc.openapi), tc.root, "x.json", "x")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
