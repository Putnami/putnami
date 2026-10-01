package consumer

// The lifecycle family of the Go consumer cells: what a stopped consumer
// application and a provider that goes away leave behind — typed refusals and
// released connections, never a call that still leaves or a stream that waits.

import (
	"context"
	"net/http/httptest"
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

// startStoppableConsumer is boundClient with the stop in the test's hands: a
// lifecycle cell stops the consumer application mid-test and keeps calling the
// client it handed out.
func startStoppableConsumer(t *testing.T, options client.ServicesOptions) (*itemsclient.ItemsClient, func()) {
	t.Helper()
	application := app.New("catalog-consumer")
	application.Use(Module(options))
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
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		if err := <-done; err != nil {
			t.Errorf("consumer app run: %v", err)
		}
		if err := application.Stop(context.Background()); err != nil {
			t.Errorf("consumer app stop: %v", err)
		}
	}
	t.Cleanup(stop)
	value, err := application.Context().Get(inject.TokenOf[*itemsclient.ItemsClient]())
	if err != nil {
		t.Fatal(err)
	}
	generated, ok := value.(*itemsclient.ItemsClient)
	if !ok {
		t.Fatalf("resolved generated client has type %T", value)
	}
	return generated, stop
}

// Stopping the consumer application closes the registry its generated clients
// were built from. A client retained past the stop refuses every call with the
// registry's one stable code, and nothing reaches the provider — anonymous or
// credentialed, REST or Connect, unary or streamed.
func TestARetainedClientRefusesEveryCallAfterItsApplicationStopped(t *testing.T) {
	spectest.Proves(t, matrixFeature, "a-call-is-observable-and-ends-with-its-application", "a-retained-client-refuses-every-call-after-its-application-stopped")
	baseURL, wire := realConnectProvider(t)
	generated, stop := startStoppableConsumer(t, sampleBinding(baseURL))
	if _, err := generated.GetQuotes(t.Context(), itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "1"}}); err != nil {
		t.Fatalf("a call before the stop failed: %v", err)
	}
	stop()
	before := len(wire.snapshot())

	calls := map[string]func() error{
		"anonymous REST": func() error {
			_, err := generated.ListItems(t.Context(), itemsclient.ListItemsInput{
				Query: itemsclient.ListItemsQuery{Search: "et", Limit: 10},
			})
			return err
		},
		"api-key REST": func() error {
			_, err := generated.ListCredentialCheck(t.Context(), itemsclient.ListCredentialCheckInput{})
			return err
		},
		"api-key Connect": func() error {
			_, err := generated.GetQuotes(t.Context(), itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "1"}})
			return err
		},
		"Connect server stream": func() error {
			stream, err := generated.GetQuotesTicks(t.Context(), itemsclient.GetQuotesTicksInput{Path: itemsclient.GetQuotesTicksPath{Id: "1"}})
			if err != nil {
				return err
			}
			for range stream.Messages() {
				t.Error("a stream opened after the stop delivered a message")
			}
			return stream.Err()
		},
	}
	for name, call := range calls {
		if err := call(); !perrors.Is(err, client.CodeClientClosed) {
			t.Errorf("%s after the stop = %v, want %s", name, err, client.CodeClientClosed)
		}
	}
	if after := len(wire.snapshot()); after != before {
		t.Errorf("the provider saw %d requests from a stopped consumer", after-before)
	}
}

// A provider that goes away while a stream is open ends that stream with an
// error the consumer reads, instead of leaving it waiting on a dead socket.
func TestAStreamEndsWhenItsProviderGoesAway(t *testing.T) {
	serverPlugin := phttp.NewServerPlugin(phttp.ServerConfig{})
	serverPlugin.Use(service.IdentityResolver())
	apiPlugin := api.New(serverPlugin, service.ClientContract())
	service.Register(apiPlugin)
	if err := apiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewServer(serverPlugin.Handler())
	t.Cleanup(provider.Close)
	generated := boundClient(t, sampleBinding(provider.URL))

	stream, err := generated.GetItemsWatch(t.Context(), itemsclient.GetItemsWatchInput{
		Path:  itemsclient.GetItemsWatchPath{Id: "1"},
		Query: itemsclient.GetItemsWatchQuery{Follow: true},
	})
	if err != nil {
		t.Fatalf("open the followed stream: %v", err)
	}
	select {
	case _, ok := <-stream.Messages():
		if !ok {
			t.Fatalf("the followed stream ended before its first message: %v", stream.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the followed stream delivered nothing")
	}

	// The provider goes away: every connection it holds is cut.
	provider.CloseClientConnections()

	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for range stream.Messages() {
			// Messages already in flight may still arrive; the end is what counts.
		}
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream is still open after its provider went away")
	}
	if stream.Err() == nil {
		t.Fatal("a stream cut by its provider ended as a completion")
	}
	if code := perrors.GetError(stream.Err()); code == nil || code.Code() == "" {
		t.Fatalf("the stream ended with %T %v, want a typed client error", stream.Err(), stream.Err())
	}
}
