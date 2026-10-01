package connect

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"google.golang.org/protobuf/encoding/protowire"
)

// maxProtoDepth bounds message nesting on both directions. A descriptor is
// finite, but a self-referential message plus a hostile payload is not: without
// a bound a decoder recurses until the stack ends.
const maxProtoDepth = 64

// ProtoCodec converts between the JSON shape the published schema declares and
// the protobuf binary shape the published descriptor declares. It is driven
// entirely by clientcontract.ProtobufDescriptor: no generated stub exists, and
// none is needed, because the descriptor carries every field number, wire kind,
// presence flag, map shape and enum number the wire needs.
//
// The JSON side is deliberately the *schema's* JSON, not proto3 canonical JSON:
// an int64 is a JSON number (not a string), an enum is its published member (not
// its SCREAMING_SNAKE name), and a Duration is int64 nanoseconds (not "1.5s").
// That is what keeps one declaration behind `connect+proto`, `connect+json` and
// `rest-json` — see the client-contract reader's parity rules.
type ProtoCodec struct {
	pkg      string
	messages map[string]clientcontract.ProtobufMessage
	enums    map[string]clientcontract.ProtobufEnum
	methods  map[string]clientcontract.ProtobufMethod
}

// NewProtoCodec indexes a published descriptor. It refuses a descriptor whose
// syntax is not proto3 rather than guessing an encoding for proto2 presence.
func NewProtoCodec(descriptor *clientcontract.ProtobufDescriptor) (*ProtoCodec, error) {
	if descriptor == nil {
		return nil, fmt.Errorf("connect: the contract declares no protobuf descriptor, so no proto encoding can be served")
	}
	if descriptor.Syntax != "proto3" {
		return nil, fmt.Errorf("connect: protobuf descriptor declares syntax %q; only proto3 has a first-party encoding", descriptor.Syntax)
	}
	codec := &ProtoCodec{
		pkg:      descriptor.Package,
		messages: make(map[string]clientcontract.ProtobufMessage, len(descriptor.Messages)),
		enums:    make(map[string]clientcontract.ProtobufEnum, len(descriptor.Enums)),
		methods:  make(map[string]clientcontract.ProtobufMethod),
	}
	for _, message := range descriptor.Messages {
		codec.messages[message.Name] = message
	}
	for _, enum := range descriptor.Enums {
		codec.enums[enum.Name] = enum
	}
	for _, service := range descriptor.Services {
		for _, method := range service.Methods {
			codec.methods["/"+descriptor.Package+"."+service.Name+"/"+method.Name] = method
		}
	}
	return codec, nil
}

// Method resolves a Connect method identity ("/package.Service/Rpc") to the
// declared method. The identity is the URL, so this is the one join between a
// mounted URL and the messages it carries.
func (c *ProtoCodec) Method(identity string) (clientcontract.ProtobufMethod, bool) {
	method, ok := c.methods[identity]
	return method, ok
}

// Encode converts one schema-JSON document into the binary form of message.
func (c *ProtoCodec) Encode(message string, document []byte) ([]byte, error) {
	return c.encodeMessage(message, document, nil, 0)
}

// Decode converts the binary form of message into its schema-JSON document.
func (c *ProtoCodec) Decode(message string, data []byte) ([]byte, error) {
	return c.decodeMessage(message, data, 0)
}

func (c *ProtoCodec) encodeMessage(name string, document []byte, out []byte, depth int) ([]byte, error) {
	if depth > maxProtoDepth {
		return nil, fmt.Errorf("connect: message %s nests deeper than %d levels", name, maxProtoDepth)
	}
	if wellKnown, ok := c.encodeWellKnown(name, document, out); ok {
		return wellKnown.bytes, wellKnown.err
	}
	message, declared := c.messages[name]
	if !declared {
		return nil, fmt.Errorf("connect: the descriptor declares no message %q", name)
	}
	fields, err := decodeJSONObject(document)
	if err != nil {
		return nil, fmt.Errorf("connect: message %s: %w", name, err)
	}
	seen := make(map[string]bool, len(message.Fields))
	for _, field := range message.Fields {
		seen[field.JSONName] = true
		raw, present := fields[field.JSONName]
		if !present || IsJSONNull(raw) {
			// proto3 has one absence: the descriptor states it with `optional`,
			// and the reader already proved that flag agrees with what the
			// schema lets be absent or null. Writing a zero value here would
			// turn "no value" into "the zero value" on the wire.
			continue
		}
		if isProto3DefaultScalar(field, raw) {
			// Canonical proto3 omits the default value of a field with no
			// declared presence: the value is recovered from the descriptor on
			// the other side. Emitting it anyway would make two encoders of the
			// same document produce different bytes, which is exactly what a
			// cross-implementation vector is meant to catch.
			continue
		}
		out, err = c.encodeField(name, field, raw, out, depth)
		if err != nil {
			return nil, err
		}
	}
	for key := range fields {
		if !seen[key] {
			return nil, fmt.Errorf("connect: message %s carries member %q, which the descriptor does not declare; the descriptor and the published schema disagree", name, key)
		}
	}
	return out, nil
}

func (c *ProtoCodec) encodeField(message string, field clientcontract.ProtobufField, raw json.RawMessage, out []byte, depth int) ([]byte, error) {
	number, valid := protobufFieldNumber(field.Number)
	if !valid {
		return nil, fmt.Errorf("connect: message %s field %s declares field number %d, which is outside the protobuf range", message, field.JSONName, field.Number)
	}
	switch {
	case field.Map != nil:
		return c.encodeMap(message, field, number, raw, out, depth)
	case field.Repeated:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("connect: message %s field %s declares a list; the value is not a JSON array", message, field.JSONName)
		}
		if packedWireType(field) == protowire.BytesType {
			for _, item := range items {
				var err error
				out, err = c.encodeSingle(message, field, number, item, out, depth)
				if err != nil {
					return nil, err
				}
			}
			return out, nil
		}
		// proto3 packs repeated scalars of a fixed or varint wire type into one
		// length-delimited run. Both forms decode identically; the packed form
		// is the canonical one.
		var packed []byte
		for _, item := range items {
			var err error
			packed, err = c.encodeScalarValue(message, field, field.Type, item, packed)
			if err != nil {
				return nil, err
			}
		}
		if len(packed) == 0 {
			return out, nil
		}
		out = protowire.AppendTag(out, number, protowire.BytesType)
		return protowire.AppendBytes(out, packed), nil
	default:
		return c.encodeSingle(message, field, number, raw, out, depth)
	}
}

func (c *ProtoCodec) encodeSingle(message string, field clientcontract.ProtobufField, number protowire.Number, raw json.RawMessage, out []byte, depth int) ([]byte, error) {
	switch field.TypeKind {
	case "message":
		nested, err := c.encodeMessage(field.Type, raw, nil, depth+1)
		if err != nil {
			return nil, err
		}
		out = protowire.AppendTag(out, number, protowire.BytesType)
		return protowire.AppendBytes(out, nested), nil
	case "enum":
		value, err := c.encodeEnum(field.Type, raw)
		if err != nil {
			return nil, fmt.Errorf("connect: message %s field %s: %w", message, field.JSONName, err)
		}
		out = protowire.AppendTag(out, number, protowire.VarintType)
		// A proto3 enum is a signed 32-bit value on the wire, written in its
		// two's complement form like any other varint.
		return protowire.AppendVarint(out, uint64(uint32(value))), nil //nolint:gosec // enumValue keeps value in the int32 range
	case "scalar":
		wire := scalarWireType(field.Type)
		if wire == -1 {
			return nil, fmt.Errorf("connect: message %s field %s declares scalar type %q, which proto3 does not define", message, field.JSONName, field.Type)
		}
		out = protowire.AppendTag(out, number, wire)
		return c.encodeScalarValue(message, field, field.Type, raw, out)
	case "map":
		return nil, fmt.Errorf("connect: message %s field %s declares kind map without map metadata", message, field.JSONName)
	default:
		return nil, fmt.Errorf("connect: message %s field %s declares typeKind %q; the descriptor is not usable for a proto encoding", message, field.JSONName, field.TypeKind)
	}
}

func (c *ProtoCodec) encodeMap(message string, field clientcontract.ProtobufField, number protowire.Number, raw json.RawMessage, out []byte, depth int) ([]byte, error) {
	entries, err := decodeJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("connect: message %s field %s declares a map: %w", message, field.JSONName, err)
	}
	if field.Map.KeyType != "string" {
		return nil, fmt.Errorf("connect: message %s field %s declares a map keyed by %q; a first-party map key is a string on both wires", message, field.JSONName, field.Map.KeyType)
	}
	// A protobuf map is repeated entries, and repeated entries have no declared
	// order. Sorting the keys makes one JSON document produce one byte string,
	// which is what makes a cross-implementation vector meaningful.
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	valueField := clientcontract.ProtobufField{
		Name: "value", JSONName: "value", Number: 2,
		TypeKind: field.Map.ValueKind, Type: field.Map.ValueType,
	}
	for _, key := range keys {
		var entry []byte
		encodedKey, err := json.Marshal(key)
		if err != nil {
			return nil, fmt.Errorf("connect: message %s field %s map key %q is not encodable", message, field.JSONName, key)
		}
		entry, err = c.encodeSingle(message, clientcontract.ProtobufField{
			Name: "key", JSONName: "key", Number: 1, TypeKind: "scalar", Type: field.Map.KeyType,
		}, 1, encodedKey, entry, depth)
		if err != nil {
			return nil, err
		}
		if !IsJSONNull(entries[key]) {
			entry, err = c.encodeSingle(message, valueField, 2, entries[key], entry, depth)
			if err != nil {
				return nil, err
			}
		}
		out = protowire.AppendTag(out, number, protowire.BytesType)
		out = protowire.AppendBytes(out, entry)
	}
	return out, nil
}

func (c *ProtoCodec) encodeEnum(name string, raw json.RawMessage) (int, error) {
	enum, declared := c.enums[name]
	if !declared {
		return 0, fmt.Errorf("the descriptor declares no enum %q", name)
	}
	var member string
	if err := json.Unmarshal(raw, &member); err != nil {
		return 0, fmt.Errorf("enum %s carries a non-string value; a published enum member is text on the JSON wire", name)
	}
	for index, value := range enum.Values {
		if index == 0 {
			// The proto3 zero value is the absence marker: the reader already
			// proved it is not one of the published members.
			continue
		}
		if clientcontract.EnumMember(enum.Name, value.Name) == member {
			return value.Number, nil
		}
	}
	return 0, fmt.Errorf("enum %s has no value for published member %q", name, member)
}

func (c *ProtoCodec) encodeScalarValue(message string, field clientcontract.ProtobufField, scalar string, raw json.RawMessage, out []byte) ([]byte, error) {
	fail := func(want string) error {
		return fmt.Errorf("connect: message %s field %s declares %s; the JSON value is not %s", message, field.JSONName, scalar, want)
	}
	switch scalar {
	case "bool":
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fail("a boolean")
		}
		if value {
			return protowire.AppendVarint(out, 1), nil
		}
		return protowire.AppendVarint(out, 0), nil
	case "string":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fail("a string")
		}
		return protowire.AppendString(out, value), nil
	case "bytes":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fail("a base64 string")
		}
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fail("standard base64 text")
		}
		return protowire.AppendBytes(out, decoded), nil
	case "double":
		value, err := jsonFloat(raw)
		if err != nil {
			return nil, fail("a number")
		}
		return protowire.AppendFixed64(out, math.Float64bits(value)), nil
	case "float":
		value, err := jsonFloat(raw)
		if err != nil {
			return nil, fail("a number")
		}
		if value > math.MaxFloat32 || value < -math.MaxFloat32 {
			return nil, fmt.Errorf("connect: message %s field %s declares float; %v does not fit in 32 bits", message, field.JSONName, value)
		}
		return protowire.AppendFixed32(out, math.Float32bits(float32(value))), nil
	case "int32", "int64", "sint32", "sint64", "sfixed32", "sfixed64":
		value, err := jsonSigned(raw, signedBits(scalar))
		if err != nil {
			return nil, fmt.Errorf("connect: message %s field %s declares %s: %w", message, field.JSONName, scalar, err)
		}
		switch scalar {
		case "sint32", "sint64":
			return protowire.AppendVarint(out, protowire.EncodeZigZag(value)), nil
		case "sfixed32":
			return protowire.AppendFixed32(out, uint32(int32(value))), nil //nolint:gosec // range-checked by jsonSigned
		case "sfixed64":
			return protowire.AppendFixed64(out, uint64(value)), nil //nolint:gosec // two's complement is the declared encoding
		default:
			return protowire.AppendVarint(out, uint64(value)), nil //nolint:gosec // two's complement is the declared encoding
		}
	case "uint32", "uint64", "fixed32", "fixed64":
		value, err := jsonUnsigned(raw, unsignedBits(scalar))
		if err != nil {
			return nil, fmt.Errorf("connect: message %s field %s declares %s: %w", message, field.JSONName, scalar, err)
		}
		switch scalar {
		case "fixed32":
			return protowire.AppendFixed32(out, uint32(value)), nil //nolint:gosec // range-checked by jsonUnsigned
		case "fixed64":
			return protowire.AppendFixed64(out, value), nil
		default:
			return protowire.AppendVarint(out, value), nil
		}
	default:
		return nil, fmt.Errorf("connect: message %s field %s declares scalar type %q, which proto3 does not define", message, field.JSONName, scalar)
	}
}

type wellKnownResult struct {
	bytes []byte
	err   error
}

// encodeWellKnown carries the four well-known types the provider projection can
// declare. They are a declared correspondence, not an exemption: the same value
// has one form on each wire, and this is the only place the two forms differ.
func (c *ProtoCodec) encodeWellKnown(name string, document []byte, out []byte) (wellKnownResult, bool) {
	switch name {
	case "google.protobuf.Timestamp":
		var text string
		if err := json.Unmarshal(document, &text); err != nil {
			return wellKnownResult{err: fmt.Errorf("connect: google.protobuf.Timestamp carries a non-string value; the schema declares string/date-time")}, true
		}
		at, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return wellKnownResult{err: fmt.Errorf("connect: google.protobuf.Timestamp value is not an RFC 3339 instant")}, true
		}
		encoded := out
		encoded = protowire.AppendTag(encoded, 1, protowire.VarintType)
		encoded = protowire.AppendVarint(encoded, uint64(at.Unix())) //nolint:gosec // two's complement is the declared encoding
		if nanos := at.Nanosecond(); nanos != 0 {
			encoded = protowire.AppendTag(encoded, 2, protowire.VarintType)
			encoded = protowire.AppendVarint(encoded, uint64(nanos)) //nolint:gosec // Nanosecond is in [0,1e9)
		}
		return wellKnownResult{bytes: encoded}, true
	case "google.protobuf.Duration":
		value, err := jsonSigned(document, 64)
		if err != nil {
			return wellKnownResult{err: fmt.Errorf("connect: google.protobuf.Duration carries %w; the schema declares integer/int64 nanoseconds", err)}, true
		}
		encoded := out
		seconds := value / int64(time.Second)
		nanos := value % int64(time.Second)
		if seconds != 0 {
			encoded = protowire.AppendTag(encoded, 1, protowire.VarintType)
			encoded = protowire.AppendVarint(encoded, uint64(seconds)) //nolint:gosec // two's complement is the declared encoding
		}
		if nanos != 0 {
			encoded = protowire.AppendTag(encoded, 2, protowire.VarintType)
			encoded = protowire.AppendVarint(encoded, uint64(int32(nanos))) //nolint:gosec // |nanos| < 1e9
		}
		return wellKnownResult{bytes: encoded}, true
	case "google.protobuf.Empty":
		return wellKnownResult{bytes: out}, true
	case "google.protobuf.Any":
		return wellKnownResult{err: fmt.Errorf("connect: google.protobuf.Any has no first-party JSON form; declare the concrete message instead")}, true
	default:
		return wellKnownResult{}, false
	}
}

func (c *ProtoCodec) decodeMessage(name string, data []byte, depth int) ([]byte, error) {
	if depth > maxProtoDepth {
		return nil, fmt.Errorf("connect: message %s nests deeper than %d levels", name, maxProtoDepth)
	}
	if decoded, ok, err := c.decodeWellKnown(name, data); ok {
		return decoded, err
	}
	message, declared := c.messages[name]
	if !declared {
		return nil, fmt.Errorf("connect: the descriptor declares no message %q", name)
	}
	byNumber := make(map[protowire.Number]clientcontract.ProtobufField, len(message.Fields))
	for _, field := range message.Fields {
		number, valid := protobufFieldNumber(field.Number)
		if !valid {
			return nil, fmt.Errorf("connect: message %s field %s declares field number %d, which is outside the protobuf range", name, field.JSONName, field.Number)
		}
		byNumber[number] = field
	}
	values := make(map[string][]json.RawMessage, len(message.Fields))
	mapEntries := make(map[string][][2]json.RawMessage)
	for len(data) > 0 {
		number, wire, headerLen := protowire.ConsumeTag(data)
		if headerLen < 0 {
			return nil, fmt.Errorf("connect: message %s carries a malformed field tag", name)
		}
		data = data[headerLen:]
		field, known := byNumber[number]
		if !known {
			// proto3 requires an unknown field to be skipped rather than to fail:
			// that is how a provider adds a field without breaking a client that
			// predates it. Only a *declared* disagreement is a contract error.
			skip := protowire.ConsumeFieldValue(number, wire, data)
			if skip < 0 {
				return nil, fmt.Errorf("connect: message %s carries a malformed value for unknown field %d", name, number)
			}
			data = data[skip:]
			continue
		}
		rest, err := c.decodeFieldValue(name, field, wire, data, values, mapEntries, depth)
		if err != nil {
			return nil, err
		}
		data = rest
	}
	return c.renderMessageJSON(message, values, mapEntries)
}

func (c *ProtoCodec) decodeFieldValue(
	message string,
	field clientcontract.ProtobufField,
	wire protowire.Type,
	data []byte,
	values map[string][]json.RawMessage,
	mapEntries map[string][][2]json.RawMessage,
	depth int,
) ([]byte, error) {
	if field.Map != nil {
		if wire != protowire.BytesType {
			return nil, fmt.Errorf("connect: message %s field %s declares a map but the wire type is %d", message, field.JSONName, wire)
		}
		entry, size := protowire.ConsumeBytes(data)
		if size < 0 {
			return nil, fmt.Errorf("connect: message %s field %s carries a malformed map entry", message, field.JSONName)
		}
		key, value, err := c.decodeMapEntry(message, field, entry, depth)
		if err != nil {
			return nil, err
		}
		mapEntries[field.JSONName] = append(mapEntries[field.JSONName], [2]json.RawMessage{key, value})
		return data[size:], nil
	}
	// A packed run carries every element of a repeated scalar field in one
	// length-delimited value. Accepting both forms is required: an encoder is
	// free to emit either.
	if field.Repeated && wire == protowire.BytesType && packedWireType(field) != protowire.BytesType {
		run, size := protowire.ConsumeBytes(data)
		if size < 0 {
			return nil, fmt.Errorf("connect: message %s field %s carries a malformed packed run", message, field.JSONName)
		}
		element := packedWireType(field)
		for len(run) > 0 {
			value, consumed, err := c.decodeSingleValue(message, field, element, run, depth)
			if err != nil {
				return nil, err
			}
			values[field.JSONName] = append(values[field.JSONName], value)
			run = run[consumed:]
		}
		return data[size:], nil
	}
	value, consumed, err := c.decodeSingleValue(message, field, wire, data, depth)
	if err != nil {
		return nil, err
	}
	values[field.JSONName] = append(values[field.JSONName], value)
	return data[consumed:], nil
}

func (c *ProtoCodec) decodeMapEntry(message string, field clientcontract.ProtobufField, entry []byte, depth int) (json.RawMessage, json.RawMessage, error) {
	keyField := clientcontract.ProtobufField{Name: "key", JSONName: "key", Number: 1, TypeKind: "scalar", Type: field.Map.KeyType}
	valueField := clientcontract.ProtobufField{Name: "value", JSONName: "value", Number: 2, TypeKind: field.Map.ValueKind, Type: field.Map.ValueType}
	key := json.RawMessage(`""`)
	value := zeroJSONFor(valueField)
	for len(entry) > 0 {
		number, wire, headerLen := protowire.ConsumeTag(entry)
		if headerLen < 0 {
			return nil, nil, fmt.Errorf("connect: message %s field %s carries a malformed map entry tag", message, field.JSONName)
		}
		entry = entry[headerLen:]
		switch number {
		case 1:
			decoded, consumed, err := c.decodeSingleValue(message, keyField, wire, entry, depth)
			if err != nil {
				return nil, nil, err
			}
			key, entry = decoded, entry[consumed:]
		case 2:
			decoded, consumed, err := c.decodeSingleValue(message, valueField, wire, entry, depth)
			if err != nil {
				return nil, nil, err
			}
			value, entry = decoded, entry[consumed:]
		default:
			skip := protowire.ConsumeFieldValue(number, wire, entry)
			if skip < 0 {
				return nil, nil, fmt.Errorf("connect: message %s field %s carries a malformed map entry member", message, field.JSONName)
			}
			entry = entry[skip:]
		}
	}
	return key, value, nil
}

func (c *ProtoCodec) decodeSingleValue(message string, field clientcontract.ProtobufField, wire protowire.Type, data []byte, depth int) (json.RawMessage, int, error) {
	malformed := func() error {
		return fmt.Errorf("connect: message %s field %s carries a malformed value", message, field.JSONName)
	}
	switch field.TypeKind {
	case "message":
		if wire != protowire.BytesType {
			return nil, 0, fmt.Errorf("connect: message %s field %s declares a message but the wire type is %d", message, field.JSONName, wire)
		}
		nested, size := protowire.ConsumeBytes(data)
		if size < 0 {
			return nil, 0, malformed()
		}
		decoded, err := c.decodeMessage(field.Type, nested, depth+1)
		if err != nil {
			return nil, 0, err
		}
		return decoded, size, nil
	case "enum":
		if wire != protowire.VarintType {
			return nil, 0, fmt.Errorf("connect: message %s field %s declares an enum but the wire type is %d", message, field.JSONName, wire)
		}
		raw, size := protowire.ConsumeVarint(data)
		if size < 0 {
			return nil, 0, malformed()
		}
		member, err := c.decodeEnum(field.Type, int(int32(uint32(raw)))) //nolint:gosec // a proto3 enum is a signed 32-bit varint
		if err != nil {
			return nil, 0, fmt.Errorf("connect: message %s field %s: %w", message, field.JSONName, err)
		}
		return member, size, nil
	case "scalar":
		return c.decodeScalarValue(message, field, wire, data)
	default:
		return nil, 0, fmt.Errorf("connect: message %s field %s declares typeKind %q; the descriptor is not usable for a proto encoding", message, field.JSONName, field.TypeKind)
	}
}

func (c *ProtoCodec) decodeScalarValue(message string, field clientcontract.ProtobufField, wire protowire.Type, data []byte) (json.RawMessage, int, error) {
	expected := scalarWireType(field.Type)
	if expected == -1 {
		return nil, 0, fmt.Errorf("connect: message %s field %s declares scalar type %q, which proto3 does not define", message, field.JSONName, field.Type)
	}
	if wire != expected {
		return nil, 0, fmt.Errorf("connect: message %s field %s declares %s (wire type %d) but the value carries wire type %d", message, field.JSONName, field.Type, expected, wire)
	}
	malformed := func() (json.RawMessage, int, error) {
		return nil, 0, fmt.Errorf("connect: message %s field %s carries a malformed %s", message, field.JSONName, field.Type)
	}
	switch field.Type {
	case "bool":
		raw, size := protowire.ConsumeVarint(data)
		if size < 0 {
			return malformed()
		}
		if raw != 0 {
			return json.RawMessage("true"), size, nil
		}
		return json.RawMessage("false"), size, nil
	case "string":
		raw, size := protowire.ConsumeString(data)
		if size < 0 {
			return malformed()
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			return malformed()
		}
		return encoded, size, nil
	case "bytes":
		raw, size := protowire.ConsumeBytes(data)
		if size < 0 {
			return malformed()
		}
		encoded, err := json.Marshal(base64.StdEncoding.EncodeToString(raw))
		if err != nil {
			return malformed()
		}
		return encoded, size, nil
	case "double":
		raw, size := protowire.ConsumeFixed64(data)
		if size < 0 {
			return malformed()
		}
		encoded, err := jsonNumber(math.Float64frombits(raw), 64)
		return encoded, size, err
	case "float":
		raw, size := protowire.ConsumeFixed32(data)
		if size < 0 {
			return malformed()
		}
		encoded, err := jsonNumber(float64(math.Float32frombits(raw)), 32)
		return encoded, size, err
	case "int32", "int64":
		raw, size := protowire.ConsumeVarint(data)
		if size < 0 {
			return malformed()
		}
		return json.RawMessage(strconv.FormatInt(narrowSigned(int64(raw), signedBits(field.Type)), 10)), size, nil //nolint:gosec // two's complement is the declared encoding
	case "sint32", "sint64":
		raw, size := protowire.ConsumeVarint(data)
		if size < 0 {
			return malformed()
		}
		return json.RawMessage(strconv.FormatInt(narrowSigned(protowire.DecodeZigZag(raw), signedBits(field.Type)), 10)), size, nil
	case "sfixed32":
		raw, size := protowire.ConsumeFixed32(data)
		if size < 0 {
			return malformed()
		}
		return json.RawMessage(strconv.FormatInt(int64(int32(raw)), 10)), size, nil //nolint:gosec // sfixed32 is two's complement by definition
	case "sfixed64":
		raw, size := protowire.ConsumeFixed64(data)
		if size < 0 {
			return malformed()
		}
		return json.RawMessage(strconv.FormatInt(int64(raw), 10)), size, nil //nolint:gosec // two's complement is the declared encoding
	case "uint32", "uint64":
		raw, size := protowire.ConsumeVarint(data)
		if size < 0 {
			return malformed()
		}
		return json.RawMessage(strconv.FormatUint(narrowUnsigned(raw, unsignedBits(field.Type)), 10)), size, nil
	case "fixed32":
		raw, size := protowire.ConsumeFixed32(data)
		if size < 0 {
			return malformed()
		}
		return json.RawMessage(strconv.FormatUint(uint64(raw), 10)), size, nil
	case "fixed64":
		raw, size := protowire.ConsumeFixed64(data)
		if size < 0 {
			return malformed()
		}
		return json.RawMessage(strconv.FormatUint(raw, 10)), size, nil
	default:
		return nil, 0, fmt.Errorf("connect: message %s field %s declares scalar type %q, which proto3 does not define", message, field.JSONName, field.Type)
	}
}

func (c *ProtoCodec) decodeEnum(name string, number int) (json.RawMessage, error) {
	enum, declared := c.enums[name]
	if !declared {
		return nil, fmt.Errorf("the descriptor declares no enum %q", name)
	}
	for index, value := range enum.Values {
		if value.Number != number {
			continue
		}
		if index == 0 {
			return nil, nil
		}
		encoded, err := json.Marshal(clientcontract.EnumMember(enum.Name, value.Name))
		if err != nil {
			return nil, fmt.Errorf("enum %s member %q is not encodable", name, value.Name)
		}
		return encoded, nil
	}
	return nil, fmt.Errorf("enum %s declares no value %d; a first-party client never invents a member", name, number)
}

// decodeWellKnown reverses encodeWellKnown.
func (c *ProtoCodec) decodeWellKnown(name string, data []byte) (json.RawMessage, bool, error) {
	switch name {
	case "google.protobuf.Timestamp", "google.protobuf.Duration":
		seconds, nanos, err := decodeSecondsNanos(name, data)
		if err != nil {
			return nil, true, err
		}
		if name == "google.protobuf.Duration" {
			total := seconds*int64(time.Second) + int64(nanos)
			return json.RawMessage(strconv.FormatInt(total, 10)), true, nil
		}
		encoded, err := json.Marshal(time.Unix(seconds, int64(nanos)).UTC().Format(time.RFC3339Nano))
		if err != nil {
			return nil, true, fmt.Errorf("connect: google.protobuf.Timestamp is not encodable")
		}
		return encoded, true, nil
	case "google.protobuf.Empty":
		return json.RawMessage("{}"), true, nil
	case "google.protobuf.Any":
		return nil, true, fmt.Errorf("connect: google.protobuf.Any has no first-party JSON form; declare the concrete message instead")
	default:
		return nil, false, nil
	}
}

func decodeSecondsNanos(name string, data []byte) (int64, int32, error) {
	var seconds int64
	var nanos int32
	for len(data) > 0 {
		number, wire, headerLen := protowire.ConsumeTag(data)
		if headerLen < 0 || wire != protowire.VarintType {
			return 0, 0, fmt.Errorf("connect: %s carries a malformed value", name)
		}
		data = data[headerLen:]
		raw, size := protowire.ConsumeVarint(data)
		if size < 0 {
			return 0, 0, fmt.Errorf("connect: %s carries a malformed value", name)
		}
		switch number {
		case 1:
			seconds = int64(raw) //nolint:gosec // two's complement is the declared encoding
		case 2:
			nanos = int32(int64(raw)) //nolint:gosec // nanos is a 32-bit field
		}
		data = data[size:]
	}
	return seconds, nanos, nil
}

// renderMessageJSON writes the decoded values in ascending field-number order so
// one wire message always produces one JSON document.
func (c *ProtoCodec) renderMessageJSON(
	message clientcontract.ProtobufMessage,
	values map[string][]json.RawMessage,
	mapEntries map[string][][2]json.RawMessage,
) ([]byte, error) {
	var out strings.Builder
	out.WriteByte('{')
	first := true
	writeMember := func(name string, value []byte) error {
		if !first {
			out.WriteByte(',')
		}
		first = false
		encodedName, err := json.Marshal(name)
		if err != nil {
			return fmt.Errorf("connect: message %s member %q is not encodable", message.Name, name)
		}
		out.Write(encodedName)
		out.WriteByte(':')
		out.Write(value)
		return nil
	}
	for _, field := range message.Fields {
		switch {
		case field.Map != nil:
			entries := mapEntries[field.JSONName]
			var object strings.Builder
			object.WriteByte('{')
			for index, entry := range entries {
				if index > 0 {
					object.WriteByte(',')
				}
				object.Write(entry[0])
				object.WriteByte(':')
				object.Write(entry[1])
			}
			object.WriteByte('}')
			if err := writeMember(field.JSONName, []byte(object.String())); err != nil {
				return nil, err
			}
		case field.Repeated:
			var array strings.Builder
			array.WriteByte('[')
			for index, value := range values[field.JSONName] {
				if index > 0 {
					array.WriteByte(',')
				}
				array.Write(value)
			}
			array.WriteByte(']')
			if err := writeMember(field.JSONName, []byte(array.String())); err != nil {
				return nil, err
			}
		default:
			decoded := values[field.JSONName]
			if len(decoded) == 0 {
				if field.Optional {
					// Declared presence: the schema lets this member be absent,
					// and it is.
					continue
				}
				if err := writeMember(field.JSONName, zeroJSONFor(field)); err != nil {
					return nil, err
				}
				continue
			}
			// proto3 keeps the last occurrence of a non-repeated field.
			last := decoded[len(decoded)-1]
			if last == nil {
				// An enum resolved to its proto3 zero value, which is the
				// absence marker and has no published member.
				continue
			}
			if err := writeMember(field.JSONName, last); err != nil {
				return nil, err
			}
		}
	}
	out.WriteByte('}')
	return []byte(out.String()), nil
}

// isProto3DefaultScalar reports whether a scalar or enum value is the proto3
// default of a field that carries no declared presence. A field the descriptor
// marks optional is never omitted: that flag is precisely the statement that
// its zero value and its absence are two different things.
func isProto3DefaultScalar(field clientcontract.ProtobufField, raw json.RawMessage) bool {
	if field.Optional || field.Repeated || field.Map != nil {
		return false
	}
	if field.TypeKind != "scalar" {
		return false
	}
	return string(TrimSpace(raw)) == string(zeroJSONFor(field))
}

// zeroJSONFor is the JSON form of a proto3 default. proto3 does not distinguish
// "absent" from "the zero value" for a field without declared presence, so a
// field the descriptor does not mark optional always has a value on the JSON
// side too — the schema declares it required for exactly that reason.
func zeroJSONFor(field clientcontract.ProtobufField) json.RawMessage {
	if field.TypeKind == "message" {
		return json.RawMessage("{}")
	}
	if field.TypeKind == "enum" {
		return nil
	}
	switch field.Type {
	case "bool":
		return json.RawMessage("false")
	case "string":
		return json.RawMessage(`""`)
	case "bytes":
		return json.RawMessage(`""`)
	case "double", "float":
		return json.RawMessage("0")
	default:
		return json.RawMessage("0")
	}
}

func scalarWireType(scalar string) protowire.Type {
	switch scalar {
	case "int32", "int64", "uint32", "uint64", "sint32", "sint64", "bool":
		return protowire.VarintType
	case "fixed64", "sfixed64", "double":
		return protowire.Fixed64Type
	case "fixed32", "sfixed32", "float":
		return protowire.Fixed32Type
	case "string", "bytes":
		return protowire.BytesType
	default:
		return -1
	}
}

// packedWireType is the element wire type of a repeated field, or BytesType when
// the field is not packable (strings, bytes and messages never pack).
func packedWireType(field clientcontract.ProtobufField) protowire.Type {
	switch field.TypeKind {
	case "enum":
		return protowire.VarintType
	case "scalar":
		wire := scalarWireType(field.Type)
		if wire == protowire.BytesType || wire == -1 {
			return protowire.BytesType
		}
		return wire
	default:
		return protowire.BytesType
	}
}

func signedBits(scalar string) int {
	switch scalar {
	case "int32", "sint32", "sfixed32":
		return 32
	default:
		return 64
	}
}

func unsignedBits(scalar string) int {
	switch scalar {
	case "uint32", "fixed32":
		return 32
	default:
		return 64
	}
}

// narrowSigned and narrowUnsigned re-apply the declared width. proto3 writes a
// 32-bit integer as a full varint, so a peer that sent a wider value is truncated
// to the width the descriptor declares — which is what every protobuf runtime
// does, and what keeps the decoded value inside the published schema's bounds.
func narrowSigned(value int64, bits int) int64 {
	if bits == 32 {
		return int64(int32(value)) //nolint:gosec // the declared 32-bit width is the truncation
	}
	return value
}

func narrowUnsigned(value uint64, bits int) uint64 {
	if bits == 32 {
		return uint64(uint32(value)) //nolint:gosec // the declared 32-bit width is the truncation
	}
	return value
}

// protobufFieldNumber converts a declared field number, refusing one outside the
// protobuf range before the conversion rather than wrapping it.
func protobufFieldNumber(number int) (protowire.Number, bool) {
	if number < int(protowire.MinValidNumber) || number > int(protowire.MaxValidNumber) {
		return 0, false
	}
	return protowire.Number(number), true
}

func jsonSigned(raw json.RawMessage, bits int) (int64, error) {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	value, err := strconv.ParseInt(text, 10, bits)
	if err != nil {
		return 0, fmt.Errorf("%q is not a %d-bit signed integer", text, bits)
	}
	return value, nil
}

func jsonUnsigned(raw json.RawMessage, bits int) (uint64, error) {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	value, err := strconv.ParseUint(text, 10, bits)
	if err != nil {
		return 0, fmt.Errorf("%q is not a %d-bit unsigned integer", text, bits)
	}
	return value, nil
}

func jsonFloat(raw json.RawMessage) (float64, error) {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	return strconv.ParseFloat(text, 64)
}

// jsonNumber renders a decoded float without letting a non-finite value reach a
// JSON document, which has no form for one.
func jsonNumber(value float64, bits int) (json.RawMessage, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf("connect: a %d-bit float carries a non-finite value, which JSON cannot represent", bits)
	}
	return json.RawMessage(strconv.FormatFloat(value, 'g', -1, bits)), nil
}

func decodeJSONObject(document []byte) (map[string]json.RawMessage, error) {
	if len(document) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document, &fields); err != nil {
		return nil, fmt.Errorf("the value is not a JSON object")
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	return fields, nil
}

// IsJSONNull reports whether raw is the JSON literal null, ignoring surrounding whitespace.
func IsJSONNull(raw json.RawMessage) bool {
	return string(TrimSpace(raw)) == "null"
}

// TrimSpace returns raw without leading and trailing whitespace.
func TrimSpace(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(raw)))
}
