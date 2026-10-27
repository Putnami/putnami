package httproutes

import (
	"encoding/json"
	"os"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestVersionIdentifiersArePinned(t *testing.T) {
	spectest.Proves(t, "http-routes/route-inventory-contract", "the-identifiers-are-pinned", "the-version-identifiers-are-pinned")
	if Protocol != "putnami.http-routes.v1" {
		t.Fatalf("Protocol = %q; matching or wire changes require a new major identifier", Protocol)
	}
	if SchemaURL != "https://putnami.dev/schemas/putnami-http-routes-v1.json" {
		t.Fatalf("SchemaURL = %q; want pinned v1 URL", SchemaURL)
	}
	data, err := os.ReadFile("schemas/http-routes.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		ID         string `json:"$id"`
		Properties map[string]struct {
			Const string `json:"const"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.ID != SchemaURL {
		t.Fatalf("schema $id = %q, want %q", schema.ID, SchemaURL)
	}
	if schema.Properties["protocol"].Const != Protocol {
		t.Fatalf("schema protocol const = %q, want %q", schema.Properties["protocol"].Const, Protocol)
	}
}
