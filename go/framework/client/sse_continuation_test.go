package client

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

const sseContinuationRequirement = "declared-sse-continuation"

// sseSceneCorpus is protocols/clientcontract/fixtures/sse/scenes.json: the
// reader outcomes both runtimes replay (clientcontract ADR 0013).
type sseSceneCorpus struct {
	Description   string                                    `json:"description"`
	MaxFrameBytes int64                                     `json:"maxFrameBytes"`
	Continuations map[string]clientcontract.SSEContinuation `json:"continuations"`
	Scenes        []sseScene                                `json:"scenes"`
}

type sseSceneError struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
}

type sseScene struct {
	Name           string     `json:"name"`
	Continuation   string     `json:"continuation"`
	Requested      bool       `json:"requested"`
	Acknowledgment []string   `json:"acknowledgment"`
	Query          url.Values `json:"query"`
	MaxFrameBytes  int64      `json:"maxFrameBytes"`
	Body           []string   `json:"body"`
	End            string     `json:"end"`
	Expect         struct {
		Outcome         string            `json:"outcome"`
		Messages        []json.RawMessage `json:"messages"`
		DeliveredCursor *string           `json:"deliveredCursor"`
		Error           *sseSceneError    `json:"error"`
		Reopen          *struct {
			Query url.Values `json:"query"`
		} `json:"reopen"`
	} `json:"expect"`
}

func readSSESceneCorpus(t *testing.T) sseSceneCorpus {
	t.Helper()
	data, err := os.ReadFile("../../../protocols/clientcontract/fixtures/sse/scenes.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var corpus sseSceneCorpus
	if err := decoder.Decode(&corpus); err != nil {
		t.Fatalf("fixtures/sse/scenes.json: %v", err)
	}
	if len(corpus.Scenes) == 0 {
		t.Fatal("the shared scene corpus is empty")
	}
	return corpus
}

// sseLogOperation is a safe server stream of log lines. A nil continuation is
// the operation an old runtime was generated for: no marker, legacy framing.
func sseLogOperation(continuation *clientcontract.SSEContinuation, maxFrameBytes int64, reconnect bool,
	security clientcontract.Security) Operation {
	required := []string{"line"}
	if continuation != nil && continuation.Mode == clientcontract.SSEContinuationCursor {
		required = []string{"cursor", "line"}
	}
	message := clientcontract.Schema{
		Type: "object",
		Properties: map[string]clientcontract.Schema{
			"cursor": {Type: "string"}, "line": {Type: "string"}, "service": {Type: "string"},
		},
		Required: required, AdditionalProperties: additionalForbidden(),
	}
	transport := clientcontract.Transport{Protocol: clientcontract.TransportSSE, Path: "/logs/tail", Encoding: clientcontract.EncodingJSON}
	stream := &clientcontract.StreamPolicy{MaxFrameBytes: &maxFrameBytes}
	if continuation != nil {
		transport.SSE = &clientcontract.SSETransport{Continuation: continuation}
		stream.Reconnect = &reconnect
	}
	return Operation{
		ID: "tailLogs",
		Contract: clientcontract.OperationV1{
			Stream:     clientcontract.StreamServer,
			Messages:   &clientcontract.MessageShapes{Output: &message},
			Transports: []clientcontract.Transport{transport},
			Security:   security,
			Errors: []clientcontract.DeclaredError{
				{Status: http.StatusBadRequest, Code: "http.bad_request"},
				{Status: http.StatusForbidden, Code: "forbidden"},
				{Status: http.StatusNotFound, Code: "not_found"},
			},
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
			Resilience:  &clientcontract.ResiliencePolicy{Stream: stream},
		},
		Successes: []OperationSuccess{{Status: http.StatusOK, Content: []OperationContent{{MediaType: "text/event-stream", Schema: &message}}}},
	}
}

func anonymousSecurity() clientcontract.Security {
	return clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}}
}

func cursorContinuation() *clientcontract.SSEContinuation {
	return &clientcontract.SSEContinuation{
		Mode:   clientcontract.SSEContinuationCursor,
		Cursor: &clientcontract.SSECursor{OutputField: "cursor", QueryParameter: "cursor"},
	}
}

func bestEffortContinuation() *clientcontract.SSEContinuation {
	return &clientcontract.SSEContinuation{Mode: clientcontract.SSEContinuationBestEffort}
}

// sseRequestLog records what each connection of one session asked for.
type sseRequestLog struct {
	mu       sync.Mutex
	requests []sseRequestRecord
}

type sseRequestRecord struct {
	query         url.Values
	wire          []string
	authorization string
}

func (log *sseRequestLog) record(request *http.Request) int {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.requests = append(log.requests, sseRequestRecord{
		query:         request.URL.Query(),
		wire:          request.Header.Values(clientcontract.SSEWireHeader),
		authorization: request.Header.Get("Authorization"),
	})
	return len(log.requests)
}

func (log *sseRequestLog) all() []sseRequestRecord {
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]sseRequestRecord(nil), log.requests...)
}

// sseSceneRun is what the real runtime observed replaying one scene.
type sseSceneRun struct {
	messages  []json.RawMessage
	openErr   error
	streamErr error
	requests  []sseRequestRecord
}

// replaySSEScene serves the scene's first connection over real HTTP and reads
// it through the real runtime. A reopening is answered with an acknowledged
// `complete`, so the query it asked for is observable and the session ends.
//
// An interrupted scene holds the break until the caller has received every
// message the scene expects: the scene describes a caller that took all of
// them, so the delivered position is the last one.
func replaySSEScene(t *testing.T, corpus sseSceneCorpus, scene sseScene, reconnect bool) sseSceneRun {
	t.Helper()
	var continuation *clientcontract.SSEContinuation
	if scene.Requested {
		declared := corpus.Continuations[scene.Continuation]
		continuation = &declared
	}
	maxFrameBytes := scene.MaxFrameBytes
	if maxFrameBytes == 0 {
		maxFrameBytes = corpus.MaxFrameBytes
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBreak := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseBreak()
	holdBreak := scene.Expect.Outcome == "interrupted"

	requests := &sseRequestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection := requests.record(request)
		writer.Header().Set("Content-Type", "text/event-stream")
		if connection > 1 {
			writer.Header().Set(clientcontract.SSEWireHeader, clientcontract.SSEWireV1)
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, clientcontract.SSECompleteFrame)
			return
		}
		if len(scene.Acknowledgment) > 0 {
			writer.Header()[clientcontract.SSEWireHeader] = scene.Acknowledgment
		}
		writer.WriteHeader(http.StatusOK)
		for _, chunk := range scene.Body {
			writeSSE(t, writer, []byte(chunk))
		}
		if holdBreak {
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
		}
		if scene.End == "reset" {
			panic(http.ErrAbortHandler)
		}
	}))
	defer server.Close()

	bound := boundTestClient(t, server.URL, testDescriptor(map[string]clientcontract.CredentialProfile{}), ServiceBinding{})
	operation := sseLogOperation(continuation, maxFrameBytes, reconnect, anonymousSecurity())
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	run := sseSceneRun{messages: []json.RawMessage{}}
	stream, err := OpenServerStream[json.RawMessage](ctx, bound, &Request{QueryValues: cloneURLValues(scene.Query)}, operation)
	if err != nil {
		run.openErr = err
		run.requests = requests.all()
		return run
	}
	expected := len(scene.Expect.Messages)
	if expected == 0 {
		releaseBreak()
	}
	for message := range stream.Messages() {
		run.messages = append(run.messages, message)
		if len(run.messages) == expected {
			releaseBreak()
		}
	}
	run.streamErr = stream.Err()
	run.requests = requests.all()
	return run
}

// TestTheSharedSSEScenesReplayAgainstTheRealRuntime replays every scene of the
// corpus both runtimes share through OpenServerStream over real HTTP. An
// interrupted scene is replayed twice: with reconnect, the reopened
// connection asks for exactly the query the scene names; without it, the
// interruption is the session's terminal and nothing reopens.
func TestTheSharedSSEScenesReplayAgainstTheRealRuntime(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"the-shared-sse-scenes-replay-against-the-real-runtime")
	corpus := readSSESceneCorpus(t)
	outcomes := map[string]bool{}
	for _, scene := range corpus.Scenes {
		t.Run(scene.Name, func(t *testing.T) {
			outcomes[scene.Expect.Outcome] = true
			reconnects := []bool{false}
			if scene.Expect.Outcome == "interrupted" {
				reconnects = []bool{true, false}
			}
			for _, reconnect := range reconnects {
				run := replaySSEScene(t, corpus, scene, reconnect)
				assertSSEScene(t, scene, reconnect, run)
			}
		})
	}
	for _, outcome := range []string{"complete", "error", "interrupted", "contract-error", "transport-error"} {
		if !outcomes[outcome] {
			t.Errorf("no scene reaches the %s outcome", outcome)
		}
	}
}

func assertSSEScene(t *testing.T, scene sseScene, reconnect bool, run sseSceneRun) {
	t.Helper()
	if len(run.requests) == 0 {
		t.Fatalf("reconnect=%v: the provider saw no request (open error %v)", reconnect, run.openErr)
	}
	first := run.requests[0]
	if asked := clientcontract.NegotiatesSSEWire(first.wire); asked != scene.Requested {
		t.Fatalf("the runtime asked for the negotiated wire = %v (%q), want %v", asked, first.wire, scene.Requested)
	}
	if !sameJSONMessages(run.messages, scene.Expect.Messages) {
		t.Fatalf("reconnect=%v: messages = %s, want %s", reconnect, run.messages, scene.Expect.Messages)
	}
	if scene.Requested && scene.Continuation == "cursor" {
		if got := lastCursor(t, run.messages); !reflect.DeepEqual(got, scene.Expect.DeliveredCursor) {
			t.Fatalf("delivered cursor = %v, want %v", printablePosition(got), printablePosition(scene.Expect.DeliveredCursor))
		}
	}
	terminal := run.streamErr
	if run.openErr != nil {
		terminal = run.openErr
	}
	switch scene.Expect.Outcome {
	case "complete":
		if terminal != nil {
			t.Fatalf("terminal = %v, want a clean completion", terminal)
		}
	case "error":
		var remote *RemoteError
		if !stderrors.As(terminal, &remote) || remote.StatusCode != scene.Expect.Error.Status || remote.RemoteCode != scene.Expect.Error.Code {
			t.Fatalf("terminal = %T %v, want the typed %d %s", terminal, terminal, scene.Expect.Error.Status, scene.Expect.Error.Code)
		}
	case "contract-error", "transport-error":
		if terminal == nil || !perrors.Is(terminal, CodeClientResponse) {
			t.Fatalf("terminal = %T %v, want %s", terminal, terminal, CodeClientResponse)
		}
	case "interrupted":
		if run.openErr != nil {
			t.Fatalf("an interrupted scene failed to open: %v", run.openErr)
		}
		if !reconnect {
			if terminal == nil || !perrors.Is(terminal, CodeClientResponse) {
				t.Fatalf("an interruption without reconnect ended with %T %v, want %s", terminal, terminal, CodeClientResponse)
			}
			break
		}
		if terminal != nil {
			t.Fatalf("the reopened session ended with %v, want the reopening's clean completion", terminal)
		}
		if len(run.requests) != 2 {
			t.Fatalf("connections = %d, want the first and one reopening", len(run.requests))
		}
		reopened := run.requests[1]
		if want := normalizedValues(scene.Expect.Reopen.Query); !reflect.DeepEqual(reopened.query, want) {
			t.Fatalf("reopened query = %v, want %v", reopened.query, want)
		}
		if !clientcontract.NegotiatesSSEWire(reopened.wire) {
			t.Fatalf("the reopening did not ask for the negotiated wire: %q", reopened.wire)
		}
		return
	default:
		t.Fatalf("unknown scene outcome %q", scene.Expect.Outcome)
	}
	if len(run.requests) != 1 {
		t.Fatalf("connections = %d, want exactly one: only an interruption reopens", len(run.requests))
	}
}

func sameJSONMessages(got, want []json.RawMessage) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		var left, right any
		if json.Unmarshal(got[i], &left) != nil || json.Unmarshal(want[i], &right) != nil || !reflect.DeepEqual(left, right) {
			return false
		}
	}
	return true
}

func lastCursor(t *testing.T, messages []json.RawMessage) *string {
	t.Helper()
	if len(messages) == 0 {
		return nil
	}
	var message struct {
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal(messages[len(messages)-1], &message); err != nil {
		t.Fatal(err)
	}
	return &message.Cursor
}

func printablePosition(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

func normalizedValues(values url.Values) url.Values {
	if values == nil {
		return url.Values{}
	}
	return values
}

// TestTheDeliveryBridgeAdvancesThePositionOnlyOnAHandoff drives the bridge
// directly: a value decoded and queued is not delivered, a value the caller
// took is, and a reset drops exactly the values the caller has not taken.
func TestTheDeliveryBridgeAdvancesThePositionOnlyOnAHandoff(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"values-queued-but-not-received-never-advance-the-position-nor-arrive-twice")
	session, ctx := newStreamSession(t.Context(), streamSessionConfig{})
	defer func() { _ = session.Close() }()
	var handoffs atomic.Int32
	delivery := newSSEDelivery[string](3, make(chan struct{}), func() { handoffs.Add(1) })
	enqueue := func(value string) {
		t.Helper()
		if err := delivery.enqueue(ctx, session, ssePending[string]{value: value, position: "c-" + value}); err != nil {
			t.Fatal(err)
		}
	}
	receive := func() string {
		t.Helper()
		select {
		case value := <-delivery.out:
			return value
		case <-time.After(2 * time.Second):
			t.Fatal("the bridge offered nothing")
			return ""
		}
	}

	if position, live := delivery.reset(); !live || position != "" {
		t.Fatalf("a reset before any handoff = %q %v, want no position", position, live)
	}
	// Three values fill the bound: one offered, two queued.
	enqueue("1")
	enqueue("2")
	enqueue("3")
	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		enqueue("4")
	}()
	select {
	case <-blocked:
		t.Fatal("a fourth value entered a bridge bounded at three")
	case <-time.After(50 * time.Millisecond):
	}
	if got := receive(); got != "1" {
		t.Fatalf("first handoff = %q", got)
	}
	<-blocked
	// 2, 3 and 4 were decoded and queued; only 1 was taken.
	position, live := delivery.reset()
	if !live || position != "c-1" {
		t.Fatalf("position after one handoff = %q %v, want c-1", position, live)
	}
	// The dropped values are not offered again: the next one is new.
	enqueue("2'")
	if got := receive(); got != "2'" {
		t.Fatalf("after a reset the caller was offered %q, want the value queued after it", got)
	}
	if position, _ := delivery.reset(); position != "c-2'" {
		t.Fatalf("position = %q, want c-2'", position)
	}
	if got := handoffs.Load(); got != 2 {
		t.Fatalf("handoffs = %d, want 2", got)
	}

	// finish hands over what is queued, in order, then closes the channel.
	enqueue("5")
	enqueue("6")
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		delivery.finish()
		close(delivery.out)
	}()
	rest := make([]string, 0, 2)
	for value := range delivery.out {
		rest = append(rest, value)
	}
	<-finished
	if !reflect.DeepEqual(rest, []string{"5", "6"}) {
		t.Fatalf("flushed = %v, want [5 6]", rest)
	}
	if _, live := delivery.reset(); live {
		t.Fatal("a finished bridge accepted a reset")
	}
}

// TestTheDeliveryBridgeDropsWhatTheCallerNeverTookWhenTheCallerCloses proves
// the bridge holds no goroutine past the caller closing its stream.
func TestTheDeliveryBridgeDropsWhatTheCallerNeverTookWhenTheCallerCloses(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", sseContinuationRequirement,
		"closing-a-reopening-stream-releases-its-connection-and-emits-one-measurement")
	session, ctx := newStreamSession(t.Context(), streamSessionConfig{})
	closed := make(chan struct{})
	// A bound of one leaves no queue slot, so an enqueue after the session
	// ended can only observe that end.
	delivery := newSSEDelivery[string](1, closed, func() {})
	if err := delivery.enqueue(ctx, session, ssePending[string]{value: "1"}); err != nil {
		t.Fatal(err)
	}
	close(closed)
	_ = session.Close()
	select {
	case <-delivery.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the bridge outlived the caller closing its stream")
	}
	if err := delivery.enqueue(ctx, session, ssePending[string]{value: "2"}); err == nil || !perrors.Is(err, CodeClientCanceled) {
		t.Fatalf("an enqueue after the session ended = %v, want %s", err, CodeClientCanceled)
	}
	delivery.finish()
	select {
	case value := <-delivery.out:
		t.Fatalf("a value the caller never took was offered after the stream closed: %q", value)
	default:
	}
}

// TestTheDeliveryBridgeKeepsQueuedValuesPastTheEndOfTheSession proves that a
// session context ending after the terminal — the declared duration, the
// caller's context, or the session's own close — drops nothing the provider
// sent before it.
func TestTheDeliveryBridgeKeepsQueuedValuesPastTheEndOfTheSession(t *testing.T) {
	session, ctx := newStreamSession(t.Context(), streamSessionConfig{})
	delivery := newSSEDelivery[string](4, make(chan struct{}), func() {})
	for _, value := range []string{"1", "2", "3"} {
		if err := delivery.enqueue(ctx, session, ssePending[string]{value: value}); err != nil {
			t.Fatal(err)
		}
	}
	session.Complete()
	_ = session.Close()
	go func() {
		delivery.finish()
		close(delivery.out)
	}()
	received := make([]string, 0, 3)
	for value := range delivery.out {
		received = append(received, value)
	}
	if !reflect.DeepEqual(received, []string{"1", "2", "3"}) {
		t.Fatalf("received = %v, want [1 2 3]", received)
	}
}
