package client

import (
	"encoding/json"
	stderrors "errors"
	"net/http"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

func opaqueAuditSchema() *clientcontract.Schema {
	closed, open := false, true
	return &clientcontract.Schema{
		Type: "object",
		Properties: map[string]clientcontract.Schema{
			"value":      {OpaqueJSON: clientcontract.OpaqueJSONAny},
			"attributes": {Type: "object", AdditionalProperties: &clientcontract.AdditionalProperties{Allowed: &open}},
			"trail":      {Type: "array", Items: &clientcontract.Schema{OpaqueJSON: clientcontract.OpaqueJSONAny}},
		},
		Required:             []string{"value", "attributes"},
		AdditionalProperties: &clientcontract.AdditionalProperties{Allowed: &closed},
	}
}

// An opaque declaration admits every JSON value, null included, and a
// free-form object admits every member value. The runtime validates the rest of
// the shape around them and never narrows them.
func TestProjectJSONAdmitsEveryValueOfAnOpaqueDeclaration(t *testing.T) {
	schema := opaqueAuditSchema()
	for _, body := range []string{
		`{"value":null,"attributes":{}}`,
		`{"value":18446744073709551616,"attributes":{"a":null,"b":[1,"x",{"c":true}]}}`,
		`{"value":"text","attributes":{"n":0.1000000000000000055511151231257827},"trail":[null,{},[],1e400]}`,
		`{"value":{"z":1,"a":2},"attributes":{"z":{"nested":[{}]}}}`,
	} {
		if _, ok := projectJSON([]byte(body), schema, nil, nil, false); !ok {
			t.Errorf("%s was refused by an opaque declaration", body)
		}
	}
	for _, body := range []string{
		`{"attributes":{}}`,                      // the required opaque member is absent
		`{"value":1,"attributes":[]}`,            // a free-form object is still an object
		`{"value":1,"attributes":null}`,          // and is not nullable unless declared so
		`{"value":1,"attributes":{},"x":1}`,      // the closed envelope stays closed
		`{"value":1,"attributes":{},"trail":{}}`, // an array of opaque values is an array
	} {
		if _, ok := projectJSON([]byte(body), schema, nil, nil, false); ok {
			t.Errorf("%s was accepted", body)
		}
	}
}

// Redaction reads an opaque value the way it reads an undeclared member: a
// credential inside a string is scrubbed, a credential-shaped key is dropped,
// and a scalar equal to a credential invalidates the value.
func TestProjectJSONRedactsCredentialsInsideAnOpaqueValue(t *testing.T) {
	schema := opaqueAuditSchema()
	projected, ok := projectJSON([]byte(`{"value":{"note":"token s3cr3t here","password":"x","kept":1},"attributes":{"k":"s3cr3t"}}`),
		schema, nil, []string{"s3cr3t"}, true)
	if !ok {
		t.Fatal("redaction refused a payload it can scrub")
	}
	var got map[string]any
	if err := json.Unmarshal(projected, &got); err != nil {
		t.Fatal(err)
	}
	value := got["value"].(map[string]any)
	if value["note"] != "[REDACTED]" || value["kept"] != float64(1) {
		t.Errorf("opaque value = %v, want the credential scrubbed and the rest kept", value)
	}
	if _, present := value["password"]; present {
		t.Errorf("a credential-shaped key survived redaction: %v", value)
	}
	if attributes := got["attributes"].(map[string]any); attributes["k"] != "[REDACTED]" {
		t.Errorf("free-form member = %v, want the credential scrubbed", attributes)
	}
	if _, ok := projectJSON([]byte(`{"value":42,"attributes":{}}`), schema, nil, []string{"42"}, true); ok {
		t.Error("an opaque scalar equal to a credential was published")
	}
}

// itemResponseSchema is a response a client was generated from: every object is
// closed, one is reached through a $ref and one through array items.
func itemResponseSchema() (*clientcontract.Schema, map[string]clientcontract.Schema) {
	schemas := map[string]clientcontract.Schema{
		"Owner": {
			Type:                 "object",
			Properties:           map[string]clientcontract.Schema{"name": {Type: "string", MinLength: intPointer(1)}},
			Required:             []string{"name"},
			AdditionalProperties: additionalForbidden(),
		},
	}
	return &clientcontract.Schema{
		Type: "object",
		Properties: map[string]clientcontract.Schema{
			"id":    {Type: "string"},
			"count": {Type: "integer", Format: "int32"},
			"owner": {Ref: "#/components/schemas/Owner"},
			"tags": {Type: "array", Items: &clientcontract.Schema{
				Type:                 "object",
				Properties:           map[string]clientcontract.Schema{"label": {Type: "string"}},
				Required:             []string{"label"},
				AdditionalProperties: additionalForbidden(),
			}},
		},
		Required:             []string{"id", "owner"},
		AdditionalProperties: additionalForbidden(),
	}, schemas
}

// A provider that adds a property to a response it already serves still
// answers a client generated before the addition: the projection drops the
// property at every depth and keeps every declared one.
func TestResponseProjectionDropsPropertiesTheClientDoesNotDeclare(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "additive-responses", "a-response-property-the-client-does-not-declare-is-dropped")
	schema, schemas := itemResponseSchema()
	body := `{"id":"item-1","count":3,"addedTop":{"nested":true},` +
		`"owner":{"name":"ada","addedNested":"x"},` +
		`"tags":[{"label":"a","addedInItem":[1,2]},{"label":"b"}]}`
	projected, ok := projectResponseJSON([]byte(body), schema, schemas, nil, false)
	if !ok {
		t.Fatalf("a response carrying added properties was refused: %s", body)
	}
	for _, added := range []string{"addedTop", "addedNested", "addedInItem"} {
		if strings.Contains(string(projected), added) {
			t.Errorf("projection %s still carries %s", projected, added)
		}
	}
	var got map[string]any
	if err := json.Unmarshal(projected, &got); err != nil {
		t.Fatal(err)
	}
	if got["id"] != "item-1" || got["count"] != float64(3) || got["owner"].(map[string]any)["name"] != "ada" {
		t.Errorf("declared properties = %v", got)
	}
	if tags := got["tags"].([]any); len(tags) != 2 || tags[1].(map[string]any)["label"] != "b" {
		t.Errorf("tags = %v", got["tags"])
	}

	// The same document is still refused where the client is its author.
	if _, ok := projectJSON([]byte(body), schema, schemas, nil, false); ok {
		t.Error("a request carrying undeclared properties was accepted")
	}
}

// Dropping what the client does not declare never relaxes what it does: a
// declared property of the wrong type, a missing required one and a declared
// bound still refuse the response, added properties or not.
func TestResponseProjectionStillValidatesDeclaredProperties(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "additive-responses", "a-declared-response-property-is-still-validated")
	schema, schemas := itemResponseSchema()
	for _, body := range []string{
		`{"id":42,"owner":{"name":"ada"},"added":1}`,                // declared type
		`{"owner":{"name":"ada"},"added":1}`,                        // required id
		`{"id":"i","owner":{"added":1}}`,                            // required nested name
		`{"id":"i","owner":{"name":"","added":1}}`,                  // declared bound
		`{"id":"i","owner":{"name":"ada"},"count":1.5}`,             // declared integer
		`{"id":"i","owner":{"name":"ada"},"tags":[{"label":true}]}`, // declared item type
		`{"id":"i","owner":{"name":"ada"},"tags":{"label":"a"}}`,    // declared array
		`{"id":"i","owner":{"name":"ada"},"added":1} {}`,            // trailing data
		`["id","owner"]`, // not an object
	} {
		if _, ok := projectResponseJSON([]byte(body), schema, schemas, nil, false); ok {
			t.Errorf("%s was accepted", body)
		}
	}
}

// A union without a discriminator selects among its variants exactly as before
// when a value matches one of them as sent. A value that matches none as sent
// is projected with undeclared properties dropped and must then match exactly
// one variant.
func TestResponseProjectionSelectsAUnionVariantAfterDroppingAddedProperties(t *testing.T) {
	closedObject := func(required bool, names ...string) clientcontract.Schema {
		schema := clientcontract.Schema{Type: "object", Properties: map[string]clientcontract.Schema{}, AdditionalProperties: additionalForbidden()}
		for _, name := range names {
			schema.Properties[name] = clientcontract.Schema{Type: "string"}
			if required {
				schema.Required = append(schema.Required, name)
			}
		}
		return schema
	}
	union := &clientcontract.Schema{OneOf: []clientcontract.Schema{closedObject(true, "a"), closedObject(true, "b")}}
	projected, ok := projectResponseJSON([]byte(`{"a":"x","added":1}`), union, nil, nil, false)
	if !ok || string(projected) != `{"a":"x"}` {
		t.Fatalf("union projection = %s %v, want the a variant without the added property", projected, ok)
	}
	// Once dropped properties let two variants match, the value is ambiguous.
	ambiguous := &clientcontract.Schema{OneOf: []clientcontract.Schema{closedObject(true, "a"), closedObject(false, "b")}}
	if projected, ok := projectResponseJSON([]byte(`{"a":"x"}`), ambiguous, nil, nil, false); !ok || string(projected) != `{"a":"x"}` {
		t.Fatalf("a value one variant matches as sent = %s %v", projected, ok)
	}
	if _, ok := projectResponseJSON([]byte(`{"a":"x","added":1}`), ambiguous, nil, nil, false); ok {
		t.Error("a value two variants match after dropping was accepted")
	}
}

// Declared error details follow the response rule, and a dropped property
// never reaches RemoteError.Payload, whatever it carries.
func TestErrorDetailsDropPropertiesTheClientDoesNotDeclare(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "additive-responses", "a-response-property-the-client-does-not-declare-is-dropped")
	details := &clientcontract.Schema{
		Type:                 "object",
		Properties:           map[string]clientcontract.Schema{"field": {Type: "string"}, "note": {Type: "string"}},
		Required:             []string{"field"},
		AdditionalProperties: additionalForbidden(),
	}
	err := decodeRemoteError(
		callIdentity{serviceID: "inventory", operationID: "putItem"},
		&Response{StatusCode: http.StatusBadRequest, Body: errorEnvelope("errors.invalid",
			`{"field":"name","note":"sent s3cr3t","addedHint":"plain","addedEcho":"s3cr3t","addedObject":{"k":"v"}}`)},
		[]clientcontract.DeclaredError{{Status: http.StatusBadRequest, Code: "errors.invalid", Schema: details}},
		nil, []string{"s3cr3t"}, false,
	)
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		t.Fatalf("declared error = %T %v", err, err)
	}
	if string(remote.Payload) != `{"field":"name","note":"[REDACTED]"}` {
		t.Fatalf("payload = %s, want the declared properties only, redacted", remote.Payload)
	}
}
