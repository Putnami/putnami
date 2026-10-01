import { describe, expect, it } from 'bun:test';
import {
  CAPABILITY_DIAGNOSTIC_CODES,
  buildCapabilityManifest,
  serializeCapabilityManifest,
  validateCapabilityManifest,
} from '../../src/capabilities/manifest';
import type { Manifest } from '../../src/capabilities/manifest.types';

const LEGACY_PROTOCOL_VERSION = 1 as const;

describe('capability manifest Go compatibility', () => {
  it('sorts protocol-valid Unicode by UTF-8 bytes like Go strings', () => {
    const project = 'unicode-project';
    const manifest = buildCapabilityManifest({
      protocolVersion: LEGACY_PROTOCOL_VERSION,
      project,
      migrations: [
        { name: '\u{10000}', provenance: { project, sourceKind: 'framework' } },
        { name: '\uE000', provenance: { project, sourceKind: 'framework' } },
      ],
    });

    expect(manifest.migrations?.map(({ name }) => name)).toEqual(['\uE000', '\u{10000}']);
  });

  it('escapes HTML characters and JavaScript separators like encoding/json', () => {
    const special = '<>&\u2028\u2029';
    const manifest = buildCapabilityManifest({
      protocolVersion: LEGACY_PROTOCOL_VERSION,
      project: special,
      migrations: [{ name: special, provenance: { project: special, sourceKind: 'framework' } }],
    });
    const serialized = serializeCapabilityManifest(manifest);

    expect(serialized).toContain('\\u003c\\u003e\\u0026\\u2028\\u2029');
    for (const character of special) expect(serialized).not.toContain(character);
  });

  it('validates required providers and provider collisions with stable codes', () => {
    const project = 'validation-project';
    const provenance = { project, sourceKind: 'framework' as const, evidencePath: 'src/sql.ts' };
    const manifest = buildCapabilityManifest({
      protocolVersion: LEGACY_PROTOCOL_VERSION,
      project,
      configDefinitions: [
        { path: 'iam', fields: [{ name: 'primary' }], provenance },
        { path: 'iam', fields: [{ name: 'replica' }], provenance },
      ],
      migrations: [{ name: 'iam', datasource: 'primary', provenance }],
      healthContributors: [
        { name: 'db', probe: 'health', provenance },
        { name: 'db', probe: 'health', provenance },
      ],
      requiredCapabilities: [{ name: 'sql', requires: ['datasource', 'migration', 'readiness'], provenance }],
    });

    expect(validateCapabilityManifest(manifest).map(({ code }) => code)).toEqual([
      CAPABILITY_DIAGNOSTIC_CODES.conflictingProvider,
      CAPABILITY_DIAGNOSTIC_CODES.duplicateProvider,
      CAPABILITY_DIAGNOSTIC_CODES.missingRequiredProvider,
    ]);
    expect(validateCapabilityManifest(manifest)[2]?.message).toContain('project "validation-project"');
    expect(validateCapabilityManifest(manifest)[2]?.message).toContain('src/sql.ts');
  });

  it('does not infer v1 migration identity from namespace alone', () => {
    const project = 'migration-v1';
    const provenance = { project, sourceKind: 'framework' as const };
    const manifest = buildCapabilityManifest({
      protocolVersion: LEGACY_PROTOCOL_VERSION,
      project,
      migrations: [
        { name: 'secrets', datasource: 'primary', provenance },
        { name: 'secrets', datasource: 'replica', provenance },
      ],
    });

    expect(validateCapabilityManifest(manifest)).toEqual([]);
  });

  it('keys infra providers by kind and name', () => {
    const project = 'infra-identity';
    const provenance = { project, sourceKind: 'framework' as const };
    const crossKind = buildCapabilityManifest({
      protocolVersion: LEGACY_PROTOCOL_VERSION,
      project,
      infraRequirements: [
        { name: 'primary', kind: 'database', provenance },
        { name: 'primary', kind: 'secret', provenance },
      ],
    });
    expect(validateCapabilityManifest(crossKind)).toEqual([]);

    const duplicate = buildCapabilityManifest({
      protocolVersion: LEGACY_PROTOCOL_VERSION,
      project,
      infraRequirements: [
        { name: 'primary', kind: 'database', provenance },
        { name: 'primary', kind: 'database', provenance },
      ],
    });
    expect(validateCapabilityManifest(duplicate).map(({ code }) => code)).toEqual([
      CAPABILITY_DIAGNOSTIC_CODES.duplicateProvider,
    ]);
  });

  it('validates runtime requirement structure with Go-compatible codes', () => {
    const manifest = {
      protocolVersion: 1,
      project: 'runtime-invalid',
      requiredCapabilities: [
        { name: '', requires: [], provenance: { project: '', sourceKind: 'robot' } },
        { name: 'sql', requires: ['banana'], provenance: { project: 'runtime-invalid', sourceKind: 'framework' } },
      ],
    } as unknown as Manifest;

    expect(validateCapabilityManifest(manifest).map(({ code }) => code)).toEqual([
      CAPABILITY_DIAGNOSTIC_CODES.invalidName,
      CAPABILITY_DIAGNOSTIC_CODES.missingRequires,
      CAPABILITY_DIAGNOSTIC_CODES.missingProvenance,
      CAPABILITY_DIAGNOSTIC_CODES.invalidSourceKind,
      CAPABILITY_DIAGNOSTIC_CODES.invalidCapabilityKind,
    ]);
  });
});
