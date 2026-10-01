import type { BinaryBody } from '@putnami/application';

/**
 * The raw octet payloads this provider serves.
 *
 * The octets are deliberately not valid UTF-8 and not valid JSON: a pipeline
 * that re-encoded them as text or as a JSON string would corrupt them, and the
 * cross-language tests compare them byte for byte.
 */
export const blobs = new Map<string, BinaryBody>([
  ['1', new Uint8Array([0x00, 0xff, 0xfe, 0x80, 0x7f, 0x22, 0x5c, 0x0a])],
  // An empty payload is a payload: zero octets must survive the round trip
  // rather than being read as "no body".
  ['2', new Uint8Array(0)],
]);

/**
 * Bounds every raw octet payload this provider carries. It is small on
 * purpose: a sample must be able to prove the over-bound refusal without
 * moving megabytes through a loopback socket.
 */
export const BLOB_MAX_BYTES = 4096;

/** The wire media type of the sample's raw octet payloads. */
export const BLOB_MEDIA_TYPE = 'application/octet-stream';
