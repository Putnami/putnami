import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { serializeFeatureEvidenceDocument } from '../../src/features/evidence';
import {
  type GeneratedEvidenceInput,
  buildGeneratedEvidence,
  generatedEvidenceId,
} from '../../src/features/generated-evidence';

const VECTOR = join(
  import.meta.dir,
  '../../../../../protocols/features/fixtures/equivalence/generated-evidence.golden.json',
);

const BINDING = 'source-v1:sha256:3f79bb7b435b05321651daefd374cdc681dc06faa65e374e38337b88ca046dea';

/**
 * The same semantic scenario the Go conformance test composes: one authored
 * `coded` requirement, two mapped contributions, one published contribution
 * nobody mapped, and one owned by a dependency.
 */
function scenario(): GeneratedEvidenceInput {
  return {
    issuer: { kind: 'build', id: 'putnami.conformance/producer' },
    source: { root: 'project', ownerProject: 'conformance.example', binding: BINDING },
    authored: {
      features: [
        {
          id: 'conformance/generated-evidence',
          requirements: [{ id: 'implementation', stage: 'coded', evidenceKinds: ['capability'] }],
        },
      ],
    },
    contributions: [
      { ownerProject: 'conformance.example', kind: 'config', key: 'receiver' },
      { ownerProject: 'conformance.example', kind: 'migration', subkind: 'sql', key: 'events' },
      { ownerProject: 'conformance.example', kind: 'lifecycle', subkind: 'starter', key: 'receiver' },
      { ownerProject: 'go.putnami.dev/http', kind: 'package', key: 'go.putnami.dev/http' },
    ],
    mappings: [
      {
        feature: 'conformance/generated-evidence',
        requirement: 'implementation',
        kind: 'migration',
        subkind: 'sql',
        key: 'events',
        provenance: { root: 'project', path: 'receiver.go', symbol: 'ReceiverFeature' },
      },
      {
        feature: 'conformance/generated-evidence',
        requirement: 'implementation',
        kind: 'config',
        key: 'receiver',
        provenance: { root: 'project', path: 'receiver.go', symbol: 'ReceiverFeature' },
      },
    ],
  };
}

describe('generated feature evidence', () => {
  it('reproduces the shared Go/TypeScript vector byte for byte', () => {
    const { document, diagnostics } = buildGeneratedEvidence(scenario());
    expect(diagnostics).toEqual([]);
    expect(document).toBeDefined();
    // biome-ignore lint/style/noNonNullAssertion: asserted above
    expect(serializeFeatureEvidenceDocument(document!)).toBe(readFileSync(VECTOR, 'utf8'));
  });

  it('omits unmapped and dependency-owned contributions', () => {
    const { document } = buildGeneratedEvidence(scenario());
    const contributions = (document?.evidence ?? []).map((record) =>
      record.subject.kind === 'capability' ? record.subject.contribution : undefined,
    );
    expect(contributions.map((contribution) => contribution?.kind)).toEqual(['migration', 'config']);
    for (const contribution of contributions) {
      expect(contribution?.ownerProject).toBe('conformance.example');
    }
  });

  it('derives an identity that is canonical and contribution-specific', () => {
    const contribution = { ownerProject: 'conformance.example', kind: 'config', key: 'receiver' } as const;
    const id = generatedEvidenceId('conformance/generated-evidence', 'implementation', contribution);
    expect(id).toMatch(/^[a-z0-9]+(?:-[a-z0-9]+)*(?:\/[a-z0-9]+(?:-[a-z0-9]+)*)+$/);
    // Same digest the Go resolver derives, so a separator or hash change in
    // either language fails here as well as in the vector.
    expect(id).toBe('conformance/generated-evidence/implementation/fd13eb407f45');
    expect(
      generatedEvidenceId('conformance/generated-evidence', 'implementation', { ...contribution, key: 'other' }),
    ).not.toBe(id);
  });

  it('publishes the same bytes whatever order the mappings arrive in', () => {
    const forward = scenario();
    const reverse: GeneratedEvidenceInput = {
      ...forward,
      contributions: [...forward.contributions].reverse(),
      mappings: [...forward.mappings].reverse(),
    };
    const left = buildGeneratedEvidence(forward).document;
    const right = buildGeneratedEvidence(reverse).document;
    // Compare the returned records, not only the serialized bytes: serialization
    // canonicalizes on its way out, so a byte check alone could never fail.
    expect((left?.evidence ?? []).map((record) => record.id)).toEqual(
      (right?.evidence ?? []).map((record) => record.id),
    );
    // biome-ignore lint/style/noNonNullAssertion: both documents are defined
    expect(serializeFeatureEvidenceDocument(left!)).toBe(serializeFeatureEvidenceDocument(right!));
  });

  it('coalesces identical mappings and refuses contradictory ones', () => {
    const base = scenario();
    const identical = buildGeneratedEvidence({ ...base, mappings: [base.mappings[0]!, base.mappings[0]!] });
    expect(identical.document?.evidence).toHaveLength(1);

    const contradictory = buildGeneratedEvidence({
      ...base,
      mappings: [
        base.mappings[0]!,
        { ...base.mappings[0]!, provenance: { root: 'project', path: 'receiver.go', symbol: 'Other' } },
      ],
    });
    expect(contradictory.document).toBeUndefined();
    expect(contradictory.diagnostics.map(({ code }) => code)).toContain('features.duplicate_evidence');
  });

  it('refuses every association it cannot resolve exactly', () => {
    const base = scenario();
    const cases: [string, GeneratedEvidenceInput, string][] = [
      [
        'unknown feature',
        { ...base, mappings: [{ ...base.mappings[0]!, feature: 'conformance/absent' }] },
        'features.unknown_feature',
      ],
      ['no authored manifest', { ...base, authored: undefined }, 'features.unknown_feature'],
      [
        'unknown requirement',
        { ...base, mappings: [{ ...base.mappings[0]!, requirement: 'absent' }] },
        'features.unknown_requirement',
      ],
      [
        'modeled requirement stage',
        {
          ...base,
          authored: {
            features: [
              {
                id: 'conformance/generated-evidence',
                requirements: [{ id: 'implementation', stage: 'modeled', evidenceKinds: ['capability'] }],
              },
            ],
          },
        },
        'features.invalid_stage',
      ],
      [
        'requirement rejects capability evidence',
        {
          ...base,
          authored: {
            features: [
              {
                id: 'conformance/generated-evidence',
                requirements: [{ id: 'implementation', stage: 'coded', evidenceKinds: ['attestation'] }],
              },
            ],
          },
        },
        'features.invalid_subject',
      ],
      [
        'unpublished contribution',
        { ...base, mappings: [{ ...base.mappings[0]!, key: 'never-emitted' }] },
        'capabilities.unresolved_reference',
      ],
      [
        // Published in this manifest, but owned by a dependency. The producer
        // stamps its own project, so the identity cannot resolve.
        'dependency-owned contribution',
        {
          ...base,
          mappings: [
            {
              feature: 'conformance/generated-evidence',
              requirement: 'implementation',
              kind: 'package',
              key: 'go.putnami.dev/http',
              provenance: { root: 'project', path: 'receiver.go', symbol: 'ReceiverFeature' },
            },
          ],
        },
        'capabilities.unresolved_reference',
      ],
      [
        'non-producer issuer',
        { ...base, issuer: { kind: 'framework', id: 'putnami.conformance/producer' } },
        'features.invalid_authority',
      ],
      [
        'unavailable source binding',
        { ...base, source: { ...base.source, binding: '' } },
        'features.invalid_source_binding',
      ],
      [
        'provenance outside the source root',
        {
          ...base,
          mappings: [{ ...base.mappings[0]!, provenance: { root: 'workspace', path: 'receiver.go' } }],
        },
        'features.invalid_subject',
      ],
      [
        'escaping provenance path',
        {
          ...base,
          mappings: [{ ...base.mappings[0]!, provenance: { root: 'project', path: '../outside.go' } }],
        },
        'features.path_escape',
      ],
    ];
    for (const [label, input, code] of cases) {
      const { document, diagnostics } = buildGeneratedEvidence(input);
      expect(document, label).toBeUndefined();
      expect(
        diagnostics.map(({ code: value }) => value),
        label,
      ).toContain(code);
    }
  });

  it('reads the stage from the authored requirement rather than assuming coded', () => {
    const base = scenario();
    const { document, diagnostics } = buildGeneratedEvidence({
      ...base,
      authored: {
        features: [
          {
            id: 'conformance/generated-evidence',
            requirements: [{ id: 'implementation', stage: 'wired', evidenceKinds: ['capability'] }],
          },
        ],
      },
    });
    expect(diagnostics).toEqual([]);
    expect((document?.evidence ?? []).map((record) => record.stage)).toEqual(['wired', 'wired']);
  });

  it('publishes an empty document when nothing is mapped', () => {
    const { document, diagnostics } = buildGeneratedEvidence({ ...scenario(), mappings: [] });
    expect(diagnostics).toEqual([]);
    expect(document?.evidence).toEqual([]);
  });
});
