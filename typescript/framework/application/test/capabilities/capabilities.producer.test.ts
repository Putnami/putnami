import { afterEach, describe, expect, it } from 'bun:test';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { Config, type ConfigContributor, getRegisteredConfigDefinitions } from '@putnami/runtime';
import type { MigrationContributor, MigrationSource } from '@putnami/migration';
import { application } from '../../src/application';
import { runGenerate, runPostGenerate } from '../../src/application/app-build';
import type { Plugin } from '../../src/application/module.types';
import {
  type CapabilitiesProducerOptions,
  createCapabilitiesProducer,
} from '../../src/capabilities/capabilities.producer';
import type { CapabilityInventoryContributor } from '../../src/capabilities/inventory';
import type { CapabilityProvenanceContributor } from '../../src/capabilities/provenance';
import type { RequiredCapabilityContributor } from '../../src/capabilities/requirement';
import type { FeatureProof } from '../../src/features/design-graph';
import type { ReadinessChecker } from '../../src/platform/checker';

const roots: string[] = [];

afterEach(() => {
  for (const root of roots.splice(0)) rmSync(root, { recursive: true, force: true });
});

function projectRoot(label: string): string {
  const root = mkdtempSync(join(tmpdir(), `putnami-capabilities-${label}-`));
  roots.push(root);
  return root;
}

function manifestPath(root: string): string {
  return join(root, '.gen', 'schema', 'capabilities.json');
}

function evidencePath(root: string): string {
  return join(root, '.gen', 'schema', 'feature-evidence', 'typescript-framework.json');
}

const SOURCE_BINDING = `source-v1:sha256:${'0'.repeat(64)}`;

/**
 * Stamp the declaration site a real application would capture.
 *
 * `getExternalCaller` treats every frame under `typescript/framework/*` as
 * framework-internal, so a `.feature()` call made from this package captures no
 * provenance — correctly, because the framework is not the author. These tests
 * therefore declare the feature normally and then set the provenance the
 * declaring workload would have produced. The natural no-provenance path is
 * covered on its own below.
 */
function stampDeclarationSite(app: ReturnType<typeof application>, path = 'src/main.ts'): void {
  const declared = app.getFeature();
  if (!declared) throw new Error('feature declaration is missing');
  (declared as { provenance?: { path: string; symbol?: string } }).provenance = { path, symbol: 'app' };
}

/**
 * Authored intent is read from the project root, exactly as a real build reads
 * it, so these tests exercise discovery rather than a stubbed catalog.
 */
function writeAuthoredFeatures(root: string): void {
  writeFileSync(
    join(root, 'putnami.features.json'),
    `${JSON.stringify(
      {
        $schema: 'https://putnami.dev/schemas/putnami-features.json',
        protocolVersion: 1,
        namespace: 'capabilities',
        features: [
          {
            id: 'capabilities/typescript-evidence',
            type: 'feature',
            name: 'TypeScript capability evidence',
            outcome: 'A build proves a feature requirement from an exact capability contribution',
            owner: 'capabilities',
            target: 'coded',
            requirements: [{ id: 'implementation', stage: 'coded', evidenceKinds: ['capability'] }],
          },
        ],
      },
      null,
      2,
    )}\n`,
  );
}

function sourceBoundProducer(
  root: string,
  project: string,
  options: Omit<CapabilitiesProducerOptions, 'projectRoot' | 'project' | 'capabilityPackages'> = {},
) {
  return createCapabilitiesProducer({
    ...options,
    projectRoot: root,
    project,
    capabilityPackages: [
      {
        package: project,
        version: '0.1.0',
        evidencePath: 'package.json',
        sourceRoot: '.',
        sourceBinding: SOURCE_BINDING,
      },
    ],
  });
}

describe('capabilities producer', () => {
  it('imports generated route loaders before collecting route-owned config', async () => {
    const root = projectRoot('route-config');
    const fixturePath = join(import.meta.dir, 'fixtures', 'route-loader.ts');
    const loaderPath = join(root, '.gen', 'src', 'api.gen.ts');
    const sourceLoaderPath = join(root, '.gen', 'src', 'sql.gen.ts');
    mkdirSync(join(root, '.gen', 'src'), { recursive: true });
    writeFileSync(loaderPath, `import ${JSON.stringify(fixturePath)};\n`);
    writeFileSync(sourceLoaderPath, 'export const discovered = true;\n');
    const loaderWriter: Plugin = {
      generate: () => ({ exports: { 'api-loader': loaderPath, 'sql-loader': sourceLoaderPath } }),
    };
    const app = application().use(loaderWriter);
    // The registry `configToken()` writes to is process-global, so a definition
    // any other suite in this process registered would land in the manifest and
    // change the exact discoverer list asserted below. Read the live registry —
    // that is the point of the test, the fixture only reaches it by being
    // imported during postGenerate — but scope it to the path this fixture owns.
    const producer = sourceBoundProducer(root, 'route-config-proof', {
      registeredConfigDefinitions: () =>
        getRegisteredConfigDefinitions().filter((definition) => definition.path === 'capabilities.routeOwned'),
    });
    const plugins = [...app.collectPlugins(), { plugin: producer, owner: app }];

    const generated = await runGenerate(plugins);
    expect(existsSync(manifestPath(root))).toBe(false);

    const postGenerated = await runPostGenerate(plugins, generated);
    const manifest = JSON.parse(readFileSync(manifestPath(root), 'utf8')) as {
      configDefinitions?: Array<{ path: string }>;
      schemas?: Array<{
        identity: { ownerProject: string; kind: string; subkind?: string; key: string };
        name: string;
        kind: string;
        path: string;
        provenance: { declaration: { path: string }; artifacts?: Array<{ path: string }> };
      }>;
      discoverers?: Array<{
        identity: { ownerProject: string; kind: string; subkind?: string; key: string };
        name: string;
        kind: string;
        provenance: { project: string; sourceKind: string; declaration: { path: string } };
      }>;
    };
    expect(manifest.configDefinitions?.some((definition) => definition.path === 'capabilities.routeOwned')).toBe(true);
    expect(manifest.schemas).toEqual([
      {
        identity: {
          ownerProject: 'route-config-proof',
          kind: 'schema',
          subkind: 'route',
          key: 'api-loader',
        },
        name: 'api-loader',
        kind: 'route',
        path: '.gen/src/api.gen.ts',
        provenance: {
          project: 'route-config-proof',
          package: 'route-config-proof',
          sourceKind: 'generated',
          declaration: { root: 'project', path: 'package.json' },
          artifacts: [{ root: 'project', path: '.gen/src/api.gen.ts' }],
        },
      },
    ]);
    expect(manifest.discoverers).toEqual([
      {
        identity: {
          ownerProject: 'route-config-proof',
          kind: 'discoverer',
          subkind: 'config',
          key: 'capabilities.routeOwned',
        },
        name: 'capabilities.routeOwned',
        kind: 'config',
        provenance: {
          project: 'route-config-proof',
          package: 'route-config-proof',
          sourceKind: 'framework',
          declaration: { root: 'project', path: 'package.json' },
        },
      },
      {
        identity: {
          ownerProject: 'route-config-proof',
          kind: 'discoverer',
          subkind: 'source',
          key: 'sql-loader',
        },
        name: 'sql-loader',
        kind: 'source',
        provenance: {
          project: 'route-config-proof',
          package: 'route-config-proof',
          sourceKind: 'generated',
          declaration: { root: 'project', path: 'package.json' },
          artifacts: [{ root: 'project', path: '.gen/src/sql.gen.ts' }],
        },
      },
    ]);
    expect(postGenerated.assets?.['schema/capabilities.json']).toBe(manifestPath(root));
  });

  it('removes an existing manifest when duplicate config collection fails', async () => {
    const root = projectRoot('duplicate');
    const finalPath = manifestPath(root);
    mkdirSync(join(root, '.gen', 'schema'), { recursive: true });
    writeFileSync(finalPath, 'previous-manifest\n');

    const first = Config('duplicate.path', { first: String });
    const second = Config('duplicate.path', { second: String });
    const firstPlugin: Plugin & ConfigContributor = { configDefinitions: () => [first] };
    const secondPlugin: Plugin & ConfigContributor = { configDefinitions: () => [second] };
    const app = application().use(firstPlugin).use(secondPlugin);
    const producer = sourceBoundProducer(root, 'duplicate-proof', {
      registeredConfigDefinitions: () => [],
    });

    await expect(producer.postGenerate?.(app, {})).rejects.toThrow(/duplicate config path "duplicate\.path"/);
    expect(existsSync(finalPath)).toBe(false);
  });

  it('omits static OpenAPI/proto inventory when the concrete artifact is missing', async () => {
    const root = projectRoot('missing-static-schema');
    const inventory: Plugin & CapabilityInventoryContributor = {
      capabilityInventory: () => ({
        schemas: [
          { name: 'api-proto', kind: 'proto', path: 'schema/api.proto' },
          { name: 'route', kind: 'route', path: '/route' },
          { name: 'openapi', kind: 'openapi', path: 'schema/openapi.json' },
        ],
      }),
    };
    const producer = sourceBoundProducer(root, 'missing-static-schema');
    await producer.postGenerate?.(application().use(inventory), {});
    const manifest = JSON.parse(readFileSync(manifestPath(root), 'utf8')) as {
      schemas?: Array<{ name: string; kind: string }>;
    };
    expect(manifest.schemas).toEqual([expect.objectContaining({ name: 'route', kind: 'route' })]);
  });

  it('fails closed with a stable diagnostic when a required provider is missing', async () => {
    const root = projectRoot('missing-readiness');
    const requirement: Plugin & RequiredCapabilityContributor & CapabilityProvenanceContributor = {
      requiredCapabilities: () => [{ name: 'sql', requires: ['readiness'] }],
      capabilityProvenance: () => ({ evidencePath: 'src/sql.ts', sourceKind: 'framework' }),
    };
    const app = application().use(requirement);
    const producer = sourceBoundProducer(root, 'missing-readiness');

    await expect(producer.postGenerate?.(app, {})).rejects.toThrow(
      /capabilities\.missing_required_provider.*project "missing-readiness".*src\/sql\.ts.*contribute a "readiness" provider/s,
    );
    expect(existsSync(manifestPath(root))).toBe(false);
  });

  it('satisfies package requirements with a version-independent provider', async () => {
    const root = projectRoot('stable-package-provider');
    const requirement: Plugin & RequiredCapabilityContributor & CapabilityProvenanceContributor = {
      requiredCapabilities: () => [{ name: 'packaged', requires: ['package'] }],
      capabilityProvenance: () => ({ evidencePath: 'package.json', sourceKind: 'framework' }),
    };
    const producer = sourceBoundProducer(root, 'stable-package-provider');

    await expect(producer.postGenerate?.(application().use(requirement), {})).resolves.toBeDefined();
    const bytes = readFileSync(manifestPath(root), 'utf8');
    const manifest = JSON.parse(bytes) as {
      packages?: Array<{ package: string }>;
      packageVersions?: unknown;
    };
    expect(manifest.packages).toEqual([
      {
        identity: { ownerProject: 'stable-package-provider', kind: 'package', key: 'stable-package-provider' },
        package: 'stable-package-provider',
        provenance: {
          project: 'stable-package-provider',
          package: 'stable-package-provider',
          sourceKind: 'framework',
          declaration: { root: 'project', path: 'package.json' },
        },
      },
    ]);
    expect(manifest.packageVersions).toBeUndefined();
    expect(bytes).not.toContain('"version"');
  });

  it('fails closed with a stable diagnostic for duplicate providers', async () => {
    const root = projectRoot('duplicate-readiness');
    const first: Plugin & ReadinessChecker = { name: 'primary', checkReadiness: async () => {} };
    const second: Plugin & ReadinessChecker = { name: 'primary', checkReadiness: async () => {} };
    const app = application().use(first).use(second);
    const producer = sourceBoundProducer(root, 'duplicate-readiness');

    await expect(producer.postGenerate?.(app, {})).rejects.toThrow(/capabilities\.duplicate_provider/);
    expect(existsSync(manifestPath(root))).toBe(false);
  });

  it('validates runtime-invalid requirements before sorting and removes stale output', async () => {
    const root = projectRoot('invalid-requirement');
    const finalPath = manifestPath(root);
    mkdirSync(join(root, '.gen', 'schema'), { recursive: true });
    writeFileSync(finalPath, 'previous-manifest\n');
    const requirement = {
      requiredCapabilities: () => [{ name: 'sql', requires: [] }],
    } as Plugin & RequiredCapabilityContributor;
    const app = application().use(requirement);
    const producer = sourceBoundProducer(root, 'invalid-requirement');

    await expect(producer.postGenerate?.(app, {})).rejects.toThrow(/capabilities\.missing_requires/);
    expect(existsSync(finalPath)).toBe(false);
  });

  it('rejects runtime capability kinds outside the closed enum', async () => {
    const root = projectRoot('invalid-capability-kind');
    const requirement = {
      requiredCapabilities: () => [{ name: 'sql', requires: ['banana'] }],
    } as unknown as Plugin & RequiredCapabilityContributor;
    const app = application().use(requirement);
    const producer = sourceBoundProducer(root, 'invalid-capability-kind');

    await expect(producer.postGenerate?.(app, {})).rejects.toThrow(/capabilities\.invalid_capability_kind/);
    expect(existsSync(manifestPath(root))).toBe(false);
  });

  it('scopes migration provider identity by kind and namespace', async () => {
    const source = (kind: string, namespace: string, datasource?: string): MigrationSource => ({
      kind,
      namespace,
      infraDatabase: datasource ? () => ({ name: datasource, engine: 'postgres', schemas: [namespace] }) : undefined,
    });

    const crossKindRoot = projectRoot('cross-kind-migration');
    const crossKind: Plugin & MigrationContributor = {
      migrationSources: () => [source('sql', 'secrets'), source('gcs', 'secrets')],
    };
    const crossKindApp = application().use(crossKind);
    const crossKindProducer = sourceBoundProducer(crossKindRoot, 'cross-kind-migration');
    await expect(crossKindProducer.postGenerate?.(crossKindApp, {})).resolves.toBeDefined();
    expect(existsSync(manifestPath(crossKindRoot))).toBe(true);

    for (const testCase of [
      {
        label: 'duplicate',
        code: 'capabilities.duplicate_provider',
        sources: [source('sql', 'iam', 'primary'), source('sql', 'iam', 'primary')],
      },
      {
        label: 'conflict',
        code: 'capabilities.conflicting_provider',
        sources: [source('sql', 'iam', 'primary'), source('sql', 'iam', 'replica')],
      },
    ]) {
      const root = projectRoot(`migration-${testCase.label}`);
      const plugin: Plugin & MigrationContributor = { migrationSources: () => testCase.sources };
      const app = application().use(plugin);
      const producer = sourceBoundProducer(root, `migration-${testCase.label}`);
      await expect(producer.postGenerate?.(app, {})).rejects.toThrow(new RegExp(testCase.code.replace('.', '\\.')));
      expect(existsSync(manifestPath(root))).toBe(false);
    }
  });

  it('merges scheduler-stamped dependency manifests and rejects root escapes', async () => {
    const workspaceRoot = projectRoot('dependency-merge');
    const workloadRoot = join(workspaceRoot, 'workload');
    const dependencyPath = join(workspaceRoot, 'dependency', 'schema', 'capabilities.json');
    mkdirSync(workloadRoot, { recursive: true });
    mkdirSync(join(workspaceRoot, 'dependency', 'schema'), { recursive: true });
    writeFileSync(
      dependencyPath,
      `${JSON.stringify({
        $schema: 'https://putnami.dev/schemas/putnami-capabilities-v2.json',
        protocolVersion: 2,
        project: 'example/dependency',
        schemas: [
          {
            identity: {
              ownerProject: 'example/dependency',
              kind: 'schema',
              subkind: 'route',
              key: 'dependencyRoute',
            },
            name: 'dependencyRoute',
            kind: 'route',
            path: '/dependency',
            provenance: {
              project: 'example/dependency',
              package: 'example/dependency',
              version: '1.2.3',
              sourceKind: 'framework',
              declaration: { root: 'project', path: 'routes.ts' },
            },
          },
        ],
      })}\n`,
    );
    const packages = [
      {
        package: 'example/workload',
        version: '0.1.0',
        evidencePath: 'workload/package.json',
        sourceRoot: 'workload',
        sourceBinding: SOURCE_BINDING,
      },
      {
        package: 'example/dependency',
        version: '1.2.3',
        evidencePath: 'dependency/package.json',
        sourceRoot: 'dependency',
        sourceBinding: SOURCE_BINDING,
        capabilityManifestPath: '../dependency/schema/capabilities.json',
      },
    ];
    const app = application();
    const producer = createCapabilitiesProducer({
      projectRoot: workloadRoot,
      project: 'example/workload',
      capabilityRoot: '..',
      capabilityPackages: packages,
    });
    await expect(producer.postGenerate?.(app, {})).resolves.toBeDefined();
    const manifest = JSON.parse(readFileSync(manifestPath(workloadRoot), 'utf8')) as {
      schemas?: Array<{ name: string }>;
    };
    expect(manifest.schemas?.map(({ name }) => name)).toEqual(['dependencyRoute']);
    const manifestBytes = readFileSync(manifestPath(workloadRoot), 'utf8');
    expect(manifestBytes).not.toContain('packageVersions');
    expect(manifestBytes).not.toContain('"version"');

    writeFileSync(dependencyPath, '{"protocolVersion":1,"project":"example/dependency"}\n');
    await expect(producer.postGenerate?.(app, {})).rejects.toThrow(
      /uses protocol v1 and cannot be aggregated into v2 without precise identity/,
    );

    const escaping = createCapabilitiesProducer({
      projectRoot: workloadRoot,
      project: 'example/workload',
      capabilityRoot: '.',
      capabilityPackages: packages,
    });
    await expect(escaping.postGenerate?.(app, {})).rejects.toThrow(/escapes the stamped workspace root/);
  });

  it('publishes protocol v2 without embedding volatile scheduler metadata', async () => {
    const root = projectRoot('source-bound-forward-compatibility');
    const producer = createCapabilitiesProducer({
      projectRoot: root,
      project: 'example/workload',
      capabilityPackages: [
        {
          package: 'example/workload',
          version: '1.0.0',
          evidencePath: 'typescript/framework/application/package.json',
          sourceRoot: 'typescript/framework/application',
          sourceBinding: `source-v1:sha256:${'0'.repeat(64)}`,
        },
      ],
      registeredConfigDefinitions: () => [],
    });

    await expect(producer.postGenerate?.(application(), {})).resolves.toBeDefined();
    const manifest = readFileSync(manifestPath(root), 'utf8');
    expect(manifest).not.toContain('sourceRoot');
    expect(manifest).not.toContain('sourceBinding');
    expect(manifest).not.toContain('packageVersions');
    expect(manifest).not.toContain('"version"');
    expect(JSON.parse(manifest)).toMatchObject({ protocolVersion: 2, project: 'example/workload' });
  });

  it('publishes only the v1 activation contract for a complete legacy scheduler stamp', async () => {
    const root = projectRoot('legacy-scheduler-compatibility');
    mkdirSync(join(root, '.gen', 'schema', 'feature-evidence'), { recursive: true });
    writeFileSync(manifestPath(root), 'stale\n');
    writeFileSync(join(root, '.gen', 'schema', 'feature-evidence', 'typescript-framework.json'), 'stale\n');
    const loaderPath = join(root, '.gen', 'src', 'api.gen.ts');
    mkdirSync(join(root, '.gen', 'src'), { recursive: true });
    writeFileSync(loaderPath, 'export const route = true;\n');
    const producer = createCapabilitiesProducer({
      projectRoot: root,
      project: 'example/workload',
      capabilityPackages: [{ package: 'example/workload', version: '1.0.0', evidencePath: 'package.json' }],
    });
    const result = await producer.postGenerate?.(application(), {
      exports: { 'api-loader': loaderPath },
    });
    expect(result?.assets).toEqual({ 'schema/capabilities.json': manifestPath(root) });
    expect(JSON.parse(readFileSync(manifestPath(root), 'utf8'))).toMatchObject({
      protocolVersion: 1,
      project: 'example/workload',
      schemas: [{ name: 'api-loader', kind: 'route', path: '.gen/src/api.gen.ts' }],
    });
    expect(existsSync(join(root, '.gen', 'schema', 'feature-evidence', 'typescript-framework.json'))).toBe(false);
  });

  // Twin of TestReconcileCapabilityManifestV2_SlottedLoadersAreSourceDiscoverersAndActivate
  // in the TypeScript extension: both runtimes keep route schemas for the
  // first-slot keys only and classify every slotted key as a source discoverer
  // (ADR 0006), so either side reconciles what the other emits.
  it('publishes the slotted loaders of a second api(), static() or events() as source discoverers', async () => {
    const root = projectRoot('slotted-loaders');
    mkdirSync(join(root, '.gen', 'src'), { recursive: true });
    const keys = ['api-loader', 'api-1-loader', 'static-loader', 'static-1-loader', 'events-loader', 'events-1-loader'];
    const exports: Record<string, string> = {};
    for (const key of keys) {
      exports[key] = join(root, '.gen', 'src', `${key}.gen.ts`);
      writeFileSync(exports[key], 'export const loaded = true;\n');
    }
    const producer = sourceBoundProducer(root, 'slotted-loaders', { registeredConfigDefinitions: () => [] });

    await producer.postGenerate?.(application(), { exports });

    const manifest = JSON.parse(readFileSync(manifestPath(root), 'utf8')) as {
      schemas?: Array<{ name: string; kind: string }>;
      discoverers?: Array<{ name: string; kind: string }>;
    };
    expect(manifest.schemas?.map(({ name, kind }) => `${kind}:${name}`).sort()).toEqual([
      'route:api-loader',
      'route:static-loader',
    ]);
    expect(
      manifest.discoverers
        ?.filter(({ kind }) => kind === 'source')
        .map(({ name }) => name)
        .sort(),
    ).toEqual(['api-1-loader', 'events-1-loader', 'events-loader', 'static-1-loader']);
  });

  it('emits deterministic build-time feature evidence without changing capability bytes', async () => {
    const root = projectRoot('feature-evidence');
    writeAuthoredFeatures(root);
    const migration: Plugin & MigrationContributor = {
      migrationSources: () => [
        { kind: 'sql', namespace: 'iam', infraDatabase: () => ({ name: 'default', engine: 'postgres' }) },
      ],
    };
    const producer = sourceBoundProducer(root, 'feature-evidence-proof', { registeredConfigDefinitions: () => [] });
    const app = application()
      .feature({
        id: 'capabilities/typescript-evidence',
        name: 'TypeScript capability evidence',
        outcome: 'A build proves a feature requirement from an exact capability contribution',
        owner: 'capabilities',
        proves: [{ requirement: 'implementation', contribution: { kind: 'migration', subkind: 'sql', key: 'iam' } }],
      })
      .use(migration);
    stampDeclarationSite(app);

    const firstResult = await producer.postGenerate?.(app, {});
    const firstManifest = readFileSync(manifestPath(root), 'utf8');
    const firstEvidence = readFileSync(evidencePath(root), 'utf8');
    expect(firstResult?.assets?.['schema/feature-evidence/typescript-framework.json']).toBe(evidencePath(root));
    expect(JSON.parse(firstEvidence)).toMatchObject({
      protocolVersion: 1,
      evidence: [
        {
          feature: 'capabilities/typescript-evidence',
          requirement: 'implementation',
          // Stage is read from the authored requirement, never from the proof.
          stage: 'coded',
          outcome: 'supports',
          issuer: { kind: 'build', id: '@putnami/application' },
          source: { root: 'project', ownerProject: 'feature-evidence-proof', binding: SOURCE_BINDING },
          subject: { kind: 'capability', contribution: { kind: 'migration', subkind: 'sql', key: 'iam' } },
        },
      ],
    });

    // Re-running over unchanged sources must reproduce the same bytes: the
    // record identity is derived from the association, not from a written value.
    await producer.postGenerate?.(app, {});
    expect(readFileSync(manifestPath(root), 'utf8')).toBe(firstManifest);
    expect(readFileSync(evidencePath(root), 'utf8')).toBe(firstEvidence);
  });

  it('leaves an unproven contribution out of the evidence document', async () => {
    const root = projectRoot('unproven-contribution');
    writeAuthoredFeatures(root);
    const migrations: Plugin & MigrationContributor = {
      migrationSources: () => [
        { kind: 'sql', namespace: 'iam', infraDatabase: () => ({ name: 'default', engine: 'postgres' }) },
        { kind: 'sql', namespace: 'billing' },
      ],
    };
    const producer = sourceBoundProducer(root, 'feature-evidence-proof', { registeredConfigDefinitions: () => [] });
    const app = application()
      .feature({
        id: 'capabilities/typescript-evidence',
        name: 'TypeScript capability evidence',
        outcome: 'A build proves a feature requirement from an exact capability contribution',
        owner: 'capabilities',
        proves: [{ requirement: 'implementation', contribution: { kind: 'migration', subkind: 'sql', key: 'iam' } }],
      })
      .use(migrations);
    stampDeclarationSite(app);
    await producer.postGenerate?.(app, {});

    const document = JSON.parse(readFileSync(evidencePath(root), 'utf8')) as {
      evidence: Array<{ subject: { contribution: { key: string } } }>;
    };
    expect(document.evidence.map(({ subject }) => subject.contribution.key)).toEqual(['iam']);
  });

  it('fails publication atomically for a proof that does not resolve', async () => {
    const cases: Array<{ label: string; proves: FeatureProof[]; authored: boolean; code: RegExp }> = [
      {
        label: 'unknown-requirement',
        proves: [{ requirement: 'not-declared', contribution: { kind: 'migration', subkind: 'sql', key: 'iam' } }],
        authored: true,
        code: /features\.unknown_requirement/,
      },
      {
        label: 'unpublished-contribution',
        proves: [
          { requirement: 'implementation', contribution: { kind: 'migration', subkind: 'sql', key: 'never-emitted' } },
        ],
        authored: true,
        code: /capabilities\.unresolved_reference/,
      },
      {
        label: 'dependency-owned-contribution',
        proves: [{ requirement: 'implementation', contribution: { kind: 'package', key: '@putnami/database' } }],
        authored: true,
        code: /capabilities\.unresolved_reference/,
      },
      {
        label: 'no-authored-manifest',
        proves: [{ requirement: 'implementation', contribution: { kind: 'migration', subkind: 'sql', key: 'iam' } }],
        authored: false,
        code: /features\.unknown_feature/,
      },
    ];
    for (const testCase of cases) {
      const root = projectRoot(`invalid-proof-${testCase.label}`);
      if (testCase.authored) writeAuthoredFeatures(root);
      const migration: Plugin & MigrationContributor = {
        migrationSources: () => [{ kind: 'sql', namespace: 'iam' }],
      };
      const producer = sourceBoundProducer(root, 'invalid-feature-evidence', {
        registeredConfigDefinitions: () => [],
      });
      const app = application()
        .feature({
          id: 'capabilities/typescript-evidence',
          name: 'TypeScript capability evidence',
          outcome: 'A build proves a feature requirement from an exact capability contribution',
          owner: 'capabilities',
          proves: testCase.proves,
        })
        .use(migration);
      stampDeclarationSite(app);

      await expect(producer.postGenerate?.(app, {})).rejects.toThrow(testCase.code);
      expect(existsSync(manifestPath(root))).toBe(false);
      expect(existsSync(evidencePath(root))).toBe(false);
    }
  });

  it('refuses a proof whose declaration site cannot be resolved', async () => {
    const root = projectRoot('unlocatable-proof');
    writeAuthoredFeatures(root);
    const migration: Plugin & MigrationContributor = { migrationSources: () => [{ kind: 'sql', namespace: 'iam' }] };
    const producer = sourceBoundProducer(root, 'unlocatable-proof', { registeredConfigDefinitions: () => [] });
    // Declared from inside the framework package, so no call site is captured.
    const app = application()
      .feature({
        id: 'capabilities/typescript-evidence',
        name: 'TypeScript capability evidence',
        outcome: 'A build proves a feature requirement from an exact capability contribution',
        owner: 'capabilities',
        proves: [{ requirement: 'implementation', contribution: { kind: 'migration', subkind: 'sql', key: 'iam' } }],
      })
      .use(migration);
    expect(app.getFeature()?.provenance).toBeUndefined();

    await expect(producer.postGenerate?.(app, {})).rejects.toThrow(/declaration site/);
    expect(existsSync(manifestPath(root))).toBe(false);
    expect(existsSync(evidencePath(root))).toBe(false);
  });

  it('skips the aggregator runtime defaults that share the infra directory', () => {
    const root = projectRoot('runtime-defaults');
    writeAuthoredFeatures(root);
    mkdirSync(join(root, '.gen', 'infra'), { recursive: true });
    // Written by `putnami build`; its ingress/scaling members are runtime
    // intent that the strict per-project sidecar schema rejects. Pins the
    // NON_SIDECAR_INFRA_FILES skip in collectInfraSidecars: without it any
    // build-then-collect sequence throws on this file.
    writeFileSync(
      join(root, '.gen', 'infra', 'runtime.json'),
      JSON.stringify({ ingress: { public: false }, scaling: { min: 0, max: 1, concurrency: 500 } }),
    );
    writeFileSync(
      join(root, '.gen', 'infra', 'database.json'),
      JSON.stringify({ protocolVersion: 2, databases: [{ name: 'default', engine: 'postgres' }] }),
    );
    const producer = sourceBoundProducer(root, 'runtime-defaults', { registeredConfigDefinitions: () => [] });

    expect(() => producer.postGenerate?.(application(), {})).not.toThrow();
    expect(
      (
        JSON.parse(readFileSync(manifestPath(root), 'utf8')) as { infraRequirements?: Array<{ name: string }> }
      ).infraRequirements?.map(({ name }) => name),
    ).toEqual(['default']);
  });

  it('fails closed for malformed scheduler capability package metadata', async () => {
    const cases: Array<{ label: string; packages: unknown }> = [
      { label: 'wrong-shape', packages: {} },
      { label: 'empty', packages: [] },
      { label: 'incomplete', packages: [{ package: 'example/workload', version: '1.0.0' }] },
      {
        label: 'unresolved',
        packages: [{ package: 'example/workload', version: 'workspace:*', evidencePath: 'package.json' }],
      },
      {
        label: 'partial-source-metadata',
        packages: [
          {
            package: 'example/workload',
            version: '1.0.0',
            evidencePath: 'package.json',
            sourceRoot: 'typescript/framework/application',
          },
        ],
      },
      {
        label: 'evidence-outside-source-root',
        packages: [
          {
            package: 'example/workload',
            version: '1.0.0',
            evidencePath: 'elsewhere/package.json',
            sourceRoot: 'typescript/framework/application',
            sourceBinding: SOURCE_BINDING,
          },
        ],
      },
      {
        label: 'conflict',
        packages: [
          { package: 'example/workload', version: '1.0.0', evidencePath: 'package.json' },
          { package: 'example/workload', version: '2.0.0', evidencePath: 'package.json' },
        ],
      },
    ];
    for (const { label, packages } of cases) {
      const root = projectRoot(`scheduler-${label}`);
      const producer = createCapabilitiesProducer({
        projectRoot: root,
        project: 'example/workload',
        capabilityPackages: packages as never,
      });
      await expect(producer.postGenerate?.(application(), {})).rejects.toThrow();
      expect(existsSync(manifestPath(root))).toBe(false);
    }

    const root = projectRoot('scheduler-version-file-shape');
    mkdirSync(join(root, '.gen'), { recursive: true });
    writeFileSync(join(root, '.gen', 'version.json'), '{"name":"example/workload","capabilityPackages":{}}\n');
    const producer = createCapabilitiesProducer({ projectRoot: root });
    await expect(producer.postGenerate?.(application(), {})).rejects.toThrow(/capabilityPackages must be an array/);
  });

  it('ignores runtime.json in .gen/infra, which the CLI writes and which is not a sidecar', async () => {
    const root = projectRoot('infra-runtime-json');
    mkdirSync(join(root, '.gen', 'infra'), { recursive: true });
    // What the CLI's aggregateWorkloadInfra leaves in the shared directory. Its
    // `ingress` key is not in the sidecar schema, so parsing it as one throws and
    // takes an unrelated build down with it.
    writeFileSync(
      join(root, '.gen', 'infra', 'runtime.json'),
      `${JSON.stringify({ ingress: { domains: [] }, databases: [] })}\n`,
    );
    // A real sidecar beside it, so this proves the file is skipped rather than the
    // whole scan being disabled.
    writeFileSync(
      join(root, '.gen', 'infra', 'inventory.json'),
      `${JSON.stringify({ protocolVersion: 2, storage: [{ name: 'avatars' }] })}\n`,
    );

    const producer = sourceBoundProducer(root, 'infra-runtime-json-proof');
    const app = application();
    const plugins = [...app.collectPlugins(), { plugin: producer, owner: app }];
    const generated = await runGenerate(plugins);
    await runPostGenerate(plugins, generated);

    const manifest = JSON.parse(readFileSync(manifestPath(root), 'utf8')) as {
      infraRequirements?: Array<{ name: string; kind: string }>;
    };
    const infra = manifest.infraRequirements ?? [];
    expect(infra.map((entry) => entry.name)).toContain('avatars');
    expect(infra.every((entry) => entry.kind !== 'ingress')).toBe(true);
  });
});
