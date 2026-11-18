package schematest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/screenshots"
)

// schemaInexpressible lists the invalid fixtures whose rule the schema
// description names as one a schema cannot express. Every other invalid
// fixture fails the schema, and each one listed here passes it.
var schemaInexpressible = map[string]string{
	"duplicate-member.json":                   "no duplicate member",
	"trailing-data.json":                      "no trailing data",
	"text-too-long.json":                      "a text member of at most 2048 bytes",
	"route-listed-twice.json":                 "no route listed twice",
	"capture-file-listed-twice.json":          "no capture file listed twice",
	"capture-of-unknown-route.json":           "a capture route that is one of the pages",
	"capture-on-a-side-without-the-page.json": "a capture side that holds its page",
	"capture-taken-twice.json":                "exactly one capture per route, side, viewport and scheme",
	"capture-missing.json":                    "exactly one capture per route, side, viewport and scheme",
	"side-missing.json":                       "exactly one capture per route, side, viewport and scheme",
}

func TestEveryFixtureIsJudgedByTheSchema(t *testing.T) {
	spectest.Proves(t, "screenshots/manifest-contract", "the-corpus-is-the-contract", "every-fixture-is-judged-by-the-schema")
	for _, published := range [][]byte{screenshots.SchemaV1(), screenshots.Schema()} {
		var schema struct {
			ID          string `json:"$id"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(published, &schema); err != nil {
			t.Fatal(err)
		}
		for name, rule := range schemaInexpressible {
			if !strings.Contains(schema.Description, rule) {
				t.Errorf("%s: the description of %s does not name %q", name, schema.ID, rule)
			}
		}
	}
	for _, validity := range []string{"valid", "invalid"} {
		paths, err := filepath.Glob(filepath.Join("..", "fixtures", validity, "*.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("fixture corpus: %v, %v", paths, err)
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			name := filepath.Base(path)
			refusal := Check(t, data)
			_, inexpressible := schemaInexpressible[name]
			switch {
			case validity == "valid" && refusal != nil:
				t.Errorf("the valid fixture %s fails the schema: %v", name, refusal)
			case validity == "invalid" && refusal == nil && !inexpressible:
				t.Errorf("the invalid fixture %s passes the schema", name)
			case validity == "invalid" && refusal != nil && inexpressible:
				t.Errorf("the invalid fixture %s fails the schema (%v): drop it from schemaInexpressible", name, refusal)
			}
		}
	}
}

// fatal records the failure a test helper would stop the test with.
type fatal struct {
	testing.TB
	message string
}

func (f *fatal) Helper() {}

func (f *fatal) Fatalf(format string, args ...any) {
	f.message = format
	panic(f)
}

func TestTheEvaluatorRefusesASchemaItCannotJudge(t *testing.T) {
	cases := map[string]string{
		"an unknown keyword":           `{"minProperties": 1}`,
		"an open additionalProperties": `{"additionalProperties": true}`,
		"an unresolvable reference":    `{"$ref": "#/$defs/absent"}`,
		"a node that is not an object": `{"not": 3}`,
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			var root map[string]any
			if err := decode([]byte(text), &root); err != nil {
				t.Fatal(err)
			}
			recorder := &fatal{TB: t}
			defer func() {
				if recover() == nil || recorder.message == "" {
					t.Fatalf("the evaluator judged a document with %s", name)
				}
			}()
			_ = (&evaluator{tb: recorder, root: root}).check(root, map[string]any{"a": "b"}, "$")
		})
	}
}

func TestCheckNamesWhatTheSchemaRefuses(t *testing.T) {
	if err := Check(t, []byte(`{"protocolVersion": 1`)); err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("a truncated document: %v", err)
	}
	cases := map[string]string{
		`[]`:                     "$: is no object",
		`{"protocolVersion": 3}`: "$.protocolVersion: is not 2",
		`{"protocolVersion": 1.0, "project": "a", "baseSHA": 3}`: "$.baseSHA: is no string",
	}
	for document, want := range cases {
		err := Check(t, []byte(document))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", document, err, want)
		}
	}
}

// TestEachFixtureIsJudgedByTheSchemaOfItsVersion reads one page that proves a
// requirement under both versions: the version 2 schema accepts it, and the
// version 1 schema, which judges a version 1 document, refuses the member.
func TestEachFixtureIsJudgedByTheSchemaOfItsVersion(t *testing.T) {
	spectest.Proves(t, "screenshots/manifest-contract", "both-versions-decode", "each-fixture-is-judged-by-the-schema-of-its-version")
	data, err := os.ReadFile(filepath.Join("..", "fixtures", "valid", "v2-page-proves.json"))
	if err != nil {
		t.Fatal(err)
	}
	if refusal := Check(t, data); refusal != nil {
		t.Fatalf("the version 2 schema refuses a page that proves requirements: %v", refusal)
	}
	v1 := strings.Replace(string(data), `"protocolVersion": 2`, `"protocolVersion": 1`, 1)
	if refusal := Check(t, []byte(v1)); refusal == nil || !strings.Contains(refusal.Error(), "unknown member proves") {
		t.Fatalf("the version 1 schema judged a version 1 page with proves: %v", refusal)
	}
	for document, want := range map[string]string{
		`{"protocolVersion": 1}`:   screenshots.SchemaURLV1,
		`{"protocolVersion": 1.0}`: screenshots.SchemaURLV1,
		`{"protocolVersion": 2}`:   screenshots.SchemaURL,
		`{"protocolVersion": "1"}`: screenshots.SchemaURL,
		`[]`:                       screenshots.SchemaURL,
	} {
		var value any
		if err := decode([]byte(document), &value); err != nil {
			t.Fatal(err)
		}
		var schema struct {
			ID string `json:"$id"`
		}
		if err := json.Unmarshal(SchemaOf(value), &schema); err != nil || schema.ID != want {
			t.Errorf("%s is judged by %s, want %s", document, schema.ID, want)
		}
	}
}
