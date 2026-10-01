import { afterEach, describe, expect, it } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { application, type Plugin } from '@putnami/application';
import { type SQLSource, sqlSourceInline } from '@putnami/database';
import { resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { analytics } from '../src/server/analytics.plugin';
import type { AnalyticsOptions } from '../src/server/analytics.config';
import { MIGRATION_NAMESPACE } from '../src/server/sink/migrations';
import { restoreProjectRoot, TEST_SECRET, useTempProjectRoot } from './utils/runtime';

const FIXTURES = join(import.meta.dir, 'fixtures', 'migration-bundle');
const FEATURE = 'typescript/web-analytics-collection';
const REQUIREMENT = 'the-tables-follow-the-datasource-schema';

interface BundleOperation {
  namespace: string;
  schema?: string;
}

/** A stand-in for `sql()`: the probe name, the infra member, and the declared primary. */
function fakeSql(primaryDatasource?: { name: string; schema?: string }): Plugin {
  return { name: 'database', designInfraRequirements: () => [], primaryDatasource } as unknown as Plugin;
}

/** A workload plugin whose own migrations put `datasource` in `schema`. */
function workloadMigrations(datasource: string, schema: string): Plugin & { migrationSources(): SQLSource[] } {
  return {
    name: 'workload-migrations',
    migrationSources: () => [
      sqlSourceInline({
        namespace: 'app',
        datasource: { name: datasource, schema },
        definitions: [{ name: '001_create_campaign', sql: 'CREATE TABLE campaign (id uuid PRIMARY KEY);' }],
      }),
    ],
  };
}

/**
 * Runs the build the way the build-generate hook does, and returns the text of
 * the bundle it wrote.
 */
let root = '';

async function buildBundle(plugins: Plugin[]): Promise<string> {
  process.env['CONFIG_DATA'] = JSON.stringify({ analytics: { secret: TEST_SECRET } });
  root = useTempProjectRoot();
  const app = application();
  for (const plugin of plugins) {
    app.use(plugin);
  }
  await app.build({ projectName: 'site', publishCapabilityManifest: false });
  return readFileSync(join(root, '.gen', 'migration-bundle', 'bundle.json'), 'utf8');
}

/** The schema every analytics operation of a bundle carries. */
function analyticsSchemas(bundle: string): (string | undefined)[] {
  const { operations } = JSON.parse(bundle) as { operations: BundleOperation[] };
  return [...new Set(operations.filter((op) => op.namespace === MIGRATION_NAMESPACE).map((op) => op.schema))];
}

afterEach(() => {
  process.env['CONFIG_DATA'] = undefined;
  restoreProjectRoot();
  resetConfigLoader();
});

describe('the analytics migration bundle', () => {
  // The two goldens are what the build wrote before the schema followed the
  // datasource. A workload that declares no schema must not see its bundle
  // digest move.
  for (const [retentionMode, golden] of [
    ['sweep', 'public-sweep.json'],
    ['pg_cron', 'public-pg-cron.json'],
  ] as const) {
    specTest(
      `keeps a workload without a schema in public, byte for byte (${retentionMode})`,
      { feature: FEATURE, requirement: REQUIREMENT, check: 'a-workload-without-a-schema-keeps-its-bundle' },
      async () => {
        const bundle = await buildBundle([fakeSql({ name: 'analytics' }), analytics({ retentionMode })]);

        expect(bundle).toBe(readFileSync(join(FIXTURES, golden), 'utf8'));
      },
    );
  }

  specTest(
    "follows the schema the workload's own sources declare on the datasource",
    { feature: FEATURE, requirement: REQUIREMENT, check: 'the-tables-follow-the-schema-the-datasource-declares' },
    async () => {
      // Composed after analytics on purpose: the schema is read when the
      // sources are, so composition order does not matter.
      const bundle = await buildBundle([
        fakeSql(),
        analytics({ datasource: 'marketing' }),
        workloadMigrations('marketing', 'marketing'),
      ]);

      expect(analyticsSchemas(bundle)).toEqual(['marketing']);
    },
  );

  it('follows the schema sql() declares for its primary datasource', async () => {
    const bundle = await buildBundle([
      fakeSql({ name: 'marketing', schema: 'marketing' }),
      analytics({ datasource: 'marketing' }),
    ]);

    expect(analyticsSchemas(bundle)).toEqual(['marketing']);
  });

  it('stays in public when the primary schema is not a single lowercase name', async () => {
    // A comma list is a valid search path for the datasource, and a mixed-case
    // name folds to another schema: neither is a place for the tables, so they
    // stay where they have always been.
    for (const schema of ['app, public', 'Marketing']) {
      const bundle = await buildBundle([
        fakeSql({ name: 'marketing', schema }),
        analytics({ datasource: 'marketing' }),
      ]);

      expect(analyticsSchemas(bundle)).toEqual(['public']);
      restoreProjectRoot();
      resetConfigLoader();
    }
  });

  it('ignores a schema declared for another datasource', async () => {
    const bundle = await buildBundle([
      fakeSql({ name: 'identity', schema: 'identity_auth' }),
      workloadMigrations('identity', 'identity_auth'),
      analytics({ datasource: 'marketing' }),
    ]);

    expect(analyticsSchemas(bundle)).toEqual(['public']);
  });

  it('puts the tables in analytics.schema when it is set', async () => {
    const options: AnalyticsOptions = { datasource: 'marketing', schema: 'analytics', retentionMode: 'pg_cron' };
    const bundle = await buildBundle([fakeSql({ name: 'marketing', schema: 'marketing' }), analytics(options)]);

    expect(analyticsSchemas(bundle)).toEqual(['analytics']);
  });

  specTest(
    'fails the build when analytics.schema contradicts the datasource',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-contradicting-schema-fails-the-build' },
    async () => {
      const build = buildBundle([
        fakeSql(),
        workloadMigrations('marketing', 'marketing'),
        analytics({ datasource: 'marketing', schema: 'public' }),
      ]);

      // The runner's own message, raised where the bundle is assembled instead
      // of where it is applied.
      await expect(build).rejects.toThrow(
        'datasource "marketing" has conflicting schemas across sources: "marketing" and "public" (namespaces "app" and "putnami-analytics")',
      );
      // Refused before the infra fragment names either schema.
      expect(existsSync(join(root, '.gen', 'infra', 'migration.json'))).toBe(false);
    },
  );
});
