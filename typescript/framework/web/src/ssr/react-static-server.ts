import type { RouteHandler } from '@putnami/application';
import { getBuildInfo, getProjectRoot, joinPath } from '@putnami/utils';
import type { RouteObject } from 'react-router';
import type { ReactPluginConfig } from './react-plugin';
import type { PutnamiReactConfigExtras } from './react-ssr.config';
import { renderStaticDocument, type StaticRenderResult } from './static-render';
import { staticServeHandler } from './static-serve.utils';
import { type StaticPathParams, type StaticRouteMeta, toStaticConfig } from './static';

type ReactRuntimeConfig = ReactPluginConfig & PutnamiReactConfigExtras;

/**
 * Serves and pre-renders static (SSG/ISR) routes for a ReactApplication. Owns
 * the static output directory and the zero-JavaScript render pipeline shared by
 * the build step and runtime ISR refresh.
 */
export class ReactStaticServer {
  constructor(
    private readonly config: ReactRuntimeConfig,
    private readonly routes: () => RouteObject[],
    private readonly basename: () => string | undefined,
    private readonly islandsScript: () => string | undefined,
  ) {}

  /**
   * Absolute directory holding pre-rendered static HTML files
   * (`.gen/<public>/static`).
   */
  getStaticOutputDir(): string {
    const base = this.config.assetsDir ?? joinPath(getProjectRoot(), '.gen', this.config.publicFolder);
    return joinPath(base, 'static');
  }

  /**
   * Render a route to a complete, zero-JavaScript HTML document.
   *
   * Used by the build pipeline to emit SSG/ISR output and at runtime to refresh
   * stale ISR pages. Loaders run with a request-free context; touching request
   * data throws {@link StaticRenderViolation}.
   */
  prerender(pathname: string, params: StaticPathParams = {}, route = pathname): Promise<StaticRenderResult> {
    return this.render(pathname, params, route);
  }

  /**
   * Build the runtime handler that serves a `.static()` route from pre-rendered,
   * zero-JavaScript HTML, falling back to a live render for paths missing from
   * the build output.
   */
  buildStaticServe(route: string, staticMeta: StaticRouteMeta): RouteHandler {
    return staticServeHandler({
      route,
      staticConfig: toStaticConfig(staticMeta),
      staticDir: this.getStaticOutputDir(),
      basename: this.basename(),
      render: (pathname, params) => this.render(pathname, params, route),
      currentVersion: () => getBuildInfo()?.version,
    });
  }

  private render(pathname: string, params: StaticPathParams, route: string): Promise<StaticRenderResult> {
    return renderStaticDocument({
      routes: this.routes(),
      route,
      pathname,
      params,
      basename: this.basename(),
      timeoutMs: this.config.ssrTimeout,
      islandsScript: this.islandsScript(),
    });
  }
}
