package client

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// A client retained past its registry's close sends nothing, even for an
// anonymous operation that acquires no credential: DoOperation refuses the call
// with the registry's one stable code before any byte leaves. The TypeScript
// runtime refuses a disposed client the same way.
func TestAClientRetainedPastItsRegistryCloseSendsNothing(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		if _, err := writer.Write([]byte(`{"value":"ok"}`)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)

	bindings, err := newServiceBindings(ServicesOptions{
		ClientID: "consumer.workload",
		Services: map[string]ServiceBinding{"inventory": {URL: server.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	generated, err := NewServiceClient(bindings, testDescriptor(map[string]clientcontract.CredentialProfile{}))
	if err != nil {
		t.Fatal(err)
	}
	anonymous := testOperation(
		clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
	)

	if _, err := generated.DoOperation(t.Context(), &Request{Method: http.MethodGet, Path: "/items"}, anonymous); err != nil {
		t.Fatalf("a call before the close failed: %v", err)
	}
	if err := bindings.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := generated.DoOperation(t.Context(), &Request{Method: http.MethodGet, Path: "/items"}, anonymous); !perrors.Is(err, CodeClientClosed) {
		t.Fatalf("a call after the close = %v, want %s", err, CodeClientClosed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("the provider saw %d requests, want only the one made before the close", got)
	}
}
