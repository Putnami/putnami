/**
 * The Connect protocol, version 1, as published at https://connectrpc.com/docs/protocol/.
 *
 * This module is the single place either side of a first-party Connect call
 * reads the protocol from: the provider (`grpc-handlers`, `grpc-protocol`) and
 * the generated client runtime (`@putnami/client`'s `connect-transport`) both
 * import these tables. Sharing them keeps the two halves from drifting; it does
 * not prove either half is right. The arbiter is
 * `test/grpc/connect-conformance/corpus.json`, whose scenes are transcribed
 * from the specification text and its worked examples.
 *
 * Three tables the specification separates, and which are not each other's
 * inverse:
 *
 * 1. {@link connectCodeToHttpStatus} — the status a server sends for a code.
 * 2. {@link httpStatusToConnectCode} — the code a client infers when the body
 *    carried none. `404` infers `unimplemented`, not `not_found`; `400` infers
 *    `internal`, not `invalid_argument`.
 * 3. {@link connectCodeToGrpcNumber} — the `google.rpc.Code` number, which is
 *    what a caller branching on a numeric status observes.
 */

import type { ProtoFieldMeta } from '../proto';
import { decodeProto, encodeProto } from './proto-codec';

/** The value of the `Connect-Protocol-Version` header for this protocol revision. */
export const CONNECT_PROTOCOL_VERSION = '1';

/** Header carrying the protocol revision on a unary call. */
export const CONNECT_PROTOCOL_VERSION_HEADER = 'Connect-Protocol-Version';

/** Header carrying a unary deadline, in whole milliseconds (max 10 digits). */
export const CONNECT_TIMEOUT_HEADER = 'Connect-Timeout-Ms';

/** Largest `Connect-Timeout-Ms` value the specification allows (10 digits). */
export const MAX_CONNECT_TIMEOUT_MS = 9_999_999_999;

/** Per-message compression declared on a streaming request or response. */
export const CONNECT_CONTENT_ENCODING_HEADER = 'Connect-Content-Encoding';

/** Per-message compression a streaming caller can decode. */
export const CONNECT_ACCEPT_ENCODING_HEADER = 'Connect-Accept-Encoding';

/** Streaming content type carrying JSON payloads. */
export const CT_CONNECT_STREAM_JSON = 'application/connect+json';

/** Streaming content type carrying binary protobuf payloads. */
export const CT_CONNECT_STREAM_PROTO = 'application/connect+proto';

/** Envelope flag bit 0: the message is compressed with `Connect-Content-Encoding`. */
export const ENVELOPE_FLAG_COMPRESSED = 0x01;

/** Envelope flag bit 1: the message is an {@link ConnectEndStreamResponse}. */
export const ENVELOPE_FLAG_END_STREAM = 0x02;

/** Bits 2..7 are reserved; a conforming peer never sets them. */
export const ENVELOPE_RESERVED_FLAGS = 0xfc;

/** Bytes in an envelope header: one flag byte plus a 4-byte big-endian length. */
export const ENVELOPE_HEADER_BYTES = 5;

/**
 * The closed set of Connect error codes. There are no user-defined codes.
 */
export const CONNECT_CODES = [
  'canceled',
  'unknown',
  'invalid_argument',
  'deadline_exceeded',
  'not_found',
  'already_exists',
  'permission_denied',
  'resource_exhausted',
  'failed_precondition',
  'aborted',
  'out_of_range',
  'unimplemented',
  'internal',
  'unavailable',
  'data_loss',
  'unauthenticated',
] as const;

/** One of the sixteen Connect error codes. */
export type ConnectCode = (typeof CONNECT_CODES)[number];

const CONNECT_CODE_SET: ReadonlySet<string> = new Set<string>(CONNECT_CODES);

/** True when `value` is one of the sixteen codes the protocol defines. */
export function isConnectCode(value: unknown): value is ConnectCode {
  return typeof value === 'string' && CONNECT_CODE_SET.has(value);
}

/** Code → HTTP status, per the specification's "Error Codes" table. */
const CODE_TO_HTTP: Readonly<Record<ConnectCode, number>> = {
  canceled: 499,
  unknown: 500,
  invalid_argument: 400,
  deadline_exceeded: 504,
  not_found: 404,
  already_exists: 409,
  permission_denied: 403,
  resource_exhausted: 429,
  failed_precondition: 400,
  aborted: 409,
  out_of_range: 400,
  unimplemented: 501,
  internal: 500,
  unavailable: 503,
  data_loss: 500,
  unauthenticated: 401,
};

/** Code → `google.rpc.Code`, the numeric status a caller branches on. */
const CODE_TO_GRPC: Readonly<Record<ConnectCode, number>> = {
  canceled: 1,
  unknown: 2,
  invalid_argument: 3,
  deadline_exceeded: 4,
  not_found: 5,
  already_exists: 6,
  permission_denied: 7,
  resource_exhausted: 8,
  failed_precondition: 9,
  aborted: 10,
  out_of_range: 11,
  unimplemented: 12,
  internal: 13,
  unavailable: 14,
  data_loss: 15,
  unauthenticated: 16,
};

const GRPC_TO_CODE = new Map<number, ConnectCode>(
  Object.entries(CODE_TO_GRPC).map(([code, number]) => [number, code as ConnectCode]),
);

/** The HTTP status a server sends alongside `code`. */
export function connectCodeToHttpStatus(code: ConnectCode): number {
  return CODE_TO_HTTP[code];
}

/** The `google.rpc.Code` number for `code`. */
export function connectCodeToGrpcNumber(code: ConnectCode): number {
  return CODE_TO_GRPC[code];
}

/** The Connect code for a `google.rpc.Code` number; unmapped numbers read `unknown`. */
export function grpcNumberToConnectCode(value: number): ConnectCode {
  return GRPC_TO_CODE.get(value) ?? 'unknown';
}

/**
 * The code a client infers from a non-200 status when the body carried no code.
 *
 * This is deliberately *not* the inverse of {@link connectCodeToHttpStatus}: the
 * specification's "HTTP to Error Code" table maps 400 to `internal` and 404 to
 * `unimplemented`, because an intermediary — not the service — is the likely
 * author of a bare status.
 */
export function httpStatusToConnectCode(status: number): ConnectCode {
  switch (status) {
    case 400:
      return 'internal';
    case 401:
      return 'unauthenticated';
    case 403:
      return 'permission_denied';
    case 404:
      return 'unimplemented';
    case 429:
    case 502:
    case 503:
    case 504:
      return 'unavailable';
    default:
      return 'unknown';
  }
}

/**
 * The code a *server* names for one of its own HTTP statuses.
 *
 * This is not {@link httpStatusToConnectCode}: that table is what a client
 * infers from a bare status written by an unknown party, and it deliberately
 * reads `404` as `unimplemented` and `400` as `internal`. A server knows which
 * of its own errors it raised, so it names the semantically matching code —
 * `404` really is `not_found` here — and the specification's own code-to-status
 * table then decides the status that ships with it.
 */
export function statusToConnectCode(status: number): ConnectCode {
  switch (status) {
    case 400:
    case 422:
      return 'invalid_argument';
    case 401:
      return 'unauthenticated';
    case 403:
      return 'permission_denied';
    case 404:
      return 'not_found';
    case 405:
    case 501:
      return 'unimplemented';
    case 408:
    case 504:
      return 'deadline_exceeded';
    case 409:
      return 'already_exists';
    case 412:
      return 'failed_precondition';
    case 413:
    case 429:
      return 'resource_exhausted';
    case 416:
      return 'out_of_range';
    case 499:
      return 'canceled';
    case 503:
      return 'unavailable';
    default:
      return status >= 500 ? 'internal' : 'unknown';
  }
}

/** One strongly-typed message attached to an error. */
export interface ConnectErrorDetail {
  /** Fully-qualified protobuf message name. */
  readonly type: string;
  /** Unpadded base64 of the binary protobuf payload. */
  readonly value: string;
  /** Optional human-readable rendering. Clients must not depend on it. */
  readonly debug?: unknown;
}

/** A Connect error: a code, an optional message, and optional details. */
export interface ConnectError {
  readonly code: ConnectCode;
  readonly message?: string;
  readonly details?: readonly ConnectErrorDetail[];
}

/** The final enveloped message of a response stream. */
export interface ConnectEndStreamResponse {
  /** Present only when the RPC failed. */
  readonly error?: ConnectError;
  /** Trailing metadata; header names to arrays of values. */
  readonly metadata?: Readonly<Record<string, readonly string[]>>;
}

/** Encode bytes as unpadded base64, the encoding a detail `value` uses. */
export function toUnpaddedBase64(bytes: Uint8Array): string {
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/=+$/, '');
}

/** Decode an unpadded (or padded) base64 detail `value` back to bytes. */
export function fromUnpaddedBase64(value: string): Uint8Array {
  const padded = value.length % 4 === 0 ? value : value + '='.repeat(4 - (value.length % 4));
  const binary = atob(padded);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index++) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

/**
 * Read a JSON body as a Connect `Error`.
 *
 * Returns `undefined` for every shape the specification calls invalid —
 * `{}`, `{"code": null}`, an unknown code — so the caller falls back to the
 * HTTP-status inference the specification mandates instead of inventing a code.
 */
export function parseConnectErrorBody(body: unknown): ConnectError | undefined {
  if (typeof body !== 'object' || body === null || Array.isArray(body)) return undefined;
  const record = body as Record<string, unknown>;
  if (!isConnectCode(record['code'])) return undefined;
  const message = typeof record['message'] === 'string' ? record['message'] : undefined;
  const details = parseConnectDetails(record['details']);
  return {
    code: record['code'],
    ...(message !== undefined ? { message } : {}),
    ...(details ? { details } : {}),
  };
}

function parseConnectDetails(raw: unknown): ConnectErrorDetail[] | undefined {
  if (!Array.isArray(raw)) return undefined;
  const details: ConnectErrorDetail[] = [];
  for (const entry of raw) {
    if (typeof entry !== 'object' || entry === null) continue;
    const record = entry as Record<string, unknown>;
    if (typeof record['type'] !== 'string' || typeof record['value'] !== 'string') continue;
    details.push({
      type: record['type'],
      value: record['value'],
      ...(Object.hasOwn(record, 'debug') ? { debug: record['debug'] } : {}),
    });
  }
  return details.length ? details : undefined;
}

/**
 * Read a decoded end-of-stream payload.
 *
 * `{"error": null}`, `{"error": {}}` and `{"error": {"code": null}}` are
 * invalid; each is reported as a malformed terminal rather than a success, so a
 * stream can never end successfully because its failure was unreadable.
 */
export function parseEndStreamResponse(body: unknown): ConnectEndStreamResponse | undefined {
  if (typeof body !== 'object' || body === null || Array.isArray(body)) return undefined;
  const record = body as Record<string, unknown>;
  const metadata = parseTrailingMetadata(record['metadata']);
  if (!Object.hasOwn(record, 'error')) {
    return { ...(metadata ? { metadata } : {}) };
  }
  const error = parseConnectErrorBody(record['error']);
  if (!error) return undefined;
  return { error, ...(metadata ? { metadata } : {}) };
}

function parseTrailingMetadata(raw: unknown): Record<string, string[]> | undefined {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return undefined;
  const metadata: Record<string, string[]> = {};
  for (const [key, value] of Object.entries(raw as Record<string, unknown>)) {
    if (!Array.isArray(value)) continue;
    metadata[key.toLowerCase()] = value.filter((entry): entry is string => typeof entry === 'string');
  }
  return Object.keys(metadata).length ? metadata : undefined;
}

/** Serialize a Connect error the way a conforming server writes it. */
export function serializeConnectError(error: ConnectError): Record<string, unknown> {
  return {
    code: error.code,
    ...(error.message ? { message: error.message } : {}),
    ...(error.details?.length
      ? {
          details: error.details.map((detail) => ({
            type: detail.type,
            value: detail.value,
            ...(detail.debug !== undefined ? { debug: detail.debug } : {}),
          })),
        }
      : {}),
  };
}

/**
 * Serialize the terminal message of a response stream.
 *
 * A successful RPC omits `error` entirely — `{"error": null}` is invalid — so
 * the simplest terminal is `{}`.
 */
export function serializeEndStreamResponse(response: ConnectEndStreamResponse): Record<string, unknown> {
  return {
    ...(response.error ? { error: serializeConnectError(response.error) } : {}),
    ...(response.metadata && Object.keys(response.metadata).length ? { metadata: response.metadata } : {}),
  };
}

// ---------------------------------------------------------------------------
// The first-party error envelope, carried as a Connect error detail
// ---------------------------------------------------------------------------

/**
 * Fully-qualified name of the detail carrying a Putnami framework error.
 *
 * Connect's sixteen codes are categories. A first-party operation declares a
 *stable* code (`not_found`, `http.bad_request`) plus a typed `details` member,
 * and D0.1 fixes that envelope for every transport. Details are the protocol's
 * own mechanism for "strongly-typed messages" attached to an error, so the
 * framework envelope rides there instead of overloading `code` with a value the
 * protocol does not define.
 */
export const FRAMEWORK_ERROR_DETAIL_TYPE = 'putnami.client.v1.FrameworkError';

/**
 * Descriptor for {@link FRAMEWORK_ERROR_DETAIL_TYPE}.
 *
 * ```proto
 * message FrameworkError {
 *   string code = 1;         // stable framework code
 *   int32 http_status = 2;   // the status the same error carries over REST
 *   string details_json = 3; // canonical JSON of the declared `details` member
 * }
 * ```
 *
 * `details_json` is a string because the declared detail schema differs per
 * operation: the descriptor stays fixed while the payload stays exactly the
 * bytes D0.9 says a client validates against its declared schema.
 */
export const FRAMEWORK_ERROR_DESCRIPTOR = [
  { name: 'code', jsonName: 'code', number: 1, type: 'string', optional: false, repeated: false },
  { name: 'http_status', jsonName: 'httpStatus', number: 2, type: 'int32', optional: false, repeated: false },
  { name: 'details_json', jsonName: 'detailsJson', number: 3, type: 'string', optional: false, repeated: false },
] as const;

/** The D0.1 envelope members a Connect error carries as a typed detail. */
export interface FrameworkErrorDetail {
  /** Stable framework code, the only vocabulary a generated client matches on. */
  readonly code: string;
  /** The status the same error carries over REST. */
  readonly status: number;
  /** The declared `details` member, absent when the operation declared none. */
  readonly details?: unknown;
}

/** Encode the D0.1 envelope members as the typed detail a Connect error carries. */
export function encodeFrameworkErrorDetail(envelope: FrameworkErrorDetail): ConnectErrorDetail {
  const message: Record<string, unknown> = { code: envelope.code, http_status: envelope.status };
  if (envelope.details !== undefined) message['details_json'] = JSON.stringify(envelope.details);
  return {
    type: FRAMEWORK_ERROR_DETAIL_TYPE,
    value: toUnpaddedBase64(encodeProto(message, [...FRAMEWORK_ERROR_DESCRIPTOR])),
    // `debug` is a readability affordance the specification lets a server add
    // and forbids a client from depending on; the binary `value` above is what
    // the client actually reads.
    debug: { code: envelope.code, httpStatus: envelope.status },
  };
}

/**
 * Read a first-party framework envelope out of a decoded Connect error.
 *
 * Returns `undefined` when no detail carries one, which is the ordinary case
 * for a third-party Connect service — the caller then has no declared identity
 * to select and reports the transport-level failure instead of inventing one.
 */
export function findFrameworkErrorDetail(error: ConnectError | undefined): FrameworkErrorDetail | undefined {
  const detail = error?.details?.find((entry) => entry.type === FRAMEWORK_ERROR_DETAIL_TYPE);
  if (!detail) return undefined;
  let decoded: Record<string, unknown>;
  try {
    decoded = decodeProto(fromUnpaddedBase64(detail.value), [...FRAMEWORK_ERROR_DESCRIPTOR]);
  } catch {
    return undefined;
  }
  const code = decoded['code'];
  if (typeof code !== 'string' || !code) return undefined;
  const status = decoded['httpStatus'];
  const detailsJson = decoded['detailsJson'];
  let details: unknown;
  if (typeof detailsJson === 'string' && detailsJson.length > 0) {
    try {
      details = JSON.parse(detailsJson);
    } catch {
      return undefined;
    }
  }
  return {
    code,
    status: typeof status === 'number' ? status : 0,
    ...(details !== undefined ? { details } : {}),
  };
}

// ---------------------------------------------------------------------------
// google.rpc.BadRequest
// ---------------------------------------------------------------------------

/** Fully-qualified name of the standard field-violation detail. */
export const BAD_REQUEST_DETAIL_TYPE = 'google.rpc.BadRequest';

/**
 * Descriptors for `google.rpc.BadRequest`, as published by `google/rpc/error_details.proto`.
 *
 * ```proto
 * message BadRequest {
 *   repeated FieldViolation field_violations = 1;
 *   message FieldViolation { string field = 1; string description = 2; }
 * }
 * ```
 */
export const BAD_REQUEST_DESCRIPTOR: Readonly<Record<string, readonly ProtoFieldMeta[]>> = {
  'google.rpc.BadRequest': [
    {
      name: 'field_violations',
      jsonName: 'fieldViolations',
      number: 1,
      type: 'google.rpc.BadRequest.FieldViolation',
      optional: false,
      repeated: true,
    },
  ],
  'google.rpc.BadRequest.FieldViolation': [
    { name: 'field', jsonName: 'field', number: 1, type: 'string', optional: false, repeated: false },
    { name: 'description', jsonName: 'description', number: 2, type: 'string', optional: false, repeated: false },
  ],
};

/** One field a request violated. */
export interface FieldViolation {
  readonly field: string;
  readonly description: string;
}

/**
 * Encode field violations as the standard `google.rpc.BadRequest` detail.
 *
 * The binary `value` is what the specification calls normative; `debug` carries
 * the same violations in readable form for a caller inspecting the wire, and no
 * client is allowed to depend on it.
 */
export function encodeBadRequestDetail(violations: readonly FieldViolation[]): ConnectErrorDetail {
  const message = {
    fieldViolations: violations.map((entry) => ({ field: entry.field, description: entry.description })),
  };
  return {
    type: BAD_REQUEST_DETAIL_TYPE,
    value: toUnpaddedBase64(
      encodeProto(
        message,
        [...BAD_REQUEST_DESCRIPTOR['google.rpc.BadRequest']],
        BAD_REQUEST_DESCRIPTOR as Record<string, ProtoFieldMeta[]>,
      ),
    ),
    debug: message,
  };
}

/** Decode a `google.rpc.BadRequest` detail back into its field violations. */
export function decodeBadRequestDetail(detail: ConnectErrorDetail): FieldViolation[] {
  const decoded = decodeProto(
    fromUnpaddedBase64(detail.value),
    [...BAD_REQUEST_DESCRIPTOR['google.rpc.BadRequest']],
    BAD_REQUEST_DESCRIPTOR as Record<string, ProtoFieldMeta[]>,
  );
  const violations = decoded['fieldViolations'];
  if (!Array.isArray(violations)) return [];
  return violations.map((entry) => ({
    field: String((entry as Record<string, unknown>)['field'] ?? ''),
    description: String((entry as Record<string, unknown>)['description'] ?? ''),
  }));
}

/**
 * Parse `Connect-Timeout-Ms`.
 *
 * The value is a positive integer of at most ten digits. Anything else — a
 * unit suffix, a sign, an overlong run of digits — is not a timeout, and is
 * reported as such rather than silently becoming an infinite deadline.
 */
export function parseConnectTimeout(raw: string | null): number | undefined {
  if (raw === null) return undefined;
  if (!/^\d{1,10}$/.test(raw)) return undefined;
  const value = Number(raw);
  return value > 0 ? value : undefined;
}

/** Format a deadline as `Connect-Timeout-Ms`, clamped to the ten-digit maximum. */
export function formatConnectTimeout(timeoutMs: number): string | undefined {
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) return undefined;
  return String(Math.min(Math.ceil(timeoutMs), MAX_CONNECT_TIMEOUT_MS));
}
