package clientcontract

import "testing"

const referenceDigest = "1111111111111111111111111111111111111111111111111111111111111111"

func referenceManifest(body string) []byte { return []byte(body) }

// TestDecodeGeneratedClientReference_ReadsBothEndsOfTheEdge pins what a
// workspace graph reads out of a committed target: the provider service and the
// contract bytes, with the inventory left unparsed.
func TestDecodeGeneratedClientReference_ReadsBothEndsOfTheEdge(t *testing.T) {
	reference, ok := DecodeGeneratedClientReference(referenceManifest(`{
		"protocolVersion": 1,
		"generatedBy": "@putnami/clientgen",
		"language": "ts",
		"service": {"id": "catalog.items", "audience": "api://catalog.items"},
		"contractSha256": "` + referenceDigest + `",
		"operations": [{"operationId": "getItems"}],
		"files": [{"path": "src/index.ts", "sha256": "` + referenceDigest + `"}]
	}`))
	if !ok {
		t.Fatal("a complete first-party manifest was not read as a reference")
	}
	if reference.ServiceID != "catalog.items" {
		t.Errorf("ServiceID = %q, want catalog.items", reference.ServiceID)
	}
	if reference.ContractSHA256 != referenceDigest {
		t.Errorf("ContractSHA256 = %q, want %s", reference.ContractSHA256, referenceDigest)
	}
	if reference.Language != GeneratedLanguageTypeScript {
		t.Errorf("Language = %q, want ts", reference.Language)
	}
}

// TestDecodeGeneratedClientReference_ToleratesUnknownMembers pins the tolerant
// decode: a member a later protocol version adds still yields the edge, because
// a dropped edge under-selects while a kept one only over-orders.
func TestDecodeGeneratedClientReference_ToleratesUnknownMembers(t *testing.T) {
	reference, ok := DecodeGeneratedClientReference(referenceManifest(`{
		"protocolVersion": 2,
		"generatedBy": "@putnami/clientgen",
		"language": "go",
		"service": {"id": "catalog.items", "audience": "api://catalog.items"},
		"contractSha256": "` + referenceDigest + `",
		"aMemberThisVersionDoesNotKnow": {"any": "shape"}
	}`))
	if !ok || reference.ServiceID != "catalog.items" {
		t.Fatalf("a newer manifest yielded %+v, ok=%v; want the edge it still names", reference, ok)
	}
}

// TestDecodeGeneratedClientReference_RefusesWhatCannotNameTheEdge pins the
// refusals: bytes no first-party generator wrote, and bytes that name only one
// end of the edge.
func TestDecodeGeneratedClientReference_RefusesWhatCannotNameTheEdge(t *testing.T) {
	cases := map[string]string{
		"not json":            `{`,
		"foreign generator":   `{"protocolVersion":1,"generatedBy":"other","language":"ts","service":{"id":"a"},"contractSha256":"` + referenceDigest + `"}`,
		"no protocol version": `{"generatedBy":"@putnami/clientgen","language":"ts","service":{"id":"a"},"contractSha256":"` + referenceDigest + `"}`,
		"no service":          `{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"ts","contractSha256":"` + referenceDigest + `"}`,
		"no contract digest":  `{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"ts","service":{"id":"a"}}`,
		"short digest":        `{"protocolVersion":1,"generatedBy":"@putnami/clientgen","language":"ts","service":{"id":"a"},"contractSha256":"abcd"}`,
	}
	for name, body := range cases {
		if _, ok := DecodeGeneratedClientReference(referenceManifest(body)); ok {
			t.Errorf("%s was read as a generated client reference", name)
		}
	}
}

// TestDecodeContractService_ReadsTheProviderIdentity pins the provider end: the
// document-level marker names the service a generated target's manifest points
// back at.
func TestDecodeContractService_ReadsTheProviderIdentity(t *testing.T) {
	service, ok := DecodeContractService([]byte(`{
		"openapi": "3.1.0",
		"x-putnami-client": {
			"protocolVersion": 1,
			"service": {"id": "catalog.items", "audience": "api://catalog.items"}
		},
		"paths": {"/items": {"get": {"operationId": "getItems"}}}
	}`))
	if !ok {
		t.Fatal("a marked contract declared no provider identity")
	}
	if service.ID != "catalog.items" || service.Audience != "api://catalog.items" {
		t.Errorf("service = %+v, want catalog.items / api://catalog.items", service)
	}
}

// TestDecodeContractService_RefusesAnUnmarkedContract pins that a document with
// no first-party marker, or a blank identity, provides no service: neither
// identifies a provider a client could name.
func TestDecodeContractService_RefusesAnUnmarkedContract(t *testing.T) {
	cases := map[string]string{
		"no marker":      `{"openapi":"3.1.0","paths":{}}`,
		"blank identity": `{"x-putnami-client":{"protocolVersion":1,"service":{"id":"  "}}}`,
		"not json":       `{`,
	}
	for name, body := range cases {
		if service, ok := DecodeContractService([]byte(body)); ok {
			t.Errorf("%s resolved to provider %+v", name, service)
		}
	}
}
