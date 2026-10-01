package itemsclient_test

// The lifecycle family of the TS→Go cells: a Go consumer application that
// stops refuses every call a retained generated client makes, and none of them
// reaches the TypeScript provider.

import (
	"context"
	"testing"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/client"
	perrors "go.putnami.dev/errors"
	itemsclient "go.putnami.dev/examples/ts-items-client"
	"go.putnami.dev/inject"
)

func TestARetainedGoClientRefusesEveryCallAfterItsApplicationStopped(t *testing.T) {
	providerURL := startForeignProvider(t)
	wireURL, wire := recordConnectWire(t, providerURL)

	module := app.NewModule("ts-items-consumer")
	module.Use(client.Services(client.ServicesOptions{
		ClientID: "cross-language-consumer",
		Services: map[string]client.ServiceBinding{serviceID: {
			URL:         wireURL,
			Credentials: map[string]client.CredentialBinding{"catalog-key": {Source: client.CredentialSourceStatic, Value: catalogAPIKey}},
		}},
	}))
	itemsclient.RegisterItemsClient(module)
	application := app.New("cross-language-consumer")
	application.Use(module)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- application.Start(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
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

	if _, err := generated.GetQuotes(t.Context(), itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "1"}}); err != nil {
		t.Fatalf("a call before the stop failed: %v", err)
	}
	stop()
	wire.reset()

	calls := map[string]func() error{
		"anonymous REST": func() error {
			_, err := generated.ListItems(t.Context(), itemsclient.ListItemsInput{
				Query:  itemsclient.ListItemsQuery{Search: "", Limit: 10},
				Header: itemsclient.ListItemsHeader{XCatalogTenant: "tenant-go"},
			})
			return err
		},
		"api-key Connect": func() error {
			_, err := generated.GetQuotes(t.Context(), itemsclient.GetQuotesInput{Path: itemsclient.GetQuotesPath{Id: "1"}})
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !perrors.Is(err, client.CodeClientClosed) {
			t.Errorf("%s after the stop = %v, want %s", name, err, client.CodeClientClosed)
		}
	}
	if requests := wire.snapshot(); len(requests) != 0 {
		t.Errorf("the provider saw %d requests from a stopped consumer: %+v", len(requests), requests)
	}
}
