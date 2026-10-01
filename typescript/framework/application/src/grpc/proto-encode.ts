import type { ProtoFieldMeta } from '../proto';
import {
  PACKED_TYPES,
  WIRE_32BIT,
  WIRE_64BIT,
  WIRE_LENGTH_DELIMITED,
  WIRE_VARINT,
  WriteBuffer,
  encodeZigZag32,
  encodeZigZag64,
  isProtoZeroValue,
  resolveFieldType,
  resolveWireType,
  resolveWireTypeForScalar,
  snakeToCamel,
  textEncoder,
} from './proto-wire';

/**
 * Declared values of every enum in the descriptor, in declaration order.
 *
 * A contract enum travels as a string (`"active"`); protobuf carries a number.
 * Position `i` in this list is number `i + 1`; number `0` is the
 * `_UNSPECIFIED` member the proto3 zero requires and the declared union has no
 * name for.
 */
export type ProtoEnumValues = Readonly<Record<string, readonly string[]>>;

/**
 * Every enum a descriptor declares: the set of type names, and each type's
 * declared members. Carried as one value because knowing a type is an enum
 * without knowing its members lets a codec write a number the peer cannot name.
 */
export interface ProtoEnumRegistry {
  readonly types: ReadonlySet<string>;
  readonly values: ProtoEnumValues;
}

/** Build an enum registry from a generated {@link ProtoDocument}'s enum metadata. */
export function protoEnumRegistry(enumMeta: Readonly<Record<string, readonly string[]>>): ProtoEnumRegistry {
  return { types: new Set(Object.keys(enumMeta)), values: enumMeta };
}

/** Resolve the wire number for an enum member, by declared string or by number. */
export function enumWireNumber(type: string, value: unknown, enumValues?: ProtoEnumValues): number {
  if (typeof value === 'number') return value;
  const declared = enumValues?.[type];
  if (typeof value === 'string' && declared) {
    const index = declared.indexOf(value);
    if (index >= 0) return index + 1;
  }
  // An undeclared member cannot be represented. Writing a guessed number would
  // hand the peer a different state than the caller named.
  throw new Error(`proto: value ${JSON.stringify(value)} is not a declared member of enum ${type}`);
}

// ---------------------------------------------------------------------------
// Proto3 binary encoder
//
// Supported wire types:
//   varint (0):  bool, int32, int64, uint32, uint64, sint32, sint64, enum
//   64-bit (1):  double, fixed64, sfixed64
//   length-delimited (2): string, bytes, nested message, packed repeated
//   32-bit (5):  float, fixed32, sfixed32
//
// Repeated scalar fields use packed encoding (proto3 default).
// ---------------------------------------------------------------------------

/**
 * Encode a JS object into proto3 binary format using field metadata.
 * Field names in `data` should be in snake_case (matching proto field names)
 * or camelCase (auto-converted).
 *
 * Uses a pre-allocated WriteBuffer that grows geometrically — avoids
 * per-field Uint8Array allocations and the final concatenation pass.
 */
export function encodeProto(
  data: Record<string, unknown>,
  fields: ProtoFieldMeta[],
  allMessages?: Record<string, ProtoFieldMeta[]>,
  enumTypes?: ReadonlySet<string>,
  enumValues?: ProtoEnumValues,
): Uint8Array {
  const wb = new WriteBuffer();
  encodeMessageInto(wb, data, fields, allMessages, enumTypes, enumValues);
  return wb.finish();
}

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: presence, oneof membership, maps and packed repeats each decide separately whether a field reaches the wire
function encodeMessageInto(
  wb: WriteBuffer,
  data: Record<string, unknown>,
  fields: ProtoFieldMeta[],
  allMessages?: Record<string, ProtoFieldMeta[]>,
  enumTypes?: ReadonlySet<string>,
  enumValues?: ProtoEnumValues,
): void {
  const writtenOneofs = new Set<string>();
  for (const field of fields) {
    const value = readFieldValue(data, field);
    if (value === undefined || value === null) continue;

    // A `oneof` holds at most one member. Writing two would hand the peer a
    // message whose resolution depends on field order.
    if (field.oneof) {
      if (writtenOneofs.has(field.oneof)) {
        throw new Error(`proto: oneof ${field.oneof} carries more than one member`);
      }
      writtenOneofs.add(field.oneof);
    } else if (!field.optional && isProtoZeroValue(field, value, enumTypes)) {
      // Implicit presence: proto3 omits a zero value, and a conforming peer
      // reads the omission back as that same zero. Writing it produces bytes no
      // other implementation produces for the same message.
      continue;
    }

    // Map fields: encode as repeated length-delimited entries (key=1, value=2)
    if (field.mapKeyType && field.mapValueType && typeof value === 'object' && !Array.isArray(value)) {
      encodeMapField(wb, field, value as Record<string, unknown>, allMessages, enumTypes, enumValues);
    } else if (field.repeated && Array.isArray(value)) {
      const fieldType = resolveFieldType(field.type, enumTypes);
      // Packed encoding for repeated scalar fields (proto3 default)
      if (PACKED_TYPES.has(fieldType)) {
        const items = enumTypes?.has(field.type)
          ? value.map((item) => enumWireNumber(field.type, item, enumValues))
          : value;
        encodePackedField(wb, field, items, fieldType);
      } else {
        for (const item of value) {
          encodeField(wb, field, item, allMessages, enumTypes, enumValues);
        }
      }
    } else {
      encodeField(wb, field, value, allMessages, enumTypes, enumValues);
    }
  }
}

/**
 * Read one field out of the source object.
 *
 * The wire name is snake case; a caller working from the generated TypeScript
 * types has the `json_name` (lower camel case). Both are accepted, wire name
 * first, so the same object encodes whichever shape produced it.
 */
function readFieldValue(data: Record<string, unknown>, field: ProtoFieldMeta): unknown {
  if (Object.hasOwn(data, field.name)) return data[field.name];
  const jsonName = field.jsonName ?? snakeToCamel(field.name);
  return data[jsonName];
}

/**
 * Write a single field into the WriteBuffer.
 */
function encodeField(
  wb: WriteBuffer,
  field: ProtoFieldMeta,
  value: unknown,
  allMessages?: Record<string, ProtoFieldMeta[]>,
  enumTypes?: ReadonlySet<string>,
  enumValues?: ProtoEnumValues,
): void {
  const wireType = resolveWireType(field.type, enumTypes);

  switch (wireType) {
    case WIRE_VARINT: {
      wb.writeVarint((field.number << 3) | WIRE_VARINT);
      if (enumTypes?.has(field.type)) {
        wb.writeVarintSigned32(enumWireNumber(field.type, value, enumValues));
        return;
      }
      writeVarintValue(wb, field.type, value);
      return;
    }
    case WIRE_64BIT: {
      wb.writeVarint((field.number << 3) | WIRE_64BIT);
      write64bit(wb, field.type, value);
      return;
    }
    case WIRE_32BIT: {
      wb.writeVarint((field.number << 3) | WIRE_32BIT);
      write32bit(wb, field.type, value);
      return;
    }
    case WIRE_LENGTH_DELIMITED: {
      wb.writeVarint((field.number << 3) | WIRE_LENGTH_DELIMITED);
      if (field.type === 'string') {
        const encoded = textEncoder.encode(String(value));
        wb.writeVarint(encoded.length);
        wb.writeBytes(encoded);
        return;
      }
      if (field.type === 'bytes') {
        const bytes = value instanceof Uint8Array ? value : textEncoder.encode(String(value));
        wb.writeVarint(bytes.length);
        wb.writeBytes(bytes);
        return;
      }
      // Nested message — encode into a temporary buffer to get the length prefix
      const nestedFields = allMessages?.[field.type];
      if (nestedFields && typeof value === 'object') {
        const nested = new WriteBuffer();
        encodeMessageInto(nested, value as Record<string, unknown>, nestedFields, allMessages, enumTypes, enumValues);
        wb.writeVarint(nested.pos);
        wb.writeBytes(nested.buf.subarray(0, nested.pos));
        return;
      }
      // Fallback: encode as string
      const fallback = textEncoder.encode(String(value));
      wb.writeVarint(fallback.length);
      wb.writeBytes(fallback);
      return;
    }
  }
}

/**
 * Packed encoding: single tag (wire type 2) + varint length + concatenated values.
 * Used for repeated scalar fields (proto3 default).
 */
function encodePackedField(wb: WriteBuffer, field: ProtoFieldMeta, values: unknown[], fieldType: string): void {
  if (values.length === 0) return;

  const inner = new WriteBuffer();
  const wireType = resolveWireTypeForScalar(fieldType);

  for (const value of values) {
    switch (wireType) {
      case WIRE_VARINT:
        writeVarintValue(inner, fieldType, value);
        break;
      case WIRE_64BIT:
        write64bit(inner, fieldType, value);
        break;
      case WIRE_32BIT:
        write32bit(inner, fieldType, value);
        break;
    }
  }

  wb.writeVarint((field.number << 3) | WIRE_LENGTH_DELIMITED);
  wb.writeVarint(inner.pos);
  wb.writeBytes(inner.buf.subarray(0, inner.pos));
}

/**
 * Encode a map field as repeated length-delimited entries.
 * Each entry is a sub-message with field 1 = key and field 2 = value.
 * This is the proto3 wire format for map<K, V> fields.
 */
function encodeMapField(
  wb: WriteBuffer,
  field: ProtoFieldMeta,
  mapData: Record<string, unknown>,
  allMessages?: Record<string, ProtoFieldMeta[]>,
  enumTypes?: ReadonlySet<string>,
  enumValues?: ProtoEnumValues,
): void {
  const keyType = field.mapKeyType!;
  const valueType = field.mapValueType!;

  for (const [key, value] of Object.entries(mapData)) {
    const entry = new WriteBuffer();

    // Field 1: key. A map entry has explicit presence on both members, so a
    // zero key or a zero value is still written — otherwise an entry keyed on
    // `0` or `""` would arrive as an entry with no key at all.
    const keyField: ProtoFieldMeta = { name: 'key', number: 1, type: keyType, optional: true, repeated: false };
    const coercedKey = coerceMapKey(key, keyType);
    encodeField(entry, keyField, coercedKey, allMessages, enumTypes, enumValues);

    // Field 2: value
    const valueField: ProtoFieldMeta = { name: 'value', number: 2, type: valueType, optional: true, repeated: false };
    encodeField(entry, valueField, value, allMessages, enumTypes, enumValues);

    wb.writeVarint((field.number << 3) | WIRE_LENGTH_DELIMITED);
    wb.writeVarint(entry.pos);
    wb.writeBytes(entry.buf.subarray(0, entry.pos));
  }
}

/** Coerce a JS string key to the correct type for proto map key encoding. */
function coerceMapKey(key: string, keyType: string): unknown {
  switch (keyType) {
    case 'int32':
    case 'int64':
    case 'uint32':
    case 'uint64':
    case 'sint32':
    case 'sint64':
    case 'fixed32':
    case 'fixed64':
    case 'sfixed32':
    case 'sfixed64':
      return Number(key);
    case 'bool':
      return key === 'true';
    default:
      return key;
  }
}

// ---------------------------------------------------------------------------
// Scalar value encoders — write directly into WriteBuffer
// ---------------------------------------------------------------------------

/** Write a single varint-type value (bool, int32, sint32, enum, etc.) */
function writeVarintValue(wb: WriteBuffer, type: string, value: unknown): void {
  if (type === 'bool') {
    wb.writeVarint(value ? 1 : 0);
  } else if (type === 'sint32') {
    wb.writeVarint(encodeZigZag32(Number(value)));
  } else if (type === 'sint64') {
    wb.writeVarint64(encodeZigZag64(BigInt(value as number | bigint)));
  } else if (type === 'int64' || type === 'uint64') {
    wb.writeVarint64(BigInt(value as number | bigint));
  } else if (type === 'int32') {
    // A negative int32 is ten sign-extended bytes, not five truncated ones.
    wb.writeVarintSigned32(Number(value));
  } else {
    // uint32
    wb.writeVarint(Number(value));
  }
}

/** Write a single 64-bit value */
function write64bit(wb: WriteBuffer, type: string, value: unknown): void {
  if (type === 'sfixed64') {
    wb.writeSFixed64(value as number | bigint);
  } else if (type === 'fixed64') {
    wb.writeFixed64(value as number | bigint);
  } else {
    // double
    wb.writeFloat64(Number(value));
  }
}

/** Write a single 32-bit value */
function write32bit(wb: WriteBuffer, type: string, value: unknown): void {
  if (type === 'sfixed32') {
    wb.writeSFixed32(Number(value));
  } else if (type === 'fixed32') {
    wb.writeFixed32(Number(value));
  } else {
    // float
    wb.writeFloat32(Number(value));
  }
}
