import type { HttpMiddleware } from '.';
import { HttpResponse } from './http-response';

/** Creates a middleware that extracts or generates a trace/correlation ID and propagates it in the response. */
export const TraceMiddleware = (): HttpMiddleware => {
  return async (ctx, next) => {
    // 1. GCP Trace Header: "TRACE_ID/SPAN_ID;o=TRACE_TRUE"
    const cloudTraceCtx = ctx.req.headers.get('X-Cloud-Trace-Context');
    let traceId: string | undefined;

    if (cloudTraceCtx) {
      // Extract TRACE_ID (everything before the first slash)
      const parts = cloudTraceCtx.split('/');
      if (parts.length > 0 && parts[0]) {
        traceId = parts[0];
      }
    }

    // 2. Correlation ID Header
    if (!traceId) {
      traceId = ctx.req.headers.get('X-Correlation-ID') || crypto.randomUUID();
    }

    // Set traceId in context for logger usage
    ctx.traceId = traceId;

    let res = await next();

    // Propagate traceId in response headers
    if (res instanceof HttpResponse && traceId) {
      res = res.setHeader('X-Correlation-ID', traceId);
    }

    return res;
  };
};
