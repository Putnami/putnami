package consumer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"time"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	itemsclient "go.putnami.dev/examples/service-to-service/clients/go"
	"go.putnami.dev/examples/service-to-service/service"
	phttp "go.putnami.dev/http"
	"go.putnami.dev/inject"
)

// matrixFeature is the feature this sample owns in putnami.features.json. The
// tests below bind the checks it declares, so a result becomes evidence through
// the verification wire rather than through this file.
const matrixFeature = "samples/go-first-party-client-matrix"

type traceTransport struct {
	request *client.Request
}

func (transport *traceTransport) Do(_ context.Context, request *client.Request) (*client.Response, error) {
	transport.request = request
	return &client.Response{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"items":[]}`)}, nil
}

func TestGeneratedClientCarriesExactFeatureTrace(t *testing.T) {
	transport := &traceTransport{}
	base, err := client.NewBuilder().BaseURL("http://example.test").Transport(transport).Build()
	if err != nil {
		t.Fatal(err)
	}
	generated := itemsclient.NewItemsClient(base)
	if _, err := generated.ListItems(context.Background(), itemsclient.ListItemsInput{
		Query: itemsclient.ListItemsQuery{Search: "et", Limit: 10},
	}); err != nil {
		t.Fatal(err)
	}
	if transport.request == nil || transport.request.FeatureTrace == nil {
		t.Fatal("generated client request has no feature trace")
	}
	// The trace resolves method, path, spec hash, canonical operation, project,
	// and feature from the invoked operation — not from the base URL.
	trace := *transport.request.FeatureTrace
	want := client.FeatureTrace{
		GeneratedClient: "ItemsClient",
		OperationID:     "getItems",
		Method:          "GET",
		Path:            "/items",
		SpecHash:        itemsclient.ItemsClientDesign.SpecHash,
		ProducerProject: "go.putnami.dev/examples/service-to-service",
		ProducerFeature: "items/manage",
	}
	if trace != want {
		t.Fatalf("feature trace = %+v, want %+v", trace, want)
	}
}

// Producer identity is recorded per operation, so a descriptor entry that names
// no producer must not pick one up from a sibling operation in the same client.
func TestGeneratedClientDescriptorAttributesEveryOperation(t *testing.T) {
	for _, operation := range itemsclient.ItemsClientDesign.Operations {
		if operation.ProducerFeature != "items/manage" {
			t.Errorf("operation %q feature = %q", operation.OperationID, operation.ProducerFeature)
		}
		if operation.ProducerProject != "go.putnami.dev/examples/service-to-service" {
			t.Errorf("operation %q project = %q", operation.OperationID, operation.ProducerProject)
		}
	}
	if itemsclient.ItemsClientDesign.Trace("ListItems") != nil {
		t.Error("the Go method symbol resolved as a canonical operation id")
	}
}

// realProvider starts the sample provider on a real loopback socket and returns
// its base URL. It is the same Register() route set the client was generated
// from, with the same identity resolver the runnable binary installs.
func realProvider(t *testing.T) string {
	t.Helper()
	serverPlugin := phttp.NewServerPlugin(phttp.ServerConfig{})
	serverPlugin.Use(service.IdentityResolver())
	apiPlugin := api.New(serverPlugin, service.ClientContract())
	service.Register(apiPlugin)
	if err := apiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	server := serverPlugin.TestServer()
	t.Cleanup(server.Close)
	return server.URL
}

// sampleBinding is the whole consumer-side configuration: a URL and the values
// for the credential profiles the provider declared. Production callers supply
// the same block through Putnami config.
func sampleBinding(baseURL string) client.ServicesOptions {
	return client.ServicesOptions{
		ClientID: "catalog.consumer",
		Services: map[string]client.ServiceBinding{"items": {
			URL: baseURL,
			Credentials: map[string]client.CredentialBinding{
				"workload": {
					Provider: client.TokenSourceFunc(func(context.Context, client.CredentialRequest) (client.Credential, error) {
						return client.Credential{Value: service.WorkloadToken, Expiry: time.Now().Add(time.Hour)}, nil
					}),
				},
				"catalog-key": {Source: client.CredentialSourceStatic, Value: service.CatalogAPIKey},
				// A second named header the provider requires together with the
				// api key on one operation.
				"tenant": {Source: client.CredentialSourceStatic, Value: service.SampleTenant},
				// Forwarding the caller's own user token is opt-in: the binding
				// enables it, and the runtime supplies the value per call from
				// the inbound request.
				"user": {Source: client.CredentialSourceForwardedUser},
			},
		}},
	}
}

// boundClient resolves the generated client the way an application does: the
// consumer declares a binding, the framework builds the client, and DI hands it
// over. No base URL, no interceptor, no header is written by consumer code.
func boundClient(t *testing.T, options client.ServicesOptions) *itemsclient.ItemsClient {
	t.Helper()
	consumerModule := Module(options)
	application := app.New("catalog-consumer")
	application.Use(consumerModule)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- application.Start(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for !application.IsRunning() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !application.IsRunning() {
		cancel()
		t.Fatalf("consumer application did not start: %v", <-done)
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("consumer app run: %v", err)
		}
		if err := application.Stop(context.Background()); err != nil {
			t.Errorf("consumer app stop: %v", err)
		}
	})
	value, err := application.Context().Get(inject.TokenOf[*itemsclient.ItemsClient]())
	if err != nil {
		t.Fatal(err)
	}
	generated, ok := value.(*itemsclient.ItemsClient)
	if !ok {
		t.Fatalf("resolved generated client has type %T", value)
	}
	return generated
}

func generatedClientAgainstRealProvider(t *testing.T) *itemsclient.ItemsClient {
	t.Helper()
	return boundClient(t, sampleBinding(realProvider(t)))
}

// TestFetchItem proves provider declaration → generated artifact → typed DI
// binding → real Putnami HTTP routing and response decoding.
func TestFetchItem(t *testing.T) {
	item, err := FetchItem(t.Context(), generatedClientAgainstRealProvider(t), "1")
	if err != nil {
		t.Fatalf("FetchItem: %v", err)
	}
	if item.Id != "1" || item.Name != "Widget" || item.Price == nil || *item.Price != 100 {
		t.Errorf("got %+v, want {Id:1 Name:Widget Price:100}", item)
	}
}

func TestCreateItem(t *testing.T) {
	item, err := CreateItem(t.Context(), generatedClientAgainstRealProvider(t), "Sprocket", 75)
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	if item.Id != "3" || item.Name != "Sprocket" || item.Price == nil || *item.Price != 75 {
		t.Errorf("got %+v, want {Id:3 Name:Sprocket Price:75}", item)
	}
}

// The query string is declared by the provider and typed by the client. An
// absent optional parameter and a present one are two different requests.
func TestGeneratedQueryStringReachesTheProvider(t *testing.T) {
	generated := generatedClientAgainstRealProvider(t)

	both, err := ListItems(t.Context(), generated, itemsclient.ListItemsQuery{Search: "et", Limit: 10})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if both.Items == nil || len(*both.Items) != 2 {
		t.Fatalf("search=et list = %+v, want 2 items", both.Items)
	}

	filtered, err := ListItems(t.Context(), generated, itemsclient.ListItemsQuery{Search: "Gad", Limit: 10})
	if err != nil {
		t.Fatalf("ListItems narrowed: %v", err)
	}
	if filtered.Items == nil || len(*filtered.Items) != 1 || (*filtered.Items)[0].Name != "Gadget" {
		t.Fatalf("search=Gad list = %+v, want only Gadget", filtered.Items)
	}

	limited, err := ListItems(t.Context(), generated, itemsclient.ListItemsQuery{Search: "et", Limit: 1})
	if err != nil {
		t.Fatalf("ListItems limited: %v", err)
	}
	if limited.Items == nil || len(*limited.Items) != 1 {
		t.Fatalf("limit=1 list = %+v, want 1 item", limited.Items)
	}
}

// The declared api-key profile is injected as a request header by the binding.
// Consumer code names no header and holds no key.
func TestDeclaredCredentialHeaderIsInjectedByTheBinding(t *testing.T) {
	check, err := CheckCredential(t.Context(), generatedClientAgainstRealProvider(t))
	if err != nil {
		t.Fatalf("CheckCredential: %v", err)
	}
	if check.Profile != "catalog-key" || check.Header != service.CatalogKeyHeader || !check.Presented {
		t.Fatalf("credential check = %+v", check)
	}
}

// A binding that cannot satisfy the declared credential must fail before the
// request leaves the process — never as an anonymous call the provider rejects.
func TestMissingDeclaredCredentialNeverCallsAnonymously(t *testing.T) {
	reached := false
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(provider.Close)

	anonymous := client.ServicesOptions{
		ClientID: "catalog.consumer",
		Services: map[string]client.ServiceBinding{"items": {URL: provider.URL}},
	}
	if _, err := CheckCredential(t.Context(), boundClient(t, anonymous)); err == nil {
		t.Fatal("call with no credential binding succeeded")
	}
	if reached {
		t.Fatal("the provider received an anonymous request for a credential-guarded operation")
	}
}

// A failure the operation never declared reaches the consumer as an unknown
// remote error: a status it can act on, no typed narrowing that would claim the
// provider declared it, and none of the provider's own prose.
func TestUndeclaredRemoteFailureArrivesUntypedWithItsStatus(t *testing.T) {
	spectest.Proves(t, matrixFeature, "a-result-is-what-the-contract-declared", "an-undeclared-remote-failure-stays-untyped-and-carries-no-prose")
	_, err := FetchItem(t.Context(), generatedClientAgainstRealProvider(t), service.UndeclaredFailureID)
	if err == nil {
		t.Fatal("an undeclared failure resolved")
	}
	var notFound *itemsclient.GetItemsNotFoundError
	if errors.As(err, &notFound) {
		t.Fatal("an undeclared failure narrowed to a declared error type")
	}
	var remote *client.RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("error = %T %v, want a remote error carrying 503", err, err)
	}
	// The provider's message named a shard and a lag; neither is the consumer's
	// business and neither belongs in a client-side error.
	if strings.Contains(err.Error(), "shard") || strings.Contains(err.Error(), "replica") {
		t.Fatalf("the provider's prose reached the consumer: %v", err)
	}
}

// The provider declares 404 → not_found with MayThrow; the generated client
// narrows it to its own error type and keeps the stable code.
func TestDeclaredErrorArrivesTypedWithItsStableCode(t *testing.T) {
	spectest.Proves(t, matrixFeature, "a-result-is-what-the-contract-declared", "a-declared-error-narrows-to-its-generated-type")
	_, err := FetchItem(t.Context(), generatedClientAgainstRealProvider(t), "missing")
	var notFound *itemsclient.GetItemsNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("error = %T %v, want *GetItemsNotFoundError", err, err)
	}
	if notFound.Remote.Code() != "not_found" {
		t.Fatalf("RemoteError.Code() = %q, want not_found", notFound.Remote.Code())
	}
	// D0.9 (ADR 0006): a declared error's schema describes details only; this error declares none,
	// so the envelope is read from Remote and no typed payload exists.
	// Provider prose (`error`, `message`) is deliberately not carried onto RemoteError.
	if notFound.Remote.Payload != nil {
		t.Fatalf("typed payload = %s, want none for an error without a details schema", notFound.Remote.Payload)
	}
	if notFound.Remote.Service() != "items" || notFound.Remote.Operation() != "getItems_Id" {
		t.Fatalf("remote identity = %q/%q", notFound.Remote.Service(), notFound.Remote.Operation())
	}
}

// A response that does not honor the declared success schema is rejected, not
// decoded into a lossy value.
func TestResponseThatViolatesTheDeclaredSchemaIsRejected(t *testing.T) {
	spectest.Proves(t, matrixFeature, "a-result-is-what-the-contract-declared", "a-response-that-violates-the-declared-schema-is-refused")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// `id` is declared required and a string.
		if _, err := w.Write([]byte(`{"name":"Widget","price":100}`)); err != nil {
			t.Errorf("write invalid response: %v", err)
		}
	}))
	t.Cleanup(provider.Close)

	_, err := FetchItem(t.Context(), boundClient(t, sampleBinding(provider.URL)), "1")
	if err == nil {
		t.Fatal("a response missing a required declared property was accepted")
	}
	if !perrors.Is(err, client.CodeClientResponse) {
		t.Fatalf("error = %v, want client.response", err)
	}
}

func TestGeneratedJSONBodiesAgainstRealProvider(t *testing.T) {
	generated := generatedClientAgainstRealProvider(t)
	fidelity := itemsclient.BodyFidelity{
		Enabled:  false,
		Count:    0,
		Label:    "",
		Signed:   9007199254740993,
		Unsigned: ^uint64(0),
		Nullable: nil,
	}
	echoed, err := EchoBodyFidelity(t.Context(), generated, fidelity)
	if err != nil {
		t.Fatalf("EchoBodyFidelity: %v", err)
	}
	if echoed.Enabled || echoed.Count != 0 || echoed.Label != "" || echoed.Signed != fidelity.Signed ||
		echoed.Unsigned != fidelity.Unsigned || echoed.Nullable != nil {
		t.Fatalf("fidelity body changed over generated client/provider round trip: %#v", echoed)
	}

	values := []int64{0, 9007199254740993, 9223372036854775807}
	echoedValues, err := EchoBodyValues(t.Context(), generated, values)
	if err != nil {
		t.Fatalf("EchoBodyValues: %v", err)
	}
	if !slices.Equal(*echoedValues, values) {
		t.Fatalf("array body = %v, want %v", *echoedValues, values)
	}

	echoedCounter, err := EchoBodyCounter(t.Context(), generated, ^uint64(0))
	if err != nil {
		t.Fatalf("EchoBodyCounter: %v", err)
	}
	if *echoedCounter != ^uint64(0) {
		t.Fatalf("counter = %d, want %d", *echoedCounter, ^uint64(0))
	}

	nullable, err := EchoNullableBody(t.Context(), generated, nil)
	if err != nil {
		t.Fatalf("EchoNullableBody: %v", err)
	}
	if nullable != nil {
		t.Fatalf("nullable root = %q, want nil", *nullable)
	}
}

func TestGeneratedClientRejectsInvalidJSONBodyBeforeDispatch(t *testing.T) {
	transport := &traceTransport{}
	base, err := client.NewBuilder().BaseURL("http://example.test").Transport(transport).Build()
	if err != nil {
		t.Fatal(err)
	}
	generated := itemsclient.NewItemsClient(base)
	_, err = generated.CreateBodyFidelity(t.Context(), itemsclient.CreateBodyFidelityInput{Body: itemsclient.BodyFidelity{
		Signed:   9007199254740992,
		Unsigned: 9007199254740993,
	}})
	if err == nil || !perrors.Is(err, client.CodeClientRequest) {
		t.Fatalf("invalid generated body error = %v, want client.request", err)
	}
	if transport.request != nil {
		t.Fatalf("invalid generated body was dispatched: %#v", transport.request)
	}
}

func TestWatchItemUsesGeneratedSecuredSSEStream(t *testing.T) {
	stream, err := WatchItem(t.Context(), generatedClientAgainstRealProvider(t), "1", false)
	if err != nil {
		t.Fatalf("WatchItem: %v", err)
	}
	messages := make([]itemsclient.Item, 0, 1)
	for message := range stream.Messages() {
		messages = append(messages, message)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("WatchItem terminal: %v", err)
	}
	if len(messages) != 1 || messages[0].Id != "1" || messages[0].Name != "Widget" {
		t.Fatalf("messages = %+v", messages)
	}
}

func TestWatchItemMapsProviderTerminalErrorToGeneratedType(t *testing.T) {
	stream, err := WatchItem(t.Context(), generatedClientAgainstRealProvider(t), "missing", false)
	if err != nil {
		t.Fatalf("WatchItem open: %v", err)
	}
	for range stream.Messages() {
	}
	var notFound *itemsclient.GetItemsWatchNotFoundError
	if !errors.As(stream.Err(), &notFound) {
		t.Fatalf("terminal error = %T %v", stream.Err(), stream.Err())
	}
	if notFound.Remote.Code() != "not_found" || notFound.Remote.Payload != nil {
		t.Fatalf("typed terminal error = %v payload=%s", notFound, notFound.Remote.Payload)
	}
}

// sampleBindingWithTokens is the sample binding whose workload token comes from
// tokens, one value per acquisition. It is how a rejected credential and the
// reacquisition that follows are observed from the consumer side.
func sampleBindingWithTokens(baseURL string, tokens []string, acquisitions *atomic.Int32) client.ServicesOptions {
	options := sampleBinding(baseURL)
	binding := options.Services["items"]
	binding.Credentials = map[string]client.CredentialBinding{
		"workload": {Provider: client.TokenSourceFunc(func(context.Context, client.CredentialRequest) (client.Credential, error) {
			index := int(acquisitions.Add(1)) - 1
			if index >= len(tokens) {
				index = len(tokens) - 1
			}
			return client.Credential{Value: tokens[index], Expiry: time.Now().Add(time.Hour)}, nil
		})},
		"catalog-key": {Source: client.CredentialSourceStatic, Value: service.CatalogAPIKey},
	}
	options.Services["items"] = binding
	return options
}

// A refused credential is an admission refusal: the consumer sees a typed error
// and no message at all, the credential is dropped exactly once, and the next
// open reacquires instead of replaying the stream that was refused.
func TestWatchItemRejectedAdmissionReachesTheConsumerBeforeAnyMessage(t *testing.T) {
	var acquisitions atomic.Int32
	generated := boundClient(t, sampleBindingWithTokens(realProvider(t), []string{"not-the-workload-token", service.WorkloadToken}, &acquisitions))

	stream, err := WatchItem(t.Context(), generated, "1", false)
	if err == nil {
		t.Fatalf("a refused credential opened a stream: %+v", stream)
	}
	var remote *client.RemoteError
	if !errors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("admission error = %T %v", err, err)
	}
	if stream != nil {
		t.Fatal("a refused admission returned a stream handle")
	}
	rejected := acquisitions.Load()

	refreshed, err := WatchItem(t.Context(), generated, "1", false)
	if err != nil {
		t.Fatalf("reacquisition after a refused credential: %v", err)
	}
	messages := 0
	for range refreshed.Messages() {
		messages++
	}
	if err := refreshed.Err(); err != nil {
		t.Fatalf("refreshed stream terminal: %v", err)
	}
	if messages != 1 {
		t.Fatalf("messages after refresh = %d", messages)
	}
	if got := acquisitions.Load(); got != rejected+1 {
		t.Fatalf("token acquisitions = %d, want %d", got, rejected+1)
	}
}

// The consumer cancels; the provider is still producing. Exactly one terminal
// reaches the caller and it is the typed cancellation.
func TestWatchItemFollowStreamEndsOnConsumerCancellation(t *testing.T) {
	generated := generatedClientAgainstRealProvider(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := WatchItem(ctx, generated, "1", true)
	if err != nil {
		t.Fatalf("WatchItem follow: %v", err)
	}
	received := 0
	for message := range stream.Messages() {
		if message.Id != "1" {
			t.Fatalf("message = %+v", message)
		}
		received++
		if received == 2 {
			cancel()
		}
	}
	if received < 2 {
		t.Fatalf("messages before cancellation = %d", received)
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, client.CodeClientCanceled) {
		t.Fatalf("terminal after cancellation = %T %v", err, err)
	}
}

// Closing the stream handle is the caller's other exit: it releases the
// provider without the caller ever canceling its own context.
func TestWatchItemFollowStreamIsReleasedByClose(t *testing.T) {
	generated := generatedClientAgainstRealProvider(t)
	stream, err := WatchItem(t.Context(), generated, "1", true)
	if err != nil {
		t.Fatalf("WatchItem follow: %v", err)
	}
	if _, ok := <-stream.Messages(); !ok {
		t.Fatal("follow stream delivered nothing")
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close left the stream open")
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, client.CodeClientCanceled) {
		t.Fatalf("terminal after Close = %T %v", err, err)
	}
}
