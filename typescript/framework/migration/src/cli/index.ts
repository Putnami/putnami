/**
 * Kind-agnostic migrate CLI driver. A service binary's `bin/migrate.ts`
 * is ~5 lines:
 *
 * ```ts
 * #!/usr/bin/env bun
 * import { runMigrate } from '@putnami/migration/cli';
 * import { buildApp } from '../src/app';
 *
 * process.exit(await runMigrate(buildApp, process.argv.slice(2)));
 * ```
 *
 * The CLI builds the production application graph (same Application
 * the production cmd/server uses), calls `prepare()` so the migration
 * registry is populated, then dispatches the requested subcommand
 * directly against the registry — no HTTP listener starts, no `start()`
 * runs.
 *
 * Subcommand surface:
 *
 *   up                Apply all pending migrations across kinds.
 *   up to <name>      Apply forward through <name> (inclusive).
 *   down              Roll back the most recently applied migration.
 *   down to <name>    Roll back every migration applied after <name>.
 *   status            Tabular per-row state of every kind.
 *   verify            Drift report (exit 3 on drift).
 *   inspect           JSON dump of the registry view (no DB access).
 *
 * Exit codes:
 *   0  success
 *   1  user error (unknown subcommand, missing argument)
 *   2  operational error (prepare failed, runner error)
 *   3  drift detected by verify
 */

import type { MigrationRegistry } from '../registry';
import type { ApplyOpts, DriftReport, Kind, MigrationRecord, MigrationSource, RollbackOpts } from '../types';
import { isDriftReportEmpty } from '../types';

/** Builder that returns the production Application instance. */
export interface AppLike {
  prepare(): Promise<void>;
  stop(): Promise<void>;
  getMigrationRegistry(): MigrationRegistry;
}

export type AppBuilder = () => AppLike;

/** Output format for command results: human table/prose (default) or JSON Lines. */
type OutputMode = 'table' | 'jsonl';

interface Streams {
  stdout: (s: string) => void;
  stderr: (s: string) => void;
}

const defaultStreams: Streams = {
  stdout: (s) => process.stdout.write(s),
  stderr: (s) => process.stderr.write(s),
};

/**
 * Entry point. `argv` is everything after the binary name, e.g.
 * process.argv.slice(2). Returns the exit code; callers wrap with
 * process.exit.
 */
export async function runMigrate(build: AppBuilder, argv: string[], streams: Partial<Streams> = {}): Promise<number> {
  const out: Streams = { ...defaultStreams, ...streams };

  if (!build) {
    out.stderr('migrate: AppBuilder is required\n');
    return 1;
  }
  if (argv.length === 0) {
    printUsage(out.stderr);
    return 1;
  }

  if (argv[0] === '-h' || argv[0] === '--help' || argv[0] === 'help') {
    printUsage(out.stdout);
    return 0;
  }

  const parsedOutput = parseOutputFlag(argv, out);
  if (parsedOutput.exit !== 0) return parsedOutput.exit;
  const { output } = parsedOutput;

  const [sub, ...rest] = parsedOutput.rest;
  if (!sub) {
    printUsage(out.stderr);
    return 1;
  }

  const app = build();
  if (!app) {
    out.stderr('migrate: AppBuilder returned a falsy application\n');
    return 1;
  }

  try {
    await app.prepare();
  } catch (err) {
    out.stderr(`migrate: prepare failed: ${formatError(err)}\n`);
    return 2;
  }

  try {
    const reg = app.getMigrationRegistry();
    switch (sub) {
      case 'up':
        return await runUp(reg, rest, out, output);
      case 'down':
        return await runDown(reg, rest, out, output);
      case 'status':
        return await runStatus(reg, out, output);
      case 'verify':
        return await runVerify(reg, out, output);
      case 'inspect':
        return runInspect(reg, out, output);
      default:
        out.stderr(`migrate: unknown subcommand "${sub}"\n`);
        printUsage(out.stderr);
        return 1;
    }
  } finally {
    await safeStop(app);
  }
}

async function safeStop(app: AppLike): Promise<void> {
  try {
    await app.stop();
  } catch {
    // best-effort
  }
}

async function runUp(reg: MigrationRegistry, rest: string[], out: Streams, output: OutputMode): Promise<number> {
  const parsed = parseToArg('up', rest, out);
  if (parsed.exit !== 0) return parsed.exit;
  const opts: ApplyOpts = { to: parsed.target, force: true };
  try {
    const records = await reg.applyAll(opts);
    if (records.length === 0) {
      if (output === 'table') out.stdout('no pending migrations\n');
      return 0;
    }
    writeRecords(records, out.stdout, output);
    return 0;
  } catch (err) {
    out.stderr(`migrate up: ${formatError(err)}\n`);
    return 2;
  }
}

async function runDown(reg: MigrationRegistry, rest: string[], out: Streams, output: OutputMode): Promise<number> {
  const parsed = parseToArg('down', rest, out);
  if (parsed.exit !== 0) return parsed.exit;
  const opts: RollbackOpts = { to: parsed.target };

  const all: MigrationRecord[] = [];
  // Roll back every kind that has a runner, collecting per-kind failures
  // instead of discarding them. A rollback is destructive, so a partial
  // failure must never be reported as success: we still print the records
  // that DID roll back, but surface every kind's error and exit non-zero.
  const errors: Error[] = [];
  for (const kind of reg.kinds()) {
    const runner = reg.runnerFor(kind);
    if (!runner) {
      // Skip silently on `down`: kinds() unions sources + runners; a
      // kind with sources-but-no-runner is caught loudly by
      // assertNoOrphanSources on apply / status / verify. Tolerating
      // the absence here lets `down` proceed for kinds that DO have
      // runners instead of short-circuiting on unrelated config drift.
      continue;
    }
    try {
      // biome-ignore lint/performance/noAwaitInLoops: ordered across kinds
      const records = await runner.rollback(opts);
      all.push(...records);
    } catch (err) {
      const cause = err instanceof Error ? err : new Error(formatError(err));
      errors.push(new Error(`[${kind}] ${cause.message}`, { cause }));
    }
  }
  writeRecords(all, out.stdout, output);
  if (errors.length > 0) {
    const failure =
      errors.length === 1
        ? (errors[0] as Error)
        : new AggregateError(errors, `rollback failed for ${errors.length} kinds`);
    out.stderr(`migrate down: ${formatError(failure)}\n`);
    for (const e of errors) {
      out.stderr(`  ${e.message}\n`);
    }
    return 2;
  }
  if (all.length === 0 && output === 'table') {
    out.stdout('nothing to roll back\n');
  }
  return 0;
}

async function runStatus(reg: MigrationRegistry, out: Streams, output: OutputMode): Promise<number> {
  try {
    const records = await reg.statusAll();
    if (records.length === 0) {
      if (output === 'table') out.stdout('no migrations registered\n');
      return 0;
    }
    writeRecords(records, out.stdout, output);
    return 0;
  } catch (err) {
    out.stderr(`migrate status: ${formatError(err)}\n`);
    return 2;
  }
}

async function runVerify(reg: MigrationRegistry, out: Streams, output: OutputMode): Promise<number> {
  let reports: DriftReport[];
  try {
    reports = await reg.verifyAll();
  } catch (err) {
    out.stderr(`migrate verify: ${formatError(err)}\n`);
    return 2;
  }

  const exit = reports.some((r) => !isDriftReportEmpty(r)) ? 3 : 0;

  if (output === 'jsonl') {
    for (const r of reports) {
      out.stdout(`${JSON.stringify(r)}\n`);
    }
    return exit;
  }

  for (const r of reports) {
    if (isDriftReportEmpty(r)) {
      out.stdout(`[${r.kind}] no drift\n`);
      continue;
    }
    out.stdout(`[${r.kind}] DRIFT DETECTED\n`);
    for (const h of r.hashDrifts ?? []) {
      out.stdout(
        `  hash-drift: ${h.namespace ?? ''}/${h.name} target=${h.target ?? ''} stored=${short(h.storedHash)} current=${short(h.currentHash)}\n`,
      );
    }
    for (const rec of r.missingFromRegistry ?? []) {
      out.stdout(
        `  missing-from-registry: ${rec.name} target=${rec.target ?? ''} (applied row not present in registered migrations)\n`,
      );
    }
    for (const rec of r.missingFromStore ?? []) {
      out.stdout(`  pending: ${rec.name} target=${rec.target ?? ''} source=${rec.source ?? ''}\n`);
    }
  }
  return exit;
}

function runInspect(reg: MigrationRegistry, out: Streams, output: OutputMode): number {
  const kinds = reg.kinds().map((kind: Kind) => ({
    kind,
    runnerRegistered: reg.runnerFor(kind) !== undefined,
    sources: reg.sourcesFor(kind).map((s: MigrationSource) => ({ namespace: s.namespace })),
  }));
  if (output === 'jsonl') {
    for (const k of kinds) {
      out.stdout(`${JSON.stringify(k)}\n`);
    }
    return 0;
  }
  out.stdout(`${JSON.stringify({ kinds }, null, 2)}\n`);
  return 0;
}

// --- helpers -------------------------------------------------------------

function parseToArg(sub: string, args: string[], out: Streams): { target?: string; exit: number } {
  if (args.length === 0) return { exit: 0 };
  if (args.length === 2 && args[0]?.toLowerCase() === 'to') {
    if (!args[1]) {
      out.stderr(`migrate ${sub}: 'to' requires a migration name\n`);
      return { exit: 1 };
    }
    return { target: args[1], exit: 0 };
  }
  out.stderr(`migrate ${sub}: unexpected arguments ${JSON.stringify(args)} (use '${sub}' or '${sub} to <name>')\n`);
  return { exit: 1 };
}

/**
 * Extracts `--output table|jsonl` (default `table`) from argv, returning the
 * remaining tokens for subcommand dispatch. The flag may appear anywhere.
 */
function parseOutputFlag(argv: string[], out: Streams): { output: OutputMode; rest: string[]; exit: number } {
  const rest: string[] = [];
  let output: OutputMode = 'table';
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    let value: string | undefined;
    if (arg === '--output') {
      value = argv[++i];
    } else if (arg?.startsWith('--output=')) {
      value = arg.slice('--output='.length);
    } else {
      if (arg !== undefined) rest.push(arg);
      continue;
    }
    if (value !== 'table' && value !== 'jsonl') {
      out.stderr(`migrate: --output must be 'table' or 'jsonl'${value === undefined ? '' : `, got "${value}"`}\n`);
      return { output, rest, exit: 1 };
    }
    output = value;
  }
  return { output, rest, exit: 0 };
}

function writeRecords(records: MigrationRecord[], write: (s: string) => void, output: OutputMode): void {
  if (records.length === 0) return;
  if (output === 'jsonl') {
    for (const r of records) {
      write(`${JSON.stringify(r)}\n`);
    }
    return;
  }
  const headers = ['KIND', 'NAMESPACE', 'NAME', 'STATUS', 'TARGET', 'SOURCE'];
  const rows = records.map((r) => [r.kind, r.namespace ?? '', r.name, r.status, r.target ?? '', r.source ?? '']);
  const widths = headers.map((h, i) => Math.max(h.length, ...rows.map((row) => row[i]?.length ?? 0)));
  const fmt = (cols: string[]): string => cols.map((c, i) => c.padEnd(widths[i] ?? 0)).join('  ');
  write(`${fmt(headers)}\n`);
  for (const row of rows) {
    write(`${fmt(row)}\n`);
  }
}

function short(hash: string): string {
  return hash.length <= 8 ? hash : `${hash.slice(0, 8)}...`;
}

function formatError(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}

function printUsage(write: (s: string) => void): void {
  write(`migrate — Putnami migration driver

Usage:
  migrate <subcommand> [args]

Subcommands:
  up                    Apply all pending migrations across kinds
  up to <name>          Apply forward through <name> (inclusive)
  down                  Roll back the most recently applied migration
  down to <name>        Roll back every migration applied after <name>
  status                Tabular per-row state of every kind
  verify                Drift report (exit 3 on drift)
  inspect               JSON dump of the registry view (no DB access)

Options:
  --output table|jsonl  Output format; jsonl emits one JSON object per line (default: table)
`);
}

/** Convenience exit: process.exit(await runMigrate(...)) one-liner. */
export async function runMigrateAndExit(build: AppBuilder): Promise<never> {
  const code = await runMigrate(build, process.argv.slice(2));
  process.exit(code);
}
