import { spawnSync } from 'node:child_process';
import { existsSync, realpathSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, relative, resolve } from 'node:path';
import { joinPath } from '@putnami/utils';
import type { GeneratedFile } from './ts/ts-generator';

const clientRequire = createRequire(import.meta.url);
const BIOME_PACKAGE = '@biomejs/biome';
const BIOME_PHASE_TIMEOUT_MS = 30_000;
const MIN_OUTPUT_BUFFER_BYTES = 1024 * 1024;
type ProcessEnvironment = Record<string, string | undefined> & {
  RAYON_NUM_THREADS?: string;
  PUTNAMI_CPU_BUDGET?: string;
};

export type GeneratedFileCanonicalizationErrorCode =
  | 'clientgen_formatter_unavailable'
  | 'clientgen_format_config_missing'
  | 'clientgen_format_failed';

/** A formatter failure whose message is safe for the standalone CLI to print. */
export class GeneratedFileCanonicalizationError extends Error {
  constructor(
    readonly code: GeneratedFileCanonicalizationErrorCode,
    detail: string,
  ) {
    super(`${code}: ${detail}`);
    this.name = 'GeneratedFileCanonicalizationError';
  }
}

/**
 * Apply the same writer phases as `putnami lint` to generated files before
 * callers expose any bytes on disk. The stdin path is each file's final path,
 * so Biome evaluates overrides and EditorConfig against its real identity.
 */
export function canonicalizeGeneratedFiles(
  projectRoot: string,
  outputDir: string,
  files: readonly GeneratedFile[],
): GeneratedFile[] {
  const resolvedProjectRoot = resolve(projectRoot);
  const canonicalProjectRoot = realpathSync(resolvedProjectRoot);
  const canonicalOutputDir = resolve(canonicalProjectRoot, relative(resolvedProjectRoot, resolve(outputDir)));
  const configPath = resolveBiomeConfig(canonicalProjectRoot);
  const biomeCli = resolveBiomeCli(canonicalProjectRoot);

  return files.map((file) => {
    const finalPath = resolve(canonicalOutputDir, file.path);
    const configRoot = dirname(configPath);
    const stdinPath = relative(configRoot, finalPath).replaceAll('\\', '/');
    const formatted = runBiome(biomeCli, configPath, configRoot, file.path, stdinPath, file.content, 'format', [
      '--write',
    ]);
    const linted = runBiome(biomeCli, configPath, configRoot, file.path, stdinPath, formatted, 'lint', [
      '--write',
      '--unsafe',
    ]);
    return { path: file.path, content: linted };
  });
}

/** Resolve the provider's Biome package first, then the client's dependency. */
export function resolveBiomeCli(projectRoot: string): string {
  const projectRequire = createRequire(resolve(projectRoot, 'package.json'));
  const projectBiome = resolveInstalledBiomeCli(projectRequire);
  if (projectBiome) return projectBiome;

  const clientBiome = resolveInstalledBiomeCli(clientRequire);
  if (clientBiome) return clientBiome;

  throw new GeneratedFileCanonicalizationError(
    'clientgen_formatter_unavailable',
    '@biomejs/biome is not installed for the provider or @putnami/client; run `putnami deps install`',
  );
}

/** Discover only packages already present on disk, so Bun cannot auto-install a missing provider dependency. */
function resolveInstalledBiomeCli(requireFrom: ReturnType<typeof createRequire>): string | undefined {
  for (const searchPath of requireFrom.resolve.paths(BIOME_PACKAGE) ?? []) {
    const packageRoot = resolve(searchPath, BIOME_PACKAGE);
    const packageJson = resolve(packageRoot, 'package.json');
    const biomeCli = resolve(packageRoot, 'bin/biome');
    if (!existsSync(packageJson) || !existsSync(biomeCli)) continue;
    try {
      return createRequire(packageJson).resolve('./bin/biome');
    } catch {
      // An incomplete installation is not a usable candidate; continue through the normal search order.
    }
  }
  return undefined;
}

/** Apply the scheduler's CPU budget exactly as the TypeScript lint extension does. */
export function resolveBiomeEnvironment(environment: ProcessEnvironment): ProcessEnvironment {
  if (environment.RAYON_NUM_THREADS !== undefined) return environment;
  const rawBudget = environment.PUTNAMI_CPU_BUDGET;
  if (!rawBudget || !/^[+-]?\d+$/.test(rawBudget)) return environment;
  const budget = Number(rawBudget);
  if (!Number.isSafeInteger(budget) || budget <= 0) return environment;
  return { ...environment, RAYON_NUM_THREADS: String(budget) };
}

function resolveBiomeConfig(projectRoot: string): string {
  const projectConfig = joinPath(projectRoot, 'biome.json');
  if (existsSync(projectConfig)) return projectConfig;

  const workspaceRoot = findWorkspaceRoot(projectRoot);
  if (workspaceRoot) {
    const workspaceConfig = joinPath(workspaceRoot, 'biome.json');
    if (existsSync(workspaceConfig)) return workspaceConfig;
  }

  throw new GeneratedFileCanonicalizationError(
    'clientgen_format_config_missing',
    'no project or workspace biome.json was found; create one or run `putnami deps install`',
  );
}

function findWorkspaceRoot(start: string): string | undefined {
  let current = resolve(start);
  while (true) {
    if (['putnami.workspace.json', '.putnamirc.json'].some((file) => existsSync(joinPath(current, file)))) {
      return current;
    }
    const parent = dirname(current);
    if (parent === current) return undefined;
    current = parent;
  }
}

function runBiome(
  biomeCli: string,
  configPath: string,
  configRoot: string,
  relativePath: string,
  stdinPath: string,
  source: string,
  phase: 'format' | 'lint',
  writeArgs: string[],
): string {
  const sourceBytes = Buffer.byteLength(source);
  const result = spawnSync(
    process.execPath,
    [biomeCli, phase, ...writeArgs, `--config-path=${configPath}`, `--stdin-file-path=${stdinPath}`],
    {
      cwd: configRoot,
      encoding: 'utf8',
      env: resolveBiomeEnvironment(process.env),
      input: source,
      maxBuffer: Math.max(MIN_OUTPUT_BUFFER_BYTES, sourceBytes * 2 + 65_536),
      timeout: BIOME_PHASE_TIMEOUT_MS,
    },
  );
  if (result.error || result.signal || result.status !== 0 || typeof result.stdout !== 'string') {
    const exit = result.status === null ? 'unavailable' : String(result.status);
    // Biome fails on the emitted bytes as often as on the configuration — a
    // parse error in generated source lands here too. Its own diagnostic is the
    // only thing that tells the two apart, so it is reported instead of a guess.
    throw new GeneratedFileCanonicalizationError(
      'clientgen_format_failed',
      `Biome ${phase} failed for ${relativePath} (exit ${exit}): ${biomeDiagnostic(result)}`,
    );
  }
  return result.stdout;
}

const BIOME_DIAGNOSTIC_MAX_CHARS = 500;

/**
 * Biome's own summary of what it refused, without the code frames it prints
 * around it.
 *
 * The summary says which of the two possible causes fired — the emitted bytes
 * or the provider's configuration — and that is the whole question a
 * `clientgen_format_failed` has to answer. The frames are dropped because they
 * quote provider source and provider configuration verbatim, and this message
 * is printed by the standalone CLI, which must not echo either.
 */
function biomeDiagnostic(result: { error?: Error; signal?: string | null; stderr?: string; stdout?: string }): string {
  if (result.error) return result.error.message;
  const summary = [result.stderr, result.stdout]
    .filter((stream): stream is string => typeof stream === 'string')
    .join('\n')
    .split('\n')
    .map((line) => line.trim())
    .filter((line) => line.startsWith('\u00d7'))
    .map((line) => line.slice(1).trim())
    .filter((line) => line.length > 0)
    .join(' ');
  if (!summary) {
    return result.signal ? `Biome was terminated by ${result.signal}` : 'Biome reported no diagnostic';
  }
  return summary.length > BIOME_DIAGNOSTIC_MAX_CHARS
    ? `${summary.slice(0, BIOME_DIAGNOSTIC_MAX_CHARS)}\u2026 (truncated)`
    : summary;
}
