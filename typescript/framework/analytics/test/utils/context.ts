import type { HttpRequestContext } from '@putnami/application';

/** What a synthetic request context varies. */
interface FakeContextInit {
  method?: string;
  url?: string;
  headers?: Record<string, string>;
  body?: string;
  route?: string;
  user?: { sub?: string };
  secure?: boolean;
}

/**
 * Builds a request context without a socket.
 *
 * Used where a live server would prove the wrong thing: a body the handler
 * refuses without reading leaves a real upload half-sent, so the proof would
 * be measuring connection draining rather than the rejection itself.
 *
 * @param init - The request to synthesize.
 * @returns A context carrying everything the ingest path reads.
 */
export function fakeContext(init: FakeContextInit = {}): HttpRequestContext {
  const url = init.url ?? 'http://localhost/_putnami/analytics/events';
  const headers = new Headers(init.headers ?? {});
  const request = new Request(url, {
    method: init.method ?? 'POST',
    headers,
    ...(init.body === undefined ? {} : { body: init.body }),
  });
  const parsed = new URL(url);
  return {
    url,
    method: init.method ?? 'POST',
    headers,
    req: request,
    route: init.route,
    user: init.user,
    secured: () => init.secure ?? false,
    host: () => parsed.host,
    path: () => parsed.pathname,
    query: () => parsed.search,
    queryParams: () => Object.fromEntries(parsed.searchParams),
  } as unknown as HttpRequestContext;
}
