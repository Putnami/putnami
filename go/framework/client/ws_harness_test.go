package client

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// wsTestServer is a minimal first-party WebSocket provider. It performs the RFC
// 6455 opening handshake with its own accept computation and plays scripted
// frames, so the client is exercised against an implementation it does not
// share code with on the wire-shape side.
type wsTestServer struct {
	*httptest.Server

	// finished closes when the first scripted conversation returns, so a test
	// asserting on client frames never races the provider goroutine.
	finished   chan struct{}
	finishOnce sync.Once

	mu       sync.Mutex
	failures []string
	upgrades []http.Header
	received []any
}

// wsTestOptions describes one scripted provider.
type wsTestOptions struct {
	// subprotocol overrides the echoed Sec-WebSocket-Protocol. Empty echoes the
	// offered token.
	subprotocol string
	// corruptAccept answers with an accept token that does not prove the key.
	corruptAccept bool
	// extensions echoes a Sec-WebSocket-Extensions the client never offered.
	extensions string
	// rejectStatus answers with an ordinary HTTP response instead of a switch.
	rejectStatus int
	// rejectBody is the body of that ordinary response.
	rejectBody string
	// maxMessageBytes bounds one message the provider reassembles.
	maxMessageBytes int64
	// play scripts the conversation once the socket is framed.
	play func(peer *wsTestPeer)
}

// wsTestPeer is the provider side of one framed conversation.
type wsTestPeer struct {
	t       *testing.T
	server  *wsTestServer
	conn    *wsConn
	request *http.Request
}

func newWSTestServer(t *testing.T, options wsTestOptions) *wsTestServer {
	t.Helper()
	harness := &wsTestServer{finished: make(chan struct{})}
	if options.maxMessageBytes <= 0 {
		options.maxMessageBytes = 1 << 20
	}
	harness.Server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		harness.recordUpgrade(request.Header.Clone())
		if options.rejectStatus != 0 {
			defer harness.finishOnce.Do(func() { close(harness.finished) })
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(options.rejectStatus)
			_, _ = writer.Write([]byte(options.rejectBody))
			return
		}
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			harness.fail("test server response does not support hijacking")
			return
		}
		conn, buffered, err := hijacker.Hijack()
		if err != nil {
			harness.fail("hijack failed: " + err.Error())
			return
		}
		defer func() { _ = conn.Close() }()
		if err := harness.writeUpgrade(conn, request, options); err != nil {
			harness.fail("upgrade failed: " + err.Error())
			return
		}
		if options.play == nil {
			harness.finishOnce.Do(func() { close(harness.finished) })
			return
		}
		defer harness.finishOnce.Do(func() { close(harness.finished) })
		options.play(&wsTestPeer{
			t: t, server: harness, request: request,
			conn: newWSConn(conn, buffered.Reader, wsRoleServer, options.maxMessageBytes),
		})
	}))
	t.Cleanup(harness.Close)
	return harness
}

// writeUpgrade answers the opening handshake byte by byte.
func (harness *wsTestServer) writeUpgrade(conn net.Conn, request *http.Request, options wsTestOptions) error {
	accept := webSocketAcceptToken(request.Header.Get("Sec-WebSocket-Key"))
	if options.corruptAccept {
		accept = webSocketAcceptToken("a-key-the-client-never-sent")
	}
	subprotocol := options.subprotocol
	if subprotocol == "" {
		subprotocol = request.Header.Get("Sec-WebSocket-Protocol")
	}
	response := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n"
	if subprotocol != "none" {
		response += "Sec-WebSocket-Protocol: " + subprotocol + "\r\n"
	}
	if options.extensions != "" {
		response += "Sec-WebSocket-Extensions: " + options.extensions + "\r\n"
	}
	_, err := conn.Write([]byte(response + "\r\n"))
	return err
}

func (harness *wsTestServer) fail(message string) {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	harness.failures = append(harness.failures, message)
}

func (harness *wsTestServer) recordUpgrade(header http.Header) {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	harness.upgrades = append(harness.upgrades, header)
}

func (harness *wsTestServer) record(frame any) {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	harness.received = append(harness.received, frame)
}

// waitForPlay blocks until the first scripted conversation returns.
func (harness *wsTestServer) waitForPlay(t *testing.T) {
	t.Helper()
	select {
	case <-harness.finished:
	case <-t.Context().Done():
		t.Fatal("the scripted provider never finished")
	}
}

// assertNoFailures fails the test with every provider-side complaint.
func (harness *wsTestServer) assertNoFailures(t *testing.T) {
	t.Helper()
	harness.mu.Lock()
	defer harness.mu.Unlock()
	for _, failure := range harness.failures {
		t.Errorf("provider: %s", failure)
	}
}

// clientFrames returns the frames the provider read, in order.
func (harness *wsTestServer) clientFrames() []any {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return append([]any(nil), harness.received...)
}

// upgradeHeader returns the header of the first opening handshake.
func (harness *wsTestServer) upgradeHeader(t *testing.T) http.Header {
	t.Helper()
	harness.mu.Lock()
	defer harness.mu.Unlock()
	if len(harness.upgrades) == 0 {
		t.Fatal("no websocket upgrade reached the provider")
	}
	return harness.upgrades[0]
}

// read returns the next parsed client frame, or nil once the socket ends.
func (peer *wsTestPeer) read() any {
	opcode, payload, err := peer.conn.readMessage()
	if err != nil {
		return nil
	}
	if opcode != wsOpcodeText {
		peer.server.fail("client sent a non-text frame")
		return nil
	}
	frame, diagnostics := clientcontract.ParseAndValidateWebSocketFrameV1(payload)
	if len(diagnostics) > 0 {
		peer.server.fail(fmt.Sprintf("client frame is refused by the contract: %v", diagnostics))
		return nil
	}
	peer.server.record(frame)
	return frame
}

// expect reads until it sees a frame of the wanted type, tolerating the
// heartbeats a declared cadence may interleave.
func (peer *wsTestPeer) expect(want clientcontract.WebSocketFrameType) any {
	for {
		frame := peer.read()
		if frame == nil {
			peer.server.fail("socket ended while waiting for a " + string(want) + " frame")
			return nil
		}
		if webSocketFrameTypeOf(frame) == want {
			return frame
		}
		heartbeat, ok := frame.(*clientcontract.WebSocketHeartbeatFrameV1)
		if !ok {
			peer.server.fail(fmt.Sprintf("expected a %s frame, read a %s frame", want, webSocketFrameTypeOf(frame)))
			return nil
		}
		if heartbeat.Type == clientcontract.WebSocketFramePing {
			peer.send(&clientcontract.WebSocketHeartbeatFrameV1{
				V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFramePong, Nonce: heartbeat.Nonce,
			})
		}
	}
}

// await reads until it sees a frame of the wanted type, answering the
// heartbeats it is not waiting for. It reports failure without complaining, so
// a scripted refusal can end the socket mid-scene.
func (peer *wsTestPeer) await(want clientcontract.WebSocketFrameType) bool {
	for {
		opcode, payload, err := peer.conn.readMessage()
		if err != nil || opcode != wsOpcodeText {
			return false
		}
		frame, diagnostics := clientcontract.ParseAndValidateWebSocketFrameV1(payload)
		if len(diagnostics) > 0 {
			peer.server.fail(fmt.Sprintf("client frame is refused by the contract: %v", diagnostics))
			return false
		}
		peer.server.record(frame)
		if webSocketFrameTypeOf(frame) == want {
			return true
		}
		heartbeat, ok := frame.(*clientcontract.WebSocketHeartbeatFrameV1)
		if !ok {
			return false
		}
		if heartbeat.Type == clientcontract.WebSocketFramePing {
			peer.send(&clientcontract.WebSocketHeartbeatFrameV1{
				V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFramePong, Nonce: heartbeat.Nonce,
			})
		}
	}
}

// send writes one provider frame.
func (peer *wsTestPeer) send(frame any) {
	encoded, err := json.Marshal(frame)
	if err != nil {
		peer.server.fail("provider frame cannot be encoded: " + err.Error())
		return
	}
	peer.sendRaw(encoded)
}

// sendRaw writes exact bytes as one text message.
func (peer *wsTestPeer) sendRaw(payload []byte) {
	if err := peer.conn.writeText(payload); err != nil {
		peer.server.fail("provider write failed: " + err.Error())
	}
}

// sendFragments writes one text message split across continuation frames, with
// framing written by hand so the client's reassembly is checked against bytes
// this harness composes itself.
func (peer *wsTestPeer) sendFragments(payload []byte, chunk int) {
	offset := 0
	first := true
	for offset < len(payload) {
		end := min(offset+chunk, len(payload))
		opcode := wsOpcodeContinuation
		if first {
			opcode = wsOpcodeText
		}
		if err := writeRawWSFrame(peer.conn.conn, end == len(payload), opcode, payload[offset:end]); err != nil {
			peer.server.fail("provider fragment write failed: " + err.Error())
			return
		}
		first = false
		offset = end
	}
}

// abort closes the socket without a close frame.
func (peer *wsTestPeer) abort() {
	_ = peer.conn.conn.Close()
}

// drain reads and records every remaining client frame until the client
// releases the socket, so a provider write never races the client's close and a
// closing frame is still observed.
func (peer *wsTestPeer) drain() {
	for {
		opcode, payload, err := peer.conn.readMessage()
		if err != nil {
			return
		}
		if opcode != wsOpcodeText {
			continue
		}
		frame, diagnostics := clientcontract.ParseAndValidateWebSocketFrameV1(payload)
		if len(diagnostics) > 0 {
			peer.server.fail(fmt.Sprintf("client frame is refused by the contract: %v", diagnostics))
			return
		}
		peer.server.record(frame)
	}
}

// writeRawWSFrame composes one unmasked frame by hand. It is the harness's own
// framing, independent of the client writer under test.
func writeRawWSFrame(conn io.Writer, fin bool, opcode wsOpcode, payload []byte) error {
	first := byte(opcode)
	if fin {
		first |= 0x80
	}
	header := []byte{first}
	switch length := len(payload); {
	case length <= 125:
		header = append(header, byte(length))
	case length <= 0xFFFF:
		var extended [2]byte
		binary.BigEndian.PutUint16(extended[:], uint16(length))
		header = append(header, 126)
		header = append(header, extended[:]...)
	default:
		var extended [8]byte
		binary.BigEndian.PutUint64(extended[:], uint64(length))
		header = append(header, 127)
		header = append(header, extended[:]...)
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

// webSocketFrameTypeOf names a parsed frame.
func webSocketFrameTypeOf(frame any) clientcontract.WebSocketFrameType {
	switch value := frame.(type) {
	case *clientcontract.WebSocketInitFrameV1:
		return value.Type
	case *clientcontract.WebSocketReadyFrameV1:
		return value.Type
	case *clientcontract.WebSocketMessageFrameV1:
		return value.Type
	case *clientcontract.WebSocketHalfCloseFrameV1:
		return value.Type
	case *clientcontract.WebSocketResultFrameV1:
		return value.Type
	case *clientcontract.WebSocketErrorFrameV1:
		return value.Type
	case *clientcontract.WebSocketCancelFrameV1:
		return value.Type
	case *clientcontract.WebSocketHeartbeatFrameV1:
		return value.Type
	default:
		return ""
	}
}

// readRawWSFrame reads one frame with the harness's own framing.
func readRawWSFrame(reader *bufio.Reader) (bool, wsOpcode, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return false, 0, nil, err
	}
	fin := header[0]&0x80 != 0
	opcode := wsOpcode(header[0] & 0x0F)
	masked := header[1]&0x80 != 0
	length := int64(header[1] & 0x7F)
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(extended[:]))
	}
	var key [4]byte
	if masked {
		if _, err := io.ReadFull(reader, key[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= key[i%4]
		}
	}
	return fin, opcode, payload, nil
}

// webSocketTestOperation builds one generated operation for the harness.
func webSocketTestOperation(id string, mode clientcontract.StreamMode, encoding clientcontract.Encoding,
	resume bool, shapes *clientcontract.MessageShapes, policy *clientcontract.ResiliencePolicy,
	declared []clientcontract.DeclaredError) Operation {
	if declared == nil {
		declared = []clientcontract.DeclaredError{}
	}
	return Operation{
		ID: id,
		Contract: clientcontract.OperationV1{
			Stream:     mode,
			Messages:   shapes,
			Transports: []clientcontract.Transport{webSocketTestTransport(mode, encoding, resume)},
			Security: clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{
				AllOf: []clientcontract.SecurityRequirement{{Profile: "service"}},
			}}},
			Errors:      declared,
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
			Resilience:  policy,
		},
	}
}

// webSocketProtoProjection is the minimal document protobuf projection a proto
// transport must declare before it can be refused for its encoding.
func webSocketProtoProjection() *clientcontract.ProtobufDescriptor {
	return &clientcontract.ProtobufDescriptor{
		Syntax: "proto3", Package: "fixtures.items.v1",
		Services: []clientcontract.ProtobufService{{Name: "Items", Methods: []clientcontract.ProtobufMethod{
			{Name: "Get", Input: "WatchRequest", Output: "Item"},
			{Name: "Watch", Input: "WatchRequest", Output: "Item", ServerStreaming: true},
			{Name: "Upload", Input: "WatchRequest", Output: "Item", ClientStreaming: true},
			{Name: "Chat", Input: "WatchRequest", Output: "Item", ClientStreaming: true, ServerStreaming: true},
		}}},
		Messages: []clientcontract.ProtobufMessage{
			{Name: "WatchRequest", Fields: []clientcontract.ProtobufField{}},
			{Name: "Item", Fields: []clientcontract.ProtobufField{}},
		},
		Enums: []clientcontract.ProtobufEnum{},
	}
}

// webSocketTestTransport is one declared websocket transport.
func webSocketTestTransport(mode clientcontract.StreamMode, encoding clientcontract.Encoding,
	resume bool) clientcontract.Transport {
	transport := clientcontract.Transport{
		Protocol: clientcontract.TransportWebSocket, Path: "/items/stream", Encoding: encoding,
		WebSocket: &clientcontract.WebSocketTransport{
			Subprotocol: clientcontract.WebSocketSubprotocolV1, Resume: resume,
		},
	}
	if encoding != clientcontract.EncodingProto {
		return transport
	}
	method := "Get"
	switch mode {
	case clientcontract.StreamServer:
		method = "Watch"
	case clientcontract.StreamClient:
		method = "Upload"
	case clientcontract.StreamBidirectional:
		method = "Chat"
	}
	transport.ProtobufMethod = "/fixtures.items.v1.Items/" + method
	return transport
}

// webSocketTestClient binds a client to the harness with one service token.
func webSocketTestClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	descriptor := testDescriptor(map[string]clientcontract.CredentialProfile{
		"service": {Kind: clientcontract.CredentialServiceToken},
	})
	descriptor.Contract.Protobuf = webSocketProtoProjection()
	return boundTestClient(t, endpoint, descriptor, ServiceBinding{Credentials: map[string]CredentialBinding{
		"service": {Provider: freshSource("stream-token", nil)},
	}})
}

// webSocketReadyFrame is the provider's admission frame.
func webSocketReadyFrame(resumed bool, token string) *clientcontract.WebSocketReadyFrameV1 {
	return &clientcontract.WebSocketReadyFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameReady,
		Resumed: &resumed, ResumeToken: token,
	}
}

// webSocketMessageFrame is one provider application message.
func webSocketMessageFrame(sequence, value string) *clientcontract.WebSocketMessageFrameV1 {
	return &clientcontract.WebSocketMessageFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameMessage, Sequence: sequence,
		Payload: clientcontract.WebSocketEncodedPayloadV1{
			Encoding: clientcontract.EncodingJSON, Value: json.RawMessage(value),
		},
	}
}

// webSocketResultFrame is the provider's successful terminal frame.
func webSocketResultFrame(value string) *clientcontract.WebSocketResultFrameV1 {
	frame := &clientcontract.WebSocketResultFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameResult,
	}
	if value != "" {
		frame.Payload = &clientcontract.WebSocketEncodedPayloadV1{
			Encoding: clientcontract.EncodingJSON, Value: json.RawMessage(value),
		}
	}
	return frame
}

// webSocketErrorFrame is the provider's failed terminal frame.
func webSocketErrorFrame(status int, code, message string, details string) *clientcontract.WebSocketErrorFrameV1 {
	frame := &clientcontract.WebSocketErrorFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameError,
		Error: clientcontract.WebSocketRemoteErrorV1{Status: status, Code: code, Message: message},
	}
	if details != "" {
		frame.Error.Details = json.RawMessage(details)
	}
	return frame
}

// stringObjectSchema is a closed object of string properties.
func stringObjectSchema(names ...string) clientcontract.Schema {
	properties := make(map[string]clientcontract.Schema, len(names))
	for _, name := range names {
		properties[name] = clientcontract.Schema{Type: "string"}
	}
	return clientcontract.Schema{Type: "object", Properties: properties, AdditionalProperties: additionalForbidden()}
}

// upgradeCount is the number of opening handshakes that reached the provider.
func (harness *wsTestServer) upgradeCount() int {
	harness.mu.Lock()
	defer harness.mu.Unlock()
	return len(harness.upgrades)
}
