import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { resetConfigLoader, useLogger } from '@putnami/runtime';
import type { AnalyticsConfigValues } from '../../src/server/analytics.config';
import type { DeclaredEvents } from '../../src/server/declare';
import { INGEST_PATH } from '../../src/server/http/ingest.route';
import { type AnalyticsRuntime, createRuntime } from '../../src/server/runtime';
import type { Sink } from '../../src/server/sink/sink';
import { testConfig } from './fixtures';

/** A server-side key long enough to be a credible HMAC key. */
export const TEST_SECRET = 'a-server-side-key-of-at-least-32-chars';

/** The instant every deterministic proof in this slice is stamped with. */
const FIXED_NOW = new Date('2026-09-02T10:00:00.000Z');

const roots: string[] = [];
let previousRoot: string | undefined;

/**
 * Points `getProjectRoot()` at an empty temp directory.
 *
 * Mandatory before `createTestApp`: `HttpPlugin.generate()` writes a `./serve`
 * export into the project's `package.json`, and a test that let it run against
 * this package would rewrite the manifest and break the next transpile.
 *
 * @returns The temp project root.
 */
export function useTempProjectRoot(): string {
  if (previousRoot === undefined) {
    previousRoot = process.env['PUTNAMI_PROJECT_ROOT'] ?? '';
  }
  const root = mkdtempSync(join(tmpdir(), 'putnami-analytics-'));
  roots.push(root);
  process.env['PUTNAMI_PROJECT_ROOT'] = root;
  resetConfigLoader();
  return root;
}

/** Restores the project root and removes every temp directory. */
export function restoreProjectRoot(): void {
  for (const root of roots.splice(0)) {
    rmSync(root, { recursive: true, force: true });
  }
  if (previousRoot !== undefined) {
    if (previousRoot === '') {
      process.env['PUTNAMI_PROJECT_ROOT'] = undefined;
    } else {
      process.env['PUTNAMI_PROJECT_ROOT'] = previousRoot;
    }
    previousRoot = undefined;
  }
  resetConfigLoader();
}

/** What a test wants to vary on the runtime. */
export interface TestRuntimeOptions {
  sink: Sink;
  config?: Partial<AnalyticsConfigValues>;
  events?: DeclaredEvents;
  knownRoutes?: Iterable<string>;
  trackerUrl?: string;
  consent?: AnalyticsRuntime['consent'];
  now?: () => Date;
}

/**
 * Builds a fully resolved runtime without composing the plugin, so a proof can
 * exercise one seam at a time.
 *
 * @param options - What the test varies.
 * @returns The runtime.
 */
export function testRuntime(options: TestRuntimeOptions): AnalyticsRuntime {
  const events = options.events ?? {};
  return createRuntime({
    config: testConfig(options.config),
    secret: TEST_SECRET,
    datasource: 'analytics',
    declared: new Set(Object.keys(events)),
    declaredSchemas: events,
    trackerUrl: options.trackerUrl,
    knownRoutes: new Set(options.knownRoutes ?? []),
    endpoint: INGEST_PATH,
    sink: options.sink,
    logger: useLogger('@putnami/analytics'),
    consent: options.consent,
    app: 'demo',
    now: options.now ?? (() => FIXED_NOW),
  });
}
