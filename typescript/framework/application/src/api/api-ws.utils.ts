import { BadRequestException } from '@putnami/runtime';
import type { HttpPlugin } from '../http/http.plugin';
import type { HttpRequestContext, HttpRequestContextInternal } from '../http/http-context.type';
import type { HttpMethod } from '../http/http-method.type';
import { HttpResponse } from '../http/http-response';
import type { WebSocketContext } from './ws-context.type';

/** WebSocket lifecycle methods */
export const WS_METHODS: HttpMethod[] = ['MESSAGE', 'CLOSE', 'OPEN'];

/**
 * Upgrade an HTTP request to a WebSocket connection.
 */
export function upgradeToWebSocket(context: HttpRequestContext, options?: { subprotocol?: string }): HttpResponse {
  const headers = options?.subprotocol ? { 'Sec-WebSocket-Protocol': options.subprotocol } : undefined;
  const upgraded = context.server?.upgrade(context.req, {
    ...(headers ? { headers } : {}),
    data: {
      __identityResolvers: (context as HttpRequestContextInternal).__identityResolvers,
      __httpContext: context,
      context: {
        req: context.req,
        route: context.route,
        params: context.params,
        queryParams: context.queryParams,
        domain: context.domain,
        host: context.host,
        path: context.path,
        query: context.query,
        secured: context.secured,
        user: context.user,
      } as Omit<WebSocketContext, 'ws'>,
    },
  });

  if (!upgraded) {
    throw new BadRequestException('Upgrade failed');
  }

  return HttpResponse.json({ message: 'upgraded' });
}

/**
 * Register WS lifecycle event handlers (OPEN, MESSAGE, CLOSE) for a route.
 */
// biome-ignore lint/suspicious/noExplicitAny: WS handlers have different signatures than RouteHandler
export function registerWsHandlers(httpPlugin: HttpPlugin, route: string, handlers: any): void {
  for (const m of WS_METHODS) {
    if (typeof handlers[m] === 'function') {
      httpPlugin.route(m, route, handlers[m]);
    }
  }
}
