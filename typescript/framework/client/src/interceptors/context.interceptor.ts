import { tryContext } from '@putnami/runtime';
import type { ClientRequest, ClientResponse, Interceptor } from '../runtime/transport.type';

/**
 * Routing context available in the async context.
 * Propagated as headers for service-to-service calls.
 */
interface RoutingContext {
  /** Trace ID for distributed tracing */
  traceId?: string;
  /** Request ID for correlation */
  requestId?: string;
  /** Region hint for geo-routing */
  __region?: string;
  /** Experiment/A/B test variant IDs */
  __experiments?: string;
}

/** Header names for routing context propagation. */
const TRACE_ID_HEADER = 'X-Trace-Id';
const REQUEST_ID_HEADER = 'X-Request-Id';
const REGION_HEADER = 'X-Region';
const EXPERIMENTS_HEADER = 'X-Experiments';

/**
 * Sanitize a header value by stripping control characters (CR, LF, NUL).
 * Prevents header injection via CRLF sequences in propagated context values.
 */
function sanitizeHeaderValue(value: string): string {
  return value.replace(/[\r\n\0]/g, '');
}

/**
 * Creates an interceptor that propagates routing context headers.
 *
 * Extracts trace ID, request ID, region, and experiment context from the
 * current async context and injects them as headers on outgoing requests.
 * This enables distributed tracing, geo-routing, and A/B experiment propagation
 * across service boundaries.
 */
export function contextInterceptor(): Interceptor {
  return async (request: ClientRequest, next: (req: ClientRequest) => Promise<ClientResponse>) => {
    const context = tryContext<RoutingContext>();

    if (context?.traceId && !request.headers.has(TRACE_ID_HEADER)) {
      request.headers.set(TRACE_ID_HEADER, sanitizeHeaderValue(context.traceId));
    }

    if (context?.requestId && !request.headers.has(REQUEST_ID_HEADER)) {
      request.headers.set(REQUEST_ID_HEADER, sanitizeHeaderValue(context.requestId));
    }

    if (context?.__region && !request.headers.has(REGION_HEADER)) {
      request.headers.set(REGION_HEADER, sanitizeHeaderValue(context.__region));
    }

    if (context?.__experiments && !request.headers.has(EXPERIMENTS_HEADER)) {
      request.headers.set(EXPERIMENTS_HEADER, sanitizeHeaderValue(context.__experiments));
    }

    return next(request);
  };
}
