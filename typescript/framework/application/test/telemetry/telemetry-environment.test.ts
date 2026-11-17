import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { registerTelemetryTokenSource, resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryLogger, resetTelemetryTokenSourceForTest } from '@putnami/runtime/testing';
import type { Application } from '../../src/application';
import { application, module } from '../../src/application';
import { HttpPlugin, http } from '../../src/http/http.plugin';
import { platform } from '../../src/platform/platform.plugin';
import { parseResourceAttributes } from '../../src/telemetry/telemetry.environment';
import { TelemetryPlugin, telemetry, telemetryFromEnvironment } from '../../src/telemetry/telemetry.plugin';
import { getCollector } from '../../src/telemetry/telemetry.utils';

const FEATURE = 'typescript/service-telemetry';
/** The fixture a process-instance-identity test starts as a child process. */
const PUSH_ONCE = join(import.meta.dir, 'fixtures', 'push-once.ts');
const OTEL_KEYS = [
  'OTEL_EXPORTER_OTLP_ENDPOINT',
  'OTEL_SDK_DISABLED',
  'OTEL_SERVICE_NAME',
  'OTEL_RESOURCE_ATTRIBUTES',
] as const;

interface ReceivedPush {
  path: string;
  authorization: string | null;
  body: Record<string, unknown>;
}

/** A collector on a local port that records every push and answers `status`. */
function fakeCollector(status = 200) {
  const pushes: ReceivedPush[] = [];
  const server = Bun.serve({
    port: 0,
    async fetch(req) {
      pushes.push({
        path: new URL(req.url).pathname,
        authorization: req.headers.get('authorization'),
        body: (await req.json()) as Record<string, unknown>,
      });
      return new Response(null, { status });
    },
  });
  return { url: `http://localhost:${server.port}`, pushes, stop: () => server.stop(true) };
}

type Collector = ReturnType<typeof fakeCollector>;

/** The string attributes of the first resource of an OTLP request body. */
function resourceAttributes(body: Record<string, unknown>): Record<string, string> {
  const list = (body['resourceMetrics'] ?? body['resourceSpans']) as Array<{
    resource: { attributes: Array<{ key: string; value: { stringValue: string } }> };
  }>;
  return Object.fromEntries(list[0].resource.attributes.map((a) => [a.key, a.value.stringValue]));
}

/** Names of the metrics a metrics request body carries. */
function metricNames(body: Record<string, unknown>): string[] {
  const list = body['resourceMetrics'] as Array<{ scopeMetrics: Array<{ metrics: Array<{ name: string }> }> }>;
  return list.flatMap((r) => r.scopeMetrics.flatMap((s) => s.metrics.map((m) => m.name)));
}

describe('telemetry from the environment', () => {
  let app: Application | undefined;
  let collector: Collector | undefined;
  let saved: Record<string, string | undefined>;
  let logger: MemoryLogger;

  beforeEach(() => {
    saved = Object.fromEntries(OTEL_KEYS.map((key) => [key, process.env[key]]));
    for (const key of OTEL_KEYS) delete process.env[key];
    logger = new MemoryLogger();
    setRootLogger(logger);
  });

  afterEach(async () => {
    await app?.stop();
    app = undefined;
    collector?.stop();
    collector = undefined;
    for (const key of OTEL_KEYS) {
      if (saved[key] === undefined) delete process.env[key];
      else process.env[key] = saved[key];
    }
    resetTelemetryTokenSourceForTest();
    resetDefaultLogger();
  });

  /** Start an app with one GET /ping route, call it once, stop the app. */
  async function serveOneRequest(compose: (app: Application) => Application = (a) => a): Promise<void> {
    const server = http({ port: 0 });
    server.get('/ping', () => ({ ok: true }));
    app = compose(application()).use(server);
    await app.start();
    await fetch(`http://localhost:${server.getServer()?.port}/ping`);
    await app.stop();
    app = undefined;
  }

  const warnings = () => logger.entries.filter((entry) => entry.level === 'warn').map((entry) => entry.message);

  specTest(
    'an application pushes to the collector OTEL_EXPORTER_OTLP_ENDPOINT names, with no telemetry() plugin',
    { feature: FEATURE, requirement: 'default-from-environment', check: 'the-environment-endpoint-turns-telemetry-on' },
    async () => {
      collector = fakeCollector();
      process.env['OTEL_EXPORTER_OTLP_ENDPOINT'] = `${collector.url}/`;

      await serveOneRequest();

      const metrics = collector.pushes.filter((push) => push.path === '/v1/metrics');
      expect(metrics).toHaveLength(1);
      expect(metricNames(metrics[0].body)).toContain('http.GET./ping.200');
      expect(collector.pushes.some((push) => push.path === '/v1/traces')).toBe(true);
    },
  );

  specTest(
    'no endpoint installs no collector',
    { feature: FEATURE, requirement: 'default-from-environment', check: 'no-endpoint-or-a-disabled-sdk-sends-nothing' },
    async () => {
      app = application().use(http({ port: 0 }));
      await app.start();
      expect(getCollector()).toBeUndefined();
      expect(telemetryFromEnvironment({})).toBeUndefined();
      expect(telemetryFromEnvironment({ OTEL_EXPORTER_OTLP_ENDPOINT: '  ' })).toBeUndefined();
    },
  );

  specTest(
    'OTEL_SDK_DISABLED=true sends nothing even with an endpoint',
    { feature: FEATURE, requirement: 'default-from-environment', check: 'no-endpoint-or-a-disabled-sdk-sends-nothing' },
    async () => {
      collector = fakeCollector();
      process.env['OTEL_EXPORTER_OTLP_ENDPOINT'] = collector.url;
      process.env['OTEL_SDK_DISABLED'] = 'TRUE';

      await serveOneRequest();

      expect(collector.pushes).toHaveLength(0);
      expect(
        telemetryFromEnvironment({ OTEL_EXPORTER_OTLP_ENDPOINT: 'http://c', OTEL_SDK_DISABLED: 'false' }),
      ).toBeDefined();
    },
  );

  specTest(
    'a disabled explicit telemetry() wins over the environment',
    { feature: FEATURE, requirement: 'default-from-environment', check: 'an-explicit-plugin-owns-telemetry' },
    async () => {
      collector = fakeCollector();
      process.env['OTEL_EXPORTER_OTLP_ENDPOINT'] = collector.url;

      await serveOneRequest((a) => a.use(telemetry({ enabled: false })));

      expect(collector.pushes).toHaveLength(0);
    },
  );

  specTest(
    'an enabled explicit telemetry() counts each request once and pushes to its own endpoint',
    { feature: FEATURE, requirement: 'default-from-environment', check: 'an-explicit-plugin-owns-telemetry' },
    async () => {
      collector = fakeCollector();
      const environmentCollector = fakeCollector();
      process.env['OTEL_EXPORTER_OTLP_ENDPOINT'] = environmentCollector.url;
      try {
        let plugins = 0;
        await serveOneRequest((a) => {
          const composed = a.use(telemetry({ enabled: true, endpoint: collector?.url }));
          const originalPrepare = composed.prepare.bind(composed);
          composed.prepare = async () => {
            await originalPrepare();
            plugins = composed.collectPlugins().filter(({ plugin }) => plugin instanceof TelemetryPlugin).length;
          };
          return composed;
        });

        expect(plugins).toBe(1);
        expect(environmentCollector.pushes).toHaveLength(0);
        const metrics = collector.pushes.filter((push) => push.path === '/v1/metrics');
        const points = (
          metrics[0].body['resourceMetrics'] as Array<{
            scopeMetrics: Array<{ metrics: Array<{ name: string; sum?: { dataPoints: Array<{ asInt: string }> } }> }>;
          }>
        )[0].scopeMetrics[0].metrics.find((m) => m.name === 'http.GET./ping.200');
        expect(points?.sum?.dataPoints.map((p) => p.asInt)).toEqual(['1']);
      } finally {
        environmentCollector.stop();
      }
    },
  );

  specTest(
    'OTEL_SERVICE_NAME names the service, then the service.name attribute, then the app option, then unknown',
    {
      feature: FEATURE,
      requirement: 'service-identity-from-environment',
      check: 'the-service-name-comes-from-the-environment',
    },
    async () => {
      collector = fakeCollector();
      process.env['OTEL_EXPORTER_OTLP_ENDPOINT'] = collector.url;

      process.env['OTEL_SERVICE_NAME'] = 'from-env';
      await serveOneRequest((a) => a.use(telemetry({ enabled: true, endpoint: collector?.url, app: 'from-option' })));
      delete process.env['OTEL_SERVICE_NAME'];
      process.env['OTEL_RESOURCE_ATTRIBUTES'] = 'service.name=from-attributes';
      await serveOneRequest((a) => a.use(telemetry({ enabled: true, endpoint: collector?.url, app: 'from-option' })));
      delete process.env['OTEL_RESOURCE_ATTRIBUTES'];
      await serveOneRequest((a) => a.use(telemetry({ enabled: true, endpoint: collector?.url, app: 'from-option' })));
      await serveOneRequest();

      const names = collector.pushes
        .filter((push) => push.path === '/v1/metrics')
        .map((push) => resourceAttributes(push.body)['service.name']);
      expect(names).toEqual(['from-env', 'from-attributes', 'from-option', 'unknown']);
    },
  );

  /**
   * Start the push-once fixture twice from one environment, extended with
   * extra, and return the service.instance.id of every request they pushed.
   */
  async function instanceIdsPushedByTwoProcesses(extra: Record<string, string>): Promise<string[]> {
    collector = fakeCollector();
    const env = { ...process.env, OTEL_EXPORTER_OTLP_ENDPOINT: collector.url, OTEL_SDK_DISABLED: '', ...extra };
    for (let i = 0; i < 2; i++) {
      const child = Bun.spawn([process.execPath, PUSH_ONCE], { env, stdout: 'ignore', stderr: 'pipe' });
      const code = await child.exited;
      if (code !== 0) throw new Error(`push-once exited ${code}: ${await new Response(child.stderr).text()}`);
    }
    return collector.pushes.map((push) => resourceAttributes(push.body)['service.instance.id']);
  }

  specTest(
    'two processes started from one environment push distinct service.instance.id values',
    {
      feature: FEATURE,
      requirement: 'process-instance-identity',
      check: 'two-processes-from-one-environment-push-distinct-instance-ids',
    },
    async () => {
      const ids = await instanceIdsPushedByTwoProcesses({
        OTEL_RESOURCE_ATTRIBUTES: 'service=team/workloads/site,revision=site-00001',
      });

      // Each process pushes its metrics and its traces once, on stop.
      expect(collector?.pushes.map((push) => push.path).sort()).toEqual([
        '/v1/metrics',
        '/v1/metrics',
        '/v1/traces',
        '/v1/traces',
      ]);
      for (const id of ids) expect(id).toBeTruthy();
      expect(new Set(ids).size).toBe(2);
    },
    { timeout: 30_000 },
  );

  specTest(
    'a service.instance.id that OTEL_RESOURCE_ATTRIBUTES sets is kept',
    {
      feature: FEATURE,
      requirement: 'process-instance-identity',
      check: 'an-instance-id-from-the-environment-is-kept',
    },
    async () => {
      const ids = await instanceIdsPushedByTwoProcesses({
        OTEL_RESOURCE_ATTRIBUTES: 'service.name=site,service.instance.id=site-7f9c',
      });

      expect(ids).toEqual(['site-7f9c', 'site-7f9c', 'site-7f9c', 'site-7f9c']);
    },
    { timeout: 30_000 },
  );

  specTest(
    'OTEL_RESOURCE_ATTRIBUTES reaches the metrics and traces resource, framework keys win',
    {
      feature: FEATURE,
      requirement: 'service-identity-from-environment',
      check: 'resource-attributes-reach-every-signal',
    },
    async () => {
      collector = fakeCollector();
      process.env['OTEL_EXPORTER_OTLP_ENDPOINT'] = collector.url;
      process.env['OTEL_SERVICE_NAME'] = 'site';
      process.env['OTEL_RESOURCE_ATTRIBUTES'] =
        'workspace=ws-1, deployment=d%2C1,service.name=ignored,putnami.framework=go,service.version=1.2.3';

      await serveOneRequest();

      expect(collector.pushes.map((push) => push.path).sort()).toEqual(['/v1/metrics', '/v1/traces']);
      for (const push of collector.pushes) {
        expect(resourceAttributes(push.body)).toEqual({
          deployment: 'd,1',
          'putnami.framework': 'typescript',
          'service.instance.id': expect.any(String),
          'service.name': 'site',
          'service.version': '1.2.3',
          workspace: 'ws-1',
        });
      }
    },
  );

  it('parses OTEL_RESOURCE_ATTRIBUTES pairs and skips malformed ones', () => {
    expect(parseResourceAttributes(undefined)).toEqual({});
    expect(parseResourceAttributes('a=1,,=2,b,c=%E0%A4%A,d= x%20y ,a=3')).toEqual({ a: '3', d: 'x y' });
  });

  specTest(
    'a request the origin guard refuses is still counted',
    { feature: FEATURE, requirement: 'outermost-request-instrumentation', check: 'a-refused-request-is-still-counted' },
    async () => {
      const server = http({ port: 0 });
      server.post('/form', () => ({ ok: true }));
      app = application()
        .use(telemetry({ enabled: true, endpoint: 'http://localhost:0' }))
        .use(server);
      await app.start();

      const res = await fetch(`http://localhost:${server.getServer()?.port}/form`, {
        method: 'POST',
        headers: { 'Sec-Fetch-Site': 'cross-site', Origin: 'https://elsewhere.example' },
      });
      expect(res.status).toBe(403);

      const buckets = getCollector()?.drainAll() ?? [];
      const counters = Object.assign({}, ...buckets.map((bucket) => bucket.counters));
      expect(counters['http.POST./form.403']).toBe(1);
      expect(counters['http.error.4xx']).toBe(1);
    },
  );

  specTest(
    'an http() composed in a sub-module is instrumented and no second server starts',
    {
      feature: FEATURE,
      requirement: 'outermost-request-instrumentation',
      check: 'every-composed-server-is-instrumented',
    },
    async () => {
      collector = fakeCollector();
      process.env['OTEL_EXPORTER_OTLP_ENDPOINT'] = collector.url;
      // A fixed port, as Cloud Run sets PORT: a second server would fail with EADDRINUSE.
      const probe = Bun.serve({ port: 0, fetch: () => new Response() });
      const port = probe.port;
      probe.stop(true);
      const server = http({ port });
      server.get('/ping', () => ({ ok: true }));
      app = application().use(module('web').use(server));
      await app.start();

      const servers = app.collectPlugins().filter(({ plugin }) => plugin instanceof HttpPlugin);
      expect(servers).toHaveLength(1);
      await fetch(`http://localhost:${port}/ping`);
      await app.stop();
      app = undefined;

      const metrics = collector.pushes.filter((push) => push.path === '/v1/metrics');
      expect(metricNames(metrics[0].body)).toContain('http.GET./ping.200');
    },
  );

  specTest(
    'telemetry() composed before a plugin that creates the server instruments that server',
    {
      feature: FEATURE,
      requirement: 'outermost-request-instrumentation',
      check: 'a-server-another-plugin-creates-is-instrumented',
    },
    async () => {
      const savedPort = process.env['PORT'];
      process.env['PORT'] = '0';
      try {
        app = application()
          .use(telemetry({ enabled: true, endpoint: 'http://localhost:0' }))
          .use(platform());
        await app.start();

        const server = app.collectPlugins().find(({ plugin }) => plugin instanceof HttpPlugin)?.plugin as
          | HttpPlugin
          | undefined;
        const res = await fetch(`http://localhost:${server?.getServer()?.port}/livez`);
        expect(res.status).toBe(200);

        const buckets = getCollector()?.drainAll() ?? [];
        const counters = Object.assign({}, ...buckets.map((bucket) => bucket.counters));
        expect(Object.keys(counters).some((name) => name.startsWith('http.GET.') && name.endsWith('.200'))).toBe(true);
      } finally {
        if (savedPort === undefined) delete process.env['PORT'];
        else process.env['PORT'] = savedPort;
      }
    },
  );

  specTest(
    'an application without http() gets a collector and no server',
    { feature: FEATURE, requirement: 'outermost-request-instrumentation', check: 'telemetry-starts-no-server' },
    async () => {
      process.env['OTEL_EXPORTER_OTLP_ENDPOINT'] = 'http://localhost:0';
      app = application();
      await app.start();

      expect(getCollector()).toBeDefined();
      expect(app.collectPlugins().some(({ plugin }) => plugin instanceof HttpPlugin)).toBe(false);
    },
  );

  specTest(
    'each push carries the bearer the registered token source returns for the endpoint',
    {
      feature: FEATURE,
      requirement: 'per-push-credential',
      check: 'a-registered-token-source-authenticates-each-push',
    },
    async () => {
      collector = fakeCollector();
      process.env['OTEL_EXPORTER_OTLP_ENDPOINT'] = collector.url;
      const asked: string[] = [];
      registerTelemetryTokenSource(async (endpoint) => {
        asked.push(endpoint);
        return `token-${asked.length}`;
      });

      await serveOneRequest();

      expect(asked).toEqual([collector.url]);
      expect(collector.pushes.map((push) => push.authorization)).toEqual(['Bearer token-1', 'Bearer token-1']);
    },
  );

  specTest(
    'a static bearer wins over the token source, and no source sends no header',
    {
      feature: FEATURE,
      requirement: 'per-push-credential',
      check: 'a-registered-token-source-authenticates-each-push',
    },
    async () => {
      collector = fakeCollector();
      await serveOneRequest((a) => a.use(telemetry({ enabled: true, endpoint: collector?.url })));
      registerTelemetryTokenSource(async () => 'from-source');
      await serveOneRequest((a) => a.use(telemetry({ enabled: true, endpoint: collector?.url, bearer: 'static' })));

      expect(new Set(collector.pushes.slice(0, 2).map((push) => push.authorization))).toEqual(new Set([null]));
      expect(new Set(collector.pushes.slice(2).map((push) => push.authorization))).toEqual(new Set(['Bearer static']));
    },
  );

  specTest(
    'a failing token source drops the batch and warns once',
    { feature: FEATURE, requirement: 'per-push-credential', check: 'a-credential-failure-drops-the-batch' },
    async () => {
      collector = fakeCollector();
      process.env['OTEL_EXPORTER_OTLP_ENDPOINT'] = collector.url;
      registerTelemetryTokenSource(async () => {
        throw new Error('metadata server unreachable: secret-detail');
      });

      await serveOneRequest();

      expect(collector.pushes).toHaveLength(0);
      const credentialWarnings = warnings().filter((message) => message.includes('could not obtain a credential'));
      expect(credentialWarnings).toHaveLength(1);
      expect(credentialWarnings[0]).not.toContain('secret-detail');
    },
  );

  specTest(
    'a collector refusal logs one warning however many pushes it refuses',
    { feature: FEATURE, requirement: 'per-push-credential', check: 'a-refusal-warns-once' },
    async () => {
      collector = fakeCollector(401);
      process.env['OTEL_EXPORTER_OTLP_ENDPOINT'] = collector.url;

      await serveOneRequest();

      expect(collector.pushes).toHaveLength(2);
      expect(warnings().filter((message) => message.includes('refused a push (HTTP 401)'))).toHaveLength(1);
    },
  );
});
