package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 section 1.3 fixes SHA-1 as the handshake accept digest
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	phttp "go.putnami.dev/http"
	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// wsHandshakeGUID is the fixed RFC 6455 section 1.3 accept salt. It is written
// out here rather than imported so the provider's own constant is checked
// against an independent copy of the published value.
const wsHandshakeGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsProvider is a real first-party provider on a real port: the api plugin
// bound to the framework HTTP server, serving the negotiated service protocol.
type wsProvider struct {
	*httptest.Server

	plugin *Plugin
	http   *phttp.ServerPlugin
}

// wsProviderOptions describes one provider under test.
type wsProviderOptions struct {
	// credentials are the declared credential profiles of the service.
	credentials map[string]clientcontract.CredentialProfile
	// defaults is the document-wide resilience policy.
	defaults *clientcontract.ResiliencePolicy
	// register adds the endpoints under test.
	register func(*Plugin)
}

func newWSProvider(t *testing.T, options wsProviderOptions) *wsProvider {
	t.Helper()
	httpServer := phttp.NewServerPlugin(phttp.ServerConfig{})
	credentials := options.credentials
	if credentials == nil {
		credentials = map[string]clientcontract.CredentialProfile{
			"service": {Kind: clientcontract.CredentialServiceToken},
		}
	}
	plugin := New(httpServer, WithClientService(ClientServiceOptions{
		Service:     clientcontract.Service{ID: "widgets", Audience: "https://widgets.internal"},
		Credentials: credentials,
		Defaults:    &clientcontract.Defaults{Resilience: options.defaults},
	}))
	options.register(plugin)
	if err := plugin.Configure(t.Context(), nil); err != nil {
		t.Fatalf("api Configure: %v", err)
	}
	provider := &wsProvider{plugin: plugin, http: httpServer}
	provider.Server = httptest.NewServer(httpServer.Handler())
	t.Cleanup(provider.Close)
	return provider
}

// stop drains the server the way a graceful application shutdown does, so a
// hijacked conversation observes the drain signal.
func (p *wsProvider) stop(t *testing.T) {
	t.Helper()
	if err := p.http.Stop(t.Context(), nil); err != nil {
		t.Fatalf("http Stop: %v", err)
	}
}

// wsPeer is the client half of one conversation. It writes RFC 6455 frames by
// hand so the provider is exercised against framing this file composes itself.
type wsPeer struct {
	t    *testing.T
	conn net.Conn
	rw   *bufio.ReadWriter
	// pongs records every heartbeat the provider answered or asked for.
	pongs []string
}

// dialWSPeer performs the opening handshake and returns the framed peer. It
// fails the test when the provider refuses the switch; use dialWSPeerRaw to
// assert on a refusal.
func dialWSPeer(t *testing.T, provider *wsProvider, path string) *wsPeer {
	t.Helper()
	subprotocol := clientcontract.WebSocketSubprotocolV1
	peer, response, err := dialWSPeerRaw(t, provider, path, subprotocol)
	if err != nil {
		t.Fatalf("websocket handshake: %v", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status = %d, want 101", response.StatusCode)
	}
	if got := response.Header.Get("Sec-WebSocket-Protocol"); got != subprotocol {
		t.Fatalf("negotiated subprotocol = %q, want %q", got, subprotocol)
	}
	return peer
}

// dialWSPeerRaw performs the opening handshake and returns whatever the
// provider answered, including an ordinary HTTP refusal.
func dialWSPeerRaw(t *testing.T, provider *wsProvider, path, subprotocol string) (*wsPeer, *http.Response, error) {
	t.Helper()
	target, err := url.Parse(provider.URL + path)
	if err != nil {
		return nil, nil, err
	}
	conn, err := net.Dial("tcp", target.Host)
	if err != nil {
		return nil, nil, err
	}
	t.Cleanup(func() { _ = conn.Close() })

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	request := "GET " + target.RequestURI() + " HTTP/1.1\r\nHost: " + target.Host +
		"\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\n"
	if subprotocol != "" {
		request += "Sec-WebSocket-Protocol: " + subprotocol + "\r\n"
	}
	if _, err := conn.Write([]byte(request + "\r\n")); err != nil {
		return nil, nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode == http.StatusSwitchingProtocols {
		if got, want := response.Header.Get("Sec-WebSocket-Accept"), wsExpectedAccept(key); got != want {
			return nil, nil, fmt.Errorf("accept token = %q, want %q", got, want)
		}
	}
	peer := &wsPeer{t: t, conn: conn, rw: bufio.NewReadWriter(reader, bufio.NewWriter(conn))}
	return peer, response, nil
}

// wsExpectedAccept computes the RFC 6455 accept token from the offered key.
func wsExpectedAccept(key string) string {
	digest := sha1.New() //nolint:gosec // protocol-prescribed token derivation
	_, _ = digest.Write([]byte(key))
	_, _ = digest.Write([]byte(wsHandshakeGUID))
	return base64.StdEncoding.EncodeToString(digest.Sum(nil))
}

// wsPeerDeadline bounds every read and write of a test peer, so a provider that
// never answers fails the test instead of hanging it.
const wsPeerDeadline = 5 * time.Second

// deadline re-arms that bound before the next read or write.
func (peer *wsPeer) deadline() {
	peer.t.Helper()
	if err := peer.conn.SetDeadline(time.Now().Add(wsPeerDeadline)); err != nil {
		peer.t.Fatalf("set deadline: %v", err)
	}
}

// writeFrame writes one masked frame, as RFC 6455 section 5.1 requires of a
// client.
func (peer *wsPeer) writeFrame(fin bool, opcode byte, payload []byte) error {
	header := []byte{opcode}
	if fin {
		header[0] |= 0x80
	}
	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(length)|0x80)
	case length <= 0xffff:
		header = append(header, 126|0x80, byte(length>>8), byte(length))
	default:
		header = append(header, 127|0x80)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(length))
		header = append(header, buf[:]...)
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	masked := append([]byte(nil), payload...)
	for i := range masked {
		masked[i] ^= mask[i%4]
	}
	if _, err := peer.rw.Write(append(append(header, mask[:]...), masked...)); err != nil {
		return err
	}
	return peer.rw.Flush()
}

// sendRaw writes exact bytes as one unfragmented text message.
func (peer *wsPeer) sendRaw(payload []byte) {
	peer.t.Helper()
	peer.deadline()
	if err := peer.writeFrame(true, 0x1, payload); err != nil {
		peer.t.Fatalf("write text message: %v", err)
	}
}

// send marshals and writes one first-party frame.
func (peer *wsPeer) send(frame any) {
	peer.t.Helper()
	encoded, err := json.Marshal(frame)
	if err != nil {
		peer.t.Fatalf("encode frame: %v", err)
	}
	peer.sendRaw(encoded)
}

// sendFragments writes one text message split across continuation frames.
func (peer *wsPeer) sendFragments(payload []byte, chunk int) {
	peer.t.Helper()
	peer.deadline()
	offset, first := 0, true
	for offset < len(payload) {
		end := min(offset+chunk, len(payload))
		opcode := byte(0x0)
		if first {
			opcode = 0x1
		}
		if err := peer.writeFrame(end == len(payload), opcode, payload[offset:end]); err != nil {
			peer.t.Fatalf("write fragment: %v", err)
		}
		first, offset = false, end
	}
}

// readMessage reassembles the next provider message, answering pings inline.
func (peer *wsPeer) readMessage() (byte, []byte, error) {
	var (
		message  []byte
		opcode   byte
		assembly bool
	)
	for {
		fin, frameOpcode, payload, err := peer.readFrame()
		if err != nil {
			return 0, nil, err
		}
		if frameOpcode >= 0x8 {
			switch frameOpcode {
			case 0x9:
				if err := peer.writeFrame(true, 0xA, payload); err != nil {
					return 0, nil, err
				}
			case 0x8:
				code := 1005
				if len(payload) >= 2 {
					code = int(binary.BigEndian.Uint16(payload[:2]))
				}
				return 0, payload, &wsPeerClose{Code: code, Reason: string(payload[min(2, len(payload)):])}
			}
			continue
		}
		if !assembly {
			assembly, opcode = true, frameOpcode
		}
		message = append(message, payload...)
		if fin {
			return opcode, message, nil
		}
	}
}

// readFrame reads exactly one provider frame and refuses a masked one: RFC 6455
// section 5.1 forbids a server from masking.
func (peer *wsPeer) readFrame() (bool, byte, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(peer.rw, header[:]); err != nil {
		return false, 0, nil, err
	}
	if header[1]&0x80 != 0 {
		return false, 0, nil, errors.New("provider masked a server frame")
	}
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var buf [2]byte
		if _, err := io.ReadFull(peer.rw, buf[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(buf[:]))
	case 127:
		var buf [8]byte
		if _, err := io.ReadFull(peer.rw, buf[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(buf[:])
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(peer.rw, payload); err != nil {
		return false, 0, nil, err
	}
	return header[0]&0x80 != 0, header[0] & 0x0f, payload, nil
}

// wsPeerClose reports the provider's close frame.
type wsPeerClose struct {
	Code   int
	Reason string
}

func (err *wsPeerClose) Error() string {
	return fmt.Sprintf("provider closed with code %d", err.Code)
}

// next returns the next provider frame, parsed and validated by the published
// contract. Every provider frame is checked here, so a provider that emits a
// frame its own wire refuses fails the test that reads it.
func (peer *wsPeer) next() (any, error) {
	peer.deadline()
	opcode, payload, err := peer.readMessage()
	if err != nil {
		return nil, err
	}
	if opcode != 0x1 {
		return nil, fmt.Errorf("provider sent opcode %d, want a text frame", opcode)
	}
	frame, diagnostics := clientcontract.ParseAndValidateWebSocketFrameV1(payload)
	if len(diagnostics) > 0 {
		return nil, fmt.Errorf("provider frame is refused by its own contract: %v (%s)", diagnostics, payload)
	}
	return frame, nil
}

// expect reads until it sees a frame of the wanted type, answering the
// heartbeats it is not waiting for.
func (peer *wsPeer) expect(want clientcontract.WebSocketFrameType) any {
	peer.t.Helper()
	for {
		frame, err := peer.next()
		if err != nil {
			peer.t.Fatalf("waiting for a %s frame: %v", want, err)
		}
		if wsFrameType(frame) == want {
			return frame
		}
		heartbeat, ok := frame.(*clientcontract.WebSocketHeartbeatFrameV1)
		if !ok {
			peer.t.Fatalf("expected a %s frame, read a %s frame (%#v)", want, wsFrameType(frame), frame)
		}
		peer.pongs = append(peer.pongs, heartbeat.Nonce)
		if heartbeat.Type == clientcontract.WebSocketFramePing {
			peer.send(&clientcontract.WebSocketHeartbeatFrameV1{
				V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFramePong, Nonce: heartbeat.Nonce,
			})
		}
	}
}

// expectError reads the terminal error frame and returns its wire error.
func (peer *wsPeer) expectError() clientcontract.WebSocketRemoteErrorV1 {
	peer.t.Helper()
	frame, ok := peer.expect(clientcontract.WebSocketFrameError).(*clientcontract.WebSocketErrorFrameV1)
	if !ok {
		peer.t.Fatal("provider did not answer with a typed error frame")
	}
	return frame.Error
}

// expectClose reads until the provider closes and returns the close code.
func (peer *wsPeer) expectClose() int {
	peer.t.Helper()
	peer.deadline()
	for {
		_, _, err := peer.readMessage()
		if err == nil {
			continue
		}
		var closed *wsPeerClose
		if errors.As(err, &closed) {
			return closed.Code
		}
		peer.t.Fatalf("waiting for the provider close frame: %v", err)
	}
}

// wsFrameType names one parsed frame.
func wsFrameType(frame any) clientcontract.WebSocketFrameType {
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

// wsInit builds a well-formed init frame for one route.
func wsInit(operationID string, mutate ...func(*clientcontract.WebSocketInitFrameV1)) *clientcontract.WebSocketInitFrameV1 {
	frame := &clientcontract.WebSocketInitFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameInit,
		OperationID:    operationID,
		ClientID:       "harness-consumer",
		DeadlineUnixMs: "0",
		BudgetMs:       "0",
		Credentials: []clientcontract.WebSocketCredentialV1{
			{Profile: "service", Value: "Bearer harness-token"},
		},
		Headers: []clientcontract.WebSocketHeaderV1{},
	}
	for _, apply := range mutate {
		apply(frame)
	}
	return frame
}

// wsMessage builds a client message frame carrying one JSON value.
func wsMessage(t *testing.T, sequence string, value any) *clientcontract.WebSocketMessageFrameV1 {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode message payload: %v", err)
	}
	return &clientcontract.WebSocketMessageFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameMessage, Sequence: sequence,
		Payload: clientcontract.WebSocketEncodedPayloadV1{
			Encoding: clientcontract.EncodingJSON, Value: encoded,
		},
	}
}

// wsDecode reads one JSON payload into a typed value.
func wsDecode[T any](t *testing.T, payload clientcontract.WebSocketEncodedPayloadV1) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(payload.Value, &value); err != nil {
		t.Fatalf("decode payload %s: %v", payload.Value, err)
	}
	return value
}

// millis is the pointer form declared policies use.
func millis(value int) *int { return &value }

// wsEvent is the provider message shape used across these tests. Payload
// exercises empty byte slices on the wire; it is never left nil, because the
// published wire refuses a JSON null anywhere inside a frame.
type wsEvent struct {
	ID      string `json:"id"`
	Payload []byte `json:"payload"`
}

// event builds a message value with the explicit empty payload the wire needs.
func event(id string) wsEvent { return wsEvent{ID: id, Payload: []byte{}} }

// wsSummary is the single declared value a client stream returns.
type wsSummary struct {
	Count int `json:"count"`
}

// contains reports whether text holds want, with a readable failure.
func requireContains(t *testing.T, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("got %q, want it to contain %q", text, want)
	}
}

// bytesBound is the pointer form a declared byte bound uses.
func bytesBound(value int64) *int64 { return &value }

// count is the pointer form a declared message count uses.
func count(value int) *int { return &value }

// asWebSocketRefusal unwraps a typed provider refusal.
func asWebSocketRefusal(err error, target **webSocketRefusal) bool {
	return errors.As(err, target)
}

// wsSendProbe is a session with no socket: it records the frames the provider
// would put on the wire, so an emission the contract refuses is observed
// without a peer to read it.
type wsSendProbe struct {
	*webSocketSession
	written [][]byte
}

// newSendOnlySession returns a probe whose conversation has already accepted
// the client's init frame, which is where every provider frame starts.
func newSendOnlySession(t *testing.T, stream clientcontract.StreamMode) *wsSendProbe {
	t.Helper()
	probe := &wsSendProbe{}
	probe.webSocketSession = &webSocketSession{
		service: &webSocketService{
			operationID: "getProbe",
			stream:      stream,
			transport: clientcontract.Transport{
				Protocol: clientcontract.TransportWebSocket, Path: "/probe",
				Encoding: clientcontract.EncodingJSON,
				WebSocket: &clientcontract.WebSocketTransport{
					Subprotocol: clientcontract.WebSocketSubprotocolV1,
				},
			},
		},
		conversation: clientcontract.NewWebSocketConversationV1(stream, clientcontract.EncodingJSON, false),
	}
	probe.write = func(encoded []byte) error {
		probe.written = append(probe.written, append([]byte(nil), encoded...))
		return nil
	}
	init := mustParseWSFrame(t, wsInit("getProbe"))
	if diagnostics := probe.conversation.Accept(init, clientcontract.WebSocketClientToServer); len(diagnostics) > 0 {
		t.Fatalf("probe init refused: %v", diagnostics)
	}
	return probe
}

// mustParseWSFrame round-trips a frame through the published strict parser.
func mustParseWSFrame(t *testing.T, frame any) any {
	t.Helper()
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	parsed, diagnostics := clientcontract.ParseAndValidateWebSocketFrameV1(encoded)
	if len(diagnostics) > 0 {
		t.Fatalf("frame refused by the contract: %v", diagnostics)
	}
	return parsed
}

// mustWSMessage builds a provider message frame at one sequence.
func mustWSMessage(sequence uint64) *clientcontract.WebSocketMessageFrameV1 {
	return &clientcontract.WebSocketMessageFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameMessage,
		Sequence: strconv.FormatUint(sequence, 10),
		Payload: clientcontract.WebSocketEncodedPayloadV1{
			Encoding: clientcontract.EncodingJSON, Value: json.RawMessage(`{}`),
		},
	}
}

// --- published corpus scenes ----------------------------------------------

// wsScene is one published conversation from protocols/clientcontract.
type wsScene struct {
	name        string
	operationID string
	stream      string
	steps       []wsSceneStep
}

type wsSceneStep struct {
	Direction string          `json:"direction"`
	Frame     json.RawMessage `json:"frame"`
}

// loadWSScene reads one scene, keeping each frame's exact bytes so an unknown
// field or a non-canonical value survives the replay.
func loadWSScene(t *testing.T, path string) *wsScene {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read scene: %v", err)
	}
	document := struct {
		OperationID string        `json:"operationId"`
		Stream      string        `json:"stream"`
		Steps       []wsSceneStep `json:"steps"`
	}{}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode scene %s: %v", path, err)
	}
	return &wsScene{
		name: filepath.Base(path), operationID: document.OperationID,
		stream: document.Stream, steps: document.Steps,
	}
}

// wsSceneRoute maps a scene's declared stream mode onto this provider's route.
func wsSceneRoute(t *testing.T, stream string) (path, operationID string) {
	t.Helper()
	switch stream {
	case "server":
		return "/watch", "getWatch"
	case "client":
		return "/upload", "getUpload"
	case "bidirectional":
		return "/chat", "getChat"
	default:
		t.Fatalf("scene declares an unsupported stream mode %q", stream)
		return "", ""
	}
}

// dial stands up a provider carrying the three stream shapes and opens the
// socket the scene belongs on.
func (scene *wsScene) dial(t *testing.T) (*wsPeer, string) {
	t.Helper()
	provider := newWSProvider(t, wsProviderOptions{register: func(plugin *Plugin) {
		plugin.Register(Endpoint("GET", "/watch").
			Returns(StreamOf[wsEvent]()).
			Handle(ServerStream(func(_ *ServerStreamContext[wsEvent]) error { return nil })))
		plugin.Register(Endpoint("GET", "/upload").
			Body(StreamOf[wsEvent]()).
			Returns(Type[wsSummary]()).
			Handle(ClientStream(func(stream *ClientStreamContext[wsEvent, wsSummary]) error {
				total := 0
				for range stream.Messages() {
					total++
				}
				if err := stream.Err(); err != nil {
					return err
				}
				stream.Result(wsSummary{Count: total})
				return nil
			})))
		plugin.Register(Endpoint("GET", "/chat").
			Body(StreamOf[wsEvent]()).
			Returns(StreamOf[wsEvent]()).
			Handle(BidiStream(func(stream *BidiStreamContext[wsEvent, wsEvent]) error {
				for message := range stream.Messages() {
					if err := stream.Send(event("ack:" + message.ID)); err != nil {
						return err
					}
				}
				return stream.Err()
			})))
	}})
	path, operationID := wsSceneRoute(t, scene.stream)
	return dialWSPeer(t, provider, path), operationID
}

// driveClientFrames replays every client-to-server frame of the scene against
// the live provider and returns the typed error the provider answered with, or
// nil when it carried the scene to a successful terminal. Only the scene's
// declared operation identity is rewritten: every other byte, including an
// unknown field, reaches the provider exactly as the corpus wrote it.
func (scene *wsScene) driveClientFrames(t *testing.T, peer *wsPeer, operationID string) *clientcontract.WebSocketRemoteErrorV1 {
	t.Helper()
	admitted := false
	for _, step := range scene.steps {
		if step.Direction != "client-to-server" {
			continue
		}
		frame := []byte(strings.ReplaceAll(string(step.Frame),
			`"`+scene.operationID+`"`, `"`+operationID+`"`))
		// The wire forbids an application frame before ready, so the replay
		// waits for admission exactly where the scene does. Heartbeats and
		// cancellation are legal from await-ready and go straight through.
		if !admitted && wsSceneNeedsAdmission(step.Frame) {
			if failure := peer.awaitReady(); failure != nil {
				return failure
			}
			admitted = true
		}
		peer.deadline()
		// A provider that already refused the scene has closed the socket, so a
		// later write failing is part of the refusal, not a test failure.
		if err := peer.writeFrame(true, 0x1, frame); err != nil {
			break
		}
	}
	for {
		frame, err := peer.next()
		if err != nil {
			return nil
		}
		switch value := frame.(type) {
		case *clientcontract.WebSocketErrorFrameV1:
			failure := value.Error
			return &failure
		case *clientcontract.WebSocketResultFrameV1:
			return nil
		case *clientcontract.WebSocketHeartbeatFrameV1:
			if value.Type == clientcontract.WebSocketFramePing {
				peer.send(&clientcontract.WebSocketHeartbeatFrameV1{
					V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFramePong, Nonce: value.Nonce,
				})
			}
		}
	}
}

// wsSceneNeedsAdmission reports whether a client frame may only be sent once
// the provider has answered ready.
func wsSceneNeedsAdmission(frame json.RawMessage) bool {
	header := struct {
		Type string `json:"type"`
	}{}
	if err := json.Unmarshal(frame, &header); err != nil {
		return false
	}
	return header.Type == string(clientcontract.WebSocketFrameMessage) ||
		header.Type == string(clientcontract.WebSocketFrameHalfClose)
}

// awaitReady reads until the provider admits the conversation, returning the
// typed error it answered with instead.
func (peer *wsPeer) awaitReady() *clientcontract.WebSocketRemoteErrorV1 {
	for {
		frame, err := peer.next()
		if err != nil {
			return &clientcontract.WebSocketRemoteErrorV1{
				Status: 500, Code: "transport", Message: err.Error(),
			}
		}
		switch value := frame.(type) {
		case *clientcontract.WebSocketReadyFrameV1:
			return nil
		case *clientcontract.WebSocketErrorFrameV1:
			failure := value.Error
			return &failure
		case *clientcontract.WebSocketHeartbeatFrameV1:
			if value.Type == clientcontract.WebSocketFramePing {
				peer.send(&clientcontract.WebSocketHeartbeatFrameV1{
					V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFramePong, Nonce: value.Nonce,
				})
			}
		}
	}
}

// handlerContext returns the cancellation context a stream handler observes.
func handlerContext(stream *phttp.StreamContext) context.Context {
	return stream.Context.Context()
}
