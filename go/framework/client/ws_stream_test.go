package client

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

type wsItem struct {
	Value string `json:"value"`
}

// webSocketClientStreamOperation is the harness's declared client stream.
func webSocketClientStreamOperation() Operation {
	message := stringObjectSchema("value")
	return webSocketTestOperation("uploadItems", clientcontract.StreamClient, clientcontract.EncodingJSON, false,
		&clientcontract.MessageShapes{Input: &message, Output: &message}, nil, nil)
}

// webSocketBidiOperation is the harness's declared bidirectional stream.
func webSocketBidiOperation(policy *clientcontract.ResiliencePolicy) Operation {
	message := stringObjectSchema("value")
	return webSocketTestOperation("chatItems", clientcontract.StreamBidirectional, clientcontract.EncodingJSON, false,
		&clientcontract.MessageShapes{Input: &message, Output: &message}, policy, nil)
}

// TestOpenServerStreamWSAdmitsInBandAndValidatesEveryMessage is the happy path:
// the credentials, the identity, the deadline and the propagation context all
// travel in the first application frame, and every provider message is checked
// against the declared output schema before it reaches the caller.
func TestOpenServerStreamWSAdmitsInBandAndValidatesEveryMessage(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "websocket-in-band-admission",
		"admission-credentials-travel-in-the-first-application-frame")
	spectest.Proves(t, "go/typed-service-clients", "websocket-typed-streams",
		"every-websocket-message-is-validated-against-the-declared-output-schema")
	observer := &recordingServiceTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()

	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.send(webSocketMessageFrame("1", `{"value":"one"}`))
		peer.send(webSocketMessageFrame("2", `{"value":"two"}`))
		peer.send(webSocketResultFrame(""))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	request := &Request{Headers: http.Header{"X-Correlation-Key": []string{"call-1"}}}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	stream, err := OpenServerStreamWS[wsItem](ctx, bound, request, webSocketServerStreamOperation(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, 2)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	server.waitForPlay(t)
	server.assertNoFailures(t)

	frames := server.clientFrames()
	if len(frames) == 0 {
		t.Fatal("the provider read no client frame")
	}
	initFrame, ok := frames[0].(*clientcontract.WebSocketInitFrameV1)
	if !ok {
		t.Fatalf("first client frame = %T", frames[0])
	}
	if initFrame.OperationID != "watchItems" || initFrame.ClientID != "consumer.workload" {
		t.Fatalf("init identity = %q %q", initFrame.OperationID, initFrame.ClientID)
	}
	if len(initFrame.Credentials) != 1 || initFrame.Credentials[0].Profile != "service" ||
		initFrame.Credentials[0].Value != "Bearer stream-token" {
		t.Fatalf("init credentials = %#v", initFrame.Credentials)
	}
	if len(initFrame.Headers) != 1 || initFrame.Headers[0].Name != "X-Correlation-Key" {
		t.Fatalf("init headers = %#v", initFrame.Headers)
	}
	if initFrame.Context == nil || initFrame.Context.Traceparent == "" {
		t.Fatalf("init context = %#v", initFrame.Context)
	}
	if initFrame.DeadlineUnixMs == "0" || initFrame.BudgetMs != "0" {
		t.Fatalf("init deadline %q budget %q", initFrame.DeadlineUnixMs, initFrame.BudgetMs)
	}
	if strings.Join(values, ",") != "one,two" {
		t.Fatalf("delivered values = %v", values)
	}
	observer.mu.Lock()
	calls := append([]ServiceCallResult(nil), observer.calls...)
	observer.mu.Unlock()
	if len(calls) != 1 || calls[0].Code != "" || calls[0].StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("call measurements = %#v", calls)
	}
}

// TestOpenServerStreamWSRefusesAMessageOutsideTheContract keeps a provider value that
// does not match the generated contract from reaching the caller.
func TestOpenServerStreamWSRefusesAMessageOutsideTheContract(t *testing.T) {
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.send(webSocketMessageFrame("1", `{"value":1}`))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	stream, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, webSocketServerStreamOperation(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
		t.Error("an undeclared message reached the caller")
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientResponse) {
		t.Fatalf("terminal error = %T %v", err, err)
	}
}

// TestWebSocketStreamRefusesAProtoTransportBeforeAnyNetworkByte proves the
// runtime belt: a transport this runtime cannot encode fails closed at open,
// with the operation and the encoding named, and never opens a socket.
func TestWebSocketStreamRefusesAProtoTransportBeforeAnyNetworkByte(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "websocket-in-band-admission",
		"an-undeliverable-websocket-transport-fails-closed-before-any-network-byte")
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) { peer.drain() }})
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	})
	descriptor.Contract.Protobuf = webSocketProtoProjection()
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
	message := stringObjectSchema("value")
	operation := webSocketTestOperation("watchItems", clientcontract.StreamServer, clientcontract.EncodingProto, false,
		&clientcontract.MessageShapes{Output: &message}, nil, nil)
	_, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, operation)
	if err == nil || !perrors.Is(err, CodeClientConfig) {
		t.Fatalf("error = %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "watchItems") || !strings.Contains(err.Error(), "proto") {
		t.Fatalf("diagnostic does not name the operation and the encoding: %v", err)
	}
	server.mu.Lock()
	upgrades := len(server.upgrades)
	server.mu.Unlock()
	if upgrades != 0 {
		t.Fatalf("the refused transport still opened %d sockets", upgrades)
	}
}

// TestOpenClientStreamSendsHalfClosesAndReturnsTheDeclaredResult walks the
// client-stream shape: ordered messages, one half-close, one typed result.
func TestOpenClientStreamSendsHalfClosesAndReturnsTheDeclaredResult(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "websocket-typed-streams",
		"client-and-bidirectional-streams-half-close-once-and-take-one-terminal")
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.expect(clientcontract.WebSocketFrameMessage)
		peer.expect(clientcontract.WebSocketFrameMessage)
		peer.expect(clientcontract.WebSocketFrameHalfClose)
		peer.send(webSocketResultFrame(`{"value":"stored"}`))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	stream, err := OpenClientStream[wsItem, wsItem](t.Context(), bound, &Request{}, webSocketClientStreamOperation())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"one", "two"} {
		if err := stream.Send(t.Context(), wsItem{Value: value}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("a second half-close must be a no-op: %v", err)
	}
	result, err := stream.Result(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Value != "stored" {
		t.Fatalf("result = %#v", result)
	}
	server.waitForPlay(t)
	server.assertNoFailures(t)
	sequences := make([]string, 0, 2)
	halfCloses := 0
	for _, frame := range server.clientFrames() {
		switch value := frame.(type) {
		case *clientcontract.WebSocketMessageFrameV1:
			sequences = append(sequences, value.Sequence)
		case *clientcontract.WebSocketHalfCloseFrameV1:
			halfCloses++
		}
	}
	if strings.Join(sequences, ",") != "1,2" || halfCloses != 1 {
		t.Fatalf("client sequences %v, half-closes %d", sequences, halfCloses)
	}
	if err := stream.Send(t.Context(), wsItem{Value: "late"}); err == nil {
		t.Fatal("a message after the terminal frame must be refused")
	}
}

// TestOpenBidiStreamKeepsBothDirectionsAndDeliversTheFinalResult proves the
// bidirectional shape, including a terminal result that carries a value.
func TestOpenBidiStreamKeepsBothDirectionsAndDeliversTheFinalResult(t *testing.T) {
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.expect(clientcontract.WebSocketFrameMessage)
		peer.send(webSocketMessageFrame("1", `{"value":"ack"}`))
		peer.expect(clientcontract.WebSocketFrameHalfClose)
		peer.send(webSocketMessageFrame("2", `{"value":"after-half-close"}`))
		peer.send(webSocketResultFrame(`{"value":"final"}`))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	stream, err := OpenBidiStream[wsItem, wsItem](t.Context(), bound, &Request{}, webSocketBidiOperation(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(t.Context(), wsItem{Value: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	received := make([]string, 0, 3)
	for {
		message, recvErr := stream.Recv(t.Context())
		if stderrors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		received = append(received, message.Value)
	}
	if strings.Join(received, ",") != "ack,after-half-close,final" {
		t.Fatalf("received = %v", received)
	}
	server.assertNoFailures(t)
}

// TestWebSocketStreamProjectsATypedTerminalError keeps a provider error frame
// inside the operation's declared errors and drops credential material the
// declared payload would otherwise carry out.
func TestWebSocketStreamProjectsATypedTerminalError(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "websocket-typed-streams",
		"a-terminal-websocket-error-is-projected-as-a-typed-remote-error")
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.send(webSocketErrorFrame(http.StatusNotFound, "not_found", "missing",
			`{"resource":"item-1","token":"Bearer stream-token"}`))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	errorSchema := stringObjectSchema("resource", "token")
	declared := []clientcontract.DeclaredError{{
		Status: http.StatusNotFound, Code: "not_found", Schema: &errorSchema, Retryable: boolPointer(false),
	}}
	stream, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{},
		webSocketServerStreamOperation(nil, declared))
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
	}
	var remote *RemoteError
	if !stderrors.As(stream.Err(), &remote) || remote.RemoteCode != "not_found" || remote.StatusCode != http.StatusNotFound {
		t.Fatalf("terminal error = %T %v", stream.Err(), stream.Err())
	}
	if remote.ServiceID != "inventory" || remote.OperationID != "watchItems" {
		t.Fatalf("remote error identity = %q %q", remote.ServiceID, remote.OperationID)
	}
	if strings.Contains(string(remote.Payload), "stream-token") {
		t.Fatalf("terminal error payload carries credential material: %s", remote.Payload)
	}
	if !strings.Contains(string(remote.Payload), "item-1") {
		t.Fatalf("terminal error payload dropped a declared business field: %s", remote.Payload)
	}
}

// TestWebSocketStreamInvalidatesCredentialsOnAnUnauthorizedAdmission proves a
// refusal before admission invalidates the credential exactly once and is never
// replayed with the same token.
func TestWebSocketStreamInvalidatesCredentialsOnAnUnauthorizedAdmission(t *testing.T) {
	var acquisitions atomic.Int32
	var connections atomic.Int32
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		if connections.Add(1) == 1 {
			peer.send(webSocketErrorFrame(http.StatusUnauthorized, "unauthorized", "expired", ""))
		} else {
			peer.send(webSocketReadyFrame(false, ""))
			peer.send(webSocketResultFrame(""))
		}
		peer.drain()
	}})
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	})
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", &acquisitions)},
	}})
	declared := []clientcontract.DeclaredError{{Status: http.StatusUnauthorized, Code: "unauthorized"}}
	operation := webSocketServerStreamOperation(nil, declared)
	_, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, operation)
	var remote *RemoteError
	if !stderrors.As(err, &remote) || remote.StatusCode != http.StatusUnauthorized {
		t.Fatalf("error = %T %v", err, err)
	}
	refusedAcquisitions := acquisitions.Load()
	if refusedAcquisitions == 0 {
		t.Fatal("the refused admission acquired no credential")
	}
	stream, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, operation)
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if acquisitions.Load() <= refusedAcquisitions {
		t.Fatalf("the invalidated credential was reused: %d acquisitions", acquisitions.Load())
	}
}

// TestWebSocketStreamReassemblesFragmentedProviderFrames proves a first-party
// frame split across continuation frames is one message to the contract.
func TestWebSocketStreamReassemblesFragmentedProviderFrames(t *testing.T) {
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		ready, _ := json.Marshal(webSocketReadyFrame(false, ""))
		peer.sendFragments(ready, 7)
		message, _ := json.Marshal(webSocketMessageFrame("1", `{"value":"fragmented"}`))
		peer.sendFragments(message, 5)
		peer.send(webSocketResultFrame(""))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	stream, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, webSocketServerStreamOperation(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, 1)
	for message := range stream.Messages() {
		values = append(values, message.Value)
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(values, ",") != "fragmented" {
		t.Fatalf("values = %v", values)
	}
	server.assertNoFailures(t)
}

// TestWebSocketStreamAnswersAHeartbeatDuringAdmission proves the heartbeat is
// legal before the ready frame, so a slow asynchronous credential check does not
// look idle.
func TestWebSocketStreamAnswersAHeartbeatDuringAdmission(t *testing.T) {
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(&clientcontract.WebSocketHeartbeatFrameV1{
			V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFramePing, Nonce: "await-ready-1",
		})
		if pong, ok := peer.expect(clientcontract.WebSocketFramePong).(*clientcontract.WebSocketHeartbeatFrameV1); !ok ||
			pong.Nonce != "await-ready-1" {
			peer.server.fail("the client did not answer the admission heartbeat with its nonce")
		}
		peer.send(webSocketReadyFrame(false, ""))
		peer.send(webSocketResultFrame(""))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	stream, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, webSocketServerStreamOperation(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	server.assertNoFailures(t)
}

// TestWebSocketStreamSendsTheDeclaredHeartbeat proves the client keeps the
// cadence the provider declared, and stops with the session.
func TestWebSocketStreamSendsTheDeclaredHeartbeat(t *testing.T) {
	pinged := make(chan string, 1)
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		for {
			frame := peer.read()
			if frame == nil {
				return
			}
			if heartbeat, ok := frame.(*clientcontract.WebSocketHeartbeatFrameV1); ok &&
				heartbeat.Type == clientcontract.WebSocketFramePing {
				select {
				case pinged <- heartbeat.Nonce:
				default:
				}
				peer.send(webSocketResultFrame(""))
				peer.drain()
				return
			}
		}
	}})
	bound := webSocketTestClient(t, server.URL)
	policy := &clientcontract.ResiliencePolicy{Stream: &clientcontract.StreamPolicy{HeartbeatMs: intPointer(10)}}
	stream, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, webSocketServerStreamOperation(policy, nil))
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	select {
	case nonce := <-pinged:
		if nonce == "" {
			t.Fatal("heartbeat nonce is empty")
		}
	default:
		t.Fatal("the declared heartbeat cadence produced no ping")
	}
}

// TestWebSocketStreamReleasesAStalledProviderOnTheIdleBudget proves the idle
// budget is distinct from the declared duration and releases a socket that went
// quiet after admission.
func TestWebSocketStreamReleasesAStalledProviderOnTheIdleBudget(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		<-blocked
	}})
	bound := webSocketTestClient(t, server.URL)
	policy := &clientcontract.ResiliencePolicy{
		Stream: &clientcontract.StreamPolicy{IdleTimeoutMs: intPointer(40)},
	}
	stream, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, webSocketServerStreamOperation(policy, nil))
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientDeadline) {
		t.Fatalf("terminal error = %T %v", stream.Err(), stream.Err())
	}
}

// TestWebSocketStreamCancelEmitsOneCancelFrameAndOneMeasurement proves the
// caller's withdrawal reaches the provider once, keeps the terminal with the
// reader, and emits exactly one call measurement.
func TestWebSocketStreamCancelEmitsOneCancelFrameAndOneMeasurement(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "websocket-shared-lifecycle",
		"a-canceled-websocket-stream-tells-the-provider-once-and-measures-once")
	observer := &recordingServiceTelemetry{}
	restore := InstallServiceCallTelemetry(observer)
	defer restore()
	delivered := make(chan struct{})
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.send(webSocketMessageFrame("1", `{"value":"first"}`))
		<-delivered
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	stream, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, webSocketServerStreamOperation(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	<-stream.Messages()
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	close(delivered)
	<-stream.Done()
	server.waitForPlay(t)
	if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientCanceled) {
		t.Fatalf("terminal error = %T %v", stream.Err(), stream.Err())
	}
	cancels := 0
	for _, frame := range server.clientFrames() {
		if value, ok := frame.(*clientcontract.WebSocketCancelFrameV1); ok {
			cancels++
			if value.Code != clientcontract.WebSocketCancelCodeCanceled {
				t.Fatalf("cancel code = %q", value.Code)
			}
		}
	}
	if cancels != 1 {
		t.Fatalf("cancel frames = %d", cancels)
	}
	observer.mu.Lock()
	calls := append([]ServiceCallResult(nil), observer.calls...)
	observer.mu.Unlock()
	if len(calls) != 1 || calls[0].Code != string(CodeClientCanceled) {
		t.Fatalf("call measurements = %#v", calls)
	}
}

// TestWebSocketStreamReportsAnAbruptClose keeps a socket that disappears without
// a terminal frame reportable instead of silent.
func TestWebSocketStreamReportsAnAbruptClose(t *testing.T) {
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.send(webSocketMessageFrame("1", `{"value":"one"}`))
		peer.abort()
	}})
	bound := webSocketTestClient(t, server.URL)
	stream, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, webSocketServerStreamOperation(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
	}
	if err := stream.Err(); err == nil || !perrors.Is(err, CodeClientResponse) {
		t.Fatalf("terminal error = %T %v", stream.Err(), stream.Err())
	}
}

// TestWebSocketStreamCarriesWideSequencesAndEmptyBytesLosslessly proves the
// uint64 sequence space survives past the JavaScript safe integer and that an
// empty byte payload stays empty rather than becoming absent.
func TestWebSocketStreamCarriesWideSequencesAndEmptyBytesLosslessly(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "websocket-typed-streams",
		"wide-sequences-and-empty-payloads-survive-the-wire-unchanged")
	const wide = "9007199254740993"
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		initFrame, ok := peer.expect(clientcontract.WebSocketFrameInit).(*clientcontract.WebSocketInitFrameV1)
		if !ok || initFrame.Resume == nil || initFrame.Resume.AfterSequence != "9007199254740992" {
			peer.server.fail("the resume request did not reach the provider")
			return
		}
		peer.send(webSocketReadyFrame(true, "resume-token-2"))
		peer.send(webSocketMessageFrame(wide, `{"value":""}`))
		last, err := strconv.ParseUint(wide, 10, 64)
		if err != nil {
			peer.server.fail(err.Error())
			return
		}
		peer.send(webSocketMessageFrame(strconv.FormatUint(last+1, 10), `{"value":"next"}`))
		peer.send(webSocketResultFrame(""))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	message := stringObjectSchema("value")
	operation := webSocketTestOperation("watchItems", clientcontract.StreamServer, clientcontract.EncodingJSON, true,
		&clientcontract.MessageShapes{Output: &message}, nil, nil)
	service, err := openWebSocketService(t.Context(), bound, &Request{}, operation,
		clientcontract.StreamServer, webSocketOpenOptions{resume: &clientcontract.WebSocketResumeRequestV1{
			Token: "resume-token-1", AfterSequence: "9007199254740992",
		}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := make(chan wsItem, 8)
	delivery := &webSocketDelivery[wsItem]{messages: messages}
	terminal := make(chan error, 1)
	go func() {
		defer close(messages)
		terminal <- runWebSocketStream(service, delivery)
	}()
	values := make([]string, 0, 2)
	for message := range messages {
		values = append(values, "["+message.Value+"]")
	}
	if err := <-terminal; err != nil {
		t.Fatal(err)
	}
	if strings.Join(values, ",") != "[],[next]" {
		t.Fatalf("values = %v", values)
	}
	server.assertNoFailures(t)
}

// TestWebSocketStreamRecordsAdmissionOnceAndNothingAfterIt is the phase rule
// D0.7 fixes, observed through a WebSocket rather than through SSE: a provider
// that accepted and then broke is a session fact, not an availability fact.
func TestWebSocketStreamRecordsAdmissionOnceAndNothingAfterIt(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "websocket-shared-lifecycle",
		"the-websocket-runtime-drives-the-shared-session-and-states-no-phase-rule")
	admitted := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.abort()
	}})
	bound := webSocketTestClient(t, admitted.URL)
	policy := &clientcontract.ResiliencePolicy{Circuit: &clientcontract.CircuitPolicy{FailureThreshold: intPointer(1)}}
	operation := webSocketServerStreamOperation(policy, nil)
	stream, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, operation)
	if err != nil {
		t.Fatal(err)
	}
	for range stream.Messages() {
	}
	if stream.Err() == nil {
		t.Fatal("a provider that cut the stream must surface a terminal error")
	}
	if state := bound.service.breakers[operation.ID].State(); state != CircuitClosed {
		t.Fatalf("circuit after a mid-stream break = %s", state)
	}

	refused := newWSTestServer(t, wsTestOptions{rejectStatus: http.StatusInternalServerError, rejectBody: "{}"})
	refusedClient := webSocketTestClient(t, refused.URL)
	if _, err := OpenServerStreamWS[wsItem](t.Context(), refusedClient, &Request{}, operation); err == nil {
		t.Fatal("a refused handshake must fail")
	}
	if state := refusedClient.service.breakers[operation.ID].State(); state != CircuitOpen {
		t.Fatalf("circuit after a refused handshake = %s", state)
	}
}

// TestWebSocketStreamRefusesAnAnonymousCallForADeclaredCredential keeps an
// operation that declares credentials from ever opening a socket without them.
func TestWebSocketStreamRefusesAnAnonymousCallForADeclaredCredential(t *testing.T) {
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) { peer.drain() }})
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	})
	bound := boundTestClient(t, server.URL, descriptor, ServiceBinding{})
	_, err := OpenServerStreamWS[wsItem](t.Context(), bound, &Request{}, webSocketServerStreamOperation(nil, nil))
	if err == nil || !perrors.Is(err, CodeClientCredential) {
		t.Fatalf("error = %T %v", err, err)
	}
	server.mu.Lock()
	upgrades := len(server.upgrades)
	server.mu.Unlock()
	if upgrades != 0 {
		t.Fatalf("an unauthenticated stream opened %d sockets", upgrades)
	}
}

// TestWebSocketTypedHandlesCloseOnceAndReportTheirTerminal covers the caller
// surface of the two writable handles: Close is idempotent, Done closes once,
// and Err reports the same terminal the reader recorded.
func TestWebSocketTypedHandlesCloseOnceAndReportTheirTerminal(t *testing.T) {
	for _, mode := range []clientcontract.StreamMode{clientcontract.StreamClient, clientcontract.StreamBidirectional} {
		t.Run(string(mode), func(t *testing.T) {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
				peer.expect(clientcontract.WebSocketFrameInit)
				peer.send(webSocketReadyFrame(false, ""))
				<-release
			}})
			bound := webSocketTestClient(t, server.URL)
			var done <-chan struct{}
			var terminal func() error
			var closer func() error
			afterClose := func() {}
			if mode == clientcontract.StreamClient {
				stream, err := OpenClientStream[wsItem, wsItem](t.Context(), bound, &Request{},
					webSocketClientStreamOperation())
				if err != nil {
					t.Fatal(err)
				}
				done, terminal, closer = stream.Done(), stream.Err, stream.Close
				afterClose = func() {
					if _, err := stream.Result(t.Context()); err == nil || !perrors.Is(err, CodeClientCanceled) {
						t.Errorf("result after a caller close = %T %v", err, err)
					}
				}
			} else {
				stream, err := OpenBidiStream[wsItem, wsItem](t.Context(), bound, &Request{},
					webSocketBidiOperation(nil))
				if err != nil {
					t.Fatal(err)
				}
				done, terminal, closer = stream.Done(), stream.Err, stream.Close
			}
			if err := closer(); err != nil {
				t.Fatal(err)
			}
			if err := closer(); err != nil {
				t.Fatal(err)
			}
			<-done
			if err := terminal(); err == nil || !perrors.Is(err, CodeClientCanceled) {
				t.Fatalf("terminal error = %T %v", err, err)
			}
			afterClose()
		})
	}
}

// TestBidiStreamRecvReportsTheTerminalErrorToTheCaller keeps a failed
// bidirectional stream from looking like a clean end of stream.
func TestBidiStreamRecvReportsTheTerminalErrorToTheCaller(t *testing.T) {
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.send(webSocketErrorFrame(http.StatusServiceUnavailable, "unavailable", "try later", ""))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	stream, err := OpenBidiStream[wsItem, wsItem](t.Context(), bound, &Request{}, webSocketBidiOperation(nil))
	if err != nil {
		t.Fatal(err)
	}
	_, recvErr := stream.Recv(t.Context())
	var remote *RemoteError
	if !stderrors.As(recvErr, &remote) || remote.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("recv error = %T %v", recvErr, recvErr)
	}
	if stderrors.Is(recvErr, io.EOF) {
		t.Fatal("a failed stream must not look like a clean end of stream")
	}
}

// TestWebSocketCloseSendAfterTheProviderEndedLeavesTheTerminalToTheReader pins
// an ordering the provider is allowed: it may end a client stream before it
// reads one client frame, so the caller's half-close lands after the terminal
// frame or after the socket is gone. CloseSend succeeds either way, and the
// caller reads the typed terminal from Result. Waiting on Done makes each
// ordering deterministic instead of a scheduling accident.
func TestWebSocketCloseSendAfterTheProviderEndedLeavesTheTerminalToTheReader(t *testing.T) {
	scenes := []struct {
		name     string
		play     func(peer *wsTestPeer)
		terminal func(t *testing.T, err error)
	}{
		{
			name: "after the terminal frame",
			play: func(peer *wsTestPeer) {
				peer.expect(clientcontract.WebSocketFrameInit)
				peer.send(webSocketReadyFrame(false, ""))
				peer.send(webSocketErrorFrame(http.StatusNotFound, "not_found", "missing", ""))
				peer.drain()
			},
			terminal: func(t *testing.T, err error) {
				var remote *RemoteError
				if !stderrors.As(err, &remote) || remote.StatusCode != http.StatusNotFound {
					t.Fatalf("result error = %T %v, want the provider's typed refusal", err, err)
				}
			},
		},
		{
			name: "after the socket is gone",
			play: func(peer *wsTestPeer) {
				peer.expect(clientcontract.WebSocketFrameInit)
				peer.send(webSocketReadyFrame(false, ""))
				peer.abort()
			},
			terminal: func(t *testing.T, err error) {
				if err == nil || !perrors.Is(err, CodeClientResponse) {
					t.Fatalf("result error = %T %v, want the typed response failure", err, err)
				}
			},
		},
	}
	for _, scene := range scenes {
		t.Run(scene.name, func(t *testing.T) {
			server := newWSTestServer(t, wsTestOptions{play: scene.play})
			bound := webSocketTestClient(t, server.URL)
			stream, err := OpenClientStream[wsItem, wsItem](t.Context(), bound, &Request{},
				webSocketClientStreamOperation())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = stream.Close() }()
			<-stream.Done()
			if err := stream.CloseSend(); err != nil {
				t.Fatalf("CloseSend after the provider ended = %T %v", err, err)
			}
			if err := stream.CloseSend(); err != nil {
				t.Fatalf("second CloseSend after the provider ended = %T %v", err, err)
			}
			_, resultErr := stream.Result(t.Context())
			scene.terminal(t, resultErr)
			server.waitForPlay(t)
			server.assertNoFailures(t)
			for _, frame := range server.clientFrames() {
				if _, ok := frame.(*clientcontract.WebSocketHalfCloseFrameV1); ok {
					t.Fatal("a half-close reached the provider after the conversation ended")
				}
			}
		})
	}
}

// TestWebSocketSendRefusesAnUndeclaredMessage keeps a caller value that does not
// match the generated input schema off the wire.
func TestWebSocketSendRefusesAnUndeclaredMessage(t *testing.T) {
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	message := clientcontract.Schema{
		Type: "object", Properties: map[string]clientcontract.Schema{"value": {Type: "string", MaxLength: intPointer(3)}},
		Required: []string{"value"}, AdditionalProperties: additionalForbidden(),
	}
	operation := webSocketTestOperation("uploadItems", clientcontract.StreamClient, clientcontract.EncodingJSON, false,
		&clientcontract.MessageShapes{Input: &message, Output: &message}, nil, nil)
	stream, err := OpenClientStream[wsItem, wsItem](t.Context(), bound, &Request{}, operation)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(t.Context(), wsItem{Value: "far-too-long"}); err == nil ||
		!perrors.Is(err, CodeClientRequest) {
		t.Fatalf("error = %T %v", err, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestWebSocketSendRefusesACanceledCallerContext keeps a caller that already
// withdrew from putting another frame on the wire.
func TestWebSocketSendRefusesACanceledCallerContext(t *testing.T) {
	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	stream, err := OpenClientStream[wsItem, wsItem](t.Context(), bound, &Request{}, webSocketClientStreamOperation())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := stream.Send(ctx, wsItem{Value: "one"}); err == nil || !perrors.Is(err, CodeClientCanceled) {
		t.Fatalf("error = %T %v", err, err)
	}
	if _, err := stream.Result(ctx); err == nil || !perrors.Is(err, CodeClientCanceled) {
		t.Fatalf("result error = %T %v", err, err)
	}
}
