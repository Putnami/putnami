import { file, Glob, gzipSync, write } from 'bun';
import { copyFileSync, mkdirSync, readdirSync, statSync } from 'node:fs';
import { basename, resolve } from 'node:path';
import { useConfig, useLogger, type InferConfig } from '@putnami/runtime';
import {
  fileExists,
  GeneratorHelper,
  getDirectoryName,
  getProjectRoot,
  isAbsolutePath,
  joinPath,
  joinPosixPath,
  readFileContent,
  relativePath,
  toPosixPath,
} from '@putnami/utils';
import { applyPrefix } from '../api/api.utils';
import type { GenerateResult, Module, Plugin } from '../application';
import { HttpPlugin } from '../http/http.plugin';
import { HttpResponse } from '../http/http-response';
import { PutnamiConfig } from '../application/putnami.config';
import { loadRegisteredModule } from '../bundled/module-registry';
import { generatedLoaderKey, generatedLoaderSlot } from '../bundled/loader-slot';
import { getMimeFromPath } from './mime-types';
import { StaticConfig, type StaticConfigExtras } from './static.config';
import { publicFolder } from './static.utils';
import type { GeneratedHttpRoute } from '../http-routes/generation';

/** Name of the plugin variable the generated static loader declares and default-exports. */
const LOADER_VARIABLE = 'staticPlugin';

/** Loader family: `static-loader` for the first loading StaticPlugin, `static-<n>-loader` after it (ADR 0006). */
const LOADER_FAMILY = 'static';

/**
 * Where the StaticPlugin at `slot` stages its files and writes its loader.
 *
 * Slot 0 keeps the historical `.gen/public/` root and `.static.gen.ts`, so a
 * workload with one StaticPlugin builds the same bytes. Any other slot stages
 * under its own hidden folder of that root, `.static-<n>/`: the slot-0 scan
 * never matches a hidden entry, two plugins never overwrite each other's
 * `index.html`, and every file still ships inside the one public folder that
 * packaging copies and `publicFolder()` serves from.
 */
function slotLayout(slot: number): { publicSubdir: string; loaderFile: string } {
  return slot === 0
    ? { publicSubdir: '', loaderFile: '.static.gen' }
    : { publicSubdir: `.static-${slot}`, loaderFile: `.static-${slot}.gen` };
}

/**
 * Static file serving plugin.
 * Scans a directory for static files and registers routes for them.
 *
 * @example Basic usage
 * ```typescript
 * const app = application()
 *   .use(http())
 *   .use(staticFiles());
 * ```
 *
 * @example With prefix
 * ```typescript
 * app.use(staticFiles({ prefix: '/assets' }));
 * ```
 *
 * @example Custom cache duration
 * ```typescript
 * app.use(staticFiles({ cacheMaxAge: 604800 })); // 1 week
 * ```
 */
type StaticPluginConfig = InferConfig<typeof PutnamiConfig> & InferConfig<typeof StaticConfig>;

interface StaticRoute {
  filePath: string;
  gzipFilePath?: string;
  headers: Record<string, string>;
  etag: string;
  size: number;
  lastModified: string;
  gzipSize?: number;
  gzipLastModified?: string;
  gzipHeaders?: Record<string, string>;
}

export class StaticPlugin implements Plugin {
  config: StaticPluginConfig;
  private staticRoutes: Map<string, StaticRoute> = new Map();
  /**
   * Pre-loaded module to use instead of dynamic import.
   * When provided, warmup() skips the `import()` call and uses this module directly.
   * This enables bundled builds where dynamic imports cannot be resolved.
   */
  private preloadedModule?: Record<string, unknown>;

  constructor(config?: Partial<StaticPluginConfig> & StaticConfigExtras) {
    const putnami = useConfig(PutnamiConfig, { confInit: config });
    const staticConf = useConfig(StaticConfig, { confInit: config });
    this.config = { ...putnami, ...staticConf };
    this.preloadedModule = config?.preloadedModule;

    // Auto-detect scanPath if not provided
    if (!this.config.scanPath) {
      const pathToScan = joinPath(getProjectRoot(), this.config.publicFolder);
      if (fileExists(pathToScan)) {
        this.config.scanPath = pathToScan;
      }
    }
  }

  /**
   * Apply prefix to a route path.
   */
  private prefixPath(path: string): string {
    return applyPrefix(path, this.config.prefix);
  }

  /**
   * Generate ETag from file path and modification time.
   */
  private generateETag(filePath: string): string {
    try {
      const content = readFileContent(filePath);
      const hasher = new Bun.CryptoHasher('md5');
      hasher.update(content);
      const hash = hasher.digest('hex').substring(0, 16);
      return `"${hash}"`;
    } catch {
      return '';
    }
  }

  /**
   * Register a static file route.
   */
  routeStatic(path: string, staticFile: string, options: { gzip: boolean; mime: string }): this {
    const pub = publicFolder();
    const filePath = options.gzip ? joinPath(pub, staticFile.replace(/\.gz$/, '')) : joinPath(pub, staticFile);
    const gzipFilePath = options.gzip ? joinPath(pub, staticFile) : undefined;
    const etag = this.generateETag(gzipFilePath ?? filePath);

    const headers: Record<string, string> = {
      'Cache-Control': `public, max-age=${this.config.cacheMaxAge ?? 86_400}`,
      'Content-Type': options.mime,
    };
    if (etag) {
      headers['ETag'] = etag;
    }

    // Pre-compute file metadata at registration time (not per-request)
    const fileStat = this.tryStatSync(filePath);
    const route: StaticRoute = {
      filePath,
      gzipFilePath,
      headers,
      etag,
      size: fileStat?.size ?? 0,
      lastModified: fileStat?.mtime.toUTCString() ?? '',
    };

    if (gzipFilePath) {
      const gzipStat = this.tryStatSync(gzipFilePath);
      route.gzipSize = gzipStat?.size ?? 0;
      route.gzipLastModified = gzipStat?.mtime.toUTCString() ?? '';
      route.gzipHeaders = {
        ...headers,
        'Content-Encoding': 'gzip',
        'Content-Disposition': `inline; filename="${basename(filePath)}"`,
        Vary: 'Accept-Encoding',
      };
    }

    this.staticRoutes.set(path, route);
    return this;
  }

  private tryStatSync(filePath: string): { size: number; mtime: Date } | undefined {
    try {
      return statSync(filePath);
    } catch {
      return undefined;
    }
  }

  /**
   * Warmup: Register static routes with the HttpPlugin.
   */
  async warmup(app: Module): Promise<void> {
    const httpPlugin = await app.ensurePlugin(HttpPlugin);

    // Register all static routes
    for (const [path, route] of this.staticRoutes) {
      const routePath = this.prefixPath(path);
      httpPlugin.route('GET', routePath, (ctx) => serveStatic(ctx, route), {
        headMeta: (ctx) => buildStaticHeadResponse(ctx, route),
      });
    }

    // Load generated static routes if they exist
    if (this.config.skipLoading) {
      return;
    }

    const slot = this.loaderSlot(app);
    const routeLoaders = await this.resolveRouteLoaders(slot);

    let loadedRouteCount = 0;
    if (routeLoaders) {
      for (const loadedPlugin of Object.values(routeLoaders)) {
        if (loadedPlugin instanceof StaticPlugin) {
          // Apply this plugin's prefix to loaded routes
          for (const [path, route] of loadedPlugin.staticRoutes) {
            const routePath = this.prefixPath(path);
            httpPlugin.route('GET', routePath, (ctx) => serveStatic(ctx, route), {
              headMeta: (ctx) => buildStaticHeadResponse(ctx, route),
            });
            loadedRouteCount++;
          }
        }
      }
    }

    // A deployment that generated static assets but registered no routes for
    // them serves 404s for every public file — surface it instead of staying
    // silent. Typical cause: the generated static loader was not registered
    // for a bundled build and the dynamic-import fallback found nothing.
    if (this.staticRoutes.size === 0 && loadedRouteCount === 0 && this.hasGeneratedAssets(slot)) {
      useLogger('@putnami/application').warn(
        'StaticPlugin: no static routes registered (static loader missing); files under the public folder will 404',
      );
    }
  }

  /**
   * This plugin's slot among the application's StaticPlugins that load a
   * generated route module — every one without `skipLoading` — in module-tree
   * registration order. Configuration only: the packaged binary has no scan
   * folder, so `scanPath` would give the build and the binary different slots.
   */
  private loaderSlot(owner: Module | undefined): number {
    return generatedLoaderSlot(
      owner,
      this,
      (plugin): plugin is StaticPlugin => plugin instanceof StaticPlugin && !plugin.config.skipLoading,
    );
  }

  /**
   * Resolve the generated static route module of the plugin at `slot`:
   * preloaded > the loader registered under its own key > dynamic import of its
   * `.gen/src/static/.static[-<n>].gen.{js,ts}`.
   */
  private async resolveRouteLoaders(slot: number): Promise<Record<string, unknown> | undefined> {
    if (this.preloadedModule) {
      return this.preloadedModule;
    }
    const registered = await loadRegisteredModule(generatedLoaderKey(LOADER_FAMILY, slot));
    if (registered) {
      return registered;
    }
    // Try both .js (transpiled) and .ts (source) extensions
    const genBasePath = joinPath(getProjectRoot(), '.gen/src/static/', slotLayout(slot).loaderFile);
    const genPath = fileExists(`${genBasePath}.js`) ? `${genBasePath}.js` : `${genBasePath}.ts`;
    if (fileExists(genPath)) {
      return await import(genPath);
    }
    return undefined;
  }

  /**
   * Whether the generated public folder contains any files to serve.
   * `publicFolder()` creates the directory on access, so emptiness — not
   * existence — is the meaningful signal.
   */
  private hasGeneratedAssets(slot: number): boolean {
    const { publicSubdir } = slotLayout(slot);
    try {
      return readdirSync(publicSubdir ? joinPath(publicFolder(), publicSubdir) : publicFolder()).length > 0;
    } catch {
      return false;
    }
  }

  /**
   * Generate: Scan for static files, copy/compress to .gen, and create route loader.
   *
   * This method:
   * 1. Scans the configured scanPath for static files
   * 2. Copies and optionally compresses files to .gen/public/
   * 3. Generates a route loader TypeScript file
   * 4. Returns assets map for Phase 3 copy task to copy to dist
   */
  async generate(owner?: Module): Promise<GenerateResult> {
    if (this.config.skipLoading || !this.config.scanPath) {
      return {};
    }

    const slot = this.loaderSlot(owner);
    const { publicSubdir, loaderFile } = slotLayout(slot);
    /** A staged file's path relative to `publicFolder()`, which serves every slot. */
    const publicRef = (file: string): string => (publicSubdir ? `${publicSubdir}/${file}` : file);
    const projectRoot = getProjectRoot();
    const genSrcDir = '.gen/src/static/';
    const genPublicDir = publicSubdir ? `.gen/public/${publicSubdir}/` : '.gen/public/';

    // Anchored to the project like every path below: the working directory is
    // not the project when a test or another tool builds it.
    mkdirSync(joinPath(projectRoot, genSrcDir), { recursive: true });
    mkdirSync(joinPath(projectRoot, genPublicDir), { recursive: true });

    const sourcePath = isAbsolutePath(this.config.scanPath)
      ? this.config.scanPath
      : joinPath(projectRoot, this.config.scanPath);
    const generatedPublicPath = joinPath(projectRoot, genPublicDir);

    const source = discoverStaticSource(sourcePath);
    if (source.exists && sourcePath !== generatedPublicPath && !sourcePath.startsWith(generatedPublicPath)) {
      const sourceStat = statSync(sourcePath);
      if (sourceStat.isDirectory()) {
        copyDirRecursive(sourcePath, generatedPublicPath);
      } else {
        const targetPath = joinPath(generatedPublicPath, basename(sourcePath));
        mkdirSync(getDirectoryName(targetPath), { recursive: true });
        copyFileSync(sourcePath, targetPath);
      }
    }

    const staticLoaderPath = joinPath(projectRoot, genSrcDir, `${loaderFile}.ts`);

    const generator = new GeneratorHelper(staticLoaderPath);
    generator.appendHead(
      `// generated by the StaticPlugin exploring '${toPosixPath(relativePath(projectRoot, sourcePath))}'`,
    );
    generator.appendHead(`import { StaticPlugin } from "@putnami/application";`);
    // One statement per route, never a `new StaticPlugin().routeStatic(...).routeStatic(...)`
    // chain: the TypeScript checker recurses once per link when it resolves a chained
    // call, so a site with a few hundred assets overflows the Node stack during
    // `build~types`. Separate statements cost the checker constant depth each.
    generator.append(`const ${LOADER_VARIABLE} = new StaticPlugin();`);

    const assets: Record<string, string> = {};

    // Sort the scan: `Glob.scanSync` yields directory order, which differs between
    // machines and even between runs on the same machine. An unstable route order
    // rewrites this file on every generate, which misses the `build~types` cache
    // every run and makes the emitted bytes unreviewable. The scan yields native
    // separators; the paths become route keys and asset names in generated
    // source, so they take the forward-slash form on every platform.
    const scanner = new Glob('**/*.*');
    for (const fileRelativePath of [...scanner.scanSync({ cwd: generatedPublicPath })].map(toPosixPath).sort()) {
      if (fileRelativePath.endsWith('.gz')) {
        continue;
      }
      const fileAbsolutePath = joinPath(generatedPublicPath, fileRelativePath);
      const f = file(fileAbsolutePath);
      const compressed = this.config.compress && f.size > this.config.compress;

      const targetFile = compressed ? `${fileRelativePath}.gz` : fileRelativePath;
      // Copy to .gen/public/ (will be copied to dist by Phase 3)
      const targetFilePath = joinPath(projectRoot, genPublicDir, targetFile);
      mkdirSync(getDirectoryName(targetFilePath), { recursive: true });

      if (compressed) {
        // Keep both original and gzip so the server can negotiate encoding
        const originalTargetPath = joinPath(projectRoot, genPublicDir, fileRelativePath);
        if (originalTargetPath !== f.name) {
          await write(originalTargetPath, f);
        }
        await write(targetFilePath, gzipSync(await f.arrayBuffer()));
      } else if (targetFilePath !== f.name) {
        await write(targetFilePath, f);
      }

      const mime = getMimeFromPath(fileRelativePath);

      // Register index.html at folder root
      if (fileRelativePath.endsWith('index.html')) {
        generator.append(
          `${LOADER_VARIABLE}.routeStatic('${fileRelativePath.substring(0, fileRelativePath.length - 11)}', '${publicRef(targetFile)}', { gzip: ${compressed}, mime: '${mime}' });`,
        );
      }

      // Register .html without extension
      if (fileRelativePath.endsWith('.html')) {
        generator.append(
          `${LOADER_VARIABLE}.routeStatic('${fileRelativePath.substring(0, fileRelativePath.length - 5)}', '${publicRef(targetFile)}', { gzip: ${compressed}, mime: '${mime}' });`,
        );
      }

      // Register assets: relative path in dist -> absolute path in .gen
      // Phase 3 will copy from .gen/public/ to dist/public/
      assets[joinPosixPath(this.config.publicFolder, publicRef(targetFile))] = targetFilePath;
      if (compressed) {
        // Also include the original uncompressed file for clients that don't accept gzip
        const originalTargetPath = joinPath(projectRoot, genPublicDir, fileRelativePath);
        assets[joinPosixPath(this.config.publicFolder, publicRef(fileRelativePath))] = originalTargetPath;
      }

      // Register with full path
      generator.append(
        `${LOADER_VARIABLE}.routeStatic('${fileRelativePath}', '${publicRef(targetFile)}', { gzip: ${compressed}, mime: '${mime}' });`,
      );
    }

    generator.append(`export default ${LOADER_VARIABLE};`);
    generator.write();

    return {
      assets,
      httpRoutes: buildStaticHttpRoutes({
        projectRoot,
        sourcePath,
        sourceIsDirectory: source.isDirectory,
        prefix: this.config.prefix,
        files: source.files,
      }),
      exports: {
        [generatedLoaderKey(LOADER_FAMILY, slot)]: staticLoaderPath,
      },
    };
  }
}

function discoverStaticSource(sourcePath: string): {
  exists: boolean;
  isDirectory: boolean;
  files: string[];
} {
  if (!fileExists(sourcePath)) return { exists: false, isDirectory: true, files: [] };
  const sourceStat = statSync(sourcePath);
  if (!sourceStat.isDirectory()) {
    return { exists: true, isDirectory: false, files: [basename(sourcePath)] };
  }
  const files = [...new Glob('**/*.*').scanSync({ cwd: sourcePath })].filter((path) => !path.endsWith('.gz')).sort();
  return { exists: true, isDirectory: true, files };
}

export function buildStaticHttpRoutes(input: {
  projectRoot: string;
  sourcePath: string;
  sourceIsDirectory: boolean;
  prefix?: string;
  files: readonly string[];
}): GeneratedHttpRoute[] {
  if (input.files.length === 0) return [];
  const routes: GeneratedHttpRoute[] = [];
  const seen = new Set<string>();
  const sourceEvidence = safeEvidencePath(input.projectRoot, input.sourcePath);
  const normalizedPrefix = input.prefix ? applyPrefix('/', input.prefix) : '/';

  const add = (
    match: 'exact' | 'prefix',
    path: string,
    sourceKind: 'static-mount' | 'public-file',
    evidencePath?: string,
  ) => {
    const key = `${match}\0${path}`;
    if (seen.has(key)) return;
    seen.add(key);
    routes.push({
      match,
      path,
      methods: ['GET', 'HEAD'],
      publicEdge: true,
      provenance: {
        package: '@putnami/application',
        sourceKind,
        ...(evidencePath ? { evidencePath } : {}),
      },
    });
  };

  if (!input.sourceIsDirectory) {
    for (const file of input.files) {
      for (const route of staticFileAliases(file)) {
        add('exact', applyPrefix(route, input.prefix), 'public-file', sourceEvidence);
      }
    }
    return routes;
  }

  if (normalizedPrefix !== '/') {
    add('prefix', `${normalizedPrefix.replace(/\/+$/, '')}/`, 'static-mount', sourceEvidence);
    if (input.files.includes('index.html')) add('exact', normalizedPrefix, 'public-file', sourceEvidence);
    return routes;
  }

  const topLevelDirectories = new Set<string>();
  for (const file of input.files) {
    const normalized = file.replaceAll('\\', '/').replace(/^\/+/, '');
    const slash = normalized.indexOf('/');
    if (slash >= 0) {
      topLevelDirectories.add(normalized.slice(0, slash));
      continue;
    }
    const evidence = safeEvidencePath(input.projectRoot, joinPath(input.sourcePath, normalized));
    for (const route of staticFileAliases(normalized)) add('exact', route, 'public-file', evidence);
  }
  for (const directory of [...topLevelDirectories].sort()) {
    const indexFile = `${directory}/index.html`;
    if (input.files.includes(indexFile)) {
      add(
        'exact',
        `/${directory}`,
        'public-file',
        safeEvidencePath(input.projectRoot, joinPath(input.sourcePath, indexFile)),
      );
    }
    add(
      'prefix',
      `/${directory}/`,
      'static-mount',
      safeEvidencePath(input.projectRoot, joinPath(input.sourcePath, directory)),
    );
  }
  return routes;
}

function staticFileAliases(file: string): string[] {
  const route = `/${file.replaceAll('\\', '/').replace(/^\/+/, '')}`;
  const aliases = [route];
  if (route.endsWith('.html')) aliases.push(route.slice(0, -5) || '/');
  if (route.endsWith('/index.html')) aliases.push(route.slice(0, -'index.html'.length));
  return aliases;
}

function safeEvidencePath(projectRoot: string, path: string): string | undefined {
  const evidence = relativePath(projectRoot, path).replaceAll('\\', '/');
  if (!evidence || evidence === '..' || evidence.startsWith('../') || isAbsolutePath(evidence)) return undefined;
  return evidence;
}

/**
 * Factory function for creating a StaticPlugin with auto-scanning.
 *
 * During build, scans the configured public folder for static files,
 * optionally compresses them, and generates route registrations.
 *
 * @example Basic usage
 * ```typescript
 * app.use(staticFiles());
 * ```
 *
 * @example With prefix
 * ```typescript
 * app.use(staticFiles({ prefix: '/assets' }));
 * ```
 *
 * @example Custom cache duration (1 week)
 * ```typescript
 * app.use(staticFiles({ cacheMaxAge: 604800 }));
 * ```
 */
export function staticFiles(config: Partial<StaticPluginConfig> & StaticConfigExtras = {}): StaticPlugin {
  // Auto-detect scanPath if not provided in config
  if (!config.scanPath) {
    const resolvedConfig = useConfig(PutnamiConfig, { confInit: config });
    const pathToScan = joinPath(getProjectRoot(), resolvedConfig.publicFolder);
    if (fileExists(pathToScan)) {
      config.scanPath = pathToScan;
    }
  }

  return new StaticPlugin(config);
}

function buildStaticHeadResponse(ctx: { headers: Headers }, route: StaticRoute): HttpResponse | undefined {
  const { etag } = route;

  // 304 Not Modified
  if (etag && ctx.headers.get('If-None-Match') === etag) {
    return new HttpResponse(undefined, { status: 304, headers: { ETag: etag } });
  }

  const useGzip = route.gzipFilePath && ctx.headers.get('Accept-Encoding')?.includes('gzip');

  const baseHeaders = useGzip && route.gzipHeaders ? route.gzipHeaders : route.headers;
  const responseHeaders: Record<string, string> = {
    ...baseHeaders,
    'Content-Length': String(useGzip ? route.gzipSize : route.size),
    'Last-Modified': (useGzip ? route.gzipLastModified : route.lastModified) || '',
  };

  return new HttpResponse(undefined, { status: 200, headers: responseHeaders });
}

function serveStatic(ctx: { headers: Headers }, route: StaticRoute): HttpResponse {
  const { filePath, gzipFilePath, headers, etag, gzipHeaders } = route;

  if (etag && ctx.headers.get('If-None-Match') === etag) {
    return new HttpResponse(undefined, { status: 304, headers: { ETag: etag } });
  }

  // Negotiate encoding: serve gzip only if the client accepts it
  if (gzipFilePath && gzipHeaders && ctx.headers.get('Accept-Encoding')?.includes('gzip')) {
    return new HttpResponse(file(gzipFilePath), { headers: gzipHeaders });
  }

  return new HttpResponse(file(filePath), { headers });
}

function copyDirRecursive(sourceDir: string, destDir: string, destRoot: string = resolve(destDir)): void {
  mkdirSync(destDir, { recursive: true });
  for (const entry of readdirSync(sourceDir)) {
    const sourcePath = joinPath(sourceDir, entry);
    // A slot's staging folder can sit inside the folder it copies (a scanPath
    // of `.gen/public`): never copy the destination into itself.
    if (resolve(sourcePath) === destRoot) continue;
    const destPath = joinPath(destDir, entry);
    const stat = statSync(sourcePath);
    if (stat.isDirectory()) {
      copyDirRecursive(sourcePath, destPath, destRoot);
    } else {
      mkdirSync(getDirectoryName(destPath), { recursive: true });
      copyFileSync(sourcePath, destPath);
    }
  }
}
