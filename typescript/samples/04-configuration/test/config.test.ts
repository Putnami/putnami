import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { existsSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { api, config } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';

const SAMPLE_ROOT = join(import.meta.dir, '..');
const packagedRoots: string[] = [];

describe('configuration sample', () => {
  let testApp: TestApp;

  beforeAll(async () => {
    testApp = await createTestApp({
      plugins: [config(), api({ scanPath: 'src/api' })],
    });
  });

  afterAll(async () => {
    await testApp.stop();
    for (const root of packagedRoots.splice(0)) rmSync(root, { recursive: true, force: true });
  });

  it('runs the generated packaged entrypoint with routes and config activated while .gen/src is absent', async () => {
    const bundledEntrypoint = join(SAMPLE_ROOT, '.gen', 'src', 'serve.bundled.ts');
    expect(existsSync(bundledEntrypoint)).toBe(true);

    const packagedRoot = mkdtempSync(join(tmpdir(), 'putnami-capability-activation-'));
    packagedRoots.push(packagedRoot);
    const build = await Bun.build({
      entrypoints: [bundledEntrypoint],
      outdir: packagedRoot,
      target: 'bun',
      naming: 'packaged-server.js',
    });
    if (!build.success) {
      throw new Error(build.logs.map((log) => log.message).join('\n'));
    }
    const output = build.outputs.find((artifact) => artifact.kind === 'entry-point');
    if (!output) throw new Error('packaged entrypoint output was not emitted');

    // The process runs from an isolated package root. There is no generated
    // source tree for ApiPlugin's dynamic-import fallback to scan.
    expect(existsSync(join(packagedRoot, '.gen', 'src'))).toBe(false);

    const port = 43_000 + (process.pid % 1000);
    const childEnv = { ...process.env };
    childEnv['PWD'] = packagedRoot;
    childEnv['PUTNAMI_PROJECT_ROOT'] = packagedRoot;
    childEnv['PORT'] = String(port);
    childEnv['NODE_ENV'] = 'test';
    childEnv['PUTNAMI_WORKSPACE_ROOT'] = undefined;
    const server = Bun.spawn([process.execPath, output.path], {
      cwd: packagedRoot,
      env: childEnv,
      stdout: 'pipe',
      stderr: 'pipe',
    });

    try {
      let response: Response | undefined;
      for (let attempt = 0; attempt < 50; attempt++) {
        try {
          response = await fetch(`http://127.0.0.1:${port}/database`);
          break;
        } catch {
          await Bun.sleep(100);
        }
      }
      expect(response?.status).toBe(200);
      const data = (await response?.json()) as {
        details?: { host?: string; port?: number; database?: string; user?: string };
      };
      expect(data.details).toEqual({ host: 'localhost', port: 5432, database: 'test_db', user: 'test' });
    } finally {
      server.kill();
      await server.exited;
    }
  }, 20_000);

  describe('GET /', () => {
    it('should return overview of all configuration', async () => {
      const res = await testApp.fetch('/');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.message).toContain('Welcome');
      expect(data.app).toBeDefined();
      expect(data.app.name).toBe('Config Sample (Test)');
      expect(data.app.version).toBe('1.0.0');
      expect(data.databases).toBeDefined();
      expect(data.databases.primary.host).toBe('localhost');
      expect(data.databases.primary.port).toBe(5432);
    });
  });

  describe('GET /env', () => {
    it('should return environment detection info', async () => {
      const res = await testApp.fetch('/env');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.currentEnvironment).toBeDefined();
      expect(data.detectionLogic).toBeDefined();
    });
  });

  describe('GET /database', () => {
    it('should return primary database configuration', async () => {
      const res = await testApp.fetch('/database');
      expect(res.status).toBe(200);

      const data = await res.json();
      expect(data.description).toBe('Primary Database Configuration');
      expect(data.details.host).toBe('localhost');
      expect(data.details.port).toBe(5432);
      expect(data.details.database).toBe('test_db');
    });
  });
});
