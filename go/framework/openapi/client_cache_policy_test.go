package openapi

import (
	"context"
	"strings"
	"testing"

	"go.putnami.dev/api"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// cachePolicyContract builds one real first-party provider with a single
// route and returns the operation contract it publishes, or the refusal the
// declaration earned.
func cachePolicyContract(t *testing.T, endpoint api.EndpointDefinition, path, method string) (*clientcontract.OperationV1, error) {
	t.Helper()
	apiPlugin := api.New(&fakeServer{}, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "identity", Audience: "https://identity.internal"},
	}))
	apiPlugin.Register(endpoint)
	openapiPlugin := NewPlugin(PluginOptions{Title: "Identity", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		return nil, err
	}
	raw, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		return nil, err
	}
	ir, err := api.ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("strict reader rejected the projected contract: %v\n%s", err, raw)
	}
	// The generator consumes exactly what the provider published: an emitted
	// client of a cached operation pins the runtime capability that honors it.
	source, err := api.GenerateClientFromIR(ir, api.ClientGenOptions{PackageName: "identityclient", ClientName: "IdentityClient"})
	if err != nil {
		t.Fatalf("emit the published contract: %v", err)
	}
	contract := openapiPlugin.Spec().Paths[path][method].ClientContract
	cached := contract.Resilience != nil && contract.Resilience.Cache != nil
	if pinned := strings.Contains(source, `client.RequireRuntimeCapabilities("response-cache")`); pinned != cached {
		t.Fatalf("emitted client pins the response-cache capability = %v, operation declares a cache = %v", pinned, cached)
	}
	return contract, nil
}

func cacheDeclaration(keyFields ...string) api.ClientOperationOptions {
	staleMs := 300000
	return api.ClientOperationOptions{
		Idempotency: &clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
		Resilience: &clientcontract.ResiliencePolicy{Cache: &clientcontract.CachePolicy{
			FreshMs: 5000, StaleMs: &staleMs, KeyFields: keyFields,
		}},
	}
}

// The provider states the cache beside the endpoint and the published
// contract carries it unchanged. Cache() keeps emitting Cache-Control for
// HTTP intermediaries; the two declarations are independent.
func TestOpenAPI_ADeclaredCachePolicyIsPublishedInTheOperationContract(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-response-cache",
		"a-declared-cache-policy-is-published-in-the-operation-contract")
	options := cacheDeclaration("path.id")
	options.Resilience.Cache.InvalidationFields = []string{"id", "nickname"}
	operation, err := cachePolicyContract(t, api.Endpoint("GET", "/users/{id}").
		Params(api.Type[apiUserParams]()).
		Returns(api.Type[apiFirstPartyUser]()).
		Cache(phttp.CacheOptions{MaxAge: 5}).
		Client(options).
		Document(), "/users/{id}", "get")
	if err != nil {
		t.Fatal(err)
	}
	if operation.Resilience == nil || operation.Resilience.Cache == nil {
		t.Fatalf("published contract dropped the cache policy: %+v", operation.Resilience)
	}
	cache := operation.Resilience.Cache
	if cache.FreshMs != 5000 || cache.StaleMs == nil || *cache.StaleMs != 300000 || strings.Join(cache.KeyFields, ",") != "path.id" ||
		strings.Join(cache.InvalidationFields, ",") != "id,nickname" {
		t.Fatalf("published cache policy = %+v", cache)
	}
	// Mutating the options after registration cannot change what was published.
	options.Resilience.Cache.KeyFields[0] = "path.other"
	options.Resilience.Cache.InvalidationFields[0] = "labels"
	*options.Resilience.Cache.StaleMs = 1
	if cache.KeyFields[0] != "path.id" || cache.InvalidationFields[0] != "id" || *cache.StaleMs != 300000 {
		t.Fatalf("the published policy aliases the provider's options: %+v", cache)
	}
}

// An invalidation field must name a string, integer or boolean property of
// the answer: one that names nothing, or names a map, would tag no answer and
// make every invalidation by it drop nothing. Publication refuses both.
func TestOpenAPI_AnInvalidationFieldThatNamesNoScalarResponsePropertyIsRefused(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-response-cache",
		"an-invalidation-field-naming-no-scalar-response-property-is-refused-at-publication")
	for _, field := range []string{"userId", "labels"} {
		options := cacheDeclaration("path.id")
		options.Resilience.Cache.InvalidationFields = []string{field}
		if _, err := cachePolicyContract(t, api.Endpoint("GET", "/users/{id}").
			Params(api.Type[apiUserParams]()).
			Returns(api.Type[apiFirstPartyUser]()).
			Client(options).
			Document(), "/users/{id}", "get"); err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("invalidation field %q was published: %v", field, err)
		}
	}
}

// A cache that would replay an effect, and a key field that names no input,
// are refused when the contract is published, with the route named.
func TestOpenAPI_ACacheOnANonIdempotentOperationOrAnUndeclaredKeyFieldIsRefused(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-response-cache",
		"a-cache-on-a-non-idempotent-operation-or-an-undeclared-key-field-is-refused-at-publication")
	nonIdempotent := cacheDeclaration()
	nonIdempotent.Idempotency = &clientcontract.Idempotency{Kind: clientcontract.IdempotencyNonIdempotent}
	if _, err := cachePolicyContract(t, api.Endpoint("POST", "/users").
		Body(api.Type[apiCreateBody]()).
		Returns(api.Type[apiFirstPartyUser]()).
		Client(nonIdempotent).
		Document(), "/users", "post"); err == nil || !strings.Contains(err.Error(), "resilience.cache") {
		t.Fatalf("a cache on a non-idempotent operation was published: %v", err)
	}
	if _, err := cachePolicyContract(t, api.Endpoint("GET", "/users/{id}").
		Params(api.Type[apiUserParams]()).
		Returns(api.Type[apiFirstPartyUser]()).
		Client(cacheDeclaration("path.userId")).
		Document(), "/users/{id}", "get"); err == nil || !strings.Contains(err.Error(), "path.userId") {
		t.Fatalf("a key field naming no input was published: %v", err)
	}
	if _, err := cachePolicyContract(t, api.Endpoint("POST", "/users/lookup").
		Body(api.Type[apiCreateBody]()).
		Returns(api.Type[apiFirstPartyUser]()).
		Client(func() api.ClientOperationOptions {
			options := cacheDeclaration("body.email")
			options.Idempotency = &clientcontract.Idempotency{Kind: clientcontract.IdempotencyIdempotent}
			return options
		}()).
		Document(), "/users/lookup", "post"); err != nil {
		t.Fatalf("a body key field naming a declared property was refused: %v", err)
	}
}
