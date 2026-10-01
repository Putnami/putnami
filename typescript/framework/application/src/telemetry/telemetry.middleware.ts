/**
 * Telemetry Middleware — Automatic HTTP Metrics Collection
 *
 * Collects per-route metrics for every request:
 *   - Counter: `http.{METHOD}.{route}.{status}`  — request count
 *   - Counter: `http.error.{statusClass}`         — 4xx / 5xx totals
 *   - Histogram: `http.{METHOD}.{route}.duration` — response time (ms)
 */

import { HttpException } from '@putnami/runtime';
import type { HttpMiddleware } from '../http/http-middleware.type';
import { attachTelemetryContext, startTelemetrySpan } from './client-telemetry';
import type { TelemetryCollector } from './telemetry.collector';

/** Creates an HTTP middleware that records per-route request count, duration, and error metrics. */
export const TelemetryMiddleware = (collector: TelemetryCollector): HttpMiddleware => {
  return async (ctx, next) => {
    const start = Date.now();
    const method = ctx.method;
    const route = normalizedRoute(ctx.route);
    const span = startTelemetrySpan({
      name: `${method} ${route}`,
      kind: 'server',
      remoteHeaders: ctx.req.headers,
      attributes: {
        'http.request.method': method,
        'http.route': route,
      },
    });
    attachTelemetryContext(ctx, span);

    let status = 200;
    try {
      const res = await next();
      status = res?.status ?? 404;
      return res;
    } catch (error) {
      if (error instanceof HttpException) {
        status = error.getStatus();
      } else {
        status = 500;
      }
      throw error;
    } finally {
      const duration = Date.now() - start;
      span.finish({
        attributes: { 'http.response.status_code': status },
        ...(status >= 400 ? { code: `http.${status}` } : {}),
      });

      // Counter: per-route request count
      collector.incCounter(`http.${method}.${route}.${status}`);

      // Histogram: per-route duration
      collector.observeHistogram(`http.${method}.${route}.duration`, duration);

      // Counter: error class (4xx / 5xx)
      if (status >= 400 && status < 500) {
        collector.incCounter('http.error.4xx');
      } else if (status >= 500) {
        collector.incCounter('http.error.5xx');
      }
    }
  };
};

function normalizedRoute(input: string | undefined): string {
  // Never use the raw URL: route patterns are provider-owned and bounded,
  // while unmatched paths are attacker-controlled cardinality.
  const route = input || '__unmatched__';
  return route.startsWith('/') ? route : `/${route}`;
}
