package i18n

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

const feature = "i18n/catalog-format"

func TestSchemasNameTheirPublishedID(t *testing.T) {
	spectest.Proves(t, feature, "the-schemas-are-published", "each-schema-names-its-published-id")
	for name, tc := range map[string]struct {
		schema []byte
		id     string
	}{
		"catalog":  {CatalogSchema(), CatalogSchemaID},
		"glossary": {GlossarySchema(), GlossarySchemaID},
	} {
		var document struct {
			Dialect string `json:"$schema"`
			ID      string `json:"$id"`
		}
		if err := json.Unmarshal(tc.schema, &document); err != nil {
			t.Fatalf("%s schema is not JSON: %v", name, err)
		}
		if document.ID != tc.id {
			t.Errorf("%s schema $id = %q, want %q", name, document.ID, tc.id)
		}
		if document.Dialect != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("%s schema dialect = %q, want draft 2020-12", name, document.Dialect)
		}
	}
}

func TestEveryFixtureIsAJSONObject(t *testing.T) {
	spectest.Proves(t, feature, "the-fixture-corpus-is-the-contract", "every-fixture-is-a-json-object")
	for _, format := range []string{"catalog", "glossary"} {
		for _, kind := range []string{"valid", "invalid"} {
			files, err := filepath.Glob(filepath.Join("fixtures", format, kind, "*.json"))
			if err != nil || len(files) == 0 {
				t.Fatalf("fixtures/%s/%s holds no fixture", format, kind)
			}
			for _, file := range files {
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				var value map[string]any
				if err := json.Unmarshal(raw, &value); err != nil {
					t.Errorf("%s is not a JSON object: %v", file, err)
				}
			}
		}
	}
}
