import { readFileSync, statSync } from 'node:fs';
import { joinPath } from '@putnami/utils';
import type { AuthoredFeature, AuthoredFeatureCatalog, AuthoredRequirement } from './generated-evidence';
import type { EvidenceKind, MaturityStage } from './evidence.types';

/** Canonical discovery location of durable authored feature intent. */
export const FEATURE_MANIFEST_FILENAME = 'putnami.features.json';

/**
 * The authored manifest is a small semantic catalog. Anything larger is a
 * defect rather than a scale requirement, and describe must not become a path
 * for reading arbitrarily large project files into memory.
 */
const MAX_MANIFEST_BYTES = 1 << 20;

const STAGES = new Set<MaturityStage>([
  'modeled',
  'coded',
  'wired',
  'default-on',
  'live-verified',
  'design-partner-proven',
  'ga',
]);

const EVIDENCE_KINDS = new Set<EvidenceKind>(['capability', 'artifact', 'attestation']);

/**
 * Read `putnami.features.json` from a project root, reduced to what the
 * generated-evidence resolver needs.
 *
 * Absence returns undefined rather than throwing: that makes every mapping an
 * unknown-feature failure in the resolver, which is a clearer report than "no
 * manifest" from a project that declared no proof at all.
 *
 * This reader is deliberately narrow. It is not a second interpretation of the
 * manifest wire — `putnami features validate` owns strict, workspace-wide
 * validation. It refuses only what it cannot interpret without guessing, so a
 * shape it does not understand can never widen what a producer publishes.
 */
export function readAuthoredFeatureManifest(projectRoot: string): AuthoredFeatureCatalog | undefined {
  const path = joinPath(projectRoot, FEATURE_MANIFEST_FILENAME);
  let size: number;
  try {
    const info = statSync(path);
    if (!info.isFile()) {
      throw new Error(`authored feature manifest ${FEATURE_MANIFEST_FILENAME} is not a regular file`);
    }
    size = info.size;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
    throw error;
  }
  if (size > MAX_MANIFEST_BYTES) {
    throw new Error(`authored feature manifest ${FEATURE_MANIFEST_FILENAME} exceeds the bounded describe read limit`);
  }

  const raw: unknown = JSON.parse(readFileSync(path, 'utf8'));
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new Error(`authored feature manifest ${FEATURE_MANIFEST_FILENAME} is not a JSON object`);
  }
  const document = raw as { protocolVersion?: unknown; features?: unknown };
  if (document.protocolVersion !== 1 && document.protocolVersion !== 2) {
    throw new Error(
      `authored feature manifest ${FEATURE_MANIFEST_FILENAME} declares unsupported protocolVersion ${JSON.stringify(document.protocolVersion)}`,
    );
  }
  if (!Array.isArray(document.features)) {
    throw new Error(`authored feature manifest ${FEATURE_MANIFEST_FILENAME} must declare a features array`);
  }

  const features: AuthoredFeature[] = [];
  for (const [index, entry] of document.features.entries()) {
    if (typeof entry !== 'object' || entry === null || Array.isArray(entry)) {
      throw new Error(`${FEATURE_MANIFEST_FILENAME} features[${index}] must be an object`);
    }
    const feature = entry as { id?: unknown; requirements?: unknown };
    if (typeof feature.id !== 'string' || !feature.id) {
      throw new Error(`${FEATURE_MANIFEST_FILENAME} features[${index}].id must be a non-empty string`);
    }
    features.push({ id: feature.id, requirements: readRequirements(index, feature.requirements) });
  }
  return { features };
}

function readRequirements(featureIndex: number, raw: unknown): AuthoredRequirement[] {
  if (raw === undefined) return [];
  if (!Array.isArray(raw)) {
    throw new Error(`${FEATURE_MANIFEST_FILENAME} features[${featureIndex}].requirements must be an array`);
  }
  return raw.map((entry, index) => {
    const field = `${FEATURE_MANIFEST_FILENAME} features[${featureIndex}].requirements[${index}]`;
    if (typeof entry !== 'object' || entry === null || Array.isArray(entry)) {
      throw new Error(`${field} must be an object`);
    }
    const requirement = entry as { id?: unknown; stage?: unknown; evidenceKinds?: unknown };
    if (typeof requirement.id !== 'string' || !requirement.id) {
      throw new Error(`${field}.id must be a non-empty string`);
    }
    if (typeof requirement.stage !== 'string' || !STAGES.has(requirement.stage as MaturityStage)) {
      throw new Error(`${field}.stage ${JSON.stringify(requirement.stage)} is not a supported maturity stage`);
    }
    if (!Array.isArray(requirement.evidenceKinds) || requirement.evidenceKinds.length === 0) {
      throw new Error(`${field}.evidenceKinds must be a non-empty array`);
    }
    for (const kind of requirement.evidenceKinds) {
      if (typeof kind !== 'string' || !EVIDENCE_KINDS.has(kind as EvidenceKind)) {
        throw new Error(`${field}.evidenceKinds contains unsupported kind ${JSON.stringify(kind)}`);
      }
    }
    return {
      id: requirement.id,
      stage: requirement.stage as MaturityStage,
      evidenceKinds: requirement.evidenceKinds as EvidenceKind[],
    };
  });
}
