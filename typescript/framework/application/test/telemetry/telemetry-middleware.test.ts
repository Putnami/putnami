import { afterEach, describe, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { http } from '../../src/http/http.plugin';
import { HISTOGRAM_BOUNDS, TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { TelemetryMiddleware } from '../../src/telemetry/telemetry.middleware';
import { buildMetricsBody } from '../../src/telemetry/telemetry.sender';
import { requestCount, requestCounts } from './http-server-series';

const FEATURE = 'typescript/service-telemetry';
const CONVENTIONS = join(
  import.meta.dir,
  '../../../../../protocols/telemetry/fixtures/conventions/http-server-metrics.json',
);

interface Convention {
  name: string;
  kind: 'counter' | 'histogram';
  unit?: string;
  description: string;
  attributes: string[];
  bounds?: number[];
}

interface RenderedMetric {
  name: string;
  unit?: string;
  description?: string;
  sum?: { dataPoints: Array<{ attributes: Array<{ key: string }> }> };
  histogram?: { dataPoints: Array<{ attributes: Array<{ key: string }>; explicitBounds?: number[] }> };
}

describe('TelemetryMiddleware', () => {
  let app: Application;

  afterEach(async () => {
    await app?.stop();
  });

  async function serve(register: (plugin: ReturnType<typeof http>) => void): Promise<{
    collector: TelemetryCollector;
    url: string;
  }> {
    const collector = new TelemetryCollector();
    const httpPlugin = http({ port: 0 }).use(TelemetryMiddleware(collector));
    register(httpPlugin);
    app = application().use(httpPlugin);
    await app.start();
    return { collector, url: `http://localhost:${httpPlugin.getServer()?.port}` };
  }

  specTest(
    'counts and times a request under its method, route pattern and status, with no per-route metric name',
    { feature: FEATURE, requirement: 'bounded-http-metrics', check: 'a-request-is-counted-under-its-route-pattern' },
    async () => {
      const { collector, url } = await serve((plugin) =>
        plugin.get('/users/[id]', (ctx) => ({ id: ctx.params?.['id'] })),
      );

      expect((await fetch(`${url}/users/123`)).status).toBe(200);

      const buckets = collector.drainAll();
      expect(requestCounts(buckets)).toEqual([{ method: 'GET', route: '/users/[id]', status: 200, count: 1 }]);
      const durations = buckets.flatMap((bucket) => bucket.histogramSeries ?? []);
      expect(durations.map((series) => [series.name, series.attributes])).toEqual([
        ['http.server.duration', { 'http.method': 'GET', 'http.route': '/users/[id]', 'http.status_code': 200 }],
      ]);
      expect(durations[0].aggregate.count).toBe(1);
      expect(durations[0].aggregate.bucketCounts).toHaveLength(HISTOGRAM_BOUNDS.length + 1);
      for (const bucket of buckets) {
        expect(Object.keys(bucket.counters)).toEqual([]);
        expect(Object.keys(bucket.histograms)).toEqual([]);
      }
    },
  );

  specTest(
    'labels a HEAD request with the GET route that answers it',
    { feature: FEATURE, requirement: 'bounded-http-metrics', check: 'a-request-is-counted-under-its-route-pattern' },
    async () => {
      const { collector, url } = await serve((plugin) => plugin.get('/items/[id]', () => ({ ok: true })));

      expect((await fetch(`${url}/items/4`, { method: 'HEAD' })).status).toBe(200);

      expect(requestCounts(collector.drainAll())).toEqual([
        { method: 'HEAD', route: '/items/[id]', status: 200, count: 1 },
      ]);
    },
  );

  specTest(
    'labels each request with its response status',
    { feature: FEATURE, requirement: 'bounded-http-metrics', check: 'the-status-code-labels-each-request' },
    async () => {
      const { collector, url } = await serve((plugin) => {
        plugin.get('/protected', (ctx) => {
          ctx.throw(403, 'Forbidden');
        });
        plugin.get('/broken', () => {
          throw new Error('boom');
        });
        plugin.get('/ok', () => 'ok');
      });

      await fetch(`${url}/protected`);
      await fetch(`${url}/broken`);
      await fetch(`${url}/ok`);

      const buckets = collector.drainAll();
      expect(requestCount(buckets, { method: 'GET', route: '/protected', status: 403 })).toBe(1);
      expect(requestCount(buckets, { method: 'GET', route: '/broken', status: 500 })).toBe(1);
      expect(requestCount(buckets, { method: 'GET', route: '/ok', status: 200 })).toBe(1);
    },
  );

  specTest(
    'counts 1,000 distinct unmatched paths and nonstandard methods in one series per method and status',
    { feature: FEATURE, requirement: 'bounded-http-metrics', check: 'an-unmatched-request-never-names-a-series' },
    async () => {
      const { collector, url } = await serve((plugin) => plugin.get('/exists', () => 'ok'));

      for (let i = 0; i < 1000; i += 50) {
        await Promise.all(
          Array.from({ length: 50 }, (_, j) => fetch(`${url}/_react/islands/chunk-${i + j}.js`).then((r) => r.text())),
        );
      }
      await fetch(`${url}/anything`, { method: 'PURGE' }).then((r) => r.text());

      const series = requestCounts(collector.drainAll());
      expect(series.filter((s) => s.method === 'GET')).toEqual([
        { method: 'GET', route: '__unmatched__', status: 404, count: 1000 },
      ]);
      expect(series.filter((s) => s.method !== 'GET').map((s) => [s.method, s.route])).toEqual([
        ['_OTHER', '__unmatched__'],
      ]);
    },
  );

  specTest(
    'records and renders the HTTP server metrics the protocol commits for every runtime',
    {
      feature: 'telemetry/otlp-push-contract',
      requirement: 'http-server-metrics-are-shared',
      check: 'the-typescript-middleware-records-the-conventions',
    },
    async () => {
      const conventions = (JSON.parse(readFileSync(CONVENTIONS, 'utf8')) as { metrics: Convention[] }).metrics;
      const { collector, url } = await serve((plugin) => plugin.get('/widgets/[id]', () => 'ok'));

      await fetch(`${url}/widgets/7`);

      const body = JSON.parse(
        buildMetricsBody(collector.drainAll(), { app: 'svc', descriptions: collector.metricDescriptions() }),
      ) as { resourceMetrics: Array<{ scopeMetrics: Array<{ metrics: RenderedMetric[] }> }> };
      const rendered = body.resourceMetrics[0].scopeMetrics[0].metrics;
      expect(rendered.map((metric) => metric.name)).toEqual(conventions.map((convention) => convention.name));

      for (const convention of conventions) {
        const metric = rendered.find((m) => m.name === convention.name);
        expect(metric?.unit).toBe(convention.unit);
        expect(metric?.description).toBe(convention.description);
        const point = convention.kind === 'histogram' ? metric?.histogram?.dataPoints[0] : metric?.sum?.dataPoints[0];
        expect(point?.attributes.map((attribute) => attribute.key)).toEqual(convention.attributes);
        if (convention.kind === 'histogram') {
          expect(metric?.histogram?.dataPoints[0].explicitBounds).toEqual(convention.bounds);
        }
      }
      expect([...HISTOGRAM_BOUNDS]).toEqual(conventions.find((c) => c.kind === 'histogram')?.bounds ?? []);
    },
  );
});
