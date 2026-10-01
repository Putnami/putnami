import type { MigrationSource } from './types';

/**
 * Plugins that own migrations of any kind implement this interface
 * alongside `Plugin`. The application's lifecycle walks every
 * `MigrationContributor` between the warmup and migrate phases,
 * calling `migrationSources()` to collect their contributions.
 *
 * Returning multiple sources is the supported stacking pattern: a
 * single feature can mix codegen-backed SQL sources with inline
 * definitions, and can contribute to several kinds (SQL today; GCS /
 * document / cache / event-topic later) in the same plugin.
 *
 * @example
 * ```typescript
 * import { iamMigrations } from './.gen/migrations.gen';
 *
 * class IamPlugin implements Plugin, MigrationContributor {
 *   name = 'iam';
 *   migrationSources(): MigrationSource[] {
 *     return [sqlSourceInline({
 *       namespace: 'iam',
 *       datasource: { name: 'default', schema: 'identity' },
 *       definitions: iamMigrations,
 *     })];
 *   }
 * }
 * ```
 */
export interface MigrationContributor {
  migrationSources(): MigrationSource[];
}

/**
 * Runtime type guard for `MigrationContributor`. The application
 * lifecycle uses this to filter the plugin list during the
 * `collectMigrationSources` step.
 */
export function isMigrationContributor(value: unknown): value is MigrationContributor {
  return (
    typeof value === 'object' &&
    value !== null &&
    'migrationSources' in value &&
    typeof (value as MigrationContributor).migrationSources === 'function'
  );
}
