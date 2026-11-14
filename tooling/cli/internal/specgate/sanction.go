package specgate

import (
	"fmt"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/workspace"
	diag "go.putnami.dev/protocol/diagnostic"
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

// Sanction is the one synthetic failure the engine reports a spec-owning
// project under: a stable diagnostic code and its message.
type Sanction struct {
	// Code is features.ErrorCodeUnresolvedRequirement when enforce-mode
	// requirements did not resolve verified, and
	// features.ErrorCodeVerificationUnreadable when the criteria projection or
	// its observations could not be read.
	Code    string
	Message string
}

// Sanctions returns, per spec-owning project with at least one blocked group,
// the one synthetic failure the engine reports it under. Counts are summed
// across the project's blocked groups so a project with two specs fails once
// with one legible message — decision 9's "one failed synthetic result per
// spec-owning project".
func Sanctions(record *features.SpecVerificationRecord) map[string]Sanction {
	if record == nil {
		return nil
	}
	totals := make(map[string]features.SpecVerificationCounts)
	duplicated := make(map[string]int)
	findings := make(map[string][]string)
	for _, group := range record.Groups {
		if !group.Blocked {
			continue
		}
		duplicated[group.Project] += duplicatedRequirements(group.Requirements)
		findings[group.Project] = append(findings[group.Project], repeatedFindings(group)...)
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
	sanctions := make(map[string]Sanction, len(totals))
	for _, project := range projects {
		counts := totals[project]
		if counts.SpecRequirements == 0 {
			// A blocked group with no requirement rows means the inputs
			// themselves were unreadable; the group diagnostics carry the detail.
			sanctions[project] = Sanction{
				Code:    features.ErrorCodeVerificationUnreadable,
				Message: "spec verification: the criteria projection or its observations could not be read; run `putnami specs verify` for the audit report (docs: https://putnami.dev/docs/spec-driven-development/validation-jobs)",
			}
			continue
		}
		// Unobserved requirements are subtracted from the sanctioned count, not
		// hidden: they never caused this failure (GroupBlocks does not block on
		// them), so they are not counted among the blockers. They are still
		// stated, because the project is failing anyway and the reader needs the
		// whole picture.
		unresolved := counts.SpecRequirements - counts.Verified - counts.Unobserved
		sanctions[project] = Sanction{Code: features.ErrorCodeUnresolvedRequirement, Message: fmt.Sprintf(
			"spec verification: %d of %d textual requirement(s) unresolved under enforce (unmapped %d, unexecutable %d, missing %d%s, stale %d, contradicted %d)%s%s; run `putnami specs verify` for the audit report (docs: https://putnami.dev/docs/spec-driven-development/validation-jobs)",
			unresolved, counts.SpecRequirements, counts.Unmapped, counts.Unexecutable,
			counts.Missing-duplicated[project], duplicatedCount(duplicated[project]), counts.Stale, counts.Contradicted,
			findingsClause(findings[project]), unobservedClause(counts.Unobserved))}
	}
	return sanctions
}

// duplicatedRequirements counts the requirements whose only defect is a check
// two reports carry: the requirement is missing on the wire, and each of its
// checks is either satisfied or refused as a duplicate. Their proofs exist, one
// too many, so the failure does not count them among the proofs nobody wrote.
// A requirement that also has an unproven check stays counted as missing.
func duplicatedRequirements(verdicts []features.SpecRequirementVerdict) int {
	count := 0
	for _, verdict := range verdicts {
		if verdict.State != features.RequirementMissing {
			continue
		}
		if onlyDuplicated(verdict.Checks) {
			count++
		}
	}
	return count
}

func onlyDuplicated(checks []features.SpecCheckVerdict) bool {
	duplicate := false
	for _, check := range checks {
		switch {
		case check.State == features.CheckSatisfied:
		case check.Reason == features.ReasonDuplicateObservation:
			duplicate = true
		default:
			return false
		}
	}
	return duplicate
}

// repeatedFindings returns the group findings its failure repeats: each
// duplicated-check finding, which names the requirement, the check and the
// reporting projects, and each error finding, which blocks the group on its own
// whatever its requirement counts say.
func repeatedFindings(group features.SpecVerificationGroup) []string {
	var messages []string
	for _, finding := range group.Diagnostics {
		if finding.Code == features.ErrorCodeDuplicateObservation || finding.Severity == diag.Error {
			messages = append(messages, finding.Message)
		}
	}
	return messages
}

func duplicatedCount(duplicated int) string {
	if duplicated == 0 {
		return ""
	}
	return fmt.Sprintf(", duplicated %d", duplicated)
}

// repeatedFindingsLimit bounds how many findings one failure spells out. The
// record's group diagnostics carry every one, and the JSON audit report prints
// them; its human rendering does not print group diagnostics.
const repeatedFindingsLimit = 5

// findingsClause repeats the repeated findings in the failure, the one message
// every surface prints, in deterministic order.
func findingsClause(findings []string) string {
	if len(findings) == 0 {
		return ""
	}
	sort.Strings(findings)
	more := ""
	if len(findings) > repeatedFindingsLimit {
		more = fmt.Sprintf("; and %d more finding(s), in the group diagnostics of `putnami specs verify --output=json`",
			len(findings)-repeatedFindingsLimit)
		findings = findings[:repeatedFindingsLimit]
	}
	return ": " + strings.Join(findings, "; ") + more
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
			// A blocked group's repeated findings are in its sanction already.
			repeated := (finding.Code == features.ErrorCodeDuplicateObservation || finding.Severity == diag.Error) &&
				!group.Blocked
			if finding.Code != features.WarningCodeUnobservedRequirement && !repeated {
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
