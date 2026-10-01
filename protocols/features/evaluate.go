package features

import (
	"sort"
	"time"
)

// RequirementVerificationState is the closed executable-verification vocabulary
// reported for one textual spec requirement.
//
// It answers one question only: does the run that just executed prove the
// sentence the team agreed to? Durable evidence freshness and maturity stay
// with the repository evidence resolver, which remains the only state reducer
// over committed evidence. Nothing here derives a maturity stage.
type RequirementVerificationState string

const (
	// RequirementUnmapped says no feature requirement shares the spec
	// requirement's ID, so nothing can execute it.
	RequirementUnmapped RequirementVerificationState = "unmapped"
	// RequirementUnexecutable says the same-ID feature requirement exists but
	// declares no machine verification criterion.
	RequirementUnexecutable RequirementVerificationState = "unexecutable"
	// RequirementMissing says an expected check or measurement did not run, was
	// skipped, or was reported in a shape the criterion cannot accept.
	RequirementMissing RequirementVerificationState = "missing"
	// RequirementStale says the only support available is outside the declared
	// rolling window, environment, or freshness bound.
	RequirementStale RequirementVerificationState = "stale"
	// RequirementContradicted says an active check failed or an objective was
	// missed.
	RequirementContradicted RequirementVerificationState = "contradicted"
	// RequirementUnobserved says every unresolved check of the requirement had
	// NO observation source in this run's scope at all — no producer reported
	// on it, and none could be consulted. It is deliberately distinct from
	// RequirementMissing, which means a source WAS consulted and still did not
	// support the check.
	//
	// The pure evaluator never returns it: with no observations, "nothing ran"
	// must keep reading as missing (RequirementMissing) so a reader replaying a
	// record cannot mistake absence for an excuse. Only a collector that knows
	// which producers were in scope — and therefore that a source was never
	// consulted rather than consulted and silent — may reclassify a missing
	// requirement into this state, and GroupBlocks does not block on it.
	RequirementUnobserved RequirementVerificationState = "unobserved"
	// RequirementVerified says every expected check passed, or the current
	// objective was met, with no contradiction.
	RequirementVerified RequirementVerificationState = "verified"
)

// ValidRequirementVerificationStates is the closed vocabulary a persisted
// record may carry. It is exported so the record's strict reader and every
// surface that renders a state branch on ONE set, rather than each on a
// private switch that silently ignores a value it does not know.
//
// The record's protocol version is NOT bumped when a state is added. The
// version selects the document SHAPE, and the shape is unchanged; an older
// reader still parses every member it knows and reads the state as an opaque
// string, which degrades a roll-up count at worst. Bumping instead would make
// every older reader refuse the whole record — a strictly worse failure for a
// derived audit document.
var ValidRequirementVerificationStates = map[RequirementVerificationState]bool{
	RequirementUnmapped:     true,
	RequirementUnexecutable: true,
	RequirementMissing:      true,
	RequirementStale:        true,
	RequirementContradicted: true,
	RequirementVerified:     true,
	RequirementUnobserved:   true,
}

// CheckState is the closed per-check classification of one declared check.
type CheckState string

const (
	// CheckSatisfied is a passing acceptance check or a met objective.
	CheckSatisfied CheckState = "satisfied"
	// CheckViolated is a failing acceptance check or a missed objective.
	CheckViolated CheckState = "violated"
	// CheckMissing is a declared check with no usable observation.
	CheckMissing CheckState = "missing"
	// CheckStale is an observation outside the declared window, environment, or
	// freshness bound.
	CheckStale CheckState = "stale"
	// CheckUnclassified is an observation the criterion cannot interpret. It
	// never grants support.
	CheckUnclassified CheckState = "unclassified"
)

// Stable bounded reason codes. They are automation vocabulary, so they never
// echo authored text, paths, or observed values.
const (
	ReasonCheckNotObserved         = "check-not-observed"
	ReasonCheckSkipped             = "check-skipped"
	ReasonCheckFailed              = "check-failed"
	ReasonCheckPassed              = "check-passed"
	ReasonObjectiveMet             = "objective-met"
	ReasonObjectiveMissed          = "objective-missed"
	ReasonMeasurementMissing       = "measurement-missing"
	ReasonMeasurementUnexpected    = "measurement-unexpected"
	ReasonMetricMismatch           = "metric-mismatch"
	ReasonAggregationMismatch      = "aggregation-mismatch"
	ReasonUnitMismatch             = "unit-mismatch"
	ReasonEnvironmentMismatch      = "environment-mismatch"
	ReasonWindowTooShort           = "window-too-short"
	ReasonWindowUnusable           = "window-unusable"
	ReasonObjectiveUnauthored      = "objective-unauthored"
	ReasonObservationExpired       = "observation-expired"
	ReasonObservationAhead         = "observation-ahead-of-evaluation"
	ReasonEvaluationInstantMissing = "evaluation-instant-missing"
	ReasonUndeclaredCheck          = "undeclared-check"
	ReasonDuplicateObservation     = "duplicate-observation"
)

// CheckEvaluation is the deterministic classification of one check.
type CheckEvaluation struct {
	// Check is the declared or observed check identity.
	Check string
	// State is the closed per-check classification.
	State CheckState
	// Reason is a stable bounded reason code explaining State.
	Reason string
	// Measurement echoes the observed aggregate of a threshold check.
	Measurement *ObservationMeasurement
	// Provenance echoes the reported declaration location, when one was observed.
	Provenance *ObservationProvenance
}

// EvidenceOutcome maps one check classification onto the durable evidence
// vocabulary. The second result is false when the check grants neither support
// nor a contradiction, so an adapter emits no record at all rather than an
// empty one.
func (evaluation CheckEvaluation) EvidenceOutcome() (EvidenceOutcome, bool) {
	switch evaluation.State {
	case CheckSatisfied:
		return EvidenceOutcomeSupports, true
	case CheckViolated:
		return EvidenceOutcomeContradicts, true
	default:
		return "", false
	}
}

// RequirementEvaluation is the deterministic result of joining one authored
// criterion with the observations reported for it.
type RequirementEvaluation struct {
	// Feature is the exact feature identity the requirement belongs to.
	Feature string
	// Requirement is the feature-local requirement identity.
	Requirement string
	// State is the executable-verification state of the requirement.
	State RequirementVerificationState
	// Stage is the maturity stage the requirement contributes to.
	Stage MaturityStage
	// Kind is the verification family the criterion declares.
	Kind VerificationKind
	// Checks classifies every declared check, sorted by check identity.
	Checks []CheckEvaluation
	// Unexpected lists observations for checks the criterion never declared,
	// sorted by check identity. They are reported for review and never grant
	// support.
	Unexpected []CheckEvaluation
}

// AttestationClaim returns the automated claim code an adapter uses when it
// turns this evaluation into an in-memory evidence record.
func (criterion VerificationCriterion) AttestationClaim() AttestationClaim {
	if criterion.Kind == VerificationKindThreshold {
		return AttestationClaimObjectiveMet
	}
	return AttestationClaimAcceptance
}

// EvaluateRequirement classifies the observations reported for one authored
// criterion. It is pure: it reads no filesystem, no clock, and no CLI state.
//
// At is the evaluation instant and is used only by rolling freshness. Passing
// the zero instant with a rolling criterion is treated as an absent clock and
// resolves stale, because a gate must not read an unbounded observation as
// current.
func EvaluateRequirement(feature string, requirement Requirement, observations []VerificationObservation, at time.Time) RequirementEvaluation {
	evaluation := RequirementEvaluation{Feature: feature, Requirement: requirement.ID, Stage: requirement.Stage}
	if requirement.Verification == nil {
		evaluation.State = RequirementUnexecutable
		return evaluation
	}
	criterion := *requirement.Verification
	evaluation.Kind = criterion.Kind

	declared := make(map[string]bool, len(criterion.Checks))
	for _, check := range criterion.Checks {
		declared[check] = true
	}
	observed, duplicated := indexObservations(feature, requirement.ID, observations)

	checks := make([]CheckEvaluation, 0, len(declared))
	for _, check := range sortedKeys(declared) {
		checks = append(checks, evaluateCheck(check, criterion, observed[check], duplicated[check], at))
	}
	evaluation.Checks = checks

	for _, check := range sortedKeys(observed) {
		if declared[check] {
			continue
		}
		observation := observed[check]
		evaluation.Unexpected = append(evaluation.Unexpected, CheckEvaluation{
			Check:       check,
			State:       CheckUnclassified,
			Reason:      ReasonUndeclaredCheck,
			Measurement: copyMeasurement(observation.Measurement),
			Provenance:  &ObservationProvenance{Path: observation.Provenance.Path, Symbol: observation.Provenance.Symbol},
		})
	}
	evaluation.State = reduceCheckStates(checks)
	return evaluation
}

// copyMeasurement keeps an evaluation independent of the report it read, so a
// caller that inspects a result cannot reach back into the observation.
func copyMeasurement(measurement *ObservationMeasurement) *ObservationMeasurement {
	if measurement == nil {
		return nil
	}
	copied := *measurement
	return &copied
}

// indexObservations selects the observations that belong to one requirement and
// records which checks were reported more than once. A repeated check is never
// resolved by preferring one report: a duplicate is exactly the case where two
// producers could disagree, so it is reported and blocks instead.
func indexObservations(feature, requirement string, observations []VerificationObservation) (map[string]VerificationObservation, map[string]bool) {
	observed := make(map[string]VerificationObservation)
	duplicated := make(map[string]bool)
	for _, observation := range observations {
		if observation.Feature != feature || observation.Requirement != requirement {
			continue
		}
		if _, exists := observed[observation.Check]; exists {
			duplicated[observation.Check] = true
			continue
		}
		observed[observation.Check] = observation
	}
	return observed, duplicated
}

func evaluateCheck(check string, criterion VerificationCriterion, observation VerificationObservation, duplicated bool, at time.Time) CheckEvaluation {
	evaluation := CheckEvaluation{Check: check}
	if duplicated {
		evaluation.State, evaluation.Reason = CheckUnclassified, ReasonDuplicateObservation
		return evaluation
	}
	if observation.Check == "" {
		evaluation.State, evaluation.Reason = CheckMissing, ReasonCheckNotObserved
		return evaluation
	}
	evaluation.Provenance = &ObservationProvenance{Path: observation.Provenance.Path, Symbol: observation.Provenance.Symbol}
	evaluation.Measurement = copyMeasurement(observation.Measurement)
	if observation.Status == ObservationSkipped {
		evaluation.State, evaluation.Reason = CheckMissing, ReasonCheckSkipped
		return evaluation
	}
	if criterion.Kind == VerificationKindAcceptance {
		return evaluateAcceptanceCheck(evaluation, observation)
	}
	return evaluateThresholdCheck(evaluation, criterion, observation, at)
}

func evaluateAcceptanceCheck(evaluation CheckEvaluation, observation VerificationObservation) CheckEvaluation {
	switch observation.Status {
	case ObservationPassed:
		evaluation.State, evaluation.Reason = CheckSatisfied, ReasonCheckPassed
	case ObservationFailed:
		evaluation.State, evaluation.Reason = CheckViolated, ReasonCheckFailed
	default:
		// A measurement answers a question an acceptance criterion never asked.
		evaluation.State, evaluation.Reason = CheckUnclassified, ReasonMeasurementUnexpected
	}
	return evaluation
}

func evaluateThresholdCheck(evaluation CheckEvaluation, criterion VerificationCriterion, observation VerificationObservation, at time.Time) CheckEvaluation {
	// A producer may report that a benchmark did not run, but never that it
	// passed: the verdict for a numeric objective is recomputed here from the
	// authored target, so a self-declared status is not usable support.
	if observation.Measurement == nil {
		evaluation.State, evaluation.Reason = CheckUnclassified, ReasonMeasurementMissing
		return evaluation
	}
	measurement := *observation.Measurement
	switch {
	case measurement.Name != criterion.Metric:
		evaluation.State, evaluation.Reason = CheckUnclassified, ReasonMetricMismatch
		return evaluation
	case measurement.Aggregation != criterion.Aggregation:
		evaluation.State, evaluation.Reason = CheckUnclassified, ReasonAggregationMismatch
		return evaluation
	case measurement.Unit != criterion.Unit:
		evaluation.State, evaluation.Reason = CheckUnclassified, ReasonUnitMismatch
		return evaluation
	}
	if criterion.Window != nil && criterion.Window.Kind == WindowKindRolling {
		if reason, stale := rollingStaleReason(criterion, observation, at); stale {
			evaluation.State, evaluation.Reason = CheckStale, reason
			return evaluation
		}
	}
	// A criterion without a target is refused by manifest validation. Reaching
	// here means the caller built one directly, so the check grants nothing
	// rather than reporting a verdict against an objective nobody authored.
	if criterion.Target == nil {
		evaluation.State, evaluation.Reason = CheckUnclassified, ReasonObjectiveUnauthored
		return evaluation
	}
	if satisfiesTarget(measurement.Value, criterion.Operator, *criterion.Target) {
		evaluation.State, evaluation.Reason = CheckSatisfied, ReasonObjectiveMet
		return evaluation
	}
	evaluation.State, evaluation.Reason = CheckViolated, ReasonObjectiveMissed
	return evaluation
}

// rollingStaleReason decides whether a rolling observation is still current.
// The observation must have been taken in the declared environment, must cover
// at least the declared duration, and must be no older than the declared
// freshness bound at the evaluation instant.
func rollingStaleReason(criterion VerificationCriterion, observation VerificationObservation, at time.Time) (string, bool) {
	if observation.Environment != criterion.Environment {
		return ReasonEnvironmentMismatch, true
	}
	start, startErr := time.Parse(time.RFC3339, windowBound(observation.Window, true))
	end, endErr := time.Parse(time.RFC3339, windowBound(observation.Window, false))
	if startErr != nil || endErr != nil {
		return ReasonWindowUnusable, true
	}
	if end.Sub(start) < time.Duration(criterion.Window.Seconds)*time.Second {
		return ReasonWindowTooShort, true
	}
	if at.IsZero() {
		return ReasonEvaluationInstantMissing, true
	}
	if end.After(at) {
		return ReasonObservationAhead, true
	}
	if at.Sub(end) > time.Duration(criterion.MaxAgeSeconds)*time.Second {
		return ReasonObservationExpired, true
	}
	return "", false
}

func windowBound(window *ObservedWindow, start bool) string {
	if window == nil {
		return ""
	}
	if start {
		return window.Start
	}
	return window.End
}

// satisfiesTarget compares an observed aggregate to the authored objective.
// Equality is exact: an authored target is a reviewed number, and silently
// widening it with a tolerance would let a producer choose its own margin.
func satisfiesTarget(value float64, operator VerificationOperator, target float64) bool {
	switch operator {
	case OperatorEq:
		return value == target
	case OperatorLte:
		return value <= target
	case OperatorGte:
		return value >= target
	default:
		return false
	}
}

// reduceCheckStates folds declared-check states into one requirement state. An
// active contradiction always wins, and one passing check can never hide a
// check that did not run.
func reduceCheckStates(checks []CheckEvaluation) RequirementVerificationState {
	if len(checks) == 0 {
		return RequirementMissing
	}
	state := RequirementVerified
	for _, check := range checks {
		switch check.State {
		case CheckViolated:
			return RequirementContradicted
		case CheckMissing, CheckUnclassified:
			state = RequirementMissing
		case CheckStale:
			if state != RequirementMissing {
				state = RequirementStale
			}
		}
	}
	return state
}

// SpecRequirementEvaluation is one textual requirement joined to the same
// feature's authored requirement of the same ID.
type SpecRequirementEvaluation struct {
	// Feature is the exact feature identity the spec details.
	Feature string
	// Requirement is the shared spec-local and feature-local requirement ID.
	Requirement string
	// State is the executable-verification state of the textual requirement.
	State RequirementVerificationState
	// Evaluation carries the per-check detail. It is nil when the requirement is
	// unmapped or has no criterion to execute.
	Evaluation *RequirementEvaluation
}

// EvaluateSpecRequirements joins one spec's textual requirements to the same
// feature's authored requirements and the observations reported for them. The
// join key is the requirement ID, so a sentence becomes executable only when
// the feature declares a requirement with that exact ID and a criterion.
//
// Feature requirements without a matching textual requirement are deliberately
// left alone: they express maturity evidence the spec never states. Results are
// returned sorted by requirement ID so two callers reading the same inputs in a
// different order report the same thing.
func EvaluateSpecRequirements(feature string, specRequirements []SpecRequirement, requirements []Requirement, observations []VerificationObservation, at time.Time) []SpecRequirementEvaluation {
	byID := make(map[string]Requirement, len(requirements))
	for _, requirement := range requirements {
		if _, exists := byID[requirement.ID]; !exists {
			byID[requirement.ID] = requirement
		}
	}
	results := make([]SpecRequirementEvaluation, 0, len(specRequirements))
	for _, specRequirement := range specRequirements {
		result := SpecRequirementEvaluation{Feature: feature, Requirement: specRequirement.ID}
		requirement, mapped := byID[specRequirement.ID]
		switch {
		case !mapped:
			result.State = RequirementUnmapped
		case requirement.Verification == nil:
			result.State = RequirementUnexecutable
		default:
			evaluation := EvaluateRequirement(feature, requirement, observations, at)
			result.State, result.Evaluation = evaluation.State, &evaluation
		}
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Requirement < results[j].Requirement })
	return results
}
