/**
 * Telemetry Middleware — Automatic HTTP Metrics Collection
 *
 * Records every request under the telemetry protocol's HTTP server metrics,
 * the same the Go runtime records:
 *   - Counter: `http.server.request_count`
 *   - Histogram: `http.server.duration`, in ms
 * both labeled `http.method`, `http.route` (the route pattern) and
 * `http.status_code`.
 */

import { HttpException } from '@putnami/runtime';
import { requestEnded } from '../http/http-dispatcher';
import type { HttpMiddleware } from '../http/http-middleware.type';
import { attachTelemetryContext, startTelemetrySpan } from './client-telemetry';
import {
  ATTR_HTTP_METHOD,
  ATTR_HTTP_ROUTE,
  ATTR_HTTP_STATUS_CODE,
  HTTP_SERVER_DURATION,
  HTTP_SERVER_METRIC_DESCRIPTIONS,
  HTTP_SERVER_REQUEST_COUNT,
  httpMethodLabel,
  httpRouteLabel,
} from './http-server-metrics';
import type { TelemetryCollector } from './telemetry.collector';

/** Creates an HTTP middleware that counts and times every request, labeled by method, route pattern and status. */
export const TelemetryMiddleware = (collector: TelemetryCollector): HttpMiddleware => {
  for (const [name, description] of Object.entries(HTTP_SERVER_METRIC_DESCRIPTIONS)) {
    collector.describeMetric(name, description);
  }
  return async (ctx, next) => {
    const start = performance.now();
    const method = httpMethodLabel(ctx.method);
    const route = httpRouteLabel(ctx.route);
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
      // A response without a status is served as 200; no response at all as 404.
      status = res ? res.status || 200 : 404;
      return res;
    } catch (error) {
      if (requestEnded(ctx)) {
        // The request ended (it reached its deadline or its client left): the
        // HTTP plugin answers 408 whatever the error, so the request counts as 408.
        // A step that failed for its own reason keeps `error` on its span.
        status = 408;
      } else if (error instanceof HttpException) {
        status = error.getStatus();
      } else {
        status = 500;
      }
      throw error;
    } finally {
      // Fractional milliseconds, so a sub-millisecond request is not recorded as 0.
      const duration = performance.now() - start;
      span.finish({
        attributes: { 'http.response.status_code': status },
        ...(status >= 400 ? { code: `http.${status}` } : {}),
      });

      // Read the route again: a HEAD request is matched to its GET route
      // inside the chain, after the span started.
      const attributes = {
        [ATTR_HTTP_METHOD]: method,
        [ATTR_HTTP_ROUTE]: httpRouteLabel(ctx.route),
        [ATTR_HTTP_STATUS_CODE]: status,
      };
      collector.incCounterWithAttributes(HTTP_SERVER_REQUEST_COUNT, 1, attributes);
      collector.observeHistogramWithAttributes(HTTP_SERVER_DURATION, duration, attributes);
    }
  };
};
