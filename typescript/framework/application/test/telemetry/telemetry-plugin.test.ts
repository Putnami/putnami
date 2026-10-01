import { afterEach, describe, expect, it } from 'bun:test';
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

  it('should register a collector on warmup', async () => {
    const httpPlugin = http({ port: 0 });
    app = application()
      .use(telemetry({ enabled: true, endpoint: ENDPOINT }))
      .use(httpPlugin);
    await app.start();

    expect(getCollector()).toBeDefined();
  });

  it('should not register a collector when disabled', async () => {
    const httpPlugin = http({ port: 0 });
    app = application()
      .use(telemetry({ enabled: false, endpoint: ENDPOINT }))
      .use(httpPlugin);
    await app.start();

    expect(getCollector()).toBeUndefined();
  });

  it('should be disabled by default (opt-in)', async () => {
    const httpPlugin = http({ port: 0 });
    app = application().use(telemetry()).use(httpPlugin);
    await app.start();

    expect(getCollector()).toBeUndefined();
  });

  it('should not start when enabled but no endpoint is configured', async () => {
    const httpPlugin = http({ port: 0 });
    app = application()
      .use(telemetry({ enabled: true }))
      .use(httpPlugin);
    await app.start();

    // No public default endpoint: enabling without an endpoint stays inert.
    expect(getCollector()).toBeUndefined();
  });

  it('should collect HTTP metrics automatically', async () => {
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
  });

  it('should support custom metrics via utils', async () => {
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
  });

  it('should clear collector on stop', async () => {
    const httpPlugin = http({ port: 0 });
    app = application()
      .use(telemetry({ enabled: true, endpoint: ENDPOINT }))
      .use(httpPlugin);
    await app.start();

    expect(getCollector()).toBeDefined();

    await app.stop();

    expect(getCollector()).toBeUndefined();
  });

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
