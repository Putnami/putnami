package features

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/protocol/capabilities"
	diag "go.putnami.dev/protocol/diagnostic"
)

const (
	decisionLinkMaxLength = 512
	// checkIDMaxLength bounds one declared or observed check identity.
	checkIDMaxLength = 128
	// semanticCodeMaxLength bounds metric, unit, and environment codes.
	semanticCodeMaxLength = 128
	// rollingWindowMaxSeconds bounds a rolling duration and its freshness bound
	// at ten years, so an authored objective stays a reviewable period rather
	// than an unbounded integer.
	rollingWindowMaxSeconds = 315360000

	// VerificationReportMaxObservations bounds one run-scoped report so a
	// producer cannot make core read an unbounded result envelope.
	VerificationReportMaxObservations = 10000
)

var (
	featureIDPattern     = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*(?:/[a-z0-9]+(?:-[a-z0-9]+)*)+$`)
	segmentIDPattern     = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	claimPattern         = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$`)
	sourceBindingPattern = regexp.MustCompile(`^source-v1:sha256:[0-9a-f]{64}$`)
	sha256Pattern        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	revisionPattern      = regexp.MustCompile(`^git:(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	schemePattern        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	// decisionLinkPattern is the exact wire form of a durable decision record:
	// an optional owning-project prefix followed by the established project-local
	// doc/adr/<name>.md convention. It is kept byte-identical to the
	// decisionLink definition in schemas/putnami-spec.json.
	decisionLinkPattern = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?/)*doc/adr/[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?\.md$`)
	// specSourcePattern is the exact wire form of a spec discovery path: direct
	// JSON children of specs/ at a workspace or exact project root.
	specSourcePattern = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?/)*specs/[^/]+\.json$`)
)

var validFeatureTypes = map[FeatureType]bool{
	FeatureTypeFeature: true,
	FeatureTypeJourney: true,
}

var validRelationKinds = map[RelationKind]bool{
	RelationKindParent: true, RelationKindDependsOn: true, RelationKindIncludes: true,
}

var validEvidenceKinds = map[EvidenceKind]bool{
	EvidenceKindCapability: true, EvidenceKindArtifact: true, EvidenceKindAttestation: true,
}

var supportedManifestVersions = map[int]bool{
	MinimumManifestProtocolVersion: true, ManifestProtocolVersion: true,
}

var validVerificationKinds = map[VerificationKind]bool{
	VerificationKindAcceptance: true, VerificationKindThreshold: true,
}

var validAggregations = map[VerificationAggregation]bool{
	AggregationValue: true, AggregationCount: true, AggregationSum: true,
	AggregationAvg: true, AggregationMin: true, AggregationMax: true,
	AggregationP50: true, AggregationP95: true, AggregationP99: true,
	AggregationRatio: true,
}

var validOperators = map[VerificationOperator]bool{
	OperatorEq: true, OperatorLte: true, OperatorGte: true,
}

var validWindowKinds = map[VerificationWindowKind]bool{
	WindowKindInvocation: true, WindowKindRolling: true,
}

var validObservationStatuses = map[ObservationStatus]bool{
	ObservationPassed: true, ObservationFailed: true, ObservationSkipped: true,
}

var validOutcomes = map[EvidenceOutcome]bool{
	EvidenceOutcomeSupports: true, EvidenceOutcomeContradicts: true,
}

var validIssuerKinds = map[IssuerKind]bool{
	IssuerKindFramework: true, IssuerKindBuild: true, IssuerKindTest: true,
	IssuerKindDelivery: true, IssuerKindRuntime: true, IssuerKindHuman: true,
}

var productPromiseClaims = map[AttestationClaim]bool{
	AttestationClaimAdoption: true, AttestationClaimSupport: true,
	AttestationClaimAvailability: true, AttestationClaimCustomerProof: true,
}

// ManifestSource associates a parsed manifest with its canonical
// workspace-relative discovery path for deterministic aggregate diagnostics.
type ManifestSource struct {
	// Path is the canonical workspace-relative manifest discovery path.
	Path string
	// Manifest is the strictly parsed authored-intent document found at Path.
	Manifest *Manifest
}

// EvidenceSource associates a parsed evidence fragment with its canonical
// workspace-relative discovery path.
type EvidenceSource struct {
	// Path is the canonical workspace-relative evidence discovery path.
	Path string
	// Document is the strictly parsed evidence fragment found at Path.
	Document *EvidenceDocument
}

// SpecSource associates a parsed spec with its canonical workspace-relative
// discovery path. The path locates the document; it never names it.
type SpecSource struct {
	// Path is the canonical workspace-relative spec discovery path.
	Path string
	// Spec is the strictly parsed durable specification found at Path.
	Spec *Spec
}

// ValidateManifest validates semantics that are local to one authored
// document. Workspace-wide identity, relation, and evidence resolution are
// intentionally deferred to ValidateRepository.
func ValidateManifest(input *Manifest) []diag.Diagnostic {
	if input == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "feature manifest is nil")}
	}
	var diagnostics []diag.Diagnostic
	if input.Features == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "features", "features is required and must be an array"))
	}
	manifest := CanonicalManifest(input)
	if !supportedManifestVersions[manifest.ProtocolVersion] {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocolVersion %d is not supported (want %d or %d)", manifest.ProtocolVersion, MinimumManifestProtocolVersion, ManifestProtocolVersion))
	}
	if !segmentIDPattern.MatchString(manifest.Namespace) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, "namespace", "namespace %q must be one lower-case ASCII ID segment", manifest.Namespace))
	}
	seenFeatures := make(map[string]string)
	for i, feature := range manifest.Features {
		field := fmt.Sprintf("features[%d]", i)
		diagnostics = append(diagnostics, validateFeature(field, manifest.Namespace, manifest.ProtocolVersion, feature)...)
		if first, exists := seenFeatures[feature.ID]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateFeature, field+".id", "feature %q is duplicated; first declaration is %s", feature.ID, first))
		} else {
			seenFeatures[feature.ID] = field
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func validateFeature(field, namespace string, version int, feature Feature) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if !featureIDPattern.MatchString(feature.ID) || strings.Split(feature.ID, "/")[0] != namespace {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".id", "feature ID %q must be lower-case slash-separated and begin with namespace %q", feature.ID, namespace))
	}
	if !validFeatureTypes[feature.Type] {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidType, field+".type", "feature type %q is not supported", feature.Type))
	}
	for name, value := range map[string]string{"name": feature.Name, "outcome": feature.Outcome, "owner": feature.Owner} {
		if !boundedText(value, 512) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+"."+name, "%s must be non-empty bounded text without control characters", name))
		}
	}
	targetRank, validTarget := StageRank(feature.Target)
	if !validTarget {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidStage, field+".target", "target %q is not in the maturity ladder", feature.Target))
	}

	seenRelations := make(map[string]bool)
	for i, relation := range feature.Relations {
		relationField := fmt.Sprintf("%s.relations[%d]", field, i)
		if !validRelationKinds[relation.Kind] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRelation, relationField+".kind", "relation kind %q is not supported", relation.Kind))
		}
		if !featureIDPattern.MatchString(relation.Target) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRelation, relationField+".target", "relation target %q is not a canonical feature ID", relation.Target))
		}
		if relation.Kind == RelationKindIncludes && feature.Type != FeatureTypeJourney {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRelation, relationField, "only journeys may declare includes relations"))
		}
		key := string(relation.Kind) + "\x00" + relation.Target
		if seenRelations[key] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRelation, relationField, "relation (%q, %q) is duplicated", relation.Kind, relation.Target))
		}
		seenRelations[key] = true
	}

	seenRequirements := make(map[string]bool)
	coveredStages := make(map[MaturityStage]bool)
	for i, requirement := range feature.Requirements {
		requirementField := fmt.Sprintf("%s.requirements[%d]", field, i)
		if !segmentIDPattern.MatchString(requirement.ID) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, requirementField+".id", "requirement ID %q must be one lower-case ASCII ID segment", requirement.ID))
		}
		if seenRequirements[requirement.ID] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, requirementField+".id", "requirement ID %q is duplicated in feature %q", requirement.ID, feature.ID))
		}
		seenRequirements[requirement.ID] = true
		rank, validStage := StageRank(requirement.Stage)
		if !validStage || rank == 0 {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidStage, requirementField+".stage", "requirement stage %q must be a post-modeled maturity stage", requirement.Stage))
		} else {
			coveredStages[requirement.Stage] = true
		}
		if len(requirement.EvidenceKinds) == 0 {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, requirementField+".evidenceKinds", "requirement %q must accept at least one evidence kind", requirement.ID))
		}
		seenKinds := make(map[EvidenceKind]bool)
		for j, kind := range requirement.EvidenceKinds {
			kindField := fmt.Sprintf("%s.evidenceKinds[%d]", requirementField, j)
			if !validEvidenceKinds[kind] {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, kindField, "evidence kind %q is not supported", kind))
			}
			if seenKinds[kind] {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, kindField, "evidence kind %q is duplicated", kind))
			}
			seenKinds[kind] = true
		}
		diagnostics = append(diagnostics, validateVerificationCriterion(requirementField+".verification", version, requirement, seenKinds)...)
	}
	if validTarget {
		for rank := 1; rank <= targetRank; rank++ {
			stage := maturityStages[rank]
			if !coveredStages[stage] {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, field+".requirements", "feature %q must declare at least one requirement at stage %q", feature.ID, stage))
			}
		}
	}
	return diagnostics
}

// validateVerificationCriterion validates the closed machine criterion that
// makes a same-ID textual spec requirement executable. The criterion is
// authored beside the maturity requirement it belongs to, so a wrong shape is a
// manifest error rather than something a producer can work around at run time.
func validateVerificationCriterion(field string, version int, requirement Requirement, kinds map[EvidenceKind]bool) []diag.Diagnostic {
	criterion := requirement.Verification
	if criterion == nil {
		return nil
	}
	if version < ManifestProtocolVersion {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeUnknownField, field, "verification requires feature manifest protocolVersion %d", ManifestProtocolVersion)}
	}
	if !validVerificationKinds[criterion.Kind] {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidVerification, field+".kind", "verification kind %q is not supported", criterion.Kind)}
	}
	var diagnostics []diag.Diagnostic
	// The adapter turns every observed check into a source-bound automated
	// attestation, so a criterion that refuses attestation evidence could never
	// be satisfied by the run it declares.
	if !kinds[EvidenceKindAttestation] {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field, "a verified requirement must accept %q evidence", EvidenceKindAttestation))
	}
	diagnostics = append(diagnostics, validateVerificationChecks(field+".checks", *criterion)...)
	if criterion.Kind == VerificationKindAcceptance {
		return append(diagnostics, validateAcceptanceCriterion(field, *criterion)...)
	}
	return append(diagnostics, validateThresholdCriterion(field, requirement.Stage, *criterion)...)
}

// validateVerificationChecks pins the expected set. It is the whole reason a
// renamed, skipped, or no-longer-executed check becomes missing instead of
// leaving an earlier success green.
func validateVerificationChecks(field string, criterion VerificationCriterion) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if len(criterion.Checks) == 0 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field, "verification must declare at least one expected check"))
	}
	if criterion.Kind == VerificationKindThreshold && len(criterion.Checks) > 1 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field, "a threshold objective must declare exactly one check"))
	}
	seen := make(map[string]bool, len(criterion.Checks))
	for i, check := range criterion.Checks {
		checkField := fmt.Sprintf("%s[%d]", field, i)
		if len(check) > checkIDMaxLength || !segmentIDPattern.MatchString(check) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, checkField, "check ID must be one bounded lower-case ASCII ID segment"))
		}
		if seen[check] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, checkField, "check ID is duplicated"))
		}
		seen[check] = true
	}
	return diagnostics
}

func validateAcceptanceCriterion(field string, criterion VerificationCriterion) []diag.Diagnostic {
	if criterion.Metric != "" || criterion.Aggregation != "" || criterion.Operator != "" ||
		criterion.Target != nil || criterion.Unit != "" || criterion.Window != nil ||
		criterion.Environment != "" || criterion.MaxAgeSeconds != 0 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidVerification, field, "an acceptance criterion must omit metric, aggregation, operator, target, unit, window, environment, and maxAgeSeconds")}
	}
	return nil
}

func validateThresholdCriterion(field string, stage MaturityStage, criterion VerificationCriterion) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if len(criterion.Metric) > semanticCodeMaxLength || !claimPattern.MatchString(criterion.Metric) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".metric", "threshold metric must be a bounded lower-case semantic code"))
	}
	if len(criterion.Unit) > semanticCodeMaxLength || !claimPattern.MatchString(criterion.Unit) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".unit", "threshold unit must be a bounded lower-case semantic code"))
	}
	if !validAggregations[criterion.Aggregation] {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".aggregation", "threshold aggregation %q is not supported", criterion.Aggregation))
	}
	if !validOperators[criterion.Operator] {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".operator", "threshold operator %q is not supported", criterion.Operator))
	}
	// A target is a pointer precisely so zero and negative objectives stay
	// authorable and an omitted objective stays detectable.
	switch {
	case criterion.Target == nil:
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".target", "a threshold objective must declare a target"))
	case math.IsNaN(*criterion.Target) || math.IsInf(*criterion.Target, 0):
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".target", "threshold target must be a finite number"))
	}
	if criterion.Window == nil {
		return append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".window", "a threshold objective must declare an observation window"))
	}
	return append(diagnostics, validateVerificationWindow(field, stage, criterion)...)
}

func validateVerificationWindow(field string, stage MaturityStage, criterion VerificationCriterion) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	window := *criterion.Window
	if !validWindowKinds[window.Kind] {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidVerification, field+".window.kind", "verification window kind %q is not supported", window.Kind)}
	}
	if window.Kind == WindowKindInvocation {
		if window.Seconds != 0 {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".window.seconds", "an invocation window represents one run and carries no duration"))
		}
		if criterion.Environment != "" || criterion.MaxAgeSeconds != 0 {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field, "environment and maxAgeSeconds describe a rolling window only"))
		}
		return diagnostics
	}
	if window.Seconds <= 0 || window.Seconds > rollingWindowMaxSeconds {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".window.seconds", "a rolling window requires a positive duration of at most %d seconds", rollingWindowMaxSeconds))
	}
	if len(criterion.Environment) > semanticCodeMaxLength || !claimPattern.MatchString(criterion.Environment) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".environment", "a rolling objective requires a bounded lower-case environment code"))
	}
	if criterion.MaxAgeSeconds <= 0 || criterion.MaxAgeSeconds > rollingWindowMaxSeconds {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".maxAgeSeconds", "a rolling objective requires a positive freshness bound of at most %d seconds", rollingWindowMaxSeconds))
	}
	// A rolling production objective can only be observed where the feature is
	// actually live, so it stays reserved for that exact stage.
	if stage != MaturityLiveVerified {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidVerification, field+".window", "a rolling window is valid only on a %q requirement", MaturityLiveVerified))
	}
	return diagnostics
}

// ValidateVerificationReport validates one run-scoped report in isolation.
// Whether an observed check is declared by a criterion, which project may claim
// it, and how it resolves are deliberately not decided here: the report is
// transport, and the evaluator plus core own the verdict.
func ValidateVerificationReport(input *VerificationReport) []diag.Diagnostic {
	if input == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "verification report is nil")}
	}
	var diagnostics []diag.Diagnostic
	if input.Observations == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "observations", "observations is required and must be an array"))
	}
	report := CanonicalVerificationReport(input)
	if report.ProtocolVersion != VerificationReportProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocolVersion %d is not supported (want %d)", report.ProtocolVersion, VerificationReportProtocolVersion))
	}
	if len(report.Observations) > VerificationReportMaxObservations {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, "observations", "a verification report carries at most %d observations", VerificationReportMaxObservations))
	}
	seen := make(map[string]string, len(report.Observations))
	for i, observation := range report.Observations {
		field := fmt.Sprintf("observations[%d]", i)
		diagnostics = append(diagnostics, validateObservation(field, observation)...)
		key := observation.Feature + "\x00" + observation.Requirement + "\x00" + observation.Check
		if first, exists := seen[key]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateObservation, field, "check is observed more than once; first observation is %s", first))
		} else {
			seen[key] = field
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func validateObservation(field string, observation VerificationObservation) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if !featureIDPattern.MatchString(observation.Feature) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownFeature, field+".feature", "feature reference %q is not a canonical feature ID", observation.Feature))
	}
	if !segmentIDPattern.MatchString(observation.Requirement) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownRequirement, field+".requirement", "requirement reference %q is not a canonical requirement ID", observation.Requirement))
	}
	if len(observation.Check) > checkIDMaxLength || !segmentIDPattern.MatchString(observation.Check) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field+".check", "check ID must be one bounded lower-case ASCII ID segment"))
	}
	diagnostics = append(diagnostics, validateProtocolPath(field+".provenance.path", observation.Provenance.Path)...)
	if observation.Provenance.Symbol != "" && !boundedText(observation.Provenance.Symbol, 512) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field+".provenance.symbol", "provenance symbol must be bounded text without control characters"))
	}
	// An observation states either an acceptance verdict or a measurement. A
	// producer that stated both could describe a numeric objective and then
	// declare its own result for it.
	if (observation.Status == "") == (observation.Measurement == nil) {
		return append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field, "an observation must carry exactly one of status or measurement"))
	}
	if observation.Status != "" {
		if !validObservationStatuses[observation.Status] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field+".status", "observation status %q is not supported", observation.Status))
		}
		if observation.Window != nil || observation.Environment != "" {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field, "an acceptance observation must omit window and environment"))
		}
		return diagnostics
	}
	return append(diagnostics, validateMeasuredObservation(field, observation)...)
}

func validateMeasuredObservation(field string, observation VerificationObservation) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	measurement := *observation.Measurement
	if len(measurement.Name) > semanticCodeMaxLength || !claimPattern.MatchString(measurement.Name) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field+".measurement.name", "measurement name must be a bounded lower-case semantic code"))
	}
	if !validAggregations[measurement.Aggregation] {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field+".measurement.aggregation", "measurement aggregation %q is not supported", measurement.Aggregation))
	}
	if math.IsNaN(measurement.Value) || math.IsInf(measurement.Value, 0) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field+".measurement.value", "measurement value must be a finite number"))
	}
	if len(measurement.Unit) > semanticCodeMaxLength || !claimPattern.MatchString(measurement.Unit) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field+".measurement.unit", "measurement unit must be a bounded lower-case semantic code"))
	}
	if observation.Environment != "" && (len(observation.Environment) > semanticCodeMaxLength || !claimPattern.MatchString(observation.Environment)) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field+".environment", "environment must be a bounded lower-case semantic code"))
	}
	if observation.Window == nil {
		return append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field+".window", "a measured observation must state the window it covers"))
	}
	start, startErr := time.Parse(time.RFC3339, observation.Window.Start)
	end, endErr := time.Parse(time.RFC3339, observation.Window.End)
	if startErr != nil || endErr != nil {
		return append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field+".window", "observation window bounds must be valid RFC 3339 timestamps"))
	}
	if end.Before(start) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidObservation, field+".window", "observation window must not end before it starts"))
	}
	return diagnostics
}

// ValidateEvidenceDocument validates record shapes local to one evidence
// fragment. Global IDs and feature/requirement references are checked by
// ValidateRepository.
func ValidateEvidenceDocument(input *EvidenceDocument) []diag.Diagnostic {
	if input == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "evidence document is nil")}
	}
	var diagnostics []diag.Diagnostic
	if input.Evidence == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "evidence", "evidence is required and must be an array"))
	}
	document := CanonicalEvidenceDocument(input)
	if document.ProtocolVersion != EvidenceProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocolVersion %d is not supported (want 1)", document.ProtocolVersion))
	}
	seenEvidence := make(map[string]string)
	for i, evidence := range document.Evidence {
		field := fmt.Sprintf("evidence[%d]", i)
		diagnostics = append(diagnostics, validateEvidenceRecord(field, evidence)...)
		if first, exists := seenEvidence[evidence.ID]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateEvidence, field+".id", "evidence %q is duplicated; first declaration is %s", evidence.ID, first))
		} else {
			seenEvidence[evidence.ID] = field
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func validateEvidenceRecord(field string, evidence EvidenceRecord) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if !featureIDPattern.MatchString(evidence.ID) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, field+".id", "evidence ID %q is not a canonical stable ID", evidence.ID))
	}
	if !featureIDPattern.MatchString(evidence.Feature) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownFeature, field+".feature", "feature reference %q is not a canonical feature ID", evidence.Feature))
	}
	if !segmentIDPattern.MatchString(evidence.Requirement) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownRequirement, field+".requirement", "requirement reference %q is not a canonical requirement ID", evidence.Requirement))
	}
	stageRank, validStage := StageRank(evidence.Stage)
	if !validStage || stageRank == 0 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidStage, field+".stage", "evidence stage %q must be a post-modeled maturity stage", evidence.Stage))
	}
	if !validOutcomes[evidence.Outcome] {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field+".outcome", "evidence outcome %q is not supported", evidence.Outcome))
	}
	if !validIssuerKinds[evidence.Issuer.Kind] || !boundedText(evidence.Issuer.ID, 256) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidAuthority, field+".issuer", "issuer must have a supported kind and a bounded non-empty ID"))
	}
	diagnostics = append(diagnostics, validateSourceSelector(field+".source", evidence.Source)...)
	diagnostics = append(diagnostics, validateEvidenceSubject(field+".subject", evidence.Subject, evidence.Issuer)...)
	diagnostics = append(diagnostics, validateEvidenceProvenance(field+".provenance", evidence.Provenance, evidence.Source)...)
	if evidence.Persistent && (evidence.Issuer.Kind != IssuerKindHuman || evidence.Subject.Kind != EvidenceKindAttestation) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidAuthority, field+".persistent", "persistent validity is reserved for human-issued attestations"))
	}
	if evidence.ObservedAt != "" {
		if _, err := time.Parse(time.RFC3339, evidence.ObservedAt); err != nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field+".observedAt", "observedAt must be a valid RFC 3339 timestamp"))
		}
		if evidence.Subject.Kind != EvidenceKindAttestation {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field+".observedAt", "observedAt is allowed only for intrinsically temporal attestation subjects"))
		}
	}
	if evidence.ObservedRepositoryRevision != "" && !revisionPattern.MatchString(evidence.ObservedRepositoryRevision) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field+".observedRepositoryRevision", "observed repository revision must be git:<40-or-64-lower-hex>"))
	}
	return diagnostics
}

func validateSourceSelector(field string, source SourceSelector) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if !capabilities.ValidLocationRoots[source.Root] {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field+".root", "source root %q is not supported", source.Root))
	}
	switch source.Root {
	case LocationRootWorkspace:
		if source.OwnerProject != "" || source.Package != "" || source.Version != "" {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field, "workspace source must omit ownerProject, package, and version"))
		}
	case LocationRootProject:
		if !boundedText(source.OwnerProject, 256) || source.Package != "" || source.Version != "" {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field, "project source requires ownerProject and must omit package and version"))
		}
	case LocationRootPackage:
		if !boundedText(source.Package, 256) || !boundedText(source.Version, 128) || source.OwnerProject != "" {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field, "package source requires exact package and version and must omit ownerProject"))
		}
	}
	if !sourceBindingPattern.MatchString(source.Binding) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSourceBinding, field+".binding", "source binding must match source-v1:sha256:<64-lower-hex>"))
	}
	if source.Environment != "" && (len(source.Environment) > 128 || !claimPattern.MatchString(source.Environment)) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field+".environment", "environment must be a bounded lower-case semantic code"))
	}
	return diagnostics
}

func validateEvidenceSubject(field string, subject EvidenceSubject, issuer Issuer) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	payloads := 0
	if subject.Contribution != nil {
		payloads++
	}
	if subject.Artifact != nil {
		payloads++
	}
	if subject.Attestation != nil {
		payloads++
	}
	if !validEvidenceKinds[subject.Kind] || payloads != 1 {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidSubject, field, "subject must name one supported kind and carry exactly one payload")}
	}
	switch subject.Kind {
	case EvidenceKindCapability:
		if subject.Contribution == nil || subject.Artifact != nil || subject.Attestation != nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field, "capability subject must carry only contribution"))
		} else {
			for _, finding := range capabilities.ValidateContributionReference(*subject.Contribution) {
				finding.Field = field + "." + finding.Field
				diagnostics = append(diagnostics, finding)
			}
		}
	case EvidenceKindArtifact:
		if subject.Artifact == nil || subject.Contribution != nil || subject.Attestation != nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field, "artifact subject must carry only artifact"))
		} else {
			diagnostics = append(diagnostics, validateProtocolPath(field+".artifact.path", subject.Artifact.Path)...)
			if !sha256Pattern.MatchString(subject.Artifact.Digest) {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field+".artifact.digest", "artifact digest must match sha256:<64-lower-hex>"))
			}
		}
	case EvidenceKindAttestation:
		if subject.Attestation == nil || subject.Contribution != nil || subject.Artifact != nil {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field, "attestation subject must carry only attestation"))
		} else {
			claim := subject.Attestation.Claim
			if len(claim) > 128 || !claimPattern.MatchString(string(claim)) {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field+".attestation.claim", "attestation claim must be a bounded lower-case semantic code"))
			}
			if productPromiseClaims[claim] && issuer.Kind != IssuerKindHuman {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidAuthority, field+".attestation.claim", "product-promise attestation %q requires a human issuer", claim))
			}
		}
	}
	return diagnostics
}

func validateEvidenceProvenance(field string, provenance EvidenceProvenance, source SourceSelector) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	if !capabilities.ValidLocationRoots[provenance.Root] {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field+".root", "provenance root %q is not supported", provenance.Root))
	}
	if provenance.Root != source.Root {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field+".root", "provenance root must equal source root so the same exact root selector resolves both"))
	}
	diagnostics = append(diagnostics, validateProtocolPath(field+".path", provenance.Path)...)
	if provenance.Symbol != "" && !boundedText(provenance.Symbol, 512) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, field+".symbol", "provenance symbol must be bounded text without control characters"))
	}
	return diagnostics
}

// ValidateSpec validates semantics that are local to one durable
// specification. Whether the referenced feature exists, and whether another
// document already specifies it, are workspace facts and belong to
// ValidateSpecRepository.
func ValidateSpec(input *Spec) []diag.Diagnostic {
	if input == nil {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeParseError, "", "spec is nil")}
	}
	var diagnostics []diag.Diagnostic
	if input.Outcomes == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "outcomes", "outcomes is required and must be an array"))
	}
	if input.Requirements == nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeParseError, "requirements", "requirements is required and must be an array"))
	}
	spec := CanonicalSpec(input)
	if spec.ProtocolVersion != SpecProtocolVersion {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidProtocolVersion, "protocolVersion", "protocolVersion %d is not supported (want 1)", spec.ProtocolVersion))
	}
	if !featureIDPattern.MatchString(spec.Feature) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownFeature, "feature", "feature reference %q is not a canonical feature ID", spec.Feature))
	}
	if input.Outcomes != nil && len(spec.Outcomes) == 0 {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, "outcomes", "a spec must state at least one intended outcome"))
	}
	diagnostics = append(diagnostics, validateSpecStatements("outcomes", spec.Outcomes)...)
	diagnostics = append(diagnostics, validateSpecStatements("nonGoals", spec.NonGoals)...)

	seenRequirements := make(map[string]string)
	for i, requirement := range spec.Requirements {
		field := fmt.Sprintf("requirements[%d]", i)
		if !segmentIDPattern.MatchString(requirement.ID) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, field+".id", "requirement ID %q must be one lower-case ASCII ID segment", requirement.ID))
		}
		if first, exists := seenRequirements[requirement.ID]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, field+".id", "requirement %q is duplicated; first declaration is %s", requirement.ID, first))
		} else {
			seenRequirements[requirement.ID] = field
		}
		if !boundedText(requirement.Text, 512) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRequirement, field+".text", "requirement text must be non-empty bounded text without control characters"))
		}
	}

	seenDecisions := make(map[string]string)
	for i, decision := range spec.Decisions {
		field := fmt.Sprintf("decisions[%d]", i)
		diagnostics = append(diagnostics, validateDecisionLink(field, decision)...)
		if first, exists := seenDecisions[decision]; exists {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidDecision, field, "decision link is duplicated; first declaration is %s", first))
		} else {
			seenDecisions[decision] = field
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

func validateSpecStatements(field string, statements []string) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	for i, statement := range statements {
		if !boundedText(statement, 512) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidID, fmt.Sprintf("%s[%d]", field, i), "%s statements must be non-empty bounded text without control characters", field))
		}
	}
	return diagnostics
}

// validateDecisionLink keeps durable decisions on the one existing convention:
// a contained workspace-relative path to a project-local doc/adr/*.md record.
// The message never echoes the offending path, so an absolute or private path
// cannot leak through a diagnostic.
func validateDecisionLink(field, value string) []diag.Diagnostic {
	if findings := validateProtocolPath(field, value); len(findings) > 0 {
		return findings
	}
	if len(value) > decisionLinkMaxLength {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidDecision, field, "decision link must be at most %d characters", decisionLinkMaxLength)}
	}
	if !decisionLinkPattern.MatchString(value) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidDecision, field, "decision link must be a workspace-relative %s/<name>%s durable decision record", DecisionDirectory, DecisionExtension)}
	}
	return nil
}

// AuthoredFeatureIDs returns the sorted, de-duplicated feature identities
// declared by authored manifests. Spec validation resolves references against
// an explicit authored set precisely because a spec never mints a feature; a
// caller that also composes native design-graph feature nodes unions those IDs
// into the same set.
func AuthoredFeatureIDs(manifestSources []ManifestSource) []string {
	unique := make(map[string]bool)
	for _, source := range manifestSources {
		if source.Manifest == nil {
			continue
		}
		for _, feature := range source.Manifest.Features {
			unique[feature.ID] = true
		}
	}
	return sortedKeys(unique)
}

// ValidateSpecRepository validates all local spec shapes plus the workspace
// facts a single document cannot decide: that every referenced feature is
// already authored elsewhere, that exactly one spec details each feature, and
// that every spec was discovered at a canonical specs/*.json location. Inputs
// are sorted by canonical path; caller order never affects findings.
func ValidateSpecRepository(specSources []SpecSource, authoredFeatureIDs []string) []diag.Diagnostic {
	specs := append([]SpecSource(nil), specSources...)
	sort.Slice(specs, func(i, j int) bool { return specs[i].Path < specs[j].Path })
	authored := make(map[string]bool, len(authoredFeatureIDs))
	for _, id := range authoredFeatureIDs {
		authored[id] = true
	}

	var diagnostics []diag.Diagnostic
	specsByFeature := make(map[string][]string)
	for _, source := range specs {
		canonicalSource := source.Path
		if finding := validateSourcePath(source.Path); finding != nil {
			diagnostics = append(diagnostics, *finding)
			canonicalSource = ""
		} else if !specSourcePattern.MatchString(source.Path) {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeOutsideDiscoveryRoot, "path", "a spec must be a direct JSON child of %s/ at a workspace or exact project root", SpecDirectory))
			canonicalSource = ""
		}
		for _, finding := range ValidateSpec(source.Spec) {
			diagnostics = append(diagnostics, withSource(canonicalSource, finding))
		}
		if source.Spec == nil {
			continue
		}
		feature := source.Spec.Feature
		if !featureIDPattern.MatchString(feature) {
			continue
		}
		if !authored[feature] {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownFeature, sourceField(canonicalSource, "feature"), "spec references feature %q, which no authored declaration mints", feature))
			continue
		}
		specsByFeature[feature] = append(specsByFeature[feature], sourceField(canonicalSource, "feature"))
	}
	for _, feature := range sortedKeys(specsByFeature) {
		fields := specsByFeature[feature]
		sort.Strings(fields)
		for i := 1; i < len(fields); i++ {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateSpec, fields[i], "feature %q is specified more than once; first spec is %s", feature, fields[0]))
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

// ValidateRepository validates all local shapes plus workspace-wide feature
// and evidence identities and references. Inputs are sorted by canonical path;
// caller order never affects findings.
func ValidateRepository(manifestSources []ManifestSource, evidenceSources []EvidenceSource) []diag.Diagnostic {
	manifests := append([]ManifestSource(nil), manifestSources...)
	evidence := append([]EvidenceSource(nil), evidenceSources...)
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Path < manifests[j].Path })
	sort.Slice(evidence, func(i, j int) bool { return evidence[i].Path < evidence[j].Path })
	var diagnostics []diag.Diagnostic

	featureDeclarations := make(map[string][]featureAt)
	for _, source := range manifests {
		canonicalSource := source.Path
		if finding := validateSourcePath(source.Path); finding != nil {
			diagnostics = append(diagnostics, *finding)
			canonicalSource = ""
		}
		for _, finding := range ValidateManifest(source.Manifest) {
			diagnostics = append(diagnostics, withSource(canonicalSource, finding))
		}
		if source.Manifest == nil {
			continue
		}
		manifest := CanonicalManifest(source.Manifest)
		for i, feature := range manifest.Features {
			location := sourceField(canonicalSource, fmt.Sprintf("features[%d]", i))
			featureDeclarations[feature.ID] = append(featureDeclarations[feature.ID], featureAt{feature: feature, field: location, source: canonicalSource})
		}
	}

	featureIDs := sortedKeys(featureDeclarations)
	uniqueFeatures := make(map[string]featureAt)
	for _, id := range featureIDs {
		declarations := featureDeclarations[id]
		sort.Slice(declarations, func(i, j int) bool { return declarations[i].field < declarations[j].field })
		bySource := firstFeatureBySource(declarations)
		if len(bySource) > 1 {
			for i := 1; i < len(bySource); i++ {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateFeature, bySource[i].field+".id", "feature %q is duplicated across source documents; first declaration is %s", id, bySource[0].field))
			}
		}
		if len(declarations) == 1 {
			uniqueFeatures[id] = declarations[0]
		}
	}
	diagnostics = append(diagnostics, validateAggregateRelations(uniqueFeatures, featureDeclarations)...)

	evidenceDeclarations := make(map[string][]evidenceAt)
	for _, source := range evidence {
		canonicalSource := source.Path
		if finding := validateSourcePath(source.Path); finding != nil {
			diagnostics = append(diagnostics, *finding)
			canonicalSource = ""
		}
		for _, finding := range ValidateEvidenceDocument(source.Document) {
			diagnostics = append(diagnostics, withSource(canonicalSource, finding))
		}
		if source.Document == nil {
			continue
		}
		document := CanonicalEvidenceDocument(source.Document)
		for i, record := range document.Evidence {
			location := sourceField(canonicalSource, fmt.Sprintf("evidence[%d]", i))
			evidenceDeclarations[record.ID] = append(evidenceDeclarations[record.ID], evidenceAt{record: record, field: location, source: canonicalSource})
			declaration, exists := uniqueFeatures[record.Feature]
			if !exists {
				if len(featureDeclarations[record.Feature]) == 0 {
					diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownFeature, location+".feature", "evidence %q references unknown feature %q", record.ID, record.Feature))
				}
				continue
			}
			requirement, found := findRequirement(declaration.feature, record.Requirement)
			if !found {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeUnknownRequirement, location+".requirement", "evidence %q references unknown requirement %q on feature %q", record.ID, record.Requirement, record.Feature))
				continue
			}
			if record.Stage != requirement.Stage {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeStageMismatch, location+".stage", "evidence stage %q does not match requirement stage %q", record.Stage, requirement.Stage))
			}
			if !containsEvidenceKind(requirement.EvidenceKinds, record.Subject.Kind) {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidSubject, location+".subject.kind", "subject kind %q is not accepted by requirement %q", record.Subject.Kind, record.Requirement))
			}
		}
	}
	for _, id := range sortedKeys(evidenceDeclarations) {
		declarations := evidenceDeclarations[id]
		sort.Slice(declarations, func(i, j int) bool { return declarations[i].field < declarations[j].field })
		bySource := firstEvidenceBySource(declarations)
		for i := 1; i < len(bySource); i++ {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDuplicateEvidence, bySource[i].field+".id", "evidence %q is duplicated across source documents; first declaration is %s", id, bySource[0].field))
		}
	}
	sortDiagnostics(diagnostics)
	return diagnostics
}

type featureAt struct {
	feature Feature
	field   string
	source  string
}
type evidenceAt struct {
	record EvidenceRecord
	field  string
	source string
}

func firstFeatureBySource(declarations []featureAt) []featureAt {
	seen := make(map[string]bool)
	var output []featureAt
	for _, declaration := range declarations {
		if !seen[declaration.source] {
			seen[declaration.source] = true
			output = append(output, declaration)
		}
	}
	return output
}

func firstEvidenceBySource(declarations []evidenceAt) []evidenceAt {
	seen := make(map[string]bool)
	var output []evidenceAt
	for _, declaration := range declarations {
		if !seen[declaration.source] {
			seen[declaration.source] = true
			output = append(output, declaration)
		}
	}
	return output
}

func validateAggregateRelations(unique map[string]featureAt, all map[string][]featureAt) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	graphs := map[RelationKind]map[string][]string{
		RelationKindParent: {}, RelationKindDependsOn: {},
	}
	for _, id := range sortedKeys(unique) {
		declaration := unique[id]
		for i, relation := range declaration.feature.Relations {
			field := fmt.Sprintf("%s.relations[%d]", declaration.field, i)
			if relation.Target == id {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeInvalidRelation, field, "feature %q cannot relate to itself", id))
			}
			if len(all[relation.Target]) == 0 {
				diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDanglingRelation, field+".target", "relation target %q does not exist in the workspace aggregation", relation.Target))
			}
			if graph, ok := graphs[relation.Kind]; ok && len(all[relation.Target]) == 1 && relation.Target != id {
				graph[id] = append(graph[id], relation.Target)
			}
		}
	}
	for kind, graph := range graphs {
		for node := range graph {
			sort.Strings(graph[node])
		}
		for _, source := range sortedKeys(graph) {
			for _, target := range graph[source] {
				if reachable(graph, target, source, map[string]bool{}) {
					declaration := unique[source]
					for i, relation := range declaration.feature.Relations {
						if relation.Kind == kind && relation.Target == target {
							diagnostics = append(diagnostics, diag.Errorf(ErrorCodeRelationCycle, fmt.Sprintf("%s.relations[%d]", declaration.field, i), "%s relation %q -> %q participates in a cycle", kind, source, target))
						}
					}
				}
			}
		}
	}
	return diagnostics
}

func reachable(graph map[string][]string, current, want string, seen map[string]bool) bool {
	if current == want {
		return true
	}
	if seen[current] {
		return false
	}
	seen[current] = true
	for _, next := range graph[current] {
		if reachable(graph, next, want, seen) {
			return true
		}
	}
	return false
}

func findRequirement(feature Feature, id string) (Requirement, bool) {
	for _, requirement := range feature.Requirements {
		if requirement.ID == id {
			return requirement, true
		}
	}
	return Requirement{}, false
}

func containsEvidenceKind(kinds []EvidenceKind, want EvidenceKind) bool {
	for _, kind := range kinds {
		if kind == want {
			return true
		}
	}
	return false
}

func withSource(source string, finding diag.Diagnostic) diag.Diagnostic {
	if source == "" {
		return finding
	}
	if finding.Field == "" {
		finding.Field = source
	} else {
		finding.Field = source + "#" + finding.Field
	}
	return finding
}

func sourceField(source, field string) string {
	if source == "" {
		return field
	}
	return source + "#" + field
}

func validateSourcePath(value string) *diag.Diagnostic {
	findings := validateProtocolPath("path", value)
	if len(findings) == 0 {
		return nil
	}
	finding := findings[0]
	finding.Message = "discovery source path must be a canonical contained workspace-relative slash path"
	return &finding
}

func validateProtocolPath(field, value string) []diag.Diagnostic {
	if value == "" || strings.Contains(value, "\\") || strings.ContainsRune(value, 0) || strings.HasPrefix(value, "/") || schemePattern.MatchString(value) {
		return []diag.Diagnostic{diag.Errorf(ErrorCodeInvalidPath, field, "path must be a non-empty contained relative slash path")}
	}
	if path.Clean(value) != value || value == "." {
		code := ErrorCodeInvalidPath
		for _, segment := range strings.Split(value, "/") {
			if segment == ".." {
				code = ErrorCodePathEscape
				break
			}
		}
		return []diag.Diagnostic{diag.Errorf(code, field, "path must be its own contained lexical clean form")}
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			code := ErrorCodeInvalidPath
			if segment == ".." {
				code = ErrorCodePathEscape
			}
			return []diag.Diagnostic{diag.Errorf(code, field, "path contains an invalid segment")}
		}
	}
	return nil
}

func boundedText(value string, maximum int) bool {
	if strings.TrimSpace(value) == "" || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortDiagnostics(diagnostics []diag.Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if left.Severity != right.Severity {
			return left.Severity < right.Severity
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		return left.Message < right.Message
	})
}
