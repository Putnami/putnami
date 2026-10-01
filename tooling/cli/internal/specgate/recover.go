package specgate

import (
	"fmt"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	diag "go.putnami.dev/protocol/diagnostic"
	features "go.putnami.dev/protocol/features"
)

// The unplanned-attester half of the gate.
//
// A spec-owning project's requirements are attested by `spectest.Proves` calls
// that live wherever the code they protect lives — routinely NOT in the owner
// itself. Two shapes occur, and both are real in this repository: a library the
// owner depends on (a workload's requirement proven by its core library), and a
// direct dependent that implements the owner's contract
// (`architecture/executable-contracts` is owned by protocols/architecture and
// proven by tooling/sdd-extension; `go/api-contracts` is owned by
// go/framework/api and proven by openapi, proto and grpc). Under a narrowed
// selection (`--impacted`, `--projects`) the planner schedules neither shape's
// `test~test`, so no observation report exists for them and the owner's
// requirements read as check-not-observed even though nothing regressed.
//
// The fix does NOT widen the plan and does NOT add a second authored
// declaration of who attests what. It reads the observation the attesting
// project's own `test~test` cache entry already contains, at the key the current
// inputs derive:
//
//   - the attester changed → the narrowed selection already includes it → its
//     test runs → a FRESH observation joins in-session;
//   - the attester did not change → its key did not move → the entry it
//     published last time is the entry for exactly these inputs → restoring
//     the observation states the same fact the run would have re-derived.
//
// It is also not a new trust assumption. jobs.restoreReservedDeclaredArtifactEvents
// already reconstructs this exact artifact from a warm hit when the project
// happens to be selected, on the stated grounds that "losing either on a warm
// hit can fail closed on valid evidence". This extends the same admission to a
// project the selection did not name.
//
// LAYERING. This package stays the policy half: it decides WHEN to ask (only
// after an ordinary in-session join left something unresolved — a fully green
// run pays nothing), WHICH projects may be asked (the owner's transitive
// dependency closure plus its DIRECT dependents — see candidates for why those
// two radii differ), and what the answer means. HOW to turn a project into
// cache bytes — plan, key, look the entry up — belongs to the engine, which
// already owns the run's planner, its command params and its one memoized cache
// manager.

// ObservationRecovery is the engine-owned half: given candidate attesting
// projects, locate each one's captured verification report inside its own
// task-owned cache entry, without restoring anything into the working tree.
//
// A nil ObservationRecovery disables recovery entirely; every unresolved check
// then falls to the unobserved classification below, which warns.
type ObservationRecovery interface {
	// RecoverReports answers for every candidate exactly once. Candidates are
	// passed in canonical project-ID order and the result must be deterministic:
	// this decides a gate verdict, so two runs over the same store must produce
	// the same answer.
	RecoverReports(candidates []*workspace.Project) Recovery
}

// RecoveredReport locates one candidate project's captured verification report.
// Path is ABSOLUTE and points inside the cache store, never into the session
// output tree — the bytes are read in place and nothing is materialized.
type RecoveredReport struct {
	// Project is the candidate project whose task published the entry. It is
	// the project the report's provenance must be contained in.
	Project string
	// Task is the cached task's canonical key, recorded so the audit surface
	// can name the producer that never ran in this session.
	Task string
	// Path is the absolute location of the captured report bytes.
	Path string
	// Field is the rendered, workspace-relative form of Path used in report
	// references and diagnostics.
	Field string
}

// UnconsultedProject is one candidate no observation source could be reached
// for at all: its test was not planned in this session AND no cache entry
// answered for it. Reason is short human prose naming why.
type UnconsultedProject struct {
	Project string
	Reason  string
}

// Recovery is one lookup pass's complete answer.
type Recovery struct {
	Reports     []RecoveredReport
	Unconsulted []UnconsultedProject
	Diagnostics []diag.Diagnostic
}

// evidenceSource resolves, per spec-owning project and at most once, which
// unplanned dependencies could be consulted and what they reported.
type evidenceSource struct {
	ws *workspace.Workspace
	// planned holds the project IDs whose verification-report producer WAS
	// planned in this session. Their absence of an observation is a genuine
	// miss, never an unconsulted source, so they are excluded from the
	// candidate set.
	planned  map[string]bool
	recovery ObservationRecovery
	resolved map[string]*recoveredEvidence
}

// recoveredEvidence is what one spec-owning project's closure yielded.
type recoveredEvidence struct {
	observations     []features.VerificationObservation
	reportsByFeature map[string][]features.VerificationReportRef
	unconsulted      []UnconsultedProject
	diagnostics      []diag.Diagnostic
	// candidates is how many projects could have been asked at all. With
	// unconsulted it says how much of the evidence scope actually answered,
	// which is what separates "nobody could tell us" from "everybody told us
	// nothing".
	candidates int
}

// consulted is the number of candidate attesters that answered.
func (evidence *recoveredEvidence) consulted() int {
	if evidence == nil {
		return 0
	}
	return evidence.candidates - len(evidence.unconsulted)
}

func newEvidenceSource(ws *workspace.Workspace, planned []*jobs.ScheduledJob, recovery ObservationRecovery) *evidenceSource {
	if ws == nil || recovery == nil {
		return nil
	}
	// A project's tests ran only when its report producer ran. The command name
	// is not that fact: a narrowed run plans every dependency's
	// `test~generate` and `test~config-merge` as prerequisites, and counting
	// those dropped every dependency from the candidate set.
	tested := make(map[string]bool)
	for _, job := range planned {
		if job != nil && job.Project != nil && jobs.ProducesVerificationReport(job) {
			tested[job.Project.ID] = true
		}
	}
	return &evidenceSource{ws: ws, planned: tested, recovery: recovery,
		resolved: make(map[string]*recoveredEvidence)}
}

// forProject returns the recovered evidence for one spec-owning project,
// computing it on first use. Two specs of the same project share one lookup.
func (source *evidenceSource) forProject(projectID string) *recoveredEvidence {
	if source == nil {
		return nil
	}
	if cached, found := source.resolved[projectID]; found {
		return cached
	}
	evidence := source.resolve(projectID)
	source.resolved[projectID] = evidence
	return evidence
}

func (source *evidenceSource) resolve(projectID string) *recoveredEvidence {
	evidence := &recoveredEvidence{reportsByFeature: map[string][]features.VerificationReportRef{}}
	candidates := source.candidates(projectID)
	evidence.candidates = len(candidates)
	if len(candidates) == 0 {
		return evidence
	}
	answer := source.recovery.RecoverReports(candidates)
	evidence.diagnostics = append(evidence.diagnostics, answer.Diagnostics...)
	evidence.unconsulted = append(evidence.unconsulted, answer.Unconsulted...)
	sort.Slice(evidence.unconsulted, func(i, j int) bool {
		return evidence.unconsulted[i].Project < evidence.unconsulted[j].Project
	})

	for _, report := range answer.Reports {
		candidate := source.ws.ProjectByID(report.Project)
		if candidate == nil {
			continue
		}
		// The same bounded, symlink-refusing reader and the same content digest
		// the in-session path uses. A restored report is evidence exactly to the
		// degree a session-produced one is, so it goes through one reader.
		data, digest, findings := readContained(source.ws.Root, report.Path)
		evidence.diagnostics = append(evidence.diagnostics, findings...)
		if data == nil {
			continue
		}
		parsed, parseFindings := features.ParseAndValidateVerificationReport(data)
		if parsed == nil || diag.HasErrors(parseFindings) {
			evidence.diagnostics = append(evidence.diagnostics, diag.Errorf(features.ErrorCodeInvalidObservation,
				report.Field, "restored verification report of task %s failed its strict reader", report.Task))
			continue
		}
		reference := features.VerificationReportRef{
			Task: report.Task, Project: candidate.ID, Path: report.Field, Digest: digest, Restored: true,
		}
		seenFeature := make(map[string]bool)
		for _, observation := range parsed.Observations {
			// Provenance containment is NOT relaxed for a restored report: the
			// declaration it names must still be a regular file of the CANDIDATE
			// project, so a cached report can no more claim another project's
			// source as its proof than a fresh one can.
			if !provenanceIsContained(source.ws.Root, candidate.Path, observation.Provenance.Path) {
				evidence.diagnostics = append(evidence.diagnostics, diag.Warningf(features.ErrorCodeInvalidPath,
					reference.Path,
					"restored observation (%s, %s, %s) names provenance %q that is not a regular file of project %s; dropped",
					observation.Feature, observation.Requirement, observation.Check,
					observation.Provenance.Path, candidate.ID))
				continue
			}
			evidence.observations = append(evidence.observations, observation)
			if !seenFeature[observation.Feature] {
				seenFeature[observation.Feature] = true
				evidence.reportsByFeature[observation.Feature] =
					append(evidence.reportsByFeature[observation.Feature], reference)
			}
		}
	}
	return evidence
}

// candidates is the closed set of projects that may be asked for a spec-owning
// project's missing evidence: its TRANSITIVE dependency closure plus its DIRECT
// dependents, minus every project whose report producer this session already
// planned (see jobs.ProducesVerificationReport).
//
// The asymmetry is the whole rule, and it is not a compromise between two
// arbitrary radii — the two directions attest for two different reasons.
//
// A DEPENDENCY can attest because the owner's own tests exercise it: a
// `spectest.Proves` call can only observe code that runs, and the code a
// project's tests run is its own plus what it depends on. Depth is unbounded
// there because the closure is what one test process actually executes.
//
// A DIRECT DEPENDENT can attest because it is the concrete PRODUCER of the
// artifact the owner's contract describes. A contract project owns the rule and
// the wire; the project one hop above it owns an implementation, and its tests
// are where the rule is exercised against a real producer. That is the shape of
// every cross-project attestation in this repository:
// `go/api-contracts` (owned by go/framework/api) is proven by openapi, proto and
// grpc; `architecture/executable-contracts` (owned by protocols/architecture) is
// proven by tooling/sdd-extension. One hop is where the pattern lives.
//
// Beyond one hop the dependent relationship is incidental — a consumer of a
// consumer exercises the contract only through the layer between them, which is
// already a candidate — and the set explodes: the transitive dependents of a
// protocol module in this workspace are very nearly the whole workspace, while
// its direct dependents are bounded (31 for the widest protocol here, 5 for the
// widest framework library). Keying a bounded set on a path that only runs when
// the gate is already unresolved is affordable; keying the workspace is the cost
// `--impacted` exists to avoid.
func (source *evidenceSource) candidates(projectID string) []*workspace.Project {
	if source.ws.Graph == nil {
		return nil
	}
	seen := make(map[string]bool)
	candidates := make([]*workspace.Project, 0, 8)
	add := func(id string) {
		if seen[id] || source.planned[id] {
			return
		}
		seen[id] = true
		if project := source.ws.ProjectByID(id); project != nil {
			candidates = append(candidates, project)
		}
	}
	for _, id := range source.ws.Graph.TransitiveDependenciesOf([]string{projectID}) {
		add(id)
	}
	// DependentsOf is the DIRECT reverse edge and may repeat an id when a
	// project declares the dependency more than once; add dedupes.
	for _, id := range source.ws.Graph.DependentsOf(projectID) {
		add(id)
	}
	// The engine's lookup order — and therefore its diagnostics and the report
	// references it produces — must not depend on graph insertion order.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	return candidates
}

// unresolvedInSession reports whether any verdict still needs evidence. It is
// the LAZY trigger: a run whose in-session join resolved everything never
// reaches the cache, never plans a second time and never hashes a second tree.
func unresolvedInSession(verdicts []features.SpecRequirementVerdict) bool {
	for _, verdict := range verdicts {
		if verdict.State != features.RequirementVerified {
			return true
		}
	}
	return false
}

// reclassifyUnobserved downgrades the requirements whose only defect is that no
// observation source existed for them, and reports how many it moved.
//
// The rule is deliberately narrow on the CHECK side and deliberately
// conservative on the PROJECT side.
//
// On checks: a requirement qualifies only when every one of its checks is
// either satisfied or missing-because-never-observed. A check reported skipped,
// failed, stale, duplicated, or in a shape the criterion cannot read rests on an
// observation that DID arrive, so it keeps blocking. That is what makes "a
// genuinely failing or skipped attesting check still blocks" hold by
// construction rather than by care.
//
// On projects: the gate cannot know WHICH project attests WHICH check without
// an observation naming it — that knowledge lives in the `spectest.Proves` call
// in committed source, and reading it here would be the duplicated declaration
// this design exists to avoid. So the trigger is the honest one: if some
// candidate attester could not be consulted at all, the run has no basis to
// call a still-missing check a regression. When every candidate WAS consulted —
// the ordinary warm case, and every un-narrowed run — the set is empty and the
// gate keeps its full strength.
//
// An EMPTY candidate set therefore blocks, deliberately. This is not the
// false-positive case it might look like. Candidates are empty only when
// every project that could possibly attest — the owner itself, its dependency
// closure, its direct dependents — already had its report producer planned in
// THIS session. A prerequisite of the same command is not one. The evidence
// scope was exhausted, every source spoke, and none of them reported the
// check: that is a requirement with no test, which is exactly what the gate
// exists to sanction. Excusing it here would turn "nobody wrote the test"
// into a warning on the one run that is best placed to prove it. The DX gap
// this creates is answered instead by saying so — exhaustedScopeDiagnostic
// below states that the gate looked everywhere, so a reader is never left
// with a bare "check-not-observed" and no idea what to do about it.
func reclassifyUnobserved(verdicts []features.SpecRequirementVerdict, unconsulted []UnconsultedProject) int {
	if len(unconsulted) == 0 {
		return 0
	}
	indexes := unobservedRequirements(verdicts)
	for _, index := range indexes {
		verdicts[index].State = features.RequirementUnobserved
	}
	return len(indexes)
}

// unobservedRequirements returns the indexes of the requirements whose ONLY
// defect is a declared check nothing reported. It is the shared predicate: the
// same rows are excused when a source was unreachable and named as an
// exhausted-scope gap when every source answered.
func unobservedRequirements(verdicts []features.SpecRequirementVerdict) []int {
	var indexes []int
	for i := range verdicts {
		if verdicts[i].State != features.RequirementMissing {
			continue
		}
		if !onlyUnobservedChecks(verdicts[i].Checks) {
			continue
		}
		indexes = append(indexes, i)
	}
	return indexes
}

func onlyUnobservedChecks(checks []features.SpecCheckVerdict) bool {
	unobserved := false
	for _, check := range checks {
		switch {
		case check.State == features.CheckSatisfied:
		case check.State == features.CheckMissing && check.Reason == features.ReasonCheckNotObserved:
			unobserved = true
		default:
			return false
		}
	}
	return unobserved
}

// unobservedDiagnostic is the DX deliverable: the message that replaces a bare
// "check-not-observed" with the projects whose tests could have attested the
// requirement, why none of them was consulted, and what to do next.
//
// Reasons are GROUPED rather than repeated per project. A protocol module in
// this workspace has around thirty candidate attesters and they usually share
// one cause (`--no-cache`, a cold store); printing that cause thirty times
// buries the project names, which are the only part a reader acts on.
func unobservedDiagnostic(projectID, feature, spec string, moved int, unconsulted []UnconsultedProject) diag.Diagnostic {
	return diag.Warningf(features.WarningCodeUnobservedRequirement, spec,
		"%d requirement(s) of feature %q have checks that NO observation source reported. "+
			"A test can attest them from %s itself, from a project it depends on, or from a "+
			"direct dependent that implements its contract; none of the %d project(s) that "+
			"could have answered was consulted — %s. "+
			"Unresolvable evidence is warned, never sanctioned, so this did not fail the run. "+
			"To decide these requirements, %s",
		moved, feature, projectID, len(unconsulted), groupedReasons(unconsulted),
		resolutionAdvice(projectID, unconsulted))
}

// groupedReasons renders the unconsulted set as one clause per distinct cause,
// in deterministic reason order, with each cause's project names bounded.
func groupedReasons(unconsulted []UnconsultedProject) string {
	byReason := make(map[string][]string)
	for _, entry := range unconsulted {
		byReason[entry.Reason] = append(byReason[entry.Reason], entry.Project)
	}
	reasons := make([]string, 0, len(byReason))
	for reason := range byReason {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	clauses := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		clauses = append(clauses, reason+": "+joinBounded(byReason[reason]))
	}
	return strings.Join(clauses, "; ")
}

// resolutionAdvice names the exact command when the candidate set is small
// enough to spell out, and otherwise refuses to guess.
//
// Printing the first eight candidates as a command would be worse than useless:
// they are the alphabetically first, not the ones that attest anything, so the
// reader would run a long selection that still resolves nothing. When the set is
// too wide, point at the surface that NAMES the unproven checks with their
// declaration sites instead.
func resolutionAdvice(projectID string, unconsulted []UnconsultedProject) string {
	if len(unconsulted) > unobservedNamesLimit {
		return "run `putnami specs verify` to see which checks are unproven, then select " +
			projectID + " together with the project whose tests declare them — or re-run without --no-cache."
	}
	selection := make([]string, 0, len(unconsulted)+1)
	selection = append(selection, projectID)
	for _, entry := range unconsulted {
		selection = append(selection, entry.Project)
	}
	return "run `putnami test,validate --projects " + strings.Join(selection, ",") + "`."
}

// exhaustedScopeDiagnostic answers the other half of the DX complaint: a check
// that was never observed even though EVERY project that could attest it was
// consulted. The verdict stays blocking — that is a requirement with no test —
// but the message says the gate looked everywhere, so the reader knows the fix
// is to write the attestation, not to widen the selection.
//
// It is a warning, not an error: the group is already blocked by the missing
// requirement itself, and an error finding here would make GroupBlocks return
// true for a second, redundant reason.
func exhaustedScopeDiagnostic(projectID, feature, spec string, unresolved, consulted int) diag.Diagnostic {
	return diag.Warningf(features.WarningCodeUnobservedRequirement, spec,
		"%d requirement(s) of feature %q have checks that were never observed, and every "+
			"project that could attest them was in this run: %s plus %d project(s) of its "+
			"dependency closure and direct dependents. The evidence scope is exhausted, so "+
			"this is a requirement no test proves yet — add a spectest.Proves call for its "+
			"declared check(s) rather than widening the selection.",
		unresolved, feature, projectID, consulted)
}

// unobservedNamesLimit bounds how many project names one clause spells out. A
// protocol module here has around thirty candidate attesters; a diagnostic that
// prints all of them is not read.
const unobservedNamesLimit = 8

// joinBounded renders a name list, capped, with an explicit count of the rest —
// "…" alone hides whether two were dropped or two hundred.
func joinBounded(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	if len(names) <= unobservedNamesLimit {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(names[:unobservedNamesLimit], ", "), len(names)-unobservedNamesLimit)
}
