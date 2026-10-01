import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { CAPABILITY_DIAGNOSTIC_CODES, CapabilityManifestValidationError } from '../../src/capabilities/manifest';
import {
  availableProviderKindsV2,
  capabilitySurfaceV2,
  computeSourceBinding,
  parseCapabilityManifestDocument,
  serializeCapabilityManifestV2,
  sourceDigest,
  validateCapabilityManifestV2,
  type SourceBindingFile,
} from '../../src/capabilities/manifest.v2';
import type {
  ContributionIdentity,
  ContributionKind,
  ManifestV2,
  PackageV2,
  ProvenanceV2,
} from '../../src/capabilities/manifest.types';

const FIXTURE = join(
  import.meta.dir,
  '../../../../../protocols/capabilities/fixtures/v2/equivalence/capabilities-v2.golden.json',
);
const INVALID = join(import.meta.dir, '../../../../../protocols/capabilities/fixtures/v2/invalid');
const LEGACY_BINDING = 'source-v1:sha256:0000000000000000000000000000000000000000000000000000000000000000';
function identity(kind: ContributionKind, subkind: string, key: string): ContributionIdentity {
  return { ownerProject: 'example', kind, subkind: subkind || undefined, key };
}

function provenance(path: string): ProvenanceV2 {
  return {
    project: 'example',
    sourceKind: 'manual',
    declaration: { root: 'project', path },
  };
}

function everyKind(): ManifestV2 {
  return {
    protocolVersion: 2,
    project: 'example',
    configDefinitions: [
      {
        identity: identity('config', '', 'database.default'),
        path: 'database.default',
        provenance: provenance('config.ts'),
      },
    ],
    schemas: [
      {
        identity: identity('schema', 'route', 'listUsers'),
        name: 'listUsers',
        kind: 'route',
        provenance: provenance('routes.ts'),
      },
    ],
    discoverers: [
      {
        identity: identity('discoverer', 'source', 'routes'),
        name: 'routes',
        kind: 'source',
        provenance: provenance('discover.ts'),
      },
    ],
    migrations: [
      {
        identity: identity('migration', 'sql', 'billing'),
        name: 'billing',
        kind: 'sql',
        datasource: 'default',
        provenance: provenance('sql.ts'),
      },
      {
        identity: identity('migration', 'code', 'billing'),
        name: 'billing',
        kind: 'code',
        provenance: provenance('code.ts'),
      },
    ],
    infraRequirements: [
      {
        identity: identity('infra', 'database', 'primary'),
        name: 'primary',
        kind: 'database',
        provenance: provenance('infra.ts'),
      },
    ],
    healthContributors: [
      {
        identity: identity('health', 'readiness', 'database'),
        name: 'database',
        probe: 'readiness',
        provenance: provenance('health.ts'),
      },
    ],
    lifecycleHooks: [
      {
        identity: identity('lifecycle', 'starter', 'pool'),
        name: 'pool',
        phase: 'starter',
        provenance: provenance('lifecycle.ts'),
      },
    ],
    packages: [
      {
        identity: identity('package', '', '@putnami/database'),
        package: '@putnami/database',
        provenance: provenance('package.json'),
      },
    ],
    requiredCapabilities: [
      {
        identity: identity('requiredCapability', '', 'sql'),
        name: 'sql',
        requires: ['readiness', 'migration', 'datasource'],
        provenance: provenance('requirements.ts'),
      },
    ],
    domainAccess: [
      {
        identity: identity('domainAccess', 'projection', 'example.workspace-context.v1'),
        import: 'example.workspace-context.v1',
        mode: 'projection',
        status: 'active',
        transports: [
          {
            role: 'updates',
            kind: 'event',
            contract: 'runtime.workspace-binding-changed.v1',
            availability: 'active',
          },
        ],
        enforced: {
          ordering: 'source-version',
          lateEvents: 'ignore-older',
          deletion: 'tombstone',
        },
        provenance: provenance('darc.go'),
      },
    ],
  };
}

describe('capability manifest v2', () => {
  it('strictly dispatches v1/v2 and reproduces the shared Go v2 bytes', () => {
    const bytes = readFileSync(FIXTURE, 'utf8');
    const document = parseCapabilityManifestDocument(bytes);
    expect(document.protocolVersion).toBe(2);
    if (document.protocolVersion !== 2) throw new Error('expected v2');
    expect(serializeCapabilityManifestV2(document.manifest)).toBe(bytes);

    const v1 = readFileSync(
      join(import.meta.dir, '../../../../../protocols/capabilities/fixtures/valid/minimal.json'),
      'utf8',
    );
    expect(parseCapabilityManifestDocument(v1).protocolVersion).toBe(1);
  });

  it('accepts historical volatile metadata but never emits it', () => {
    const legacy = everyKind() as unknown as {
      configDefinitions: Array<{ provenance: Record<string, unknown> }>;
      packages?: Array<{ identity: unknown; package: string; provenance: Record<string, unknown> }>;
      packageVersions?: Array<{
        identity: unknown;
        package: string;
        version: string;
        provenance: Record<string, unknown>;
      }>;
    };
    legacy.configDefinitions[0].provenance['sourceBinding'] = LEGACY_BINDING;
    const stablePackage = legacy.packages?.[0];
    if (!stablePackage) throw new Error('missing stable package fixture');
    legacy.packageVersions = [
      {
        ...stablePackage,
        version: '1.4.0',
        provenance: { ...stablePackage.provenance, version: '1.4.0' },
      },
    ];
    legacy.packages = undefined;
    const document = parseCapabilityManifestDocument(JSON.stringify(legacy));
    if (document.protocolVersion !== 2) throw new Error('expected v2');
    const serialized = serializeCapabilityManifestV2(document.manifest);
    expect(serialized).not.toContain('sourceBinding');
    expect(serialized).not.toContain('packageVersions');
    expect(serialized).not.toContain('"version"');
    expect(serialized).toContain('"packages"');
  });

  it('requires exact top-level integer version tokens', () => {
    for (const value of ['{"protocolVersion":1,"project":"p"}', '{"protocolVersion":2,"project":"p"}']) {
      expect(parseCapabilityManifestDocument(value).protocolVersion).toBe(Number(JSON.parse(value).protocolVersion));
    }
    for (const value of [
      '{"project":"p"}',
      '{"protocolVersion":null,"project":"p"}',
      '{"protocolVersion":"2","project":"p"}',
      '{"protocolVersion":1.5,"project":"p"}',
      '{"protocolVersion":1.0,"project":"p"}',
      '{"protocolVersion":2.0,"project":"p"}',
      '{"protocolVersion":1e0,"project":"p"}',
      '{"protocolVersion":2e0,"project":"p"}',
      '{"protocolVersion":3,"project":"p"}',
      '{"nested":{"protocolVersion":2},"project":"p"}',
      '{"protocolVersion":1,"protocolVersion":2,"project":"p"}',
    ]) {
      try {
        parseCapabilityManifestDocument(value);
        throw new Error(`${value} should fail`);
      } catch (error) {
        expect(error).toBeInstanceOf(CapabilityManifestValidationError);
        expect((error as CapabilityManifestValidationError).diagnostics[0]?.code).toBe(
          CAPABILITY_DIAGNOSTIC_CODES.invalidProtocolVersion,
        );
      }
    }
  });

  it('covers every contribution kind and sorts by complete owner identity', () => {
    const manifest = everyKind();
    expect(validateCapabilityManifestV2(manifest)).toEqual([]);
    const serialized = serializeCapabilityManifestV2(manifest);
    expect(serialized.indexOf('"subkind": "code"')).toBeLessThan(serialized.indexOf('"subkind": "sql"'));
    expect(serialized).not.toContain('packageVersions');
    expect(serialized).not.toContain('"version"');
    expect(parseCapabilityManifestDocument(serialized).protocolVersion).toBe(2);
  });

  // Canonical emission scopes packages[] to the manifest's own capability
  // surface, so a committed manifest can never enumerate the reachable dependency
  // closure. The Go twin is TestCanonicalPackagesAreScopedToTheCapabilitySurface;
  // both emitters must agree on the canonical bytes.
  it('scopes package contributions to the capability surface', () => {
    const manifest = everyKind();
    const closure = (name: string): PackageV2 => ({
      identity: { ownerProject: name, kind: 'package', key: name },
      package: name,
      provenance: {
        project: name,
        package: name,
        sourceKind: 'framework',
        declaration: { root: 'project', path: 'package.json' },
      },
    });

    const surface = capabilitySurfaceV2(manifest);
    expect(surface.has('example')).toBe(true);
    expect(surface.has('@putnami/unrelated')).toBe(false);

    const baseline = serializeCapabilityManifestV2(manifest);
    manifest.packages = [...(manifest.packages ?? []), closure('@putnami/unrelated'), closure('@putnami/also-new')];
    const widened = serializeCapabilityManifestV2(manifest);

    expect(widened).toBe(baseline);
    expect(widened).toContain('@putnami/database');
    expect(widened).not.toContain('@putnami/unrelated');
  });

  it('checks every container-scoped required-provider mapping', () => {
    const manifest = everyKind();
    manifest.healthContributors?.push(
      {
        identity: identity('health', 'health', 'service'),
        name: 'service',
        probe: 'health',
        provenance: provenance('health.ts'),
      },
      {
        identity: identity('health', 'liveness', 'service'),
        name: 'service',
        probe: 'liveness',
        provenance: provenance('liveness.ts'),
      },
    );
    const allProviders = [
      'config',
      'schema',
      'discoverer',
      'migration',
      'datasource',
      'infra',
      'health',
      'liveness',
      'readiness',
      'lifecycle',
      'package',
    ] as const;
    (manifest.requiredCapabilities as NonNullable<ManifestV2['requiredCapabilities']>)[0].requires = [...allProviders];
    expect([...availableProviderKindsV2(manifest)].sort()).toEqual([...allProviders].sort());
    expect(validateCapabilityManifestV2(manifest)).toEqual([]);

    const copiedProvider = everyKind();
    const config = (copiedProvider.configDefinitions as NonNullable<ManifestV2['configDefinitions']>)[0];
    config.identity.ownerProject = 'dependency';
    config.provenance.project = 'dependency';
    (copiedProvider.requiredCapabilities as NonNullable<ManifestV2['requiredCapabilities']>)[0].requires = ['config'];
    expect(
      validateCapabilityManifestV2(copiedProvider).some(
        ({ code }) => code === CAPABILITY_DIAGNOSTIC_CODES.missingRequiredProvider,
      ),
    ).toBe(false);

    copiedProvider.configDefinitions = undefined;
    const missing = validateCapabilityManifestV2(copiedProvider);
    expect(missing.some(({ code }) => code === CAPABILITY_DIAGNOSTIC_CODES.missingRequiredProvider)).toBe(true);
    expect(missing.find(({ code }) => code === CAPABILITY_DIAGNOSTIC_CODES.missingRequiredProvider)?.field).toBe(
      'requiredCapabilities[0].requires[0]',
    );
  });

  it('pins strict and semantic diagnostics', () => {
    for (const [file, code] of [
      ['missing-identity.json', CAPABILITY_DIAGNOSTIC_CODES.missingContributionIdentity],
      ['path-escape.json', CAPABILITY_DIAGNOSTIC_CODES.pathEscape],
      ['unknown-field.json', CAPABILITY_DIAGNOSTIC_CODES.unknownField],
      ['version-decimal.json', CAPABILITY_DIAGNOSTIC_CODES.invalidProtocolVersion],
    ] as const) {
      try {
        parseCapabilityManifestDocument(readFileSync(join(INVALID, file), 'utf8'));
        throw new Error(`${file} should fail`);
      } catch (error) {
        expect(error).toBeInstanceOf(CapabilityManifestValidationError);
        expect((error as CapabilityManifestValidationError).diagnostics.some((item) => item.code === code)).toBe(true);
      }
    }

    const duplicate = everyKind();
    duplicate.migrations = [
      duplicate.migrations?.[0] as NonNullable<ManifestV2['migrations']>[number],
      duplicate.migrations?.[0] as NonNullable<ManifestV2['migrations']>[number],
    ];
    expect(
      validateCapabilityManifestV2(duplicate).some(
        ({ code }) => code === CAPABILITY_DIAGNOSTIC_CODES.duplicateContribution,
      ),
    ).toBe(true);

    const malformed = everyKind() as unknown as Record<string, unknown>;
    malformed['migrations'] = [
      {
        ...(everyKind().migrations?.[0] as object),
        kind: 17,
      },
    ];
    expect(() => parseCapabilityManifestDocument(JSON.stringify(malformed))).toThrow(CapabilityManifestValidationError);
  });

  it('orders diagnostics canonically across shuffled invalid entries', () => {
    const manifest = everyKind();
    const firstConfig = (manifest.configDefinitions as NonNullable<ManifestV2['configDefinitions']>)[0];
    const a = JSON.parse(JSON.stringify(firstConfig)) as typeof firstConfig;
    a.identity = { ownerProject: 'a-owner', kind: 'config', key: 'a.config' };
    a.path = 'a.config';
    a.provenance = provenance('a.ts');
    a.provenance.project = 'a-owner';
    a.provenance.artifacts = [{ root: 'project', path: 'generated.json', digest: 'invalid' }];
    const z = JSON.parse(JSON.stringify(firstConfig)) as typeof firstConfig;
    z.identity = { ownerProject: 'z-owner', kind: 'config', key: 'wrong' };
    z.path = 'z.config';
    z.provenance = provenance('z.ts');
    z.provenance.project = 'z-owner';
    manifest.configDefinitions = [z, a];
    (manifest.requiredCapabilities as NonNullable<ManifestV2['requiredCapabilities']>)[0].requires = [
      'schema',
      'config',
      'health',
    ];
    const signature = (value: ManifestV2): string[] =>
      validateCapabilityManifestV2(value).map(({ code, field }) => `${code}|${field}`);
    const first = signature(manifest);
    manifest.configDefinitions.reverse();
    (manifest.requiredCapabilities as NonNullable<ManifestV2['requiredCapabilities']>)[0].requires.reverse();
    expect(signature(manifest)).toEqual(first);
  });

  it('matches the shared source-v1 input-record golden independent of order', () => {
    const records: SourceBindingFile[] = [
      { path: 'z.sh', mode: '100755', digest: sourceDigest('echo z\n') },
      { path: 'a.txt', mode: '100644', digest: sourceDigest('a\n') },
      { path: 'link', mode: '120000', digest: sourceDigest('a.txt') },
      { path: 'vendor/dependency', mode: '160000', digest: 'git:1111111111111111111111111111111111111111' },
      { path: '.gen/schema/generated.json', mode: '100644', digest: sourceDigest('generated') },
      { path: 'nested/.gen/generated.json', mode: '100644', digest: sourceDigest('generated') },
      { path: 'schema/capabilities.json', mode: '100644', digest: sourceDigest('output') },
      { path: 'nested/schema/feature-evidence/a.json', mode: '100644', digest: sourceDigest('evidence') },
    ];
    const expected = 'source-v1:sha256:33ff6e16efb726685f63c102204dacf0c36ffe449419e1fc2798470ec06c6689';
    expect(computeSourceBinding(records)).toBe(expected);
    expect(computeSourceBinding([...records].reverse())).toBe(expected);
  });
});
