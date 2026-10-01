// Package consumer shows the consumer side of the service-to-service sample: it
// imports the generated typed client (clients/go) and calls the Items provider
// through it. It lives in a separate package from the provider so the describe
// step — which compiles the provider to generate the client — never has to
// compile code that imports the to-be-generated client.
package consumer

import (
	"context"

	"go.putnami.dev/app"
	"go.putnami.dev/client"
	itemsclient "go.putnami.dev/examples/service-to-service/clients/go"
)

// Module registers the generated client through the framework-owned service
// binding. Production callers normally omit options and supply the same typed
// `clients` block through Putnami config; tests may pass an explicit binding.
func Module(options ...client.ServicesOptions) *app.Module {
	module := app.NewModule("items-consumer")
	module.Use(client.Services(options...))
	itemsclient.RegisterItemsClient(module)
	return module
}

// ListItems calls GET /items through the generated client. The query string is
// a typed input: the consumer never formats a URL.
func ListItems(ctx context.Context, items *itemsclient.ItemsClient, query itemsclient.ListItemsQuery) (*itemsclient.ItemList, error) {
	return items.ListItems(ctx, itemsclient.ListItemsInput{Query: query})
}

// FetchItem calls GET /items/{id} through the generated client and returns the
// fully-typed item. No URL strings, no manual JSON: the client owns the path
// substitution and response decoding.
func FetchItem(ctx context.Context, items *itemsclient.ItemsClient, id string) (*itemsclient.Item, error) {
	return items.GetItems(ctx, itemsclient.GetItemsInput{
		Path: itemsclient.GetItemsPath{Id: id},
	})
}

// CreateItem calls POST /items through the generated client with a typed body.
func CreateItem(ctx context.Context, items *itemsclient.ItemsClient, name string, price int64) (*itemsclient.Item, error) {
	return items.CreateItems(ctx, itemsclient.CreateItemsInput{
		Body: itemsclient.CreateItemBody{Name: name, Price: &price},
	})
}

// CheckCredential calls the operation the contract guards with the `catalog-key`
// API-key profile. The consumer sets no header: the binding injects it from the
// credential the provider declared.
func CheckCredential(ctx context.Context, items *itemsclient.ItemsClient) (*itemsclient.CredentialCheck, error) {
	return items.ListCredentialCheck(ctx, itemsclient.ListCredentialCheckInput{})
}

// CheckTenant calls the operation one alternative guards with two credentials
// at once: the `catalog-key` api key and the `tenant` named header. A binding
// that carries only one of them never dispatches.
func CheckTenant(ctx context.Context, items *itemsclient.ItemsClient) (*itemsclient.TenantCheck, error) {
	return items.ListTenantCheck(ctx, itemsclient.ListTenantCheckInput{})
}

// WhoAmI calls the operation guarded by the caller's own user identity. The
// consumer passes the context of its inbound request and writes no header: the
// binding opts into forwarding, and the runtime carries the inbound token.
func WhoAmI(ctx context.Context, items *itemsclient.ItemsClient) (*itemsclient.Caller, error) {
	return items.ListWhoami(ctx, itemsclient.ListWhoamiInput{})
}

// EchoBodyFidelity proves required zero values, nullable properties, and wide
// integers cross the generated JSON client without map or float conversion.
func EchoBodyFidelity(ctx context.Context, items *itemsclient.ItemsClient, body itemsclient.BodyFidelity) (*itemsclient.BodyFidelity, error) {
	return items.CreateBodyFidelity(ctx, itemsclient.CreateBodyFidelityInput{Body: body})
}

// EchoBodyValues calls the provider-declared root-array operation.
func EchoBodyValues(ctx context.Context, items *itemsclient.ItemsClient, body []int64) (*[]int64, error) {
	return items.CreateBodyValues(ctx, itemsclient.CreateBodyValuesInput{Body: body})
}

// EchoBodyCounter calls the provider-declared root-primitive operation.
func EchoBodyCounter(ctx context.Context, items *itemsclient.ItemsClient, body uint64) (*uint64, error) {
	return items.CreateBodyCounter(ctx, itemsclient.CreateBodyCounterInput{Body: body})
}

// EchoNullableBody sends an explicit JSON null when body is nil.
func EchoNullableBody(ctx context.Context, items *itemsclient.ItemsClient, body *string) (*string, error) {
	return items.CreateBodyNullable(ctx, itemsclient.CreateBodyNullableInput{Body: body})
}

// WatchItem opens the provider-declared server stream through the same binding
// as unary calls. The generated method owns path encoding, credentials, stream
// transport, bounds and typed terminal errors.
func WatchItem(ctx context.Context, items *itemsclient.ItemsClient, id string, follow bool) (*client.Stream[itemsclient.Item], error) {
	return items.GetItemsWatch(ctx, itemsclient.GetItemsWatchInput{
		Path:  itemsclient.GetItemsWatchPath{Id: id},
		Query: itemsclient.GetItemsWatchQuery{Follow: follow},
	})
}

// ReadItemHistory opens the provider-declared revision feed. The provider
// declares `websocket` before `sse` for this operation and declares it
// resumable; the consuming application says none of that and opens the stream
// exactly the way it opens the SSE one.
func ReadItemHistory(ctx context.Context, items *itemsclient.ItemsClient, id string) (*client.Stream[itemsclient.ItemRevision], error) {
	return items.GetItemsHistory(ctx, itemsclient.GetItemsHistoryInput{
		Path: itemsclient.GetItemsHistoryPath{Id: id},
	})
}

// FollowChanges opens the provider-declared change feed up to revision until.
// The provider declares a cursor continuation on it, so the generated method
// keeps one stream open across an instance change: it reopens after the last
// change this caller received, with a credential resolved at that moment. The
// consuming application says none of that — no reconnect loop, no cursor.
func FollowChanges(ctx context.Context, items *itemsclient.ItemsClient, until int64) (*client.Stream[itemsclient.ItemChange], error) {
	return items.ListItemsChanges(ctx, itemsclient.ListItemsChangesInput{
		Query: itemsclient.ListItemsChangesQuery{Until: until},
	})
}

// AdjustStock opens the provider-declared client stream through the same
// binding as every other call. The consumer sends deltas, ends its own
// direction with CloseSend, and reads the single declared result. It writes no
// header and no frame of its own: identity, credentials and the conversation
// state belong to the runtime.
func AdjustStock(ctx context.Context, items *itemsclient.ItemsClient, id string) (*client.RequestStream[itemsclient.StockDelta, itemsclient.StockTotal], error) {
	return items.GetItemsAdjust(ctx, itemsclient.GetItemsAdjustInput{
		Path: itemsclient.GetItemsAdjustPath{Id: id},
	})
}

// NegotiateStock opens the provider-declared bidirectional stream. Both
// directions run at once, and the declared result is the last value Recv
// returns before io.EOF.
func NegotiateStock(ctx context.Context, items *itemsclient.ItemsClient, id string) (*client.BidiStream[itemsclient.StockDelta, itemsclient.StockTotal], error) {
	return items.GetItemsNegotiate(ctx, itemsclient.GetItemsNegotiateInput{
		Path: itemsclient.GetItemsNegotiatePath{Id: id},
	})
}
