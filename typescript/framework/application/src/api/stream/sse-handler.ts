import { BadRequestException, useLogger } from '@putnami/runtime';
import type { HttpRequestContext, HttpRequestContextInternal } from '../../http/http-context.type';
import { HttpResponse } from '../../http/http-response';
import type { RouteHandlerResult } from '../../http/route.type';
import type { StreamEndpointDefinition } from '../route/stream-endpoint';
import { validateSchema } from '../route/validate';
import { SSE_COMPLETE_FRAME, SSE_WIRE_HEADER, SSE_WIRE_V1, negotiatesSseWire } from './sse-wire';
import { projectStreamError } from './stream-error';

/** Comment frame the provider writes to prove the stream is still alive. */
const HEARTBEAT_FRAME = ': heartbeat\n\n';

/**
 * Default interval between two liveness comments. It is well under the idle
 * budget a first-party client declares, so a stream that produces nothing for a
 * while is not mistaken for a dead one.
 */
const DEFAULT_HEARTBEAT_MS = 15_000;

/** Default largest single SSE frame the provider will write. */
const DEFAULT_MAX_FRAME_BYTES = 1 << 20;

/**
 * Create an HTTP handler that serves a server-stream endpoint via SSE.
 *
 * The handler writes `text/event-stream` responses using a `ReadableStream`.
 * Each call to `ctx.send(data)` in the user's handler emits an SSE `data:` frame.
 *
 * Everything that decides whether the provider accepts the operation happens
 * before the first byte of the body: a request that carries a body and a
 * request whose params or query do not validate are refused with a non-2xx
 * status, never with a 200 followed by a terminal event. Past that point the
 * three bounds apply — queue depth, frame size, and the liveness heartbeat.
 *
 * A route that declares a continuation (`negotiatesWire`) also speaks the
 * negotiated wire of ADR 0013 of protocols/clientcontract: a request that asks
 * for it is acknowledged on the response head, and its stream ends with the
 * `complete` terminal when the handler returns, every message was queued and
 * the consumer did not cancel. A failure ends it with the typed error, a
 * cancellation with nothing. Every other request keeps the legacy framing.
 */
export function createSseHandler(
  def: StreamEndpointDefinition,
  options: {
    readonly maxBufferedMessages?: number;
    readonly maxFrameBytes?: number;
    readonly heartbeatMs?: number;
    readonly negotiatesWire?: boolean;
  } = {},
): (ctx: HttpRequestContext) => RouteHandlerResult | Promise<RouteHandlerResult> {
  return (ctx: HttpRequestContext) => {
    // A server stream is a GET: a body has no declared meaning here, so
    // accepting one would silently discard what the caller sent.
    if (hasRequestBody(ctx)) {
      throw new BadRequestException('server stream requests must not carry a body');
    }

    // Validate params & query
    if (def.schemas?.params) {
      ctx.params = validateSchema(def.schemas.params, ctx.params ?? {}, {
        coerce: true,
        label: 'params',
      }) as Record<string, string>;
    }
    if (def.schemas?.query) {
      const rawQuery = ctx.queryParams();
      const validatedQuery = validateSchema(def.schemas.query, rawQuery, {
        coerce: true,
        label: 'query',
      });
      ctx.queryParams = () => validatedQuery as Record<string, string>;
    }

    // Admission has run: the endpoint's middleware before this handler, and
    // the params and query above. A negotiating request is acknowledged on the
    // response head built below, before the first body byte.
    const negotiated =
      options.negotiatesWire === true && negotiatesSseWire((ctx.req?.headers ?? ctx.headers)?.get(SSE_WIRE_HEADER));
    const encoder = new TextEncoder();

    // Aborted when the client disconnects (ReadableStream.cancel) so the
    // handler can stop instead of looping forever and leaking timers/cursors/CPU.
    const abortController = new AbortController();
    let closed = false;
    // A draining provider (a graceful stop, a scale to zero, an instance
    // replacement) is the instance change a continuation exists for: it stops
    // the handler of a negotiated stream and ends the response with no
    // terminal, so the consumer reads an interruption and continues on another
    // instance. A legacy stream keeps its framing. The drain is the serving
    // HTTP plugin's, stamped on the request: the one run of the one instance
    // that admitted this stream.
    const drain = negotiated ? (ctx as HttpRequestContextInternal).__drain : undefined;
    const stopOnDrain = (): void => abortController.abort(drain?.reason);
    const stopped = (): boolean => abortController.signal.aborted || drain?.aborted === true;

    const maxBufferedMessages = options.maxBufferedMessages ?? 64;
    const maxFrameBytes = options.maxFrameBytes ?? DEFAULT_MAX_FRAME_BYTES;
    const heartbeatMs = options.heartbeatMs ?? DEFAULT_HEARTBEAT_MS;
    const stream = new ReadableStream(
      {
        async start(controller) {
          // A message the provider could not queue never left, so a negotiated
          // stream that dropped one never ends with the successful terminal.
          let sendFailed = false;
          const write = (data: unknown): void => {
            if (closed) return;
            const desiredSize = controller.desiredSize;
            if (desiredSize === null) {
              sendFailed = true;
              return;
            }
            // Reserve one queue slot for the terminal event. A synchronous
            // producer cannot outrun a slow client and accumulate unbounded data:
            // the handler fails explicitly as soon as its declared queue is full.
            if (desiredSize <= 1) {
              throw new Error('SSE stream producer exceeded maxBufferedMessages');
            }
            const output = def.schemas?.returns
              ? validateSchema(def.schemas.returns, data, { label: 'message' })
              : data;
            const frame = encoder.encode(`data: ${JSON.stringify(output)}\n\n`);
            // A frame past the declared bound is one the first-party client
            // contract requires the consumer to reject. Failing here names
            // the producer instead of surfacing as a client-side decode
            // error on the other end of the wire.
            if (frame.byteLength > maxFrameBytes) {
              throw new Error('SSE stream producer exceeded maxFrameBytes');
            }
            try {
              controller.enqueue(frame);
            } catch {
              // Stream already closed: the message never left.
              sendFailed = true;
            }
          };
          const streamCtx = {
            req: ctx.req,
            headers: ctx.headers,
            url: ctx.url,
            params: ctx.params,
            queryParams: ctx.queryParams,
            secured: ctx.secured,
            host: ctx.host,
            domain: ctx.domain,
            path: ctx.path,
            query: ctx.query,
            signal: abortController.signal,
            send: (data: unknown) => {
              try {
                write(data);
              } catch (error) {
                sendFailed = true;
                throw error;
              }
            },
          };

          // Liveness comments carry no data, so a consumer parses them away and
          // only its idle budget observes them. One is skipped rather than
          // queued when the consumer is already behind: a full queue is itself
          // evidence the stream is alive, and the terminal slot stays reserved.
          const heartbeat =
            heartbeatMs > 0
              ? setInterval(() => {
                  if (closed || (controller.desiredSize ?? 0) <= 1) return;
                  try {
                    controller.enqueue(encoder.encode(HEARTBEAT_FRAME));
                  } catch {
                    // Stream already closed — ignore
                  }
                }, heartbeatMs)
              : undefined;
          // A heartbeat must never be the reason a process stays up.
          (heartbeat as { unref?: () => void } | undefined)?.unref?.();

          drain?.addEventListener('abort', stopOnDrain, { once: true });
          if (drain?.aborted) stopOnDrain();
          let failed = false;
          try {
            await def.handler(streamCtx);
          } catch (err) {
            failed = true;
            // A handler throw rejects the stream silently otherwise — log it
            // server-side (cause preserved) so production stream failures are
            // diagnosable. The client just sees the stream end.
            useLogger('http').error('SSE stream handler error', toError(err), { path: ctx.path() });
            // A negotiated stream the consumer canceled gets no terminal at all.
            if (!(negotiated && stopped())) {
              try {
                controller.enqueue(
                  encoder.encode(`event: error\ndata: ${JSON.stringify(projectStreamError(err, def))}\n\n`),
                );
              } catch {
                // Client disconnected while the typed terminal event was written.
              }
            }
          } finally {
            if (heartbeat !== undefined) clearInterval(heartbeat);
            drain?.removeEventListener('abort', stopOnDrain);
            // The successful terminal of the negotiated wire takes the slot
            // reserved for the terminal: the handler returned, every message was
            // queued and the consumer is still there. Never after a failure.
            if (negotiated && !failed && !sendFailed && !stopped()) {
              try {
                controller.enqueue(encoder.encode(SSE_COMPLETE_FRAME));
              } catch {
                // Client disconnected while the terminal was written.
              }
            }
            try {
              controller.close();
            } catch {
              // Already closed
            } finally {
              closed = true;
            }
          }
        },
        cancel(reason) {
          // Client disconnected — signal the handler so it can stop producing.
          abortController.abort(reason);
        },
      },
      { highWaterMark: maxBufferedMessages + 1, size: () => 1 },
    );

    return new HttpResponse(stream, {
      headers: {
        'Content-Type': 'text/event-stream',
        'Cache-Control': 'no-cache',
        Connection: 'keep-alive',
        ...(negotiated ? { [SSE_WIRE_HEADER]: SSE_WIRE_V1 } : {}),
      },
    });
  };
}

/**
 * Report whether the incoming request carries a body. `req.body` is the
 * authority; the framing headers are read too because a request reconstructed
 * without a stream still states its framing there.
 */
function hasRequestBody(ctx: HttpRequestContext): boolean {
  if (ctx.req?.body) return true;
  const headers = ctx.req?.headers ?? ctx.headers;
  if (!headers) return false;
  if (headers.get('transfer-encoding')) return true;
  const length = headers.get('content-length');
  return length !== null && Number(length) > 0;
}

/** Normalize a thrown value to an Error, preserving a non-Error reason as the cause chain. */
function toError(value: unknown): Error {
  return value instanceof Error ? value : new Error(String(value), { cause: value });
}
