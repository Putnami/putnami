package itemsclient_test

// Go→TS cell of the cross-language auth matrix: the same identity families the Go
// provider serves, asserted against the real TypeScript provider through the
// client generated from its contract. The provider is the subprocess
// foreign_provider_test.go starts; only the credential bindings change.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.putnami.dev/app"
	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	itemsclient "go.putnami.dev/examples/ts-items-client"
	phttp "go.putnami.dev/http"
)

// The identity values the TypeScript provider knows (src/caller-identity.ts).
// They are credentials of a sample, not secrets. The api key itself is
// `catalogAPIKey`, declared beside the watch-stream helpers.
const (
	sampleTenant = "tenant-a"
	userToken    = "user-token-alice" //nolint:gosec // sample identity, not a credential of any real system
	userSubject  = "alice"
)

// identityBinding carries every credential profile the TypeScript provider
// declares: the api key, the named tenant header, and the opt-in forwarding of
// the caller's own user token.
func identityBinding(baseURL string) client.ServicesOptions {
	return client.ServicesOptions{
		ClientID: "cross-language-consumer",
		Services: map[string]client.ServiceBinding{serviceID: {
			URL: baseURL,
			Credentials: map[string]client.CredentialBinding{
				"catalog-key": {Source: client.CredentialSourceStatic, Value: catalogAPIKey},
				"tenant":      {Source: client.CredentialSourceStatic, Value: sampleTenant},
				"user":        {Source: client.CredentialSourceForwardedUser},
			},
		}},
	}
}

func identityClient(t *testing.T, options client.ServicesOptions) *itemsclient.ItemsClient {
	t.Helper()
	module := app.NewModule("ts-items-consumer")
	module.Use(client.Services(options))
	itemsclient.RegisterItemsClient(module)
	return startBoundConsumer(t, module)
}

// Two credentials of one alternative cross the language boundary together, and
// the TypeScript provider reports both from the identity its own resolver
// built — the same answer the Go provider gives its Go consumer.
func TestTypeScriptProviderRequiresBothCredentialsOfOneAlternative(t *testing.T) {
	generated := identityClient(t, identityBinding(startForeignProvider(t)))

	check, err := generated.ListTenantCheck(t.Context(), itemsclient.ListTenantCheckInput{})
	if err != nil {
		t.Fatalf("ListTenantCheck: %v", err)
	}
	if !check.Key || !check.Tenant {
		t.Fatalf("tenant check = %+v, want both credentials reported", *check)
	}
}

// A half-bound alternative is refused in the consumer, so the language on the
// other side never sees the call at all.
func TestTypeScriptProviderNeverSeesAHalfSatisfiedAlternative(t *testing.T) {
	partial := identityBinding(startForeignProvider(t))
	delete(partial.Services[serviceID].Credentials, "tenant")
	generated := identityClient(t, partial)

	_, err := generated.ListTenantCheck(t.Context(), itemsclient.ListTenantCheckInput{})
	if err == nil {
		t.Fatal("a binding carrying one credential of the alternative dispatched")
	}
	if !perrors.Is(err, client.CodeClientCredential) {
		t.Fatalf("error = %v, want %s", err, client.CodeClientCredential)
	}
}

// A Connect call the provider refuses reaches this consumer as the refusal it
// is, on the unary wire and on the stream wire, and no message is delivered.
func TestTypeScriptProviderConnectRefusalReachesTheGoConsumer(t *testing.T) {
	baseURL := startForeignProvider(t)
	refused := identityClient(t, client.ServicesOptions{
		ClientID: "cross-language-consumer",
		Services: map[string]client.ServiceBinding{serviceID: {
			URL:         baseURL,
			Credentials: map[string]client.CredentialBinding{"catalog-key": {Source: client.CredentialSourceStatic, Value: "not-the-catalog-key"}},
		}},
	})

	_, err := refused.GetQuotes(t.Context(), itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "1"}})
	var remote *client.RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unary refusal = %T %v", err, err)
	}

	stream, err := refused.GetQuotesTicks(t.Context(), itemsclient.GetQuotesTicksInput{
		Path: itemsclient.GetQuotesTicksPath{Id: "1"},
	})
	if err != nil {
		// A stream refused at admission may surface on the open or on the
		// terminal; either way it must carry the status and no message.
		if !errors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
			t.Fatalf("stream open refusal = %T %v", err, err)
		}
		return
	}
	defer func() { _ = stream.Close() }()
	for range stream.Messages() {
		t.Fatal("a refused stream delivered a message")
	}
	if !errors.As(stream.Err(), &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stream terminal = %T %v", stream.Err(), stream.Err())
	}
}

// The user identity is the caller's own and comes from a real inbound request:
// the Go consumer runs behind its own route, and the TypeScript provider names
// the user that route's token identified.
func TestTypeScriptProviderNamesTheUserTheGoConsumerForwarded(t *testing.T) {
	generated := identityClient(t, identityBinding(startForeignProvider(t)))
	consumerURL := goConsumerBehindItsOwnRoute(t, generated)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, consumerURL+"/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+userToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("inbound status = %d, want 200", response.StatusCode)
	}
	var caller itemsclient.ListWhoamiOutput
	if err := json.NewDecoder(response.Body).Decode(&caller); err != nil {
		t.Fatal(err)
	}
	if caller.Subject != userSubject {
		t.Fatalf("subject = %q, want %q", caller.Subject, userSubject)
	}
}

// Nothing to forward means no call: the consumer refuses before dispatch rather
// than reaching the foreign provider with its own workload credentials.
func TestTypeScriptProviderIsNeverCalledWithoutAnInboundIdentity(t *testing.T) {
	generated := identityClient(t, identityBinding(startForeignProvider(t)))

	// Called outside any inbound request, the forwarded-user profile has no
	// value to carry.
	_, err := generated.ListWhoami(t.Context(), itemsclient.ListWhoamiInput{})
	if err == nil {
		t.Fatal("a forwarded-user call with no inbound identity dispatched")
	}
	if !perrors.Is(err, client.CodeClientCredential) {
		t.Fatalf("error = %v, want %s", err, client.CodeClientCredential)
	}
}

// goConsumerBehindItsOwnRoute serves one route that calls the foreign provider
// with the context of the request it received, and returns its base URL.
func goConsumerBehindItsOwnRoute(t *testing.T, generated *itemsclient.ItemsClient) string {
	t.Helper()
	server := phttp.NewServerPlugin(phttp.ServerConfig{})
	server.GET("/me", func(ctx *phttp.Context) *phttp.Response {
		caller, err := generated.ListWhoami(ctx.Context(), itemsclient.ListWhoamiInput{})
		if err != nil {
			return phttp.JSONStatus(http.StatusBadGateway, map[string]string{"error": err.Error()})
		}
		return phttp.JSON(caller)
	})
	consumerServer := httptest.NewServer(server.Handler())
	t.Cleanup(consumerServer.Close)
	return consumerServer.URL
}
