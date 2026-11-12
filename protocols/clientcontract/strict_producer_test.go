package clientcontract

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func TestValidateOperation_AProducerNamesProjectAndFeature(t *testing.T) {
	spectest.Proves(t, "tooling/cross-language-rest-clients", "contract-carries-attribution",
		"the-contract-carries-each-operations-producer-attribution")
	base := `{"stream":"unary","transports":[{"protocol":"rest-json","path":"/items","encoding":"json"}],` +
		`"security":{"alternatives":[{"allOf":[]}]},"errors":[],"idempotency":{"kind":"safe"}`
	operation, diags := ParseOperation([]byte(base + `,"producer":{"project":"/svc","feature":"items"}}`))
	if operation == nil || operation.Producer == nil || operation.Producer.Project != "/svc" || operation.Producer.Feature != "items" {
		t.Fatalf("producer did not decode: %+v %v", operation, diags)
	}
	for _, diag := range ValidateOperation(operation, &DocumentV1{}) {
		if diag.Field == "producer.project" || diag.Field == "producer.feature" {
			t.Errorf("a complete producer was refused: %v", diag)
		}
	}
	blank, _ := ParseOperation([]byte(base + `,"producer":{"project":"/svc","feature":" "}}`))
	found := false
	for _, diag := range ValidateOperation(blank, &DocumentV1{}) {
		found = found || diag.Field == "producer.feature"
	}
	if !found {
		t.Error("a blank producer feature was accepted")
	}
	if _, diags := ParseOperation([]byte(base + `,"producer":{"project":"/svc","feature":"items","x":1}}`)); len(diags) == 0 {
		t.Error("an unknown producer member was accepted")
	}
}
