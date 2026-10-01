import { afterEach, describe, expect, it } from 'bun:test';
import { mkdtempSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { HttpRequestContext } from '@putnami/application';
import { clearCache } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { clearTagRegistry, revalidateTag, staticServeHandler, tagsFor } from '../../src/ssr/static-serve.utils';
import type { StaticRenderResult } from '../../src/ssr/static-render';
import { STATIC_BUILD_VERSION_FILE } from '../../src/ssr/static-build-version';

function ctxFor(pathname: string, params: Record<string, string> = {}): HttpRequestContext {
  return { req: { url: `http://localhost${pathname}` }, params } as unknown as HttpRequestContext;
}

function tempStaticDir(files: Record<string, string>): string {
  const dir = mkdtempSync(join(tmpdir(), 'putnami-static-'));
  for (const [rel, content] of Object.entries(files)) {
    const full = join(dir, rel);
    mkdirSync(join(full, '..'), { recursive: true });
    writeFileSync(full, content);
  }
  return dir;
}

afterEach(async () => {
  clearTagRegistry();
  await clearCache();
});

describe('staticServeHandler — SSG', () => {
  it('serves the pre-rendered HTML file', async () => {
    const dir = tempStaticDir({ 'index.html': '<html>PREBUILT</html>' });
    const handler = staticServeHandler({
      route: '/',
      staticConfig: { mode: 'ssg' },
      staticDir: dir,
      render: async () => ({ html: 'LIVE', status: 200 }) as StaticRenderResult,
    });

    const res = (await handler(ctxFor('/'))) as { get: () => Response };
    const text = await res.get().text();
    expect(text).toContain('PREBUILT');
    rmSync(dir, { recursive: true, force: true });
  });

  it('renders live when the path was not pre-built', async () => {
    const dir = tempStaticDir({ 'index.html': '<html>PREBUILT</html>' });
    const handler = staticServeHandler({
      route: '/tasks/:id',
      staticConfig: { mode: 'ssg' },
      staticDir: dir,
      render: async (pathname) => ({ html: `LIVE ${pathname}`, status: 200 }) as StaticRenderResult,
    });

    const res = (await handler(ctxFor('/tasks/99', { id: '99' }))) as { get: () => Response };
    const text = await res.get().text();
    expect(text).toContain('LIVE /tasks/99');
    rmSync(dir, { recursive: true, force: true });
  });

  it('propagates a loader-driven redirect from the live fallback (e.g. legacy URLs)', async () => {
    const dir = tempStaticDir({});
    const handler = staticServeHandler({
      route: '/docs/*',
      staticConfig: { mode: 'ssg' },
      staticDir: dir,
      render: async () =>
        ({ html: '', status: 301, redirectLocation: '/docs/frameworks/typescript' }) as StaticRenderResult,
    });

    const res = (await handler(ctxFor('/docs/framework', { '*': 'framework' }))) as { get: () => Response };
    const response = res.get();
    expect(response.status).toBe(301);
    expect(response.headers.get('location')).toBe('/docs/frameworks/typescript');
    rmSync(dir, { recursive: true, force: true });
  });
});

describe('staticServeHandler — ISR', () => {
  it('serves the build output first, then renders fresh after tag revalidation', async () => {
    const dir = tempStaticDir({ 'index.html': '<html>BUILD</html>' });
    let renders = 0;
    const handler = staticServeHandler({
      route: '/',
      staticConfig: { mode: 'isr', revalidate: { tags: ['posts'] } },
      staticDir: dir,
      render: async () => {
        renders += 1;
        return { html: `FRESH ${renders}`, status: 200 } as StaticRenderResult;
      },
    });

    const first = await (handler(ctxFor('/')) as Promise<{ get: () => Response }>);
    expect(await first.get().text()).toContain('BUILD');

    // Same request again — still cached build output, no render.
    const second = await (handler(ctxFor('/')) as Promise<{ get: () => Response }>);
    expect(await second.get().text()).toContain('BUILD');
    expect(renders).toBe(0);

    // The route registered itself under the tag.
    expect(tagsFor('posts')).toContain('/:/');

    // Revalidate the tag → next request renders fresh.
    const evicted = await revalidateTag('posts');
    expect(evicted).toBeGreaterThan(0);

    const third = await (handler(ctxFor('/')) as Promise<{ get: () => Response }>);
    expect(await third.get().text()).toContain('FRESH 1');
    rmSync(dir, { recursive: true, force: true });
  });

  it('revalidateTag returns 0 for unknown tags', async () => {
    expect(await revalidateTag('nope')).toBe(0);
  });

  it('renders live on the first serve when the tag was revalidated before the page was cached', async () => {
    // A content-overlay swap on a cold instance calls revalidateTag('docs')
    // before any request has registered/cached this page. Its build output is
    // now stale, so the first serve must render live rather than cache the
    // prebuilt HTML for a full TTL.
    const dir = tempStaticDir({ 'index.html': '<html>BUILD</html>' });
    let renders = 0;
    const handler = staticServeHandler({
      route: '/',
      staticConfig: { mode: 'isr', revalidate: { seconds: 86_400, tags: ['docs'] } },
      staticDir: dir,
      render: async () => {
        renders += 1;
        return { html: `FRESH ${renders}`, status: 200 } as StaticRenderResult;
      },
    });

    expect(await revalidateTag('docs')).toBe(0); // nothing registered/cached yet

    const first = await (handler(ctxFor('/')) as Promise<{ get: () => Response }>);
    expect(await first.get().text()).toContain('FRESH 1');
    expect(renders).toBe(1);
    rmSync(dir, { recursive: true, force: true });
  });
});

describe('staticServeHandler — build version', () => {
  const recorded = JSON.stringify({ version: '0.1.0-rendered' });

  specTest(
    'serves a pre-rendered page under the running build version',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'static-build-version',
      check: 'an-ssg-page-serves-the-running-version',
    },
    async () => {
      const dir = tempStaticDir({
        'index.html': '<footer>v0.1.0-rendered</footer>',
        [STATIC_BUILD_VERSION_FILE]: recorded,
      });
      const handler = staticServeHandler({
        route: '/',
        staticConfig: { mode: 'ssg' },
        staticDir: dir,
        render: async () => ({ html: 'LIVE', status: 200 }) as StaticRenderResult,
        currentVersion: () => '0.1.0-running',
      });

      const res = (await handler(ctxFor('/'))) as { get: () => Response };
      expect(await res.get().text()).toBe('<footer>v0.1.0-running</footer>');
      rmSync(dir, { recursive: true, force: true });
    },
  );

  specTest(
    'restamps the build output an ISR route serves first',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'static-build-version',
      check: 'an-isr-build-output-serves-the-running-version',
    },
    async () => {
      const dir = tempStaticDir({
        'index.html': '<footer>v0.1.0-rendered</footer>',
        [STATIC_BUILD_VERSION_FILE]: recorded,
      });
      const handler = staticServeHandler({
        route: '/',
        staticConfig: { mode: 'isr', revalidate: { seconds: 3600, tags: ['docs'] } },
        staticDir: dir,
        render: async () => ({ html: 'LIVE', status: 200 }) as StaticRenderResult,
        currentVersion: () => '0.1.0-running',
      });

      const res = await (handler(ctxFor('/')) as Promise<{ get: () => Response }>);
      expect(await res.get().text()).toBe('<footer>v0.1.0-running</footer>');
      rmSync(dir, { recursive: true, force: true });
    },
  );

  it('serves the page as rendered without a running version or a record', async () => {
    const withRecord = tempStaticDir({
      'index.html': '<footer>v0.1.0-rendered</footer>',
      [STATIC_BUILD_VERSION_FILE]: recorded,
    });
    const withoutRecord = tempStaticDir({ 'index.html': '<footer>v0.1.0-rendered</footer>' });
    const render = async () => ({ html: 'LIVE', status: 200 }) as StaticRenderResult;

    const noVersion = staticServeHandler({ route: '/', staticConfig: { mode: 'ssg' }, staticDir: withRecord, render });
    const noRecord = staticServeHandler({
      route: '/',
      staticConfig: { mode: 'ssg' },
      staticDir: withoutRecord,
      render,
      currentVersion: () => '0.1.0-running',
    });

    for (const handler of [noVersion, noRecord]) {
      const res = (await handler(ctxFor('/'))) as { get: () => Response };
      expect(await res.get().text()).toBe('<footer>v0.1.0-rendered</footer>');
    }
    rmSync(withRecord, { recursive: true, force: true });
    rmSync(withoutRecord, { recursive: true, force: true });
  });
});
