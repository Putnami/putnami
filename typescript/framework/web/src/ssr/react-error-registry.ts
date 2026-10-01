import type { HttpMiddleware } from '@putnami/application';
import React, { type ReactNode } from 'react';
import type { RouteObject } from 'react-router';
import { LazyNotFoundErrorBoundary, NotFoundErrorBoundary } from './error-boundaries';
import type {
  ErrorModule,
  LazyErrorModule,
  LazyNotFoundModule,
  LazyPageModule,
  NotFoundModule,
  PageModule,
} from './module.types';
import type { ReactModuleCache } from './react-module-cache';
import { normalizeRoute } from './route-cache.utils';
import { composeMiddleware } from './route-middleware.utils';
import { resolveErrorOrDefinition, resolveNotFoundOrDefinition } from './route-module.utils';
import type { ReactPageOptions } from './route-registration.utils';

/** Resolves the nearest layout node for a route. */
type FindNearestLayout = (route: string) => { layout: RouteObject; remainingPath: string };

/**
 * Registers error boundaries and not-found pages for a ReactApplication.
 *
 * Owns error-middleware tracking and wires not-found components both as
 * catch-all page routes and as layout error boundaries, so a 404 thrown by a
 * child route loader renders the not-found component instead of the default
 * error boundary.
 */
export class ReactErrorRegistry {
  private readonly errorMiddleware = new Map<string, HttpMiddleware[]>();

  constructor(
    private readonly cache: ReactModuleCache,
    private readonly findNearestLayout: FindNearestLayout,
    private readonly collectLayoutMiddleware: (route: string) => HttpMiddleware[] | undefined,
    private readonly registerPage: (route: string, options: ReactPageOptions) => void,
  ) {}

  /**
   * Add an error boundary component to the nearest layout for `route`.
   *
   * @param route - The route path (e.g., '/')
   * @param options - Error module (lazy or eager)
   */
  register(route: string, options: { error: ErrorModule | LazyErrorModule }): void {
    const { layout } = this.findNearestLayout(route);
    const isLazyError = typeof options.error === 'function';

    if (isLazyError) {
      const lazyError = options.error as LazyErrorModule;
      const LazyErrorComponent = React.lazy(async () => {
        const errorModule = await this.cache.loadModule(`error:${route}`, lazyError);
        const { element, errorDef } = resolveErrorOrDefinition(errorModule);
        if (errorDef?.middleware && errorDef.middleware.length > 0) {
          this.errorMiddleware.set(normalizeRoute(route), [...errorDef.middleware]);
        }
        return { default: () => element };
      });
      layout.errorElement = React.createElement(
        React.Suspense,
        { fallback: null },
        React.createElement(LazyErrorComponent),
      );
    } else {
      const { element: errorElement, errorDef } = resolveErrorOrDefinition(options.error as ErrorModule);
      if (errorElement) {
        layout.errorElement = errorElement;
      }
      if (errorDef?.middleware && errorDef.middleware.length > 0) {
        this.errorMiddleware.set(normalizeRoute(route), [...errorDef.middleware]);
      }
    }
  }

  /**
   * Add a not-found page to `route`, registered as a catch-all page route and
   * as the layout error boundary for 404s thrown by child loaders.
   *
   * @param route - The route path (e.g., '/')
   * @param options - NotFound module (lazy or eager)
   */
  registerNotFound(route: string, options: { notFound: NotFoundModule | LazyNotFoundModule }): void {
    const isLazyNotFound = typeof options.notFound === 'function';

    if (isLazyNotFound) {
      const lazyNotFound = options.notFound as LazyNotFoundModule;

      // Convert LazyNotFoundModule to LazyPageModule for the catch-all page.
      const lazyPageAdapter: LazyPageModule = async () => {
        const notFoundModule = await lazyNotFound();
        return { default: notFoundModule.default } as PageModule;
      };

      this.registerPage(`${route}*`, {
        page: lazyPageAdapter,
        statusCode: 404,
      });

      // Set up lazy error element on the layout.
      const { layout } = this.findNearestLayout(route);
      const existingErrorElement = layout.errorElement;
      const LazyNotFoundComponent = React.lazy(async () => {
        const notFoundModule = await this.cache.loadModule(`not-found:${route}`, lazyNotFound);
        const { element } = resolveNotFoundOrDefinition(notFoundModule);
        return { default: () => element };
      });

      layout.errorElement = React.createElement(LazyNotFoundErrorBoundary, {
        lazyNotFoundElement: React.createElement(
          React.Suspense,
          { fallback: null },
          React.createElement(LazyNotFoundComponent),
        ),
        fallbackErrorElement: existingErrorElement,
      });
    } else {
      const { element: notFoundElement, notFoundDef } = resolveNotFoundOrDefinition(options.notFound as NotFoundModule);

      // Compose layout + not-found middleware.
      const layoutMw = this.collectLayoutMiddleware(route);
      const notFoundMw =
        notFoundDef?.middleware && notFoundDef.middleware.length > 0 ? [...notFoundDef.middleware] : undefined;
      const middleware = composeMiddleware(layoutMw, notFoundMw);

      // Register as a catch-all page route — pass the component as a ReactNode.
      this.registerPage(`${route}*`, {
        page: notFoundElement as ReactNode,
        statusCode: 404,
        notFoundMiddleware: middleware,
      });

      // Also set an errorElement on the layout so a child loader's 404 Response
      // renders the not-found component instead of the default error boundary.
      if (notFoundElement) {
        const { layout } = this.findNearestLayout(route);
        const existingErrorElement = layout.errorElement;
        layout.errorElement = React.createElement(NotFoundErrorBoundary, {
          notFoundElement: notFoundElement as unknown as ReactNode,
          fallbackErrorElement: existingErrorElement,
        });
      }
    }
  }
}
