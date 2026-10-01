#!/usr/bin/env bun
/**
 * @putnami/application:generate CLI command
 *
 * Standalone command for running the Application build phase.
 * Designed to be invoked as a subprocess with JSONL output.
 *
 * Usage:
 *   bunx @putnami/application:generate --putnami-context <path> --output jsonl
 */

import { copyFileSync, mkdirSync, readdirSync, statSync } from 'node:fs';
import { basename } from 'node:path';
import { emitInfraRequirements } from '@putnami/runtime';
import {
  fileExists,
  getDirectoryName,
  getWorkspaceRoot,
  joinPath,
  readPackageJson,
  readProjectConfigFile,
  relativePath,
  toPosixPath,
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
import { activateBuildTimeConfig } from './_activate-config';
import { type EntryPointFailure, findApplication, formatEntryPointFailures } from './_find-application';
import { writeHttpRoutesFragment } from '../src/http-routes/generation';

const generateModel = standardHookModel.extend({
  name: 'generate',
  description: 'Run the Application build phase to generate code and assets',
});

type BuildAssetCopy = {
  from: string;
  to: string;
};

/**
 * Copy the assets declared in putnami.json `options.generate.assets`. The older
 * keys (`options["@putnami/application:generate"].assets`, package.json
 * `putnami.application.assets` / `putnami.build.assets`) are read after it, as
 * recorded in the compatibility table of tooling/cli/doc/21-compatibility-and-migration.md.
 */
function copyPackageAssets(projectPath: string): Record<string, string> {
  const projectConfig = readProjectConfigFile(projectPath);
  const configuredAssets =
    projectConfig?.options?.['generate']?.['assets'] ||
    projectConfig?.options?.['@putnami/application:generate']?.['assets'];
  const optionAssets = Array.isArray(configuredAssets) ? (configuredAssets as BuildAssetCopy[]) : undefined;

  const projectJson = readPackageJson(joinPath(projectPath, 'package.json')) as
    | {
        putnami?: {
          build?: { assets?: BuildAssetCopy[] };
          application?: { assets?: BuildAssetCopy[] };
        };
      }
    | undefined;
  const legacyAssets = projectJson?.putnami?.application?.assets || projectJson?.putnami?.build?.assets;
  const configAssets = optionAssets || legacyAssets;

  if (!Array.isArray(configAssets) || configAssets.length === 0) {
    return {};
  }

  const generatedRoot = joinPath(projectPath, '.gen');
  const assets: Record<string, string> = {};

  for (const entry of configAssets) {
    if (!entry?.from || !entry?.to) {
      continue;
    }
    const fromSpec = entry.from;
    const toSpec = entry.to.replace(/^\/+/, '');
    const sourceRoot = fromSpec.startsWith('/') ? getWorkspaceRoot() : projectPath;
    const sourcePath = joinPath(sourceRoot, fromSpec.replace(/^\/+/, ''));

    if (!fileExists(sourcePath)) {
      emitLog('warn', `skip asset copy (missing): ${sourcePath}`);
      continue;
    }

    const stat = statSync(sourcePath);
    if (stat.isDirectory()) {
      copyDirToGenerated(sourcePath, sourcePath, generatedRoot, toSpec, assets);
    } else {
      const destRelative = toSpec.endsWith('/') ? joinPath(toSpec, basename(sourcePath)) : toSpec;
      const destPath = joinPath(generatedRoot, destRelative);
      mkdirSync(getDirectoryName(destPath), { recursive: true });
      copyFileSync(sourcePath, destPath);
      assets[toPosixPath(destRelative)] = destPath;
    }
  }

  return assets;
}

function copyDirToGenerated(
  sourceRoot: string,
  currentSource: string,
  generatedRoot: string,
  destBase: string,
  assets: Record<string, string>,
): void {
  for (const entry of readdirSync(currentSource)) {
    const entryPath = joinPath(currentSource, entry);
    const stat = statSync(entryPath);
    if (stat.isDirectory()) {
      copyDirToGenerated(sourceRoot, entryPath, generatedRoot, destBase, assets);
      continue;
    }
    const relativeFromSource = relativePath(sourceRoot, entryPath);
    const destRelative = joinPath(destBase, relativeFromSource);
    const destPath = joinPath(generatedRoot, destRelative);
    mkdirSync(getDirectoryName(destPath), { recursive: true });
    copyFileSync(entryPath, destPath);
    // An asset name is a forward-slash path on every platform.
    assets[toPosixPath(destRelative)] = destPath;
  }
}

/**
 * Run the Application generation process.
 */
async function runGenerate(_options: unknown, context: HookContext): Promise<HookResult> {
  const { projectRoot, debug } = context;

  if (debug) {
    emitLog('debug', `Starting pre-build hook for project at: ${projectRoot}`);
  }

  emitProgress('Copying package assets', 10, 'assets');
  const packageAssets = copyPackageAssets(projectRoot);

  if (Object.keys(packageAssets).length > 0) {
    emitLog('info', `Copied ${Object.keys(packageAssets).length} package assets`);
  }

  emitProgress('Finding Application', 30, 'discover');
  const discoveryFailures: EntryPointFailure[] = [];
  const app = await findApplication(projectRoot, debug, undefined, discoveryFailures);

  if (app) {
    if (debug) {
      emitLog('debug', 'Application found, calling app.build()');
    }

    emitProgress('Running Application.build()', 50, 'build');
    const result = await app.build({ debug, projectName: context.projectName });

    // Activate the full config registry BEFORE emitting the infra-requirements
    // fragment below. This hook is the only producer of that fragment on a
    // normal `putnami build`, and the extension reconciles the committed
    // `infra/requirements.json` from the fragments the moment the preBuild hooks
    // return (build_generate.go, doGenerate) — nothing runs a second, later sync.
    //
    // What `build()` leaves undone is the ConfigContributor walk. It already
    // imports the generated `*-loader` modules itself, from the capability
    // producer's postGenerate, so route-owned blocks were never the missing
    // half; dependency-owned blocks were. Reading the registry here without the
    // walk committed a deployability manifest short every secret a dependency
    // declares, while `schema/config.json` — written by the configExtract hook,
    // which does walk — listed them. The two manifests then disagree, and the
    // disagreement is invisible: an omitted secret NAME skips provisioning and
    // preflight instead of failing anything.
    //
    // The loader loop inside the helper is therefore a contract, not a repair:
    // it costs a module-cache hit on a real Application and keeps activation
    // readable in one place instead of resting on a side effect of capability
    // publication.
    //
    // The helper never throws for an app that predates the walk. This hook runs
    // on EVERY build, and `isApplicationLike` supports sibling-realm apps whose
    // method set is older, so that case degrades with a warning.
    emitProgress('Activating build-time config', 60, 'activate');
    await activateBuildTimeConfig(app, result, debug);

    const projectName =
      context.projectName ||
      (readProjectConfigFile(projectRoot) as { name?: string } | undefined)?.name ||
      (readPackageJson(joinPath(projectRoot, 'package.json')) as { name?: string } | undefined)?.name ||
      basename(projectRoot);
    const httpRoutesFragment = writeHttpRoutesFragment(
      projectRoot,
      'application',
      projectName,
      result.httpRoutes ?? [],
    );
    emitArtifact('write', relativePath(projectRoot, httpRoutesFragment));
    // No `app.registerContributedConfigs()` here: `activateBuildTimeConfig`
    // above already ran the ConfigContributor walk, and it is the only call site
    // that may. Calling it again unguarded threw a raw TypeError on the
    // sibling-realm apps `isApplicationLike` deliberately supports — after
    // `build()` had already published its generated sources — which is the very
    // failure the helper's probe exists to degrade into a warning.
    const infraPath = emitInfraRequirements(projectRoot);
    if (debug && infraPath) {
      emitLog('debug', `Wrote config infra requirements scratch fragment: ${relativePath(projectRoot, infraPath)}`);
    }

    if (debug) {
      emitLog(
        'debug',
        `app.build() result - exports: ${JSON.stringify(result.exports || {})}, assets: ${Object.keys(result.assets || {}).length} files`,
      );
    }

    // Collect exports from GenerateResult
    const exports: Record<string, string> = {};
    if (result.exports) {
      for (const [exportName, exportPath] of Object.entries(result.exports)) {
        exports[exportName] = exportPath;
      }
    }

    // Collect assets from GenerateResult
    const assets = { ...packageAssets, ...(result.assets || {}) };

    emitProgress('Generation complete', 100, 'done');

    const totalExports = Object.keys(exports).length;
    const totalAssets = Object.keys(assets).length;
    if (totalExports > 0 || totalAssets > 0) {
      emitLog('info', `Generated ${totalExports} exports, ${totalAssets} assets`);
    }

    return { exports, assets };
  }

  // An entry point that exists but cannot be LOADED is not "this project has no
  // Application" — it is a broken build, and returning empty exports here hides
  // it three layers away: the hook succeeds reporting no capability manifest,
  // and the extension fails much later with "generated server loaders require
  // schema/capabilities.json" and no cause.
  //
  // Only `import-error` is fatal. The other two misses are legitimate and stay
  // exactly as they were: a module that loads and exposes no Application is a
  // plain library, and a factory that throws is entitled to demand deploy-time
  // configuration at build time (typescript/samples/07-authentication requires
  // OAUTH_CLIENT_ID inside `app()`). Widening this filter breaks builds that are
  // green today, which is why the classification lives in findApplication rather
  // than being re-derived from the error text here.
  const unloadable = discoveryFailures.filter((failure) => failure.kind === 'import-error');
  if (unloadable.length > 0) {
    throw new Error(
      `generate: no Application could be loaded from ${projectRoot}, and these entry points could not be loaded at all:\n` +
        `${formatEntryPointFailures(unloadable)}\n` +
        'Fix the entry point (package.json "main", src/main.ts, src/app.ts, or src/index.ts). ' +
        'An import of a generated module that does not exist yet means the extension that generates it must run first — ' +
        'see hooks.<kind>.order in putnami.extension.json.',
    );
  }

  if (debug) {
    emitLog('debug', 'No Application found - returning empty exports');
  }
  // No activation step here, and none is missing: with no Application there is
  // no plugin tree to walk for ConfigContributors and no build result to take
  // loaders from. Whatever the probed entry point registered at module level is
  // the whole registry, and this call still runs so a fragment left by an
  // earlier build — when the project DID have an Application — is removed before
  // the extension reconciles the committed manifest.
  emitInfraRequirements(projectRoot);

  emitProgress('Generation complete', 100, 'done');

  return {
    exports: {},
    assets: packageAssets,
  };
}

// Only run when executed directly, not when imported
if (import.meta.main) {
  runHookCommand({
    model: generateModel,
    extension: '@putnami/application',
    hook: 'preBuild',
    version: '0.0.3',
    run: runGenerate,
  });
}
