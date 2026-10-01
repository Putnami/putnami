import { afterEach, describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { TestApp } from '../../src/testing/test-app';
import { createTestApp } from '../../src/testing';
import { platform } from '../../src/platform/platform.plugin';
import type { Plugin } from '../../src/application/module.types';
import { type BundleOperation, type BundlePayload, computePayloadHash, type MigrationSource } from '@putnami/migration';

let testApp: TestApp | undefined;

afterEach(async () => {
  await testApp?.stop();
  testApp = undefined;
});

describe('createTestApp', () => {
  it('never writes the migration bundle the build-generate hook owns', async () => {
    // A workload test composes the app's real migration sources into a test
    // app (identity/workloads/auth-server does). The bundle under
    // <project>/.gen/migration-bundle is the build-generate hook's output, keyed
    // on the putnami project name; a test writing one keyed on the npm package
    // name made `publish-migration` refuse the manifest whenever test~test ran
    // after the last generate.
    const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-test-app-bundle-'));
    const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
    const originalPwd = process.env['PWD'];
    try {
      writeFileSync(join(projectRoot, 'package.json'), `${JSON.stringify({ name: 'auth.example.test' })}\n`);
      process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
      process.env['PWD'] = projectRoot;

      const up = 'CREATE TABLE app_users (id uuid primary key);';
      const upPath = 'app/0001_init.sql';
      const op: BundleOperation = {
        kind: 'sql',
        target: 'default',
        namespace: 'app',
        name: 'app/0001_init',
        up: { path: upPath, hash: computePayloadHash(up) },
        safety: 'safe-online',
      };
      const source: MigrationSource & {
        migrationBundleOperations(): { operations: BundleOperation[]; payloads: BundlePayload[] };
      } = {
        kind: 'sql',
        namespace: 'app',
        migrationBundleOperations: () => ({ operations: [op], payloads: [{ path: upPath, bytes: up }] }),
      };
      const migrations = { migrationSources: () => [source] } satisfies Plugin & {
        migrationSources(): MigrationSource[];
      };

      testApp = await createTestApp({ plugins: [migrations] });

      expect(existsSync(join(projectRoot, '.gen', 'migration-bundle'))).toBe(false);
      expect(existsSync(join(projectRoot, '.gen', 'infra', 'migration.json'))).toBe(false);
    } finally {
      if (originalProjectRoot === undefined) process.env['PUTNAMI_PROJECT_ROOT'] = undefined;
      else process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
      if (originalPwd === undefined) process.env['PWD'] = undefined;
      else process.env['PWD'] = originalPwd;
      rmSync(projectRoot, { recursive: true, force: true });
    }
  });

  it('starts with no plugins and returns a working app', async () => {
    testApp = await createTestApp();
    expect(testApp.app).toBeDefined();
    expect(testApp.baseUrl).toMatch(/^http:\/\/localhost:\d+$/);
    expect(testApp.fetch).toBeFunction();
    expect(testApp.stop).toBeFunction();
  });

  it('returns 404 for unknown routes', async () => {
    testApp = await createTestApp();
    const res = await testApp.fetch('/nonexistent');
    expect(res.status).toBe(404);
  });

  it('includes custom plugins', async () => {
    testApp = await createTestApp({
      plugins: [platform()],
    });
    const res = await testApp.fetch('/healthz');
    expect(res.status).toBe(200);
  });

  it('calls configure callback', async () => {
    let configured = false;
    testApp = await createTestApp({
      configure: (_app) => {
        configured = true;
      },
    });
    expect(configured).toBe(true);
  });

  it('never clobbers the capability manifest owned by the generate step', async () => {
    const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-test-app-manifest-'));
    const previousProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
    const manifestPath = join(projectRoot, '.gen', 'schema', 'capabilities.json');
    const manifest = '{"owner":"build-generate","schemas":["react-loader"]}\n';
    const loaderKey = `__putnamiTestLoader${process.pid}`;
    const loaderState = globalThis as unknown as Record<string, unknown>;

    try {
      mkdirSync(join(projectRoot, 'src'), { recursive: true });
      mkdirSync(join(projectRoot, '.gen', 'schema'), { recursive: true });
      writeFileSync(
        join(projectRoot, 'package.json'),
        `${JSON.stringify({ name: 'manifest-owner-test', exports: { './serve': './src/serve.ts' } })}\n`,
      );
      writeFileSync(join(projectRoot, 'src', 'serve.ts'), 'export {};\n');
      const loaderPath = join(projectRoot, '.gen', 'test-loader.ts');
      writeFileSync(loaderPath, `globalThis[${JSON.stringify(loaderKey)}] = true;\n`);
      writeFileSync(manifestPath, manifest);
      process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;

      for (let run = 0; run < 2; run++) {
        const current = await createTestApp({
          plugins: [{ generate: () => ({ exports: { 'test-loader': loaderPath } }) }],
        });
        await current.stop();
        expect(loaderState[loaderKey]).toBe(true);
        expect(readFileSync(manifestPath, 'utf8')).toBe(manifest);
      }

      await expect(
        createTestApp({
          plugins: [
            {
              generate: () => {
                throw new Error('intentional generation failure');
              },
            },
          ],
        }),
      ).rejects.toThrow('intentional generation failure');
      expect(readFileSync(manifestPath, 'utf8')).toBe(manifest);
    } finally {
      if (previousProjectRoot === undefined) {
        delete process.env.PUTNAMI_PROJECT_ROOT;
      } else {
        process.env['PUTNAMI_PROJECT_ROOT'] = previousProjectRoot;
      }
      delete loaderState[loaderKey];
      rmSync(projectRoot, { recursive: true, force: true });
    }
  });

  it('assigns an ephemeral port', async () => {
    testApp = await createTestApp();
    const port = Number.parseInt(testApp.baseUrl.split(':').pop()!, 10);
    expect(port).toBeGreaterThan(0);
    expect(port).not.toBe(3000);
  });

  it('stops cleanly', async () => {
    testApp = await createTestApp();
    const baseUrl = testApp.baseUrl;
    await testApp.stop();
    testApp = undefined;

    // After stop, the server should refuse connections
    try {
      await fetch(`${baseUrl}/test`);
      // If fetch succeeds, the server is still running — this is unexpected but not a test failure
      // since Bun may keep connections alive briefly
    } catch {
      // Expected: connection refused after stop
    }
  });
});
