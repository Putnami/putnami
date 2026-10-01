import type { ServerWebSocket } from 'bun';
import { useLogger } from '@putnami/runtime';
import type { WsCloseContext, WsDataType, WsMessageContext, WsOpenContext } from '../ws-context.type';
import { validateSchema } from '../route/validate';
import type { StreamEndpointDefinition } from '../route/stream-endpoint';
import { MessageStream } from './message-stream';

// ---------------------------------------------------------------------------
// resolveStreamHandlers — convert a StreamEndpointDefinition into WS handlers
// ---------------------------------------------------------------------------

/**
 * Bridge a `StreamEndpointDefinition` (single handler) to the WebSocket
 * lifecycle `{ OPEN, MESSAGE, CLOSE }` handlers used by `WsDispatcher`.
 */
export function resolveStreamHandlers(def: StreamEndpointDefinition): {
  OPEN?: (ctx: WsOpenContext) => void | Promise<void>;
  MESSAGE?: (ctx: WsMessageContext) => void | Promise<void>;
  CLOSE?: (ctx: WsCloseContext) => void | Promise<void>;
} {
  return {
    OPEN: (ctx: WsOpenContext) => {
      const ws = ctx.ws as ServerWebSocket<WsStreamData>;

      // Validate params & query
      validateContext(ctx, def.schemas);

      // Create message stream for body(Stream(...)) modes
      const messageStream = new MessageStream<unknown>();
      ws.data.__messageStream = messageStream;

      // Aborted on CLOSE so long-running handlers learn of client disconnect.
      const abortController = new AbortController();
      ws.data.__abort = abortController;

      // Build the stream handler context
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
        // Available when body is Stream()
        messages: () => messageStream,
        // Available when returns is Stream()
        send: (data: unknown) => {
          ws.send(JSON.stringify(data));
        },
      };

      // Start the handler — don't await it.
      // It runs for the connection lifetime (blocks on for-await).
      const result = def.handler(streamCtx);

      // When handler completes (returns or throws), close the connection
      if (result && typeof result.then === 'function') {
        result.then(
          (returnValue) => {
            // Client-stream: handler returns a final value
            if (returnValue !== undefined && def.mode === 'client') {
              ws.send(JSON.stringify(returnValue));
            }
            ws.close();
          },
          (err) => {
            // Log the originating error server-side (cause preserved) before
            // closing; the client reason stays generic so internals don't leak.
            useLogger('ws').error('stream handler error', toError(err), { path: ctx.path() });
            ws.close(1011, 'handler error');
          },
        );
      }
    },

    MESSAGE: (ctx: WsMessageContext) => {
      const ws = ctx.ws as ServerWebSocket<WsStreamData>;
      const messageStream = ws.data.__messageStream;
      if (!messageStream) return;

      // Parse & validate message against body schema
      let message: unknown = ctx.message;
      if (def.schemas?.body) {
        const raw = typeof ctx.message === 'string' ? parseJson(ctx.message) : ctx.message;
        message = validateSchema(def.schemas.body, raw, { label: 'message' });
      }

      messageStream.push(message);
    },

    CLOSE: (ctx: WsCloseContext) => {
      const ws = ctx.ws as ServerWebSocket<WsStreamData>;
      // Signal disconnect first so an awaiting handler unblocks, then end the stream.
      ws.data.__abort?.abort();
      const messageStream = ws.data.__messageStream;
      if (messageStream) {
        // Make the inbound DoS guard observable: if the buffer bound was hit and
        // frames were dropped, surface it once per connection (Putnami principle:
        // observable by construction) rather than dropping entirely silently.
        if (messageStream.droppedCount > 0) {
          useLogger('ws').warn('stream inbound buffer bound reached; dropped frames', {
            dropped: messageStream.droppedCount,
            path: ctx.path(),
          });
        }
        messageStream.close();
      }
    },
  };
}

/** Per-connection state stashed on the Bun socket for stream endpoints. */
type WsStreamData = WsDataType & {
  __messageStream?: MessageStream<unknown>;
  __abort?: AbortController;
};

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

function validateContext(ctx: WsOpenContext, schemas?: StreamEndpointDefinition['schemas']): void {
  if (schemas?.params) {
    ctx.params = validateSchema(schemas.params, ctx.params ?? {}, {
      coerce: true,
      label: 'params',
    }) as Record<string, string>;
  }
  if (schemas?.query) {
    const rawQuery = ctx.queryParams();
    const validatedQuery = validateSchema(schemas.query, rawQuery, {
      coerce: true,
      label: 'query',
    });
    (ctx as { queryParams: () => Record<string, string> }).queryParams = () => validatedQuery as Record<string, string>;
  }
}

function parseJson(value: string): unknown {
  try {
    return JSON.parse(value);
  } catch {
    return value;
  }
}

/** Normalize a thrown value to an Error, preserving a non-Error reason as the cause chain. */
function toError(value: unknown): Error {
  return value instanceof Error ? value : new Error(String(value), { cause: value });
}
