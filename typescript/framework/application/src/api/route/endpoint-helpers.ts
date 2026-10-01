import type { AotValidators, SchemaDefinition, TagSelector, Token } from '@putnami/runtime';
import { useLogger } from '@putnami/runtime';
import { resolveInjection, useContainer } from '@putnami/runtime/inject';
import type { HttpRequestContext, HttpRequestContextInternal } from '../../http/http-context.type';
import type { HttpMiddleware } from '../../http/http-middleware.type';
import { HttpResponse } from '../../http/http-response';
import type { RouteHandler, RouteHandlerResult } from '../../http/route.type';
import { ENDPOINT_MARKER, type EndpointDefinition } from './endpoint.types';
import type { EndpointMeta, ResponseDeclarations } from './response-meta';
import { validateSchema } from './validate';
import { applyInputValidation } from './validation';
import { isConcreteBinaryContentType } from './binary';

export function buildDefinition(
  handler: RouteHandler,
  schemas?: EndpointDefinition['schemas'],
  middleware?: HttpMiddleware[],
  responses?: ResponseDeclarations,
  meta?: EndpointMeta,
  minimal?: boolean,
  csrfExempt?: boolean,
  dependencies?: readonly string[],
  provenance?: EndpointDefinition['provenance'],
): EndpointDefinition {
  const hasInputSchemas = schemas?.params || schemas?.query || schemas?.body || schemas?.bodyBinary || schemas?.headers;
  const hasResponseSchema = schemas?.returns || schemas?.returnsBinary?.streamed;
  const needsWrapping = hasInputSchemas || hasResponseSchema;
  const hasMiddleware = middleware && middleware.length > 0;

  let finalHandler = needsWrapping ? wrapWithValidation(handler, schemas) : handler;
  if (hasMiddleware) {
    finalHandler = wrapWithMiddleware(finalHandler, middleware);
  }

  return {
    __endpoint: ENDPOINT_MARKER,
    handler: finalHandler,
    __rawHandler: handler,
    ...(dependencies && dependencies.length > 0 ? { dependencies } : {}),
    ...(provenance ? { provenance } : {}),
    schemas,
    ...(responses ? { responses } : {}),
    ...(meta ? { meta } : {}),
    middleware: hasMiddleware ? middleware : undefined,
    ...(minimal ? { minimal: true } : {}),
    // Emit the flag for any explicit value (including `false`) so the consumer's
    // `def.csrfExempt ?? apiDefault` inherits only when it was never set. Emitting
    // only on `true` would make `.csrfExempt(false)` silently inherit (and thus
    // stay exempt under a default api()), the opposite of the caller's intent.
    ...(csrfExempt !== undefined ? { csrfExempt } : {}),
  };
}

/**
 * Rebuild an endpoint's validation around compiled (AOT) validators, reusing its
 * pre-validation handler. Used by the build-time codegen for endpoints with no
 * route-level middleware. Each compiled validator falls back to the generic
 * `validateSchema` on any anomaly, so behaviour is identical to the interpreted path.
 */
export function buildAotValidatedHandler(def: EndpointDefinition, aot: AotValidators): RouteHandler {
  return wrapWithValidation(def.__rawHandler ?? def.handler, def.schemas, aot);
}

function wrapWithMiddleware(handler: RouteHandler, middleware: HttpMiddleware[]): RouteHandler {
  // Pre-compose middleware chain once at definition time (not per request).
  type ChainFn = (
    ctx: HttpRequestContext,
    dispatch: () => Promise<HttpResponse | undefined>,
  ) => Promise<HttpResponse | undefined>;

  const chain: ChainFn = middleware.reduceRight<ChainFn>(
    (next, mw) => (ctx, dispatch) => mw(ctx, () => next(ctx, dispatch)),
    (_ctx, dispatch) => dispatch(),
  );

  return async (ctx: HttpRequestContext) => {
    const dispatch = async (): Promise<HttpResponse | undefined> => {
      const result = await handler(ctx);
      if (result === undefined) return undefined;
      if (result instanceof HttpResponse) {
        return result;
      }
      if (typeof result === 'string') {
        return new HttpResponse(result, {
          status: ctx.statusCode || 200,
          headers: { 'Content-Type': 'text/plain' },
        });
      }
      return HttpResponse.json(result, { status: ctx.statusCode || 200 });
    };

    const res = await chain(ctx, dispatch);
    // Return the HttpResponse directly — it will be handled by the dispatcher
    return res;
  };
}

const DEV_MODE = process.env.NODE_ENV !== 'production';

function wrapWithValidation(
  handler: RouteHandler,
  schemas: EndpointDefinition['schemas'],
  aot?: AotValidators,
): RouteHandler {
  const shouldValidateResponse = DEV_MODE && !!schemas?.returns;

  return async (ctx: HttpRequestContext) => {
    applyInputValidation(ctx, schemas, aot);

    const result = await handler(ctx);
    if (schemas?.returnsBinary?.streamed && (!(result instanceof HttpResponse) || (result.status ?? 200) < 300)) {
      const contentType =
        result instanceof HttpResponse
          ? result.rawHeaderEntries().find(([name]) => name.toLowerCase() === 'content-type')?.[1]
          : undefined;
      if (!(result instanceof HttpResponse) || !isConcreteBinaryContentType(contentType)) {
        const body = result instanceof HttpResponse ? result.getBodyInit() : undefined;
        if (body instanceof ReadableStream) await body.cancel().catch(() => {});
        throw new Error('BinaryStream response requires a concrete Content-Type');
      }
      (ctx as HttpRequestContextInternal).__rawHttpStreamResponse = true;
    }

    // Dev-mode response validation: warn on contract mismatches
    if (shouldValidateResponse && schemas?.returns) {
      validateResponse(result, schemas.returns, ctx.route);
    }

    return result;
  };
}

/**
 * Validate a handler's response against the `returns` schema.
 * Logs a warning instead of throwing so dev servers stay running.
 */
function validateResponse(result: unknown, schema: SchemaDefinition, route?: string): void {
  try {
    // Extract the JSON body from the response
    const body = extractResponseBody(result);
    if (body === undefined) {
      return;
    }

    validateSchema(schema, body, { label: 'response' });
  } catch (err) {
    const logger = useLogger('putnami:route');
    const routeLabel = route ?? 'unknown';
    const message = err instanceof Error ? err.message : String(err);
    logger.warn(`[response-validation] ${routeLabel} — ${message}`);
  }
}

/**
 * Extract the serializable body from a handler result.
 */
function extractResponseBody(result: unknown): unknown {
  if (result === undefined || result === null) {
    return undefined;
  }
  if (result instanceof HttpResponse) {
    // Cannot introspect opaque HttpResponse — skip validation
    return undefined;
  }
  if (typeof result === 'string') {
    try {
      return JSON.parse(result);
    } catch {
      return undefined;
    }
  }
  // Plain object / array — the most common case
  return result;
}

/**
 * Wrap a handler with DI injection — resolves tokens from the current container scope
 * and exposes the resolved dependencies on `ctx.deps` for the handler.
 */
export function wrapWithInjection(
  handler: (ctx: HttpRequestContext & { deps: unknown }) => unknown,
  tokens: Record<string, Token | TagSelector>,
): RouteHandler {
  return async (ctx: HttpRequestContext) => {
    const scope = useContainer();
    const deps = resolveInjection(tokens, scope);
    (ctx as HttpRequestContext & { deps: unknown }).deps = deps;
    return handler(ctx as HttpRequestContext & { deps: unknown }) as RouteHandlerResult;
  };
}
