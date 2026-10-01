/**
 * Per-project infrastructure-requirements emission for the migration
 * framework — the TypeScript counterpart of the Go emitter.
 *
 * Migration is the authoritative source for the `schemas` list inside each
 * declared database: it walks its contributed sources, asks each for its
 * infra contribution, merges them by `(name, engine)`, and emits a per-project
 * scratch fragment at `<project>/.gen/infra/migration.json`. The TypeScript
 * generator syncs scratch fragments into committed `infra/requirements.json`;
 * the build aggregator then reads that committed contract across the workload
 * dependency graph.
 *
 * The shapes here are a hand-port of the v2 protocol defined in Go at
 * protocols/infra/ (schema: schemas/infra.json). The per-project
 * manifest carries no contributor field — provenance is assigned by the
 * aggregator — so emitting one is just declaring `{ databases }`.
 */

import { randomBytes } from 'node:crypto';
import { mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { robustRemoveSync, robustRenameSync } from '@putnami/runtime/robustio';
import { isMigrationContributor } from './contributor';
import type { MigrationSource } from './types';

/**
 * Current infra-requirements protocol version. Must match the Go reference
 * (infra.ProtocolVersion); the generator sync and aggregator strict parsers
 * drop fragments/manifests that declare an unsupported version.
 */
export const INFRA_PROTOCOL_VERSION = 2 as const;

/** JSON Schema reference written into generated manifests so editors validate it. */
export const INFRA_PER_PROJECT_SCHEMA_URL = 'https://putnami.dev/schemas/putnami-infra.json';

/** Directory (relative to the project root) the framework scratch fragment lives in. */
export const INFRA_SIDECAR_DIR = '.gen/infra';

/**
 * Per-producer scratch slug. Each framework producer owns one file at
 * <project>/.gen/infra/<slug>.json; the TypeScript generator syncs all
 * fragments into committed infra/requirements.json.
 */
export const INFRA_SIDECAR_SLUG = 'migration';
export const INFRA_SIDECAR_FILENAME = `${INFRA_SIDECAR_SLUG}.json`;

/**
 * Database engine. Closed enum — mirrors the protocol's `ValidEngines`. The
 * only engine a migration source emits is `postgres` (SQL); the rest keep the
 * type in sync with the protocol.
 */
export type InfraEngine = 'postgres' | 'mysql' | 'sqlite' | 'firestore';

/** A single database requirement a migration source contributes. */
export interface InfraDatabaseRequirement {
  /** Logical database identifier (the datasource / pool name). */
  name: string;
  /** Engine backing the database. */
  engine: InfraEngine;
  /** Schema names this source migrates inside the database. */
  schemas?: string[];
}

/** Per-project infra manifest — the scratch shape the generator sync reads. */
export interface PerProjectInfraManifest {
  $schema?: string;
  protocolVersion: typeof INFRA_PROTOCOL_VERSION;
  databases?: InfraDatabaseRequirement[];
}

/** Join `(name, engine)` into a merge key. `|` cannot appear in a resource name
 *  (regex `^[a-z0-9][a-z0-9_./-]`) or an engine value, so it is collision-free. */
function databaseKey(name: string, engine: InfraEngine): string {
  return `${name}|${engine}`;
}

/**
 * Merge per-source database requirements into a deterministic, deduplicated
 * list. Entries are keyed by `(name, engine)` — the protocol's merge identity —
 * so multiple sources targeting the same database collapse into one entry whose
 * `schemas` are the sorted union. The result is sorted by `(name, engine)` and
 * every schema list is sorted and deduplicated, so the emitted fragment is
 * byte-identical for a given input (the idempotency the scratch contract
 * requires of concurrent producers).
 */
export function mergeInfraDatabases(requirements: Iterable<InfraDatabaseRequirement>): InfraDatabaseRequirement[] {
  const byKey = new Map<string, { name: string; engine: InfraEngine; schemas: Set<string> }>();
  for (const req of requirements) {
    const key = databaseKey(req.name, req.engine);
    let entry = byKey.get(key);
    if (!entry) {
      entry = { name: req.name, engine: req.engine, schemas: new Set<string>() };
      byKey.set(key, entry);
    }
    for (const schema of req.schemas ?? []) {
      if (schema) entry.schemas.add(schema);
    }
  }

  const databases = [...byKey.values()].map((entry) => {
    const db: InfraDatabaseRequirement = { name: entry.name, engine: entry.engine };
    if (entry.schemas.size > 0) {
      db.schemas = [...entry.schemas].sort();
    }
    return db;
  });
  databases.sort((a, b) => (a.name === b.name ? a.engine.localeCompare(b.engine) : a.name.localeCompare(b.name)));
  return databases;
}

/**
 * Build the per-project manifest from a set of migration sources. Each source
 * is asked for its `infraDatabase()` contribution; sources of a kind with no
 * infra footprint (no `infraDatabase()`) are skipped. Returns `undefined` when
 * nothing is contributed, signalling the caller to emit no scratch fragment.
 */
export function buildInfraManifest(sources: Iterable<MigrationSource>): PerProjectInfraManifest | undefined {
  const requirements: InfraDatabaseRequirement[] = [];
  for (const source of sources) {
    const requirement = source.infraDatabase?.();
    if (requirement) requirements.push(requirement);
  }

  const databases = mergeInfraDatabases(requirements);
  if (databases.length === 0) return undefined;

  return {
    $schema: INFRA_PER_PROJECT_SCHEMA_URL,
    protocolVersion: INFRA_PROTOCOL_VERSION,
    databases,
  };
}

/** Absolute path of a project's framework scratch fragment. */
export function infraSidecarPath(projectRoot: string): string {
  return join(projectRoot, INFRA_SIDECAR_DIR, INFRA_SIDECAR_FILENAME);
}

/**
 * Write the scratch fragment atomically (unique temp file + rename) so a
 * concurrent reader never observes a torn file. A per-call random temp name
 * keeps two concurrent writers from clobbering each other's temp file. Content
 * is indented and newline-terminated to match other `.gen/` artifacts.
 */
export function writeInfraSidecar(projectRoot: string, manifest: PerProjectInfraManifest): string {
  const finalPath = infraSidecarPath(projectRoot);
  mkdirSync(dirname(finalPath), { recursive: true });
  const data = `${JSON.stringify(manifest, null, 2)}\n`;
  const tmpPath = `${finalPath}.${randomBytes(6).toString('hex')}.tmp`;
  writeFileSync(tmpPath, data);
  try {
    robustRenameSync(tmpPath, finalPath);
  } catch (err) {
    // Best-effort cleanup: it must not replace the write error.
    try {
      rmSync(tmpPath, { force: true });
    } catch {
      // Keep the original error.
    }
    throw err;
  }
  return finalPath;
}

/** Remove a stale scratch fragment if present. Best-effort; a missing file is fine. */
export function removeInfraSidecar(projectRoot: string): void {
  robustRemoveSync(infraSidecarPath(projectRoot));
}

/**
 * Build the per-project manifest from `sources` and reconcile the scratch fragment on
 * disk: write it when there is something to declare, otherwise remove any stale
 * file from a prior generate so generator sync never reads requirements the
 * project no longer has. Returns the manifest that was written, or `undefined`
 * when the scratch fragment was removed/left absent.
 */
export function emitInfraRequirements(
  sources: Iterable<MigrationSource>,
  projectRoot: string,
): PerProjectInfraManifest | undefined {
  const manifest = buildInfraManifest(sources);
  if (!manifest) {
    removeInfraSidecar(projectRoot);
    return undefined;
  }
  writeInfraSidecar(projectRoot, manifest);
  return manifest;
}

/**
 * Minimal structural view of the module tree the infra plugin needs. Typed
 * structurally rather than importing `Module` from `@putnami/application`,
 * which depends on `@putnami/migration` — importing it here would be a cycle.
 */
export interface InfraGenerateOwner {
  getRoot(): { collectPlugins(): Array<{ plugin: unknown }> };
}

/** Result of a generate hook — structurally compatible with the app's `GenerateResult`. */
export interface InfraGenerateResult {
  exports?: Record<string, string>;
  assets?: Record<string, string>;
}

/** A plugin that emits the infra scratch fragment during the project's generate phase. */
export interface InfraRequirementsPlugin {
  name: string;
  generate(owner: InfraGenerateOwner): InfraGenerateResult;
}

/**
 * Plugin that emits the per-project infra scratch fragment during `app.build()`.
 *
 * Unlike the runtime registry — which is populated only during `prepare()`
 * (warmup → source collection) — the generate phase runs `generate()` alone,
 * so this hook collects `MigrationContributor` sources directly from the module
 * tree (`owner.getRoot().collectPlugins()`) the same way `prepare()` does. The
 * project root defaults to the generate hook's working directory, which the
 * build pipeline sets to the project being built.
 *
 * @example
 * ```ts
 * application()
 *   .use(sql())
 *   .use(infraRequirements());
 * ```
 */
export function infraRequirements(options: { projectRoot?: string } = {}): InfraRequirementsPlugin {
  return {
    name: 'migration-infra',
    generate(owner: InfraGenerateOwner): InfraGenerateResult {
      const sources: MigrationSource[] = [];
      for (const { plugin } of owner.getRoot().collectPlugins()) {
        if (isMigrationContributor(plugin)) {
          sources.push(...plugin.migrationSources());
        }
      }
      const projectRoot = options.projectRoot ?? process.env.PUTNAMI_PROJECT_ROOT ?? process.cwd();
      emitInfraRequirements(sources, projectRoot);
      return {};
    },
  };
}
