package security

import (
	"net/http/httptest"
	"testing"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/protocol/features/spectest"
)

// Contract generators discover an optional rule through this claim; a rename
// on either side must fail to compile rather than silently publish a required
// credential.
var _ phttp.SecurityOptionalAuthentication = Options{}

func optionalRequest(authorization string) *phttp.Context {
	request := httptest.NewRequest("GET", "/packages/public/resolve", nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	return phttp.NewContext(httptest.NewRecorder(), request)
}

// emptyAuthorizationRequest carries an Authorization header whose value is
// empty or blank: present on the wire, yet presenting no credential.
func emptyAuthorizationRequest(value string) *phttp.Context {
	request := httptest.NewRequest("GET", "/packages/public/resolve", nil)
	request.Header["Authorization"] = []string{value}
	return phttp.NewContext(httptest.NewRecorder(), request)
}

func TestAnOptionalRuleServesARequestThatPresentsNoCredential(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "optional-authentication",
		"an-optional-rule-serves-a-request-that-presents-no-credential")
	rule := Options{Optional: true, Scopes: []string{"packages:read"}, Client: []string{"registry.cli"}}
	if !rule.OptionalAuthentication() {
		t.Fatal("an optional rule does not claim optional authentication")
	}
	// An empty or blank header presents no credential either: it is absent for
	// the rule, as it is in the TypeScript twin.
	requests := map[string]*phttp.Context{
		"no Authorization header": optionalRequest(""),
		"an empty Authorization":  emptyAuthorizationRequest(""),
		"a blank Authorization":   emptyAuthorizationRequest("   "),
	}
	for name, ctx := range requests {
		t.Run(name, func(t *testing.T) {
			served := false
			response := Middleware(rule)(ctx, func() *phttp.Response {
				served = true
				return phttp.JSON("public")
			})
			if response.Status != 200 || !served {
				t.Fatalf("anonymous request status = %d served = %v, want 200 and the handler reached", response.Status, served)
			}
		})
	}
}

func TestAnOptionalRuleStillAuthorizesAnAuthenticatedCaller(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "optional-authentication",
		"an-optional-rule-authorizes-an-authenticated-caller-like-a-required-one")
	rule := Options{Optional: true, Scopes: []string{"packages:read"}}
	cases := []struct {
		name   string
		user   *phttp.Claims
		status int
	}{
		{name: "granted scope", user: &phttp.Claims{Subject: "reader", Scopes: []string{"packages:read"}}, status: 200},
		{name: "missing scope", user: &phttp.Claims{Subject: "stranger"}, status: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := optionalRequest("Bearer resolved")
			ctx.User = tc.user
			response := Middleware(rule)(ctx, func() *phttp.Response { return phttp.JSON("private") })
			if response.Status != tc.status {
				t.Fatalf("status = %d, want %d", response.Status, tc.status)
			}
		})
	}
}

func TestAnOptionalRuleRefusesACredentialThatResolvedNoIdentity(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "optional-authentication",
		"an-optional-rule-refuses-a-credential-that-resolved-no-identity")
	served := false
	response := Middleware(Options{Optional: true})(optionalRequest("Bearer expired"), func() *phttp.Response {
		served = true
		return phttp.JSON("public")
	})
	if response.Status != 401 || served {
		t.Fatalf("unresolved credential status = %d served = %v, want 401 before the handler", response.Status, served)
	}
}

func TestARuleIsRequiredUnlessItSaysOptional(t *testing.T) {
	spectest.Proves(t, "go/authentication-authorization", "optional-authentication",
		"a-rule-requires-authentication-unless-it-declares-optional")
	rule := Options{Scopes: []string{"packages:read"}}
	if rule.OptionalAuthentication() {
		t.Fatal("a rule that never declared Optional claims optional authentication")
	}
	response := Middleware(rule)(optionalRequest(""), func() *phttp.Response { return phttp.JSON("public") })
	if response.Status != 401 {
		t.Fatalf("anonymous request to a required rule status = %d, want 401", response.Status)
	}
}
