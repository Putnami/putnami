import { existsSync, readFileSync, statSync } from 'node:fs';
import { join, normalize, sep } from 'node:path';
import {
  BUNDLE_FILE_NAME,
  BUNDLE_OUTPUT_DIR,
  type Bundle,
  type BundleOperation,
  computePayloadHash,
  DEFAULT_DATASOURCE,
  KindSQL,
  type PayloadRef,
} from '@putnami/migration';
import { type SQLDefinition, SQLSource } from './sql-source';

/**
 * The project's migrations as the build emitted them: the SQL sources of the
 * migration bundle `putnami build` writes under `<projectRoot>/.gen/migration-bundle/`
 * from the sources the application's plugins contribute.
 *
 * The TypeScript counterpart of Go's `database.LoadBundleSources`: it reads the
 * manifest, checks every payload against the hash the manifest pins, and groups
 * the SQL operations back into one SQLSource per datasource, schema and
 * namespace, in the manifest's order. Operations of another kind are skipped.
 */
export interface ProjectBundle {
  /** The bundle directory read. */
  dir: string;
  /** The sources the bundle carries; empty when it holds no SQL operation. */
  sources: SQLSource[];
  /** When the manifest was written, in milliseconds since the epoch. */
  writtenAt: number;
}

/** The directory the build writes a project's migration bundle into. */
export function projectBundleDir(projectRoot: string): string {
  return join(projectRoot, BUNDLE_OUTPUT_DIR);
}

/**
 * loadProjectBundle reads the migration bundle under `projectRoot`, or returns
 * undefined when the project has none. A bundle that is malformed, or whose
 * payload does not match its pinned hash, throws.
 */
export function loadProjectBundle(projectRoot: string): ProjectBundle | undefined {
  const dir = projectBundleDir(projectRoot);
  const manifest = join(dir, BUNDLE_FILE_NAME);
  if (!existsSync(manifest)) {
    return undefined;
  }
  let bundle: Bundle;
  try {
    bundle = JSON.parse(readFileSync(manifest, 'utf8')) as Bundle;
  } catch (err) {
    throw new Error(
      `migration bundle ${manifest} is not valid JSON: ${err instanceof Error ? err.message : String(err)}`,
    );
  }
  if (!Array.isArray(bundle.operations)) {
    throw new Error(`migration bundle ${manifest} has no operations list`);
  }
  return { dir, sources: bundleSources(dir, bundle.operations), writtenAt: statSync(manifest).mtimeMs };
}

function bundleSources(dir: string, operations: readonly BundleOperation[]): SQLSource[] {
  const groups = new Map<string, { datasource: string; schema: string; namespace: string; defs: SQLDefinition[] }>();
  for (const op of operations) {
    if (op.kind !== KindSQL) {
      continue;
    }
    const datasource = op.target || DEFAULT_DATASOURCE;
    const schema = op.schema ?? '';
    const namespace = op.namespace || op.name.split('/')[0] || datasource;
    const key = JSON.stringify([datasource, schema, namespace]);
    let group = groups.get(key);
    if (!group) {
      group = { datasource, schema, namespace, defs: [] };
      groups.set(key, group);
    }
    const def: SQLDefinition = { name: op.name, sql: readPayload(dir, op.up) };
    if (op.down) {
      def.down = readPayload(dir, op.down);
    }
    group.defs.push(def);
  }
  return [...groups.values()].map(
    (g) =>
      new SQLSource({
        namespace: g.namespace,
        datasource: { name: g.datasource, schema: g.schema },
        definitions: g.defs,
      }),
  );
}

function readPayload(dir: string, ref: PayloadRef): string {
  const rel = normalize(ref.path);
  if (rel.startsWith('..') || rel.startsWith(sep)) {
    throw new Error(`migration bundle payload "${ref.path}" escapes the bundle directory`);
  }
  const path = join(dir, rel);
  if (!existsSync(path)) {
    throw new Error(`migration bundle payload "${ref.path}" is missing from ${dir}`);
  }
  const bytes = readFileSync(path, 'utf8');
  const got = computePayloadHash(bytes);
  if (got !== ref.hash) {
    throw new Error(`migration bundle payload "${ref.path}" hashes to ${got} but the manifest pins ${ref.hash}`);
  }
  return bytes;
}
