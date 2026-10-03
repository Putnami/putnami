// Package infra defines the shared infrastructure-requirements protocol
// used by Putnami workloads and the framework modules they import to
// declare what infrastructure they need to run — databases, event topics,
// object storage buckets, secrets, scheduled jobs, and workload-level
// runtime concerns (ingress, scaling).
//
// The protocol defines two manifest shapes:
//
//   - PerProjectManifest: generator-owned and committed, lives at
//     "<project>/infra/requirements.json". A library declares only what its
//     own code needs; workload-level runtime concerns are rejected.
//
//   - AggregatedManifest: build-emitted at "<workload>/.gen/requirements.json".
//     Produced by Merge() from the committed per-project manifests of every
//     project in a workload's dependency graph, then layered with
//     "<workload>/infra/runtime.json". Each entry carries provenance
//     (sources: which project + which contributor produced it). Deployers
//     consume this ephemeral release artifact, but deployment activation uses
//     committed infra/runtime.json and/or infra/requirements.json markers.
//
// The aggregator that walks the dependency graph and writes the aggregated
// manifest, plus framework integrations that auto-generate per-project
// manifests (e.g. the SQL framework declaring a Postgres requirement), are
// out of scope for this package — it defines the contract, not the
// pipeline that produces or consumes it.
package infra

import "sort"

// ProtocolVersion is the current infra-requirements protocol version: the
// version every in-tree producer stamps and both JSON schemas pin. Bumped
// whenever a backwards-incompatible change to manifest shapes, resource
// kinds, or merge semantics lands.
const ProtocolVersion = 2

// MigrationGuide is the one-line instruction every "your manifest is out of
// date" diagnostic carries. A reader that drops a contribution has to say how
// to get it back, because the drop is otherwise invisible: the aggregator
// reports findings as warnings and still emits a manifest, so a stale file
// costs a workload its declared infrastructure without failing the build.
const MigrationGuide = "set protocolVersion to 2, remove runtime.scaling.min, " +
	"and re-run `putnami build` (see protocols/infra/doc/adr/0003-deployers-own-runtime-cost-policy.md)"

// RemovedRuntimeFields maps every runtime field this protocol has retired onto
// the reason it went away. The strict readers consult it so an author who left
// one in place gets that reason instead of encoding/json's bare "unknown
// field", which says nothing about who owns the value now.
var RemovedRuntimeFields = map[string]string{
	"min":                "minimum residency is deployer-owned cost policy",
	"billing":            "billing posture is deployer-owned cost policy",
	"requestBased":       "CPU-allocation posture is deployer-owned cost policy",
	"cpuIdle":            "CPU-allocation posture is deployer-owned cost policy",
	"cpuAlwaysAllocated": "CPU-allocation posture is deployer-owned cost policy",
}

// Canonical filenames and directories used by the protocol.
const (
	// PerProjectManifestDir is the directory under each project that holds
	// the per-project manifest.
	PerProjectManifestDir = "infra"

	// PerProjectManifestFilename is the file name inside PerProjectManifestDir.
	PerProjectManifestFilename = "requirements.json"

	// AggregatedManifestDir is the directory under each workload root that
	// holds the aggregated manifest emitted by the build.
	AggregatedManifestDir = ".gen"

	// AggregatedManifestFilename is the file name inside AggregatedManifestDir.
	AggregatedManifestFilename = "requirements.json"

	// DeploymentFilename is the file name inside AggregatedManifestDir of a
	// workload's deployment declaration (see MarshalDeployment).
	DeploymentFilename = "deployment.json"
)

// Engine identifies a database engine. The enum is intentionally
// closed: adding a new engine requires a ProtocolVersion bump so
// deployers can decide how to react.
type Engine string

// Engine values.
const (
	EnginePostgres  Engine = "postgres"
	EngineMySQL     Engine = "mysql"
	EngineSQLite    Engine = "sqlite"
	EngineFirestore Engine = "firestore"
)

// ValidEngines enumerates the canonical engine values.
var ValidEngines = map[Engine]bool{
	EnginePostgres:  true,
	EngineMySQL:     true,
	EngineSQLite:    true,
	EngineFirestore: true,
}

// StorageAccess is the access level a workload needs to an object-storage
// bucket. It is the bucket analog of Engine: the closed, deployer-facing enum
// a provisioner maps onto an IAM grant when it grants the runtime identity
// access. The canonical set is owned by the storage protocol
// (go.putnami.dev/protocol/storage); infra carries the projected value so the
// needed role travels with the workload's declared requirements.
type StorageAccess string

// StorageAccess values.
const (
	StorageAccessRead      StorageAccess = "read"
	StorageAccessWrite     StorageAccess = "write"
	StorageAccessReadWrite StorageAccess = "readwrite"
)

// ValidStorageAccess enumerates the canonical storage access levels.
var ValidStorageAccess = map[StorageAccess]bool{
	StorageAccessRead:      true,
	StorageAccessWrite:     true,
	StorageAccessReadWrite: true,
}

// EngineNames returns the accepted engine values, sorted. Diagnostics build
// their "accepted set" text from this rather than repeating the list inline:
// a hand-written copy drifts silently, which is how the pre-v2 message came
// to omit firestore for as long as it did.
func EngineNames() []string {
	names := make([]string, 0, len(ValidEngines))
	for e := range ValidEngines {
		names = append(names, string(e))
	}
	sort.Strings(names)
	return names
}

// StorageAccessNames returns the accepted bucket access levels, sorted. Same
// anti-drift reason as EngineNames.
func StorageAccessNames() []string {
	names := make([]string, 0, len(ValidStorageAccess))
	for a := range ValidStorageAccess {
		names = append(names, string(a))
	}
	sort.Strings(names)
	return names
}

// ContributorID identifies the producer of an entry in the aggregated
// manifest. The literal "manual" marks a developer-authored entry; a
// framework generator uses "framework:<framework-name>" (built via
// FrameworkContributor).
type ContributorID string

// ContributorManual marks a legacy hand-written per-project manifest. Current
// Putnami generators own infra/requirements.json and workload owners put human
// runtime intent in infra/runtime.json.
const ContributorManual ContributorID = "manual"

// frameworkPrefix is the canonical prefix for framework-generator
// contributor identifiers.
const frameworkPrefix = "framework:"

// FrameworkContributor builds a ContributorID for a framework generator
// named name (e.g. "go.putnami.dev/database" → "framework:go.putnami.dev/database").
func FrameworkContributor(name string) ContributorID {
	return ContributorID(frameworkPrefix + name)
}

// PerProjectManifest is the generator-owned requirements manifest for a single
// project. It lives at "<project>/infra/requirements.json" and declares only
// what this project's own code needs.
//
// Runtime concerns (ingress, scaling) are intentionally absent: they are
// workload-root concerns and rejected by strict parsing of per-project
// manifests.
type PerProjectManifest struct {
	Schema          string          `json:"$schema,omitempty"`
	ProtocolVersion int             `json:"protocolVersion"`
	Databases       []Database      `json:"databases,omitempty"`
	Events          *Events         `json:"events,omitempty"`
	Storage         []StorageBucket `json:"storage,omitempty"`
	Secrets         []string        `json:"secrets,omitempty"`
	ScheduledJobs   []ScheduledJob  `json:"scheduledJobs,omitempty"`
}

// AggregatedManifest is the build-emitted artifact at
// "<workload>/.gen/requirements.json". It is produced by Merge() from
// the per-project manifests of every project in the workload's
// dependency graph, plus any workload-root Runtime block.
//
// Every resource entry carries a Sources list that names which projects
// and which contributors asked for it, so operators can trace why a
// requirement is present.
type AggregatedManifest struct {
	Schema          string                   `json:"$schema,omitempty"`
	ProtocolVersion int                      `json:"protocolVersion"`
	Workload        string                   `json:"workload"`
	Databases       []AggregatedDatabase     `json:"databases,omitempty"`
	Events          *AggregatedEvents        `json:"events,omitempty"`
	Storage         []AggregatedStorage      `json:"storage,omitempty"`
	Secrets         []AggregatedSecret       `json:"secrets,omitempty"`
	ScheduledJobs   []AggregatedScheduledJob `json:"scheduledJobs,omitempty"`
	Runtime         *Runtime                 `json:"runtime,omitempty"`
}

// Database declares a database requirement. Identity for merge purposes
// is (Name, Engine) — the same name across two engines is two distinct
// databases.
type Database struct {
	Name    string   `json:"name"`
	Engine  Engine   `json:"engine"`
	Schemas []string `json:"schemas,omitempty"`
}

// AggregatedDatabase is a Database in the aggregated manifest, augmented
// with the list of contributing sources.
type AggregatedDatabase struct {
	Name    string   `json:"name"`
	Engine  Engine   `json:"engine"`
	Schemas []string `json:"schemas,omitempty"`
	Sources []Source `json:"sources"`
}

// Events declares topic publish/subscribe requirements for a project.
//
// Publishes are plain topic names. Subscribes are Subscriptions: each carries a
// topic and an optional delivery model (pull, the default; stream; or push).
// Subscriptions marshal back to a bare topic string for the default delivery,
// so a project that does not use push delivery emits the same shape it always
// has — only push subscriptions widen the wire form (see Subscription).
type Events struct {
	Publishes  []string       `json:"publishes,omitempty"`
	Subscribes []Subscription `json:"subscribes,omitempty"`
}

// AggregatedEvents is Events in the aggregated manifest, augmented
// with sources per topic.
type AggregatedEvents struct {
	Publishes  []AggregatedTopic `json:"publishes,omitempty"`
	Subscribes []AggregatedTopic `json:"subscribes,omitempty"`
}

// AggregatedTopic is a single publish or subscribe entry with sources. Delivery
// is set only for subscribe entries that use a non-default delivery model; it is
// absent (the pull default) for publishes and for pull subscriptions, so the
// aggregated shape is unchanged for workloads that do not use push delivery.
type AggregatedTopic struct {
	Name     string   `json:"name"`
	Delivery Delivery `json:"delivery,omitempty"`
	Sources  []Source `json:"sources"`
}

// StorageBucket declares an object-storage bucket requirement. Identity for
// merge purposes is Name. Access and Public are additive capabilities the
// provisioner grants the runtime identity: when two projects share a bucket the
// merge unions their access (read ∪ write = readwrite) and ORs their public
// flag, so the bucket is granted exactly enough for every consumer. The rich
// storage contract (isolation scope, signed-URL signer, the resolved binding)
// lives in go.putnami.dev/protocol/storage; infra carries only the thin,
// secret-free projection a deployer provisions and grants from.
type StorageBucket struct {
	Name      string        `json:"name"`
	Access    StorageAccess `json:"access,omitempty"`
	Public    bool          `json:"public,omitempty"`
	Retention string        `json:"retention,omitempty"`
}

// AggregatedStorage is a StorageBucket in the aggregated manifest,
// augmented with sources.
type AggregatedStorage struct {
	Name      string        `json:"name"`
	Access    StorageAccess `json:"access,omitempty"`
	Public    bool          `json:"public,omitempty"`
	Retention string        `json:"retention,omitempty"`
	Sources   []Source      `json:"sources"`
}

// AggregatedSecret is a single secret name in the aggregated manifest
// with the list of projects that asked for it.
type AggregatedSecret struct {
	Name    string   `json:"name"`
	Sources []Source `json:"sources"`
}

// ScheduledJob declares a scheduled job requirement. The Schedule field
// is a 5-field Cloud Scheduler (unix-cron) expression; validation rejects
// empty strings and anything that is not a well-formed 5-field cron (no
// seconds field, no @macros).
type ScheduledJob struct {
	Name       string `json:"name"`
	Schedule   string `json:"schedule"`
	Entrypoint string `json:"entrypoint,omitempty"`
}

// AggregatedScheduledJob is a ScheduledJob in the aggregated manifest,
// augmented with sources.
type AggregatedScheduledJob struct {
	Name       string   `json:"name"`
	Schedule   string   `json:"schedule"`
	Entrypoint string   `json:"entrypoint,omitempty"`
	Sources    []Source `json:"sources"`
}

// Runtime is the workload-root runtime block. It lives only in the
// aggregated manifest (and on the WithRuntime input passed to the
// aggregator) — per-project manifests must not declare it.
type Runtime struct {
	Ingress   *Ingress          `json:"ingress,omitempty"`
	Scaling   *Scaling          `json:"scaling,omitempty"`
	Resources *Resources        `json:"resources,omitempty"`
	Security  *Security         `json:"security,omitempty"`
	Protocols *RuntimeProtocols `json:"protocols,omitempty"`
}

// Ingress declares workload-level ingress concerns. Domain/Public describe a
// single canonical host; Domains carries a deploy-target's list of custom
// hostnames. The framework passes all three through opaquely — deployers
// decide which they honor and how they map onto their platform.
type Ingress struct {
	Domain  *string  `json:"domain,omitempty"`
	Domains []string `json:"domains,omitempty"`
	Public  *bool    `json:"public,omitempty"`
}

// Scaling declares workload-level capacity ceilings. Minimum residency and
// billing posture are deployer-owned cost policy and intentionally absent.
type Scaling struct {
	Max         *int `json:"max,omitempty"`
	Concurrency *int `json:"concurrency,omitempty"`
}

// Resources declares workload-level compute resource limits. Both the flat
// {cpu, memory} form and the nested {limits: {cpu, memory}} form are accepted;
// values are opaque strings (e.g. "1000m", "1Gi"). The framework passes them
// through unparsed — the deploy target validates platform-specific syntax.
type Resources struct {
	CPU    *string         `json:"cpu,omitempty"`
	Memory *string         `json:"memory,omitempty"`
	Limits *ResourceLimits `json:"limits,omitempty"`
}

// ResourceLimits is the nested {limits: {cpu, memory}} form of Resources.
type ResourceLimits struct {
	CPU    *string `json:"cpu,omitempty"`
	Memory *string `json:"memory,omitempty"`
}

// Security declares workload-level platform security concerns. PlatformAuth
// toggles the deploy target's platform-level invoker authentication; its value
// is opaque to the framework and validated by the deployer.
//
// SignBlob, when true, declares that the workload mints credentials as its
// own runtime identity — e.g. object-storage V4 signed URLs produced by
// signing bytes with the runtime identity rather than a local private key.
// The framework passes it through opaquely; a deploy target maps it onto
// whatever self-impersonation grant its platform requires (on GCP, the
// runtime service account holding roles/iam.serviceAccountTokenCreator on
// itself). Nil/false leaves the workload with no signing grant.
//
// EventsGateway, when true, declares that the workload acts as an event
// pull/stream gateway and requires the deployer to grant its runtime identity
// event subscriber access. Nil/false leaves the workload with no additional
// event subscriber grant.
type Security struct {
	PlatformAuth  *string `json:"platformAuth,omitempty"`
	SignBlob      *bool   `json:"signBlob,omitempty"`
	EventsGateway *bool   `json:"eventsGateway,omitempty"`
}

// RuntimeProtocols declares workload HTTP protocol capabilities. HTTP2 maps to
// deploy-target HTTP/2-to-container settings. On platforms such as Cloud Run
// that means h2c; runtimes that cannot serve h2c set this to false so deployers
// keep the service on HTTP/1.1 even if their platform default is HTTP/2.
type RuntimeProtocols struct {
	HTTP2 *bool `json:"http2,omitempty"`
}

// Default runtime values applied to any workload that has no developer-
// authored <workload>/infra/runtime.json. They expose capacity ceilings only;
// deployers own minimum residency and billing posture. The high per-instance
// concurrency suits I/O-bound request handlers. A workload owner overrides
// these ceilings by writing their own runtime.json.
const (
	DefaultScalingMax         = 1
	DefaultScalingConcurrency = 500
	DefaultIngressPublic      = false
)

// DefaultRuntime returns a fresh Runtime populated with the canonical
// workload defaults. Callers should never share the returned pointer
// because Runtime fields are themselves pointers.
func DefaultRuntime() *Runtime {
	max := DefaultScalingMax
	concurrency := DefaultScalingConcurrency
	public := DefaultIngressPublic
	return &Runtime{
		Ingress: &Ingress{Public: &public},
		Scaling: &Scaling{Max: &max, Concurrency: &concurrency},
	}
}

// Source records which project + which contributor produced an
// aggregated requirement entry.
type Source struct {
	Project     string        `json:"project"`
	Contributor ContributorID `json:"contributor"`
}

// ProjectContribution is one input to Merge: the per-project manifest
// produced by Project under the named Contributor (manual or framework
// generator). The Manifest is assumed already strict-parsed by the
// caller — Merge does not re-validate it.
type ProjectContribution struct {
	Project     string
	Contributor ContributorID
	Manifest    PerProjectManifest
}
