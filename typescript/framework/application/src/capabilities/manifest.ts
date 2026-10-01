/**
 * Assembly + canonical serialization of the capability manifest.
 *
 * {@link buildCapabilityManifest} sorts every collection deterministically (the
 * same comparators the Go emitter uses) and stamps the protocol version +
 * schema URL. {@link serializeCapabilityManifest} renders the canonical wire
 * form — Go struct field order, two-space indentation, every `omitempty` field
 * dropped when empty, Go-compatible escaping, and a trailing newline —
 * byte-identical to Go's `json.MarshalIndent(m, "", "  ") + "\n"`.
 */

import { Buffer } from 'node:buffer';
import {
  CAPABILITIES_SCHEMA_URL,
  type ConfigDefinition,
  type ConfigField,
  type Discoverer,
  type HealthContributor,
  type InfraRequirement,
  type LifecycleHook,
  type Manifest,
  type MigrationBundle,
  type PackageVersion,
  type CapabilityKind,
  type Provenance,
  type RequiredCapability,
  type SchemaContribution,
} from './manifest.types';

export const CAPABILITY_DIAGNOSTIC_CODES = {
  parseError: 'capabilities.parse_error',
  unknownField: 'capabilities.unknown_field',
  invalidProtocolVersion: 'capabilities.invalid_protocol_version',
  missingProject: 'capabilities.missing_project',
  invalidName: 'capabilities.invalid_name',
  missingProvenance: 'capabilities.missing_provenance',
  invalidSourceKind: 'capabilities.invalid_source_kind',
  invalidSchemaKind: 'capabilities.invalid_schema_kind',
  invalidDiscovererKind: 'capabilities.invalid_discoverer_kind',
  invalidInfraKind: 'capabilities.invalid_infra_kind',
  invalidProbe: 'capabilities.invalid_probe',
  invalidPhase: 'capabilities.invalid_phase',
  invalidCapabilityKind: 'capabilities.invalid_capability_kind',
  missingRequires: 'capabilities.missing_requires',
  missingRequiredProvider: 'capabilities.missing_required_provider',
  duplicateProvider: 'capabilities.duplicate_provider',
  conflictingProvider: 'capabilities.conflicting_provider',
  missingContributionIdentity: 'capabilities.missing_contribution_identity',
  invalidContributionIdentity: 'capabilities.invalid_contribution_identity',
  missingOwnerProject: 'capabilities.missing_owner_project',
  ownerProjectMismatch: 'capabilities.owner_project_mismatch',
  identityMismatch: 'capabilities.identity_mismatch',
  duplicateContribution: 'capabilities.duplicate_contribution',
  conflictingContributionCopy: 'capabilities.conflicting_contribution_copy',
  missingMigrationKind: 'capabilities.missing_migration_kind',
  missingDomainAccessMode: 'capabilities.missing_domain_access_mode',
  missingDomainAccessStatus: 'capabilities.missing_domain_access_status',
  missingDeclaration: 'capabilities.missing_declaration',
  invalidSourceBinding: 'capabilities.invalid_source_binding',
  sourceBindingMismatch: 'capabilities.source_binding_mismatch',
  sourceBindingUnavailable: 'capabilities.source_binding_unavailable',
  invalidPath: 'capabilities.invalid_path',
  pathEscape: 'capabilities.path_escape',
  unresolvedReference: 'capabilities.unresolved_reference',
  ambiguousReference: 'capabilities.ambiguous_reference',
  v1Unreferenceable: 'capabilities.v1_unreferenceable',
} as const;

export interface CapabilityDiagnostic {
  severity: 'error';
  code: (typeof CAPABILITY_DIAGNOSTIC_CODES)[keyof typeof CAPABILITY_DIAGNOSTIC_CODES];
  message: string;
  field: string;
}

/** Error carrying the same structured diagnostics as the Go protocol validator. */
export class CapabilityManifestValidationError extends Error {
  readonly diagnostics: CapabilityDiagnostic[];

  constructor(diagnostics: CapabilityDiagnostic[]) {
    super(
      `capability manifest validation failed:\n${diagnostics
        .map((diagnostic) => `[${diagnostic.code}] ${diagnostic.field}: ${diagnostic.message}`)
        .join('\n')}`,
    );
    this.name = diagnostics[0]?.code ?? 'CapabilityManifestValidationError';
    this.diagnostics = diagnostics;
  }
}

/** Explicit contributions an emitter has collected for one project. */
export interface CapabilityManifestInput {
  protocolVersion: Manifest['protocolVersion'];
  project: string;
  /** Defaults to {@link CAPABILITIES_SCHEMA_URL}. */
  schema?: string;
  configDefinitions?: ConfigDefinition[];
  schemas?: SchemaContribution[];
  discoverers?: Discoverer[];
  migrations?: MigrationBundle[];
  infraRequirements?: InfraRequirement[];
  healthContributors?: HealthContributor[];
  lifecycleHooks?: LifecycleHook[];
  packageVersions?: PackageVersion[];
  requiredCapabilities?: RequiredCapability[];
}

/**
 * Byte-order string comparison, matching Go's `<` on UTF-8 strings. JavaScript
 * compares UTF-16 code units, which reverses some BMP/supplementary pairs, so
 * compare encoded bytes explicitly for all protocol-valid Unicode.
 */
function cmp(a: string, b: string): number {
  return Buffer.compare(Buffer.from(a, 'utf8'), Buffer.from(b, 'utf8'));
}

/** Provenance package with the empty default the sort tie-breaks on. */
function pkg(p: Provenance): string {
  return p.package ?? '';
}

/**
 * Build the manifest from explicit contributions: sort each collection with the
 * Go emitter's comparators, then stamp version + schema. Inputs are copied
 * before sorting, so the caller's arrays are not mutated. Fields inside a config
 * block are NOT reordered — they preserve declaration order, matching Go.
 */
export function buildCapabilityManifest(input: CapabilityManifestInput): Manifest {
  const configDefinitions = sorted(
    input.configDefinitions,
    (a, b) => cmp(a.path, b.path) || cmp(pkg(a.provenance), pkg(b.provenance)),
  );
  const schemas = sorted(
    input.schemas,
    (a, b) => cmp(a.name, b.name) || cmp(a.kind, b.kind) || cmp(pkg(a.provenance), pkg(b.provenance)),
  );
  const discoverers = sorted(
    input.discoverers,
    (a, b) => cmp(a.name, b.name) || cmp(a.kind, b.kind) || cmp(pkg(a.provenance), pkg(b.provenance)),
  );
  const migrations = sorted(
    input.migrations,
    (a, b) =>
      cmp(a.name, b.name) || cmp(a.datasource ?? '', b.datasource ?? '') || cmp(pkg(a.provenance), pkg(b.provenance)),
  );
  const infraRequirements = sorted(input.infraRequirements, (a, b) => cmp(a.name, b.name));
  const healthContributors = sorted(
    input.healthContributors,
    (a, b) => cmp(a.probe, b.probe) || cmp(a.name, b.name) || cmp(pkg(a.provenance), pkg(b.provenance)),
  );
  const lifecycleHooks = sorted(
    input.lifecycleHooks,
    (a, b) => cmp(a.phase, b.phase) || cmp(a.name, b.name) || cmp(pkg(a.provenance), pkg(b.provenance)),
  );
  const packageVersions = sorted(
    input.packageVersions,
    (a, b) => cmp(a.package, b.package) || cmp(a.version, b.version) || cmp(pkg(a.provenance), pkg(b.provenance)),
  );
  const requiredCapabilities = sorted(
    input.requiredCapabilities,
    (a, b) => cmp(a.name, b.name) || cmp(pkg(a.provenance), pkg(b.provenance)),
  )?.map((requirement) => ({
    ...requirement,
    requires: [...requirement.requires].sort(cmp),
  }));

  return {
    $schema: input.schema ?? CAPABILITIES_SCHEMA_URL,
    protocolVersion: input.protocolVersion,
    project: input.project,
    configDefinitions,
    schemas,
    discoverers,
    migrations,
    infraRequirements,
    healthContributors,
    lifecycleHooks,
    packageVersions,
    requiredCapabilities,
  };
}

const SOURCE_KINDS = new Set(['framework', 'manual', 'generated']);
const SCHEMA_KINDS = new Set(['route', 'openapi', 'proto']);
const DISCOVERER_KINDS = new Set(['source', 'config']);
const INFRA_KINDS = new Set(['database', 'events', 'storage', 'secret', 'scheduledJob']);
const PROBE_KINDS = new Set(['health', 'liveness', 'readiness']);
const LIFECYCLE_PHASES = new Set(['starter', 'stopper']);
const CAPABILITY_KINDS = new Set<CapabilityKind>([
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
]);

interface UnknownRecord {
  [key: string]: unknown;
  protocolVersion?: unknown;
  project?: unknown;
  configDefinitions?: unknown;
  schemas?: unknown;
  discoverers?: unknown;
  migrations?: unknown;
  infraRequirements?: unknown;
  healthContributors?: unknown;
  lifecycleHooks?: unknown;
  packageVersions?: unknown;
  requiredCapabilities?: unknown;
  path?: unknown;
  fields?: unknown;
  name?: unknown;
  provenance?: unknown;
  kind?: unknown;
  probe?: unknown;
  phase?: unknown;
  package?: unknown;
  requires?: unknown;
  sourceKind?: unknown;
}

/**
 * Validate the runtime manifest shape before sorting or serialization. This is
 * intentionally independent of TypeScript's compile-time types: plugin methods
 * are runtime extension points and can return malformed JavaScript values.
 */
export function validateCapabilityManifestStructure(manifest: Manifest): CapabilityDiagnostic[] {
  const diagnostics: CapabilityDiagnostic[] = [];
  const raw = asRecord(manifest);
  if (raw.protocolVersion !== 1) {
    diagnostics.push({
      severity: 'error',
      code: CAPABILITY_DIAGNOSTIC_CODES.invalidProtocolVersion,
      field: 'protocolVersion',
      message: `protocolVersion ${JSON.stringify(raw.protocolVersion)} is not supported by this emitter (want 1)`,
    });
  }
  validateName(
    'project',
    raw.project,
    diagnostics,
    CAPABILITY_DIAGNOSTIC_CODES.missingProject,
    'manifest is missing the project identifier',
  );

  for (const [index, config] of collection(raw.configDefinitions, 'configDefinitions', diagnostics).entries()) {
    const field = `configDefinitions[${index}]`;
    validateName(`${field}.path`, config.path, diagnostics);
    for (const [fieldIndex, configField] of collection(config.fields, `${field}.fields`, diagnostics).entries()) {
      validateName(`${field}.fields[${fieldIndex}].name`, configField.name, diagnostics);
    }
    validateProvenance(field, config.provenance, diagnostics);
  }
  for (const [index, schema] of collection(raw.schemas, 'schemas', diagnostics).entries()) {
    const field = `schemas[${index}]`;
    validateName(`${field}.name`, schema.name, diagnostics);
    validateEnum(
      `${field}.kind`,
      schema.kind,
      SCHEMA_KINDS,
      CAPABILITY_DIAGNOSTIC_CODES.invalidSchemaKind,
      diagnostics,
    );
    validateProvenance(field, schema.provenance, diagnostics);
  }
  for (const [index, discoverer] of collection(raw.discoverers, 'discoverers', diagnostics).entries()) {
    const field = `discoverers[${index}]`;
    validateName(`${field}.name`, discoverer.name, diagnostics);
    validateEnum(
      `${field}.kind`,
      discoverer.kind,
      DISCOVERER_KINDS,
      CAPABILITY_DIAGNOSTIC_CODES.invalidDiscovererKind,
      diagnostics,
    );
    validateProvenance(field, discoverer.provenance, diagnostics);
  }
  for (const [index, migration] of collection(raw.migrations, 'migrations', diagnostics).entries()) {
    const field = `migrations[${index}]`;
    validateName(`${field}.name`, migration.name, diagnostics);
    validateProvenance(field, migration.provenance, diagnostics);
  }
  for (const [index, infra] of collection(raw.infraRequirements, 'infraRequirements', diagnostics).entries()) {
    const field = `infraRequirements[${index}]`;
    validateName(`${field}.name`, infra.name, diagnostics);
    validateEnum(`${field}.kind`, infra.kind, INFRA_KINDS, CAPABILITY_DIAGNOSTIC_CODES.invalidInfraKind, diagnostics);
    validateProvenance(field, infra.provenance, diagnostics);
  }
  for (const [index, health] of collection(raw.healthContributors, 'healthContributors', diagnostics).entries()) {
    const field = `healthContributors[${index}]`;
    validateName(`${field}.name`, health.name, diagnostics);
    validateEnum(`${field}.probe`, health.probe, PROBE_KINDS, CAPABILITY_DIAGNOSTIC_CODES.invalidProbe, diagnostics);
    validateProvenance(field, health.provenance, diagnostics);
  }
  for (const [index, hook] of collection(raw.lifecycleHooks, 'lifecycleHooks', diagnostics).entries()) {
    const field = `lifecycleHooks[${index}]`;
    validateName(`${field}.name`, hook.name, diagnostics);
    validateEnum(`${field}.phase`, hook.phase, LIFECYCLE_PHASES, CAPABILITY_DIAGNOSTIC_CODES.invalidPhase, diagnostics);
    validateProvenance(field, hook.provenance, diagnostics);
  }
  for (const [index, packageVersion] of collection(raw.packageVersions, 'packageVersions', diagnostics).entries()) {
    const field = `packageVersions[${index}]`;
    validateName(`${field}.package`, packageVersion.package, diagnostics);
    validateProvenance(field, packageVersion.provenance, diagnostics);
  }
  for (const [index, requirement] of collection(
    raw.requiredCapabilities,
    'requiredCapabilities',
    diagnostics,
  ).entries()) {
    const field = `requiredCapabilities[${index}]`;
    validateName(`${field}.name`, requirement.name, diagnostics);
    if (!Array.isArray(requirement.requires) || requirement.requires.length === 0) {
      diagnostics.push({
        severity: 'error',
        code: CAPABILITY_DIAGNOSTIC_CODES.missingRequires,
        field: `${field}.requires`,
        message: `requiredCapability ${JSON.stringify(requirement.name)} must depend on at least one capability kind`,
      });
    }
    if (Array.isArray(requirement.requires)) {
      for (const [kindIndex, kind] of requirement.requires.entries()) {
        validateEnum(
          `${field}.requires[${kindIndex}]`,
          kind,
          CAPABILITY_KINDS,
          CAPABILITY_DIAGNOSTIC_CODES.invalidCapabilityKind,
          diagnostics,
        );
      }
    }
    validateProvenance(field, requirement.provenance, diagnostics);
  }
  return diagnostics;
}

/** Validate structure, provider uniqueness, and required-provider completeness. */
export function validateCapabilityManifest(manifest: Manifest): CapabilityDiagnostic[] {
  const diagnostics = validateCapabilityManifestStructure(manifest);
  if (diagnostics.length > 0) return diagnostics;
  diagnostics.push(
    ...providerCollisions(
      'configDefinitions',
      manifest.configDefinitions,
      (item) => item.path,
      (a, b) => JSON.stringify(a.fields ?? []) === JSON.stringify(b.fields ?? []),
    ),
    ...providerCollisions(
      'schemas',
      manifest.schemas,
      (item) => `${item.kind}:${item.name}`,
      (a, b) => (a.path ?? '') === (b.path ?? ''),
    ),
    ...providerCollisions(
      'discoverers',
      manifest.discoverers,
      (item) => `${item.kind}:${item.name}`,
      () => true,
    ),
    ...providerCollisions(
      'infraRequirements',
      manifest.infraRequirements,
      (item) => `${item.kind}:${item.name}`,
      () => true,
    ),
    ...providerCollisions(
      'healthContributors',
      manifest.healthContributors,
      (item) => `${item.probe}:${item.name}`,
      () => true,
    ),
    ...providerCollisions(
      'lifecycleHooks',
      manifest.lifecycleHooks,
      (item) => `${item.phase}:${item.name}`,
      () => true,
    ),
    ...providerCollisions(
      'packageVersions',
      manifest.packageVersions,
      (item) => item.package,
      (a, b) => a.version === b.version,
    ),
    ...providerCollisions(
      'requiredCapabilities',
      manifest.requiredCapabilities,
      (item) => item.name,
      (a, b) => equalCapabilityKinds(a.requires, b.requires),
    ),
  );

  const available = availableProviderKinds(manifest);
  for (const [requirementIndex, requirement] of (manifest.requiredCapabilities ?? []).entries()) {
    for (const [kindIndex, kind] of requirement.requires.entries()) {
      if (available.has(kind)) continue;
      diagnostics.push({
        severity: 'error',
        code: CAPABILITY_DIAGNOSTIC_CODES.missingRequiredProvider,
        field: `requiredCapabilities[${requirementIndex}].requires[${kindIndex}]`,
        message: `project ${JSON.stringify(manifest.project)} required capability ${JSON.stringify(requirement.name)} declared by ${provenanceLabel(requirement.provenance)} needs a ${JSON.stringify(kind)} provider, but the manifest has none; contribute a ${JSON.stringify(kind)} provider to project ${JSON.stringify(manifest.project)} or remove ${JSON.stringify(kind)} from requires`,
      });
    }
  }
  return diagnostics;
}

function asRecord(value: unknown): UnknownRecord {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as UnknownRecord) : {};
}

function collection(value: unknown, field: string, diagnostics: CapabilityDiagnostic[]): UnknownRecord[] {
  if (value === undefined) return [];
  if (!Array.isArray(value)) {
    diagnostics.push({
      severity: 'error',
      code: CAPABILITY_DIAGNOSTIC_CODES.parseError,
      field,
      message: `${field} must be an array`,
    });
    return [];
  }
  return value.map((item, index) => {
    if (typeof item === 'object' && item !== null && !Array.isArray(item)) return item as UnknownRecord;
    diagnostics.push({
      severity: 'error',
      code: CAPABILITY_DIAGNOSTIC_CODES.parseError,
      field: `${field}[${index}]`,
      message: `${field}[${index}] must be an object`,
    });
    return {};
  });
}

function validateName(
  field: string,
  value: unknown,
  diagnostics: CapabilityDiagnostic[],
  code: CapabilityDiagnostic['code'] = CAPABILITY_DIAGNOSTIC_CODES.invalidName,
  message = 'name is required',
): void {
  if (typeof value !== 'string' || value.trim() === '') {
    diagnostics.push({ severity: 'error', code, field, message });
  }
}

function validateEnum(
  field: string,
  value: unknown,
  valid: ReadonlySet<unknown>,
  code: CapabilityDiagnostic['code'],
  diagnostics: CapabilityDiagnostic[],
): void {
  if (!valid.has(value)) {
    diagnostics.push({
      severity: 'error',
      code,
      field,
      message: `${field} value ${JSON.stringify(value)} is not in the protocol v1 set`,
    });
  }
}

function validateProvenance(field: string, value: unknown, diagnostics: CapabilityDiagnostic[]): void {
  const provenance = asRecord(value);
  validateName(
    `${field}.provenance.project`,
    provenance.project,
    diagnostics,
    CAPABILITY_DIAGNOSTIC_CODES.missingProvenance,
    'entry must carry a provenance project',
  );
  validateEnum(
    `${field}.provenance.sourceKind`,
    provenance.sourceKind,
    SOURCE_KINDS,
    CAPABILITY_DIAGNOSTIC_CODES.invalidSourceKind,
    diagnostics,
  );
}

type WithProvenance = { provenance: Provenance };

function providerCollisions<T extends WithProvenance>(
  field: string,
  items: T[] | undefined,
  identity: (item: T) => string,
  equalPayload: (first: T, second: T) => boolean,
): CapabilityDiagnostic[] {
  const seen = new Map<string, number>();
  const diagnostics: CapabilityDiagnostic[] = [];
  for (const [index, item] of (items ?? []).entries()) {
    const key = identity(item);
    const firstIndex = seen.get(key);
    if (firstIndex === undefined) {
      seen.set(key, index);
      continue;
    }
    const first = (items as T[])[firstIndex];
    const duplicate = equalPayload(first, item);
    diagnostics.push({
      severity: 'error',
      code: duplicate ? CAPABILITY_DIAGNOSTIC_CODES.duplicateProvider : CAPABILITY_DIAGNOSTIC_CODES.conflictingProvider,
      field: `${field}[${index}]`,
      message: `provider ${JSON.stringify(key)} ${duplicate ? 'is declared more than once' : 'has conflicting declarations'} (${provenanceLabel(first.provenance)} and ${provenanceLabel(item.provenance)}); ${duplicate ? 'remove one duplicate declaration or give each provider a unique name' : 'reconcile the definitions or give each provider a unique name'}`,
    });
  }
  return diagnostics;
}

function provenanceLabel(provenance: Provenance): string {
  return provenance.evidencePath || provenance.package || provenance.project || 'unknown source';
}

function equalCapabilityKinds(first: CapabilityKind[], second: CapabilityKind[]): boolean {
  if (first.length !== second.length) return false;
  const counts = new Map<CapabilityKind, number>();
  for (const kind of first) counts.set(kind, (counts.get(kind) ?? 0) + 1);
  for (const kind of second) counts.set(kind, (counts.get(kind) ?? 0) - 1);
  return [...counts.values()].every((count) => count === 0);
}

function availableProviderKinds(manifest: Manifest): Set<CapabilityKind> {
  const available = new Set<CapabilityKind>();
  if (manifest.configDefinitions?.length) available.add('config');
  if (manifest.schemas?.length) available.add('schema');
  if (manifest.discoverers?.length) available.add('discoverer');
  if (manifest.migrations?.length) available.add('migration');
  if (manifest.migrations?.some((migration) => Boolean(migration.datasource?.trim()))) available.add('datasource');
  if (manifest.infraRequirements?.length) available.add('infra');
  if (manifest.lifecycleHooks?.length) available.add('lifecycle');
  if (manifest.packageVersions?.length) available.add('package');
  for (const health of manifest.healthContributors ?? []) available.add(health.probe);
  return available;
}

/** Return a sorted copy of `items`, or `undefined` when empty/absent. */
function sorted<T>(items: T[] | undefined, compare: (a: T, b: T) => number): T[] | undefined {
  if (!items || items.length === 0) {
    return undefined;
  }
  return [...items].sort(compare);
}

/**
 * Render the manifest to its canonical wire form. Object literals are built key
 * by key in the Go struct field order; optional fields are set to `undefined`
 * (which `JSON.stringify` drops) when empty, so `omitempty` semantics hold and
 * key order stays byte-identical to the Go emitter.
 */
export function serializeCapabilityManifest(manifest: Manifest): string {
  return `${escapeLikeGo(JSON.stringify(manifestOut(manifest), null, 2))}\n`;
}

/** Match encoding/json's default HTML and JavaScript-separator escaping. */
function escapeLikeGo(json: string): string {
  return json.replace(/[<>&\u2028\u2029]/g, (character) => {
    switch (character) {
      case '<':
        return '\\u003c';
      case '>':
        return '\\u003e';
      case '&':
        return '\\u0026';
      case '\u2028':
        return '\\u2028';
      case '\u2029':
        return '\\u2029';
      default:
        return character;
    }
  });
}

function emptyToUndefined<T>(items: T[] | undefined): T[] | undefined {
  return items && items.length > 0 ? items : undefined;
}

function provenanceOut(p: Provenance): Record<string, unknown> {
  return {
    project: p.project,
    package: p.package || undefined,
    version: p.version || undefined,
    sourceKind: p.sourceKind,
    evidencePath: p.evidencePath || undefined,
  };
}

function configFieldOut(f: ConfigField): Record<string, unknown> {
  return {
    name: f.name,
    type: f.type || undefined,
    sensitive: f.sensitive ? true : undefined,
  };
}

function configDefinitionOut(d: ConfigDefinition): Record<string, unknown> {
  return {
    path: d.path,
    fields: emptyToUndefined(d.fields)?.map(configFieldOut),
    provenance: provenanceOut(d.provenance),
  };
}

function schemaOut(s: SchemaContribution): Record<string, unknown> {
  return {
    name: s.name,
    kind: s.kind,
    path: s.path || undefined,
    provenance: provenanceOut(s.provenance),
  };
}

function discovererOut(d: Discoverer): Record<string, unknown> {
  return {
    name: d.name,
    kind: d.kind,
    provenance: provenanceOut(d.provenance),
  };
}

function migrationOut(m: MigrationBundle): Record<string, unknown> {
  return {
    name: m.name,
    datasource: m.datasource || undefined,
    digest: m.digest || undefined,
    provenance: provenanceOut(m.provenance),
  };
}

function infraOut(r: InfraRequirement): Record<string, unknown> {
  return {
    name: r.name,
    kind: r.kind,
    provenance: provenanceOut(r.provenance),
  };
}

function healthOut(h: HealthContributor): Record<string, unknown> {
  return {
    name: h.name,
    probe: h.probe,
    provenance: provenanceOut(h.provenance),
  };
}

function lifecycleOut(h: LifecycleHook): Record<string, unknown> {
  return {
    name: h.name,
    phase: h.phase,
    provenance: provenanceOut(h.provenance),
  };
}

function packageVersionOut(v: PackageVersion): Record<string, unknown> {
  return {
    package: v.package,
    version: v.version,
    provenance: provenanceOut(v.provenance),
  };
}

function requiredCapabilityOut(r: RequiredCapability): Record<string, unknown> {
  return {
    name: r.name,
    requires: r.requires,
    provenance: provenanceOut(r.provenance),
  };
}

function manifestOut(m: Manifest): Record<string, unknown> {
  return {
    $schema: m.$schema || undefined,
    protocolVersion: m.protocolVersion,
    project: m.project,
    configDefinitions: emptyToUndefined(m.configDefinitions)?.map(configDefinitionOut),
    schemas: emptyToUndefined(m.schemas)?.map(schemaOut),
    discoverers: emptyToUndefined(m.discoverers)?.map(discovererOut),
    migrations: emptyToUndefined(m.migrations)?.map(migrationOut),
    infraRequirements: emptyToUndefined(m.infraRequirements)?.map(infraOut),
    healthContributors: emptyToUndefined(m.healthContributors)?.map(healthOut),
    lifecycleHooks: emptyToUndefined(m.lifecycleHooks)?.map(lifecycleOut),
    packageVersions: emptyToUndefined(m.packageVersions)?.map(packageVersionOut),
    requiredCapabilities: emptyToUndefined(m.requiredCapabilities)?.map(requiredCapabilityOut),
  };
}
