import { HttpException, runInContext, useLogger } from '@putnami/runtime';
import type { DiscoveredRoute } from '../api';
import type { StreamEndpointDefinition } from '../api/route/stream-endpoint';
import type { HttpRequestContext } from '../http/http-context.type';
import { HttpResponse } from '../http/http-response';
import type { RouteHandler } from '../http/route.type';
import type { ProtoFieldMeta } from '../proto';
import { protoRpcName } from '../proto/proto';
import { createEnvelope, encodeProto, type ProtoEnumRegistry } from './proto-codec';
import {
  type ConnectEndStreamResponse,
  type ConnectError,
  CONNECT_ACCEPT_ENCODING_HEADER,
  CONNECT_CONTENT_ENCODING_HEADER,
  CT_CONNECT_STREAM_JSON,
  CT_CONNECT_STREAM_PROTO,
  ENVELOPE_FLAG_COMPRESSED,
  ENVELOPE_FLAG_END_STREAM,
  serializeEndStreamResponse,
} from './connect-protocol';
import {
  CT_GRPC_WEB_JSON,
  CT_GRPC_WEB_PROTO,
  CT_JSON,
  CT_PROTO,
  acceptContains,
  buildGrpcWebErrorResponse,
  clientAcceptsGzip,
  connectErrorResponse,
  compressGzip,
  compressResponse,
  connectResponseHeaders,
  handleGrpcError,
  parseGrpcTimeout,
  parseMediaType,
  projectConnectError,
} from './grpc-protocol';
import {
  buildDispatchContext,
  buildForwardHeaders,
  buildInternalUrl,
  parseRequestBody,
  readRequestEnvelope,
} from './grpc-request';
import { formatResult } from './grpc-response';

const textEncoder = new TextEncoder();

/**
 * Run a stream endpoint's own middleware chain and return the refusal it
 * produced, or `undefined` when the chain accepted. It mirrors what the
 * first-party SSE and WebSocket dispatchers do before admitting a stream.
 */
async function refusedByEndpointChain(
  streamDef: StreamEndpointDefinition,
  ctx: HttpRequestContext,
): Promise<HttpException | undefined> {
  const accepted = new HttpResponse(undefined, { status: 204 });
  const chain = [...(streamDef.middleware ?? [])].reduceRight<() => Promise<HttpResponse | undefined>>(
    (next, middleware) => () => middleware(ctx, next),
    async () => accepted,
  );
  const result = await chain();
  if (result === accepted) return undefined;
  return refusedResponse(result) ?? new HttpException('Request refused', ctx.user ? 403 : 401);
}

/**
 * The exception an error-status response stands for, or `undefined` when the
 * result is a success. A middleware refuses by returning a response, so this is
 * what carries that refusal into the same projection a thrown exception takes.
 */
function refusedResponse(result: unknown): HttpException | undefined {
  if (!(result instanceof HttpResponse)) return undefined;
  const status = result.status ?? 200;
  if (status < 400) return undefined;
  const body = result.rawData();
  const message =
    typeof body === 'object' && body !== null
      ? ((body as { message?: unknown; error?: unknown }).message ?? (body as { error?: unknown }).error)
      : body;
  return new HttpException(typeof message === 'string' ? message : 'Request refused', status);
}

// ---------------------------------------------------------------------------
// Unary RPC handler — direct dispatch to API handler
// ---------------------------------------------------------------------------

/**
 * Build an HTTP handler for a unary gRPC/Connect RPC.
 *
 * Dispatches directly to the resolved API handler — no server.fetch() loopback.
 * Supports both JSON and binary protobuf content types.
 */
export function buildUnaryRpcHandler(
  apiHandler: RouteHandler,
  route: DiscoveredRoute,
  messageMeta: Record<string, ProtoFieldMeta[]>,
  enums?: ProtoEnumRegistry,
  compressionEnabled = false,
): (ctx: HttpRequestContext) => Promise<HttpResponse> {
  const rpcName = buildRpcName(route);
  const requestMeta = messageMeta[`${rpcName}Request`];
  const responseMeta = messageMeta[`${rpcName}Response`];

  return async (ctx: HttpRequestContext) => {
    const logger = useLogger('putnami:grpc');
    const deadline = parseGrpcTimeout(ctx.headers);
    let deadlineTimer: ReturnType<typeof setTimeout> | undefined;

    // Content negotiation (before try so catch can use isGrpcWeb)
    const contentType = ctx.headers.get('content-type') ?? CT_JSON;
    const mediaType = parseMediaType(contentType);
    const useBinaryRequest = mediaType === CT_PROTO || mediaType === CT_GRPC_WEB_PROTO;
    const isGrpcWeb = mediaType === CT_GRPC_WEB_PROTO || mediaType === CT_GRPC_WEB_JSON;
    const acceptHeader = ctx.headers.get('accept') ?? contentType;
    const useBinaryResponse = acceptContains(acceptHeader, CT_PROTO, CT_GRPC_WEB_PROTO);

    try {
      // Parse request body (JSON or protobuf binary), decompress if needed
      const requestData = await parseRequestBody(ctx, useBinaryRequest, requestMeta, messageMeta, { isGrpcWeb, enums });

      // Read the request envelope's params, query and body sections
      const { params, query, body } = readRequestEnvelope(requestData);

      // Build internal URL and forward headers
      const url = buildInternalUrl(route, params, query);
      const headers = buildForwardHeaders(ctx);

      // Build internal Request — body is passed through context directly (no JSON round-trip)
      const signal = deadline ? deadline.controller.signal : undefined;
      const internalReq = new Request(url, {
        method: route.method,
        headers: new Headers(headers),
        signal,
      });

      // Start deadline timer
      if (deadline) {
        deadlineTimer = setTimeout(() => deadline.controller.abort(), deadline.timeoutMs);
      }

      // Build context and dispatch directly to the handler
      const apiCtx = buildDispatchContext(ctx, internalReq, route, params, query, body, signal);

      const result = await runInContext(apiCtx, () => apiHandler(apiCtx));

      // A refusal the chain produced as a response rather than by throwing —
      // the security guard's 401 and 403 are the ones that matter — is an error
      // on the Connect wire too. Encoding it as the success message would
      // answer 200 with an error body, which a conforming client reads as a
      // successful call.
      const refusal = refusedResponse(result);
      if (refusal) {
        return handleGrpcError(logger, refusal, isGrpcWeb, useBinaryResponse, route.responses);
      }

      const acceptsGzip = compressionEnabled && clientAcceptsGzip(ctx.headers);

      // Format response
      const response = formatResult(result, useBinaryResponse, responseMeta, messageMeta, enums, isGrpcWeb);

      // Compress response if client accepts gzip (unary only, not gRPC-Web)
      if (acceptsGzip && !isGrpcWeb) {
        return response instanceof Promise ? compressResponse(await response) : compressResponse(response);
      }
      return response instanceof Promise ? await response : response;
    } catch (err) {
      if (deadline?.controller.signal.aborted) {
        if (isGrpcWeb) {
          return buildGrpcWebErrorResponse(4, 'Request deadline exceeded', useBinaryResponse);
        }
        return connectErrorResponse({ code: 'deadline_exceeded', message: 'Request deadline exceeded' });
      }
      return handleGrpcError(logger, err, isGrpcWeb, useBinaryResponse, route.responses);
    } finally {
      if (deadlineTimer) clearTimeout(deadlineTimer);
    }
  };
}

// ---------------------------------------------------------------------------
// Server streaming RPC handler
// ---------------------------------------------------------------------------

/**
 * Build an HTTP handler that rejects a client- or bidi-streaming RPC with
 * gRPC status 12 UNIMPLEMENTED.
 *
 * Connect over HTTP/1.1 (Bun.serve's ceiling) cannot carry a client stream,
 * so registering the RPC as unary would fail with a mis-shaped request
 * instead of a clean status. The generated clients route these modes through
 * the WebSocket stream transport; this handler names that path for callers
 * that hit the Connect route directly.
 */
export function buildUnimplementedRpcHandler(mode: string): (ctx: HttpRequestContext) => Promise<HttpResponse> {
  return async (ctx: HttpRequestContext) => {
    const logger = useLogger('putnami:grpc');
    const contentType = ctx.headers.get('content-type') ?? CT_JSON;
    const mediaType = parseMediaType(contentType);
    const isGrpcWeb = mediaType === CT_GRPC_WEB_PROTO || mediaType === CT_GRPC_WEB_JSON;
    const acceptHeader = ctx.headers.get('accept') ?? contentType;
    const useBinaryResponse = acceptContains(acceptHeader, CT_PROTO, CT_GRPC_WEB_PROTO);
    const error = new HttpException(
      `${mode} streaming is not implemented over Connect/HTTP1.1; use the WebSocket stream transport for this RPC`,
      501,
    );
    return handleGrpcError(logger, error, isGrpcWeb, useBinaryResponse);
  };
}

/**
 * Build an HTTP handler for a server-streaming gRPC/Connect RPC.
 *
 * Uses the Connect streaming protocol — response is a sequence of enveloped frames.
 * Bridges the stream endpoint's `ctx.send()` to Connect envelope frames.
 */
export function buildStreamRpcHandler(
  streamDef: StreamEndpointDefinition,
  route: DiscoveredRoute,
  messageMeta: Record<string, ProtoFieldMeta[]>,
  enums?: ProtoEnumRegistry,
  compressionEnabled = false,
): (ctx: HttpRequestContext) => Promise<HttpResponse> {
  const rpcName = buildRpcName(route);
  const requestMeta = messageMeta[`${rpcName}Request`];
  const responseMeta = messageMeta[`${rpcName}Response`];

  return async (ctx: HttpRequestContext) => {
    const logger = useLogger('putnami:grpc');
    const deadline = parseGrpcTimeout(ctx.headers);

    try {
      // Streaming-Content-Type is `application/connect+{proto,json}`; the
      // response codec mirrors the request's unless the caller narrowed it with
      // Accept. `application/proto` is accepted on the request line too because
      // the unary and streaming routes share one HTTP path.
      const contentType = ctx.headers.get('content-type') ?? CT_CONNECT_STREAM_JSON;
      const streamMediaType = parseMediaType(contentType);
      const useBinaryRequest = streamMediaType === CT_CONNECT_STREAM_PROTO || streamMediaType === CT_PROTO;
      const acceptHeader = ctx.headers.get('accept') ?? contentType;
      const useBinaryResponse = acceptContains(acceptHeader, CT_PROTO, CT_CONNECT_STREAM_PROTO);

      const requestData = await parseRequestBody(ctx, useBinaryRequest, requestMeta, messageMeta, {
        enveloped: true,
        enums,
      });
      const { params, query } = readRequestEnvelope(requestData);

      const useGzip = compressionEnabled && clientAcceptsGzip(ctx.headers);

      // The endpoint's own chain — its `.secure()` guard above all — decides
      // admission before the first frame. Calling the stream handler directly
      // would serve a secured route to a caller the chain refuses.
      const refusal = await refusedByEndpointChain(streamDef, ctx);
      if (refusal) {
        const projection = projectConnectError(refusal, route.responses);
        // A streaming RPC states its error in the terminal the protocol
        // reserves for it, under HTTP 200 — never as a unary error body.
        return new HttpResponse(
          endStreamFrame({
            error: {
              code: projection.connectCode,
              message: projection.message,
              ...(projection.details ? { details: projection.details } : {}),
            },
          }).buffer.slice(0) as ArrayBuffer,
          {
            headers: {
              'Content-Type': useBinaryResponse ? CT_CONNECT_STREAM_PROTO : CT_CONNECT_STREAM_JSON,
              ...connectResponseHeaders(),
            },
          },
        );
      }

      let terminated = false;
      const stream = new ReadableStream({
        async start(controller) {
          // Start deadline timer for streaming
          let deadlineTimer: ReturnType<typeof setTimeout> | undefined;
          if (deadline) {
            deadlineTimer = setTimeout(() => {
              deadline.controller.abort();
              try {
                controller.enqueue(
                  endStreamFrame({ error: { code: 'deadline_exceeded', message: 'Stream deadline exceeded' } }),
                );
                controller.close();
                terminated = true;
              } catch {
                // Already closed
              }
            }, deadline.timeoutMs);
          }

          // Build stream context matching StreamHandlerBaseContext
          const streamCtx = {
            req: ctx.req,
            headers: ctx.headers,
            url: ctx.url,
            params,
            queryParams: () => query,
            secured: ctx.secured,
            host: ctx.host,
            domain: ctx.domain,
            path: ctx.path,
            query: ctx.query,
            signal: deadline?.controller.signal,
            send: (data: unknown) => {
              try {
                let payload: Uint8Array;
                if (useBinaryResponse && responseMeta) {
                  payload = encodeProto(
                    data as Record<string, unknown>,
                    responseMeta,
                    messageMeta,
                    enums?.types,
                    enums?.values,
                  );
                } else {
                  payload = textEncoder.encode(JSON.stringify(data));
                }
                // Per-message compression: flag 0x01 = compressed
                if (useGzip) {
                  payload = compressGzip(payload);
                  controller.enqueue(createEnvelope(ENVELOPE_FLAG_COMPRESSED, payload));
                } else {
                  controller.enqueue(createEnvelope(0x00, payload));
                }
              } catch (error) {
                if (error instanceof Error && !error.message.includes('closed')) {
                  useLogger('putnami:grpc').warn('Stream write failed', { error: error.message });
                }
              }
            },
          };

          let failure: ConnectError | undefined;
          try {
            await streamDef.handler(streamCtx);
          } catch (err) {
            if (deadline?.controller.signal.aborted) {
              failure = { code: 'deadline_exceeded', message: 'Stream deadline exceeded' };
            } else {
              const projection = projectConnectError(err, route.responses);
              failure = {
                code: projection.connectCode,
                message: projection.message,
                ...(projection.details ? { details: projection.details } : {}),
              };
            }
          } finally {
            if (deadlineTimer) clearTimeout(deadlineTimer);
            // Exactly one EndStreamResponse ends the stream. The deadline timer
            // may already have written it; writing a second one would be the
            // one thing the specification forbids about this frame.
            if (!terminated) {
              try {
                controller.enqueue(endStreamFrame(failure ? { error: failure } : {}));
                controller.close();
                terminated = true;
              } catch {
                // Already closed
              }
            }
          }
        },
      });

      const streamingContentType = useBinaryResponse ? CT_CONNECT_STREAM_PROTO : CT_CONNECT_STREAM_JSON;
      const streamHeaders: Record<string, string> = {
        'Content-Type': streamingContentType,
        ...connectResponseHeaders(),
        [CONNECT_ACCEPT_ENCODING_HEADER]: 'gzip, identity',
      };
      if (useGzip) {
        streamHeaders[CONNECT_CONTENT_ENCODING_HEADER] = 'gzip';
      }

      return new HttpResponse(stream, {
        headers: streamHeaders,
      });
    } catch (err) {
      return handleGrpcError(logger, err);
    }
  };
}

// ---------------------------------------------------------------------------
// Route matching — maps RPC names back to DiscoveredRoutes
// ---------------------------------------------------------------------------

export function matchRpcToRoute(rpcName: string, routes: DiscoveredRoute[]): DiscoveredRoute | undefined {
  for (const route of routes) {
    if (buildRpcName(route) === rpcName) {
      return route;
    }
  }
  return undefined;
}

/** The RPC name the Proto emitter gives a route, so dispatch matches the served schema. */
function buildRpcName(route: DiscoveredRoute): string {
  return protoRpcName(route.method, route.path);
}

/**
 * Frame the terminal message of a response stream.
 *
 * The end-stream bit is set on this envelope and on no other, and the payload is
 * always a JSON `EndStreamResponse` — never the response codec, and never
 * gRPC-Web's `key: value` trailer block.
 */
function endStreamFrame(response: ConnectEndStreamResponse): Uint8Array {
  return createEnvelope(
    ENVELOPE_FLAG_END_STREAM,
    textEncoder.encode(JSON.stringify(serializeEndStreamResponse(response))),
  );
}
