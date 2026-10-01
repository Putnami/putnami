import type { ProtoFieldMeta } from '../proto';
import type { ProtoEnumValues } from './proto-encode';
import {
  PACKED_TYPES,
  WIRE_32BIT,
  WIRE_64BIT,
  WIRE_LENGTH_DELIMITED,
  WIRE_VARINT,
  bigintToNumber,
  clearOneofSiblings,
  decodeZigZag64,
  protoFieldKey,
  protoZeroValue,
  readVarint,
  readVarint64,
  resolveFieldType,
  resolveWireTypeForScalar,
  setField,
  textDecoder,
} from './proto-wire';

// ---------------------------------------------------------------------------
// Proto3 binary decoder
//
// Unknown fields (fields not in the metadata) are silently skipped.
// This is the proto3 default behavior — unknown fields are discarded
// but do not break parsing. All remaining fields continue to decode
// correctly even when unknown fields are present in the stream.
//
// A field with implicit presence that the peer omitted reads back as its zero
// value — `0`, `''`, `false`, `[]`, `{}` — because that is the value the peer
// held. A field with explicit presence (`optional`) that the peer omitted stays
// absent, and that absence is the observable difference between the two.
// ---------------------------------------------------------------------------

/**
 * Decode proto3 binary data into a JS object using field metadata.
 *
 * Keys are each field's `json_name` — the same key the JSON codec on this route
 * produces — so a message is the same object whichever encoding delivered it. A
 * descriptor that declares no `jsonName` falls back to the snake-case wire name.
 */
// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: each wire type decides separately how many bytes it consumes and whether the field is known
export function decodeProto(
  buffer: Uint8Array,
  fields: ProtoFieldMeta[],
  allMessages?: Record<string, ProtoFieldMeta[]>,
  enumTypes?: ReadonlySet<string>,
  enumValues?: ProtoEnumValues,
): Record<string, unknown> {
  const fieldMap = new Map<number, ProtoFieldMeta>();
  for (const f of fields) {
    fieldMap.set(f.number, f);
  }

  const result: Record<string, unknown> = {};
  let offset = 0;

  while (offset < buffer.length) {
    const [tagValue, newOffset] = readVarint(buffer, offset);
    offset = newOffset;

    const fieldNumber = tagValue >>> 3;
    const wireType = tagValue & 0x07;

    const field = fieldMap.get(fieldNumber);

    // Skip unknown fields by consuming the correct number of bytes
    // based on wire type, then continue decoding remaining fields.
    switch (wireType) {
      case WIRE_VARINT: {
        // Every varint is read as 64 bits: a negative int32, sint32 or enum
        // arrives sign-extended over ten bytes, and reading only its low 32
        // bits would turn -1 into 4294967295.
        const [val, nextOffset] = readVarint64(buffer, offset);
        offset = nextOffset;
        if (field) {
          setField(result, field, decodeVarint(field, val, enumTypes, enumValues));
          clearOneofSiblings(result, fields, field);
        }
        break;
      }
      case WIRE_32BIT: {
        // Bounds-check before constructing the DataView: a truncated message
        // would otherwise throw RangeError on attacker input (consistent with
        // the varint guards, which are bounds-tolerant).
        if (offset + 4 > buffer.length) {
          offset = buffer.length;
          break;
        }
        if (field) {
          const dv = new DataView(buffer.buffer, buffer.byteOffset + offset, 4);
          setField(result, field, decode32bit(field.type, dv));
          clearOneofSiblings(result, fields, field);
        }
        offset += 4; // Always skip 4 bytes, known or unknown
        break;
      }
      case WIRE_64BIT: {
        if (offset + 8 > buffer.length) {
          offset = buffer.length;
          break;
        }
        if (field) {
          const dv = new DataView(buffer.buffer, buffer.byteOffset + offset, 8);
          setField(result, field, decode64bit(field.type, dv));
          clearOneofSiblings(result, fields, field);
        }
        offset += 8; // Always skip 8 bytes, known or unknown
        break;
      }
      case WIRE_LENGTH_DELIMITED: {
        const [length, dataOffset] = readVarint(buffer, offset);
        const data = buffer.subarray(dataOffset, dataOffset + length);
        offset = dataOffset + length; // Always advance past the blob
        if (field) {
          // Map field: decode entry sub-message into a map object
          if (field.mapKeyType && field.mapValueType) {
            decodeMapEntry(result, field, data, allMessages, enumTypes, enumValues);
          } else {
            const fieldType = resolveFieldType(field.type, enumTypes);
            // Packed repeated scalar: the blob contains concatenated values
            if (field.repeated && PACKED_TYPES.has(fieldType)) {
              decodePacked(
                result,
                field,
                data,
                fieldType,
                enumTypes?.has(field.type) ? field.type : undefined,
                enumValues,
              );
            } else if (field.type === 'string') {
              setField(result, field, textDecoder.decode(data));
            } else if (field.type === 'bytes') {
              setField(result, field, new Uint8Array(data));
            } else {
              // Nested message
              const nestedFields = allMessages?.[field.type];
              if (nestedFields) {
                setField(result, field, decodeProto(data, nestedFields, allMessages, enumTypes, enumValues));
              } else {
                setField(result, field, textDecoder.decode(data));
              }
            }
          }
          clearOneofSiblings(result, fields, field);
        }
        break;
      }
      default:
        // Wire types 3/4 (deprecated group start/end) and 6/7 (reserved)
        // cannot be safely skipped without context — bail out of this message.
        // This is a defensive measure; well-formed proto3 never uses these.
        offset = buffer.length;
        break;
    }
  }

  // Fill in what the peer omitted. Implicit presence means an absent field and
  // a zero-valued field are the same message, so the caller must see the zero
  // rather than `undefined` — the difference is what makes `optional` mean
  // something.
  for (const field of fields) {
    const key = protoFieldKey(field);
    if (Object.hasOwn(result, key)) continue;
    if (field.optional) continue;
    if (field.oneof) continue;
    const zero = protoZeroValue(field, enumTypes, enumValues);
    if (zero !== undefined) result[key] = zero;
  }

  return result;
}

// ---------------------------------------------------------------------------
// Scalar value decoders
// ---------------------------------------------------------------------------

/** Decode one varint field, read as 64 bits, into the value its type names. */
function decodeVarint(
  field: ProtoFieldMeta,
  raw: bigint,
  enumTypes?: ReadonlySet<string>,
  enumValues?: ProtoEnumValues,
): unknown {
  if (enumTypes?.has(field.type)) return decodeEnumValue(field.type, raw, enumValues);
  return decodeVarintOfType(field.type, raw);
}

function decodeVarintOfType(type: string, raw: bigint): unknown {
  switch (type) {
    case 'bool':
      return raw !== 0n;
    case 'sint32':
      return Number(BigInt.asIntN(32, decodeZigZag64(raw)));
    case 'sint64':
      return bigintToNumber(decodeZigZag64(raw));
    case 'int32':
      // int32 is the low 32 bits of the varint, read as signed.
      return Number(BigInt.asIntN(32, raw));
    case 'uint32':
      return Number(BigInt.asUintN(32, raw));
    case 'int64':
      return bigintToNumber(BigInt.asIntN(64, raw));
    default:
      // uint64
      return bigintToNumber(BigInt.asUintN(64, raw));
  }
}

/**
 * Resolve an enum's wire number back to the string the contract declared.
 *
 * Number `0` is `_UNSPECIFIED`, which the declared union has no member for: it
 * reads back as absent so a caller cannot mistake "not set" for a state. A
 * number outside the declared range keeps its numeric form rather than being
 * dropped — protobuf keeps unknown enum values, and dropping one would hide a
 * provider that has since added a member.
 */
function decodeEnumValue(type: string, raw: bigint, enumValues?: ProtoEnumValues): unknown {
  const number = Number(BigInt.asIntN(32, raw));
  const declared = enumValues?.[type];
  if (!declared) return number;
  if (number === 0) return undefined;
  return declared[number - 1] ?? number;
}

function decode32bit(type: string, dv: DataView): number {
  if (type === 'sfixed32') return dv.getInt32(0, true);
  if (type === 'fixed32') return dv.getUint32(0, true);
  // float
  return dv.getFloat32(0, true);
}

function decode64bit(type: string, dv: DataView): number | bigint {
  if (type === 'sfixed64') return bigintToNumber(dv.getBigInt64(0, true));
  if (type === 'fixed64') return bigintToNumber(dv.getBigUint64(0, true));
  // double
  return dv.getFloat64(0, true);
}

/**
 * Decode a packed repeated field.
 * The `data` blob contains concatenated scalar values without individual tags.
 */
function decodePacked(
  result: Record<string, unknown>,
  field: ProtoFieldMeta,
  data: Uint8Array,
  fieldType: string,
  enumType?: string,
  enumValues?: ProtoEnumValues,
): void {
  const wireType = resolveWireTypeForScalar(fieldType);
  let pos = 0;
  while (pos < data.length) {
    switch (wireType) {
      case WIRE_VARINT: {
        const [raw, next] = readVarint64(data, pos);
        pos = next;
        setField(
          result,
          field,
          enumType ? decodeEnumValue(enumType, raw, enumValues) : decodeVarintOfType(fieldType, raw),
        );
        break;
      }
      case WIRE_32BIT: {
        if (pos + 4 > data.length) {
          pos = data.length; // truncated packed blob — stop rather than throw
          break;
        }
        const dv = new DataView(data.buffer, data.byteOffset + pos, 4);
        pos += 4;
        setField(result, field, decode32bit(fieldType, dv));
        break;
      }
      case WIRE_64BIT: {
        if (pos + 8 > data.length) {
          pos = data.length; // truncated packed blob — stop rather than throw
          break;
        }
        const dv = new DataView(data.buffer, data.byteOffset + pos, 8);
        pos += 8;
        setField(result, field, decode64bit(fieldType, dv));
        break;
      }
      default:
        pos = data.length;
        break;
    }
  }
}

/**
 * Decode a single map entry sub-message and merge it into the result map.
 * Each entry has field 1 = key and field 2 = value.
 */
function decodeMapEntry(
  result: Record<string, unknown>,
  field: ProtoFieldMeta,
  data: Uint8Array,
  allMessages?: Record<string, ProtoFieldMeta[]>,
  enumTypes?: ReadonlySet<string>,
  enumValues?: ProtoEnumValues,
): void {
  // Decode the entry as a message with field 1 (key) and field 2 (value).
  // Both carry implicit presence, so an entry the peer wrote with a zero key or
  // a zero value arrives with that zero rather than with the member missing.
  const entryFields: ProtoFieldMeta[] = [
    { name: 'key', number: 1, type: field.mapKeyType!, optional: false, repeated: false },
    { name: 'value', number: 2, type: field.mapValueType!, optional: false, repeated: false },
  ];
  const entry = decodeProto(data, entryFields, allMessages, enumTypes, enumValues);

  // Merge into the result map. The map is built with a null prototype and
  // prototype-sensitive wire keys (`__proto__`/`constructor`/`prototype`) are
  // skipped: `key` is attacker-controlled, and on a plain object
  // `map['__proto__'] = value` would reassign the object's prototype.
  const map = (result[protoFieldKey(field)] as Record<string, unknown>) ?? Object.create(null);
  const key = String(entry['key'] ?? '');

  if (key !== '__proto__' && key !== 'constructor' && key !== 'prototype') {
    map[key] = entry['value'];
  }
  result[protoFieldKey(field)] = map;
}
