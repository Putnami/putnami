package grpc

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/api"
	perrors "go.putnami.dev/errors"
	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	connectwire "go.putnami.dev/protocol/clientcontract/connect"
	"go.putnami.dev/protocol/features/spectest"
)

type connectWidget struct {
	ID    string            `json:"id" validate:"required"`
	Count uint64            `json:"count" validate:"required"`
	Tags  []string          `json:"tags" validate:"required"`
	Meta  map[string]string `json:"meta" validate:"required"`
}

type connectWidgetParams struct {
	ID string `json:"id" validate:"required"`
}

// connectBridgeDescriptor is the descriptor the provider publishes for the
// routes below. It is hand-written here so this file tests the bridge, not the
// projection that produces a descriptor — connect_interop_test.go runs the real
// proto plugin end to end.
func connectBridgeDescriptor() *clientcontract.ProtobufDescriptor {
	widget := []clientcontract.ProtobufField{
		{Name: "id", JSONName: "id", Number: 1, TypeKind: "scalar", Type: "string"},
		{Name: "count", JSONName: "count", Number: 2, TypeKind: "scalar", Type: "uint64"},
		{Name: "tags", JSONName: "tags", Number: 3, TypeKind: "scalar", Type: "string", Repeated: true},
		{Name: "meta", JSONName: "meta", Number: 4, TypeKind: "map", Type: "map",
			Map: &clientcontract.ProtobufMap{KeyType: "string", ValueKind: "scalar", ValueType: "string"}},
	}
	return &clientcontract.ProtobufDescriptor{
		Syntax:  "proto3",
		Package: "widgets.v1",
		Services: []clientcontract.ProtobufService{{Name: "ApiService", Methods: []clientcontract.ProtobufMethod{
			{Name: "GetWidgets", Input: "GetWidgetsRequest", Output: "GetWidgetsReply"},
			{Name: "WatchWidgets", Input: "WatchWidgetsRequest", Output: "WatchWidgetsReply", ServerStreaming: true},
		}}},
		Messages: []clientcontract.ProtobufMessage{
			{Name: "GetWidgetsRequest", Fields: []clientcontract.ProtobufField{
				{Name: "params", JSONName: "params", Number: 1, TypeKind: "message", Type: "GetWidgetsParams"},
			}},
			{Name: "GetWidgetsParams", Fields: []clientcontract.ProtobufField{
				{Name: "id", JSONName: "id", Number: 1, TypeKind: "scalar", Type: "string"},
			}},
			{Name: "GetWidgetsReply", Fields: widget},
			{Name: "WatchWidgetsRequest", Fields: []clientcontract.ProtobufField{}},
			{Name: "WatchWidgetsReply", Fields: widget},
		},
		Enums: []clientcontract.ProtobufEnum{},
	}
}

// HandleStream lets the shared fake transport carry a declared stream endpoint.
// The api plugin refuses to bind one to a server that cannot, so a bridge test
// covering the streamed Connect codecs needs the fake to accept them.
func (f *fakeServer) HandleStream(path string, handler phttp.StreamHandler) {
	f.streams[path] = handler
}

type connectBridgeFixture struct {
	server *fakeServer
	bridge *ApiBridge
	plugin *api.Plugin
	// blocked is closed by the test to release the deadline endpoint.
	streamFailure error
}

// newConnectBridgeFixture mounts the bridge over three routes: a unary one, a
// declared server stream, and a stream that fails after its first message.
func newConnectBridgeFixture(t *testing.T) *connectBridgeFixture {
	t.Helper()
	fixture := &connectBridgeFixture{server: newFakeServer()}
	apiPlugin := api.New(fixture.server, api.WithClientService(api.ClientServiceOptions{
		Service: clientcontract.Service{ID: "widgets", Audience: "https://widgets.internal"},
	}))
	apiPlugin.Register(api.Endpoint("GET", "/widgets/{id}").
		Params(api.Type[connectWidgetParams]()).
		Returns(api.Type[connectWidget]()).
		MayThrow(perrors.CodeNotFound).
		Handle(func(ctx *phttp.EndpointContext) *phttp.Response {
			if ctx.Params["id"] == "missing" {
				return phttp.ErrorResponse(perrors.NotFound("no such widget"))
			}
			if ctx.Params["id"] == "slow" {
				<-ctx.Request.Context().Done()
				return phttp.ErrorResponse(perrors.New(perrors.CodeTimeout, "the caller's deadline expired"))
			}
			return phttp.JSON(connectWidget{
				ID: ctx.Params["id"], Count: 18446744073709551615,
				Tags: []string{"a", "b"}, Meta: map[string]string{"k": "v"},
			})
		}))
	apiPlugin.Register(api.Endpoint("GET", "/widgets/watch").
		Returns(api.StreamOf[connectWidget]()).
		Handle(api.ServerStream(func(stream *api.ServerStreamContext[connectWidget]) error {
			if err := stream.Send(connectWidget{ID: "one", Count: 1, Tags: []string{}, Meta: map[string]string{}}); err != nil {
				return err
			}
			if fixture.streamFailure != nil {
				return fixture.streamFailure
			}
			return stream.Send(connectWidget{ID: "two", Count: 2, Tags: []string{}, Meta: map[string]string{}})
		})))
	if err := apiPlugin.Configure(context.Background(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	apiPlugin.PublishClientProtobuf(api.ClientProtobufProjection{
		Descriptor: connectBridgeDescriptor(),
		RouteMethods: map[string]string{
			"GET /widgets/{id}":  "/widgets.v1.ApiService/GetWidgets",
			"GET /widgets/watch": "/widgets.v1.ApiService/WatchWidgets",
		},
	})
	bridge := NewApiBridge(apiPlugin, fixture.server, WithPackage("widgets.v1"), WithMaxMessageBytes(1<<16))
	if err := bridge.Configure(context.Background(), nil); err != nil {
		t.Fatalf("bridge Configure: %v", err)
	}
	if err := bridge.Start(context.Background(), nil); err != nil {
		t.Fatalf("bridge Start: %v", err)
	}
	fixture.bridge, fixture.plugin = bridge, apiPlugin
	return fixture
}

func (f *connectBridgeFixture) handler(t *testing.T, rpcPath string) phttp.Handler {
	t.Helper()
	handler, ok := f.server.calls["POST "+rpcPath]
	if !ok {
		t.Fatalf("no handler mounted at %s; have %v", rpcPath, sortedCallKeys(f.server))
	}
	return handler
}

func (f *connectBridgeFixture) call(t *testing.T, rpcPath, contentType string, body []byte, headers map[string]string) (*httptest.ResponseRecorder, *phttp.Response) {
	t.Helper()
	request := httptest.NewRequest("POST", rpcPath, bytes.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	response := f.handler(t, rpcPath)(phttp.NewContext(recorder, request))
	return recorder, response
}

// The bridge speaks four shapes and refuses anything else, rather than guessing
// a codec and answering with bytes the caller cannot read.
func TestConnectBridge_RefusesACodecItDoesNotServe(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "the-bridge-refuses-a-codec-it-does-not-serve")
	fixture := newConnectBridgeFixture(t)
	_, response := fixture.call(t, "/widgets.v1.ApiService/GetWidgets", "application/grpc+proto", nil, nil)
	if response == nil || response.Status != http.StatusUnsupportedMediaType {
		t.Fatalf("response = %v, want 415", response)
	}
	document := decodeConnectErrorBody(t, response)
	if document.Code != "unimplemented" {
		t.Errorf("code = %q, want unimplemented", document.Code)
	}
	// A streamed codec on a unary method, and a unary codec on a stream, are
	// both refusals: the cardinality is declared, not negotiated.
	_, streamed := fixture.call(t, "/widgets.v1.ApiService/GetWidgets", connectwire.StreamJSONContentType, nil, nil)
	if streamed == nil || streamed.Status != http.StatusUnsupportedMediaType {
		t.Errorf("a streamed codec on a unary method = %v, want 415", streamed)
	}
	_, unary := fixture.call(t, "/widgets.v1.ApiService/WatchWidgets", connectwire.UnaryJSONContentType, nil, nil)
	if unary == nil || unary.Status != http.StatusUnsupportedMediaType {
		t.Errorf("a unary codec on a stream = %v, want 415", unary)
	}
}

// A successful unary Connect response is HTTP 200 whatever the endpoint's
// declared status is, because that is what the specification says. The declared
// status is not lost: the operation contract carries it, and a first-party
// client restores it from there.
func TestConnectBridge_ServesTheUnaryProtoCodec(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "the-bridge-serves-the-published-proto-encoding")
	fixture := newConnectBridgeFixture(t)
	codec, err := connectwire.NewProtoCodec(connectBridgeDescriptor())
	if err != nil {
		t.Fatalf("connectwire.NewProtoCodec: %v", err)
	}
	request, err := codec.Encode("GetWidgetsRequest", []byte(`{"params":{"id":"w-1"}}`))
	if err != nil {
		t.Fatalf("Encode request: %v", err)
	}
	_, response := fixture.call(t, "/widgets.v1.ApiService/GetWidgets", connectwire.UnaryProtoContentType, request, nil)
	if response == nil || response.Status != http.StatusOK {
		t.Fatalf("response = %v, want 200", response)
	}
	if got := response.Headers.Get("Content-Type"); got != connectwire.UnaryProtoContentType {
		t.Errorf("content type = %q, want %q", got, connectwire.UnaryProtoContentType)
	}
	body, err := response.BodyBytes()
	if err != nil {
		t.Fatalf("BodyBytes: %v", err)
	}
	decoded, err := codec.Decode("GetWidgetsReply", body)
	if err != nil {
		t.Fatalf("Decode reply: %v", err)
	}
	// The uint64 survives past the JavaScript safe-integer range, which is the
	// value REST JSON already carries intact.
	const want = `{"id":"w-1","count":18446744073709551615,"tags":["a","b"],"meta":{"k":"v"}}`
	if string(decoded) != want {
		t.Errorf("reply = %s, want %s", decoded, want)
	}
}

// The Connect error document carries the gRPC code, and the stable first-party
// code plus the exact HTTP status travel beside it in google.rpc.ErrorInfo.
func TestConnectBridge_ProjectsTheFirstPartyErrorOntoTheConnectEnvelope(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "a-declared-error-travels-as-the-connect-error-document")
	fixture := newConnectBridgeFixture(t)
	_, response := fixture.call(t, "/widgets.v1.ApiService/GetWidgets", connectwire.UnaryJSONContentType,
		[]byte(`{"params":{"id":"missing"}}`), nil)
	if response == nil || response.Status != http.StatusNotFound {
		t.Fatalf("response = %v, want 404", response)
	}
	document := decodeConnectErrorBody(t, response)
	if document.Code != "not_found" {
		t.Errorf("connect code = %q, want not_found", document.Code)
	}
	if len(document.Details) == 0 || document.Details[0].Type != connectwire.FrameworkErrorType {
		t.Fatalf("details = %#v, want the first-party framework detail first", document.Details)
	}
	if strings.Contains(document.Details[0].Value, "=") {
		t.Errorf("detail value %q is padded; the specification says unpadded base64", document.Details[0].Value)
	}
	raw, err := connectwire.DetailBase64.DecodeString(document.Details[0].Value)
	if err != nil {
		t.Fatalf("detail is not unpadded standard base64: %v", err)
	}
	firstParty, err := connectwire.DecodeFrameworkError(raw)
	if err != nil {
		t.Fatalf("connectwire.DecodeFrameworkError: %v", err)
	}
	if firstParty.Code != string(perrors.CodeNotFound) {
		t.Errorf("detail code = %q, want the stable first-party code %q", firstParty.Code, perrors.CodeNotFound)
	}
	if firstParty.Status != http.StatusNotFound {
		t.Errorf("detail http_status = %d, want 404", firstParty.Status)
	}
	if firstParty.DetailsJSON != "" {
		t.Errorf("detail details_json = %q; this endpoint declares no details body", firstParty.DetailsJSON)
	}
}

// Connect-Timeout-Ms is the caller's bound, and the bridge applies it to the
// context the endpoint runs under. The endpoint below returns only when that
// context is done, so the test finishes on the declared budget and never on a
// wall-clock sleep.
func TestConnectBridge_AppliesTheCallersTimeoutHeader(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "the-bridge-applies-the-callers-connect-timeout")
	fixture := newConnectBridgeFixture(t)
	started := time.Now()
	_, response := fixture.call(t, "/widgets.v1.ApiService/GetWidgets", connectwire.UnaryJSONContentType,
		[]byte(`{"params":{"id":"slow"}}`), map[string]string{connectwire.TimeoutHeader: "40"})
	if response == nil {
		t.Fatal("the bounded endpoint produced no response")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the endpoint ran for %s; the caller's 40ms bound was not applied", elapsed)
	}
	if response.Status < 400 {
		t.Errorf("status = %d, want the endpoint's own failure once its context expired", response.Status)
	}
	_, malformed := fixture.call(t, "/widgets.v1.ApiService/GetWidgets", connectwire.UnaryJSONContentType,
		[]byte(`{"params":{"id":"w-1"}}`), map[string]string{connectwire.TimeoutHeader: "99999999999"})
	if malformed == nil || malformed.Status != http.StatusBadRequest {
		t.Errorf("an eleven-digit timeout = %v, want 400 rather than a silently ignored bound", malformed)
	}
}

// gzip is negotiated in both directions and bounded in the decompressed one.
func TestConnectBridge_CompressesWithinItsBudget(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "connect-compression-is-negotiated-and-bounded")
	fixture := newConnectBridgeFixture(t)
	compressed, err := connectwire.CompressGzip([]byte(`{"params":{"id":"w-1"}}`))
	if err != nil {
		t.Fatalf("connectwire.CompressGzip: %v", err)
	}
	_, response := fixture.call(t, "/widgets.v1.ApiService/GetWidgets", connectwire.UnaryJSONContentType, compressed,
		map[string]string{"Content-Encoding": connectwire.EncodingGzip})
	if response == nil || response.Status != http.StatusOK {
		t.Fatalf("a gzip request = %v, want 200", response)
	}
	// A gzip bomb is refused on the declared budget rather than allocated for.
	bomb, err := connectwire.CompressGzip(bytes.Repeat([]byte{'a'}, 1<<20))
	if err != nil {
		t.Fatalf("connectwire.CompressGzip: %v", err)
	}
	_, refused := fixture.call(t, "/widgets.v1.ApiService/GetWidgets", connectwire.UnaryJSONContentType, bomb,
		map[string]string{"Content-Encoding": connectwire.EncodingGzip})
	if refused == nil || refused.Status != http.StatusBadRequest {
		t.Errorf("a payload past the 64KiB budget = %v, want 400", refused)
	}
}

// A declared server stream is enveloped messages ended by exactly one
// EndStreamResponse, and nothing follows it.
func TestConnectBridge_ServesAServerStreamWithOneTerminalEnvelope(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "a-server-stream-ends-with-exactly-one-endstreamresponse")
	fixture := newConnectBridgeFixture(t)
	recorder, response := fixture.call(t, "/widgets.v1.ApiService/WatchWidgets", connectwire.StreamJSONContentType,
		connectwire.AppendEnvelope(nil, 0, []byte(`{}`)), nil)
	if response != nil {
		t.Fatalf("a streamed response writes itself; handler returned %v", response)
	}
	if got := recorder.Header().Get("Content-Type"); got != connectwire.StreamJSONContentType {
		t.Fatalf("content type = %q, want %q", got, connectwire.StreamJSONContentType)
	}
	messages, end := readConnectStreamBody(t, recorder.Body.Bytes())
	if len(messages) != 2 {
		t.Fatalf("messages = %v, want two", messages)
	}
	if !strings.Contains(messages[0], `"id":"one"`) || !strings.Contains(messages[1], `"id":"two"`) {
		t.Errorf("messages = %v", messages)
	}
	if end.Error != nil {
		t.Errorf("EndStreamResponse carries an error on a clean stream: %#v", end.Error)
	}
}

// A stream that fails after admission reports the failure in the terminal
// envelope, because the response headers are already written.
func TestConnectBridge_ReportsAStreamFailureInTheTerminalEnvelope(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "a-server-stream-ends-with-exactly-one-endstreamresponse")
	fixture := newConnectBridgeFixture(t)
	fixture.streamFailure = perrors.NotFound("the widget vanished mid-stream")
	recorder, _ := fixture.call(t, "/widgets.v1.ApiService/WatchWidgets", connectwire.StreamJSONContentType,
		connectwire.AppendEnvelope(nil, 0, []byte(`{}`)), nil)
	messages, end := readConnectStreamBody(t, recorder.Body.Bytes())
	if len(messages) != 1 {
		t.Fatalf("messages = %v, want the one delivered before the failure", messages)
	}
	if end.Error == nil || end.Error.Code != "not_found" {
		t.Fatalf("EndStreamResponse = %#v, want the not_found code", end.Error)
	}
}

// Only the server ends a Connect stream, and a server stream carries one
// request message. Both rules are refusals, not tolerated extras.
func TestConnectBridge_RefusesAMalformedStreamRequest(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "a-server-stream-ends-with-exactly-one-endstreamresponse")
	fixture := newConnectBridgeFixture(t)
	_, terminal := fixture.call(t, "/widgets.v1.ApiService/WatchWidgets", connectwire.StreamJSONContentType,
		connectwire.AppendEnvelope(nil, connectwire.FlagEndStream, []byte(`{}`)), nil)
	if terminal == nil || terminal.Status != http.StatusBadRequest {
		t.Errorf("a caller-sent EndStreamResponse = %v, want 400", terminal)
	}
	two := connectwire.AppendEnvelope(nil, 0, []byte(`{}`))
	two = connectwire.AppendEnvelope(two, 0, []byte(`{}`))
	_, extra := fixture.call(t, "/widgets.v1.ApiService/WatchWidgets", connectwire.StreamJSONContentType, two, nil)
	if extra == nil || extra.Status != http.StatusBadRequest {
		t.Errorf("a second request message = %v, want 400", extra)
	}
	_, reserved := fixture.call(t, "/widgets.v1.ApiService/WatchWidgets", connectwire.StreamJSONContentType,
		[]byte{0x80, 0, 0, 0, 0}, nil)
	if reserved == nil || reserved.Status != http.StatusBadRequest {
		t.Errorf("a reserved flag bit = %v, want 400", reserved)
	}
}

// The status projection is the one place the mapping is a choice rather than a
// derivation, so it is pinned.
func TestConnectStatusForHTTPNamesOneCodePerStatus(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "a-declared-error-travels-as-the-connect-error-document")
	cases := map[int]string{
		400: "invalid_argument", 401: "unauthenticated", 403: "permission_denied", 404: "not_found",
		409: "already_exists", 408: "deadline_exceeded", 429: "resource_exhausted", 501: "unimplemented",
		503: "unavailable", 504: "deadline_exceeded", 418: "invalid_argument", 502: "internal", 200: "unknown",
	}
	for status, want := range cases {
		if got := connectStatusForHTTP(status).Name; got != want {
			t.Errorf("connectStatusForHTTP(%d) = %q, want %q", status, got, want)
		}
	}
}

func decodeConnectErrorBody(t *testing.T, response *phttp.Response) connectwire.ErrorEnvelope {
	t.Helper()
	body, err := response.BodyBytes()
	if err != nil {
		t.Fatalf("BodyBytes: %v", err)
	}
	var document connectwire.ErrorEnvelope
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("error body %s is not a Connect error document: %v", body, err)
	}
	return document
}

// readConnectStreamBody reads a streamed response the way a conforming client
// does, and proves the terminal envelope is last and unique.
func readConnectStreamBody(t *testing.T, body []byte) ([]string, connectwire.EndStreamResponse) {
	t.Helper()
	reader := bytes.NewReader(body)
	var messages []string
	var end connectwire.EndStreamResponse
	seenEnd := false
	for reader.Len() > 0 {
		flags, payload, err := connectwire.ReadEnvelope(reader, 1<<20)
		if err != nil {
			t.Fatalf("stream body is malformed at offset %d: %v (%s)", len(body)-reader.Len(), err, hex.EncodeToString(body))
		}
		if flags&connectwire.FlagEndStream != 0 {
			if seenEnd {
				t.Fatal("the stream carries more than one EndStreamResponse")
			}
			seenEnd = true
			if err := json.Unmarshal(payload, &end); err != nil {
				t.Fatalf("EndStreamResponse %s is not JSON: %v", payload, err)
			}
			continue
		}
		if seenEnd {
			t.Fatal("the stream carries data after its EndStreamResponse")
		}
		messages = append(messages, string(payload))
	}
	if !seenEnd {
		t.Fatal("the stream ended without an EndStreamResponse")
	}
	return messages, end
}

// deadlineRecorder is a ResponseWriter that records what the handler asked of
// its write deadline. The server-wide deadline exists to bound one response; a
// stream that outlives it is not a slow response, so the handler must clear it
// and re-arm a bounded one per write instead.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (recorder *deadlineRecorder) SetWriteDeadline(at time.Time) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.deadlines = append(recorder.deadlines, at)
	return nil
}

func (recorder *deadlineRecorder) recorded() []time.Time {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]time.Time(nil), recorder.deadlines...)
}

func TestConnectBridge_ReplacesTheServerWideWriteDeadline(t *testing.T) {
	spectest.Proves(t, "go/grpc-services", "connect-protocol", "a-server-stream-ends-with-exactly-one-endstreamresponse")
	fixture := newConnectBridgeFixture(t)
	request := httptest.NewRequest("POST", "/widgets.v1.ApiService/WatchWidgets",
		bytes.NewReader(connectwire.AppendEnvelope(nil, 0, []byte(`{}`))))
	request.Header.Set("Content-Type", connectwire.StreamJSONContentType)
	recorder := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	started := time.Now()
	fixture.handler(t, "/widgets.v1.ApiService/WatchWidgets")(phttp.NewContext(recorder, request))

	deadlines := recorder.recorded()
	if len(deadlines) < 4 {
		t.Fatalf("write deadlines = %v; want the clear, then one per envelope", deadlines)
	}
	if !deadlines[0].IsZero() {
		t.Errorf("first deadline = %v, want the zero time that clears the server-wide bound", deadlines[0])
	}
	for index, at := range deadlines[1:] {
		if at.IsZero() {
			t.Errorf("deadline %d is the zero time; every write after the clear is bounded", index+1)
			continue
		}
		if bound := at.Sub(started); bound <= 0 || bound > 2*defaultConnectWriteTimeout {
			t.Errorf("deadline %d is %s away, want a bound near %s", index+1, bound, defaultConnectWriteTimeout)
		}
	}
	// The two messages and the terminal all went out under a re-armed bound.
	messages, end := readConnectStreamBody(t, recorder.Body.Bytes())
	if len(messages) != 2 || end.Error != nil {
		t.Errorf("stream = %v, %#v", messages, end.Error)
	}
}
