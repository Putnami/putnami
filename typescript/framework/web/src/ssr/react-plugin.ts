import {
  HttpPlugin,
  loadRegisteredModule,
  type Module,
  type Plugin,
  PutnamiConfig,
  applyPrefix,
} from '@putnami/application';
import { useConfig, type InferConfig, useLogger } from '@putnami/runtime';
import {
  fileExists,
  getDirectoryName,
  getExternalCaller,
  getProjectRoot,
  isAbsolutePath,
  joinPath,
  relativePath,
} from '@putnami/utils';
import type { ReactApplication } from './react-application.js';
import { PutnamiReactConfig, type PutnamiReactConfigExtras } from './react-ssr.config';

export type ReactPluginConfig = InferConfig<typeof PutnamiConfig> & InferConfig<typeof PutnamiReactConfig>;

/**
 * Check if an object looks like a ReactApplication instance using duck typing.
 * This is necessary because instanceof checks fail across different package instances
 * (e.g., when the plugin runs from CLI bundle and user code imports from npm package).
 */
function isReactApplicationLike(obj: unknown): obj is ReactApplication {
  if (!obj || typeof obj !== 'object') return false;
  const app = obj as Record<string, unknown>;
  return (
    typeof app['getHttpPlugin'] === 'function' &&
    typeof app['reactLayout'] === 'function' &&
    typeof app['reactPage'] === 'function' &&
    typeof app['reactScript'] === 'function' &&
    typeof app['setBasename'] === 'function'
  );
}

/**
 * React SSR plugin for Putnami Framework
 *
 * Generation is now handled by the command hook (bin/generate.ts).
 * This plugin handles runtime warmup to load generated routes.
 */
export class ReactPlugin implements Plugin {
  reactApplicationPath: string;
  /**
   * Pre-loaded module to use instead of dynamic import.
   * When provided, warmup() skips the `import()` call and uses this module directly.
   * This enables bundled builds where dynamic imports cannot be resolved.
   */
  private preloadedModule?: Record<string, unknown>;

  constructor(config: ReactPluginConfig & PutnamiReactConfigExtras) {
    const projectRoot = getProjectRoot();
    const relativeScanDir = relativePath(projectRoot, config.scanPath || joinPath(projectRoot, 'src', 'app'));
    // Use absolute path for .gen directory to avoid cwd-dependent relative path resolution
    const genScanDir = joinPath(projectRoot, '.gen', relativeScanDir);
    this.reactApplicationPath = joinPath(genScanDir, '.react-application.gen.tsx');
    this.preloadedModule = config.preloadedModule;
  }

  async warmup(app: Module) {
    const logger = useLogger('@putnami/web');
    const debug = app.buildOptions?.debug;
    const log = debug ? (msg: string) => logger.debug(msg) : undefined;

    log?.(`ReactPlugin.warmup: reactApplicationPath=${this.reactApplicationPath}`);
    log?.(`ReactPlugin.warmup: file exists=${fileExists(this.reactApplicationPath)}`);

    // Resolve module: preloaded > registered loader > dynamic import
    let routeLoaders: Record<string, unknown> | undefined;
    if (this.preloadedModule) {
      log?.('ReactPlugin.warmup: using preloaded module');
      routeLoaders = this.preloadedModule;
    } else {
      routeLoaders = await loadRegisteredModule('react-loader');
      if (routeLoaders) {
        log?.('ReactPlugin.warmup: using registered module loader');
      } else if (this.reactApplicationPath && fileExists(this.reactApplicationPath)) {
        // Handle both absolute and relative paths
        const importPath = isAbsolutePath(this.reactApplicationPath)
          ? this.reactApplicationPath
          : joinPath(getProjectRoot(), this.reactApplicationPath);

        log?.(`ReactPlugin.warmup: importing from ${importPath}`);
        routeLoaders = await import(importPath);
      } else {
        log?.('ReactPlugin.warmup: No reactApplicationPath or file does not exist, skipping');
      }
    }

    if (routeLoaders) {
      const httpPlugin = await app.ensurePlugin(HttpPlugin);
      log?.(`ReactPlugin.warmup: module exports: ${Object.keys(routeLoaders).join(', ')}`);

      const prefix = app.getPath();
      for (const [key, reactApplication] of Object.entries(routeLoaders)) {
        // Use duck typing instead of instanceof to handle cross-package instances
        if (isReactApplicationLike(reactApplication)) {
          log?.(`ReactPlugin.warmup: Found ReactApplication-like export '${key}', merging routes`);
          if (prefix) {
            log?.(`ReactPlugin.warmup: Applying module path prefix '${prefix}'`);
            reactApplication.setBasename(prefix);
            httpPlugin.mergeWithPrefix(reactApplication.getHttpPlugin(), prefix, applyPrefix);
          } else {
            httpPlugin.merge(reactApplication.getHttpPlugin());
          }
        } else {
          log?.(`ReactPlugin.warmup: Export '${key}' is not ReactApplication-like`);
        }
      }
    }
  }
}

export const react = (pConfig?: Partial<ReactPluginConfig> & PutnamiReactConfigExtras) => {
  const putnami = useConfig(PutnamiConfig, { confInit: pConfig });
  const reactConf = useConfig(PutnamiReactConfig, { confInit: pConfig });
  const config = { ...putnami, ...reactConf, page: pConfig?.page, notFound: pConfig?.notFound };

  if (config.autoScan && !config.scanPath && !(config.scanRoots && config.scanRoots.length > 0)) {
    const caller = getExternalCaller();
    if (caller?.filePath) {
      const pathToScan = `${getDirectoryName(caller.filePath)}/${config.scanFolder}`;
      if (fileExists(pathToScan)) {
        config.scanPath = pathToScan;
      }
    }
  }
  return new ReactPlugin(config);
};
