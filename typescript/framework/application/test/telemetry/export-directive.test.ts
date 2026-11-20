import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  type LogEntry,
  Logger,
  type LogLevel,
  type LogRecordOutcome,
  observeLogRecords,
  resetDefaultLogger,
  resetLoggerConfig,
  setLogLevel,
  setLogLevelLimit,
  setRootLogger,
} from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryLogger, MemorySink } from '@putnami/runtime/testing';
import {
  DirectiveReceiver,
  EXPORT_DIRECTIVE_HEADER,
  type ExportDirective,
  applyTraceSampling,
  limitLogLevel,
  MAX_DIRECTIVE_TTL_SECONDS,
  MAX_RETRY_AFTER_SECONDS,
  parseExportDirective,
  retryAfterDelayMs,
  SignalDelays,
  setDirectiveReceiver,
  setSignalDelays,
} from '../../src/telemetry/export-directive';
import { startTelemetrySpan } from '../../src/telemetry/client-telemetry';
import { PATH_LOGS, PATH_METRICS, PATH_TRACES } from '../../src/telemetry/otlp';
import { OtlpLogSink } from '../../src/telemetry/otlp-log.sink';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { postOtlp } from '../../src/telemetry/telemetry.sender';
import { clearCollector, setCollector } from '../../src/telemetry/telemetry.utils';
import {
  ATTR_TRACE_DECISION_OUTCOME,
  clearTraceSampler,
  setTraceSampler,
  TRACE_DECISIONS,
  TraceSampler,
  type TraceSampling,
} from '../../src/telemetry/trace-sampling';

interface FixtureDirective {
  traceBudget?: number;
  traceRatio?: number;
  traceFloor?: number;
  traceParent?: 'follow' | 'decide';
  logLevel?: LogLevel;
  ttlSeconds: number;
}

interface FixtureLimits {
  traces: TraceSampling;
  logLevel: LogLevel;
}

interface DirectiveEvent {
  atSecond: number;
  directive?: string;
  traceId?: string;
  kept?: boolean;
  record?: LogLevel;
  emitted?: boolean;
}

interface DirectivesFixture {
  header: string;
  keys: {
    traceBudget: string;
    traceRatio: string;
    traceFloor: string;
    traceParent: string;
    logLevel: string;
    ttl: string;
  };
  maxTtlSeconds: number;
  parseCases: Array<{ value: string; directive: FixtureDirective | null }>;
  applyCases: Array<{ name: string; own: FixtureLimits; directive: string; effective: FixtureLimits }>;
  traceScenarios: Array<{ name: string; settings: TraceSampling; events: DirectiveEvent[] }>;
  logScenarios: Array<{ name: string; level: LogLevel; events: DirectiveEvent[] }>;
  retryAfter: {
    status: number;
    maxSeconds: number;
    cases: Array<{ status: number; value: string; nowSecond: number; delaySeconds: number }>;
  };
}

const fixture = JSON.parse(
  readFileSync(
    join(import.meta.dir, '../../../../../protocols/telemetry/fixtures/conventions/export-directives.json'),
    'utf8',
  ),
) as DirectivesFixture;

const ENDPOINT = 'http://collector.test';
const NONE_PICKED = 'ffffffffffffffffffffffffffffffff';
const feature = 'typescript/service-telemetry';
const requirement = 'export-directives';

/** A clock a test moves by hand. */
class TestClock {
  constructor(public ms = 0) {}
  readonly now = (): number => this.ms;
}

function mustParse(value: string): ExportDirective {
  const directive = parseExportDirective(value);
  if (!directive) throw new Error(`the directive ${value} is void`);
  return directive;
}

/** A fetch double whose answer depends on the path; it counts the requests per path. */
function collectorStub(answer: (path: string) => ResponseInit) {
  const requests = new Map<string, number>();
  globalThis.fetch = mock((url: string) => {
    const path = new URL(url).pathname;
    requests.set(path, (requests.get(path) ?? 0) + 1);
    return Promise.resolve(new Response(null, answer(path)));
  }) as unknown as typeof fetch;
  return (path: string) => requests.get(path) ?? 0;
}

describe('export directives', () => {
  const originalFetch = globalThis.fetch;
  let clock: TestClock;
  let previousReceiver: DirectiveReceiver;
  let previousDelays: SignalDelays;
  let receiver: DirectiveReceiver;
  let sampler: TraceSampler | undefined;
  let collector: TelemetryCollector | undefined;

  beforeEach(() => {
    clock = new TestClock(1_000_000);
    receiver = new DirectiveReceiver(clock.now);
    previousReceiver = setDirectiveReceiver(receiver);
    previousDelays = setSignalDelays(new SignalDelays(clock.now));
    resetLoggerConfig();
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    if (sampler) clearTraceSampler(sampler);
    sampler = undefined;
    if (collector) clearCollector(collector);
    collector = undefined;
    setDirectiveReceiver(previousReceiver);
    setSignalDelays(previousDelays);
    resetLoggerConfig();
  });

  describe('the shared rule', () => {
    specTest(
      'parses, limits and applies every fixture case and scenario',
      {
        feature: 'telemetry/otlp-push-contract',
        requirement: 'export-directives-are-shared',
        check: 'the-typescript-runtime-applies-the-fixture-directives',
      },
      () => {
        expect(fixture.header).toBe(EXPORT_DIRECTIVE_HEADER);
        expect(fixture.keys).toEqual({
          traceBudget: 'trace-budget',
          traceRatio: 'trace-ratio',
          traceFloor: 'trace-floor',
          traceParent: 'trace-parent',
          logLevel: 'log-level',
          ttl: 'ttl',
        });
        expect(fixture.maxTtlSeconds).toBe(MAX_DIRECTIVE_TTL_SECONDS);
        for (const c of fixture.parseCases) {
          expect({ value: c.value, directive: parseExportDirective(c.value) ?? null }).toEqual(c);
        }
        expect(fixture.applyCases.length).toBeGreaterThan(0);
        for (const c of fixture.applyCases) {
          const directive = mustParse(c.directive);
          expect({
            name: c.name,
            traces: applyTraceSampling(directive, c.own.traces),
            logLevel: limitLogLevel(directive, c.own.logLevel),
          }).toEqual({ name: c.name, traces: c.effective.traces, logLevel: c.effective.logLevel });
        }

        expect(fixture.traceScenarios.length).toBeGreaterThan(0);
        for (const scenario of fixture.traceScenarios) {
          const scenarioClock = new TestClock();
          const scenarioReceiver = new DirectiveReceiver(scenarioClock.now);
          const budget = new TraceSampler(scenario.settings);
          scenarioReceiver.follow(budget);
          const got = scenario.events.map((event) => {
            scenarioClock.ms = event.atSecond * 1000;
            if (event.directive !== undefined) {
              scenarioReceiver.receive(event.directive);
              return event;
            }
            return { ...event, kept: budget.decide(event.traceId ?? '', scenarioClock.ms) };
          });
          expect({ name: scenario.name, events: got }).toEqual({ name: scenario.name, events: scenario.events });
          expect(budget.settings).toEqual(scenario.settings);
        }

        expect(fixture.logScenarios.length).toBeGreaterThan(0);
        for (const scenario of fixture.logScenarios) {
          resetLoggerConfig();
          setLogLevel(scenario.level);
          const scenarioClock = new TestClock();
          const scenarioReceiver = new DirectiveReceiver(scenarioClock.now);
          const sink = new MemorySink();
          const logger = new Logger([sink], 'directive');
          const got = scenario.events.map((event) => {
            scenarioClock.ms = event.atSecond * 1000;
            if (event.directive !== undefined) {
              scenarioReceiver.receive(event.directive);
              return event;
            }
            const before = sink.entries.length;
            logger[event.record ?? 'info']('record');
            return { ...event, emitted: sink.entries.length > before };
          });
          expect({ name: scenario.name, events: got }).toEqual({ name: scenario.name, events: scenario.events });
        }
      },
    );

    specTest(
      'a 429 delays its signal by the fixture Retry-After',
      {
        feature: 'telemetry/otlp-push-contract',
        requirement: 'too-many-requests-delays-a-signal',
        check: 'the-typescript-exporters-wait-out-a-retry-after',
      },
      async () => {
        expect(fixture.retryAfter.status).toBe(429);
        expect(fixture.retryAfter.maxSeconds).toBe(MAX_RETRY_AFTER_SECONDS);
        for (const c of fixture.retryAfter.cases) {
          expect({ ...c, delaySeconds: retryAfterDelayMs(c.status, c.value, c.nowSecond * 1000) / 1000 }).toEqual(c);
        }

        const count = collectorStub(() => ({ status: 429, headers: { 'Retry-After': '120' } }));
        await postOtlp(ENDPOINT, PATH_TRACES, '{}');
        expect(await postOtlp(ENDPOINT, PATH_TRACES, '{}')).toEqual({ ok: false, delayed: true });
        expect(count(PATH_TRACES)).toBe(1);
        clock.ms += 120_000;
        await postOtlp(ENDPOINT, PATH_TRACES, '{}');
        expect(count(PATH_TRACES)).toBe(2);
      },
    );
  });

  specTest(
    'a directive sets the sampler until its ttl ends',
    { feature, requirement, check: 'a-directive-sets-the-sampler-until-its-ttl' },
    async () => {
      const own: TraceSampling = { budget: 0, ratio: 0.5, floorPerMinute: 3 };
      sampler = new TraceSampler(own);
      setTraceSampler(sampler);
      collectorStub((path) =>
        path === PATH_METRICS
          ? {
              status: 200,
              headers: { [EXPORT_DIRECTIVE_HEADER]: 'trace-budget=7, trace-ratio=0.9, trace-floor=1, ttl=60' },
            }
          : { status: 200 },
      );

      // A metrics answer carries the directive; it applies to the traces,
      // higher or lower.
      await postOtlp(ENDPOINT, PATH_METRICS, '{}');
      expect(sampler.inForce(clock.ms)).toEqual({ budget: 7, ratio: 0.9, floorPerMinute: 1 });
      expect(sampler.settings).toEqual(own);
      expect(sampler.inForce(clock.ms + 60_000)).toEqual(own);

      // A sampler installed while a directive holds follows it at once.
      const late = new TraceSampler(own);
      setTraceSampler(late);
      sampler = late;
      expect(late.inForce(clock.ms).ratio).toBe(0.9);
    },
  );

  specTest(
    'a directive sets the parent policy until its ttl ends',
    { feature, requirement: 'parent-policy', check: 'a-directive-sets-the-parent-policy' },
    () => {
      collector = new TelemetryCollector();
      setCollector(collector);
      sampler = new TraceSampler({ budget: 0, ratio: 1, floorPerMinute: 0 });
      setTraceSampler(sampler);
      const outside = new Headers({ traceparent: `00-${'1'.repeat(32)}-${'2'.repeat(16)}-00` });
      const flags = () => startTelemetrySpan({ name: 'in', kind: 'server', remoteHeaders: outside }).context.traceFlags;

      // A span starts on the wall clock, so the directive arrives on it too.
      clock.ms = Date.now();
      expect(flags()).toBe('00');
      receiver.receive('trace-parent=decide, ttl=60');
      expect(sampler.inForce(clock.ms).parent).toBe('decide');
      expect(flags()).toBe('01');
      expect(sampler.decidesOutsideParents(clock.ms + 60_000)).toBe(false);
    },
  );

  specTest(
    'a directive raises the log level until its ttl ends',
    { feature, requirement, check: 'a-directive-raises-the-log-level-until-its-ttl' },
    async () => {
      setLogLevel('debug');
      // Even a failed push carries a directive, the log sink's included.
      collectorStub(() => ({ status: 503, headers: { [EXPORT_DIRECTIVE_HEADER]: 'log-level=warn, ttl=30' } }));
      const otlp = new OtlpLogSink({ endpoint: ENDPOINT, flushIntervalMs: 3_600_000 });
      otlp.write({ level: 'info', message: 'queued', timestamp: new Date().toISOString() } as LogEntry);
      await otlp.flush();
      await otlp.close();

      const sink = new MemorySink();
      const logger = new Logger([sink], 'app');
      logger.info('hidden');
      logger.warn('shown');
      logger.error('always');
      clock.ms += 30_000;
      logger.debug('back');
      expect(sink.entries.map((e) => e.message)).toEqual(['shown', 'always', 'back']);
    },
  );

  specTest(
    'trace.decisions and log.records count what a directive drops',
    { feature, requirement, check: 'a-directive-drop-is-counted' },
    () => {
      collector = new TelemetryCollector();
      setCollector(collector);
      sampler = new TraceSampler({ budget: 0, ratio: 1, floorPerMinute: 0 });
      setTraceSampler(sampler);
      receiver.receive('trace-ratio=0, log-level=error, ttl=60');

      const kept = [0, 1, 2].map(() => sampler?.decide(NONE_PICKED, clock.ms));
      expect(kept).toEqual([false, false, false]);
      let dropped = 0;
      for (const bucket of collector.drainAll()) {
        for (const series of bucket.counterSeries ?? []) {
          if (series.name === TRACE_DECISIONS && series.attributes[ATTR_TRACE_DECISION_OUTCOME] === 'dropped') {
            dropped += series.value;
          }
        }
      }
      expect(dropped).toBe(3);

      setLogLevel('debug');
      const outcomes: string[] = [];
      const remove = observeLogRecords((level: LogLevel, outcome: LogRecordOutcome) =>
        outcomes.push(`${level}/${outcome}`),
      );
      new Logger([new MemorySink()], 'app').warn('dropped by the directive');
      remove();
      expect(outcomes).toEqual(['warn/dropped']);
    },
  );

  specTest(
    'a 429 Retry-After delays only its own signal',
    { feature, requirement, check: 'a-retry-after-delays-only-its-signal' },
    async () => {
      let throttle = true;
      const count = collectorStub((path) =>
        throttle && (path === PATH_TRACES || path === PATH_LOGS)
          ? { status: 429, headers: { 'Retry-After': '120' } }
          : { status: 200 },
      );
      await postOtlp(ENDPOINT, PATH_TRACES, '{}');
      expect((await postOtlp(ENDPOINT, PATH_TRACES, '{}')).delayed).toBe(true);
      expect((await postOtlp(ENDPOINT, PATH_METRICS, '{}')).ok).toBe(true);
      expect(count(PATH_TRACES)).toBe(1);

      // The log sink keeps its records through the delay and sends them after.
      const otlp = new OtlpLogSink({ endpoint: ENDPOINT, flushIntervalMs: 3_600_000 });
      otlp.write({ level: 'info', message: 'first', timestamp: new Date().toISOString() } as LogEntry);
      await otlp.flush();
      otlp.write({ level: 'info', message: 'second', timestamp: new Date().toISOString() } as LogEntry);
      await otlp.flush();
      expect(count(PATH_LOGS)).toBe(1);
      throttle = false;
      clock.ms += 120_000;
      await otlp.close();
      expect(count(PATH_LOGS)).toBe(2);
    },
  );

  it('drops the queued records and warns when it closes during the delay', async () => {
    const count = collectorStub(() => ({ status: 429, headers: { 'Retry-After': '120' } }));
    const warnings = new MemoryLogger();
    setRootLogger(warnings);
    try {
      const otlp = new OtlpLogSink({ endpoint: ENDPOINT, flushIntervalMs: 3_600_000 });
      otlp.write({ level: 'info', message: 'first', timestamp: new Date().toISOString() } as LogEntry);
      await otlp.flush();
      otlp.write({ level: 'info', message: 'left', timestamp: new Date().toISOString() } as LogEntry);
      await otlp.close();
      expect(count(PATH_LOGS)).toBe(1);
      expect(warnings.entries.some((e) => e.level === 'warn' && e.message.includes('asked to wait'))).toBe(true);
    } finally {
      resetDefaultLogger();
    }
  });

  it('a directive installs the level limit again after a caller removed it', () => {
    setLogLevel('debug');
    receiver.receive('log-level=warn, ttl=60');
    setLogLevelLimit(undefined);
    receiver.receive('log-level=warn, ttl=60');
    const sink = new MemorySink();
    new Logger([sink], 'app').info('hidden');
    expect(sink.entries).toEqual([]);
  });
});
