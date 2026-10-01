#!/usr/bin/env bun
/**
 * `@putnami/analytics:generate` pre-build hook.
 *
 * Bundles the browser tracker into `.gen/<publicFolder>/analytics/` and writes
 * the HTTP route fragment describing the tracker mount and the ingest route.
 *
 * Usage:
 *   bun run bin/generate --putnami-context <path>
 */

import { PutnamiConfig } from '@putnami/application';
import { type GeneratedHttpRoute, writeHttpRoutesFragment } from '@putnami/application/http-routes';
import { type InferConfig, useConfig } from '@putnami/runtime';
import { fileExists, joinPath, joinPosixPath, readProjectConfigFile, relativePath } from '@putnami/utils';
import {
  emitArtifact,
  emitProgress,
  type HookContext,
  type HookResult,
  runHookCommand,
  standardHookModel,
} from '@putnami/utils/hooks';
import { buildTrackerBundle } from './_build';

const generateModel = standardHookModel.extend({
  name: 'generate',
  description: 'Bundle the browser analytics tracker',
});

/**
 * Build the tracker bundle and declare the analytics routes.
 *
 * @param _options - Parsed command options (unused).
 * @param context - The hook context written by the CLI runner.
 * @returns The generated public assets. No `*-loader` export: the hook
 *   generates nothing server-side.
 */
async function runGenerate(_options: unknown, context: HookContext): Promise<HookResult> {
  const { projectRoot } = context;
  const projectConfig = readProjectConfigFile(projectRoot);
  const block = (projectConfig?.options?.['@putnami/analytics:generate'] ?? {}) as Partial<
    InferConfig<typeof PutnamiConfig>
  >;
  const putnami = useConfig(PutnamiConfig, { confInit: block });

  emitProgress('Bundling analytics tracker', 10, 'build');
  const scripts = await buildTrackerBundle(resolveTrackerEntry(), putnami.publicFolder, projectRoot);

  const assets: Record<string, string> = {};
  for (const script of scripts) {
    // An asset name is a forward-slash path on every platform.
    assets[joinPosixPath(putnami.publicFolder, script.path)] = joinPath(
      projectRoot,
      '.gen',
      putnami.publicFolder,
      script.path,
    );
    emitArtifact('write', joinPath(putnami.publicFolder, script.path), `Built tracker bundle: ${script.path}`);
  }

  const projectName = context.projectName || projectConfig?.name || 'app';
  const routes: GeneratedHttpRoute[] = [
    {
      match: 'prefix',
      path: '/analytics/',
      methods: ['GET', 'HEAD'],
      publicEdge: true,
      provenance: {
        package: '@putnami/analytics',
        sourceKind: 'static-mount',
        evidencePath: `.gen/${putnami.publicFolder}/analytics`,
      },
    },
    {
      match: 'exact',
      path: '/_putnami/analytics/events',
      methods: ['POST'],
      publicEdge: true,
      provenance: { package: '@putnami/analytics', sourceKind: 'manual' },
    },
  ];
  const fragment = writeHttpRoutesFragment(projectRoot, 'analytics', projectName, routes);
  emitArtifact('write', relativePath(projectRoot, fragment));

  emitProgress('Generation complete', 100, 'done');
  return { assets };
}

/**
 * Locates the tracker entry the hook bundles for the consuming project.
 *
 * In this repository the entry is the TypeScript source. A published package
 * carries no `.ts` under `src/`: the TypeScript extension transpiles every
 * export to `.js` and ships declarations beside them, so the entry is the
 * `./tracker` export's compiled output, `src/client/entry.js`. A hook that
 * only knew the source path would fail every npm consumer's build with
 * `ModuleNotFound` while the workspace, which consumes the source, stays
 * green.
 *
 * @returns Absolute path of the entry module to bundle.
 */
export function resolveTrackerEntry(): string {
  const source = joinPath(import.meta.dir, '..', 'src', 'client', 'entry.ts');
  if (fileExists(source)) {
    return source;
  }
  return joinPath(import.meta.dir, '..', 'src', 'client', 'entry.js');
}

// Only run when executed directly, not when imported
if (import.meta.main) {
  runHookCommand({
    model: generateModel,
    extension: '@putnami/analytics',
    hook: 'preBuild',
    version: '0.0.1',
    run: runGenerate,
  });
}
