import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { checkBudget, formatBudgetReport, hasBudgetViolations } from '../../src/ssr/budget';
import { buildManifest, type WebManifest } from '../../src/ssr/manifest';

/** Build a manifest with explicit per-route client-JS budgets. */
function manifestWith(routeBytes: Record<string, number>): WebManifest {
  // `clientJsBytes` derives from hydration mode + bundle sizes, so model each
  // route as a `full`-hydration route whose bytes equal the desired figure.
  const routes = Object.keys(routeBytes).map((route) => ({ route, mode: 'ssr' as const, hydration: 'full' as const }));
  // buildManifest assigns every `full` route the same hydrateBytes, so build one
  // manifest per distinct size and stitch the routes back together.
  const base = buildManifest({ routes, islands: [], hydrateBytes: 0, islandsBytes: 0 });
  return {
    ...base,
    routes: base.routes.map((r) => ({ ...r, clientJsBytes: routeBytes[r.route] })),
  };
}

describe('checkBudget — regression vs baseline', () => {
  it('passes when client JS is unchanged', () => {
    const baseline = manifestWith({ '/': 50_000, '/tasks': 80_000 });
    const current = manifestWith({ '/': 50_000, '/tasks': 80_000 });
    const report = checkBudget(current, {}, baseline);
    expect(hasBudgetViolations(report)).toBe(false);
    expect(report.routesChecked).toBe(2);
    expect(report.baselineUsed).toBe(true);
  });

  it('passes when client JS decreases', () => {
    const baseline = manifestWith({ '/tasks': 80_000 });
    const current = manifestWith({ '/tasks': 60_000 });
    expect(hasBudgetViolations(checkBudget(current, {}, baseline))).toBe(false);
  });

  specTest(
    'fails when growth exceeds zero tolerance',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'render-manifest',
      check: 'the-budget-gate-rejects-growth-beyond-the-tolerance',
    },
    () => {
      const baseline = manifestWith({ '/tasks': 80_000 });
      const current = manifestWith({ '/tasks': 80_001 });
      const report = checkBudget(current, {}, baseline);
      expect(hasBudgetViolations(report)).toBe(true);
      expect(report.violations[0]).toMatchObject({
        route: '/tasks',
        kind: 'regression',
        currentBytes: 80_001,
        baselineBytes: 80_000,
        limitBytes: 80_000,
      });
    },
  );

  it('tolerates growth within the configured threshold', () => {
    const baseline = manifestWith({ '/tasks': 100_000 });
    const within = manifestWith({ '/tasks': 110_000 }); // +10%
    const over = manifestWith({ '/tasks': 110_001 }); // just over +10%
    expect(hasBudgetViolations(checkBudget(within, { maxGrowthPercent: 10 }, baseline))).toBe(false);
    expect(hasBudgetViolations(checkBudget(over, { maxGrowthPercent: 10 }, baseline))).toBe(true);
  });

  it('ignores routes absent from the baseline when only checking regressions', () => {
    const baseline = manifestWith({ '/tasks': 80_000 });
    const current = manifestWith({ '/tasks': 80_000, '/new': 200_000 });
    expect(hasBudgetViolations(checkBudget(current, {}, baseline))).toBe(false);
  });
});

describe('checkBudget — absolute cap', () => {
  specTest(
    'fails any route at or above the cap, even without a baseline',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'render-manifest',
      check: 'the-budget-gate-rejects-a-route-at-or-above-the-absolute-cap',
    },
    () => {
      const current = manifestWith({ '/': 50_000, '/tasks': 120_000 });
      const report = checkBudget(current, { maxBytes: 100_000 });
      expect(report.baselineUsed).toBe(false);
      expect(report.violations).toHaveLength(1);
      expect(report.violations[0]).toMatchObject({ route: '/tasks', kind: 'over-budget', limitBytes: 100_000 });
    },
  );

  specTest(
    'reports the absolute cap in preference to a regression for the same route',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'render-manifest',
      check: 'the-absolute-cap-is-reported-in-preference-to-a-regression',
    },
    () => {
      const baseline = manifestWith({ '/tasks': 90_000 });
      const current = manifestWith({ '/tasks': 120_000 });
      const report = checkBudget(current, { maxBytes: 100_000, maxGrowthPercent: 0 }, baseline);
      expect(report.violations).toHaveLength(1);
      expect(report.violations[0]).toMatchObject({ kind: 'over-budget', baselineBytes: 90_000 });
    },
  );
});

describe('formatBudgetReport', () => {
  it('summarizes a passing report', () => {
    const report = checkBudget(manifestWith({ '/': 10 }), {}, manifestWith({ '/': 10 }));
    expect(formatBudgetReport(report)).toContain('Client-JS budget OK');
    expect(formatBudgetReport(report)).toContain('1 route(s)');
  });

  it('lists each violation with the breached limit', () => {
    const baseline = manifestWith({ '/tasks': 80_000 });
    const current = manifestWith({ '/tasks': 100_000 });
    const out = formatBudgetReport(checkBudget(current, {}, baseline));
    expect(out).toContain('Client-JS budget exceeded');
    expect(out).toContain('/tasks');
    expect(out).toContain('baseline');
  });
});
