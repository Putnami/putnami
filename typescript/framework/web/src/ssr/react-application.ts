import {
  CsrfMiddleware,
  HttpPlugin,
  PutnamiConfig,
  type RouteHandler,
  type RouteOptions,
  SecurityHeadersMiddleware,
} from '@putnami/application';
import { useConfig } from '@putnami/runtime';
import React, { type ReactNode } from 'react';
import type { RouteObject } from 'react-router';
import { createPageElement } from '../shared/page-element';
import { RouteTreeHelper } from '../shared/route-tree.utils';
import type {
  ErrorModule,
  LayoutModule,
  LazyErrorModule,
  LazyLayoutModule,
  LazyLoaderModule,
  LazyNotFoundModule,
  LazyPageModule,
  LoaderModule,
  NotFoundModule,
  PageModule,
} from './module.types';
import { ReactErrorRegistry } from './react-error-registry';
import { type MiddlewareResolver, ReactModuleCache } from './react-module-cache';
import type { ReactPluginConfig } from './react-plugin';
import { ReactScriptRegistry } from './react-script-registry';
import { ReactStaticServer } from './react-static-server';
import { PutnamiReactConfig, type PutnamiReactConfigExtras } from './react-ssr.config';
import { RouteMiddlewareResolver } from './route-middleware-resolver';
import { applyMiddlewareSource, type MiddlewareSource } from './route-middleware.utils';
import { resolveLayoutOrDefinition, resolvePageComponent, resolvePageOrDefinition } from './route-module.utils';
import {
  createLazyPageRenderer,
  createPageRenderer,
  ensurePageNode,
  type PageRenderContext,
  type ReactPageOptions,
  resolveLazyPageData,
  registerRouteAction,
  registerRouteLoader,
} from './route-registration.utils';
import type { StaticPathParams } from './static';

/**
 * ReactApplication manages React routing configuration with SSR support.
 *
 * Uses an internal HttpPlugin to handle HTTP routes for pages, loaders, and
 * actions. Page and layout assembly lives here; error/not-found registration,
 * static (SSG/ISR) serving, and script endpoints are delegated to focused
 * collaborators ({@link ReactErrorRegistry}, {@link ReactStaticServer},
 * {@link ReactScriptRegistry}).
 */
export class ReactApplication {
  private readonly reactRouter: RouteObject[] = [{ path: '/' }];
  private readonly routeHelper: RouteTreeHelper<RouteObject>;
  private readonly middlewareResolver: RouteMiddlewareResolver;
  private readonly cache: ReactModuleCache;
  private _basename?: string;
  private readonly scripts: ReactScriptRegistry;
  private readonly statics: ReactStaticServer;
  private readonly errors: ReactErrorRegistry;

  constructor(
    private config: ReactPluginConfig & PutnamiReactConfigExtras = {
      ...useConfig(PutnamiConfig),
      ...useConfig(PutnamiReactConfig),
    },
    private httpPlugin = new HttpPlugin(),
    routeHelper?: RouteTreeHelper<RouteObject>,
  ) {
    this.cache = new ReactModuleCache(this.config.moduleCacheSize);
    this.middlewareResolver = new RouteMiddlewareResolver(this.cache);
    this.routeHelper =
      routeHelper ??
      new RouteTreeHelper<RouteObject>(this.reactRouter[0], {
        isLayout: (node) => node.id?.endsWith('-layout') === true,
        createLayoutNode: (path, id) => ({ path, id }),
      });
    this.scripts = new ReactScriptRegistry(this.httpPlugin, this.config);
    this.statics = new ReactStaticServer(
      this.config,
      () => this.reactRouter,
      () => this._basename,
      () => this.scripts.islandsScript,
    );
    this.errors = new ReactErrorRegistry(
      this.cache,
      (route) => this.findNearestLayout(route),
      (route) => this.middlewareResolver.collectLayoutMiddleware(route),
      (route, options) => {
        this.reactPage(route, options);
      },
    );

    // Secure-by-default: the framework ships a full CSRF token
    // round-trip (SSR provides the `_csrf` cookie token to <CsrfInput />, the
    // client action handler echoes it as X-CSRF-Token), so the server must
    // actually validate it. CsrfMiddleware issues the double-submit `_csrf`
    // cookie on safe methods, publishes the effective token on the request
    // context for the page renderer (first-visit no-JS forms), and rejects
    // action() POSTs whose header/form-field token doesn't match the cookie
    // (403). Routes registered with `csrfExempt` (e.g. api() routes) are
    // skipped. Prepended BEFORE SecurityHeadersMiddleware (whose prepend below
    // then lands outermost) so CSRF rejections still get the security headers.
    // When this plugin is merged into an HttpPlugin that already configures
    // CSRF, HttpPlugin keeps that single configured instance (and its options).
    // Opt out with `react({ csrf: false })` (or `putnami.react.csrf`).
    if (this.config.csrf !== false && typeof this.httpPlugin.prepend === 'function') {
      this.httpPlugin.prepend(CsrfMiddleware());
    }

    // Secure-by-default: SSR ships inline hydration <script> blocks, so XSS
    // defense-in-depth (CSP + nosniff + Referrer-Policy) must be ON by default.
    // SecurityHeadersMiddleware weaves the per-request hydration nonce (published
    // on the request context as CSP_NONCE_CONTEXT_KEY by page.renderer) into the
    // CSP script-src after the handler runs, so the framework's own inline
    // scripts are permitted under a strict, nonce-based policy. Opt out with
    // `react({ securityHeaders: false })` (or `putnami.react.securityHeaders`).
    // The middleware is idempotent (it never clobbers headers an inner handler
    // already set unless forced), so stacking another securityHeaders() plugin
    // is safe. Guarded for the partial HttpPlugin test doubles that omit prepend.
    if (this.config.securityHeaders !== false && typeof this.httpPlugin.prepend === 'function') {
      this.httpPlugin.prepend(SecurityHeadersMiddleware());
    }
  }

  /**
   * Set the basename for this ReactApplication.
   * Used by ReactPlugin to apply module-level path at warmup time.
   * Updates the React Router root path for correct SSR URL matching.
   */
  setBasename(basename: string): void {
    this._basename = basename;
    // Update the root route path so React Router SSR matches prefixed URLs
    if (this.reactRouter[0]) {
      this.reactRouter[0].path = basename;
    }
  }

  /**
   * Get the basename, if set.
   */
  getBasename(): string | undefined {
    return this._basename;
  }

  /**
   * Get the internal HttpPlugin for merging routes.
   */
  getHttpPlugin(): HttpPlugin {
    return this.httpPlugin;
  }

  private renderContext(): PageRenderContext {
    return {
      config: this.config,
      routes: () => this.reactRouter,
      basename: () => this._basename,
      hydrateScript: () => this.scripts.hydrateScript || '/hydrate.main.js',
    };
  }

  private routeOptions(options: RouteOptions = {}): RouteOptions {
    let opts = options;
    if (this.config.securityHeaders === false) {
      opts = { ...opts, securityHeaders: false };
    }
    if (this.config.csrf === false) {
      // The opt-out must survive merging into a root HttpPlugin where another
      // module (or the server options) wires CsrfMiddleware: mark this app's
      // routes exempt so any CSRF middleware skips them.
      opts = { ...opts, csrfExempt: true };
    }
    return opts;
  }

  /**
   * Register a page-serving GET handler with the standard HTML route options.
   *
   * Every page route (eager, lazy, and static) is served with the same option
   * shape — base {@link routeOptions} plus an HTML-first `accept` and the
   * resolved `statusCode`. Centralizing it here keeps that route-assembly
   * invariant in one place; behavior is identical to inlining the options.
   */
  private registerPageGet(httpRoute: string, handler: RouteHandler, statusCode: number): void {
    this.httpPlugin.get(httpRoute, handler, {
      ...this.routeOptions(),
      accept: ['text/html', '*/*'],
      statusCode,
    });
  }

  /**
   * Adds a layout component to the specified route.
   *
   * @param route - The route path (e.g., '/dashboard')
   * @param options - Layout module (lazy or eager) and optional loader module
   * @returns The ReactApplication instance for chaining
   */
  reactLayout(
    route: string,
    options: { layout: LayoutModule | LazyLayoutModule; loader?: LoaderModule | LazyLoaderModule },
  ) {
    const node = this.ensureLayoutRoute(route);
    node.id ||= 'root-layout';
    if (!node.id.endsWith('-layout')) {
      node.id += '-layout';
    }
    if (node.element && typeof node.element !== 'string') {
      // There was a page here before the layout, move it to a child
      node.children ||= [];
      let pageRoute = node.children.find((c: RouteObject) => c.path === '' && !c.id?.endsWith('-layout'));
      if (!pageRoute) {
        pageRoute = {
          path: '',
          index: true,
        };
        node.children.push(pageRoute);
      }
      pageRoute.id = `${node.id.slice(0, -7)}-page`;
      pageRoute.element = node.element;
      pageRoute.loader = node.loader;
      pageRoute.action = node.action;
    }

    const isLazyLayout = typeof options.layout === 'function';
    const isLazyLoader = typeof options.loader === 'function';

    if (isLazyLayout) {
      // Lazy layout: create a lazy React component that loads on first render.
      //
      // A lazy layout's `.secure()`/middleware cannot be known until its module
      // loads. Registering it with the middleware resolver lets descendant
      // handlers resolve — and enforce — that security at request time, closing
      // the fail-open where a child page/loader/action under a lazy secured
      // layout could otherwise ship with no server-side enforcement. The SSR
      // generator still imports layouts eagerly; this branch covers hand-written
      // lazy layouts.
      const lazyLayout = options.layout as LazyLayoutModule;
      this.middlewareResolver.registerLazyLayout(route, lazyLayout);
      const LazyLayoutComponent = React.lazy(async () => {
        const layoutModule = await this.cache.loadModule(`layout:${route}`, lazyLayout);
        const { element } = resolveLayoutOrDefinition(layoutModule);
        return { default: () => element };
      });
      node.element = React.createElement(React.Suspense, { fallback: null }, React.createElement(LazyLayoutComponent));
    } else {
      // Eager layout: the generator uses this path so security middleware is
      // known synchronously and route registration can fail closed.
      const { element: layoutElement, layoutDef } = resolveLayoutOrDefinition(options.layout as LayoutModule);
      if (layoutElement) {
        node.element = layoutElement;
      }
      if (layoutDef?.middleware) {
        this.middlewareResolver.registerEagerLayout(route, layoutDef.middleware);
      }
    }

    if (options.loader) {
      // When the layout (or any ancestor) is lazy, its `.secure()` chain is only
      // knowable at request time, so pass a resolver thunk instead of an array.
      const middleware: MiddlewareSource = this.middlewareResolver.hasLazyLayoutAncestor(route)
        ? () => this.middlewareResolver.resolveLayoutMiddleware(route)
        : this.middlewareResolver.collectLayoutMiddleware(route);
      registerRouteLoader({
        httpPlugin: this.httpPlugin,
        cache: this.cache,
        route,
        httpRoute: toHttpRoute(route),
        node,
        loader: options.loader,
        isLazy: isLazyLoader,
        cacheKeyPrefix: 'layout-loader',
        suffix: '-layout.json',
        middleware,
        routeOptions: this.routeOptions(),
      });
    }

    return this;
  }

  /**
   * Adds a page component to the specified route.
   *
   * @param route - The route path (e.g., '/users/[id]')
   * @param options - Page options including component (lazy or eager), loader, action, and status code
   * @returns The ReactApplication instance for chaining
   */
  reactPage(route: string, options: ReactPageOptions) {
    const pageNode = ensurePageNode(this.routeHelper, route);
    const httpRoute = toHttpRoute(route);
    const staticMeta = options.static;
    const notFoundMiddleware = options.notFoundMiddleware;

    const isLazyPage = typeof options.page === 'function';
    const isLazyLoader = typeof options.loader === 'function';
    const isLazyAction = typeof options.action === 'function';

    // A lazy ancestor layout's `.secure()` is only knowable once its module
    // loads, so every handler under it must resolve its middleware at request
    // time rather than from the still-empty eager map. This is a fail-closed
    // security boundary, not a rendering optimization.
    const deferLayoutMiddleware = this.middlewareResolver.hasLazyLayoutAncestor(route);
    // Async resolver for this route's full middleware (ancestor layout chain,
    // loading lazy layouts, composed with the page's own middleware).
    const composePageMiddleware: MiddlewareResolver = (r, pageDef, nfMw) =>
      this.middlewareResolver.resolvePageMiddleware(r, pageDef, nfMw);

    // For lazy pages, we can't know the statusCode until the module loads.
    // Default to 200; the renderer overrides it on first request.
    let statusCode = options.statusCode ?? 200;
    // The route's composed page middleware (layout chain + the page's own),
    // shared by the page renderer, loader, and action. An array on the eager
    // fast path; a thunk when a lazy layout forces request-time resolution.
    let pageMiddleware: MiddlewareSource;

    if (isLazyPage) {
      const lazyPage = options.page as LazyPageModule;
      // Resolved on first request via the page-data cache, which composes the
      // ancestor layout chain (lazy layouts included) with the page's own.
      pageMiddleware = async () =>
        (await resolveLazyPageData(this.cache, composePageMiddleware, route, lazyPage, notFoundMiddleware)).middleware;

      const LazyPageComponent = React.lazy(async () => {
        const pageModule = await this.cache.loadModule(`page:${route}`, lazyPage);
        const { component, pageDef } = resolvePageComponent(pageModule);
        // Update statusCode if defined in page definition
        if (pageDef?.statusCode) {
          statusCode = pageDef.statusCode;
        }
        return { default: component };
      });
      // The lazy component stands for the page component inside the page boundary.
      pageNode.element = createPageElement(LazyPageComponent);

      // The lazy renderer resolves and applies its own middleware on first
      // request, so it is registered directly. Skipped for static routes (below).
      if (!staticMeta) {
        const lazyRenderer = createLazyPageRenderer(
          this.renderContext(),
          this.cache,
          composePageMiddleware,
          route,
          lazyPage,
          notFoundMiddleware,
        );
        this.registerPageGet(httpRoute, lazyRenderer, statusCode);
      }
    } else {
      // Eager page: used by eager not-found registration and direct/manual routes.
      const { page: pageElement, pageDef } = resolvePageOrDefinition(options.page as ReactNode | PageModule);
      statusCode = pageDef?.statusCode ?? options.statusCode ?? 200;
      pageMiddleware = deferLayoutMiddleware
        ? () => this.middlewareResolver.resolvePageMiddleware(route, pageDef, notFoundMiddleware)
        : this.middlewareResolver.resolvePageMiddlewareSync(route, pageDef, notFoundMiddleware);

      if (pageElement) {
        pageNode.element = pageElement;
        if (!staticMeta) {
          const renderer = createPageRenderer(this.renderContext(), route, pageDef?.cache);
          this.registerPageGet(httpRoute, applyMiddlewareSource(renderer, pageMiddleware), statusCode);
        }
      }
    }

    // Static routes (SSG/ISR) serve pre-rendered, zero-JS HTML from the build
    // output instead of rendering per request.
    if (staticMeta) {
      this.registerPageGet(httpRoute, this.statics.buildStaticServe(route, staticMeta), statusCode);
    }

    if (options.loader) {
      registerRouteLoader({
        httpPlugin: this.httpPlugin,
        cache: this.cache,
        route,
        httpRoute,
        node: pageNode,
        loader: options.loader,
        isLazy: isLazyLoader,
        cacheKeyPrefix: 'loader',
        suffix: '.json',
        middleware: pageMiddleware,
        routeOptions: this.routeOptions(),
      });
    }

    if (options.action) {
      registerRouteAction({
        httpPlugin: this.httpPlugin,
        cache: this.cache,
        route,
        httpRoute,
        node: pageNode,
        action: options.action,
        isLazy: isLazyAction,
        middleware: pageMiddleware,
        routeOptions: this.routeOptions(),
      });
    }

    return this;
  }

  /**
   * Adds an error boundary component to the specified route.
   *
   * @param route - The route path (e.g., '/')
   * @param options - Error module (lazy or eager)
   */
  reactError(route: string, options: { error: ErrorModule | LazyErrorModule }) {
    this.errors.register(route, options);
    return this;
  }

  /**
   * Adds a not-found page to the specified route.
   *
   * @param route - The route path (e.g., '/')
   * @param options - NotFound module (lazy or eager)
   */
  reactNotFound(route: string, options: { notFound: NotFoundModule | LazyNotFoundModule }) {
    this.errors.registerNotFound(route, options);
    return this;
  }

  private ensureLayoutRoute(route: string): RouteObject {
    return this.routeHelper.ensureLayoutRoute(route);
  }

  private findNearestLayout(route: string): { layout: RouteObject; remainingPath: string } {
    return this.routeHelper.findNearestLayout(route);
  }

  /**
   * Absolute directory holding pre-rendered static HTML files
   * (`.gen/<public>/static`).
   */
  getStaticOutputDir(): string {
    return this.statics.getStaticOutputDir();
  }

  /**
   * Render a route to a complete, zero-JavaScript HTML document.
   *
   * Used by the build pipeline to emit SSG/ISR output and at runtime to refresh
   * stale ISR pages.
   */
  prerender(pathname: string, params: StaticPathParams = {}, route = pathname) {
    return this.statics.prerender(pathname, params, route);
  }

  /**
   * Register the islands hydration entry. Static pages that contain island
   * markers load this script to hydrate their islands; pages without islands
   * stay zero-JavaScript.
   */
  reactIslandsScript(route: string, path: string) {
    this.scripts.registerIslands(route, path);
    return this;
  }

  /**
   * Registers a static script file to be served (typically for hydration).
   */
  reactScript(route: string, path: string, isEntry = true) {
    this.scripts.register(route, path, isEntry);
    return this;
  }
}

/**
 * Convert React Router param format (`:param`) to bracket format (`[param]`)
 * used by the HTTP router.
 *
 * @example toHttpRoute('/tasks/:id') => '/tasks/[id]'
 * @example toHttpRoute('/users/:id/posts/:postId') => '/users/[id]/posts/[postId]'
 */
function toHttpRoute(route: string): string {
  return route.replace(/:([a-zA-Z_]\w*)/g, '[$1]');
}
