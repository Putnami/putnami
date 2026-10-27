import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { serializeFeatureEvidenceDocument, validateFeatureEvidenceDocument } from '../../src/features/evidence';
import type { FeatureEvidenceDocument } from '../../src/features/evidence.types';

const EQUIVALENCE_FIXTURE = join(
  import.meta.dir,
  '../../../../../protocols/features/fixtures/equivalence/go-typescript-evidence.golden.json',
);

describe('feature evidence protocol', () => {
  specTest(
    'validates and reproduces the shared Go/TypeScript bytes',
    {
      feature: 'features/feature-contracts',
      requirement: 'canonical-bytes-across-languages',
      check: 'typescript-reproduces-the-evidence-golden',
    },
    () => {
      const bytes = readFileSync(EQUIVALENCE_FIXTURE, 'utf8');
      const document = JSON.parse(bytes) as FeatureEvidenceDocument;
      expect(validateFeatureEvidenceDocument(document)).toEqual([]);
      expect(serializeFeatureEvidenceDocument(document)).toBe(bytes);

      document.evidence.reverse();
      expect(serializeFeatureEvidenceDocument(document)).toBe(bytes);
    },
  );

  it('rejects invalid source authority and contribution identities deterministically', () => {
    const document = JSON.parse(readFileSync(EQUIVALENCE_FIXTURE, 'utf8')) as FeatureEvidenceDocument;
    const capability = document.evidence.find(({ subject }) => subject.kind === 'capability');
    if (capability?.subject.kind !== 'capability') throw new Error('missing capability fixture');
    capability.source.binding = 'branch:main';
    capability.subject.contribution.subkind = 'unexpected';

    const signature = validateFeatureEvidenceDocument(document).map(({ code, field }) => `${code}|${field}`);
    expect(signature).toContain('features.invalid_source_binding|evidence[0].source.binding');
    expect(signature).toContain('features.invalid_subject|evidence[0].subject.contribution.subkind');
    expect(validateFeatureEvidenceDocument(document).map(({ code, field }) => `${code}|${field}`)).toEqual(signature);
  });

  it('names a finding by the record position in the file, not in canonical order', () => {
    const document = JSON.parse(readFileSync(EQUIVALENCE_FIXTURE, 'utf8')) as FeatureEvidenceDocument;
    document.evidence[0]!.feature = 'zeta/feature';
    document.evidence[0]!.stage = 'unknown' as FeatureEvidenceDocument['evidence'][number]['stage'];
    document.evidence.push(document.evidence[1]!);

    const fields = validateFeatureEvidenceDocument(document).map(({ code, field }) => `${code}|${field}`);
    expect(fields).toContain('features.invalid_stage|evidence[0].stage');
    expect(fields).toContain('features.duplicate_evidence|evidence[4].id');
    expect(validateFeatureEvidenceDocument(document).find(({ field }) => field === 'evidence[4].id')?.message).toBe(
      'evidence is duplicated; first declaration is evidence[1]',
    );
  });

  it('requires full RFC 3339 timestamps and attestation subjects for observations', () => {
    const document = JSON.parse(readFileSync(EQUIVALENCE_FIXTURE, 'utf8')) as FeatureEvidenceDocument;
    document.evidence[0]!.observedAt = '2026-08-10';

    expect(
      validateFeatureEvidenceDocument(document)
        .filter(({ field }) => field.endsWith('.observedAt'))
        .map(({ message }) => message),
    ).toEqual([
      'observedAt is allowed only for intrinsically temporal attestation subjects',
      'observedAt must be a valid RFC 3339 timestamp',
    ]);
  });
});
