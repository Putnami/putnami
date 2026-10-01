import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { api, platform } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { closeAllDatabases, database, sql } from '@putnami/database';
import '../src/tables/users';

// Skip tests when the `default` datasource is not reachable. Resolve it through
// the framework — a provisioned test binding (injected by `putnami test` from the
// declared infra/requirements.json) wins, conf/.env.test.yaml is the fallback —
// so the suite has no hardcoded connection of its own.
const dbAvailable = await (async () => {
  try {
    const probe = await database('default');
    await probe`SELECT 1`;
    return true;
  } catch {
    return false;
  }
})();

describe.skipIf(!dbAvailable)('database sample', () => {
  let testApp: TestApp;

  beforeAll(async () => {
    testApp = await createTestApp({
      plugins: [sql(), platform(), api({ scanPath: 'src/api' })],
    });
  });

  afterAll(async () => {
    await testApp.stop();
    await closeAllDatabases().catch(() => {});
  });

  describe('GET /users', () => {
    it('should return a list of users', async () => {
      const res = await testApp.fetch('/users');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.users).toBeArray();
      expect(data.total).toBeNumber();
    });
  });

  describe('POST /users', () => {
    it('should create a new user', async () => {
      const res = await testApp.fetch('/users', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          email: `test-${Date.now()}@example.com`,
          name: 'Test User',
          age: 25,
        }),
      });

      expect(res.status).toBe(201);
      const data = await res.json();
      expect(data.user.name).toBe('Test User');
      expect(data.user.email).toContain('@example.com');
    });
  });

  describe('GET /healthz', () => {
    it('should return healthy status', async () => {
      const res = await testApp.fetch('/healthz');
      expect(res.status).toBe(200);
    });
  });
});
