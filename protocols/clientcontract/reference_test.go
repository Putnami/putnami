package clientcontract

import "testing"

// TestDecodeContractService_ReadsTheProviderIdentity pins the provider end: the
// document-level marker names the service a client project's generatedFrom
// points back at.
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
