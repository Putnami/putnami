// Package features defines the durable, framework-neutral wire contracts for
// authored feature intent, feature evidence, and durable specifications. It
// deliberately excludes the derived snapshot and delta projections produced by
// local tooling.
package features

import "go.putnami.dev/protocol/capabilities"

const (
	// ManifestProtocolVersion is the current authored feature-manifest wire
	// version. Manifest readers also accept version 1, which cannot carry a
	// verification criterion; evidence and spec documents keep their own exact
	// versions so adding an executable criterion never reopens their wires.
	ManifestProtocolVersion = 2
	// MinimumManifestProtocolVersion is the oldest authored feature-manifest
	// wire version readers still accept.
	MinimumManifestProtocolVersion = 1
	// EvidenceProtocolVersion is the exact integer version accepted by feature
	// evidence documents.
	EvidenceProtocolVersion = 1
	// SpecProtocolVersion is the exact integer version accepted by durable
	// specifications.
	SpecProtocolVersion = 1
	// VerificationReportProtocolVersion is the exact integer version accepted by
	// run-scoped feature verification reports.
	VerificationReportProtocolVersion = 1

	// ManifestFilename is the only authored feature-intent filename discovered
	// at a workspace or exact project root.
	ManifestFilename = "putnami.features.json"

	// EvidenceDirectory is the committed directory containing evidence fragments.
	EvidenceDirectory = "schema/feature-evidence"
	// EmitEvidenceDirectory is the build-time source promoted to EvidenceDirectory.
	EmitEvidenceDirectory = ".gen/schema/feature-evidence"

	// SpecDirectory is the only directory whose direct JSON children are read as
	// durable specifications, at a workspace or exact project root. A spec
	// filename never participates in spec identity.
	SpecDirectory = "specs"

	// DecisionDirectory is the project-local durable decision-record directory.
	// Specs link to records already written under this existing convention;
	// there is no second decision wire protocol.
	DecisionDirectory = "doc/adr"
	// DecisionExtension is the only durable decision-record file extension.
	DecisionExtension = ".md"

	// ManifestSchemaURL is the canonical authored-intent JSON Schema URI.
	ManifestSchemaURL = "https://putnami.dev/schemas/putnami-features.json"
	// EvidenceSchemaURL is the canonical feature-evidence JSON Schema URI.
	EvidenceSchemaURL = "https://putnami.dev/schemas/putnami-feature-evidence.json"
	// SpecSchemaURL is the canonical durable-specification JSON Schema URI.
	SpecSchemaURL = "https://putnami.dev/schemas/putnami-spec.json"
	// VerificationReportSchemaURL is the canonical run-report JSON Schema URI.
	VerificationReportSchemaURL = "https://putnami.dev/schemas/putnami-feature-verification.json"

	// VerificationReportArtifactID is the reserved declared-artifact identity a
	// task uses to publish one project's verification report. The report is a
	// run-scoped result envelope retained with the session, never a durable
	// authored inventory committed to the source tree.
	VerificationReportArtifactID = "putnami-feature-verification"
	// VerificationReportFilename is the canonical report artifact filename
	// inside a test task's command-output directory.
	VerificationReportFilename = VerificationReportArtifactID + ".json"
)

// FeatureType is the closed authored-intent type vocabulary.
type FeatureType string

const (
	// FeatureTypeFeature declares one independently assessable product feature.
	FeatureTypeFeature FeatureType = "feature"
	// FeatureTypeJourney declares a cross-feature product journey.
	FeatureTypeJourney FeatureType = "journey"
)

// RelationKind is the closed, unordered feature relation vocabulary.
type RelationKind string

const (
	// RelationKindParent links a feature to its semantic parent.
	RelationKindParent RelationKind = "parent"
	// RelationKindDependsOn links a feature to required product intent.
	RelationKindDependsOn RelationKind = "dependsOn"
	// RelationKindIncludes links a journey to one of its included features.
	RelationKindIncludes RelationKind = "includes"
)

// MaturityStage is one element in the closed maturity ladder.
type MaturityStage string

const (
	// MaturityModeled is earned by a valid authored declaration.
	MaturityModeled MaturityStage = "modeled"
	// MaturityCoded requires implementation evidence.
	MaturityCoded MaturityStage = "coded"
	// MaturityWired requires integration evidence.
	MaturityWired MaturityStage = "wired"
	// MaturityDefaultOn requires evidence that the feature is enabled by default.
	MaturityDefaultOn MaturityStage = "default-on"
	// MaturityLiveVerified requires active-environment verification.
	MaturityLiveVerified MaturityStage = "live-verified"
	// MaturityDesignPartnerProven requires human authority at that exact stage.
	MaturityDesignPartnerProven MaturityStage = "design-partner-proven"
	// MaturityGA requires human authority at that exact stage.
	MaturityGA MaturityStage = "ga"
)

var maturityStages = [...]MaturityStage{
	MaturityModeled,
	MaturityCoded,
	MaturityWired,
	MaturityDefaultOn,
	MaturityLiveVerified,
	MaturityDesignPartnerProven,
	MaturityGA,
}

// StageRank returns the stable maturity rank and whether stage is valid.
func StageRank(stage MaturityStage) (int, bool) {
	for rank, candidate := range maturityStages {
		if candidate == stage {
			return rank, true
		}
	}
	return 0, false
}

// OrderedMaturityStages returns a fresh copy of the canonical low-to-high
// maturity order.
func OrderedMaturityStages() []MaturityStage {
	return append([]MaturityStage(nil), maturityStages[:]...)
}

// EvidenceKind is the closed typed-subject vocabulary used by requirements
// and evidence records.
type EvidenceKind string

const (
	// EvidenceKindCapability references one exact capability contribution.
	EvidenceKindCapability EvidenceKind = "capability"
	// EvidenceKindArtifact references content-addressed contained bytes.
	EvidenceKindArtifact EvidenceKind = "artifact"
	// EvidenceKindAttestation references a bounded accepted claim.
	EvidenceKindAttestation EvidenceKind = "attestation"
)

// EvidenceOutcome says whether a record supports or contradicts its target.
type EvidenceOutcome string

const (
	// EvidenceOutcomeSupports asserts support for a requirement.
	EvidenceOutcomeSupports EvidenceOutcome = "supports"
	// EvidenceOutcomeContradicts asserts a contradiction of a requirement.
	EvidenceOutcomeContradicts EvidenceOutcome = "contradicts"
)

// IssuerKind identifies the bounded authority responsible for an evidence
// assertion.
type IssuerKind string

const (
	// IssuerKindFramework identifies framework-authored evidence.
	IssuerKindFramework IssuerKind = "framework"
	// IssuerKindBuild identifies build-authored evidence.
	IssuerKindBuild IssuerKind = "build"
	// IssuerKindTest identifies test-authored evidence.
	IssuerKindTest IssuerKind = "test"
	// IssuerKindDelivery identifies delivery-system evidence.
	IssuerKindDelivery IssuerKind = "delivery"
	// IssuerKindRuntime identifies runtime-observation evidence.
	IssuerKindRuntime IssuerKind = "runtime"
	// IssuerKindHuman identifies evidence accepted by a named human authority.
	IssuerKindHuman IssuerKind = "human"
)

// AttestationClaim is a bounded semantic claim code, never an arbitrary claim
// payload. Product-promise constants require a human issuer.
type AttestationClaim string

const (
	// AttestationClaimAdoption is the human-authority adoption promise.
	AttestationClaimAdoption AttestationClaim = "adoption"
	// AttestationClaimSupport is the human-authority support promise.
	AttestationClaimSupport AttestationClaim = "support"
	// AttestationClaimAvailability is the human-authority availability promise.
	AttestationClaimAvailability AttestationClaim = "availability"
	// AttestationClaimCustomerProof is the human-authority customer-proof promise.
	AttestationClaimCustomerProof AttestationClaim = "customer-proof"

	// AttestationClaimAcceptance is the automated claim an adapter uses when a
	// declared acceptance check reports its verdict.
	AttestationClaimAcceptance AttestationClaim = "acceptance"
	// AttestationClaimObjectiveMet is the automated claim an adapter uses when a
	// declared threshold objective is recomputed from an observed measurement.
	AttestationClaimObjectiveMet AttestationClaim = "objective-met"
)

// LocationRoot uses the same root selector vocabulary as capability
// provenance so source binding interpretation cannot drift between protocols.
type LocationRoot = capabilities.LocationRoot

const (
	// LocationRootWorkspace resolves against the selected workspace root.
	LocationRootWorkspace = capabilities.LocationRootWorkspace
	// LocationRootProject resolves against an exact semantic owner project root.
	LocationRootProject = capabilities.LocationRootProject
	// LocationRootPackage resolves against an exact package and version root.
	LocationRootPackage = capabilities.LocationRootPackage
)

// Manifest is one strict authored feature-intent document.
type Manifest struct {
	// Schema is the optional URI of the JSON schema describing the manifest.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the authored-intent wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Namespace is the repository-local prefix shared by every feature ID.
	Namespace string `json:"namespace"`
	// Features lists the product features and journeys declared by the repository.
	Features []Feature `json:"features"`
}

// Feature declares repository-local product intent. Current maturity is never
// authored here; it is derived later from evidence.
type Feature struct {
	// ID is the canonical slash-separated feature identity.
	ID string `json:"id"`
	// Type distinguishes independently assessable features from journeys.
	Type FeatureType `json:"type"`
	// Name is the concise human-readable feature name.
	Name string `json:"name"`
	// Outcome states the user or business result the feature intends to deliver.
	Outcome string `json:"outcome"`
	// Owner identifies the team or authority accountable for the feature.
	Owner string `json:"owner"`
	// Target is the intended maturity stage for the feature.
	Target MaturityStage `json:"target"`
	// Relations links this feature to other feature identities.
	Relations []Relation `json:"relations,omitempty"`
	// Requirements declares the evidence needed to earn maturity stages.
	Requirements []Requirement `json:"requirements,omitempty"`
}

// Relation is one unordered link to another feature in the same workspace
// aggregation.
type Relation struct {
	// Kind selects the semantic relationship to Target.
	Kind RelationKind `json:"kind"`
	// Target is the canonical ID of the related feature.
	Target string `json:"target"`
}

// Requirement declares what evidence is necessary at one maturity stage.
type Requirement struct {
	// ID is the feature-local stable requirement identity.
	ID string `json:"id"`
	// Stage is the maturity stage whose proof this requirement contributes to.
	Stage MaturityStage `json:"stage"`
	// EvidenceKinds is the non-empty set of evidence subject kinds accepted.
	EvidenceKinds []EvidenceKind `json:"evidenceKinds"`
	// Verification is the optional machine criterion that makes a same-ID
	// textual spec requirement executable. It requires manifest version 2; a
	// requirement without it stays an ordinary maturity requirement.
	Verification *VerificationCriterion `json:"verification,omitempty"`
}

// VerificationKind selects the closed machine-verification family.
type VerificationKind string

const (
	// VerificationKindAcceptance expects a named set of checks to report a verdict.
	VerificationKindAcceptance VerificationKind = "acceptance"
	// VerificationKindThreshold expects one measured objective compared to a target.
	VerificationKindThreshold VerificationKind = "threshold"
)

// VerificationAggregation is the closed measurement-reduction vocabulary.
type VerificationAggregation string

const (
	// AggregationValue is one directly observed value.
	AggregationValue VerificationAggregation = "value"
	// AggregationCount is the number of observations in the window.
	AggregationCount VerificationAggregation = "count"
	// AggregationSum is the additive total over the window.
	AggregationSum VerificationAggregation = "sum"
	// AggregationAvg is the arithmetic mean over the window.
	AggregationAvg VerificationAggregation = "avg"
	// AggregationMin is the smallest observation in the window.
	AggregationMin VerificationAggregation = "min"
	// AggregationMax is the largest observation in the window.
	AggregationMax VerificationAggregation = "max"
	// AggregationP50 is the median observation in the window.
	AggregationP50 VerificationAggregation = "p50"
	// AggregationP95 is the 95th-percentile observation in the window.
	AggregationP95 VerificationAggregation = "p95"
	// AggregationP99 is the 99th-percentile observation in the window.
	AggregationP99 VerificationAggregation = "p99"
	// AggregationRatio is the success share over the window, in [0,1].
	AggregationRatio VerificationAggregation = "ratio"
)

// VerificationOperator is the closed comparison vocabulary. It deliberately
// excludes strict inequalities so an authored target is always attainable.
type VerificationOperator string

const (
	// OperatorEq requires the observed value to equal the target exactly.
	OperatorEq VerificationOperator = "eq"
	// OperatorLte requires the observed value to be at most the target.
	OperatorLte VerificationOperator = "lte"
	// OperatorGte requires the observed value to be at least the target.
	OperatorGte VerificationOperator = "gte"
)

// VerificationWindowKind selects the observation window a threshold objective
// is measured over.
type VerificationWindowKind string

const (
	// WindowKindInvocation is one run or benchmark and carries no duration.
	WindowKindInvocation VerificationWindowKind = "invocation"
	// WindowKindRolling is a bounded production duration ending at the observation.
	WindowKindRolling VerificationWindowKind = "rolling"
)

// VerificationCriterion is the closed machine criterion that turns a textual
// spec requirement of the same ID into an executable one. It is authored beside
// the maturity requirement it belongs to, so the spec wire stays prose.
type VerificationCriterion struct {
	// Kind selects the acceptance or threshold verification family.
	Kind VerificationKind `json:"kind"`
	// Checks is the expected, non-empty, unique set of stable check identities.
	Checks []string `json:"checks"`
	// Metric is the bounded semantic metric name of a threshold objective.
	Metric string `json:"metric,omitempty"`
	// Aggregation reduces the observed metric over the window.
	Aggregation VerificationAggregation `json:"aggregation,omitempty"`
	// Operator compares the observed aggregate to Target.
	Operator VerificationOperator `json:"operator,omitempty"`
	// Target is the authored finite objective. It is a pointer so zero and
	// negative targets stay expressible and an absent target stays detectable.
	Target *float64 `json:"target,omitempty"`
	// Unit is the bounded semantic unit the observation must report.
	Unit string `json:"unit,omitempty"`
	// Window declares the observation window of a threshold objective.
	Window *VerificationWindow `json:"window,omitempty"`
	// Environment is the active environment a rolling objective is measured in.
	Environment string `json:"environment,omitempty"`
	// MaxAgeSeconds bounds how old a rolling observation may be when evaluated.
	MaxAgeSeconds int `json:"maxAgeSeconds,omitempty"`
}

// VerificationWindow is the authored observation window of a threshold
// objective.
type VerificationWindow struct {
	// Kind selects the invocation or rolling window family.
	Kind VerificationWindowKind `json:"kind"`
	// Seconds is the positive rolling duration; an invocation window omits it.
	Seconds int `json:"seconds,omitempty"`
}

// ObservationStatus is the closed verdict vocabulary of an acceptance check.
type ObservationStatus string

const (
	// ObservationPassed reports that a declared acceptance check held.
	ObservationPassed ObservationStatus = "passed"
	// ObservationFailed reports that a declared acceptance check did not hold.
	ObservationFailed ObservationStatus = "failed"
	// ObservationSkipped reports that a declared check did not execute.
	ObservationSkipped ObservationStatus = "skipped"
)

// VerificationReport is one run-scoped result envelope carrying what a test,
// build, delivery, or runtime job observed. It is transport, not authority: it
// declares no issuer, project, source binding, threshold, or objective verdict,
// so a producer can never choose both its target and its result. Core derives
// those from the scheduled task, the owning project, the current source
// binding, and the authored criterion.
type VerificationReport struct {
	// Schema is the optional URI of the JSON schema describing the report.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the verification-report wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Observations lists what the run actually observed.
	Observations []VerificationObservation `json:"observations"`
}

// VerificationObservation is one observed check. It carries either an
// acceptance status or a measurement with its observed window, never both.
type VerificationObservation struct {
	// Feature is the exact feature identity the observed check belongs to.
	Feature string `json:"feature"`
	// Requirement is the feature-local requirement identity being observed.
	Requirement string `json:"requirement"`
	// Check is the declared check identity this observation reports.
	Check string `json:"check"`
	// Status is the acceptance verdict of a declared check.
	Status ObservationStatus `json:"status,omitempty"`
	// Measurement is the observed value of a threshold objective.
	Measurement *ObservationMeasurement `json:"measurement,omitempty"`
	// Window is the period the measurement was observed over.
	Window *ObservedWindow `json:"window,omitempty"`
	// Environment is the active environment a rolling measurement was taken in.
	Environment string `json:"environment,omitempty"`
	// Provenance points at the declaration that produced the observation,
	// relative to the reporting task's own project root.
	Provenance ObservationProvenance `json:"provenance"`
}

// ObservationMeasurement is one observed metric aggregate. Core compares it to
// the authored target; the producer never states a verdict.
type ObservationMeasurement struct {
	// Name is the bounded semantic metric name that was observed.
	Name string `json:"name"`
	// Aggregation is the reduction that produced Value.
	Aggregation VerificationAggregation `json:"aggregation"`
	// Value is the finite observed aggregate.
	Value float64 `json:"value"`
	// Unit is the bounded semantic unit of Value.
	Unit string `json:"unit"`
}

// ObservedWindow is the closed period a measurement covers.
type ObservedWindow struct {
	// Start is the RFC 3339 instant the observation window opens at.
	Start string `json:"start"`
	// End is the RFC 3339 instant the observation window closes at.
	End string `json:"end"`
}

// ObservationProvenance names the checked, project-relative declaration that
// produced an observation. It carries no root selector: core resolves the
// reporting task's own project root so a report cannot claim another root.
type ObservationProvenance struct {
	// Path is the canonical project-relative path of the declaration.
	Path string `json:"path"`
	// Symbol names the declaration within Path.
	Symbol string `json:"symbol,omitempty"`
}

// EvidenceDocument is one transport fragment. Filenames do not participate
// in evidence identity.
type EvidenceDocument struct {
	// Schema is the optional URI of the JSON schema describing the document.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the feature-evidence wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Evidence lists the assertions transported by this fragment.
	Evidence []EvidenceRecord `json:"evidence"`
}

// EvidenceRecord associates one typed assertion with an exact feature
// requirement and stage.
type EvidenceRecord struct {
	// ID is the globally stable identity of this evidence assertion.
	ID string `json:"id"`
	// Feature is the exact feature identity this assertion assesses.
	Feature string `json:"feature"`
	// Requirement is the feature-local requirement identity being satisfied.
	Requirement string `json:"requirement"`
	// Stage is the maturity stage claimed by the requirement.
	Stage MaturityStage `json:"stage"`
	// Outcome says whether the assertion supports or contradicts its target.
	Outcome EvidenceOutcome `json:"outcome"`
	// Issuer identifies the authority accepting the assertion.
	Issuer Issuer `json:"issuer"`
	// Source selects the exact root state against which Subject is interpreted.
	Source SourceSelector `json:"source"`
	// Subject is the typed capability, artifact, or attestation being asserted.
	Subject EvidenceSubject `json:"subject"`
	// Provenance identifies the declaration or registration that produced the record.
	Provenance EvidenceProvenance `json:"provenance"`
	// Persistent marks evidence whose validity is not tied to one observation time.
	Persistent bool `json:"persistent,omitempty"`
	// ObservedAt is the RFC 3339 timestamp of a non-persistent observation.
	ObservedAt string `json:"observedAt,omitempty"`
	// ObservedRepositoryRevision is the full Git object ID observed by live evidence.
	ObservedRepositoryRevision string `json:"observedRepositoryRevision,omitempty"`
}

// Issuer is the named authority accepting an evidence assertion.
type Issuer struct {
	// Kind selects the bounded issuer authority class.
	Kind IssuerKind `json:"kind"`
	// ID is the stable identity of the concrete issuing authority.
	ID string `json:"id"`
}

// SourceSelector identifies the exact root state against which an evidence
// assertion is interpreted.
type SourceSelector struct {
	// Root selects the source root against which paths are resolved.
	Root LocationRoot `json:"root"`
	// OwnerProject identifies the semantic project root when Root is project.
	OwnerProject string `json:"ownerProject,omitempty"`
	// Package identifies the dependency package root when Root is package.
	Package string `json:"package,omitempty"`
	// Version is the resolved Package version when Root is package.
	Version string `json:"version,omitempty"`
	// Binding is the source-v1 digest of the selected root state.
	Binding string `json:"binding"`
	// Environment identifies the active environment for live observations.
	Environment string `json:"environment,omitempty"`
}

// EvidenceSubject is a tagged union. Exactly one payload matching Kind is
// required.
type EvidenceSubject struct {
	// Kind selects which one of the subject payloads must be present.
	Kind EvidenceKind `json:"kind"`
	// Contribution references one exact capability contribution.
	Contribution *capabilities.ContributionReference `json:"contribution,omitempty"`
	// Artifact identifies content-addressed bytes within Source.
	Artifact *ArtifactSubject `json:"artifact,omitempty"`
	// Attestation carries a bounded semantic claim.
	Attestation *AttestationSubject `json:"attestation,omitempty"`
}

// ArtifactSubject identifies contained bytes under the selected source root.
type ArtifactSubject struct {
	// Path is the canonical Source-relative path of the contained bytes.
	Path string `json:"path"`
	// Digest is the artifact's lower-case SHA-256 content address.
	Digest string `json:"digest"`
}

// AttestationSubject carries only a bounded claim code.
type AttestationSubject struct {
	// Claim is the bounded semantic assertion accepted by the issuer.
	Claim AttestationClaim `json:"claim"`
}

// EvidenceProvenance points to the declaration or generator registration that
// produced the evidence record.
type EvidenceProvenance struct {
	// Root selects the source root against which Path is resolved.
	Root LocationRoot `json:"root"`
	// Path is the canonical relative path to the evidence declaration.
	Path string `json:"path"`
	// Symbol names the declaration or generator registration within Path.
	Symbol string `json:"symbol,omitempty"`
}

// Spec is one strict durable specification for exactly one already-authored
// feature.
//
// A spec is a reader, never an authority. It cannot mint a feature, a maturity
// stage, an evidence record, or a design-graph node, and it never restates
// derived facts — current maturity, owners, source bindings, routes, schemas,
// and migrations stay owned by the declarations and producers that already emit
// them. The public field set is deliberately closed to the feature reference,
// intended outcomes, non-goals, stable textual requirements, and links to
// durable decision records.
type Spec struct {
	// Schema is the optional URI of the JSON schema describing the spec.
	Schema string `json:"$schema,omitempty"`
	// ProtocolVersion selects the durable-specification wire-contract version.
	ProtocolVersion int `json:"protocolVersion"`
	// Feature is the exact identity of the one authored feature this spec details.
	Feature string `json:"feature"`
	// Outcomes states, in authored order, the results the feature intends to deliver.
	Outcomes []string `json:"outcomes"`
	// NonGoals states, in authored order, what the feature deliberately excludes.
	NonGoals []string `json:"nonGoals,omitempty"`
	// Requirements carries the stable textual requirements agreed for the feature.
	Requirements []SpecRequirement `json:"requirements"`
	// Decisions links to durable decision records by workspace-relative path.
	Decisions []string `json:"decisions,omitempty"`
}

// SpecRequirement is one stable textual requirement. Its ID is spec-local and
// exists so reviews, decisions, and later tooling can reference the exact
// sentence that changed; the text itself carries no evidence authority.
type SpecRequirement struct {
	// ID is the spec-local stable requirement identity.
	ID string `json:"id"`
	// Text is the bounded requirement statement.
	Text string `json:"text"`
}
