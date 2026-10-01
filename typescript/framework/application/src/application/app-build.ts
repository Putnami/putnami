import type { Module } from './module';
import type { BuildOptions, GenerateResult, Plugin } from './module.types';

/**
 * Propagate build options to a module and all of its sub-modules recursively.
 */
export function propagateBuildOptions(modules: Module[], options?: BuildOptions): void {
  const propagate = (mod: Module) => {
    mod.buildOptions = options;
    for (const child of mod.getModules()) {
      propagate(child);
    }
  };
  for (const mod of modules) {
    propagate(mod);
  }
}

/**
 * Run `generate()` on every plugin (in parallel) and merge their emitted
 * assets and exports into a single {@link GenerateResult}.
 */
export async function runGenerate(plugins: Array<{ plugin: Plugin; owner: Module }>): Promise<GenerateResult> {
  const assets: Record<string, string> = {};
  const exports: Record<string, string> = {};
  const httpRoutes: NonNullable<GenerateResult['httpRoutes']> = [];

  const results = await Promise.all(
    plugins
      .filter(({ plugin }) => typeof plugin.generate === 'function')
      .map(({ plugin, owner }) => plugin.generate?.(owner)),
  );

  mergeGenerateResults(results, assets, exports, httpRoutes);
  return { assets, exports, httpRoutes };
}

/**
 * Run `postGenerate()` on every plugin that implements it, after all plugins'
 * `generate()` hooks have resolved. This is the ordering barrier the parallel
 * `generate()` pass lacks: a plugin whose output depends on another plugin's
 * emitted artifact (e.g. the client generator reading the OpenAPI spec) runs
 * here, once every spec/loader is guaranteed written to disk.
 */
export async function runPostGenerate(
  plugins: Array<{ plugin: Plugin; owner: Module }>,
  generated?: GenerateResult,
): Promise<GenerateResult> {
  const assets: Record<string, string> = {};
  const exports: Record<string, string> = {};
  const httpRoutes: NonNullable<GenerateResult['httpRoutes']> = [];

  const results = await Promise.all(
    plugins
      .filter(({ plugin }) => typeof plugin.postGenerate === 'function')
      .map(({ plugin, owner }) => plugin.postGenerate?.(owner, generated)),
  );

  mergeGenerateResults(results, assets, exports, httpRoutes);
  return { assets, exports, httpRoutes };
}

/**
 * Merge a list of {@link GenerateResult}s into the supplied asset/export maps.
 */
function mergeGenerateResults(
  results: Array<GenerateResult | undefined>,
  assets: Record<string, string>,
  exports: Record<string, string>,
  httpRoutes: NonNullable<GenerateResult['httpRoutes']>,
): void {
  for (const result of results) {
    if (result?.assets) {
      for (const [key, value] of Object.entries(result.assets)) {
        assets[key] = value as string;
      }
    }
    if (result?.exports) {
      for (const [key, value] of Object.entries(result.exports)) {
        exports[key] = value as string;
      }
    }
    if (result?.httpRoutes) httpRoutes.push(...result.httpRoutes);
  }
}
