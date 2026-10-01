package itemsclient_test

// TS→Go continuation cell of the client matrix: the TypeScript provider's
// catalog change feed declares a cursor continuation, and this generated Go
// client keeps one stream open across an instance change. The two provider
// instances are two real TypeScript processes that share no memory: each seeds
// the same change log from the same catalog, so a continuation placed by the
// cursor alone lands correctly on either. The front in between is the routing
// boundary, the one double in this file.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"

	itemsclient "go.putnami.dev/examples/ts-items-client"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// matrixFeature is the feature the TypeScript sample owns in its
// putnami.features.json; this consumer's tests bind the checks it declares.
const (
	matrixFeature           = "samples/ts-first-party-client-matrix"
	continuationRequirement = "a-declared-continuation-survives-an-instance-change"
	// seededRevisions is how many changes a fresh TypeScript provider seeds:
	// one per catalog item.
	seededRevisions = 3
)

// opening is what the front observed on one stream connection: the query the
// consumer sent, the wire it asked for, and the wire the provider acknowledged
// on the response head.
type opening struct {
	query        url.Values
	requested    []string
	acknowledged []string
}

// changeFront routes every connection to the current provider process, the way
// a load balancer does, and records what crossed it on the change feed.
type changeFront struct {
	*httptest.Server
	current  atomic.Pointer[url.URL]
	mu       sync.Mutex
	openings []*opening
}

func newChangeFront(t *testing.T, first *foreignProvider) *changeFront {
	t.Helper()
	front := &changeFront{}
	front.route(t, first)
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(front.current.Load())
		},
		// Every event is forwarded as it is written: a stream is not a body
		// to buffer.
		FlushInterval: -1,
		ModifyResponse: func(response *http.Response) error {
			if seen, ok := response.Request.Context().Value(openingKey{}).(*opening); ok {
				seen.acknowledged = response.Header.Values(clientcontract.SSEWireHeader)
			}
			return nil
		},
	}
	front.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/items/changes" {
			proxy.ServeHTTP(writer, request)
			return
		}
		seen := &opening{query: request.URL.Query(), requested: request.Header.Values(clientcontract.SSEWireHeader)}
		front.mu.Lock()
		front.openings = append(front.openings, seen)
		front.mu.Unlock()
		// The opening travels on the request, so the response hook records
		// the provider's acknowledgment against it.
		proxy.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), openingKey{}, seen)))
	}))
	t.Cleanup(front.Close)
	return front
}

type openingKey struct{}

// route sends every following connection to the given provider process.
func (front *changeFront) route(t *testing.T, provider *foreignProvider) {
	t.Helper()
	target, err := url.Parse(provider.baseURL)
	if err != nil {
		t.Fatal(err)
	}
	front.current.Store(target)
}

func (front *changeFront) all() []*opening {
	front.mu.Lock()
	defer front.mu.Unlock()
	return append([]*opening(nil), front.openings...)
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func revisions(changes []itemsclient.ListItemsChangesMessage) []float64 {
	out := make([]float64, 0, len(changes))
	for _, change := range changes {
		out = append(out, change.Revision)
	}
	return out
}

func sequence(to int) []float64 {
	out := make([]float64, 0, to)
	for revision := 1; revision <= to; revision++ {
		out = append(out, float64(revision))
	}
	return out
}

// TestTypeScriptProviderChangeFeedContinuesOnAnotherProcessForTheGeneratedGoClient
// opens the feed through the front, reads the seeded revisions from the first
// process, and drains that process behind the front while the feed waits for
// more. The generated client reopens on the second process after the last
// change the caller received, with the credential the binding supplies and the
// wire negotiated again; three creations on the second process then arrive
// once, in order, and the feed completes explicitly. Nothing opens after the
// terminal.
func TestTypeScriptProviderChangeFeedContinuesOnAnotherProcessForTheGeneratedGoClient(t *testing.T) {
	spectest.Proves(t, matrixFeature, continuationRequirement,
		"a-cursor-stream-continues-on-another-instance-after-the-last-delivered-change")
	first, second := startForeignProviderInstance(t), startForeignProviderInstance(t)
	front := newChangeFront(t, first)
	generated := boundClientWithKey(t, front.URL, catalogAPIKey)

	until := seededRevisions + 3
	stream, err := generated.ListItemsChanges(t.Context(), itemsclient.ListItemsChangesInput{
		Query: itemsclient.ListItemsChangesQuery{Until: float64(until)},
	})
	if err != nil {
		t.Fatalf("ListItemsChanges: %v", err)
	}
	defer func() { _ = stream.Close() }()

	received := make([]itemsclient.ListItemsChangesMessage, 0, until)
	for range seededRevisions {
		select {
		case change, ok := <-stream.Messages():
			if !ok {
				t.Fatalf("the feed ended early: %v", stream.Err())
			}
			received = append(received, change)
		case <-time.After(10 * time.Second):
			t.Fatal("no change reached the caller")
		}
	}
	if got := revisions(received); !reflect.DeepEqual(got, sequence(seededRevisions)) {
		t.Fatalf("seeded revisions = %v, want %v in order", got, sequence(seededRevisions))
	}

	// The instance change: the front now routes to the second process and the
	// first one drains. The two processes share no memory: the second places
	// the continuation by the cursor alone, on the revisions it seeded from
	// the same catalog.
	front.route(t, second)
	first.drain(t)
	eventually(t, "the continuation to reach the second process", func() bool { return len(front.all()) == 2 })

	// Three creations through the front, on the second process; the last one
	// completes the feed.
	for _, name := range []string{"Sprocket-0", "Sprocket-1", "Sprocket-2"} {
		if _, err := generated.CreateItems(t.Context(), itemsclient.CreateItemsInput{
			Body: itemsclient.CreateItemsBody{Name: name, Price: 3.5, Stock: 7},
		}); err != nil {
			t.Fatalf("CreateItems(%s): %v", name, err)
		}
	}
	for change := range stream.Messages() {
		received = append(received, change)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("the continued feed ended with %v, want the explicit completion", err)
	}
	if got := revisions(received); !reflect.DeepEqual(got, sequence(until)) {
		t.Fatalf("received revisions %v, want %v: a change was skipped or arrived twice", got, sequence(until))
	}
	for index, change := range received {
		if want := "r" + strconv.Itoa(index+1); change.Cursor != want {
			t.Fatalf("change %d carries cursor %q, want %q", index, change.Cursor, want)
		}
	}

	openings := front.all()
	if len(openings) != 2 {
		t.Fatalf("openings = %d, want the first connection and one continuation, and nothing after the terminal", len(openings))
	}
	if openings[0].query.Has("cursor") {
		t.Fatalf("the first opening carried a position: %v", openings[0].query)
	}
	if got := openings[1].query.Get("cursor"); got != "r"+strconv.Itoa(seededRevisions) {
		t.Fatalf("continuation cursor = %q, want r%d, the last change the caller received", got, seededRevisions)
	}
	if openings[1].query.Get("until") != openings[0].query.Get("until") {
		t.Fatalf("the continuation lost the original query: %v", openings[1].query)
	}
	for index, seen := range openings {
		if !clientcontract.NegotiatesSSEWire(seen.requested) {
			t.Fatalf("opening %d did not ask for the negotiated wire: %q", index, seen.requested)
		}
		if !clientcontract.NegotiatesSSEWire(seen.acknowledged) {
			t.Fatalf("the provider did not acknowledge the wire on opening %d: %q", index, seen.acknowledged)
		}
	}
}

// TestTypeScriptProviderRefusesAPositionItNeverIssuedAsATypedError proves a
// forged cursor reaches this consumer as the declared not_found: the feed never
// restarts from the beginning on a position it does not recognize, and the
// runtime never reopens after a typed terminal.
func TestTypeScriptProviderRefusesAPositionItNeverIssuedAsATypedError(t *testing.T) {
	spectest.Proves(t, matrixFeature, continuationRequirement,
		"a-position-the-provider-never-issued-is-its-typed-refusal")
	front := newChangeFront(t, startForeignProviderInstance(t))
	generated := boundClientWithKey(t, front.URL, catalogAPIKey)
	forged := "r999999"
	stream, err := generated.ListItemsChanges(t.Context(), itemsclient.ListItemsChangesInput{
		Query: itemsclient.ListItemsChangesQuery{Cursor: &forged, Until: 1},
	})
	if err != nil {
		t.Fatalf("ListItemsChanges: %v", err)
	}
	for range stream.Messages() {
		t.Fatal("a refused position delivered a change")
	}
	var notFound *itemsclient.ListItemsChangesNotFoundError
	if !errors.As(stream.Err(), &notFound) || notFound.Remote.StatusCode != http.StatusNotFound {
		t.Fatalf("terminal = %T %v, want the declared not_found", stream.Err(), stream.Err())
	}
	if got := front.all(); len(got) != 1 {
		t.Fatalf("openings = %d, want exactly one: a typed refusal is never reopened", len(got))
	}
}
