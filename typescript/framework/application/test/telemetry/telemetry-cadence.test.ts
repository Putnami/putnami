import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import {
  cadenceMs,
  DEFAULT_METRIC_INTERVAL_MS,
  DEFAULT_SPAN_BATCH_DELAY_MS,
  ENV_METRIC_EXPORT_INTERVAL,
  ENV_SPAN_BATCH_DELAY,
  ENV_SPAN_BATCH_SIZE,
  MAX_SPAN_BATCH_SIZE,
  spanBatchSize,
} from '../../src/telemetry/telemetry.cadence';
import type { TelemetryCollector, TelemetrySpanRecord } from '../../src/telemetry/telemetry.collector';
import { telemetry } from '../../src/telemetry/telemetry.plugin';
import { getCollector, incCounter } from '../../src/telemetry/telemetry.utils';

const FEATURE = 'typescript/service-telemetry';
const REQUIREMENT = 'export-cadence';
const ENV_KEYS = [ENV_METRIC_EXPORT_INTERVAL, ENV_SPAN_BATCH_DELAY, ENV_SPAN_BATCH_SIZE] as const;
/** The export defaults both runtimes test against. */
const EXPORT_CONTRACT = join(
  import.meta.dir,
  '../../../../../protocols/telemetry/fixtures/conventions/export-contract.json',
);

/** The collector the running plugin installed; a test without one fails instead of passing vacuously. */
function active(): TelemetryCollector {
  const collector = getCollector();
  if (!collector) throw new Error('the telemetry plugin installed no collector');
  return collector;
}

interface ReceivedPush {
  path: string;
  body: Record<string, unknown>;
}

/** A collector on a local port that records every push. */
function fakeCollector() {
  const pushes: ReceivedPush[] = [];
  const server = Bun.serve({
    port: 0,
    async fetch(req) {
      pushes.push({ path: new URL(req.url).pathname, body: (await req.json()) as Record<string, unknown> });
      return new Response(null, { status: 200 });
    },
  });
  return { url: `http://localhost:${server.port}`, pushes, stop: () => server.stop(true) };
}

/** The spans a traces request body carries. */
function spanCount(body: Record<string, unknown>): number {
  const list = body['resourceSpans'] as Array<{ scopeSpans: Array<{ spans: unknown[] }> }>;
  return list.reduce((sum, r) => sum + r.scopeSpans.reduce((n, s) => n + s.spans.length, 0), 0);
}

function span(index: number): TelemetrySpanRecord {
  return {
    traceId: '5b8efff798038103d269b633813fc60c',
    spanId: index.toString(16).padStart(16, '0'),
    name: 'work',
    kind: 2,
    startTimeUnixNano: '1700000000000000000',
    endTimeUnixNano: '1700000000001000000',
    attributes: {},
    statusCode: 1,
  };
}

async function waitFor(condition: () => boolean, timeoutMs = 5000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!condition() && Date.now() < deadline) await Bun.sleep(10);
}

describe('telemetry export cadence', () => {
  let app: Application | undefined;
  let collector: ReturnType<typeof fakeCollector>;
  let saved: Record<string, string | undefined>;

  beforeEach(() => {
    saved = Object.fromEntries(ENV_KEYS.map((key) => [key, process.env[key]]));
    for (const key of ENV_KEYS) delete process.env[key];
    collector = fakeCollector();
  });

  afterEach(async () => {
    await app?.stop();
    app = undefined;
    collector.stop();
    for (const key of ENV_KEYS) {
      if (saved[key] === undefined) delete process.env[key];
      else process.env[key] = saved[key];
    }
  });

  const paths = () => collector.pushes.map((push) => push.path);

  specTest(
    'metrics leave every 60 seconds by default',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'metrics-leave-every-60-seconds-by-default' },
    async () => {
      expect(DEFAULT_METRIC_INTERVAL_MS).toBe(60_000);
      expect(cadenceMs(undefined, ENV_METRIC_EXPORT_INTERVAL, DEFAULT_METRIC_INTERVAL_MS, {})).toBe(60_000);

      app = application().use(telemetry({ enabled: true, endpoint: collector.url }));
      await app.start();
      active();
      incCounter('jobs.done');
      // A completed second is ready to push, yet the 60-second timer has not fired.
      await Bun.sleep(1100);
      expect(paths()).not.toContain('/v1/metrics');
    },
  );

  specTest(
    'spans leave in 30-second batches by default',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'spans-leave-in-30-second-batches-by-default' },
    async () => {
      expect(DEFAULT_SPAN_BATCH_DELAY_MS).toBe(30_000);
      expect(cadenceMs(undefined, ENV_SPAN_BATCH_DELAY, DEFAULT_SPAN_BATCH_DELAY_MS, {})).toBe(30_000);

      app = application().use(telemetry({ enabled: true, endpoint: collector.url }));
      await app.start();
      active().addSpan(span(1));
      await Bun.sleep(100);
      expect(paths()).not.toContain('/v1/traces');
    },
  );

  specTest(
    'a full span batch leaves early',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-full-span-batch-leaves-early' },
    async () => {
      app = application().use(telemetry({ enabled: true, endpoint: collector.url }));
      await app.start();
      const spans = active();
      for (let i = 1; i < MAX_SPAN_BATCH_SIZE; i++) spans.addSpan(span(i));
      await Bun.sleep(50);
      expect(paths()).not.toContain('/v1/traces');

      spans.addSpan(span(MAX_SPAN_BATCH_SIZE));
      await waitFor(() => paths().includes('/v1/traces'));
      const traces = collector.pushes.filter((push) => push.path === '/v1/traces');
      expect(traces).toHaveLength(1);
      expect(spanCount(traces[0].body)).toBe(MAX_SPAN_BATCH_SIZE);
    },
  );

  specTest(
    'config and environment override the cadence',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'config-and-environment-override-the-cadence' },
    async () => {
      const env = { [ENV_METRIC_EXPORT_INTERVAL]: '15000' };
      expect(cadenceMs(undefined, ENV_METRIC_EXPORT_INTERVAL, DEFAULT_METRIC_INTERVAL_MS, env)).toBe(15_000);
      expect(cadenceMs(5, ENV_METRIC_EXPORT_INTERVAL, DEFAULT_METRIC_INTERVAL_MS, env)).toBe(5000);
      expect(cadenceMs(1e9, ENV_METRIC_EXPORT_INTERVAL, DEFAULT_METRIC_INTERVAL_MS, {})).toBe(2_147_483_647);
      expect(spanBatchSize({})).toBe(MAX_SPAN_BATCH_SIZE);
      expect(spanBatchSize({ [ENV_SPAN_BATCH_SIZE]: '128' })).toBe(128);
      for (const bad of ['soon', '-5', '0', '1.5', '+15000']) {
        expect(
          cadenceMs(undefined, ENV_SPAN_BATCH_DELAY, DEFAULT_SPAN_BATCH_DELAY_MS, { [ENV_SPAN_BATCH_DELAY]: bad }),
        ).toBe(30_000);
      }

      // The plugin reads both variables: short cadences push before stop.
      process.env[ENV_METRIC_EXPORT_INTERVAL] = '20';
      process.env[ENV_SPAN_BATCH_DELAY] = '20';
      app = application().use(telemetry({ enabled: true, endpoint: collector.url }));
      await app.start();
      incCounter('jobs.done');
      active().addSpan(span(1));
      await waitFor(() => paths().includes('/v1/metrics') && paths().includes('/v1/traces'));
      expect(paths()).toContain('/v1/traces');
      expect(paths()).toContain('/v1/metrics');
    },
  );

  specTest(
    'stopping pushes the remaining metrics and spans',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'stopping-pushes-the-remaining-metrics-and-spans' },
    async () => {
      app = application().use(telemetry({ enabled: true, endpoint: collector.url }));
      await app.start();
      incCounter('jobs.done');
      active().addSpan(span(1));
      expect(collector.pushes).toHaveLength(0);

      await app.stop();
      app = undefined;
      expect(paths()).toContain('/v1/metrics');
      expect(paths()).toContain('/v1/traces');
    },
  );

  specTest(
    'the defaults match the protocol fixture',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'the-defaults-match-the-protocol-fixture' },
    () => {
      const contract = JSON.parse(readFileSync(EXPORT_CONTRACT, 'utf8')) as Record<string, unknown>;
      expect(DEFAULT_METRIC_INTERVAL_MS).toBe(contract['defaultMetricIntervalMs'] as number);
      expect(DEFAULT_SPAN_BATCH_DELAY_MS).toBe(contract['defaultSpanBatchDelayMs'] as number);
      expect(MAX_SPAN_BATCH_SIZE).toBe(contract['maxSpanBatchSize'] as number);
    },
  );
});
