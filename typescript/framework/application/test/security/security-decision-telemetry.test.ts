import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { type LogEntry, resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { MemoryLogger } from '@putnami/runtime/testing';
import type { HttpRequestContext } from '../../src/http/http-context.type';
import type { HttpResponse } from '../../src/http/http-response';
import { SecurityMiddleware } from '../../src/security/security.middleware';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { clearCollector, setCollector } from '../../src/telemetry/telemetry.utils';

// ---------------------------------------------------------------------------
// Harness: capture the security log entries and the in-process metric counters
// the middleware emits, so we can assert on the decision telemetry and prove the
// redaction contract holds for both the client body and the logs.
// ---------------------------------------------------------------------------

let logger: MemoryLogger;
let collector: TelemetryCollector;

beforeEach(() => {
  logger = new MemoryLogger();
  setRootLogger(logger);
  collector = new TelemetryCollector();
  setCollector(collector);
});

afterEach(() => {
  clearCollector(collector);
  resetDefaultLogger();
});

function createMockContext(user?: Record<string, unknown>): HttpRequestContext {
  return {
    user,
    req: new Request('http://localhost/orders'),
    url: 'http://localhost/orders',
    method: 'POST',
    headers: new Headers(),
    queryParams: () => ({}),
    body: async () => undefined,
    secured: () => false,
    host: () => 'localhost',
    domain: () => 'localhost',
    path: () => '/orders',
    query: () => '',
    throw: (status: number, message?: string) => {
      throw new Error(message ?? `${status}`);
    },
  } as HttpRequestContext;
}

// biome-ignore lint/suspicious/noExplicitAny: test helper
async function run(middleware: any, ctx: HttpRequestContext): Promise<HttpResponse | undefined> {
  return (await middleware(ctx, async () => undefined)) as HttpResponse | undefined;
}

/** Merge every drained bucket's counters into one flat map. */
function counters(): Record<string, number> {
  const merged: Record<string, number> = {};
  for (const bucket of collector.drainAll()) {
    for (const [key, value] of Object.entries(bucket.counters)) {
      merged[key] = (merged[key] ?? 0) + value;
    }
  }
  return merged;
}

function securityEntries(): LogEntry[] {
  return logger.entries.filter((e) => e.logger === 'security');
}

describe('security decision telemetry', () => {
  it('counts and logs a grant at debug keyed by the allow label', async () => {
    const mw = SecurityMiddleware({ roles: ['admin'] });
    const ctx = createMockContext({ sub: 'user-1', roles: ['admin'] });

    await run(mw, ctx);

    expect(counters()['security.auth_decisions.allow']).toBe(1);
    const entries = securityEntries();
    expect(entries).toHaveLength(1);
    expect(entries[0].level).toBe('debug');
    expect(entries[0].message).toBe('authorization granted');
    expect(entries[0].data?.[0]).toMatchObject({ decision: 'allow', status: 200, subject: 'user-1' });
  });

  it('counts and logs a denial at warn keyed by the decision label plus the failing dimension', async () => {
    const mw = SecurityMiddleware({ scopes: ['admin:write'] });
    const ctx = createMockContext({ sub: 'user-1', scope: 'read' });

    const res = await run(mw, ctx);

    expect(res?.status).toBe(403);
    // Counter carries the coarse decision AND the failing dimension (the reason).
    expect(counters()['security.auth_decisions.deny_scope.missing_required_scope']).toBe(1);

    const entries = securityEntries();
    expect(entries).toHaveLength(1);
    expect(entries[0].level).toBe('warn');
    expect(entries[0].message).toBe('authorization denied');
    expect(entries[0].data?.[0]).toMatchObject({
      decision: 'deny_scope',
      status: 403,
      reason: 'missing required scope',
    });
  });

  it('records an unauthenticated denial as 401 with the deny_unauthenticated label', async () => {
    const mw = SecurityMiddleware({ roles: ['admin'] });
    const ctx = createMockContext(undefined);

    const res = await run(mw, ctx);

    expect(res?.status).toBe(401);
    expect(counters()['security.auth_decisions.deny_unauthenticated.no_identity']).toBe(1);
    const entries = securityEntries();
    expect(entries[0].data?.[0]).toMatchObject({ decision: 'deny_unauthenticated', status: 401, subject: 'anonymous' });
  });

  // -------------------------------------------------------------------------
  // Redaction contract (ported from observe.go): never log the token, and never
  // leak the missing scope/role detail to the CLIENT.
  // -------------------------------------------------------------------------

  describe('redaction contract', () => {
    it('does not leak the missing-scope detail to the client body', async () => {
      const mw = SecurityMiddleware({ scopes: ['secret:scope'] });
      const ctx = createMockContext({ sub: 'user-1', scope: 'read' });

      const res = await run(mw, ctx);
      const rawBody = (res?.getBodyInit() as string | undefined) ?? '{}';
      const body = JSON.parse(rawBody);

      // Client sees only a bare 403 — no scope name, no role, no claim detail.
      expect(res?.status).toBe(403);
      expect(body).toEqual({ error: 'Forbidden' });
      expect(rawBody).not.toContain('secret:scope');
    });

    it('does not leak the missing-role detail to the client body', async () => {
      const mw = SecurityMiddleware({ roles: ['secret-role'] });
      const ctx = createMockContext({ sub: 'user-1', roles: ['viewer'] });

      const res = await run(mw, ctx);
      const rawBody = (res?.getBodyInit() as string | undefined) ?? '{}';
      const body = JSON.parse(rawBody);

      expect(res?.status).toBe(403);
      expect(body).toEqual({ error: 'Forbidden' });
      expect(rawBody).not.toContain('secret-role');
    });

    it('never writes the bearer token to the logs on a failed verification', async () => {
      const secretToken = 'tok_live_SUPER_SECRET_credential_value';
      const mw = SecurityMiddleware({ verify: async () => undefined });
      const ctx = createMockContext(undefined);
      ctx.req = new Request('http://localhost/orders', {
        headers: { Authorization: `Bearer ${secretToken}` },
      });

      const res = await run(mw, ctx);
      const recorded = counters();

      expect(res?.status).toBe(401);
      // The token must appear in neither the logs nor any recorded metric key.
      expect(JSON.stringify(logger.entries)).not.toContain(secretToken);
      expect(Object.keys(recorded).join('\n')).not.toContain(secretToken);
      // The failing dimension is still recorded server-side.
      expect(recorded['security.auth_decisions.deny_unauthenticated.token_verification_failed']).toBe(1);
    });
  });
});
