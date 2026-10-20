import { beforeAll, describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { resolve } from 'node:path';
import type { LinkNavigationReport } from '../fixtures/link-navigation/browser';

const packageRoot = resolve(import.meta.dir, '..', '..');
const BROWSER_TIMEOUT_MS = 20_000;
const navigation = {
  feature: 'typescript/web-application-delivery',
  requirement: 'link-navigation',
} as const;

/**
 * Clicks Links inside a real memory router, in a DOM, in a separate browser
 * process, and checks what the router did for each click.
 */
describe('Link navigation in a router', () => {
  let report: LinkNavigationReport;

  beforeAll(() => {
    const child = Bun.spawnSync({
      cmd: [process.execPath, resolve(packageRoot, 'test/fixtures/link-navigation/browser.ts')],
      cwd: packageRoot,
      stdout: 'pipe',
      stderr: 'pipe',
      timeout: BROWSER_TIMEOUT_MS,
    });
    if (child.exitCode !== 0) {
      throw new Error(`the browser process failed:\n${child.stderr.toString()}`);
    }
    report = JSON.parse(child.stdout.toString()) as LinkNavigationReport;
  });

  specTest(
    'navigates when only the query changes',
    { ...navigation, check: 'a-link-that-changes-only-the-query-navigates' },
    () => {
      expect(report.errors).toEqual([]);
      expect(report.results['queryOnly']).toEqual({ href: '/tasks?env=qa', historyAction: 'PUSH', loads: 1 });
      expect(report.results['queryRemoved']).toEqual({ href: '/tasks', historyAction: 'PUSH', loads: 1 });
    },
  );

  specTest(
    'replaces the entry and re-runs the loader for the current URL',
    { ...navigation, check: 'a-link-to-the-current-url-revalidates' },
    () => {
      expect(report.results['sameUrl']).toEqual({ href: '/tasks', historyAction: 'REPLACE', loads: 1 });
    },
  );

  specTest(
    'navigates when only the hash changes',
    { ...navigation, check: 'a-link-that-changes-only-the-hash-navigates' },
    () => {
      expect(report.results['hashOnly']).toEqual({ href: '/tasks?page=2#details', historyAction: 'PUSH', loads: 0 });
    },
  );
});
