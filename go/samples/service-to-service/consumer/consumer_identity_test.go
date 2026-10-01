package consumer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	itemsclient "go.putnami.dev/examples/service-to-service/clients/go"
	"go.putnami.dev/examples/service-to-service/service"
	phttp "go.putnami.dev/http"
)

// The `catalog-key` api key and the `tenant` named header are one alternative:
// the provider requires both together. A binding that carries both dispatches,
// and the provider answers from the identity its resolver built — no consumer
// code writes either header.
func TestOneAlternativeCarriesBothOfItsCredentials(t *testing.T) {
	check, err := CheckTenant(t.Context(), generatedClientAgainstRealProvider(t))
	if err != nil {
		t.Fatalf("CheckTenant: %v", err)
	}
	if !check.Key || !check.Tenant {
		t.Fatalf("tenant check = %+v, want both credentials reported", check)
	}
}

// An alternative is satisfied whole or not at all. A binding missing one of its
// two credentials must fail before the request leaves the process: dispatching
// with the api key alone would be a silent downgrade to a weaker identity.
func TestAnAlternativeMissingOneCredentialNeverDowngrades(t *testing.T) {
	spectest.Proves(t, matrixFeature, "identity-is-declared-and-never-downgraded", "an-alternative-is-satisfied-whole-or-not-at-all")
	provider, requests := recordingProvider(t)

	partial := client.ServicesOptions{
		ClientID: "catalog.consumer",
		Services: map[string]client.ServiceBinding{"items": {
			URL:         provider,
			Credentials: map[string]client.CredentialBinding{"catalog-key": {Source: client.CredentialSourceStatic, Value: service.CatalogAPIKey}},
		}},
	}
	_, err := CheckTenant(t.Context(), boundClient(t, partial))
	if err == nil {
		t.Fatal("a binding carrying one credential of the alternative dispatched")
	}
	// The refusal is the consumer's own, not a status the provider returned.
	if !perrors.Is(err, client.CodeClientCredential) {
		t.Fatalf("error = %v, want %s", err, client.CodeClientCredential)
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("the provider received %d request(s) for a half-satisfied alternative", len(got))
	}
}

// The user identity is the caller's own, and it comes from a real inbound
// request: the consumer runs behind its own route, and the runtime carries the
// token that route received. The consumer passes its request context and writes
// no header.
func TestForwardedUserIdentityComesFromTheInboundRequest(t *testing.T) {
	spectest.Proves(t, matrixFeature, "identity-is-declared-and-never-downgraded", "a-forwarded-user-identity-comes-from-the-inbound-request")
	consumerServer := consumerBehindItsOwnRoute(t, sampleBinding(realProvider(t)))

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, consumerServer+"/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+service.UserToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("inbound status = %d, want 200", response.StatusCode)
	}
	var caller itemsclient.Caller
	if err := json.NewDecoder(response.Body).Decode(&caller); err != nil {
		t.Fatal(err)
	}
	// The provider names the user the consumer forwarded, and names it from the
	// identity its resolver built — never from a value the consumer chose.
	if caller.Subject != service.UserSubject {
		t.Fatalf("subject = %q, want %q", caller.Subject, service.UserSubject)
	}
}

// No inbound identity means no call: a forwarded-user operation has nothing to
// forward, so it fails in the consumer instead of reaching the provider with
// the calling workload's own credentials.
func TestAnAbsentInboundIdentityNeverCallsAnonymously(t *testing.T) {
	provider, requests := recordingProvider(t)
	consumerServer := consumerBehindItsOwnRoute(t, sampleBinding(provider))

	response, err := http.Get(consumerServer + "/me") //nolint:noctx // the inbound request under test carries no identity
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusOK {
		t.Fatal("a call with no inbound identity succeeded")
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("the provider received %d request(s) for an unforwardable identity", len(got))
	}
}

// Forwarding is opt-in per binding: an inbound token is not a license to send
// it onwards. A binding that never declares the profile refuses the call even
// when the inbound request carries exactly the right token.
func TestForwardingRequiresTheBindingToOptIn(t *testing.T) {
	provider, requests := recordingProvider(t)
	binding := sampleBinding(provider)
	delete(binding.Services["items"].Credentials, "user")
	consumerServer := consumerBehindItsOwnRoute(t, binding)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, consumerServer+"/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+service.UserToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusOK {
		t.Fatal("an unbound forwarded-user profile forwarded the inbound token")
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("the provider received %d request(s) for an unbound profile", len(got))
	}
}

// A forwarded user token travels only to the operation that declares it. The
// same inbound request also lists items, and that operation declares no user
// credential, so its request must carry none.
func TestAForwardedTokenReachesOnlyTheOperationThatDeclaresIt(t *testing.T) {
	spectest.Proves(t, matrixFeature, "identity-is-declared-and-never-downgraded", "a-credential-reaches-only-the-operation-that-declares-it")
	provider, requests := recordingProvider(t)
	items := boundClient(t, sampleBinding(provider))
	server := phttp.NewServerPlugin(phttp.ServerConfig{})
	server.GET("/list", func(ctx *phttp.Context) *phttp.Response {
		list, err := ListItems(ctx.Context(), items, itemsclient.ListItemsQuery{Search: "", Limit: 10})
		if err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return phttp.JSON(list)
	})
	consumerServer := httptest.NewServer(server.Handler())
	t.Cleanup(consumerServer.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, consumerServer.URL+"/list", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+service.UserToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()

	got := requests()
	if len(got) != 1 {
		t.Fatalf("the provider received %d request(s), want 1", len(got))
	}
	if authorization := got[0].Get("Authorization"); authorization != "" {
		t.Fatalf("an operation declaring no user credential carried Authorization %q", authorization)
	}
	if got[0].Get(service.CatalogKeyHeader) != "" {
		t.Fatalf("an operation declaring no api key carried %s", service.CatalogKeyHeader)
	}
}

// consumerBehindItsOwnRoute runs the generated client inside an inbound route,
// which is what makes the forwarded identity real: the token is the one that
// route received, carried by the runtime from that request's context.
func consumerBehindItsOwnRoute(t *testing.T, options client.ServicesOptions) string {
	t.Helper()
	items := boundClient(t, options)
	server := phttp.NewServerPlugin(phttp.ServerConfig{})
	server.GET("/me", func(ctx *phttp.Context) *phttp.Response {
		// The consumer passes the context of its inbound request and nothing
		// else: no header, no token value, no interceptor.
		caller, err := WhoAmI(ctx.Context(), items)
		if err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return phttp.JSON(caller)
	})
	consumerServer := httptest.NewServer(server.Handler())
	t.Cleanup(consumerServer.Close)
	return consumerServer.URL
}

// recordingProvider answers every request with an empty item list and records
// the headers it received. It is how a test asserts what did *not* leave the
// consumer: an absent request and an absent header are both observable here.
func recordingProvider(t *testing.T) (string, func() []http.Header) {
	t.Helper()
	var received []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(server.Close)
	return server.URL, func() []http.Header { return received }
}
