import { createHash } from 'node:crypto';
import type { ContributionIdentity, ContributionKind } from '../capabilities/manifest.types';
import {
  FEATURE_EVIDENCE_PROTOCOL_VERSION,
  FEATURE_EVIDENCE_SCHEMA_URL,
  canonicalFeatureEvidenceDocument,
  validateFeatureEvidenceDocument,
} from './evidence';
import type { FeatureDiagnostic } from './evidence';
import type {
  EvidenceIssuer,
  EvidenceKind,
  EvidenceProvenance,
  EvidenceSourceSelector,
  FeatureEvidenceDocument,
  FeatureEvidenceRecord,
  MaturityStage,
} from './evidence.types';

/**
 * Port of `features.BuildGeneratedEvidence`
 * (`protocols/features/generated_evidence.go`). The association rules,
 * derivation, and identity live in one algorithm implemented twice, and the
 * shared `generated-evidence.golden.json` conformance vector pins the two
 * against each other. Change one side and the vector fails in the other.
 *
 * See `protocols/features/doc/adr/0003-generated-feature-evidence.md`.
 */

/** How much of the contribution digest enters a generated evidence ID. */
const EVIDENCE_ID_DIGEST_LENGTH = 12;

const STAGES: readonly MaturityStage[] = [
  'modeled',
  'coded',
  'wired',
  'default-on',
  'live-verified',
  'design-partner-proven',
  'ga',
];

const CONTRIBUTION_KINDS = new Set<ContributionKind>([
  'config',
  'schema',
  'discoverer',
  'migration',
  'infra',
  'health',
  'lifecycle',
  'package',
  'requiredCapability',
]);

/** Kinds whose identity carries no subkind, matching the capability contract. */
const KINDS_WITHOUT_SUBKIND = new Set<ContributionKind>(['config', 'package', 'requiredCapability']);

/** One authored maturity requirement, as read from `putnami.features.json`. */
export interface AuthoredRequirement {
  readonly id: string;
  readonly stage: MaturityStage;
  readonly evidenceKinds: readonly EvidenceKind[];
}

/** One authored feature, reduced to what the generated-evidence resolver needs. */
export interface AuthoredFeature {
  readonly id: string;
  readonly requirements?: readonly AuthoredRequirement[];
}

/** Durable authored intent for the producing project. */
export interface AuthoredFeatureCatalog {
  readonly features: readonly AuthoredFeature[];
}

/**
 * One explicit association between an authored feature requirement and one
 * exact canonical contribution.
 *
 * It carries no owner project: the emitting producer owns exactly one project
 * and stamps it, so a workload cannot claim a contribution belonging to one of
 * its dependencies.
 */
export interface GeneratedEvidenceMapping {
  readonly feature: string;
  readonly requirement: string;
  readonly kind: ContributionKind;
  readonly subkind?: string;
  readonly key: string;
  readonly provenance: EvidenceProvenance;
}

/** Everything one build or test producer needs to publish its mappings. */
export interface GeneratedEvidenceInput {
  readonly issuer: EvidenceIssuer;
  readonly source: EvidenceSourceSelector;
  readonly authored?: AuthoredFeatureCatalog;
  readonly contributions: readonly ContributionIdentity[];
  readonly mappings: readonly GeneratedEvidenceMapping[];
}

export interface GeneratedEvidenceResult {
  /** The canonical document, or undefined when publication was refused. */
  readonly document?: FeatureEvidenceDocument;
  readonly diagnostics: readonly FeatureDiagnostic[];
}

function finding(code: string, field: string, message: string): FeatureDiagnostic {
  return { code, field, message, severity: 'error' };
}

function identityKey(identity: ContributionIdentity): string {
  // NUL-joined, matching features.contributionIdentityKey byte for byte: the
  // digest derived from this string is protocol identity, so the separator is
  // part of the wire contract rather than a formatting choice.
  return [identity.ownerProject, identity.kind, identity.subkind ?? '', identity.key].join('\u0000');
}

function identityLabel(identity: ContributionIdentity): string {
  return `(${[identity.ownerProject, identity.kind, identity.subkind ?? '', identity.key].join(',')})`;
}

/**
 * Derive the stable identity of one generated record.
 *
 * A pure function of the association, so declaration order, plugin discovery
 * order, and module composition order cannot reach it. The contribution is
 * folded into a digest because a contribution key is free semantic text and the
 * ID grammar accepts only lower-case kebab segments; the exact identity stays
 * readable in the record's own `subject.contribution`.
 */
export function generatedEvidenceId(feature: string, requirement: string, contribution: ContributionIdentity): string {
  const digest = createHash('sha256').update(identityKey(contribution), 'utf8').digest('hex');
  return `${feature}/${requirement}/${digest.slice(0, EVIDENCE_ID_DIGEST_LENGTH)}`;
}

function compareMappings(left: GeneratedEvidenceMapping, right: GeneratedEvidenceMapping): number {
  const pairs: [string, string][] = [
    [left.feature, right.feature],
    [left.requirement, right.requirement],
    [left.kind, right.kind],
    [left.subkind ?? '', right.subkind ?? ''],
    [left.key, right.key],
    [left.provenance?.root ?? '', right.provenance?.root ?? ''],
    [left.provenance?.path ?? '', right.provenance?.path ?? ''],
    [left.provenance?.symbol ?? '', right.provenance?.symbol ?? ''],
  ];
  for (const [a, b] of pairs) {
    if (a < b) return -1;
    if (a > b) return 1;
  }
  return 0;
}

function validateContributionReference(
  contribution: ContributionIdentity,
  field: string,
  diagnostics: FeatureDiagnostic[],
): void {
  if (!contribution.ownerProject.trim()) {
    diagnostics.push(
      finding('capabilities.missing_owner_project', `${field}.contribution.ownerProject`, 'ownerProject is required'),
    );
  }
  if (!CONTRIBUTION_KINDS.has(contribution.kind)) {
    diagnostics.push(
      finding(
        'capabilities.invalid_contribution_identity',
        `${field}.contribution.kind`,
        `contribution kind ${JSON.stringify(contribution.kind)} is not in the v2 set`,
      ),
    );
  }
  if (!contribution.key.trim()) {
    diagnostics.push(
      finding(
        'capabilities.invalid_contribution_identity',
        `${field}.contribution.key`,
        'contribution key is required',
      ),
    );
  }
  const requiresSubkind = !KINDS_WITHOUT_SUBKIND.has(contribution.kind);
  const subkind = contribution.subkind ?? '';
  if (requiresSubkind && !subkind.trim()) {
    diagnostics.push(
      finding(
        'capabilities.invalid_contribution_identity',
        `${field}.contribution.subkind`,
        `subkind is required for contribution kind ${JSON.stringify(contribution.kind)}`,
      ),
    );
  }
  if (!requiresSubkind && subkind !== '') {
    diagnostics.push(
      finding(
        'capabilities.invalid_contribution_identity',
        `${field}.contribution.subkind`,
        `subkind must be absent for contribution kind ${JSON.stringify(contribution.kind)}`,
      ),
    );
  }
}

/**
 * Read the stage and accepted subject kinds from the authored requirement. A
 * producer never chooses them: choosing the stage it proves and the proof at
 * once is exactly the self-certification the evidence contract prevents.
 */
function resolveRequirement(
  field: string,
  authored: AuthoredFeatureCatalog | undefined,
  mapping: GeneratedEvidenceMapping,
): { requirement?: AuthoredRequirement; diagnostics: FeatureDiagnostic[] } {
  if (!authored) {
    return {
      diagnostics: [
        finding(
          'features.unknown_feature',
          `${field}.feature`,
          `mapping references feature ${JSON.stringify(mapping.feature)} but the project authors no feature manifest`,
        ),
      ],
    };
  }
  const matches = authored.features.filter((feature) => feature.id === mapping.feature);
  if (matches.length !== 1) {
    return {
      diagnostics: [
        finding(
          'features.unknown_feature',
          `${field}.feature`,
          `mapping references feature ${JSON.stringify(mapping.feature)}, which the authored manifest does not declare exactly once`,
        ),
      ],
    };
  }
  const requirement = (matches[0]?.requirements ?? []).find((candidate) => candidate.id === mapping.requirement);
  if (!requirement) {
    return {
      diagnostics: [
        finding(
          'features.unknown_requirement',
          `${field}.requirement`,
          `mapping references requirement ${JSON.stringify(mapping.requirement)}, which feature ${JSON.stringify(mapping.feature)} does not declare`,
        ),
      ],
    };
  }
  const stageRank = STAGES.indexOf(requirement.stage);
  if (stageRank <= 0) {
    return {
      diagnostics: [
        finding(
          'features.invalid_stage',
          `${field}.requirement`,
          `requirement ${JSON.stringify(mapping.requirement)} declares stage ${JSON.stringify(requirement.stage)}, which cannot be supported by evidence`,
        ),
      ],
    };
  }
  if (!(requirement.evidenceKinds ?? []).includes('capability')) {
    return {
      diagnostics: [
        finding(
          'features.invalid_subject',
          `${field}.requirement`,
          `requirement ${JSON.stringify(mapping.requirement)} does not accept capability evidence`,
        ),
      ],
    };
  }
  return { requirement, diagnostics: [] };
}

function sameRecord(left: FeatureEvidenceRecord, right: FeatureEvidenceRecord): boolean {
  return JSON.stringify(left) === JSON.stringify(right);
}

/**
 * Resolve explicit mappings into one canonical evidence document, or report why
 * they cannot be published.
 *
 * Nothing is inferred. A mapping publishes only when its feature, requirement,
 * and contribution all already exist, the requirement accepts capability
 * evidence, and the contribution belongs to the producing project. Any other
 * outcome returns diagnostics and no document: publishing the records that
 * happened to resolve would leave a producer's generated file claiming less
 * than it did last run while looking like a complete result.
 */
export function buildGeneratedEvidence(input: GeneratedEvidenceInput): GeneratedEvidenceResult {
  const diagnostics: FeatureDiagnostic[] = [];
  if (input.issuer?.kind !== 'build' && input.issuer?.kind !== 'test') {
    diagnostics.push(
      finding(
        'features.invalid_authority',
        'issuer.kind',
        `generated evidence must be issued by a build or test producer, not ${JSON.stringify(input.issuer?.kind)}`,
      ),
    );
  }
  // Narrower than Go, which runs the protocol's full selector validation here
  // and so reports extra diagnostics on inputs this producer never constructs.
  // ownerProject and binding are computed from the canonical manifest and the
  // build's package metadata, and the document validator in evidence.ts
  // enforces the whole selector contract on every read — so the publish/refuse
  // outcome cannot diverge, and only the root, the one field that changes what
  // the evidence means, is worth refusing at this seam.
  if (input.source?.root !== 'project') {
    diagnostics.push(
      finding(
        'features.invalid_subject',
        'source.root',
        'generated capability evidence is interpreted against the owning project root',
      ),
    );
  }

  const published = new Set(input.contributions.map((identity) => identityKey(identity)));
  const records = new Map<string, FeatureEvidenceRecord>();

  for (const mapping of [...input.mappings].sort(compareMappings)) {
    const field = `mappings[${mapping.feature}/${mapping.requirement}]`;
    const contribution: ContributionIdentity = {
      ownerProject: input.source?.ownerProject ?? '',
      kind: mapping.kind,
      ...(mapping.subkind ? { subkind: mapping.subkind } : {}),
      key: mapping.key,
    };
    validateContributionReference(contribution, field, diagnostics);
    const resolved = resolveRequirement(field, input.authored, mapping);
    diagnostics.push(...resolved.diagnostics);
    if (!published.has(identityKey(contribution))) {
      diagnostics.push(
        finding(
          'capabilities.unresolved_reference',
          `${field}.contribution`,
          `no published contribution resolves identity ${identityLabel(contribution)}`,
        ),
      );
      continue;
    }
    if (!resolved.requirement) continue;

    const record: FeatureEvidenceRecord = {
      id: generatedEvidenceId(mapping.feature, mapping.requirement, contribution),
      feature: mapping.feature,
      requirement: mapping.requirement,
      stage: resolved.requirement.stage,
      outcome: 'supports',
      issuer: { kind: input.issuer.kind, id: input.issuer.id },
      source: { ...input.source },
      subject: { kind: 'capability', contribution },
      provenance: { ...mapping.provenance },
    };
    const existing = records.get(record.id);
    if (existing) {
      if (!sameRecord(existing, record)) {
        diagnostics.push(
          finding(
            'features.duplicate_evidence',
            field,
            `mapping duplicates evidence ${JSON.stringify(record.id)} with different content`,
          ),
        );
      }
      continue;
    }
    records.set(record.id, record);
  }

  const document = canonicalFeatureEvidenceDocument({
    $schema: FEATURE_EVIDENCE_SCHEMA_URL,
    protocolVersion: FEATURE_EVIDENCE_PROTOCOL_VERSION,
    evidence: [...records.values()],
  });
  diagnostics.push(...validateFeatureEvidenceDocument(document));
  const sorted = diagnostics.sort(
    (left, right) =>
      (left.code < right.code ? -1 : left.code > right.code ? 1 : 0) ||
      (left.field < right.field ? -1 : left.field > right.field ? 1 : 0) ||
      (left.message < right.message ? -1 : left.message > right.message ? 1 : 0),
  );
  if (sorted.length > 0) return { diagnostics: sorted };
  return { document, diagnostics: sorted };
}
