import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import {
  createStaticRenderContext,
  fillRoutePath,
  isDynamicRoute,
  isStaticRenderViolation,
  normalizeStatic,
  StaticRenderViolation,
  staticHtmlPath,
  toStaticConfig,
} from '../../src/ssr/static';

describe('normalizeStatic', () => {
  specTest(
    'defaults to ssg with no options',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'explicit-render-mode',
      check: 'page-static-defaults-to-ssg',
    },
    () => {
      expect(normalizeStatic()).toEqual({ mode: 'ssg' });
    },
  );

  specTest(
    'treats a bare revalidate number as ISR seconds',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'explicit-render-mode',
      check: 'a-bare-revalidate-number-selects-isr',
    },
    () => {
      expect(normalizeStatic({ revalidate: 60 })).toEqual({ mode: 'isr', revalidate: { seconds: 60 } });
    },
  );

  specTest(
    'ignores a non-positive revalidate number (stays ssg)',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'explicit-render-mode',
      check: 'a-non-positive-revalidate-stays-ssg',
    },
    () => {
      expect(normalizeStatic({ revalidate: 0 })).toEqual({ mode: 'ssg' });
    },
  );

  it('supports tag-based revalidation', () => {
    expect(normalizeStatic({ revalidate: { tags: ['posts'] } })).toEqual({
      mode: 'isr',
      revalidate: { tags: ['posts'] },
    });
  });

  it('supports combined seconds + tags', () => {
    expect(normalizeStatic({ revalidate: { seconds: 30, tags: ['a', 'b'] } })).toEqual({
      mode: 'isr',
      revalidate: { seconds: 30, tags: ['a', 'b'] },
    });
  });

  it('retains the paths function', () => {
    const paths = async () => [{ id: '1' }];
    const config = normalizeStatic({ paths });
    expect(config.mode).toBe('ssg');
    expect(config.paths).toBe(paths);
  });
});

describe('toStaticConfig', () => {
  it('drops paths and keeps mode + revalidate', () => {
    expect(toStaticConfig({ mode: 'isr', revalidate: { seconds: 10 } })).toEqual({
      mode: 'isr',
      revalidate: { seconds: 10 },
    });
    expect(toStaticConfig({ mode: 'ssg' })).toEqual({ mode: 'ssg' });
  });
});

describe('staticHtmlPath', () => {
  it('maps root to index.html', () => {
    expect(staticHtmlPath('/')).toBe('index.html');
  });

  it('maps nested routes to <path>.html', () => {
    expect(staticHtmlPath('/blog/hello')).toBe('blog/hello.html');
  });

  it('trims trailing slashes', () => {
    expect(staticHtmlPath('/blog/hello/')).toBe('blog/hello.html');
  });
});

describe('fillRoutePath / isDynamicRoute', () => {
  it('substitutes params', () => {
    expect(fillRoutePath('/tasks/:id', { id: '7' })).toBe('/tasks/7');
  });

  it('encodes param values', () => {
    expect(fillRoutePath('/tags/:name', { name: 'a b' })).toBe('/tags/a%20b');
  });

  it('throws on missing params', () => {
    expect(() => fillRoutePath('/tasks/:id', {})).toThrow('Missing param "id"');
  });

  it('substitutes the catch-all splat verbatim, preserving sub-path slashes', () => {
    expect(fillRoutePath('/docs/*', { '*': 'getting-started/introduction' })).toBe(
      '/docs/getting-started/introduction',
    );
  });

  it('keeps splat slugs literal (no percent-encoding) so url/file/lookup align', () => {
    expect(fillRoutePath('/docs/*', { '*': 'tooling-&-workspace/cli' })).toBe('/docs/tooling-&-workspace/cli');
  });

  it('throws on a missing splat param', () => {
    expect(() => fillRoutePath('/docs/*', {})).toThrow('Missing splat param "*"');
  });

  specTest(
    'rejects splat values that could escape or normalize static output paths',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'explicit-render-mode',
      check: 'a-dynamic-path-cannot-escape-the-static-output-root',
    },
    () => {
      const unsafeSplats = [
        '../secret',
        'guide/../secret',
        'guide/./intro',
        'guide//intro',
        '/absolute',
        'guide\\intro',
      ];
      for (const splat of unsafeSplats) {
        expect(() => fillRoutePath('/docs/*', { '*': splat })).toThrow('Unsafe splat param "*"');
      }
    },
  );

  it('detects dynamic routes', () => {
    expect(isDynamicRoute('/tasks/:id')).toBe(true);
    expect(isDynamicRoute('/docs/*')).toBe(true);
    expect(isDynamicRoute('/tasks')).toBe(false);
  });
});

describe('createStaticRenderContext — determinism contract', () => {
  it('exposes route params', () => {
    const ctx = createStaticRenderContext('/tasks/:id', { id: '7' }) as unknown as { params: Record<string, string> };
    expect(ctx.params).toEqual({ id: '7' });
  });

  specTest(
    'throws StaticRenderViolation on request-scoped access',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'explicit-render-mode',
      check: 'a-static-loader-reading-request-data-raises-a-violation',
    },
    () => {
      const ctx = createStaticRenderContext('/dash', {}) as unknown as Record<string, unknown>;
      expect(() => ctx['user']).toThrow(StaticRenderViolation);
      expect(() => ctx['headers']).toThrow(/request-scoped/);
      expect(() => ctx['queryParams']).toThrow(StaticRenderViolation);
    },
  );

  it('provides a no-op logger', () => {
    const ctx = createStaticRenderContext('/dash', {}) as unknown as { logger: { info: (m: string) => void } };
    expect(() => ctx.logger.info('hi')).not.toThrow();
  });

  it('isStaticRenderViolation narrows correctly', () => {
    expect(isStaticRenderViolation(new StaticRenderViolation('/x', 'user'))).toBe(true);
    expect(isStaticRenderViolation(new Error('nope'))).toBe(false);
  });
});
