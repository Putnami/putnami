// Package dependencydocs resolves AI.md from the exact npm package a
// TypeScript project sees through Node's nearest-node_modules lookup.
package dependencydocs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The status and reason vocabulary is shared with the Go extension's
// putnami.go_docs: one tool contract, two ecosystems, so an agent reads the
// same words from either.
const (
	StatusAvailable   = "available"
	StatusUnavailable = "unavailable"

	ReasonUnresolved     = "unresolved"
	ReasonDocsMissing    = "docs_missing"
	ReasonOfflineMissing = "offline_missing"
)

const (
	maxDocumentationBytes = 1024 * 1024
	maxManifestBytes      = 1024 * 1024
	maxProjectManifests   = 512
	maxWalkEntries        = 10000
)

var packageNamePattern = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)

// Source identifies the exact bytes returned by Resolve.
type Source struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Result is the stable JSON value returned by putnami.typescript_docs.
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

type packageManifest struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

// Resolve returns only documentation carried by the exact package resolved
// from the chosen project's node_modules ancestry. It does not invoke Bun,
// install dependencies, consult a registry, or use extension-carried docs.
//
// The context bounds project discovery, which walks the workspace. Cancellation
// is observed between directory entries; no work continues in the background.
func Resolve(ctx context.Context, workspaceRoot, reference, project string) Result {
	if ctx == nil {
		ctx = context.Background()
	}
	if realRoot, err := filepath.EvalSymlinks(workspaceRoot); err == nil {
		workspaceRoot = realRoot
	}
	reference = strings.TrimSpace(reference)
	result := Result{Status: StatusUnavailable, Reference: reference}
	if !packageNamePattern.MatchString(reference) {
		result.Reason = ReasonUnresolved
		result.Fallback = "Use the project's source and package metadata; the reference is not a valid npm package name."
		return result
	}

	projectDir, projectLabel, declared, err := resolveProject(ctx, workspaceRoot, project, reference)
	if err != nil {
		result.Reason = ReasonUnresolved
		result.Fallback = err.Error()
		return result
	}
	result.Project = projectLabel
	result.Package = reference

	packageDir, manifest, err := resolveInstalledPackage(workspaceRoot, projectDir, reference)
	if err != nil {
		result.Reason = ReasonUnresolved
		if declared {
			result.Reason = ReasonOfflineMissing
		}
		result.Fallback = "Use local source and package.json metadata; exact documentation will become available after the locked package is present in this project's installed dependency tree."
		return result
	}
	result.Package = manifest.Name
	result.Version = manifest.Version

	docPath, content, digest, err := readDocumentation(packageDir)
	if err != nil {
		result.Reason = ReasonDocsMissing
		result.Fallback = "Use the resolved package's source and type declarations; its AI.md is missing, unsafe, or exceeds the 1 MiB documentation limit."
		return result
	}
	kind := "installed-package"
	if pathWithin(workspaceRoot, packageDir) && !pathContainsNodeModules(workspaceRoot, packageDir) {
		kind = "workspace-replacement"
	}
	result.Status = StatusAvailable
	result.Reason = ""
	result.Fallback = ""
	result.Source = &Source{Kind: kind, Path: displayPath(workspaceRoot, docPath), SHA256: digest}
	result.Content = content
	return result
}

func resolveInstalledPackage(workspaceRoot, projectDir, reference string) (string, packageManifest, error) {
	realRoot, err := filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return "", packageManifest{}, err
	}
	current := projectDir
	for {
		candidate := filepath.Join(current, "node_modules", filepath.FromSlash(reference))
		if info, statErr := os.Lstat(candidate); statErr == nil && (info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			realPackage, evalErr := filepath.EvalSymlinks(candidate)
			if evalErr != nil {
				return "", packageManifest{}, evalErr
			}
			manifest, readErr := readPackageManifest(filepath.Join(realPackage, "package.json"))
			if readErr != nil || manifest.Name != reference || strings.TrimSpace(manifest.Version) == "" {
				return "", packageManifest{}, errors.New("resolved package manifest is missing or does not match the reference")
			}
			return realPackage, manifest, nil
		}
		if filepath.Clean(current) == filepath.Clean(realRoot) {
			break
		}
		parent := filepath.Dir(current)
		if parent == current || !pathWithin(realRoot, parent) {
			break
		}
		current = parent
	}
	return "", packageManifest{}, os.ErrNotExist
}

// resolveProject deliberately has no "single root package.json" fallback, which
// the Go tool does have for a root go.mod. A root go.mod is itself a module an
// agent can ask about; a monorepo's root package.json is normally the workspace
// manifest rather than a project, so answering from it would silently resolve
// against the wrong dependency tree. An ambiguous or unmatched request stays an
// explicit error instead.
func resolveProject(ctx context.Context, workspaceRoot, requested, reference string) (string, string, bool, error) {
	root, err := confinedDirectory(workspaceRoot, workspaceRoot)
	if err != nil {
		return "", "", false, errors.New("workspace root is unavailable")
	}
	requested = strings.TrimSpace(requested)
	if requested != "" {
		trimmed := strings.TrimPrefix(filepath.ToSlash(requested), "/")
		if trimmed != "" && trimmed != "." && safeRelative(trimmed) {
			candidate := filepath.Join(root, filepath.FromSlash(trimmed))
			if dir, err := confinedDirectory(root, candidate); err == nil {
				if manifest, readErr := readPackageManifest(filepath.Join(dir, "package.json")); readErr == nil {
					return dir, relativeLabel(root, dir), declares(manifest, reference), nil
				}
			}
		}
	}

	type candidate struct {
		dir      string
		label    string
		declared bool
	}
	manifests, err := findPackageManifests(ctx, root)
	if err != nil {
		return "", "", false, errors.New("project discovery is unavailable for TypeScript")
	}
	var candidates []candidate
	for _, manifestPath := range manifests {
		manifest, readErr := readPackageManifest(manifestPath)
		if readErr != nil {
			continue
		}
		dir := filepath.Dir(manifestPath)
		label := relativeLabel(root, dir)
		if requested != "" && requested != label && requested != manifest.Name && !putnamiNameMatches(dir, requested) {
			continue
		}
		declared := declares(manifest, reference)
		if requested == "" && !declared {
			continue
		}
		candidates = append(candidates, candidate{dir: dir, label: label, declared: declared})
	}
	if len(candidates) != 1 {
		return "", "", false, fmt.Errorf("choose one TypeScript project with the project argument; %d matching projects were found", len(candidates))
	}
	return candidates[0].dir, candidates[0].label, candidates[0].declared, nil
}

func findPackageManifests(ctx context.Context, root string) ([]string, error) {
	var paths []string
	visited := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
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
		if !entry.IsDir() && entry.Name() == "package.json" {
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

func readPackageManifest(path string) (packageManifest, error) {
	data, err := readRegularFile(path, maxManifestBytes)
	if err != nil {
		return packageManifest{}, err
	}
	var manifest packageManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return packageManifest{}, err
	}
	return manifest, nil
}

func declares(manifest packageManifest, reference string) bool {
	for _, dependencies := range []map[string]string{
		manifest.Dependencies, manifest.DevDependencies, manifest.PeerDependencies, manifest.OptionalDependencies,
	} {
		if _, ok := dependencies[reference]; ok {
			return true
		}
	}
	return false
}

func putnamiNameMatches(dir, requested string) bool {
	manifest, err := readPutnamiName(filepath.Join(dir, "putnami.json"))
	return err == nil && manifest == requested
}

func readPutnamiName(path string) (string, error) {
	data, err := readRegularFile(path, 256*1024)
	if err != nil {
		return "", err
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	return manifest.Name, nil
}

func readDocumentation(packageDir string) (string, string, string, error) {
	realPackage, err := filepath.EvalSymlinks(packageDir)
	if err != nil {
		return "", "", "", err
	}
	realDoc, err := filepath.EvalSymlinks(filepath.Join(realPackage, "AI.md"))
	if err != nil || !pathWithin(realPackage, realDoc) {
		return "", "", "", errors.New("documentation path escapes package")
	}
	data, err := readRegularFile(realDoc, maxDocumentationBytes)
	if err != nil {
		return "", "", "", err
	}
	digest := sha256.Sum256(data)
	return realDoc, string(data), hex.EncodeToString(digest[:]), nil
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

func safeRelative(path string) bool {
	if filepath.IsAbs(filepath.FromSlash(path)) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." || part == "" {
			return false
		}
	}
	return true
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func pathContainsNodeModules(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "node_modules" {
			return true
		}
	}
	return false
}

func relativeLabel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == "" {
		return "."
	}
	return filepath.ToSlash(rel)
}

func displayPath(workspaceRoot, path string) string {
	if pathWithin(workspaceRoot, path) {
		if rel, err := filepath.Rel(workspaceRoot, path); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(filepath.Clean(path))
}
