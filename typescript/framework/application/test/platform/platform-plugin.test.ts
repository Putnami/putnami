import { afterEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import type { Application, HealthChecker, Module, Plugin, ReadinessChecker } from '../../src';
import { application, http, module, platform } from '../../src';
import {
  type Envelope,
  HTTP_STATUS_OK,
  HTTP_STATUS_UNAVAILABLE,
  STATUS_DEGRADED,
  STATUS_OK,
  STATUS_UNAVAILABLE,
  httpStatusFor,
  validateEnvelope,
} from '../../src/platform';

// --- helpers ---

class FakeHealthPlugin implements Plugin, HealthChecker {
  constructor(
    readonly name: string,
    private readonly error?: Error,
  ) {}

  async checkHealth(): Promise<void> {
    if (this.error) throw this.error;
  }
}

class FakeReadyPlugin implements Plugin, ReadinessChecker {
  constructor(
    readonly name: string,
    private readonly error?: Error,
  ) {}

  async checkReadiness(): Promise<void> {
    if (this.error) throw this.error;
  }
}

class ProbePool {
  calls = 0;
  sawSignal = false;

  ping(signal: AbortSignal): void {
    this.calls++;
    this.sawSignal = signal instanceof AbortSignal;
  }
}

class WarmupHealthContributionPlugin implements Plugin {
  async warmup(owner: Module): Promise<void> {
    owner.health.contribute('pool', [ProbePool], (resolved, signal) => resolved.ping(signal));
  }
}

// Plugin implementing both — one stub feeds both endpoints.
class BothPlugin implements Plugin, HealthChecker, ReadinessChecker {
  constructor(
    readonly name: string,
    private readonly health?: Error,
    private readonly ready?: Error,
  ) {}

  async checkHealth(): Promise<void> {
    if (this.health) throw this.health;
  }
  async checkReadiness(): Promise<void> {
    if (this.ready) throw this.ready;
  }
}

async function fetchEnvelope(baseUrl: string, path: string): Promise<{ status: number; body: Envelope }> {
  const res = await fetch(`${baseUrl}${path}`);
  const body = (await res.json()) as Envelope;
  return { status: res.status, body };
}

/** Bounds every wait on a pending start. It detects a hang; it is never a latency assertion. */
const HANG_DETECTOR_MS = 30_000;

/** Polls until `poll` returns a value, within the hang detector. */
async function eventually<T>(what: string, poll: () => Promise<T | undefined>): Promise<T> {
  const deadline = Date.now() + HANG_DETECTOR_MS;
  for (;;) {
    const value = await poll();
    if (value !== undefined) return value;
    if (Date.now() > deadline) throw new Error(`${what} did not happen`);
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
}

/**
 * Starts an application whose server mounts a platform plugin next to a plugin
 * whose `start()` stays pending until `release()`, then resolves, or rejects
 * with `error`. `track` receives the application before it starts, so the
 * caller stops it. Returns once that start is pending and the platform plugin
 * itself started: `/healthz` answers 200.
 */
async function startWithPendingPlugin(
  track: (started: Application) => void,
  error?: Error,
): Promise<{
  baseUrl: string;
  starting: Promise<void>;
  release: () => void;
}> {
  let release = (): void => {};
  const released = new Promise<void>((resolve) => {
    release = resolve;
  });
  let enter = (): void => {};
  const entered = new Promise<void>((resolve) => {
    enter = resolve;
  });
  const httpPlugin = http({ port: 0 });
  const app = application()
    .use(httpPlugin)
    .use(platform())
    .use({
      name: 'pending',
      start: async () => {
        enter();
        await released;
        if (error) throw error;
      },
    } satisfies Plugin);
  track(app);
  const starting = app.start();
  // A rejection is asserted by the caller; this keeps it from surfacing as unhandled meanwhile.
  starting.catch(() => {});
  await entered;
  const port = await eventually('the server listening', async () => httpPlugin.getServer()?.port);
  const baseUrl = `http://localhost:${port}`;
  await eventually('/healthz answering 200', async () =>
    (await fetchEnvelope(baseUrl, '/healthz')).status === HTTP_STATUS_OK ? true : undefined,
  );
  return { baseUrl, starting, release };
}

// --- tests ---

describe('PlatformPlugin', () => {
  let app: Application | undefined;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
  });

  describe('/livez', () => {
    it('returns 200 even after stop (process-alive, drain-safe)', async () => {
      // /livez must stay green during drain — a 503 here would trigger
      // a pod restart, which is the opposite of graceful shutdown.
      // Started then stopped puts the plugin in the post-stop state
      // while the http server is still listening; that's the path k8s
      // hits during a SIGTERM grace window.
      const httpPlugin = http({ port: 0 });
      const platformPlugin = platform();
      app = application().use(httpPlugin).use(platformPlugin);
      await app.start();
      await platformPlugin.stop();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/livez`);
      expect(res.status).toBe(HTTP_STATUS_OK);
      const env = (await res.json()) as Envelope;
      expect(env.status).toBe(STATUS_OK);
    });
  });

  describe('/healthz', () => {
    it('returns 503 unavailable after stop (drain window)', async () => {
      // Mirror of the /livez test: after stop, /healthz should
      // explicitly signal unavailable so load balancers drain traffic.
      const httpPlugin = http({ port: 0 });
      const platformPlugin = platform();
      app = application().use(httpPlugin).use(platformPlugin);
      await app.start();
      await platformPlugin.stop();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
      expect(status).toBe(HTTP_STATUS_UNAVAILABLE);
      expect(body.status).toBe(STATUS_UNAVAILABLE);
      expect(body.checks).toBeUndefined();
      expect(validateEnvelope(body)).toEqual([]);
    });

    it('200 ok when running with no probes', async () => {
      const httpPlugin = http({ port: 0 });
      app = application().use(httpPlugin).use(platform());
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
      expect(status).toBe(HTTP_STATUS_OK);
      expect(body.status).toBe(STATUS_OK);
      expect(validateEnvelope(body)).toEqual([]);
    });

    it('auto-discovers HealthChecker plugins across the module tree', async () => {
      const httpPlugin = http({ port: 0 });
      const apiModule = module('api').use(new FakeHealthPlugin('cache'));
      app = application().use(httpPlugin).use(new FakeHealthPlugin('db')).use(apiModule).use(platform());
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
      expect(status).toBe(HTTP_STATUS_OK);
      expect(body.status).toBe(STATUS_OK);
      expect(body.checks).toEqual({ db: 'ok', cache: 'ok' });
      expect(validateEnvelope(body)).toEqual([]);
    });

    it('reports degraded with verbatim error messages when a probe fails', async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(httpPlugin)
        .use(new FakeHealthPlugin('db', new Error('conn refused')))
        .use(new FakeHealthPlugin('cache'))
        .use(platform());
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
      expect(status).toBe(HTTP_STATUS_UNAVAILABLE);
      expect(body.status).toBe(STATUS_DEGRADED);
      expect(body.checks?.['db']).toBe('conn refused');
      expect(body.checks?.['cache']).toBe('ok');
      expect(validateEnvelope(body)).toEqual([]);
    });

    it('explicit addHealthChecker wins over auto-discovered probe of same name', async () => {
      const httpPlugin = http({ port: 0 });
      const platformPlugin = platform().addHealthChecker('db', () => {
        throw new Error('explicit wins');
      });
      app = application()
        .use(httpPlugin)
        .use(new FakeHealthPlugin('db')) // would otherwise return ok
        .use(platformPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { body } = await fetchEnvelope(baseUrl, '/healthz');
      expect(body.checks?.['db']).toBe('explicit wins');
    });

    it('discovers DI-injected health contributions', async () => {
      const pool = new ProbePool();
      const httpPlugin = http({ port: 0 });
      app = application()
        .provide(ProbePool, () => pool)
        .use(httpPlugin)
        .use(platform());
      app.health.contribute('pool', [ProbePool], (resolved, signal) => resolved.ping(signal));
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
      expect(status).toBe(HTTP_STATUS_OK);
      expect(body.checks).toEqual({ pool: 'ok' });
      expect(pool.calls).toBe(1);
      expect(pool.sawSignal).toBe(true);
    });

    it('discovers health contributions registered by later plugin warmup', async () => {
      const pool = new ProbePool();
      const httpPlugin = http({ port: 0 });
      app = application()
        .provide(ProbePool, () => pool)
        .use(httpPlugin)
        .use(platform())
        .use(new WarmupHealthContributionPlugin());
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
      expect(status).toBe(HTTP_STATUS_OK);
      expect(body.checks).toEqual({ pool: 'ok' });
      expect(pool.calls).toBe(1);
      expect(pool.sawSignal).toBe(true);
    });

    it('caps probes that ignore the abort signal (no /healthz hang)', async () => {
      // Regression: probes that never settle and ignore the controller
      // signal used to stall Promise.allSettled, so the response hung
      // past probeTimeoutMs. Each probe now races against the signal
      // directly so a non-cooperating probe still surfaces as a
      // timed-out check rather than blocking the aggregate.
      const hangPlugin: Plugin & HealthChecker = {
        name: 'hang',
        // Ignores the signal on purpose — mirrors a driver that doesn't
        // accept AbortSignal (postgres.js tagged-template queries today).
        checkHealth(): Promise<void> {
          return new Promise<void>(() => {
            /* intentionally never settles */
          });
        },
      };
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(httpPlugin)
        .use(hangPlugin)
        .use(platform({ probeTimeoutMs: 50 }));
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const startedAt = Date.now();
      const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
      const elapsed = Date.now() - startedAt;

      // Generous ceiling vs the 50ms probe timeout — any value here that
      // is dramatically less than the test runner's default timeout
      // proves the aggregate didn't wait on the never-settling probe.
      expect(elapsed).toBeLessThan(2000);
      expect(status).toBe(HTTP_STATUS_UNAVAILABLE);
      expect(body.status).toBe(STATUS_DEGRADED);
      expect(body.checks?.['hang']).toContain('timed out');
      expect(validateEnvelope(body)).toEqual([]);
    });
  });

  describe('/readyz', () => {
    it('auto-discovers ReadinessChecker plugins', async () => {
      const httpPlugin = http({ port: 0 });
      app = application().use(httpPlugin).use(new FakeReadyPlugin('leader-elect')).use(platform());
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { status, body } = await fetchEnvelope(baseUrl, '/readyz');
      expect(status).toBe(HTTP_STATUS_OK);
      expect(body.checks).toEqual({ 'leader-elect': 'ok' });
      expect(validateEnvelope(body)).toEqual([]);
    });

    it('treats health and readiness as independent registries', async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(httpPlugin)
        .use(new BothPlugin('deps', undefined, new Error('warmup')))
        .use(platform());
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const healthz = await fetchEnvelope(baseUrl, '/healthz');
      expect(healthz.body.checks).toEqual({ deps: 'ok' });

      const readyz = await fetchEnvelope(baseUrl, '/readyz');
      expect(readyz.status).toBe(HTTP_STATUS_UNAVAILABLE);
      expect(readyz.body.status).toBe(STATUS_DEGRADED);
      expect(readyz.body.checks?.['deps']).toBe('warmup');
    });

    it('discovers DI-injected readiness contributions without registering them as health', async () => {
      const pool = new ProbePool();
      const httpPlugin = http({ port: 0 });
      app = application()
        .provide(ProbePool, () => pool)
        .use(httpPlugin)
        .use(platform());
      app.health.contributeReadiness('pool', [ProbePool], (resolved, signal) => resolved.ping(signal));
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const healthz = await fetchEnvelope(baseUrl, '/healthz');
      expect(healthz.body.checks).toBeUndefined();

      const readyz = await fetchEnvelope(baseUrl, '/readyz');
      expect(readyz.status).toBe(HTTP_STATUS_OK);
      expect(readyz.body.checks).toEqual({ pool: 'ok' });
      expect(pool.calls).toBe(1);
      expect(pool.sawSignal).toBe(true);
    });
  });

  describe('/readyz until startup completed', () => {
    specTest(
      'stays unavailable while a plugin start is pending',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'completed-startup-readiness',
        check: 'readiness-answers-ready-only-after-every-plugin-start',
      },
      async () => {
        const { baseUrl, starting, release } = await startWithPendingPlugin((started) => {
          app = started;
        });
        try {
          const pending = await fetchEnvelope(baseUrl, '/readyz');
          expect(pending.status).toBe(HTTP_STATUS_UNAVAILABLE);
          expect(pending.body.status).toBe(STATUS_UNAVAILABLE);
        } finally {
          release();
        }
        await starting;

        const started = await fetchEnvelope(baseUrl, '/readyz');
        expect(started.status).toBe(HTTP_STATUS_OK);
        expect(started.body.status).toBe(STATUS_OK);
      },
    );

    specTest(
      'never answers ready when a plugin start rejects',
      {
        feature: 'typescript/application-lifecycle',
        requirement: 'completed-startup-readiness',
        check: 'a-rejected-plugin-start-never-answers-ready',
      },
      async () => {
        const { baseUrl, starting, release } = await startWithPendingPlugin((started) => {
          app = started;
        }, new Error('boom'));
        try {
          const pending = await fetchEnvelope(baseUrl, '/readyz');
          expect(pending.status).toBe(HTTP_STATUS_UNAVAILABLE);
          expect(pending.body.status).toBe(STATUS_UNAVAILABLE);
        } finally {
          release();
        }
        await expect(starting).rejects.toThrow('boom');
      },
    );
  });

  describe('required readiness', () => {
    // The synthesized missing-required message is byte-identical to the Go
    // runtime's missingRequiredProbeMessage — that literal parity is how the
    // two runtimes stay behaviorally equivalent for the missing-probe case.
    const MISSING = 'required probe not registered or discovered';

    it('degrades /readyz with a synthesized failing check for a missing required probe', async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(httpPlugin)
        .use(platform({ required: ['leader-elect'] }));
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { status, body } = await fetchEnvelope(baseUrl, '/readyz');
      expect(status).toBe(HTTP_STATUS_UNAVAILABLE);
      expect(body.status).toBe(STATUS_DEGRADED);
      expect(body.checks?.['leader-elect']).toBe(MISSING);
      // Expressed through the existing envelope — still a valid degraded shape.
      expect(validateEnvelope(body)).toEqual([]);
    });

    it('a required probe that is registered runs normally (no synthesized entry)', async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(httpPlugin)
        .use(new FakeReadyPlugin('leader-elect'))
        .use(platform({ required: ['leader-elect'] }));
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { status, body } = await fetchEnvelope(baseUrl, '/readyz');
      expect(status).toBe(HTTP_STATUS_OK);
      expect(body.status).toBe(STATUS_OK);
      expect(body.checks).toEqual({ 'leader-elect': 'ok' });
      expect(validateEnvelope(body)).toEqual([]);
    });

    it('a missing required probe coexists with passing registered probes and forces degraded', async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(httpPlugin)
        .use(new FakeReadyPlugin('warm'))
        .use(platform({ required: ['leader-elect'] }));
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { status, body } = await fetchEnvelope(baseUrl, '/readyz');
      expect(status).toBe(HTTP_STATUS_UNAVAILABLE);
      expect(body.status).toBe(STATUS_DEGRADED);
      expect(body.checks?.['warm']).toBe('ok');
      expect(body.checks?.['leader-elect']).toBe(MISSING);
      expect(validateEnvelope(body)).toEqual([]);
    });

    it('the required list is readiness-only — /healthz ignores it', async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(httpPlugin)
        .use(platform({ required: ['leader-elect'] }));
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const { status, body } = await fetchEnvelope(baseUrl, '/healthz');
      expect(status).toBe(HTTP_STATUS_OK);
      expect(body.status).toBe(STATUS_OK);
      expect(validateEnvelope(body)).toEqual([]);
    });

    it('rejects start() for a non-conforming required probe name', async () => {
      // A required name with a space/uppercase would be synthesized verbatim into
      // a degraded /readyz checks key, making the envelope fail the protocol's own
      // validator. start() must fail fast — symmetric with go/framework/platform.
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(httpPlugin)
        .use(platform({ required: ['Leader Elect'] }));
      await expect(app.start()).rejects.toThrow(/probe name/);
    });
  });

  describe('/version', () => {
    it('returns configured VersionInfo and omits empty fields', async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(httpPlugin)
        .use(
          platform({
            version: {
              name: 'my-service',
              version: '1.0.0',
              sha: 'abc123',
            },
          }),
        );
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const res = await fetch(`${baseUrl}/version`);
      expect(res.status).toBe(200);
      const body = (await res.json()) as Record<string, unknown>;
      expect(body).toEqual({ name: 'my-service', version: '1.0.0', sha: 'abc123' });
    });
  });

  describe('prefix', () => {
    it('mounts under a configured prefix and does not expose root paths', async () => {
      const httpPlugin = http({ port: 0 });
      app = application()
        .use(httpPlugin)
        .use(platform({ prefix: '/_' }));
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;
      const prefixed = await fetch(`${baseUrl}/_/livez`);
      expect(prefixed.status).toBe(200);

      const root = await fetch(`${baseUrl}/livez`);
      // Under /_, the bare /livez is not mounted — the HTTP layer
      // returns 404 for unmatched routes. Asserting the precise status
      // (vs `not.toBe(200)`) catches accidental 500s from middleware
      // changes.
      expect(root.status).toBe(404);
    });
  });

  describe('protocol conformance', () => {
    it('every response shape passes protocol.validateEnvelope', async () => {
      const httpPlugin = http({ port: 0 });
      const platformPlugin = platform();
      app = application()
        .use(httpPlugin)
        .use(new FakeHealthPlugin('db', new Error('conn refused')))
        .use(new FakeReadyPlugin('warm'))
        .use(platformPlugin);
      await app.start();

      const baseUrl = `http://localhost:${httpPlugin.getServer()?.port}`;

      // /healthz — degraded
      const healthz = await fetchEnvelope(baseUrl, '/healthz');
      expect(validateEnvelope(healthz.body)).toEqual([]);
      expect(httpStatusFor(healthz.body.status)).toBe(healthz.status);

      // /readyz — ok
      const readyz = await fetchEnvelope(baseUrl, '/readyz');
      expect(validateEnvelope(readyz.body)).toEqual([]);
      expect(httpStatusFor(readyz.body.status)).toBe(readyz.status);

      // /livez — ok (always)
      const livez = await fetchEnvelope(baseUrl, '/livez');
      expect(validateEnvelope(livez.body)).toEqual([]);
    });
  });
});
