// Package architecture owns the framework-neutral ARC/DARC v1 wire contracts.
//
// An architecture manifest declares one domain's authority, published facts,
// cross-domain imports, and the exact project-dependency bindings that are
// currently allowed to implement those imports. Repository tooling supplies
// observed edges; this package validates, aggregates, compares, and ratchets
// them without reading a filesystem or invoking a network service.
package architecture

import (
	"bytes"
	"encoding/json"

	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	// ProtocolVersion is the only wire version accepted by v1 readers.
	ProtocolVersion = 1

	// ManifestFilename is the recursively discovered, domain-owned declaration.
	ManifestFilename = "putnami.architecture.json"
	// ManifestSchemaURL is the published JSON Schema a declaration authors against.
	ManifestSchemaURL = "https://putnami.dev/schemas/putnami-architecture.json"
	// BaselineFilename is the workspace-root inventory of pre-existing debt.
	BaselineFilename = "architecture.baseline.json"
	// WaiverFilename is the workspace-root inventory of temporary exceptions.
	WaiverFilename = "architecture.waivers.json"
)

// LifecycleStatus distinguishes current contracts from targets and debt.
type LifecycleStatus string

// Lifecycle status values are the closed v1 contract-state vocabulary.
const (
	StatusPlanned    LifecycleStatus = "planned"
	StatusActive     LifecycleStatus = "active"
	StatusLegacy     LifecycleStatus = "legacy"
	StatusDeprecated LifecycleStatus = "deprecated"
)

// AccessMode describes the semantic reason one domain accesses another.
type AccessMode string

// Access mode values state why a consumer crosses a domain boundary.
const (
	ModeReference  AccessMode = "reference"
	ModeQuery      AccessMode = "query"
	ModeSnapshot   AccessMode = "snapshot"
	ModeProjection AccessMode = "projection"
	ModeCommand    AccessMode = "command"
)

// TransportKind describes how a contract is carried, not which transport is
// globally preferred. The semantic access mode remains authoritative.
type TransportKind string

// Transport kind values describe concrete contract carriers.
const (
	TransportNone      TransportKind = "none"
	TransportInProcess TransportKind = "in-process"
	TransportAPI       TransportKind = "api"
	TransportEvent     TransportKind = "event"
	TransportFile      TransportKind = "file"
)

// Classification is the bounded confidentiality vocabulary for an exported fact.
type Classification string

// Classification values are ordered from public to most restricted.
const (
	ClassificationPublic       Classification = "public"
	ClassificationInternal     Classification = "internal"
	ClassificationConfidential Classification = "confidential"
	ClassificationRestricted   Classification = "restricted"
)

// PersonalData classifies whether a fact carries personal information.
type PersonalData string

// Personal-data values distinguish non-personal, personal, and sensitive facts.
const (
	PersonalDataNone      PersonalData = "none"
	PersonalDataPersonal  PersonalData = "personal"
	PersonalDataSensitive PersonalData = "sensitive"
)

// CompatibilityStrategy describes how an export evolves across versions.
type CompatibilityStrategy string

// Compatibility strategy values describe supported export evolution policies.
const (
	CompatibilityAdditive              CompatibilityStrategy = "additive"
	CompatibilityVersioned             CompatibilityStrategy = "versioned"
	CompatibilityBreakingWithMigration CompatibilityStrategy = "breaking-with-migration"
)

// FailureMode describes behavior when a remote or copied fact is unavailable.
type FailureMode string

// Failure-mode values define consumer behavior for unavailable copied facts.
const (
	FailureFailOpen    FailureMode = "fail-open"
	FailureFailClosed  FailureMode = "fail-closed"
	FailureUseStale    FailureMode = "use-stale"
	FailureUnavailable FailureMode = "unavailable"
)

// OrderingStrategy describes how concurrent or late updates are ordered.
type OrderingStrategy string

// Ordering strategy values define how update monotonicity is established.
const (
	OrderingNone           OrderingStrategy = "none"
	OrderingSourceVersion  OrderingStrategy = "source-version"
	OrderingSourceSequence OrderingStrategy = "source-sequence"
	OrderingEventTime      OrderingStrategy = "event-time"
)

// LateEventStrategy describes an update older than the local source version.
type LateEventStrategy string

// Late-event strategy values define handling of older updates.
const (
	LateEventIgnoreOlder LateEventStrategy = "ignore-older"
	LateEventReject      LateEventStrategy = "reject"
	LateEventApply       LateEventStrategy = "apply"
)

// DeletionStrategy describes how a copied fact leaves the local model.
type DeletionStrategy string

// Deletion strategy values define how source removal reaches a local model.
const (
	DeletionTombstone     DeletionStrategy = "tombstone"
	DeletionHardDelete    DeletionStrategy = "hard-delete"
	DeletionRetain        DeletionStrategy = "retain"
	DeletionNotApplicable DeletionStrategy = "not-applicable"
)

// LocalModelKind separates a projection from an immutable snapshot or ID ref.
type LocalModelKind string

// Local-model kind values distinguish copied projections, snapshots, and references.
const (
	LocalModelProjection LocalModelKind = "projection"
	LocalModelSnapshot   LocalModelKind = "snapshot"
	LocalModelReference  LocalModelKind = "reference"
)

// RebuildStrategy states how a copied model is reconstructed.
type RebuildStrategy string

// Rebuild strategy values define the recovery source for copied data.
const (
	RebuildBootstrap          RebuildStrategy = "bootstrap"
	RebuildReplay             RebuildStrategy = "replay"
	RebuildBootstrapAndReplay RebuildStrategy = "bootstrap-and-replay"
	RebuildNotApplicable      RebuildStrategy = "not-applicable"
)

// OwnershipKind identifies a semantic boundary fact, not a source-code inventory.
type OwnershipKind string

// Ownership kind values classify semantic domain authorities.
const (
	OwnershipFact   OwnershipKind = "fact"
	OwnershipModel  OwnershipKind = "model"
	OwnershipSchema OwnershipKind = "schema"
	OwnershipAPI    OwnershipKind = "api"
	OwnershipEvent  OwnershipKind = "event"
)

// EvidenceKind identifies how an implementation of a declared import was
// observed. It is deliberately not a binding kind: a binding AUTHORIZES a
// structural edge, and evidence only reports that something exists.
type EvidenceKind string

// EvidenceFrameworkPrimitive is a framework component that took a declared
// contract as its configuration and enforces it at run time. It is the only
// evidence kind v1 recognizes.
const EvidenceFrameworkPrimitive EvidenceKind = "framework-primitive"

// Detector coverage values state exactly what a snapshot's detectors performed.
const (
	// CoverageNotDetected means the category was not observed at all.
	CoverageNotDetected = "not-detected"
	// CoverageEnforcedForMappedProjects means exact project dependencies are
	// compared for every project a domain maps.
	CoverageEnforcedForMappedProjects = "enforced-for-mapped-projects"
	// CoverageFrameworkEvidence means implementations were read from committed
	// framework evidence — what a build recorded a component was configured
	// with. It is deliberately NOT runtime observation: nothing here watched a
	// request, a query, or a delivered event.
	CoverageFrameworkEvidence = "framework-evidence"
)

// BindingKind identifies an observed detector category a declaration may authorize.
type BindingKind string

// BindingProjectDependency authorizes one exact Putnami project graph edge.
const BindingProjectDependency BindingKind = "project-dependency"

// Manifest is one domain-owned architecture declaration.
type Manifest struct {
	// Schema identifies the JSON Schema used to author the declaration.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the exact architecture wire contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Domain is the stable identifier of the declaring domain.
	Domain string `json:"domain"`
	// Owner identifies the team or role accountable for the domain.
	Owner string `json:"owner"`
	// Projects lists the Putnami projects contained by the domain.
	Projects []string `json:"projects"`
	// Owns lists semantic concepts for which the domain is authoritative.
	Owns []OwnedConcept `json:"owns,omitempty"`
	// Exports lists producer-owned contracts offered to other domains.
	Exports []Export `json:"exports"`
	// Imports lists consumer-owned cross-domain access contracts.
	Imports []Import `json:"imports"`
}

// OwnedConcept records a stable semantic authority boundary.
type OwnedConcept struct {
	// ID is the stable domain-local concept identifier.
	ID string `json:"id"`
	// Kind classifies the kind of semantic authority being declared.
	Kind OwnershipKind `json:"kind"`
	// Description explains the concept in domain language.
	Description string `json:"description"`
}

// Export is a producer-owned surface of authoritative or provenance-preserving facts.
type Export struct {
	// ID is the stable producer-local export identifier.
	ID string `json:"id"`
	// Version is the positive version of this export contract.
	Version int `json:"version"`
	// Status states whether the export is planned, usable, or being retired.
	Status LifecycleStatus `json:"status"`
	// Description explains the exported capability in domain language.
	Description string `json:"description"`
	// Facts lists the named values exposed by this export.
	Facts []Fact `json:"facts"`
	// Modes lists the access semantics the producer supports.
	Modes []AccessMode `json:"modes"`
	// Compatibility declares how this export evolves across versions.
	Compatibility Compatibility `json:"compatibility"`
}

// Fact is one field exposed by an export. Authority is always explicit. The
// data classifications may both be omitted only when the export supports
// reference access and no other mode.
type Fact struct {
	// Name is the stable export-local fact name.
	Name string `json:"name"`
	// Authority identifies the domain that remains authoritative for the fact.
	Authority string `json:"authority"`
	// Classification records the confidentiality class of a data-bearing fact.
	Classification Classification `json:"classification,omitempty"`
	// PersonalData records whether a data-bearing fact contains personal information.
	PersonalData          PersonalData `json:"personalData,omitempty"`
	classificationPresent bool
	personalDataPresent   bool
}

// UnmarshalJSON retains whether optional metadata was absent or explicitly
// supplied as an empty string. The wire accepts omission for reference-only
// facts; it never treats an invalid enum token as omission.
func (fact *Fact) UnmarshalJSON(data []byte) error {
	type wireFact Fact
	var decoded wireFact
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*fact = Fact(decoded)
	_, fact.classificationPresent = fields["classification"]
	_, fact.personalDataPresent = fields["personalData"]
	return nil
}

// Compatibility defines how consumers move between export versions.
type Compatibility struct {
	// Strategy identifies the producer's supported evolution policy.
	Strategy CompatibilityStrategy `json:"strategy"`
	// MinimumConsumerVersion is the oldest compatible positive export version.
	MinimumConsumerVersion int `json:"minimumConsumerVersion"`
}

// Import is the consumer-owned half of a DARC.
type Import struct {
	// ID is the stable consumer-local import identifier.
	ID string `json:"id"`
	// Version selects the producer export version consumed by this import.
	Version int `json:"version"`
	// From identifies the producer domain and export.
	From ExportReference `json:"from"`
	// As names the imported capability in the consumer's domain language.
	As string `json:"as"`
	// Mode states why the consumer crosses the domain boundary.
	Mode AccessMode `json:"mode"`
	// Status states whether the import is planned, active, or being retired.
	Status LifecycleStatus `json:"status"`
	// Facts lists the exact producer-owned facts consumed by this import.
	Facts []string `json:"facts"`
	// Transport describes the carrier for reference, query, snapshot, or command access.
	Transport *Transport `json:"transport,omitempty"`
	// Bootstrap describes how a projection obtains its initial state.
	Bootstrap *Transport `json:"bootstrap,omitempty"`
	// Updates describes how a projection receives subsequent changes.
	Updates *Transport `json:"updates,omitempty"`
	// Consistency defines freshness, ordering, and failure behavior.
	Consistency *Consistency `json:"consistency,omitempty"`
	// Deletion defines how producer deletion propagates to a local copy.
	Deletion *Deletion `json:"deletion,omitempty"`
	// LocalModel describes the consumer-owned representation of copied facts.
	LocalModel *LocalModel `json:"localModel,omitempty"`
	// Bindings authorize exact observable implementation dependencies.
	Bindings []Binding `json:"bindings,omitempty"`
	// Justification explains the domain need for this dependency.
	Justification string `json:"justification"`
}

// ExportReference identifies the producer-owned export consumed by an import.
type ExportReference struct {
	// Domain is the stable identifier of the producer domain.
	Domain string `json:"domain"`
	// Export is the producer-local export identifier.
	Export string `json:"export"`
}

// Transport names an actual or planned carrier and its independent availability.
type Transport struct {
	// Kind identifies the carrier category.
	Kind TransportKind `json:"kind"`
	// Contract names the concrete API, event, file, or in-process contract.
	Contract string `json:"contract,omitempty"`
	// Availability states whether that concrete carrier is planned or usable.
	Availability LifecycleStatus `json:"availability"`
}

// Consistency records freshness, ordering, idempotence, and safe failure behavior.
type Consistency struct {
	// MaxStaleness is the declared upper freshness bound for copied facts.
	MaxStaleness string `json:"maxStaleness"`
	// OnMissing defines consumer behavior when no source fact is available.
	OnMissing FailureMode `json:"onMissing"`
	// OnStale defines consumer behavior after the freshness bound is exceeded.
	OnStale FailureMode `json:"onStale"`
	// Ordering defines how concurrent or late updates are sequenced.
	Ordering OrderingStrategy `json:"ordering"`
	// SourceVersion names the value used to identify producer state versions.
	SourceVersion string `json:"sourceVersion"`
	// IdempotencyKey names the value used to deduplicate updates.
	IdempotencyKey string `json:"idempotencyKey"`
	// LateEvents defines how an older update is handled.
	LateEvents LateEventStrategy `json:"lateEvents"`
}

// Deletion defines how source deletion is represented locally.
type Deletion struct {
	// Strategy identifies how source deletion affects the local representation.
	Strategy DeletionStrategy `json:"strategy"`
	// TombstoneField names the local deletion marker when tombstones are used.
	TombstoneField string `json:"tombstoneField,omitempty"`
}

// LocalModel makes copied facts distinct from local configuration and state.
type LocalModel struct {
	// Name is the stable consumer-local model name.
	Name string `json:"name"`
	// Kind distinguishes a projection, snapshot, or reference.
	Kind LocalModelKind `json:"kind"`
	// SourceIdentity names the producer identity retained by the local model.
	SourceIdentity string `json:"sourceIdentity"`
	// ProjectedFields lists copied producer-owned facts.
	ProjectedFields []string `json:"projectedFields"`
	// LocalFields lists consumer-owned configuration or state kept alongside copies.
	LocalFields []string `json:"localFields,omitempty"`
	// ProvenanceField names the field that records the source contract.
	ProvenanceField string `json:"provenanceField"`
	// ObservedAtField names the field that records when source state was observed.
	ObservedAtField string `json:"observedAtField"`
	// FreshnessField names the field used to evaluate copied-data freshness.
	FreshnessField string `json:"freshnessField"`
	// Writer identifies the sole component allowed to update copied facts.
	Writer string `json:"writer"`
	// Rebuildable states whether source contracts can reconstruct the local model.
	Rebuildable bool `json:"rebuildable"`
	// Rebuild identifies the reconstruction strategy.
	Rebuild RebuildStrategy `json:"rebuild"`
}

// Binding authorizes one exact structural dependency as an implementation of an import.
type Binding struct {
	// Kind identifies the detector category this binding authorizes.
	Kind BindingKind `json:"kind"`
	// ConsumerProject is the exact consuming Putnami project ID.
	ConsumerProject string `json:"consumerProject"`
	// ProducerProject is the exact producing Putnami project ID.
	ProducerProject string `json:"producerProject"`
}

// ManifestSource attaches repository provenance to a parsed declaration.
type ManifestSource struct {
	// Path is the repository-relative declaration path.
	Path string
	// Manifest is the parsed declaration found at Path.
	Manifest *Manifest
}

// DebtRecord is shared by initial debt and temporary waivers. Tracking is
// optional because a repository must never invent an issue number.
type DebtRecord struct {
	// Finding is the exact stable finding ID being classified.
	Finding string `json:"finding"`
	// Owner identifies who is accountable for removing the exception.
	Owner string `json:"owner"`
	// Reason explains why the exception currently exists.
	Reason string `json:"reason"`
	// Scope describes the bounded architecture surface affected.
	Scope string `json:"scope"`
	// Tracking optionally links an existing issue or other work record.
	Tracking string `json:"tracking,omitempty"`
	// RemoveWhen defines an objective condition for deleting the record.
	RemoveWhen string `json:"removeWhen,omitempty"`
	// Expires is the waiver expiration date in YYYY-MM-DD form.
	Expires string `json:"expires,omitempty"`
}

// Baseline is the frozen workspace-root inventory of adoption-time violations.
type Baseline struct {
	// Schema identifies the JSON Schema used to author the baseline.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the exact architecture wire contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Findings is the frozen set of adoption-time finding records.
	Findings []DebtRecord `json:"findings"`
}

// WaiverFile contains later, explicitly justified temporary exceptions.
type WaiverFile struct {
	// Schema identifies the JSON Schema used to author the waiver file.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the exact architecture wire contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Waivers lists the temporary exceptions granted after adoption.
	Waivers []DebtRecord `json:"waivers"`
}

// DetectionCoverage makes non-detection explicit in every machine snapshot.
type DetectionCoverage struct {
	// ProjectDependencies reports coverage of Putnami project graph edges.
	ProjectDependencies string `json:"projectDependencies"`
	// Database reports whether database coupling is detected.
	Database string `json:"database"`
	// HTTP reports whether HTTP coupling is detected.
	HTTP string `json:"http"`
	// Events reports whether event coupling is detected.
	Events string `json:"events"`
	// DomainAccess reports whether any implementation of a declared import was
	// observed. It is framework-evidence at most: a build recorded what a
	// component was configured with, and no request, query, or delivered event
	// was ever watched.
	DomainAccess string `json:"domainAccess"`
}

// EvidenceTransport is one carrier a framework implementation was configured
// with, as the implementing component reported it.
type EvidenceTransport struct {
	// Role is the carrier's part in the contract: transport, bootstrap, updates.
	Role string `json:"role"`
	// Kind identifies the carrier category.
	Kind TransportKind `json:"kind"`
	// Availability states whether that carrier is usable yet.
	Availability LifecycleStatus `json:"availability"`
}

// EvidenceRecord is one framework-observed implementation of a declared import.
//
// It is derived repository fact, exactly like ObservedEdge: tooling supplies it,
// this package compares it, and NEITHER creates a permission. A record cannot
// authorize a cross-domain dependency — only a reviewed declaration can — which
// is the same rule that keeps an observed edge from declaring itself (ADR 0001).
type EvidenceRecord struct {
	// Kind identifies how the implementation was observed.
	Kind EvidenceKind `json:"kind"`
	// ConsumerDomain is the domain whose manifest declares the import.
	ConsumerDomain string `json:"consumerDomain"`
	// ConsumerProject is the exact Putnami project that carries the
	// implementation. It is empty on the record a declared-without-evidence
	// finding carries, which describes what was EXPECTED and not found.
	ConsumerProject string `json:"consumerProject,omitempty"`
	// Import is the consumer-local import identifier being implemented.
	Import string `json:"import"`
	// Mode is the access mode the implementation enforces.
	Mode AccessMode `json:"mode"`
	// Transports lists the carriers the implementation was configured with.
	Transports []EvidenceTransport `json:"transports,omitempty"`
}

// ObservedEdge is one exact repository fact supplied by a detector.
type ObservedEdge struct {
	// Kind identifies the detector category that observed the edge.
	Kind BindingKind `json:"kind"`
	// ProducerDomain is the domain containing the depended-on project.
	ProducerDomain string `json:"producerDomain"`
	// ConsumerDomain is the domain containing the depending project.
	ConsumerDomain string `json:"consumerDomain"`
	// ProducerProject is the exact depended-on Putnami project ID.
	ProducerProject string `json:"producerProject"`
	// ConsumerProject is the exact depending Putnami project ID.
	ConsumerProject string `json:"consumerProject"`
}

// DeclaredEdge is the deterministic global projection of one import.
type DeclaredEdge struct {
	// ID is the stable semantic identity of the consumer import.
	ID string `json:"id"`
	// Export is the producer-local export identifier.
	Export string `json:"export"`
	// ProducerDomain is the domain that owns the export.
	ProducerDomain string `json:"producerDomain"`
	// ConsumerDomain is the domain that declares the import.
	ConsumerDomain string `json:"consumerDomain"`
	// Mode states why the consumer crosses the domain boundary.
	Mode AccessMode `json:"mode"`
	// Status states whether the declared edge is planned, active, or retiring.
	Status LifecycleStatus `json:"status"`
	// Facts lists the exact producer-owned facts consumed across the edge.
	Facts []string `json:"facts"`
	// LocalModel describes copied facts when the access mode stores them locally.
	LocalModel *LocalModel `json:"localModel,omitempty"`
	// Bindings lists exact observable dependencies authorized by the declaration.
	Bindings []Binding `json:"bindings,omitempty"`
}

// DomainView is the deterministic agent-facing projection of one domain.
type DomainView struct {
	// ID is the stable domain identifier.
	ID string `json:"id"`
	// Owner identifies the team or role accountable for the domain.
	Owner string `json:"owner"`
	// Source is the repository-relative declaration path.
	Source string `json:"source"`
	// Projects lists the Putnami projects contained by the domain.
	Projects []string `json:"projects"`
	// Owns lists semantic concepts for which the domain is authoritative.
	Owns []OwnedConcept `json:"owns"`
	// Exports lists producer-owned contracts offered by the domain.
	Exports []Export `json:"exports"`
	// Imports lists consumer-owned contracts declared by the domain.
	Imports []Import `json:"imports"`
}

// Graph is the aggregated declared architecture.
type Graph struct {
	// Domains is the stable, sorted projection of all declarations.
	Domains []DomainView `json:"domains"`
	// Edges is the stable, sorted projection of all declared imports.
	Edges []DeclaredEdge `json:"edges"`
}

// FindingDisposition records how a violation relates to the ratchet.
type FindingDisposition string

// Finding disposition values record how the ratchet classified a violation.
const (
	DispositionNew            FindingDisposition = "new"
	DispositionKnownDebt      FindingDisposition = "known-debt"
	DispositionWaived         FindingDisposition = "waived"
	DispositionStaleBaseline  FindingDisposition = "stale-baseline"
	DispositionStaleWaiver    FindingDisposition = "stale-waiver"
	DispositionBaselineGrowth FindingDisposition = "baseline-growth"
	DispositionExpiredWaiver  FindingDisposition = "expired-waiver"
)

// Finding is one stable, ratcheted architecture violation.
type Finding struct {
	// ID is the stable semantic identity used by baselines and waivers.
	ID string `json:"id"`
	// Code is the closed machine-readable architecture violation code.
	Code string `json:"code"`
	// Severity determines whether the current snapshot passes validation.
	Severity diag.Severity `json:"severity"`
	// Disposition records how the ratchet classified the violation.
	Disposition FindingDisposition `json:"disposition"`
	// Message explains the violation for a human reader.
	Message string `json:"message"`
	// Edge contains the observed repository fact when one caused the finding.
	Edge *ObservedEdge `json:"edge,omitempty"`
	// Evidence contains the framework record when one caused the finding. On a
	// declared-without-evidence finding it describes the record that was
	// expected and not found, so the reader sees what is missing rather than
	// only that something is.
	Evidence *EvidenceRecord `json:"evidence,omitempty"`
}

// RatchetSummary is a compact deterministic count projection.
type RatchetSummary struct {
	// New is the count of unclassified current violations.
	New int `json:"new"`
	// KnownDebt is the count of current violations accepted at adoption.
	KnownDebt int `json:"knownDebt"`
	// Waived is the count of current violations with valid temporary waivers.
	Waived int `json:"waived"`
	// StaleBaseline is the count of resolved records still present in the baseline.
	StaleBaseline int `json:"staleBaseline"`
	// StaleWaiver is the count of resolved records still present in the waiver file.
	StaleWaiver int `json:"staleWaiver"`
	// BaselineGrowth is the count of records added after the immutable baseline.
	BaselineGrowth int `json:"baselineGrowth"`
	// ExpiredWaiver is the count of current violations whose waiver has expired.
	ExpiredWaiver int `json:"expiredWaiver"`
}

// Snapshot is the deterministic machine view consumed by CI, agents, and docs.
type Snapshot struct {
	// Schema identifies the JSON Schema for the generated snapshot.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the exact architecture wire contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Coverage states which repository coupling categories were detected.
	Coverage DetectionCoverage `json:"coverage"`
	// Graph contains the aggregated declared architecture.
	Graph Graph `json:"graph"`
	// Observed contains the repository dependencies supplied by detectors.
	Observed []ObservedEdge `json:"observed"`
	// Evidence contains the framework implementations supplied by detectors.
	Evidence []EvidenceRecord `json:"evidence"`
	// Findings contains deterministic, ratcheted architecture violations.
	Findings []Finding `json:"findings"`
	// Ratchet contains disposition counts for the current evaluation.
	Ratchet RatchetSummary `json:"ratchet"`
}
