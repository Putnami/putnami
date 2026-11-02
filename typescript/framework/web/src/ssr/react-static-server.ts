import type { RouteHandler } from '@putnami/application';
import { getBuildInfo, getProjectRoot, joinPath } from '@putnami/utils';
import { useLogger } from '@putnami/runtime';
import type { RouteObject } from 'react-router';
import type { LocaleServing } from '../shared/i18n/locale';
import type { ReactPluginConfig } from './react-plugin';
import type { PutnamiReactConfigExtras } from './react-ssr.config';
import { serving } from './i18n/negotiation';
import type { I18nRuntimeOptions } from './i18n/runtime';
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
    private readonly i18n: () => I18nRuntimeOptions | undefined = () => undefined,
  ) {}

  /** The hosts already reported as serving every locale under its prefix. */
  private readonly prefixHosts = new Set<string>();

  /**
   * Absolute directory holding pre-rendered static HTML files
   * (`.gen/<public>/static`).
   */
  getStaticOutputDir(): string {
    return joinPath(this.getPublicOutputDir(), 'static');
  }

  /** Absolute directory of the build's public output (`.gen/<public>`). */
  getPublicOutputDir(): string {
    return this.config.assetsDir ?? joinPath(getProjectRoot(), '.gen', this.config.publicFolder);
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
      // The build renders a localized application's pages as its site and its
      // domains serve them; another host serves every locale under its prefix.
      renderLive: (ctx) => {
        const i18n = this.i18n();
        if (!i18n || Object.keys(i18n.settings.domains).length === 0) return undefined;
        if (serving(ctx, i18n.settings) !== 'prefix') return undefined;
        const host = ctx.host();
        if (!this.prefixHosts.has(host)) {
          this.prefixHosts.add(host);
          useLogger('@putnami/web').warn(
            `'${host}' is neither the host of i18n.siteUrl nor a locale domain: its static pages render under their locale prefix`,
          );
        }
        return (pathname, params) => this.render(pathname, params, route, 'prefix');
      },
      currentVersion: () => getBuildInfo()?.version,
    });
  }

  private render(
    pathname: string,
    params: StaticPathParams,
    route: string,
    served?: LocaleServing,
  ): Promise<StaticRenderResult> {
    return renderStaticDocument({
      routes: this.routes(),
      route,
      pathname,
      params,
      basename: this.basename(),
      timeoutMs: this.config.ssrTimeout,
      islandsScript: this.islandsScript(),
      i18n: this.i18n(),
      served,
    });
  }
}
