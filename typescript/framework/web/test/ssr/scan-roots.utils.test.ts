import { afterEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { inferRoutePrefix, resolveScanRoots } from '../../src/ssr/scan-roots.utils';

describe('scan-roots.utils', () => {
  const tempDirs: string[] = [];

  afterEach(() => {
    for (const tempDir of tempDirs.splice(0)) {
      rmSync(tempDir, { recursive: true, force: true });
    }
  });

  it('infers route prefixes from web scan roots', () => {
    expect(inferRoutePrefix('/workspace/src/admin/web')).toBe('/admin');
    expect(inferRoutePrefix('/workspace/src/admin/(web)')).toBe('/admin');
    expect(inferRoutePrefix('/workspace/src/app')).toBeUndefined();
  });

  specTest(
    'resolves configured scan roots and normalizes prefixes',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'route-graph',
      check: 'a-scan-root-prefix-is-inferred-and-normalized',
    },
    () => {
      const projectRoot = mkdtempSync('/tmp/putnami-web-scan-roots-');
      tempDirs.push(projectRoot);
      mkdirSync(join(projectRoot, 'src', 'marketing', 'web'), { recursive: true });
      mkdirSync(join(projectRoot, 'src', 'docs', 'app'), { recursive: true });

      const roots = resolveScanRoots(projectRoot, {
        scanRoots: [
          { path: 'src/marketing/web' },
          { path: 'src/docs/app', routePrefix: 'docs' },
          { path: 'src/missing/app' },
        ],
      } as never);

      expect(roots).toEqual([
        {
          scanPath: join(projectRoot, 'src', 'marketing', 'web'),
          relativePath: 'src/marketing/web',
          routePrefix: '/marketing',
        },
        {
          scanPath: join(projectRoot, 'src', 'docs', 'app'),
          relativePath: 'src/docs/app',
          routePrefix: '/docs',
        },
      ]);
    },
  );

  specTest(
    'falls back to the default src/app scan path when no roots are configured',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'route-graph',
      check: 'the-default-scan-path-applies-when-none-is-configured',
    },
    () => {
      const projectRoot = mkdtempSync('/tmp/putnami-web-default-root-');
      tempDirs.push(projectRoot);
      mkdirSync(join(projectRoot, 'src', 'app'), { recursive: true });

      const roots = resolveScanRoots(projectRoot, {} as never);

      expect(roots).toEqual([
        {
          scanPath: join(projectRoot, 'src', 'app'),
          relativePath: 'src/app',
          routePrefix: undefined,
        },
      ]);
    },
  );
});
