// Package dependencydocs resolves documentation from the exact Go module
// selected for one project. Resolution delegates to cmd/go's module resolver
// in strict offline/read-only mode; it never downloads a module or edits the
// project's module files.
package dependencydocs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/go/extension/internal/toolchain"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

const (
	maxDocumentationBytes = 1024 * 1024
	maxProjectManifests   = 512
	maxWalkEntries        = 10000
)

const (
	StatusAvailable   = "available"
	StatusUnavailable = "unavailable"

	ReasonUnresolved     = "unresolved"
	ReasonDocsMissing    = "docs_missing"
	ReasonOfflineMissing = "offline_missing"
)

// Source identifies the bytes returned by Resolve.
type Source struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Result is the stable JSON value returned by putnami.go_docs.
type Result struct {
	Status    string  `json:"status"`
	Reference string  `json:"reference"`
	Project   string  `json:"project,omitempty"`
	Package   string  `json:"package,omitempty"`
	Version   string  `json:"version,omitempty"`
	Source    *Source `json:"source,omitempty"`
	Content   string  `json:"content,omitempty"`
	Reason    string  `json:"reason,omitempty"`
	Fallback  string  `json:"fallback,omitempty"`
}

type listedModule struct {
	Path    string        `json:"Path"`
	Version string        `json:"Version"`
	Dir     string        `json:"Dir"`
	Main    bool          `json:"Main"`
	Replace *listedModule `json:"Replace"`
}

// moduleLister is the cmd/go seam used by tests and by Resolve.
type moduleLister func(context.Context, moduleQuery) (listedModule, error)

// moduleQuery carries every root cmd/go resolution needs. It exists so the
// toolchain lookup sees the project it is answering for instead of whatever a
// surrounding job happened to export.
type moduleQuery struct {
	WorkspaceRoot string
	ExtensionRoot string
	ProjectDir    string
	Reference     string
}

// Resolve returns documentation only when cmd/go can identify the exact
// selected module using already-present workspace and cache state.
func Resolve(ctx context.Context, workspaceRoot, extensionRoot, reference, project string) Result {
	return resolve(ctx, workspaceRoot, extensionRoot, reference, project, listModule)
}

func resolve(ctx context.Context, workspaceRoot, extensionRoot, reference, project string, list moduleLister) Result {
	if realRoot, err := filepath.EvalSymlinks(workspaceRoot); err == nil {
		workspaceRoot = realRoot
	}
	reference = strings.TrimSpace(reference)
	base := Result{Status: StatusUnavailable, Reference: reference}
	if reference == "" || module.CheckPath(reference) != nil {
		base.Reason = ReasonUnresolved
		base.Fallback = "Use the project's source and module metadata; the reference is not a valid Go module path."
		return base
	}

	projectDir, projectLabel, declared, err := resolveProject(workspaceRoot, project, reference)
	if err != nil {
		base.Reason = ReasonUnresolved
		base.Fallback = err.Error()
		return base
	}
	base.Project = projectLabel
	base.Package = reference

	selected, err := list(ctx, moduleQuery{
		WorkspaceRoot: workspaceRoot, ExtensionRoot: extensionRoot,
		ProjectDir: projectDir, Reference: reference,
	})
	if err != nil || selected.Path == "" {
		base.Reason = ReasonUnresolved
		if declared {
			base.Reason = ReasonOfflineMissing
		}
		base.Fallback = "Use local source and go.mod metadata; exact documentation will become available after the locked module is present in the offline module cache."
		return base
	}

	source := selected
	kind := "installed-package"
	version := selected.Version
	if selected.Replace != nil {
		source = *selected.Replace
		if source.Version != "" {
			version = source.Version
		}
	}
	if source.Dir != "" {
		if realSource, evalErr := filepath.EvalSymlinks(source.Dir); evalErr == nil {
			source.Dir = realSource
		}
	}
	if source.Dir != "" && pathWithin(workspaceRoot, source.Dir) && !pathContainsComponent(workspaceRoot, source.Dir, ".putnami") {
		kind = "workspace-replacement"
	}
	if version == "" {
		version = requiredVersion(projectDir, reference)
	}
	base.Version = version
	if version == "" {
		base.Reason = ReasonUnresolved
		base.Fallback = "Use the resolved workspace module's source; no exact dependency version is declared for this project."
		return base
	}
	if source.Dir == "" {
		base.Reason = ReasonOfflineMissing
		base.Fallback = "Use local source and go.mod metadata; cmd/go selected the module but its source is absent from the offline module cache."
		return base
	}

	docPath, content, digest, err := readDocumentation(source.Dir)
	if err != nil {
		base.Reason = ReasonDocsMissing
		base.Fallback = "Use the resolved module's source and package comments; its AI.md is missing, unsafe, or exceeds the 1 MiB documentation limit."
		return base
	}
	base.Status = StatusAvailable
	base.Reason = ""
	base.Fallback = ""
	base.Source = &Source{Kind: kind, Path: displayPath(workspaceRoot, docPath), SHA256: digest}
	base.Content = content
	return base
}

func listModule(ctx context.Context, query moduleQuery) (listedModule, error) {
	binary, err := resolveGoBinary(query)
	if err != nil {
		return listedModule{}, err
	}
	cmd := exec.CommandContext(ctx, binary, "list", "-m", "-json", "-mod=readonly", query.Reference)
	cmd.Dir = query.ProjectDir
	cmd.Env = offlineGoEnv(os.Environ(), query.ProjectDir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitedWriter{W: &stdout, N: 256 * 1024}
	cmd.Stderr = &limitedWriter{W: &stderr, N: 32 * 1024}
	if err := cmd.Run(); err != nil {
		return listedModule{}, fmt.Errorf("resolve module offline: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var selected listedModule
	decoder := json.NewDecoder(&stdout)
	if err := decoder.Decode(&selected); err != nil {
		return listedModule{}, fmt.Errorf("decode selected module: %w", err)
	}
	return selected, nil
}

type limitedWriter struct {
	W io.Writer
	N int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.N {
		return 0, errors.New("command output exceeds limit")
	}
	n, err := w.W.Write(p)
	w.N -= int64(n)
	return n, err
}

// resolveGoBinary uses the extension's one toolchain resolver so a
// documentation read selects the same Go every build, test and lint job in this
// workspace selects — including a workspace-managed toolchain when the machine
// has none on PATH. The project path is passed explicitly: offlineGoEnv pins
// GOTOOLCHAIN=local, so a candidate that predates this project's `go` directive
// must be rejected here rather than fail opaquely inside `go list`.
func resolveGoBinary(query moduleQuery) (string, error) {
	binary, err := toolchain.ResolveGoFor(toolchain.GoResolution{
		WorkspaceRoot: query.WorkspaceRoot,
		ExtensionRoot: query.ExtensionRoot,
		ProjectPath:   query.ProjectDir,
	})
	if err != nil {
		return "", fmt.Errorf("no compatible Go toolchain is available: %w", err)
	}
	return binary, nil
}

func offlineGoEnv(env []string, projectDir string) []string {
	inheritedGoWork := envValue(env, "GOWORK")
	blocked := map[string]bool{
		"GONOPROXY": true, "GONOSUMDB": true, "GOPRIVATE": true,
		"GOPROXY": true, "GOSUMDB": true, "GOTELEMETRY": true,
		"GOTOOLCHAIN": true, "GOWORK": true, "PWD": true,
	}
	out := make([]string, 0, len(env)+7)
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !blocked[key] {
			out = append(out, entry)
		}
	}
	out = append(out,
		"GONOPROXY=none",
		"GONOSUMDB=*",
		"GOPRIVATE=",
		"GOPROXY=off",
		"GOSUMDB=off",
		"GOTELEMETRY=off",
		"GOTOOLCHAIN=local",
	)
	if inheritedGoWork != "" {
		out = append(out, "GOWORK="+inheritedGoWork)
	} else if work := toolchain.FindGoWork(projectDir); work != "" {
		out = append(out, "GOWORK="+work)
	} else {
		out = append(out, "GOWORK=off")
	}
	return out
}

func envValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		entryKey, entryValue, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			value = entryValue
		}
	}
	return value
}

func resolveProject(workspaceRoot, requested, reference string) (string, string, bool, error) {
	root, err := confinedDirectory(workspaceRoot, workspaceRoot)
	if err != nil {
		return "", "", false, errors.New("workspace root is unavailable")
	}
	requested = strings.TrimSpace(requested)
	if requested != "" {
		trimmed := strings.TrimPrefix(filepath.ToSlash(requested), "/")
		if trimmed != "" && trimmed != "." && !strings.Contains(trimmed, "..") {
			candidate := filepath.Join(root, filepath.FromSlash(trimmed))
			if dir, err := confinedDirectory(root, candidate); err == nil && fileExists(filepath.Join(dir, "go.mod")) {
				return dir, relativeLabel(root, dir), moduleDeclared(dir, reference), nil
			}
		}
	}

	type candidate struct {
		dir      string
		label    string
		declared bool
	}
	var candidates []candidate
	manifests, err := findManifests(root, "go.mod")
	if err != nil {
		return "", "", false, errors.New("go project discovery is unavailable")
	}
	for _, manifest := range manifests {
		dir := filepath.Dir(manifest)
		mod, modErr := readGoModBounded(manifest)
		if modErr != nil || mod == nil {
			continue
		}
		label := relativeLabel(root, dir)
		if requested != "" && requested != label && requested != mod.Module && !projectNameMatches(dir, requested) {
			continue
		}
		declared := moduleDeclaredFrom(mod, reference)
		if requested == "" && !declared {
			continue
		}
		candidates = append(candidates, candidate{dir: dir, label: label, declared: declared})
	}
	if requested == "" && len(candidates) == 0 && fileExists(filepath.Join(root, "go.mod")) {
		return root, ".", moduleDeclared(root, reference), nil
	}
	if len(candidates) != 1 {
		return "", "", false, fmt.Errorf("choose one Go project with the project argument; %d matching projects were found", len(candidates))
	}
	return candidates[0].dir, candidates[0].label, candidates[0].declared, nil
}

func moduleDeclared(dir, reference string) bool {
	mod, err := readGoModBounded(filepath.Join(dir, "go.mod"))
	return err == nil && mod != nil && moduleDeclaredFrom(mod, reference)
}

func requiredVersion(projectDir, reference string) string {
	data, err := readRegularFile(filepath.Join(projectDir, "go.mod"), 1024*1024)
	if err != nil {
		return ""
	}
	parsed, err := modfile.Parse("go.mod", data, nil)
	if err != nil {
		return ""
	}
	for _, requirement := range parsed.Require {
		if requirement.Mod.Path == reference {
			return requirement.Mod.Version
		}
	}
	return ""
}

func moduleDeclaredFrom(mod *toolchain.GoModFile, reference string) bool {
	for _, required := range mod.Requires {
		if required == reference {
			return true
		}
	}
	for _, replace := range mod.Replaces {
		if replace.Old == reference {
			return true
		}
	}
	return false
}

func projectNameMatches(dir, requested string) bool {
	data, err := readRegularFile(filepath.Join(dir, "putnami.json"), 256*1024)
	if err != nil {
		return false
	}
	var manifest struct {
		Name string `json:"name"`
	}
	return json.Unmarshal(data, &manifest) == nil && manifest.Name == requested
}

func findManifests(root, name string) ([]string, error) {
	var paths []string
	visited := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() && path != root {
			switch entry.Name() {
			case ".cache", ".context", ".gen", ".git", ".putnami", "node_modules", "dist", "vendor":
				return filepath.SkipDir
			}
		}
		visited++
		if visited > maxWalkEntries {
			return errors.New("project discovery entry limit exceeded")
		}
		if !entry.IsDir() && entry.Name() == name {
			paths = append(paths, path)
			if len(paths) > maxProjectManifests {
				return errors.New("project manifest limit exceeded")
			}
		}
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func readDocumentation(moduleDir string) (string, string, string, error) {
	realRoot, err := filepath.EvalSymlinks(moduleDir)
	if err != nil {
		return "", "", "", err
	}
	docPath := filepath.Join(realRoot, "AI.md")
	realDoc, err := filepath.EvalSymlinks(docPath)
	if err != nil || !pathWithin(realRoot, realDoc) {
		return "", "", "", errors.New("documentation path escapes module")
	}
	data, err := readRegularFile(realDoc, maxDocumentationBytes)
	if err != nil {
		return "", "", "", err
	}
	digest := sha256.Sum256(data)
	return realDoc, string(data), hex.EncodeToString(digest[:]), nil
}

func readGoModBounded(path string) (*toolchain.GoModFile, error) {
	data, err := readRegularFile(path, 1024*1024)
	if err != nil {
		return nil, err
	}
	return toolchain.ParseGoMod(string(data))
}

func readRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	if info.Size() > limit {
		return nil, errors.New("file exceeds limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("file exceeds limit")
	}
	return data, nil
}

func confinedDirectory(root, candidate string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	realCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil || !pathWithin(realRoot, realCandidate) {
		return "", errors.New("path escapes workspace")
	}
	info, err := os.Stat(realCandidate)
	if err != nil || !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return realCandidate, nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func relativeLabel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "" || rel == "." {
		return "."
	}
	return filepath.ToSlash(rel)
}

func displayPath(workspaceRoot, path string) string {
	if pathWithin(workspaceRoot, path) {
		rel, err := filepath.Rel(workspaceRoot, path)
		if err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(filepath.Clean(path))
}

func pathContainsComponent(root, path, component string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == component {
			return true
		}
	}
	return false
}
