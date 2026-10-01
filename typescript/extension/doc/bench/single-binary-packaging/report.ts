#!/usr/bin/env bun
/**
 * Renders the markdown tables in README.md from a bench run's raw results.
 *
 *   bun report.ts $BENCH_WORK
 *
 * Reads build.tsv, process.json, docker.tsv and compat.json — whichever exist —
 * so a partial run (`./bench.sh build && ./bench.sh report`) still prints.
 */
import { existsSync, readFileSync } from 'node:fs';

const work = process.argv[2] ?? `${process.env['TMPDIR'] ?? '/tmp'}/putnami-single-binary-bench`;

/** Bench output is the product here, so it goes to stdout, not a logger. */
const out = (text: string) => process.stdout.write(`${text}\n`);

const mib = (bytes: number) => `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
const kib = (kilobytes: number | null) => (kilobytes === null ? 'n/a' : `${(kilobytes / 1024).toFixed(1)} MiB`);
const pct = (value: number, baseline: number) => {
  const delta = ((value - baseline) / baseline) * 100;
  return `${delta >= 0 ? '+' : ''}${delta.toFixed(0)}%`;
};

function table(header: string[], rows: string[][]): string {
  const lines = [`| ${header.join(' | ')} |`, `| ${header.map(() => '---').join(' | ')} |`];
  for (const row of rows) lines.push(`| ${row.join(' | ')} |`);
  return lines.join('\n');
}

if (existsSync(`${work}/build.tsv`)) {
  const rows = readFileSync(`${work}/build.tsv`, 'utf8')
    .trim()
    .split('\n')
    .map((line) => line.split('\t'));
  const baseline = Number(rows[0][1]);
  out('\n### Build time and artifact size\n');
  out(
    table(
      ['Variant', 'Artifact', 'vs bundle', 'Build (best of 3)'],
      rows.map(([name, bytes, ms]) => [
        `\`${name}\``,
        mib(Number(bytes)),
        name === 'bun-bundle' ? '—' : `${(Number(bytes) / baseline).toFixed(0)}×`,
        `${ms} ms`,
      ]),
    ),
  );
}

if (existsSync(`${work}/determinism.tsv`)) {
  const rows = readFileSync(`${work}/determinism.tsv`, 'utf8')
    .trim()
    .split('\n')
    .map((line) => line.split('\t'));
  out('\n### Build reproducibility (two compiles of identical inputs)\n');
  out(
    table(
      ['Compile flags', 'Byte-identical', 'Differing bytes'],
      rows.map(([label, identical, differing]) => [`\`${label}\``, identical, differing]),
    ),
  );
}

if (existsSync(`${work}/process.json`)) {
  const data = JSON.parse(readFileSync(`${work}/process.json`, 'utf8'));
  const baseline = data.results[0];
  out('\n### Bare-process cold start and memory\n');
  out(
    table(
      ['Variant', 'Cold start median', 'min', 'max', 'vs baseline', 'RSS idle', 'RSS peak under load'],
      data.results.map((result: Record<string, never>) => {
        const cold = result['coldStartMs'] as unknown as { median: number; min: number; max: number };
        return [
          result['name'] as unknown as string,
          `${cold.median} ms`,
          `${cold.min} ms`,
          `${cold.max} ms`,
          result === baseline ? '—' : pct(cold.median, (baseline.coldStartMs as { median: number }).median),
          kib(result['idleRssKb'] as unknown as number),
          kib(result['peakRssKb'] as unknown as number),
        ];
      }),
    ),
  );
  out('\n### Load phase (same run as the memory numbers)\n');
  out(
    table(
      ['Variant', 'HTTP ok', 'HTTP errors', 'HTTP req/s', 'WS frames received', 'WS errors'],
      data.results.map((result: Record<string, never>) => [
        result['name'] as unknown as string,
        String(result['httpOps']),
        String(result['httpErrors']),
        String(result['httpOpsPerSec']),
        String(result['wsMessagesReceived']),
        String(result['wsErrors']),
      ]),
    ),
  );
}

if (existsSync(`${work}/docker.tsv`)) {
  const rows = readFileSync(`${work}/docker.tsv`, 'utf8')
    .trim()
    .split('\n')
    .map((line) => line.split('\t'));
  const baselinePull = Number(rows[0][1]);
  const median = (samples: string) => {
    const values = samples
      .trim()
      .split(/\s+/)
      .map(Number)
      .sort((a, b) => a - b);
    const mid = Math.floor(values.length / 2);
    return { min: values[0], median: values.length % 2 === 0 ? (values[mid - 1] + values[mid]) / 2 : values[mid] };
  };
  // docker stats emits `11.94MiB`; `n/a` marks a container that never got ready.
  const formatMem = (mem: string) => mem.replace(/^(\d[\d.]*)([KMGT]i?B)$/, '$1 $2');
  out('\n### Image size, container cold start, container idle memory\n');
  out(
    table(
      [
        'Image',
        'Pull (compressed)',
        'vs baseline',
        'Unpacked',
        'Cold start min / median',
        '`docker run` overhead (median)',
        'Idle memory',
      ],
      rows.map(([name, pull, unpacked, totals, overheads, mem]) => {
        const cold = median(totals);
        return [
          `\`${name}\``,
          mib(Number(pull)),
          name === rows[0][0] ? '—' : pct(Number(pull), baselinePull),
          mib(Number(unpacked)),
          `${cold.min} ms / ${cold.median} ms`,
          `${median(overheads).median} ms`,
          formatMem(mem),
        ];
      }),
    ),
  );
}

if (existsSync(`${work}/compat.json`)) {
  const data = JSON.parse(readFileSync(`${work}/compat.json`, 'utf8'));
  const icon = (status: string) => (status === 'pass' ? 'yes' : status === 'partial' ? 'conditional' : 'no');
  out('\n### Compatibility checklist\n');
  out(
    table(
      ['Check', 'Works', 'Observed'],
      data.results.map((result: { check: string; status: string; detail: string }) => [
        result.check,
        icon(result.status),
        result.detail.replace(/\|/g, '\\|'),
      ]),
    ),
  );
}
