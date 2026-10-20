import { beforeAll, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { resolve } from 'node:path';
import type { RouterNavigationReport } from '../fixtures/router-navigation/browser';

const packageRoot = resolve(import.meta.dir, '..', '..');
const BROWSER_TIMEOUT_MS = 20_000;
const scroll = {
  feature: 'typescript/web-application-delivery',
  requirement: 'navigation-scroll',
} as const;

/**
 * Navigates a memory router whose root route restores scroll, in a DOM, in a
 * separate browser process, and checks where the window scrolled and which
 * documents it loaded.
 */
describe('client navigation in a router', () => {
  let report: RouterNavigationReport;

  beforeAll(() => {
    const child = Bun.spawnSync({
      cmd: [process.execPath, resolve(packageRoot, 'test/fixtures/router-navigation/browser.ts')],
      cwd: packageRoot,
      stdout: 'pipe',
      stderr: 'pipe',
      timeout: BROWSER_TIMEOUT_MS,
    });
    if (child.exitCode !== 0) {
      throw new Error(`the browser process failed:\n${child.stderr.toString()}`);
    }
    report = JSON.parse(child.stdout.toString()) as RouterNavigationReport;
  });

  specTest('scrolls a new page to the top', { ...scroll, check: 'a-new-location-scrolls-to-the-top' }, () => {
    expect(report.errors).toEqual([]);
    expect(report.scrolls['newPage']).toEqual(['top 0']);
  });

  specTest('scrolls to the hash target', { ...scroll, check: 'a-new-location-scrolls-to-its-hash-target' }, () => {
    expect(report.scrolls['hash']).toEqual(['into target']);
  });

  specTest(
    'restores the saved position on back',
    { ...scroll, check: 'back-and-forward-restore-the-saved-position' },
    () => {
      expect(report.scrolls['back']).toEqual(['top 250']);
    },
  );

  it('keeps the position when the navigation asks to', () => {
    expect(report.scrolls['preventScrollReset']).toEqual([]);
  });

  specTest(
    'loads a server-only redirect target as a document',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'redirect-outside-the-router',
      check: 'a-redirect-to-a-page-the-router-does-not-render-loads-a-document',
    },
    () => {
      expect(report.documentLoads).toEqual(['/login']);
      expect(report.locations['toLogin']).not.toBe('/login');
    },
  );

  specTest(
    'follows a redirect to a router page in the router',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'redirect-outside-the-router',
      check: 'a-redirect-to-a-router-page-stays-in-the-router',
    },
    () => {
      expect(report.locations['toB']).toBe('/b');
      expect(report.documentLoads).not.toContain('/b');
    },
  );
});
