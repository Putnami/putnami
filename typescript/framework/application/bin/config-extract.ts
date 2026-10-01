#!/usr/bin/env bun
/**
 * @putnami/application:config-extract CLI command
 *
 * Walks the workload's registered ConfigDefinitions and emits a CPA-publishable
 * config schema manifest, mirroring the Go-side `config-extract` task (see
 * `go/extension/internal/jobs/configextract/`). Invoked as a subprocess with
 * JSONL output.
 *
 * Output paths (relative to the workload root):
 *   - `schema/config.json`         — committed manifest, default
 *   - `.gen/config-schema.json`    — gitignored fallback, used when
 *                                    options.generate.schema=false
 *   - `<manifest>.jsonschema.json` — companion JSON Schema (Draft 2020-12)
 *   - `.gen/infra/secrets.json`    — infra-requirements scratch fragment
 *                                    derived from sensitive fields
 *
 * Deliberately NOT written here: `.gen/schema/capabilities.json` (and the
 * committed copy `build-generate` promotes from it), its feature evidence, and
 * `.gen/design/graph.json`. Those are `build-generate`'s declared outputs and it
 * is the only task that composes them — see the `app.build()` call below.
 *
 * The extractor needs the workload's plugin tree fully wired so every
 * `configToken()` call has executed and registered its definition.
 * `Application.build()` traverses the plugin tree's `generate()` hooks and
 * writes the generated `*-loader` modules, but building is not the same as
 * activating: the route-owned and dependency-owned blocks only reach the global
 * registry once `activateBuildTimeConfig` (bin/_activate-config.ts) has run.
 * `bin/generate.ts` runs the identical step before it emits its own
 * infra-requirements fragment, so the committed manifest and the committed
 * schema are derived from one registry rather than from whichever hook ran last.
 *
 * Usage:
 *   bun run @putnami/application/bin/config-extract --putnami-context <path>
 */

import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { emitInfraRequirements } from '@putnami/runtime';
import {
  emitLog,
  emitProgress,
  type HookContext,
  type HookResult,
  runHookCommand,
  standardHookModel,
} from '@putnami/utils/hooks';
import { DEFAULT_OUTPUT_PATH, extractConfigSchema, FALLBACK_OUTPUT_PATH } from '../src/config/config-schema-extract';
import { activateBuildTimeConfig } from './_activate-config';
import { findApplication } from './_find-application';

const configExtractModel = standardHookModel.extend({
  name: 'config-extract',
  description: 'Extract the workload config schema to schema/config.json',
});

export function resolveConfigSchemaOutput(config?: unknown): false | undefined {
  const configuredSchema = readSchemaOutputOption(config);
  if (configuredSchema === false) return false;
  return undefined;
}

function readSchemaOutputOption(config: unknown): boolean | undefined {
  if (typeof config !== 'object' || config === null || Array.isArray(config)) return undefined;
  const value = (config as Record<string, unknown>)['schema'];
  return typeof value === 'boolean' ? value : undefined;
}

/** Evidence that a config schema manifest already exists for this project. */
export interface ExistingManifestEvidence {
  /** Absolute path of the manifest file found on disk. */
  path: string;
  /** Number of config blocks it declares, or null when the file is unparsable. */
  blocks: number | null;
}

/**
 * Project-relative manifest paths to probe for stale-manifest evidence, in
 * priority order: the active output location first, then the remaining
 * known locations. This is a superset of the candidate set the Go-side
 * runner peeks after the hook exits (config_extract.go), so whenever this
 * hook reports "nothing to extract" without throwing, the runner cannot
 * find a manifest either — the two sides never disagree about staleness.
 */
export function manifestCandidatePaths(output: string | false | undefined): string[] {
  const active = output === false ? FALLBACK_OUTPUT_PATH : (output ?? DEFAULT_OUTPUT_PATH);
  const candidates = [active];
  for (const known of [DEFAULT_OUTPUT_PATH, FALLBACK_OUTPUT_PATH]) {
    if (!candidates.includes(known)) candidates.push(known);
  }
  return candidates;
}

/**
 * Look for an existing config schema manifest that this run would be unable
 * to regenerate. A manifest that declares one or more config blocks (or one
 * that cannot be parsed) is evidence that extraction SHOULD have produced
 * output — the caller must fail loudly instead of silently keeping the
 * stale file. A manifest that itself declares zero blocks is consistent
 * with an empty registry and is not treated as evidence.
 */
export function findExistingManifest(
  projectRoot: string,
  output: string | false | undefined,
): ExistingManifestEvidence | null {
  for (const relative of manifestCandidatePaths(output)) {
    const path = join(projectRoot, relative);
    if (!existsSync(path)) continue;
    try {
      const manifest = JSON.parse(readFileSync(path, 'utf-8')) as { configs?: unknown };
      const blocks = Array.isArray(manifest.configs) ? manifest.configs.length : null;
      if (blocks === null || blocks > 0) {
        return { path, blocks };
      }
    } catch {
      return { path, blocks: null };
    }
  }
  return null;
}

function describeBlocks(blocks: number | null): string {
  return blocks === null ? 'an unparsable manifest' : `${blocks} config block(s)`;
}

/**
 * Run the config schema extraction process. Returns no exports/assets — the
 * task's value is the side-effect file write. The summary's `data` block
 * exposes the resolved paths and block count so the Go-side runner can
 * surface them in its JSONL output.
 */
async function runConfigExtract(_options: unknown, context: HookContext): Promise<HookResult> {
  const { projectRoot, debug } = context;

  if (debug) {
    emitLog('debug', `Starting configExtract hook for project at: ${projectRoot}`);
  }

  const schemaOutput = resolveConfigSchemaOutput(context.config);

  emitProgress('Finding Application', 20, 'discover');
  const discoveryDiagnostics: string[] = [];
  const app = await findApplication(projectRoot, debug, discoveryDiagnostics);

  if (!app) {
    // An existing manifest with config blocks is proof this project used to
    // extract configs — "no application found" then means the schema can no
    // longer be regenerated from source, and silently keeping the stale
    // file would let it drift forever (the runner would re-report it as a
    // fresh extraction). Fail loudly with whatever discovery saw.
    const existing = findExistingManifest(projectRoot, schemaOutput);
    if (existing) {
      const details =
        discoveryDiagnostics.length > 0 ? `\nEntry point errors:\n  - ${discoveryDiagnostics.join('\n  - ')}` : '';
      throw new Error(
        `config-extract: a config schema manifest with ${describeBlocks(existing.blocks)} exists at ${existing.path}, ` +
          `but no Application could be loaded from ${projectRoot} — the manifest cannot be regenerated from source ` +
          `and may be stale.${details}\n` +
          `Fix the application entry point (package.json "main", src/main.ts, src/app.ts, or src/index.ts), ` +
          `or delete ${existing.path} if this project no longer declares config blocks.`,
      );
    }
    if (debug) {
      emitLog('debug', 'No Application found - skipping config extraction');
    }
    emitProgress('Config extraction skipped', 100, 'done');
    return {
      exports: {},
      assets: {},
      data: { status: 'skipped', reason: 'no application found' },
    };
  }

  // Trigger the plugin tree so all configToken() calls execute and populate the
  // global config registry. `build()` runs every plugin's `generate()` hook —
  // the api plugin emits a loader module under `.gen/src/api/` that statically
  // imports every route handler. The handlers contain the `configToken(Config)`
  // calls that populate the registry, so the loaders have to be IMPORTED after
  // build for those module-level effects to fire, and the dependency-owned
  // blocks have to be walked in. Both steps live in `activateBuildTimeConfig`,
  // which `bin/generate.ts` runs too: the preBuild hook emits the same
  // infra-requirements fragment, and the two must read the same registry or the
  // committed manifest and the committed schema disagree.
  //
  // Capability manifest and design graph are NOT republished here, and that is
  // an ownership statement, not an optimization. `.gen/schema/capabilities.json`
  // and `.gen/design/graph.json` are declared outputs of `build-generate` (see
  // typescript/extension/putnami.extension.json: the `gen` output is "owned
  // exclusively by this task", and `capabilities` names the framework producer
  // as "its sole author"), and `build-generate` is the task that promotes the
  // manifest to the committed `schema/capabilities.json`. `config-extract`
  // always runs AFTER it in the same plan (command `config-extract`: step
  // `generate` then step `extract`), so re-publishing from here can only
  // overwrite a fresher artifact with a different one:
  //
  //  - `build-generate` clears `.gen/infra/*.json` before its preBuild hooks and
  //    the fragments land after `build()` returns, so ITS producer run sees no
  //    secrets fragment; this hook runs with those fragments on disk, so the
  //    producer's `.gen/infra` scan adds infra secret capabilities the promoted
  //    manifest does not have;
  //  - only `build-generate` reconciles the generated loader exports into the
  //    manifest (`build.ReconcileCapabilityManifest`), so a manifest written
  //    here silently DROPS the generated route schemas.
  //
  // Publishing also invalidates first (`invalidateCapabilityManifest`), so the
  // default would delete `build-generate`'s manifest and feature evidence before
  // writing its own divergent bytes. Both flags are passed explicitly rather
  // than relying on `publishDesignGraph` defaulting to `publishCapabilityManifest`:
  // a future default change must not silently hand this hook a second write into
  // another task's declared output.
  //
  // The migration artifacts (`.gen/infra/migration.json`, `.gen/migration-bundle/`)
  // have the same shape: `build-generate` writes them into the `gen` output it
  // owns, and this task declares no output under `.gen`. They are not
  // republished here either, so the bundle the publisher reads has exactly one
  // author.
  //
  // Nothing downstream of this call needs any of these artifacts: `buildResult`
  // is used only for its `exports` map (the generated loaders
  // `activateBuildTimeConfig` imports), and the capability producer still
  // imports those loaders when publication is off, so the config registry is
  // activated exactly as before.
  emitProgress('Running Application.build()', 40, 'build');
  const buildResult = await app.build({
    debug,
    projectName: context.projectName,
    publishCapabilityManifest: false,
    publishDesignGraph: false,
    publishMigrationArtifacts: false,
  });

  emitProgress('Activating build-time config', 60, 'activate');
  await activateBuildTimeConfig(app, buildResult, debug);

  emitProgress('Extracting config schema', 75, 'extract');
  // Use the putnami project identity plumbed through the hook context as the
  // manifest appName so it keys on the project path (matching the Go extractor's
  // ctx.Project.Name), not the npm package.json name. An older Go runner omits
  // projectName; extractConfigSchema then falls back to getCurrentProject().name
  // (the pre-fix behavior), so the wire format stays backward compatible.
  const manifest = extractConfigSchema({ output: schemaOutput, appName: context.projectName });

  if (!manifest) {
    // Zero registered configs while a manifest with config blocks already
    // exists on disk means this run failed to see definitions that a
    // previous run extracted. Returning `empty` here would leave the stale
    // file untouched and let the runner re-report it as fresh — the exact
    // silent no-op this guard exists to prevent.
    const existing = findExistingManifest(projectRoot, schemaOutput);
    if (existing) {
      throw new Error(
        `config-extract: this run registered zero config definitions, but a manifest with ` +
          `${describeBlocks(existing.blocks)} already exists at ${existing.path} — refusing to silently keep a ` +
          `stale schema. Likely causes: config tokens only referenced at request time ` +
          `(ctx.get(configToken(...)) inside a handler body) — reference them at module level via ` +
          `.inject({ ... : configToken(...) }) or provideConfig(); or the workload loads a duplicate copy of ` +
          `@putnami/runtime so registrations land in a sibling module realm. ` +
          `If all config blocks were removed intentionally, delete ${existing.path} and re-run.`,
      );
    }

    // No registered configs is a legitimate state for workloads that don't
    // declare any (utility libs, simple jobs). Still emit the infra
    // requirements call so a stale scratch fragment from a prior build is
    // cleared.
    emitInfraRequirements(projectRoot);
    if (debug) {
      emitLog('debug', 'No config definitions registered — nothing to extract');
    }
    emitProgress('Config extraction complete', 100, 'done');
    return {
      exports: {},
      assets: {},
      data: { status: 'empty', reason: 'no config definitions registered' },
    };
  }

  // Emit infra-requirements scratch fragment from the same registry so the
  // secrets list stays in lockstep with the schema. The Go side performs the
  // same pairing inside configextract.Run.
  emitProgress('Emitting infra requirements', 90, 'infra');
  const infraPath = emitInfraRequirements(projectRoot);

  emitProgress('Config extraction complete', 100, 'done');

  emitLog(
    'info',
    `Extracted ${manifest.configs.length} config blocks (${manifest.schemaHash}) for ${manifest.appName || 'unnamed app'}`,
  );

  return {
    exports: {},
    assets: {},
    data: {
      status: 'ok',
      appName: manifest.appName,
      version: manifest.version,
      schemaHash: manifest.schemaHash,
      blocks: manifest.configs.length,
      infraSidecar: infraPath ?? null,
    },
  };
}

// Only run when executed directly, not when imported
if (import.meta.main) {
  runHookCommand({
    model: configExtractModel,
    extension: '@putnami/application',
    hook: 'configExtract',
    version: '0.0.3',
    run: runConfigExtract,
  });
}
