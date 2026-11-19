import { afterEach, beforeEach, describe, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  currentLogLevel,
  type Logger,
  resetConfigLoader,
  resetLoggerConfig,
  setLogLevel,
  useLogger,
} from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { type Application, application } from '../../src/application';
import { countLogRecords, LOG_RECORDS_METRIC } from '../../src/telemetry/log-records';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { buildMetricsBody } from '../../src/telemetry/telemetry.sender';

const CONVENTION = join(import.meta.dir, '../../../../../protocols/telemetry/fixtures/conventions/log-records.json');

interface LogRecordsConvention {
  metrics: Array<{ name: string; kind: 'counter'; description: string; attributes: string[] }>;
  levels: string[];
  outcomes: string[];
}

interface RenderedSum {
  name: string;
  description?: string;
  sum?: {
    isMonotonic?: boolean;
    dataPoints: Array<{ attributes: Array<{ key: string; value: { stringValue?: string } }>; asInt?: string | number }>;
  };
}

const ORIGINAL_CONFIG_DATA = process.env['CONFIG_DATA'];
const ORIGINAL_LOG_LEVEL = process.env['LOG_LEVEL'];

function restoreEnv(): void {
  if (ORIGINAL_CONFIG_DATA === undefined) delete process.env['CONFIG_DATA'];
  else process.env['CONFIG_DATA'] = ORIGINAL_CONFIG_DATA;
  if (ORIGINAL_LOG_LEVEL === undefined) delete process.env['LOG_LEVEL'];
  else process.env['LOG_LEVEL'] = ORIGINAL_LOG_LEVEL;
  resetConfigLoader();
  resetLoggerConfig();
}

describe('log records', () => {
  let app: Application | undefined;

  beforeEach(() => {
    delete process.env['LOG_LEVEL'];
    resetLoggerConfig();
  });

  afterEach(async () => {
    await app?.stop();
    app = undefined;
    restoreEnv();
  });

  specTest(
    'the TypeScript logger records log.records like the shared convention',
    {
      feature: 'telemetry/otlp-push-contract',
      requirement: 'log-records-are-shared',
      check: 'the-typescript-logger-records-the-log-records-convention',
    },
    () => {
      const convention = JSON.parse(readFileSync(CONVENTION, 'utf8')) as LogRecordsConvention;
      const [want] = convention.metrics;
      expect(LOG_RECORDS_METRIC).toBe(want.name);

      const collector = new TelemetryCollector();
      const stop = countLogRecords(collector);
      setLogLevel('info');
      const logger: Logger = useLogger('log-records-test');
      logger.debug('dropped');
      logger.error('emitted');
      stop();
      logger.error('not counted');

      const body = JSON.parse(
        buildMetricsBody(collector.drainAll(), { app: 'svc', descriptions: collector.metricDescriptions() }),
      ) as { resourceMetrics: Array<{ scopeMetrics: Array<{ metrics: RenderedSum[] }> }> };
      const metric = body.resourceMetrics[0].scopeMetrics[0].metrics.find((m) => m.name === want.name);
      expect(metric?.description).toBe(want.description);
      expect(metric?.sum?.isMonotonic).toBe(true);

      const series = new Map<string, number>();
      for (const point of metric?.sum?.dataPoints ?? []) {
        expect(point.attributes.map((attribute) => attribute.key).sort()).toEqual(want.attributes);
        const label = (key: string) => point.attributes.find((a) => a.key === key)?.value.stringValue ?? '';
        expect(convention.levels).toContain(label('level'));
        expect(convention.outcomes).toContain(label('outcome'));
        series.set(`${label('level')}/${label('outcome')}`, Number(point.asInt));
      }
      expect(Object.fromEntries(series)).toEqual({ 'debug/dropped': 1, 'error/emitted': 1 });
    },
  );

  specTest(
    'a started application applies a changed logger.level until it stops',
    { feature: 'typescript/structured-logging', requirement: 'live-level', check: 'the-application-watches-the-level' },
    async () => {
      process.env['CONFIG_DATA'] = 'logger:\n  level: warn\n  refreshSeconds: 0.02\n';
      resetConfigLoader();
      resetLoggerConfig();

      app = application();
      await app.start();
      expect(currentLogLevel()).toBe('warn');

      process.env['CONFIG_DATA'] = 'logger:\n  level: debug\n  refreshSeconds: 0.02\n';
      const deadline = Date.now() + 5000;
      while (currentLogLevel() !== 'debug') {
        if (Date.now() > deadline) throw new Error(`level = ${currentLogLevel()}, want debug`);
        await Bun.sleep(10);
      }

      await app.stop();
      app = undefined;
      process.env['CONFIG_DATA'] = 'logger:\n  level: error\n  refreshSeconds: 0.02\n';
      await Bun.sleep(100);
      expect(currentLogLevel()).toBe('debug');
    },
  );
});
