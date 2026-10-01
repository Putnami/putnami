package api

import (
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// binaryDocument is one first-party document with one raw octet download. The
// tests below mutate exactly one fact of it at a time, so each failure names
// the rule it broke rather than a bundle of them.
const binaryDocument = `{
  "openapi":"3.0.3",
  "x-putnami-client":{"protocolVersion":1,"service":{"id":"blobs","audience":"urn:blobs"},"credentials":{}},
  "paths":{"/blobs":{"get":{
    "operationId":"getBlobs",
    "x-putnami-client":{
      "stream":"unary",
      "transports":[{"protocol":"rest-json","path":"/blobs","encoding":"json"}],
      "security":{"alternatives":[{"allOf":[]}]},
      "errors":[],
      "idempotency":{"kind":"safe"}
    },
    "responses":{"200":{"description":"ok","content":{"application/octet-stream":{
      "schema":{"type":"string","format":"binary"},
      "x-putnami-max-bytes":4096
    }}}}
  }}}
}`

// TestReadOpenAPISpec_ReadsTheDeclaredOctetBound proves the bound survives the
// reader: it is the only fact of a binary representation a JSON Schema keyword
// cannot carry, so losing it would leave the emitters to invent one.
func TestReadOpenAPISpec_ReadsTheDeclaredOctetBound(t *testing.T) {
	spec, err := ReadOpenAPISpec([]byte(binaryDocument))
	if err != nil {
		t.Fatalf("ReadOpenAPISpec: %v", err)
	}
	content := spec.Services[0].Methods[0].Successes[0].Content[0]
	if !content.IsBinary() {
		t.Fatalf("content %q was not read as raw octets", content.MediaType)
	}
	if content.MediaType != "application/octet-stream" || content.MaxBytes != 4096 {
		t.Fatalf("read %q/%d, want application/octet-stream/4096", content.MediaType, content.MaxBytes)
	}
}

// TestReadOpenAPISpec_RefusesOctetsWhereTheyAreNotRepresentable is the
// diagnostic half of the declaration: every position where `format: binary`
// would force a reader to invent an encoding is named and refused.
func TestReadOpenAPISpec_RefusesOctetsWhereTheyAreNotRepresentable(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "binary-payloads", "raw-octets-are-refused-inside-a-json-document")
	tests := []struct {
		name string
		old  string
		new  string
		want string
	}{
		{
			name: "inside a JSON document",
			old: `"application/octet-stream":{
      "schema":{"type":"string","format":"binary"},
      "x-putnami-max-bytes":4096
    }`,
			new:  `"application/json":{"schema":{"type":"object","properties":{"blob":{"type":"string","format":"binary"}},"required":["blob"],"additionalProperties":false}}`,
			want: `declare base64 bytes as format "byte"`,
		},
		{
			name: "raw octets under a JSON media type",
			old:  `"application/octet-stream":{`,
			new:  `"application/json":{`,
			want: "declares raw octets under a JSON media type",
		},
		{
			name: "no declared bound",
			old: `,
      "x-putnami-max-bytes":4096`,
			new:  ``,
			want: "without a positive x-putnami-max-bytes bound",
		},
		{
			name: "a bound on a JSON representation",
			old:  `"schema":{"type":"string","format":"binary"}`,
			new:  `"schema":{"type":"string"}`,
			want: "the bound describes raw octets only",
		},
		{
			name: "octets constrained with JSON vocabulary",
			old:  `"schema":{"type":"string","format":"binary"}`,
			new:  `"schema":{"type":"string","format":"binary","enum":["a"]}`,
			want: "constrains raw octets with JSON schema vocabulary",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			raw := strings.Replace(binaryDocument, testCase.old, testCase.new, 1)
			if raw == binaryDocument {
				t.Fatalf("fixture replacement %q did not match", testCase.old)
			}
			_, err := ReadOpenAPISpec([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("ReadOpenAPISpec error = %v, want %q", err, testCase.want)
			}
		})
	}
}

// TestReadOpenAPISpec_KeepsBase64BytesInsideJSON is the other half of the same
// rule: base64 bytes in a JSON document stay supported everywhere, because
// `format: byte` says how they are encoded and `format: binary` does not.
func TestReadOpenAPISpec_KeepsBase64BytesInsideJSON(t *testing.T) {
	raw := strings.Replace(binaryDocument,
		`"application/octet-stream":{
      "schema":{"type":"string","format":"binary"},
      "x-putnami-max-bytes":4096
    }`,
		`"application/json":{"schema":{"type":"object","properties":{"blob":{"type":"string","format":"byte"}},"required":["blob"],"additionalProperties":false}}`, 1)
	if raw == binaryDocument {
		t.Fatal("fixture replacement did not match")
	}
	if _, err := ReadOpenAPISpec([]byte(raw)); err != nil {
		t.Fatalf("base64 bytes inside a JSON document were refused: %v", err)
	}
}
