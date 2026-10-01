import type { HttpRequestContext } from './http-context.type';
import type { HttpResponse } from './http-response';

/** An async function that intercepts HTTP requests, optionally delegating to the next middleware in the chain. */
export type HttpMiddleware = (
  context: HttpRequestContext,
  next: () => Promise<HttpResponse | undefined>,
) => Promise<HttpResponse | undefined>;
