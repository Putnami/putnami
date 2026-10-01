import type { ProtoFieldMeta } from '../proto';

// ---------------------------------------------------------------------------
// Wire type constants
// ---------------------------------------------------------------------------

export const WIRE_VARINT = 0;
export const WIRE_64BIT = 1;
export const WIRE_LENGTH_DELIMITED = 2;
export const WIRE_32BIT = 5;

/** Scalar types eligible for packed repeated encoding (proto3 default). */
export const PACKED_TYPES = new Set([
  'bool',
  'int32',
  'int64',
  'uint32',
  'uint64',
  'sint32',
  'sint64',
  'double',
  'float',
  'fixed32',
  'fixed64',
  'sfixed32',
  'sfixed64',
]);

export const textEncoder = new TextEncoder();
export const textDecoder = new TextDecoder();

// ---------------------------------------------------------------------------
// WriteBuffer — pre-allocated, geometrically-growing byte buffer
//
// Eliminates per-field Uint8Array allocations during encoding. All writes
// go into a single contiguous buffer that doubles when full.
// ---------------------------------------------------------------------------

const INITIAL_CAPACITY = 256;

export class WriteBuffer {
  buf: Uint8Array;
  pos = 0;
  private dv: DataView;

  constructor(capacity = INITIAL_CAPACITY) {
    this.buf = new Uint8Array(capacity);
    this.dv = new DataView(this.buf.buffer);
  }

  private ensure(needed: number): void {
    const required = this.pos + needed;
    if (required <= this.buf.length) return;
    let newCap = this.buf.length;
    while (newCap < required) newCap <<= 1;
    const next = new Uint8Array(newCap);
    next.set(this.buf.subarray(0, this.pos));
    this.buf = next;
    this.dv = new DataView(this.buf.buffer);
  }

  writeByte(v: number): void {
    this.ensure(1);
    this.buf[this.pos++] = v;
  }

  writeBytes(src: Uint8Array): void {
    this.ensure(src.length);
    this.buf.set(src, this.pos);
    this.pos += src.length;
  }

  /** Write an unsigned 32-bit varint. Only valid for tags, lengths and uint32. */
  writeVarint(value: number): void {
    let v = value >>> 0;
    this.ensure(5);
    while (v > 0x7f) {
      this.buf[this.pos++] = (v & 0x7f) | 0x80;
      v >>>= 7;
    }
    this.buf[this.pos++] = v & 0x7f;
  }

  /**
   * Write a signed 32-bit varint.
   *
   * A negative `int32` (and a negative enum) is sign-extended to 64 bits and
   * occupies ten bytes: that is what protobuf specifies and what every other
   * implementation writes. Truncating to five bytes produces a value a
   * conforming peer reads as a large positive `uint64`.
   */
  writeVarintSigned32(value: number): void {
    const normalized = value | 0;
    if (normalized >= 0) {
      this.writeVarint(normalized);
      return;
    }
    this.writeVarint64(BigInt(normalized));
  }

  writeVarint64(value: number | bigint): void {
    let v = BigInt.asUintN(64, BigInt(value));
    this.ensure(10);
    while (v > 0x7fn) {
      this.buf[this.pos++] = Number(v & 0x7fn) | 0x80;
      v >>= 7n;
    }
    this.buf[this.pos++] = Number(v & 0x7fn);
  }

  writeFloat64(value: number): void {
    this.ensure(8);
    this.dv.setFloat64(this.pos, value, true);
    this.pos += 8;
  }

  writeFloat32(value: number): void {
    this.ensure(4);
    this.dv.setFloat32(this.pos, value, true);
    this.pos += 4;
  }

  writeFixed32(value: number): void {
    this.ensure(4);
    this.dv.setUint32(this.pos, value, true);
    this.pos += 4;
  }

  writeSFixed32(value: number): void {
    this.ensure(4);
    this.dv.setInt32(this.pos, value, true);
    this.pos += 4;
  }

  writeFixed64(value: number | bigint): void {
    this.ensure(8);
    this.dv.setBigUint64(this.pos, BigInt(value), true);
    this.pos += 8;
  }

  writeSFixed64(value: number | bigint): void {
    this.ensure(8);
    this.dv.setBigInt64(this.pos, BigInt(value), true);
    this.pos += 8;
  }

  /** Return a trimmed copy of the written bytes. */
  finish(): Uint8Array {
    return this.buf.slice(0, this.pos);
  }
}

// ---------------------------------------------------------------------------
// Varint encoding / decoding
// ---------------------------------------------------------------------------

export function encodeVarint(value: number): Uint8Array {
  const bytes: number[] = [];
  let v = value >>> 0; // Ensure unsigned 32-bit

  while (v > 0x7f) {
    bytes.push((v & 0x7f) | 0x80);
    v >>>= 7;
  }
  bytes.push(v & 0x7f);

  return new Uint8Array(bytes);
}

/**
 * Read a varint, returning a 32-bit number.
 * Consumes up to 10 bytes (full 64-bit varint) but only captures the low 32 bits.
 * Safe for tags, lengths, and 32-bit field values.
 */
export function readVarint(buffer: Uint8Array, offset: number): [value: number, newOffset: number] {
  let result = 0;
  let shift = 0;
  let pos = offset;

  while (pos < buffer.length) {
    const byte = buffer[pos];
    if (shift < 32) {
      result |= (byte & 0x7f) << shift;
    }
    pos++;
    if ((byte & 0x80) === 0) {
      return [result >>> 0, pos];
    }
    shift += 7;
    if (shift >= 70) break; // Max 10 bytes for 64-bit varint
  }

  return [result >>> 0, pos];
}

/** Read a varint, returning a full 64-bit BigInt value. */
export function readVarint64(buffer: Uint8Array, offset: number): [value: bigint, newOffset: number] {
  let result = 0n;
  let shift = 0n;
  let pos = offset;

  while (pos < buffer.length) {
    const byte = buffer[pos];
    result |= BigInt(byte & 0x7f) << shift;
    pos++;
    if ((byte & 0x80) === 0) {
      return [result, pos];
    }
    shift += 7n;
    if (shift >= 70n) break; // Max 10 bytes for 64-bit varint
  }

  return [result, pos];
}

// ---------------------------------------------------------------------------
// ZigZag encoding — used by sint32/sint64 for efficient negative values
// ---------------------------------------------------------------------------

export function encodeZigZag32(value: number): number {
  return ((value << 1) ^ (value >> 31)) >>> 0;
}

export function encodeZigZag64(value: bigint): bigint {
  return (value << 1n) ^ (value >> 63n);
}

export function decodeZigZag64(value: bigint): bigint {
  return (value >> 1n) ^ -(value & 1n);
}

/** Convert BigInt to Number if it fits in safe integer range. */
export function bigintToNumber(v: bigint): number | bigint {
  if (v >= -9007199254740991n && v <= 9007199254740991n) {
    return Number(v);
  }
  return v;
}

/**
 * The value an implicit-presence field of this type reads back as when absent.
 *
 * `declaredEnums` names the enums whose members the contract declared as
 * strings. For those, the zero member is `_UNSPECIFIED`, which the declared
 * union has no name for, so the field reads back as absent rather than as a
 * state the caller could mistake for a declared one. A numeric enum with no
 * declared members keeps protobuf's own zero.
 */
export function protoZeroValue(
  field: ProtoFieldMeta,
  enumTypes?: ReadonlySet<string>,
  declaredEnums?: Readonly<Record<string, readonly string[]>>,
): unknown {
  if (field.repeated) return [];
  if (field.mapKeyType && field.mapValueType) return Object.create(null);
  if (enumTypes?.has(field.type)) return declaredEnums?.[field.type] ? undefined : 0;
  switch (field.type) {
    case 'bool':
      return false;
    case 'string':
      return '';
    case 'bytes':
      return new Uint8Array(0);
    case 'int64':
    case 'uint64':
    case 'sint64':
    case 'fixed64':
    case 'sfixed64':
      return 0;
    case 'int32':
    case 'uint32':
    case 'sint32':
    case 'fixed32':
    case 'sfixed32':
    case 'double':
    case 'float':
      return 0;
    default:
      // A nested message that was never sent is absent, not an empty message:
      // proto3 gives message fields explicit presence.
      return undefined;
  }
}

/** True when `value` is the implicit-presence zero for this field, and so is not written. */
export function isProtoZeroValue(field: ProtoFieldMeta, value: unknown, enumTypes?: ReadonlySet<string>): boolean {
  if (field.repeated) return Array.isArray(value) && value.length === 0;
  if (field.mapKeyType && field.mapValueType) {
    return typeof value === 'object' && value !== null && Object.keys(value).length === 0;
  }
  if (enumTypes?.has(field.type)) return value === 0 || value === '' || value === undefined;
  switch (field.type) {
    case 'bool':
      return value === false;
    case 'string':
      return value === '';
    case 'bytes':
      return value instanceof Uint8Array && value.length === 0;
    case 'int64':
    case 'uint64':
    case 'sint64':
    case 'fixed64':
    case 'sfixed64':
      return value === 0 || value === 0n;
    case 'int32':
    case 'uint32':
    case 'sint32':
    case 'fixed32':
    case 'sfixed32':
    case 'double':
    case 'float':
      // `-0` is a distinct IEEE value but the same protobuf default.
      return value === 0;
    default:
      return false;
  }
}

// ---------------------------------------------------------------------------
// Wire type resolution
// ---------------------------------------------------------------------------

/** Resolve wire type for a proto field type (may be an enum or message name). */
export function resolveWireType(type: string, enumTypes?: ReadonlySet<string>): number {
  const scalar = resolveWireTypeForScalar(type);
  if (scalar !== -1) return scalar;
  // Check if this is an enum type (varint-encoded)
  if (enumTypes?.has(type)) return WIRE_VARINT;
  // Nested message types are length-delimited
  return WIRE_LENGTH_DELIMITED;
}

/** Resolve wire type for known scalar types. Returns -1 for unknown types. */
export function resolveWireTypeForScalar(type: string): number {
  switch (type) {
    case 'bool':
    case 'int32':
    case 'int64':
    case 'uint32':
    case 'uint64':
    case 'sint32':
    case 'sint64':
      return WIRE_VARINT;
    case 'double':
    case 'fixed64':
    case 'sfixed64':
      return WIRE_64BIT;
    case 'float':
    case 'fixed32':
    case 'sfixed32':
      return WIRE_32BIT;
    case 'string':
    case 'bytes':
      return WIRE_LENGTH_DELIMITED;
    default:
      return -1;
  }
}

/**
 * Resolve an enum type to its underlying scalar type name.
 * Enum types → the enum name itself (but treated as varint).
 * Known scalars → returned as-is.
 */
export function resolveFieldType(type: string, enumTypes?: ReadonlySet<string>): string {
  if (PACKED_TYPES.has(type)) return type;
  if (enumTypes?.has(type)) return 'int32'; // Enums are varint-encoded integers
  return type;
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

export function snakeToCamel(str: string): string {
  return str.replace(/_([a-z])/g, (_, c) => c.toUpperCase());
}

/**
 * The object key a decoded field takes.
 *
 * `json_name` — lower camel case — is the key the contract's own schemas use
 * and the key the JSON codec on the same route produces, so a message decoded
 * from protobuf and the same message decoded from JSON have the same shape. The
 * snake-case wire name is the fallback for a hand-written descriptor that
 * declares no `jsonName`.
 */
export function protoFieldKey(field: ProtoFieldMeta): string {
  return field.jsonName ?? field.name;
}

/** Set a field value on the result object, handling repeated fields as arrays. */
export function setField(result: Record<string, unknown>, field: ProtoFieldMeta, value: unknown): void {
  const key = protoFieldKey(field);
  if (field.repeated) {
    const arr = (result[key] as unknown[]) ?? [];
    arr.push(value);
    result[key] = arr;
  } else {
    result[key] = value;
  }
}

/**
 * Drop the other members of a `oneof` once one of them has been read.
 *
 * A `oneof` holds at most one member: a message that carries two is malformed,
 * and protobuf resolves it as last-one-wins. Clearing the siblings here is what
 * makes that resolution observable to the caller instead of leaving two
 * mutually exclusive members set.
 */
export function clearOneofSiblings(
  result: Record<string, unknown>,
  fields: readonly ProtoFieldMeta[],
  field: ProtoFieldMeta,
): void {
  if (!field.oneof) return;
  for (const sibling of fields) {
    if (sibling.oneof === field.oneof && sibling.name !== field.name) delete result[protoFieldKey(sibling)];
  }
}
