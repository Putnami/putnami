import { Buffer } from 'node:buffer';
import { createHash } from 'node:crypto';
import {
  CAPABILITY_DIAGNOSTIC_CODES,
  type CapabilityDiagnostic,
  CapabilityManifestValidationError,
  validateCapabilityManifest,
} from './manifest';
import type {
  ArtifactLocation,
  CapabilityKind,
  CapabilityManifestDocument,
  ConfigDefinitionV2,
  ContributionIdentity,
  ContributionKind,
  DeclarationLocation,
  DiscovererV2,
  HealthContributorV2,
  InfraRequirementV2,
  LifecycleHookV2,
  Manifest,
  ManifestV2,
  MigrationBundleV2,
  PackageV2,
  PackageVersionV2,
  ProvenanceV2,
  RequiredCapabilityV2,
  SchemaContributionV2,
} from './manifest.types';

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
  'domainAccess',
]);
const SOURCE_KINDS = new Set(['framework', 'manual', 'generated']);
const SCHEMA_KINDS = new Set(['route', 'openapi', 'proto']);
const DISCOVERER_KINDS = new Set(['source', 'config']);
const INFRA_KINDS = new Set(['database', 'events', 'storage', 'secret', 'scheduledJob']);
const PROBE_KINDS = new Set(['health', 'liveness', 'readiness']);
const PHASES = new Set(['starter', 'stopper']);
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
const ROOTS = new Set(['workspace', 'project', 'package']);
const SOURCE_BINDING = /^source-v1:sha256:[0-9a-f]{64}$/;
const SHA256 = /^sha256:[0-9a-f]{64}$/;
const SCHEME = /^[A-Za-z][A-Za-z0-9+.-]*:/;

type RawObject = Record<string, unknown>;

function diagnostic(code: CapabilityDiagnostic['code'], field: string, message: string): CapabilityDiagnostic {
  return { severity: 'error', code, field, message };
}

function fail(code: CapabilityDiagnostic['code'], field: string, message: string): never {
  throw new CapabilityManifestValidationError([diagnostic(code, field, message)]);
}

function object(value: unknown, field: string): RawObject {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    fail(CAPABILITY_DIAGNOSTIC_CODES.parseError, field, `${field || 'manifest'} must be an object`);
  }
  return value as RawObject;
}

function exact(value: unknown, allowed: readonly string[], field: string): RawObject {
  const raw = object(value, field);
  const unknown = Object.keys(raw).find((key) => !allowed.includes(key));
  if (unknown) fail(CAPABILITY_DIAGNOSTIC_CODES.unknownField, unknown, `unknown field ${JSON.stringify(unknown)}`);
  return raw;
}

function each(value: unknown, field: string, inspect: (raw: RawObject, itemField: string) => void): void {
  if (value === undefined) return;
  if (!Array.isArray(value)) fail(CAPABILITY_DIAGNOSTIC_CODES.parseError, field, `${field} must be an array`);
  value.forEach((item, index) => {
    inspect(object(item, `${field}[${index}]`), `${field}[${index}]`);
  });
}

function strictV1(raw: RawObject): Manifest {
  exact(raw, MANIFEST_FIELDS, 'manifest');
  each(raw['configDefinitions'], 'configDefinitions', (item, field) => {
    exact(item, ['path', 'fields', 'provenance'], field);
    each(item['fields'], `${field}.fields`, (entry, entryField) =>
      exact(entry, ['name', 'type', 'sensitive'], entryField),
    );
    strictV1Provenance(item['provenance'], `${field}.provenance`);
  });
  each(raw['schemas'], 'schemas', (item, field) => strictV1Entry(item, ['name', 'kind', 'path', 'provenance'], field));
  each(raw['discoverers'], 'discoverers', (item, field) => strictV1Entry(item, ['name', 'kind', 'provenance'], field));
  each(raw['migrations'], 'migrations', (item, field) =>
    strictV1Entry(item, ['name', 'datasource', 'digest', 'provenance'], field),
  );
  each(raw['infraRequirements'], 'infraRequirements', (item, field) =>
    strictV1Entry(item, ['name', 'kind', 'provenance'], field),
  );
  each(raw['healthContributors'], 'healthContributors', (item, field) =>
    strictV1Entry(item, ['name', 'probe', 'provenance'], field),
  );
  each(raw['lifecycleHooks'], 'lifecycleHooks', (item, field) =>
    strictV1Entry(item, ['name', 'phase', 'provenance'], field),
  );
  each(raw['packageVersions'], 'packageVersions', (item, field) =>
    strictV1Entry(item, ['package', 'version', 'provenance'], field),
  );
  each(raw['requiredCapabilities'], 'requiredCapabilities', (item, field) =>
    strictV1Entry(item, ['name', 'requires', 'provenance'], field),
  );
  return raw as unknown as Manifest;
}

function strictV1Entry(item: RawObject, fields: string[], field: string): void {
  exact(item, fields, field);
  strictV1Provenance(item['provenance'], `${field}.provenance`);
}

function strictV1Provenance(value: unknown, field: string): void {
  exact(value, ['project', 'package', 'version', 'sourceKind', 'evidencePath'], field);
}

const MANIFEST_FIELDS = [
  '$schema',
  'protocolVersion',
  'project',
  'configDefinitions',
  'schemas',
  'discoverers',
  'migrations',
  'infraRequirements',
  'healthContributors',
  'lifecycleHooks',
  'packageVersions',
  'requiredCapabilities',
] as const;

const MANIFEST_V2_FIELDS = [...MANIFEST_FIELDS, 'packages', 'domainAccess'] as const;

function strictV2(raw: RawObject): ManifestV2 {
  exact(raw, MANIFEST_V2_FIELDS, 'manifest');
  each(raw['configDefinitions'], 'configDefinitions', (item, field) => {
    strictV2Entry(item, ['identity', 'path', 'fields', 'provenance'], field);
    each(item['fields'], `${field}.fields`, (entry, entryField) =>
      exact(entry, ['name', 'type', 'sensitive'], entryField),
    );
  });
  each(raw['schemas'], 'schemas', (item, field) =>
    strictV2Entry(item, ['identity', 'name', 'kind', 'path', 'provenance'], field),
  );
  each(raw['discoverers'], 'discoverers', (item, field) =>
    strictV2Entry(item, ['identity', 'name', 'kind', 'provenance'], field),
  );
  each(raw['migrations'], 'migrations', (item, field) =>
    strictV2Entry(item, ['identity', 'name', 'kind', 'datasource', 'digest', 'provenance'], field),
  );
  each(raw['infraRequirements'], 'infraRequirements', (item, field) =>
    strictV2Entry(item, ['identity', 'name', 'kind', 'provenance'], field),
  );
  each(raw['healthContributors'], 'healthContributors', (item, field) =>
    strictV2Entry(item, ['identity', 'name', 'probe', 'provenance'], field),
  );
  each(raw['lifecycleHooks'], 'lifecycleHooks', (item, field) =>
    strictV2Entry(item, ['identity', 'name', 'phase', 'provenance'], field),
  );
  each(raw['packages'], 'packages', (item, field) => strictV2Entry(item, ['identity', 'package', 'provenance'], field));
  each(raw['packageVersions'], 'packageVersions', (item, field) =>
    strictV2Entry(item, ['identity', 'package', 'version', 'provenance'], field),
  );
  each(raw['requiredCapabilities'], 'requiredCapabilities', (item, field) =>
    strictV2Entry(item, ['identity', 'name', 'requires', 'provenance'], field),
  );
  each(raw['domainAccess'], 'domainAccess', (item, field) => {
    strictV2Entry(item, ['identity', 'import', 'mode', 'status', 'transports', 'enforced', 'provenance'], field);
    each(item['transports'], `${field}.transports`, (entry, entryField) =>
      exact(entry, ['role', 'kind', 'contract', 'availability'], entryField),
    );
    if (item['enforced'] !== undefined)
      exact(
        item['enforced'],
        ['maxStaleness', 'onMissing', 'onStale', 'ordering', 'lateEvents', 'deletion', 'writer', 'rebuild'],
        `${field}.enforced`,
      );
  });
  return raw as unknown as ManifestV2;
}

function strictV2Entry(item: RawObject, fields: string[], field: string): void {
  exact(item, fields, field);
  if (item['identity'] !== undefined) {
    exact(item['identity'], ['ownerProject', 'kind', 'subkind', 'key'], `${field}.identity`);
  }
  if (item['provenance'] === undefined) return;
  const provenance = exact(
    item['provenance'],
    ['project', 'package', 'version', 'sourceKind', 'sourceBinding', 'declaration', 'artifacts'],
    `${field}.provenance`,
  );
  if (provenance['declaration'] !== undefined) {
    exact(provenance['declaration'], ['root', 'path', 'symbol'], `${field}.provenance.declaration`);
  }
  each(provenance['artifacts'], `${field}.provenance.artifacts`, (artifact, artifactField) =>
    exact(artifact, ['root', 'path', 'digest'], artifactField),
  );
}

/** Strictly parse and semantically validate either supported wire version. */
export function parseCapabilityManifestDocument(text: string): CapabilityManifestDocument {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch (error) {
    fail(CAPABILITY_DIAGNOSTIC_CODES.parseError, '', String(error));
  }
  const raw = object(parsed, 'manifest');
  const versionToken = topLevelProtocolVersionToken(text);
  if (versionToken === '1') {
    const manifest = strictV1(raw);
    const diagnostics = validateCapabilityManifest(manifest);
    if (diagnostics.length > 0) throw new CapabilityManifestValidationError(diagnostics);
    return { protocolVersion: 1, manifest };
  }
  if (versionToken === '2') {
    const manifest = strictV2(raw);
    const diagnostics = validateCapabilityManifestV2(manifest);
    if (diagnostics.length > 0) throw new CapabilityManifestValidationError(diagnostics);
    return { protocolVersion: 2, manifest };
  }
  fail(
    CAPABILITY_DIAGNOSTIC_CODES.invalidProtocolVersion,
    'protocolVersion',
    `protocolVersion token ${JSON.stringify(versionToken)} is not supported (want exact integer token 1 or 2)`,
  );
}

function topLevelProtocolVersionToken(text: string): string {
  let index = skipJSONWhitespace(text, 0);
  if (text[index] !== '{') fail(CAPABILITY_DIAGNOSTIC_CODES.parseError, '', 'manifest must be a JSON object');
  index = skipJSONWhitespace(text, index + 1);
  let versionToken: string | undefined;
  while (text[index] !== '}') {
    const keyStart = index;
    const keyEnd = scanJSONStringEnd(text, keyStart);
    const key = JSON.parse(text.slice(keyStart, keyEnd)) as string;
    index = skipJSONWhitespace(text, keyEnd);
    index = skipJSONWhitespace(text, index + 1); // JSON.parse already proved this byte is ':'.
    const valueStart = index;
    const valueEnd = scanJSONValueEnd(text, valueStart);
    if (key === 'protocolVersion') {
      if (versionToken !== undefined)
        fail(
          CAPABILITY_DIAGNOSTIC_CODES.invalidProtocolVersion,
          'protocolVersion',
          'protocolVersion must appear exactly once',
        );
      versionToken = text.slice(valueStart, valueEnd).trim();
    }
    index = skipJSONWhitespace(text, valueEnd);
    if (text[index] === ',') index = skipJSONWhitespace(text, index + 1);
  }
  if (versionToken === undefined)
    fail(CAPABILITY_DIAGNOSTIC_CODES.invalidProtocolVersion, 'protocolVersion', 'protocolVersion is required');
  return versionToken;
}

function skipJSONWhitespace(text: string, start: number): number {
  let index = start;
  while (text[index] === ' ' || text[index] === '\n' || text[index] === '\r' || text[index] === '\t') index++;
  return index;
}

function scanJSONStringEnd(text: string, start: number): number {
  let index = start + 1;
  while (index < text.length) {
    if (text[index] === '\\') {
      index += 2;
      continue;
    }
    if (text[index] === '"') return index + 1;
    index++;
  }
  return index;
}

function scanJSONValueEnd(text: string, start: number): number {
  if (text[start] === '"') return scanJSONStringEnd(text, start);
  if (text[start] !== '{' && text[start] !== '[') {
    let index = start;
    while (index < text.length && text[index] !== ',' && text[index] !== '}') index++;
    return index;
  }
  const closing: string[] = [text[start] === '{' ? '}' : ']'];
  let index = start + 1;
  while (closing.length > 0 && index < text.length) {
    if (text[index] === '"') {
      index = scanJSONStringEnd(text, index);
      continue;
    }
    if (text[index] === '{') closing.push('}');
    else if (text[index] === '[') closing.push(']');
    else if (text[index] === closing[closing.length - 1]) closing.pop();
    index++;
  }
  return index;
}

function required(
  field: string,
  value: unknown,
  diagnostics: CapabilityDiagnostic[],
  code: CapabilityDiagnostic['code'] = CAPABILITY_DIAGNOSTIC_CODES.invalidName,
): void {
  if (typeof value !== 'string' || value.trim() === '')
    diagnostics.push(diagnostic(code, field, `${field} is required`));
}

function enumValue(
  field: string,
  value: unknown,
  values: ReadonlySet<unknown>,
  code: CapabilityDiagnostic['code'],
  diagnostics: CapabilityDiagnostic[],
): void {
  if (!values.has(value))
    diagnostics.push(diagnostic(code, field, `${field} value ${JSON.stringify(value)} is not in the protocol v2 set`));
}

function validateIdentity(
  field: string,
  identity: ContributionIdentity | undefined,
  provenance: ProvenanceV2,
  kind: ContributionKind,
  subkind: string,
  key: string,
  diagnostics: CapabilityDiagnostic[],
): void {
  if (!identity || Object.keys(identity).length === 0) {
    diagnostics.push(
      diagnostic(
        CAPABILITY_DIAGNOSTIC_CODES.missingContributionIdentity,
        field,
        'v2 contribution identity is required',
      ),
    );
    return;
  }
  required(
    `${field}.ownerProject`,
    identity.ownerProject,
    diagnostics,
    CAPABILITY_DIAGNOSTIC_CODES.missingOwnerProject,
  );
  enumValue(
    `${field}.kind`,
    identity.kind,
    CONTRIBUTION_KINDS,
    CAPABILITY_DIAGNOSTIC_CODES.invalidContributionIdentity,
    diagnostics,
  );
  required(`${field}.key`, identity.key, diagnostics, CAPABILITY_DIAGNOSTIC_CODES.invalidContributionIdentity);
  const needsSubkind = !['config', 'package', 'requiredCapability'].includes(kind);
  if (needsSubkind)
    required(
      `${field}.subkind`,
      identity.subkind,
      diagnostics,
      CAPABILITY_DIAGNOSTIC_CODES.invalidContributionIdentity,
    );
  if (!needsSubkind && identity.subkind)
    diagnostics.push(
      diagnostic(
        CAPABILITY_DIAGNOSTIC_CODES.invalidContributionIdentity,
        `${field}.subkind`,
        `subkind must be absent for ${kind}`,
      ),
    );
  if (identity.ownerProject && provenance.project && identity.ownerProject !== provenance.project)
    diagnostics.push(
      diagnostic(
        CAPABILITY_DIAGNOSTIC_CODES.ownerProjectMismatch,
        `${field}.ownerProject`,
        `ownerProject ${JSON.stringify(identity.ownerProject)} must equal provenance.project ${JSON.stringify(provenance.project)}`,
      ),
    );
  if (identity.kind !== kind || (identity.subkind ?? '') !== subkind || identity.key !== key)
    diagnostics.push(
      diagnostic(
        CAPABILITY_DIAGNOSTIC_CODES.identityMismatch,
        field,
        `identity must equal (${JSON.stringify(kind)}, ${JSON.stringify(subkind)}, ${JSON.stringify(key)}) for this contribution`,
      ),
    );
}

function validateProtocolPath(field: string, value: unknown, diagnostics: CapabilityDiagnostic[]): void {
  if (typeof value !== 'string' || !value) {
    diagnostics.push(diagnostic(CAPABILITY_DIAGNOSTIC_CODES.invalidPath, field, 'path is required'));
    return;
  }
  if (value.includes('\\') || value.includes('\0') || value.split('/').some((part) => part === '' || part === '.'))
    diagnostics.push(
      diagnostic(CAPABILITY_DIAGNOSTIC_CODES.invalidPath, field, 'path must be its slash-separated lexical clean form'),
    );
  if (value.startsWith('/') || SCHEME.test(value) || value.split('/').includes('..'))
    diagnostics.push(
      diagnostic(CAPABILITY_DIAGNOSTIC_CODES.pathEscape, field, 'path must remain relative to its declared root'),
    );
}

function validateLocation(
  field: string,
  location: DeclarationLocation | ArtifactLocation | undefined,
  provenance: ProvenanceV2,
  diagnostics: CapabilityDiagnostic[],
): void {
  if (!location) return;
  enumValue(`${field}.root`, location.root, ROOTS, CAPABILITY_DIAGNOSTIC_CODES.invalidPath, diagnostics);
  validateProtocolPath(`${field}.path`, location.path, diagnostics);
  if (location.root === 'package' && (typeof provenance.package !== 'string' || provenance.package.trim() === ''))
    diagnostics.push(
      diagnostic(
        CAPABILITY_DIAGNOSTIC_CODES.missingProvenance,
        `${field}.root`,
        'package locations require provenance package',
      ),
    );
}

function validateProvenance(
  field: string,
  provenance: ProvenanceV2 | undefined,
  diagnostics: CapabilityDiagnostic[],
): void {
  if (!provenance) {
    diagnostics.push(diagnostic(CAPABILITY_DIAGNOSTIC_CODES.missingProvenance, field, 'v2 provenance is required'));
    return;
  }
  required(`${field}.project`, provenance.project, diagnostics, CAPABILITY_DIAGNOSTIC_CODES.missingProvenance);
  enumValue(
    `${field}.sourceKind`,
    provenance.sourceKind,
    SOURCE_KINDS,
    CAPABILITY_DIAGNOSTIC_CODES.invalidSourceKind,
    diagnostics,
  );
  if (provenance.sourceBinding !== undefined && !SOURCE_BINDING.test(provenance.sourceBinding))
    diagnostics.push(
      diagnostic(
        CAPABILITY_DIAGNOSTIC_CODES.invalidSourceBinding,
        `${field}.sourceBinding`,
        'legacy sourceBinding must use source-v1:sha256:<64-lower-hex>',
      ),
    );
  if (!provenance.declaration)
    diagnostics.push(
      diagnostic(
        CAPABILITY_DIAGNOSTIC_CODES.missingDeclaration,
        `${field}.declaration`,
        'a precise declaration is required',
      ),
    );
  else validateLocation(`${field}.declaration`, provenance.declaration, provenance, diagnostics);
  for (const [index, artifact] of validationArtifacts(provenance.artifacts).entries()) {
    const artifactField = `${field}.artifacts[${index}]`;
    validateLocation(artifactField, artifact, provenance, diagnostics);
    if (artifact.digest && !SHA256.test(artifact.digest))
      diagnostics.push(
        diagnostic(
          CAPABILITY_DIAGNOSTIC_CODES.invalidSourceBinding,
          `${artifactField}.digest`,
          'artifact digest must use sha256:<64-lower-hex>',
        ),
      );
    if (
      provenance.declaration &&
      artifact.root === provenance.declaration.root &&
      artifact.path === provenance.declaration.path
    )
      diagnostics.push(
        diagnostic(
          CAPABILITY_DIAGNOSTIC_CODES.invalidPath,
          `${artifactField}.path`,
          'artifact must not duplicate the declaration location',
        ),
      );
  }
}

/** Validate v2 identities, contribution agreement, provenance, and duplicates. */
export function validateCapabilityManifestV2(manifest: ManifestV2): CapabilityDiagnostic[] {
  const diagnostics: CapabilityDiagnostic[] = [];
  if (manifest.protocolVersion !== 2)
    diagnostics.push(
      diagnostic(CAPABILITY_DIAGNOSTIC_CODES.invalidProtocolVersion, 'protocolVersion', 'protocolVersion must equal 2'),
    );
  required('project', manifest.project, diagnostics, CAPABILITY_DIAGNOSTIC_CODES.missingProject);
  const identities: [ContributionIdentity, string][] = [];
  const inspect = (
    field: string,
    identity: ContributionIdentity,
    provenance: ProvenanceV2,
    kind: ContributionKind,
    subkind: string,
    key: string,
  ): void => {
    validateIdentity(
      `${field}.identity`,
      identity,
      provenance ?? ({} as ProvenanceV2),
      kind,
      subkind,
      key,
      diagnostics,
    );
    validateProvenance(`${field}.provenance`, provenance, diagnostics);
    if (identity && Object.keys(identity).length > 0) identities.push([identity, field]);
  };
  for (const [index, item] of validationItems(manifest.configDefinitions).entries()) {
    required(`configDefinitions[${index}].path`, item.path, diagnostics);
    item.fields?.forEach((value, fieldIndex) => {
      required(`configDefinitions[${index}].fields[${fieldIndex}].name`, value.name, diagnostics);
    });
    inspect(`configDefinitions[${index}]`, item.identity, item.provenance, 'config', '', item.path);
  }
  for (const [index, item] of validationItems(manifest.schemas).entries()) {
    required(`schemas[${index}].name`, item.name, diagnostics);
    enumValue(
      `schemas[${index}].kind`,
      item.kind,
      SCHEMA_KINDS,
      CAPABILITY_DIAGNOSTIC_CODES.invalidSchemaKind,
      diagnostics,
    );
    inspect(`schemas[${index}]`, item.identity, item.provenance, 'schema', item.kind, item.name);
  }
  for (const [index, item] of validationItems(manifest.discoverers).entries()) {
    required(`discoverers[${index}].name`, item.name, diagnostics);
    enumValue(
      `discoverers[${index}].kind`,
      item.kind,
      DISCOVERER_KINDS,
      CAPABILITY_DIAGNOSTIC_CODES.invalidDiscovererKind,
      diagnostics,
    );
    inspect(`discoverers[${index}]`, item.identity, item.provenance, 'discoverer', item.kind, item.name);
  }
  for (const [index, item] of validationItems(manifest.migrations).entries()) {
    required(`migrations[${index}].name`, item.name, diagnostics);
    const migrationKind = typeof item.kind === 'string' ? item.kind : '';
    if (!migrationKind.trim())
      diagnostics.push(
        diagnostic(
          CAPABILITY_DIAGNOSTIC_CODES.missingMigrationKind,
          `migrations[${index}].kind`,
          'migration kind is required in v2',
        ),
      );
    inspect(`migrations[${index}]`, item.identity, item.provenance, 'migration', migrationKind, item.name);
  }
  for (const [index, item] of validationItems(manifest.infraRequirements).entries()) {
    required(`infraRequirements[${index}].name`, item.name, diagnostics);
    enumValue(
      `infraRequirements[${index}].kind`,
      item.kind,
      INFRA_KINDS,
      CAPABILITY_DIAGNOSTIC_CODES.invalidInfraKind,
      diagnostics,
    );
    inspect(`infraRequirements[${index}]`, item.identity, item.provenance, 'infra', item.kind, item.name);
  }
  for (const [index, item] of validationItems(manifest.healthContributors).entries()) {
    required(`healthContributors[${index}].name`, item.name, diagnostics);
    enumValue(
      `healthContributors[${index}].probe`,
      item.probe,
      PROBE_KINDS,
      CAPABILITY_DIAGNOSTIC_CODES.invalidProbe,
      diagnostics,
    );
    inspect(`healthContributors[${index}]`, item.identity, item.provenance, 'health', item.probe, item.name);
  }
  for (const [index, item] of validationItems(manifest.lifecycleHooks).entries()) {
    required(`lifecycleHooks[${index}].name`, item.name, diagnostics);
    enumValue(
      `lifecycleHooks[${index}].phase`,
      item.phase,
      PHASES,
      CAPABILITY_DIAGNOSTIC_CODES.invalidPhase,
      diagnostics,
    );
    inspect(`lifecycleHooks[${index}]`, item.identity, item.provenance, 'lifecycle', item.phase, item.name);
  }
  for (const [index, item] of validationItems(manifest.packages).entries()) {
    required(`packages[${index}].package`, item.package, diagnostics);
    inspect(`packages[${index}]`, item.identity, item.provenance, 'package', '', item.package);
  }
  for (const [index, item] of validationItems(manifest.packageVersions).entries()) {
    required(`packageVersions[${index}].package`, item.package, diagnostics);
    inspect(`packageVersions[${index}]`, item.identity, item.provenance, 'package', '', item.package);
  }
  const requiredCapabilities = validationItems(manifest.requiredCapabilities);
  for (const [index, item] of requiredCapabilities.entries()) {
    required(`requiredCapabilities[${index}].name`, item.name, diagnostics);
    if (!Array.isArray(item.requires) || item.requires.length === 0)
      diagnostics.push(
        diagnostic(
          CAPABILITY_DIAGNOSTIC_CODES.missingRequires,
          `requiredCapabilities[${index}].requires`,
          'requires must not be empty',
        ),
      );
    if (Array.isArray(item.requires))
      [...item.requires]
        .sort((left, right) => cmp(validationString(left), validationString(right)))
        .forEach((kind, kindIndex) => {
          enumValue(
            `requiredCapabilities[${index}].requires[${kindIndex}]`,
            kind,
            CAPABILITY_KINDS,
            CAPABILITY_DIAGNOSTIC_CODES.invalidCapabilityKind,
            diagnostics,
          );
        });
    inspect(`requiredCapabilities[${index}]`, item.identity, item.provenance, 'requiredCapability', '', item.name);
  }
  for (const [index, item] of validationItems(manifest.domainAccess).entries()) {
    required(`domainAccess[${index}].import`, item.import, diagnostics);
    // The vocabulary itself stays opaque: protocols/architecture owns what a
    // mode or a status means, and a second copy here is exactly the drift a
    // checker joining the two documents exists to catch.
    if (!validationString(item.mode))
      diagnostics.push(
        diagnostic(
          CAPABILITY_DIAGNOSTIC_CODES.missingDomainAccessMode,
          `domainAccess[${index}].mode`,
          `domainAccess ${JSON.stringify(item.import)} must carry the access mode it enforces`,
        ),
      );
    if (!validationString(item.status))
      diagnostics.push(
        diagnostic(
          CAPABILITY_DIAGNOSTIC_CODES.missingDomainAccessStatus,
          `domainAccess[${index}].status`,
          `domainAccess ${JSON.stringify(item.import)} must carry the declared lifecycle status`,
        ),
      );
    // Transports have no contribution identity of their own — they are members
    // of one — so they are walked in declared order rather than through the
    // identity sort every contribution collection uses.
    for (const [transportIndex, transport] of (item.transports ?? []).entries()) {
      const field = `domainAccess[${index}].transports[${transportIndex}]`;
      required(`${field}.role`, transport.role, diagnostics);
      required(`${field}.kind`, transport.kind, diagnostics);
      required(`${field}.availability`, transport.availability, diagnostics);
    }
    inspect(`domainAccess[${index}]`, item.identity, item.provenance, 'domainAccess', item.mode, item.import);
  }
  const seen = new Map<string, string>();
  for (const [identity, field] of identities) {
    const key = identityKey(identity);
    const first = seen.get(key);
    if (first)
      diagnostics.push(
        diagnostic(
          CAPABILITY_DIAGNOSTIC_CODES.duplicateContribution,
          `${field}.identity`,
          `contribution identity is duplicated within the manifest (${first} and ${field})`,
        ),
      );
    else seen.set(key, field);
  }

  const available = availableProviderKindsV2(manifest);
  for (const [requirementIndex, requirement] of requiredCapabilities.entries()) {
    if (!Array.isArray(requirement.requires)) continue;
    for (const [kindIndex, kind] of [...requirement.requires]
      .sort((left, right) => cmp(validationString(left), validationString(right)))
      .entries()) {
      if (!CAPABILITY_KINDS.has(kind) || available.has(kind)) continue;
      diagnostics.push(
        diagnostic(
          CAPABILITY_DIAGNOSTIC_CODES.missingRequiredProvider,
          `requiredCapabilities[${requirementIndex}].requires[${kindIndex}]`,
          `project ${JSON.stringify(manifest.project)} required capability ${JSON.stringify(requirement.name)} declared by ${provenanceLabelV2(requirement.provenance)} needs a ${JSON.stringify(kind)} provider, but the manifest has none; contribute a ${JSON.stringify(kind)} provider to project ${JSON.stringify(manifest.project)} or remove ${JSON.stringify(kind)} from requires`,
        ),
      );
    }
  }
  return diagnostics.sort(compareDiagnosticsV2);
}

function validationItems<T extends { identity: ContributionIdentity }>(items: T[] | undefined): T[] {
  return [...(items ?? [])].sort((left, right) => compareValidationIdentity(left.identity, right.identity));
}

function compareValidationIdentity(left: unknown, right: unknown): number {
  const leftIdentity = typeof left === 'object' && left !== null ? (left as RawObject) : {};
  const rightIdentity = typeof right === 'object' && right !== null ? (right as RawObject) : {};
  return (
    cmp(validationString(leftIdentity['ownerProject']), validationString(rightIdentity['ownerProject'])) ||
    cmp(validationString(leftIdentity['kind']), validationString(rightIdentity['kind'])) ||
    cmp(validationString(leftIdentity['subkind']), validationString(rightIdentity['subkind'])) ||
    cmp(validationString(leftIdentity['key']), validationString(rightIdentity['key']))
  );
}

function validationArtifacts(items: ArtifactLocation[] | undefined): ArtifactLocation[] {
  return [...(items ?? [])].sort(
    (left, right) =>
      cmp(validationString(left.root), validationString(right.root)) ||
      cmp(validationString(left.path), validationString(right.path)) ||
      cmp(validationString(left.digest), validationString(right.digest)),
  );
}

function validationString(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

/** Container-scoped v2 provider projection, preserving v1 aggregate semantics. */
export function availableProviderKindsV2(manifest: ManifestV2): Set<CapabilityKind> {
  const available = new Set<CapabilityKind>();
  if (manifest.configDefinitions?.length) available.add('config');
  if (manifest.schemas?.length) available.add('schema');
  if (manifest.discoverers?.length) available.add('discoverer');
  if (manifest.migrations?.length) available.add('migration');
  if (
    manifest.migrations?.some(
      (migration) => typeof migration.datasource === 'string' && migration.datasource.trim() !== '',
    )
  )
    available.add('datasource');
  if (manifest.infraRequirements?.length) available.add('infra');
  if (manifest.lifecycleHooks?.length) available.add('lifecycle');
  if (manifest.packages?.length || manifest.packageVersions?.length) available.add('package');
  for (const health of manifest.healthContributors ?? []) {
    if (PROBE_KINDS.has(health.probe)) available.add(health.probe);
  }
  return available;
}

function provenanceLabelV2(provenance: ProvenanceV2): string {
  return provenance?.declaration?.path || provenance?.package || provenance?.project || 'unknown source';
}

function compareDiagnosticsV2(left: CapabilityDiagnostic, right: CapabilityDiagnostic): number {
  return (
    cmp(left.severity, right.severity) ||
    cmp(left.code, right.code) ||
    cmp(left.field, right.field) ||
    cmp(left.message, right.message)
  );
}

function cmp(left: string, right: string): number {
  return Buffer.compare(Buffer.from(left, 'utf8'), Buffer.from(right, 'utf8'));
}

function compareIdentity(left: ContributionIdentity, right: ContributionIdentity): number {
  return (
    cmp(left.ownerProject, right.ownerProject) ||
    cmp(left.kind, right.kind) ||
    cmp(left.subkind ?? '', right.subkind ?? '') ||
    cmp(left.key, right.key)
  );
}

function identityKey(identity: ContributionIdentity): string {
  return JSON.stringify([identity.ownerProject, identity.kind, identity.subkind ?? '', identity.key]);
}

function artifacts(value: ProvenanceV2): ProvenanceV2 {
  const sorted = value.artifacts?.length
    ? [...value.artifacts].sort(
        (left, right) =>
          cmp(left.root, right.root) || cmp(left.path, right.path) || cmp(left.digest ?? '', right.digest ?? ''),
      )
    : undefined;
  return { ...value, version: undefined, sourceBinding: undefined, artifacts: sorted };
}

function sorted<T extends { identity: ContributionIdentity; provenance: ProvenanceV2 }>(
  items: T[] | undefined,
): T[] | undefined {
  if (!items?.length) return undefined;
  return [...items]
    .sort((left, right) => compareIdentity(left.identity, right.identity))
    .map((item) => ({ ...item, provenance: artifacts(item.provenance) }));
}

/**
 * The direct capability surface of a manifest: the project itself, every owner
 * project that declares a contribution in it, and every package those
 * contributions were declared by.
 *
 * Go twin: `capabilities.CapabilitySurfaceV2`. Package contributions are
 * excluded from the derivation on purpose — a package entry may not justify
 * itself, or the reachable closure would be self-certifying.
 */
export function capabilitySurfaceV2(manifest: ManifestV2): Set<string> {
  const surface = new Set<string>();
  const add = (...values: (string | undefined)[]): void => {
    for (const value of values) if (value?.trim()) surface.add(value);
  };
  add(manifest.project);
  for (const items of [
    manifest.configDefinitions,
    manifest.schemas,
    manifest.discoverers,
    manifest.migrations,
    manifest.infraRequirements,
    manifest.healthContributors,
    manifest.lifecycleHooks,
    manifest.requiredCapabilities,
    manifest.domainAccess,
  ]) {
    for (const item of items ?? []) {
      add(item.identity?.ownerProject, item.provenance?.project, item.provenance?.package);
    }
  }
  return surface;
}

/**
 * Scope package contributions to the manifest's capability surface — the
 * committed-manifest shape.
 *
 * The alternative, the workload's reachable module closure, is workspace state:
 * adding a dependency edge anywhere would rewrite every workload's committed
 * manifest at once. The closure stays in the ephemeral scheduler stamp
 * (`.gen/version.json`), never in committed content. This is a subset rule —
 * emission drops entries, it never invents one — and it applies to EMISSION
 * only, so validation and reference resolution still read a document as written.
 *
 * Go twin: `capabilities.ScopePackagesToContributionsV2`; the two must agree
 * byte-for-byte on the canonical form.
 */
function scopePackagesToContributions(manifest: ManifestV2, packages: PackageV2[]): PackageV2[] {
  const surface = capabilitySurfaceV2(manifest);
  return packages.filter(
    (entry) =>
      surface.has(entry.identity?.ownerProject) || surface.has(entry.package) || surface.has(entry.provenance?.project),
  );
}

/** Return a canonical sorted copy without mutating caller-owned arrays. */
export function canonicalCapabilityManifestV2(manifest: ManifestV2): ManifestV2 {
  const packages: PackageV2[] = scopePackagesToContributions(manifest, [
    ...(manifest.packages ?? []),
    ...(manifest.packageVersions ?? []).map(({ identity, package: packageName, provenance }) => ({
      identity,
      package: packageName,
      provenance,
    })),
  ]);
  return {
    $schema: manifest.$schema,
    protocolVersion: 2,
    project: manifest.project,
    configDefinitions: sorted(manifest.configDefinitions),
    schemas: sorted(manifest.schemas),
    discoverers: sorted(manifest.discoverers),
    migrations: sorted(manifest.migrations),
    infraRequirements: sorted(manifest.infraRequirements),
    healthContributors: sorted(manifest.healthContributors),
    lifecycleHooks: sorted(manifest.lifecycleHooks),
    packages: sorted(packages),
    packageVersions: undefined,
    requiredCapabilities: sorted(manifest.requiredCapabilities)?.map((item) => ({
      ...item,
      requires: [...item.requires].sort(cmp),
    })),
    // Transports are sorted by ROLE, which is their identity within one
    // contract: a projection has exactly one bootstrap and one updates carrier,
    // so the order they were listed in carries no meaning.
    domainAccess: sorted(manifest.domainAccess)?.map((item) => ({
      ...item,
      transports: item.transports ? [...item.transports].sort((left, right) => cmp(left.role, right.role)) : undefined,
    })),
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

/** Canonical v2 JSON: declared field order, omissions, two spaces, newline. */
export function serializeCapabilityManifestV2(manifest: ManifestV2): string {
  return `${escapeLikeGo(JSON.stringify(manifestV2Out(canonicalCapabilityManifestV2(manifest)), null, 2))}\n`;
}

function identityOut(identity: ContributionIdentity): RawObject {
  return {
    ownerProject: identity.ownerProject,
    kind: identity.kind,
    subkind: identity.subkind || undefined,
    key: identity.key,
  };
}

function declarationOut(value: DeclarationLocation): RawObject {
  return { root: value.root, path: value.path, symbol: value.symbol || undefined };
}

function provenanceOut(value: ProvenanceV2): RawObject {
  return {
    project: value.project,
    package: value.package || undefined,
    sourceKind: value.sourceKind,
    declaration: declarationOut(value.declaration),
    artifacts: value.artifacts?.length
      ? value.artifacts.map((artifact) => ({
          root: artifact.root,
          path: artifact.path,
          digest: artifact.digest || undefined,
        }))
      : undefined,
  };
}

function manifestV2Out(manifest: ManifestV2): RawObject {
  return {
    $schema: manifest.$schema || undefined,
    protocolVersion: manifest.protocolVersion,
    project: manifest.project,
    configDefinitions: manifest.configDefinitions?.length
      ? manifest.configDefinitions.map((item) => ({
          identity: identityOut(item.identity),
          path: item.path,
          fields: item.fields?.length
            ? item.fields.map((field) => ({
                name: field.name,
                type: field.type || undefined,
                sensitive: field.sensitive ? true : undefined,
              }))
            : undefined,
          provenance: provenanceOut(item.provenance),
        }))
      : undefined,
    schemas: manifest.schemas?.length
      ? manifest.schemas.map((item) => ({
          identity: identityOut(item.identity),
          name: item.name,
          kind: item.kind,
          path: item.path || undefined,
          provenance: provenanceOut(item.provenance),
        }))
      : undefined,
    discoverers: manifest.discoverers?.length
      ? manifest.discoverers.map((item) => ({
          identity: identityOut(item.identity),
          name: item.name,
          kind: item.kind,
          provenance: provenanceOut(item.provenance),
        }))
      : undefined,
    migrations: manifest.migrations?.length
      ? manifest.migrations.map((item) => ({
          identity: identityOut(item.identity),
          name: item.name,
          kind: item.kind,
          datasource: item.datasource || undefined,
          digest: item.digest || undefined,
          provenance: provenanceOut(item.provenance),
        }))
      : undefined,
    infraRequirements: manifest.infraRequirements?.length
      ? manifest.infraRequirements.map((item) => ({
          identity: identityOut(item.identity),
          name: item.name,
          kind: item.kind,
          provenance: provenanceOut(item.provenance),
        }))
      : undefined,
    healthContributors: manifest.healthContributors?.length
      ? manifest.healthContributors.map((item) => ({
          identity: identityOut(item.identity),
          name: item.name,
          probe: item.probe,
          provenance: provenanceOut(item.provenance),
        }))
      : undefined,
    lifecycleHooks: manifest.lifecycleHooks?.length
      ? manifest.lifecycleHooks.map((item) => ({
          identity: identityOut(item.identity),
          name: item.name,
          phase: item.phase,
          provenance: provenanceOut(item.provenance),
        }))
      : undefined,
    packages: manifest.packages?.length
      ? manifest.packages.map((item) => ({
          identity: identityOut(item.identity),
          package: item.package,
          provenance: provenanceOut(item.provenance),
        }))
      : undefined,
    requiredCapabilities: manifest.requiredCapabilities?.length
      ? manifest.requiredCapabilities.map((item) => ({
          identity: identityOut(item.identity),
          name: item.name,
          requires: item.requires,
          provenance: provenanceOut(item.provenance),
        }))
      : undefined,
    domainAccess: manifest.domainAccess?.length
      ? manifest.domainAccess.map((item) => ({
          identity: identityOut(item.identity),
          import: item.import,
          mode: item.mode,
          status: item.status,
          transports: item.transports?.length
            ? item.transports.map((transport) => ({
                role: transport.role,
                kind: transport.kind,
                contract: transport.contract || undefined,
                availability: transport.availability,
              }))
            : undefined,
          enforced: item.enforced ? { ...item.enforced } : undefined,
          provenance: provenanceOut(item.provenance),
        }))
      : undefined,
  };
}

export type SourceFileMode = '100644' | '100755' | '120000' | '160000';
export interface SourceBindingFile {
  path: string;
  mode: SourceFileMode;
  digest: string;
}

const GIT_OBJECT = /^git:[0-9a-f]{40}([0-9a-f]{24})?$/;
const SOURCE_DOMAIN = 'putnami-source-binding-v1\n';

function excluded(path: string): boolean {
  const parts = path.split('/');
  return parts.some((part, index) => {
    if (part === '.git' || part === '.gen') return true;
    return part === 'schema' && (parts[index + 1] === 'capabilities.json' || parts[index + 1] === 'feature-evidence');
  });
}

export function sourceDigest(bytes: Uint8Array | string): string {
  return `sha256:${createHash('sha256').update(bytes).digest('hex')}`;
}

/** Canonical protocol input-record boundary; Git enumeration is caller-owned. */
export function canonicalSourceBindingInput(records: SourceBindingFile[]): string {
  const values: SourceBindingFile[] = [];
  const seen = new Set<string>();
  for (const record of records) {
    const pathDiagnostics: CapabilityDiagnostic[] = [];
    validateProtocolPath('path', record.path, pathDiagnostics);
    if (pathDiagnostics.length) throw new CapabilityManifestValidationError(pathDiagnostics);
    if (excluded(record.path)) continue;
    if (seen.has(record.path))
      fail(
        CAPABILITY_DIAGNOSTIC_CODES.invalidPath,
        'path',
        `duplicate source-binding path ${JSON.stringify(record.path)}`,
      );
    seen.add(record.path);
    if (record.mode === '160000') {
      if (!GIT_OBJECT.test(record.digest))
        fail(
          CAPABILITY_DIAGNOSTIC_CODES.invalidSourceBinding,
          'digest',
          'gitlink requires git:<full-lower-hex-object-id>',
        );
    } else if (!['100644', '100755', '120000'].includes(record.mode) || !SHA256.test(record.digest)) {
      fail(CAPABILITY_DIAGNOSTIC_CODES.invalidSourceBinding, 'digest', `${record.mode} requires sha256:<64-lower-hex>`);
    }
    values.push({ ...record });
  }
  values.sort((left, right) => cmp(left.path, right.path));
  return `${escapeLikeGo(JSON.stringify({ bindingVersion: 1, files: values }, null, 2))}\n`;
}

export function computeSourceBinding(records: SourceBindingFile[]): string {
  const input = canonicalSourceBindingInput(records);
  const digest = createHash('sha256').update(SOURCE_DOMAIN).update(input).digest('hex');
  return `source-v1:sha256:${digest}`;
}

// Re-export concrete types used by consumers without forcing them to import
// the hand-written wire declarations separately.
export type {
  CapabilityManifestDocument,
  ConfigDefinitionV2,
  ContributionIdentity,
  DiscovererV2,
  HealthContributorV2,
  InfraRequirementV2,
  LifecycleHookV2,
  ManifestV2,
  MigrationBundleV2,
  PackageVersionV2,
  ProvenanceV2,
  RequiredCapabilityV2,
  SchemaContributionV2,
};
