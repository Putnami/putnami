/**
 * HTTP server metric conventions — the telemetry protocol's names, unit,
 * descriptions and label keys, which the Go runtime records too. The protocol
 * commits them in protocols/telemetry/fixtures/conventions/http-server-metrics.json,
 * and a test keeps these constants equal to that file.
 */

import type { MetricDescription } from './telemetry.collector';

/** Counter of the HTTP requests a service answers. */
export const HTTP_SERVER_REQUEST_COUNT = 'http.server.request_count';
/** Histogram of HTTP request durations, in milliseconds. */
export const HTTP_SERVER_DURATION = 'http.server.duration';

/** Label of the request's HTTP method, or `_OTHER` for a nonstandard one. */
export const ATTR_HTTP_METHOD = 'http.method';
/** Label of the matched route pattern, never the request path. */
export const ATTR_HTTP_ROUTE = 'http.route';
/** Label of the response status, an integer. */
export const ATTR_HTTP_STATUS_CODE = 'http.status_code';

/** Route label of a request no route pattern matched. */
export const UNMATCHED_ROUTE = '__unmatched__';
/** Method label of a request whose method is not a standard HTTP method. */
export const OTHER_METHOD = '_OTHER';

/** Unit and description of each HTTP server metric, as the collector renders them. */
export const HTTP_SERVER_METRIC_DESCRIPTIONS: Readonly<Record<string, MetricDescription>> = Object.freeze({
  [HTTP_SERVER_DURATION]: Object.freeze({ unit: 'ms', description: 'HTTP request duration in milliseconds' }),
  [HTTP_SERVER_REQUEST_COUNT]: Object.freeze({ description: 'Total HTTP requests' }),
});

const STANDARD_METHODS: ReadonlySet<string> = new Set([
  'CONNECT',
  'DELETE',
  'GET',
  'HEAD',
  'OPTIONS',
  'PATCH',
  'POST',
  'PUT',
  'QUERY',
  'TRACE',
]);

/** The method label of a request: the method itself when standard, `_OTHER` otherwise. */
export function httpMethodLabel(method: string): string {
  return STANDARD_METHODS.has(method) ? method : OTHER_METHOD;
}

/** The route label of a request: its route pattern, or `__unmatched__`. */
export function httpRouteLabel(route: string | undefined): string {
  // Never use the raw URL: route patterns are provider-owned and bounded,
  // while unmatched paths are attacker-controlled cardinality.
  const pattern = route || UNMATCHED_ROUTE;
  return pattern.startsWith('/') || pattern === UNMATCHED_ROUTE ? pattern : `/${pattern}`;
}
