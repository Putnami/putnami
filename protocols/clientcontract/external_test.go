package clientcontract

import (
	"encoding/json"
	"os"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const externalContractRequirement = "external-contract-operations"

// TestExternalContractAuthorityClassifiesOneOperation pins the three answers a
// reader gets for one operation of a first-party document: owned by an external
// authority, first-party, or a declaration that contradicts itself.
func TestExternalContractAuthorityClassifiesOneOperation(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", externalContractRequirement,
		"an-external-contract-that-contradicts-itself-is-refused")

	client := json.RawMessage(`{"stream":"unary"}`)
	accepted := []struct {
		name     string
		client   json.RawMessage
		external json.RawMessage
		want     string
	}{
		{"external operation", nil, json.RawMessage(`"OCI Distribution Specification v1.1"`), "OCI Distribution Specification v1.1"},
		{"external operation with surrounding JSON whitespace", nil, json.RawMessage(" \"npm registry API\"\n"), "npm registry API"},
		{"first-party operation", client, nil, ""},
		{"operation with neither extension", nil, nil, ""},
	}
	for _, tc := range accepted {
		t.Run("accepts/"+tc.name, func(t *testing.T) {
			authority, diags := ExternalContractAuthority(tc.client, tc.external)
			if len(diags) != 0 {
				t.Fatalf("diagnostics = %v, want none", diags)
			}
			if authority != tc.want {
				t.Fatalf("authority = %q, want %q", authority, tc.want)
			}
		})
	}

	refused := []struct {
		name     string
		client   json.RawMessage
		external json.RawMessage
		code     string
	}{
		{"both extensions", client, json.RawMessage(`"OCI Distribution Specification v1.1"`), ErrorCodeDuplicate},
		{"null client beside the marker", json.RawMessage(`null`), json.RawMessage(`"OCI Distribution Specification v1.1"`), ErrorCodeDuplicate},
		{"empty authority", nil, json.RawMessage(`""`), ErrorCodeRequired},
		{"blank authority", nil, json.RawMessage(`"  \t "`), ErrorCodeRequired},
		{"null authority", nil, json.RawMessage(`null`), ErrorCodeParseError},
		{"object authority", nil, json.RawMessage(`{"authority":"OCI"}`), ErrorCodeParseError},
		{"boolean authority", nil, json.RawMessage(`true`), ErrorCodeParseError},
		{"malformed string", nil, json.RawMessage(`"OCI`), ErrorCodeParseError},
	}
	for _, tc := range refused {
		t.Run("refuses/"+tc.name, func(t *testing.T) {
			authority, diags := ExternalContractAuthority(tc.client, tc.external)
			if authority != "" {
				t.Fatalf("authority = %q for a refused declaration", authority)
			}
			if !diag.HasErrors(diags) || diags[0].Code != tc.code {
				t.Fatalf("diagnostics = %v, want %s", diags, tc.code)
			}
			if diags[0].Field != ExternalContractKey {
				t.Fatalf("diagnostic field = %q, want %q", diags[0].Field, ExternalContractKey)
			}
		})
	}
}

// TestExternalOperationFixtureIsSkippedWhole proves the valid corpus fixture is
// not accepted vacuously: its external operation carries a schema the
// first-party subset refuses, so the fixture passes only because the reader
// skips the operation whole, and its first-party operation is still read.
func TestExternalOperationFixtureIsSkippedWhole(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", externalContractRequirement,
		"an-operation-an-external-authority-owns-is-skipped-whole-by-the-go-reader")

	path := "fixtures/openapi/valid/external-operation.openapi.json"
	if diags := validateOpenAPIFixture(t, path); diag.HasErrors(diags) {
		t.Fatalf("valid external-operation fixture produced diagnostics: %v", diags)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture openAPIFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	external, firstParty := 0, 0
	for _, item := range fixture.Paths {
		for _, operation := range item {
			authority, diags := ExternalContractAuthority(operation.Client, operation.External)
			if len(diags) != 0 {
				t.Fatalf("fixture operation %s: %v", operation.OperationID, diags)
			}
			if authority == "" {
				firstParty++
				continue
			}
			external++
			// Non-vacuity: the external operation's own schema is one the
			// first-party subset refuses, so reading it would fail the fixture.
			for _, response := range operation.Responses {
				for _, media := range response.Content {
					if _, schemaDiags := ParseAndValidateSchema(media.Schema); !diag.HasErrors(schemaDiags) {
						t.Fatalf("external operation schema %s is valid first-party; the fixture would not prove the skip", media.Schema)
					}
				}
			}
		}
	}
	if external != 1 || firstParty != 1 {
		t.Fatalf("fixture has %d external and %d first-party operations, want 1 and 1", external, firstParty)
	}
}

// TestSchemaPublishesTheExternalContractMarker pins the published definition of
// the marker's value to what the Go reader accepts: a string that names an
// authority.
func TestSchemaPublishesTheExternalContractMarker(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", externalContractRequirement,
		"the-published-schema-carries-the-external-contract-marker")

	defs := loadSchema(t)["$defs"].(map[string]any)
	definition, ok := defs["externalContract"].(map[string]any)
	if !ok {
		t.Fatal("x-putnami-client-v1.json publishes no externalContract definition")
	}
	if definition["type"] != "string" || definition["minLength"] != float64(1) || definition["pattern"] != `\S` {
		t.Fatalf("externalContract = %v, want a non-blank string", definition)
	}
	if ExternalContractKey != "x-putnami-external-contract" {
		t.Fatalf("ExternalContractKey = %q", ExternalContractKey)
	}
}
