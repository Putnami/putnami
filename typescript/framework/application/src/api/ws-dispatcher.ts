import type { ServerWebSocket, WebSocketHandler } from 'bun';
import { HttpException, runInContext, useLogger } from '@putnami/runtime';
import { parseQueryString } from '@putnami/utils';
import type { HttpMethod } from '../http/http-method.type';
import type { RouteController } from '../http/route.controller';
import { UrlScanner } from '../http/url.scanner';
import type {
  WebSocketContext,
  WsCloseContext,
  WsContextBase,
  WsDataType,
  WsMessageContext,
  WsOpenContext,
} from './ws-context.type';

/** WebSocket close code for a policy violation (RFC 6455 §7.4.1) — used for bad frames. */
const WS_CLOSE_POLICY_VIOLATION = 1008;
/** WebSocket close code for an unexpected server error (RFC 6455 §7.4.1). */
const WS_CLOSE_INTERNAL_ERROR = 1011;

export class WebSocketDispatcher implements WebSocketHandler<WsDataType> {
  constructor(private router: RouteController<WebSocketContext>) {}

  async message(ws: ServerWebSocket<WsDataType>, message: string | Buffer): Promise<void> {
    const context = buildWsContext<WsMessageContext>(ws, {
      ws,
      message,
      method: 'MESSAGE',
    });

    const handler = this.router.findFirst<WsMessageContext>(context);
    if (!handler) return;

    try {
      await runInContext(context, () => handler.handler(context));
    } catch (err) {
      // Without this, a frame that fails schema validation throws into Bun's
      // websocket.message wiring as an unhandled rejection (process crash under
      // --unhandled-rejections=strict) with no client-facing error.
      this.handleDispatchError(ws, 'MESSAGE', err);
    }
  }

  async open(ws: ServerWebSocket<WsDataType>): Promise<void> {
    const context = buildWsContext<WsOpenContext>(ws, { ws, method: 'OPEN' });

    const handler = this.router.findFirst<WsOpenContext>(context);
    if (!handler) return;

    try {
      await runInContext(context, () => handler.handler(context));
    } catch (err) {
      this.handleDispatchError(ws, 'OPEN', err);
    }
  }

  async close(ws: ServerWebSocket<WsDataType>, code: number, reason: string): Promise<void> {
    const context = buildWsContext<WsCloseContext>(ws, {
      ws,
      method: 'CLOSE',
      code,
      reason,
    });

    const handler = this.router.findFirst<WsCloseContext>(context);
    if (!handler) return;

    try {
      await runInContext(context, () => handler.handler(context));
    } catch (err) {
      // The socket is already closing — just log; don't try to send/close again.
      useLogger('ws').error('WebSocket CLOSE handler error', err);
    }
  }

  /**
   * Reports a dispatch error to the client and the structured logger.
   *
   * Developer-authored {@link HttpException}s (e.g. a `BadRequestException` from
   * frame validation) surface their status/message in a structured error frame;
   * unexpected errors send a generic message (no internal detail leak) and close
   * the connection. Send/close are best-effort — the socket may already be gone.
   */
  private handleDispatchError(ws: ServerWebSocket<WsDataType>, phase: string, err: unknown): void {
    const logger = useLogger('ws');
    if (err instanceof HttpException) {
      logger.warn(`WebSocket ${phase} handler rejected a frame`, {
        status: err.getStatus(),
        message: err.message,
      });
      trySend(ws, { error: { code: err.getStatus(), message: err.message } });
      if (phase === 'OPEN') tryClose(ws, WS_CLOSE_POLICY_VIOLATION, 'invalid connection');
    } else {
      logger.error(`WebSocket ${phase} handler error`, err);
      trySend(ws, { error: { code: 500, message: 'Internal error' } });
      tryClose(ws, WS_CLOSE_INTERNAL_ERROR, 'handler error');
    }
  }
}

/** Best-effort structured error frame; ignores failures when the socket is closed. */
function trySend(ws: ServerWebSocket<WsDataType>, payload: unknown): void {
  try {
    ws.send(JSON.stringify(payload));
  } catch {
    // Socket already closed — nothing to do.
  }
}

/** Best-effort close; ignores failures when the socket is already closed. */
function tryClose(ws: ServerWebSocket<WsDataType>, code: number, reason: string): void {
  try {
    ws.close(code, reason);
  } catch {
    // Socket already closed — nothing to do.
  }
}

export function buildWsContext<C extends WebSocketContext>(ws: ServerWebSocket<WsDataType>, extra: Partial<C>): C {
  // The URL/headers/host/path are fixed for the connection's lifetime, so the
  // UrlScanner + accessor closures are computed once and cached on ws.data,
  // then reused by every frame — only the per-frame fields in `extra`
  // (message/code/reason/method) are merged on top.
  ws.data.__wsBase ??= buildWsContextBase(ws);
  return { ...ws.data.__wsBase, ...extra } as C;
}

/** Compute the connection-invariant context fields for a socket (see {@link buildWsContext}). */
function buildWsContextBase(ws: ServerWebSocket<WsDataType>): WsContextBase {
  const req = ws.data.context.req;
  const urlScanner = new UrlScanner(req.url, req.headers);
  return {
    req,
    headers: req.headers,
    method: req.method as HttpMethod,
    url: req.url,
    queryParams: () => parseQueryString(urlScanner.query),
    secured: () => urlScanner.secured,
    host: () => urlScanner.host,
    domain: () => urlScanner.domain,
    path: () => urlScanner.path,
    query: () => urlScanner.query,
  };
}
