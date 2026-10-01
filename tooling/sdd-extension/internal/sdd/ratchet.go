package sdd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// The rollout ratchet of the executable-spec gate. The CURRENT
// enforce floor is derived from exactly the sources the gate itself judges —
// the committed manifests and specs through the one discovery pass, and the
// committed options through the one policy resolver — and compared against
// the committed specs.baseline.json files, one per enforced project, with the
// protocol's pure shrink-only rule. Both sides are worktree-only: no git
// history enters the verdict, so the validate-workspace task stays honestly
// cacheable, and "the reviewed policy change" is literally an edit to the
// project's committed baseline in the same diff.

// SpecsRatchetReport is the typed data of one ratchet judgment.
type SpecsRatchetReport struct {
	Revision featureengine.Revision `json:"revision"`
	// BaselinePresent reports whether any committed specs.baseline.json
	// exists; absence is initial adoption, never a failure.
	BaselinePresent bool `json:"baselinePresent"`
	// Current is the enforce floor the committed worktree states right now.
	Current *featureproto.SpecsBaseline `json:"current"`
	// Summary is the protocol's deterministic accounting.
	Summary featureproto.SpecsRatchetSummary `json:"summary"`
	// Diagnostics carries the shrink findings (errors) and the growth nudge
	// (warning).
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// SpecsBaselineReport is the typed data of `putnami specs baseline`.
type SpecsBaselineReport struct {
	Revision featureengine.Revision `json:"revision"`
	// Paths are the workspace-relative baselines the floor commits, one per
	// enforced project, sorted.
	Paths []string `json:"paths,omitempty"`
	// Removed are the committed baselines the floor no longer names, sorted:
	// --update deletes them.
	Removed []string `json:"removed,omitempty"`
	// Current is the enforce floor derived from the committed worktree — the
	// union of what --update writes.
	Current *featureproto.SpecsBaseline `json:"current"`
	// Updated reports whether --update rewrote the committed file.
	Updated bool `json:"updated"`
	// Changed reports whether a committed file differs from the derived
	// floor, is absent, or is no longer named by it.
	Changed     bool              `json:"changed"`
	Diagnostics []diag.Diagnostic `json:"diagnostics,omitempty"`
}

// deriveSpecsFloor states the CURRENT enforce floor: every spec-owning
// project whose effective specs mode is enforce, with the executable
// (feature#requirement) identities its criteria cover. It reuses the same
// discovery and the same policy resolution every other gate surface uses, so
// the ratchet can never disagree with the gate about what is enforced. A
// spec whose owner is not a resolvable workspace member draws a warning and
// stays out of the floor: the baseline records commitments by projects, and
// an unattributable owner cannot make one — baking the fallback scope in
// would fail the ratchet spuriously the day the project becomes resolvable.
func deriveSpecsFloor(ws *workspace.Workspace, repository *specRepository) (*featureproto.SpecsBaseline, []diag.Diagnostic, error) {
	projection := criteriaProjectionFromRepository(repository)
	perProject := make(map[string][]string)
	var workspaceOptions map[string]map[string]any
	if ws.Config != nil {
		workspaceOptions = ws.Config.Options
	}
	var findings []diag.Diagnostic
	modes := make(map[string]featureproto.VerificationMode)
	for _, group := range projection.Groups {
		projectID, projectOptions := specOwnerPolicy(ws, repository, group.Feature)
		if projectID == "workspace" {
			// specOwnerPolicy's fallback scope; real member IDs derive from
			// paths as "/<path>" and can never collide with it.
			findings = append(findings, diag.Warningf(featureproto.WarningCodeUnresolvedFeatureAuthority,
				group.Feature,
				"spec owner for feature %s is not a resolvable workspace member; the enforced floor leaves it out of %s",
				group.Feature, featureproto.SpecsBaselineFilename))
			continue
		}
		mode, resolved := modes[projectID]
		if !resolved {
			var err error
			mode, _, err = featureproto.ResolveVerificationMode(featureproto.VerificationDomainSpecs,
				"workspace", workspaceOptions, projectID, projectOptions)
			if err != nil {
				return nil, findings, protocolcli.Classify(err, protocolcli.ErrInvalidConfig)
			}
			modes[projectID] = mode
		}
		if mode != featureproto.VerificationModeEnforce {
			continue
		}
		for _, requirement := range group.Requirements {
			if requirement.Verification == nil {
				continue
			}
			perProject[projectID] = append(perProject[projectID],
				featureproto.SpecsBaselineRequirementIdentity(group.Feature, requirement.ID))
		}
		if _, exists := perProject[projectID]; !exists {
			// An enforced spec owner with no executable requirement yet still
			// holds a floor entry: leaving enforce is a policy change even
			// before the first criterion lands.
			perProject[projectID] = []string{}
		}
	}
	floor := &featureproto.SpecsBaseline{ProtocolVersion: featureproto.WorkspaceSpecsBaselineProtocolVersion}
	for id, requirements := range perProject {
		floor.Projects = append(floor.Projects, featureproto.SpecsBaselineProject{
			Project:                id,
			Mode:                   featureproto.VerificationModeEnforce,
			ExecutableRequirements: dedupeSorted(requirements),
		})
	}
	// CanonicalSpecsBaseline owns the byte contract's ordering; the map
	// iteration above needs none of its own.
	return featureproto.CanonicalSpecsBaseline(floor), findings, nil
}

// floorFromWorktree is the shared prologue of the two floor surfaces: load
// the repository once, copy its diagnostics, and derive the current floor.
// Contract errors come back as diagnostics with a nil floor and a nil error —
// each caller wraps them with its own typed report — while load and derive
// failures come back as the error itself.
func floorFromWorktree(ws *workspace.Workspace) (featureengine.Revision, []diag.Diagnostic, *featureproto.SpecsBaseline, error) {
	repository, err := loadSpecRepository(ws, Selection{})
	if err != nil {
		return featureengine.Revision{}, nil, nil, err
	}
	diagnostics := append([]diag.Diagnostic(nil), repository.diagnostics...)
	if diag.HasErrors(diagnostics) {
		return repository.revision, featureDiagnostics(diagnostics), nil, nil
	}
	floor, findings, err := deriveSpecsFloor(ws, repository)
	return repository.revision, append(diagnostics, findings...), floor, err
}

func dedupeSorted(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for i, value := range values {
		if i == 0 || value != values[i-1] {
			out = append(out, value)
		}
	}
	return out
}

// committedSpecsBaseline is the enforced floor the worktree commits: the
// union of every baseline file.
type committedSpecsBaseline struct {
	// floor is the merged canonical baseline, nil when no file exists.
	floor *featureproto.SpecsBaseline
	// present reports that at least one baseline file exists.
	present bool
	// misplaced are the projects the workspace-root file records although
	// they do not sit at the root: the single-file layout that predates one
	// baseline per project.
	misplaced []string
}

// specsBaselineScopes lists every directory a baseline may sit in, sorted —
// the workspace root, then every project directory — and maps each to the ID
// of the project it records. The root maps to "" unless a project sits there.
// A baseline anywhere else records nothing a reader can name, so it is not
// read.
func specsBaselineScopes(ws *workspace.Workspace) ([]string, map[string]string) {
	owners := map[string]string{"": ""}
	for _, project := range ws.Projects {
		if project == nil {
			continue
		}
		owners[workspace.CleanWorkspacePath(project.Path)] = project.ID
	}
	scopes := make([]string, 0, len(owners))
	for scope := range owners {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	return scopes, owners
}

// readCommittedSpecsBaselines loads and merges every committed baseline.
// A project's file is a project baseline: it names no project, because its
// directory does. A project recorded twice is refused, since a change could
// then weaken the floor through a file a reviewer does not expect to hold it.
// The workspace root may still hold the older workspace-wide document, so a
// workspace that commits it keeps its floor enforced until
// `putnami specs baseline --update` moves each entry out.
func readCommittedSpecsBaselines(ws *workspace.Workspace) (committedSpecsBaseline, []diag.Diagnostic) {
	scopes, owners := specsBaselineScopes(ws)
	var committed committedSpecsBaseline
	var diagnostics []diag.Diagnostic
	recordedIn := make(map[string]string)
	var projects []featureproto.SpecsBaselineProject
	record := func(file string, entry featureproto.SpecsBaselineProject) {
		if first, taken := recordedIn[entry.Project]; taken {
			diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeDuplicateSpec, file,
				"project %s is already recorded in %s; one project has one baseline", entry.Project, first))
			return
		}
		recordedIn[entry.Project] = file
		projects = append(projects, entry)
	}
	for _, scope := range scopes {
		file := featureproto.SpecsBaselinePath(scope)
		data, err := os.ReadFile(filepath.Join(ws.Root, filepath.FromSlash(file)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		committed.present = true
		if err != nil {
			diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeParseError,
				file, "read committed specs baseline: %v", err))
			continue
		}
		owner := owners[scope]
		if scope == "" && !featureproto.IsProjectSpecsBaseline(data) {
			floor, findings := featureproto.ParseAndValidateSpecsBaseline(data)
			diagnostics = append(diagnostics, anchorDiagnostics(file, findings)...)
			if floor == nil {
				continue
			}
			for _, entry := range floor.Projects {
				if entry.Project != owner {
					committed.misplaced = append(committed.misplaced, entry.Project)
				}
				record(file, entry)
			}
			continue
		}
		baseline, findings := featureproto.ParseAndValidateProjectSpecsBaseline(data)
		diagnostics = append(diagnostics, anchorDiagnostics(file, findings)...)
		if baseline == nil {
			continue
		}
		if owner == "" {
			diagnostics = append(diagnostics, diag.Errorf(featureproto.ErrorCodeInvalidPath, file,
				"%s is a project baseline, but no project sits at the workspace root", file))
			continue
		}
		record(file, featureproto.SpecsBaselineProject{
			Project:                owner,
			Mode:                   featureproto.VerificationModeEnforce,
			ExecutableRequirements: baseline.ExecutableRequirements,
		})
	}
	if committed.present {
		committed.floor = featureproto.CanonicalSpecsBaseline(&featureproto.SpecsBaseline{
			ProtocolVersion: featureproto.WorkspaceSpecsBaselineProtocolVersion,
			Projects:        append([]featureproto.SpecsBaselineProject{}, projects...),
		})
	}
	sort.Strings(committed.misplaced)
	return committed, diagnostics
}

// anchorDiagnostics anchors a document's own findings on its file.
func anchorDiagnostics(file string, findings []diag.Diagnostic) []diag.Diagnostic {
	anchored := make([]diag.Diagnostic, 0, len(findings))
	for _, finding := range findings {
		finding.Field = documentField(file, finding.Field)
		anchored = append(anchored, finding)
	}
	return anchored
}

// BuildSpecsRatchetResult is the whole of the workspace-scoped ratchet check:
// derive the current floor, read the committed one, and apply the protocol's
// shrink-only comparison. Shrink findings are errors and fail the task; a
// floor grown beyond the committed baseline draws one warning nudge, because
// raising the floor is desirable and deliberate, never forced.
func BuildSpecsRatchetResult(ws *workspace.Workspace) (SpecsRatchetReport, error) {
	revision, diagnostics, current, err := floorFromWorktree(ws)
	report := SpecsRatchetReport{Revision: revision, Diagnostics: diagnostics}
	if err != nil {
		return report, err
	}
	if current == nil {
		return report, specContractError("specs ratchet", report.Diagnostics, report)
	}
	report.Current = current

	committed, findings := readCommittedSpecsBaselines(ws)
	present := committed.floor != nil
	report.BaselinePresent = present
	report.Diagnostics = append(report.Diagnostics, findings...)
	if diag.HasErrors(findings) {
		return report, WithResultData(protocolcli.Classify(
			fmt.Errorf("specs ratchet: a committed %s is unreadable", featureproto.SpecsBaselineFilename),
			protocolcli.ErrInvalidConfig), report)
	}
	if len(committed.misplaced) > 0 {
		report.Diagnostics = append(report.Diagnostics, diag.Warningf(featureproto.WarningCodeMisplacedBaseline,
			featureproto.SpecsBaselineFilename,
			"the workspace-root %s records %d project(s) that belong in their own directory (%s); run `putnami specs baseline --update` to move each into its project's %s",
			featureproto.SpecsBaselineFilename, len(committed.misplaced), strings.Join(committed.misplaced, ", "),
			featureproto.SpecsBaselineFilename))
	}

	comparison, summary := featureproto.CompareSpecsBaseline(committed.floor, current)
	report.Summary = summary
	report.Diagnostics = append(report.Diagnostics, comparison...)
	switch {
	case !present && len(current.Projects) > 0:
		report.Diagnostics = append(report.Diagnostics, diag.Warningf(featureproto.WarningCodeRatchetGrowth,
			featureproto.SpecsBaselineFilename,
			"no committed %s records the enforced floor yet; run `putnami specs baseline --update` to adopt the ratchet",
			featureproto.SpecsBaselineFilename))
	case present && (summary.GrownProjects > 0 || summary.GrownRequirements > 0):
		report.Diagnostics = append(report.Diagnostics, diag.Warningf(featureproto.WarningCodeRatchetGrowth,
			featureproto.SpecsBaselineFilename,
			"the enforced floor grew beyond the committed baseline (%d project(s), %d requirement(s)); raise it with `putnami specs baseline --update`",
			summary.GrownProjects, summary.GrownRequirements))
	}
	if diag.HasErrors(comparison) {
		projects := make([]string, 0, len(comparison))
		for _, finding := range comparison {
			projects = append(projects, finding.Field)
		}
		return report, WithResultData(protocolcli.Classify(
			fmt.Errorf("specs ratchet: %d regression(s) against the committed %s files (%s)",
				len(comparison), featureproto.SpecsBaselineFilename, strings.Join(dedupeSorted(projects), ", ")),
			protocolcli.ErrInvalidConfig), report)
	}
	return report, nil
}

// BuildSpecsBaselineResult derives the current enforce floor and, with
// update, writes it as the committed canonical baselines: one project
// baseline in the directory of each enforced project. A
// committed baseline the floor no longer names — a project that left enforce,
// or the workspace-root file of the older single-file layout — is removed.
// Without update it only reports whether the committed files match — the
// read-only half a reviewer runs.
func BuildSpecsBaselineResult(ws *workspace.Workspace, update bool) (SpecsBaselineReport, error) {
	revision, diagnostics, current, err := floorFromWorktree(ws)
	report := SpecsBaselineReport{Revision: revision, Diagnostics: diagnostics}
	if err != nil {
		return report, err
	}
	if current == nil {
		return report, specContractError("specs baseline", report.Diagnostics, report)
	}
	report.Current = current

	desired := make(map[string][]byte, len(current.Projects))
	for _, project := range current.Projects {
		member := ws.ProjectByID(project.Project)
		if member == nil {
			return report, fmt.Errorf("specs baseline: enforced project %s is not a workspace member", project.Project)
		}
		file := featureproto.SpecsBaselinePath(workspace.CleanWorkspacePath(member.Path))
		data, err := featureproto.MarshalProjectSpecsBaseline(&featureproto.ProjectSpecsBaseline{
			ProtocolVersion:        featureproto.SpecsBaselineProtocolVersion,
			ExecutableRequirements: project.ExecutableRequirements,
		})
		if err != nil {
			return report, err
		}
		desired[file] = data
		report.Paths = append(report.Paths, file)
	}
	sort.Strings(report.Paths)

	var writes []string
	for _, file := range report.Paths {
		committed, readErr := os.ReadFile(filepath.Join(ws.Root, filepath.FromSlash(file)))
		if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
			// An unreadable committed file is a real failure, not "out of
			// date": reporting Changed here would send --update to overwrite a
			// file it never managed to read.
			return report, fmt.Errorf("read %s: %w", file, readErr)
		}
		if readErr != nil || !bytes.Equal(committed, desired[file]) {
			writes = append(writes, file)
		}
	}
	scopes, _ := specsBaselineScopes(ws)
	for _, scope := range scopes {
		file := featureproto.SpecsBaselinePath(scope)
		if _, wanted := desired[file]; wanted {
			continue
		}
		if _, err := os.Lstat(filepath.Join(ws.Root, filepath.FromSlash(file))); err == nil {
			report.Removed = append(report.Removed, file)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return report, fmt.Errorf("check %s: %w", file, err)
		}
	}
	report.Changed = len(writes) > 0 || len(report.Removed) > 0
	if !update || !report.Changed {
		return report, nil
	}
	for _, file := range writes {
		if err := atomicWriteFile(filepath.Join(ws.Root, filepath.FromSlash(file)), desired[file]); err != nil {
			return report, fmt.Errorf("write %s: %w", file, err)
		}
	}
	for _, file := range report.Removed {
		if err := os.Remove(filepath.Join(ws.Root, filepath.FromSlash(file))); err != nil {
			return report, fmt.Errorf("remove %s: %w", file, err)
		}
	}
	report.Updated = true
	return report, nil
}
