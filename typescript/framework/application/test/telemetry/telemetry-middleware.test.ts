import { afterEach, describe, expect, it } from 'bun:test';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { http } from '../../src/http/http.plugin';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { TelemetryMiddleware } from '../../src/telemetry/telemetry.middleware';

describe('TelemetryMiddleware', () => {
  let app: Application;

  afterEach(async () => {
    await app?.stop();
  });

  it('should record counter and histogram for successful requests', async () => {
    const collector = new TelemetryCollector();
    const httpPlugin = http({ port: 0 }).use(TelemetryMiddleware(collector));
    httpPlugin.get('/api/users', () => ({ users: [] }));

    app = application().use(httpPlugin);
    await app.start();

    const port = httpPlugin.getServer()?.port;
    const res = await fetch(`http://localhost:${port}/api/users`);
    expect(res.status).toBe(200);

    const buckets = collector.drainAll();
    expect(buckets.length).toBeGreaterThanOrEqual(1);

    // Find the bucket(s) with our metrics
    const allCounters = Object.assign({}, ...buckets.map((b) => b.counters));
    const allHistograms = Object.assign({}, ...buckets.map((b) => b.histograms));

    expect(allCounters['http.GET./api/users.200']).toBe(1);
    expect(allHistograms['http.GET./api/users.duration']).toBeDefined();
    expect(allHistograms['http.GET./api/users.duration'].count).toBe(1);
  });

  it('should record error counters for 4xx', async () => {
    const collector = new TelemetryCollector();
    const httpPlugin = http({ port: 0 }).use(TelemetryMiddleware(collector));
    httpPlugin.get('/protected', (ctx) => {
      ctx.throw(403, 'Forbidden');
    });

    app = application().use(httpPlugin);
    await app.start();

    const port = httpPlugin.getServer()?.port;
    await fetch(`http://localhost:${port}/protected`);

    const buckets = collector.drainAll();
    const allCounters = Object.assign({}, ...buckets.map((b) => b.counters));

    expect(allCounters['http.error.4xx']).toBe(1);
  });

  it('should use route pattern instead of actual path', async () => {
    const collector = new TelemetryCollector();
    const httpPlugin = http({ port: 0 }).use(TelemetryMiddleware(collector));
    httpPlugin.get('/users/[id]', (ctx) => ({ id: ctx.params?.id }));

    app = application().use(httpPlugin);
    await app.start();

    const port = httpPlugin.getServer()?.port;
    await fetch(`http://localhost:${port}/users/123`);

    const buckets = collector.drainAll();
    const allCounters = Object.assign({}, ...buckets.map((b) => b.counters));

    // Should use the route pattern, not /users/123
    expect(allCounters['http.GET./users/[id].200']).toBe(1);
  });

  it('should record 404 for unmatched routes', async () => {
    const collector = new TelemetryCollector();
    const httpPlugin = http({ port: 0 }).use(TelemetryMiddleware(collector));
    httpPlugin.get('/exists', () => 'ok');

    app = application().use(httpPlugin);
    await app.start();

    const port = httpPlugin.getServer()?.port;
    await fetch(`http://localhost:${port}/nope`);

    const buckets = collector.drainAll();
    const allCounters = Object.assign({}, ...buckets.map((b) => b.counters));

    // Unmatched routes must bucket under a fixed label, never the raw request
    // path, so attacker-controlled URLs cannot explode metric series cardinality.
    expect(allCounters['http.GET./__unmatched__.404']).toBe(1);
    expect(allCounters['http.GET./nope.404']).toBeUndefined();
    expect(allCounters['http.error.4xx']).toBe(1);
  });
});
