import type { ConfigDefinition } from './config';

/**
 * Implemented by plugins that own one or more config blocks. The config
 * extraction phase walks every `ConfigContributor` in the composed plugin tree
 * and aggregates their definitions into the workload's published config schema,
 * so a library can own its config — and the secrets its sensitive fields
 * declare — and any workload that composes it publishes them transitively.
 *
 * This is the config dual of `MigrationContributor`: the same "compose the
 * plugin, get its contribution in the build artifact" wiring, with no
 * per-workload restatement.
 *
 * @example
 * ```typescript
 * export const CoreConfig = Config('core', {
 *   auth: { clientSecret: Sensitive(String) },
 * });
 *
 * class CorePlugin implements Plugin, ConfigContributor {
 *   name = 'core';
 *   configDefinitions(): ConfigDefinition[] {
 *     return [CoreConfig];
 *   }
 * }
 * ```
 */
export interface ConfigContributor {
  configDefinitions(): ConfigDefinition[];
}

/**
 * Runtime type guard for `ConfigContributor`. The application uses this to
 * filter the plugin list when aggregating dependency-owned config.
 */
export function isConfigContributor(value: unknown): value is ConfigContributor {
  return (
    typeof value === 'object' &&
    value !== null &&
    'configDefinitions' in value &&
    typeof (value as ConfigContributor).configDefinitions === 'function'
  );
}
