package consumer

// The continuation family of the Go consumer's SSE cell: the catalog change
// feed declares a cursor continuation, and the generated client keeps one
// stream open across an instance change. The provider instances are two real
// route sets of this sample sharing the process-wide change log; the front in
// between is the routing boundary, the one double in this file.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"

	"go.putnami.dev/api"
	itemsclient "go.putnami.dev/examples/service-to-service/clients/go"
	"go.putnami.dev/examples/service-to-service/service"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// continuationRequirement is the requirement of the matrix feature the tests
// below bind their checks to.
const continuationRequirement = "a-declared-continuation-survives-an-instance-change"

// providerInstance is one real provider instance: the sample's route set on
// its own server plugin. Two of them share nothing but the change log, the
// way two replicas share a durable store.
type providerInstance struct {
	plugin  *phttp.ServerPlugin
	handler http.Handler
}

func newProviderInstance(t *testing.T) *providerInstance {
	t.Helper()
	serverPlugin := phttp.NewServerPlugin(phttp.ServerConfig{})
	serverPlugin.Use(service.IdentityResolver())
	apiPlugin := api.New(serverPlugin, service.ClientContract())
	service.Register(apiPlugin)
	if err := apiPlugin.Configure(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	return &providerInstance{plugin: serverPlugin, handler: serverPlugin.Handler()}
}

// drain is the instance replacement a continuation exists for: the graceful
// stop ends every negotiated stream with no terminal.
func (instance *providerInstance) drain(t *testing.T) {
	t.Helper()
	if err := instance.plugin.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

// opening is what the front observed on one stream connection: the query the
// consumer sent, the wire it asked for, the wire the provider acknowledged on
// the response head, and how many events the provider wrote before the
// connection ended.
type opening struct {
	query        url.Values
	requested    []string
	acknowledged []string
	events       atomic.Int32
}

// changeFront routes every connection to the current instance, the way a load
// balancer does, and records what crossed it on the change feed.
type changeFront struct {
	*httptest.Server
	current  atomic.Pointer[providerInstance]
	mu       sync.Mutex
	openings []*opening
}

func newChangeFront(t *testing.T, first *providerInstance) *changeFront {
	t.Helper()
	front := &changeFront{}
	front.current.Store(first)
	front.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/items/changes" {
			front.current.Load().handler.ServeHTTP(writer, request)
			return
		}
		seen := &opening{query: request.URL.Query(), requested: request.Header.Values(clientcontract.SSEWireHeader)}
		front.mu.Lock()
		front.openings = append(front.openings, seen)
		front.mu.Unlock()
		front.current.Load().handler.ServeHTTP(&observedWriter{ResponseWriter: writer, seen: seen}, request)
	}))
	t.Cleanup(front.Close)
	return front
}

func (front *changeFront) all() []*opening {
	front.mu.Lock()
	defer front.mu.Unlock()
	return append([]*opening(nil), front.openings...)
}

// observedWriter records the response head and counts the events written
// through it. It unwraps for the response controller the stream writer uses.
type observedWriter struct {
	http.ResponseWriter
	seen        *opening
	wroteHeader bool
}

func (writer *observedWriter) WriteHeader(status int) {
	if !writer.wroteHeader {
		writer.wroteHeader = true
		writer.seen.acknowledged = writer.Header().Values(clientcontract.SSEWireHeader)
	}
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *observedWriter) Write(chunk []byte) (int, error) {
	if !writer.wroteHeader {
		writer.WriteHeader(http.StatusOK)
	}
	if len(chunk) >= 5 && string(chunk[:5]) == "data:" {
		writer.seen.events.Add(1)
	}
	return writer.ResponseWriter.Write(chunk)
}

func (writer *observedWriter) Flush() {
	if flusher, ok := writer.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (writer *observedWriter) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// receiveChange takes the next change the generated stream delivers.
func receiveChange(t *testing.T, stream interface {
	Messages() <-chan itemsclient.ItemChange
	Err() error
}) itemsclient.ItemChange {
	t.Helper()
	select {
	case change, ok := <-stream.Messages():
		if !ok {
			t.Fatalf("the feed ended early: %v", stream.Err())
		}
		return change
	case <-time.After(5 * time.Second):
		t.Fatal("no change reached the caller")
		return itemsclient.ItemChange{}
	}
}

// revisions lists the revisions of the changes received, in order.
func revisions(changes []itemsclient.ItemChange) []int64 {
	out := make([]int64, 0, len(changes))
	for _, change := range changes {
		out = append(out, change.Revision)
	}
	return out
}

// sequence lists the revisions 1 to to: the contiguous feed a reader expects.
func sequence(to int64) []int64 {
	out := make([]int64, 0, to)
	for revision := int64(1); revision <= to; revision++ {
		out = append(out, revision)
	}
	return out
}

// TestAChangeFeedContinuesOnAnotherInstanceAfterTheLastChangeTheCallerReceived
// is the delivery-api shape on this sample: the consumer opens the feed, reads
// every retained change, and three creations are written by the first
// instance while the caller takes none of them. The instance is then drained
// behind the front, one revision short of the feed's end. The generated client
// reopens on the second instance after the last change the caller received —
// not after the ones it had only queued — so the three creations arrive once,
// in order, then the last creation, and the feed completes explicitly. Both
// openings asked for the negotiated wire and were acknowledged; nothing opened
// after the terminal.
func TestAChangeFeedContinuesOnAnotherInstanceAfterTheLastChangeTheCallerReceived(t *testing.T) {
	spectest.Proves(t, matrixFeature, continuationRequirement,
		"a-cursor-stream-continues-on-another-instance-after-the-last-delivered-change")
	first, second := newProviderInstance(t), newProviderInstance(t)
	front := newChangeFront(t, first)
	generated := boundClient(t, sampleBinding(front.URL))

	head := service.Changes.Head()
	// Three creations before the drain, one after it: the feed cannot complete
	// on the first instance.
	until := head + 4
	stream, err := FollowChanges(t.Context(), generated, until)
	if err != nil {
		t.Fatalf("FollowChanges: %v", err)
	}
	defer func() { _ = stream.Close() }()

	received := make([]itemsclient.ItemChange, 0, until)
	for range head {
		received = append(received, receiveChange(t, stream))
	}
	if got := revisions(received); !reflect.DeepEqual(got, sequence(head)) {
		t.Fatalf("retained changes = %v, want %v in order", got, sequence(head))
	}

	// Three creations through the front, on the first instance. Its stream
	// writes them; the caller takes none of them.
	for index := range 3 {
		if _, err := CreateItem(t.Context(), generated, "Sprocket-"+strconv.Itoa(index), 75); err != nil {
			t.Fatalf("CreateItem: %v", err)
		}
	}
	eventually(t, "the first instance to write every change", func() bool {
		openings := front.all()
		return len(openings) == 1 && openings[0].events.Load() == int32(head+3)
	})

	front.current.Store(second)
	first.drain(t)
	eventually(t, "the continuation to reach the second instance", func() bool { return len(front.all()) == 2 })

	// The last creation lands on the second instance and completes the feed.
	if _, err := CreateItem(t.Context(), generated, "Sprocket-3", 75); err != nil {
		t.Fatalf("CreateItem: %v", err)
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
		if change.Cursor != "r"+strconv.FormatInt(change.Revision, 10) || change.Revision != int64(index+1) {
			t.Fatalf("change %d = %+v", index, change)
		}
	}

	openings := front.all()
	if len(openings) != 2 {
		t.Fatalf("openings = %d, want the first connection and one continuation, and nothing after the terminal", len(openings))
	}
	if openings[0].query.Has("cursor") {
		t.Fatalf("the first opening carried a position: %v", openings[0].query)
	}
	lastReceived := "r" + strconv.FormatInt(head, 10)
	if got := openings[1].query.Get("cursor"); got != lastReceived {
		t.Fatalf("continuation cursor = %q, want %q, the last change the caller received", got, lastReceived)
	}
	if openings[1].query.Get("until") != strconv.FormatInt(until, 10) {
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
	if got := openings[1].events.Load(); got != 4 {
		t.Fatalf("the continuation wrote %d changes, want the four after the position", got)
	}
}

// TestAPositionTheProviderNeverIssuedIsItsTypedRefusal proves a forged cursor
// reaches the consumer as the declared not_found — the feed never restarts
// from the beginning on a position it does not recognize, and the runtime
// never reopens after a typed terminal.
func TestAPositionTheProviderNeverIssuedIsItsTypedRefusal(t *testing.T) {
	spectest.Proves(t, matrixFeature, continuationRequirement,
		"a-position-the-provider-never-issued-is-its-typed-refusal")
	front := newChangeFront(t, newProviderInstance(t))
	generated := boundClient(t, sampleBinding(front.URL))
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
