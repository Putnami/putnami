#!/usr/bin/env bun
/**
 * @putnami/web:generate CLI command
 *
 * Standalone command for generating React client bundles and SSR routes.
 * Designed to be invoked as a subprocess with JSONL output.
 *
 * Usage:
 *   bunx @putnami/web:generate --putnami-context <path> --output jsonl
 */

import { Glob, write } from 'bun';
import { statSync } from 'node:fs';
import { basename } from 'node:path';
import { PutnamiConfig } from '@putnami/application';
import { writeHttpRoutesFragment } from '@putnami/application/http-routes';
import { useConfig } from '@putnami/runtime';
import {
  fileExists,
  getDirectoryName,
  isAbsolutePath,
  joinPath,
  joinPosixPath,
  readPackageJson,
  readProjectConfigFile,
  relativePath,
} from '@putnami/utils';
import {
  emitArtifact,
  emitLog,
  emitProgress,
  type HookContext,
  type HookResult,
  runHookCommand,
  standardHookModel,
} from '@putnami/utils/hooks';
import { islandIdFromFile } from '../src/client/island/island-types';
import type { CompilationResult } from '../src/ssr/generator/build';
import { detectIslandStrategy, IslandClientGenerator } from '../src/ssr/generator/island.generator';
import { ReactClientGenerator } from '../src/ssr/generator/react-client.generator';
import { ReactApplicationGenerator } from '../src/ssr/generator/react-ssr.generator';
import {
  detectRouteLoaderInject,
  detectStaticConfig,
  prerenderStaticRoutes,
  type LayoutLoaderCandidate,
  type StaticRouteSpec,
} from '../src/ssr/generator/static-prerender';
import {
  buildManifest,
  formatManifestTable,
  type ManifestIsland,
  type ManifestRoute,
  serializeManifest,
} from '../src/ssr/manifest';
import type { ReactApplication } from '../src/ssr/react-application';
import { buildWebAssetHttpRoutes, buildWebPageHttpRoutes, type WebPageRouteFact } from '../src/ssr/http-routes';
import { PutnamiReactConfig } from '../src/ssr/react-ssr.config';
import { applyRoutePrefix, asRoute } from '../src/ssr/react-ssr.utils';
import { resolveScanRoots } from '../src/ssr/scan-roots.utils';
import { toStaticMeta } from '../src/ssr/static';
import { proveStaticRouteSafety } from '../src/ssr/static-di';
import { resolveGenerationConfig } from './generate-config';

const generateModel = standardHookModel.extend({
  name: 'generate',
  description: 'Generate React client bundles and SSR routes',
});

/**
 * Run the React generation process.
 */
async function runGenerate(_options: unknown, context: HookContext): Promise<HookResult> {
  const { projectRoot } = context;

  if (context.debug) {
    emitLog('debug', `Working directory: ${projectRoot}`);
  }

  const projectConfig = readProjectConfigFile(projectRoot);
  const configuredPutnamiBlock = projectConfig?.options?.['@putnami/web:generate'];
  const projectJson = readPackageJson(joinPath(projectRoot, 'package.json')) as
    | { name?: string; putnami?: unknown }
    | undefined;
  const legacyPutnamiBlock = projectJson?.putnami;
  // Resolve and validate before route discovery or any artifact write. Project
  // configuration is explicit; only package.json retains the legacy flat
  // React shape.
  const generationConfig = resolveGenerationConfig(legacyPutnamiBlock, configuredPutnamiBlock);
  const putnami = useConfig(PutnamiConfig, { confInit: generationConfig.shared });
  const reactConf = useConfig(PutnamiReactConfig, {
    confInit: generationConfig.react,
  });
  const config = { ...putnami, ...reactConf };
  const scanRoots = resolveScanRoots(projectRoot, config);
  if (scanRoots.length === 0) {
    emitLog('info', 'No React app directory found, skipping generation');
    return { exports: {}, assets: {} };
  }

  const defaultScanPath = joinPath(projectRoot, 'src', 'app');
  const outputScanPath = config.scanPath
    ? isAbsolutePath(config.scanPath)
      ? config.scanPath
      : joinPath(projectRoot, config.scanPath)
    : defaultScanPath;
  const outputRelatifScanDir = relativePath(projectRoot, outputScanPath);

  const genScanDirAbsolute = joinPath(projectRoot, '.gen', outputRelatifScanDir);
  const resolvedGenScanDir = genScanDirAbsolute;

  const reactApplicationPath = joinPath(genScanDirAbsolute, '.react-application.gen.tsx');
  const reactClientPath = joinPath(genScanDirAbsolute, '.react-client.gen.tsx');
  const islandsEntryPath = joinPath(genScanDirAbsolute, '.react-islands.gen.ts');

  const appGenerator = new ReactApplicationGenerator(reactApplicationPath, outputRelatifScanDir, outputScanPath);
  const clientGenerator = new ReactClientGenerator(reactClientPath, outputRelatifScanDir, outputScanPath);
  const islandGenerator = new IslandClientGenerator(islandsEntryPath, outputRelatifScanDir);

  // Scan for React files
  const layouts: string[] = [];
  const pages: string[] = [];
  const errors: string[] = [];
  const notFounds: string[] = [];
  const staticSpecs: StaticRouteSpec[] = [];
  // Mutable manifest rows, keyed by route so the prerender pass can refine
  // each static route's hydration mode after rendering.
  type ManifestRow = Pick<ManifestRoute, 'route' | 'mode' | 'hydration' | 'revalidate' | 'diProvenStatic'>;
  const manifestRoutes = new Map<string, ManifestRow>();
  const manifestIslands: ManifestIsland[] = [];
  const httpPageRoutes: WebPageRouteFact[] = [];

  emitProgress('Scanning for React components', 10, 'scan');

  for (const root of scanRoots) {
    emitLog(
      'info',
      `Scanning React files in ${root.relativePath}${root.routePrefix ? ` (prefix ${root.routePrefix})` : ''}`,
    );

    const relativeGenDir = relativePath(resolvedGenScanDir, root.scanPath);
    const options = {
      routePrefix: root.routePrefix,
      scannedDir: root.scanPath,
      relativeGenDir,
    };
    const layoutLoaders: LayoutLoaderCandidate[] = [];

    for (const f of new Glob('**/layout.tsx').scanSync(root.scanPath)) {
      layouts.push(joinPath(root.relativePath, f));
      appGenerator.addLayout(f, options);
      clientGenerator.addLayout(f, options);
      const loaderPath = joinPath(root.scanPath, getDirectoryName(f), 'layout.loader.ts');
      if (fileExists(loaderPath)) {
        layoutLoaders.push({ route: applyRoutePrefix(asRoute(f), root.routePrefix), absPath: loaderPath });
      }
    }
    for (const f of new Glob('**/page.tsx').scanSync(root.scanPath)) {
      pages.push(joinPath(root.relativePath, f));
      // Detect `.static()` so the route is registered as SSG/ISR and pre-rendered.
      const staticConfig = await detectStaticConfig(joinPath(root.scanPath, f));
      const staticMeta = staticConfig ? toStaticMeta(staticConfig) : undefined;
      appGenerator.addPage(f, { ...options, staticMeta });
      clientGenerator.addPage(f, options);
      const route = applyRoutePrefix(asRoute(f), root.routePrefix);
      httpPageRoutes.push({
        route,
        evidencePath: joinPath(root.relativePath, f),
        hasAction: fileExists(joinPath(root.scanPath, getDirectoryName(f), 'action.ts')),
      });
      if (staticConfig) {
        staticSpecs.push({ route, staticConfig });
        // DI-graph static-safety proof. The web build subprocess has no
        // application DI registry, so the proof is bounded to the route/layout
        // loader roots visible from this route. Routes with DI roots or dynamic
        // tag/filter selectors fall back to the runtime guard (hybrid).
        const injectMeta = await detectRouteLoaderInject(
          route,
          joinPath(root.scanPath, getDirectoryName(f), 'loader.ts'),
          layoutLoaders,
        );
        const proof = proveStaticRouteSafety(
          { route, roots: injectMeta.tokens, dynamicRoots: injectMeta.dynamic },
          () => undefined,
        );
        // Hydration starts at `none`; the prerender pass upgrades to `islands`
        // for static pages that emit island markers.
        manifestRoutes.set(route, {
          route,
          mode: staticConfig.mode,
          hydration: 'none',
          diProvenStatic: proof.diProven,
          ...(staticConfig.revalidate ? { revalidate: staticConfig.revalidate } : {}),
        });
      } else {
        manifestRoutes.set(route, { route, mode: 'ssr', hydration: 'full' });
      }
    }
    for (const f of new Glob('**/error.tsx').scanSync(root.scanPath)) {
      errors.push(joinPath(root.relativePath, f));
      appGenerator.addError(f, options);
      clientGenerator.addError(f, options);
    }
    for (const f of new Glob('**/not-found.tsx').scanSync(root.scanPath)) {
      notFounds.push(joinPath(root.relativePath, f));
      appGenerator.addNotFound(f, options);
      clientGenerator.addNotFound(f, options);
    }
    for (const f of new Glob('**/*.island.tsx').scanSync(root.scanPath)) {
      const id = islandIdFromFile(joinPath(root.routePrefix ?? '', f));
      appGenerator.addIsland(f, id, options);
      islandGenerator.add(f, id, relativeGenDir);
      manifestIslands.push({ id, strategy: await detectIslandStrategy(joinPath(root.scanPath, f)) });
    }
  }

  emitLog(
    'info',
    `Found ${layouts.length} layouts, ${pages.length} pages, ${errors.length} errors, ${notFounds.length} not-founds`,
  );

  if (pages.length === 0 && layouts.length === 0) {
    emitLog('info', 'No React components found, skipping generation');
    return { exports: {}, assets: {} };
  }

  emitProgress('Writing client generator', 30, 'generate');
  clientGenerator.write();
  emitArtifact('write', relativePath(projectRoot, reactClientPath));

  emitProgress('Building client bundles', 50, 'build');
  const addClientScripts: CompilationResult[] = await clientGenerator.build();

  for (const script of addClientScripts) {
    emitArtifact('write', script.path, `Built ${script.isEntry ? 'entry' : 'chunk'}: ${script.path}`);
  }

  // Build the islands hydration bundle (one entry + per-island chunks).
  let islandScripts: CompilationResult[] = [];
  if (islandGenerator.count > 0) {
    emitProgress('Building island bundles', 70, 'islands');
    emitLog('info', `Found ${islandGenerator.count} island(s)`);
    islandGenerator.write();
    islandScripts = await islandGenerator.build();
    for (const script of islandScripts) {
      emitArtifact('write', script.path, `Built island ${script.isEntry ? 'entry' : 'chunk'}: ${script.path}`);
    }
  }

  emitProgress('Writing SSR generator', 80, 'generate');
  appGenerator.addClientScripts(addClientScripts);
  if (islandScripts.length > 0) {
    appGenerator.addIslandScripts(islandScripts);
  }
  appGenerator.write();
  emitArtifact('write', relativePath(projectRoot, reactApplicationPath));

  // Pre-render static routes (SSG/ISR) to zero-JS HTML.
  if (staticSpecs.length > 0) {
    emitProgress('Pre-rendering static routes', 90, 'prerender');
    const staticDir = joinPath(projectRoot, '.gen', config.publicFolder, 'static');
    try {
      const appModule = (await import(reactApplicationPath)) as { default: ReactApplication };
      const prerendered = await prerenderStaticRoutes(appModule.default, staticSpecs, staticDir, (msg) =>
        emitLog('info', msg),
      );
      for (const p of prerendered) {
        emitArtifact(
          'write',
          joinPath(config.publicFolder, 'static', p.file),
          `Pre-rendered ${p.pathname} (${p.mode})`,
        );
        // A static route that emitted island markers hydrates as `islands`.
        if (p.hasIslands) {
          const row = manifestRoutes.get(p.route);
          if (row) row.hydration = 'islands';
        }
        if (p.route.includes('*')) {
          for (const fact of httpPageRoutes) {
            if (fact.route !== p.route) continue;
            if (!fact.expandedPaths) fact.expandedPaths = [];
            fact.expandedPaths.push(p.pathname);
          }
        }
      }
      emitLog('info', `Pre-rendered ${prerendered.length} static page(s)`);
    } catch (error) {
      // Determinism violations and other fatal pre-render errors fail the build.
      emitLog('error', `Static pre-render failed: ${error instanceof Error ? error.message : String(error)}`);
      throw error;
    }
  }

  // Emit the per-route determinism manifest (route · mode · hydration · JS budget).
  const sumGzBytes = (scripts: CompilationResult[]): number =>
    scripts.reduce((total, s) => {
      try {
        return total + statSync(joinPath(projectRoot, '.gen', config.publicFolder, s.path)).size;
      } catch {
        return total;
      }
    }, 0);

  const manifest = buildManifest({
    routes: [...manifestRoutes.values()],
    islands: manifestIslands,
    hydrateBytes: sumGzBytes(addClientScripts),
    islandsBytes: sumGzBytes(islandScripts),
  });
  const manifestPath = joinPath(projectRoot, '.gen', 'putnami-web-manifest.json');
  await write(manifestPath, serializeManifest(manifest));
  emitArtifact('write', relativePath(projectRoot, manifestPath));
  emitLog('info', `Web rendering manifest (${manifest.totals.routes} routes):\n${formatManifestTable(manifest)}`);

  // Collect assets
  const assets: Record<string, string> = {};
  for (const script of [...addClientScripts, ...islandScripts]) {
    const absolutePath = joinPath(projectRoot, '.gen', config.publicFolder, script.path);
    // An asset name is a forward-slash path on every platform.
    const assetKey = joinPosixPath(config.publicFolder, script.path);
    assets[assetKey] = absolutePath;
  }

  const projectName =
    context.projectName ||
    (projectConfig as { name?: string } | undefined)?.name ||
    projectJson?.name ||
    basename(projectRoot);
  const httpRoutesFragment = writeHttpRoutesFragment(projectRoot, 'web', projectName, [
    ...buildWebPageHttpRoutes(httpPageRoutes),
    ...buildWebAssetHttpRoutes(
      config.publicFolder,
      [...addClientScripts, ...islandScripts].map((script) => script.route),
    ),
  ]);
  emitArtifact('write', relativePath(projectRoot, httpRoutesFragment));

  emitProgress('Generation complete', 100, 'done');

  return {
    exports: {
      'react-loader': reactApplicationPath,
      'react-client-loader': reactClientPath,
    },
    assets,
  };
}

// Only run when executed directly, not when imported
if (import.meta.main) {
  runHookCommand({
    model: generateModel,
    extension: '@putnami/web',
    hook: 'preBuild',
    version: '0.0.3',
    run: runGenerate,
  });
}
