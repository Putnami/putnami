/**
 * migration-bundle.v1 — the TypeScript port of the Go bundle protocol
 * (protocols/migration). A bundle is an immutable, content-addressed
 * release artifact: a `bundle.json` manifest plus a `payload/` tree of
 * materialized migration files, carrying definitions and payloads but never
 * secrets, DSNs, or environment credentials.
 *
 * The digest and payload-hash algorithms are byte-for-byte identical to the Go
 * implementation so a bundle emitted from either runtime addresses the same
 * content. Parity is pinned by two shared golden fixtures
 * (protocols/migration/fixtures/equivalence/bundle.golden.json and
 * bundle-compatible.golden.json), asserted from both languages.
 */

import { mkdir, rm, writeFile } from 'node:fs/promises';
import { dirname, join, posix } from 'node:path';
import { DEFAULT_DATASOURCE } from './canonical';
import { resolveDatasourceSchemas } from './datasource-schema';
import { sha256HexSync } from './hash';

/** Canonical identifier embedded in every bundle manifest. */
export const BUNDLE_PROTOCOL = 'migration-bundle.v1';

/** Numeric version of the bundle wire format. */
export const BUNDLE_PROTOCOL_VERSION = 1;

/** Canonical manifest file name inside a bundle directory. */
export const BUNDLE_FILE_NAME = 'bundle.json';

/** Target system an operation acts on. SQL is the only kind emitted today. */
export type OperationKind = 'sql' | 'document' | 'events';

/** Operational risk classification used to gate unattended apply. */
export type SafetyClass = 'safe-online' | 'long-running' | 'destructive' | 'requires-approval';

/** Reference to a payload file inside the bundle and its pinned content hash. */
export interface PayloadRef {
  path: string;
  hash: string;
}

/** Runner-relevant traits of an operation. */
export interface Capabilities {
  transactional?: boolean;
  reversible?: boolean;
  async?: boolean;
  resumable?: boolean;
  /**
   * Reports that the schema after this operation stays readable and writable by
   * the previous application image (expand/contract), so an environment may roll
   * back across it without running `down`. Mirrors Go's Capabilities.Compatible
   * and participates in the digest in the same position.
   */
  compatible?: boolean;
}

/** One migration operation captured in a bundle. */
export interface BundleOperation {
  kind: OperationKind | string;
  target: string;
  /**
   * The schema the operation's DDL targets — the schema half of the source's
   * datasource. Travels in the bundle so an applier reconstructs a schema-aware
   * source and sets it as the migration search_path. Empty/absent means
   * schema-agnostic (the applier's connection search_path governs). Mirrors Go's
   * BundleOperation.Schema and participates in the digest in the same position.
   */
  schema?: string;
  namespace?: string;
  name: string;
  orderKey?: string;
  up: PayloadRef;
  down?: PayloadRef;
  safety: SafetyClass | string;
  capabilities?: Capabilities;
}

/** Source-revision provenance; never participates in the digest. */
export interface GitMetadata {
  revision?: string;
  branch?: string;
  dirty?: boolean;
}

/** Top-level migration-bundle.v1 document. */
export interface Bundle {
  protocol?: string;
  appName: string;
  source?: string;
  version?: string;
  git?: GitMetadata;
  imageDigest?: string;
  digest?: string;
  generatedAt?: string;
  operations: BundleOperation[];
}

/** One materialized payload file destined for a bundle's payload tree. */
export interface BundlePayload {
  path: string;
  bytes: string;
}

/**
 * The optional capability a runner implements to contribute its operations and
 * payloads to a build-time bundle. Mirrors Go's
 * `protocolmigration.BundleContributor`.
 */
export interface MigrationBundleContributor {
  migrationBundleOperations(): { operations: BundleOperation[]; payloads: BundlePayload[] };
}

/** Type guard for {@link MigrationBundleContributor}. */
export function isMigrationBundleContributor(value: unknown): value is MigrationBundleContributor {
  return (
    typeof value === 'object' &&
    value !== null &&
    'migrationBundleOperations' in value &&
    typeof (value as MigrationBundleContributor).migrationBundleOperations === 'function'
  );
}

/**
 * Canonical lowercase SHA-256 hex digest of a payload's content. Matches Go's
 * `ComputePayloadHash` and the runner's `sha256Hex` byte-for-byte.
 */
export function computePayloadHash(content: string): string {
  return sha256HexSync(content);
}

/** Applies canonical defaults to one operation. */
export function normalizeOperation(op: BundleOperation): BundleOperation {
  const capabilities: Capabilities = { ...(op.capabilities ?? {}) };
  if (op.down) {
    capabilities.reversible = true;
  }
  return {
    ...op,
    target: op.target || DEFAULT_DATASOURCE,
    orderKey: op.orderKey || op.name,
    safety: op.safety || 'safe-online',
    capabilities,
  };
}

/** Returns a normalized copy with defaults applied and operations sorted. */
export function normalizeBundle(b: Bundle): Bundle {
  const operations = b.operations.map(normalizeOperation);
  operations.sort(compareOperations);
  return { ...b, protocol: b.protocol || BUNDLE_PROTOCOL, operations };
}

/** Canonical operation ordering: kind, target, namespace, orderKey, name, up hash. */
function compareOperations(a: BundleOperation, b: BundleOperation): number {
  return (
    cmp(a.kind, b.kind) ||
    cmp(a.target, b.target) ||
    cmp(a.namespace ?? '', b.namespace ?? '') ||
    cmp(a.orderKey ?? '', b.orderKey ?? '') ||
    cmp(a.name, b.name) ||
    cmp(a.up.hash, b.up.hash)
  );
}

function cmp(a: string, b: string): number {
  if (a < b) return -1;
  if (a > b) return 1;
  return 0;
}

/**
 * Content-addresses a bundle: it hashes only the migration-meaningful subset
 * (protocol, appName, normalized+sorted operations), excluding release and
 * provenance metadata, so identical migrations hash identically across rebuilds
 * and languages — the basis for idempotent publish-by-digest.
 */
export function computeBundleDigest(b: Bundle): string {
  const normalized = normalizeBundle(b);
  // The shape and field order below mirror Go's bundleDigestPayload /
  // BundleOperation struct marshaling exactly; goMarshal reproduces Go's
  // encoding/json byte output. Do not reorder keys.
  const payload = {
    protocol: normalized.protocol,
    appName: normalized.appName,
    operations: normalized.operations.map(operationForDigest),
  };
  return sha256HexSync(goMarshal(payload));
}

/** Builds the digest projection of one operation in Go struct field order. */
function operationForDigest(op: BundleOperation): Record<string, unknown> {
  const out: Record<string, unknown> = { kind: op.kind, target: op.target };
  if (op.schema) out['schema'] = op.schema;
  if (op.namespace) out['namespace'] = op.namespace;
  out['name'] = op.name;
  if (op.orderKey) out['orderKey'] = op.orderKey;
  out['up'] = { path: op.up.path, hash: op.up.hash };
  if (op.down) out['down'] = { path: op.down.path, hash: op.down.hash };
  out['safety'] = op.safety;
  // Go's `omitempty` has no effect on a struct field, so capabilities is always
  // present (as {} when every flag is false); the inner bool fields are omitted
  // when false, in declaration order.
  const caps: Record<string, boolean> = {};
  if (op.capabilities?.transactional) caps['transactional'] = true;
  if (op.capabilities?.reversible) caps['reversible'] = true;
  if (op.capabilities?.async) caps['async'] = true;
  if (op.capabilities?.resumable) caps['resumable'] = true;
  if (op.capabilities?.compatible) caps['compatible'] = true;
  out['capabilities'] = caps;
  return out;
}

/**
 * Serializes a value to the exact bytes Go's encoding/json produces: compact
 * (no whitespace), insertion-order keys, and Go's HTML escaping of `<`, `>`,
 * `&`, U+2028, and U+2029. JSON.stringify already matches Go for every other
 * escape, so a targeted post-pass on those five code points completes parity.
 */
function goMarshal(value: unknown): string {
  return JSON.stringify(value).replace(
    /[<>&\u2028\u2029]/g,
    (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, '0')}`,
  );
}

/**
 * Materializes a complete bundle directory at root: the canonical manifest
 * (digest filled in) plus every referenced payload file. Payloads are
 * cross-checked against their manifest hashes before anything is written, so a
 * malformed emission fails loudly. Mirrors Go's WriteBundle.
 */
export async function writeBundle(root: string, b: Bundle, payloads: readonly BundlePayload[]): Promise<void> {
  const normalized = normalizeBundle(b);
  normalized.digest = computeBundleDigest(normalized);

  const index = new Map<string, string>();
  for (const p of payloads) {
    assertCleanRelativePayloadPath(p.path);
    index.set(p.path, p.bytes);
  }
  for (const op of normalized.operations) {
    verifyPayload(index, op.up);
    if (op.down) verifyPayload(index, op.down);
  }

  await mkdir(root, { recursive: true });
  for (const rel of payloadPaths(normalized)) {
    assertCleanRelativePayloadPath(rel);
    const dest = join(root, rel);
    await mkdir(dirname(dest), { recursive: true });
    await writeFile(dest, index.get(rel) ?? '');
  }
  await writeFile(join(root, BUNDLE_FILE_NAME), `${JSON.stringify(normalized, null, 2)}\n`);
}

function verifyPayload(index: Map<string, string>, ref: PayloadRef): void {
  assertCleanRelativePayloadPath(ref.path);
  const bytes = index.get(ref.path);
  if (bytes === undefined) {
    throw new Error(`migration bundle payload "${ref.path}" has no materialized bytes`);
  }
  const got = computePayloadHash(bytes);
  if (got !== ref.hash) {
    throw new Error(`migration bundle payload "${ref.path}" hashes to ${got} but manifest pins ${ref.hash}`);
  }
}

/**
 * Directory (relative to the project root) a build-emitted bundle lives in —
 * the TypeScript counterpart of Go's `<OutputDir>/migration-bundle/`.
 */
export const BUNDLE_OUTPUT_DIR = '.gen/migration-bundle';

/**
 * Aggregate the bundle operations and payloads contributed by the `sources`
 * that implement {@link MigrationBundleContributor}. Sources of a kind with no
 * bundle footprint are skipped. Mirrors Go's source-driven bundle collection,
 * so emission depends only on the contributed sources — never on a runtime
 * runner or the persistence backend.
 */
export function collectBundleContributions(sources: Iterable<unknown>): {
  operations: BundleOperation[];
  payloads: BundlePayload[];
} {
  const operations: BundleOperation[] = [];
  const payloads: BundlePayload[] = [];
  for (const source of sources) {
    if (!isMigrationBundleContributor(source)) continue;
    const contribution = source.migrationBundleOperations();
    operations.push(...contribution.operations);
    payloads.push(...contribution.payloads);
  }
  return { operations, payloads };
}

/**
 * Build the per-project migration bundle from `sources` and reconcile it on
 * disk under `<projectRoot>/.gen/migration-bundle/`: materialize it when any
 * source contributes operations, otherwise remove a stale bundle from a prior
 * build. Returns the written bundle (digest filled in), or `undefined` when
 * nothing was contributed.
 *
 * Fails when two sources put one datasource in two schemas, and removes the
 * bundle an earlier build left: the runner would refuse that bundle at apply
 * time, after the workload is packaged.
 *
 * This is the TypeScript counterpart of Go's `describeMigrationBundle`: it
 * walks the contributed sources directly, so `putnami build` emits a faithful
 * bundle with no per-workload wiring and independent of the runtime backend.
 */
export async function emitMigrationBundle(
  sources: Iterable<unknown>,
  appName: string,
  projectRoot: string,
): Promise<Bundle | undefined> {
  const { operations, payloads } = collectBundleContributions(sources);
  const outputDir = join(projectRoot, BUNDLE_OUTPUT_DIR);
  try {
    checkBundleSchemas(operations);
  } catch (error) {
    await rm(outputDir, { recursive: true, force: true });
    throw error;
  }
  if (operations.length === 0) {
    await rm(outputDir, { recursive: true, force: true });
    return undefined;
  }
  const normalized = normalizeBundle({ appName, operations });
  normalized.digest = computeBundleDigest(normalized);
  await writeBundle(outputDir, normalized, payloads);
  return normalized;
}

/**
 * Runs the runner's schema rule over each kind's operations, so a bundle the
 * runner would refuse is never written. Mirrors Go's
 * `migration.CheckBundleSchemas`.
 *
 * @param operations - The operations a bundle would carry.
 * @throws DatasourceSchemaConflictError when two operations of one kind put
 *   one target in two schemas.
 */
export function checkBundleSchemas(operations: readonly BundleOperation[]): void {
  const byKind = new Map<string, BundleOperation[]>();
  for (const op of operations) {
    const ops = byKind.get(op.kind);
    if (ops) {
      ops.push(op);
    } else {
      byKind.set(op.kind, [op]);
    }
  }
  for (const ops of byKind.values()) {
    resolveDatasourceSchemas(
      ops.map((op) => ({
        datasource: op.target || DEFAULT_DATASOURCE,
        schema: op.schema,
        namespace: op.namespace ?? '',
      })),
    );
  }
}

/** Sorted, de-duplicated set of payload paths a bundle references. */
export function payloadPaths(b: Bundle): string[] {
  const seen = new Set<string>();
  for (const op of b.operations) {
    if (op.up.path) seen.add(op.up.path);
    if (op.down?.path) seen.add(op.down.path);
  }
  return [...seen].sort();
}

function assertCleanRelativePayloadPath(path: string): void {
  if (!isCleanRelativePayloadPath(path)) {
    throw new Error(`migration bundle payload path "${path}" must be a clean relative path under the bundle root`);
  }
}

function isCleanRelativePayloadPath(path: string): boolean {
  if (!path || path.startsWith('/') || path.includes('\\')) return false;
  const cleaned = posix.normalize(path);
  if (cleaned !== path) return false;
  return cleaned !== '..' && !cleaned.startsWith('../');
}
