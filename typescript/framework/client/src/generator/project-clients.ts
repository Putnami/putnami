import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, relative, resolve, sep } from 'node:path';
import { gunzipSync } from 'node:zlib';
import type { ClientCachePolicy } from '@putnami/application';
import { fileExists, getWorkspaceRoot, joinPath, readPackageJson, toPosixPath } from '@putnami/utils';
import { resolveClientDependencySpec } from './client-dependency';
import type { ClientGenConfig } from './config.type';
import { canonicalizeGeneratedFiles } from './generated-file-canonicalizer';
import type { SpecIR } from './ir.type';
import { readOpenApiSource } from './openapi-reader';
import { generateTypeScriptClient } from './ts/ts-generator';

/** Project-relative path of the client-generation contract a build emits. */
const CONFIG_REL = '.gen/clientgen/config.json';

/** Result of {@link generateProjectClients}. */
export interface ProjectClientResult {
  /** True when a TypeScript client was emitted (ts target enabled + the spec exposed services). */
  generated: boolean;
  /** Project-relative output directory (e.g. "clients/ts"), set whenever a ts target is configured. */
  outputDir: string;
  /** Project-relative paths written this run. */
  files: string[];
}

/** Optional exact provider artifact selected by the build lifecycle. */
export interface ProjectClientSource {
  sourcePath?: string;
  /** Original project root whose formatting identity applies when rendering through an isolated mirror. */
  formatProjectRoot?: string;
}

interface ProjectSpec {
  ir: SpecIR;
  source: string;
}

interface GeneratedClientManifest {
  protocolVersion: 1;
  generatedBy: '@putnami/clientgen';
  language: 'ts';
  service: { id: string; audience: string };
  binding: {
    importPath: string;
    clients: Array<{ service: string; clientSymbol: string; bindingSymbol: string }>;
  };
  contractSha256: string;
  operations: Array<{
    operationId: string;
    service: string;
    methodSymbol: string;
    stream: 'unary' | 'server' | 'client' | 'bidirectional';
    transports: NonNullable<SpecIR['services'][number]['methods'][number]['client']>['transports'];
    cache?: ClientCachePolicy;
  }>;
  /**
   * Client runtime capabilities the target cannot run without, derived from
   * the operations exactly as the Go emitter derives them: `response-cache`
   * when any operation declares a cache policy.
   */
  runtimeCapabilities?: string[];
  files: Array<{ path: string; sha256: string }>;
}

const MANIFEST_FILE = 'client.putnami.json';

/**
 * Emit the TypeScript client for a provider from the OpenAPI spec and contract a
 * build already produced under `projectRoot`, writing it into
 * `projectRoot/<ts.output>` (default `clients/ts`).
 *
 * This is the standalone, app-less counterpart of {@link ClientGeneratorPlugin}'s
 * in-app `postGenerate()`: the workspace `clientgen` command runs it — in its own
 * Bun toolchain — to emit a TypeScript client for a provider written in ANOTHER
 * language (e.g. a Go service), reading that provider's OWN spec. Cross-language
 * emission therefore needs no running app and no cross-project reads; it is
 * mediated entirely through the shared spec artifact.
 *
 * Returns `generated: false` (no throw) when the project has no clientgen
 * contract, the ts target is disabled, or the spec exposes no services, so
 * callers can treat "nothing to do" as success. Throws when an enabled target
 * has no spec (the build must run first), the output escapes the project root,
 * or the provider's Biome configuration cannot be applied before writing.
 */
export function generateProjectClients(projectRoot: string, selected: ProjectClientSource = {}): ProjectClientResult {
  const configPath = joinPath(projectRoot, CONFIG_REL);
  if (!fileExists(configPath)) {
    return { generated: false, outputDir: '', files: [] };
  }
  const config: ClientGenConfig = JSON.parse(readFileSync(configPath, 'utf8'));
  const outputRel = config.ts?.output ?? 'clients/ts';
  if (!config.targets?.includes('ts')) {
    return { generated: false, outputDir: outputRel, files: [] };
  }

  const projectSpec = readProjectSpec(projectRoot, config, selected.sourcePath);
  const specIR = projectSpec.ir;
  const outputDir = resolveOutputDirectory(projectRoot, outputRel);
  const formatOutputDir = resolveOutputDirectory(selected.formatProjectRoot ?? projectRoot, outputRel);
  if (specIR.services.length === 0) {
    // Removing every endpoint must also remove artifacts owned by the previous
    // manifest. Otherwise a deleted first-party operation remains callable.
    removeOwnedOutput(outputDir, undefined);
    return { generated: false, outputDir: outputRel, files: [] };
  }

  // The generated tsconfig extends the workspace base config; compute the path
  // against the actual output dir so it is correct at any nesting depth. The
  // generated file is committed, so it takes the forward-slash form everywhere.
  const tsconfigExtends = toPosixPath(relative(outputDir, joinPath(getWorkspaceRoot(), 'tsconfig.base.json')));
  const formatProjectRoot = selected.formatProjectRoot ?? projectRoot;
  const files = canonicalizeGeneratedFiles(
    formatProjectRoot,
    formatOutputDir,
    generateTypeScriptClient(specIR, {
      packageName: config.ts.packageName,
      version: readPackageJson(joinPath(projectRoot, 'package.json'))?.version,
      tsconfigExtends,
      clientDependency: resolveClientDependencySpec(getWorkspaceRoot()),
      ...(config.design ? { design: config.design } : {}),
    }),
  );

  const manifest = specIR.contract
    ? buildManifest(specIR, config.ts.packageName, projectSpec.source, files)
    : undefined;
  // The manifest sits in the generated client project, so the workspace's
  // Biome lints it like the sources: render it through the same writer phases.
  const manifestContent = manifest
    ? canonicalizeGeneratedFiles(formatProjectRoot, formatOutputDir, [
        { path: MANIFEST_FILE, content: `${JSON.stringify(manifest, null, 2)}\n` },
      ])[0].content
    : undefined;
  preflightOwnedOutput(outputDir, new Set(files.map((file) => file.path)), manifest !== undefined);

  const written: string[] = [];
  for (const file of files) {
    const filePath = resolve(outputDir, file.path);
    if (!filePath.startsWith(`${outputDir}${sep}`)) {
      throw new Error(`generated file "${file.path}" escapes output directory "${outputDir}".`);
    }
    mkdirSync(resolve(outputDir, dirname(file.path)), { recursive: true });
    writeFileSync(filePath, file.content);
    written.push(`${outputRel}/${file.path}`);
  }

  removeOwnedOutput(outputDir, new Set(files.map((file) => file.path)));
  if (manifestContent !== undefined) {
    writeFileSync(resolve(outputDir, MANIFEST_FILE), manifestContent);
    written.push(`${outputRel}/${MANIFEST_FILE}`);
  }

  return { generated: true, outputDir: outputRel, files: written };
}

function resolveOutputDirectory(projectRoot: string, outputRel: string): string {
  const outputDir = resolve(projectRoot, outputRel);
  const resolvedRoot = resolve(projectRoot);
  // `resolve` returns native separators, so containment is checked with `sep`.
  if (!outputDir.startsWith(`${resolvedRoot}${sep}`) && outputDir !== resolvedRoot) {
    throw new Error(`clientgen ts.output "${outputRel}" escapes project root (resolved to "${outputDir}").`);
  }
  return outputDir;
}

function buildManifest(
  spec: SpecIR,
  importPath: string,
  source: string,
  files: Array<{ path: string; content: string }>,
): GeneratedClientManifest {
  const contract = spec.contract;
  if (!contract) throw new Error('clientgen_first_party_required: generated manifest requires a first-party contract');
  const clients = spec.services
    .map((service) => ({
      service: service.name,
      clientSymbol: service.className,
      bindingSymbol: `register${service.className}`,
    }))
    .sort((left, right) =>
      compareStrings(`${left.service}\0${left.clientSymbol}`, `${right.service}\0${right.clientSymbol}`),
    );
  const operations = spec.services
    .flatMap((service) =>
      service.methods.map((method) => {
        if (!method.client) {
          throw new Error(`clientgen_first_party_required: operation ${method.operationId} has no client contract`);
        }
        const cache = method.client.resilience?.cache;
        return {
          operationId: method.operationId,
          service: service.name,
          methodSymbol: method.name,
          stream: method.client.stream,
          transports: method.client.transports,
          ...(cache ? { cache: manifestCachePolicy(cache) } : {}),
        };
      }),
    )
    .sort((left, right) => compareStrings(left.operationId, right.operationId));
  // Derived exactly as the Go emitter derives them (RequiredRuntimeCapabilities):
  // the response cache when an operation declares a cache policy, and SSE
  // continuation when one of its transports declares a continuation.
  const runtimeCapabilities = [
    ...(operations.some((operation) => operation.cache !== undefined) ? ['response-cache'] : []),
    ...(operations.some((operation) => operation.transports.some((transport) => transport.sse?.continuation))
      ? ['sse-continuation']
      : []),
  ];
  return {
    protocolVersion: 1,
    generatedBy: '@putnami/clientgen',
    language: 'ts',
    service: contract.service,
    binding: { importPath, clients },
    contractSha256: sha256(source),
    operations,
    ...(runtimeCapabilities.length ? { runtimeCapabilities } : {}),
    files: files
      .map((file) => ({ path: file.path, sha256: sha256(file.content) }))
      .sort((left, right) => compareStrings(left.path, right.path)),
  };
}

/**
 * The inventory copy of a cache policy, in the Go emitter's field order, so
 * both targets inventory one policy in one shape.
 */
function manifestCachePolicy(cache: ClientCachePolicy): ClientCachePolicy {
  return {
    freshMs: cache.freshMs,
    ...(cache.staleMs === undefined ? {} : { staleMs: cache.staleMs }),
    ...(cache.maxEntries === undefined ? {} : { maxEntries: cache.maxEntries }),
    ...(cache.keyFields === undefined ? {} : { keyFields: [...cache.keyFields] }),
    ...(cache.invalidationFields === undefined ? {} : { invalidationFields: [...cache.invalidationFields] }),
  };
}

function compareStrings(left: string, right: string): number {
  if (left < right) return -1;
  if (left > right) return 1;
  return 0;
}

function sha256(content: string | Uint8Array): string {
  return createHash('sha256').update(content).digest('hex');
}

function readPreviousManifest(outputDir: string): GeneratedClientManifest | undefined {
  const manifestPath = resolve(outputDir, MANIFEST_FILE);
  if (!existsSync(manifestPath)) return undefined;
  try {
    const value = JSON.parse(readFileSync(manifestPath, 'utf8')) as Partial<GeneratedClientManifest>;
    if (!Array.isArray(value.files)) throw new Error('files must be an array');
    return value as GeneratedClientManifest;
  } catch (error) {
    throw new Error(
      `clientgen: cannot safely synchronize ${manifestPath}: existing manifest is invalid (${error instanceof Error ? error.message : String(error)})`,
    );
  }
}

/** Validate stale ownership before any generated byte is changed. */
function preflightOwnedOutput(outputDir: string, nextFiles: ReadonlySet<string>, nextHasManifest: boolean): void {
  const previous = readPreviousManifest(outputDir);
  if (!previous) return;
  for (const file of previous.files) {
    if (nextFiles.has(file.path)) continue;
    const filePath = resolveOwnedPath(outputDir, file.path);
    if (!existsSync(filePath)) continue;
    const actual = sha256(readFileSync(filePath));
    if (actual !== file.sha256) {
      throw new Error(
        `clientgen: refusing to remove modified generated file ${file.path}; restore it or move the external edits before synchronizing`,
      );
    }
  }
  if (!nextHasManifest && previous.files.some((file) => nextFiles.has(file.path))) {
    throw new Error('clientgen: internal manifest ownership mismatch');
  }
}

/** Delete only stale bytes explicitly owned by the previous valid inventory. */
function removeOwnedOutput(outputDir: string, nextFiles: ReadonlySet<string> | undefined): void {
  const previous = readPreviousManifest(outputDir);
  if (!previous) return;
  preflightOwnedOutput(outputDir, nextFiles ?? new Set(), nextFiles !== undefined);
  for (const file of previous.files) {
    if (nextFiles?.has(file.path)) continue;
    rmSync(resolveOwnedPath(outputDir, file.path), { force: true });
  }
  if (nextFiles === undefined) rmSync(resolve(outputDir, MANIFEST_FILE), { force: true });
}

function resolveOwnedPath(outputDir: string, relativePath: string): string {
  const filePath = resolve(outputDir, relativePath);
  if (!filePath.startsWith(`${outputDir}${sep}`) || relativePath === MANIFEST_FILE) {
    throw new Error(`clientgen: existing manifest contains unsafe generated path ${JSON.stringify(relativePath)}`);
  }
  return filePath;
}

/**
 * Resolve the OpenAPI spec a build produced for `projectRoot`, preferring the
 * fresh `.gen` JSON or gzipped artifact before the committed snapshot. This covers a
 * TypeScript provider (committed `schema/openapi.json` by default) and a Go
 * provider (`.gen/schema/openapi.json` from the describer's OutputDir).
 */
function readProjectSpec(projectRoot: string, config: ClientGenConfig, selectedPath?: string): ProjectSpec {
  for (const p of uniquePaths([selectedPath, joinPath(projectRoot, '.gen', 'schema', 'openapi.json')])) {
    if (fileExists(p)) {
      const source = readFileSync(p, 'utf8');
      return { ir: readOpenApiSource(source, { mode: config.thirdParty ? 'thirdParty' : 'firstParty' }), source };
    }
  }
  const gz = joinPath(projectRoot, '.gen', 'schema', 'openapi.json.gz');
  if (fileExists(gz)) {
    const source = gunzipSync(readFileSync(gz)).toString('utf8');
    return { ir: readOpenApiSource(source, { mode: config.thirdParty ? 'thirdParty' : 'firstParty' }), source };
  }
  const committed = joinPath(projectRoot, 'schema', 'openapi.json');
  if (fileExists(committed)) {
    const source = readFileSync(committed, 'utf8');
    return { ir: readOpenApiSource(source, { mode: config.thirdParty ? 'thirdParty' : 'firstParty' }), source };
  }
  throw new Error(
    `clientgen: no OpenAPI spec under ${projectRoot} (looked for schema/openapi.json, .gen/schema/openapi.json[.gz]); run \`putnami build\` first.`,
  );
}

function uniquePaths(paths: Array<string | undefined>): string[] {
  return [...new Set(paths.filter((path): path is string => path !== undefined))];
}
