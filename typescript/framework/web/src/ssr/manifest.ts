import type { IslandStrategy } from '../client/island/island-types';
import type { HydrationMode, RenderMode, StaticRevalidate } from './static';

/** Manifest entry for a single route. */
export interface ManifestRoute {
  /** React-Router style route, e.g. `/tasks/:id`. */
  route: string;
  /** When the route renders: `ssr` | `ssg` | `isr`. */
  mode: RenderMode;
  /** What hydrates: `full` | `islands` | `none`. */
  hydration: HydrationMode;
  /** Client JavaScript budget for this route, in bytes (gzipped). */
  clientJsBytes: number;
  /** ISR revalidation directives, when `mode` is `isr`. */
  revalidate?: StaticRevalidate;
  /**
   * For static routes (`ssg`/`isr`): whether static-means-static is **proven
   * from the DI dependency graph** (`true`) or relies on the runtime
   * `StaticRenderViolation` guard as a hybrid backstop (`false`). Omitted for
   * `ssr` routes, where the distinction does not apply.
   */
  diProvenStatic?: boolean;
}

/** Manifest entry for a single island boundary. */
export interface ManifestIsland {
  id: string;
  strategy: IslandStrategy;
}

/** The per-build web rendering manifest — diffable in CI. */
export interface WebManifest {
  version: 1;
  routes: ManifestRoute[];
  islands: ManifestIsland[];
  bundles: {
    /** Total gzipped size of the full-page hydration bundle. */
    hydrateBytes: number;
    /** Total gzipped size of the islands runtime + island chunks. */
    islandsBytes: number;
  };
  totals: {
    routes: number;
    ssr: number;
    ssg: number;
    isr: number;
    islands: number;
    /** Largest single-route client-JS budget across all routes. */
    maxClientJsBytes: number;
  };
}

export interface ManifestInput {
  routes: Array<{
    route: string;
    mode: RenderMode;
    hydration: HydrationMode;
    revalidate?: StaticRevalidate;
    diProvenStatic?: boolean;
  }>;
  islands: ManifestIsland[];
  hydrateBytes: number;
  islandsBytes: number;
}

/**
 * Assemble a deterministic {@link WebManifest} from collected build data.
 * Entries are sorted so the JSON output is stable and diffable.
 */
export function buildManifest(input: ManifestInput): WebManifest {
  const clientJsFor = (hydration: HydrationMode): number => {
    switch (hydration) {
      case 'full':
        return input.hydrateBytes;
      case 'islands':
        return input.islandsBytes;
      default:
        return 0;
    }
  };

  const routes: ManifestRoute[] = input.routes
    .map((r) => ({
      route: r.route,
      mode: r.mode,
      hydration: r.hydration,
      clientJsBytes: clientJsFor(r.hydration),
      ...(r.revalidate ? { revalidate: r.revalidate } : {}),
      ...(r.diProvenStatic !== undefined ? { diProvenStatic: r.diProvenStatic } : {}),
    }))
    .sort((a, b) => a.route.localeCompare(b.route));

  const islands = [...input.islands].sort((a, b) => a.id.localeCompare(b.id));

  const totals = {
    routes: routes.length,
    ssr: routes.filter((r) => r.mode === 'ssr').length,
    ssg: routes.filter((r) => r.mode === 'ssg').length,
    isr: routes.filter((r) => r.mode === 'isr').length,
    islands: islands.length,
    maxClientJsBytes: routes.reduce((max, r) => Math.max(max, r.clientJsBytes), 0),
  };

  return {
    version: 1,
    routes,
    islands,
    bundles: { hydrateBytes: input.hydrateBytes, islandsBytes: input.islandsBytes },
    totals,
  };
}

/** Serialize a manifest to stable, newline-terminated JSON. */
export function serializeManifest(manifest: WebManifest): string {
  return `${JSON.stringify(manifest, null, 2)}\n`;
}

/** Format a byte count for human-readable manifest / budget output. */
export function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B';
  if (bytes < 1024) return `${bytes} B`;
  return `${(bytes / 1024).toFixed(1)} KB`;
}

function formatDiProven(value: boolean | undefined): string {
  if (value === undefined) return 'n/a';
  return value ? 'proven' : 'hybrid';
}

/** Render a human-readable table of routes for build logs. */
export function formatManifestTable(manifest: WebManifest): string {
  const header = ['Route', 'Mode', 'Hydration', 'Client JS'];
  const rows = manifest.routes.map((r) => [r.route, r.mode.toUpperCase(), r.hydration, formatBytes(r.clientJsBytes)]);
  const all = [header, ...rows];
  const widths = header.map((_, col) => Math.max(...all.map((row) => row[col].length)));
  const line = (row: string[]) => row.map((cell, col) => cell.padEnd(widths[col])).join('  ');
  return [line(header), widths.map((w) => '-'.repeat(w)).join('  '), ...rows.map(line)].join('\n');
}

// ---------------------------------------------------------------------------
// Diffing — for the CI manifest-diff command
// ---------------------------------------------------------------------------

export interface ManifestRouteChange {
  route: string;
  before: ManifestRoute;
  after: ManifestRoute;
}

export interface ManifestDiff {
  added: ManifestRoute[];
  removed: ManifestRoute[];
  changed: ManifestRouteChange[];
}

/** Compute the route-level differences between two manifests. */
export function diffManifests(prev: WebManifest, next: WebManifest): ManifestDiff {
  const prevByRoute = new Map(prev.routes.map((r) => [r.route, r]));
  const nextByRoute = new Map(next.routes.map((r) => [r.route, r]));

  const added = next.routes.filter((r) => !prevByRoute.has(r.route));
  const removed = prev.routes.filter((r) => !nextByRoute.has(r.route));
  const changed: ManifestRouteChange[] = [];

  for (const after of next.routes) {
    const before = prevByRoute.get(after.route);
    if (!before) continue;
    if (
      before.mode !== after.mode ||
      before.hydration !== after.hydration ||
      before.clientJsBytes !== after.clientJsBytes ||
      before.diProvenStatic !== after.diProvenStatic
    ) {
      changed.push({ route: after.route, before, after });
    }
  }

  return { added, removed, changed };
}

export function hasManifestChanges(diff: ManifestDiff): boolean {
  return diff.added.length > 0 || diff.removed.length > 0 || diff.changed.length > 0;
}

/** Render a manifest diff for human review / CI output. */
export function formatManifestDiff(diff: ManifestDiff): string {
  if (!hasManifestChanges(diff)) {
    return 'No web rendering changes.';
  }
  const lines: string[] = [];
  for (const r of diff.added) {
    lines.push(`+ ${r.route}  (${r.mode}, ${r.hydration}, ${formatBytes(r.clientJsBytes)})`);
  }
  for (const r of diff.removed) {
    lines.push(`- ${r.route}  (was ${r.mode}, ${r.hydration})`);
  }
  for (const c of diff.changed) {
    const parts: string[] = [];
    if (c.before.mode !== c.after.mode) parts.push(`mode ${c.before.mode} → ${c.after.mode}`);
    if (c.before.hydration !== c.after.hydration) {
      parts.push(`hydration ${c.before.hydration} → ${c.after.hydration}`);
    }
    if (c.before.clientJsBytes !== c.after.clientJsBytes) {
      parts.push(`client JS ${formatBytes(c.before.clientJsBytes)} → ${formatBytes(c.after.clientJsBytes)}`);
    }
    if (c.before.diProvenStatic !== c.after.diProvenStatic) {
      parts.push(`di-proven ${formatDiProven(c.before.diProvenStatic)} → ${formatDiProven(c.after.diProvenStatic)}`);
    }
    lines.push(`~ ${c.route}  (${parts.join(', ')})`);
  }
  return lines.join('\n');
}
