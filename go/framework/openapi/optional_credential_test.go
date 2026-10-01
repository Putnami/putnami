package openapi

import (
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/api"
	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/security"
)

// An optional credential is a route that answers anonymous and authenticated
// callers alike — a public package read that answers more to an authorized
// caller. The route's rule says so (security.Options{Optional: true}, through
// phttp.SecurityOptionalAuthentication); the contract then offers the
// credential first and the anonymous alternative last. ADR 0002 of
// go/framework/security.

const optionalCredentialFeature = "go/api-contracts"
const optionalCredentialRequirement = "optional-credential"

type resolvedPackage struct {
	Caller string `json:"caller" validate:"required"`
}

type packageParams struct {
	Name string `json:"name" validate:"required"`
}

func userAlternatives(anonymousFirst bool) clientcontract.Security {
	user := clientcontract.SecurityAlternative{AllOf: []clientcontract.SecurityRequirement{{Profile: "user"}}}
	anonymous := clientcontract.SecurityAlternative{AllOf: []clientcontract.SecurityRequirement{}}
	if anonymousFirst {
		return clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{anonymous, user}}
	}
	return clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{user, anonymous}}
}

// publishOptionalRoute registers one route on a first-party provider and
// returns the published operation, or the error publication failed with.
func publishOptionalRoute(t *testing.T, profiles []string, rule phttp.SecurityRule, security *clientcontract.Security) (Operation, error) {
	t.Helper()
	credentials := map[string]clientcontract.CredentialProfile{}
	for _, profile := range profiles {
		credentials[profile] = clientcontract.CredentialProfile{Kind: clientcontract.CredentialForwardedUserToken}
	}
	apiPlugin := api.New(&fakeServer{}, api.WithClientService(api.ClientServiceOptions{
		Service:     clientcontract.Service{ID: "put-server", Audience: "put-server"},
		Credentials: credentials,
	}))
	endpoint := api.Endpoint("GET", "/{namespace}/{package}/resolve").Returns(api.Type[resolvedPackage]())
	if rule != nil {
		endpoint = endpoint.Secure(rule)
	}
	if security != nil {
		endpoint = endpoint.Client(api.ClientOperationOptions{Security: *security})
	}
	apiPlugin.Register(endpoint.Handle(func(_ *phttp.EndpointContext) *phttp.Response { return phttp.JSON(nil) }))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	openapiPlugin := NewPlugin(PluginOptions{Title: "Packages", Version: "1.0.0"}).From(apiPlugin)
	if err := openapiPlugin.Configure(context.Background(), nil); err != nil {
		return Operation{}, err
	}
	return openapiPlugin.Spec().Paths["/{namespace}/{package}/resolve"]["get"], nil
}

func TestOpenAPI_AnOptionalRulePublishesItsCredentialThenTheAnonymousAlternative(t *testing.T) {
	spectest.Proves(t, optionalCredentialFeature, optionalCredentialRequirement,
		"an-optional-rule-publishes-its-credential-then-the-anonymous-alternative")
	optional := security.Options{Optional: true, Scopes: []string{"packages:read"}}
	wantSecurity := clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
		{AllOf: []clientcontract.SecurityRequirement{{Profile: "user", Scopes: []string{"packages:read"}}}},
		{AllOf: []clientcontract.SecurityRequirement{}},
	}}

	t.Run("declared alternatives keep the anonymous one empty", func(t *testing.T) {
		declared := userAlternatives(false)
		operation, err := publishOptionalRoute(t, []string{"user"}, optional, &declared)
		if err != nil {
			t.Fatalf("an optional rule with a trailing anonymous alternative failed publication: %v", err)
		}
		// The rule's scope binds the authenticated caller only: it is merged into
		// the credentialed alternative and never into the anonymous one.
		if !reflect.DeepEqual(operation.ClientContract.Security, wantSecurity) {
			t.Fatalf("security = %#v, want %#v", operation.ClientContract.Security, wantSecurity)
		}
		if len(declared.Alternatives[0].AllOf[0].Scopes) != 0 {
			t.Fatalf("publication mutated the declared alternatives: %#v", declared)
		}
		// The plain OpenAPI document says the same thing in its own vocabulary:
		// the empty security requirement is the anonymous alternative.
		if want := []SecurityReq{{"bearerAuth": {}}, {}}; !reflect.DeepEqual(operation.Security, want) {
			t.Fatalf("OpenAPI security = %#v, want %#v", operation.Security, want)
		}
	})

	t.Run("one credential profile is derived without a declaration", func(t *testing.T) {
		operation, err := publishOptionalRoute(t, []string{"user"}, optional, nil)
		if err != nil {
			t.Fatalf("an optional rule with one credential profile failed publication: %v", err)
		}
		if !reflect.DeepEqual(operation.ClientContract.Security, wantSecurity) {
			t.Fatalf("derived security = %#v, want %#v", operation.ClientContract.Security, wantSecurity)
		}
	})

	t.Run("a provider may still require first-party clients to authenticate", func(t *testing.T) {
		declared := clientcontract.Security{Alternatives: userAlternatives(false).Alternatives[:1]}
		operation, err := publishOptionalRoute(t, []string{"user"}, optional, &declared)
		if err != nil {
			t.Fatalf("a credential-only declaration on an optional rule failed publication: %v", err)
		}
		if got := operation.ClientContract.Security.Alternatives; len(got) != 1 || len(got[0].AllOf) != 1 {
			t.Fatalf("security = %#v, want the declared credential alone", got)
		}
	})
}

func TestOpenAPI_AnAnonymousAlternativeIsRefusedUnlessTheRuleIsOptionalAndItComesLast(t *testing.T) {
	spectest.Proves(t, optionalCredentialFeature, optionalCredentialRequirement,
		"an-anonymous-alternative-is-refused-unless-the-rule-is-optional-and-it-comes-last")
	declared := userAlternatives(false)
	anonymousFirst := userAlternatives(true)
	cases := []struct {
		name     string
		rule     phttp.SecurityRule
		security *clientcontract.Security
		reason   string
	}{
		{
			// The issue's repro: the route has no rule at all, so it cannot
			// advertise a credential either.
			name: "an unsecured route", security: &declared,
			reason: "the route has no security rule",
		},
		{
			name: "a rule that requires authentication", rule: security.Options{Scopes: []string{"packages:read"}}, security: &declared,
			reason: "its security rule requires authentication",
		},
		{
			name: "an anonymous alternative before a credentialed one", rule: security.Options{Optional: true}, security: &anonymousFirst,
			reason: "must come last",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := publishOptionalRoute(t, []string{"user"}, tc.rule, tc.security)
			if err == nil {
				t.Fatal("publication accepted the alternatives")
			}
			for _, want := range []string{"GET /{namespace}/{package}/resolve", tc.reason, clientcontract.ErrorCodeInvalidSecurity} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal = %v, want it to name %q", err, want)
				}
			}
		})
	}
}

func TestOpenAPI_SeveralCredentialProfilesAreNeverGuessedForAnOptionalRule(t *testing.T) {
	spectest.Proves(t, optionalCredentialFeature, optionalCredentialRequirement,
		"several-credential-profiles-are-never-guessed-for-an-optional-rule")
	_, err := publishOptionalRoute(t, []string{"user", "registry"}, security.Options{Optional: true}, nil)
	if err == nil || !strings.Contains(err.Error(), clientcontract.ErrorCodeInvalidSecurity) {
		t.Fatalf("publication error = %v, want the provider to declare its alternatives", err)
	}
}

// optionalCredentialProvider stands up a real provider whose resolve route
// serves anonymous and authenticated callers, reads its published document
// back through the strict reader, and returns the operation a generated client
// embeds for it.
func optionalCredentialProvider(t *testing.T) (string, client.ServiceDescriptor, client.Operation) {
	t.Helper()
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	httpServer.Use(security.IdentityResolver(func(ctx *phttp.Context) *phttp.Claims {
		switch ctx.Header("Authorization") {
		case "Bearer reader-token":
			return &phttp.Claims{Subject: "reader", Scopes: []string{"packages:read"}}
		case "Bearer stranger-token":
			return &phttp.Claims{Subject: "stranger"}
		}
		return nil
	}))
	apiPlugin := api.New(httpServer, api.WithClientService(api.ClientServiceOptions{
		Service:     clientcontract.Service{ID: "put-server", Audience: "put-server"},
		Credentials: map[string]clientcontract.CredentialProfile{"user": {Kind: clientcontract.CredentialForwardedUserToken}},
	}))
	declared := userAlternatives(false)
	apiPlugin.Register(api.Endpoint("GET", "/packages/{name}/resolve").
		Params(api.Type[packageParams]()).
		Returns(api.Type[resolvedPackage]()).
		Secure(security.Options{Optional: true, Scopes: []string{"packages:read"}}).
		Client(api.ClientOperationOptions{Security: declared}).
		MayThrow(perrors.CodeUnauthorized, perrors.CodeForbidden).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			if ctx.User == nil {
				return phttp.JSON(resolvedPackage{Caller: "anonymous"})
			}
			return phttp.JSON(resolvedPackage{Caller: ctx.User.Subject})
		}))
	openapiPlugin := NewPlugin(PluginOptions{Title: "Packages", Version: "1.0.0"}).From(apiPlugin)
	if err := apiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	if err := openapiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("openapi Configure: %v", err)
	}
	raw, err := openapiPlugin.OpenAPISpecJSON()
	if err != nil {
		t.Fatalf("publish provider document: %v", err)
	}
	ir, err := api.ReadOpenAPISpec(raw)
	if err != nil {
		t.Fatalf("strict reader rejected the provider document: %v", err)
	}
	operation, ok := generatedOperations(ir)[api.CanonicalOperationID("GET", "/packages/{name}/resolve")]
	if !ok {
		t.Fatalf("the provider document declares no resolve operation: %v", keys(generatedOperations(ir)))
	}
	server := httptest.NewServer(httpServer.Handler())
	t.Cleanup(server.Close)
	return server.URL, client.ServiceDescriptor{Contract: *ir.Contract, Schemas: ir.Schemas}, operation
}

func TestOpenAPI_ABoundClientPresentsTheOptionalCredentialItHoldsAndCallsAnonymouslyOtherwise(t *testing.T) {
	spectest.Proves(t, optionalCredentialFeature, optionalCredentialRequirement,
		"a-bound-client-presents-the-optional-credential-it-holds-and-calls-anonymously-otherwise")
	url, descriptor, operation := optionalCredentialProvider(t)
	bind := func(credentials map[string]client.CredentialBinding) *client.Client {
		t.Helper()
		bound, err := client.NewServiceClientBinding(client.ServiceBinding{
			URL: url, ClientID: "registry.cli", AllowInsecure: true, Credentials: credentials,
		}, descriptor)
		if err != nil {
			t.Fatalf("bind the generated client: %v", err)
		}
		return bound
	}
	forwarding := bind(map[string]client.CredentialBinding{"user": {Source: client.CredentialSourceForwardedUser}})
	anonymous := bind(nil)
	resolve := func(ctx context.Context, bound *client.Client) (resolvedPackage, error) {
		return client.Call[resolvedPackage](ctx, bound, &client.Request{Method: http.MethodGet, Path: "/packages/private/resolve"}, operation)
	}

	cases := []struct {
		name   string
		ctx    context.Context
		bound  *client.Client
		caller string
		code   perrors.Code
	}{
		{name: "the binding holds the credential", ctx: client.WithForwardedUserToken(t.Context(), "reader-token"), bound: forwarding, caller: "reader"},
		{name: "the binding forwards and the call carries no token", ctx: t.Context(), bound: forwarding, caller: "anonymous"},
		{name: "the binding declares no credential", ctx: client.WithForwardedUserToken(t.Context(), "reader-token"), bound: anonymous, caller: "anonymous"},
		// Presenting a credential is never a downgrade: the provider authorizes
		// it like a required rule would, and refuses one it cannot resolve.
		{name: "the credential lacks the scope", ctx: client.WithForwardedUserToken(t.Context(), "stranger-token"), bound: forwarding, code: perrors.CodeForbidden},
		{name: "the credential resolves no identity", ctx: client.WithForwardedUserToken(t.Context(), "expired-token"), bound: forwarding, code: perrors.CodeUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := resolve(tc.ctx, tc.bound)
			if tc.code != "" {
				var remote *client.RemoteError
				if !stderrors.As(err, &remote) || remote.Code() != string(tc.code) {
					t.Fatalf("error = %T %v, want the provider's %s", err, err, tc.code)
				}
				return
			}
			if err != nil || resolved.Caller != tc.caller {
				t.Fatalf("resolved = %#v, %v; want caller %q", resolved, err, tc.caller)
			}
		})
	}
}
