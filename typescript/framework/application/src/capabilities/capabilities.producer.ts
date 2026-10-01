import {
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  realpathSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs';
import { randomBytes } from 'node:crypto';
import { isAbsolute, relative, resolve } from 'node:path';
import { isMigrationContributor } from '@putnami/migration';
import {
  type ConfigDefinition as RuntimeConfigDefinition,
  getRegisteredConfigDefinitions,
  isConfigContributor,
} from '@putnami/runtime';
import { robustRemoveSync, robustRenameSync } from '@putnami/runtime/robustio';
import { getCurrentProject, getProjectRoot, joinPath, relativePath } from '@putnami/utils';
import type { Module } from '../application/module';
import type { GenerateResult, Plugin } from '../application/module.types';
import { extractConfigBlock } from '../config/config-schema-extract';
import { isHealthChecker, isReadinessChecker } from '../platform/checker';
import { isDomainAccessContributor } from './domain-access';
import { isLifecycleContributor } from './lifecycle';
import { isCapabilityInventoryContributor } from './inventory';
import {
  buildCapabilityManifest,
  CAPABILITY_DIAGNOSTIC_CODES,
  type CapabilityDiagnostic,
  type CapabilityManifestInput,
  CapabilityManifestValidationError,
  serializeCapabilityManifest,
  validateCapabilityManifest,
  validateCapabilityManifestStructure,
} from './manifest';
import {
  canonicalCapabilityManifestV2,
  parseCapabilityManifestDocument,
  serializeCapabilityManifestV2,
  validateCapabilityManifestV2,
} from './manifest.v2';
import {
  type ArtifactLocation,
  type ConfigDefinition,
  type CapabilityKind,
  CAPABILITIES_V2_SCHEMA_URL,
  COMMITTED_PATH,
  type ContributionIdentity,
  type ContributionKind,
  type DeclarationLocation,
  type Discoverer,
  type DomainAccessV2,
  EMIT_DIR,
  type HealthContributor,
  type InfraRequirement,
  type LifecycleHook,
  type Manifest,
  type ManifestV2,
  MANIFEST_FILENAME,
  type MigrationBundle,
  type PackageVersion,
  type Provenance,
  type ProvenanceV2,
  type RequiredCapability,
  type SchemaContribution,
} from './manifest.types';
import { isCapabilityProvenanceContributor } from './provenance';
import { isRequiredCapabilityContributor } from './requirement';
import {
  type FeatureEvidenceDocument,
  type GeneratedEvidenceMapping,
  FEATURE_EVIDENCE_DIRECTORY,
  FeatureEvidenceValidationError,
  buildGeneratedEvidence,
  readAuthoredFeatureManifest,
  serializeFeatureEvidenceDocument,
} from '../features';

/**
 * Current capabilities-manifest protocol version. This literal lives in the
 * actual producer so the Go cross-language guard scans the code that stamps the
 * emitted artifact.
 */
export const CAPABILITIES_PROTOCOL_VERSION = 2 as const;

/** Stable TypeScript framework evidence-fragment name. */
export const FEATURE_EVIDENCE_FILENAME = 'typescript-framework.json';

/**
 * Names this producer on every record it emits. The issuer kind is build: the
 * assertion is that a build observed the fact, not that a framework asserted it.
 */
const FEATURE_EVIDENCE_ISSUER_ID = '@putnami/application';

/** Test/tooling overrides; the built-in application producer uses defaults. */
export interface CapabilitiesProducerOptions {
  projectRoot?: string;
  project?: string;
  registeredConfigDefinitions?: () => RuntimeConfigDefinition[];
  capabilityRoot?: string;
  capabilityPackages?: CapabilityPackageStamp[];
  /** Import generated loaders but leave the scheduler-owned manifest untouched. */
  publishCapabilityManifest?: boolean;
}

/** Stable scheduler metadata for the workload and its transitive packages. */
export interface CapabilityPackageStamp {
  package: string;
  version: string;
  evidencePath: string;
  /** Workspace-relative root whose exact contents produced this package. */
  sourceRoot?: string;
  /** Required source-v1 binding for Capability Manifest v2 publication. */
  sourceBinding?: string;
  /** True when the scheduler could not compute sourceBinding. */
  sourceBindingUnavailable?: boolean;
  capabilityManifestPath?: string;
}

/**
 * The built-in capability-manifest producer, the TypeScript counterpart of Go's
 * `app.describeCapabilities`. It walks every contribution kind the app tree
 * exposes through the existing per-kind mechanisms (the config registry plus
 * ConfigContributor, MigrationContributor, HealthChecker/ReadinessChecker,
 * module health contributions, and LifecycleContributor), folds them into a
 * a structurally validated inventory, upgrades it to Capability Manifest v2,
 * and writes the deterministic per-project manifest plus optional feature
 * evidence.
 *
 * The serialization uses the Go field order, indentation, trailing newline,
 * and encoding/json escaping rules. Every collection is sorted by UTF-8 bytes,
 * so the output is byte-identical to the Go emitter's for an equivalent
 * workload.
 *
 * Finalization runs in `postGenerate()`: all route/spec loaders exist by then,
 * and importing each generated `*-loader` makes route-owned `configToken()`
 * registrations visible before collection. Collection/import failures
 * propagate, matching the Go describer. The write itself is atomic (unique temp
 * file + rename), so a crash mid-write never leaves a partial manifest on the
 * promotion path.
 */
export function createCapabilitiesProducer(options: CapabilitiesProducerOptions = {}): Plugin {
  return {
    async postGenerate(owner: Module, generated?: GenerateResult): Promise<GenerateResult> {
      const publishCapabilityManifest = options.publishCapabilityManifest !== false;
      if (publishCapabilityManifest) invalidateCapabilityManifest(options.projectRoot);
      await importGeneratedLoaders(generated);
      if (!publishCapabilityManifest) return {};
      const emitted = emitCapabilityManifest(owner, generated, options);
      if (!emitted) return {};
      // Match the OpenAPI/proto producer convention: the key is the packaged
      // schema path and the value is the concrete generated source file.
      const assets: Record<string, string> = { [COMMITTED_PATH]: emitted.manifestPath };
      if (emitted.evidencePath) {
        assets[`${FEATURE_EVIDENCE_DIRECTORY}/${FEATURE_EVIDENCE_FILENAME}`] = emitted.evidencePath;
      }
      return { assets };
    },
  };
}

/** The stable project identity read from `.gen/version.json`. */
interface GeneratedVersion {
  name?: string;
  capabilityRoot?: string;
  capabilityPackages?: CapabilityPackageStamp[];
}

interface EmittedCapabilityArtifacts {
  manifestPath: string;
  evidencePath?: string;
}

function emitCapabilityManifest(
  owner: Module,
  generated: GenerateResult | undefined,
  options: CapabilitiesProducerOptions,
): EmittedCapabilityArtifacts | undefined {
  const projectRoot = options.projectRoot ?? getProjectRoot();
  const version = readGeneratedVersion(projectRoot);
  // The scheduler-stamped identity is authoritative. Direct unit/build calls
  // may not have a version stamp, so fall back to the package identity rather
  // than silently leaving a stale manifest behind.
  const project = options.project ?? version.name ?? getCurrentProject().name;
  const schedulerPackages = options.capabilityPackages ?? version.capabilityPackages;
  // Non-scheduler callers have no exact source state to bind. Match the Go
  // producer by publishing nothing instead of inventing v2 provenance.
  if (schedulerPackages === undefined) return undefined;

  const plugins = owner.getRoot().collectPlugins();
  // Hosted CI can compile this framework with the previous scheduler while a
  // CLI rollout is in flight. That scheduler carries enough provenance for the
  // v1 activation contract but not enough to make v2 source-binding claims.
  // Keep the manifest authoritative for TypeScript loader/config activation by
  // emitting v1 in that one compatibility case.
  const legacyScheduler = legacyCapabilitySchedulerMetadata(project, schedulerPackages);
  const inventory = legacyScheduler
    ? buildLegacyPackageInventory(project, schedulerPackages)
    : buildPackageInventory(project, schedulerPackages);
  const migrations = collectMigrations(plugins, inventory);
  const generatedInventory = collectGeneratedCapabilities(generated, projectRoot, inventory);
  const staticInventory = collectStaticCapabilities(plugins, projectRoot, inventory);
  const configDefinitions = collectConfig(
    plugins,
    inventory,
    options.registeredConfigDefinitions?.() ?? getRegisteredConfigDefinitions(),
  );
  const input: CapabilityManifestInput = {
    // The existing collectors use the frozen v1 structural types as an
    // internal runtime-validation projection. No v1 bytes are published.
    protocolVersion: 1,
    project,
    configDefinitions,
    schemas: [...generatedInventory.schemas, ...staticInventory.schemas],
    discoverers: [
      ...generatedInventory.discoverers,
      ...staticInventory.discoverers,
      ...configDefinitions.map(({ path, provenance }) => ({ name: path, kind: 'config' as const, provenance })),
      ...collectMigrationDiscoverers(plugins, inventory),
    ],
    migrations: migrations.bundles,
    infraRequirements: [...collectInfraSidecars(projectRoot, inventory), ...staticInventory.infraRequirements],
    healthContributors: collectHealth(owner.getRoot(), plugins, inventory),
    lifecycleHooks: collectLifecycle(plugins, inventory),
    packageVersions: packageVersions(inventory),
    requiredCapabilities: collectRequiredCapabilities(plugins, inventory),
  };

  if (legacyScheduler) {
    mergeDependencyCapabilityManifestsV1(
      projectRoot,
      options.capabilityRoot ?? version.capabilityRoot ?? '.',
      input,
      inventory,
    );
  }

  // Validate runtime contributor values before deterministic sorting: a plugin
  // can be plain JavaScript or cast through `unknown`, so compile-time types do
  // not make malformed names/requires/provenance safe to serialize.
  const structuralDiagnostics = validateCapabilityManifestStructure(input as Manifest);
  if (structuralDiagnostics.length > 0) {
    throw new CapabilityManifestValidationError(structuralDiagnostics);
  }

  const manifest = buildCapabilityManifest(input);
  const diagnostics = [
    ...migrations.diagnostics,
    ...(legacyScheduler ? validateCompleteV1Provenance(manifest) : []),
    ...validateCapabilityManifest(manifest),
  ];
  if (diagnostics.length > 0) {
    throw new CapabilityManifestValidationError(diagnostics);
  }
  if (legacyScheduler) return writeLegacyCapabilityManifest(projectRoot, manifest);
  const sourceBoundInventory = inventory as SourceBoundPackageInventory;
  const manifestV2 = upgradeLocalManifestToV2(
    manifest,
    sourceBoundInventory,
    migrations.kinds,
    collectDomainAccess(plugins, sourceBoundInventory),
  );
  mergeDependencyCapabilityManifests(
    projectRoot,
    options.capabilityRoot ?? version.capabilityRoot ?? '.',
    manifestV2,
    sourceBoundInventory,
  );
  const v2Diagnostics = validateCapabilityManifestV2(manifestV2);
  if (v2Diagnostics.length > 0) throw new CapabilityManifestValidationError(v2Diagnostics);
  const evidence = buildFeatureEvidenceDocument(owner, projectRoot, manifestV2, sourceBoundInventory);
  return writeCapabilityArtifacts(projectRoot, manifestV2, evidence);
}

interface CollectedCapabilityInventory {
  schemas: SchemaContribution[];
  discoverers: Discoverer[];
  infraRequirements: InfraRequirement[];
}

const ROUTE_LOADER_NAMES = new Set(['api-loader', 'react-loader', 'static-loader']);

function collectGeneratedCapabilities(
  generated: GenerateResult | undefined,
  projectRoot: string,
  inventory: PackageInventory,
): CollectedCapabilityInventory {
  const schemas: SchemaContribution[] = [];
  const discoverers: Discoverer[] = [];
  for (const [assetName, assetPath] of Object.entries(generated?.assets ?? {}).sort(([a], [b]) => cmp(a, b))) {
    const normalized = assetName.replaceAll('\\', '/');
    let kind: SchemaContribution['kind'] | undefined;
    let name = '';
    if (normalized === 'schema/openapi.json') {
      kind = 'openapi';
      name = 'openapi';
    } else if (normalized === 'schema/api.proto') {
      kind = 'proto';
      name = 'api-proto';
    }
    if (!kind || !existsSync(assetPath) || statSync(assetPath).isDirectory()) continue;
    schemas.push({
      name,
      kind,
      path: normalized,
      provenance: generatedProvenance(inventory, relativeProjectPath(projectRoot, assetPath)),
    });
  }
  for (const [name, loaderPath] of Object.entries(generated?.exports ?? {})) {
    if (name === 'config-loader' && isConfigArtifact(loaderPath)) {
      if (!existsSync(loaderPath) || statSync(loaderPath).isDirectory()) {
        throw new Error(`generated config discoverer ${JSON.stringify(name)} does not exist: ${loaderPath}`);
      }
      discoverers.push({
        name,
        kind: 'config',
        provenance: generatedProvenance(inventory, relativeProjectPath(projectRoot, loaderPath)),
      });
      continue;
    }
    if (!isImportableServerLoader(name, loaderPath)) continue;

    const path = relativeProjectPath(projectRoot, loaderPath);
    if (!existsSync(loaderPath) || statSync(loaderPath).isDirectory()) {
      throw new Error(`generated loader ${JSON.stringify(name)} does not exist: ${loaderPath}`);
    }
    const provenance = generatedProvenance(inventory, path);
    if (ROUTE_LOADER_NAMES.has(name)) {
      schemas.push({ name, kind: 'route', path, provenance });
    } else {
      discoverers.push({ name, kind: 'source', provenance });
    }
  }
  return { schemas, discoverers, infraRequirements: [] };
}

function relativeProjectPath(projectRoot: string, path: string): string {
  const value = relativePath(projectRoot, path).replaceAll('\\', '/');
  if (value === '..' || value.startsWith('../') || isAbsolute(value)) {
    throw new Error(`generated capability evidence resolves outside the project: ${path}`);
  }
  return value;
}

function isConfigArtifact(path: string): boolean {
  return /\.(?:json|ya?ml)$/i.test(path);
}

function isImportableServerLoader(name: string, path: string): boolean {
  return name.endsWith('-loader') && !name.endsWith('client-loader') && /\.(?:[cm]?[jt]sx?)$/.test(path);
}

type PluginEntry = { plugin: Plugin };

interface PackageInventory {
  project: string;
  packages: ReadonlyMap<string, CapabilityPackageStamp>;
  ordered: readonly CapabilityPackageStamp[];
}

interface BoundCapabilityPackageStamp extends CapabilityPackageStamp {
  sourceRoot: string;
  sourceBinding: string;
}

interface SourceBoundPackageInventory extends PackageInventory {
  packages: ReadonlyMap<string, BoundCapabilityPackageStamp>;
  ordered: readonly BoundCapabilityPackageStamp[];
}

function cmp(first: string, second: string): number {
  return Buffer.compare(Buffer.from(first, 'utf8'), Buffer.from(second, 'utf8'));
}

function legacyCapabilitySchedulerMetadata(project: string, packages: CapabilityPackageStamp[]): boolean {
  if (!project.trim() || !Array.isArray(packages) || packages.length === 0) return false;
  const seen = new Set<string>();
  let workloadPresent = false;
  for (const metadata of packages) {
    if (typeof metadata !== 'object' || metadata === null || Array.isArray(metadata)) return false;
    const unknownKeys = Object.keys(metadata).filter(
      (key) => !['package', 'version', 'evidencePath', 'capabilityManifestPath'].includes(key),
    );
    if (
      unknownKeys.length > 0 ||
      metadata.sourceRoot !== undefined ||
      metadata.sourceBinding !== undefined ||
      metadata.sourceBindingUnavailable !== undefined ||
      typeof metadata.package !== 'string' ||
      !metadata.package.trim() ||
      typeof metadata.version !== 'string' ||
      !isResolvedVersion(metadata.version) ||
      typeof metadata.evidencePath !== 'string' ||
      !canonicalWorkspaceRelativePath(metadata.evidencePath, false) ||
      (metadata.capabilityManifestPath !== undefined &&
        (typeof metadata.capabilityManifestPath !== 'string' || !metadata.capabilityManifestPath.trim())) ||
      seen.has(metadata.package)
    ) {
      return false;
    }
    seen.add(metadata.package);
    workloadPresent ||= metadata.package === project;
  }
  return workloadPresent;
}

function buildPackageInventory(
  project: string,
  packages: CapabilityPackageStamp[] | undefined,
): SourceBoundPackageInventory {
  const byPackage = new Map<string, BoundCapabilityPackageStamp>();
  if (packages !== undefined && (!Array.isArray(packages) || packages.length === 0)) {
    throw new Error('capabilityPackages must be a non-empty array when scheduler metadata is present');
  }
  for (const [index, rawMetadata] of (packages ?? []).entries()) {
    if (typeof rawMetadata !== 'object' || rawMetadata === null || Array.isArray(rawMetadata)) {
      throw new Error(`capabilityPackages[${index}] must be an object`);
    }
    const metadata = rawMetadata as CapabilityPackageStamp;
    const hasSourceMetadata =
      metadata.sourceRoot !== undefined ||
      metadata.sourceBinding !== undefined ||
      metadata.sourceBindingUnavailable !== undefined;
    const malformedSourceMetadata =
      !hasSourceMetadata ||
      typeof metadata.sourceRoot !== 'string' ||
      !metadata.sourceRoot.trim() ||
      typeof metadata.sourceBinding !== 'string' ||
      !SOURCE_BINDING_PATTERN.test(metadata.sourceBinding) ||
      metadata.sourceBindingUnavailable === true ||
      (metadata.sourceBindingUnavailable !== undefined && typeof metadata.sourceBindingUnavailable !== 'boolean');
    const unknownKeys = Object.keys(metadata).filter(
      (key) =>
        ![
          'package',
          'version',
          'evidencePath',
          'sourceRoot',
          'sourceBinding',
          'sourceBindingUnavailable',
          'capabilityManifestPath',
        ].includes(key),
    );
    if (
      unknownKeys.length > 0 ||
      malformedSourceMetadata ||
      typeof metadata.package !== 'string' ||
      !metadata.package.trim() ||
      typeof metadata.version !== 'string' ||
      !metadata.version.trim() ||
      typeof metadata.evidencePath !== 'string' ||
      !metadata.evidencePath.trim() ||
      (metadata.capabilityManifestPath !== undefined &&
        (typeof metadata.capabilityManifestPath !== 'string' || !metadata.capabilityManifestPath.trim()))
    ) {
      throw new Error(`capabilityPackages[${index}] is incomplete or malformed`);
    }
    const sourceRoot = metadata.sourceRoot as string;
    const sourceBinding = metadata.sourceBinding as string;
    if (
      !canonicalWorkspaceRelativePath(sourceRoot, true) ||
      !canonicalWorkspaceRelativePath(metadata.evidencePath, false)
    ) {
      throw new Error(
        `capabilityPackages[${index}] sourceRoot/evidencePath is not a canonical contained workspace-relative path`,
      );
    }
    if (!isResolvedVersion(metadata.version)) {
      throw new Error(
        `capability package ${JSON.stringify(metadata.package)} has unresolved version ${JSON.stringify(metadata.version)}`,
      );
    }
    const normalized: BoundCapabilityPackageStamp = {
      ...metadata,
      evidencePath: metadata.evidencePath.replaceAll('\\', '/'),
      sourceRoot: sourceRoot.replaceAll('\\', '/'),
      sourceBinding,
      capabilityManifestPath: metadata.capabilityManifestPath?.replaceAll('\\', '/'),
    };
    const existing = byPackage.get(metadata.package);
    if (existing) {
      if (
        existing.version !== normalized.version ||
        existing.evidencePath !== normalized.evidencePath ||
        existing.sourceRoot !== normalized.sourceRoot ||
        existing.sourceBinding !== normalized.sourceBinding ||
        existing.sourceBindingUnavailable !== normalized.sourceBindingUnavailable ||
        existing.capabilityManifestPath !== normalized.capabilityManifestPath
      ) {
        throw new Error(`capability package ${JSON.stringify(metadata.package)} has conflicting scheduler metadata`);
      }
      continue;
    }
    byPackage.set(metadata.package, normalized);
  }
  if (packages !== undefined && !byPackage.has(project)) {
    throw new Error(`capabilityPackages is missing the workload package ${JSON.stringify(project)}`);
  }
  return {
    project,
    packages: byPackage,
    ordered: [...byPackage.values()].sort((a, b) => cmp(a.package, b.package)),
  };
}

/**
 * Validate the complete pre-source-binding scheduler shape without fabricating
 * any v2 fields. The returned inventory is used only to reproduce the v1
 * activation manifest during a rolling CLI upgrade.
 */
function buildLegacyPackageInventory(project: string, packages: CapabilityPackageStamp[]): PackageInventory {
  const byPackage = new Map<string, CapabilityPackageStamp>();
  for (const [index, metadata] of packages.entries()) {
    if (
      typeof metadata !== 'object' ||
      metadata === null ||
      Array.isArray(metadata) ||
      typeof metadata.package !== 'string' ||
      !metadata.package.trim() ||
      typeof metadata.version !== 'string' ||
      !isResolvedVersion(metadata.version) ||
      typeof metadata.evidencePath !== 'string' ||
      !canonicalWorkspaceRelativePath(metadata.evidencePath, false) ||
      (metadata.capabilityManifestPath !== undefined &&
        (typeof metadata.capabilityManifestPath !== 'string' || !metadata.capabilityManifestPath.trim()))
    ) {
      throw new Error(`capabilityPackages[${index}] is incomplete or malformed`);
    }
    const normalized: CapabilityPackageStamp = {
      package: metadata.package,
      version: metadata.version,
      evidencePath: metadata.evidencePath,
      capabilityManifestPath: metadata.capabilityManifestPath?.replaceAll('\\', '/'),
    };
    if (byPackage.has(normalized.package)) {
      throw new Error(`capability package ${JSON.stringify(normalized.package)} has conflicting scheduler metadata`);
    }
    byPackage.set(normalized.package, normalized);
  }
  if (!byPackage.has(project)) {
    throw new Error(`capabilityPackages is missing the workload package ${JSON.stringify(project)}`);
  }
  return {
    project,
    packages: byPackage,
    ordered: [...byPackage.values()].sort((a, b) => cmp(a.package, b.package)),
  };
}

const SOURCE_BINDING_PATTERN = /^source-v1:sha256:[0-9a-f]{64}$/;

function canonicalWorkspaceRelativePath(value: string, allowRoot: boolean): boolean {
  if (value === '.' && allowRoot) return true;
  if (!value || value.includes('\\') || value.startsWith('/') || /^[A-Za-z][A-Za-z0-9+.-]*:/.test(value)) {
    return false;
  }
  return !value.split('/').some((segment) => segment === '' || segment === '.' || segment === '..');
}

function contributionIdentityKey(identity: ContributionIdentity): string {
  return JSON.stringify([identity.ownerProject, identity.kind, identity.subkind ?? '', identity.key]);
}

function contributionIdentityLabel(identity: ContributionIdentity): string {
  return `(${JSON.stringify(identity.ownerProject)},${JSON.stringify(identity.kind)},${JSON.stringify(identity.subkind ?? '')},${JSON.stringify(identity.key)})`;
}

function localContributionIdentity(
  inventory: PackageInventory,
  kind: ContributionKind,
  subkind: string,
  key: string,
): ContributionIdentity {
  return { ownerProject: inventory.project, kind, subkind: subkind || undefined, key };
}

function packageMetadata(inventory: SourceBoundPackageInventory, packageName: string): BoundCapabilityPackageStamp {
  const exact = inventory.packages.get(packageName);
  if (exact) return exact;
  const matches = inventory.ordered
    .filter(({ package: candidate }) => packageName.startsWith(`${candidate}/`))
    .sort((left, right) => right.package.length - left.package.length);
  if (matches[0]) return matches[0];
  throw new Error(`capability package ${JSON.stringify(packageName)} has no scheduler-stamped source root`);
}

function pathWithinSourceRoot(metadata: BoundCapabilityPackageStamp, value: string): string {
  const normalized = value.replaceAll('\\', '/');
  const sourceRoot = metadata.sourceRoot.replace(/^\/+|\/+$/g, '');
  if (sourceRoot && sourceRoot !== '.' && !normalized.startsWith(`${sourceRoot}/`)) {
    throw new Error(
      `capability declaration ${JSON.stringify(value)} is not contained by stamped source root ${JSON.stringify(metadata.sourceRoot)}`,
    );
  }
  const path = sourceRoot && sourceRoot !== '.' ? normalized.slice(sourceRoot.length + 1) : normalized;
  if (!canonicalWorkspaceRelativePath(path, false)) {
    throw new Error(
      `capability declaration ${JSON.stringify(value)} is not contained by stamped source root ${JSON.stringify(metadata.sourceRoot)}`,
    );
  }
  return path;
}

function uniqueArtifacts(values: ArtifactLocation[]): ArtifactLocation[] | undefined {
  const seen = new Set<string>();
  const output: ArtifactLocation[] = [];
  for (const artifact of values) {
    const key = JSON.stringify([artifact.root, artifact.path, artifact.digest ?? '']);
    if (seen.has(key)) continue;
    seen.add(key);
    output.push({ ...artifact });
  }
  return output.length > 0 ? output : undefined;
}

function upgradeProvenance(
  provenance: Provenance,
  identity: ContributionIdentity,
  inventory: SourceBoundPackageInventory,
  artifacts: ArtifactLocation[] = [],
): ProvenanceV2 {
  packageMetadata(inventory, identity.ownerProject);
  const producerPackage = provenance.package ?? inventory.project;
  const producer = packageMetadata(inventory, producerPackage);
  const rawPath =
    provenance.sourceKind === 'generated' ? producer.evidencePath : (provenance.evidencePath ?? producer.evidencePath);
  const declaration: DeclarationLocation = {
    root: producer.package === identity.ownerProject ? 'project' : 'package',
    path: pathWithinSourceRoot(producer, rawPath),
  };
  const associatedArtifacts = [
    ...artifacts,
    ...(provenance.sourceKind === 'generated' && provenance.evidencePath
      ? [{ root: 'project' as const, path: provenance.evidencePath.replaceAll('\\', '/') }]
      : []),
  ];
  return {
    project: identity.ownerProject,
    package: producerPackage,
    sourceKind: provenance.sourceKind,
    declaration,
    artifacts: uniqueArtifacts(associatedArtifacts),
  };
}

function upgradeLocalManifestToV2(
  manifest: Manifest,
  inventory: SourceBoundPackageInventory,
  migrationKinds: Map<MigrationBundle, string>,
  domainAccess: DomainAccessV2[],
): ManifestV2 {
  return {
    $schema: CAPABILITIES_V2_SCHEMA_URL,
    protocolVersion: CAPABILITIES_PROTOCOL_VERSION,
    project: manifest.project,
    configDefinitions: manifest.configDefinitions?.map((item) => {
      const identity = localContributionIdentity(inventory, 'config', '', item.path);
      return { ...item, identity, provenance: upgradeProvenance(item.provenance, identity, inventory) };
    }),
    schemas: manifest.schemas?.map((item) => {
      const identity = localContributionIdentity(inventory, 'schema', item.kind, item.name);
      const artifacts = item.path && item.kind !== 'route' ? [{ root: 'project' as const, path: item.path }] : [];
      return { ...item, identity, provenance: upgradeProvenance(item.provenance, identity, inventory, artifacts) };
    }),
    discoverers: manifest.discoverers?.map((item) => {
      const identity = localContributionIdentity(inventory, 'discoverer', item.kind, item.name);
      return { ...item, identity, provenance: upgradeProvenance(item.provenance, identity, inventory) };
    }),
    migrations: manifest.migrations?.map((item) => {
      const kind = migrationKinds.get(item);
      if (!kind) throw new Error(`migration ${JSON.stringify(item.name)} has no precise migration kind`);
      const identity = localContributionIdentity(inventory, 'migration', kind, item.name);
      return { ...item, identity, kind, provenance: upgradeProvenance(item.provenance, identity, inventory) };
    }),
    infraRequirements: manifest.infraRequirements?.map((item) => {
      const identity = localContributionIdentity(inventory, 'infra', item.kind, item.name);
      return { ...item, identity, provenance: upgradeProvenance(item.provenance, identity, inventory) };
    }),
    healthContributors: manifest.healthContributors?.map((item) => {
      const identity = localContributionIdentity(inventory, 'health', item.probe, item.name);
      return { ...item, identity, provenance: upgradeProvenance(item.provenance, identity, inventory) };
    }),
    lifecycleHooks: manifest.lifecycleHooks?.map((item) => {
      const identity = localContributionIdentity(inventory, 'lifecycle', item.phase, item.name);
      return { ...item, identity, provenance: upgradeProvenance(item.provenance, identity, inventory) };
    }),
    // The scheduler stamps the workload's whole reachable package closure, but a
    // COMMITTED manifest may only name the packages that actually declared one of
    // its contributions. Emitting the closure made this project's tracked
    // manifest a function of the entire dependency graph, so a dependency edit
    // anywhere re-stamped every workload. canonicalCapabilityManifestV2 enforces
    // the same surface rule; the closure itself stays in the ephemeral
    // .gen/version.json stamp. Go twin: capabilityInventory.packageContributions.
    packages: inventory.ordered.map((metadata) => ({
      identity: { ownerProject: metadata.package, kind: 'package', key: metadata.package },
      package: metadata.package,
      provenance: {
        project: metadata.package,
        package: metadata.package,
        sourceKind: 'framework',
        declaration: {
          root: 'project',
          path: pathWithinSourceRoot(metadata, metadata.evidencePath),
        },
      },
    })),
    requiredCapabilities: manifest.requiredCapabilities?.map((item) => {
      const identity = localContributionIdentity(inventory, 'requiredCapability', '', item.name);
      return { ...item, identity, provenance: upgradeProvenance(item.provenance, identity, inventory) };
    }),
    // v2-only: a domain access contract has no v1 shape to upgrade from, so it
    // is collected directly rather than projected out of the frozen v1 types.
    domainAccess: domainAccess.length > 0 ? domainAccess : undefined,
  };
}

async function importGeneratedLoaders(generated?: GenerateResult): Promise<void> {
  const loaders = Object.entries(generated?.exports ?? {})
    .filter(([key]) => key.endsWith('-loader') && !key.endsWith('client-loader'))
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  for (const [key, path] of loaders) {
    try {
      // biome-ignore lint/performance/noAwaitInLoops: deterministic import order keeps registration failures stable
      await import(path);
    } catch (error) {
      throw new Error(`Could not import ${key} loader at ${path}: ${String(error)}`);
    }
  }
}

/**
 * Resolve framework provenance from the scheduler-owned package graph. A
 * contributor may identify its package explicitly; otherwise it belongs to the
 * workload project. Explicit packages outside the graph must provide their own
 * exact version and evidence path so the emitter never invents provenance.
 */
function frameworkProvenance(inventory: PackageInventory, contributor?: unknown): Provenance {
  const declared = isCapabilityProvenanceContributor(contributor) ? contributor.capabilityProvenance() : undefined;
  const packageName = declared?.package ?? inventory.project;
  const metadata = inventory.packages.get(packageName);
  const self = inventory.packages.get(inventory.project)!;
  if (declared?.version && !isResolvedVersion(declared.version)) {
    throw new Error(
      `capability package ${JSON.stringify(packageName)} declared unresolved version ${JSON.stringify(declared.version)}`,
    );
  }
  if (declared?.package && !metadata && (!declared.version || !declared.evidencePath)) {
    throw new Error(
      `capability package ${JSON.stringify(packageName)} is absent from the scheduler inventory and must declare exact version + evidencePath`,
    );
  }
  return {
    project: inventory.project,
    package: packageName,
    version: declared?.version ?? metadata?.version ?? self.version,
    sourceKind: declared?.sourceKind ?? 'framework',
    evidencePath: declared?.evidencePath ?? metadata?.evidencePath ?? self.evidencePath,
  };
}

function isResolvedVersion(version: string): boolean {
  const value = version.trim();
  return value.length > 0 && !value.startsWith('workspace:') && !/[~^*<>=|]/.test(value);
}

function generatedProvenance(
  inventory: PackageInventory,
  evidencePath: string,
  packageName = inventory.project,
): Provenance {
  const metadata = inventory.packages.get(packageName) ?? inventory.packages.get(inventory.project)!;
  return {
    project: inventory.project,
    package: metadata.package,
    version: metadata.version,
    sourceKind: 'generated',
    evidencePath,
  };
}

function packageVersions(inventory: PackageInventory): PackageVersion[] {
  return inventory.ordered.map((metadata) => ({
    package: metadata.package,
    version: metadata.version,
    provenance: {
      project: inventory.project,
      package: metadata.package,
      version: metadata.version,
      sourceKind: 'framework',
      evidencePath: metadata.evidencePath,
    },
  }));
}

function collectConfig(
  plugins: PluginEntry[],
  inventory: PackageInventory,
  registeredDefinitions: RuntimeConfigDefinition[],
): ConfigDefinition[] {
  const defs: ConfigDefinition[] = [];
  const seen = new Map<string, RuntimeConfigDefinition>();

  const append = (def: RuntimeConfigDefinition, provenance: Provenance): void => {
    const existing = seen.get(def.path);
    if (existing) {
      if (existing !== def) {
        throw new Error(`duplicate config path ${JSON.stringify(def.path)} contributed by multiple sources`);
      }
      return;
    }
    seen.set(def.path, def);

    const block = extractConfigBlock(def);
    // Only top-level fields are modeled by the flat ConfigField shape, matching
    // the Go emitter; nested field trees are not flattened into the manifest.
    const fields = block.fields.map((f) => ({
      name: f.name,
      type: f.type || undefined,
      sensitive: f.sensitive ? true : undefined,
    }));
    defs.push({ path: block.path, fields, provenance });
  };

  // Dependency-owned config stays on the plugin tree; collect it first so an
  // optional plugin provenance contribution wins when the same definition was
  // also registered through configToken()/provideConfig().
  for (const { plugin } of plugins) {
    if (!isConfigContributor(plugin)) {
      continue;
    }
    for (const def of plugin.configDefinitions()) {
      append(def, frameworkProvenance(inventory, plugin));
    }
  }
  for (const def of registeredDefinitions) {
    append(def, frameworkProvenance(inventory));
  }
  return defs;
}

interface CollectedMigrations {
  bundles: MigrationBundle[];
  kinds: Map<MigrationBundle, string>;
  diagnostics: CapabilityDiagnostic[];
}

function collectMigrations(plugins: PluginEntry[], inventory: PackageInventory): CollectedMigrations {
  const bundles: MigrationBundle[] = [];
  const kinds = new Map<MigrationBundle, string>();
  const diagnostics: CapabilityDiagnostic[] = [];
  const seen = new Map<string, MigrationBundle>();
  for (const { plugin } of plugins) {
    if (!isMigrationContributor(plugin)) {
      continue;
    }
    const provenance = frameworkProvenance(inventory, plugin);
    for (const source of plugin.migrationSources()) {
      const bundle: MigrationBundle = {
        name: source.namespace,
        datasource: source.infraDatabase?.()?.name || undefined,
        provenance,
      };
      kinds.set(bundle, source.kind);
      const key = `${source.kind}:${source.namespace}`;
      const first = seen.get(key);
      if (first) {
        const duplicate =
          (first.datasource ?? '') === (bundle.datasource ?? '') && (first.digest ?? '') === (bundle.digest ?? '');
        diagnostics.push({
          severity: 'error',
          code: duplicate
            ? CAPABILITY_DIAGNOSTIC_CODES.duplicateProvider
            : CAPABILITY_DIAGNOSTIC_CODES.conflictingProvider,
          field: `migrations[${bundles.length}]`,
          message: `migration provider ${JSON.stringify(key)} ${duplicate ? 'is declared more than once' : 'has conflicting declarations'} (${capabilityProvenanceLabel(first.provenance)} and ${capabilityProvenanceLabel(bundle.provenance)}); ${duplicate ? 'remove one duplicate declaration or give each provider a unique namespace within its migration kind' : 'reconcile the definitions or give each provider a unique namespace within its migration kind'}`,
        });
      } else {
        seen.set(key, bundle);
      }
      bundles.push(bundle);
    }
  }
  return { bundles, kinds, diagnostics };
}

function capabilityProvenanceLabel(provenance: Provenance): string {
  return provenance.evidencePath || provenance.package || provenance.project || 'unknown source';
}

function collectStaticCapabilities(
  plugins: PluginEntry[],
  projectRoot: string,
  inventory: PackageInventory,
): CollectedCapabilityInventory {
  const result: CollectedCapabilityInventory = { schemas: [], discoverers: [], infraRequirements: [] };
  for (const { plugin } of plugins) {
    if (!isCapabilityInventoryContributor(plugin)) continue;
    const provenance = frameworkProvenance(inventory, plugin);
    const contributed = plugin.capabilityInventory();
    for (const schema of contributed.schemas ?? []) {
      if (schema.kind !== 'route') {
        const schemaPath = schema.path;
        if (
          !schemaPath ||
          ![joinPath(projectRoot, schemaPath), joinPath(projectRoot, '.gen', schemaPath)].some(
            (path) => existsSync(path) && !statSync(path).isDirectory(),
          )
        ) {
          continue;
        }
      }
      result.schemas.push({ ...schema, provenance });
    }
    for (const discoverer of contributed.discoverers ?? []) result.discoverers.push({ ...discoverer, provenance });
    for (const requirement of contributed.infraRequirements ?? []) {
      result.infraRequirements.push({ ...requirement, provenance });
    }
  }
  return result;
}

function collectMigrationDiscoverers(plugins: PluginEntry[], inventory: PackageInventory): Discoverer[] {
  const result: Discoverer[] = [];
  for (const { plugin } of plugins) {
    if (!isMigrationContributor(plugin)) continue;
    const provenance = frameworkProvenance(inventory, plugin);
    for (const source of plugin.migrationSources()) {
      result.push({ name: `${source.kind}:${source.namespace}`, kind: 'source', provenance });
    }
  }
  return result;
}

type InfraSidecar = {
  protocolVersion: number;
  databases?: Array<{ name: string; engine: string; schemas?: string[] }>;
  events?: {
    publishes?: string[];
    subscribes?: Array<string | { topic: string; delivery?: string; push?: unknown }>;
  };
  storage?: Array<{ name: string; access?: string; public?: boolean; retention?: string }>;
  secrets?: string[];
  scheduledJobs?: Array<{ name: string; schedule: string; entrypoint?: string }>;
};

// Capability collection consumes scratch sidecars produced in the same build,
// so it requires the current infra producer version rather than the legacy v1
// compatibility accepted when committed requirements are normalized.
const INFRA_SIDECAR_PROTOCOL_VERSION = 2;

// Files in .gen/infra/ that are NOT capability sidecars. The directory is shared:
// capability plugins drop one sidecar per package there, but the CLI's own
// aggregateWorkloadInfra also writes runtime.json, the merged workload descriptor.
// The scan below derives a package name from the filename and validates against an
// exact key set, so an unlisted file is not merely ignored — it throws, and the
// failure surfaces as an unrelated build() or producer test failing whenever a
// build~infra run has left the file behind and test~generate has not yet pruned it.
const NON_SIDECAR_INFRA_FILES = new Set(['runtime.json']);

function collectInfraSidecars(projectRoot: string, inventory: PackageInventory): InfraRequirement[] {
  const dir = joinPath(projectRoot, '.gen', 'infra');
  if (!existsSync(dir)) return [];
  const result: InfraRequirement[] = [];
  const seen = new Set<string>();
  const append = (name: string, kind: InfraRequirement['kind'], provenance: Provenance): void => {
    const key = `${kind}:${name}`;
    if (seen.has(key)) return;
    seen.add(key);
    result.push({ name, kind, provenance });
  };
  for (const filename of readdirSync(dir)
    .filter((name) => name.endsWith('.json') && !NON_SIDECAR_INFRA_FILES.has(name))
    .sort(cmp)) {
    const path = joinPath(dir, filename);
    if (statSync(path).isDirectory()) continue;
    const sidecar = parseInfraSidecar(readFileSync(path, 'utf8'), filename);
    const slug = filename.slice(0, -'.json'.length);
    const packageName = slug === 'secrets' ? '@putnami/runtime' : `@putnami/${slug}`;
    const provenance = generatedProvenance(inventory, `.gen/infra/${filename}`, packageName);
    for (const database of sidecar.databases ?? []) append(database.name, 'database', provenance);
    for (const topic of sidecar.events?.publishes ?? []) append(topic, 'events', provenance);
    for (const subscription of sidecar.events?.subscribes ?? []) {
      append(typeof subscription === 'string' ? subscription : subscription.topic, 'events', provenance);
    }
    for (const storage of sidecar.storage ?? []) append(storage.name, 'storage', provenance);
    for (const secret of sidecar.secrets ?? []) append(secret, 'secret', provenance);
    for (const job of sidecar.scheduledJobs ?? []) append(job.name, 'scheduledJob', provenance);
  }
  return result;
}

function parseInfraSidecar(text: string, label: string): InfraSidecar {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch (error) {
    throw new Error(`invalid capability infra sidecar ${label}: ${String(error)}`);
  }
  const root = exactObject(
    raw,
    ['$schema', 'protocolVersion', 'databases', 'events', 'storage', 'secrets', 'scheduledJobs'],
    label,
  );
  if (root['protocolVersion'] !== INFRA_SIDECAR_PROTOCOL_VERSION)
    throw new Error(
      `invalid capability infra sidecar ${label}: protocolVersion must be ${INFRA_SIDECAR_PROTOCOL_VERSION}`,
    );
  const sidecar = root as InfraSidecar;
  for (const [index, database] of arrayOfObjects(
    root['databases'],
    ['name', 'engine', 'schemas'],
    `${label}.databases`,
  )) {
    requiredString(database['name'], `${label}.databases[${index}].name`);
    requiredString(database['engine'], `${label}.databases[${index}].engine`);
    stringArray(database['schemas'], `${label}.databases[${index}].schemas`);
  }
  if (root['events'] !== undefined) {
    const events = exactObject(root['events'], ['publishes', 'subscribes'], `${label}.events`);
    stringArray(events['publishes'], `${label}.events.publishes`);
    if (events['subscribes'] !== undefined) {
      if (!Array.isArray(events['subscribes']))
        throw new Error(`invalid capability infra sidecar ${label}.events.subscribes`);
      for (const [index, subscription] of events['subscribes'].entries()) {
        if (typeof subscription === 'string') {
          requiredString(subscription, `${label}.events.subscribes[${index}]`);
        } else {
          const value = exactObject(
            subscription,
            ['topic', 'delivery', 'push'],
            `${label}.events.subscribes[${index}]`,
          );
          requiredString(value['topic'], `${label}.events.subscribes[${index}].topic`);
        }
      }
    }
  }
  for (const [index, storage] of arrayOfObjects(
    root['storage'],
    ['name', 'access', 'public', 'retention'],
    `${label}.storage`,
  )) {
    requiredString(storage['name'], `${label}.storage[${index}].name`);
  }
  stringArray(root['secrets'], `${label}.secrets`);
  for (const [index, job] of arrayOfObjects(
    root['scheduledJobs'],
    ['name', 'schedule', 'entrypoint'],
    `${label}.scheduledJobs`,
  )) {
    requiredString(job['name'], `${label}.scheduledJobs[${index}].name`);
    requiredString(job['schedule'], `${label}.scheduledJobs[${index}].schedule`);
  }
  return sidecar;
}

function exactObject(value: unknown, allowed: string[], label: string): Record<string, unknown> {
  if (typeof value !== 'object' || value === null || Array.isArray(value))
    throw new Error(`invalid ${label}: expected object`);
  const record = value as Record<string, unknown>;
  const allowedSet = new Set(allowed);
  for (const key of Object.keys(record)) {
    if (!allowedSet.has(key)) throw new Error(`invalid ${label}: unknown field ${JSON.stringify(key)}`);
  }
  return record;
}

function arrayOfObjects(value: unknown, allowed: string[], label: string): [number, Record<string, unknown>][] {
  if (value === undefined) return [];
  if (!Array.isArray(value)) throw new Error(`invalid ${label}: expected array`);
  return value.map((item, index) => [index, exactObject(item, allowed, `${label}[${index}]`)]);
}

function requiredString(value: unknown, label: string): asserts value is string {
  if (typeof value !== 'string' || !value.trim()) throw new Error(`invalid ${label}: expected non-empty string`);
}

function stringArray(value: unknown, label: string): void {
  if (value === undefined) return;
  if (!Array.isArray(value)) throw new Error(`invalid ${label}: expected string array`);
  value.forEach((item, index) => {
    requiredString(item, `${label}[${index}]`);
  });
}

/** Merge only v1 dependencies while the previous scheduler is still active. */
function mergeDependencyCapabilityManifestsV1(
  projectRoot: string,
  capabilityRoot: string,
  target: CapabilityManifestInput,
  inventory: PackageInventory,
): void {
  const root = resolve(projectRoot, capabilityRoot);
  for (const metadata of inventory.ordered) {
    if (!metadata.capabilityManifestPath) continue;
    const path = resolve(projectRoot, metadata.capabilityManifestPath);
    const rel = relative(root, path);
    if (rel === '..' || rel.startsWith(`..${pathSeparator()}`) || isAbsolute(rel)) {
      throw new Error(
        `dependency capability manifest ${JSON.stringify(metadata.package)} escapes the stamped workspace root`,
      );
    }
    if (path.replaceAll('\\', '/').split('/').slice(-2).join('/') !== `schema/${MANIFEST_FILENAME}`) {
      throw new Error(
        `dependency capability manifest ${JSON.stringify(metadata.package)} must target schema/${MANIFEST_FILENAME}`,
      );
    }
    if (!existsSync(path)) continue;
    if (statSync(path).isDirectory()) throw new Error(`dependency capability manifest is a directory: ${path}`);
    const realRoot = realpathSync(root);
    const realPath = realpathSync(path);
    const realRel = relative(realRoot, realPath);
    if (realRel === '..' || realRel.startsWith(`..${pathSeparator()}`) || isAbsolute(realRel)) {
      throw new Error(
        `dependency capability manifest ${JSON.stringify(metadata.package)} escapes the stamped workspace root`,
      );
    }
    const dependency = parseStrictCapabilityManifestV1(readFileSync(path, 'utf8'), metadata.package);
    target.configDefinitions = [...(target.configDefinitions ?? []), ...(dependency.configDefinitions ?? [])];
    target.schemas = [...(target.schemas ?? []), ...(dependency.schemas ?? [])];
    target.discoverers = [...(target.discoverers ?? []), ...(dependency.discoverers ?? [])];
    target.migrations = [...(target.migrations ?? []), ...(dependency.migrations ?? [])];
    target.infraRequirements = [...(target.infraRequirements ?? []), ...(dependency.infraRequirements ?? [])];
    target.healthContributors = [...(target.healthContributors ?? []), ...(dependency.healthContributors ?? [])];
    target.lifecycleHooks = [...(target.lifecycleHooks ?? []), ...(dependency.lifecycleHooks ?? [])];
    target.requiredCapabilities = [...(target.requiredCapabilities ?? []), ...(dependency.requiredCapabilities ?? [])];
  }
}

function parseStrictCapabilityManifestV1(text: string, label: string): Manifest {
  let document: ReturnType<typeof parseCapabilityManifestDocument>;
  try {
    document = parseCapabilityManifestDocument(text);
  } catch (error) {
    throw new Error(`invalid dependency capability manifest ${JSON.stringify(label)}: ${String(error)}`);
  }
  if (document.protocolVersion === 2) {
    throw new Error(
      `dependency capability manifest ${JSON.stringify(label)} uses protocol v2 and cannot be aggregated by the rolling v1 activation producer without a lossy projection`,
    );
  }
  const diagnostics = validateCompleteV1Provenance(document.manifest);
  if (diagnostics.length > 0) throw new CapabilityManifestValidationError(diagnostics);
  return document.manifest;
}

function validateCompleteV1Provenance(manifest: Manifest): CapabilityDiagnostic[] {
  const diagnostics: CapabilityDiagnostic[] = [];
  const inspect = (field: string, provenance: Provenance): void => {
    const missing = (['package', 'version', 'evidencePath'] as const).filter(
      (key) => typeof provenance?.[key] !== 'string' || !provenance[key]?.trim(),
    );
    if (missing.length > 0) {
      diagnostics.push({
        severity: 'error',
        code: CAPABILITY_DIAGNOSTIC_CODES.missingProvenance,
        field: `${field}.provenance`,
        message: `emitted contribution is missing traceable ${missing.join(', ')}`,
      });
    }
  };
  const collections: [string, Array<{ provenance: Provenance }> | undefined][] = [
    ['configDefinitions', manifest.configDefinitions],
    ['schemas', manifest.schemas],
    ['discoverers', manifest.discoverers],
    ['migrations', manifest.migrations],
    ['infraRequirements', manifest.infraRequirements],
    ['healthContributors', manifest.healthContributors],
    ['lifecycleHooks', manifest.lifecycleHooks],
    ['packageVersions', manifest.packageVersions],
    ['requiredCapabilities', manifest.requiredCapabilities],
  ];
  for (const [name, entries] of collections) {
    entries?.forEach((entry, index) => {
      inspect(`${name}[${index}]`, entry.provenance);
    });
  }
  return diagnostics;
}

function mergeDependencyCapabilityManifests(
  projectRoot: string,
  capabilityRoot: string,
  target: ManifestV2,
  inventory: SourceBoundPackageInventory,
): void {
  Object.assign(target, canonicalCapabilityManifestV2(target));
  type Contribution = { identity: ContributionIdentity; provenance: ProvenanceV2 };
  const collections = [
    'configDefinitions',
    'schemas',
    'discoverers',
    'migrations',
    'infraRequirements',
    'healthContributors',
    'lifecycleHooks',
    'packages',
    'requiredCapabilities',
  ] as const;
  type CollectionName = (typeof collections)[number];
  const states = new Map<CollectionName, Map<string, { wire: string; origins: string[] }>>();
  const values = (manifest: ManifestV2, name: CollectionName): Contribution[] =>
    (manifest as unknown as Record<string, Contribution[] | undefined>)[name] ?? [];
  const canonicalWire = (name: CollectionName, value: Contribution): string =>
    serializeCapabilityManifestV2({ protocolVersion: 2, project: '__copy__', [name]: [value] } as ManifestV2);
  for (const name of collections) {
    const state = new Map<string, { wire: string; origins: string[] }>();
    for (const value of values(target, name)) {
      state.set(contributionIdentityKey(value.identity), {
        wire: canonicalWire(name, value),
        origins: [target.project],
      });
    }
    states.set(name, state);
  }
  const merge = (name: CollectionName, incoming: Contribution[], origin: string): void => {
    const targetValues = values(target, name);
    const state = states.get(name)!;
    for (const value of incoming) {
      const key = contributionIdentityKey(value.identity);
      const wire = canonicalWire(name, value);
      const existing = state.get(key);
      if (existing) {
        if (existing.wire !== wire) {
          const origins = [...new Set([...existing.origins, origin])].sort(cmp);
          throw new Error(
            `capability contribution ${contributionIdentityLabel(value.identity)} has conflicting copies in ${origins.join(', ')}`,
          );
        }
        if (!existing.origins.includes(origin)) existing.origins.push(origin);
        continue;
      }
      state.set(key, { wire, origins: [origin] });
      targetValues.push(value);
    }
    (target as unknown as Record<string, Contribution[]>)[name] = targetValues;
  };
  const root = resolve(projectRoot, capabilityRoot);
  for (const metadata of inventory.ordered) {
    if (!metadata.capabilityManifestPath) continue;
    const path = resolve(projectRoot, metadata.capabilityManifestPath);
    const rel = relative(root, path);
    if (rel === '..' || rel.startsWith(`..${pathSeparator()}`) || isAbsolute(rel)) {
      throw new Error(
        `dependency capability manifest ${JSON.stringify(metadata.package)} escapes the stamped workspace root`,
      );
    }
    if (path.replaceAll('\\', '/').split('/').slice(-2).join('/') !== `schema/${MANIFEST_FILENAME}`) {
      throw new Error(
        `dependency capability manifest ${JSON.stringify(metadata.package)} must target schema/${MANIFEST_FILENAME}`,
      );
    }
    if (!existsSync(path)) continue;
    if (statSync(path).isDirectory()) throw new Error(`dependency capability manifest is a directory: ${path}`);
    const realRoot = realpathSync(root);
    const realPath = realpathSync(path);
    const realRel = relative(realRoot, realPath);
    if (realRel === '..' || realRel.startsWith(`..${pathSeparator()}`) || isAbsolute(realRel)) {
      throw new Error(
        `dependency capability manifest ${JSON.stringify(metadata.package)} escapes the stamped workspace root`,
      );
    }
    const dependency = parseStrictCapabilityManifest(readFileSync(path, 'utf8'), metadata.package);
    if (dependency.project !== metadata.package) {
      throw new Error(
        `dependency capability manifest ${JSON.stringify(metadata.package)} declares container project ${JSON.stringify(dependency.project)}`,
      );
    }
    validateDependencyProvenance(dependency, inventory);
    for (const name of collections) merge(name, values(dependency, name), metadata.package);
  }
  Object.assign(target, canonicalCapabilityManifestV2(target));
}

function pathSeparator(): string {
  return process.platform === 'win32' ? '\\' : '/';
}

function parseStrictCapabilityManifest(text: string, label: string): ManifestV2 {
  let document: ReturnType<typeof parseCapabilityManifestDocument>;
  try {
    document = parseCapabilityManifestDocument(text);
  } catch (error) {
    throw new Error(`invalid dependency capability manifest ${JSON.stringify(label)}: ${String(error)}`);
  }
  if (document.protocolVersion === 1) {
    throw new Error(
      `dependency capability manifest ${JSON.stringify(label)} uses protocol v1 and cannot be aggregated into v2 without precise identity, migration kind, and declaration`,
    );
  }
  return canonicalCapabilityManifestV2(document.manifest);
}

function validateDependencyProvenance(manifest: ManifestV2, inventory: SourceBoundPackageInventory): void {
  const inspect = ({ identity, provenance }: { identity: ContributionIdentity; provenance: ProvenanceV2 }): void => {
    if (provenance.declaration.root === 'project') packageMetadata(inventory, provenance.project);
    else if (provenance.declaration.root === 'package' && provenance.package) {
      packageMetadata(inventory, provenance.package);
    } else {
      throw new Error(
        `contribution ${contributionIdentityLabel(identity)} uses unsupported workspace-root provenance for project-scoped dependency aggregation`,
      );
    }
    if (provenance.package) packageMetadata(inventory, provenance.package);
  };
  for (const entries of [
    manifest.configDefinitions,
    manifest.schemas,
    manifest.discoverers,
    manifest.migrations,
    manifest.infraRequirements,
    manifest.healthContributors,
    manifest.lifecycleHooks,
    manifest.packages,
    manifest.requiredCapabilities,
  ]) {
    for (const entry of entries ?? []) inspect(entry);
  }
}

function collectHealth(root: Module, plugins: PluginEntry[], inventory: PackageInventory): HealthContributor[] {
  const contributors: HealthContributor[] = [];
  for (const { plugin } of plugins) {
    if (isHealthChecker(plugin)) {
      contributors.push({ name: plugin.name, probe: 'health', provenance: frameworkProvenance(inventory, plugin) });
    }
    if (isReadinessChecker(plugin)) {
      contributors.push({ name: plugin.name, probe: 'readiness', provenance: frameworkProvenance(inventory, plugin) });
    }
  }
  for (const contribution of root.collectHealthContributions()) {
    contributors.push({
      name: contribution.name,
      probe: contribution.kind,
      provenance: frameworkProvenance(inventory),
    });
  }
  return contributors;
}

function collectLifecycle(plugins: PluginEntry[], inventory: PackageInventory): LifecycleHook[] {
  const hooks: LifecycleHook[] = [];
  for (const { plugin } of plugins) {
    if (!isLifecycleContributor(plugin)) {
      continue;
    }
    const provenance = frameworkProvenance(inventory, plugin);
    for (const contribution of plugin.lifecycleContributions()) {
      hooks.push({ name: contribution.name, phase: contribution.phase, provenance });
    }
  }
  return hooks;
}

/**
 * Collect the runtime-enforced domain access contracts of the plugin tree,
 * stamping the same identity and provenance every other contribution carries.
 *
 * The rows are EVIDENCE. `putnami architecture validate` reads them beside the
 * declared imports and reports a declared active contract nothing implements, or
 * an implemented one nobody declared. A row never authorizes anything: emitting
 * it cannot create a cross-domain permission, which is why this collector reads
 * only what a plugin already stated and invents no member.
 *
 * Nothing here validates the ARC/DARC vocabulary. The contract was validated by
 * `../architecture` when the component was constructed, and a second opinion here
 * would be the drift the checker exists to catch. Go twin:
 * `(*Application).collectDomainAccess`.
 */
function collectDomainAccess(plugins: PluginEntry[], inventory: SourceBoundPackageInventory): DomainAccessV2[] {
  const access: DomainAccessV2[] = [];
  for (const { plugin } of plugins) {
    if (!isDomainAccessContributor(plugin)) continue;
    const provenance = frameworkProvenance(inventory, plugin);
    for (const declaration of plugin.domainAccessContracts()) {
      // The mode is the subkind, so one project enforcing two modes of one
      // import keeps two distinct identities instead of colliding.
      const identity = localContributionIdentity(inventory, 'domainAccess', declaration.mode, declaration.import);
      access.push({
        identity,
        import: declaration.import,
        mode: declaration.mode,
        status: declaration.status,
        ...(declaration.transports && declaration.transports.length > 0
          ? { transports: [...declaration.transports] }
          : {}),
        ...(declaration.enforced ? { enforced: declaration.enforced } : {}),
        provenance: upgradeProvenance(provenance, identity, inventory),
      });
    }
  }
  // Deterministic before canonicalization, which re-sorts every collection by the
  // protocol's contribution identity. Sorting here costs nothing and keeps the
  // collector's own output stable for a reader stepping through it.
  return access.sort((left, right) =>
    left.import === right.import ? cmp(left.mode, right.mode) : cmp(left.import, right.import),
  );
}

function collectRequiredCapabilities(plugins: PluginEntry[], inventory: PackageInventory): RequiredCapability[] {
  const required: RequiredCapability[] = [];
  for (const { plugin } of plugins) {
    if (!isRequiredCapabilityContributor(plugin)) continue;
    const provenance = frameworkProvenance(inventory, plugin);
    const declarations = plugin.requiredCapabilities() as unknown;
    if (!Array.isArray(declarations)) {
      throw new CapabilityManifestValidationError([
        {
          severity: 'error',
          code: CAPABILITY_DIAGNOSTIC_CODES.parseError,
          field: 'requiredCapabilities',
          message: 'requiredCapabilities() must return an array',
        },
      ]);
    }
    for (const rawDeclaration of declarations) {
      const declaration =
        typeof rawDeclaration === 'object' && rawDeclaration !== null
          ? (rawDeclaration as Record<string, unknown>)
          : ({} as Record<string, unknown>);
      required.push({
        name: declaration['name'] as string,
        requires: declaration['requires'] as CapabilityKind[],
        provenance,
      });
    }
  }
  return required;
}

/**
 * Collect the explicit proof mappings declared beside this project's native
 * features.
 *
 * Provenance is the feature declaration's own call site, captured when the
 * module declared it — never a value an author writes. A declaration whose call
 * site could not be resolved is refused rather than published without
 * provenance: evidence a reader cannot locate is worse than no evidence.
 */
function collectFeatureProofMappings(owner: Module): GeneratedEvidenceMapping[] {
  const mappings: GeneratedEvidenceMapping[] = [];
  const root = owner.getRoot();
  for (const module of [root, ...root.collectModules()]) {
    const feature = module.getFeature();
    if (!feature?.proves?.length) continue;
    if (!feature.provenance?.path) {
      throw new Error(
        `feature '${feature.id}' proves a requirement but its declaration site could not be resolved to a project-relative path`,
      );
    }
    for (const [index, proof] of feature.proves.entries()) {
      if (!proof.requirement?.trim()) {
        throw new Error(`feature '${feature.id}' proof[${index}] must name a requirement`);
      }
      mappings.push({
        feature: feature.id,
        requirement: proof.requirement,
        kind: proof.contribution.kind,
        ...(proof.contribution.subkind ? { subkind: proof.contribution.subkind } : {}),
        key: proof.contribution.key,
        provenance: {
          root: 'project',
          path: feature.provenance.path,
          ...(feature.provenance.symbol ? { symbol: feature.provenance.symbol } : {}),
        },
      });
    }
  }
  return mappings;
}

/**
 * Emit generated evidence for the explicit mappings declared beside this
 * project's native features.
 *
 * It runs after the v2 manifest is validated, so every record resolves against
 * the exact manifest that ships beside it. The shared resolver owns the
 * association rules; this function only supplies what the build knows — the
 * published identities, the owner project, and its computed source binding.
 */
function buildFeatureEvidenceDocument(
  owner: Module,
  projectRoot: string,
  manifest: ManifestV2,
  inventory: SourceBoundPackageInventory,
): FeatureEvidenceDocument | undefined {
  const mappings = collectFeatureProofMappings(owner);
  if (mappings.length === 0) return undefined;

  const contributions: ContributionIdentity[] = [];
  for (const entries of [
    manifest.configDefinitions,
    manifest.schemas,
    manifest.discoverers,
    manifest.migrations,
    manifest.infraRequirements,
    manifest.healthContributors,
    manifest.lifecycleHooks,
    manifest.packages,
    manifest.requiredCapabilities,
  ] as Array<Array<{ identity: ContributionIdentity }> | undefined>) {
    for (const { identity } of entries ?? []) contributions.push(identity);
  }

  const { document, diagnostics } = buildGeneratedEvidence({
    issuer: { kind: 'build', id: FEATURE_EVIDENCE_ISSUER_ID },
    source: {
      root: 'project',
      ownerProject: manifest.project,
      binding: packageMetadata(inventory, manifest.project).sourceBinding,
    },
    authored: readAuthoredFeatureManifest(projectRoot),
    contributions,
    mappings,
  });
  if (!document) throw new FeatureEvidenceValidationError([...diagnostics]);
  return document;
}

/**
 * Read the stable project name from `<projectRoot>/.gen/version.json`, ignoring
 * its volatile deployable version (`0.1.0-<sha>`). Reading directly avoids the
 * deploy-time environment overlay and cache used by `getBuildInfo`.
 */
function readGeneratedVersion(projectRoot: string): GeneratedVersion {
  const path = joinPath(projectRoot, '.gen', 'version.json');
  if (!existsSync(path)) {
    return {};
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(readFileSync(path, 'utf8'));
  } catch (error) {
    throw new Error(`invalid generated version metadata at ${path}: ${String(error)}`);
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    throw new Error(`invalid generated version metadata at ${path}: expected object`);
  }
  const value = parsed as Record<string, unknown>;
  if (value['name'] !== undefined && typeof value['name'] !== 'string') {
    throw new Error(`invalid generated version metadata at ${path}: name must be a string`);
  }
  if (value['capabilityRoot'] !== undefined && typeof value['capabilityRoot'] !== 'string') {
    throw new Error(`invalid generated version metadata at ${path}: capabilityRoot must be a string`);
  }
  if (value['capabilityPackages'] !== undefined && !Array.isArray(value['capabilityPackages'])) {
    throw new Error(`invalid generated version metadata at ${path}: capabilityPackages must be an array`);
  }
  return {
    name: value['name'] as string | undefined,
    capabilityRoot: value['capabilityRoot'] as string | undefined,
    capabilityPackages: value['capabilityPackages'] as CapabilityPackageStamp[] | undefined,
  };
}

/**
 * Publish the legacy activation contract without feature evidence. This path is
 * used only when the running scheduler predates source bindings; v2 remains the
 * sole output once source-bound metadata is available.
 */
function writeLegacyCapabilityManifest(projectRoot: string, manifest: Manifest): EmittedCapabilityArtifacts {
  const dir = joinPath(projectRoot, EMIT_DIR);
  mkdirSync(dir, { recursive: true });
  const manifestPath = joinPath(dir, MANIFEST_FILENAME);
  const evidencePath = joinPath(dir, 'feature-evidence', FEATURE_EVIDENCE_FILENAME);
  const manifestTmp = `${manifestPath}.${randomBytes(6).toString('hex')}.tmp`;
  try {
    robustRemoveSync(evidencePath);
    writeFileSync(manifestTmp, serializeCapabilityManifest(manifest));
    robustRenameSync(manifestTmp, manifestPath);
  } catch (error) {
    rmSync(manifestTmp, { force: true });
    robustRemoveSync(manifestPath);
    robustRemoveSync(evidencePath);
    throw error;
  }
  return { manifestPath };
}

/** Publish optional evidence first and the capability manifest commit marker last. */
function writeCapabilityArtifacts(
  projectRoot: string,
  manifest: ManifestV2,
  evidence: FeatureEvidenceDocument | undefined,
): EmittedCapabilityArtifacts {
  const dir = joinPath(projectRoot, EMIT_DIR);
  mkdirSync(dir, { recursive: true });
  const manifestPath = joinPath(dir, MANIFEST_FILENAME);
  const evidenceDir = joinPath(dir, 'feature-evidence');
  const evidencePath = joinPath(evidenceDir, FEATURE_EVIDENCE_FILENAME);
  const manifestTmp = `${manifestPath}.${randomBytes(6).toString('hex')}.tmp`;
  const evidenceTmp = `${evidencePath}.${randomBytes(6).toString('hex')}.tmp`;
  const hasEvidence = (evidence?.evidence.length ?? 0) > 0;
  try {
    if (evidence && hasEvidence) {
      mkdirSync(evidenceDir, { recursive: true });
      writeFileSync(evidenceTmp, serializeFeatureEvidenceDocument(evidence));
      robustRenameSync(evidenceTmp, evidencePath);
    } else {
      robustRemoveSync(evidencePath);
    }
    writeFileSync(manifestTmp, serializeCapabilityManifestV2(manifest));
    robustRenameSync(manifestTmp, manifestPath);
  } catch (err) {
    rmSync(manifestTmp, { force: true });
    rmSync(evidenceTmp, { force: true });
    robustRemoveSync(manifestPath);
    robustRemoveSync(evidencePath);
    throw err;
  }
  return { manifestPath, evidencePath: hasEvidence ? evidencePath : undefined };
}

/** Remove TypeScript-owned publishable artifacts before a build attempt. */
export function invalidateCapabilityManifest(projectRoot = getProjectRoot()): void {
  robustRemoveSync(joinPath(projectRoot, EMIT_DIR, MANIFEST_FILENAME));
  robustRemoveSync(joinPath(projectRoot, EMIT_DIR, 'feature-evidence', FEATURE_EVIDENCE_FILENAME));
}
