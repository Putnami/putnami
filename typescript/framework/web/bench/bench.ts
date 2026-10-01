/**
 * Performance benchmarks for the @putnami/web SSR render path.
 *
 * Run:  bun run bench/bench.ts        (from typescript/framework/web)
 *
 * Measures in-process SSR render throughput (render-to-completion) for
 * representative pages. Deterministic — no network, no server — so the numbers
 * are stable enough for same-host comparison against a committed baseline,
 * making render-cost regressions measurable instead of qualitative.
 *
 * Mirrors the harness in `application/bench/bench.ts`.
 */

import React from 'react';
import { runInContext } from '../../runtime/src/context/context.utils';
import RootHtml from '../src/ssr/default.html';
import { pageRenderer } from '../src/ssr/page.renderer';

// ---------------------------------------------------------------------------
// Helpers (shared shape with application/bench/bench.ts)
// ---------------------------------------------------------------------------

interface BenchResult {
  name: string;
  ops: number;
  avgNs: number;
  p99Ns: number;
}

async function bench(name: string, iterations: number, fn: () => void | Promise<void>): Promise<BenchResult> {
  // Warmup
  const warmup = Math.min(1000, Math.floor(iterations / 10));
  for (let i = 0; i < warmup; i++) await fn();

  // Measure
  const timings: number[] = [];
  const batchSize = 50;
  const batches = Math.ceil(iterations / batchSize);

  for (let b = 0; b < batches; b++) {
    const count = Math.min(batchSize, iterations - b * batchSize);
    const start = performance.now();
    for (let i = 0; i < count; i++) await fn();
    const elapsed = performance.now() - start;
    timings.push(elapsed / count);
  }

  timings.sort((a, b) => a - b);
  const avg = timings.reduce((s, t) => s + t, 0) / timings.length;
  const p99 = timings[Math.floor(timings.length * 0.99)] ?? timings[timings.length - 1];

  return {
    name,
    ops: Math.round(1000 / avg),
    avgNs: Math.round(avg * 1_000_000),
    p99Ns: Math.round(p99 * 1_000_000),
  };
}

function printResults(results: BenchResult[]) {
  const nameWidth = Math.max(40, ...results.map((r) => r.name.length + 2));
  const sep = '-'.repeat(nameWidth + 40);

  const log = (msg: string) => process.stdout.write(`${msg}\n`);

  log(sep);
  log(`${'Benchmark'.padEnd(nameWidth)}${'ops/s'.padStart(12)}${'avg (ns)'.padStart(14)}${'p99 (ns)'.padStart(14)}`);
  log(sep);
  for (const r of results) {
    log(
      `${r.name.padEnd(nameWidth)}${r.ops.toLocaleString().padStart(12)}${r.avgNs.toLocaleString().padStart(14)}${r.p99Ns.toLocaleString().padStart(14)}`,
    );
  }
  log(sep);
}

// ---------------------------------------------------------------------------
// SSR render harness — drives the real pageRenderer to completion
// ---------------------------------------------------------------------------

function createMockContext(overrides: Record<string, unknown> = {}) {
  return {
    req: new Request('http://localhost/'),
    headers: new Headers(),
    params: {},
    queryParams: () => ({}),
    body: async () => undefined,
    statusCode: 0,
    ...overrides,
  } as never;
}

/** Render a single route to completion (streams the full document body). */
async function renderToCompletion(route: Record<string, unknown>): Promise<void> {
  const handler = pageRenderer(
    () => [route as never],
    () => RootHtml,
    () => 'hydrate.js',
    5000,
  );
  const response = await runInContext(createMockContext(), async () => await handler());
  // Drain the streamed body so we measure render-to-completion, not just TTFB.
  await response.get().text();
}

/** A component that re-executes per render (representative of a real page). */
function Table({ rows }: { rows: number }) {
  return React.createElement(
    'table',
    null,
    React.createElement(
      'tbody',
      null,
      Array.from({ length: rows }, (_, i) =>
        React.createElement(
          'tr',
          { key: i },
          React.createElement('td', null, String(i + 1)),
          React.createElement('td', null, `Item ${i + 1}`),
          React.createElement('td', null, (i * 1.5).toFixed(2)),
        ),
      ),
    ),
  );
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

async function benchSsrRender(): Promise<BenchResult[]> {
  const results: BenchResult[] = [];

  results.push(
    await bench('SSR render: minimal page', 5000, () =>
      renderToCompletion({ path: '/', element: React.createElement('div', null, 'Hello SSR') }),
    ),
  );

  results.push(
    await bench('SSR render: 100-row table', 3000, () =>
      renderToCompletion({ path: '/', element: React.createElement(Table, { rows: 100 }) }),
    ),
  );

  results.push(
    await bench('SSR render: 1000-row table', 800, () =>
      renderToCompletion({ path: '/', element: React.createElement(Table, { rows: 1000 }) }),
    ),
  );

  return results;
}

async function main() {
  const log = (msg: string) => process.stdout.write(`${msg}\n`);

  log('Running: SSR render throughput...');
  const allResults = await benchSsrRender();

  log('\n');
  printResults(allResults);
}

// biome-ignore lint/suspicious/noConsole: benchmark output tool
main().catch(console.error);
