package specgate

import (
	"fmt"
	"sort"

	"go.putnami.dev/cli/model/workspace"
	features "go.putnami.dev/protocol/features"
)

// GateActive reports whether any committed policy turns the spec gate on: the
// workspace's effective specs mode, or at least one project's override, is
// not off. Under off everywhere the collector is never attached — off means
// "do not collect or evaluate automatically", not "collect and stay quiet".
//
// ValidatePolicy has already refused unreadable blocks before jobs ran, so a
// decode error here resolves to active: the gate fails closed toward
// evaluation, never silently off.
func GateActive(ws *workspace.Workspace) bool {
	if ws == nil {
		return false
	}
	mode, _, err := EffectiveMode(features.VerificationDomainSpecs, ws, nil)
	if err != nil || mode != features.VerificationModeOff {
		return true
	}
	for _, project := range ws.Projects {
		if project == nil || project.Config == nil {
			continue
		}
		policy, err := features.DecodeVerificationPolicy(projectScope(project), project.Config.Options)
		if err != nil {
			return true
		}
		if override, exists := policy[features.VerificationDomainSpecs]; exists && override != features.VerificationModeOff {
			return true
		}
	}
	return false
}

// SanctionMessages returns, per spec-owning project with at least one blocked
// group, the one synthetic-failure message the engine reports it under, in
// deterministic project order. Counts are summed across the project's blocked
// groups so a project with two specs fails once with one legible message —
// decision 9's "one failed synthetic result per spec-owning project".
func SanctionMessages(record *features.SpecVerificationRecord) map[string]string {
	if record == nil {
		return nil
	}
	totals := make(map[string]features.SpecVerificationCounts)
	for _, group := range record.Groups {
		if !group.Blocked {
			continue
		}
		counts := totals[group.Project]
		counts.SpecRequirements += group.Counts.SpecRequirements
		counts.Executable += group.Counts.Executable
		counts.Verified += group.Counts.Verified
		counts.Unmapped += group.Counts.Unmapped
		counts.Unexecutable += group.Counts.Unexecutable
		counts.Missing += group.Counts.Missing
		counts.Stale += group.Counts.Stale
		counts.Contradicted += group.Counts.Contradicted
		counts.Unobserved += group.Counts.Unobserved
		totals[group.Project] = counts
	}
	projects := make([]string, 0, len(totals))
	for project := range totals {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	messages := make(map[string]string, len(totals))
	for _, project := range projects {
		counts := totals[project]
		if counts.SpecRequirements == 0 {
			// A blocked group with no requirement rows means the inputs
			// themselves were unreadable; the group diagnostics carry the detail.
			messages[project] = "spec verification: the criteria projection or its observations could not be read; run `putnami specs verify` for the audit report (docs: https://putnami.dev/docs/spec-driven-development/validation-jobs)"
			continue
		}
		// Unobserved requirements are subtracted from the sanctioned count, not
		// hidden: they never caused this failure (GroupBlocks does not block on
		// them), so counting them among the blockers would send a reader looking
		// for a regression that is not there. They are still stated, because the
		// project is failing anyway and the reader needs the whole picture.
		unresolved := counts.SpecRequirements - counts.Verified - counts.Unobserved
		messages[project] = fmt.Sprintf(
			"spec verification: %d of %d textual requirement(s) unresolved under enforce (unmapped %d, unexecutable %d, missing %d, stale %d, contradicted %d)%s; run `putnami specs verify` for the audit report (docs: https://putnami.dev/docs/spec-driven-development/validation-jobs)",
			unresolved, counts.SpecRequirements, counts.Unmapped, counts.Unexecutable,
			counts.Missing, counts.Stale, counts.Contradicted, unobservedClause(counts.Unobserved))
	}
	return messages
}

func unobservedClause(unobserved int) string {
	if unobserved == 0 {
		return ""
	}
	return fmt.Sprintf(
		"; a further %d had no observation source in this run's scope and were warned, not sanctioned", unobserved)
}

// WarningMessages returns, per spec-owning project, the non-blocking findings
// the run must still SAY out loud, in deterministic project order.
//
// It exists because a warning nobody sees is not a warning: the gate prints
// only sanctions, so a group whose requirements were left unresolvable would
// otherwise pass in complete silence and the next reader would believe the
// requirement was verified. The text names the projects whose tests would have
// attested the checks and why they were not consulted — see
// unobservedDiagnostic, which composes it at collection time so the persisted
// record and the terminal say exactly the same thing.
func WarningMessages(record *features.SpecVerificationRecord) map[string][]string {
	if record == nil {
		return nil
	}
	messages := make(map[string][]string)
	for _, group := range record.Groups {
		for _, finding := range group.Diagnostics {
			if finding.Code != features.WarningCodeUnobservedRequirement {
				continue
			}
			messages[group.Project] = append(messages[group.Project], finding.Message)
		}
	}
	for project := range messages {
		sort.Strings(messages[project])
	}
	return messages
}
