package clientcontract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

type webSocketFixtureExpectations struct {
	Valid   []string          `json:"valid"`
	Invalid map[string]string `json:"invalid"`
}

type webSocketScenario struct {
	OperationID         string          `json:"operationId"`
	Stream              StreamMode      `json:"stream"`
	Encoding            Encoding        `json:"encoding"`
	Idempotency         IdempotencyKind `json:"idempotency"`
	Resume              bool            `json:"resume"`
	MaxFrameBytes       int             `json:"maxFrameBytes"`
	MaxBufferedMessages int             `json:"maxBufferedMessages"`
	Steps               []struct {
		Direction WebSocketDirection `json:"direction"`
		Frame     json.RawMessage    `json:"frame"`
	} `json:"steps"`
}

func TestWebSocketStateMachineFixtures(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "websocket-state-machine", "auth-is-admitted-before-handler-messages")
	spectest.Proves(t, "client-contract/first-party-generated-clients", "websocket-state-machine", "control-data-half-close-terminal-and-resume-transitions-are-closed")
	spectest.Proves(t, "client-contract/first-party-generated-clients", "websocket-state-machine", "frame-bounds-and-wide-sequences-are-enforced-and-lossless")

	data, err := os.ReadFile("fixtures/websocket/expectations.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected webSocketFixtureExpectations
	if err := json.Unmarshal(data, &expected); err != nil {
		t.Fatal(err)
	}
	assertFixtureCoverage(t, "fixtures/websocket/valid", expected.Valid)
	invalidNames := make([]string, 0, len(expected.Invalid))
	for name := range expected.Invalid {
		invalidNames = append(invalidNames, name)
	}
	sort.Strings(invalidNames)
	assertFixtureCoverage(t, "fixtures/websocket/invalid", invalidNames)

	for _, name := range expected.Valid {
		t.Run("valid/"+name, func(t *testing.T) {
			if diags := validateWebSocketScenarioFile(t, filepath.Join("fixtures/websocket/valid", name)); diag.HasErrors(diags) {
				t.Fatalf("valid state-machine fixture produced diagnostics: %v", diags)
			}
		})
	}
	for _, name := range invalidNames {
		t.Run("invalid/"+name, func(t *testing.T) {
			diags := validateWebSocketScenarioFile(t, filepath.Join("fixtures/websocket/invalid", name))
			want := expected.Invalid[name]
			found := false
			for _, diagnostic := range diags {
				if strings.Contains(diagnostic.Message, "must-not-appear-in-diagnostics") || strings.Contains(diagnostic.Message, "super-secret") {
					t.Fatalf("runtime diagnostic leaked frame material: %v", diags)
				}
				if diagnostic.Code == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("diagnostics %v do not contain expected code %q", diags, want)
			}
		})
	}
}

func validateWebSocketScenarioFile(t *testing.T, path string) []diag.Diagnostic {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var scenario webSocketScenario
	if err := json.Unmarshal(data, &scenario); err != nil {
		return []diag.Diagnostic{webSocketFrameParseDiagnostic()}
	}
	return validateWebSocketScenario(scenario)
}

// validateWebSocketScenario replays one corpus scenario through the published
// state machine. It deliberately holds no rule of its own: every transition,
// sequence and admission decision comes from WebSocketConversationV1, so the
// corpus proves the shipped table rather than a second copy of it.
func validateWebSocketScenario(scenario webSocketScenario) []diag.Diagnostic {
	if scenario.MaxFrameBytes <= 0 || scenario.MaxBufferedMessages <= 0 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidResilience, "", "stream bounds must be positive")}
	}
	document := &DocumentV1{
		ProtocolVersion: ProtocolVersion,
		Service:         Service{ID: "fixtures.widgets", Audience: "fixtures.widgets"},
		Credentials: map[string]CredentialProfile{
			"service": {Kind: CredentialServiceToken},
		},
	}
	operation := &OperationV1{
		Stream: scenario.Stream,
		Security: Security{Alternatives: []SecurityAlternative{{
			AllOf: []SecurityRequirement{{Profile: "service"}},
		}}},
		Idempotency: Idempotency{Kind: scenario.Idempotency},
	}
	transport := Transport{
		Protocol: TransportWebSocket, Path: "/fixtures", Encoding: scenario.Encoding,
		WebSocket: &WebSocketTransport{Subprotocol: WebSocketSubprotocolV1, Resume: scenario.Resume},
	}
	conversation := NewWebSocketConversationForOperationV1(scenario.OperationID, operation, document, transport)

	for index, step := range scenario.Steps {
		field := fmt.Sprintf("steps[%d]", index)
		if len(step.Frame) > scenario.MaxFrameBytes {
			return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidResilience, field, "frame exceeds maxFrameBytes")}
		}
		parsed, frameDiags := ParseAndValidateWebSocketFrameV1(step.Frame)
		if diag.HasErrors(frameDiags) {
			return frameDiags
		}
		if stepDiags := conversation.Accept(parsed, step.Direction); diag.HasErrors(stepDiags) {
			return stepDiags
		}
	}
	if conversation.State() != WebSocketTerminal {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidTransport, "steps", "scenario has no terminal frame")}
	}
	return nil
}

// TestWebSocketTransitionTableIsShippedNotTestOnly fails if this file ever
// regains a frame-type switch. The table lived here before it was published as
// shipped code; publishing it is worthless if a copy quietly grows back
// beside it, because the three runtimes that consume it would then have two
// authorities to choose from.
func TestWebSocketTransitionTableIsShippedNotTestOnly(t *testing.T) {
	source, err := os.ReadFile("websocket_conformance_test.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)
	guard := regexp.MustCompile(`(?m)^\s*(case \*WebSocket\w*FrameV1|switch \w+ := \w+\.\(type\)|switch \w+\.\(type\))`)
	if found := guard.FindAllString(body, -1); len(found) > 0 {
		t.Fatalf("the websocket transition table must stay in websocket.go; this test file switches on frame types: %v", found)
	}
}

// TestWebSocketWireSchemaPublishesClosedFrameVocabulary is the schema-to-Go
// drift guard: every closed vocabulary the parser accepts is exactly the set the
// published schema enumerates, and every frame definition carries exactly the
// fields its Go type marshals.
func TestWebSocketWireSchemaPublishesClosedFrameVocabulary(t *testing.T) {
	spectest.Proves(t, "client-contract/first-party-generated-clients", "websocket-state-machine", "published-wire-schema-matches-frame-vocabulary")
	schema := loadWebSocketWireSchema(t)
	if schema["$id"] != WebSocketWireSchemaURL {
		t.Fatalf("wire schema id = %v, want %q", schema["$id"], WebSocketWireSchemaURL)
	}
	defs := schema["$defs"].(map[string]any)

	t.Run("frame types", func(t *testing.T) {
		want := make([]string, 0, len(WebSocketFrameTypesV1()))
		for _, frameType := range WebSocketFrameTypesV1() {
			want = append(want, string(frameType))
		}
		sort.Strings(want)
		got := map[string]bool{}
		for name, definition := range defs {
			for _, value := range schemaTypeConstants(definition) {
				got[value] = true
			}
			_ = name
		}
		if !reflect.DeepEqual(sortedKeys(got), want) {
			t.Fatalf("wire schema frame types %v, parser accepts %v", sortedKeys(got), want)
		}
		for _, frameType := range WebSocketFrameTypesV1() {
			if _, diags := ParseAndValidateWebSocketFrameV1(minimalFrame(t, frameType)); diag.HasErrors(diags) {
				t.Errorf("parser rejects published frame type %q: %v", frameType, diags)
			}
		}
		if _, diags := ParseAndValidateWebSocketFrameV1([]byte(`{"v":1,"type":"resume"}`)); !diag.HasErrors(diags) {
			t.Error("parser accepts a frame type the schema does not publish")
		}
	})

	t.Run("cancel codes", func(t *testing.T) {
		published := schemaStringSet(defs["cancel"].(map[string]any)["properties"].(map[string]any)["code"])
		want := append([]string(nil), WebSocketCancelCodesV1()...)
		sort.Strings(want)
		if !reflect.DeepEqual(published, want) {
			t.Fatalf("wire schema cancel codes %v, parser accepts %v", published, want)
		}
		for _, code := range WebSocketCancelCodesV1() {
			frame := fmt.Sprintf(`{"v":1,"type":"cancel","code":%q}`, code)
			if _, diags := ParseAndValidateWebSocketFrameV1([]byte(frame)); diag.HasErrors(diags) {
				t.Errorf("parser rejects published cancel code %q: %v", code, diags)
			}
		}
		// The doubled-l spelling is the exact drift D0.8 closed. It is derived
		// rather than written out because `putnami lint` runs misspell with --fix,
		// which silently rewrites that literal and turns this guard inside out.
		doubled := strings.Replace(WebSocketCancelCodeCanceled, "cel", "cell", 1)
		frame := fmt.Sprintf(`{"v":1,"type":"cancel","code":%q}`, doubled)
		if _, diags := ParseAndValidateWebSocketFrameV1([]byte(frame)); !diag.HasErrors(diags) {
			t.Errorf("parser accepts %q; the canonical framework spelling is %q", doubled, WebSocketCancelCodeCanceled)
		}
	})

	t.Run("payload encodings", func(t *testing.T) {
		published := map[string]bool{}
		for _, name := range []string{"jsonPayload", "protoPayload"} {
			definition := defs[name].(map[string]any)["properties"].(map[string]any)["encoding"].(map[string]any)
			published[definition["const"].(string)] = true
		}
		want := make([]string, 0, len(WebSocketPayloadEncodingsV1()))
		for _, encoding := range WebSocketPayloadEncodingsV1() {
			want = append(want, string(encoding))
		}
		sort.Strings(want)
		if !reflect.DeepEqual(sortedKeys(published), want) {
			t.Fatalf("wire schema payload encodings %v, parser accepts %v", sortedKeys(published), want)
		}
		if _, diags := ParseAndValidateWebSocketFrameV1(
			[]byte(`{"v":1,"type":"message","sequence":"1","payload":{"encoding":"cbor","value":{}}}`),
		); !diag.HasErrors(diags) {
			t.Error("parser accepts a payload encoding the schema does not publish")
		}
	})

	t.Run("frame properties", func(t *testing.T) {
		cases := []struct {
			definition string
			value      any
		}{
			{"init", WebSocketInitFrameV1{}},
			{"ready", WebSocketReadyFrameV1{}},
			{"message", WebSocketMessageFrameV1{}},
			{"halfClose", WebSocketHalfCloseFrameV1{}},
			{"result", WebSocketResultFrameV1{}},
			{"error", WebSocketErrorFrameV1{}},
			{"remoteError", WebSocketRemoteErrorV1{}},
			{"cancel", WebSocketCancelFrameV1{}},
			{"heartbeat", WebSocketHeartbeatFrameV1{}},
			{"credential", WebSocketCredentialV1{}},
			{"header", WebSocketHeaderV1{}},
			{"context", WebSocketContextV1{}},
			{"resume", WebSocketResumeRequestV1{}},
		}
		for _, tc := range cases {
			got := schemaPropertyNames(defs[tc.definition])
			want := goJSONFieldNames(reflect.TypeOf(tc.value))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s properties drift:\n schema: %v\n     Go: %v", tc.definition, got, want)
			}
		}
	})

	t.Run("ready requires a resume token when it reports a resumed stream", func(t *testing.T) {
		ready := defs["ready"].(map[string]any)
		conditionals, _ := ready["allOf"].([]any)
		expressed := false
		for _, item := range conditionals {
			branch := item.(map[string]any)
			then, ok := branch["then"].(map[string]any)
			if !ok {
				continue
			}
			for _, required := range then["required"].([]any) {
				if required == "resumeToken" {
					expressed = true
				}
			}
		}
		if !expressed {
			t.Error("the published ready definition does not require resumeToken when resumed is true")
		}
		if _, diags := ParseAndValidateWebSocketFrameV1([]byte(`{"v":1,"type":"ready","resumed":true}`)); !diag.HasErrors(diags) {
			t.Error("parser accepts a resumed ready frame without a resume token")
		}
	})
}

// TestWebSocketTransitionTableIsTotalAndClosed asserts the published table
// answers every (state, frame, direction) triple without panicking and never
// invents a state outside the closed vocabulary. A runtime that consumes the
// table needs that totality: an unanswered triple would become a local rule.
func TestWebSocketTransitionTableIsTotalAndClosed(t *testing.T) {
	states := map[WebSocketState]bool{}
	for _, state := range WebSocketStatesV1() {
		states[state] = true
	}
	directions := []WebSocketDirection{WebSocketClientToServer, WebSocketServerToClient}
	streams := []StreamMode{StreamUnary, StreamServer, StreamClient, StreamBidirectional}
	for _, state := range WebSocketStatesV1() {
		for _, frameType := range WebSocketFrameTypesV1() {
			frame, diags := ParseAndValidateWebSocketFrameV1(minimalFrame(t, frameType))
			if diag.HasErrors(diags) {
				t.Fatalf("minimal %q frame is invalid: %v", frameType, diags)
			}
			for _, direction := range directions {
				for _, stream := range streams {
					next, transitionDiags := NextWebSocketStateV1(state, frame, direction, stream)
					if !states[next] {
						t.Fatalf("transition (%s, %s, %s, %s) returned unknown state %q",
							state, frameType, direction, stream, next)
					}
					if diag.HasErrors(transitionDiags) && next != state {
						t.Fatalf("refused transition (%s, %s, %s, %s) moved the state to %q",
							state, frameType, direction, stream, next)
					}
				}
			}
		}
	}
}

// TestWebSocketTransitionsMatchTheDecidedTable pins the eight transitions D0.8
// decided, each as its own named case, so a regression names the rule it broke.
func TestWebSocketTransitionsMatchTheDecidedTable(t *testing.T) {
	cases := []struct {
		name      string
		state     WebSocketState
		frameType WebSocketFrameType
		direction WebSocketDirection
		stream    StreamMode
		allowed   bool
		next      WebSocketState
	}{
		{"cancel is refused before init exists", WebSocketAwaitInit, WebSocketFrameCancel, WebSocketClientToServer, StreamServer, false, WebSocketAwaitInit},
		{"cancel is accepted while awaiting admission", WebSocketAwaitReady, WebSocketFrameCancel, WebSocketClientToServer, StreamServer, true, WebSocketTerminal},
		{"cancel is accepted from open", WebSocketOpen, WebSocketFrameCancel, WebSocketClientToServer, StreamServer, true, WebSocketTerminal},
		{"the provider never cancels", WebSocketOpen, WebSocketFrameCancel, WebSocketServerToClient, StreamServer, false, WebSocketOpen},
		{"a refused admission is a typed error", WebSocketAwaitInit, WebSocketFrameError, WebSocketServerToClient, StreamServer, true, WebSocketTerminal},
		{"heartbeats start at await-ready", WebSocketAwaitReady, WebSocketFramePing, WebSocketServerToClient, StreamServer, true, WebSocketAwaitReady},
		{"heartbeats are refused before init", WebSocketAwaitInit, WebSocketFramePong, WebSocketClientToServer, StreamServer, false, WebSocketAwaitInit},
		{"half-close is client-only", WebSocketOpen, WebSocketFrameHalfClose, WebSocketServerToClient, StreamBidirectional, false, WebSocketOpen},
		{"half-close ends the client direction", WebSocketOpen, WebSocketFrameHalfClose, WebSocketClientToServer, StreamBidirectional, true, WebSocketHalfClosed},
		{"a server stream refuses client messages", WebSocketOpen, WebSocketFrameMessage, WebSocketClientToServer, StreamServer, false, WebSocketOpen},
		{"a client stream refuses server messages", WebSocketOpen, WebSocketFrameMessage, WebSocketServerToClient, StreamClient, false, WebSocketOpen},
		{"the provider still delivers after half-close", WebSocketHalfClosed, WebSocketFrameMessage, WebSocketServerToClient, StreamBidirectional, true, WebSocketHalfClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame, diags := ParseAndValidateWebSocketFrameV1(minimalFrame(t, tc.frameType))
			if diag.HasErrors(diags) {
				t.Fatalf("minimal frame is invalid: %v", diags)
			}
			next, transitionDiags := NextWebSocketStateV1(tc.state, frame, tc.direction, tc.stream)
			if allowed := !diag.HasErrors(transitionDiags); allowed != tc.allowed {
				t.Fatalf("allowed = %v, want %v (%v)", allowed, tc.allowed, transitionDiags)
			}
			if next != tc.next {
				t.Fatalf("next state = %q, want %q", next, tc.next)
			}
		})
	}
}

// TestWebSocketResultPayloadIsRefusedOnAServerStream pins the one transition
// that depends on the frame's own content rather than on direction alone.
func TestWebSocketResultPayloadIsRefusedOnAServerStream(t *testing.T) {
	withPayload, diags := ParseAndValidateWebSocketFrameV1(
		[]byte(`{"v":1,"type":"result","payload":{"encoding":"json","value":{"done":true}}}`))
	if diag.HasErrors(diags) {
		t.Fatalf("result frame is invalid: %v", diags)
	}
	if _, refusal := NextWebSocketStateV1(WebSocketOpen, withPayload, WebSocketServerToClient, StreamServer); !diag.HasErrors(refusal) {
		t.Error("a server stream accepted a second delivery channel through result")
	}
	for _, stream := range []StreamMode{StreamClient, StreamBidirectional} {
		if _, refusal := NextWebSocketStateV1(WebSocketOpen, withPayload, WebSocketServerToClient, stream); diag.HasErrors(refusal) {
			t.Errorf("%s stream refused its declared final result payload: %v", stream, refusal)
		}
	}
}

// TestWebSocketConversationRefusesUndeclaredResume pins the resume agreement:
// a provider cannot report a resumed stream the client never asked for on a
// transport that never declared it.
func TestWebSocketConversationRefusesUndeclaredResume(t *testing.T) {
	conversation := NewWebSocketConversationV1(StreamServer, EncodingJSON, false)
	init, diags := ParseAndValidateWebSocketFrameV1([]byte(`{"v":1,"type":"init","operationId":"widgets.watch",` +
		`"clientId":"fixtures-consumer","deadlineUnixMs":"0","budgetMs":"0","credentials":[],"headers":[]}`))
	if diag.HasErrors(diags) {
		t.Fatalf("init frame is invalid: %v", diags)
	}
	if accepted := conversation.Accept(init, WebSocketClientToServer); diag.HasErrors(accepted) {
		t.Fatalf("init refused: %v", accepted)
	}
	ready, diags := ParseAndValidateWebSocketFrameV1([]byte(`{"v":1,"type":"ready","resumed":true,"resumeToken":"token"}`))
	if diag.HasErrors(diags) {
		t.Fatalf("ready frame is invalid: %v", diags)
	}
	if accepted := conversation.Accept(ready, WebSocketServerToClient); !diag.HasErrors(accepted) {
		t.Error("conversation accepted a resumed stream the init never requested")
	}
	if conversation.State() != WebSocketAwaitReady {
		t.Fatalf("refused ready moved the conversation to %q", conversation.State())
	}
}

func loadWebSocketWireSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("schemas/client-websocket-wire-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("wire schema is not valid JSON: %v", err)
	}
	return schema
}

// minimalFrame is the smallest valid frame of each published type. It exists so
// the totality and drift tests exercise the parser, not a hand-built value.
func minimalFrame(t *testing.T, frameType WebSocketFrameType) []byte {
	t.Helper()
	bodies := map[WebSocketFrameType]string{
		WebSocketFrameInit: `{"v":1,"type":"init","operationId":"widgets.watch","clientId":"fixtures-consumer",` +
			`"deadlineUnixMs":"0","budgetMs":"0","credentials":[],"headers":[]}`,
		WebSocketFrameReady:     `{"v":1,"type":"ready","resumed":false}`,
		WebSocketFrameMessage:   `{"v":1,"type":"message","sequence":"1","payload":{"encoding":"json","value":{}}}`,
		WebSocketFrameHalfClose: `{"v":1,"type":"half-close"}`,
		WebSocketFrameResult:    `{"v":1,"type":"result"}`,
		WebSocketFrameError:     `{"v":1,"type":"error","error":{"status":503,"code":"unavailable"}}`,
		WebSocketFrameCancel:    `{"v":1,"type":"cancel","code":"canceled"}`,
		WebSocketFramePing:      `{"v":1,"type":"ping","nonce":"heartbeat-1"}`,
		WebSocketFramePong:      `{"v":1,"type":"pong","nonce":"heartbeat-1"}`,
	}
	body, ok := bodies[frameType]
	if !ok {
		t.Fatalf("no minimal frame is declared for %q", frameType)
	}
	return []byte(body)
}

// schemaTypeConstants returns the frame-type constants a definition pins,
// including the ones an allOf refinement narrows.
func schemaTypeConstants(raw any) []string {
	definition, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	var values []string
	if properties, ok := definition["properties"].(map[string]any); ok {
		if typed, ok := properties["type"].(map[string]any); ok {
			if constant, ok := typed["const"].(string); ok {
				values = append(values, constant)
			}
		}
	}
	if refinements, ok := definition["allOf"].([]any); ok {
		for _, refinement := range refinements {
			values = append(values, schemaTypeConstants(refinement)...)
		}
	}
	return values
}

func schemaStringSet(raw any) []string {
	definition := raw.(map[string]any)
	values := make([]string, 0, 2)
	for _, item := range definition["enum"].([]any) {
		values = append(values, item.(string))
	}
	sort.Strings(values)
	return values
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
