package specgate

import (
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/cli/model/extension"
	"go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/cli/model/workspace"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
)

// EvidenceVerdictTask is the synthetic task a project's evidence verdict is
// reported under: <project>:evidence~verdict.
const EvidenceVerdictTask = "evidence~verdict"

// maxListedFindings bounds the findings one sanction message names; the
// diagnostics list every one.
const maxListedFindings = 5

// Finding is one finding fragment a task of the session wrote, located in the
// workspace and stamped with the project that owns its path.
type Finding struct {
	// Project is the canonical id of the project that owns Path, or "" when
	// no project does.
	Project string
	// Path is the workspace-relative slash path of the subject.
	Path string
	// Task is the key of the job whose output held the fragment.
	Task string
	// Fragment is the fragment as its reader decoded it.
	Fragment evidence.Fragment
}

// Refusal is a fragment file the reader refused: the task that wrote it, the
// project it ran for, and why.
type Refusal struct {
	Project string
	Task    string
	// File is the workspace-relative slash path of the refused file.
	File        string
	Diagnostics []diag.Diagnostic
}

// CollectFindings reads the fragment file of every task of the session that
// declares one and succeeded, whether it ran or was restored from the cache,
// and returns its finding fragments located in the workspace (ADR 0134).
//
// The declaration decides, on both paths: a task states evidence by declaring
// putnami-evidence.jsonl as a required file of its command output, which every
// execution and every restore replaces whole, so the file read is always this
// session's. A declared file that is missing, or that the reader refuses, is a
// refusal of the task, never an empty file. So is a putnami-evidence artifact
// that a task reports without declaring the file.
func CollectFindings(ws *workspace.Workspace, planned []*jobs.ScheduledJob, results map[string]*jobs.JobResult) ([]Finding, []Refusal) {
	if ws == nil {
		return nil, nil
	}
	owners := newProjectOwners(ws)
	var findings []Finding
	var refusals []Refusal
	refuse := func(job *jobs.ScheduledJob, diagnostics []diag.Diagnostic) {
		refusals = append(refusals, Refusal{Project: job.Project.ID, Task: job.Key(), File: evidenceFileOf(ws.Root, job), Diagnostics: diagnostics})
	}
	for _, job := range planned {
		if job == nil || job.Project == nil {
			continue
		}
		result := results[job.Key()]
		declared, required := declaresEvidence(job)
		switch {
		case !declared:
			if reportsEvidenceArtifact(result) {
				refuse(job, []diag.Diagnostic{diag.Errorf(evidence.ErrorCodeInvalidFragment, job.Key(),
					"the task reports the %s artifact but does not declare %s in its command output", evidence.FragmentArtifactID, evidence.FragmentFilename)})
			}
			continue
		case !required:
			refuse(job, []diag.Diagnostic{diag.Errorf(evidence.ErrorCodeInvalidFragment, job.Key(),
				"the task declares %s as optional; a task that states evidence writes the file on every run, empty when it finds nothing", evidence.FragmentFilename)})
			continue
		case result == nil || result.Status != "success":
			// A task that did not succeed this session fails the run on its
			// own, and its file may be an earlier run's.
			continue
		}
		fragments, diagnostics := readFragments(ws.Root, job)
		if diagnostics != nil {
			refuse(job, diagnostics)
			continue
		}
		for _, fragment := range fragments {
			if fragment.Kind != evidence.FragmentKindFinding {
				continue
			}
			located := path.Join(slashProjectPath(job.Project.Path), fragment.Subject.Path)
			findings = append(findings, Finding{
				Project:  owners.owner(located),
				Path:     located,
				Task:     job.Key(),
				Fragment: fragment,
			})
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Fragment.Subject.Line < findings[j].Fragment.Subject.Line
	})
	return findings, refusals
}

// declaresEvidence reports whether job's declaration names the reserved
// fragment file in its command output, and whether it declares it required.
func declaresEvidence(job *jobs.ScheduledJob) (declared, required bool) {
	declaration := jobs.TaskDeclarationOf(job)
	if declaration == nil {
		return false, false
	}
	for _, output := range declaration.Outputs {
		if output.EffectiveRoot() == extension.OutputRootCommandOutput && output.Kind == extension.OutputKindFile &&
			output.PathFrom == "" && output.Path == evidence.FragmentFilename {
			return true, !output.OptionalEmpty
		}
	}
	return false, false
}

// reportsEvidenceArtifact reports whether result carries the reserved
// fragment artifact.
func reportsEvidenceArtifact(result *jobs.JobResult) bool {
	for _, artifact := range jobs.TaskArtifactsOf(result) {
		if artifact.ID == evidence.FragmentArtifactID {
			return true
		}
	}
	return false
}

// evidenceFileOf is the workspace-relative slash path of job's fragment file.
func evidenceFileOf(wsRoot string, job *jobs.ScheduledJob) string {
	file := filepath.Join(wsproto.CommandOutputDir(wsRoot, job.Project.Path, job.CommandName()), evidence.FragmentFilename)
	if rel, err := filepath.Rel(wsRoot, file); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(file)
}

// readFragments reads job's fragment file with the gate's bounded, contained
// reader and parses it strictly.
func readFragments(wsRoot string, job *jobs.ScheduledJob) ([]evidence.Fragment, []diag.Diagnostic) {
	resolved, diagnostics := resolveArtifact(wsRoot, job, evidence.FragmentFilename)
	if resolved == "" {
		return nil, diagnostics
	}
	data, _, diagnostics := readContained(wsRoot, resolved)
	if data == nil {
		return nil, diagnostics
	}
	return evidence.ParseFragments(data)
}

// EvidenceSanction is one project's failed evidence verdict.
type EvidenceSanction struct {
	Message     string
	Diagnostics []jobs.TaskDiagnostic
}

// EvidenceVerdict decides what the run's findings fail, under its selection:
// every project of an unscoped run, the selected projects of a narrowed one,
// and, under any selection, a finding no project owns and a fragment file the
// reader refused. It returns the sanction of each
// failing project, keyed by its id ("" for findings no project owns), and the
// warnings of the counted projects, in a deterministic order.
func EvidenceVerdict(findings []Finding, refusals []Refusal, selection *protocoljob.Selection) (map[string]EvidenceSanction, []string) {
	counts := func(project string) bool {
		if project == "" || selection == nil || !selection.Scoped {
			return true
		}
		for _, id := range selection.ProjectIDs {
			if id == project {
				return true
			}
		}
		return false
	}
	errorsByProject := map[string][]Finding{}
	var warnings []string
	for _, finding := range findings {
		if !counts(finding.Project) {
			continue
		}
		switch finding.Fragment.Severity {
		case evidence.FindingError:
			errorsByProject[finding.Project] = append(errorsByProject[finding.Project], finding)
		case evidence.FindingWarning:
			warnings = append(warnings, fmt.Sprintf("%s: %s (%s)", location(finding), finding.Fragment.Message, finding.Fragment.Rule))
		}
	}
	sanctions := map[string]EvidenceSanction{}
	for project, list := range errorsByProject {
		sanction := EvidenceSanction{}
		lines := make([]string, 0, maxListedFindings)
		for index, finding := range list {
			if index < maxListedFindings {
				lines = append(lines, fmt.Sprintf("%s: %s (%s)", location(finding), finding.Fragment.Message, finding.Fragment.Rule))
			}
			sanction.Diagnostics = append(sanction.Diagnostics, jobs.TaskDiagnostic{
				Project:  project,
				Job:      finding.Task,
				Severity: "error",
				Code:     finding.Fragment.Rule,
				Message:  finding.Fragment.Message,
				File:     finding.Path,
				Line:     finding.Fragment.Subject.Line,
				Column:   finding.Fragment.Subject.Column,
			})
		}
		if len(list) > maxListedFindings {
			lines = append(lines, fmt.Sprintf("and %d more", len(list)-maxListedFindings))
		}
		sanction.Message = fmt.Sprintf("%d error finding(s): %s", len(list), strings.Join(lines, "; "))
		sanctions[project] = sanction
	}
	// A refused file fails whatever the selection: it may hold any project's
	// findings, so no selection can say it is not concerned.
	for _, refusal := range refusals {
		sanction := sanctions[refusal.Project]
		for _, d := range refusal.Diagnostics {
			sanction.Diagnostics = append(sanction.Diagnostics, jobs.TaskDiagnostic{
				Project: refusal.Project, Job: refusal.Task, Severity: "error", Code: d.Code, Message: d.Field + ": " + d.Message, File: refusal.File,
			})
		}
		refused := fmt.Sprintf("the evidence of %s was refused: %s", refusal.Task, refusal.Diagnostics[0].Message)
		if sanction.Message == "" {
			sanction.Message = refused
		} else {
			sanction.Message += "; " + refused
		}
		sanctions[refusal.Project] = sanction
	}
	sort.Strings(warnings)
	return sanctions, warnings
}

// location renders a finding's place: path, then line and column when known.
func location(finding Finding) string {
	at := finding.Path
	if finding.Fragment.Subject.Line > 0 {
		at += fmt.Sprintf(":%d", finding.Fragment.Subject.Line)
		if finding.Fragment.Subject.Column > 0 {
			at += fmt.Sprintf(":%d", finding.Fragment.Subject.Column)
		}
	}
	return at
}

// projectOwners answers which project owns a workspace path: the project
// whose directory is the longest prefix of it. The workspace root project, if
// the workspace declares one, owns what no other project does.
type projectOwners struct {
	byPath []ownedDir
}

type ownedDir struct {
	dir string
	id  string
}

func newProjectOwners(ws *workspace.Workspace) projectOwners {
	owners := projectOwners{}
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		owners.byPath = append(owners.byPath, ownedDir{dir: slashProjectPath(project.Path), id: project.ID})
	}
	// The longest directory first, and the root, which owns every path, last.
	depth := func(dir string) int {
		if dir == "." {
			return 0
		}
		return len(dir)
	}
	sort.SliceStable(owners.byPath, func(i, j int) bool { return depth(owners.byPath[i].dir) > depth(owners.byPath[j].dir) })
	return owners
}

func (o projectOwners) owner(rel string) string {
	for _, candidate := range o.byPath {
		if candidate.dir == "." || rel == candidate.dir || strings.HasPrefix(rel, candidate.dir+"/") {
			return candidate.id
		}
	}
	return ""
}

// slashProjectPath is a project path in slash form, "." for the root.
func slashProjectPath(projectPath string) string {
	cleaned := path.Clean(strings.ReplaceAll(projectPath, "\\", "/"))
	if cleaned == "" || cleaned == "/" {
		return "."
	}
	return cleaned
}

// EvidenceVerdictFinalizer is the session finalizer of the evidence verdict
// (CLI ADR 0134). After the session it reads the finding fragments every
// result carries, run or restored from the cache, and decides under the run's
// selection: each counted project with an error finding fails as
// <project>:evidence~verdict, and the counted projects' warnings go to warn.
// The selection enters no task's key, so one cache entry serves every
// selection.
func EvidenceVerdictFinalizer(ws *workspace.Workspace, planned []*jobs.ScheduledJob, selection *protocoljob.Selection, warn io.Writer) func(map[string]*jobs.JobResult) {
	return func(results map[string]*jobs.JobResult) {
		findings, refusals := CollectFindings(ws, planned, results)
		if len(findings) == 0 && len(refusals) == 0 {
			return
		}
		sanctions, warnings := EvidenceVerdict(findings, refusals, selection)
		for _, warning := range warnings {
			_, _ = fmt.Fprintf(warn, "putnami: evidence: %s\n", warning)
		}
		// A finding no project owns is reported under "workspace", which a
		// workspace task's own sanction may also be: the two merge, in a
		// deterministic order, rather than one replacing the other.
		projects := make([]string, 0, len(sanctions))
		for project := range sanctions {
			projects = append(projects, project)
		}
		sort.Strings(projects)
		for _, project := range projects {
			sanction := sanctions[project]
			owner := project
			if owner == "" {
				owner = "workspace"
			}
			key := owner + ":" + EvidenceVerdictTask
			if earlier, ok := results[key]; ok && earlier.Error != nil && earlier.Canonical != nil {
				sanction.Message = earlier.Error.Message + "; " + sanction.Message
				sanction.Diagnostics = append(append([]jobs.TaskDiagnostic(nil), earlier.Canonical.Diagnostics...), sanction.Diagnostics...)
			}
			results[key] = &jobs.JobResult{
				Status:    "failed",
				Error:     &jobs.JobError{Message: sanction.Message},
				Canonical: &jobs.TaskResult{Diagnostics: sanction.Diagnostics},
			}
		}
	}
}
