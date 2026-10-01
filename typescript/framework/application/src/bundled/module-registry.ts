/**
 * Module loader registry for bundled builds.
 *
 * Plugins use dynamic `import()` calls with runtime-computed paths to load generated
 * route/handler files. In bundled builds, bundlers cannot resolve these dynamic imports
 * because the paths are variables.
 *
 * The module registry solves this by providing a map of async loaders. In dev,
 * loaders can wrap `import()` with known paths that bundlers statically analyze;
 * in packaged binaries, loaders can resolve statically imported modules.
 *
 * The generated bundled serve entrypoint reads loader keys and module paths
 * from the validated capability manifest and registers them before app startup.
 * Packaged entrypoints may statically import the generated modules and return
 * them from the loader so single-binary builds include `.gen/src` code. Plugins
 * then check the registry in warmup() before falling back to dynamic import.
 *
 * @example Generated bundled serve entrypoint
 * ```typescript
 * import { registerModuleLoader } from '@putnami/application';
 *
 * import * as staticLoader from './static/.static.gen.ts';
 * registerModuleLoader('static-loader', () => Promise.resolve(staticLoader));
 *
 * import { app } from '../../src/main';
 * await app().start();
 * ```
 */

type ModuleLoader = () => Promise<Record<string, unknown>>;

const loaders = new Map<string, ModuleLoader>();

/**
 * Register an async module loader by key.
 * Called by the generated bundled serve entrypoint before app startup.
 *
 * @param key - Module key matching the manifest route-schema name
 *   (e.g., 'react-loader', 'api-loader', 'static-loader', 'events-loader')
 * @param loader - Async module provider, commonly `() => import('./path')` in dev
 *   or `() => Promise.resolve(staticModule)` in packaged binaries.
 */
export function registerModuleLoader(key: string, loader: ModuleLoader): void {
  loaders.set(key, loader);
}

/**
 * Load a pre-registered module by key.
 * Called by plugins in warmup() before falling back to dynamic import.
 *
 * @param key - Module key to look up
 * @returns The loaded module, or undefined if no loader is registered for this key
 */
export async function loadRegisteredModule(key: string): Promise<Record<string, unknown> | undefined> {
  const loader = loaders.get(key);
  if (loader) {
    return await loader();
  }
  return undefined;
}

/**
 * Check if any module loaders have been registered.
 * Useful for plugins to quickly skip registry lookups in non-bundled mode.
 */
export function hasRegisteredModules(): boolean {
  return loaders.size > 0;
}
