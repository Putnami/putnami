import { afterAll, beforeAll, describe, expect, it } from 'bun:test';
import { existsSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { react } from '@putnami/web';
import { tasks } from '../src/store';

const SAMPLE_ROOT = join(import.meta.dir, '..');
const packagedRoots: string[] = [];

describe('web sample', () => {
  let testApp: TestApp;

  beforeAll(async () => {
    testApp = await createTestApp({
      plugins: [react()],
    });
  });

  afterAll(async () => {
    await testApp.stop();
    for (const root of packagedRoots.splice(0)) rmSync(root, { recursive: true, force: true });
  });

  it('serves SSR from the actual packaged entrypoint while .gen/src is absent', async () => {
    const bundledEntrypoint = join(SAMPLE_ROOT, '.gen', 'src', 'serve.bundled.ts');
    expect(existsSync(bundledEntrypoint)).toBe(true);

    const packagedRoot = mkdtempSync(join(tmpdir(), 'putnami-react-capability-activation-'));
    packagedRoots.push(packagedRoot);
    const build = await Bun.build({
      entrypoints: [bundledEntrypoint],
      outdir: packagedRoot,
      target: 'bun',
      naming: 'packaged-react-server.js',
    });
    if (!build.success) {
      throw new Error(build.logs.map((log) => log.message).join('\n'));
    }
    const output = build.outputs.find((artifact) => artifact.kind === 'entry-point');
    if (!output) throw new Error('packaged React entrypoint output was not emitted');
    expect(existsSync(join(packagedRoot, '.gen', 'src'))).toBe(false);

    const port = 42_000 + (process.pid % 1000);
    const childEnv = { ...process.env };
    childEnv['PWD'] = packagedRoot;
    childEnv['PUTNAMI_PROJECT_ROOT'] = packagedRoot;
    childEnv['PORT'] = String(port);
    childEnv['NODE_ENV'] = 'test';
    // The packaged root carries no `conf/`, and analytics() fails closed
    // without a server-side key. A container hands the value in as inline YAML,
    // which is exactly what a deployment does.
    childEnv['CONFIG_DATA'] = 'analytics:\n  secret: sample-only-secret-do-not-reuse-0123456789\n';
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
          response = await fetch(`http://127.0.0.1:${port}/tasks`);
          break;
        } catch {
          await Bun.sleep(100);
        }
      }
      expect(response?.status).toBe(200);
      const html = await response?.text();
      expect(html).toContain('<!DOCTYPE html');
      expect(html).toContain('Tasks');
    } finally {
      server.kill();
      await server.exited;
    }
  }, 20_000);

  describe('GET / (static / SSG + islands)', () => {
    it('serves pre-rendered HTML with no full-page hydration', async () => {
      const res = await testApp.fetch('/');
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('<!DOCTYPE html');
      expect(html).toContain('Welcome');
      // SSG output ships no full-page hydration bundle.
      expect(html).not.toContain('hydrate.main');
      expect(html).not.toContain('__staticRouterHydrationData');
    });

    it('emits an island boundary and loads only the islands bundle', async () => {
      const res = await testApp.fetch('/');
      const html = await res.text();
      expect(html).toContain('<putnami-island data-island="Counter"');
      expect(html).toContain('data-strategy="visible"');
      expect(html).toContain('/react-islands/islands.');
    });
  });

  describe('GET /tasks', () => {
    it('should render the tasks page as HTML', async () => {
      const res = await testApp.fetch('/tasks');
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('<!DOCTYPE html');
    });
  });

  describe('GET /tasks/1', () => {
    it('should render a single task page', async () => {
      const res = await testApp.fetch('/tasks/1');
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('<!DOCTYPE html');
    });
  });

  describe('GET /tasks/new', () => {
    it('should render the new task form', async () => {
      const res = await testApp.fetch('/tasks/new');
      expect(res.status).toBe(200);

      const html = await res.text();
      expect(html).toContain('<!DOCTYPE html');
    });

    it('rejects a mutation without a CSRF token and accepts the no-JavaScript double-submit form', async () => {
      const rejectedTitle = `CSRF rejected sample task ${process.pid}`;
      const acceptedTitle = `CSRF protected sample task ${process.pid}`;
      try {
        const rejected = await testApp.fetch('/tasks/new', {
          method: 'POST',
          headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
          body: new URLSearchParams({ title: rejectedTitle }),
        });
        expect(rejected.status).toBe(403);

        const rejectedList = await testApp.fetch('/tasks');
        expect(rejectedList.status).toBe(200);
        expect(await rejectedList.text()).not.toContain(rejectedTitle);

        const formResponse = await testApp.fetch('/tasks/new');
        const setCookie = formResponse.headers.get('set-cookie') ?? '';
        const token = /_csrf=([^;]+)/.exec(setCookie)?.[1];
        expect(token).toBeString();

        const formHtml = await formResponse.text();
        expect(formHtml).toContain('name="_csrf"');
        expect(formHtml).toContain(`value="${token}"`);

        const accepted = await testApp.fetch('/tasks/new', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/x-www-form-urlencoded',
            Cookie: `_csrf=${token}`,
          },
          body: new URLSearchParams({ _csrf: token as string, title: acceptedTitle }),
        });
        expect(accepted.status).toBe(200);

        const list = await testApp.fetch('/tasks');
        expect(await list.text()).toContain(acceptedTitle);
      } finally {
        for (const title of [rejectedTitle, acceptedTitle]) {
          for (const task of [...tasks.values()]) {
            if (task.title === title) tasks.delete(task.id);
          }
        }
      }
    });
  });
});
