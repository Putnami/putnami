package specgate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	diag "go.putnami.dev/protocol/diagnostic"
	features "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
)

// maxArtifactBytes bounds one artifact read. It matches the SDD engines' own
// contained-reader bound: a projection or report larger than this is a defect,
// not a scale requirement.
const maxArtifactBytes = 16 << 20

// Collect is the collection-and-join half of the spec gate: it reads the
// criteria projections and observation reports the session's jobs declared,
// joins them through the pure evaluator, and returns the verdict record —
// including each group's one Blocked decision — without touching exit status.
// Sanction stays with the engine's canonical session reducer.
//
// Everything read goes through the contained artifact path: a declared path is
// resolved under the session's own output tree, symlinks and irregular files
// are refused, sizes are bounded, and the exact bytes' SHA-256 is recorded in
// the report references. A projection or report that cannot be read safely
// becomes an error finding that fails closed under enforce; it is never
// skipped into a silently green gate.
//
// A session in which NO job declared a criteria projection returns a record
// with no group when every selected project a projection producer applies to
// ran one that succeeded: the gate ran, and no selected project states a
// requirement. It returns (nil, nil) when a producer applies to a selected
// project the session did not run it for, as in a session that runs a
// verification command without the criteria command that projects, or when a
// producer failed or never finished: the gate had nothing to evaluate, which
// is not the same fact as evaluating and finding nothing. A projection that
// was declared but could not be read still produces a record, because that is
// a finding that must fail closed.
//
// recovery is the unplanned-attester seam (recover.go). It is consulted
// LAZILY — only for a group the in-session join left unresolved — so a green
// run pays nothing for it, and it may be nil, which disables recovery.
//
// producers is consulted only when nothing was projected. It may be nil, and
// then such a session records nothing.
func Collect(ws *workspace.Workspace, planned []*jobs.ScheduledJob, results map[string]*jobs.JobResult,
	at time.Time, recovery ObservationRecovery, producers ProjectionProducers) (*features.SpecVerificationRecord, error) {
	if ws == nil {
		return nil, fmt.Errorf("spec gate collection requires a loaded workspace")
	}
	record := &features.SpecVerificationRecord{
		ProtocolVersion: features.SpecVerificationRecordProtocolVersion,
		GeneratedAt:     at.UTC().Format(time.RFC3339),
		Groups:          []features.SpecVerificationGroup{},
	}

	ordered := orderedJobs(planned, results)
	observations, reportsByFeature, reporters, reportDiagnostics := collectObservations(ws.Root, ordered, results)
	record.Diagnostics = append(record.Diagnostics, reportDiagnostics...)
	// The candidate set is derived from the FULL plan, not from the jobs that
	// produced results: a project whose test was planned owns its own silence,
	// whether or not the task got as far as reporting.
	evidence := newEvidenceSource(ws, planned, recovery)

	// projected records whether the session carried a criteria projection at
	// all, which is the one signal that separates "the gate ran" from "the gate
	// was never asked". It is set even when the projection turned out to be
	// unreadable, because that path reports a finding of its own.
	projected := false
	seenProjects := make(map[string]bool)
	for _, job := range ordered {
		projection, found, diagnostics := readProjection(ws.Root, job, results[job.Key()])
		if !found {
			continue
		}
		projected = true
		projectID := job.Project.ID
		if seenProjects[projectID] {
			record.Diagnostics = append(record.Diagnostics, diag.Warningf(features.ErrorCodeDuplicateSpec, projectID,
				"a second criteria projection for project %s was rejected (task %s)", projectID, job.Key()))
			continue
		}
		seenProjects[projectID] = true

		mode, source, err := EffectiveMode(features.VerificationDomainSpecs, ws, ws.ProjectByID(projectID))
		if err != nil {
			return nil, err
		}
		if projection == nil {
			group := features.SpecVerificationGroup{
				Project:             projectID,
				Mode:                mode,
				ModeSource:          source,
				AutomaticEvaluation: mode != features.VerificationModeOff,
				Diagnostics:         diagnostics,
			}
			group.Blocked = features.GroupBlocks(mode, nil, group.Diagnostics)
			record.Groups = append(record.Groups, group)
			continue
		}
		owned := make(map[string]bool, len(projection.Groups))
		for _, criteria := range projection.Groups {
			owned[criteria.Feature] = true
		}
		for _, criteria := range projection.Groups {
			record.Groups = append(record.Groups, evaluateGroup(projectID, criteria, mode, source,
				observations, reportsByFeature, reporters, at, evidence, owned))
		}
	}
	// Nothing was projected and nothing was diagnosed. The record is empty when
	// every selected project a producer applies to ran one that succeeded,
	// because each of them then answered that its project states no
	// requirement. Otherwise there is no record, which the persistence seam
	// contracts as "the gate did not run".
	if !projected && len(record.Diagnostics) == 0 && !producersAnswered(planned, results, producers) {
		return nil, nil
	}
	sortRecord(record)
	return record, nil
}

// evaluateGroup joins one spec's criteria with the session's observations, and
// — only when that join leaves something unresolved — with the observations a
// dependency's cache entry can still supply (recover.go). owned is the set of
// features the project's projection declares, which decides the projects that
// name one of them as their feature authority.
func evaluateGroup(projectID string, criteria features.SpecCriteriaGroup, mode features.VerificationMode,
	source features.VerificationModeSource, observations []features.VerificationObservation,
	reportsByFeature map[string][]features.VerificationReportRef, reporters checkReporters, at time.Time,
	evidence *evidenceSource, owned map[string]bool) features.SpecVerificationGroup {
	group := features.SpecVerificationGroup{
		Project:    projectID,
		Feature:    criteria.Feature,
		Spec:       criteria.Spec,
		Target:     criteria.Target,
		Mode:       mode,
		ModeSource: source,
	}
	if mode == features.VerificationModeOff {
		// Off collects and evaluates nothing automatically; the group is listed
		// so the audit surface can say so instead of showing an absence.
		group.AutomaticEvaluation = false
		return group
	}
	group.AutomaticEvaluation = true

	specRequirements := make([]features.SpecRequirement, 0, len(criteria.SpecRequirements))
	for _, id := range criteria.SpecRequirements {
		specRequirements = append(specRequirements, features.SpecRequirement{ID: id})
	}
	verdicts, unexpected := evaluateVerdicts(criteria, specRequirements, observations, at)
	group.Reports = reportsByFeature[criteria.Feature]

	// The lazy trigger. Everything below runs only for a group the ordinary
	// in-session join could not close, which is what keeps a fully green run
	// free of any extra planning, keying or hashing.
	if evidence != nil && unresolvedInSession(verdicts) {
		recovered := evidence.forProject(projectID, owned)
		group.Diagnostics = append(group.Diagnostics, recovered.diagnostics...)
		group.Unconsulted = unconsultedCandidates(recovered.unconsulted)
		if len(recovered.observations) > 0 {
			// One re-evaluation over the UNION, never a merge of two verdicts: a
			// restored observation must be able to contradict as well as support,
			// and only the one pure evaluator decides that.
			joined := make([]features.VerificationObservation, 0, len(observations)+len(recovered.observations))
			joined = append(joined, observations...)
			joined = append(joined, recovered.observations...)
			verdicts, unexpected = evaluateVerdicts(criteria, specRequirements, joined, at)
			reporters = reporters.with(recovered.reporters)
			group.Reports = append(group.Reports, recovered.reportsByFeature[criteria.Feature]...)
		}
		if moved := reclassifyUnobserved(verdicts, recovered.unconsulted); moved > 0 {
			group.Diagnostics = append(group.Diagnostics,
				unobservedDiagnostic(projectID, criteria.Feature, criteria.Spec, moved, recovered.unconsulted))
		} else if stalled := len(unobservedRequirements(verdicts)); stalled > 0 {
			// Every source answered and still nobody reported the check. The
			// verdict stays blocking — that is a requirement no test proves —
			// but the reader is told the gate looked everywhere, instead of
			// being left with a bare "check-not-observed".
			group.Diagnostics = append(group.Diagnostics, exhaustedScopeDiagnostic(
				projectID, criteria.Feature, criteria.Spec, stalled, recovered.consulted()))
		}
	}

	group.Diagnostics = append(group.Diagnostics, duplicateDiagnostics(criteria, verdicts, reporters)...)
	group.Diagnostics = append(group.Diagnostics, unexpected...)
	group.Requirements = verdicts
	group.Counts = features.CountSpecVerdicts(verdicts)
	group.Blocked = features.GroupBlocks(mode, verdicts, group.Diagnostics)
	return group
}

// evaluateVerdicts is the one projection of the pure evaluator onto the record's
// verdict rows. It is a function rather than inline code because the group may
// be evaluated twice — once on the session's own observations, once on those
// plus what the cache recovered — and both passes must project identically.
func evaluateVerdicts(criteria features.SpecCriteriaGroup, specRequirements []features.SpecRequirement,
	observations []features.VerificationObservation, at time.Time) ([]features.SpecRequirementVerdict, []diag.Diagnostic) {
	evaluations := features.EvaluateSpecRequirements(criteria.Feature, specRequirements,
		criteria.Requirements, observations, at)
	verdicts := make([]features.SpecRequirementVerdict, 0, len(evaluations))
	var unexpected []diag.Diagnostic
	for _, evaluation := range evaluations {
		verdict := features.SpecRequirementVerdict{Requirement: evaluation.Requirement, State: evaluation.State}
		if evaluation.Evaluation != nil {
			for _, check := range evaluation.Evaluation.Checks {
				checkVerdict := features.SpecCheckVerdict{
					Check:       check.Check,
					State:       check.State,
					Reason:      check.Reason,
					Measurement: check.Measurement,
				}
				if check.Provenance != nil {
					checkVerdict.Path, checkVerdict.Symbol = check.Provenance.Path, check.Provenance.Symbol
				}
				verdict.Checks = append(verdict.Checks, checkVerdict)
			}
			for _, undeclared := range evaluation.Evaluation.Unexpected {
				unexpected = append(unexpected, diag.Warningf(features.ErrorCodeInvalidObservation,
					criteria.Feature,
					"observation for undeclared check %q on requirement %q grants no support",
					undeclared.Check, evaluation.Requirement))
			}
		}
		verdicts = append(verdicts, verdict)
	}
	return verdicts, unexpected
}

// checkReporters names, per observed check, the projects whose reports carry
// it, in the order the reports were read. An observation states no project, so
// the collector records the one it read each from: that is the only place a
// duplicated check can be traced back to the proofs to remove.
type checkReporters map[string][]string

func checkKey(feature, requirement, check string) string {
	return feature + "\x00" + requirement + "\x00" + check
}

func (reporters checkReporters) add(observation features.VerificationObservation, project string) {
	key := checkKey(observation.Feature, observation.Requirement, observation.Check)
	reporters[key] = append(reporters[key], project)
}

// with returns the union of two indexes without changing either: the session's
// index is shared by every group, and only the group that joined recovered
// observations may see their reporters.
func (reporters checkReporters) with(other checkReporters) checkReporters {
	if len(other) == 0 {
		return reporters
	}
	union := make(checkReporters, len(reporters)+len(other))
	for key, projects := range reporters {
		union[key] = append([]string(nil), projects...)
	}
	for key, projects := range other {
		union[key] = append(union[key], projects...)
	}
	return union
}

// duplicateDiagnostics is the diagnosis of a check two reports carry. The pure
// evaluator refuses to prefer one report and leaves the requirement missing
// with reason duplicate-observation; alone, that reads as a proof nobody wrote.
// The finding names the feature, the requirement, the check and the reporting
// projects, so the author knows which proof to remove.
//
// It is a warning, not an error, for the reason exhaustedScopeDiagnostic gives:
// the requirement's own state already blocks the group under enforce, and an
// error here would block it a second time and change what report mode does.
func duplicateDiagnostics(criteria features.SpecCriteriaGroup, verdicts []features.SpecRequirementVerdict,
	reporters checkReporters) []diag.Diagnostic {
	var diagnostics []diag.Diagnostic
	for _, verdict := range verdicts {
		for _, check := range verdict.Checks {
			if check.Reason != features.ReasonDuplicateObservation {
				continue
			}
			projects := reporters[checkKey(criteria.Feature, verdict.Requirement, check.Check)]
			diagnostics = append(diagnostics, diag.Warningf(features.ErrorCodeDuplicateObservation, criteria.Spec,
				"check %q of requirement %q of feature %q is reported %d times, by %s: "+
					"a check has one proof, so keep one spectest.Proves call for it and remove the others",
				check.Check, verdict.Requirement, criteria.Feature, len(projects), reportingProjects(projects)))
		}
	}
	return diagnostics
}

// reportingProjects renders the reporters of one check: each project once, in
// sorted order, and a project that reported it twice says so, because two
// report tasks of one project are then the duplicate.
func reportingProjects(projects []string) string {
	if len(projects) == 0 {
		return "reports this run could not attribute to a project"
	}
	times := make(map[string]int, len(projects))
	for _, project := range projects {
		times[project]++
	}
	names := make([]string, 0, len(times))
	for project := range times {
		names = append(names, project)
	}
	sort.Strings(names)
	for i, project := range names {
		if times[project] > 1 {
			names[i] = fmt.Sprintf("%s (%d reports)", project, times[project])
		}
	}
	return joinBounded(names)
}

// collectObservations reads every observation report the session's report
// producers emitted (jobs.ProducesVerificationReport): the jobs of its
// verification commands that can write the report. Observations survive only
// with trusted provenance: the declaration they name must be a regular,
// contained file of the REPORTING task's own project, so a report can never
// claim another project's source as its proof.
func collectObservations(wsRoot string, ordered []*jobs.ScheduledJob, results map[string]*jobs.JobResult) (
	[]features.VerificationObservation, map[string][]features.VerificationReportRef, checkReporters, []diag.Diagnostic) {
	var observations []features.VerificationObservation
	var diagnostics []diag.Diagnostic
	reportsByFeature := make(map[string][]features.VerificationReportRef)
	reporters := make(checkReporters)
	readPaths := make(map[string]bool)

	for _, job := range ordered {
		if !jobs.ProducesVerificationReport(job) {
			continue
		}
		result := results[job.Key()]
		for _, artifact := range jobs.TaskResultOf(job, result).Artifacts {
			if artifact.ID != features.VerificationReportArtifactID {
				continue
			}
			resolved, findings := resolveArtifact(wsRoot, job, artifact.Path)
			diagnostics = append(diagnostics, findings...)
			if resolved == "" || readPaths[resolved] {
				continue
			}
			readPaths[resolved] = true
			data, digest, findings := readContained(wsRoot, resolved)
			diagnostics = append(diagnostics, findings...)
			if data == nil {
				continue
			}
			report, parseFindings := features.ParseAndValidateVerificationReport(data)
			if report == nil || diag.HasErrors(parseFindings) {
				diagnostics = append(diagnostics, diag.Errorf(features.ErrorCodeInvalidObservation, artifactField(wsRoot, resolved),
					"verification report from task %s failed its strict reader", job.Key()))
				continue
			}
			reference := features.VerificationReportRef{
				Task:    job.Key(),
				Project: job.Project.ID,
				Path:    artifactField(wsRoot, resolved),
				Digest:  digest,
			}
			seenFeature := make(map[string]bool)
			for _, observation := range report.Observations {
				if !provenanceIsContained(wsRoot, job.Project.Path, observation.Provenance.Path) {
					diagnostics = append(diagnostics, diag.Warningf(features.ErrorCodeInvalidPath, reference.Path,
						"observation (%s, %s, %s) names provenance %q that is not a regular file of project %s; dropped",
						observation.Feature, observation.Requirement, observation.Check,
						observation.Provenance.Path, job.Project.ID))
					continue
				}
				observations = append(observations, observation)
				reporters.add(observation, job.Project.ID)
				if !seenFeature[observation.Feature] {
					seenFeature[observation.Feature] = true
					reportsByFeature[observation.Feature] = append(reportsByFeature[observation.Feature], reference)
				}
			}
		}
	}
	return observations, reportsByFeature, reporters, diagnostics
}

// readProjection finds and reads one job's declared criteria projection.
// found is false when the job declares none; a declared projection that cannot
// be read returns found=true with a nil projection and error findings, so the
// caller fails closed instead of treating the project as gate-free.
func readProjection(wsRoot string, job *jobs.ScheduledJob, result *jobs.JobResult) (
	*features.SpecCriteriaProjection, bool, []diag.Diagnostic) {
	var diagnostics []diag.Diagnostic
	for _, artifact := range jobs.TaskResultOf(job, result).Artifacts {
		if artifact.ID != features.SpecCriteriaProjectionArtifactID {
			continue
		}
		resolved, findings := resolveArtifact(wsRoot, job, artifact.Path)
		diagnostics = append(diagnostics, findings...)
		if resolved == "" {
			return nil, true, diagnostics
		}
		data, _, findings := readContained(wsRoot, resolved)
		diagnostics = append(diagnostics, findings...)
		if data == nil {
			return nil, true, diagnostics
		}
		projection, parseFindings := features.ParseAndValidateSpecCriteriaProjection(data)
		if projection == nil || diag.HasErrors(parseFindings) {
			diagnostics = append(diagnostics, diag.Errorf(features.ErrorCodeInvalidVerification, artifactField(wsRoot, resolved),
				"criteria projection from task %s failed its strict reader", job.Key()))
			return nil, true, diagnostics
		}
		return projection, true, diagnostics
	}
	return nil, false, diagnostics
}

// resolveArtifact turns a declared artifact path into an absolute location
// inside the session's own output tree. Event paths are output-relative for a
// solo task and workspace-relative in a batch member's normalized form; both
// are accepted, and anything resolving outside .putnami/out is refused.
func resolveArtifact(wsRoot string, job *jobs.ScheduledJob, artifactPath string) (string, []diag.Diagnostic) {
	if artifactPath == "" || strings.Contains(artifactPath, "\\") || filepath.IsAbs(artifactPath) {
		return "", []diag.Diagnostic{diag.Errorf(features.ErrorCodeInvalidPath, job.Key(),
			"artifact path %q is not a relative slash path", artifactPath)}
	}
	outputRoot := filepath.Join(wsRoot, ".putnami", "out")
	candidates := []string{
		filepath.Join(wsproto.CommandOutputDir(wsRoot, job.Project.Path, job.CommandName()), filepath.FromSlash(artifactPath)),
	}
	if strings.HasPrefix(artifactPath, ".putnami/out/") {
		candidates = append(candidates, filepath.Join(wsRoot, filepath.FromSlash(artifactPath)))
	}
	for _, candidate := range candidates {
		cleaned := filepath.Clean(candidate)
		if !pathWithin(outputRoot, cleaned) {
			return "", []diag.Diagnostic{diag.Errorf(features.ErrorCodePathEscape, job.Key(),
				"artifact path %q escapes the session output tree", artifactPath)}
		}
		// Any existing entry resolves; readContained owns the regular-file and
		// symlink discipline so a planted link is refused loudly, not skipped
		// into a quieter "not found".
		if _, err := os.Lstat(cleaned); err == nil {
			return cleaned, nil
		}
	}
	return "", []diag.Diagnostic{diag.Errorf(features.ErrorCodeInvalidPath, job.Key(),
		"declared artifact %q was not found under the session output tree", artifactPath)}
}

// readContained reads one resolved artifact with the bounded, symlink-refusing
// discipline and records the exact content digest of the bytes evaluated.
func readContained(wsRoot, path string) ([]byte, string, []diag.Diagnostic) {
	field := artifactField(wsRoot, path)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, "", []diag.Diagnostic{diag.Errorf(features.ErrorCodeInvalidPath, field, "artifact is unreadable")}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, "", []diag.Diagnostic{diag.Errorf(features.ErrorCodeSymlinkEscape, field, "artifact is a symlink, which is never followed")}
	}
	if !info.Mode().IsRegular() {
		return nil, "", []diag.Diagnostic{diag.Errorf(features.ErrorCodeInvalidPath, field, "artifact is not a regular file")}
	}
	if info.Size() > maxArtifactBytes {
		return nil, "", []diag.Diagnostic{diag.Errorf(features.ErrorCodeSensitiveContent, field, "artifact exceeds the bounded read size")}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", []diag.Diagnostic{diag.Errorf(features.ErrorCodeInvalidPath, field, "artifact is unreadable")}
	}
	sum := sha256.Sum256(data)
	return data, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// provenanceIsContained reports whether a project-relative provenance path is
// a regular, non-symlink file inside the reporting project's root. The lexical
// half is already guaranteed by the report's strict reader; this is the
// filesystem half a wire contract cannot see.
func provenanceIsContained(wsRoot, projectPath, provenance string) bool {
	if provenance == "" {
		return false
	}
	projectRoot := filepath.Join(wsRoot, filepath.FromSlash(projectPath))
	resolved := filepath.Clean(filepath.Join(projectRoot, filepath.FromSlash(provenance)))
	if !pathWithin(projectRoot, resolved) {
		return false
	}
	info, err := os.Lstat(resolved)
	return err == nil && info.Mode().IsRegular()
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// artifactField renders one absolute artifact location as the
// workspace-relative slash path diagnostics and report references use.
func artifactField(wsRoot, path string) string {
	relative, err := filepath.Rel(wsRoot, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(relative)
}

// ProjectionProducers is the engine-owned seam that names the selected
// projects a criteria-projection producer applies to. The planner decides it,
// and this package does not import the planner.
type ProjectionProducers interface {
	// ApplicableProjects returns the IDs of the session's selected projects
	// for which a plan of the criteria commands that declare the projection
	// holds a criteria-projection producer, whether or not the session planned
	// them.
	ApplicableProjects() (map[string]bool, error)
}

// producersAnswered reports whether every planned criteria-projection producer
// succeeded and every selected project a producer applies to ran one. A
// producer that succeeded without emitting a projection states that its
// project has no requirement; a failed, canceled, skipped or missing result
// states nothing, and neither does a producer the session did not plan.
func producersAnswered(planned []*jobs.ScheduledJob, results map[string]*jobs.JobResult,
	producers ProjectionProducers) bool {
	if producers == nil {
		return false
	}
	answered := make(map[string]bool)
	for _, job := range planned {
		if job == nil || job.Project == nil || !jobs.ProducesSpecCriteriaProjection(job) {
			continue
		}
		result := results[job.Key()]
		if result == nil || result.Status != string(jobs.TaskStatusSuccess) {
			return false
		}
		answered[job.Project.ID] = true
	}
	applicable, err := producers.ApplicableProjects()
	if err != nil {
		return false
	}
	for projectID := range applicable {
		if !answered[projectID] {
			return false
		}
	}
	return true
}

// orderedJobs returns the session's jobs with results, sorted by key, so two
// runs over the same session decide in the same order.
func orderedJobs(planned []*jobs.ScheduledJob, results map[string]*jobs.JobResult) []*jobs.ScheduledJob {
	ordered := make([]*jobs.ScheduledJob, 0, len(planned))
	for _, job := range planned {
		if job == nil || job.Project == nil || results[job.Key()] == nil {
			continue
		}
		ordered = append(ordered, job)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key() < ordered[j].Key() })
	return ordered
}

func sortRecord(record *features.SpecVerificationRecord) {
	canonical := features.CanonicalSpecVerificationRecord(record)
	*record = *canonical
}
