package consumer

import (
	"bytes"
	"encoding/json"
	"testing"

	itemsclient "go.putnami.dev/examples/service-to-service/clients/go"
	"go.putnami.dev/protocol/features/spectest"
)

// TestGeneratedOpaqueJSONAgainstRealProvider proves provider declaration →
// generated client → real provider for values the provider does not
// interpret. The provider holds Payload and Note as json.RawMessage, so their
// bytes survive exactly — unsorted keys, an integer past uint64, a decimal past
// float64. Value and Attributes go through the provider's own encoding/json,
// so they are sent in the form Go writes back: sorted keys, float64-exact
// numbers. The generated client never reinterprets any of them.
func TestGeneratedOpaqueJSONAgainstRealProvider(t *testing.T) {
	spectest.Proves(t, matrixFeature, "a-result-is-what-the-contract-declared", "opaque-json-crosses-the-wire-without-loss")
	generated := generatedClientAgainstRealProvider(t)
	sent := itemsclient.AuditRecord{
		Attributes: map[string]json.RawMessage{
			"actor": json.RawMessage(`{"id":"u-1","roles":["admin","owner"]}`),
			"gone":  json.RawMessage(`null`),
		},
		Payload: json.RawMessage(`{"z":1,"a":[18446744073709551616,0.1000000000000000055511151231257827,"x y"],"m":{}}`),
		Value:   json.RawMessage(`{"a":[1,2.5,"x",null,true],"b":{"c":false}}`),
		Note:    json.RawMessage(`null`),
	}
	echoed, err := generated.CreateAudit(t.Context(), itemsclient.CreateAuditInput{Body: sent})
	if err != nil {
		t.Fatalf("CreateAudit: %v", err)
	}
	for name, pair := range map[string][2]json.RawMessage{
		"payload":          {sent.Payload, echoed.Payload},
		"value":            {sent.Value, echoed.Value},
		"note":             {sent.Note, echoed.Note},
		"attributes.actor": {sent.Attributes["actor"], echoed.Attributes["actor"]},
		"attributes.gone":  {sent.Attributes["gone"], echoed.Attributes["gone"]},
	} {
		if !bytes.Equal(pair[0], pair[1]) {
			t.Errorf("%s: sent %s, got back %s", name, pair[0], pair[1])
		}
	}
	if len(echoed.Attributes) != len(sent.Attributes) {
		t.Errorf("attributes = %v, want %v", echoed.Attributes, sent.Attributes)
	}

	// An absent optional member stays absent: it does not come back as null.
	echoed, err = generated.CreateAudit(t.Context(), itemsclient.CreateAuditInput{Body: itemsclient.AuditRecord{
		Attributes: map[string]json.RawMessage{},
		Payload:    json.RawMessage(`[]`),
		Value:      json.RawMessage(`"text"`),
	}})
	if err != nil {
		t.Fatalf("CreateAudit without a note: %v", err)
	}
	if echoed.Note != nil || string(echoed.Payload) != `[]` || string(echoed.Value) != `"text"` {
		t.Fatalf("echoed = %+v, want no note, an empty array and a string", echoed)
	}
}
