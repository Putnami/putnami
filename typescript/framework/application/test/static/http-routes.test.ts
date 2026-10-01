import { describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildStaticHttpRoutes, StaticPlugin } from '../../src/static/static.plugin';

describe('static HTTP route generation', () => {
  it('uses exact root files and bounded directories without a root prefix', () => {
    const routes = buildStaticHttpRoutes({
      projectRoot: '/workspace/app',
      sourcePath: '/workspace/app/public',
      sourceIsDirectory: true,
      files: ['favicon.ico', 'assets/app.ABC123.js', 'docs/index.html'],
    });

    expect(routes).toEqual([
      expect.objectContaining({ match: 'exact', path: '/favicon.ico', methods: ['GET', 'HEAD'] }),
      expect.objectContaining({ match: 'prefix', path: '/assets/', methods: ['GET', 'HEAD'] }),
      expect.objectContaining({ match: 'exact', path: '/docs', methods: ['GET', 'HEAD'] }),
      expect.objectContaining({ match: 'prefix', path: '/docs/', methods: ['GET', 'HEAD'] }),
    ]);
    expect(routes.some((route) => route.match === 'prefix' && route.path === '/')).toBe(false);
  });

  it('emits a declared non-root mount as one bounded prefix', () => {
    const routes = buildStaticHttpRoutes({
      projectRoot: '/workspace/app',
      sourcePath: '/workspace/app/public',
      sourceIsDirectory: true,
      prefix: '/assets',
      files: ['app.ABC123.js', 'images/logo.svg'],
    });
    expect(routes).toEqual([
      expect.objectContaining({
        match: 'prefix',
        path: '/assets/',
        provenance: expect.objectContaining({ sourceKind: 'static-mount', evidencePath: 'public' }),
      }),
    ]);
  });

  it('does not claim framework assets already present in generated public output', async () => {
    const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
    const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-static-routes-'));
    try {
      process.env['PUTNAMI_PROJECT_ROOT'] = projectRoot;
      const sourcePath = join(projectRoot, 'public');
      mkdirSync(sourcePath, { recursive: true });
      writeFileSync(join(sourcePath, 'favicon.ico'), 'icon');
      const frameworkAssets = join(projectRoot, '.gen', 'public', 'react');
      mkdirSync(frameworkAssets, { recursive: true });
      writeFileSync(join(frameworkAssets, 'chunk.ABC123.js'), 'bundle');

      const result = await new StaticPlugin({ scanPath: sourcePath, compress: 0 }).generate();

      expect(result.httpRoutes).toContainEqual(expect.objectContaining({ match: 'exact', path: '/favicon.ico' }));
      expect(result.httpRoutes).not.toContainEqual(expect.objectContaining({ match: 'prefix', path: '/react/' }));
    } finally {
      if (originalProjectRoot === undefined) {
        delete process.env.PUTNAMI_PROJECT_ROOT;
      } else {
        process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
      }
      rmSync(projectRoot, { recursive: true, force: true });
    }
  });
});
