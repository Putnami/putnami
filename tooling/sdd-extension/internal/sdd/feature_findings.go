package sdd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	featureproto "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	pctx "go.putnami.dev/sdk/extension/context"
	featureengine "go.putnami.dev/tooling/sdd/extension/internal/features"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

// FeatureFindingsFormat is the source format of the finding fragments the
// feature validation states.
const FeatureFindingsFormat = "putnami-sdd"

// FeatureFindingsReport is the result data of the workspace feature
// validation. The findings themselves are the fragments the task writes; the
// report only counts them, so no diagnostic of a project reaches the task's
// own result.
type FeatureFindingsReport struct {
	// Revision names the worktree and no commit.
	Revision featureengine.Revision `json:"revision"`
	// Projects are the ids of the projects whose features were validated,
	// sorted.
	Projects []string `json:"projects"`
	// Summary counts the distinct features, requirements and evidence the
	// evaluations assessed.
	Summary FeatureCounts `json:"summary"`
	// Findings counts the stated findings by severity.
	Findings FeatureDiagnosticCounts `json:"findings"`
}

// BuildFeatureFindingsResult validates the features of every project of ws
// that `validate` activates on (featureValidatedProjects), each exactly as
// BuildFeatureTaskValidationResult validates a selection of that one project,
// and states what each evaluation reports as finding fragments located in the
// workspace (featureFindingSubject). The fragments are deduplicated and
// sorted, so the same tree gives the same bytes, and they hold every
// severity: what fails a run is the caller's policy, never this function's.
//
// It reads no selection. The evaluations run on up to workers goroutines, at
// least one, and share one reader of the candidate cut, whose reads it
// memoizes, and one parse of each authored document, so a file every
// evaluation reads is read and parsed once. Each evaluation's answer is merged
// in project order, so the bytes do not depend on workers. The error is a
// failure to read the cut, never a finding.
func BuildFeatureFindingsResult(ws *workspace.Workspace, extensionReferences []string, workers int) (FeatureFindingsReport, []evidence.Fragment, error) {
	if ws == nil {
		return FeatureFindingsReport{}, nil, protocolcli.Classify(
			errors.New("workspace is required to evaluate features"),
			protocolcli.ErrInvalidConfig,
		)
	}
	cut, err := featureengine.NewTaskReaderFrom(ws.Tree, ws.Root)
	if err != nil {
		return FeatureFindingsReport{}, nil, fmt.Errorf("features validate: %w", err)
	}
	reader := newMemoReader(cut)
	documents := featureengine.NewDocumentCache()
	view := atTreeBaseVersion(ws)
	report := FeatureFindingsReport{
		Revision: featureengine.Revision{Kind: featureengine.RevisionKindWorktree},
		Projects: []string{},
	}
	projectRoots := make(map[string]bool, len(view.Projects))
	for _, project := range view.Projects {
		if project != nil {
			projectRoots[project.Path] = true
		}
	}
	projects := featureValidatedProjects(view, reader, extensionReferences)
	results := evaluateEachProject(view, reader, documents, projects, workers)

	features := map[string]featureengine.FeatureAssessment{}
	unclassified := map[string]featureengine.ContributionAssessment{}
	stated := map[string]evidence.Fragment{}
	state := func(fragment evidence.Fragment) error {
		line, err := json.Marshal(fragment)
		if err != nil {
			return fmt.Errorf("features validate: encode a finding: %w", err)
		}
		stated[string(line)] = fragment
		return nil
	}
	var duplicates []diag.Diagnostic
	for index, project := range projects {
		result := results[index]
		if result.Snapshot == nil && !diag.HasErrors(result.Diagnostics) {
			return FeatureFindingsReport{}, nil, protocolcli.Classify(
				fmt.Errorf("features validate: the evaluation of %s returned no snapshot and no error", project.ID),
				protocolcli.ErrInvalidConfig,
			)
		}
		report.Projects = append(report.Projects, project.ID)
		if result.Snapshot != nil {
			for _, feature := range result.Snapshot.Features {
				features[feature.Source+"\x00"+feature.ID] = feature
			}
			for _, contribution := range result.Snapshot.Unclassified {
				identity := contribution.Identity
				unclassified[identity.OwnerProject+"\x00"+string(identity.Kind)+"\x00"+identity.Subkind+"\x00"+identity.Key] = contribution
			}
		}
		for _, finding := range result.Diagnostics {
			if err := evaluationInputFailure(project, finding); err != nil {
				return FeatureFindingsReport{}, nil, err
			}
			if finding.Code == featureproto.ErrorCodeDuplicateFeature {
				duplicates = append(duplicates, finding)
			}
			if err := state(featureFinding(finding, project, projectRoots)); err != nil {
				return FeatureFindingsReport{}, nil, err
			}
		}
	}
	// A duplicate feature id names its later declaration; it is stated at every
	// declaration too, so it counts for each project that declares the id.
	if len(duplicates) > 0 {
		for _, fragment := range duplicateDeclarations(view, reader, duplicates) {
			if err := state(fragment); err != nil {
				return FeatureFindingsReport{}, nil, err
			}
		}
	}
	fragments := make([]evidence.Fragment, 0, len(stated))
	for _, fragment := range stated {
		fragments = append(fragments, fragment)
		switch fragment.Severity {
		case evidence.FindingError:
			report.Findings.Errors++
		case evidence.FindingWarning:
			report.Findings.Warnings++
		default:
			report.Findings.Info++
		}
	}
	sort.Slice(fragments, func(i, j int) bool { return lessFinding(fragments[i], fragments[j]) })
	merged := &featureengine.Snapshot{}
	for _, feature := range features {
		merged.Features = append(merged.Features, feature)
	}
	for _, contribution := range unclassified {
		merged.Unclassified = append(merged.Unclassified, contribution)
	}
	report.Summary = summarizeFeatures(merged)
	return report, fragments, nil
}

// evaluateEachProject runs the evaluation of each project, a selection of that
// one project, on up to workers goroutines, and returns the results in the
// order of projects.
func evaluateEachProject(view *workspace.Workspace, reader featureengine.Reader, documents *featureengine.DocumentCache, projects []*workspace.Project, workers int) []featureengine.Result {
	results := make([]featureengine.Result, len(projects))
	workers = max(1, min(workers, len(projects)))
	next := make(chan int)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range next {
				selection := Selection{Mode: pctx.SelectionModeProjects, Scoped: true, ProjectIDs: []string{projects[index].ID}}
				results[index], _ = evaluateFeatureTask(view, reader, documents, selection)
			}
		}()
	}
	for index := range projects {
		next <- index
	}
	close(next)
	wait.Wait()
	return results
}

// featureValidatedProjects returns the projects of ws whose features the
// workspace validation evaluates, in id order: the projects `validate`
// activates on. A project qualifies when its root holds putnami.features.json,
// a features/*.json document or a specs/*.json document in the candidate cut,
// and its resolved extensions or direct dependencies name this extension by
// one of references.
func featureValidatedProjects(ws *workspace.Workspace, reader featureengine.Reader, references []string) []*workspace.Project {
	var projects []*workspace.Project
	for _, project := range ws.Projects {
		if project == nil || !namesExtension(project, references) || !authorsSDDDocuments(reader, project.Path) {
			continue
		}
		projects = append(projects, project)
	}
	sort.SliceStable(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return projects
}

// namesExtension reports whether project's resolved extensions or direct
// dependencies name one of references.
func namesExtension(project *workspace.Project, references []string) bool {
	for _, reference := range append(append([]string(nil), project.Extensions...), project.Dependencies...) {
		if reference != "" && slices.Contains(references, reference) {
			return true
		}
	}
	return false
}

// authorsSDDDocuments reports whether the project root holds a document
// `validate` activates on, in the reader's tree.
func authorsSDDDocuments(reader featureengine.Reader, root string) bool {
	if _, err := reader.ReadFile(root, featureproto.ManifestFilename); err == nil {
		return true
	}
	for _, directory := range []string{featureproto.FeatureDirectory, featureproto.SpecDirectory} {
		entries, err := reader.ReadDir(root, directory)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDirectory && strings.HasSuffix(entry.Name, ".json") {
				return true
			}
		}
	}
	return false
}

// evaluationInputFailure is the task's failure for a finding about the
// workspace view, the reader or the revision, nil for any other. Such a
// finding means the evaluation could not read its inputs: the workspace, its
// candidate files, or the revision this task sets itself. Stating it at a file
// one project owns would let a run that selects another project pass, so the
// task fails instead.
func evaluationInputFailure(project *workspace.Project, finding diag.Diagnostic) error {
	if !workspaceLevelFields[finding.Field] {
		return nil
	}
	return protocolcli.Classify(
		fmt.Errorf("features validate: the evaluation of %s could not read its inputs: %s: %s", project.ID, finding.Field, finding.Message),
		protocolcli.ErrInvalidConfig,
	)
}

// workspaceLevelFields are the diagnostic fields of the feature engine that
// name no file: the workspace view and the revision every evaluation reads.
var workspaceLevelFields = map[string]bool{
	"workspace":     true,
	"reader":        true,
	"revision":      true,
	"revision.kind": true,
	"projects":      true,
}

// duplicateDeclarations states each duplicate feature finding at every
// declaration of its id in the workspace's authored manifests, with the
// finding's rule, severity and message. A finding whose declaration it does
// not find adds nothing; the finding itself is already stated.
func duplicateDeclarations(ws *workspace.Workspace, reader featureengine.Reader, duplicates []diag.Diagnostic) []evidence.Fragment {
	authored := featureengine.DiscoverAuthoredManifestsWith(ws, reader).Manifests
	byPath := make(map[string]featureproto.ManifestSource, len(authored))
	for _, source := range authored {
		byPath[source.Path] = source
	}
	declarationField := func(source featureproto.ManifestSource, index int) string {
		return source.Field(fmt.Sprintf("features[%d]", index)) + ".id"
	}
	var fragments []evidence.Fragment
	for _, finding := range duplicates {
		artifact, member, _ := strings.Cut(finding.Field, "#")
		source, ok := byPath[artifact]
		if !ok || source.Manifest == nil {
			continue
		}
		id := ""
		for index, feature := range source.Manifest.Features {
			if declarationField(source, index) == member {
				id = feature.ID
			}
		}
		if id == "" {
			continue
		}
		stated := featureFinding(finding, &workspace.Project{}, nil)
		for _, declaring := range authored {
			if declaring.Manifest == nil {
				continue
			}
			for index, feature := range declaring.Manifest.Features {
				if feature.ID == id {
					stated.Subject = evidence.FragmentSubject{Path: declaring.Path, Symbol: declarationField(declaring, index)}
					fragments = append(fragments, stated)
				}
			}
		}
	}
	return fragments
}

// featureFinding states one diagnostic of the evaluation of project as a
// finding fragment: its code as the rule, its severity and message, and its
// place as featureFindingSubject locates it.
func featureFinding(finding diag.Diagnostic, project *workspace.Project, projectRoots map[string]bool) evidence.Fragment {
	severity := evidence.FindingInfo
	switch finding.Severity {
	case diag.Error:
		severity = evidence.FindingError
	case diag.Warning:
		severity = evidence.FindingWarning
	}
	return evidence.Fragment{
		ProtocolVersion: evidence.FragmentProtocolVersion,
		Kind:            evidence.FragmentKindFinding,
		Source:          evidence.FragmentSource{Format: FeatureFindingsFormat},
		Subject:         featureFindingSubject(finding, project, projectRoots),
		Rule:            finding.Code,
		Severity:        severity,
		Message:         finding.Message,
	}
}

// featureFindingSubject places a diagnostic of the evaluation of project in
// the workspace. The engine names its subject as "<path>" or
// "<path>#<member>"; the path becomes the subject's workspace-relative path,
// the member its symbol. A field that names no file is placed at the file it
// is about:
//
//   - the featureAuthority finding, which names a project root, at that
//     project's putnami.json;
//   - any other field that names no file the engine reads, such as a
//     contribution no capability manifest is named for, at putnami.features.json
//     of the project whose evaluation reported it, with the field as its symbol.
func featureFindingSubject(finding diag.Diagnostic, project *workspace.Project, projectRoots map[string]bool) evidence.FragmentSubject {
	artifact, member, _ := strings.Cut(finding.Field, "#")
	switch {
	case finding.Code == featureproto.WarningCodeUnresolvedFeatureAuthority && projectRoots[artifact]:
		return evidence.FragmentSubject{Path: joinRootPath(artifact, wsproto.ConfigFilename), Symbol: "featureAuthority"}
	case discoveredArtifact(artifact):
		return evidence.FragmentSubject{Path: artifact, Symbol: member}
	default:
		return evidence.FragmentSubject{Path: joinRootPath(project.Path, featureproto.ManifestFilename), Symbol: finding.Field}
	}
}

// discoveredArtifact reports whether field names a file or a directory the
// feature engine reads: a JSON document, a features/ directory or an evidence
// directory, as a clean workspace-relative path.
func discoveredArtifact(field string) bool {
	if field == "" || path.Clean(field) != field || strings.HasPrefix(field, "/") || strings.HasPrefix(field, "../") || field == ".." ||
		strings.ContainsAny(field, "\\\x00") {
		return false
	}
	return strings.HasSuffix(field, ".json") ||
		path.Base(field) == featureproto.FeatureDirectory ||
		field == featureproto.EvidenceDirectory || strings.HasSuffix(field, "/"+featureproto.EvidenceDirectory)
}

// joinRootPath joins a file name to a workspace-relative root, "" for the
// workspace root.
func joinRootPath(root, file string) string {
	if root == "" {
		return file
	}
	return path.Join(root, file)
}

// lessFinding orders fragments by place, then by severity, rule and message.
func lessFinding(left, right evidence.Fragment) bool {
	if left.Subject.Path != right.Subject.Path {
		return left.Subject.Path < right.Subject.Path
	}
	if left.Subject.Symbol != right.Subject.Symbol {
		return left.Subject.Symbol < right.Subject.Symbol
	}
	if rank := findingSeverityRank(left.Severity) - findingSeverityRank(right.Severity); rank != 0 {
		return rank < 0
	}
	if left.Rule != right.Rule {
		return left.Rule < right.Rule
	}
	return left.Message < right.Message
}

func findingSeverityRank(severity string) int {
	switch severity {
	case evidence.FindingError:
		return 0
	case evidence.FindingWarning:
		return 1
	default:
		return 2
	}
}

// memoReader is a Reader that answers each read of the reader it wraps once.
// The evaluations of one task read one tree, so an answer stays true for the
// task. It returns copies, so a caller that sorts an answer changes no other
// caller's. It is safe for concurrent use: it serializes the reads of the
// reader it wraps, which is not.
type memoReader struct {
	mu       sync.Mutex
	reader   featureengine.Reader
	files    map[string]memoFile
	dirs     map[string]memoDir
	bindings map[string]memoBinding
}

type memoFile struct {
	data []byte
	err  error
}

type memoDir struct {
	entries []featureengine.DirEntry
	err     error
}

type memoBinding struct {
	binding string
	err     error
}

func newMemoReader(reader featureengine.Reader) *memoReader {
	return &memoReader{
		reader:   reader,
		files:    map[string]memoFile{},
		dirs:     map[string]memoDir{},
		bindings: map[string]memoBinding{},
	}
}

func (m *memoReader) ReadFile(root, relative string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := root + "\x00" + relative
	answer, ok := m.files[key]
	if !ok {
		data, err := m.reader.ReadFile(root, relative)
		answer = memoFile{data: data, err: err}
		m.files[key] = answer
	}
	return bytes.Clone(answer.data), answer.err
}

func (m *memoReader) ReadDir(root, relative string) ([]featureengine.DirEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := root + "\x00" + relative
	answer, ok := m.dirs[key]
	if !ok {
		entries, err := m.reader.ReadDir(root, relative)
		answer = memoDir{entries: entries, err: err}
		m.dirs[key] = answer
	}
	return slices.Clone(answer.entries), answer.err
}

func (m *memoReader) SourceBinding(root string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	answer, ok := m.bindings[root]
	if !ok {
		binding, err := m.reader.SourceBinding(root)
		answer = memoBinding{binding: binding, err: err}
		m.bindings[root] = answer
	}
	return answer.binding, answer.err
}
