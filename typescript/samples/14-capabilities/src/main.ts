import {
  application,
  type HealthChecker,
  type LifecycleContribution,
  type LifecycleContributor,
  type Plugin,
  type RequiredCapabilityContributor,
} from '@putnami/application';
import { type MigrationContributor, type MigrationSource, KindSQL } from '@putnami/migration';
import { Config, configToken, Sensitive } from '@putnami/runtime';

/**
 * Capability-manifest proof workload.
 *
 * The app itself does nothing at runtime — it exists to compose every
 * capability kind the built-in capabilities producer can collect, so
 * `app.build()` emits a representative `.gen/schema/capabilities.json`:
 *
 *   - a config block with a sensitive field,
 *   - a migration bound to a datasource (which also yields the infra requirement),
 *   - a health probe and a readiness probe,
 *   - a start/stop lifecycle hook.
 *
 * The emitted bytes are pinned by `test/capabilities.test.ts` against the
 * committed golden — the TypeScript half of the cross-language contract with
 * the Go emitter.
 */

/** A config block whose `password` field is sensitive. */
export const DatabaseConfig = Config('database.default', {
  url: String,
  password: Sensitive(String),
});

// Register the workload-owned block through the same config registry used by
// real endpoint injections and provideConfig().
configToken(DatabaseConfig);

/** A migration source bound to the `default` datasource (drives migrations + infra). */
const iamMigrations: MigrationSource = {
  kind: KindSQL,
  namespace: 'iam',
  infraDatabase: () => ({ name: 'default', engine: 'postgres', schemas: ['identity'] }),
};

/** Contributes the migration source. */
const migrationPlugin: Plugin & MigrationContributor & RequiredCapabilityContributor = {
  migrationSources: () => [iamMigrations],
  requiredCapabilities: () => [{ name: 'sql', requires: ['datasource', 'migration', 'readiness'] }],
};

/** Feeds /healthz. */
const healthPlugin: Plugin & HealthChecker = {
  name: 'diskSpace',
  checkHealth: async () => {},
};

/** A start/stop lifecycle hook. */
const lifecyclePlugin: Plugin & LifecycleContributor = {
  lifecycleContributions: (): LifecycleContribution[] => [
    { name: 'connectionPool', phase: 'starter' },
    { name: 'connectionPool', phase: 'stopper' },
  ],
};

export const app = () => {
  const result = application()
    .feature({
      id: 'capabilities/source-bound-manifest',
      name: 'Indexed capability provenance',
      outcome: 'Indexers can resolve exact native capability provenance without volatile manifest hashes',
      owner: 'frameworks',
    })
    .use(migrationPlugin)
    .use(healthPlugin)
    .use(lifecyclePlugin);
  // Exercise Module.collectHealthContributions(), the DI-injected probe path
  // used by application modules in addition to checker plugins.
  result.health.contributeReadiness('primaryDatabase', [], async () => {});
  return result;
};
