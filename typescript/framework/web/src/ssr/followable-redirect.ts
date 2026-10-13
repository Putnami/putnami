import { HttpResponse, type RouteHandler, type RouteHandlerResult } from '@putnami/application';
import { CLIENT_REDIRECT_HEADER, CLIENT_REDIRECT_STATUS_HEADER } from '../shared/client-redirect';

/**
 * Answers a redirect with a response the client can follow, when the request
 * comes from the client action or the client loader.
 *
 * The client reaches an action or a loader with `fetch`, which follows a
 * redirect itself and receives the target page's HTML instead of data. For a
 * request that carries `requestHeader`, a returned or thrown redirect becomes a
 * 204 that carries the location in {@link CLIENT_REDIRECT_HEADER}; its other
 * headers, such as a session cookie, stay. A request without the header, such
 * as a plain HTML form POST, receives the redirect unchanged.
 *
 * It wraps the route middleware too, so a redirect a route middleware answers
 * reaches the client the same way.
 *
 * @param handler - The route handler, middleware included.
 * @param requestHeader - The header the client sets on its own requests.
 * @returns The same handler, with redirects made followable by the client.
 */
export function followableRedirects(handler: RouteHandler, requestHeader: string): RouteHandler {
  return async (ctx) => {
    if (!ctx.headers.get(requestHeader)) {
      return handler(ctx);
    }
    let result: RouteHandlerResult;
    try {
      result = await handler(ctx);
    } catch (error) {
      const followable = toFollowable(error);
      if (followable) {
        return followable;
      }
      throw error;
    }
    return toFollowable(result) ?? result;
  };
}

/**
 * Turns a redirect into a 204 that carries its location and status in headers.
 *
 * @param value - What the handler returned or threw.
 * @returns The followable response, or `undefined` when `value` is no redirect.
 */
function toFollowable(value: unknown): HttpResponse | undefined {
  if (!(value instanceof HttpResponse) || !value.redirected) {
    return undefined;
  }
  return new HttpResponse(undefined, {
    status: 204,
    headers: [
      ...value.getHeaderEntries().filter(([name]) => name !== 'Location'),
      [CLIENT_REDIRECT_HEADER, value.getHeader('Location') as string],
      [CLIENT_REDIRECT_STATUS_HEADER, String(value.status)],
    ],
  });
}
