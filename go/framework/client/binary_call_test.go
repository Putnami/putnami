package client

import (
	"bytes"
	"io"
	"strings"
	"testing"

	perrors "go.putnami.dev/errors"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/protocol/features/spectest"
)

// slowReader hands out one octet per Read and counts what it gave, so a bound
// that drains its source is visible rather than assumed.
type slowReader struct {
	remaining int
	given     int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 0x5a
	r.remaining--
	r.given++
	return 1, nil
}

// TestReadBoundedBody_StopsOneOctetPastTheBound is the request-side half of the
// declared bound: an oversized source is refused, and it is refused without
// being read to the end.
func TestReadBoundedBody_StopsOneOctetPastTheBound(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binary-payloads", "an-oversized-raw-octet-request-is-refused-before-the-source-is-drained")
	source := &slowReader{remaining: 10_000}
	if _, err := ReadBoundedBody(source, 8); err == nil {
		t.Fatal("an oversized source was accepted")
	}
	if source.given != 9 {
		t.Fatalf("the bound consumed %d octets, want 9 (the bound plus the octet that proves the overflow)", source.given)
	}
}

func TestReadBoundedBody_CarriesTheDeclaredPayload(t *testing.T) {
	payload := []byte{0x00, 0xff, 0x80}
	body, err := ReadBoundedBody(bytes.NewReader(payload), 8)
	if err != nil {
		t.Fatalf("ReadBoundedBody: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("read %x, want %x", body, payload)
	}
	empty, err := ReadBoundedBody(bytes.NewReader(nil), 8)
	if err != nil || len(empty) != 0 {
		t.Fatalf("an empty payload = %x/%v, want zero octets and no error", empty, err)
	}
	absent, err := ReadBoundedBody(nil, 8)
	if err != nil || absent != nil {
		t.Fatalf("a nil reader = %x/%v, want no payload and no error", absent, err)
	}
	if _, err := ReadBoundedBody(bytes.NewReader(payload), 0); err == nil {
		t.Fatal("an unbounded binary request was accepted")
	}
}

func binaryOperation(protocol clientcontract.TransportProtocol) Operation {
	return Operation{
		ID: "getBlob",
		Contract: clientcontract.OperationV1{
			Stream:      clientcontract.StreamUnary,
			Transports:  []clientcontract.Transport{{Protocol: protocol, Path: "/blob", Encoding: clientcontract.EncodingJSON}},
			Security:    clientcontract.Security{Alternatives: []clientcontract.SecurityAlternative{{AllOf: []clientcontract.SecurityRequirement{}}}},
			Idempotency: clientcontract.Idempotency{Kind: clientcontract.IdempotencySafe},
		},
		Successes: []OperationSuccess{{Status: 200, Content: []OperationContent{{
			MediaType: "application/octet-stream",
			Schema:    &clientcontract.Schema{Type: "string", Format: "binary"},
			MaxBytes:  16,
		}}}},
	}
}

// TestCallOperationBinary_RefusesATransportThatCannotCarryOctets names the
// refusal instead of degrading the payload into a Connect envelope.
func TestCallOperationBinary_RefusesATransportThatCannotCarryOctets(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binary-payloads", "raw-octets-are-refused-on-a-transport-that-cannot-carry-them")
	_, err := CallOperationBinary(t.Context(), nil,
		&OperationCall{Request: &Request{Method: "GET", Path: "/blob"}},
		binaryOperation(clientcontract.TransportConnect))
	if err == nil || !strings.Contains(err.Error(), "only rest-json carries octets unchanged") {
		t.Fatalf("connect + octets = %v, want an explicit refusal", err)
	}
}

func TestCallOperationBinary_RefusesACallWithNoRequest(t *testing.T) {
	if _, err := CallOperationBinary(t.Context(), nil, nil, binaryOperation(clientcontract.TransportRESTJSON)); err == nil {
		t.Fatal("a call with no request was accepted")
	}
}

// TestValidateGeneratedRequest_AppliesTheDeclaredOctetFacts proves the two
// request-side rules, including the one a JSON body would get wrong: zero
// octets are a payload, not a missing one.
func TestValidateGeneratedRequest_AppliesTheDeclaredOctetFacts(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binary-payloads", "the-declared-octet-media-type-and-bound-are-applied-to-the-request")
	declared := &OperationRequest{Required: true, Content: []OperationContent{{
		MediaType: "application/octet-stream",
		Schema:    &clientcontract.Schema{Type: "string", Format: "binary"},
		MaxBytes:  8,
	}}}
	octets := func(body []byte, contentType string) *Request {
		request := &Request{Method: "PUT", Path: "/blob", Body: body}
		request.SetHeader("Content-Type", contentType)
		return request
	}
	if err := validateGeneratedRequest(octets(nil, "application/octet-stream"), declared, nil); err != nil {
		t.Fatalf("an empty declared octet body was refused: %v", err)
	}
	if err := validateGeneratedRequest(octets([]byte{0xff}, "application/json"), declared, nil); err == nil {
		t.Fatal("an undeclared request media type was accepted")
	}
	err := validateGeneratedRequest(octets(bytes.Repeat([]byte{0x01}, 9), "application/octet-stream"), declared, nil)
	if err == nil || !perrors.Is(err, CodeClientRequest) {
		t.Fatalf("an oversized request body = %v, want a client.request refusal", err)
	}
}

// TestDecodeResponse_RefusesRawOctets keeps the JSON path from guessing an
// encoding the provider never declared.
func TestDecodeResponse_RefusesRawOctets(t *testing.T) {
	spectest.Proves(t, "go/typed-service-clients", "binary-payloads", "the-json-decoder-never-guesses-an-encoding-for-raw-octets")
	operation := binaryOperation(clientcontract.TransportRESTJSON)
	response := &Response{StatusCode: 200, Body: []byte{0xff}}
	response.Headers = map[string][]string{"Content-Type": {"application/octet-stream"}}
	if _, err := DecodeResponse[string](nil, response, operation); err == nil {
		t.Fatal("the JSON decoder accepted a raw octet payload")
	}
}
