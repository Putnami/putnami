package client

import (
	"encoding/json"
	stderrors "errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// webSocketCorpusDir is the published corpus the contract package owns. The
// client reads it where the module replacement already points, so one corpus
// proves the contract, this runtime, and every other first-party runtime.
const webSocketCorpusDir = "../../../protocols/clientcontract/fixtures/websocket"

type wsSceneStep struct {
	Direction clientcontract.WebSocketDirection `json:"direction"`
	Frame     json.RawMessage                   `json:"frame"`
}

type wsScene struct {
	OperationID         string                         `json:"operationId"`
	Stream              clientcontract.StreamMode      `json:"stream"`
	Encoding            clientcontract.Encoding        `json:"encoding"`
	Idempotency         clientcontract.IdempotencyKind `json:"idempotency"`
	Resume              bool                           `json:"resume"`
	MaxFrameBytes       int64                          `json:"maxFrameBytes"`
	MaxBufferedMessages int                            `json:"maxBufferedMessages"`
	Steps               []wsSceneStep                  `json:"steps"`
}

type wsCorpusExpectations struct {
	Valid   []string          `json:"valid"`
	Invalid map[string]string `json:"invalid"`
}

// wsSceneSchemas types the payloads each scene carries. A scene the client
// refuses before decoding still needs a declared shape, because a generated
// operation always has one.
var wsSceneSchemas = map[string]clientcontract.Schema{
	"server-stream.json": {Type: "string"},
}

// wsSceneSchema is the declared message shape for one scene.
func wsSceneSchema(name string) clientcontract.Schema {
	if schema, ok := wsSceneSchemas[name]; ok {
		return schema
	}
	return stringObjectSchema("id", "text", "value")
}

// wsSceneOutcome is what replaying one corpus scene through the client produced.
type wsSceneOutcome struct {
	err       error
	connected bool
}

// TestWebSocketClientMatchesThePublishedCorpus replays the published corpus
// through the real client against a scripted provider. A valid scene must never
// be refused by the client, and every invalid scene must be refused with the
// same diagnostic code the corpus declares — the same word the contract, the
// provider and every other first-party runtime use.
func TestWebSocketClientMatchesThePublishedCorpus(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "websocket-corpus-conformance",
		"the-client-refuses-every-invalid-corpus-scene-with-the-published-diagnostic-code")
	expectations := readWebSocketExpectations(t)
	if len(expectations.Valid) == 0 || len(expectations.Invalid) == 0 {
		t.Fatal("the published corpus declares no scenes")
	}

	// The corpus scenes the client can never emit: their invalid frame is a
	// client frame this runtime does not compose. Each has its own test below.
	unemittable := map[string]bool{
		"cancel-before-init.json":          true,
		"secret-shaped-unknown-field.json": true,
	}
	// A proto transport is refused at generation, and by this runtime before any
	// network byte, so its otherwise valid scene never reaches the wire.
	unsupportedEncoding := map[string]bool{"client-stream-empty-proto.json": true}

	replayed := 0
	for _, name := range expectations.Valid {
		t.Run("valid/"+name, func(t *testing.T) {
			scene := readWebSocketScene(t, filepath.Join(webSocketCorpusDir, "valid", name))
			outcome := replayWebSocketScene(t, name, scene)
			if unsupportedEncoding[name] {
				if outcome.connected || !perrors.Is(outcome.err, CodeClientConfig) ||
					!strings.Contains(outcome.err.Error(), string(scene.Encoding)) {
					t.Fatalf("a proto transport must fail closed before a socket: connected %t, err %v",
						outcome.connected, outcome.err)
				}
				return
			}
			var contractErr *WebSocketContractError
			if stderrors.As(outcome.err, &contractErr) {
				t.Fatalf("the client refused a valid corpus scene: %s", contractErr.Code)
			}
			if !outcome.connected {
				t.Fatal("a valid scene never reached the provider")
			}
		})
		replayed++
	}

	names := make([]string, 0, len(expectations.Invalid))
	for name := range expectations.Invalid {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if unemittable[name] {
			continue
		}
		t.Run("invalid/"+name, func(t *testing.T) {
			scene := readWebSocketScene(t, filepath.Join(webSocketCorpusDir, "invalid", name))
			outcome := replayWebSocketScene(t, name, scene)
			var contractErr *WebSocketContractError
			if !stderrors.As(outcome.err, &contractErr) {
				t.Fatalf("the client accepted an invalid corpus scene: %T %v", outcome.err, outcome.err)
			}
			if contractErr.Code != expectations.Invalid[name] {
				t.Fatalf("diagnostic code = %q, corpus declares %q", contractErr.Code, expectations.Invalid[name])
			}
			if strings.Contains(contractErr.Error(), "must-not-appear-in-diagnostics") {
				t.Fatalf("the refusal leaked frame material: %v", contractErr)
			}
		})
		replayed++
	}
	if replayed < len(expectations.Valid) {
		t.Fatalf("replayed %d scenes", replayed)
	}
}

// replayWebSocketScene drives one corpus scene through the real client while a
// scripted provider plays the provider side of the same scene.
func replayWebSocketScene(t *testing.T, name string, scene wsScene) wsSceneOutcome {
	t.Helper()
	server := newWSTestServer(t, wsTestOptions{
		maxMessageBytes: scene.MaxFrameBytes,
		play: func(peer *wsTestPeer) {
			for _, step := range scene.Steps {
				if step.Direction == clientcontract.WebSocketServerToClient {
					if err := peer.conn.writeText(step.Frame); err != nil {
						return
					}
					continue
				}
				if !peer.await(webSocketSceneFrameType(step.Frame)) {
					return
				}
			}
			peer.drain()
		},
	})
	bound := webSocketTestClient(t, server.URL)
	schema := wsSceneSchema(name)
	shapes := &clientcontract.MessageShapes{Output: &schema}
	if scene.Stream != clientcontract.StreamServer {
		shapes.Input = &schema
	}
	policy := &clientcontract.ResiliencePolicy{Stream: &clientcontract.StreamPolicy{
		MaxFrameBytes:       &scene.MaxFrameBytes,
		MaxBufferedMessages: &scene.MaxBufferedMessages,
	}}
	if webSocketSceneHasClientPing(scene) {
		policy.Stream.HeartbeatMs = intPointer(10)
	}
	operation := webSocketTestOperation(scene.OperationID, scene.Stream, scene.Encoding, scene.Resume,
		shapes, policy, nil)
	options := webSocketOpenOptions{resume: webSocketSceneResume(t, scene)}

	service, err := openWebSocketService(t.Context(), bound, &Request{}, operation, scene.Stream, options, nil)
	if err != nil {
		return wsSceneOutcome{err: err, connected: server.upgradeCount() > 0}
	}
	outcome := wsSceneOutcome{connected: true, err: driveWebSocketScene(t, service, scene)}
	server.assertNoFailures(t)
	return outcome
}

// driveWebSocketScene plays the caller's side of one scene on the typed handle
// its stream mode declares.
func driveWebSocketScene(t *testing.T, service *wsService, scene wsScene) error {
	t.Helper()
	messages := make(chan json.RawMessage, max(scene.MaxBufferedMessages, 1))
	delivery := &webSocketDelivery[json.RawMessage]{messages: messages}
	terminal := make(chan error, 1)
	go func() {
		defer close(messages)
		terminal <- runWebSocketStream(service, delivery)
	}()

	cancels := webSocketSceneHasClientCancel(scene)
	sendErr := error(nil)
	for _, payload := range webSocketSceneClientMessages(scene) {
		if err := service.sendMessage(payload); err != nil {
			sendErr = err
			break
		}
	}
	if sendErr == nil && webSocketSceneHasHalfClose(scene) {
		sendErr = service.closeSend()
	}
	delivered := 0
	for range messages {
		delivered++
		if cancels && delivered == 1 {
			service.requestCancel()
		}
	}
	if err := <-terminal; err != nil {
		return err
	}
	return sendErr
}

// webSocketSceneFrameType names the frame a corpus step carries.
func webSocketSceneFrameType(frame json.RawMessage) clientcontract.WebSocketFrameType {
	var header struct {
		Type clientcontract.WebSocketFrameType `json:"type"`
	}
	if err := json.Unmarshal(frame, &header); err != nil {
		return ""
	}
	return header.Type
}

// webSocketSceneClientMessages returns the application payloads the caller sends.
func webSocketSceneClientMessages(scene wsScene) []json.RawMessage {
	payloads := make([]json.RawMessage, 0, len(scene.Steps))
	for _, step := range scene.Steps {
		if step.Direction != clientcontract.WebSocketClientToServer ||
			webSocketSceneFrameType(step.Frame) != clientcontract.WebSocketFrameMessage {
			continue
		}
		var message clientcontract.WebSocketMessageFrameV1
		if err := json.Unmarshal(step.Frame, &message); err != nil {
			continue
		}
		payloads = append(payloads, message.Payload.Value)
	}
	return payloads
}

// webSocketSceneResume returns the resume request the scene's init frame carries.
func webSocketSceneResume(t *testing.T, scene wsScene) *clientcontract.WebSocketResumeRequestV1 {
	t.Helper()
	for _, step := range scene.Steps {
		if webSocketSceneFrameType(step.Frame) != clientcontract.WebSocketFrameInit {
			continue
		}
		var initFrame clientcontract.WebSocketInitFrameV1
		if err := json.Unmarshal(step.Frame, &initFrame); err != nil {
			return nil
		}
		return initFrame.Resume
	}
	return nil
}

func webSocketSceneHasClientCancel(scene wsScene) bool {
	return webSocketSceneHasClientFrame(scene, clientcontract.WebSocketFrameCancel)
}

func webSocketSceneHasHalfClose(scene wsScene) bool {
	return webSocketSceneHasClientFrame(scene, clientcontract.WebSocketFrameHalfClose)
}

func webSocketSceneHasClientPing(scene wsScene) bool {
	return webSocketSceneHasClientFrame(scene, clientcontract.WebSocketFramePing)
}

func webSocketSceneHasClientFrame(scene wsScene, want clientcontract.WebSocketFrameType) bool {
	for _, step := range scene.Steps {
		if step.Direction == clientcontract.WebSocketClientToServer &&
			webSocketSceneFrameType(step.Frame) == want {
			return true
		}
	}
	return false
}

func readWebSocketExpectations(t *testing.T) wsCorpusExpectations {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(webSocketCorpusDir, "expectations.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expectations wsCorpusExpectations
	if err := json.Unmarshal(data, &expectations); err != nil {
		t.Fatal(err)
	}
	return expectations
}

func readWebSocketScene(t *testing.T, path string) wsScene {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	var scene wsScene
	if err := json.Unmarshal(data, &scene); err != nil {
		t.Fatal(err)
	}
	return scene
}

// TestWebSocketClientNeverEmitsACancelBeforeInit is the corpus scene this
// runtime cannot replay because it cannot compose the frame: the published
// conversation refuses the client's own cancel before an operation identity
// exists, with the code the corpus declares.
func TestWebSocketClientNeverEmitsACancelBeforeInit(t *testing.T) {
	expectations := readWebSocketExpectations(t)
	service := &wsService{conversation: clientcontract.NewWebSocketConversationV1(
		clientcontract.StreamServer, clientcontract.EncodingJSON, false)}
	err := service.accept(&clientcontract.WebSocketCancelFrameV1{
		V: clientcontract.ProtocolVersion, Type: clientcontract.WebSocketFrameCancel,
		Code: clientcontract.WebSocketCancelCodeCanceled,
	}, clientcontract.WebSocketClientToServer)
	var contractErr *WebSocketContractError
	if !stderrors.As(err, &contractErr) || contractErr.Code != expectations.Invalid["cancel-before-init.json"] {
		t.Fatalf("error = %T %v", err, err)
	}
}

// TestWebSocketClientFramesAreAlwaysContractClean is the other corpus scene this
// runtime cannot replay: the client composes closed structures, so an unknown
// secret-shaped member cannot reach the wire. The frames it actually emitted are
// re-parsed by the same strict parser the corpus scene fails.
func TestWebSocketClientFramesAreAlwaysContractClean(t *testing.T) {
	expectations := readWebSocketExpectations(t)
	corpus, err := os.ReadFile(filepath.Join(webSocketCorpusDir, "invalid", "secret-shaped-unknown-field.json"))
	if err != nil {
		t.Fatal(err)
	}
	var scene wsScene
	if err := json.Unmarshal(corpus, &scene); err != nil {
		t.Fatal(err)
	}
	_, diagnostics := clientcontract.ParseAndValidateWebSocketFrameV1(scene.Steps[0].Frame)
	if len(diagnostics) == 0 || diagnostics[0].Code != expectations.Invalid["secret-shaped-unknown-field.json"] {
		t.Fatalf("the corpus scene is no longer refused by the parser: %v", diagnostics)
	}

	server := newWSTestServer(t, wsTestOptions{play: func(peer *wsTestPeer) {
		peer.expect(clientcontract.WebSocketFrameInit)
		peer.send(webSocketReadyFrame(false, ""))
		peer.expect(clientcontract.WebSocketFrameMessage)
		peer.expect(clientcontract.WebSocketFrameHalfClose)
		peer.send(webSocketResultFrame(`{"value":"stored"}`))
		peer.drain()
	}})
	bound := webSocketTestClient(t, server.URL)
	stream, openErr := OpenClientStream[wsItem, wsItem](t.Context(), bound, &Request{}, webSocketClientStreamOperation())
	if openErr != nil {
		t.Fatal(openErr)
	}
	if err := stream.Send(t.Context(), wsItem{Value: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Result(t.Context()); err != nil {
		t.Fatal(err)
	}
	server.waitForPlay(t)
	server.assertNoFailures(t)
	emitted := server.clientFrames()
	if len(emitted) < 3 {
		t.Fatalf("the client emitted %d frames", len(emitted))
	}
	for _, frame := range emitted {
		encoded, marshalErr := json.Marshal(frame)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if _, diagnostics := clientcontract.ParseAndValidateWebSocketFrameV1(encoded); len(diagnostics) > 0 {
			t.Fatalf("an emitted client frame is refused by the contract: %v", diagnostics)
		}
	}
}

// TestWebSocketRuntimeHasNoPhaseRuleOfItsOwn is the measurable form of the
// invariant the published state machine exists for: this runtime drives
// WebSocketConversationV1 and names no state of its own, so a second copy of the
// transition table cannot be born here.
func TestWebSocketRuntimeHasNoPhaseRuleOfItsOwn(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "websocket-shared-lifecycle",
		"the-websocket-runtime-drives-the-published-conversation-and-names-no-state")
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"NextWebSocketStateV1", "WebSocketState", "WebSocketAwaitInit", "WebSocketAwaitReady",
		"WebSocketOpen", "WebSocketHalfClosed", "WebSocketTerminal",
	}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "ws_") || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		source, readErr := os.ReadFile(filepath.Clean(name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, symbol := range forbidden {
			if strings.Contains(string(source), symbol) {
				t.Errorf("%s names %s itself instead of driving the published conversation", name, symbol)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("the guard scanned no websocket runtime file")
	}
}

// TestWebSocketCorpusIsReachable keeps the corpus path honest: a moved or empty
// corpus must fail here rather than turn the replay into a vacuous pass.
func TestWebSocketCorpusIsReachable(t *testing.T) {
	for _, dir := range []string{"valid", "invalid"} {
		entries, err := os.ReadDir(filepath.Join(webSocketCorpusDir, dir))
		if err != nil {
			t.Fatalf("the published corpus is not reachable at %s: %v", webSocketCorpusDir, err)
		}
		if len(entries) == 0 {
			t.Fatalf("the published corpus directory %s is empty", dir)
		}
	}
}
