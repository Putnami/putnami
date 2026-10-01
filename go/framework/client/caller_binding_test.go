package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

func TestCallerBindingDescriptorReusesOperationContracts(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "caller-resolved-bindings", "a-caller-binding-snapshots-routing-and-closes-only-its-own-registry")
	want := testDescriptor(map[string]clientcontract.CredentialProfile{})
	source, err := json.Marshal(want.Contract)
	if err != nil {
		t.Fatal(err)
	}
	legacy := MustServiceDescriptor(string(source))
	if !reflect.DeepEqual(legacy, want) {
		t.Fatalf("legacy one-argument constructor changed: %+v", legacy)
	}
	schemas := `{"Value":{"type":"string"}}`
	legacy = MustServiceDescriptor(string(source), schemas)
	if !reflect.DeepEqual(legacy.Contract, want.Contract) || legacy.Schemas["Value"].Type != "string" || legacy.Operations != nil {
		t.Fatalf("legacy two-argument constructor changed: %+v", legacy)
	}
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	descriptor := MustServiceDescriptorWithOperations(string(source), schemas, operation)
	if !reflect.DeepEqual(descriptor.Operations[operation.ID], operation.Contract) || !reflect.DeepEqual(descriptor.Schemas, legacy.Schemas) {
		t.Fatal("explicit inventory lost operation or schema metadata")
	}
	operation.Contract.Transports[0].Path = "/mutated"
	if descriptor.Operations[operation.ID].Transports[0].Path != "/items" {
		t.Fatal("operation mutation changed the descriptor inventory")
	}
	if err := clientcontract.ValidateOperationPaths(descriptor.Operations, map[string]string{operation.ID: "/owner/page"}); err != nil {
		t.Fatalf("shared inventory cannot validate caller paths: %v", err)
	}
	for _, test := range []struct {
		name       string
		operations []Operation
	}{
		{"duplicate identity", []Operation{operation, operation}},
		{"missing identity", []Operation{{Contract: operation.Contract}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid operation inventory accepted")
				}
			}()
			MustServiceDescriptorWithOperations(string(source), schemas, test.operations...)
		})
	}
}

func TestCallerBindingOwnsOnlyItsRegistry(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "caller-resolved-bindings", "a-caller-binding-snapshots-routing-and-closes-only-its-own-registry")
	operation := testOperation(clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}, clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe})
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{})
	descriptor.Operations = map[string]clientcontract.OperationV1{operation.ID: operation.Contract}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/owner/page" {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	paths := map[string]string{operation.ID: "/owner/page"}
	bound, err := NewServiceClientBinding(ServiceBinding{URL: server.URL, ClientID: "replica", OperationPaths: paths}, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	paths[operation.ID] = "/mutated"
	descriptor.Operations[operation.ID] = clientcontract.OperationV1{}
	call := &OperationCall{Request: &Request{Method: "GET", Path: "/items"}}
	if _, err := CallOperation[map[string]string](t.Context(), bound, call, operation); err != nil {
		t.Fatal(err)
	}
	if call.Request.Path != "/items" {
		t.Fatal("request was mutated")
	}
	if err := bound.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bound.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := CallOperation[map[string]string](context.Background(), bound, call, operation); err == nil {
		t.Fatal("anonymous call after close was allowed")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	shared, err := newServiceBindings(ServicesOptions{Services: map[string]ServiceBinding{"inventory": {URL: server.URL, ClientID: "replica"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer shared.Close()
	client, err := NewServiceClient(shared, testDescriptor(map[string]clientcontract.CredentialProfile{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if shared.isClosed() {
		t.Fatal("client closed a shared application registry")
	}
}

func TestCallerBindingRefusesPathsBeforeDispatch(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "caller-resolved-bindings", "an-invalid-caller-binding-is-refused-before-dispatch")
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{})
	if _, err := NewServiceClientBinding(ServiceBinding{URL: "https://example.com", ClientID: "replica", OperationPaths: map[string]string{"unknown": "/path"}}, descriptor); err == nil {
		t.Fatal("unknown operation accepted")
	}
	if _, err := NewServiceClientBinding(ServiceBinding{URL: "https://example.com"}, descriptor); err == nil {
		t.Fatal("missing identity accepted")
	}
}
