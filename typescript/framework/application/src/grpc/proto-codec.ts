// ---------------------------------------------------------------------------
// Proto3 binary wire format — barrel re-exports + envelope framing
//
// Encoding/decoding logic is split into focused modules:
//   proto-wire.ts    — WriteBuffer, varint, zigzag, wire type resolution
//   proto-encode.ts  — encodeProto and field/message encoding
//   proto-decode.ts  — decodeProto and field/message decoding
// ---------------------------------------------------------------------------

export { decodeProto } from './proto-decode';
export {
  encodeProto,
  enumWireNumber,
  protoEnumRegistry,
  type ProtoEnumRegistry,
  type ProtoEnumValues,
} from './proto-encode';
export { encodeVarint, isProtoZeroValue, protoZeroValue } from './proto-wire';

// ---------------------------------------------------------------------------
// Connect envelope framing
// ---------------------------------------------------------------------------

/**
 * Wrap a payload in a Connect envelope frame.
 * Format: [flags: 1 byte][length: 4 bytes big-endian][payload]
 *
 * flags: 0x00 = data, 0x02 = end-of-stream trailers
 */
export function createEnvelope(flags: number, payload: Uint8Array): Uint8Array {
  const frame = new Uint8Array(5 + payload.length);
  frame[0] = flags;
  new DataView(frame.buffer).setUint32(1, payload.length, false); // big-endian
  frame.set(payload, 5);
  return frame;
}

/**
 * Parse a Connect envelope from a buffer.
 * Returns the flags, payload, and byte count consumed.
 */
export function readEnvelope(
  buffer: Uint8Array,
  offset = 0,
): { flags: number; payload: Uint8Array; consumed: number } | undefined {
  if (buffer.length - offset < 5) return undefined;
  const flags = buffer[offset];
  const length = new DataView(buffer.buffer, buffer.byteOffset + offset + 1, 4).getUint32(0, false);
  if (buffer.length - offset < 5 + length) return undefined;
  const payload = buffer.subarray(offset + 5, offset + 5 + length);
  return { flags, payload, consumed: 5 + length };
}
