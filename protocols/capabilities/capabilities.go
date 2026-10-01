// Package capabilities defines the shared capability-manifest protocol: the
// one deterministic artifact that aggregates everything a Putnami project
// contributes to a workload, each entry carrying provenance back to the
// project, package, and evidence that produced it.
//
// A capability manifest is emitted once per project at build time — by the Go
// framework describe/build emitter and by the @putnami/application producer —
// and aggregates these contribution kinds:
//
//   - config definitions (with sensitive-field flags),
//   - route / OpenAPI / proto schemas,
//   - source and config discoverers,
//   - migration bundles and their datasources,
//   - infra requirements,
//   - health / liveness / readiness contributors,
//   - lifecycle hooks (starters and stoppers),
//   - framework and package versions.
//
// Every entry carries a Provenance block (project, package, version,
// sourceKind, evidencePath) so a reviewer can trace any contribution back to
// the code that declared it. A RequiredCapability expresses a logical
// capability that depends on a set of other capability kinds — e.g. a "sql"
// capability requires datasource + migration + readiness.
//
// This package defines the wire contract only: hand-written Go types are the
// source of truth (no codegen), strict parsing rejects unknown fields, and a
// shared fixtures/ corpus is the cross-language contract that both languages
// read by relative path. The emitters that walk a project and the consumers
// that read the manifest are out of scope here — the package is the contract,
// not the pipeline.
//
// Byte-identical cross-language emission is a goal: the field order of every
// struct is deliberate, and TypeScript mirrors json.MarshalIndent's ordering,
// indentation, HTML/JavaScript-separator escaping, and trailing newline. Keep
// field names camelCase and the ordering intentional.
package capabilities

// ProtocolVersion is the frozen v1 capabilities-manifest protocol version: the
// version schemas/capabilities.json pins and v1 readers accept. Current
// producers stamp ProtocolVersionV2; v1 stays readable for existing documents
// and for the legacy scheduler compatibility path, and its shape, API, and
// diagnostics do not change.
const ProtocolVersion = 1

// Canonical filenames and paths used by the protocol.
const (
	// ManifestFilename is the manifest file name.
	ManifestFilename = "capabilities.json"

	// EmitDir is the build-output directory an emitter writes the manifest to.
	// It lives under .gen/schema/ (not the .gen/ root) so the codegen committer
	// promotes the manifest into the tracked tree: files under .gen/schema/ are
	// committed and shipped in a packaged workload, while files at the .gen/
	// root stay ephemeral and are gitignored. Emitters MUST write here.
	EmitDir = ".gen/schema"

	// CommittedPath is the workspace-relative path of the promoted manifest in
	// the tracked tree. The committer strips the leading .gen/ from EmitDir, so
	// .gen/schema/capabilities.json is promoted to schema/capabilities.json.
	CommittedPath = "schema/" + ManifestFilename
)

// SourceKind classifies how a contribution entry was discovered. The enum is
// intentionally closed: adding a kind requires a ProtocolVersion bump so
// consumers can decide how to react.
type SourceKind string

// SourceKind values.
const (
	// SourceKindFramework marks an entry a framework integration declared
	// (e.g. the SQL framework declaring a datasource requirement).
	SourceKindFramework SourceKind = "framework"
	// SourceKindManual marks a hand-authored entry.
	SourceKindManual SourceKind = "manual"
	// SourceKindGenerated marks an entry produced by a code generator or a
	// describe/generate hook.
	SourceKindGenerated SourceKind = "generated"
)

// ValidSourceKinds enumerates the canonical source kinds.
var ValidSourceKinds = map[SourceKind]bool{
	SourceKindFramework: true,
	SourceKindManual:    true,
	SourceKindGenerated: true,
}

// SchemaKind classifies a schema contribution. Closed enum.
type SchemaKind string

// SchemaKind values.
const (
	SchemaKindRoute   SchemaKind = "route"
	SchemaKindOpenAPI SchemaKind = "openapi"
	SchemaKindProto   SchemaKind = "proto"
)

// ValidSchemaKinds enumerates the canonical schema kinds.
var ValidSchemaKinds = map[SchemaKind]bool{
	SchemaKindRoute:   true,
	SchemaKindOpenAPI: true,
	SchemaKindProto:   true,
}

// DiscovererKind classifies a discoverer contribution. Closed enum.
type DiscovererKind string

// DiscovererKind values.
const (
	DiscovererKindSource DiscovererKind = "source"
	DiscovererKindConfig DiscovererKind = "config"
)

// ValidDiscovererKinds enumerates the canonical discoverer kinds.
var ValidDiscovererKinds = map[DiscovererKind]bool{
	DiscovererKindSource: true,
	DiscovererKindConfig: true,
}

// InfraKind classifies an infra requirement contribution. It mirrors the
// resource kinds owned by go.putnami.dev/protocol/infra; capabilities carries
// the thin projection so the requirement kind travels with the manifest.
// Closed enum.
type InfraKind string

// InfraKind values.
const (
	InfraKindDatabase     InfraKind = "database"
	InfraKindEvents       InfraKind = "events"
	InfraKindStorage      InfraKind = "storage"
	InfraKindSecret       InfraKind = "secret"
	InfraKindScheduledJob InfraKind = "scheduledJob"
)

// ValidInfraKinds enumerates the canonical infra requirement kinds.
var ValidInfraKinds = map[InfraKind]bool{
	InfraKindDatabase:     true,
	InfraKindEvents:       true,
	InfraKindStorage:      true,
	InfraKindSecret:       true,
	InfraKindScheduledJob: true,
}

// ProbeKind classifies a health contributor by the probe it feeds. It mirrors
// the probe concepts owned by go.putnami.dev/protocol/platform
// (/healthz, /livez, /readyz). Closed enum.
type ProbeKind string

// ProbeKind values.
const (
	ProbeKindHealth    ProbeKind = "health"
	ProbeKindLiveness  ProbeKind = "liveness"
	ProbeKindReadiness ProbeKind = "readiness"
)

// ValidProbeKinds enumerates the canonical probe kinds.
var ValidProbeKinds = map[ProbeKind]bool{
	ProbeKindHealth:    true,
	ProbeKindLiveness:  true,
	ProbeKindReadiness: true,
}

// LifecyclePhase classifies a lifecycle hook. Closed enum.
type LifecyclePhase string

// LifecyclePhase values.
const (
	LifecyclePhaseStarter LifecyclePhase = "starter"
	LifecyclePhaseStopper LifecyclePhase = "stopper"
)

// ValidLifecyclePhases enumerates the canonical lifecycle phases.
var ValidLifecyclePhases = map[LifecyclePhase]bool{
	LifecyclePhaseStarter: true,
	LifecyclePhaseStopper: true,
}

// CapabilityKind names one kind a logical capability can depend on. It is the
// closed vocabulary a RequiredCapability's Requires list draws from — e.g. a
// "sql" capability requires datasource + migration + readiness. Closed enum.
type CapabilityKind string

// CapabilityKind values.
const (
	CapabilityKindConfig     CapabilityKind = "config"
	CapabilityKindSchema     CapabilityKind = "schema"
	CapabilityKindDiscoverer CapabilityKind = "discoverer"
	CapabilityKindMigration  CapabilityKind = "migration"
	CapabilityKindDatasource CapabilityKind = "datasource"
	CapabilityKindInfra      CapabilityKind = "infra"
	CapabilityKindHealth     CapabilityKind = "health"
	CapabilityKindLiveness   CapabilityKind = "liveness"
	CapabilityKindReadiness  CapabilityKind = "readiness"
	CapabilityKindLifecycle  CapabilityKind = "lifecycle"
	CapabilityKindPackage    CapabilityKind = "package"
)

// ValidCapabilityKinds enumerates the canonical capability kinds a
// RequiredCapability may depend on.
var ValidCapabilityKinds = map[CapabilityKind]bool{
	CapabilityKindConfig:     true,
	CapabilityKindSchema:     true,
	CapabilityKindDiscoverer: true,
	CapabilityKindMigration:  true,
	CapabilityKindDatasource: true,
	CapabilityKindInfra:      true,
	CapabilityKindHealth:     true,
	CapabilityKindLiveness:   true,
	CapabilityKindReadiness:  true,
	CapabilityKindLifecycle:  true,
	CapabilityKindPackage:    true,
}

// Provenance records where a contribution entry came from. Project is
// required; the remaining fields are best-effort context an emitter fills when
// it can. SourceKind classifies the discovery mechanism (see SourceKind).
// EvidencePath is a workspace-relative path to the file that declared the
// contribution, so a reviewer can trace any entry back to source.
type Provenance struct {
	// Project is the Putnami project that contributed the entry.
	Project string `json:"project"`
	// Package is the framework or dependency package that declared the entry.
	Package string `json:"package,omitempty"`
	// Version is the resolved version of Package when it is known.
	Version string `json:"version,omitempty"`
	// SourceKind identifies whether the entry is framework, manual, or generated.
	SourceKind SourceKind `json:"sourceKind"`
	// EvidencePath points to the project-relative source or generated artifact.
	EvidencePath string `json:"evidencePath,omitempty"`
}

// Manifest is the aggregated capability manifest for a single project. An
// emitter writes it to "<project>/.gen/schema/capabilities.json", from which
// the codegen committer promotes it into the tracked tree at
// "<project>/schema/capabilities.json" (see EmitDir and CommittedPath).
//
// Field order is deliberate: it is the canonical serialization order that both
// the Go and TypeScript emitters must reproduce byte-for-byte.
type Manifest struct {
	// Schema is the optional URI of the JSON schema describing this manifest.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the capabilities wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Project is the Putnami project described by this manifest.
	Project string `json:"project"`
	// ConfigDefinitions lists configuration blocks contributed by the project.
	ConfigDefinitions []ConfigDefinition `json:"configDefinitions,omitempty"`
	// Schemas lists route, OpenAPI, and proto schema contributions.
	Schemas []SchemaContribution `json:"schemas,omitempty"`
	// Discoverers lists source and configuration discovery contributions.
	Discoverers []Discoverer `json:"discoverers,omitempty"`
	// Migrations lists migration bundles and their logical datasources.
	Migrations []MigrationBundle `json:"migrations,omitempty"`
	// InfraRequirements lists infrastructure resources required by the project.
	InfraRequirements []InfraRequirement `json:"infraRequirements,omitempty"`
	// HealthContributors lists components contributing to platform probes.
	HealthContributors []HealthContributor `json:"healthContributors,omitempty"`
	// LifecycleHooks lists registered startup and shutdown hooks.
	LifecycleHooks []LifecycleHook `json:"lifecycleHooks,omitempty"`
	// PackageVersions lists stable resolved framework or dependency versions.
	PackageVersions []PackageVersion `json:"packageVersions,omitempty"`
	// RequiredCapabilities lists logical capabilities and their required providers.
	RequiredCapabilities []RequiredCapability `json:"requiredCapabilities,omitempty"`
}

// ConfigDefinition is a config block a project contributes, with the fields it
// declares. Fields flagged Sensitive carry secret material and must never be
// logged or emitted in cleartext.
type ConfigDefinition struct {
	// Path is the canonical dotted configuration-block path.
	Path string `json:"path"`
	// Fields describes the configuration fields declared by the block.
	Fields []ConfigField `json:"fields,omitempty"`
	// Provenance identifies where the configuration block was declared.
	Provenance Provenance `json:"provenance"`
}

// ConfigField is one field of a ConfigDefinition. Sensitive marks a field that
// holds secret material.
type ConfigField struct {
	// Name is the field name within its configuration block.
	Name string `json:"name"`
	// Type is the field's language-neutral type description when known.
	Type string `json:"type,omitempty"`
	// Sensitive marks values that must not be logged or emitted in cleartext.
	Sensitive bool `json:"sensitive,omitempty"`
}

// SchemaContribution is a route, OpenAPI document, or proto schema a project
// exposes. Path is the route path or the schema file location.
type SchemaContribution struct {
	// Name is the stable schema or module-registry identity.
	Name string `json:"name"`
	// Kind identifies whether this is a route, OpenAPI, or proto schema.
	Kind SchemaKind `json:"kind"`
	// Path is the project-relative schema or generated activation-module path.
	Path string `json:"path,omitempty"`
	// Provenance identifies where the schema contribution was declared.
	Provenance Provenance `json:"provenance"`
}

// Discoverer is a source or config discoverer a project registers.
type Discoverer struct {
	// Name is the stable discoverer or module-registry identity.
	Name string `json:"name"`
	// Kind identifies whether the discoverer scans source or configuration.
	Kind DiscovererKind `json:"kind"`
	// Provenance identifies the discoverer and any generated activation module.
	Provenance Provenance `json:"provenance"`
}

// MigrationBundle is a migration bundle a project contributes, bound to a
// logical datasource. Digest is the content address of the bundle when known.
type MigrationBundle struct {
	// Name is the migration bundle namespace.
	Name string `json:"name"`
	// Datasource is the logical datasource targeted by the bundle.
	Datasource string `json:"datasource,omitempty"`
	// Digest is the bundle content address when the producer can compute it.
	Digest string `json:"digest,omitempty"`
	// Provenance identifies where the migration bundle was declared.
	Provenance Provenance `json:"provenance"`
}

// InfraRequirement is an infrastructure requirement a project declares,
// projected from go.putnami.dev/protocol/infra.
type InfraRequirement struct {
	// Name is the logical infrastructure resource name.
	Name string `json:"name"`
	// Kind identifies the required infrastructure resource class.
	Kind InfraKind `json:"kind"`
	// Provenance identifies where the requirement was declared.
	Provenance Provenance `json:"provenance"`
}

// HealthContributor is a component that feeds one of the platform probes.
type HealthContributor struct {
	// Name is the stable contributor identity within the probe.
	Name string `json:"name"`
	// Probe identifies whether the contributor feeds health, liveness, or readiness.
	Probe ProbeKind `json:"probe"`
	// Provenance identifies where the probe contributor was declared.
	Provenance Provenance `json:"provenance"`
}

// LifecycleHook is a starter or stopper a project registers.
type LifecycleHook struct {
	// Name is the stable lifecycle-hook identity.
	Name string `json:"name"`
	// Phase identifies whether the hook starts or stops a component.
	Phase LifecyclePhase `json:"phase"`
	// Provenance identifies where the lifecycle hook was declared.
	Provenance Provenance `json:"provenance"`
}

// PackageVersion records the resolved version of a framework or package the
// project depends on. Provenance names the project that pulled it in.
type PackageVersion struct {
	// Package is the canonical framework or dependency package name.
	Package string `json:"package"`
	// Version is the stable resolved package version.
	Version string `json:"version"`
	// Provenance identifies the project and evidence that resolved the package.
	Provenance Provenance `json:"provenance"`
}

// RequiredCapability expresses a logical capability that depends on a set of
// other capability kinds. Example: a "sql" capability Requires
// [datasource, migration, readiness]. Requires must be non-empty and every
// element must be a valid CapabilityKind.
type RequiredCapability struct {
	// Name is the logical capability being declared.
	Name string `json:"name"`
	// Requires lists the non-empty provider-kind set the capability depends on.
	Requires []CapabilityKind `json:"requires"`
	// Provenance identifies where the capability requirement was declared.
	Provenance Provenance `json:"provenance"`
}
