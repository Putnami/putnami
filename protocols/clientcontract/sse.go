package clientcontract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// The negotiated SSE wire of protocol v1 (ADR 0013). A runtime asks for it
// only on an operation whose SSE transport declares a continuation, and a
// provider grants it only on a route that declares one. Everything else keeps
// the legacy framing, byte for byte.
const (
	// SSEWireHeader is the request and response header that negotiates the
	// wire. The runtime sends it on the request; the provider echoes it on an
	// admitted 2xx response, before the first body byte, and nowhere else.
	SSEWireHeader = "X-Putnami-Stream-Wire"
	// SSEWireV1 is the only negotiated SSE wire of protocol v1.
	SSEWireV1 = "putnami.sse.v1"
	// SSEEventMessage is the default event type: an event block without an
	// event field carries an application message, and so does one that names
	// this type.
	SSEEventMessage = "message"
	// SSEEventError is the typed terminal error event. Both wires carry it,
	// with the first-party error envelope plus the status.
	SSEEventError = "error"
	// SSEEventComplete is the successful terminal event of the negotiated
	// wire. It is a control event, never an application message.
	SSEEventComplete = "complete"
	// SSECompleteData is the fixed control payload of the complete event.
	SSECompleteData = "{}"
	// SSECompleteFrame is the exact byte sequence a provider writes to end a
	// negotiated stream successfully.
	SSECompleteFrame = "event: " + SSEEventComplete + "\ndata: " + SSECompleteData + "\n\n"
)

// NegotiatesSSEWire reports whether the field lines of SSEWireHeader name
// exactly the negotiated wire. The lines are joined the way RFC 9110 combines
// repeated field lines (", ") and trimmed of optional whitespace; the result
// must equal SSEWireV1, byte for byte. A provider reads the request header
// with it and a runtime reads the response header with it, so a list, a
// repeated line, another version or another letter case never negotiates.
func NegotiatesSSEWire(lines []string) bool {
	return strings.Trim(strings.Join(lines, ", "), " \t") == SSEWireV1
}

// SSEEventKind is what one dispatched event means to a reader.
type SSEEventKind string

// The kinds of dispatched events.
const (
	// SSEEventKindMessage is an application message of the declared output type.
	SSEEventKindMessage SSEEventKind = "message"
	// SSEEventKindError is the typed terminal error.
	SSEEventKindError SSEEventKind = "error"
	// SSEEventKindComplete is the negotiated successful terminal.
	SSEEventKindComplete SSEEventKind = "complete"
)

// ClassifySSEEvent names one dispatched event from its event field (empty
// when the block has none) and its data lines joined with "\n".
//
// Without negotiation it keeps the reading both runtimes have always had:
// error is the terminal error and every other type is an application
// message. The negotiated wire has a closed vocabulary: the default type is a
// message, error is the terminal error, complete carrying exactly
// SSECompleteData is the successful terminal, and any other event is a
// contract error.
func ClassifySSEEvent(eventType, data string, negotiated bool) (SSEEventKind, error) {
	if eventType == SSEEventError {
		return SSEEventKindError, nil
	}
	if !negotiated {
		return SSEEventKindMessage, nil
	}
	switch eventType {
	case "", SSEEventMessage:
		return SSEEventKindMessage, nil
	case SSEEventComplete:
		if data != SSECompleteData {
			return "", fmt.Errorf("the %s event carries %q; %s fixes its payload to %q",
				SSEEventComplete, data, SSEWireV1, SSECompleteData)
		}
		return SSEEventKindComplete, nil
	default:
		return "", fmt.Errorf("event type %q is not part of %s", eventType, SSEWireV1)
	}
}

// SSECursorValue returns the position one output message carries in the
// declared field: a non-empty JSON string, returned exactly as decoded. A
// message without it, or with any other value, is a contract error — a stream
// that cannot say where it is cannot be continued.
func SSECursorValue(message []byte, outputField string) (string, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(message, &members); err != nil || members == nil {
		return "", fmt.Errorf("the output message is not a JSON object")
	}
	raw, ok := members[outputField]
	if !ok {
		return "", fmt.Errorf("the output message carries no %q position", outputField)
	}
	var value string
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return "", fmt.Errorf("the output field %q is not a JSON string", outputField)
	}
	if value == "" {
		return "", fmt.Errorf("the output field %q is empty", outputField)
	}
	return value, nil
}

// SSEReopenQuery returns the query a reopened connection sends, as a new
// value; original is never modified.
//
// Best-effort continuation reopens with the original query. Cursor mode
// replaces every value of the declared parameter with delivered, the position
// of the last message the consumer received. Until the consumer has received
// one (delivered is empty) it keeps the original query, including an initial
// position the caller supplied. No mode ever synthesizes a position.
func SSEReopenQuery(continuation SSEContinuation, original url.Values, delivered string) url.Values {
	query := make(url.Values, len(original)+1)
	for name, values := range original {
		query[name] = append([]string(nil), values...)
	}
	if continuation.Mode != SSEContinuationCursor || continuation.Cursor == nil || delivered == "" {
		return query
	}
	query[continuation.Cursor.QueryParameter] = []string{delivered}
	return query
}
