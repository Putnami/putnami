import {
  type Claims,
  type HttpRequestContext,
  HttpResponse,
  isRequestStop,
  recordRequestError,
  requestEnded,
  type RequestEnd,
  resolveRoles,
  resolveScopes,
  CSP_NONCE_CONTEXT_KEY,
  CSRF_TOKEN_CONTEXT_KEY,
  type RouteHandler,
  SHARED_CACHE_CONTEXT_KEY,
  startTelemetrySpan,
} from '@putnami/application';
import {
  getHttpStatusText,
  HttpException,
  type Logger,
  shouldExposeErrorStack,
  useConfig,
  useContext,
  useLogger,
} from '@putnami/runtime';
import type { ClientSecurityContext } from '../shared/security.types';
import React, { type ReactElement, type ReactNode } from 'react';
import { type ReactDOMServerReadableStream, renderToReadableStream } from 'react-dom/server';
import type { RouteObject, StaticHandlerContext } from 'react-router';
import {
  createStaticHandler,
  createStaticRouter,
  isRouteErrorResponse,
  StaticRouterProvider,
  UNSAFE_ErrorResponseImpl,
} from 'react-router';
import type { StaticHandlerCache } from './static-handler.cache';
import { DocumentMetaContext } from '../client/document/document-context';
import type { DocumentMeta } from '../client/document/document.types';
import { escapeHtml } from '../client/document/tag.utils';
import { SsrDocumentHelper } from '../client/document/document-ssr.helper';
import { CsrfTokenContext } from '../shared/csrf-context';
import { I18nProvider, type I18nState } from '../shared/i18n/messages';
import { localeRoutes } from '../shared/i18n/route-aliases';
import { generateCspNonce } from './csp';
import {
  applyLocaleHead,
  createI18nState,
  htmlAttributes,
  type I18nRuntimeOptions,
  i18nPayloadScript,
  localeBasename,
  pathBelowBase,
  seedDocumentLocale,
} from './i18n/runtime';
import { serving } from './i18n/negotiation';
import { safeJsonForScript } from './safe-json';
import { PutnamiReactConfig } from './react-ssr.config';
import {
  contextSlots,
  ensureDocumentMeta,
  markClientBootstrapEmitted,
  readClientBootstrap,
  readClientScripts,
  setContextSlot,
} from '../shared/context-slots';

export { safeJsonForScript };

export const pageRenderer =
  (
    routes: () => RouteObject[],
    _page: () => ({ children }: { children?: ReactNode }) => ReactElement,
    hydrateScript: () => string,
    ssrTimeout = 3000,
    basename?: () => string | undefined,
    clientFetchTimeout?: number,
    i18n?: () => I18nRuntimeOptions | undefined,
    /** Whether a request waits for every Suspense boundary before the head. */
    waitForAll?: (req: Request) => boolean,
    /**
     * The application's static handlers, rebuilt only when its route tree
     * changes. Without it, the handler is built for every request.
     */
    staticHandlers?: StaticHandlerCache,
  ): RouteHandler =>
  async () => {
    const logger = useLogger();
    const reqContext = useContext<HttpRequestContext>();

    const { req } = reqContext;

    // Per-request CSP nonce. Threaded onto every framework-emitted inline/module
    // <script> tag and published on the request context so security middleware
    // can add it to Content-Security-Policy when CSP headers are enabled.
    const nonce = generateCspNonce();
    setContextSlot(reqContext, CSP_NONCE_CONTEXT_KEY, nonce);

    // With i18n, the locale prefix extends the router basename for this request
    // only: one route tree serves every locale. The routed URL names the locale;
    // on the locale's domain, the client's URL has no prefix and the router
    // matches it as it is.
    const i18nOptions = i18n?.();
    const moduleBase = basename?.();
    const i18nState: I18nState | undefined = i18nOptions
      ? createI18nState(
          i18nOptions,
          pathBelowBase(new URL(reqContext.url).pathname, moduleBase),
          false,
          serving(reqContext, i18nOptions.settings),
        )
      : undefined;
    const resolvedBasename = i18nState ? localeBasename(i18nState, moduleBase) : moduleBase;
    const source = routes();
    const tree = () => (i18nState ? localeRoutes(source, i18nState.settings, i18nState.locale) : source);
    // Localized applications keep one handler per locale tree and basename.
    const reactStaticHandler = staticHandlers
      ? staticHandlers.handler(
          source,
          `${i18nState?.locale ?? ''}\u0000${resolvedBasename ?? ''}`,
          tree,
          resolvedBasename,
        )
      : createStaticHandler(tree(), resolvedBasename ? { basename: resolvedBasename } : undefined);
    // The loaders, or the action of a form post, run under their own span.
    const loaders = startTelemetrySpan({ name: 'ssr loaders', kind: 'internal' });
    let reactStaticContext: Awaited<ReturnType<typeof reactStaticHandler.query>>;
    try {
      reactStaticContext = await loaders.within(() => reactStaticHandler.query(req));
    } catch (error) {
      loaders.finish({ code: failedStepCode(error, reqContext) });
      throw error;
    }
    const loaderCode = reportLoaderErrors(reactStaticContext, reqContext, logger);
    loaders.finish(loaderCode ? { code: loaderCode } : undefined);
    answerLoaderExceptions(reactStaticContext);
    // The request ended while its loaders ran: the HTTP plugin answers it, so
    // nothing is rendered.
    if (requestEnded(reqContext))
      throw reqContext.signal?.reason ?? new DOMException('Request Timeout', 'TimeoutError');

    const validationResult = handleStaticContext(reactStaticContext);
    if (validationResult) {
      return validationResult;
    }

    const router = createStaticRouter(reactStaticHandler.dataRoutes, reactStaticContext as StaticHandlerContext);

    const context = reactStaticContext as StaticHandlerContext;

    // Create a shared document meta object that all components can access.
    // Stored on the request context (AsyncLocalStorage — concurrent-safe)
    // and via React Context (for lazy-loaded components across Suspense boundaries).
    // No globalThis fallback: shared mutable SSR state would bleed/clear
    // between concurrent requests.
    const documentMeta = ensureDocumentMeta(reqContext);
    if (i18nState) seedDocumentLocale(documentMeta, i18nState);

    // The CSRF token for SSR components (<CsrfInput />). Prefer the token the
    // CSRF middleware published on the request context: on a first visit the
    // request carries no `_csrf` cookie yet, but the middleware has already
    // generated the token this response's Set-Cookie will establish — so the
    // rendered form works without JavaScript even on the first response. Fall
    // back to parsing the request cookie for setups without the middleware.
    const slotToken = contextSlots(reqContext)[CSRF_TOKEN_CONTEXT_KEY];
    let csrfToken: string | undefined = typeof slotToken === 'string' ? slotToken : undefined;
    if (!csrfToken) {
      const cookieHeader = req.headers.get('Cookie');
      if (cookieHeader) {
        const match = cookieHeader.split('; ').find((c) => c.startsWith('_csrf='));
        if (match) {
          const eqIndex = match.indexOf('=');
          csrfToken = eqIndex >= 0 ? match.slice(eqIndex + 1) : undefined;
        }
      }
    }

    // Wrap the router with context providers to ensure all components
    // (including lazy-loaded ones) can access shared state during SSR
    const routerTree = React.createElement(
      CsrfTokenContext.Provider,
      { value: csrfToken },
      React.createElement(
        DocumentMetaContext.Provider,
        { value: documentMeta },
        // hydrate: false — wrapInHtmlDocument emits the hydration data in one
        // nonced script before #root. React Router's own copy would render
        // inside #root without the nonce: a strict CSP blocks it, and the
        // client router does not render it, so hydration mismatches.
        React.createElement(StaticRouterProvider, {
          context,
          router,
          hydrate: false,
        }),
      ),
    );
    const staticRouter = i18nState ? React.createElement(I18nProvider, { value: i18nState }, routerTree) : routerTree;

    // Declared in the function scope so the catch can release it, but the
    // lifetime itself is only created inside the try below — a throw before
    // that leaves nothing to leak, and a throw after it is released in the catch.
    let lifetime: RenderLifetime | undefined;

    try {
      // The render stops at its timeout, and when the request ends: its client
      // left or it outlived the HTTP plugin's request timeout.
      const renderLifetime = new RenderLifetime(ssrTimeout, reqContext.signal);
      lifetime = renderLifetime;
      // The span ends once React has rendered the shell the stream starts with.
      const render = startTelemetrySpan({ name: 'ssr render', kind: 'internal' });
      let reactStream: ReactDOMServerReadableStream;
      try {
        reactStream = await render.within(() =>
          createReactStream(staticRouter, {
            signal: renderLifetime.signal,
            nonce,
            onError: (error) => {
              // A request that ended stops its render on purpose: that stop is
              // not a failure. Any other error still is.
              if (!isRequestStop(error, reqContext)) logger.error('SSR streaming error:', error);
            },
          }),
        );
        render.finish();
      } catch (error) {
        render.finish({ code: failedStepCode(error, reqContext) });
        throw error;
      }

      // Build client-side security context from the authenticated user (if any).
      // Only roles and scopes — never tokens or PII.
      let securityContext: ClientSecurityContext | undefined;
      if (reqContext.user) {
        const claims = reqContext.user as Claims;
        securityContext = {
          authenticated: true,
          roles: resolveRoles(claims, {}),
          scopes: resolveScopes(claims, {}),
        };
      }

      // Create document helper that reads from the shared documentMeta.
      // Head HTML is collected after allReady so that SEO meta tags from lazy
      // components are included, but the React body streams progressively.
      const docHelper = new SsrDocumentHelper(documentMeta, nonce);
      // Before the document is built, not after: a plugin reading the slot on
      // the way back out of the middleware chain must see that this response
      // carries what it published, whether or not the stream completes.
      markClientBootstrapEmitted(reqContext);
      const readable = await wrapInHtmlDocument(reactStream, docHelper, {
        release: () => renderLifetime.release(),
        context: context,
        logger,
        hydrateScript: hydrateScript(),
        basename: resolvedBasename,
        securityContext,
        nonce,
        clientFetchTimeout,
        bootstrap: readClientBootstrap(reqContext),
        clientScripts: readClientScripts(reqContext),
        i18n: i18nOptions && i18nState ? { options: i18nOptions, state: i18nState, documentMeta } : undefined,
        // A response a shared cache stores (CacheMiddleware, on the page or a
        // layout) is served to crawlers too.
        waitForAll: (waitForAll?.(req) ?? false) || contextSlots(reqContext)[SHARED_CACHE_CONTEXT_KEY] === true,
      });

      // Use React Router's computed status code (e.g. 404 from a loader-thrown Response),
      // falling back to the HTTP route's status code or 200.
      const status = context.statusCode !== 200 ? context.statusCode : reqContext.statusCode || 200;
      const headers: Record<string, string> = { 'content-type': 'text/html;charset=utf-8' };
      if (i18nState) headers['content-language'] = i18nState.locale;
      const response = new HttpResponse(readable, { status, headers });

      return response;
    } catch (error) {
      // On the happy path wrapInHtmlDocument releases this in its finally after
      // streaming; if createReactStream (or anything after the lifetime is
      // created above) throws, that finally never runs, so release it here: no
      // timer and no request listener outlive a failed render. (A finally
      // around the whole try would release it too early — the lifetime must
      // outlive this return to cover async streaming.)
      lifetime?.release();
      // The request ended (its client left or it passed its timeout): the
      // HTTP plugin answers it, quietly, so its stop is not a render failure.
      if (isRequestStop(error, reqContext)) throw error;
      recordRequestError(reqContext, error);
      logger.error(error);
      return handleRenderError(error);
    }
  };

/**
 * The abort signal of one render: it aborts at the render's timeout, and with
 * the request's reason when the request ends.
 *
 * React renders with a plain signal of its own, never one `AbortSignal.any`
 * builds over the request's: React before 19.3 removes its abort listener only
 * when the signal aborts, that listener reaches the request through `onError`,
 * and Bun keeps a signal `AbortSignal.any` built, listeners included, alive
 * while one of its sources is. The request's end reaches this signal through
 * one listener that `release` removes, with the timer, once the render settles.
 */
class RenderLifetime {
  private readonly controller = new AbortController();
  private readonly timer: ReturnType<typeof setTimeout>;

  constructor(
    timeoutMs: number,
    private readonly request: AbortSignal | undefined,
  ) {
    this.timer = setTimeout(() => this.controller.abort(), timeoutMs);
    if (request?.aborted) {
      this.controller.abort(request.reason);
    } else {
      request?.addEventListener('abort', this, { once: true });
    }
  }

  get signal(): AbortSignal {
    return this.controller.signal;
  }

  /** The request ended: the render stops with the request's reason. */
  handleEvent(): void {
    this.controller.abort(this.request?.reason);
  }

  /** The render settled: its timer and its listener on the request go. */
  release(): void {
    clearTimeout(this.timer);
    this.request?.removeEventListener('abort', this);
  }
}

/**
 * Creates the React readable stream from the static router element. React
 * stamps `nonce` on the inline scripts it writes into the stream, such as the
 * ones that move a large Suspense boundary into place.
 */
async function createReactStream(
  staticRouter: ReactElement,
  options: { signal: AbortSignal; nonce: string; onError: (err: unknown) => void },
) {
  return await renderToReadableStream(staticRouter, {
    signal: options.signal,
    nonce: options.nonce,
    onError: options.onError,
  });
}

export function sanitizeErrors(
  errors: Record<string, unknown> | null | undefined,
): Record<string, unknown> | null | undefined {
  if (errors == null) return errors;
  return Object.fromEntries(
    Object.entries(errors).map(([key, err]) => [
      key,
      err instanceof Error ? { message: err.message, status: (err as { status?: number }).status } : err,
    ]),
  );
}

/**
 * Wraps the React stream in an HTML document structure.
 */
export async function wrapInHtmlDocument(
  reactStream: ReactDOMServerReadableStream,
  docHelper: SsrDocumentHelper,
  options: {
    /** Releases the render's lifetime (its timer and request listener) once the document is written. */
    release: () => void;
    context: StaticHandlerContext;
    hydrateScript: string;
    basename?: string;
    securityContext?: ClientSecurityContext;
    /** Per-request CSP nonce stamped on every framework-emitted <script> tag. */
    nonce?: string;
    /** Client loader/action fetch timeout (ms), surfaced to the browser bundle. */
    clientFetchTimeout?: number;
    /** Plugin-owned data serialized as `window.__putnamiBootstrap`. */
    bootstrap?: Record<string, unknown>;
    /** Plugin-owned module script URLs emitted after the hydrate script. */
    clientScripts?: readonly string[];
    /** The locale of the render, when the application is localized. */
    i18n?: { options: I18nRuntimeOptions; state: I18nState; documentMeta: DocumentMeta };
    /**
     * Wait for every Suspense boundary before the head, as a crawler request
     * does. Otherwise the head and the shell are sent once the shell is ready.
     */
    waitForAll?: boolean;
    // biome-ignore lint/suspicious/noExplicitAny: logger type
    logger?: any;
  },
) {
  const { readable, writable } = new TransformStream<Uint8Array, Uint8Array>();
  const writer = writable.getWriter();
  const encoder = new TextEncoder();
  const logger = options.logger || console;

  // Start streaming in the background
  (async () => {
    try {
      // Serialize hydration data
      const hydrationData = {
        loaderData: options.context.loaderData,
        actionData: options.context.actionData,
        errors: sanitizeErrors(options.context.errors),
      };
      const basenameScript = options.basename ? `window.__basename=${safeJsonForScript(options.basename)};` : '';
      const securityScript = options.securityContext
        ? `window.__securityContext=${safeJsonForScript(options.securityContext)};`
        : '';
      const fetchTimeoutScript =
        typeof options.clientFetchTimeout === 'number' && options.clientFetchTimeout > 0
          ? `window.__reactClientFetchTimeoutMs=${safeJsonForScript(options.clientFetchTimeout)};`
          : '';
      // Plugin-owned bootstrap data. Goes through safeJsonForScript like every
      // other inline payload, so a value containing `</script>` cannot close the
      // tag it is embedded in.
      const bootstrapScript =
        options.bootstrap && Object.keys(options.bootstrap).length > 0
          ? `window.__putnamiBootstrap=${safeJsonForScript(options.bootstrap)};`
          : '';
      // Tell the browser whether it may render error diagnostics. The client
      // boundaries cannot work this out on their own: a browser has no
      // `process` to read, and an inline `process.env.NODE_ENV` check is
      // constant-folded into the published bundle. Emitted only when this
      // server exposes, so production HTML never carries it and an absent flag
      // fails closed. Same source as the server-side boundaries, so the
      // hydrated markup matches what was streamed.
      const exposeErrorsScript = shouldExposeErrorStack() ? 'window.__putnamiExposeErrors=true;' : '';
      // Stamp the per-request nonce so a strict CSP (`script-src 'nonce-...'`)
      // permits these framework-injected scripts. base64 nonces never contain
      // a double quote, so they are safe inside the quoted attribute.
      const nonceAttr = options.nonce ? ` nonce="${options.nonce}"` : '';
      const hydrationScript = `<script${nonceAttr}>${basenameScript}${securityScript}${fetchTimeoutScript}${bootstrapScript}${exposeErrorsScript}window.__staticRouterHydrationData = ${safeJsonForScript(hydrationData)};</script>`;

      const hydrateSrc = options.hydrateScript.startsWith('/') ? options.hydrateScript : `/${options.hydrateScript}`;

      // A crawler waits for every Suspense boundary, so the head it reads
      // holds every contribution, lazy components (PageMeta) included. A
      // browser gets the head and the shell as soon as the shell is ready:
      // what a boundary adds after that streams with it (lateStylesHtml) or
      // after the root (lateHeadHtml).
      if (options.waitForAll) await reactStream.allReady;

      // A page's own canonical link and `<Lang>` are known only now.
      const i18n = options.i18n;
      if (i18n) applyLocaleHead(i18n.documentMeta, i18n.state);
      const headHtml = docHelper.headHtml || '';
      const i18nScript = i18n ? i18nPayloadScript(i18n.options, i18n.state, true) : '';

      // Send HTML prefix, then the body chunk by chunk.
      await writer.write(
        encoder.encode(
          `<!DOCTYPE html><html ${htmlAttributes(docHelper.lang, i18n?.state)}>${headHtml}<body>${i18nScript}${hydrationScript}<div id="root">`,
        ),
      );

      // Stream React content as it renders
      const reader = reactStream.getReader();
      while (true) {
        // biome-ignore lint/performance/noAwaitInLoops: Sequential stream reading is required
        const { done, value } = await reader.read();
        if (done) break;
        // Styles a later boundary inserted go before the chunk that shows it.
        const lateStyles = docHelper.lateStylesHtml();
        if (lateStyles) await writer.write(encoder.encode(lateStyles));
        await writer.write(value);
      }

      // Plugin-owned module scripts, emitted after the hydrate script so they
      // observe a router that already exists. Same nonce, same escaping.
      const extraScripts = (options.clientScripts ?? [])
        .map(
          (src) =>
            `<script type="module"${nonceAttr} src="${escapeHtml(src.startsWith('/') ? src : `/${src}`)}"></script>`,
        )
        .join('');

      // Send HTML suffix
      // Inject bootstrap script after the root div so it's outside hydration boundary
      await writer.write(
        encoder.encode(
          `</div>${docHelper.lateHeadHtml()}<script type="module"${nonceAttr} src="${hydrateSrc}"></script>${extraScripts}</body></html>`,
        ),
      );
    } catch (error) {
      logger.error('Streaming error:', error);
      try {
        await writer.write(encoder.encode('</div><!-- SSR error --></body></html>'));
      } catch {
        // Ignore errors if writing to the closed/errored stream fails
      }
    } finally {
      options.release();
      await writer.close();
    }
  })();

  return readable;
}

/**
 * Handles rendering errors by returning a formatted error response.
 */
export function handleRenderError(error: unknown) {
  const config = useConfig(PutnamiReactConfig);
  const errorMessage = config.isDevelopment
    ? `<h1>Something went wrong</h1><pre>${escapeHtml(String(error))}</pre>`
    : '<h1>Something went wrong</h1><p>An unexpected error occurred. Please try again later.</p>';

  return new HttpResponse(errorMessage, {
    status: 500,
    headers: { 'content-type': 'text/html;charset=utf-8' },
  });
}

/** The status code of a step that failed: canceled when it is the stop of its ended request. */
function failedStepCode(error: unknown, request: RequestEnd): 'canceled' | 'error' {
  return isRequestStop(error, request) ? 'canceled' : 'error';
}

/**
 * Handles the errors React Router kept from loaders and actions, one per error
 * boundary, the way the dispatcher handles a route handler's error: records the
 * first one as the request error, and logs each one at ERROR with its stack
 * unless it is an `HttpException`. A thrown `Response` or `HttpResponse` is a
 * redirect or a status the loader chose, not an error, and a loader that stopped
 * with its ended request did not fail. Returns the loaders span's status code:
 * `error` when it recorded an error, `canceled` when a loader only stopped.
 */
function reportLoaderErrors(
  context: StaticHandlerContext | Response,
  reqContext: HttpRequestContext,
  logger: Logger,
): 'canceled' | 'error' | undefined {
  if (!('errors' in context) || !context.errors) return undefined;
  let recorded = false;
  let stopped = false;
  for (const error of Object.values(context.errors)) {
    if (isRouteErrorResponse(error) || error instanceof Response || error instanceof HttpResponse) continue;
    if (isRequestStop(error, reqContext)) {
      stopped = true;
      continue;
    }
    if (!recorded) {
      recordRequestError(reqContext, error);
      recorded = true;
    }
    if (!(error instanceof HttpException)) logger.error(error);
  }
  return recorded ? 'error' : stopped ? 'canceled' : undefined;
}

/**
 * Answers a 4xx or 5xx `HttpException` a loader threw with its own status:
 * React Router keeps it as an unknown error and computes a 500, so each one is
 * replaced by the route error response of its status, which the not-found and
 * error boundaries render, with `{ statusCode, error, message }` as its
 * `data`, and the router status follows the first error as React Router
 * computes it. Runs after `reportLoaderErrors`, which records the
 * exception itself as the request error.
 */
function answerLoaderExceptions(context: StaticHandlerContext | Response): void {
  if (context instanceof Response || !context.errors) return;
  let converted = false;
  const errors: Record<string, unknown> = {};
  for (const [routeId, error] of Object.entries(context.errors)) {
    // An exception with a status below 400 is not an error answer.
    if (error instanceof HttpException && error.getStatus() >= 400) {
      const status = error.getStatus();
      const statusText = getHttpStatusText(status) ?? error.name;
      // Only the status and the message reach the hydration payload, as
      // `sanitizeErrors` keeps of an `Error`: the exception's details stay on
      // the server.
      errors[routeId] = new UNSAFE_ErrorResponseImpl(status, statusText, {
        statusCode: status,
        error: statusText,
        message: error.message,
      });
      converted = true;
    } else {
      errors[routeId] = error;
    }
  }
  if (!converted) return;
  context.errors = errors;
  const first = Object.values(errors)[0];
  context.statusCode = isRouteErrorResponse(first) ? first.status : 500;
}

function handleStaticContext(reactStaticContext: StaticHandlerContext | Response): HttpResponse | undefined {
  if (reactStaticContext instanceof HttpResponse) {
    return reactStaticContext as HttpResponse;
  }
  if (reactStaticContext instanceof Response) {
    // Convert native Response (e.g. redirect thrown by a loader) to HttpResponse
    // so downstream code (caching, middleware) can use HttpResponse's API.
    const location = reactStaticContext.headers.get('Location');
    if (location) {
      return HttpResponse.redirect(location, reactStaticContext.status as 301 | 302 | 307 | 308);
    }
    const headers: Record<string, string> = {};
    reactStaticContext.headers.forEach((v, k) => {
      headers[k] = v;
    });
    return new HttpResponse(reactStaticContext.body ?? undefined, {
      status: reactStaticContext.status,
      headers,
    });
  }
  if (reactStaticContext.errors) {
    for (const error of Object.values(reactStaticContext.errors)) {
      // HttpResponse.redirected is a standard Response property not in the local type
      if (error instanceof HttpResponse && (error as unknown as Response).redirected) {
        return error as HttpResponse;
      }
    }
  }
  return undefined;
}
