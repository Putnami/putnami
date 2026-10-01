package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// sseContinuationSpec is a first-party contract with two safe server streams
// whose operations declare reconnect: tailLogs declares a cursor continuation
// (the delivery-api log tail shape) and followLogs a best-effort one (the
// observability-api live tail shape).
func sseContinuationSpec() SpecIR {
	reconnect := true
	entry := strictSchemaObject(map[string]clientcontract.Schema{
		"cursor": {Type: "string"},
		"line":   {Type: "string"},
	}, "cursor", "line")
	operation := &clientcontract.OperationV1{
		Stream:   clientcontract.StreamServer,
		Messages: &clientcontract.MessageShapes{Output: &clientcontract.Schema{Ref: "#/components/schemas/LogEntry"}},
		Transports: []clientcontract.Transport{{
			Protocol: clientcontract.TransportSSE, Path: "/logs/tail", Encoding: clientcontract.EncodingJSON,
			SSE: &clientcontract.SSETransport{Continuation: &clientcontract.SSEContinuation{
				Mode:   clientcontract.SSEContinuationCursor,
				Cursor: &clientcontract.SSECursor{OutputField: "cursor", QueryParameter: "cursor"},
			}},
		}},
		Security:    clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
		Errors:      []clientcontract.DeclaredError{},
		Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
		Resilience:  &clientcontract.ResiliencePolicy{Stream: &clientcontract.StreamPolicy{Reconnect: &reconnect}},
	}
	follow := *operation
	follow.Transports = []clientcontract.Transport{{
		Protocol: clientcontract.TransportSSE, Path: "/logs/follow", Encoding: clientcontract.EncodingJSON,
		SSE: &clientcontract.SSETransport{Continuation: &clientcontract.SSEContinuation{
			Mode: clientcontract.SSEContinuationBestEffort,
		}},
	}}
	return SpecIR{
		IRVersion: clientIRVersion,
		Contract: &clientcontract.DocumentV1{
			ProtocolVersion: clientcontract.ProtocolVersion,
			Service:         clientcontract.Service{ID: "logs", Audience: "https://logs.internal"},
			Credentials:     map[string]clientcontract.CredentialProfile{},
		},
		Schemas: map[string]clientcontract.Schema{"LogEntry": entry},
		Services: []ServiceIR{{Methods: []MethodIR{{
			Name: "tailLogs", OperationID: "tailLogs", HTTPMethod: "GET", Path: "/logs/tail",
			Parameters: []ParameterIR{
				{Name: "selector", Location: "query", Required: true, Schema: clientcontract.Schema{Type: "string"}},
				{Name: "cursor", Location: "query", Schema: clientcontract.Schema{Type: "string"}},
			},
			Successes: []SuccessIR{{Status: 200, Content: []ContentIR{{MediaType: "text/event-stream", Schema: &clientcontract.Schema{Ref: "#/components/schemas/LogEntry"}}}}},
			Client:    operation,
		}, {
			Name: "followLogs", OperationID: "followLogs", HTTPMethod: "GET", Path: "/logs/follow",
			Parameters: []ParameterIR{
				{Name: "selector", Location: "query", Required: true, Schema: clientcontract.Schema{Type: "string"}},
			},
			Successes: []SuccessIR{{Status: 200, Content: []ContentIR{{MediaType: "text/event-stream", Schema: &clientcontract.Schema{Ref: "#/components/schemas/LogEntry"}}}}},
			Client:    &follow,
		}}}},
	}
}

// generatedSSEContinuationE2E runs inside the throwaway module, in the
// generated package. The provider is the real go.putnami.dev/http server stream
// route with the negotiated wire the api plugin fills from the declaration;
// two instances share nothing but a durable log, and a front routes each
// connection to the current one. The consumer is the emitted method bound
// through the real runtime.
const generatedSSEContinuationE2E = `package logsclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/client"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

type durableLog struct {
	mu      sync.Mutex
	lines   []string
	closed  bool
	changed chan struct{}
}

func (log *durableLog) update(change func()) {
	log.mu.Lock()
	defer log.mu.Unlock()
	change()
	close(log.changed)
	log.changed = make(chan struct{})
}

func (log *durableLog) next(ctx context.Context, after int) (LogEntry, bool, bool) {
	for {
		log.mu.Lock()
		if after < len(log.lines) {
			entry := LogEntry{Cursor: fmt.Sprintf("c%d", after+1), Line: log.lines[after]}
			log.mu.Unlock()
			return entry, false, true
		}
		if log.closed {
			log.mu.Unlock()
			return LogEntry{}, true, true
		}
		changed := log.changed
		log.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return LogEntry{}, false, false
		}
	}
}

type instance struct {
	plugin  *phttp.ServerPlugin
	handler http.Handler
	mu      sync.Mutex
	queries []string
	sent    atomic.Int32
}

func newInstance(log *durableLog) *instance {
	served := &instance{plugin: phttp.NewServerPlugin(phttp.ServerConfig{})}
	wire := &phttp.SSEWire{
		Header: clientcontract.SSEWireHeader, Token: clientcontract.SSEWireV1,
		Negotiates: clientcontract.NegotiatesSSEWire, Complete: clientcontract.SSECompleteFrame,
	}
	send := func(stream *phttp.StreamContext, after int) error {
		for {
			entry, end, ok := log.next(stream.Context.Context(), after)
			if !ok || end {
				return nil
			}
			if err := stream.Send(entry); err != nil {
				return err
			}
			served.sent.Add(1)
			after++
		}
	}
	served.plugin.HandleStream("/logs/tail", phttp.StreamHandler{
		Mode: phttp.StreamModeServer, SSEWire: wire,
		Handle: func(stream *phttp.StreamContext) error {
			served.record(stream)
			after := 0
			if cursor := stream.Query("cursor"); cursor != "" {
				position, err := strconv.Atoi(strings.TrimPrefix(cursor, "c"))
				if err != nil {
					return err
				}
				after = position
			}
			return send(stream, after)
		},
	})
	// The live tail starts at the head of the log when the connection opens and
	// reads no position: what was written while no connection was open is lost.
	served.plugin.HandleStream("/logs/follow", phttp.StreamHandler{
		Mode: phttp.StreamModeServer, SSEWire: wire,
		Handle: func(stream *phttp.StreamContext) error {
			log.mu.Lock()
			after := len(log.lines)
			log.mu.Unlock()
			served.record(stream)
			return send(stream, after)
		},
	})
	served.handler = served.plugin.Handler()
	return served
}

func (served *instance) record(stream *phttp.StreamContext) {
	served.mu.Lock()
	defer served.mu.Unlock()
	served.queries = append(served.queries, stream.Request.URL.RawQuery)
}

func (served *instance) seen() []string {
	served.mu.Lock()
	defer served.mu.Unlock()
	return append([]string(nil), served.queries...)
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

func TestTheEmittedClientContinuesOnAnotherInstanceAfterTheLastDeliveredPosition(t *testing.T) {
	log := &durableLog{changed: make(chan struct{})}
	first, second := newInstance(log), newInstance(log)
	var current atomic.Pointer[instance]
	current.Store(first)
	front := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		current.Load().handler.ServeHTTP(writer, request)
	}))
	defer front.Close()

	transport, err := client.NewServiceClientBinding(client.ServiceBinding{URL: front.URL, ClientID: "consumer", AllowInsecure: true}, serviceDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	var in TailLogsInput
	in.Query.Selector = "svc=api"
	stream, err := NewLogsClient(transport).TailLogs(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var received []string
	take := func() {
		t.Helper()
		select {
		case entry, ok := <-stream.Messages():
			if !ok {
				t.Fatalf("the stream ended early: %v", stream.Err())
			}
			received = append(received, entry.Cursor+"="+entry.Line)
		case <-time.After(10 * time.Second):
			t.Fatal("no message reached the caller")
		}
	}
	log.update(func() { log.lines = append(log.lines, "one", "two") })
	take()
	take()
	// Written by the first instance, decoded and queued, never taken.
	log.update(func() { log.lines = append(log.lines, "three", "four") })
	eventually(t, "the first instance to write four entries", func() bool { return first.sent.Load() == 4 })
	current.Store(second)
	if err := first.plugin.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the continuation", func() bool { return len(second.seen()) == 1 })
	log.update(func() { log.lines = append(log.lines, "five"); log.closed = true })
	for entry := range stream.Messages() {
		received = append(received, entry.Cursor+"="+entry.Line)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("the continued stream ended with %v", err)
	}
	want := []string{"c1=one", "c2=two", "c3=three", "c4=four", "c5=five"}
	if !reflect.DeepEqual(received, want) {
		t.Fatalf("received %v, want %v", received, want)
	}
	if got := first.seen(); !reflect.DeepEqual(got, []string{"selector=svc%3Dapi"}) {
		t.Fatalf("first query = %v", got)
	}
	if got := second.seen(); !reflect.DeepEqual(got, []string{"cursor=c2&selector=svc%3Dapi"}) {
		t.Fatalf("continuation query = %v, want it after c2, the last position the caller received", got)
	}
}

func TestTheEmittedClientReopensABestEffortStreamWithTheOriginalQuery(t *testing.T) {
	log := &durableLog{changed: make(chan struct{})}
	first, second := newInstance(log), newInstance(log)
	var current atomic.Pointer[instance]
	current.Store(first)
	front := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		current.Load().handler.ServeHTTP(writer, request)
	}))
	defer front.Close()

	transport, err := client.NewServiceClientBinding(client.ServiceBinding{URL: front.URL, ClientID: "consumer", AllowInsecure: true}, serviceDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	var in FollowLogsInput
	in.Query.Selector = "svc=api"
	stream, err := NewLogsClient(transport).FollowLogs(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	eventually(t, "the first connection", func() bool { return len(first.seen()) == 1 })
	log.update(func() { log.lines = append(log.lines, "one") })
	var received []string
	select {
	case entry, ok := <-stream.Messages():
		if !ok {
			t.Fatalf("the stream ended early: %v", stream.Err())
		}
		received = append(received, entry.Cursor+"="+entry.Line)
	case <-time.After(10 * time.Second):
		t.Fatal("no message reached the caller")
	}
	current.Store(second)
	if err := first.plugin.Stop(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the reopening", func() bool { return len(second.seen()) == 1 })
	log.update(func() { log.lines = append(log.lines, "two"); log.closed = true })
	for entry := range stream.Messages() {
		received = append(received, entry.Cursor+"="+entry.Line)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("the reopened stream ended with %v", err)
	}
	if want := []string{"c1=one", "c2=two"}; !reflect.DeepEqual(received, want) {
		t.Fatalf("received %v, want %v", received, want)
	}
	// The reopening sends the original query and no position: the runtime never
	// synthesizes one in best-effort mode.
	for _, served := range []*instance{first, second} {
		if got := served.seen(); !reflect.DeepEqual(got, []string{"selector=svc%3Dapi"}) {
			t.Fatalf("queries = %v, want the original query alone", got)
		}
	}
}
`

// TestTheEmittedGoClientContinuesADeclaredSSECursorStreamThroughTheRealRuntime
// is the Go half of provider declaration → generation → compiled client → real
// call for an SSE continuation: the emitted package requires the capability,
// compiles against this repository's runtime, and its generated methods
// continue a cursor stream on another provider instance after the last
// position the caller received and reopen a best-effort stream with the
// original query.
func TestTheEmittedGoClientContinuesADeclaredSSECursorStreamThroughTheRealRuntime(t *testing.T) {
	spectest.Proves(t, "go/api-contracts", "declared-sse-continuation",
		"the-emitted-go-client-continues-a-declared-cursor-stream-through-the-real-runtime")
	spectest.Proves(t, "go/api-contracts", "declared-sse-continuation",
		"the-emitted-go-client-reopens-a-declared-best-effort-stream-through-the-real-runtime")
	spec := sseContinuationSpec()
	if got, want := specRuntimeCapabilities(spec), []clientcontract.RuntimeCapability{clientcontract.RuntimeCapabilitySSEContinuation}; !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime capabilities = %v, want %v", got, want)
	}
	source, err := GenerateClientFromIR(spec, ClientGenOptions{PackageName: "logsclient", ClientName: "LogsClient"})
	if err != nil {
		t.Fatalf("GenerateClientFromIR: %v", err)
	}
	if !strings.Contains(source, `client.RequireRuntimeCapabilities("sse-continuation")`) {
		t.Fatalf("the emitted package does not pin the sse-continuation capability:\n%s", source)
	}
	moduleDir := t.TempDir()
	writeGeneratedClientModule(t, moduleDir, source)
	if err := os.WriteFile(filepath.Join(moduleDir, "continuation_e2e_test.go"), []byte(generatedSSEContinuationE2E), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "test", "-count=1", "./...")
	command.Dir = moduleDir
	command.Env = append(os.Environ(), "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK=off")
	if output, testErr := command.CombinedOutput(); testErr != nil {
		t.Fatalf("the emitted continuation client failed against the real runtime: %v\n%s", testErr, output)
	}
}
