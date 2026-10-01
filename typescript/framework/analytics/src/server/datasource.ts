import { useRawConfigSection } from '@putnami/runtime';

/**
 * Resolves the datasource the analytics tables live in, without opening a
 * connection (`database(name)` connects eagerly, so it can never be used as a
 * presence check).
 *
 * A configured name is honoured when it is declared in the `database` config
 * section, in a merged `database.databases` binding, or in the
 * `DATABASE_BINDINGS` environment binding. Otherwise the resolution falls back
 * to `default`.
 *
 * @param configured - The `analytics.datasource` value.
 * @returns The datasource name to use.
 */
export function resolveAnalyticsDatasource(configured: string): string {
  if (configured === 'default') {
    return 'default';
  }
  const section = useRawConfigSection('database');
  const inConfig = typeof section?.[configured] === 'object' && section?.[configured] !== null;
  const merged = section?.['databases'] as Record<string, unknown> | undefined;
  const inMergedBinding = typeof merged?.[configured] === 'object' && merged?.[configured] !== null;
  let inEnvBinding = false;
  const raw = process.env['DATABASE_BINDINGS']?.trim();
  if (raw) {
    try {
      const parsed = JSON.parse(raw) as { databases?: Record<string, unknown> } | null;
      inEnvBinding = typeof parsed?.databases?.[configured] === 'object' && parsed?.databases?.[configured] !== null;
    } catch {
      inEnvBinding = false;
    }
  }
  return inConfig || inMergedBinding || inEnvBinding ? configured : 'default';
}
