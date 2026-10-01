import { gzipSync } from 'bun';
import { afterEach, describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { HttpPlugin } from '@putnami/application';
import { createTestApp, type TestApp } from '@putnami/application/testing';
import { resetConfigLoader, useConfig } from '@putnami/runtime';
import { AnalyticsConfig } from '../src/server/analytics.config';
import { registerTrackerAsset } from '../src/server/http/tracker-asset';

const BUNDLE = 'analytics.abc123.js.gz';
const ROUTE = '/analytics/analytics.abc123.js';

const tempRoots: string[] = [];
let testApp: TestApp | undefined;
const previousProjectRoot = process.env.PUTNAMI_PROJECT_ROOT;

/**
 * Points `getProjectRoot()` at an empty temp project so the asset lookup — and
 * the serve-entry generation `createTestApp` triggers — stay out of the
 * package tree.
 */
function useTempProjectRoot(withBundle: boolean): string {
  const root = mkdtempSync(join(tmpdir(), 'putnami-analytics-asset-'));
  tempRoots.push(root);
  if (withBundle) {
    const dir = join(root, '.gen', 'public', 'analytics');
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, BUNDLE), gzipSync(Buffer.from('console.log("tracker");')));
  }
  process.env.PUTNAMI_PROJECT_ROOT = root;
  resetConfigLoader();
  return root;
}

afterEach(async () => {
  await testApp?.stop();
  testApp = undefined;
  for (const root of tempRoots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
  if (previousProjectRoot === undefined) {
    delete process.env.PUTNAMI_PROJECT_ROOT;
  } else {
    process.env.PUTNAMI_PROJECT_ROOT = previousProjectRoot;
  }
  resetConfigLoader();
});

describe('registerTrackerAsset', () => {
  it('serves the hashed bundle with immutable gzip headers', async () => {
    useTempProjectRoot(true);
    let route: string | undefined;

    testApp = await createTestApp({
      configure: (app) => {
        route = registerTrackerAsset(app.getPlugin(HttpPlugin), useConfig(AnalyticsConfig));
      },
    });

    expect(route).toBe(ROUTE);
    const response = await testApp.fetch(ROUTE);
    expect(response.status).toBe(200);
    expect(response.headers.get('cache-control')).toBe('public, max-age=31536000, immutable');
    expect(response.headers.get('content-type')).toContain('application/javascript');
    expect(response.headers.get('content-disposition')).toBe('filename="analytics/analytics.abc123.js"');
  });

  it('returns undefined and registers nothing when no bundle was built', async () => {
    useTempProjectRoot(false);
    let route: string | undefined = '/unset';

    testApp = await createTestApp({
      configure: (app) => {
        route = registerTrackerAsset(app.getPlugin(HttpPlugin), useConfig(AnalyticsConfig));
      },
    });

    expect(route).toBeUndefined();
    const response = await testApp.fetch(ROUTE);
    expect(response.status).toBe(404);
  });
});
