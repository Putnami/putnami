/**
 * gRPC status codes and Connect error decoding, client side.
 *
 * A Connect error body names its code with one of sixteen lower-case strings
 * (`not_found`, `invalid_argument`); a caller branches on the numeric
 * `google.rpc.Code`. This module converts between the two and applies the
 * specification's inference rules when the body carried no readable code.
 *
 * The tables live in `@putnami/application`'s `connect-protocol`, which the
 * provider reads too, and are checked against
 * `test/grpc/connect-conformance/corpus.json` — transcribed from the published
 * specification — rather than against the provider.
 */

import {
  type ConnectCode,
  type ConnectError,
  type FrameworkErrorDetail,
  connectCodeToGrpcNumber,
  findFrameworkErrorDetail,
  grpcNumberToConnectCode,
  httpStatusToConnectCode,
  MAX_CONNECT_TIMEOUT_MS,
  parseConnectErrorBody,
  parseEndStreamResponse,
} from '@putnami/application';

/** Canonical gRPC status codes (`google.rpc.Code`). */
export const GrpcStatus = {
  OK: 0,
  CANCELLED: 1,
  UNKNOWN: 2,
  INVALID_ARGUMENT: 3,
  DEADLINE_EXCEEDED: 4,
  NOT_FOUND: 5,
  ALREADY_EXISTS: 6,
  PERMISSION_DENIED: 7,
  RESOURCE_EXHAUSTED: 8,
  FAILED_PRECONDITION: 9,
  ABORTED: 10,
  OUT_OF_RANGE: 11,
  UNIMPLEMENTED: 12,
  INTERNAL: 13,
  UNAVAILABLE: 14,
  DATA_LOSS: 15,
  UNAUTHENTICATED: 16,
} as const;

/** Canonical gRPC status name, e.g. `'UNIMPLEMENTED'`. */
export type GrpcStatusName = keyof typeof GrpcStatus;

/** Numeric gRPC status code, e.g. `12`. */
export type GrpcStatusCode = (typeof GrpcStatus)[GrpcStatusName];

const CODE_TO_NAME = new Map<number, GrpcStatusName>(
  Object.entries(GrpcStatus).map(([name, code]) => [code, name as GrpcStatusName]),
);

/** Resolve the canonical name for a numeric gRPC status; unknown codes read `UNKNOWN`. */
export function grpcStatusName(code: number): GrpcStatusName {
  return CODE_TO_NAME.get(code) ?? 'UNKNOWN';
}

/** Resolve the numeric gRPC status for a canonical name, or `undefined` when unrecognized. */
export function grpcStatusCode(name: string): GrpcStatusCode | undefined {
  return (GrpcStatus as Record<string, GrpcStatusCode | undefined>)[name];
}

/**
 * The code a client infers from an HTTP status when the response carried no
 * readable Connect error.
 *
 * This is the protocol's "HTTP to Error Code" table, and it is deliberately not
 * the inverse of the code-to-status table: `400` infers `internal` and `404`
 * infers `unimplemented`, because a bare status is usually written by an
 * intermediary rather than by the service. Reading `404` as `NOT_FOUND` would
 * report a missing route as a missing resource.
 */
export function httpStatusToGrpcCode(httpStatus: number): GrpcStatusCode {
  if (httpStatus >= 200 && httpStatus < 300) return GrpcStatus.OK;
  return connectCodeToGrpcNumber(httpStatusToConnectCode(httpStatus)) as GrpcStatusCode;
}

/** A Connect error body decoded into its gRPC status parts. */
export interface ConnectErrorInfo {
  /** Numeric gRPC status code. */
  code: GrpcStatusCode;
  /** Canonical gRPC status name. */
  status: GrpcStatusName;
  /** The Connect code string the body named, when it carried a valid one. */
  connectCode?: ConnectCode;
  /** Server-supplied message, when the body carried one. */
  message?: string;
  /** Connect error details, when the body carried them. */
  details?: readonly unknown[];
  /** The first-party D0.1 envelope, when a detail carried one. */
  framework?: FrameworkErrorDetail;
}

/**
 * Decode a Connect error response body.
 *
 * A body that is not a valid `Error` — `{}`, `{"code": null}`, an unlisted code,
 * a gRPC-style `NOT_FOUND` — carries no code at all, and the status is inferred
 * from the HTTP status instead. Accepting a code the protocol does not define
 * would let a peer name a status no conforming implementation can produce.
 */
export function parseConnectError(body: unknown, httpStatus: number): ConnectErrorInfo {
  const error = parseConnectErrorBody(body);
  const info = connectErrorInfo(error, httpStatus);
  if (info.message !== undefined) return info;
  // The body was not a readable `Error`, so the status decided the code. A
  // human-readable `message` is still worth surfacing: it changes nothing a
  // caller branches on, and losing it leaves only `HTTP 422`.
  const salvaged = typeof body === 'object' && body !== null ? (body as Record<string, unknown>)['message'] : undefined;
  return typeof salvaged === 'string' && salvaged ? { ...info, message: salvaged } : info;
}

/** Build the numeric view of a decoded Connect error, inferring from the status when needed. */
export function connectErrorInfo(error: ConnectError | undefined, httpStatus: number): ConnectErrorInfo {
  const connectCode = error?.code ?? httpStatusToConnectCode(httpStatus);
  const code = connectCodeToGrpcNumber(connectCode) as GrpcStatusCode;
  const framework = findFrameworkErrorDetail(error);
  return {
    code,
    status: grpcStatusName(code),
    ...(error?.code ? { connectCode: error.code } : {}),
    ...(error?.message ? { message: error.message } : {}),
    ...(error?.details ? { details: error.details } : {}),
    ...(framework ? { framework } : {}),
  };
}

/** The terminal message of a Connect response stream, decoded. */
export interface ConnectStreamTerminal {
  /** True when the terminal frame was a well-formed `EndStreamResponse`. */
  valid: boolean;
  /** Absent on a successful RPC. */
  failure?: ConnectErrorInfo;
  /** Trailing metadata, lower-cased keys to values. */
  metadata?: Readonly<Record<string, readonly string[]>>;
}

/**
 * Decode the `EndStreamResponse` a response stream ends with.
 *
 * `{"error": null}`, `{"error": {}}` and `{"error": {"code": null}}` are invalid
 * terminals. Each reports `valid: false` so a stream whose failure could not be
 * read is never delivered to the caller as a success.
 */
export function parseEndStreamTerminal(body: unknown): ConnectStreamTerminal {
  const terminal = parseEndStreamResponse(body);
  if (!terminal) return { valid: false };
  return {
    valid: true,
    // A stream carries HTTP 200 throughout, so a failing terminal has no status
    // to infer from: its code is the one the terminal named.
    ...(terminal.error ? { failure: connectErrorInfo(terminal.error, 200) } : {}),
    ...(terminal.metadata ? { metadata: terminal.metadata } : {}),
  };
}

/** Convert a numeric gRPC status back to the Connect code string. */
export function grpcCodeToConnectCode(code: number): ConnectCode {
  return grpcNumberToConnectCode(code);
}

/**
 * Format a per-call deadline as `Connect-Timeout-Ms`.
 *
 * The server reads it in `parseGrpcTimeout` and aborts the handler when it
 * elapses, so the deadline the client already enforces locally is enforced on
 * the server too instead of leaving work running past the client's give-up.
 * Returns `undefined` for a non-positive or non-finite timeout.
 */
export function formatConnectTimeoutMs(timeoutMs: number): string | undefined {
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) return undefined;
  return String(Math.min(Math.ceil(timeoutMs), MAX_CONNECT_TIMEOUT_MS));
}
