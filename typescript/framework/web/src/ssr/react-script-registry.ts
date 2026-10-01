import type { HttpPlugin, RouteOptions } from '@putnami/application';
import { getProjectRoot, joinPath } from '@putnami/utils';
import type { ReactPluginConfig } from './react-plugin';
import type { PutnamiReactConfigExtras } from './react-ssr.config';
import { registerScriptEndpoint } from './route-endpoint.utils';

type ReactRuntimeConfig = ReactPluginConfig & PutnamiReactConfigExtras;

/**
 * Registers static script endpoints (hydration entry, islands entry) and tracks
 * the active hydration and islands script URLs consumed by the SSR renderers.
 */
export class ReactScriptRegistry {
  private _hydrateScript?: string;
  private _islandsScript?: string;

  constructor(
    private readonly httpPlugin: HttpPlugin,
    private readonly config: ReactRuntimeConfig,
  ) {}

  /** URL of the hydration entry script, when one has been registered. */
  get hydrateScript(): string | undefined {
    return this._hydrateScript;
  }

  /** URL of the islands hydration entry, when one has been registered. */
  get islandsScript(): string | undefined {
    return this._islandsScript;
  }

  /**
   * Register the islands hydration entry. Static pages that contain island
   * markers load this script to hydrate their islands; pages without islands
   * stay zero-JavaScript.
   */
  registerIslands(route: string, path: string): void {
    this._islandsScript = route;
    registerScriptEndpoint(this.httpPlugin, route, path, this.outputDir(), this.routeOptions());
  }

  /**
   * Register a static script file to be served (typically for hydration). The
   * first `.js` entry script becomes the page hydration script.
   */
  register(route: string, path: string, isEntry = true): void {
    if (route.endsWith('.js') && isEntry) {
      this._hydrateScript = route;
    }
    registerScriptEndpoint(this.httpPlugin, route, path, this.outputDir(), this.routeOptions());
  }

  private outputDir(): string {
    return this.config.assetsDir ?? joinPath(getProjectRoot(), '.gen', this.config.publicFolder);
  }

  private routeOptions(): RouteOptions {
    return this.config.securityHeaders === false ? { securityHeaders: false } : {};
  }
}
