import { HttpException, runInContext, useLogger } from '@putnami/runtime';
import type { DiscoveredRoute } from '../api';
import type { StreamEndpointDefinition } from '../api/route/stream-endpoint';
import type { HttpRequestContext } from '../http/http-context.type';
import { isRequestStop } from '../http/http-dispatcher';
import { HttpResponse } from '../http/http-response';
import type { RouteHandler } from '../http/route.type';
import type { ProtoFieldMeta } from '../proto';
import { protoRpcName } from '../proto/proto';
import { arrivedOverHttp2 } from '../http/http-version';
import { validateSchema } from '../api/route/validate';
import { ConnectRequestStream } from './connect-request-stream';
import { createEnvelope, encodeProto, type ProtoEnumRegistry } from './proto-codec';
import { decodeProto } from './proto-decode';
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
const textDecoder = new TextDecoder();

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
      // The HTTP request ended, at its timeout or because its client left: the
      // request answers 408 and the stop of its work logs no error.
      if (isRequestStop(err, ctx)) throw err;
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
 * Connect over HTTP/1.1 cannot carry a client stream, so registering the RPC
 * as unary would fail with a mis-shaped request instead of a clean status.
 * {@link buildDuplexRpcHandler} answers with it for an HTTP/1.1 request; the
 * generated clients route these modes through the WebSocket stream
 * transport, and this answer names both paths for a caller that hit the
 * Connect route directly.
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
      `${mode} streaming is not implemented over Connect/HTTP1.1; use HTTP/2 or the WebSocket stream transport for this RPC`,
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
// Client and bidirectional streaming RPC handler
// ---------------------------------------------------------------------------

/**
 * Build an HTTP handler for a client- or bidirectional-streaming Connect RPC.
 *
 * Over HTTP/2 the request body is a sequence of envelope frames read as they
 * arrive, and the response is a sequence of envelope frames written as the
 * handler produces them, so a bidirectional stream answers while the caller
 * still sends. The first request message carries the `params` and `query`
 * sections, and the `body` section of every message is one stream message.
 * A client stream answers its returned value as its one message; a
 * bidirectional stream answers each value it sends, then its returned value
 * when it has one. Exactly one end-of-stream message ends the response.
 *
 * Over HTTP/1.1, which cannot carry a client stream, the RPC answers
 * UNIMPLEMENTED as {@link buildUnimplementedRpcHandler} does.
 */
export function buildDuplexRpcHandler(
  streamDef: StreamEndpointDefinition,
  route: DiscoveredRoute,
  messageMeta: Record<string, ProtoFieldMeta[]>,
  enums?: ProtoEnumRegistry,
  compressionEnabled = false,
): (ctx: HttpRequestContext) => Promise<HttpResponse> {
  const rpcName = buildRpcName(route);
  const requestMeta = messageMeta[`${rpcName}Request`];
  const responseMeta = messageMeta[`${rpcName}Response`];
  const unimplemented = buildUnimplementedRpcHandler(streamDef.mode);

  return async (ctx: HttpRequestContext) => {
    if (!arrivedOverHttp2(ctx.req)) return unimplemented(ctx);

    const contentType = ctx.headers.get('content-type') ?? CT_CONNECT_STREAM_JSON;
    const streamMediaType = parseMediaType(contentType);
    const useBinaryRequest = streamMediaType === CT_CONNECT_STREAM_PROTO;
    const acceptHeader = ctx.headers.get('accept') ?? contentType;
    const useBinaryResponse = acceptContains(acceptHeader, CT_PROTO, CT_CONNECT_STREAM_PROTO);
    // A stream compresses per message, and only for a caller that names gzip
    // in Connect-Accept-Encoding: Accept-Encoding speaks for the whole body,
    // which a stream never compresses.
    const useGzip = compressionEnabled && /\bgzip\b/.test(ctx.headers.get(CONNECT_ACCEPT_ENCODING_HEADER) ?? '');
    const responseHeaders: Record<string, string> = {
      'Content-Type': useBinaryResponse ? CT_CONNECT_STREAM_PROTO : CT_CONNECT_STREAM_JSON,
      ...connectResponseHeaders(),
      [CONNECT_ACCEPT_ENCODING_HEADER]: 'gzip, identity',
    };
    if (useGzip) responseHeaders[CONNECT_CONTENT_ENCODING_HEADER] = 'gzip';

    // The endpoint's own chain decides admission before any message is read.
    const refusal = await refusedByEndpointChain(streamDef, ctx);
    if (refusal) {
      const projection = projectConnectError(refusal, route.responses);
      return new HttpResponse(
        endStreamFrame({
          error: {
            code: projection.connectCode,
            message: projection.message,
            ...(projection.details ? { details: projection.details } : {}),
          },
        }).buffer.slice(0) as ArrayBuffer,
        { headers: responseHeaders },
      );
    }

    const requests = new ConnectRequestStream(ctx.req.body, ctx.headers.get(CONNECT_CONTENT_ENCODING_HEADER));
    const decodeMessage = (payload: Uint8Array): Record<string, unknown> => {
      if (useBinaryRequest && requestMeta) {
        return decodeProto(payload, requestMeta, messageMeta, enums?.types, enums?.values);
      }
      if (payload.length === 0) return {};
      let parsed: unknown;
      try {
        parsed = JSON.parse(textDecoder.decode(payload));
      } catch (error) {
        if (!(error instanceof SyntaxError)) throw error;
        throw new HttpException('Bad Request: a Connect stream message is not valid JSON', 400);
      }
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        throw new HttpException('Bad Request: a Connect stream message is not an object', 400);
      }
      return parsed as Record<string, unknown>;
    };
    const streamMessage = (message: Record<string, unknown>): { present: boolean; value?: unknown } => {
      if (message['body'] === undefined || message['body'] === null) return { present: false };
      const { body } = readRequestEnvelope({ body: message['body'] });
      return {
        present: true,
        value: streamDef.schemas?.body ? validateSchema(streamDef.schemas.body, body, { label: 'message' }) : body,
      };
    };
    const encodeMessage = (value: unknown): Uint8Array => {
      const checked = streamDef.schemas?.returns
        ? validateSchema(streamDef.schemas.returns, value, { label: 'message' })
        : value;
      let payload =
        useBinaryResponse && responseMeta
          ? encodeProto(checked as Record<string, unknown>, responseMeta, messageMeta, enums?.types, enums?.values)
          : textEncoder.encode(JSON.stringify(checked));
      if (!useGzip) return createEnvelope(0x00, payload);
      payload = compressGzip(payload);
      return createEnvelope(ENVELOPE_FLAG_COMPRESSED, payload);
    };

    const deadline = parseGrpcTimeout(ctx.headers);
    const abort = deadline?.controller ?? new AbortController();
    const onClientGone = () => abort.abort();
    ctx.req.signal.addEventListener('abort', onClientGone, { once: true });

    let terminated = false;
    const stream = new ReadableStream<Uint8Array>({
      async start(controller) {
        const terminate = (failure?: ConnectError) => {
          if (terminated) return;
          terminated = true;
          requests.cancel();
          try {
            controller.enqueue(endStreamFrame(failure ? { error: failure } : {}));
            controller.close();
          } catch {
            // The caller already left.
          }
        };
        const deadlineTimer = deadline
          ? setTimeout(() => {
              deadline.controller.abort();
              terminate({ code: 'deadline_exceeded', message: 'Stream deadline exceeded' });
            }, deadline.timeoutMs)
          : undefined;

        let failure: ConnectError | undefined;
        try {
          const first = await requests.next();
          const opening = first === undefined ? {} : decodeMessage(first);
          const { params, query } = readRequestEnvelope(opening);
          const firstMessage = first === undefined ? { present: false } : streamMessage(opening);

          async function* messages(): AsyncGenerator<unknown> {
            if (firstMessage.present) yield firstMessage.value;
            for (;;) {
              if (terminated) return;
              // biome-ignore lint/performance/noAwaitInLoops: the caller's messages are delivered in order
              const payload = await requests.next();
              if (payload === undefined) return;
              const next = streamMessage(decodeMessage(payload));
              if (next.present) yield next.value;
            }
          }
          const inbound = messages();

          const send = (value: unknown) => {
            if (terminated) return;
            controller.enqueue(encodeMessage(value));
          };
          const result = await streamDef.handler({
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
            signal: abort.signal,
            messages: () => inbound,
            send,
          });
          // A client stream answers one message, its result. A bidirectional
          // stream already answered as it went; a returned value is its last.
          if (result !== undefined || streamDef.mode === 'client') send(result);
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
          ctx.req.signal.removeEventListener('abort', onClientGone);
          terminate(failure);
        }
      },
      cancel() {
        terminated = true;
        requests.cancel();
        abort.abort();
      },
    });

    return new HttpResponse(stream, { headers: responseHeaders });
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
