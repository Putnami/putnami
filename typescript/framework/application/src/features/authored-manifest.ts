import { readdirSync, readFileSync, statSync } from 'node:fs';
import { joinPath } from '@putnami/utils';
import type { AuthoredFeature, AuthoredFeatureCatalog, AuthoredRequirement } from './generated-evidence';
import type { EvidenceKind, MaturityStage } from './evidence.types';

/** Single-file discovery location of durable authored feature intent. */
export const FEATURE_MANIFEST_FILENAME = 'putnami.features.json';

/**
 * Per-feature discovery location of durable authored feature intent: each
 * direct JSON child is one feature document. A project holds this directory or
 * `putnami.features.json`, never both.
 */
export const FEATURE_DIRECTORY = 'features';

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
 * Read the authored intent of a project root, reduced to what the
 * generated-evidence resolver needs: `putnami.features.json`, or the feature
 * documents under `features/` merged in filename order. A root that holds both
 * is refused.
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
  const manifest = readBoundedJson(joinPath(projectRoot, FEATURE_MANIFEST_FILENAME), FEATURE_MANIFEST_FILENAME);
  const documents = readFeatureDocuments(projectRoot);
  if (manifest !== undefined && documents.length > 0) {
    throw new Error(
      `a root holds ${FEATURE_MANIFEST_FILENAME} or ${FEATURE_DIRECTORY}/<feature>.json, never both: move every feature into one form`,
    );
  }
  if (manifest !== undefined) return readManifest(manifest);
  if (documents.length === 0) return undefined;
  return { features: documents.map(([name, raw]) => readFeatureDocument(name, raw)) };
}

function readManifest(raw: unknown): AuthoredFeatureCatalog {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new Error(`authored feature manifest ${FEATURE_MANIFEST_FILENAME} is not a JSON object`);
  }
  const document = raw as { protocolVersion?: unknown; features?: unknown };
  requireProtocolVersion(`authored feature manifest ${FEATURE_MANIFEST_FILENAME}`, document.protocolVersion);
  if (!Array.isArray(document.features)) {
    throw new Error(`authored feature manifest ${FEATURE_MANIFEST_FILENAME} must declare a features array`);
  }
  const features: AuthoredFeature[] = [];
  for (const [index, entry] of document.features.entries()) {
    features.push(readFeature(`${FEATURE_MANIFEST_FILENAME} features[${index}]`, entry));
  }
  return { features };
}

function readFeatureDocument(name: string, raw: unknown): AuthoredFeature {
  const path = `${FEATURE_DIRECTORY}/${name}`;
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new Error(`authored feature document ${path} is not a JSON object`);
  }
  const document = raw as { protocolVersion?: unknown; feature?: unknown };
  requireProtocolVersion(`authored feature document ${path}`, document.protocolVersion);
  return readFeature(`${path} feature`, document.feature);
}

function requireProtocolVersion(label: string, version: unknown): void {
  if (version !== 1 && version !== 2 && version !== 3) {
    throw new Error(`${label} declares unsupported protocolVersion ${JSON.stringify(version)}`);
  }
}

function readFeature(field: string, entry: unknown): AuthoredFeature {
  if (typeof entry !== 'object' || entry === null || Array.isArray(entry)) {
    throw new Error(`${field} must be an object`);
  }
  const feature = entry as { id?: unknown; requirements?: unknown };
  if (typeof feature.id !== 'string' || !feature.id) {
    throw new Error(`${field}.id must be a non-empty string`);
  }
  return { id: feature.id, requirements: readRequirements(field, feature.requirements) };
}

/** Reads one bounded regular JSON file, or undefined when it is absent. */
function readBoundedJson(path: string, name: string): unknown {
  let size: number;
  try {
    const info = statSync(path);
    if (!info.isFile()) {
      throw new Error(`authored feature manifest ${name} is not a regular file`);
    }
    size = info.size;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return undefined;
    throw error;
  }
  if (size > MAX_MANIFEST_BYTES) {
    throw new Error(`authored feature manifest ${name} exceeds the bounded describe read limit`);
  }
  return JSON.parse(readFileSync(path, 'utf8'));
}

/**
 * Reads the direct JSON children of `features/` in filename order. A
 * `features/` directory that holds a `putnami.json` is a nested project, never
 * a feature directory.
 */
function readFeatureDocuments(projectRoot: string): [string, unknown][] {
  const directory = joinPath(projectRoot, FEATURE_DIRECTORY);
  let names: string[];
  try {
    if (statSync(directory).isFile()) return [];
    names = readdirSync(directory, { withFileTypes: true })
      .filter((entry) => entry.isFile() && entry.name.endsWith('.json'))
      .map((entry) => entry.name);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') return [];
    throw error;
  }
  if (names.includes('putnami.json')) return [];
  return names
    .sort()
    .map(
      (name) => [name, readBoundedJson(joinPath(directory, name), `${FEATURE_DIRECTORY}/${name}`)] as [string, unknown],
    );
}

function readRequirements(featureField: string, raw: unknown): AuthoredRequirement[] {
  if (raw === undefined) return [];
  if (!Array.isArray(raw)) {
    throw new Error(`${featureField}.requirements must be an array`);
  }
  return raw.map((entry, index) => {
    const field = `${featureField}.requirements[${index}]`;
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
