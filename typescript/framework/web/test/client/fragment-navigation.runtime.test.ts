import { beforeAll, describe, expect, it } from 'bun:test';
import { resolve } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import type { FragmentNavigationReport } from '../fixtures/fragment-navigation/browser';

const packageRoot = resolve(import.meta.dir, '..', '..');
const BROWSER_TIMEOUT_MS = 20_000;

/**
 * Hydrates a page with `hydratePage` in a DOM, in a separate browser process,
 * follows a link to an in-page `#id`, goes back and forward, and checks where
 * the window scrolled and which loaders ran.
 */
describe('fragment navigation in a hydrated page', () => {
  let report: FragmentNavigationReport;

  beforeAll(() => {
    const child = Bun.spawnSync({
      cmd: [process.execPath, resolve(packageRoot, 'test/fixtures/fragment-navigation/browser.ts')],
      cwd: packageRoot,
      stdout: 'pipe',
      stderr: 'pipe',
      timeout: BROWSER_TIMEOUT_MS,
    });
    if (child.exitCode !== 0) {
      throw new Error(`the browser process failed:\n${child.stderr.toString()}`);
    }
    report = JSON.parse(child.stdout.toString()) as FragmentNavigationReport;
  });

  specTest(
    'keeps the browser scroll to the target of a fragment link',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'navigation-scroll',
      check: 'a-fragment-link-keeps-the-scroll-to-its-target',
    },
    () => {
      expect(report.errors).toEqual([]);
      expect(report.steps['fragmentLink']).toEqual({
        rendered: '/page#section',
        scrolls: ['into section'],
        loads: [],
      });
    },
  );

  specTest(
    'keeps the loaded data when back drops the fragment',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'fragment-history-steps',
      check: 'a-history-step-that-changes-only-the-fragment-keeps-the-loaded-data',
    },
    () => {
      // Back restores the position the page had before the fragment link.
      expect(report.steps['backDropsTheFragment']).toEqual({ rendered: '/page', scrolls: ['top 0'], loads: [] });
      // Forward restores the position the fragment entry had.
      expect(report.steps['forwardToTheFragment']).toEqual({
        rendered: '/page#section',
        scrolls: [`top 1689`],
        loads: [],
      });
    },
  );

  it('still runs the loaders for a link that drops the fragment, a revalidation and a query change', () => {
    const loads = (name: string) => [...(report.steps[name]?.loads ?? [])].sort();
    expect(loads('linkDropsTheFragment')).toEqual(['layout', 'page']);
    expect(loads('revalidate')).toEqual(['layout', 'page']);
    expect(loads('queryLink')).toEqual(['layout', 'page']);
  });
});
