package connect

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// This file is the protocol oracle. Every expected value in it comes from a
// published specification — the Connect protocol (https://connectrpc.com/docs/protocol/,
// version 1) and the protobuf encoding rules — and not from either of the two
// private implementations this repository ships. The same file is compiled in
// go/framework/grpc and in go/framework/client, so a disagreement between the
// provider codec and the client codec cannot hide: one of them fails against
// the same literals.
//
// The protobuf vectors below were derived by hand from the encoding rules
// (tag = field_number << 3 | wire_type; varint little-endian base 128; zigzag
// for sint; little-endian fixed widths; length-delimited for strings, bytes,
// messages and map entries) and are written as literal hex.

// oracleVectorMessage is the message the vector describes: one field per
// proto3 wire shape.
func oracleVectorDescriptor() *clientcontract.ProtobufDescriptor {
	return &clientcontract.ProtobufDescriptor{
		Syntax:  "proto3",
		Package: "oracle.v1",
		Services: []clientcontract.ProtobufService{{Name: "OracleService", Methods: []clientcontract.ProtobufMethod{
			{Name: "Echo", Input: "Vec", Output: "Vec"},
			{Name: "Watch", Input: "Vec", Output: "Vec", ServerStreaming: true},
		}}},
		Messages: []clientcontract.ProtobufMessage{
			{Name: "Vec", Fields: []clientcontract.ProtobufField{
				{Name: "sample", JSONName: "sample", Number: 1, TypeKind: "scalar", Type: "int32"},
				{Name: "label", JSONName: "label", Number: 2, TypeKind: "scalar", Type: "string"},
				{Name: "flag", JSONName: "flag", Number: 3, TypeKind: "scalar", Type: "bool"},
				{Name: "delta", JSONName: "delta", Number: 4, TypeKind: "scalar", Type: "sint32"},
				{Name: "sequence", JSONName: "sequence", Number: 5, TypeKind: "scalar", Type: "uint64"},
				{Name: "ratio", JSONName: "ratio", Number: 6, TypeKind: "scalar", Type: "double"},
				{Name: "payload", JSONName: "payload", Number: 7, TypeKind: "scalar", Type: "bytes"},
				{Name: "mask", JSONName: "mask", Number: 8, TypeKind: "scalar", Type: "fixed32"},
				{Name: "counts", JSONName: "counts", Number: 9, TypeKind: "scalar", Type: "int32", Repeated: true},
				{Name: "labels", JSONName: "labels", Number: 10, TypeKind: "map", Type: "map",
					Map: &clientcontract.ProtobufMap{KeyType: "string", ValueKind: "scalar", ValueType: "string"}},
				{Name: "nested", JSONName: "nested", Number: 11, TypeKind: "message", Type: "Nested"},
				{Name: "share", JSONName: "share", Number: 12, TypeKind: "scalar", Type: "float"},
				{Name: "offset", JSONName: "offset", Number: 13, TypeKind: "scalar", Type: "sfixed64"},
			}},
			{Name: "Nested", Fields: []clientcontract.ProtobufField{
				{Name: "value", JSONName: "value", Number: 1, TypeKind: "scalar", Type: "int32"},
			}},
		},
		Enums: []clientcontract.ProtobufEnum{},
	}
}

// oracleVectorHex is the protobuf encoding of oracleVectorJSON, derived from the
// published encoding rules field by field:
//
//	08 9601                     field 1 varint 150
//	12 07 "testing"             field 2 length-delimited
//	18 01                       field 3 varint true
//	20 01                       field 4 varint zigzag(-1) = 1
//	28 ff*9 01                  field 5 varint 2^64-1
//	31 <8 LE bytes of 1.5>      field 6 64-bit
//	3a 03 000 1ff               field 7 length-delimited bytes
//	45 ffffffff                 field 8 32-bit
//	4a 04 01 02 ac02            field 9 packed repeated int32
//	52 06 0a0161 120162         field 10 one map entry {key=1, value=2}
//	5a 02 0807                  field 11 nested message
//	65 <4 LE bytes of 0.5>      field 12 32-bit float
//	69 <8 LE bytes of int64 -2> field 13 64-bit
const oracleVectorHex = "089601120774657374696e671801200128ffffffffffffffffff0131000000000000f83f" +
	"3a030001ff45ffffffff4a040102ac0252060a01611201625a020807650000003f69feffffffffffffff"

// oracleVectorJSON is the same value in the JSON shape the published schema
// declares: 64-bit integers are numbers (not strings), bytes are standard
// base64, and members appear in ascending field-number order.
const oracleVectorJSON = `{"sample":150,"label":"testing","flag":true,"delta":-1,` +
	`"sequence":18446744073709551615,"ratio":1.5,"payload":"AAH/","mask":4294967295,` +
	`"counts":[1,2,300],"labels":{"a":"b"},"nested":{"value":7},"share":0.5,"offset":-2}`

func TestConnectCodecMatchesThePublishedProtobufEncoding(t *testing.T) {
	codec, err := NewProtoCodec(oracleVectorDescriptor())
	if err != nil {
		t.Fatalf("NewProtoCodec: %v", err)
	}
	want, err := hex.DecodeString(oracleVectorHex)
	if err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	encoded, err := codec.Encode("Vec", []byte(oracleVectorJSON))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(encoded, want) {
		t.Errorf("encoded bytes\n got %s\nwant %s", hex.EncodeToString(encoded), oracleVectorHex)
	}
	decoded, err := codec.Decode("Vec", want)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(decoded) != oracleVectorJSON {
		t.Errorf("decoded document\n got %s\nwant %s", decoded, oracleVectorJSON)
	}
}

// A packed run and one value per tag are two encodings of the same repeated
// field. Refusing either would reject a peer that is following the rules.
func TestConnectCodecAcceptsBothRepeatedEncodings(t *testing.T) {
	codec, err := NewProtoCodec(oracleVectorDescriptor())
	if err != nil {
		t.Fatalf("NewProtoCodec: %v", err)
	}
	// 4801 4802 48ac02: field 9, varint wire type, three separate tags — the
	// unpacked encoding an older or simpler emitter produces.
	decoded, err := codec.Decode("Vec", mustHex(t, "4801480248ac02"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(decoded, &document); err != nil {
		t.Fatalf("decoded document is not JSON: %v", err)
	}
	if string(document["counts"]) != "[1,2,300]" {
		t.Errorf("counts = %s, want [1,2,300] from the unpacked encoding", document["counts"])
	}
}

// The well-known types are a declared correspondence between two forms of one
// value. Their encodings come from the published google/protobuf definitions:
// Timestamp and Duration are {seconds = 1, nanos = 2}.
func TestConnectCodecCarriesTheWellKnownTypes(t *testing.T) {
	descriptor := &clientcontract.ProtobufDescriptor{
		Syntax:   "proto3",
		Package:  "oracle.v1",
		Services: []clientcontract.ProtobufService{{Name: "OracleService", Methods: []clientcontract.ProtobufMethod{{Name: "Echo", Input: "Known", Output: "Known"}}}},
		Messages: []clientcontract.ProtobufMessage{{Name: "Known", Fields: []clientcontract.ProtobufField{
			{Name: "at", JSONName: "at", Number: 1, TypeKind: "message", Type: "google.protobuf.Timestamp"},
			{Name: "ttl", JSONName: "ttl", Number: 2, TypeKind: "message", Type: "google.protobuf.Duration"},
			{Name: "nothing", JSONName: "nothing", Number: 3, TypeKind: "message", Type: "google.protobuf.Empty"},
		}}},
		Enums: []clientcontract.ProtobufEnum{},
	}
	codec, err := NewProtoCodec(descriptor)
	if err != nil {
		t.Fatalf("NewProtoCodec: %v", err)
	}
	// Timestamp 2026-01-02T03:04:05Z is 1767323045 seconds since the epoch, so
	// the message is `08 a5ebdcca06`. Duration 1500000000ns is 1s + 500000000ns,
	// so it is `08 01 10 80cab5ee01`. Empty carries no bytes.
	const document = `{"at":"2026-01-02T03:04:05Z","ttl":1500000000,"nothing":{}}`
	const wantHex = "0a0608a5ebdcca06120808011080cab5ee011a00"
	encoded, err := codec.Encode("Known", []byte(document))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if hex.EncodeToString(encoded) != wantHex {
		t.Errorf("encoded well-known bytes\n got %s\nwant %s", hex.EncodeToString(encoded), wantHex)
	}
	raw, err := hex.DecodeString(wantHex)
	if err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	decoded, err := codec.Decode("Known", raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(decoded) != document {
		t.Errorf("decoded well-known document\n got %s\nwant %s", decoded, document)
	}
}

// presenceDescriptor separates the three shapes the proto3 presence rules treat
// differently: a field with declared presence, a field without it, a repeated
// field and a map.
func presenceDescriptor() *clientcontract.ProtobufDescriptor {
	return &clientcontract.ProtobufDescriptor{
		Syntax:   "proto3",
		Package:  "oracle.v1",
		Services: []clientcontract.ProtobufService{{Name: "OracleService", Methods: []clientcontract.ProtobufMethod{{Name: "Echo", Input: "Presence", Output: "Presence"}}}},
		Messages: []clientcontract.ProtobufMessage{{Name: "Presence", Fields: []clientcontract.ProtobufField{
			{Name: "required", JSONName: "required", Number: 1, TypeKind: "scalar", Type: "string"},
			{Name: "note", JSONName: "note", Number: 2, TypeKind: "scalar", Type: "string", Optional: true},
			{Name: "tags", JSONName: "tags", Number: 3, TypeKind: "scalar", Type: "string", Repeated: true},
			{Name: "labels", JSONName: "labels", Number: 4, TypeKind: "map", Type: "map",
				Map: &clientcontract.ProtobufMap{KeyType: "string", ValueKind: "scalar", ValueType: "string"}},
		}}},
		Enums: []clientcontract.ProtobufEnum{},
	}
}

func TestConnectCodecFollowsTheDeclaredPresenceRules(t *testing.T) {
	codec, err := NewProtoCodec(presenceDescriptor())
	if err != nil {
		t.Fatalf("NewProtoCodec: %v", err)
	}
	cases := []struct {
		name     string
		document string
		wantHex  string
		wantBack string
	}{
		{
			// A member the schema lets be absent, and that is absent, produces
			// no bytes and comes back absent. That is the whole point of
			// declared presence.
			name: "absent optional stays absent", document: `{"required":"x"}`,
			wantHex: "0a0178", wantBack: `{"required":"x","tags":[],"labels":{}}`,
		},
		{
			// proto3 has no null. The descriptor states presence for exactly
			// the members the schema lets be absent *or* null, so an explicit
			// null and an absent member are the same declared state here.
			name: "explicit null is the declared absence", document: `{"required":"x","note":null}`,
			wantHex: "0a0178", wantBack: `{"required":"x","tags":[],"labels":{}}`,
		},
		{
			// An empty list and an empty map are absent-as-empty on both wires,
			// which is why the descriptor never marks them optional.
			name: "empty collections encode to nothing", document: `{"required":"x","tags":[],"labels":{}}`,
			wantHex: "0a0178", wantBack: `{"required":"x","tags":[],"labels":{}}`,
		},
		{
			// A member without declared presence always has a value: the zero
			// one when the wire carries nothing.
			name: "a member without presence carries its zero", document: `{"required":""}`,
			wantHex: "", wantBack: `{"required":"","tags":[],"labels":{}}`,
		},
		{
			// 0a0178 | 12016e | 1a0161 | 2206 0a016b 120176
			name: "a present optional round-trips", document: `{"required":"x","note":"n","tags":["a"],"labels":{"k":"v"}}`,
			wantHex:  "0a017812016e1a016122060a016b120176",
			wantBack: `{"required":"x","note":"n","tags":["a"],"labels":{"k":"v"}}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			encoded, encodeErr := codec.Encode("Presence", []byte(testCase.document))
			if encodeErr != nil {
				t.Fatalf("Encode: %v", encodeErr)
			}
			if testCase.wantHex != "" && hex.EncodeToString(encoded) != testCase.wantHex {
				t.Errorf("encoded = %s, want %s", hex.EncodeToString(encoded), testCase.wantHex)
			}
			if testCase.wantHex == "" && len(encoded) != 0 {
				t.Errorf("encoded = %s, want no bytes", hex.EncodeToString(encoded))
			}
			decoded, decodeErr := codec.Decode("Presence", encoded)
			if decodeErr != nil {
				t.Fatalf("Decode: %v", decodeErr)
			}
			if testCase.wantBack != "" && string(decoded) != testCase.wantBack {
				t.Errorf("decoded = %s, want %s", decoded, testCase.wantBack)
			}
		})
	}
}

// A descriptor that disagrees with the document is a contract error, never a
// value this codec invents.
func TestConnectCodecRefusesWhatItCannotCarry(t *testing.T) {
	codec, err := NewProtoCodec(oracleVectorDescriptor())
	if err != nil {
		t.Fatalf("NewProtoCodec: %v", err)
	}
	cases := []struct {
		name     string
		document string
		want     string
	}{
		{"an undeclared member", `{"sample":1,"surprise":2}`, "does not declare"},
		{"a string where a number is declared", `{"sample":"x"}`, "not a 32-bit signed integer"},
		{"a number past the declared width", `{"sample":2147483648}`, "not a 32-bit signed integer"},
		{"a negative unsigned value", `{"sequence":-1}`, "not a 64-bit unsigned integer"},
		{"a list where a scalar is declared", `{"label":["a"]}`, "not a string"},
		{"base64 that is not base64", `{"payload":"!!!"}`, "not standard base64 text"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, encodeErr := codec.Encode("Vec", []byte(testCase.document)); encodeErr == nil {
				t.Fatalf("Encode accepted %s", testCase.document)
			} else if !strings.Contains(encodeErr.Error(), testCase.want) {
				t.Errorf("Encode error = %v, want it to name %q", encodeErr, testCase.want)
			}
		})
	}
	if _, err := NewProtoCodec(&clientcontract.ProtobufDescriptor{Syntax: "proto2"}); err == nil {
		t.Error("NewProtoCodec accepted a non-proto3 descriptor")
	}
	if _, err := codec.Decode("Vec", []byte{0x08}); err == nil {
		t.Error("Decode accepted a truncated varint")
	}
	// proto3 requires an unknown field to be skipped: that is how a provider
	// adds a field without breaking a client that predates it.
	skipped, err := codec.Decode("Vec", mustHex(t, "089601f80101"))
	if err != nil {
		t.Fatalf("Decode refused an unknown field: %v", err)
	}
	if !strings.Contains(string(skipped), `"sample":150`) {
		t.Errorf("decoded = %s, want the known field to survive an unknown one", skipped)
	}
}

// The status table is the Connect specification's, quoted literally: sixteen
// codes, their canonical numbers, and the HTTP status each maps to.
func TestConnectStatusTableMatchesTheSpecification(t *testing.T) {
	want := []Status{
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
	if len(connectStatuses) != len(want) {
		t.Fatalf("status table has %d rows, want the specification's %d", len(connectStatuses), len(want))
	}
	for index, expected := range want {
		if connectStatuses[index] != expected {
			t.Errorf("status %d = %#v, want %#v", index, connectStatuses[index], expected)
		}
		resolved, known := StatusByName(expected.Name)
		if !known || resolved != expected {
			t.Errorf("StatusByName(%q) = %#v, %v", expected.Name, resolved, known)
		}
	}
	if _, known := StatusByName("teapot"); known {
		t.Error("StatusByName invented a code the specification does not define")
	}
}

// The envelope is a fixed 5-byte prefix: one flag byte then a 4-byte big-endian
// length. Bit 0 is compression, bit 1 is end-of-stream, and the other six are
// reserved — a peer that sets one is speaking a protocol this runtime does not
// know, so the envelope is refused instead of guessed at.
func TestConnectEnvelopeMatchesTheSpecification(t *testing.T) {
	// One flag byte, then the length 2 as four big-endian bytes, then "hi".
	framed := AppendEnvelope(nil, 0, []byte("hi"))
	if hex.EncodeToString(framed) != "00000000026869" {
		t.Errorf("envelope = %s, want 00000000026869", hex.EncodeToString(framed))
	}
	if FlagCompressed != 0x01 || FlagEndStream != 0x02 || connectFlagReserved != 0xFC {
		t.Errorf("flags = %#x/%#x/%#x, want 0x01, 0x02 and 0xfc reserved", FlagCompressed, FlagEndStream, connectFlagReserved)
	}
	// A 300-byte payload proves the length is big-endian rather than native.
	long := AppendEnvelope(nil, FlagEndStream, bytes.Repeat([]byte{'x'}, 300))
	if long[0] != 0x02 || long[1] != 0x00 || long[2] != 0x00 || long[3] != 0x01 || long[4] != 0x2c {
		t.Errorf("prefix = % x, want 02 00 00 01 2c", long[:5])
	}
	flags, payload, err := ReadEnvelope(bytes.NewReader(framed), 1024)
	if err != nil || flags != 0 || string(payload) != "hi" {
		t.Errorf("ReadEnvelope = %v, %q, %v", flags, payload, err)
	}
	if _, _, err := ReadEnvelope(bytes.NewReader([]byte{0x04, 0, 0, 0, 0}), 1024); err == nil {
		t.Error("ReadEnvelope accepted a reserved flag bit")
	}
	// A declared length past the budget is refused before the allocation, not
	// after it.
	if _, _, err := ReadEnvelope(bytes.NewReader([]byte{0x00, 0x7f, 0xff, 0xff, 0xff}), 16); err == nil {
		t.Error("ReadEnvelope allocated for a length past the budget")
	}
}

// Compression is bounded in the decompressed direction, because that is the one
// an attacker controls: a small gzip stream expands to an arbitrary size.
func TestConnectGzipIsBoundedAfterDecompression(t *testing.T) {
	payload := bytes.Repeat([]byte{'a'}, 1<<16)
	compressed, err := CompressGzip(payload)
	if err != nil {
		t.Fatalf("CompressGzip: %v", err)
	}
	if len(compressed) >= len(payload) {
		t.Fatalf("compressed %d bytes into %d", len(payload), len(compressed))
	}
	round, err := DecompressGzip(compressed, int64(len(payload)))
	if err != nil || !bytes.Equal(round, payload) {
		t.Fatalf("DecompressGzip round trip: %v", err)
	}
	if _, err := DecompressGzip(compressed, 1024); err == nil {
		t.Error("DecompressGzip expanded past its budget")
	}
	if _, err := DecompressGzip([]byte("not gzip"), 1024); err == nil {
		t.Error("DecompressGzip accepted a payload that is not a gzip stream")
	}
}

// Connect-Timeout-Ms is a positive integer of at most ten digits. The shared
// corpus enumerates the whole grammar; this keeps the two facts a caller most
// relies on next to the rest of the wire: an absent header is not a failure,
// and a malformed one is refused rather than ignored.
func TestConnectTimeoutHeaderFollowsTheSpecification(t *testing.T) {
	if value, present, err := ParseTimeout("250"); err != nil || !present || value != 250 {
		t.Errorf("ParseTimeout(250) = %v, %v, %v", value, present, err)
	}
	if _, present, err := ParseTimeout(""); err != nil || present {
		t.Errorf("an absent header must not be a failure: %v, %v", present, err)
	}
	for _, refused := range []string{"12345678901", "-1", "1e3", "0", " 5"} {
		if _, _, err := ParseTimeout(refused); err == nil {
			t.Errorf("ParseTimeout accepted %q", refused)
		}
	}
}

// The first-party error detail is the message the TypeScript provider publishes
// (ADR 0003 of @putnami/application): code = 1 string, http_status = 2 int32,
// details_json = 3 string. Pinning its bytes here, in both packages, is what
// makes a Go client and a TypeScript client read one encoding — and what would
// catch either side drifting from the other.
func TestConnectFirstPartyErrorDetailMatchesTheSharedMessage(t *testing.T) {
	if FrameworkErrorType != "putnami.client.v1.FrameworkError" {
		t.Fatalf("detail type = %q, want the published message name", FrameworkErrorType)
	}
	envelope := FrameworkError{Code: "not_found", Status: 404, DetailsJSON: `{"resource":"widget"}`}
	// 0a 09 "not_found" | 10 9403 (varint 404) | 1a 15 <21 bytes of JSON>
	const wantHex = "0a096e6f745f666f756e641094031a157b227265736f75726365223a22776964676574227d"
	encoded := EncodeFrameworkError(envelope)
	if hex.EncodeToString(encoded) != wantHex {
		t.Errorf("FrameworkError = %s\nwant %s", hex.EncodeToString(encoded), wantHex)
	}
	// The specification calls unpadded standard base64 the normative encoding of
	// a detail's value.
	const wantBase64 = "Cglub3RfZm91bmQQlAMaFXsicmVzb3VyY2UiOiJ3aWRnZXQifQ"
	if got := DetailBase64.EncodeToString(encoded); got != wantBase64 {
		t.Errorf("detail value = %q, want %q", got, wantBase64)
	}
	back, err := DecodeFrameworkError(encoded)
	if err != nil || back != envelope {
		t.Errorf("round trip = %#v, %v", back, err)
	}
	// proto3 omits a default-valued field with no declared presence, so an
	// operation that declares no details body carries no third member.
	const wantWithoutDetails = "0a096e6f745f666f756e64109403"
	bare := EncodeFrameworkError(FrameworkError{Code: "not_found", Status: 404})
	if hex.EncodeToString(bare) != wantWithoutDetails {
		t.Errorf("FrameworkError without details = %s, want %s", hex.EncodeToString(bare), wantWithoutDetails)
	}
	// A declared details body keeps its exact bytes, so a 64-bit number in it
	// survives — which a generic value message carrying every number as a double
	// would not.
	exact := FrameworkError{Code: "quota", Status: 429, DetailsJSON: `{"limit":18446744073709551615}`}
	roundTripped, err := DecodeFrameworkError(EncodeFrameworkError(exact))
	if err != nil || roundTripped.DetailsJSON != exact.DetailsJSON {
		t.Errorf("exact details round trip = %#v, %v", roundTripped, err)
	}
	// An unknown member is skipped, so a provider may add one without breaking a
	// client that predates it.
	extended := append(append([]byte(nil), encoded...), mustHex(t, "a00601")...)
	if extendedBack, extendedErr := DecodeFrameworkError(extended); extendedErr != nil || extendedBack != envelope {
		t.Errorf("unknown member = %#v, %v", extendedBack, extendedErr)
	}
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode vector %q: %v", value, err)
	}
	return decoded
}
