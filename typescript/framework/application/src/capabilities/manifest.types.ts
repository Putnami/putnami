/**
 * TypeScript mirror of the `go.putnami.dev/protocol/capabilities` wire contract.
 *
 * These types are a hand-declared port of the Go `Manifest` struct family (no
 * codegen). Byte-identical cross-language emission is the goal: the field order
 * of every type below is deliberate and MUST match the Go struct field order.
 * The TypeScript serializer also mirrors Go's indentation, escaping, and
 * trailing newline so both languages produce the same bytes. Field names are
 * camelCase; every `omitempty` Go field is an optional field here that MUST be
 * OMITTED (never emitted as `null`/`[]`/`false`) when empty — see
 * `serializeCapabilityManifest`.
 *
 * The Go sources of truth are protocols/capabilities/capabilities.go for v1 and
 * protocols/capabilities/v2.go for v2. Keep each mirror in lock-step with its
 * corresponding frozen wire version.
 */

/**
 * JSON Schema URL stamped into the manifest's `$schema`. Byte-identical to the
 * string the Go emitter stamps (go/framework/app/capabilities.go).
 */
export const CAPABILITIES_SCHEMA_URL = 'https://putnami.dev/schemas/putnami-capabilities.json';

/**
 * Build-output directory the emitter writes the manifest to, relative to the
 * project root. It lives under `.gen/schema/` (not the `.gen/` root) so the
 * codegen committer promotes the manifest into the tracked tree. Mirrors Go's
 * `capabilities.EmitDir`.
 */
export const EMIT_DIR = '.gen/schema';

/** Manifest file name. Mirrors Go's `capabilities.ManifestFilename`. */
export const MANIFEST_FILENAME = 'capabilities.json';

/**
 * Workspace-relative path of the promoted manifest in the tracked tree (the
 * committer strips the leading `.gen/` from {@link EMIT_DIR}). Mirrors Go's
 * `capabilities.CommittedPath`.
 */
export const COMMITTED_PATH = `schema/${MANIFEST_FILENAME}`;

/** How a contribution entry was discovered. Closed enum. */
export type SourceKind = 'framework' | 'manual' | 'generated';

/** Schema contribution classification. Closed enum. */
export type SchemaKind = 'route' | 'openapi' | 'proto';

/** Discoverer contribution classification. Closed enum. */
export type DiscovererKind = 'source' | 'config';

/** Infra requirement classification. Closed enum. */
export type InfraKind = 'database' | 'events' | 'storage' | 'secret' | 'scheduledJob';

/** Health contributor probe classification. Closed enum. */
export type ProbeKind = 'health' | 'liveness' | 'readiness';

/** Lifecycle hook phase. Closed enum. */
export type LifecyclePhase = 'starter' | 'stopper';

/** Capability kind a RequiredCapability may depend on. Closed enum. */
export type CapabilityKind =
  | 'config'
  | 'schema'
  | 'discoverer'
  | 'migration'
  | 'datasource'
  | 'infra'
  | 'health'
  | 'liveness'
  | 'readiness'
  | 'lifecycle'
  | 'package';

/** Where a contribution entry came from. Field order mirrors Go `Provenance`. */
export interface Provenance {
  project: string;
  package?: string;
  version?: string;
  sourceKind: SourceKind;
  evidencePath?: string;
}

/** One field of a ConfigDefinition. Field order mirrors Go `ConfigField`. */
export interface ConfigField {
  name: string;
  type?: string;
  sensitive?: boolean;
}

/** A config block a project contributes. Field order mirrors Go `ConfigDefinition`. */
export interface ConfigDefinition {
  path: string;
  fields?: ConfigField[];
  provenance: Provenance;
}

/** A route/OpenAPI/proto schema. Field order mirrors Go `SchemaContribution`. */
export interface SchemaContribution {
  name: string;
  kind: SchemaKind;
  path?: string;
  provenance: Provenance;
}

/** A source/config discoverer. Field order mirrors Go `Discoverer`. */
export interface Discoverer {
  name: string;
  kind: DiscovererKind;
  provenance: Provenance;
}

/** A migration bundle bound to a datasource. Field order mirrors Go `MigrationBundle`. */
export interface MigrationBundle {
  name: string;
  datasource?: string;
  digest?: string;
  provenance: Provenance;
}

/** An infra requirement. Field order mirrors Go `InfraRequirement`. */
export interface InfraRequirement {
  name: string;
  kind: InfraKind;
  provenance: Provenance;
}

/** A component feeding a platform probe. Field order mirrors Go `HealthContributor`. */
export interface HealthContributor {
  name: string;
  probe: ProbeKind;
  provenance: Provenance;
}

/** A starter/stopper hook. Field order mirrors Go `LifecycleHook`. */
export interface LifecycleHook {
  name: string;
  phase: LifecyclePhase;
  provenance: Provenance;
}

/** A resolved framework/package version. Field order mirrors Go `PackageVersion`. */
export interface PackageVersion {
  package: string;
  version: string;
  provenance: Provenance;
}

/** A logical capability depending on other kinds. Field order mirrors Go `RequiredCapability`. */
export interface RequiredCapability {
  name: string;
  requires: CapabilityKind[];
  provenance: Provenance;
}

/**
 * The aggregated capability manifest for a single project. Field order is the
 * canonical serialization order both the Go and TypeScript emitters reproduce
 * byte-for-byte; mirrors Go `Manifest`.
 */
export interface Manifest {
  $schema?: string;
  protocolVersion: 1;
  project: string;
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

/** Explicit v2 schema URL. V1's URL and producer default remain unchanged. */
export const CAPABILITIES_V2_SCHEMA_URL = 'https://putnami.dev/schemas/putnami-capabilities-v2.json';

export type ContributionKind =
  | 'config'
  | 'schema'
  | 'discoverer'
  | 'migration'
  | 'infra'
  | 'health'
  | 'lifecycle'
  | 'package'
  | 'requiredCapability'
  | 'domainAccess';

/** Complete semantic identity; the containing manifest project is not part of it. */
export interface ContributionIdentity {
  ownerProject: string;
  kind: ContributionKind;
  subkind?: string;
  key: string;
}

export type ContributionReference = ContributionIdentity;
export type LocationRoot = 'workspace' | 'project' | 'package';

export interface DeclarationLocation {
  root: LocationRoot;
  path: string;
  symbol?: string;
}

export interface ArtifactLocation {
  root: LocationRoot;
  path: string;
  digest?: string;
}

export interface ProvenanceV2 {
  project: string;
  package?: string;
  /** @deprecated Accepted on historical manifests but ignored and never emitted. */
  version?: string;
  sourceKind: SourceKind;
  /** @deprecated Accepted on historical manifests but ignored and never emitted. */
  readonly sourceBinding?: string;
  declaration: DeclarationLocation;
  artifacts?: ArtifactLocation[];
}

export interface ConfigDefinitionV2 {
  identity: ContributionIdentity;
  path: string;
  fields?: ConfigField[];
  provenance: ProvenanceV2;
}

export interface SchemaContributionV2 {
  identity: ContributionIdentity;
  name: string;
  kind: SchemaKind;
  path?: string;
  provenance: ProvenanceV2;
}

export interface DiscovererV2 {
  identity: ContributionIdentity;
  name: string;
  kind: DiscovererKind;
  provenance: ProvenanceV2;
}

export interface MigrationBundleV2 {
  identity: ContributionIdentity;
  name: string;
  kind: string;
  datasource?: string;
  digest?: string;
  provenance: ProvenanceV2;
}

export interface InfraRequirementV2 {
  identity: ContributionIdentity;
  name: string;
  kind: InfraKind;
  provenance: ProvenanceV2;
}

export interface HealthContributorV2 {
  identity: ContributionIdentity;
  name: string;
  probe: ProbeKind;
  provenance: ProvenanceV2;
}

export interface LifecycleHookV2 {
  identity: ContributionIdentity;
  name: string;
  phase: LifecyclePhase;
  provenance: ProvenanceV2;
}

/** Stable package identity; resolved versions remain in build/index snapshots. */
export interface PackageV2 {
  identity: ContributionIdentity;
  package: string;
  provenance: ProvenanceV2;
}

/** @deprecated Historical v2 package-version entry retained for strict reads. */
export interface PackageVersionV2 {
  identity: ContributionIdentity;
  package: string;
  version: string;
  provenance: ProvenanceV2;
}

export interface RequiredCapabilityV2 {
  identity: ContributionIdentity;
  name: string;
  requires: CapabilityKind[];
  provenance: ProvenanceV2;
}

/** One declared carrier of an enforced domain access contract. */
export interface DomainAccessTransportV2 {
  role: string;
  kind: string;
  contract?: string;
  availability: string;
}

/**
 * The contract parameters a component applies. Every member is optional because
 * the access modes carry different halves of the contract: a reference declares
 * none of them, a projection declares them all.
 */
export interface DomainAccessEnforcementV2 {
  maxStaleness?: string;
  onMissing?: string;
  onStale?: string;
  ordering?: string;
  lateEvents?: string;
  deletion?: string;
  writer?: string;
  rebuild?: string;
}

/**
 * One Domain Access & Replication Contract a framework component enforces at run
 * time.
 *
 * It is EVIDENCE, not authority: the contract itself is declared and reviewed in
 * a `putnami.architecture.json`, and this row only states that a running
 * component was configured with it. Every vocabulary member is carried verbatim
 * from the architecture contract and is opaque here — `protocols/architecture`
 * owns what a mode or a failure behavior means, and a second copy is exactly the
 * drift a checker joining the two documents exists to catch.
 */
export interface DomainAccessV2 {
  identity: ContributionIdentity;
  import: string;
  mode: string;
  status: string;
  transports?: DomainAccessTransportV2[];
  enforced?: DomainAccessEnforcementV2;
  provenance: ProvenanceV2;
}

export interface ManifestV2 {
  $schema?: string;
  protocolVersion: 2;
  project: string;
  configDefinitions?: ConfigDefinitionV2[];
  schemas?: SchemaContributionV2[];
  discoverers?: DiscovererV2[];
  migrations?: MigrationBundleV2[];
  infraRequirements?: InfraRequirementV2[];
  healthContributors?: HealthContributorV2[];
  lifecycleHooks?: LifecycleHookV2[];
  packages?: PackageV2[];
  /** @deprecated Accepted on historical manifests but ignored and never emitted. */
  packageVersions?: PackageVersionV2[];
  requiredCapabilities?: RequiredCapabilityV2[];
  domainAccess?: DomainAccessV2[];
}

export type CapabilityManifestDocument =
  | { protocolVersion: 1; manifest: Manifest }
  | { protocolVersion: 2; manifest: ManifestV2 };
