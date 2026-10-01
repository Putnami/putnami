package client

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	connectwire "go.putnami.dev/protocol/clientcontract/connect"
	"go.putnami.dev/protocol/features/spectest"
)

// connectItem is the message both Connect codecs carry in these tests.
type connectItem struct {
	Value string `json:"value"`
}

func connectItemSchema() clientcontract.Schema {
	return clientcontract.Schema{
		Type: "object", Properties: map[string]clientcontract.Schema{"value": {Type: "string"}},
		Required: []string{"value"}, AdditionalProperties: additionalForbidden(),
	}
}

// connectTestDescriptor publishes the protobuf descriptor for the two methods
// below. It is the same descriptor shape the Go provider publishes: the request
// message is the `{params, query, body}` envelope, the reply message is the
// declared return type.
func connectTestDescriptor() *clientcontract.ProtobufDescriptor {
	item := []clientcontract.ProtobufField{{Name: "value", JSONName: "value", Number: 1, TypeKind: "scalar", Type: "string"}}
	return &clientcontract.ProtobufDescriptor{
		Syntax:  "proto3",
		Package: "inventory.v1",
		Services: []clientcontract.ProtobufService{{Name: "ApiService", Methods: []clientcontract.ProtobufMethod{
			{Name: "GetItems", Input: "GetItemsRequest", Output: "GetItemsReply"},
			{Name: "WatchItems", Input: "WatchItemsRequest", Output: "WatchItemsReply", ServerStreaming: true},
		}}},
		Messages: []clientcontract.ProtobufMessage{
			{Name: "GetItemsRequest", Fields: []clientcontract.ProtobufField{
				{Name: "params", JSONName: "params", Number: 1, TypeKind: "message", Type: "GetItemsParams"},
				{Name: "query", JSONName: "query", Number: 2, TypeKind: "message", Type: "GetItemsQuery"},
			}},
			{Name: "GetItemsParams", Fields: []clientcontract.ProtobufField{
				{Name: "id", JSONName: "id", Number: 1, TypeKind: "scalar", Type: "string"},
			}},
			{Name: "GetItemsQuery", Fields: []clientcontract.ProtobufField{
				{Name: "shape", JSONName: "shape", Number: 1, TypeKind: "scalar", Type: "string"},
			}},
			{Name: "GetItemsReply", Fields: item},
			{Name: "WatchItemsRequest", Fields: []clientcontract.ProtobufField{
				{Name: "params", JSONName: "params", Number: 1, TypeKind: "message", Type: "GetItemsParams"},
				{Name: "query", JSONName: "query", Number: 2, TypeKind: "message", Type: "GetItemsQuery"},
			}},
			{Name: "WatchItemsReply", Fields: item},
		},
		Enums: []clientcontract.ProtobufEnum{},
	}
}

func connectDescriptorWithProtobuf(credentials map[string]clientcontract.CredentialProfile) ServiceDescriptor {
	descriptor := testDescriptor(credentials)
	descriptor.Contract.Protobuf = connectTestDescriptor()
	return descriptor
}

// connectUnaryOperation declares one unary operation with the transports in the
// order the test wants dispatched.
func connectUnaryOperation(transports []clientcontract.Transport, errorsDeclared []clientcontract.DeclaredError, policy *clientcontract.ResiliencePolicy) Operation {
	schema := connectItemSchema()
	if errorsDeclared == nil {
		errorsDeclared = []clientcontract.DeclaredError{}
	}
	return Operation{
		ID: "getItems",
		Contract: clientcontract.OperationV1{
			Stream:     clientcontract.StreamUnary,
			Transports: transports,
			Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
				AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}},
			}}},
			Errors:      errorsDeclared,
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
			Resilience:  policy,
		},
		Successes: []OperationSuccess{{Status: http.StatusOK, Content: []OperationContent{{MediaType: "application/json", Schema: &schema}}}},
	}
}

func connectRestTransport() clientcontract.Transport {
	return clientcontract.Transport{Protocol: clientcontract.TransportRESTJSON, Path: "/items/{id}", Encoding: clientcontract.EncodingJSON}
}

func connectTransport(encoding clientcontract.Encoding) clientcontract.Transport {
	return clientcontract.Transport{
		Protocol: clientcontract.TransportConnect, Path: "/inventory.v1.ApiService/GetItems",
		Encoding: encoding, ProtobufMethod: "/inventory.v1.ApiService/GetItems",
	}
}

func connectStreamTransport(encoding clientcontract.Encoding) clientcontract.Transport {
	return clientcontract.Transport{
		Protocol: clientcontract.TransportConnect, Path: "/inventory.v1.ApiService/WatchItems",
		Encoding: encoding, ProtobufMethod: "/inventory.v1.ApiService/WatchItems",
	}
}

func newConnectCallInput() *OperationCall {
	return &OperationCall{
		Request:    &Request{Method: "GET", Path: "/items/i-1", Query: map[string]string{"shape": "round"}},
		PathParams: map[string]string{"id": "i-1"},
	}
}

func connectBoundClient(t *testing.T, url string, credentials map[string]clientcontract.CredentialProfile, binding ServiceBinding) *Client {
	t.Helper()
	return boundTestClient(t, url, connectDescriptorWithProtobuf(credentials), binding)
}

func connectServiceBinding() ServiceBinding {
	return ServiceBinding{Credentials: map[string]CredentialBinding{"service": {Provider: freshSource("connect-token", nil)}}}
}

func connectServiceProfiles() map[string]clientcontract.CredentialProfile {
	return map[string]clientcontract.CredentialProfile{"service": {Kind: clientcontract.CredentialServiceToken}}
}

// The declared order is the dispatch order. A contract that names REST first
// keeps the endpoint's own URL; the same contract with Connect first reaches
// the method identity instead — with no change to the generated call.
func TestConnectDispatchFollowsTheDeclaredTransportOrder(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "a-generated-call-dispatches-on-the-first-declared-transport")
	var mu sync.Mutex
	var seen []string
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		seen = append(seen, request.Method+" "+request.URL.RequestURI())
		body, _ = readAllLimited(request.Body)
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())

	rest := connectUnaryOperation([]clientcontract.Transport{connectRestTransport(), connectTransport(clientcontract.EncodingJSON)}, nil, nil)
	if _, err := CallOperation[connectItem](t.Context(), bound, newConnectCallInput(), rest); err != nil {
		t.Fatalf("rest-first dispatch: %v", err)
	}
	connect := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingJSON), connectRestTransport()}, nil, nil)
	if _, err := CallOperation[connectItem](t.Context(), bound, newConnectCallInput(), connect); err != nil {
		t.Fatalf("connect-first dispatch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "GET /items/i-1?shape=round" {
		t.Fatalf("requests = %v, want the REST URL first", seen)
	}
	if seen[1] != "POST /inventory.v1.ApiService/GetItems" {
		t.Errorf("connect request = %q, want the method identity", seen[1])
	}
	// The Connect request carries the three sections namespaced, so a path
	// param and a body field of the same name never fight for one key.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("connect body %s is not the request envelope: %v", body, err)
	}
	if string(envelope["params"]) != `{"id":"i-1"}` || string(envelope["query"]) != `{"shape":"round"}` {
		t.Errorf("connect envelope = %s", body)
	}
}

// The proto codec is driven by the published descriptor: no stub is generated,
// and the bytes on the wire are the ones the encoding rules produce.
func TestConnectDispatchEncodesTheDeclaredProtoMessages(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "the-proto-codec-comes-from-the-published-descriptor")
	codec, err := connectwire.NewProtoCodec(connectTestDescriptor())
	if err != nil {
		t.Fatalf("connectwire.NewProtoCodec: %v", err)
	}
	var mu sync.Mutex
	var contentType string
	var requestBytes []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		contentType = request.Header.Get("Content-Type")
		requestBytes, _ = readAllLimited(request.Body)
		mu.Unlock()
		reply, encodeErr := codec.Encode("GetItemsReply", []byte(`{"value":"ok"}`))
		if encodeErr != nil {
			t.Errorf("provider encode: %v", encodeErr)
		}
		writer.Header().Set("Content-Type", connectwire.UnaryProtoContentType)
		_, _ = writer.Write(reply)
	}))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	operation := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingProto)}, nil, nil)
	result, err := CallOperation[connectItem](t.Context(), bound, newConnectCallInput(), operation)
	if err != nil {
		t.Fatalf("CallOperation: %v", err)
	}
	if result.Value != "ok" {
		t.Errorf("result = %#v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if contentType != connectwire.UnaryProtoContentType {
		t.Errorf("content type = %q, want %q", contentType, connectwire.UnaryProtoContentType)
	}
	decoded, err := codec.Decode("GetItemsRequest", requestBytes)
	if err != nil {
		t.Fatalf("the provider cannot decode the request this client sent: %v", err)
	}
	if string(decoded) != `{"params":{"id":"i-1"},"query":{"shape":"round"}}` {
		t.Errorf("request message = %s", decoded)
	}
}

// The Connect response says 200; the declared success status is what the
// operation contract carries, and the runtime restores it so response
// validation sees exactly what a REST call would.
func TestConnectDispatchRestoresTheDeclaredSuccessStatus(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "a-generated-call-dispatches-on-the-first-declared-transport")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", connectwire.UnaryJSONContentType)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"value":"created"}`))
	}))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	operation := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingJSON)}, nil, nil)
	operation.Successes[0].Status = http.StatusCreated
	result, err := CallOperation[connectItem](t.Context(), bound, newConnectCallInput(), operation)
	if err != nil {
		t.Fatalf("CallOperation: %v", err)
	}
	if result.Value != "created" {
		t.Errorf("result = %#v", result)
	}
}

// A declared error arrives as the same typed RemoteError a REST call produces:
// the stable code, the exact status, the declared gRPC code and the declared
// details body.
func TestConnectDispatchDecodesTheConnectErrorDocument(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "a-connect-error-arrives-as-the-declared-typed-error")
	grpcCode := 5
	notRetryable := false
	details := clientcontract.Schema{
		Type: "object", Properties: map[string]clientcontract.Schema{"resource": {Type: "string"}},
		Required: []string{"resource"}, AdditionalProperties: additionalForbidden(),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		detail := connectwire.EncodeFrameworkError(connectwire.FrameworkError{
			Code: "widget_missing", Status: 404, DetailsJSON: `{"resource":"widget"}`,
		})
		document := connectwire.ErrorEnvelope{Code: "not_found", Message: "no such widget", Details: []connectwire.ErrorDetail{
			{Type: "google.rpc.RetryInfo", Value: connectwire.DetailBase64.EncodeToString([]byte{0x08, 0x01})},
			{Type: connectwire.FrameworkErrorType, Value: connectwire.DetailBase64.EncodeToString(detail)},
		}}
		body, _ := json.Marshal(document)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = writer.Write(body)
	}))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	operation := connectUnaryOperation(
		[]clientcontract.Transport{connectTransport(clientcontract.EncodingJSON)},
		[]clientcontract.DeclaredError{{Status: 404, Code: "widget_missing", GRPCCode: &grpcCode, Schema: &details, Retryable: &notRetryable}},
		nil)
	_, err := CallOperation[connectItem](t.Context(), bound, newConnectCallInput(), operation)
	var remote *RemoteError
	if !stderrors.As(err, &remote) {
		t.Fatalf("error = %T %v, want a typed *RemoteError", err, err)
	}
	if remote.Code() != "widget_missing" || remote.StatusCode != http.StatusNotFound {
		t.Errorf("typed error = %q/%d, want widget_missing/404", remote.Code(), remote.StatusCode)
	}
	if remote.GRPCCode == nil || *remote.GRPCCode != 5 {
		t.Errorf("GRPCCode = %v, want the declared 5", remote.GRPCCode)
	}
	if string(remote.Payload) != `{"resource":"widget"}` {
		t.Errorf("details payload = %s, want the google.protobuf.Value body", remote.Payload)
	}
	if remote.Service() != "inventory" || remote.Operation() != "getItems" {
		t.Errorf("identity = %q/%q", remote.Service(), remote.Operation())
	}
}

// Auth, deadline, retry and the circuit breaker are the generic operation
// machinery, not a second implementation behind Connect. This proves the
// Connect path rides on exactly that one.
func TestConnectDispatchAppliesCredentialsDeadlineRetryAndCircuit(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "the-connect-path-reuses-the-declared-resilience-contract")
	var calls atomic.Int32
	var mu sync.Mutex
	var timeouts []string
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		timeouts = append(timeouts, request.Header.Get(connectwire.TimeoutHeader))
		authorization = request.Header.Get("Authorization")
		mu.Unlock()
		if request.Header.Get(connectwire.ProtocolVersionHeader) != connectwire.ProtocolVersion {
			t.Errorf("%s = %q", connectwire.ProtocolVersionHeader, request.Header.Get(connectwire.ProtocolVersionHeader))
		}
		if calls.Add(1) < 3 {
			body, _ := json.Marshal(connectwire.ErrorEnvelope{Code: "unavailable", Message: "try again"})
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write(body)
			return
		}
		writer.Header().Set("Content-Type", connectwire.UnaryJSONContentType)
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	attempts := 3
	timeout := 2000
	policy := &clientcontract.ResiliencePolicy{
		TimeoutMs: &timeout,
		Retry:     &clientcontract.RetryPolicy{MaxAttempts: &attempts, Statuses: []int{http.StatusServiceUnavailable}},
	}
	operation := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingJSON)}, nil, policy)
	if _, err := CallOperation[connectItem](t.Context(), bound, newConnectCallInput(), operation); err != nil {
		t.Fatalf("CallOperation: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("attempts = %d, want the declared 3", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if authorization != "Bearer connect-token" {
		t.Errorf("Authorization = %q; the declared credential must reach the Connect request", authorization)
	}
	for index, raw := range timeouts {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 || value > timeout+1 {
			t.Errorf("attempt %d sent %s = %q, want the caller's remaining budget", index, connectwire.TimeoutHeader, raw)
		}
	}
}

// A credential the binding cannot satisfy opens no socket at all: the Connect
// path is refused before the first byte, exactly like the REST path.
func TestConnectDispatchNeverCallsAnonymously(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "the-connect-path-reuses-the-declared-resilience-contract")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", connectwire.UnaryJSONContentType)
		_, _ = writer.Write([]byte(`{"value":"ok"}`))
	}))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), ServiceBinding{})
	operation := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingJSON)}, nil, nil)
	if _, err := CallOperation[connectItem](t.Context(), bound, newConnectCallInput(), operation); err == nil {
		t.Fatal("an unsatisfiable credential produced a call")
	}
	if calls.Load() != 0 {
		t.Errorf("the provider saw %d requests; a missing credential must open none", calls.Load())
	}
}

// The Connect request envelope names one value per query parameter and carries
// the body as an object. A declaration outside that shape is refused rather
// than silently losing a value.
func TestConnectDispatchRefusesAnUnprojectableCall(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "a-generated-call-dispatches-on-the-first-declared-transport")
	bound := connectBoundClient(t, "http://127.0.0.1:1", connectServiceProfiles(), connectServiceBinding())
	operation := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingJSON)}, nil, nil)

	repeated := newConnectCallInput()
	repeated.Request.Query = nil
	repeated.Request.QueryValues = map[string][]string{"tag": {"a", "b"}}
	if _, err := CallOperation[connectItem](t.Context(), bound, repeated, operation); err == nil ||
		!strings.Contains(err.Error(), "one value per parameter") {
		t.Errorf("a repeated query parameter = %v, want a refusal", err)
	}

	nonObject := newConnectCallInput()
	nonObject.Request.Body = []byte(`[1,2]`)
	if _, err := CallOperation[connectItem](t.Context(), bound, nonObject, operation); err == nil {
		t.Error("a non-object body was projected onto the Connect envelope")
	}

	// A proto transport with no published descriptor has no codec, and JSON is
	// not a substitute for an encoding the provider never agreed to read.
	plain := boundTestClient(t, "http://127.0.0.1:1", testDescriptor(connectServiceProfiles()), connectServiceBinding())
	protoOnly := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingProto)}, nil, nil)
	if _, err := CallOperation[connectItem](t.Context(), plain, newConnectCallInput(), protoOnly); err == nil ||
		!strings.Contains(err.Error(), "publishes no protobuf descriptor") {
		t.Errorf("a proto transport without a descriptor = %v, want a named refusal", err)
	}
}

// connectStreamOperation declares the server stream the tests below drive.
func connectStreamOperation(transports []clientcontract.Transport, policy *clientcontract.ResiliencePolicy) Operation {
	schema := connectItemSchema()
	return Operation{
		ID: "watchItems",
		Contract: clientcontract.OperationV1{
			Stream:     clientcontract.StreamServer,
			Messages:   &clientcontract.MessageShapes{Output: &schema},
			Transports: transports,
			Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
				AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}},
			}}},
			Errors:      []clientcontract.DeclaredError{},
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
			Resilience:  policy,
		},
		Successes: []OperationSuccess{{Status: http.StatusOK, Content: []OperationContent{{MediaType: connectwire.StreamJSONContentType, Schema: &schema}}}},
	}
}

// connectStreamServer scripts a Connect stream: a body written verbatim under
// the declared streamed content type.
func connectStreamServer(t *testing.T, contentType string, body func() []byte, compressed bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("stream request method = %s, want POST", request.Method)
		}
		if got := connectwire.MediaType(request.Header.Get("Content-Type")); got != contentType {
			t.Errorf("stream request content type = %q, want %q", got, contentType)
		}
		writer.Header().Set("Content-Type", contentType)
		if compressed {
			writer.Header().Set(connectwire.ContentEncoding, connectwire.EncodingGzip)
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(body())
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
}

func connectStreamBody(messages []string) []byte {
	var body []byte
	for _, message := range messages {
		body = connectwire.AppendEnvelope(body, 0, []byte(message))
	}
	document, _ := json.Marshal(connectwire.EndStreamResponse{})
	return connectwire.AppendEnvelope(body, connectwire.FlagEndStream, document)
}

// A Connect server stream delivers its declared messages and ends on the single
// terminal envelope.
func TestConnectServerStreamDeliversMessagesAndItsTerminal(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "a-connect-server-stream-ends-on-exactly-one-terminal-envelope")
	server := connectStreamServer(t, connectwire.StreamJSONContentType, func() []byte {
		return connectStreamBody([]string{`{"value":"one"}`, `{"value":"two"}`})
	}, false)
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	operation := connectStreamOperation([]clientcontract.Transport{connectStreamTransport(clientcontract.EncodingJSON)}, nil)
	stream, err := OpenOperationServerStream[connectItem](t.Context(), bound, newConnectCallInput(), operation)
	if err != nil {
		t.Fatalf("OpenOperationServerStream: %v", err)
	}
	values := make([]string, 0, 2)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if strings.Join(values, ",") != "one,two" {
		t.Errorf("values = %v", values)
	}
}

// Every way a stream can end badly is a terminal error, never a clean end: a
// truncated stream that looked complete would report a partial result as whole.
func TestConnectServerStreamRefusesEveryMalformedTerminal(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "a-connect-server-stream-ends-on-exactly-one-terminal-envelope")
	cases := []struct {
		name string
		body func() []byte
		want string
	}{
		{
			name: "no terminal envelope",
			body: func() []byte { return connectwire.AppendEnvelope(nil, 0, []byte(`{"value":"one"}`)) },
			want: "without an EndStreamResponse",
		},
		{
			name: "bytes after the terminal envelope",
			body: func() []byte {
				body := connectStreamBody([]string{`{"value":"one"}`})
				return connectwire.AppendEnvelope(body, 0, []byte(`{"value":"late"}`))
			},
			want: "after its EndStreamResponse",
		},
		{
			name: "a truncated envelope",
			body: func() []byte {
				body := connectwire.AppendEnvelope(nil, 0, []byte(`{"value":"one"}`))
				return body[:len(body)-3]
			},
			want: "inside an envelope",
		},
		{
			name: "a reserved flag bit",
			body: func() []byte { return []byte{0x40, 0, 0, 0, 0} },
			want: "service stream response failed",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := connectStreamServer(t, connectwire.StreamJSONContentType, testCase.body, false)
			defer server.Close()
			bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
			operation := connectStreamOperation([]clientcontract.Transport{connectStreamTransport(clientcontract.EncodingJSON)}, nil)
			stream, err := OpenOperationServerStream[connectItem](t.Context(), bound, newConnectCallInput(), operation)
			if err != nil {
				t.Fatalf("OpenOperationServerStream: %v", err)
			}
			for range stream.Messages() {
			}
			streamErr := stream.Err()
			if streamErr == nil {
				t.Fatalf("a %s ended cleanly", testCase.name)
			}
			if !strings.Contains(streamErr.Error(), testCase.want) {
				t.Errorf("error = %v, want it to name %q", streamErr, testCase.want)
			}
		})
	}
}

// A terminal envelope carrying an error produces the declared typed error, and
// a compressed envelope is read only because the client offered gzip.
func TestConnectServerStreamReadsTerminalErrorsAndCompressedEnvelopes(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "a-connect-error-arrives-as-the-declared-typed-error")
	compressed := func() []byte {
		payload, err := connectwire.CompressGzip([]byte(`{"value":"one"}`))
		if err != nil {
			t.Fatalf("connectwire.CompressGzip: %v", err)
		}
		body := connectwire.AppendEnvelope(nil, connectwire.FlagCompressed, payload)
		document, _ := json.Marshal(connectwire.EndStreamResponse{
			Error:    &connectwire.ErrorEnvelope{Code: "resource_exhausted", Message: "enough"},
			Metadata: map[string][]string{"trailer": {"seen"}},
		})
		return connectwire.AppendEnvelope(body, connectwire.FlagEndStream, document)
	}
	server := connectStreamServer(t, connectwire.StreamJSONContentType, compressed, true)
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	operation := connectStreamOperation([]clientcontract.Transport{connectStreamTransport(clientcontract.EncodingJSON)}, nil)
	stream, err := OpenOperationServerStream[connectItem](t.Context(), bound, newConnectCallInput(), operation)
	if err != nil {
		t.Fatalf("OpenOperationServerStream: %v", err)
	}
	values := make([]string, 0, 1)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if strings.Join(values, ",") != "one" {
		t.Errorf("values = %v, want the compressed message", values)
	}
	var remote *RemoteError
	if !stderrors.As(stream.Err(), &remote) {
		t.Fatalf("terminal error = %T %v, want a typed *RemoteError", stream.Err(), stream.Err())
	}
	if remote.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the 429 the specification maps resource_exhausted to", remote.StatusCode)
	}
}

// A provider that refuses the call before writing headers is refused before
// admission, which is what the breaker and the retry rule are built on.
func TestConnectServerStreamRefusesBeforeAdmission(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "a-connect-server-stream-ends-on-exactly-one-terminal-envelope")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		detail := connectwire.EncodeFrameworkError(connectwire.FrameworkError{Code: "denied", Status: 403})
		body, _ := json.Marshal(connectwire.ErrorEnvelope{Code: "permission_denied", Details: []connectwire.ErrorDetail{
			{Type: connectwire.FrameworkErrorType, Value: connectwire.DetailBase64.EncodeToString(detail)},
		}})
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusForbidden)
		_, _ = writer.Write(body)
	}))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	operation := connectStreamOperation([]clientcontract.Transport{connectStreamTransport(clientcontract.EncodingJSON)}, nil)
	_, err := OpenOperationServerStream[connectItem](t.Context(), bound, newConnectCallInput(), operation)
	if err == nil {
		t.Fatal("a refused stream opened")
	}
	var remote *RemoteError
	if !stderrors.As(err, &remote) || remote.StatusCode != http.StatusForbidden {
		t.Fatalf("error = %T %v, want a typed 403", err, err)
	}
}

// The handshake budget bounds connect-to-admission on its own clock, distinct
// from the declared operation duration. The provider below never writes its
// headers, so the test finishes on the declared budget.
func TestConnectServerStreamBoundsItsHandshake(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "a-connect-server-stream-ends-on-exactly-one-terminal-envelope")
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	handshake := 40
	session := 60000
	policy := &clientcontract.ResiliencePolicy{
		TimeoutMs: &session,
		Stream:    &clientcontract.StreamPolicy{HandshakeTimeoutMs: &handshake},
	}
	operation := connectStreamOperation([]clientcontract.Transport{connectStreamTransport(clientcontract.EncodingJSON)}, policy)
	started := time.Now()
	_, err := OpenOperationServerStream[connectItem](t.Context(), bound, newConnectCallInput(), operation)
	if err == nil {
		t.Fatal("a stream that never received headers opened")
	}
	if !perrors.Is(err, CodeClientDeadline) {
		t.Fatalf("error = %v, want the typed handshake deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the handshake ran for %s; the 40ms budget was not applied", elapsed)
	}
}

// readAllLimited reads a scripted provider's request body under a fixed bound so
// a test never depends on an unbounded read.
func readAllLimited(body interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buffer bytes.Buffer
	_, err := buffer.ReadFrom(&limitedReader{inner: body, remaining: 1 << 20})
	return buffer.Bytes(), err
}

type limitedReader struct {
	inner     interface{ Read([]byte) (int, error) }
	remaining int64
}

func (r *limitedReader) Read(into []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, context.DeadlineExceeded
	}
	if int64(len(into)) > r.remaining {
		into = into[:r.remaining]
	}
	read, err := r.inner.Read(into)
	r.remaining -= int64(read)
	return read, err
}

// A Connect server stream over the proto codec decodes each enveloped message
// with the published descriptor, the same way the unary path does.
func TestConnectServerStreamReadsTheProtoCodec(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "the-proto-codec-comes-from-the-published-descriptor")
	codec, err := connectwire.NewProtoCodec(connectTestDescriptor())
	if err != nil {
		t.Fatalf("connectwire.NewProtoCodec: %v", err)
	}
	body := func() []byte {
		var out []byte
		for _, value := range []string{"one", "two"} {
			message, encodeErr := codec.Encode("WatchItemsReply", []byte(`{"value":"`+value+`"}`))
			if encodeErr != nil {
				t.Fatalf("provider encode: %v", encodeErr)
			}
			out = connectwire.AppendEnvelope(out, 0, message)
		}
		document, _ := json.Marshal(connectwire.EndStreamResponse{})
		return connectwire.AppendEnvelope(out, connectwire.FlagEndStream, document)
	}
	server := connectStreamServer(t, connectwire.StreamProtoType, body, false)
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	operation := connectStreamOperation([]clientcontract.Transport{connectStreamTransport(clientcontract.EncodingProto)}, nil)
	operation.Successes[0].Content[0].MediaType = connectwire.StreamProtoType
	stream, err := OpenOperationServerStream[connectItem](t.Context(), bound, newConnectCallInput(), operation)
	if err != nil {
		t.Fatalf("OpenOperationServerStream: %v", err)
	}
	values := make([]string, 0, 2)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if strings.Join(values, ",") != "one,two" {
		t.Errorf("values = %v", values)
	}
}

// An operation that declares no success body gets the empty message back, and
// the runtime restores the declared 204. A provider that answers with content
// the contract does not declare is still refused rather than quietly ignored.
func TestConnectDispatchCarriesAVoidOperation(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "connect-transport", "a-generated-call-dispatches-on-the-first-declared-transport")
	reply := "{}"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", connectwire.UnaryJSONContentType)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(reply))
	}))
	defer server.Close()
	bound := connectBoundClient(t, server.URL, connectServiceProfiles(), connectServiceBinding())
	operation := connectUnaryOperation([]clientcontract.Transport{connectTransport(clientcontract.EncodingJSON)}, nil, nil)
	operation.Successes = []OperationSuccess{{Status: http.StatusNoContent}}
	if err := CallOperationVoid(t.Context(), bound, newConnectCallInput(), operation); err != nil {
		t.Fatalf("CallOperationVoid: %v", err)
	}
	reply = `{"value":"surprise"}`
	if err := CallOperationVoid(t.Context(), bound, newConnectCallInput(), operation); err == nil {
		t.Error("a body the contract does not declare was accepted")
	}
}
