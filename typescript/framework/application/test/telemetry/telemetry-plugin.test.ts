import { afterEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { http } from '../../src/http/http.plugin';
import { TelemetryPlugin, telemetry } from '../../src/telemetry/telemetry.plugin';
import { getCollector, incCounter } from '../../src/telemetry/telemetry.utils';

describe('TelemetryPlugin', () => {
  let app: Application;

  afterEach(async () => {
    await app?.stop();
  });

  const ENDPOINT = 'http://localhost:0/telemetry';

  specTest(
    'should register a collector on warmup',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'opt-in-with-explicit-endpoint',
      check: 'an-enabled-plugin-collects-http-metrics',
    },
    async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(telemetry({ enabled: true, endpoint: ENDPOINT }))
        .use(httpPlugin);
      await app.start();

      expect(getCollector()).toBeDefined();
    },
  );

  specTest(
    'should not register a collector when disabled',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'opt-in-with-explicit-endpoint',
      check: 'telemetry-is-off-by-default',
    },
    async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(telemetry({ enabled: false, endpoint: ENDPOINT }))
        .use(httpPlugin);
      await app.start();

      expect(getCollector()).toBeUndefined();
    },
  );

  specTest(
    'should be disabled by default (opt-in)',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'opt-in-with-explicit-endpoint',
      check: 'telemetry-is-off-by-default',
    },
    async () => {
      const httpPlugin = http({ port: 0 });
      app = application().use(telemetry()).use(httpPlugin);
      await app.start();

      expect(getCollector()).toBeUndefined();
    },
  );

  specTest(
    'should not start when enabled but no endpoint is configured',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'opt-in-with-explicit-endpoint',
      check: 'enabling-without-an-endpoint-stays-inert',
    },
    async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(telemetry({ enabled: true }))
        .use(httpPlugin);
      await app.start();

      // No public default endpoint: enabling without an endpoint stays inert.
      expect(getCollector()).toBeUndefined();
    },
  );

  specTest(
    'should collect HTTP metrics automatically',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'opt-in-with-explicit-endpoint',
      check: 'an-enabled-plugin-collects-http-metrics',
    },
    async () => {
      const httpPlugin = http({ port: 0 });
      httpPlugin.get('/api/test', () => ({ ok: true }));

      app = application()
        .use(telemetry({ enabled: true, endpoint: ENDPOINT }))
        .use(httpPlugin);
      await app.start();

      const port = httpPlugin.getServer()?.port;
      await fetch(`http://localhost:${port}/api/test`);

      const collector = getCollector();
      expect(collector).toBeDefined();

      const buckets = collector?.drainAll();
      expect(buckets.length).toBeGreaterThanOrEqual(1);

      const allCounters = Object.assign({}, ...buckets.map((b) => b.counters));
      expect(allCounters['http.GET./api/test.200']).toBe(1);
    },
  );

  specTest(
    'should support custom metrics via utils',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'collector-ownership',
      check: 'custom-metrics-record-into-the-active-collector',
    },
    async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(telemetry({ enabled: true, endpoint: ENDPOINT }))
        .use(httpPlugin);
      await app.start();

      incCounter('custom.metric', 3);

      const collector = getCollector();
      const buckets = collector?.drainAll();
      const allCounters = Object.assign({}, ...buckets.map((b) => b.counters));
      expect(allCounters['custom.metric']).toBe(3);
    },
  );

  specTest(
    'should clear collector on stop',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'collector-ownership',
      check: 'stop-clears-only-its-own-collector',
    },
    async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(telemetry({ enabled: true, endpoint: ENDPOINT }))
        .use(httpPlugin);
      await app.start();

      expect(getCollector()).toBeDefined();

      await app.stop();

      expect(getCollector()).toBeUndefined();
    },
  );

  describe('telemetry() factory', () => {
    it('should create TelemetryPlugin instance', () => {
      const plugin = telemetry();
      expect(plugin).toBeInstanceOf(TelemetryPlugin);
    });

    it('should accept options', () => {
      const plugin = telemetry({ enabled: false, flushIntervalS: 60 });
      expect(plugin).toBeInstanceOf(TelemetryPlugin);
    });
  });
});
