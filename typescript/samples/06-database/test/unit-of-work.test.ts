import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { closeAllDatabases, database, Outcome, Repository, sql, UnitOfWork } from '@putnami/database';
import { runInContext } from '@putnami/runtime';
import { DeviceCodeService } from '../src/services/device-code-service';
import { KeyRotationService } from '../src/services/key-rotation-service';
import { KeyBindings } from '../src/tables/key-bindings';
import { SigningKeys } from '../src/tables/signing-keys';
import '../src/tables/device-codes';

// Resolve the `default` datasource through the framework — never a hardcoded
// connection. `database('default')` prefers a provisioned test binding (injected
// by `putnami test` from this project's declared infra/requirements.json) and
// falls back to conf/.env.test.yaml, so the proof runs against whatever the
// declared-requirements provisioner (or local conf) supplies, with no
// project-specific probing.
const dbAvailable = await (async () => {
  try {
    const probe = await database('default');
    await probe`SELECT 1`;
    return true;
  } catch {
    // Not reachable (no provisioned binding and no local server) — the unit gate
    // stays green by skipping; a provisioned run exercises it for real.
    return false;
  }
})();

describe.skipIf(!dbAvailable)('unit-of-work + consume-once proof', () => {
  let testApp: TestApp;
  let client: Awaited<ReturnType<typeof database>>;

  // Run every scenario inside a request-like async context so `runInTransaction`
  // / `UnitOfWork` can flag it transactional.
  const inCtx = <T>(fn: () => Promise<T>): Promise<T> => runInContext({}, fn) as Promise<T>;

  const count = async (table: string, where: string, params: unknown[]): Promise<number> => {
    const rows = await client.unsafe(`SELECT COUNT(*)::int AS count FROM ${table} WHERE ${where}`, params as never[]);
    return Number((rows[0] as { count: number }).count);
  };

  beforeAll(async () => {
    // Starting sql() publishes the primary datasource + connection factory the
    // repositories resolve against; the proof's own DDL/assert client resolves
    // the SAME `default` datasource, so they can never target different servers.
    testApp = await createTestApp({ plugins: [sql()] });
    client = await database('default');
    // Own the proof schema explicitly so the suite is self-contained.
    for (const stmt of [
      'DROP TABLE IF EXISTS device_codes',
      'DROP TABLE IF EXISTS key_bindings',
      'DROP TABLE IF EXISTS signing_keys',
      'CREATE TABLE device_codes (code TEXT PRIMARY KEY, consumed BOOLEAN NOT NULL DEFAULT false, consumed_by TEXT)',
      'CREATE TABLE signing_keys (id TEXT PRIMARY KEY, tenant TEXT NOT NULL, state TEXT NOT NULL)',
      'CREATE TABLE key_bindings (key_id TEXT PRIMARY KEY, tenant TEXT NOT NULL UNIQUE)',
    ]) {
      await client.unsafe(stmt);
    }
  });

  afterAll(async () => {
    // The client is the framework's pooled `default` connection — close every
    // pool through the factory rather than ending it directly.
    await closeAllDatabases().catch(() => {});
    await testApp?.stop();
  });

  describe('consume-once (device codes)', () => {
    it('redeems a code exactly once, reporting the typed outcome', async () => {
      const svc = new DeviceCodeService();
      await client`INSERT INTO device_codes (code) VALUES (${'dev-1'})`;

      expect(await inCtx(() => svc.redeem('dev-1', 'alice'))).toBe(Outcome.Applied);
      expect(await inCtx(() => svc.redeem('dev-1', 'bob'))).toBe(Outcome.AlreadyConsumedConflict);
      expect(await inCtx(() => svc.redeem('never-issued', 'bob'))).toBe(Outcome.NotFound);

      // The losing redemption must not have clobbered the winner's claim.
      expect(
        await count('device_codes', 'code = $1 AND consumed = true AND consumed_by = $2', ['dev-1', 'alice']),
      ).toBe(1);
    });

    it('lets exactly one of many racing redemptions win', async () => {
      const svc = new DeviceCodeService();
      await client`INSERT INTO device_codes (code) VALUES (${'dev-race'})`;

      const outcomes = await Promise.all(Array.from({ length: 16 }, () => inCtx(() => svc.redeem('dev-race', 'user'))));
      const applied = outcomes.filter((o) => o === Outcome.Applied).length;
      const conflict = outcomes.filter((o) => o === Outcome.AlreadyConsumedConflict).length;
      expect(applied).toBe(1);
      expect(conflict).toBe(15);
    });
  });

  describe('atomic rotation (runInTransaction)', () => {
    it('commits the revoke, the successor, and the binding together', async () => {
      const svc = new KeyRotationService();
      await client`INSERT INTO signing_keys (id, tenant, state) VALUES (${'k1'}, ${'acme'}, 'active')`;

      expect(await inCtx(() => svc.rotate('k1', 'k2', 'acme'))).toBe(Outcome.Applied);

      expect(await count('signing_keys', "id = $1 AND state = 'revoked'", ['k1'])).toBe(1);
      expect(await count('signing_keys', "id = $1 AND state = 'active'", ['k2'])).toBe(1);
      expect(await count('key_bindings', 'key_id = $1 AND tenant = $2', ['k2', 'acme'])).toBe(1);
    });

    it('rolls the whole unit back when the second write fails', async () => {
      const svc = new KeyRotationService();
      await client`INSERT INTO signing_keys (id, tenant, state) VALUES (${'k3'}, ${'globex'}, 'active')`;
      // Pre-bind the tenant, so the rotation's binding insert violates UNIQUE(tenant).
      await client`INSERT INTO key_bindings (key_id, tenant) VALUES (${'preexisting'}, ${'globex'})`;

      await expect(inCtx(() => svc.rotate('k3', 'k4', 'globex'))).rejects.toThrow();

      // Both writes rolled back: predecessor still active, successor absent.
      expect(await count('signing_keys', "id = $1 AND state = 'active'", ['k3'])).toBe(1);
      expect(await count('signing_keys', 'id = $1', ['k4'])).toBe(0);
      // The pre-existing binding is untouched; no binding was added for the successor.
      expect(await count('key_bindings', 'tenant = $1', ['globex'])).toBe(1);
      expect(await count('key_bindings', 'key_id = $1', ['k4'])).toBe(0);
    });

    it('installs nothing and reports the outcome when the predecessor cannot be revoked', async () => {
      const svc = new KeyRotationService();

      expect(await inCtx(() => svc.rotate('missing', 's1', 't1'))).toBe(Outcome.NotFound);

      await client`INSERT INTO signing_keys (id, tenant, state) VALUES (${'k5'}, ${'t2'}, 'revoked')`;
      expect(await inCtx(() => svc.rotate('k5', 's2', 't2'))).toBe(Outcome.AlreadyConsumedConflict);

      expect(await count('signing_keys', 'id IN ($1, $2)', ['s1', 's2'])).toBe(0);
      expect(await count('key_bindings', 'key_id IN ($1, $2)', ['s1', 's2'])).toBe(0);
    });
  });

  describe('atomic rotation (request-scoped UnitOfWork class)', () => {
    it('spans two repositories inside one UnitOfWork boundary', async () => {
      await client`INSERT INTO signing_keys (id, tenant, state) VALUES (${'u1'}, ${'tenantU'}, 'active')`;

      const outcome = await inCtx(async () => {
        const uow = new UnitOfWork();
        uow.begin();
        try {
          const keys = new Repository(SigningKeys);
          const bindings = new Repository(KeyBindings);
          const o = await keys.rotateRow({
            keyColumn: 'id',
            predecessorKey: 'u1',
            stateColumn: 'state',
            expected: 'active',
            revoked: 'revoked',
            successorColumns: ['id', 'tenant', 'state'],
            successorValues: ['u2', 'tenantU', 'active'],
          });
          if (o !== Outcome.Applied) {
            uow.setRollbackOnly();
            await uow.commit();
            return o;
          }
          await bindings.save({ keyId: 'u2', tenant: 'tenantU' });
          await uow.commit();
          return o;
        } catch (error) {
          await uow.rollback(error);
          throw error;
        }
      });

      expect(outcome).toBe(Outcome.Applied);
      expect(await count('signing_keys', "id = $1 AND state = 'revoked'", ['u1'])).toBe(1);
      expect(await count('signing_keys', "id = $1 AND state = 'active'", ['u2'])).toBe(1);
      expect(await count('key_bindings', 'key_id = $1 AND tenant = $2', ['u2', 'tenantU'])).toBe(1);
    });
  });
});
