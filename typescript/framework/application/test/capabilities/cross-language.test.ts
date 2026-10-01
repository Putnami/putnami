import { afterEach, describe, expect, it } from 'bun:test';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { type MigrationContributor, type MigrationSource, KindSQL } from '@putnami/migration';
import { Config, type ConfigContributor, Sensitive } from '@putnami/runtime';
import { application } from '../../src/application';
import type { Plugin } from '../../src/application/module.types';
import {
  type CapabilitiesProducerOptions,
  createCapabilitiesProducer,
} from '../../src/capabilities/capabilities.producer';
import { parseCapabilityManifestDocument, serializeCapabilityManifestV2 } from '../../src/capabilities/manifest.v2';
import type { LifecycleContributor } from '../../src/capabilities/lifecycle';
import type { CapabilityInventoryContributor } from '../../src/capabilities/inventory';
import type { CapabilityProvenanceContributor } from '../../src/capabilities/provenance';
import type { RequiredCapabilityContributor } from '../../src/capabilities/requirement';
import type { HealthChecker, ReadinessChecker } from '../../src/platform/checker';

const EQUIVALENCE_FIXTURE_PATH = join(
  __dirname,
  '../../../../../protocols/capabilities/fixtures/v2/equivalence/capabilities-v2.golden.json',
);
const PROJECT = 'go.putnami.dev/examples/capabilities-proof';
const GO_PACKAGE = 'go.putnami.dev/app';
const SOURCE_BINDING = `source-v1:sha256:${'0'.repeat(64)}`;
const roots: string[] = [];

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function producerOptions(): CapabilitiesProducerOptions {
  const projectRoot = mkdtempSync(join(tmpdir(), 'putnami-capabilities-equivalence-'));
  roots.push(projectRoot);
  mkdirSync(join(projectRoot, '.gen', 'schema'), { recursive: true });
  writeFileSync(join(projectRoot, '.gen', 'schema', 'api.proto'), 'syntax = "proto3";\n');
  writeFileSync(join(projectRoot, '.gen', 'schema', 'openapi.json'), '{}\n');
  mkdirSync(join(projectRoot, '.gen', 'infra'), { recursive: true });
  writeFileSync(
    join(projectRoot, '.gen', 'infra', 'migration.json'),
    `${JSON.stringify({ protocolVersion: 2, databases: [{ name: 'default', engine: 'postgres', schemas: ['public'] }] }, null, 2)}\n`,
  );
  writeFileSync(
    join(projectRoot, '.gen', 'infra', 'inventory.json'),
    `${JSON.stringify(
      {
        protocolVersion: 2,
        events: { publishes: ['user.created'], subscribes: ['user.updated'] },
        storage: [{ name: 'avatars' }],
        secrets: ['oauth.client-secret'],
        scheduledJobs: [{ name: 'cleanup', schedule: '0 2 * * *' }],
      },
      null,
      2,
    )}\n`,
  );
  return {
    projectRoot,
    project: PROJECT,
    capabilityPackages: [
      {
        package: PROJECT,
        version: '0.1.0',
        evidencePath: 'example/iam/go.mod',
        sourceRoot: 'example/iam',
        sourceBinding: SOURCE_BINDING,
      },
      {
        package: GO_PACKAGE,
        version: '1.4.0',
        evidencePath: 'example/iam/db.go',
        sourceRoot: 'example/iam',
        sourceBinding: SOURCE_BINDING,
      },
    ],
    // Keep the test independent of the process-global config registry: its
    // equivalent config is contributed by the actual plugin tree below.
    registeredConfigDefinitions: () => [],
  };
}

function provenance(): { package: string } {
  return { package: GO_PACKAGE };
}

describe('cross-language equivalence', () => {
  it('actual v2 producer is canonical and the TypeScript serializer reproduces shared Go bytes', async () => {
    const DatabaseConfig = Config('database.default', {
      url: String,
      password: Sensitive(String),
    });
    const configPlugin: Plugin & ConfigContributor & CapabilityProvenanceContributor = {
      capabilityProvenance: provenance,
      configDefinitions: () => [DatabaseConfig],
    };

    const iam: MigrationSource = {
      kind: KindSQL,
      namespace: 'iam',
      infraDatabase: () => ({ name: 'default', engine: 'postgres' }),
    };
    const migrationPlugin: Plugin & MigrationContributor & CapabilityProvenanceContributor = {
      capabilityProvenance: provenance,
      migrationSources: () => [iam],
    };
    const readinessPlugin: Plugin & ReadinessChecker & CapabilityProvenanceContributor = {
      name: 'primaryDatabase',
      capabilityProvenance: provenance,
      checkReadiness: async () => {},
    };
    const healthPlugin: Plugin & HealthChecker & CapabilityProvenanceContributor = {
      name: 'diskSpace',
      capabilityProvenance: provenance,
      checkHealth: async () => {},
    };
    const lifecyclePlugin: Plugin & LifecycleContributor & CapabilityProvenanceContributor = {
      capabilityProvenance: provenance,
      // Deliberately unsorted: the producer must canonicalize stopper/starter.
      lifecycleContributions: () => [
        { name: 'connectionPool', phase: 'stopper' },
        { name: 'connectionPool', phase: 'starter' },
      ],
    };
    const requirementPlugin: Plugin & RequiredCapabilityContributor & CapabilityProvenanceContributor = {
      capabilityProvenance: provenance,
      // Deliberately unsorted: the producer canonicalizes the closed enum.
      requiredCapabilities: () => [
        {
          name: 'sql',
          requires: [
            'readiness',
            'package',
            'lifecycle',
            'infra',
            'health',
            'schema',
            'migration',
            'discoverer',
            'datasource',
            'config',
          ],
        },
      ],
    };
    const inventoryPlugin: Plugin & CapabilityInventoryContributor & CapabilityProvenanceContributor = {
      capabilityProvenance: provenance,
      capabilityInventory: () => ({
        schemas: [
          { name: 'api-proto', kind: 'proto', path: 'schema/api.proto' },
          { name: 'listUsers', kind: 'route', path: '/api/users' },
          { name: 'openapi', kind: 'openapi', path: 'schema/openapi.json' },
        ],
      }),
    };

    const app = application()
      .use(configPlugin)
      .use(migrationPlugin)
      .use(readinessPlugin)
      .use(healthPlugin)
      .use(lifecyclePlugin)
      .use(requirementPlugin)
      .use(inventoryPlugin);
    const options = producerOptions();
    const producer = createCapabilitiesProducer(options);
    const manifestPath = join(options.projectRoot as string, '.gen', 'schema', 'capabilities.json');
    const shared = readFileSync(EQUIVALENCE_FIXTURE_PATH, 'utf8');
    const sharedDocument = parseCapabilityManifestDocument(shared);
    if (sharedDocument.protocolVersion !== 2) throw new Error('expected shared v2 fixture');
    expect(serializeCapabilityManifestV2(sharedDocument.manifest)).toBe(shared);

    let first: string | undefined;
    for (let iteration = 0; iteration < 5; iteration++) {
      await producer.postGenerate?.(app, {});
      const bytes = readFileSync(manifestPath, 'utf8');
      const document = parseCapabilityManifestDocument(bytes);
      expect(document.protocolVersion).toBe(2);
      if (document.protocolVersion !== 2) throw new Error('expected producer v2 output');
      expect(serializeCapabilityManifestV2(document.manifest)).toBe(bytes);
      first ??= bytes;
      expect(bytes).toBe(first);
    }
  });
});
