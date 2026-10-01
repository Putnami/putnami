// Package service defines the Items provider API: its types, handlers, and the
// route registration shared by the runnable binary and the client generator.
//
// It deliberately does NOT import the generated clients/go module. The describe
// step compiles this provider to discover its routes and generate the client, so
// importing the to-be-generated client here would be a build cycle. Consumers of
// the typed client live in a separate package (see ../consumer).
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/api"
	"go.putnami.dev/app"
	perrors "go.putnami.dev/errors"
	"go.putnami.dev/grpc"
	"go.putnami.dev/http"
	"go.putnami.dev/openapi"
	"go.putnami.dev/platform"
	"go.putnami.dev/proto"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/security"
)

// Client generation settings, shared by NewApp (the api.Clients describer) and
// the in-sync regeneration test (clientsync_test.go) so the committed clients/go
// always matches what `putnami build` regenerates.
//
// No module path is set, so the client is emitted as a package inside this
// module (clients/go) rather than a standalone module. That keeps go.work
// untouched — there is no second module to register — while the consumer imports
// it as go.putnami.dev/examples/service-to-service/clients/go. Set a module path
// (see api.GoClientOptions.ModulePath) when the client must be a separately
// publishable module; then register it in go.work and commit its go.mod.
const (
	ProjectName   = "go.putnami.dev/examples/service-to-service"
	FeatureID     = "items/manage"
	ClientPackage = "itemsclient"
	ClientName    = "ItemsClient"
	// TSClientPackage names the cross-language TypeScript client emitted from this
	// same OpenAPI spec by `putnami clientgen` (the neutral, scheduler-mediated
	// command runs the Bun emitter; this Go binary never shells out to it).
	TSClientPackage = "@example/go-items-client"
	// ProtoPackage is the protobuf package of the published descriptor, which
	// the bridge serves as the Connect protobuf codec.
	ProtoPackage = "items.v1"
)

// Item is a catalog item.
type Item struct {
	ID    string `json:"id" validate:"required"`
	Name  string `json:"name" validate:"required"`
	Price int64  `json:"price"`
}

// ItemList is the list-items response envelope.
type ItemList struct {
	Items []Item `json:"items"`
}

// GetItemParams are the path parameters for fetching one item.
type GetItemParams struct {
	ID string `json:"id" validate:"required"`
}

// CreateItemBody is the create-item request body.
type CreateItemBody struct {
	Name  string `json:"name" validate:"required"`
	Price int64  `json:"price"`
}

// BodyFidelity exercises JSON values whose wire semantics are commonly lost by
// decoding through map[string]any. Every required field is present even when
// its value is false, zero, empty, or null.
type BodyFidelity struct {
	Enabled  bool    `json:"enabled" validate:"required"`
	Count    int64   `json:"count" validate:"required"`
	Label    string  `json:"label" validate:"required"`
	Signed   int64   `json:"signed" validate:"required,min=9007199254740993,max=9223372036854775807"`
	Unsigned uint64  `json:"unsigned" validate:"required,min=9007199254740993,max=18446744073709551615"`
	Nullable *string `json:"nullable" validate:"required"`
}

// AuditRecord carries values the provider stores without interpreting them.
// Each Go form publishes one closed declaration (clientcontract ADR 0008):
// json.RawMessage and the empty interface are `x-putnami-json: any`, and a map
// of the empty interface is an object with free-form values. Payload and Note
// keep the caller's exact bytes; Value and Attributes go through Go's own
// encoding/json, so a number is a float64 and an object comes back with its
// keys sorted.
type AuditRecord struct {
	Attributes map[string]any  `json:"attributes" validate:"required"`
	Payload    json.RawMessage `json:"payload" validate:"required"`
	Value      any             `json:"value" validate:"required"`
	// Note is optional: an explicit null and an absent member are two values.
	Note json.RawMessage `json:"note,omitempty"`
}

// ListItemsQuery declares the query string of GET /items. Declaring it here is
// what makes the generated clients carry a typed `Query` input instead of a
// hand-built URL: no consumer ever formats a query string.
type ListItemsQuery struct {
	// Search keeps items whose name contains it. Declared required: the
	// TypeScript emitter renders an optional parameter as
	// `input.query?["search"]`, which does not parse.
	Search string `json:"search" validate:"required"`
	// Declared int64: the query coercion in go.putnami.dev/schema covers int,
	// int64, float64 and bool only, so a narrower Go width would reach the
	// handler as an unconverted string.
	Limit int64 `json:"limit" validate:"required,min=1,max=100"`
}

// CredentialCheck reports whether the provider observed the credential the
// contract declares for the operation. The value itself is never echoed.
type CredentialCheck struct {
	Profile   string `json:"profile" validate:"required"`
	Header    string `json:"header" validate:"required"`
	Presented bool   `json:"presented" validate:"required"`
}

// BlobMaxBytes bounds every raw octet payload this provider carries. It is
// small on purpose: a sample must be able to prove the over-bound refusal
// without moving megabytes through a loopback socket.
const BlobMaxBytes = 4096

// BlobMediaType is the wire media type of the sample's raw octet payloads.
const BlobMediaType = "application/octet-stream"

// BlobParams are the path parameters for reading one stored blob.
type BlobParams struct {
	ID string `json:"id" validate:"required"`
}

// storedBlobs is the tiny catalog of blobs the provider can hand back. The
// octets are deliberately not valid UTF-8 and not valid JSON: a pipeline that
// re-encoded them as text or as a JSON string would corrupt them, and the
// cross-language tests compare them byte for byte.
var storedBlobs = map[string][]byte{
	"1": {0x00, 0xff, 0xfe, 0x80, 0x7f, 0x22, 0x5c, 0x0a},
	// An empty payload is a payload: zero octets must survive the round trip
	// rather than being read as "no body".
	"2": {},
}

// CatalogKeyHeader is the request header the `catalog-key` credential profile
// injects. The provider names it once; the contract carries it to both
// generated clients, and no consumer code ever writes it.
const CatalogKeyHeader = "X-Catalog-Key"

// CatalogAPIKey is the sample's shared API key and WorkloadToken its service
// bearer token. Harnesses hand them to the binding; no consumer code ever
// builds a header from them. A real provider would verify a signature and look
// the key up in its own secret store.
const (
	CatalogAPIKey = "sample-catalog-key"
	WorkloadToken = "sample-workload-token"
)

// items is a tiny in-memory catalog backing the provider.
var items = []Item{
	{ID: "1", Name: "Widget", Price: 100},
	{ID: "2", Name: "Gadget", Price: 250},
}

func listItems(ctx *http.EndpointContext) *http.Response {
	query, err := http.QueryAs[ListItemsQuery](ctx)
	if err != nil {
		return http.ErrorResponse(perrors.BadRequest("invalid item query"))
	}
	selected := make([]Item, 0, len(items))
	for _, it := range items {
		if !strings.Contains(it.Name, query.Search) {
			continue
		}
		if len(selected) == int(query.Limit) {
			break
		}
		selected = append(selected, it)
	}
	return http.JSON(ItemList{Items: selected})
}

// checkCatalogKey reports the credential the binding injected. The consumer
// declares no header: the `catalog-key` profile of the client contract does.
func checkCatalogKey(ctx *http.EndpointContext) *http.Response {
	presented := ctx.Header(CatalogKeyHeader) == CatalogAPIKey
	if !presented {
		return http.ErrorResponse(perrors.Unauthorized("catalog key missing or unknown"))
	}
	return http.JSON(CredentialCheck{Profile: "catalog-key", Header: CatalogKeyHeader, Presented: true})
}

func getItem(ctx *http.EndpointContext) *http.Response {
	params, err := http.ParamsAs[GetItemParams](ctx)
	if err != nil {
		return http.ErrorResponse(perrors.BadRequest("invalid item id"))
	}
	// One id the provider fails on with a status it never declared. A consumer
	// must read that as the unknown remote failure it is — never as one of the
	// declared errors, and never with the provider's own prose.
	if params.ID == UndeclaredFailureID {
		return http.JSONStatus(503, map[string]string{"error": "catalog replica lag 42s on shard 7"})
	}
	for _, it := range items {
		if it.ID == params.ID {
			return http.JSON(it)
		}
	}
	return http.ErrorResponse(perrors.NotFound("item not found"))
}

func createItem(ctx *http.EndpointContext) *http.Response {
	body, err := http.BodyAs[CreateItemBody](ctx)
	if err != nil {
		return http.ErrorResponse(perrors.BadRequest("invalid item"))
	}
	created := Item{ID: "3", Name: body.Name, Price: body.Price}
	// Every creation is a revision of the change feed: the one mutation this
	// provider serves is what moves the feed a consumer follows.
	Changes.Append(created.ID, created.Name)
	return http.JSONStatus(201, created)
}

func echoBodyFidelity(ctx *http.EndpointContext) *http.Response {
	body, err := http.BodyAs[BodyFidelity](ctx)
	if err != nil {
		return http.ErrorResponse(perrors.BadRequest("invalid fidelity body"))
	}
	return http.JSON(body)
}

func echoBodyValues(ctx *http.EndpointContext) *http.Response {
	body, err := http.BodyAs[[]int64](ctx)
	if err != nil {
		return http.ErrorResponse(perrors.BadRequest("invalid values body"))
	}
	return http.JSON(body)
}

func echoBodyCounter(ctx *http.EndpointContext) *http.Response {
	body, err := http.BodyAs[uint64](ctx)
	if err != nil {
		return http.ErrorResponse(perrors.BadRequest("invalid counter body"))
	}
	return http.JSON(body)
}

func echoNullableBody(ctx *http.EndpointContext) *http.Response {
	body, err := http.BodyAs[*string](ctx)
	if err != nil {
		return http.ErrorResponse(perrors.BadRequest("invalid nullable body"))
	}
	return http.JSON(body)
}

func echoAuditRecord(ctx *http.EndpointContext) *http.Response {
	body, err := http.BodyAs[AuditRecord](ctx)
	if err != nil {
		return http.ErrorResponse(perrors.BadRequest("invalid audit record"))
	}
	return http.JSON(body)
}

// echoBlob hands the request octets straight back. The endpoint pipeline has
// already applied the two declared facts — the media type and the bound — so
// the handler never validates, decodes or re-encodes anything.
func echoBlob(ctx *http.EndpointContext) *http.Response {
	body, err := api.BinaryBody(ctx)
	if err != nil {
		return http.ErrorResponse(perrors.BadRequest("invalid blob body"))
	}
	return api.BinaryResponse(200, BlobMediaType, body)
}

// readBlob answers one stored blob, or the declared not_found error. Its
// credential comes from the `catalog-key` profile the contract declares: the
// consumer writes no header.
func readBlob(ctx *http.EndpointContext) *http.Response {
	params, err := http.ParamsAs[BlobParams](ctx)
	if err != nil {
		return http.ErrorResponse(perrors.BadRequest("invalid blob id"))
	}
	blob, ok := storedBlobs[params.ID]
	if !ok {
		return http.ErrorResponse(perrors.NotFound("blob not found"))
	}
	return api.BinaryResponse(200, BlobMediaType, blob)
}

// WatchItemQuery declares the query string of GET /items/{id}/watch. Declaring
// `follow` is what makes a stream long-lived on purpose: without it a consumer
// could only ever prove the one-shot path, never cancellation or shutdown.
type WatchItemQuery struct {
	// Follow keeps the stream open, re-sending the item until the consumer
	// leaves. Declared required for the same emitter reason as ListItemsQuery.
	Follow bool `json:"follow" validate:"required"`
}

// watchFollowInterval is the gap between two updates in follow mode. It is
// short enough that a test observes several messages without waiting, and the
// stream ends only when the consumer or the provider does.
const watchFollowInterval = 20 * time.Millisecond

func watchItem(ctx *api.ServerStreamContext[Item]) error {
	id := ctx.Param("id")
	found := -1
	for index, item := range items {
		if item.ID == id {
			found = index
			break
		}
	}
	if found < 0 {
		return perrors.NotFound("item not found")
	}
	if err := ctx.Send(items[found]); err != nil {
		return err
	}
	if ctx.Query("follow") != "true" {
		return nil
	}
	done := ctx.Context.Context().Done()
	for {
		select {
		case <-done:
			return nil
		case <-time.After(watchFollowInterval):
			if err := ctx.Send(items[found]); err != nil {
				return err
			}
		}
	}
}

// ItemRevision is one message of the item history feed. The revision is the
// provider's own position in the feed, so a consumer that reads two of them can
// see for itself that nothing was skipped and nothing arrived twice.
type ItemRevision struct {
	ID       string `json:"id" validate:"required"`
	Revision string `json:"revision" validate:"required"`
}

// historyFeedLength is how many revisions one connection of the history feed
// delivers before it completes. Three is enough to observe a sequence and short
// enough that the four network couples stay fast.
const historyFeedLength = 3

// itemHistory is the server stream carried by the published WebSocket wire.
//
// The endpoint declares `websocket` before `sse` and declares itself resumable,
// so this handler is told the position a continuation resumes after and numbers
// its revisions from there. A fresh stream continues after nothing, which is
// revision 0.
func itemHistory(ctx *api.ServerStreamContext[ItemRevision]) error {
	id := ctx.Param("id")
	if findItem(id) < 0 {
		return perrors.NotFound("item not found")
	}
	from, _ := api.StreamResumeFrom(ctx.Context.Context())
	for offset := uint64(1); offset <= historyFeedLength; offset++ {
		if err := ctx.Send(ItemRevision{ID: id, Revision: strconv.FormatUint(from+offset, 10)}); err != nil {
			return err
		}
	}
	return nil
}

// ItemChange is one revision of the catalog change feed. Cursor is the
// provider's own opaque position after this change: a consumer hands it back
// unread on a continuation and never parses it. Revision is the sequence a
// reader compares to see for itself that nothing was skipped and nothing
// arrived twice.
type ItemChange struct {
	Cursor   string `json:"cursor" validate:"required"`
	Revision int64  `json:"revision" validate:"required"`
	ID       string `json:"id" validate:"required"`
	Name     string `json:"name" validate:"required"`
}

// ChangesQuery declares the query string of GET /items/changes.
type ChangesQuery struct {
	// Cursor is the position the feed continues after: the cursor of the last
	// change the consumer received. Absent, the feed starts at its first
	// retained change. The generated clients send it on every continuation of
	// a broken stream; no consumer writes it.
	Cursor string `json:"cursor"`
	// Until is the revision the feed completes after. The feed waits for it: a
	// revision that does not exist yet keeps the stream open until a creation
	// appends it, which is what lets a consumer observe a continuation.
	Until int64 `json:"until" validate:"required,min=1"`
}

// ChangeLog is the durable change feed of the catalog: every revision it ever
// issued, in order, shared by every provider instance of one process. It is
// the only state a continuation relies on. An instance keeps nothing about a
// stream it served, so a continuation that lands on another instance is placed
// by the cursor alone, and a fresh process seeds the same revisions from the
// same catalog.
type ChangeLog struct {
	mu      sync.Mutex
	changes []ItemChange
	changed chan struct{}
}

// Changes is the change feed every instance of this provider serves.
var Changes = seedChangeLog(items)

func seedChangeLog(catalog []Item) *ChangeLog {
	log := &ChangeLog{changed: make(chan struct{})}
	for _, item := range catalog {
		log.Append(item.ID, item.Name)
	}
	return log
}

// Append records one change and wakes every stream waiting for it.
func (log *ChangeLog) Append(id, name string) ItemChange {
	log.mu.Lock()
	defer log.mu.Unlock()
	revision := int64(len(log.changes) + 1)
	change := ItemChange{Cursor: fmt.Sprintf("r%d", revision), Revision: revision, ID: id, Name: name}
	log.changes = append(log.changes, change)
	close(log.changed)
	log.changed = make(chan struct{})
	return change
}

// Head is the revision of the latest change. A harness reads it to state
// where a feed it opens will complete.
func (log *ChangeLog) Head() int64 {
	log.mu.Lock()
	defer log.mu.Unlock()
	return int64(len(log.changes))
}

// position resolves a cursor this log issued to the number of changes before
// the one that follows it. The empty cursor is the start; a cursor the log
// never issued is refused.
func (log *ChangeLog) position(cursor string) (int64, bool) {
	if cursor == "" {
		return 0, true
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	revision, err := strconv.ParseInt(strings.TrimPrefix(cursor, "r"), 10, 64)
	if !strings.HasPrefix(cursor, "r") || err != nil || revision < 1 || revision > int64(len(log.changes)) {
		return 0, false
	}
	return revision, true
}

// next waits for the change after position after. It reports false when ctx
// ended first: the stream was abandoned by its consumer or drained by its
// server.
func (log *ChangeLog) next(ctx context.Context, after int64) (ItemChange, bool) {
	for {
		log.mu.Lock()
		if after < int64(len(log.changes)) {
			change := log.changes[after]
			log.mu.Unlock()
			return change, true
		}
		changed := log.changed
		log.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ItemChange{}, false
		}
	}
}

// itemChanges is the cursor-continued server stream. It continues exclusively
// after the position it is given — the provider's obligation a continuation
// relies on — sends every change up to `until`, and completes. A position this
// log never issued is the declared not_found: the feed never restarts from the
// beginning on a cursor it does not recognize.
func itemChanges(ctx *api.ServerStreamContext[ItemChange]) error {
	until, err := strconv.ParseInt(ctx.Query("until"), 10, 64)
	if err != nil || until < 1 {
		return perrors.BadRequest("invalid until revision")
	}
	after, known := Changes.position(ctx.Query("cursor"))
	if !known {
		return perrors.NotFound("the position is not in the retained change log")
	}
	for after < until {
		change, ok := Changes.next(ctx.Context.Context(), after)
		if !ok {
			// Abandoned or drained: the wire writes no terminal, and a consumer
			// that reconnects continues after the last change it received.
			return nil
		}
		if err := ctx.Send(change); err != nil {
			return err
		}
		after = change.Revision
	}
	return nil
}

// changesStreamResilience declares the consumer half of the continuation
// agreement — this operation reopens a broken stream — beside the bounds a
// reader keeps. It declares no idle timeout on purpose: a change feed is quiet
// for as long as the catalog is.
func changesStreamResilience() *clientcontract.ResiliencePolicy {
	reconnect, buffered := true, 4
	frameBytes := int64(16384)
	return &clientcontract.ResiliencePolicy{Stream: &clientcontract.StreamPolicy{
		Reconnect:           &reconnect,
		MaxFrameBytes:       &frameBytes,
		MaxBufferedMessages: &buffered,
	}}
}

// StockDelta is one message a consumer sends on a stock conversation. Declaring
// it as the stream's input schema is what gives the generated clients a typed
// Send: a consumer that ships the wrong shape fails to compile, and the runtime
// validates the frame against the same declaration before it leaves.
type StockDelta struct {
	Delta int64 `json:"delta" validate:"required,min=-1000000,max=1000000"`
}

// StockTotal is what the provider answers with: the running total on a
// bidirectional conversation, and the single declared result of a client
// stream. One schema for both directions keeps the two modes comparable.
type StockTotal struct {
	ID      string `json:"id" validate:"required"`
	Applied int64  `json:"applied" validate:"required"`
	Total   int64  `json:"total" validate:"required"`
}

// stockStreamResilience bounds every stock conversation. The values are
// declared, not defaulted: the generated clients read the same numbers from the
// published contract, so the two ends agree on when a stream is idle, how large
// a reassembled message may be, and how deep the inbound queue runs before the
// provider applies backpressure.
func stockStreamResilience() *clientcontract.ResiliencePolicy {
	handshakeMs, idleMs, heartbeatMs, buffered := 2000, 5000, 250, 4
	frameBytes := int64(16384)
	return &clientcontract.ResiliencePolicy{Stream: &clientcontract.StreamPolicy{
		HandshakeTimeoutMs:  &handshakeMs,
		IdleTimeoutMs:       &idleMs,
		HeartbeatMs:         &heartbeatMs,
		MaxFrameBytes:       &frameBytes,
		MaxBufferedMessages: &buffered,
	}}
}

// historyStreamResilience declares the same bounds as the stock conversations
// and adds the consumer half of the resume agreement: this operation may ask
// the provider to continue a broken stream. The provider half is Resume on the
// endpoint; the published contract refuses one without the other.
func historyStreamResilience() *clientcontract.ResiliencePolicy {
	policy := stockStreamResilience()
	reconnect := true
	policy.Stream.Reconnect = &reconnect
	return policy
}

// stockStreamSecurity is the same pair of alternatives the watch stream
// declares, in the same order: a consumer that can mint a service token uses
// it, and one that can only carry a static key falls through to the second.
func stockStreamSecurity() clientcontract.Security {
	return clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
		{AllOf: []clientcontract.SecurityRequirement{{Profile: "workload"}}},
		{AllOf: []clientcontract.SecurityRequirement{{Profile: "catalog-key"}}},
	}}
}

// findItem returns the index of an item, or -1.
func findItem(id string) int {
	for index, item := range items {
		if item.ID == id {
			return index
		}
	}
	return -1
}

// adjustStock is the client stream: the consumer sends deltas, ends its own
// direction, and reads one declared result. The refusal for an unknown item is
// raised before the first message is read, so a consumer never sends into a
// conversation the provider has already declined.
func adjustStock(ctx *api.ClientStreamContext[StockDelta, StockTotal]) error {
	id := ctx.Param("id")
	if findItem(id) < 0 {
		return perrors.NotFound("item not found")
	}
	var applied, total int64
	for delta := range ctx.Messages() {
		applied++
		total += delta.Delta
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx.Result(StockTotal{ID: id, Applied: applied, Total: total})
	return nil
}

// negotiateStock is the bidirectional conversation: every delta the consumer
// sends is answered with the running total, and the last value the provider
// sends is the declared result. Both directions run at once, so a consumer that
// half-closes still reads what is already in flight.
func negotiateStock(ctx *api.BidiStreamContext[StockDelta, StockTotal]) error {
	id := ctx.Param("id")
	if findItem(id) < 0 {
		return perrors.NotFound("item not found")
	}
	var applied, total int64
	for delta := range ctx.Messages() {
		applied++
		total += delta.Delta
		if err := ctx.Send(StockTotal{ID: id, Applied: applied, Total: total}); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx.Result(StockTotal{ID: id, Applied: applied, Total: total})
	return nil
}

// Quote is one price quote, the payload of the two operations this provider
// declares Connect-only. Its members carry one value of every shape the JSON and
// protobuf encodings must agree on: an unsigned 64-bit integer beyond JavaScript
// precision, a negative 32-bit integer, raw bytes, a list and a map. The member
// names are single lowercase words: the descriptor joins to the published schema
// by jsonName, and a name the proto identifier grammar would rewrite has no join.
type Quote struct {
	ID          string            `json:"id" validate:"required"`
	Units       uint64            `json:"units" validate:"required"`
	Offset      int32             `json:"offset" validate:"required"`
	Fingerprint []byte            `json:"fingerprint" validate:"required"`
	Tags        []string          `json:"tags" validate:"required"`
	Labels      map[string]string `json:"labels" validate:"required"`
}

// QuoteTick is one message of the Connect server stream. The sequence is the
// provider's own position, so a consumer sees for itself that nothing was
// skipped and nothing arrived twice.
type QuoteTick struct {
	ID       string `json:"id" validate:"required"`
	Sequence uint64 `json:"sequence" validate:"required"`
}

// quoteTicks is how many ticks one quote stream delivers before it completes.
const quoteTicks = 3

// quotes backs the Connect operations. The quote holds the widest value each
// member can carry, so a codec that narrowed any of them changes the answer the
// four couples compare byte for byte.
var quotes = map[string]Quote{
	"1": {
		ID: "1", Units: 18446744073709551615, Offset: -7,
		Fingerprint: []byte{0x00, 0xff, 0x80, 0x22},
		Tags:        []string{"a", "b"},
		Labels:      map[string]string{"k": "v"},
	},
}

func getQuote(ctx *http.EndpointContext) *http.Response {
	params, err := http.ParamsAs[GetItemParams](ctx)
	if err != nil {
		return http.ErrorResponse(perrors.BadRequest("invalid quote id"))
	}
	quote, ok := quotes[params.ID]
	if !ok {
		return http.ErrorResponse(perrors.NotFound("quote not found"))
	}
	return http.JSON(quote)
}

func watchQuote(ctx *api.ServerStreamContext[QuoteTick]) error {
	id := ctx.Param("id")
	if _, ok := quotes[id]; !ok {
		return perrors.NotFound("quote not found")
	}
	for sequence := uint64(1); sequence <= quoteTicks; sequence++ {
		if err := ctx.Send(QuoteTick{ID: id, Sequence: sequence}); err != nil {
			return err
		}
	}
	return nil
}

// UndeclaredFailureID is the item id this provider answers with a status it
// never declared, so a consumer can be checked against an unknown remote error.
const UndeclaredFailureID = "boom"

// TenantHeader is the header the `tenant` named credential carries, and
// SampleTenant the one tenant this provider serves.
const (
	TenantHeader = "X-Tenant-Id"
	SampleTenant = "tenant-a"
	// CallerScope is what a forwarded user carries, and what /whoami requires.
	CallerScope = "catalog.caller"
	// TenantScope is what a caller carries only when the api key and the tenant
	// arrived together, and what /tenant-check requires.
	TenantScope = "catalog.tenant"
	// UserToken is a user bearer a consumer forwards from its own inbound
	// request, and UserSubject the subject this provider names it with.
	UserToken   = "user-token-alice"
	UserSubject = "alice"
)

// userSubjects maps the user tokens the sample knows to the subject each names.
// A real provider would verify a signed token; the sample keeps the shape.
var userSubjects = map[string]string{UserToken: UserSubject}

// TenantCheck reports that both credentials of one alternative arrived. The
// values themselves are never echoed.
type TenantCheck struct {
	Key    bool `json:"key" validate:"required"`
	Tenant bool `json:"tenant" validate:"required"`
}

// Caller names the user a consumer forwarded, never the token it carried.
type Caller struct {
	Subject string `json:"subject" validate:"required"`
}

func checkTenant(ctx *http.EndpointContext) *http.Response {
	// The identity resolver carries this scope only when the api key and the
	// tenant arrived together, so reaching the reply is the proof itself.
	if ctx.User == nil || !ctx.User.HasScope(TenantScope) {
		return http.ErrorResponse(perrors.Unauthorized("the catalog key and the tenant are both required"))
	}
	return http.JSON(TenantCheck{Key: true, Tenant: true})
}

func whoAmI(ctx *http.EndpointContext) *http.Response {
	// The identity resolver named the user from the token the consumer
	// forwarded; the token itself never reaches this handler or the response.
	if ctx.User == nil || !ctx.User.HasScope(CallerScope) {
		return http.ErrorResponse(perrors.Unauthorized("unknown caller"))
	}
	return http.JSON(Caller{Subject: ctx.User.Subject})
}

// IdempotencyKeyHeader carries the stable request identity of the create
// operation. The runtime mints one key per call and repeats it on every attempt,
// so a provider can recognize a repeat rather than create a second item.
const IdempotencyKeyHeader = "X-Idempotency-Key"

// createItemResilience is the bounded request policy the create operation
// declares. The values are small on purpose: a sample must be able to prove a
// budget and an open circuit without holding a socket for seconds.
func createItemResilience() *clientcontract.ResiliencePolicy {
	timeout := 2000
	attemptTimeout := 250
	maxAttempts := 3
	// Two consecutive failed calls stop the traffic, and the circuit stays open
	// for a minute: a consumer that keeps calling a provider that is down turns
	// one outage into two.
	failureThreshold := 2
	resetTimeout := 60000
	return &clientcontract.ResiliencePolicy{
		TimeoutMs:        &timeout,
		AttemptTimeoutMs: &attemptTimeout,
		Retry: &clientcontract.RetryPolicy{
			MaxAttempts: &maxAttempts,
			Statuses:    []int{503},
		},
		Circuit: &clientcontract.CircuitPolicy{
			FailureThreshold: &failureThreshold,
			ResetTimeoutMs:   &resetTimeout,
		},
	}
}

// catalogKeySecurity requires the `catalog-key` API key and nothing else.
func catalogKeySecurity() clientcontract.Security {
	return clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
		AllOf: []clientcontract.SecurityRequirement{{Profile: "catalog-key"}},
	}}}
}

// ConnectPlugins returns the two plugins that serve the Connect wire, in the
// order the lifecycle configures them: the proto plugin publishes the
// descriptor and each route's method identity, then the bridge mounts those
// identities and publishes the encodings it serves. NewApp, the in-sync test and
// the harnesses share it, so all three publish one contract.
func ConnectPlugins(apiPlugin *api.Plugin, server *http.ServerPlugin) (*proto.Plugin, *grpc.ApiBridge) {
	return proto.NewPlugin(proto.PluginOptions{PackageName: ProtoPackage}).From(apiPlugin),
		grpc.NewApiBridge(apiPlugin, server, grpc.WithPackage(ProtoPackage))
}

// IdentityResolver turns the credentials the client contract declares into the
// caller identity the secured operations check. Both the runnable binary and
// the integration harnesses install it, so a cross-language consumer talking to
// this provider over a socket authenticates exactly like an in-process one.
//
// A real provider would validate a signed token and look the API key up in its
// own secret store. The sample keeps the shapes and skips the cryptography.
func IdentityResolver() http.Middleware {
	return security.IdentityResolver(func(ctx *http.Context) *http.Claims {
		// A user token a consumer forwarded names that user; the workload token
		// and the api key name the calling workload.
		if subject, known := userSubjects[strings.TrimPrefix(ctx.Header("Authorization"), "Bearer ")]; known {
			return &http.Claims{Subject: subject, ClientID: ctx.Header("X-Client-Id"), Scopes: []string{CallerScope}}
		}
		bearer := ctx.Header("Authorization") == "Bearer "+WorkloadToken
		apiKey := ctx.Header(CatalogKeyHeader) == CatalogAPIKey
		// The api key and the tenant together carry more than the key alone.
		if apiKey && ctx.Header(TenantHeader) == SampleTenant {
			return &http.Claims{
				Subject:  "sample-workload",
				ClientID: ctx.Header("X-Client-Id"),
				Scopes:   []string{TenantScope},
			}
		}
		if !bearer && !apiKey {
			return nil
		}
		return &http.Claims{Subject: "sample-workload", ClientID: ctx.Header("X-Client-Id")}
	})
}

// ClientContract is the provider-owned identity carried by every generated
// client. It contains credential profiles only; credential values live in the
// consumer's client.Services binding.
func ClientContract() api.Option {
	return api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "items", Audience: "urn:putnami:items"},
		Credentials: map[string]clientcontract.CredentialProfile{
			"workload": {Kind: clientcontract.CredentialServiceToken},
			// The API key is a request header the runtime injects from the
			// binding. It is declared here once so both generated clients know
			// the header name without any consumer ever writing it.
			"catalog-key": {Kind: clientcontract.CredentialAPIKey, Header: CatalogKeyHeader},
			// A named secondary credential: a value the binding injects under a
			// header the provider names, required together with the api key.
			"tenant": {Kind: clientcontract.CredentialNamedHeader, Header: TenantHeader},
			// The caller's own user token, forwarded from the consumer's inbound
			// request. The binding opts in; it is never cached nor minted.
			"user": {Kind: clientcontract.CredentialForwardedUserToken},
		},
	})
}

// Register declares the Items endpoints on the api plugin. Shared by NewApp and
// the offline client generator so both see exactly the same route set.
func Register(apiPlugin *api.Plugin) {
	apiPlugin.Register(api.Endpoint("GET", "/items").
		Description("List all items").
		Query(api.Type[ListItemsQuery]()).
		Returns(api.Type[ItemList]()).
		Handle(listItems))

	apiPlugin.Register(api.Endpoint("GET", "/credential-check").
		Description("Report the credential the caller's binding injected").
		Returns(api.Type[CredentialCheck]()).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		Client(api.ClientOperationOptions{Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
			AllOf: []clientcontract.SecurityRequirement{{Profile: "catalog-key"}},
		}}}}).
		MayThrow(perrors.CodeUnauthorized).
		Handle(checkCatalogKey))

	apiPlugin.Register(api.Endpoint("GET", "/items/{id}").
		Description("Get a single item by id").
		Params(api.Type[GetItemParams]()).
		Returns(api.Type[Item]()).
		MayThrow(perrors.CodeNotFound).
		Handle(getItem))

	// The one operation that declares a request policy: creating an item is not
	// safe to repeat blindly, so the provider states the identity header that
	// makes a repeat recognizable, the attempts it accepts, the statuses worth
	// repeating, the budget a call may not exceed, and the consecutive failures
	// after which a consumer must stop calling. A generated client reads all of
	// it from the contract; no consumer writes a retry loop.
	apiPlugin.Register(api.Endpoint("POST", "/items").
		Description("Create an item").
		Body(api.Type[CreateItemBody]()).
		ReturnsStatus(201, "Created", api.Type[Item]()).
		Client(api.ClientOperationOptions{
			Idempotency: &clientcontract.Idempotency{
				Kind:      clientcontract.IdempotencyIdempotent,
				KeyHeader: IdempotencyKeyHeader,
			},
			Resilience: createItemResilience(),
		}).
		Handle(createItem))

	apiPlugin.Register(api.Endpoint("POST", "/body-fidelity").
		Description("Echo exact JSON object values").
		Body(api.Type[BodyFidelity]()).
		Returns(api.Type[BodyFidelity]()).
		Handle(echoBodyFidelity))

	apiPlugin.Register(api.Endpoint("POST", "/body-values").
		Description("Echo an exact root JSON array").
		Body(api.Type[[]int64]()).
		Returns(api.Type[[]int64]()).
		Handle(echoBodyValues))

	apiPlugin.Register(api.Endpoint("POST", "/body-counter").
		Description("Echo an exact root JSON integer").
		Body(api.Type[uint64]()).
		Returns(api.Type[uint64]()).
		Handle(echoBodyCounter))

	apiPlugin.Register(api.Endpoint("POST", "/body-nullable").
		Description("Echo a nullable root JSON value").
		Body(api.Type[*string]()).
		Returns(api.Type[*string]()).
		Handle(echoNullableBody))

	// Opaque JSON: the record carries values the provider does not interpret.
	// It has no lossless protobuf form, so the route keeps REST and declares no
	// Connect transport even though the bridge is mounted.
	apiPlugin.Register(api.Endpoint("POST", "/audit").
		Description("Echo a record of opaque JSON values").
		Body(api.Type[AuditRecord]()).
		Returns(api.Type[AuditRecord]()).
		Handle(echoAuditRecord))

	apiPlugin.Register(api.Endpoint("POST", "/blobs/echo").
		Description("Echo raw octets unchanged").
		Body(api.Binary(BlobMediaType, BlobMaxBytes)).
		Returns(api.Binary(BlobMediaType, BlobMaxBytes)).
		Handle(echoBlob))

	apiPlugin.Register(api.Endpoint("GET", "/blobs/{id}").
		Description("Read one stored blob verbatim").
		Params(api.Type[BlobParams]()).
		Returns(api.Binary(BlobMediaType, BlobMaxBytes)).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		Client(api.ClientOperationOptions{Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
			AllOf: []clientcontract.SecurityRequirement{{Profile: "catalog-key"}},
		}}}}).
		MayThrow(perrors.CodeNotFound).
		Handle(readBlob))

	apiPlugin.Register(api.Endpoint("GET", "/items/{id}/watch").
		Description("Watch a single item").
		Params(api.Type[GetItemParams]()).
		Query(api.Type[WatchItemQuery]()).
		Returns(api.StreamOf[Item]()).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		// Two alternatives, in declared order: a consumer that can mint a service
		// token uses it, and one that can only carry a static key falls through
		// to the second. The stream is the operation where that matters — a
		// TypeScript consumer cannot bind a service token from configuration
		// alone.
		Client(api.ClientOperationOptions{Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{
			{AllOf: []clientcontract.SecurityRequirement{{Profile: "workload"}}},
			{AllOf: []clientcontract.SecurityRequirement{{Profile: "catalog-key"}}},
		}}}).
		MayThrow(perrors.CodeNotFound).
		Handle(api.ServerStream(watchItem)))

	// The same server-stream shape as /items/{id}/watch, declared the other way
	// round: WebSocket first, SSE second, and resumable. The declaration is the
	// only difference — the handler, the generated method and the consuming
	// application are the same shape they would be on SSE.
	apiPlugin.Register(api.Endpoint("GET", "/items/{id}/history").
		Description("Read the revision feed of a single item").
		Params(api.Type[GetItemParams]()).
		Returns(api.StreamOf[ItemRevision]()).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		Use(IdentityResolver()).
		Client(api.ClientOperationOptions{
			Security:   stockStreamSecurity(),
			Resilience: historyStreamResilience(),
			Transports: []clientcontract.TransportProtocol{
				clientcontract.TransportWebSocket, clientcontract.TransportSSE,
			},
			Resume: true,
		}).
		MayThrow(perrors.CodeNotFound).
		Handle(api.ServerStream(itemHistory)))

	// The catalog change feed: an SSE server stream that declares a cursor
	// continuation. Every change carries the provider's position after it, the
	// declaration names the output field that is and the query parameter that
	// receives it, and the consumer half — reconnect — sits beside it. A
	// generated client whose connection breaks reopens after the last change
	// its caller received, on whichever instance answers; no consumer writes a
	// reconnect loop, and no consumer reads the cursor. The route also speaks
	// the negotiated SSE wire, so a consumer reads an explicit completion and
	// never mistakes a cut connection for the end of the feed.
	apiPlugin.Register(api.Endpoint("GET", "/items/changes").
		Description("Follow the catalog change feed after a position").
		Query(api.Type[ChangesQuery]()).
		Returns(api.StreamOf[ItemChange]()).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		Client(api.ClientOperationOptions{
			Security:        stockStreamSecurity(),
			Resilience:      changesStreamResilience(),
			Transports:      []clientcontract.TransportProtocol{clientcontract.TransportSSE},
			SSEContinuation: api.SSECursorContinuation("cursor", "cursor"),
		}).
		MayThrow(perrors.CodeNotFound).
		Handle(api.ServerStream(itemChanges)))

	// A client stream: the consumer sends deltas over the published WebSocket
	// wire and reads one declared result. There is no SSE alternative — SSE
	// carries one direction — so the declared transport list holds WebSocket
	// alone and the generated clients open it without any consumer branch.
	apiPlugin.Register(api.Endpoint("GET", "/items/{id}/adjust").
		Description("Apply a stream of stock deltas and return the total").
		Params(api.Type[GetItemParams]()).
		Body(api.StreamOf[StockDelta]()).
		Returns(api.Type[StockTotal]()).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		// The resolver runs on this endpoint, not only server-wide: a first-party
		// WebSocket admits in band, and the endpoint chain is what the provider
		// replays on the request it rebuilds from the init frame. A resolver
		// installed only on the server sees the bare upgrade, which carries no
		// credential by design, and would refuse every conforming client.
		Use(IdentityResolver()).
		Client(api.ClientOperationOptions{
			Security:   stockStreamSecurity(),
			Resilience: stockStreamResilience(),
		}).
		MayThrow(perrors.CodeNotFound).
		Handle(api.ClientStream(adjustStock)))

	// The bidirectional conversation: same declaration, both directions
	// streamed. Its declared result is the last value the provider sends.
	apiPlugin.Register(api.Endpoint("GET", "/items/{id}/negotiate").
		Description("Exchange stock deltas and running totals").
		Params(api.Type[GetItemParams]()).
		Body(api.StreamOf[StockDelta]()).
		Returns(api.StreamOf[StockTotal]()).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		Use(IdentityResolver()).
		Client(api.ClientOperationOptions{
			Security:   stockStreamSecurity(),
			Resilience: stockStreamResilience(),
		}).
		MayThrow(perrors.CodeNotFound).
		Handle(api.BidiStream(negotiateStock)))

	// Two operations declared Connect-only. Mounting the bridge adds Connect
	// behind REST, SSE and WebSocket on every other route, so those keep their
	// wire; these two narrow the derived list to Connect, in the bridge's
	// encoding order (JSON, then protobuf). Every generated client dispatches
	// them over Connect without a consumer branch.
	apiPlugin.Register(api.Endpoint("GET", "/quotes/{id}").
		Description("Read one quote over Connect").
		Params(api.Type[GetItemParams]()).
		Returns(api.Type[Quote]()).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		Client(api.ClientOperationOptions{
			Security:   catalogKeySecurity(),
			Transports: []clientcontract.TransportProtocol{clientcontract.TransportConnect},
		}).
		MayThrow(perrors.CodeNotFound).
		Handle(getQuote))

	// The same quote, declared protobuf first. The bridge serves JSON before
	// protobuf; ConnectEncodings states this operation's own order, so every
	// generated client dispatches it over Connect protobuf without a consumer
	// branch. The two quote routes differ in that declaration and nothing else.
	apiPlugin.Register(api.Endpoint("GET", "/quotes/{id}/snapshot").
		Description("Read one quote over Connect, protobuf first").
		Params(api.Type[GetItemParams]()).
		Returns(api.Type[Quote]()).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		Client(api.ClientOperationOptions{
			Security:   catalogKeySecurity(),
			Transports: []clientcontract.TransportProtocol{clientcontract.TransportConnect},
			ConnectEncodings: []clientcontract.Encoding{
				clientcontract.EncodingProto, clientcontract.EncodingJSON,
			},
		}).
		MayThrow(perrors.CodeNotFound).
		Handle(getQuote))

	apiPlugin.Register(api.Endpoint("GET", "/quotes/{id}/ticks").
		Description("Stream the ticks of one quote over Connect").
		Params(api.Type[GetItemParams]()).
		Returns(api.StreamOf[QuoteTick]()).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		Client(api.ClientOperationOptions{
			Security:   catalogKeySecurity(),
			Transports: []clientcontract.TransportProtocol{clientcontract.TransportConnect},
		}).
		MayThrow(perrors.CodeNotFound).
		Handle(api.ServerStream(watchQuote)))

	// Two credentials one alternative requires together: the api key and the
	// `tenant` named header. A consumer that binds only one of them never
	// dispatches the call.
	apiPlugin.Register(api.Endpoint("GET", "/tenant-check").
		Description("Report that the api key and the tenant both arrived").
		Returns(api.Type[TenantCheck]()).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		Client(api.ClientOperationOptions{Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
			AllOf: []clientcontract.SecurityRequirement{{Profile: "catalog-key"}, {Profile: "tenant"}},
		}}}}).
		MayThrow(perrors.CodeUnauthorized).
		Handle(checkTenant))

	// The caller's user identity, forwarded from the consumer's own inbound
	// request: the operation declares the profile, the binding opts in, and the
	// consumer writes no header.
	apiPlugin.Register(api.Endpoint("GET", "/whoami").
		Description("Name the user the caller forwarded").
		Returns(api.Type[Caller]()).
		Secure(security.Options{Client: []string{"catalog.consumer"}}).
		Client(api.ClientOperationOptions{Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
			AllOf: []clientcontract.SecurityRequirement{{Profile: "user"}},
		}}}}).
		MayThrow(perrors.CodeUnauthorized).
		Handle(whoAmI))
}

// NewApp builds the provider application: the HTTP server, the api endpoints, the
// OpenAPI spec, and the Go client generator. Running it under `putnami build`
// emits .gen/clientgen/config.json and regenerates clients/go from the spec.
func NewApp() *app.Application {
	httpServer := http.NewServerPlugin(http.ServerConfig{Port: 3910})
	httpServer.Use(IdentityResolver())
	// The operational surface: /livez, /healthz, /readyz and /version. A
	// deployment's probes and `putnami qualify` read readiness here before any
	// business request is sent.
	platformPlugin := platform.NewPlugin(platform.Config{})
	platformPlugin.RegisterOn(httpServer)
	apiPlugin := api.New(httpServer, ClientContract())
	Register(apiPlugin)
	protoPlugin, bridge := ConnectPlugins(apiPlugin, httpServer)

	openapiPlugin := openapi.NewPlugin(openapi.PluginOptions{
		Title:   "Items API",
		Version: "1.0.0",
	}).From(apiPlugin)

	clients := api.Clients(api.ClientsOptions{
		// Both targets: the Go client is emitted in-app by this describer (same
		// language); the TypeScript client is emitted by `putnami clientgen` from
		// the same spec recorded in .gen/clientgen/config.json.
		Targets: []string{"go", "ts"},
		Go: api.GoClientOptions{
			PackageName: ClientPackage,
			ClientName:  ClientName,
		},
		TS: api.TSClientOptions{
			Output:      "clients/ts",
			PackageName: TSClientPackage,
		},
	}).From(apiPlugin)

	application := app.New(ProjectName)
	application.Feature(app.Feature{
		ID:      FeatureID,
		Name:    "Item management",
		Outcome: "Consumers can list and retrieve catalog items through a typed client",
		Owner:   "samples",
	})
	application.Use(httpServer)
	application.Use(platformPlugin)
	application.Use(apiPlugin)
	application.Use(protoPlugin)
	application.Use(bridge)
	application.Use(openapiPlugin)
	application.Use(clients)
	return application
}
