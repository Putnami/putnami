#!/usr/bin/env bun
/**
 * @putnami/web:manifest-budget CLI command
 *
 * Enforce the per-route client-JS budget from the web rendering determinism
 * manifest. Compares the current build's manifest against a committed baseline
 * and/or an absolute cap, and exits non-zero when a route's client JS regresses
 * beyond the configured threshold — so a budget regression fails CI.
 *
 * Usage:
 *   bunx putnami-web-manifest-budget <current.json> \
 *     [--baseline <baseline.json>] [--max-growth-percent N] [--max-bytes N]
 *
 * Exit codes:
 *   0  within budget, or the current manifest is absent (web not built — skipped)
 *   1  budget exceeded
 *   2  usage error
 */

import { file } from 'bun';
import { checkBudget, type BudgetOptions, formatBudgetReport, hasBudgetViolations } from '../src/ssr/budget';
import type { WebManifest } from '../src/ssr/manifest';

interface CliArgs {
  current?: string;
  baseline?: string;
  options: BudgetOptions;
}

function usage(): never {
  // biome-ignore lint/suspicious/noConsole: CLI usage output
  console.error(
    'Usage: putnami-web-manifest-budget <current.json> [--baseline <baseline.json>] [--max-growth-percent N] [--max-bytes N]',
  );
  process.exit(2);
}

function parseNumberFlag(name: string, raw: string | undefined): number {
  const value = Number(raw);
  if (raw === undefined || Number.isNaN(value) || value < 0) {
    // biome-ignore lint/suspicious/noConsole: CLI usage output
    console.error(`Invalid value for ${name}: ${raw ?? '(missing)'}`);
    process.exit(2);
  }
  return value;
}

function parseArgs(argv: string[]): CliArgs {
  const args: CliArgs = { options: {} };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    switch (arg) {
      case '--baseline':
        args.baseline = argv[++i];
        break;
      case '--max-growth-percent':
        args.options.maxGrowthPercent = parseNumberFlag(arg, argv[++i]);
        break;
      case '--max-bytes':
        args.options.maxBytes = parseNumberFlag(arg, argv[++i]);
        break;
      default:
        if (arg.startsWith('--') || args.current) usage();
        args.current = arg;
    }
  }
  return args;
}

async function readManifest(path: string): Promise<WebManifest> {
  return JSON.parse(await file(path).text()) as WebManifest;
}

async function main(): Promise<void> {
  const log = (msg: string) => process.stdout.write(`${msg}\n`);
  const { current, baseline, options } = parseArgs(process.argv.slice(2));

  if (!current) usage();
  if (baseline === undefined && options.maxBytes === undefined) {
    // biome-ignore lint/suspicious/noConsole: CLI usage output
    console.error('Nothing to check: pass --baseline and/or --max-bytes.');
    process.exit(2);
  }

  // Impacted-only CI runs may not rebuild the web app; treat an absent current
  // manifest as "nothing to gate" rather than a failure.
  if (!(await file(current).exists())) {
    log(`Web manifest not found at ${current} — web not built, skipping budget gate.`);
    process.exit(0);
  }

  const [currentManifest, baselineManifest] = await Promise.all([
    readManifest(current),
    baseline ? readManifest(baseline) : Promise.resolve(undefined),
  ]);

  const report = checkBudget(currentManifest, options, baselineManifest);
  log(formatBudgetReport(report));
  process.exit(hasBudgetViolations(report) ? 1 : 0);
}

if (import.meta.main) {
  void main();
}
