import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { ConfigService, type ConfigSource, runInContext, useConfig } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { startTelemetrySpan, type TelemetryTraceContext } from '../../src/telemetry/client-telemetry';
import { type MetricsBucket, TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { TelemetryConfig } from '../../src/telemetry/telemetry.config';
import { TelemetryPlugin, telemetry } from '../../src/telemetry/telemetry.plugin';
import { clearCollector, setCollector } from '../../src/telemetry/telemetry.utils';
import {
  ATTR_TRACE_DECISION_OUTCOME,
  clearTraceSampler,
  DEFAULT_TRACE_SAMPLING,
  getTraceSampler,
  setTraceSampler,
  TRACE_DECISIONS,
  TRACE_SAMPLING_CONFIG_PATH,
  TraceSampler,
  type TraceSampling,
  traceRatioPicks,
  traceSamplingError,
} from '../../src/telemetry/trace-sampling';

interface SamplingFixture {
  configPath: string;
  keys: { budget: string; ratio: string; floorPerMinute: string };
  defaults: TraceSampling;
  counter: { name: string; attribute: string; kept: string; dropped: string };
  ratioCases: Array<{ traceId: string; ratio: number; picked: boolean }>;
  scenarios: Array<{
    name: string;
    settings: TraceSampling;
    roots: Array<{ atSecond: number; traceId: string; kept: boolean }>;
  }>;
}

const fixture = JSON.parse(
  readFileSync(
    join(import.meta.dir, '../../../../../protocols/telemetry/fixtures/conventions/trace-sampling.json'),
    'utf8',
  ),
) as SamplingFixture;

const ENDPOINT = 'http://localhost:0/telemetry';
const NONE_PICKED = 'ffffffffffffffffffffffffffffffff';
const DROP_ALL: TraceSampling = { budget: 0, ratio: 0, floorPerMinute: 0 };

/** How many decisions the buckets count under each outcome. */
function decisions(buckets: readonly MetricsBucket[]): Record<string, number> {
  const totals: Record<string, number> = {};
  for (const bucket of buckets) {
    for (const series of bucket.counterSeries ?? []) {
      if (series.name !== TRACE_DECISIONS) continue;
      const outcome = String(series.attributes[ATTR_TRACE_DECISION_OUTCOME]);
      totals[outcome] = (totals[outcome] ?? 0) + series.value;
    }
  }
  return totals;
}

function setTraces(section: Record<string, unknown> | undefined): void {
  if (section === undefined) {
    delete process.env.CONFIG_DATA;
    return;
  }
  process.env.CONFIG_DATA = JSON.stringify({ telemetry: { traces: section } });
}

function remoteHeaders(flags: string): Headers {
  return new Headers({ traceparent: `00-${'1'.repeat(32)}-${'2'.repeat(16)}-${flags}` });
}

describe('trace sampling', () => {
  let app: Application | undefined;
  let collector: TelemetryCollector | undefined;
  let sampler: TraceSampler | undefined;

  beforeEach(() => {
    delete process.env.CONFIG_DATA;
  });

  afterEach(async () => {
    await app?.stop();
    app = undefined;
    if (collector) clearCollector(collector);
    collector = undefined;
    if (sampler) clearTraceSampler(sampler);
    sampler = undefined;
    delete process.env.CONFIG_DATA;
  });

  function install(settings: TraceSampling): TraceSampler {
    collector = new TelemetryCollector();
    setCollector(collector);
    sampler = new TraceSampler(settings);
    setTraceSampler(sampler);
    return sampler;
  }

  async function startPlugin(plugin: TelemetryPlugin): Promise<TraceSampler> {
    app = application().use(plugin);
    await app.start();
    const current = plugin.sampler;
    if (!current) throw new Error('the plugin installed no sampler');
    return current;
  }

  describe('the shared rule', () => {
    specTest(
      'decides every fixture ratio case and scenario',
      {
        feature: 'telemetry/otlp-push-contract',
        requirement: 'trace-sampling-is-shared',
        check: 'the-typescript-sampler-decides-the-fixture-scenarios',
      },
      () => {
        expect(fixture.configPath).toBe(TRACE_SAMPLING_CONFIG_PATH);
        expect(fixture.keys).toEqual({ budget: 'budget', ratio: 'ratio', floorPerMinute: 'floorPerMinute' });
        expect(fixture.defaults).toEqual({ ...DEFAULT_TRACE_SAMPLING });
        expect(fixture.counter).toEqual({
          name: TRACE_DECISIONS,
          attribute: ATTR_TRACE_DECISION_OUTCOME,
          kept: 'kept',
          dropped: 'dropped',
        });
        expect(fixture.ratioCases.length).toBeGreaterThan(0);
        for (const c of fixture.ratioCases) {
          expect({ ...c, picked: traceRatioPicks(c.traceId, c.ratio) }).toEqual(c);
        }
        expect(fixture.scenarios.length).toBeGreaterThan(0);
        for (const scenario of fixture.scenarios) {
          const budget = new TraceSampler(scenario.settings);
          const got = scenario.roots.map((root) => ({
            ...root,
            kept: budget.decide(root.traceId, root.atSecond * 1000),
          }));
          expect({ name: scenario.name, roots: got }).toEqual({ name: scenario.name, roots: scenario.roots });
        }
      },
    );

    it('refuses settings out of range', () => {
      for (const settings of [
        { ...DEFAULT_TRACE_SAMPLING, budget: -1 },
        { ...DEFAULT_TRACE_SAMPLING, budget: 1.5 },
        { ...DEFAULT_TRACE_SAMPLING, floorPerMinute: -1 },
        { ...DEFAULT_TRACE_SAMPLING, ratio: -0.1 },
        { ...DEFAULT_TRACE_SAMPLING, ratio: 1.5 },
        { ...DEFAULT_TRACE_SAMPLING, ratio: Number.NaN },
      ]) {
        expect(traceSamplingError(settings)).toBeDefined();
        expect(() => new TraceSampler(settings)).toThrow(TRACE_SAMPLING_CONFIG_PATH);
      }
      expect(traceSamplingError(DROP_ALL)).toBeUndefined();
      expect([traceRatioPicks('', 1), traceRatioPicks('not-a-trace-id', 0.5)]).toEqual([false, false]);

      const budget = new TraceSampler({ ...DROP_ALL, budget: 1 });
      expect(() => budget.update({ ...DROP_ALL, ratio: 2 })).toThrow('ratio');
      expect(budget.settings).toEqual({ ...DROP_ALL, budget: 1 });
    });

    it('keeps the kept count across an update and floors the minute before the epoch', () => {
      const budget = new TraceSampler({ ...DROP_ALL, budget: 1 });
      expect([budget.decide(NONE_PICKED, 0), budget.decide(NONE_PICKED, 0)]).toEqual([true, false]);
      budget.update({ ...DROP_ALL, budget: 2 });
      expect([budget.decide(NONE_PICKED, 0), budget.decide(NONE_PICKED, 0)]).toEqual([true, false]);

      const floor = new TraceSampler({ ...DROP_ALL, floorPerMinute: 1 });
      expect([
        floor.decide(NONE_PICKED, -30_000),
        floor.decide(NONE_PICKED, -1000),
        floor.decide(NONE_PICKED, 0),
      ]).toEqual([true, false, true]);
    });
  });

  describe('trace-budget', () => {
    specTest(
      'unset sampling uses the protocol defaults',
      {
        feature: 'typescript/service-telemetry',
        requirement: 'trace-budget',
        check: 'unset-sampling-uses-the-protocol-defaults',
      },
      async () => {
        const plugin = telemetry({ enabled: true, endpoint: ENDPOINT });
        const current = await startPlugin(plugin);
        expect(current.settings).toEqual(fixture.defaults);
        expect(getTraceSampler()).toBe(current);

        await app?.stop();
        app = undefined;
        expect(getTraceSampler()).toBeUndefined();
      },
    );

    specTest(
      'the config section overrides the plugin settings key by key',
      {
        feature: 'typescript/service-telemetry',
        requirement: 'trace-budget',
        check: 'the-config-section-overrides-the-plugin-settings',
      },
      async () => {
        setTraces({ budget: 3 });
        const current = await startPlugin(
          telemetry({ enabled: true, endpoint: ENDPOINT, traces: { budget: 10, ratio: 0 } }),
        );
        expect(current.settings).toEqual({ budget: 3, ratio: 0, floorPerMinute: 1 });
        // The telemetry section still reads with its traces key present.
        expect(useConfig(TelemetryConfig).enabled).toBe(false);
      },
    );

    specTest(
      'a plugin setting out of range fails the warmup',
      {
        feature: 'typescript/service-telemetry',
        requirement: 'trace-budget',
        check: 'the-config-section-overrides-the-plugin-settings',
      },
      async () => {
        app = application().use(telemetry({ enabled: true, endpoint: ENDPOINT, traces: { ratio: 2 } }));
        await expect(app.start()).rejects.toThrow('telemetry({ traces })');
      },
    );

    specTest(
      'a child follows its parent sampled flag whatever the budget',
      {
        feature: 'typescript/service-telemetry',
        requirement: 'trace-budget',
        check: 'a-child-follows-its-parents-sampled-flag',
      },
      async () => {
        install(DROP_ALL);
        const remote = startTelemetrySpan({ name: 'in', kind: 'server', remoteHeaders: remoteHeaders('01') });
        expect(remote.context.traceFlags).toBe('01');
        remote.finish();

        sampler?.update({ budget: 100, ratio: 1, floorPerMinute: 1 });
        const ambient = {
          __putnamiTrace: { traceId: '3'.repeat(32), spanId: '4'.repeat(16), traceFlags: '00' },
        } satisfies { __putnamiTrace: TelemetryTraceContext };
        await runInContext(ambient, async () => {
          const child = startTelemetrySpan({ name: 'out', kind: 'client' });
          expect(child.context.traceFlags).toBe('00');
          child.finish();
        });

        expect(collector?.drainSpans().map((span) => span.name)).toEqual(['in']);
        expect(decisions(collector?.drainAll() ?? [])).toEqual({});
      },
    );

    specTest(
      'an unsampled span is not recorded and injects its flag',
      {
        feature: 'typescript/service-telemetry',
        requirement: 'trace-budget',
        check: 'an-unsampled-span-is-not-recorded',
      },
      () => {
        install(DROP_ALL);
        const span = startTelemetrySpan({ name: 'root', kind: 'server' });
        expect(span.context.traceFlags).toBe('00');
        const headers = new Headers();
        span.inject(headers);
        expect(headers.get('traceparent')).toMatch(/^00-[0-9a-f]{32}-[0-9a-f]{16}-00$/);
        span.finish();
        expect(collector?.drainSpans()).toEqual([]);
      },
    );

    it('samples a root span when no telemetry plugin installed a sampler', () => {
      expect(getTraceSampler()).toBeUndefined();
      expect(startTelemetrySpan({ name: 'root', kind: 'server' }).context.traceFlags).toBe('01');
    });

    specTest(
      'each root decision is counted once by outcome',
      {
        feature: 'typescript/service-telemetry',
        requirement: 'trace-budget',
        check: 'each-root-decision-is-counted',
      },
      () => {
        install({ ...DROP_ALL, budget: 1 });
        const kept = startTelemetrySpan({ name: 'kept', kind: 'server' });
        const dropped = startTelemetrySpan({ name: 'dropped', kind: 'server' });
        startTelemetrySpan({ name: 'child', kind: 'server', remoteHeaders: remoteHeaders('01') });
        expect([kept.context.traceFlags, dropped.context.traceFlags]).toEqual(['01', '00']);
        expect(decisions(collector?.drainAll() ?? [])).toEqual({ kept: 1, dropped: 1 });
      },
    );
  });

  describe('live-sampling-settings', () => {
    specTest(
      'a changed key applies without a restart',
      {
        feature: 'typescript/service-telemetry',
        requirement: 'live-sampling-settings',
        check: 'a-changed-key-applies-without-a-restart',
      },
      async () => {
        setTraces({ budget: 5 });
        const current = await startPlugin(
          new TelemetryPlugin({ enabled: true, endpoint: ENDPOINT }, { samplingRefreshMs: 10 }),
        );
        expect(current.settings.budget).toBe(5);

        setTraces({ budget: 7, ratio: 0.5 });
        const deadline = Date.now() + 2000;
        while (current.settings.budget !== 7 && Date.now() < deadline) {
          await Bun.sleep(10);
        }
        expect(current.settings).toEqual({ budget: 7, ratio: 0.5, floorPerMinute: 1 });
      },
    );

    specTest(
      'an invalid section keeps the settings in force',
      {
        feature: 'typescript/service-telemetry',
        requirement: 'live-sampling-settings',
        check: 'an-invalid-section-keeps-the-settings-in-force',
      },
      async () => {
        const plugin = telemetry({ enabled: true, endpoint: ENDPOINT, traces: { budget: 4 } });
        const current = await startPlugin(plugin);

        setTraces({ ratio: 2 });
        plugin.refreshSampling();
        expect(current.settings).toEqual({ ...DEFAULT_TRACE_SAMPLING, budget: 4 });

        setTraces({ budget: 'many' });
        plugin.refreshSampling();
        plugin.refreshSampling();
        expect(current.settings).toEqual({ ...DEFAULT_TRACE_SAMPLING, budget: 4 });

        setTraces({ budget: 6 });
        plugin.refreshSampling();
        expect(current.settings).toEqual({ ...DEFAULT_TRACE_SAMPLING, budget: 6 });
      },
    );

    specTest(
      'a source that keeps its first load shows no change',
      {
        feature: 'typescript/service-telemetry',
        requirement: 'live-sampling-settings',
        check: 'a-cached-source-shows-no-change',
      },
      async () => {
        const plugin = telemetry({ enabled: true, endpoint: ENDPOINT });
        const current = await startPlugin(plugin);
        const values = [{ telemetry: { traces: { budget: 2 } } }, { telemetry: { traces: { budget: 9 } } }];
        let loads = 0;
        let first: Record<string, unknown> | undefined;
        const once: ConfigSource = {
          name: 'once',
          priority: 50,
          load: () => {
            first ??= values[loads];
            loads++;
            return first;
          },
        };
        const service = new ConfigService([once]);
        try {
          plugin.refreshSampling();
          plugin.refreshSampling();
        } finally {
          service.close();
        }
        expect(loads).toBe(2);
        expect(current.settings).toEqual({ ...DEFAULT_TRACE_SAMPLING, budget: 2 });
      },
    );

    specTest(
      'an update holds until the section changes',
      {
        feature: 'typescript/service-telemetry',
        requirement: 'live-sampling-settings',
        check: 'an-update-holds-until-the-section-changes',
      },
      async () => {
        setTraces({ budget: 5 });
        const plugin = telemetry({ enabled: true, endpoint: ENDPOINT });
        const current = await startPlugin(plugin);

        current.update(DROP_ALL);
        plugin.refreshSampling();
        expect(current.settings).toEqual(DROP_ALL);

        setTraces({ budget: 8 });
        plugin.refreshSampling();
        expect(current.settings).toEqual({ ...DEFAULT_TRACE_SAMPLING, budget: 8 });
      },
    );
  });
});
