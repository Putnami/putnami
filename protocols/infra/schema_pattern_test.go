package infra

import (
	"encoding/json"
	"os"
	"testing"
)

// The JSON schemas and the Go validator each carry the name grammars; this
// test keeps the copies equal.
func TestSchemasMatchNamePatterns(t *testing.T) {
	for _, path := range []string{"schemas/infra.json", "schemas/infra-aggregated.json", "schemas/infra-overrides.json"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Defs map[string]struct {
				Pattern string `json:"pattern"`
			} `json:"$defs"`
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for def, want := range map[string]string{
			"resourceName": resourceNamePattern.String(),
			"secretName":   secretNamePattern.String(),
		} {
			if got := schema.Defs[def].Pattern; got != want {
				t.Errorf("%s $defs.%s.pattern = %q, want %q", path, def, got, want)
			}
		}
	}
}
