import { HttpResponse } from '@putnami/application';
import { runInContext } from '@putnami/runtime';
import React from 'react';
import { renderToReadableStream } from 'react-dom/server';
import type { RouteObject, StaticHandlerContext } from 'react-router';
import { createStaticHandler, createStaticRouter, StaticRouterProvider } from 'react-router';
import { DocumentMetaContext } from '../client/document/document-context';
import { SsrDocumentHelper } from '../client/document/document-ssr.helper';
import { escapeHtml } from '../client/document/tag.utils';
import { IslandModeContext } from '../client/island/island';
import { ISLAND_TAG } from '../client/island/island-types';
import { CsrfTokenContext } from '../shared/csrf-context';
import { createStaticRenderContext, isStaticRenderViolation, type StaticPathParams } from './static';
import { ensureDocumentMeta } from '../shared/context-slots';

export interface StaticRenderResult {
  /** The complete pre-rendered HTML document. */
  html: string;
  /** The HTTP status the route resolved to (200, or e.g. 404/redirect status). */
  status: number;
  /** Set when the route resolved to a redirect rather than HTML. */
  redirectLocation?: string;
}

interface StaticRenderOptions {
  /** The composed React-Router route tree (layouts + pages). */
  routes: RouteObject[];
  /** The route pattern being rendered (for diagnostics), e.g. `/tasks/:id`. */
  route: string;
  /** The concrete pathname to render, e.g. `/tasks/7`. */
  pathname: string;
  /** Concrete params for the pathname (passed to the build-time context). */
  params?: StaticPathParams;
  /** React-Router basename, when the app is mounted under a path prefix. */
  basename?: string;
  /** Build-time render timeout in milliseconds. */
  timeoutMs?: number;
  /** Extra HTML injected just before `</head>` (e.g. island stylesheet links). */
  headExtra?: string;
  /** Extra HTML injected just before `</body>` (e.g. island hydration scripts). */
  bodyExtra?: string;
  /**
   * URL of the islands hydration entry. When the rendered page contains island
   * markers this script is injected so the islands hydrate; otherwise the page
   * stays zero-JavaScript.
   */
  islandsScript?: string;
}

/** Reads the inner HTML of a `<div>` whose open tag ended at `from`, honouring nested `<div>`s. */
function readBalancedDiv(html: string, from: number): string | undefined {
  const tagRe = /<(\/?)div\b[^>]*>/g;
  tagRe.lastIndex = from;
  let depth = 1;
  for (let m = tagRe.exec(html); m !== null; m = tagRe.exec(html)) {
    depth += m[1] ? -1 : 1;
    if (depth === 0) {
      return html.slice(from, m.index);
    }
  }
  return undefined;
}

/** Removes every `<div hidden id="S:n">…</div>` block, honouring nested `<div>`s. */
function removeHiddenBlocks(html: string): string {
  let result = html;
  for (;;) {
    const open = result.match(/<div hidden id="S:\w+"[^>]*>/);
    if (!open || open.index === undefined) {
      return result;
    }
    const contentStart = open.index + open[0].length;
    const tagRe = /<(\/?)div\b[^>]*>/g;
    tagRe.lastIndex = contentStart;
    let depth = 1;
    let end = -1;
    for (let m = tagRe.exec(result); m !== null; m = tagRe.exec(result)) {
      depth += m[1] ? -1 : 1;
      if (depth === 0) {
        end = tagRe.lastIndex;
        break;
      }
    }
    if (end === -1) {
      return result; // malformed — bail rather than loop forever
    }
    result = result.slice(0, open.index) + result.slice(end);
  }
}

/**
 * Inline React's out-of-order Suspense boundaries into a complete, in-order
 * fragment.
 *
 * The streaming renderer emits a boundary that resolves after the shell as a
 * `<!--$?--><template id="B:n"></template><!--/$-->` placeholder, with the real
 * content appended later as `<div hidden id="S:n">…</div>` and an inline reveal
 * script that splices them together on the client. Static pages have no client
 * runtime (and the site CSP forbids inline scripts), so this performs the same
 * splice at build time: every placeholder is replaced by its content, then the
 * orphaned hidden blocks and reveal scripts are dropped.
 */
export function inlineDeferredBoundaries(html: string): string {
  if (!html.includes('<template id="B:')) {
    return html;
  }

  // Index every hidden content block by id (balanced so nested <div>s in the
  // content don't terminate it early).
  const contentById = new Map<string, string>();
  const openRe = /<div hidden id="S:(\w+)"[^>]*>/g;
  for (let open = openRe.exec(html); open !== null; open = openRe.exec(html)) {
    const inner = readBalancedDiv(html, openRe.lastIndex);
    if (inner !== undefined) {
      contentById.set(open[1] as string, inner);
    }
  }

  // Replace each pending boundary placeholder with its content. Content may
  // itself contain nested placeholders, so repeat until the document is stable.
  const placeholderRe = /<!--\$\?--><template id="B:(\w+)"[^>]*><\/template><!--\/\$-->/g;
  let next = html;
  for (let prev = ''; next !== prev; ) {
    prev = next;
    next = prev.replace(placeholderRe, (match, id: string) => {
      const content = contentById.get(id);
      return content === undefined ? match : `<!--$-->${content}<!--/$-->`;
    });
  }

  // Drop the now-duplicated hidden blocks and React's inline reveal scripts.
  next = removeHiddenBlocks(next);
  return next.replace(/<script>(?:(?!<\/script>)[\s\S])*?<\/script>/g, (script) =>
    /\$R[A-Z]/.test(script) ? '' : script,
  );
}

/**
 * Render a route to a complete, **zero-JavaScript** HTML document at build time.
 *
 * This is the SSG/ISR render path. Unlike the per-request SSR renderer it
 * carries no security context, no CSP nonce, no client hydration bootstrap and
 * no request data — a static page may not depend on the request (determinism
 * rule #1). Loaders run inside a frozen build-time context that throws
 * {@link StaticRenderViolation} on any request-scoped access.
 */
export async function renderStaticDocument(options: StaticRenderOptions): Promise<StaticRenderResult> {
  const { routes, route, pathname, params = {}, basename, timeoutMs = 10_000 } = options;

  const staticHandler = createStaticHandler(routes, basename ? { basename } : undefined);
  const url = new URL(pathname, 'http://localhost');
  const request = new Request(url.toString(), { method: 'GET' });

  const ctx = createStaticRenderContext(route, params) as unknown as Record<string, unknown>;

  let queryResult: StaticHandlerContext | Response;
  try {
    queryResult = (await runInContext(ctx as never, () => staticHandler.query(request))) as
      | StaticHandlerContext
      | Response;
  } catch (error) {
    // Re-throw determinism violations untouched so the build fails loudly.
    if (isStaticRenderViolation(error)) {
      throw error;
    }
    throw error;
  }

  // A loader threw a Response (redirect / error). Static pages should not
  // redirect, but surface it so the caller can decide (skip + warn).
  if (queryResult instanceof Response) {
    const location = queryResult.headers.get('Location') ?? undefined;
    return { html: '', status: queryResult.status, redirectLocation: location };
  }

  const context = queryResult as StaticHandlerContext;
  if (context.errors) {
    for (const err of Object.values(context.errors)) {
      if (isStaticRenderViolation(err)) {
        throw err;
      }
    }
  }

  const router = createStaticRouter(staticHandler.dataRoutes, context);
  const documentMeta = ensureDocumentMeta(ctx);

  const tree = React.createElement(
    IslandModeContext.Provider,
    { value: 'static' as const },
    React.createElement(
      CsrfTokenContext.Provider,
      { value: undefined },
      React.createElement(
        DocumentMetaContext.Provider,
        { value: documentMeta },
        React.createElement(StaticRouterProvider, { context, router, hydrate: false }),
      ),
    ),
  );

  const controller = new AbortController();
  const timeoutId = setTimeout(() => controller.abort(), timeoutMs);
  let bodyHtml: string;
  try {
    // Render inside the request-free context so AsyncLocalStorage-backed document
    // helpers (Style/Favicon/HeaderLink/Meta) resolve the same `documentMeta`
    // that `headHtml` reads below; otherwise their <head> contributions (fonts,
    // favicon, inline CSS) are silently dropped from the static output. The
    // context is the determinism proxy, so any request-scoped access during
    // render still throws StaticRenderViolation.
    bodyHtml = await runInContext(ctx as never, async () => {
      const stream = await renderToReadableStream(tree, {
        signal: controller.signal,
        onError: (err) => {
          if (isStaticRenderViolation(err)) throw err;
        },
      });
      await stream.allReady;
      return new Response(stream).text();
    });
  } finally {
    clearTimeout(timeoutId);
  }

  // React's streaming renderer emits any Suspense boundary that resolves after
  // the shell (here the route's lazy `<Outlet/>`) out-of-order: a
  // `<template id="B:n">` placeholder in the shell plus a trailing
  // `<div hidden id="S:n">…</div>`, spliced together on the client by inline
  // reveal scripts. A static page ships no React runtime and the site CSP
  // forbids inline scripts, so without this the main content would stay hidden,
  // leaving only the always-ready shell (nav + footer). Inline the boundaries
  // at build time so the document is complete, in-order and zero-JavaScript.
  bodyHtml = inlineDeferredBoundaries(bodyHtml);

  const docHelper = new SsrDocumentHelper(documentMeta);
  const headHtml = docHelper.headHtml || '<head></head>';
  const headWithExtra = options.headExtra ? headHtml.replace('</head>', `${options.headExtra}</head>`) : headHtml;

  // Only ship island JS when the page actually contains island markers — a
  // static page with no islands stays zero-JavaScript.
  const hasIslands = bodyHtml.includes(`<${ISLAND_TAG}`);
  const islandScriptTag =
    hasIslands && options.islandsScript
      ? `<script type="module" src="${escapeHtml(options.islandsScript)}"></script>`
      : '';
  const bodyExtra = `${options.bodyExtra ?? ''}${islandScriptTag}`;

  const html =
    `<!DOCTYPE html><html lang="${escapeHtml(docHelper.lang)}">${headWithExtra}` +
    `<body><div id="root">${bodyHtml}</div>${bodyExtra}</body></html>`;

  const status = context.statusCode && context.statusCode !== 200 ? context.statusCode : 200;
  return { html, status };
}

/**
 * Build an {@link HttpResponse} from a pre-rendered static HTML string.
 */
export function staticHtmlResponse(html: string, status = 200): HttpResponse {
  return new HttpResponse(html, {
    status,
    headers: { 'content-type': 'text/html;charset=utf-8' },
  });
}

/**
 * Build a redirect {@link HttpResponse}. Used when a static route's loader
 * resolves to a redirect during a live (non-prebuilt) render — e.g. legacy URL
 * redirects on an otherwise pre-rendered catch-all route.
 */
export function staticRedirectResponse(location: string, status = 301): HttpResponse {
  return new HttpResponse('', {
    status,
    headers: { location },
  });
}
