import { HttpException } from '@putnami/runtime';
import type { DiscoveredRoute } from '../api';
import { buildHttpContext } from '../http/http-context.builder';
import type { HttpRequestContext } from '../http/http-context.type';
import type { ProtoFieldMeta } from '../proto';
import { decodeProto } from './proto-decode';
import { type ProtoEnumRegistry, readEnvelope } from './proto-codec';
import { ENVELOPE_FLAG_COMPRESSED, ENVELOPE_FLAG_END_STREAM, ENVELOPE_RESERVED_FLAGS } from './connect-protocol';
import { MAX_GRPC_MESSAGE_BYTES, SKIP_FORWARD_HEADERS, decompressGzip } from './grpc-protocol';
import { snakeToCamel } from './proto-wire';

/** How the request body frames its message, and what the codec needs to read it. */
interface RequestBodyOptions {
  /** gRPC-Web wraps a unary message in one envelope frame. */
  readonly isGrpcWeb?: boolean;
  /**
   * A Connect streaming request is a sequence of envelope frames. A server
   * stream reads exactly the first one: the specification allows a client to
   * send only one message on that call shape.
   */
  readonly enveloped?: boolean;
  /** Declared enums, needed to turn a wire number back into its member. */
  readonly enums?: ProtoEnumRegistry;
}

/**
 * Read the raw request body, enforcing the gRPC message-size limit that the
 * direct `ctx.req.arrayBuffer()` reads would otherwise bypass (the `ctx.body()`
 * guard does not cover the binary/compressed paths).
 */
async function readRequestBytes(ctx: HttpRequestContext): Promise<Uint8Array> {
  const declared = Number(ctx.req.headers.get('content-length'));
  if (Number.isFinite(declared) && declared > MAX_GRPC_MESSAGE_BYTES) {
    throw new HttpException(`Payload Too Large: gRPC message exceeds ${MAX_GRPC_MESSAGE_BYTES} bytes`, 413);
  }
  const buffer = new Uint8Array(await ctx.req.arrayBuffer());
  if (buffer.length > MAX_GRPC_MESSAGE_BYTES) {
    throw new HttpException(`Payload Too Large: gRPC message exceeds ${MAX_GRPC_MESSAGE_BYTES} bytes`, 413);
  }
  return buffer;
}

// ---------------------------------------------------------------------------
// Request parsing
// ---------------------------------------------------------------------------

// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: three framings (bare, gRPC-Web envelope, Connect stream envelope) times two codecs each read the body differently
export async function parseRequestBody(
  ctx: HttpRequestContext,
  useBinary: boolean,
  requestMeta: ProtoFieldMeta[] | undefined,
  allMessages: Record<string, ProtoFieldMeta[]>,
  options: RequestBodyOptions = {},
): Promise<Record<string, unknown>> {
  const { isGrpcWeb = false, enveloped = false, enums } = options;
  // Detect request-level compression
  const encoding = ctx.headers.get('grpc-encoding') ?? ctx.headers.get('content-encoding');
  const needsDecompress = encoding === 'gzip';

  if (enveloped) {
    const streamEncoding = ctx.headers.get('connect-content-encoding');
    const payload = readStreamRequestMessage(await readRequestBytes(ctx), streamEncoding);
    if (payload === undefined) return {};
    if (useBinary && requestMeta) return decodeProto(payload, requestMeta, allMessages, enums?.types, enums?.values);
    if (payload.length === 0) return {};
    const parsed: unknown = JSON.parse(new TextDecoder().decode(payload));
    return parsed && typeof parsed === 'object' ? (parsed as Record<string, unknown>) : {};
  }

  if (useBinary && requestMeta) {
    let buffer = await readRequestBytes(ctx);
    if (buffer.length === 0) return {};
    // gRPC-Web wraps the message in an envelope frame — strip the 5-byte header
    if (isGrpcWeb && buffer.length > 5) {
      const envelope = readEnvelope(buffer);
      if (envelope) {
        buffer = new Uint8Array(envelope.payload);
      }
    }
    if (needsDecompress) {
      buffer = decompressGzip(buffer);
    }
    return decodeProto(buffer, requestMeta, allMessages, enums?.types, enums?.values);
  }

  // JSON parsing (also handles gRPC-Web+JSON envelope)
  try {
    if (isGrpcWeb) {
      // gRPC-Web JSON may wrap body in an envelope frame
      let raw = await readRequestBytes(ctx);
      if (needsDecompress) {
        raw = decompressGzip(raw);
      }
      if (raw.length > 5) {
        const envelope = readEnvelope(raw);
        if (envelope) {
          return JSON.parse(new TextDecoder().decode(envelope.payload));
        }
      }
      // Fallback: try direct JSON parse
      return JSON.parse(new TextDecoder().decode(raw));
    }
    if (needsDecompress) {
      const raw = await readRequestBytes(ctx);
      const decompressed = decompressGzip(raw);
      return JSON.parse(new TextDecoder().decode(decompressed));
    }
    const raw = await ctx.body();
    if (raw && typeof raw === 'object') {
      return raw as Record<string, unknown>;
    }
  } catch (error) {
    // Size-limit / decompression-bomb rejections must surface as a gRPC error,
    // not be treated as an empty body.
    if (error instanceof HttpException) {
      throw error;
    }
    // Otherwise an empty/parameter-only body is fine.
  }
  return {};
}

/**
 * Read the first message of a Connect streaming request.
 *
 * The body is a sequence of `Enveloped-Message`. A request stream must never
 * set the end-stream bit, and the six most significant bits are reserved, so a
 * frame carrying either is refused rather than interpreted. An empty body is
 * the "zero or more messages" case and reads as an empty message.
 */
function readStreamRequestMessage(buffer: Uint8Array, streamEncoding: string | null): Uint8Array | undefined {
  if (buffer.length === 0) return undefined;
  const envelope = readEnvelope(buffer);
  if (!envelope) throw new HttpException('Bad Request: truncated Connect stream request envelope', 400);
  if ((envelope.flags & ENVELOPE_FLAG_END_STREAM) !== 0) {
    throw new HttpException('Bad Request: a Connect request stream must not set the end-stream flag', 400);
  }
  if ((envelope.flags & ENVELOPE_RESERVED_FLAGS) !== 0) {
    throw new HttpException('Bad Request: reserved Connect envelope flags are set', 400);
  }
  if ((envelope.flags & ENVELOPE_FLAG_COMPRESSED) === 0) return new Uint8Array(envelope.payload);
  if (streamEncoding !== 'gzip') {
    throw new HttpException('Bad Request: a compressed Connect envelope requires connect-content-encoding: gzip', 400);
  }
  return decompressGzip(new Uint8Array(envelope.payload));
}

// ---------------------------------------------------------------------------
// Request envelope
// ---------------------------------------------------------------------------

/**
 * Read the `{params, query, body}` request envelope.
 *
 * Every section is optional: a route with no path parameter sends no `params`.
 * Path and query values become the strings the route pipeline validates, as they
 * would arrive on a URL. A top-level member that names no section is ignored,
 * like the Go bridge's JSON decoder and like an unknown protobuf field.
 */
export function readRequestEnvelope(requestData: Record<string, unknown>): {
  params: Record<string, string>;
  query: Record<string, string>;
  body: Record<string, unknown>;
} {
  const section = (name: 'params' | 'query' | 'body'): Record<string, unknown> => {
    const value = requestData[name];
    if (value === undefined || value === null) return {};
    if (typeof value !== 'object' || Array.isArray(value)) {
      throw new HttpException(`Bad Request: the Connect request envelope carries "${name}" as a non-object`, 400);
    }
    return value as Record<string, unknown>;
  };
  const asStrings = (values: Record<string, unknown>): Record<string, string> => {
    const strings: Record<string, string> = {};
    for (const [key, value] of Object.entries(values)) {
      if (value !== undefined && value !== null) strings[snakeToCamel(key)] = String(value);
    }
    return strings;
  };
  return { params: asStrings(section('params')), query: asStrings(section('query')), body: section('body') };
}

// ---------------------------------------------------------------------------
// Dispatch context building
// ---------------------------------------------------------------------------

/** Build the internal URL for dispatching to the API handler. */
export function buildInternalUrl(
  route: DiscoveredRoute,
  params: Record<string, string>,
  query: Record<string, string>,
): string {
  const queryString = Object.entries(query)
    .filter(([, v]) => v !== '' && v !== undefined)
    .map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(v)}`)
    .join('&');

  let apiPath = route.path;
  for (const [paramName, paramValue] of Object.entries(params)) {
    apiPath = apiPath.replace(`[${paramName}]`, encodeURIComponent(paramValue));
  }
  return `http://internal${apiPath}${queryString ? `?${queryString}` : ''}`;
}

/** Build headers for the internal request — forward client headers except hop-by-hop and gRPC-specific. */
export function buildForwardHeaders(ctx: HttpRequestContext): Record<string, string> {
  const headers: Record<string, string> = {
    Accept: 'application/json',
    'Content-Type': 'application/json',
  };
  ctx.headers.forEach((value, name) => {
    const lower = name.toLowerCase();
    if (!SKIP_FORWARD_HEADERS.has(lower) && !lower.startsWith('grpc-')) {
      headers[name] = value;
    }
  });
  return headers;
}

/** Build the dispatch context for the API handler, propagating observability and auth context. */
export function buildDispatchContext(
  ctx: HttpRequestContext,
  internalReq: Request,
  route: DiscoveredRoute,
  params: Record<string, string>,
  query: Record<string, string>,
  body: Record<string, unknown>,
  signal?: AbortSignal,
): HttpRequestContext {
  const apiCtx = buildHttpContext<HttpRequestContext>({ req: internalReq });
  apiCtx.route = route.path;
  apiCtx.params = params;
  // Override queryParams to use our parsed values
  apiCtx.queryParams = () => query as Record<string, string>;
  // Pass parsed body directly — avoids JSON.stringify → JSON.parse round-trip
  apiCtx.body = <T>() => Promise.resolve(body as T | undefined);
  // Propagate deadline signal to context for downstream consumers (SQL, etc.)
  if (signal) {
    apiCtx.signal = signal;
  }
  // Propagate observability context from the outer (gRPC) request context
  // so that logs emitted inside handlers include the trace/correlation ID.
  if (ctx.traceId) {
    apiCtx.traceId = ctx.traceId;
  }
  if (ctx.logContext) {
    apiCtx.logContext = { ...ctx.logContext };
  }
  if (ctx.user) {
    apiCtx.user = ctx.user;
  }
  return apiCtx;
}
