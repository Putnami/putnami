package api

import (
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// cachedUnaryOperation is a safe unary read declaring fresh 5 s / stale 5 min.
func cachedUnaryOperation(path string) *clientcontract.OperationV1 {
	operation := strictUnaryOperation(path, []clientcontract.DeclaredError{})
	staleMs := 300000
	operation.Resilience = &clientcontract.ResiliencePolicy{Cache: &clientcontract.CachePolicy{
		FreshMs: 5000, StaleMs: &staleMs, KeyFields: []string{"path.id"}, InvalidationFields: []string{"id"},
	}}
	return operation
}

// The generated Go client states which operations it may answer from memory
// and pins, in its source, the runtime capability that does it.
func TestGeneratedGoClientPinsTheResponseCacheCapability(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-response-cache", "the-emitted-go-client-pins-the-response-cache-capability")
	spec := compileFixtureSpec()
	source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "usersclient", ClientName: "UsersClient"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	if !strings.Contains(source, `var _ = client.RequireRuntimeCapabilities("response-cache")`) {
		t.Fatalf("the emitted client does not pin the response-cache capability:\n%s", source)
	}
	if !strings.Contains(source, `\"cache\":{\"freshMs\":5000,\"staleMs\":300000,\"keyFields\":[\"path.id\"],\"invalidationFields\":[\"id\"]}`) {
		t.Fatal("the emitted operation descriptor dropped the cache policy")
	}

	// Without a cache declaration nothing is pinned.
	for i := range spec.Services[0].Methods {
		if spec.Services[0].Methods[i].OperationID == "lookupUser" {
			spec.Services[0].Methods[i].Client.Resilience = nil
		}
	}
	uncached, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "usersclient", ClientName: "UsersClient"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(uncached, "RequireRuntimeCapabilities") {
		t.Fatal("a client without a cache pins a runtime capability")
	}
}
