import { describe, expect, it } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { joinPath } from '@putnami/utils';

/**
 * Determinism guardrail for the web rendering manifest.
 *
 * The migration's whole point is that the build does NOT catch a regression
 * where the shared shell silently loses interactivity, a route flips back to
 * per-request SSR/full hydration, or the client-JS budget balloons. The manifest
 * does — so assert its invariants here. If you intentionally change rendering,
 * update these expectations in the same PR (that is the review signal).
 *
 * Regenerate with: putnami build --projects putnami.dev
 */
const MANIFEST_PATH = joinPath(import.meta.dir, '..', '.gen', 'putnami-web-manifest.json');

// Ceiling for a single route's client-JS budget (gzipped bytes). Today ~121 KB
// of islands; full-tree hydration would be ~152 KB. Bump deliberately.
const MAX_CLIENT_JS_BYTES = 140_000;

const EXPECTED_ISLANDS: Record<string, string> = {
  navbar: 'load',
  search: 'idle',
  'hero-terminal': 'visible',
  // The footer's latest Putnami version, fetched once the page is idle.
  'release-version': 'idle',
  'docs/[...page]/doc-toc': 'idle',
  'docs/[...page]/doc-enhancer': 'idle',
  // Redesign islands: the docs-hub search trigger and the per-page
  // "Copy as Markdown" agent affordance — both deferred to idle.
  'docs/search-trigger': 'idle',
  'docs/[...page]/copy-markdown': 'idle',
};

interface ManifestRoute {
  route: string;
  mode: string;
  hydration: string;
  clientJsBytes: number;
  diProvenStatic?: boolean;
}
interface ManifestIsland {
  id: string;
  strategy: string;
}
interface WebManifest {
  routes: ManifestRoute[];
  islands: ManifestIsland[];
  totals: { ssr: number; maxClientJsBytes: number };
}

describe('web rendering manifest (determinism guardrail)', () => {
  it('exists (run `putnami build` before tests)', () => {
    expect(existsSync(MANIFEST_PATH)).toBe(true);
  });

  const manifest = existsSync(MANIFEST_PATH)
    ? (JSON.parse(readFileSync(MANIFEST_PATH, 'utf8')) as WebManifest)
    : undefined;

  it('renders every route statically (no per-request SSR)', () => {
    expect(manifest).toBeDefined();
    expect(manifest?.totals.ssr).toBe(0);
    for (const route of manifest?.routes ?? []) {
      expect(['ssg', 'isr']).toContain(route.mode);
    }
  });

  it('keeps every route interactive via islands and proven static', () => {
    for (const route of manifest?.routes ?? []) {
      expect(route.hydration).toBe('islands');
      expect(route.diProvenStatic).toBe(true);
    }
  });

  it('stays within the client-JS budget', () => {
    expect(manifest?.totals.maxClientJsBytes).toBeLessThanOrEqual(MAX_CLIENT_JS_BYTES);
  });

  it('hydrates exactly the expected islands with their load strategies', () => {
    const actual = Object.fromEntries((manifest?.islands ?? []).map((i) => [i.id, i.strategy]));
    expect(actual).toEqual(EXPECTED_ISLANDS);
  });
});
