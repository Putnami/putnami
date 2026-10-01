/**
 * Build-time A/B harness for Bun bundler flags on the web client build.
 *
 * Run:  bun run bench/build-ab.ts [--entry <path>] [--iterations N]
 *       (from typescript/framework/web; requires the 03-web sample built:
 *        ./putnamiw build --projects @example/03-web)
 *
 * Rebuilds a real client entrypoint (default: the generated hydrate entry of
 * `typescript/samples/03-web`) once per arm and reports build time, raw JS
 * bytes, and gzipped JS bytes:
 *
 *   - baseline          — the exact flags `buildEntrypoint` uses today
 *                         (src/ssr/generator/build.ts)
 *   - react-compiler    — baseline + `reactCompiler: true` (Bun 1.4)
 *   - optimize-imports  — baseline + `optimizeImports` for the workspace
 *                         barrel packages (Bun 1.4)
 *
 * The arms must mirror `buildEntrypoint`'s CLI args — if that function's flag
 * set changes, update `baselineOptions` here to match, or the A/B stops
 * measuring the real pipeline.
 *
 * Bundle-byte deltas are deterministic; build times are machine-dependent, so
 * compare medians from the same host only (same policy as bench.ts).
 */

import { gzipSync } from 'bun';
import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

interface ArmResult {
  name: string;
  medianMs: number;
  rawBytes: number;
  gzipBytes: number;
  files: number;
}

/** Packages whose index files are pure re-export barrels in this workspace. */
const BARREL_PACKAGES = ['@putnami/web', '@putnami/application', '@putnami/runtime', '@putnami/utils'];

/** Mirror of the `bun build` flags in src/ssr/generator/build.ts `buildEntrypoint`. */
function baselineOptions(entrypoint: string, outdir: string): Parameters<typeof Bun.build>[0] {
  return {
    entrypoints: [entrypoint],
    outdir,
    target: 'browser',
    format: 'esm',
    banner: '"use client"',
    splitting: true,
    sourcemap: 'none',
    minify: true,
    external: ['bun'],
    conditions: ['browser'],
    define: { 'process.env.NODE_ENV': '"production"' },
    throw: false,
  };
}

const arms: { name: string; extra: Record<string, unknown> }[] = [
  { name: 'baseline', extra: {} },
  { name: 'react-compiler', extra: { reactCompiler: true } },
  { name: 'optimize-imports', extra: { optimizeImports: BARREL_PACKAGES } },
];

function median(values: number[]): number {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.floor(sorted.length / 2)] ?? 0;
}

async function runArm(
  name: string,
  extra: Record<string, unknown>,
  entrypoint: string,
  iterations: number,
  workdir: string,
): Promise<ArmResult> {
  const outdir = join(workdir, name);
  const timings: number[] = [];
  let rawBytes = 0;
  let gzipBytes = 0;
  let files = 0;

  // One extra warmup iteration, discarded (module cache, disk cache).
  for (let i = 0; i <= iterations; i++) {
    rmSync(outdir, { recursive: true, force: true });
    mkdirSync(outdir, { recursive: true });

    const start = performance.now();
    const result = await Bun.build({ ...baselineOptions(entrypoint, outdir), ...extra });
    const elapsed = performance.now() - start;

    if (!result.success) {
      const logs = result.logs.map((l) => String(l)).join('\n');
      throw new Error(`Arm '${name}' failed to build:\n${logs}`);
    }
    if (i === 0) continue;
    timings.push(elapsed);

    rawBytes = 0;
    gzipBytes = 0;
    const jsFiles = readdirSync(outdir)
      .filter((f) => f.endsWith('.js'))
      .sort();
    files = jsFiles.length;
    for (const fileName of jsFiles) {
      const content = readFileSync(join(outdir, fileName));
      rawBytes += content.byteLength;
      gzipBytes += gzipSync(content).byteLength;
    }
  }

  return { name, medianMs: median(timings), rawBytes, gzipBytes, files };
}

function printResults(results: ArmResult[]) {
  const log = (msg: string) => process.stdout.write(`${msg}\n`);
  const nameWidth = Math.max(20, ...results.map((r) => r.name.length + 2));
  const sep = '-'.repeat(nameWidth + 58);
  const baseline = results[0];

  log(sep);
  log(
    `${'Arm'.padEnd(nameWidth)}${'build (ms)'.padStart(12)}${'raw JS (B)'.padStart(14)}${'gzip JS (B)'.padStart(14)}${'Δ gzip'.padStart(10)}${'files'.padStart(8)}`,
  );
  log(sep);
  for (const r of results) {
    const delta = baseline ? r.gzipBytes - baseline.gzipBytes : 0;
    const deltaLabel = delta === 0 ? '=' : `${delta > 0 ? '+' : ''}${delta.toLocaleString()}`;
    log(
      `${r.name.padEnd(nameWidth)}${r.medianMs.toFixed(1).padStart(12)}${r.rawBytes.toLocaleString().padStart(14)}${r.gzipBytes.toLocaleString().padStart(14)}${deltaLabel.padStart(10)}${String(r.files).padStart(8)}`,
    );
  }
  log(sep);
}

function parseArgs(argv: string[]): { entry: string; iterations: number } {
  // Default: the generated hydrate entry of the 03-web sample (workspace-relative).
  let entry = resolve(import.meta.dir, '../../../samples/03-web/.gen/src/app/.react-client.gen.tsx');
  let iterations = 7;
  for (let i = 0; i < argv.length; i++) {
    if (argv[i] === '--entry' && argv[i + 1]) entry = resolve(argv[++i] as string);
    else if (argv[i] === '--iterations' && argv[i + 1]) iterations = Math.max(1, Number(argv[++i]));
  }
  return { entry, iterations };
}

async function main() {
  const log = (msg: string) => process.stdout.write(`${msg}\n`);
  const { entry, iterations } = parseArgs(process.argv.slice(2));

  const entryFile = Bun.file(entry);
  if (!(await entryFile.exists())) {
    log(`Entrypoint not found: ${entry}`);
    log('Build the sample first:  ./putnamiw build --projects @example/03-web');
    process.exitCode = 2;
    return;
  }

  log(`Bun ${Bun.version} — ${iterations} measured builds per arm (1 warmup discarded)`);
  log(`Entry: ${entry}\n`);

  const workdir = mkdtempSync(join(tmpdir(), 'putnami-build-ab-'));
  try {
    const results: ArmResult[] = [];
    for (const arm of arms) {
      results.push(await runArm(arm.name, arm.extra, entry, iterations, workdir));
    }
    printResults(results);
  } finally {
    rmSync(workdir, { recursive: true, force: true });
  }
}

// biome-ignore lint/suspicious/noConsole: benchmark output tool
main().catch(console.error);
