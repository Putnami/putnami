import type { ServerWebSocket } from 'bun';
import type { HttpRequestContext } from '../http/http-context.type';
import type { HttpMiddleware } from '../http/http-middleware.type';

export type WsDataType = {
  context: WsMessageContext;
  /** Original upgrade context used to rebuild first-frame authentication input. */
  __httpContext?: HttpRequestContext;
  /** Identity resolvers captured while the upgrade request traversed HTTP middleware. */
  __identityResolvers?: readonly HttpMiddleware[];
  /**
   * Connection-invariant WebSocket context fields (req/headers/url + the
   * url-scanner accessors), computed once in `open()` and reused by every
   * inbound frame so we don't re-scan the URL and re-wrap closures per message.
   */
  __wsBase?: WsContextBase;
};

/**
 * The slice of {@link WebSocketContext} that is fixed for a connection's
 * lifetime (everything except `ws` and the per-frame `message`/`code`/`reason`).
 */
export type WsContextBase = Pick<
  HttpRequestContext,
  'req' | 'headers' | 'method' | 'url' | 'queryParams' | 'secured' | 'host' | 'domain' | 'path' | 'query'
>;

/**
 * WSDT is intersected with WsDataType (`WsDataType & WSDT`); a `Record<string, never>` default adds an
 * index signature that conflicts with WsDataType's own properties and breaks every generated client (see
 * @putnami/client provider-wire.test.ts). `{}` is the only default that intersects cleanly and still means
 * "no extra data".
 */
// biome-ignore lint/complexity/noBannedTypes: see the comment above
export type WebSocketContext<WSDT = {}> = {
  ws: ServerWebSocket<WsDataType & WSDT>;
} & HttpRequestContext;

// biome-ignore lint/complexity/noBannedTypes: see WebSocketContext above
export type WsMessageContext<T = string | Buffer, WSDT = {}> = WebSocketContext<WSDT> & {
  message: T;
};

// biome-ignore lint/complexity/noBannedTypes: see WebSocketContext above
export type WsOpenContext<WSDT = {}> = WebSocketContext<WSDT>;

// biome-ignore lint/complexity/noBannedTypes: see WebSocketContext above
export type WsCloseContext<WSDT = {}> = WebSocketContext<WSDT> & {
  code: number;
  reason: string;
};
