package clientcontract

import (
	"encoding/json"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

type sseWireFixture struct {
	Description    string `json:"description"`
	Header         string `json:"header"`
	Token          string `json:"token"`
	ContentType    string `json:"contentType"`
	CompleteFrame  string `json:"completeFrame"`
	HeartbeatFrame string `json:"heartbeatFrame"`
	Negotiation    []struct {
		Lines      []string `json:"lines"`
		Negotiates bool     `json:"negotiates"`
	} `json:"negotiation"`
	Provider []struct {
		Name                      string   `json:"name"`
		RouteDeclaresContinuation bool     `json:"routeDeclaresContinuation"`
		Request                   []string `json:"request"`
		Admission                 string   `json:"admission"`
		Handler                   string   `json:"handler"`
		Expect                    struct {
			Acknowledges bool   `json:"acknowledges"`
			Terminal     string `json:"terminal"`
		} `json:"expect"`
	} `json:"provider"`
}

type sseSceneExpectation struct {
	Outcome         string            `json:"outcome"`
	Messages        []json.RawMessage `json:"messages"`
	DeliveredCursor *string           `json:"deliveredCursor"`
	Error           *struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	} `json:"error,omitempty"`
	Reopen *struct {
		Query url.Values `json:"query"`
	} `json:"reopen,omitempty"`
}

type sseScene struct {
	Name           string              `json:"name"`
	Continuation   string              `json:"continuation"`
	Requested      bool                `json:"requested"`
	Acknowledgment []string            `json:"acknowledgment"`
	Query          url.Values          `json:"query"`
	MaxFrameBytes  int                 `json:"maxFrameBytes"`
	Body           []string            `json:"body"`
	End            string              `json:"end"`
	Expect         sseSceneExpectation `json:"expect"`
}

type sseScenes struct {
	Description   string                     `json:"description"`
	MaxFrameBytes int                        `json:"maxFrameBytes"`
	Continuations map[string]SSEContinuation `json:"continuations"`
	Scenes        []sseScene                 `json:"scenes"`
}

func readSSEFixture(t *testing.T, name string, target any) {
	t.Helper()
	data, err := os.ReadFile("fixtures/sse/" + name)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("fixtures/sse/%s: %v", name, err)
	}
}

// TestTheNegotiatedWireTerminalAndMixedVersionOutcomesArePinned replays the
// shared SSE corpus against the exported wire vocabulary. Each provider and
// runtime that implements ADR 0013 replays the same files against real
// connections; this reference reader keeps the files consistent with the
// vocabulary they share.
func TestTheNegotiatedWireTerminalAndMixedVersionOutcomesArePinned(t *testing.T) {
	spectest.Proves(t, sseFeature, sseRequirement, "the-negotiated-wire-terminal-and-mixed-version-outcomes-are-pinned-by-shared-scenes")

	var wire sseWireFixture
	readSSEFixture(t, "wire.json", &wire)
	if wire.Header != SSEWireHeader || wire.Token != SSEWireV1 || wire.CompleteFrame != SSECompleteFrame {
		t.Fatalf("wire.json = %q %q %q, want %q %q %q", wire.Header, wire.Token, wire.CompleteFrame,
			SSEWireHeader, SSEWireV1, SSECompleteFrame)
	}
	if wire.CompleteFrame != "event: complete\ndata: {}\n\n" {
		t.Fatalf("the complete frame drifted from its settled bytes: %q", wire.CompleteFrame)
	}
	if len(wire.Negotiation) == 0 || len(wire.Provider) == 0 {
		t.Fatal("wire.json pins no negotiation or provider rows")
	}
	for _, row := range wire.Negotiation {
		if got := NegotiatesSSEWire(row.Lines); got != row.Negotiates {
			t.Errorf("NegotiatesSSEWire(%q) = %v, want %v", row.Lines, got, row.Negotiates)
		}
	}
	for _, row := range wire.Provider {
		admitted := row.Admission == "admitted"
		acknowledges := admitted && row.RouteDeclaresContinuation && NegotiatesSSEWire(row.Request)
		terminal := "none"
		switch {
		case admitted && row.Handler == "fails":
			terminal = "error"
		case acknowledges && row.Handler == "returns":
			terminal = "complete"
		}
		if acknowledges != row.Expect.Acknowledges || terminal != row.Expect.Terminal {
			t.Errorf("%s: acknowledges=%v terminal=%s, want %v %s", row.Name, acknowledges, terminal,
				row.Expect.Acknowledges, row.Expect.Terminal)
		}
	}

	var corpus sseScenes
	readSSEFixture(t, "scenes.json", &corpus)
	outcomes := map[string]bool{}
	for _, scene := range corpus.Scenes {
		t.Run(scene.Name, func(t *testing.T) {
			continuation, ok := corpus.Continuations[scene.Continuation]
			if !ok {
				t.Fatalf("scene names an undeclared continuation %q", scene.Continuation)
			}
			if diags := validateSSETransport("sse", &SSETransport{Continuation: &continuation}); diags != nil {
				t.Fatalf("scene continuation is not a valid declaration: %v", diags)
			}
			limit := scene.MaxFrameBytes
			if limit == 0 {
				limit = corpus.MaxFrameBytes
			}
			got := replaySSEScene(scene, continuation, limit)
			assertSSESceneResult(t, got, scene.Expect)
			outcomes[got.Outcome] = true
		})
	}
	for _, outcome := range []string{"complete", "error", "interrupted", "contract-error", "transport-error"} {
		if !outcomes[outcome] {
			t.Errorf("no scene reaches the %s outcome; the corpus would not pin it", outcome)
		}
	}
}

// replaySSEScene reads one connection the way ADR 0013 specifies: the
// negotiated wire has a closed vocabulary and ends only at an explicit
// terminal; the legacy framing ends at a clean end of body.
func replaySSEScene(scene sseScene, continuation SSEContinuation, maxFrameBytes int) sseSceneExpectation {
	result := sseSceneExpectation{Messages: []json.RawMessage{}}
	if scene.Requested && !NegotiatesSSEWire(scene.Acknowledgment) {
		// Admission fails before any message: an unacknowledged exchange is
		// never read as the legacy framing.
		result.Outcome = "contract-error"
		return result
	}
	negotiated := scene.Requested
	tracksCursor := negotiated && continuation.Mode == SSEContinuationCursor
	rest := strings.Join(scene.Body, "")
	eventType, data, hasData, blockBytes := "", []string(nil), false, 0
	reset := func() { eventType, data, hasData, blockBytes = "", nil, false, 0 }
	// dispatch returns true when the event ended the connection.
	dispatch := func() bool {
		defer reset()
		if !hasData {
			if negotiated && eventType != "" {
				result.Outcome = "contract-error"
				return true
			}
			return false
		}
		payload := strings.Join(data, "\n")
		kind, err := ClassifySSEEvent(eventType, payload, negotiated)
		switch {
		case err != nil:
			result.Outcome = "contract-error"
		case kind == SSEEventKindComplete:
			result.Outcome = "complete"
		case kind == SSEEventKindError:
			var envelope struct {
				Status int    `json:"status"`
				Code   string `json:"code"`
			}
			if json.Unmarshal([]byte(payload), &envelope) != nil {
				result.Outcome = "contract-error"
				return true
			}
			result.Outcome = "error"
			result.Error = &struct {
				Status int    `json:"status"`
				Code   string `json:"code"`
			}{envelope.Status, envelope.Code}
		default:
			if !json.Valid([]byte(payload)) {
				result.Outcome = "contract-error"
				return true
			}
			if tracksCursor {
				cursor, err := SSECursorValue([]byte(payload), continuation.Cursor.OutputField)
				if err != nil {
					result.Outcome = "contract-error"
					return true
				}
				result.DeliveredCursor = &cursor
			}
			result.Messages = append(result.Messages, json.RawMessage(payload))
			return false
		}
		return true
	}
	for {
		index := strings.IndexByte(rest, '\n')
		var line string
		if index < 0 {
			if rest == "" {
				break
			}
			line, rest = rest, ""
		} else {
			line, rest = rest[:index], rest[index+1:]
		}
		if index >= 0 && strings.TrimSuffix(line, "\r") == "" {
			if dispatch() {
				return result
			}
			continue
		}
		blockBytes += len(line) + 1
		if blockBytes > maxFrameBytes {
			result.Outcome = "contract-error"
			return result
		}
		line = strings.TrimSuffix(line, "\r")
		name, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "":
			// A comment: liveness only, it never advances a position.
		case "event":
			eventType = strings.TrimSpace(value)
		case "data":
			data, hasData = append(data, value), true
		}
	}
	// The transport ended with no terminal.
	switch {
	case negotiated:
		// A pending, incomplete event is discarded: it was never delivered.
		result.Outcome = "interrupted"
		delivered := ""
		if result.DeliveredCursor != nil {
			delivered = *result.DeliveredCursor
		}
		result.Reopen = &struct {
			Query url.Values `json:"query"`
		}{SSEReopenQuery(continuation, scene.Query, delivered)}
	case scene.End == "reset":
		result.Outcome = "transport-error"
	case hasData:
		result.Outcome = "contract-error"
	default:
		result.Outcome = "complete"
	}
	return result
}

func assertSSESceneResult(t *testing.T, got, want sseSceneExpectation) {
	t.Helper()
	if got.Outcome != want.Outcome {
		t.Fatalf("outcome = %s, want %s", got.Outcome, want.Outcome)
	}
	if !sameJSONValues(t, got.Messages, want.Messages) {
		t.Fatalf("messages = %s, want %s", got.Messages, want.Messages)
	}
	if !reflect.DeepEqual(got.DeliveredCursor, want.DeliveredCursor) {
		t.Fatalf("delivered cursor = %v, want %v", printable(got.DeliveredCursor), printable(want.DeliveredCursor))
	}
	if !reflect.DeepEqual(got.Error, want.Error) {
		t.Fatalf("error = %+v, want %+v", got.Error, want.Error)
	}
	if (got.Reopen == nil) != (want.Reopen == nil) {
		t.Fatalf("reopen = %+v, want %+v", got.Reopen, want.Reopen)
	}
	if want.Reopen != nil && !reflect.DeepEqual(got.Reopen.Query, normalizedQuery(want.Reopen.Query)) {
		t.Fatalf("reopen query = %v, want %v", got.Reopen.Query, want.Reopen.Query)
	}
}

func sameJSONValues(t *testing.T, got, want []json.RawMessage) bool {
	t.Helper()
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

func normalizedQuery(query url.Values) url.Values {
	if query == nil {
		return url.Values{}
	}
	return query
}

func printable(value *string) string {
	if value == nil {
		return "null"
	}
	return *value
}

func TestSSEReopenQueryNeverModifiesTheOriginalOrSynthesizesAPosition(t *testing.T) {
	spectest.Proves(t, sseFeature, sseRequirement, "the-negotiated-wire-terminal-and-mixed-version-outcomes-are-pinned-by-shared-scenes")
	cursor := *cursorContinuation().Continuation
	original := url.Values{"selector": {"svc=api"}, "cursor": {"c0", "c00"}}
	reopened := SSEReopenQuery(cursor, original, "c7")
	if !reflect.DeepEqual(reopened, url.Values{"selector": {"svc=api"}, "cursor": {"c7"}}) {
		t.Fatalf("reopened = %v", reopened)
	}
	if !reflect.DeepEqual(original, url.Values{"selector": {"svc=api"}, "cursor": {"c0", "c00"}}) {
		t.Fatalf("the original query was modified: %v", original)
	}
	reopened["selector"][0] = "changed"
	if original["selector"][0] != "svc=api" {
		t.Fatal("the reopened query shares storage with the original")
	}
	if got := SSEReopenQuery(cursor, nil, ""); len(got) != 0 {
		t.Fatalf("a reopen before any delivery synthesized %v", got)
	}
	if got := SSEReopenQuery(*bestEffortContinuation().Continuation, url.Values{"selector": {"a"}}, "c7"); !reflect.DeepEqual(got, url.Values{"selector": {"a"}}) {
		t.Fatalf("best-effort carried a position: %v", got)
	}
	for _, message := range []string{`[]`, `null`, `{"cursor":null}`, `{"cursor":["c1"]}`, `{"line":"a"}`, `not json`} {
		if _, err := SSECursorValue([]byte(message), "cursor"); err == nil {
			t.Errorf("SSECursorValue accepted %s", message)
		}
	}
	if value, err := SSECursorValue([]byte(`{"cursor":"opaqueé/+=","line":"a"}`), "cursor"); err != nil || value != "opaqueé/+=" {
		t.Fatalf("SSECursorValue = %q, %v", value, err)
	}
}
