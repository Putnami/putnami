package sdd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// `specs verify` is the audit surface of the spec gate (decision 9):
// core's post-session finalizer collects, joins, and decides; this command
// REPRODUCES what the gate decided from the record persisted beside the
// session, and renders it for human, JSON, and JSONL consumers. It computes no
// new sanction of its own — a recorded group's Blocked decision is core's, and
// a group the session never evaluated is reported through the same shared
// blocking rule (featureproto.GroupBlocks) every surface uses, so no reader
// can receive a different verdict here than the gate gave.

const (
	// sessionsDirectory is where the engine persists one directory per session.
	// The record filename inside it is protocols/features'
	// SpecVerificationRecordFilename; together they are the replay contract
	// ("persist the verification projection beside the normal
	// session report"), which is why this reader may know the layout without
	// growing a workspace loader.
	sessionsDirectory = ".putnami/sessions"
	// latestSessionAlias resolves to the most recent recorded session.
	latestSessionAlias = "latest"
	// specVerifyMaxRecordBytes bounds the persisted record read.
	specVerifyMaxRecordBytes = 16 << 20
)

// SpecVerifyReport is the typed data of both successful and failed verify runs.
type SpecVerifyReport struct {
	Revision  featureengine.Revision `json:"revision"`
	Selection SelectionReport        `json:"selection"`
	// Session is the recorded session the verdicts were replayed from; empty
	// when no recorded gate run was found.
	Session string `json:"session,omitempty"`
	// GeneratedAt echoes the record's evaluation instant.
	GeneratedAt string `json:"generatedAt,omitempty"`
	// Recorded reports whether a persisted gate record backed this answer.
	// When false, every group's states assume zero observations.
	Recorded bool `json:"recorded"`
	// Groups is one entry per selected specified feature, in the record's
	// canonical (project, feature) order.
	Groups []featureproto.SpecVerificationGroup `json:"groups"`
	// Counts aggregates every group's roll-up.
	Counts featureproto.SpecVerificationCounts `json:"counts"`
	// Blocked lists the projects whose enforce-mode groups did not resolve,
	// sorted. It is the report's final blocking decision.
	Blocked     []string          `json:"blocked"`
	Diagnostics []diag.Diagnostic `json:"diagnostics"`
}

// BuildSpecVerifyResult builds the verify report for the resolved selection.
// session is the explicit --session value; empty means the latest recorded
// session. Structural spec errors fail exactly as `specs validate` fails —
// the tri-state never weakens the structural contract.
func BuildSpecVerifyResult(ws *workspace.Workspace, selection Selection, session string) (SpecVerifyReport, error) {
	repository, err := loadSpecRepository(ws, selection)
	if err != nil {
		return SpecVerifyReport{}, err
	}
	report := SpecVerifyReport{
		Revision:    repository.revision,
		Selection:   repository.selection,
		Groups:      []featureproto.SpecVerificationGroup{},
		Blocked:     []string{},
		Diagnostics: append([]diag.Diagnostic(nil), repository.diagnostics...),
	}
	if diag.HasErrors(report.Diagnostics) {
		report.Diagnostics = featureDiagnostics(report.Diagnostics)
		return report, specContractError("specs verify", report.Diagnostics, report)
	}

	record, sessionID, findings := readRecordedVerification(ws.Root, session)
	report.Diagnostics = append(report.Diagnostics, findings...)
	if record != nil {
		report.Recorded = true
		report.Session = sessionID
		report.GeneratedAt = record.GeneratedAt
	}
	recorded := make(map[string]featureproto.SpecVerificationGroup)
	if record != nil {
		for _, group := range record.Groups {
			if group.Feature != "" {
				recorded[group.Feature] = group
			}
		}
	}

	criteria := criteriaProjectionFromRepository(repository)
	blocked := make(map[string]bool)
	for _, expected := range criteria.Groups {
		group, found := recorded[expected.Feature]
		if !found {
			group, err = unevaluatedGroup(ws, repository, expected)
			if err != nil {
				return report, err
			}
			if group.AutomaticEvaluation {
				// Two different facts read the same way in the states below —
				// "a session evaluated this and saw nothing" and "no session
				// ever evaluated it" — so each says which one it is, and the
				// second names the command that produces the evidence. Without
				// that second message a workspace whose latest session carried
				// no spec gate blocks with no stated cause at all.
				if record != nil {
					report.Diagnostics = append(report.Diagnostics, diag.Warningf(featureproto.WarningCodeMissingEvidence,
						expected.Spec, "feature %q was not evaluated by session %s; states assume no observations",
						expected.Feature, sessionID))
				} else {
					report.Diagnostics = append(report.Diagnostics, diag.Warningf(featureproto.WarningCodeMissingEvidence,
						expected.Spec, "feature %q has no recorded evaluation to replay; run `putnami test,validate` "+
							"over its project to produce one", expected.Feature))
				}
			}
		}
		report.Groups = append(report.Groups, group)
		addCounts(&report.Counts, group.Counts)
		if group.Blocked {
			blocked[group.Project] = true
		}
	}
	for project := range blocked {
		report.Blocked = append(report.Blocked, project)
	}
	sort.Strings(report.Blocked)
	report.Diagnostics = featureDiagnostics(report.Diagnostics)

	if diag.HasErrors(report.Diagnostics) {
		return report, specContractError("specs verify", report.Diagnostics, report)
	}
	if len(report.Blocked) > 0 {
		return report, WithResultData(fmt.Errorf("specs verify: %d project(s) blocked under enforce: %s",
			len(report.Blocked), strings.Join(report.Blocked, ", ")), report)
	}
	return report, nil
}

// unevaluatedGroup renders one selected spec no recorded session covered:
// the committed policy resolves exactly as the gate would resolve it, and the
// states come from the one pure evaluator with zero observations, so "nothing
// ran" reads as missing rather than as silently green.
func unevaluatedGroup(ws *workspace.Workspace, repository *specRepository, expected featureproto.SpecCriteriaGroup) (featureproto.SpecVerificationGroup, error) {
	projectID, projectOptions := specOwnerPolicy(ws, repository, expected.Feature)
	var workspaceOptions map[string]map[string]any
	if ws.Config != nil {
		workspaceOptions = ws.Config.Options
	}
	mode, source, err := featureproto.ResolveVerificationMode(featureproto.VerificationDomainSpecs,
		"workspace", workspaceOptions, projectID, projectOptions)
	if err != nil {
		return featureproto.SpecVerificationGroup{}, protocolcli.Classify(err, protocolcli.ErrInvalidConfig)
	}
	group := featureproto.SpecVerificationGroup{
		Project:    projectID,
		Feature:    expected.Feature,
		Spec:       expected.Spec,
		Mode:       mode,
		ModeSource: source,
	}
	if mode == featureproto.VerificationModeOff {
		return group, nil
	}
	group.AutomaticEvaluation = true
	specRequirements := make([]featureproto.SpecRequirement, 0, len(expected.SpecRequirements))
	for _, id := range expected.SpecRequirements {
		specRequirements = append(specRequirements, featureproto.SpecRequirement{ID: id})
	}
	evaluations := featureproto.EvaluateSpecRequirements(expected.Feature, specRequirements,
		expected.Requirements, nil, repositoryEvaluationInstant())
	for _, evaluation := range evaluations {
		verdict := featureproto.SpecRequirementVerdict{Requirement: evaluation.Requirement, State: evaluation.State}
		if evaluation.Evaluation != nil {
			for _, check := range evaluation.Evaluation.Checks {
				verdict.Checks = append(verdict.Checks, featureproto.SpecCheckVerdict{
					Check: check.Check, State: check.State, Reason: check.Reason,
				})
			}
		}
		group.Requirements = append(group.Requirements, verdict)
	}
	group.Counts = featureproto.CountSpecVerdicts(group.Requirements)
	group.Blocked = featureproto.GroupBlocks(mode, group.Requirements, nil)
	return group, nil
}

// specOwnerPolicy resolves the spec-hosting project's identity and its
// committed options block. The workspace root hosts no project, so a
// workspace-rooted spec reports the workspace scope with no project override.
func specOwnerPolicy(ws *workspace.Workspace, repository *specRepository, feature string) (string, map[string]map[string]any) {
	discovered, found := repository.byFeature[feature]
	if !found || discovered.Root == "" {
		return "workspace", nil
	}
	project := ws.ProjectByID(workspace.ProjectIDFromPath(discovered.Root))
	if project == nil {
		project = ws.ProjectByName(discovered.Project)
	}
	if project == nil {
		return "workspace", nil
	}
	if project.Config == nil {
		return project.ID, nil
	}
	return project.ID, project.Config.Options
}

// repositoryEvaluationInstant is the zero instant: with no observations there
// is nothing rolling freshness could compare, and the pure evaluator treats
// the zero clock as absent by design.
func repositoryEvaluationInstant() (zero time.Time) { return zero }

// readRecordedVerification loads the persisted gate record for one session.
// An explicit session that does not exist or cannot be read is an error; an
// absent record under the implicit latest is only the fact that no gate ran.
func readRecordedVerification(wsRoot, requested string) (*featureproto.SpecVerificationRecord, string, []diag.Diagnostic) {
	explicit := requested != "" && requested != latestSessionAlias
	sessionID := requested
	if !explicit {
		resolved, err := os.Readlink(filepath.Join(wsRoot, filepath.FromSlash(sessionsDirectory), latestSessionAlias))
		if err != nil {
			return nil, "", nil
		}
		sessionID = filepath.Base(resolved)
	}
	if strings.ContainsAny(sessionID, `/\`) || sessionID == "." || sessionID == ".." {
		return nil, "", []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeInvalidPath, "session",
			"session id %q is not one contained path segment", sessionID)}
	}
	path := filepath.Join(wsRoot, filepath.FromSlash(sessionsDirectory), sessionID, featureproto.SpecVerificationRecordFilename)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > specVerifyMaxRecordBytes {
		if explicit {
			return nil, sessionID, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeParseError, "session",
				"session %s carries no readable spec verification record", sessionID)}
		}
		return nil, "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, sessionID, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeParseError, "session",
			"session %s carries no readable spec verification record", sessionID)}
	}
	record, findings := featureproto.ParseSpecVerificationRecord(data)
	if record == nil || diag.HasErrors(featureproto.ValidateSpecVerificationRecord(record)) {
		_ = findings
		return nil, sessionID, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeParseError, "session",
			"session %s carries an unreadable spec verification record", sessionID)}
	}
	if record.SessionID != "" && record.SessionID != sessionID {
		return nil, sessionID, []diag.Diagnostic{diag.Errorf(featureproto.ErrorCodeParseError, "session",
			"session %s carries a record claiming session %q", sessionID, record.SessionID)}
	}
	return record, sessionID, nil
}

func addCounts(total *featureproto.SpecVerificationCounts, group featureproto.SpecVerificationCounts) {
	total.SpecRequirements += group.SpecRequirements
	total.Executable += group.Executable
	total.Verified += group.Verified
	total.Unmapped += group.Unmapped
	total.Unexecutable += group.Unexecutable
	total.Missing += group.Missing
	total.Stale += group.Stale
	total.Contradicted += group.Contradicted
	total.Unobserved += group.Unobserved
}
