import {
  type Claims,
  type HttpRequestContext,
  HttpResponse,
  resolveRoles,
  resolveScopes,
  CSP_NONCE_CONTEXT_KEY,
  CSRF_TOKEN_CONTEXT_KEY,
  type RouteHandler,
} from '@putnami/application';
import { shouldExposeErrorStack, useConfig, useContext, useLogger } from '@putnami/runtime';
import type { ClientSecurityContext } from '../shared/security.types';
import React, { type ReactElement, type ReactNode } from 'react';
import { type ReactDOMServerReadableStream, renderToReadableStream } from 'react-dom/server';
import type { RouteObject, StaticHandlerContext } from 'react-router';
import { createStaticHandler, createStaticRouter, StaticRouterProvider } from 'react-router';
import { DocumentMetaContext } from '../client/document/document-context';
import { escapeHtml } from '../client/document/tag.utils';
import { SsrDocumentHelper } from '../client/document/document-ssr.helper';
import { CsrfTokenContext } from '../shared/csrf-context';
import { generateCspNonce } from './csp';
import { PutnamiReactConfig } from './react-ssr.config';
import {
  contextSlots,
  ensureDocumentMeta,
  markClientBootstrapEmitted,
  readClientBootstrap,
  readClientScripts,
  setContextSlot,
} from '../shared/context-slots';

export const pageRenderer =
  (
    routes: () => RouteObject[],
    _page: () => ({ children }: { children?: ReactNode }) => ReactElement,
    hydrateScript: () => string,
    ssrTimeout = 3000,
    basename?: () => string | undefined,
    clientFetchTimeout?: number,
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

    const resolvedBasename = basename?.();
    const staticOpts = resolvedBasename ? { basename: resolvedBasename } : undefined;
    const reactStaticHandler = createStaticHandler(routes(), staticOpts);
    const reactStaticContext = await reactStaticHandler.query(req);

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
    const staticRouter = React.createElement(
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

    // Declared in the function scope so the catch can clear it, but the timer
    // itself is only created inside the try below — a throw before that leaves
    // nothing to leak, and a throw after it is released in the catch.
    let timeoutId: ReturnType<typeof setTimeout> | undefined;

    try {
      const controller = new AbortController();
      timeoutId = setTimeout(() => {
        controller.abort();
      }, ssrTimeout);

      const reactStream = await createReactStream(staticRouter, {
        signal: controller.signal,
        onError: (error) => {
          logger.error('SSR streaming error:', error);
        },
      });

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
        timeoutId,
        context: context,
        logger,
        hydrateScript: hydrateScript(),
        basename: resolvedBasename,
        securityContext,
        nonce,
        clientFetchTimeout,
        bootstrap: readClientBootstrap(reqContext),
        clientScripts: readClientScripts(reqContext),
      });

      // Use React Router's computed status code (e.g. 404 from a loader-thrown Response),
      // falling back to the HTTP route's status code or 200.
      const status = context.statusCode !== 200 ? context.statusCode : reqContext.statusCode || 200;
      const response = new HttpResponse(readable, {
        status,
        headers: { 'content-type': 'text/html;charset=utf-8' },
      });

      return response;
    } catch (error) {
      // On the happy path wrapInHtmlDocument clears this in its finally after
      // streaming; if createReactStream (or anything after the timer is created
      // above) throws, that finally never runs, so release the timer here to
      // avoid a dangling abort timer per failed render. (A finally around the
      // whole try would clear it too early — the timer must outlive this return
      // to cover async streaming.)
      if (timeoutId !== undefined) clearTimeout(timeoutId);
      logger.error(error);
      return handleRenderError(error);
    }
  };

/**
 * Creates the React readable stream from the static router element.
 */
async function createReactStream(
  staticRouter: ReactElement,
  options: { signal: AbortSignal; onError: (err: unknown) => void },
) {
  return await renderToReadableStream(staticRouter, {
    signal: options.signal,
    onError: options.onError,
  });
}

/**
 * Serialize a value to JSON safe for embedding inside an HTML `<script>` block.
 *
 * Escapes sequences that would otherwise allow content to break out of the
 * script context (`</script>`, `<!--`) or trigger charset-based XSS
 * (`\u2028`, `\u2029`).
 */
export function safeJsonForScript(value: unknown): string {
  return JSON.stringify(value)
    .replace(/</g, '\\u003c')
    .replace(/>/g, '\\u003e')
    .replace(/\u2028/g, '\\u2028')
    .replace(/\u2029/g, '\\u2029');
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
    timeoutId: ReturnType<typeof setTimeout>;
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

      // Wait for all Suspense boundaries to resolve so that lazy components
      // (including PageMeta) populate the shared documentMeta before we
      // collect head HTML.  The body will still stream progressively because
      // the reader loop below starts immediately after the head is flushed.
      await reactStream.allReady;

      const headHtml = docHelper.headHtml || '';

      // Send HTML prefix — TTFB is after allReady but the body streams
      // chunk-by-chunk from here.
      await writer.write(
        encoder.encode(
          `<!DOCTYPE html><html lang="${escapeHtml(docHelper.lang)}">${headHtml}<body>${hydrationScript}<div id="root">`,
        ),
      );

      // Stream React content as it renders
      const reader = reactStream.getReader();
      while (true) {
        // biome-ignore lint/performance/noAwaitInLoops: Sequential stream reading is required
        const { done, value } = await reader.read();
        if (done) break;
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
          `</div><script type="module"${nonceAttr} src="${hydrateSrc}"></script>${extraScripts}</body></html>`,
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
      clearTimeout(options.timeoutId);
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
