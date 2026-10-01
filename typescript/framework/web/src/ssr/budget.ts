/**
 * Client-JS budget gate for the web rendering determinism manifest.
 *
 * The build emits a per-route client-JS budget (`clientJsBytes`) into
 * `.gen/putnami-web-manifest.json`. This module turns those numbers into an
 * enforceable gate: it compares a freshly built manifest against a committed
 * baseline and/or an absolute cap, and reports the routes whose client JS
 * regressed beyond a configurable threshold — so a budget regression can fail
 * CI rather than being hidden by an aggregate package size.
 *
 * Byte counts are deterministic build outputs (unlike render timing), so a
 * committed baseline plus a growth tolerance is a stable, machine-independent
 * gate.
 */

import { formatBytes, type WebManifest } from './manifest';

/** Options controlling the client-JS budget gate. */
export interface BudgetOptions {
  /**
   * Maximum allowed growth of a route's client-JS budget relative to the
   * baseline, as a percentage (e.g. `10` allows +10%). A route whose client JS
   * grows by more than this fails the gate; decreases always pass. Defaults to
   * `0` (no growth tolerated) when omitted. Has no effect without a baseline.
   */
  maxGrowthPercent?: number;
  /**
   * Absolute cap on any single route's client JS, in bytes. A route at or above
   * this fails regardless of the baseline. Omitted = no absolute cap.
   */
  maxBytes?: number;
}

export type BudgetViolationKind = 'regression' | 'over-budget';

/** A single route that breached the budget. */
export interface BudgetViolation {
  route: string;
  kind: BudgetViolationKind;
  /** The route's client-JS budget in the current manifest, in bytes. */
  currentBytes: number;
  /**
   * The ceiling that was exceeded — the absolute cap for `over-budget`, the
   * baseline-derived allowance for `regression`.
   */
  limitBytes: number;
  /** Baseline client JS for the route, when a baseline entry exists. */
  baselineBytes?: number;
}

export interface BudgetReport {
  violations: BudgetViolation[];
  /** Number of routes evaluated against the budget. */
  routesChecked: number;
  /** Whether a baseline manifest was supplied (regression checks ran). */
  baselineUsed: boolean;
}

/**
 * Evaluate a manifest's per-route client-JS budgets.
 *
 * For every route in {@link current}: it fails when the route is at/above an
 * absolute {@link BudgetOptions.maxBytes} cap, or — when a {@link baseline} entry
 * exists — when its client JS grew past `baseline * (1 + maxGrowthPercent/100)`.
 * Each route yields at most one violation; the absolute cap takes precedence.
 */
export function checkBudget(current: WebManifest, options: BudgetOptions, baseline?: WebManifest): BudgetReport {
  const maxGrowthPercent = options.maxGrowthPercent ?? 0;
  const baselineByRoute = new Map((baseline?.routes ?? []).map((r) => [r.route, r]));
  const violations: BudgetViolation[] = [];

  for (const route of current.routes) {
    const before = baselineByRoute.get(route.route);

    if (options.maxBytes !== undefined && route.clientJsBytes >= options.maxBytes) {
      violations.push({
        route: route.route,
        kind: 'over-budget',
        currentBytes: route.clientJsBytes,
        limitBytes: options.maxBytes,
        ...(before ? { baselineBytes: before.clientJsBytes } : {}),
      });
      continue;
    }

    if (before) {
      const limit = Math.floor(before.clientJsBytes * (1 + maxGrowthPercent / 100));
      if (route.clientJsBytes > limit) {
        violations.push({
          route: route.route,
          kind: 'regression',
          currentBytes: route.clientJsBytes,
          limitBytes: limit,
          baselineBytes: before.clientJsBytes,
        });
      }
    }
  }

  return { violations, routesChecked: current.routes.length, baselineUsed: baseline !== undefined };
}

export function hasBudgetViolations(report: BudgetReport): boolean {
  return report.violations.length > 0;
}

/** Render a budget report for CI output / human review. */
export function formatBudgetReport(report: BudgetReport): string {
  if (report.violations.length === 0) {
    const how = report.baselineUsed ? 'vs baseline' : 'against cap';
    return `Client-JS budget OK — ${report.routesChecked} route(s) within budget (${how}).`;
  }

  const lines: string[] = ['Client-JS budget exceeded:'];
  for (const v of report.violations) {
    if (v.kind === 'over-budget') {
      lines.push(`✗ ${v.route}  ${formatBytes(v.currentBytes)} ≥ cap ${formatBytes(v.limitBytes)}`);
    } else {
      const delta = v.baselineBytes !== undefined ? v.currentBytes - v.baselineBytes : 0;
      const baselineNote =
        v.baselineBytes !== undefined ? ` (baseline ${formatBytes(v.baselineBytes)}, +${formatBytes(delta)})` : '';
      lines.push(`✗ ${v.route}  ${formatBytes(v.currentBytes)} > limit ${formatBytes(v.limitBytes)}${baselineNote}`);
    }
  }
  return lines.join('\n');
}
