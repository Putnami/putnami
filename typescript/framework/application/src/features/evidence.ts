import { Buffer } from 'node:buffer';
import type { ContributionReference } from '../capabilities/manifest.types';
import type {
  EvidenceProvenance,
  EvidenceSourceSelector,
  EvidenceSubject,
  FeatureEvidenceDocument,
  FeatureEvidenceRecord,
  MaturityStage,
} from './evidence.types';

export const FEATURE_EVIDENCE_SCHEMA_URL = 'https://putnami.dev/schemas/putnami-feature-evidence.json';
export const FEATURE_EVIDENCE_PROTOCOL_VERSION = 1 as const;
export const FEATURE_EVIDENCE_DIRECTORY = 'schema/feature-evidence';
export const FEATURE_EVIDENCE_EMIT_DIRECTORY = '.gen/schema/feature-evidence';

const STAGES: MaturityStage[] = [
  'modeled',
  'coded',
  'wired',
  'default-on',
  'live-verified',
  'design-partner-proven',
  'ga',
];
const FEATURE_ID = /^[a-z0-9]+(?:-[a-z0-9]+)*(?:\/[a-z0-9]+(?:-[a-z0-9]+)*)+$/;
const SEGMENT_ID = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;
const SEMANTIC_CODE = /^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$/;
const SOURCE_BINDING = /^source-v1:sha256:[0-9a-f]{64}$/;
const SHA256 = /^sha256:[0-9a-f]{64}$/;
const REVISION = /^git:(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const SCHEME = /^[A-Za-z][A-Za-z0-9+.-]*:/;
const ROOTS = new Set(['workspace', 'project', 'package']);
const OUTCOMES = new Set(['supports', 'contradicts']);
const ISSUERS = new Set(['framework', 'build', 'test', 'delivery', 'runtime', 'human']);
const CONTRIBUTION_KINDS = new Set([
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
const PRODUCT_PROMISES = new Set(['adoption', 'support', 'availability', 'customer-proof']);
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

export interface FeatureDiagnostic {
  severity: 'error';
  code: string;
  field: string;
  message: string;
}

export class FeatureEvidenceValidationError extends Error {
  readonly diagnostics: FeatureDiagnostic[];

  constructor(diagnostics: FeatureDiagnostic[]) {
    super(
      `feature evidence validation failed:\n${diagnostics
        .map(({ code, field, message }) => `[${code}] ${field}: ${message}`)
        .join('\n')}`,
    );
    this.name = diagnostics[0]?.code ?? 'FeatureEvidenceValidationError';
    this.diagnostics = diagnostics;
  }
}

function cmp(left: string, right: string): number {
  return Buffer.compare(Buffer.from(left, 'utf8'), Buffer.from(right, 'utf8'));
}

function finding(code: string, field: string, message: string): FeatureDiagnostic {
  return { severity: 'error', code, field, message };
}

function bounded(value: unknown, maximum: number): value is string {
  if (typeof value !== 'string' || value.trim().length === 0 || Buffer.byteLength(value, 'utf8') > maximum) {
    return false;
  }
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0;
    if (code < 0x20 || code === 0x7f) return false;
  }
  return true;
}

function validatePath(field: string, value: unknown, diagnostics: FeatureDiagnostic[]): void {
  if (
    typeof value !== 'string' ||
    value.length === 0 ||
    value.includes('\\') ||
    value.includes('\0') ||
    value.startsWith('/') ||
    SCHEME.test(value)
  ) {
    diagnostics.push(finding('features.invalid_path', field, 'path must be a non-empty contained relative slash path'));
    return;
  }
  const segments = value.split('/');
  if (segments.includes('..')) {
    diagnostics.push(finding('features.path_escape', field, 'path must remain within its declared source root'));
  } else if (segments.some((segment) => segment === '' || segment === '.')) {
    diagnostics.push(finding('features.invalid_path', field, 'path must be its slash-separated lexical clean form'));
  }
}

function validateContribution(field: string, value: ContributionReference, diagnostics: FeatureDiagnostic[]): void {
  if (!bounded(value?.ownerProject, 256) || !CONTRIBUTION_KINDS.has(value?.kind)) {
    diagnostics.push(finding('features.invalid_subject', field, 'capability contribution identity is invalid'));
    return;
  }
  if (!bounded(value.key, 256)) {
    diagnostics.push(finding('features.invalid_subject', `${field}.key`, 'contribution key is required'));
  }
  const needsSubkind = !['config', 'package', 'requiredCapability'].includes(value.kind);
  if ((needsSubkind && !bounded(value.subkind, 128)) || (!needsSubkind && value.subkind)) {
    diagnostics.push(
      finding(
        'features.invalid_subject',
        `${field}.subkind`,
        `contribution subkind is ${needsSubkind ? 'required' : 'not allowed'}`,
      ),
    );
  }
}

function validateSource(field: string, source: EvidenceSourceSelector, diagnostics: FeatureDiagnostic[]): void {
  if (!ROOTS.has(source?.root))
    diagnostics.push(finding('features.invalid_subject', `${field}.root`, 'source root is not supported'));
  if (source?.root === 'workspace' && (source.ownerProject || source.package || source.version)) {
    diagnostics.push(
      finding('features.invalid_subject', field, 'workspace source must omit ownerProject, package, and version'),
    );
  }
  if (
    source?.root === 'project' &&
    (!bounded(source.ownerProject, 256) || source.package !== undefined || source.version !== undefined)
  ) {
    diagnostics.push(
      finding(
        'features.invalid_subject',
        field,
        'project source requires ownerProject and must omit package and version',
      ),
    );
  }
  if (
    source?.root === 'package' &&
    (!bounded(source.package, 256) || !bounded(source.version, 128) || source.ownerProject !== undefined)
  ) {
    diagnostics.push(finding('features.invalid_subject', field, 'package source requires exact package and version'));
  }
  if (!SOURCE_BINDING.test(source?.binding ?? '')) {
    diagnostics.push(
      finding(
        'features.invalid_source_binding',
        `${field}.binding`,
        'source binding must use source-v1:sha256:<64-lower-hex>',
      ),
    );
  }
  if (source?.environment && (source.environment.length > 128 || !SEMANTIC_CODE.test(source.environment))) {
    diagnostics.push(
      finding(
        'features.invalid_subject',
        `${field}.environment`,
        'environment must be a bounded lower-case semantic code',
      ),
    );
  }
}

function validateSubject(
  field: string,
  subject: EvidenceSubject,
  issuerKind: string,
  diagnostics: FeatureDiagnostic[],
): void {
  if (!subject || !['capability', 'artifact', 'attestation'].includes(subject.kind)) {
    diagnostics.push(finding('features.invalid_subject', field, 'subject must name one supported kind'));
    return;
  }
  const raw = subject as unknown as Record<string, unknown>;
  const payloads = ['contribution', 'artifact', 'attestation'].filter((key) => raw[key] !== undefined);
  if (payloads.length !== 1 || payloads[0] !== (subject.kind === 'capability' ? 'contribution' : subject.kind)) {
    diagnostics.push(
      finding('features.invalid_subject', field, 'subject must carry exactly the payload selected by kind'),
    );
    return;
  }
  if (subject.kind === 'capability') validateContribution(`${field}.contribution`, subject.contribution, diagnostics);
  if (subject.kind === 'artifact') {
    validatePath(`${field}.artifact.path`, subject.artifact.path, diagnostics);
    if (!SHA256.test(subject.artifact.digest)) {
      diagnostics.push(
        finding(
          'features.invalid_subject',
          `${field}.artifact.digest`,
          'artifact digest must use sha256:<64-lower-hex>',
        ),
      );
    }
  }
  if (subject.kind === 'attestation') {
    const claim = subject.attestation.claim;
    if (typeof claim !== 'string' || claim.length > 128 || !SEMANTIC_CODE.test(claim)) {
      diagnostics.push(
        finding(
          'features.invalid_subject',
          `${field}.attestation.claim`,
          'attestation claim must be a bounded lower-case semantic code',
        ),
      );
    }
    if (PRODUCT_PROMISES.has(claim) && issuerKind !== 'human') {
      diagnostics.push(
        finding(
          'features.invalid_authority',
          `${field}.attestation.claim`,
          'product-promise attestations require a human issuer',
        ),
      );
    }
  }
}

function validateProvenance(
  field: string,
  provenance: EvidenceProvenance,
  source: EvidenceSourceSelector,
  diagnostics: FeatureDiagnostic[],
): void {
  if (!ROOTS.has(provenance?.root))
    diagnostics.push(finding('features.invalid_subject', `${field}.root`, 'provenance root is not supported'));
  if (provenance?.root !== source?.root) {
    diagnostics.push(finding('features.invalid_subject', `${field}.root`, 'provenance root must equal source root'));
  }
  validatePath(`${field}.path`, provenance?.path, diagnostics);
  if (provenance?.symbol !== undefined && !bounded(provenance.symbol, 512)) {
    diagnostics.push(finding('features.invalid_subject', `${field}.symbol`, 'provenance symbol must be bounded text'));
  }
}

/** Validate the durable local feature-evidence document contract. */
export function validateFeatureEvidenceDocument(document: FeatureEvidenceDocument): FeatureDiagnostic[] {
  const diagnostics: FeatureDiagnostic[] = [];
  if (document?.protocolVersion !== 1) {
    diagnostics.push(finding('features.invalid_protocol_version', 'protocolVersion', 'protocolVersion must equal 1'));
  }
  if (!Array.isArray(document?.evidence)) {
    diagnostics.push(finding('features.parse_error', 'evidence', 'evidence is required and must be an array'));
    return diagnostics;
  }
  const seen = new Map<string, string>();
  for (const [index, record] of canonicalFeatureEvidenceDocument(document).evidence.entries()) {
    const field = `evidence[${index}]`;
    if (!FEATURE_ID.test(record.id))
      diagnostics.push(finding('features.invalid_id', `${field}.id`, 'evidence ID is not canonical'));
    if (!FEATURE_ID.test(record.feature))
      diagnostics.push(finding('features.unknown_feature', `${field}.feature`, 'feature reference is not canonical'));
    if (!SEGMENT_ID.test(record.requirement))
      diagnostics.push(
        finding('features.unknown_requirement', `${field}.requirement`, 'requirement reference is not canonical'),
      );
    if (!STAGES.includes(record.stage) || record.stage === 'modeled')
      diagnostics.push(finding('features.invalid_stage', `${field}.stage`, 'evidence stage must be post-modeled'));
    if (!OUTCOMES.has(record.outcome))
      diagnostics.push(finding('features.invalid_subject', `${field}.outcome`, 'evidence outcome is not supported'));
    if (!ISSUERS.has(record.issuer?.kind) || !bounded(record.issuer?.id, 256))
      diagnostics.push(
        finding('features.invalid_authority', `${field}.issuer`, 'issuer must have a supported kind and bounded ID'),
      );
    validateSource(`${field}.source`, record.source, diagnostics);
    validateSubject(`${field}.subject`, record.subject, record.issuer?.kind, diagnostics);
    validateProvenance(`${field}.provenance`, record.provenance, record.source, diagnostics);
    if (record.persistent && (record.issuer?.kind !== 'human' || record.subject?.kind !== 'attestation'))
      diagnostics.push(
        finding(
          'features.invalid_authority',
          `${field}.persistent`,
          'persistent validity is reserved for human-issued attestations',
        ),
      );
    if (
      record.observedAt !== undefined &&
      (!RFC3339.test(record.observedAt) || Number.isNaN(Date.parse(record.observedAt)))
    )
      diagnostics.push(
        finding('features.invalid_subject', `${field}.observedAt`, 'observedAt must be a valid RFC 3339 timestamp'),
      );
    if (record.observedAt !== undefined && record.subject?.kind !== 'attestation')
      diagnostics.push(
        finding(
          'features.invalid_subject',
          `${field}.observedAt`,
          'observedAt is allowed only for intrinsically temporal attestation subjects',
        ),
      );
    if (record.observedRepositoryRevision !== undefined && !REVISION.test(record.observedRepositoryRevision))
      diagnostics.push(
        finding(
          'features.invalid_subject',
          `${field}.observedRepositoryRevision`,
          'observed repository revision must use git:<40-or-64-lower-hex>',
        ),
      );
    const first = seen.get(record.id);
    if (first)
      diagnostics.push(
        finding('features.duplicate_evidence', `${field}.id`, `evidence is duplicated; first declaration is ${first}`),
      );
    else seen.set(record.id, field);
  }
  return diagnostics.sort(
    (left, right) => cmp(left.code, right.code) || cmp(left.field, right.field) || cmp(left.message, right.message),
  );
}

function rank(stage: MaturityStage): number {
  const value = STAGES.indexOf(stage);
  return value < 0 ? Number.MAX_SAFE_INTEGER : value;
}

/** Return a canonical sorted copy without mutating caller-owned evidence. */
export function canonicalFeatureEvidenceDocument(document: FeatureEvidenceDocument): FeatureEvidenceDocument {
  return {
    $schema: document.$schema,
    protocolVersion: 1,
    evidence: [...(document.evidence ?? [])].sort(
      (left, right) =>
        cmp(left.feature, right.feature) ||
        rank(left.stage) - rank(right.stage) ||
        cmp(left.requirement, right.requirement) ||
        cmp(left.id, right.id),
    ),
  };
}

function escapeLikeGo(json: string): string {
  return json.replace(/[<>&\u2028\u2029]/g, (character) => {
    if (character === '<') return '\\u003c';
    if (character === '>') return '\\u003e';
    if (character === '&') return '\\u0026';
    if (character === '\u2028') return '\\u2028';
    return '\\u2029';
  });
}

function recordOut(record: FeatureEvidenceRecord): Record<string, unknown> {
  const source = record.source;
  const subject = record.subject;
  return {
    id: record.id,
    feature: record.feature,
    requirement: record.requirement,
    stage: record.stage,
    outcome: record.outcome,
    issuer: { kind: record.issuer.kind, id: record.issuer.id },
    source: {
      root: source.root,
      ownerProject: source.ownerProject || undefined,
      package: source.package || undefined,
      version: source.version || undefined,
      binding: source.binding,
      environment: source.environment || undefined,
    },
    subject:
      subject.kind === 'capability'
        ? {
            kind: subject.kind,
            contribution: {
              ownerProject: subject.contribution.ownerProject,
              kind: subject.contribution.kind,
              subkind: subject.contribution.subkind || undefined,
              key: subject.contribution.key,
            },
          }
        : subject.kind === 'artifact'
          ? { kind: subject.kind, artifact: { path: subject.artifact.path, digest: subject.artifact.digest } }
          : { kind: subject.kind, attestation: { claim: subject.attestation.claim } },
    provenance: {
      root: record.provenance.root,
      path: record.provenance.path,
      symbol: record.provenance.symbol || undefined,
    },
    persistent: record.persistent ? true : undefined,
    observedAt: record.observedAt || undefined,
    observedRepositoryRevision: record.observedRepositoryRevision || undefined,
  };
}

/** Canonical Go-compatible feature-evidence JSON bytes represented as a string. */
export function serializeFeatureEvidenceDocument(document: FeatureEvidenceDocument): string {
  const canonical = canonicalFeatureEvidenceDocument(document);
  return `${escapeLikeGo(
    JSON.stringify(
      {
        $schema: canonical.$schema || undefined,
        protocolVersion: canonical.protocolVersion,
        evidence: canonical.evidence.map(recordOut),
      },
      null,
      2,
    ),
  )}\n`;
}
