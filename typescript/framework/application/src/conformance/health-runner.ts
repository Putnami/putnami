import { afterEach, describe, expect, it } from 'bun:test';
import { type Application, type Plugin, application } from '../application';
import { http } from '../http';
import {
  type Envelope,
  type HealthChecker,
  type ReadinessChecker,
  HTTP_STATUS_OK,
  HTTP_STATUS_UNAVAILABLE,
  STATUS_DEGRADED,
  STATUS_OK,
  STATUS_UNAVAILABLE,
  httpStatusFor,
  platform,
  validateEnvelope,
} from '../platform';

// The synthesized missing-required message is byte-identical to the Go runtime's
// missingRequiredProbeMessage (go/framework/platform) — that literal parity is
// how the two runtimes stay behaviorally equivalent for the missing-probe
// case.
const MISSING_REQUIRED = 'required probe not registered or discovered';

// The no-op `warmup` hook is here only so the class satisfies the (all-optional)
// Plugin interface under strict src type-checking — the probe is discovered by the
// HealthChecker/ReadinessChecker interface, not by any lifecycle hook.
class FakeHealthPlugin implements Plugin, HealthChecker {
  constructor(
    readonly name: string,
    private readonly error?: Error,
  ) {}
  async warmup(): Promise<void> {}
  async checkHealth(): Promise<void> {
    if (this.error) throw this.error;
  }
}

class FakeReadyPlugin implements Plugin, ReadinessChecker {
  constructor(
    readonly name: string,
    private readonly error?: Error,
  ) {}
  async warmup(): Promise<void> {}
  async checkReadiness(): Promise<void> {
    if (this.error) throw this.error;
  }
}

async function fetchEnvelope(baseUrl: string, path: string): Promise<{ status: number; body: Envelope }> {
  const res = await fetch(`${baseUrl}${path}`);
  const body = (await res.json()) as Envelope;
  return { status: res.status, body };
}

/**
 * Register the health-probe conformance pack as a `bun:test`
 * suite. A downstream project certifies the framework's liveness / readiness /
 * version health contract in its own build with a single committed line:
 *
 * ```typescript
 * import { registerHealthConformanceTests } from '@putnami/application/conformance';
 * registerHealthConformanceTests();
 * ```
 *
 * It boots a real application with `platform()` + `http()` and drives the actual
 * HTTP surface, asserting every `/livez` / `/healthz` / `/readyz` / `/version`
 * response — status code, envelope shape, `checks` map, version fields, and
 * required-readiness behavior — against the protocol's own
 * {@link validateEnvelope}. It is the TypeScript half of the cross-language pack;
 * the Go half (`go.putnami.dev/app/conformance`) certifies the probe machinery
 * every runtime's endpoints stand on. Pure pack: no external service, no skip.
 */
export function registerHealthConformanceTests(): void {
  describe('platform health conformance (@putnami/application)', () => {
    let app: Application | undefined;

    afterEach(async () => {
      await app?.stop();
      app = undefined;
    });

    async function boot(...plugins: Plugin[]): Promise<string> {
      const httpPlugin = http({ port: 0 });
      let builder = application().use(httpPlugin);
      for (const plugin of plugins) builder = builder.use(plugin);
      app = builder;
      await app.start();
      return `http://localhost:${httpPlugin.getServer()?.port}`;
    }

    describe('/livez', () => {
      it('stays 200 ok even after stop (drain-safe liveness)', async () => {
        const platformPlugin = platform();
        const baseUrl = await boot(platformPlugin);
        await platformPlugin.stop();

        const { status, body } = await fetchEnvelope(baseUrl, '/livez');
        expect(status).toBe(HTTP_STATUS_OK);
        expect(body.status).toBe(STATUS_OK);
        expect(validateEnvelope(body)).toEqual([]);
      });
    });

    describe('/healthz', () => {
      it('503 unavailable before start / after stop, with no checks', async () => {
        const platformPlugin = platform();
        const baseUrl = await boot(platformPlugin);
        await platformPlugin.stop();

        const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
        expect(status).toBe(HTTP_STATUS_UNAVAILABLE);
        expect(body.status).toBe(STATUS_UNAVAILABLE);
        expect(body.checks).toBeUndefined();
        expect(validateEnvelope(body)).toEqual([]);
      });

      it('200 ok when running with no probes', async () => {
        const baseUrl = await boot(platform());
        const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
        expect(status).toBe(HTTP_STATUS_OK);
        expect(body.status).toBe(STATUS_OK);
        expect(validateEnvelope(body)).toEqual([]);
      });

      it('auto-discovers HealthChecker plugins and reports ok', async () => {
        const baseUrl = await boot(new FakeHealthPlugin('db'), platform());
        const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
        expect(status).toBe(HTTP_STATUS_OK);
        expect(body.checks).toEqual({ db: 'ok' });
        expect(validateEnvelope(body)).toEqual([]);
      });

      it('reports 503 degraded with the verbatim probe error on failure', async () => {
        const baseUrl = await boot(new FakeHealthPlugin('db', new Error('conn refused')), platform());
        const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
        expect(status).toBe(HTTP_STATUS_UNAVAILABLE);
        expect(body.status).toBe(STATUS_DEGRADED);
        expect(body.checks?.['db']).toBe('conn refused');
        expect(validateEnvelope(body)).toEqual([]);
        expect(httpStatusFor(body.status)).toBe(status);
      });
    });

    describe('/readyz', () => {
      it('auto-discovers ReadinessChecker plugins and reports ok', async () => {
        const baseUrl = await boot(new FakeReadyPlugin('leader-elect'), platform());
        const { status, body } = await fetchEnvelope(baseUrl, '/readyz');
        expect(status).toBe(HTTP_STATUS_OK);
        expect(body.checks).toEqual({ 'leader-elect': 'ok' });
        expect(validateEnvelope(body)).toEqual([]);
      });

      it('reports 503 degraded with the verbatim probe error on failure', async () => {
        const baseUrl = await boot(new FakeReadyPlugin('warm', new Error('loading')), platform());
        const { status, body } = await fetchEnvelope(baseUrl, '/readyz');
        expect(status).toBe(HTTP_STATUS_UNAVAILABLE);
        expect(body.status).toBe(STATUS_DEGRADED);
        expect(body.checks?.['warm']).toBe('loading');
        expect(validateEnvelope(body)).toEqual([]);
      });
    });

    describe('required readiness', () => {
      it('degrades /readyz with a synthesized failing check for a missing required probe', async () => {
        const baseUrl = await boot(platform({ required: ['leader-elect'] }));
        const { status, body } = await fetchEnvelope(baseUrl, '/readyz');
        expect(status).toBe(HTTP_STATUS_UNAVAILABLE);
        expect(body.status).toBe(STATUS_DEGRADED);
        expect(body.checks?.['leader-elect']).toBe(MISSING_REQUIRED);
        // Expressed through the existing envelope — still a valid degraded shape.
        expect(validateEnvelope(body)).toEqual([]);
      });

      it('a registered required probe runs normally (no synthesized entry)', async () => {
        const baseUrl = await boot(new FakeReadyPlugin('leader-elect'), platform({ required: ['leader-elect'] }));
        const { status, body } = await fetchEnvelope(baseUrl, '/readyz');
        expect(status).toBe(HTTP_STATUS_OK);
        expect(body.status).toBe(STATUS_OK);
        expect(body.checks).toEqual({ 'leader-elect': 'ok' });
        expect(validateEnvelope(body)).toEqual([]);
      });

      it('the required list is readiness-only — /healthz ignores it', async () => {
        const baseUrl = await boot(platform({ required: ['leader-elect'] }));
        const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
        expect(status).toBe(HTTP_STATUS_OK);
        expect(body.status).toBe(STATUS_OK);
        expect(validateEnvelope(body)).toEqual([]);
      });
    });

    describe('/version', () => {
      it('returns the configured VersionInfo and omits empty fields', async () => {
        const baseUrl = await boot(platform({ version: { name: 'my-service', version: '1.0.0', sha: 'abc123' } }));
        const res = await fetch(`${baseUrl}/version`);
        expect(res.status).toBe(HTTP_STATUS_OK);
        expect(await res.json()).toEqual({ name: 'my-service', version: '1.0.0', sha: 'abc123' });
      });
    });
  });
}
