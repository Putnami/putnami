import { describe, expect, it } from 'bun:test';
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import {
  detectLoaderInject,
  detectRouteLoaderInject,
  layoutAppliesToRoute,
  mergeLoaderInject,
  prerenderStaticRoutes,
} from '../../src/ssr/generator/static-prerender';
import type { ReactApplication } from '../../src/ssr/react-application';
import type { StaticRenderResult } from '../../src/ssr/static-render';
import { StaticRenderViolation } from '../../src/ssr/static';
import { readStaticBuildVersion, writeStaticBuildVersion } from '../../src/ssr/static-build-version';

function fakeApp(prerender: (pathname: string) => Promise<StaticRenderResult>): ReactApplication {
  return { prerender } as unknown as ReactApplication;
}

function tempDir(): string {
  return mkdtempSync(join(tmpdir(), 'putnami-prerender-'));
}

describe('prerenderStaticRoutes', () => {
  it('writes HTML for a static route', async () => {
    const dir = tempDir();
    const app = fakeApp(async (pathname) => ({ html: `<html>${pathname}</html>`, status: 200 }));
    const pages = await prerenderStaticRoutes(app, [{ route: '/', staticConfig: { mode: 'ssg' } }], dir);

    expect(pages).toHaveLength(1);
    expect(pages[0].file).toBe('index.html');
    expect(readFileSync(join(dir, 'index.html'), 'utf8')).toContain('<html>/</html>');
    rmSync(dir, { recursive: true, force: true });
  });

  it('enumerates dynamic paths()', async () => {
    const dir = tempDir();
    const app = fakeApp(async (pathname) => ({ html: `<p>${pathname}</p>`, status: 200 }));
    const pages = await prerenderStaticRoutes(
      app,
      [{ route: '/tasks/:id', staticConfig: { mode: 'ssg', paths: async () => [{ id: '1' }, { id: '2' }] } }],
      dir,
    );

    expect(pages.map((p) => p.pathname).sort()).toEqual(['/tasks/1', '/tasks/2']);
    rmSync(dir, { recursive: true, force: true });
  });

  it('enumerates catch-all (splat) paths() to concrete files', async () => {
    const dir = tempDir();
    const app = fakeApp(async (pathname) => ({ html: `<p>${pathname}</p>`, status: 200 }));
    const pages = await prerenderStaticRoutes(
      app,
      [
        {
          route: '/docs/*',
          staticConfig: {
            mode: 'isr',
            revalidate: { tags: ['docs'] },
            paths: async () => [{ '*': 'getting-started/introduction' }, { '*': 'tooling-&-workspace/cli' }],
          },
        },
      ],
      dir,
    );

    expect(pages.map((p) => p.pathname).sort()).toEqual([
      '/docs/getting-started/introduction',
      '/docs/tooling-&-workspace/cli',
    ]);
    expect(pages.map((p) => p.file).sort()).toEqual([
      'docs/getting-started/introduction.html',
      'docs/tooling-&-workspace/cli.html',
    ]);
    expect(readFileSync(join(dir, 'docs/getting-started/introduction.html'), 'utf8')).toContain(
      '/docs/getting-started/introduction',
    );
    rmSync(dir, { recursive: true, force: true });
  });

  it('does not write static output outside the static directory', async () => {
    const dir = tempDir();
    const outsideFile = join(dir, '..', 'outside.html');
    rmSync(outsideFile, { force: true });
    const app = fakeApp(async (pathname) => ({ html: `<p>${pathname}</p>`, status: 200 }));

    const pages = await prerenderStaticRoutes(app, [{ route: '/../outside', staticConfig: { mode: 'ssg' } }], dir);

    expect(pages).toHaveLength(0);
    expect(existsSync(outsideFile)).toBe(false);
    rmSync(dir, { recursive: true, force: true });
  });

  it('skips dynamic routes without paths()', async () => {
    const dir = tempDir();
    const app = fakeApp(async () => ({ html: 'x', status: 200 }));
    const pages = await prerenderStaticRoutes(app, [{ route: '/tasks/:id', staticConfig: { mode: 'ssg' } }], dir);

    expect(pages).toHaveLength(0);
    rmSync(dir, { recursive: true, force: true });
  });

  it('rethrows StaticRenderViolation as a hard build error', async () => {
    const dir = tempDir();
    const app = fakeApp(async () => {
      throw new StaticRenderViolation('/', 'user');
    });
    await expect(
      prerenderStaticRoutes(app, [{ route: '/', staticConfig: { mode: 'ssg' } }], dir),
    ).rejects.toBeInstanceOf(StaticRenderViolation);
    rmSync(dir, { recursive: true, force: true });
  });

  it('swallows non-violation errors so one page cannot break the build', async () => {
    const dir = tempDir();
    const app = fakeApp(async () => {
      throw new Error('boom');
    });
    const pages = await prerenderStaticRoutes(app, [{ route: '/', staticConfig: { mode: 'ssg' } }], dir);

    expect(pages).toHaveLength(0);
    rmSync(dir, { recursive: true, force: true });
  });

  it('reports island presence in the rendered HTML', async () => {
    const dir = tempDir();
    const app = fakeApp(async () => ({
      html: '<putnami-island data-island="X"></putnami-island>',
      status: 200,
    }));
    const pages = await prerenderStaticRoutes(app, [{ route: '/', staticConfig: { mode: 'ssg' } }], dir);

    expect(pages[0].hasIslands).toBe(true);
    rmSync(dir, { recursive: true, force: true });
  });

  const buildInfo = { version: '0.1.0-20260927164104-94392288', suffix: '20260927164104-94392288' };

  specTest(
    'records the build version a pre-rendered page shows',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'static-build-version',
      check: 'prerender-records-the-version-a-page-shows',
    },
    async () => {
      const dir = tempDir();
      const app = fakeApp(async () => ({ html: `<footer>v${buildInfo.version}</footer>`, status: 200 }));
      await prerenderStaticRoutes(app, [{ route: '/', staticConfig: { mode: 'ssg' } }], dir, undefined, buildInfo);

      expect(readStaticBuildVersion(dir)).toBe(buildInfo.version);
      rmSync(dir, { recursive: true, force: true });
    },
  );

  it('records no version when no page shows it, and removes a stale record', async () => {
    const dir = tempDir();
    writeStaticBuildVersion(dir, '0.1.0-stale');
    const app = fakeApp(async () => ({ html: '<footer>no version</footer>', status: 200 }));
    await prerenderStaticRoutes(app, [{ route: '/', staticConfig: { mode: 'ssg' } }], dir, undefined, buildInfo);

    expect(readStaticBuildVersion(dir)).toBeUndefined();
    rmSync(dir, { recursive: true, force: true });
  });

  it('records no bare release tag, which page content can contain', async () => {
    const dir = tempDir();
    const app = fakeApp(async () => ({ html: '<footer>v1.0.0</footer>', status: 200 }));
    await prerenderStaticRoutes(app, [{ route: '/', staticConfig: { mode: 'ssg' } }], dir, undefined, {
      version: '1.0.0',
      suffix: '20260927164104-94392288',
    });

    expect(readStaticBuildVersion(dir)).toBeUndefined();
    rmSync(dir, { recursive: true, force: true });
  });
});

describe('loader inject detection', () => {
  it('marks an existing loader that cannot be imported as a hybrid', async () => {
    const dir = tempDir();
    const loaderPath = join(dir, 'loader.ts');
    writeFileSync(loaderPath, 'throw new Error("boom");\n');

    const meta = await detectLoaderInject(loaderPath);
    expect(meta).toEqual({ tokens: [], dynamic: true });
    rmSync(dir, { recursive: true, force: true });
  });

  it('combines page and matching layout loader roots', async () => {
    const dir = tempDir();
    const rootLayout = join(dir, 'root.layout.loader.ts');
    const dashboardLayout = join(dir, 'dashboard.layout.loader.ts');
    const adminLayout = join(dir, 'admin.layout.loader.ts');
    const pageLoader = join(dir, 'page.loader.ts');
    writeFileSync(
      rootLayout,
      'export default { __loader: "putnami:loader", handler: () => null, inject: { tokens: [Symbol.for("root-layout")], dynamic: false } };\n',
    );
    writeFileSync(
      dashboardLayout,
      'export default { __loader: "putnami:loader", handler: () => null, inject: { tokens: [Symbol.for("dashboard-layout")], dynamic: false } };\n',
    );
    writeFileSync(
      adminLayout,
      'export default { __loader: "putnami:loader", handler: () => null, inject: { tokens: [Symbol.for("admin-layout")], dynamic: false } };\n',
    );
    writeFileSync(
      pageLoader,
      'export default { __loader: "putnami:loader", handler: () => null, inject: { tokens: [Symbol.for("page")], dynamic: false } };\n',
    );

    const meta = await detectRouteLoaderInject('/dashboard/settings', pageLoader, [
      { route: '/', absPath: rootLayout },
      { route: '/dashboard', absPath: dashboardLayout },
      { route: '/admin', absPath: adminLayout },
    ]);

    expect(meta.dynamic).toBe(false);
    expect(meta.tokens.map(String)).toEqual(['Symbol(root-layout)', 'Symbol(dashboard-layout)', 'Symbol(page)']);
    rmSync(dir, { recursive: true, force: true });
  });

  it('matches layout routes on path boundaries', () => {
    expect(layoutAppliesToRoute('/', '/dashboard')).toBe(true);
    expect(layoutAppliesToRoute('/dashboard', '/dashboard/settings')).toBe(true);
    expect(layoutAppliesToRoute('/dash', '/dashboard')).toBe(false);
  });

  it('merges dynamic loader metadata conservatively', () => {
    const meta = mergeLoaderInject([
      { tokens: [Symbol.for('a')], dynamic: false },
      { tokens: [], dynamic: true },
    ]);
    expect(meta.tokens.map(String)).toEqual(['Symbol(a)']);
    expect(meta.dynamic).toBe(true);
  });
});
