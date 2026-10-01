import { Config, Default, Int, Optional } from '@putnami/runtime';

export const StaticConfig = Config('putnami.static', {
  compress: Default(Int, 1024),
  cacheMaxAge: Default(Int, 86_400),
  scanPath: Optional(String),
  prefix: Optional(String),
});

/**
 * Extra options for the staticFiles() factory that are not part of the Config schema.
 */
export interface StaticConfigExtras {
  /**
   * Pre-loaded module to use instead of dynamic import in warmup.
   * Pass the already-imported generated `.static.gen.ts` module
   * to enable bundled builds where dynamic imports cannot be resolved.
   *
   * @example
   * ```typescript
   * import * as staticModule from './.gen/src/static/.static.gen.ts';
   * app.use(staticFiles({ preloadedModule: staticModule }));
   * ```
   */
  preloadedModule?: Record<string, unknown>;
}
