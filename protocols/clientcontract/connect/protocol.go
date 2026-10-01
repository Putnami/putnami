// Package connect implements the Connect protocol wire format and the
// descriptor-driven protobuf codec shared by go.putnami.dev/client and
// go.putnami.dev/grpc.
package connect

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// The Connect protocol, version 1 (https://connectrpc.com/docs/protocol/).
// Every constant here is the literal the specification names; nothing is
// derived, so a specification change shows up as a failing vector rather than
// as a silent behavior difference.
const (
	ProtocolVersion       = "1"
	ProtocolVersionHeader = "Connect-Protocol-Version"
	TimeoutHeader         = "Connect-Timeout-Ms"
	ContentEncoding       = "Connect-Content-Encoding"
	AcceptEncoding        = "Connect-Accept-Encoding"

	UnaryJSONContentType  = "application/json"
	UnaryProtoContentType = "application/proto"
	StreamJSONContentType = "application/connect+json"
	StreamProtoType       = "application/connect+proto"

	// connectTimeoutMaxDigits is the specification's bound on the header: at
	// most ten ASCII digits.
	connectTimeoutMaxDigits = 10

	// connectEnvelopePrefix is the fixed 5-byte prefix of a streamed message:
	// one flag byte then a 4-byte big-endian length.
	connectEnvelopePrefix = 5
	// FlagCompressed marks an envelope whose payload is compressed.
	FlagCompressed byte = 0b0000_0001
	// FlagEndStream marks the final EndStreamResponse envelope.
	FlagEndStream byte = 0b0000_0010
	// connectFlagReserved covers the six bits the specification reserves. A peer
	// that sets one is speaking a protocol this runtime does not know.
	connectFlagReserved byte = 0b1111_1100

	EncodingGzip     = "gzip"
	EncodingIdentity = "identity"
)

// Status is one canonical gRPC status: its Connect name, its numeric
// value, and the HTTP status the Connect specification maps it to.
type Status struct {
	Name   string
	Number int
	Status int
}

// connectStatuses is the specification's table, in canonical numeric order.
var connectStatuses = []Status{
	{"canceled", 1, 499},
	{"unknown", 2, 500},
	{"invalid_argument", 3, 400},
	{"deadline_exceeded", 4, 504},
	{"not_found", 5, 404},
	{"already_exists", 6, 409},
	{"permission_denied", 7, 403},
	{"resource_exhausted", 8, 429},
	{"failed_precondition", 9, 400},
	{"aborted", 10, 409},
	{"out_of_range", 11, 400},
	{"unimplemented", 12, 501},
	{"internal", 13, 500},
	{"unavailable", 14, 503},
	{"data_loss", 15, 500},
	{"unauthenticated", 16, 401},
}

// StatusByName resolves a wire code name.
func StatusByName(name string) (Status, bool) {
	for _, status := range connectStatuses {
		if status.Name == name {
			return status, true
		}
	}
	return Status{}, false
}

// ErrorEnvelope is the Connect error document. It is JSON on every
// codec: the specification defines no protobuf form for it.
type ErrorEnvelope struct {
	Code    string        `json:"code"`
	Message string        `json:"message,omitempty"`
	Details []ErrorDetail `json:"details,omitempty"`
}

// ErrorDetail is one protobuf message attached to an error. `value` is
// unpadded standard-alphabet base64 of the message's binary form.
type ErrorDetail struct {
	Type  string          `json:"type"`
	Value string          `json:"value"`
	Debug json.RawMessage `json:"debug,omitempty"`
}

// EndStreamResponse is the final envelope of a Connect stream.
type EndStreamResponse struct {
	Error    *ErrorEnvelope      `json:"error,omitempty"`
	Metadata map[string][]string `json:"metadata,omitempty"`
}

// DetailBase64 is the specification's detail encoding: standard
// alphabet, no padding.
var DetailBase64 = base64.StdEncoding.WithPadding(base64.NoPadding)

// FrameworkErrorType is the detail a first-party provider attaches to a
// Connect error. The Connect specification fixes sixteen codes and says there
// are no user-defined ones, so the finer first-party facts — the stable code, the
// exact HTTP status, and the declared `details` body — travel as a typed detail,
// which is the protocol's own mechanism for exactly that.
//
// The message and its field numbers are the ones the TypeScript provider
// publishes (ADR 0003 of @putnami/application), so one Putnami consumer reads one
// encoding whichever provider answered:
//
//	message FrameworkError {
//	  string code = 1;
//	  int32 http_status = 2;
//	  string details_json = 3;
//	}
const FrameworkErrorType = "putnami.client.v1.FrameworkError"

// FrameworkError is the first-party envelope carried inside a Connect
// error. DetailsJSON holds the declared `details` member verbatim: the declared
// schema differs per operation, so the bytes a client validates are kept exactly
// rather than reshaped through a generic value message that would round a 64-bit
// number through a float.
type FrameworkError struct {
	Code        string
	Status      int
	DetailsJSON string
}

// EncodeFrameworkError writes the binary form of a FrameworkError detail.
func EncodeFrameworkError(envelope FrameworkError) []byte {
	var out []byte
	if envelope.Code != "" {
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendString(out, envelope.Code)
	}
	if envelope.Status != 0 {
		out = protowire.AppendTag(out, 2, protowire.VarintType)
		out = protowire.AppendVarint(out, uint64(uint32(int32(envelope.Status)))) //nolint:gosec // an HTTP status is a small positive int32
	}
	if envelope.DetailsJSON != "" {
		out = protowire.AppendTag(out, 3, protowire.BytesType)
		out = protowire.AppendString(out, envelope.DetailsJSON)
	}
	return out
}

// DecodeFrameworkError reads the binary form of a FrameworkError detail.
func DecodeFrameworkError(data []byte) (FrameworkError, error) {
	envelope := FrameworkError{}
	for len(data) > 0 {
		number, wire, headerLen := protowire.ConsumeTag(data)
		if headerLen < 0 {
			return FrameworkError{}, fmt.Errorf("connect: malformed %s tag", FrameworkErrorType)
		}
		data = data[headerLen:]
		switch {
		case number == 1 && wire == protowire.BytesType:
			value, size := protowire.ConsumeString(data)
			if size < 0 {
				return FrameworkError{}, fmt.Errorf("connect: malformed %s code", FrameworkErrorType)
			}
			envelope.Code, data = value, data[size:]
		case number == 2 && wire == protowire.VarintType:
			value, size := protowire.ConsumeVarint(data)
			if size < 0 {
				return FrameworkError{}, fmt.Errorf("connect: malformed %s status", FrameworkErrorType)
			}
			envelope.Status, data = int(int32(uint32(value))), data[size:] //nolint:gosec // http_status is a 32-bit field
		case number == 3 && wire == protowire.BytesType:
			value, size := protowire.ConsumeString(data)
			if size < 0 {
				return FrameworkError{}, fmt.Errorf("connect: malformed %s details", FrameworkErrorType)
			}
			envelope.DetailsJSON, data = value, data[size:]
		default:
			skip := protowire.ConsumeFieldValue(number, wire, data)
			if skip < 0 {
				return FrameworkError{}, fmt.Errorf("connect: malformed %s member", FrameworkErrorType)
			}
			data = data[skip:]
		}
	}
	return envelope, nil
}

// AppendEnvelope writes one 5-byte-prefixed streamed message.
func AppendEnvelope(out []byte, flags byte, payload []byte) []byte {
	header := [connectEnvelopePrefix]byte{}
	header[0] = flags
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload))) //nolint:gosec // the caller bounds payload length
	out = append(out, header[:]...)
	return append(out, payload...)
}

// ReadEnvelope reads one enveloped message, refusing a declared length
// past maxBytes before allocating for it.
func ReadEnvelope(reader io.Reader, maxBytes int64) (flags byte, payload []byte, err error) {
	header := make([]byte, connectEnvelopePrefix)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, nil, err
	}
	if header[0]&connectFlagReserved != 0 {
		return 0, nil, fmt.Errorf("connect: envelope sets reserved flag bits 0x%02x", header[0]&connectFlagReserved)
	}
	length := int64(binary.BigEndian.Uint32(header[1:]))
	if maxBytes > 0 && length > maxBytes {
		return 0, nil, fmt.Errorf("connect: envelope declares %d bytes, past the %d-byte frame budget", length, maxBytes)
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

// CompressGzip compresses a payload. It is only worth the CPU when the
// result is smaller, so the caller decides with the returned flag.
func CompressGzip(payload []byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(payload); err != nil {
		return nil, fmt.Errorf("connect: gzip encode failed")
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("connect: gzip encode failed")
	}
	return buffer.Bytes(), nil
}

// DecompressGzip bounds decompression by maxBytes. A gzip stream can
// expand by three orders of magnitude, so an unbounded read here is a memory
// exhaustion primitive handed to whoever can reach the endpoint: the reader
// stops one byte past the budget and refuses.
func DecompressGzip(payload []byte, maxBytes int64) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("connect: the payload is not a gzip stream")
	}
	defer reader.Close() //nolint:errcheck // read-only reader over a memory buffer
	if maxBytes <= 0 {
		maxBytes = defaultConnectMaxBytes
	}
	decoded, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("connect: the gzip payload is malformed")
	}
	if int64(len(decoded)) > maxBytes {
		return nil, fmt.Errorf("connect: the decompressed payload exceeds the %d-byte budget", maxBytes)
	}
	return decoded, nil
}

// defaultConnectMaxBytes bounds a single decoded Connect payload when the
// contract declares no frame budget.
const defaultConnectMaxBytes int64 = 32 << 20

// ParseTimeout reads Connect-Timeout-Ms. The grammar is a *positive*
// integer of at most ten digits, with no surrounding space: an absent header
// means "no timeout from the caller", and anything else is refused rather than
// ignored, because ignoring it would silently run a call the caller bounded.
func ParseTimeout(raw string) (milliseconds int64, present bool, err error) {
	if raw == "" {
		return 0, false, nil
	}
	if len(raw) > connectTimeoutMaxDigits {
		return 0, false, fmt.Errorf("connect: %s carries %d characters; the protocol allows at most %d digits", TimeoutHeader, len(raw), connectTimeoutMaxDigits)
	}
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0, false, fmt.Errorf("connect: %s must be ASCII digits with no surrounding space", TimeoutHeader)
		}
	}
	value, parseErr := strconv.ParseInt(raw, 10, 64)
	if parseErr != nil || value <= 0 {
		return 0, false, fmt.Errorf("connect: %s is not a positive number of milliseconds", TimeoutHeader)
	}
	return value, true, nil
}

// connectInferredCodes is the specification's other status table: the code a
// client infers when a failed call carries no Connect error document at all —
// an intermediary's HTML page, a proxy's plain text. It is deliberately not the
// inverse of the code-to-status table, and a status it does not list infers
// `unknown`.
var connectInferredCodes = map[int]string{
	400: "internal",
	401: "unauthenticated",
	403: "permission_denied",
	404: "unimplemented",
	429: "unavailable",
	502: "unavailable",
	503: "unavailable",
	504: "unavailable",
}

// InferredCode reports the code a client infers from a bare status.
func InferredCode(status int) string {
	if code, listed := connectInferredCodes[status]; listed {
		return code
	}
	return "unknown"
}

// MediaType strips parameters from a Content-Type so "application/json;
// charset=utf-8" is recognized as the codec it names.
func MediaType(header string) string {
	return strings.ToLower(strings.TrimSpace(strings.Split(header, ";")[0]))
}
