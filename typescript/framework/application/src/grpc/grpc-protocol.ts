import { gunzipSync, gzipSync } from 'node:zlib';
import { HttpException } from '@putnami/runtime';
import type { ResponseDeclarations } from '../api/route/response-meta';
import type { StreamEndpointDefinition } from '../api/route/stream-endpoint';
import { projectStreamError } from '../api/stream/stream-error';
import { HttpResponse } from '../http/http-response';
import {
  type ConnectCode,
  type ConnectError,
  type ConnectErrorDetail,
  CONNECT_PROTOCOL_VERSION,
  CONNECT_TIMEOUT_HEADER,
  connectCodeToGrpcNumber,
  connectCodeToHttpStatus,
  encodeBadRequestDetail,
  encodeFrameworkErrorDetail,
  type FieldViolation,
  statusToConnectCode,
  parseConnectTimeout,
  serializeConnectError,
} from './connect-protocol';
import { createEnvelope } from './proto-codec';

const textEncoder = new TextEncoder();

/**
 * Maximum gRPC message size in bytes, applied to the raw request body and to the
 * decompressed output. Bounds memory use and prevents decompression-bomb DoS (a
 * small gzip payload inflating to an arbitrarily large buffer). Matches the
 * conventional gRPC default max receive message size (4 MiB).
 */
export const MAX_GRPC_MESSAGE_BYTES = 4 * 1024 * 1024;

// ---------------------------------------------------------------------------
// Content type constants
// ---------------------------------------------------------------------------

export const CT_JSON = 'application/json';
export const CT_PROTO = 'application/proto';
/**
 * Streaming media types, per `Streaming-Content-Type → "application/connect+"
 * ("proto" / "json")`. Re-exported from {@link ./connect-protocol} so this
 * module keeps one import surface for the gRPC handlers; the protocol module
 * owns the value.
 */
export { CT_CONNECT_STREAM_JSON, CT_CONNECT_STREAM_PROTO } from './connect-protocol';
export type { ConnectCode, ConnectError, ConnectErrorDetail } from './connect-protocol';
export const CT_GRPC_WEB_JSON = 'application/grpc-web+json';
export const CT_GRPC_WEB_PROTO = 'application/grpc-web+proto';

// ---------------------------------------------------------------------------
// Connect protocol headers
// ---------------------------------------------------------------------------

/** Returns standard Connect protocol response headers. */
export function connectResponseHeaders(): Record<string, string> {
  return {
    'Connect-Protocol-Version': CONNECT_PROTOCOL_VERSION,
    'Accept-Encoding': 'gzip, identity',
  };
}

// ---------------------------------------------------------------------------
// Content negotiation
// ---------------------------------------------------------------------------

/** Extract the media type from a Content-Type header, stripping parameters. */
export function parseMediaType(header: string): string {
  return header.split(';')[0].trim();
}

/**
 * Check if an Accept header contains any of the given media types.
 *
 * Handles quality factors (`application/proto;q=0.9`), wildcards (`application/*`),
 * and multi-value Accept headers (`application/proto, application/json`).
 */
export function acceptContains(accept: string, ...types: string[]): boolean {
  const parts = accept.split(',');
  for (let i = 0; i < parts.length; i++) {
    const mediaType = parts[i].split(';')[0].trim();
    if (types.includes(mediaType)) return true;
    // Wildcard match: application/* matches application/proto
    if (mediaType.endsWith('/*')) {
      const prefix = mediaType.slice(0, -1); // "application/"
      for (const t of types) {
        if (t.startsWith(prefix)) return true;
      }
    }
  }
  return false;
}

/** Headers that should NOT be forwarded to internal API handlers. */
export const SKIP_FORWARD_HEADERS = new Set([
  'host',
  'connection',
  'keep-alive',
  'transfer-encoding',
  'upgrade',
  'proxy-authenticate',
  'proxy-authorization',
  'te',
  'trailer',
  'content-length',
  'content-type',
  'accept',
]);

// ---------------------------------------------------------------------------
// gRPC timeout parsing
// ---------------------------------------------------------------------------

/**
 * Parse the deadline a caller declared, from either protocol on this route.
 *
 * Connect names it `Connect-Timeout-Ms` and gives it whole milliseconds; gRPC
 * and gRPC-Web name it `grpc-timeout` and give it a unit suffix. Both ride the
 * same HTTP route here, so both are read, Connect first. This is not a
 * permissive fallback: they are two protocols, each read by its own rule.
 */
export function parseGrpcTimeout(headers: Headers): { timeoutMs: number; controller: AbortController } | undefined {
  const connectTimeout = parseConnectTimeout(headers.get(CONNECT_TIMEOUT_HEADER));
  if (connectTimeout !== undefined) return { timeoutMs: connectTimeout, controller: new AbortController() };

  const raw = headers.get('grpc-timeout');
  if (!raw) return undefined;

  const match = raw.match(/^(\d+)([HMSmun])$/);
  if (!match) return undefined;

  const value = Number(match[1]);
  let timeoutMs: number;
  switch (match[2]) {
    case 'H':
      timeoutMs = value * 3_600_000;
      break;
    case 'M':
      timeoutMs = value * 60_000;
      break;
    case 'S':
      timeoutMs = value * 1000;
      break;
    case 'm':
      timeoutMs = value;
      break;
    case 'u':
      timeoutMs = Math.max(1, Math.ceil(value / 1000));
      break;
    case 'n':
      timeoutMs = Math.max(1, Math.ceil(value / 1_000_000));
      break;
    default:
      return undefined;
  }

  return { timeoutMs, controller: new AbortController() };
}

// ---------------------------------------------------------------------------
// gRPC compression (gzip) — synchronous node:zlib, no stream overhead
// ---------------------------------------------------------------------------

/** Check if the client advertises gzip support. */
export function clientAcceptsGzip(headers: Headers): boolean {
  const grpcAccept = headers.get('grpc-accept-encoding');
  if (grpcAccept) return grpcAccept.includes('gzip');
  const httpAccept = headers.get('accept-encoding');
  if (httpAccept) return httpAccept.includes('gzip');
  return false;
}

/** Compress bytes with node:zlib synchronous gzip (no stream overhead). */
export function compressGzip(data: Uint8Array): Uint8Array<ArrayBuffer> {
  return new Uint8Array(gzipSync(data));
}

/**
 * Decompress gzipped bytes, bounding the output so a small payload cannot inflate
 * to an arbitrarily large buffer (decompression-bomb DoS). Throws a 413 — mapped
 * to gRPC RESOURCE_EXHAUSTED — when the decompressed size exceeds `maxOutputBytes`.
 */
export function decompressGzip(
  data: Uint8Array,
  maxOutputBytes: number = MAX_GRPC_MESSAGE_BYTES,
): Uint8Array<ArrayBuffer> {
  try {
    return new Uint8Array(gunzipSync(data, { maxOutputLength: maxOutputBytes }));
  } catch (error) {
    if (error instanceof RangeError || (error as NodeJS.ErrnoException)?.code === 'ERR_BUFFER_TOO_LARGE') {
      throw new HttpException(`Payload Too Large: decompressed gRPC message exceeds ${maxOutputBytes} bytes`, 413);
    }
    throw error;
  }
}

/**
 * Compress the body of a HttpResponse and set Content-Encoding.
 *
 * Reads the body directly from the HttpResponse's bodyInit when available
 * (binary proto / gRPC-Web responses are already ArrayBuffer-backed),
 * avoiding a full Response → arrayBuffer() round-trip.
 */
export function compressResponse(response: HttpResponse): HttpResponse {
  const bodyInit = response.getBodyInit();
  let bodyBytes: Uint8Array;
  if (bodyInit instanceof ArrayBuffer) {
    bodyBytes = new Uint8Array(bodyInit);
  } else if (bodyInit instanceof Uint8Array) {
    bodyBytes = bodyInit;
  } else {
    // Fallback: JSON string body — encode to bytes for compression
    const encoded = typeof bodyInit === 'string' ? textEncoder.encode(bodyInit) : new Uint8Array(0);
    bodyBytes = encoded;
  }
  // Skip tiny payloads (overhead of gzip header > savings)
  if (bodyBytes.byteLength < 64) {
    return response;
  }
  const compressed = compressGzip(bodyBytes);
  return new HttpResponse(compressed.buffer as ArrayBuffer, {
    status: response.status,
    headers: [
      ...response.getHeaderEntries().filter(([n]) => n.toLowerCase() !== 'content-length'),
      ['Content-Encoding', 'gzip'],
      ['Content-Length', String(compressed.byteLength)],
    ],
  });
}

// ---------------------------------------------------------------------------
// gRPC-Web response builders
// ---------------------------------------------------------------------------

/**
 * Build a gRPC-Web response with envelope framing.
 *
 * gRPC-Web binary format:
 *   [data frame: flags=0x00, 4-byte length, payload]
 *   [trailer frame: flags=0x80, 4-byte length, HTTP-style trailers]
 *
 * Trailers use HTTP header format: `key: value\r\n`
 */
export function buildGrpcWebResponse(
  payload: Uint8Array,
  useBinary: boolean,
  grpcStatus = 0,
  grpcMessage = '',
): HttpResponse {
  const dataFrame = createEnvelope(0x00, payload);
  let trailerText = `grpc-status: ${grpcStatus}\r\n`;
  if (grpcMessage) {
    trailerText += `grpc-message: ${encodeURIComponent(grpcMessage)}\r\n`;
  }
  const trailerBytes = textEncoder.encode(trailerText);
  const trailerFrame = createEnvelope(0x80, trailerBytes);

  // Concatenate data + trailer frames
  const body = new Uint8Array(dataFrame.length + trailerFrame.length);
  body.set(dataFrame, 0);
  body.set(trailerFrame, dataFrame.length);

  return new HttpResponse(body.buffer.slice(0) as ArrayBuffer, {
    headers: {
      'Content-Type': useBinary ? CT_GRPC_WEB_PROTO : CT_GRPC_WEB_JSON,
      ...connectResponseHeaders(),
    },
  });
}

/**
 * Build a gRPC-Web trailers-only error response.
 * No data frame — just a trailer frame with the error status.
 */
export function buildGrpcWebErrorResponse(grpcCode: number, message: string, useBinary: boolean): HttpResponse {
  const trailerText = `grpc-status: ${grpcCode}\r\ngrpc-message: ${encodeURIComponent(message)}\r\n`;
  const trailerBytes = textEncoder.encode(trailerText);
  const trailerFrame = createEnvelope(0x80, trailerBytes);

  return new HttpResponse(trailerFrame.buffer.slice(0) as ArrayBuffer, {
    headers: {
      'Content-Type': useBinary ? CT_GRPC_WEB_PROTO : CT_GRPC_WEB_JSON,
      ...connectResponseHeaders(),
    },
  });
}

// ---------------------------------------------------------------------------
// gRPC error handling
// ---------------------------------------------------------------------------
/**
 * Everything a failed RPC needs on the wire, in both protocols this route
 * serves.
 *
 * `connectCode` is the protocol's category; `httpStatus` is the status the
 * specification pairs with it; `grpcCode` is the `google.rpc.Code` number
 * gRPC-Web trailers carry. `details` holds the typed messages attached to the
 * error, including the first-party D0.1 envelope when the endpoint declared
 * the error it raised.
 */
interface ConnectErrorProjection {
  connectCode: ConnectCode;
  httpStatus: number;
  grpcCode: number;
  message: string;
  details?: ConnectErrorDetail[];
}

/**
 * Project a thrown value into the Connect error a conforming client can read.
 *
 * A first-party endpoint's declared error keeps its stable code and its
 * declared `details` member: both travel in the
 * `putnami.client.v1.FrameworkError` detail, decoded by the generated client
 * into the same `ClientFrameworkError` a REST call would have produced. The
 * projection itself is `projectStreamError` — the one place the framework
 * decides what a declared error is allowed to say — so a Connect error and an
 * SSE terminal for the same endpoint cannot diverge.
 *
 * Without a route (health, reflection, an RPC with no matching endpoint) the
 * projection carries the category alone. A non-`HttpException` never reaches
 * the wire as anything but `internal`: a raw `Error.message` can carry SQL
 * fragments, paths or identifiers.
 */
export function projectConnectError(err: unknown, responses?: ResponseDeclarations): ConnectErrorProjection {
  if (!(err instanceof HttpException)) {
    return {
      connectCode: 'internal',
      httpStatus: 500,
      grpcCode: connectCodeToGrpcNumber('internal'),
      message: 'Internal error',
    };
  }
  const frameworkStatus = err.getStatus();
  const connectCode = statusToConnectCode(frameworkStatus);
  // The status that ships is the one the specification pairs with the code, so
  // a conforming client reading the pair sees no contradiction. The framework's
  // own status — which is finer-grained than the sixteen categories — travels
  // in the detail below, and is what rebuilds the typed error on the far side.
  const httpStatus = connectCodeToHttpStatus(connectCode);
  // `projectStreamError` reads `responses` and nothing else; a route and a
  // stream definition both carry it, and routing both through one projection is
  // what keeps a declared error identical on every transport.
  const envelope = projectStreamError(err, { responses } as StreamEndpointDefinition);
  const details: ConnectErrorDetail[] = [];
  // The standard violation detail comes first: a third-party Connect client
  // that knows `google.rpc.BadRequest` and nothing about Putnami still reads
  // which fields were rejected.
  const violations = fieldViolations(err.getResponse());
  if (violations) details.push(encodeBadRequestDetail(violations));
  details.push(
    encodeFrameworkErrorDetail({
      code: envelope.code,
      status: envelope.status,
      ...(envelope.details !== undefined ? { details: envelope.details } : {}),
    }),
  );
  return {
    connectCode,
    httpStatus,
    grpcCode: connectCodeToGrpcNumber(connectCode),
    message: envelope.message,
    details,
  };
}

/** Read the framework's validation-failure shape as standard field violations. */
function fieldViolations(response: string | object): FieldViolation[] | undefined {
  if (typeof response !== 'object' || response === null) return undefined;
  const errors = (response as Record<string, unknown>)['errors'];
  if (!Array.isArray(errors) || errors.length === 0) return undefined;
  return errors.map((entry) => {
    const record = (entry ?? {}) as { field?: string; message?: string };
    return { field: record.field ?? '', description: record.message ?? '' };
  });
}

/**
 * @internal Exported for tests: the numeric gRPC status and canonical name a
 * gRPC-Web caller observes for a thrown value.
 */
export function mapErrorToGrpcStatus(err: unknown): {
  grpcCode: number;
  httpStatus: number;
  code: ConnectCode;
  message: string;
  details?: ConnectErrorDetail[];
} {
  const projection = projectConnectError(err);
  return {
    grpcCode: projection.grpcCode,
    httpStatus: projection.httpStatus,
    code: projection.connectCode,
    message: projection.message,
    ...(projection.details ? { details: projection.details } : {}),
  };
}

/**
 * Convert an error into the response its protocol defines, logging server errors.
 *
 * Connect errors are a JSON `Error` under a non-200 status whose content type
 * must be `application/json`; gRPC-Web errors are a trailers-only frame under
 * HTTP 200. `responses` is the endpoint's declaration, and is what decides
 * whether the error keeps its stable code and declared details.
 */
// biome-ignore lint/suspicious/noExplicitAny: Logger type from useLogger
export function handleGrpcError(
  logger: any,
  err: unknown,
  isGrpcWeb = false,
  useBinary = false,
  responses?: ResponseDeclarations,
): HttpResponse {
  const projection = projectConnectError(err, responses);
  if (projection.httpStatus >= 500) {
    logger.error('gRPC handler error:', err);
  }
  if (isGrpcWeb) {
    return buildGrpcWebErrorResponse(projection.grpcCode, projection.message, useBinary);
  }
  return connectErrorResponse(
    {
      code: projection.connectCode,
      message: projection.message,
      ...(projection.details ? { details: projection.details } : {}),
    },
    projection.httpStatus,
  );
}

/**
 * Write a Connect `Error` as the unary response body.
 *
 * The status defaults to the one the specification pairs with the code, and the
 * content type is always `application/json`: "Errors are sent with a non-200
 * HTTP-Status. In those cases, Unary-Content-Type must be application/json."
 */
export function connectErrorResponse(error: ConnectError, httpStatus?: number): HttpResponse {
  return HttpResponse.json(serializeConnectError(error), {
    status: httpStatus ?? connectCodeToHttpStatus(error.code),
    headers: { ...connectResponseHeaders(), 'Content-Type': CT_JSON },
  });
}
