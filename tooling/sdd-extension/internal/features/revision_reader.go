package features

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	featureproto "go.putnami.dev/protocol/features"
	workspaceproto "go.putnami.dev/protocol/workspace"
	internalgit "go.putnami.dev/tooling/sdd/extension/internal/gitread"
	workspace "go.putnami.dev/tooling/sdd/extension/internal/wsview"
)

const maxGitSymlinkHops = 40

// GitTreeReader reads one immutable commit directly from Git's object database.
// It never checks out a path, creates a worktree, or consults index/worktree
// bytes for repository content.
type GitTreeReader struct {
	repoRoot string
	commit   string
}

// NewGitTreeReader resolves revision once and pins every subsequent read to the
// resulting immutable commit object ID.
func NewGitTreeReader(repoRoot, revision string) (*GitTreeReader, error) {
	absolute, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve repository root: %w", err)
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("repository root is unavailable")
	}
	commit, err := internalgit.ResolveCommit(resolvedRoot, revision)
	if err != nil {
		return nil, err
	}
	return &GitTreeReader{repoRoot: filepath.Clean(resolvedRoot), commit: commit}, nil
}

// Revision returns the reader's immutable, lower-case commit metadata.
func (reader *GitTreeReader) Revision() Revision {
	return Revision{Kind: RevisionKindGit, Commit: "git:" + reader.commit}
}

// ReadFile reads one contained regular/executable blob, following only relative
// symlinks that remain inside the selected discovery root.
func (reader *GitTreeReader) ReadFile(root, relative string) ([]byte, error) {
	if code := validateRelativePath(relative, false); code != "" {
		return nil, newReaderError(code, nil)
	}
	rootPath, err := reader.resolveRoot(root)
	if err != nil {
		return nil, err
	}
	entry, _, err := reader.resolvePath(path.Join(rootPath, relative), rootPath, 0)
	if err != nil {
		return nil, err
	}
	if entry.Type != "blob" || (entry.Mode != "100644" && entry.Mode != "100755") {
		return nil, newReaderError(ReaderErrorUnsupportedFile, nil)
	}
	data, err := internalgit.ReadTreeBlob(reader.repoRoot, entry.ObjectID, MaxReadBytes)
	if errors.Is(err, internalgit.ErrTreeObjectTooLarge) {
		return nil, newReaderError(ReaderErrorReadLimit, err)
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// ReadDir lists direct tree members without walking descendants.
func (reader *GitTreeReader) ReadDir(root, relative string) ([]DirEntry, error) {
	if code := validateRelativePath(relative, false); code != "" {
		return nil, newReaderError(code, nil)
	}
	rootPath, err := reader.resolveRoot(root)
	if err != nil {
		return nil, err
	}
	entry, _, err := reader.resolvePath(path.Join(rootPath, relative), rootPath, 0)
	if err != nil {
		return nil, err
	}
	if entry.Type != "tree" {
		return nil, newReaderError(ReaderErrorUnsupportedFile, nil)
	}
	entries, err := internalgit.TreeEntries(reader.repoRoot, entry.ObjectID)
	if err != nil {
		return nil, err
	}
	if len(entries) > MaxEvidenceFiles {
		return nil, newReaderError(ReaderErrorEvidenceFileLimit, nil)
	}
	result := make([]DirEntry, 0, len(entries))
	for _, member := range entries {
		result = append(result, DirEntry{Name: member.Path, IsDirectory: member.Type == "tree"})
	}
	return result, nil
}

// SourceBinding computes source-v1 from the selected immutable tree.
func (reader *GitTreeReader) SourceBinding(root string) (string, error) {
	rootPath, err := reader.resolveRoot(root)
	if err != nil {
		return "", err
	}
	return internalgit.CommitSourceBinding(reader.repoRoot, reader.commit, rootPath)
}

func (reader *GitTreeReader) directoryExists(relative string) (bool, error) {
	if code := validateRelativePath(relative, false); code != "" {
		return false, newReaderError(code, nil)
	}
	entry, _, err := reader.resolvePath(relative, "", 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return entry.Type == "tree", nil
}

func (reader *GitTreeReader) resolveRoot(root string) (string, error) {
	if code := validateRelativePath(root, true); code != "" {
		return "", newReaderError(code, nil)
	}
	if root == "" {
		return "", nil
	}
	entry, resolved, err := reader.resolvePath(root, "", 0)
	if err != nil {
		return "", err
	}
	if entry.Type != "tree" {
		return "", newReaderError(ReaderErrorUnsupportedFile, nil)
	}
	return resolved, nil
}

func (reader *GitTreeReader) resolvePath(repoPath, boundary string, hops int) (internalgit.TreeEntry, string, error) {
	if hops > maxGitSymlinkHops {
		return internalgit.TreeEntry{}, "", newReaderError(ReaderErrorUnsupportedFile, nil)
	}
	if repoPath == "" {
		entry, _, err := internalgit.TreeEntryAt(reader.repoRoot, reader.commit, "")
		return entry, "", err
	}
	if code := validateRelativePath(repoPath, false); code != "" {
		return internalgit.TreeEntry{}, "", newReaderError(code, nil)
	}
	if !slashPathContained(boundary, repoPath) {
		return internalgit.TreeEntry{}, "", newReaderError(ReaderErrorSymlinkEscape, nil)
	}

	segments := strings.Split(repoPath, "/")
	current := ""
	for index, segment := range segments {
		current = path.Join(current, segment)
		entry, exists, err := internalgit.TreeEntryAt(reader.repoRoot, reader.commit, current)
		if err != nil {
			return internalgit.TreeEntry{}, "", err
		}
		if !exists {
			return internalgit.TreeEntry{}, "", fs.ErrNotExist
		}
		if entry.Mode == "120000" {
			targetBytes, readErr := internalgit.ReadTreeBlob(reader.repoRoot, entry.ObjectID, maxProtocolPathBytes)
			if errors.Is(readErr, internalgit.ErrTreeObjectTooLarge) {
				return internalgit.TreeEntry{}, "", newReaderError(ReaderErrorInvalidPath, readErr)
			}
			if readErr != nil {
				return internalgit.TreeEntry{}, "", readErr
			}
			target := string(targetBytes)
			if target == "" || path.IsAbs(target) || strings.ContainsRune(target, '\x00') {
				return internalgit.TreeEntry{}, "", newReaderError(ReaderErrorSymlinkEscape, nil)
			}
			resolved := path.Clean(path.Join(path.Dir(current), target))
			if resolved == "." {
				resolved = ""
			}
			if !slashPathContained("", resolved) || !slashPathContained(boundary, resolved) {
				return internalgit.TreeEntry{}, "", newReaderError(ReaderErrorSymlinkEscape, nil)
			}
			if index+1 < len(segments) {
				resolved = path.Join(resolved, path.Join(segments[index+1:]...))
			}
			if !slashPathContained(boundary, resolved) {
				return internalgit.TreeEntry{}, "", newReaderError(ReaderErrorSymlinkEscape, nil)
			}
			return reader.resolvePath(resolved, boundary, hops+1)
		}
		if index+1 < len(segments) && entry.Type != "tree" {
			return internalgit.TreeEntry{}, "", newReaderError(ReaderErrorUnsupportedFile, nil)
		}
		if index+1 == len(segments) {
			return entry, current, nil
		}
	}
	return internalgit.TreeEntry{}, "", fs.ErrNotExist
}

func slashPathContained(root, target string) bool {
	if target == "" {
		return root == ""
	}
	if path.Clean(target) != target || strings.HasPrefix(target, "/") || target == ".." || strings.HasPrefix(target, "../") {
		return false
	}
	return root == "" || target == root || strings.HasPrefix(target, root+"/")
}

// RevisionEvaluation keeps resolved immutable metadata beside the engine
// result so command failures can still carry typed revision diagnostics.
type RevisionEvaluation struct {
	Revision Revision
	Result   Result
}

// EvaluateGitRevision independently loads one commit's workspace membership
// and feature inputs, then evaluates them through the same aggregation engine
// as the current-worktree commands.
func EvaluateGitRevision(repoRoot, revision string) (RevisionEvaluation, error) {
	reader, err := NewGitTreeReader(repoRoot, revision)
	if err != nil {
		return RevisionEvaluation{}, err
	}
	evaluation := RevisionEvaluation{Revision: reader.Revision()}
	ws, diagnostics := loadRevisionWorkspace(reader)
	if ws == nil || diag.HasErrors(diagnostics) {
		evaluation.Result = Result{Diagnostics: normalizeDiagnostics(diagnostics)}
		return evaluation, nil
	}
	result := Aggregate(Request{Workspace: ws, Reader: reader, Revision: evaluation.Revision})
	result.Diagnostics = normalizeDiagnostics(append(diagnostics, result.Diagnostics...))
	if diag.HasErrors(result.Diagnostics) {
		result.Snapshot = nil
	}
	evaluation.Result = result
	return evaluation, nil
}

type revisionPutnamiConfig struct {
	project *workspaceproto.ProjectConfig
	scope   *workspaceproto.ScopeConfig
}

type revisionWorkspaceLoader struct {
	reader      *GitTreeReader
	configs     map[string]revisionPutnamiConfig
	diagnostics []diag.Diagnostic
}

func loadRevisionWorkspace(reader *GitTreeReader) (*workspace.Workspace, []diag.Diagnostic) {
	loader := &revisionWorkspaceLoader{reader: reader, configs: make(map[string]revisionPutnamiConfig)}
	data, err := reader.ReadFile("", workspaceproto.WorkspaceConfigFilename)
	if err != nil {
		loader.diagnostics = append(loader.diagnostics, readDiagnostic(workspaceproto.WorkspaceConfigFilename, err))
		return nil, loader.diagnostics
	}
	var config workspaceproto.Config
	if err := json.Unmarshal(data, &config); err != nil {
		loader.diagnostics = append(loader.diagnostics, diag.Errorf(
			featureproto.ErrorCodeParseError,
			workspaceproto.WorkspaceConfigFilename,
			"workspace membership config could not be parsed",
		))
		return nil, loader.diagnostics
	}

	projectPaths := make(map[string]bool)
	for index, included := range config.Includes {
		if code := validateRelativePath(included, false); code != "" {
			loader.diagnostics = append(loader.diagnostics, pathDiagnostic(
				code,
				fmt.Sprintf("%s#includes[%d]", workspaceproto.WorkspaceConfigFilename, index),
				"workspace member is not a canonical contained relative path",
			))
			continue
		}
		exists, existsErr := reader.directoryExists(included)
		if existsErr != nil {
			loader.diagnostics = append(loader.diagnostics, readDiagnostic(included, existsErr))
			continue
		}
		if !exists {
			continue
		}
		putnami := loader.configAt(included)
		if putnami.scope != nil && len(putnami.scope.Includes) > 0 {
			if putnami.scope.Activate {
				projectPaths[included] = true
			}
			for childIndex, child := range putnami.scope.Includes {
				if code := validateRelativePath(child, false); code != "" {
					loader.diagnostics = append(loader.diagnostics, pathDiagnostic(
						code,
						fmt.Sprintf("%s/%s#includes[%d]", included, workspaceproto.ConfigFilename, childIndex),
						"scope member is not a canonical contained relative path",
					))
					continue
				}
				member := path.Join(included, child)
				memberExists, memberErr := reader.directoryExists(member)
				if memberErr != nil {
					loader.diagnostics = append(loader.diagnostics, readDiagnostic(member, memberErr))
					continue
				}
				if memberExists {
					projectPaths[member] = true
				}
			}
			continue
		}
		projectPaths[included] = true
	}

	paths := make([]string, 0, len(projectPaths))
	for projectPath := range projectPaths {
		paths = append(paths, projectPath)
	}
	sort.Strings(paths)
	projects := make([]*workspace.Project, 0, len(paths))
	for _, projectPath := range paths {
		projects = append(projects, loader.projectAt(projectPath))
	}
	loader.validateProjectIdentities(projects)
	if diag.HasErrors(loader.diagnostics) {
		return nil, normalizeDiagnostics(loader.diagnostics)
	}
	return workspace.NewWorkspace("", &config, projects), normalizeDiagnostics(loader.diagnostics)
}

func (loader *revisionWorkspaceLoader) configAt(directory string) revisionPutnamiConfig {
	if cached, ok := loader.configs[directory]; ok {
		return cached
	}
	config := revisionPutnamiConfig{}
	filename := path.Join(directory, workspaceproto.ConfigFilename)
	data, err := loader.reader.ReadFile("", filename)
	if errors.Is(err, fs.ErrNotExist) {
		loader.configs[directory] = config
		return config
	}
	if err != nil {
		loader.diagnostics = append(loader.diagnostics, readDiagnostic(filename, err))
		loader.configs[directory] = config
		return config
	}
	var projectConfig workspaceproto.ProjectConfig
	var scopeConfig workspaceproto.ScopeConfig
	if projectErr := json.Unmarshal(data, &projectConfig); projectErr != nil {
		loader.diagnostics = append(loader.diagnostics, diag.Errorf(featureproto.ErrorCodeParseError, filename, "project configuration could not be parsed"))
	} else if scopeErr := json.Unmarshal(data, &scopeConfig); scopeErr != nil {
		loader.diagnostics = append(loader.diagnostics, diag.Errorf(featureproto.ErrorCodeParseError, filename, "scope configuration could not be parsed"))
	} else {
		config.project = &projectConfig
		config.scope = &scopeConfig
	}
	loader.configs[directory] = config
	return config
}

func (loader *revisionWorkspaceLoader) projectAt(projectPath string) *workspace.Project {
	putnami := loader.configAt(projectPath)
	project := &workspace.Project{Path: projectPath, ID: workspace.ProjectIDFromPath(projectPath), Config: putnami.project}
	if putnami.project != nil {
		project.Name = putnami.project.Name
		project.Type = putnami.project.Type
		project.Tags = append([]string(nil), putnami.project.Tags...)
		project.Dependencies = append([]string(nil), putnami.project.Dependencies...)
		project.Publish = append([]string(nil), putnami.project.Publish...)
		project.Extensions = append([]string(nil), putnami.project.Extensions...)
		project.RunsWith = append([]string(nil), putnami.project.RunsWith...)
	}

	// LoadScopeChain includes a root-level putnami.json before intermediate
	// scope files. Preserve that identity resolution for historical trees too.
	chain := []string{""}
	for directory := path.Dir(projectPath); directory != "." && directory != ""; directory = path.Dir(directory) {
		chain = append(chain, directory)
	}
	for left, right := 1, len(chain)-1; left < right; left, right = left+1, right-1 {
		chain[left], chain[right] = chain[right], chain[left]
	}
	var mergedScope workspaceproto.ScopeConfig
	for _, directory := range chain {
		if scope := loader.configAt(directory).scope; scope != nil {
			workspaceproto.MergeScope(&mergedScope, scope)
		}
	}

	nameDeclared := project.Name != ""
	if project.Name == "" {
		project.Name = path.Base(projectPath)
	}
	project.SourceName = project.Name
	if !nameDeclared {
		if patterned := mergedScope.ResolveNamePattern(path.Base(projectPath)); patterned != "" {
			project.Name = patterned
		}
	}
	if project.Tags == nil {
		project.Tags = append([]string(nil), mergedScope.Tags...)
	}
	if len(project.Extensions) == 0 {
		project.Extensions = append([]string(nil), mergedScope.Extensions...)
	}
	return project
}

func (loader *revisionWorkspaceLoader) validateProjectIdentities(projects []*workspace.Project) {
	ids := make(map[string][]string)
	names := make(map[string][]string)
	for _, project := range projects {
		ids[project.ID] = append(ids[project.ID], project.Path)
		names[project.Name] = append(names[project.Name], project.Path)
	}
	for _, values := range []map[string][]string{ids, names} {
		keys := make([]string, 0, len(values))
		for key, paths := range values {
			if key != "" && len(paths) > 1 {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			paths := append([]string(nil), values[key]...)
			sort.Strings(paths)
			loader.diagnostics = append(loader.diagnostics, diag.Errorf(
				featureproto.ErrorCodeInvalidID,
				"projects",
				"workspace project identity %q is duplicated across %s",
				key,
				strings.Join(paths, ", "),
			))
		}
	}
}
