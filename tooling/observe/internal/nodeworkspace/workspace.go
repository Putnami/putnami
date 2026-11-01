// Package nodeworkspace detects the packages of a Node workspace: npm, pnpm and
// yarn workspaces, which Turborepo and Lerna build on. It reads the
// repository's own files and runs no package manager.
//
// Read gives what the root files declare: the package manager, the lockfile
// and the member directories. Probe answers a workspace probe request with one
// project per member, its edges to the other members and its metadata, so the
// detector can move to an extension of its own without a contract change.
package nodeworkspace

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
)

// Package managers.
const (
	ManagerNPM  = "npm"
	ManagerPNPM = "pnpm"
	ManagerYarn = "yarn"
)

// Files the workspace reads at its root.
const (
	PackageJSON   = "package.json"
	PNPMWorkspace = "pnpm-workspace.yaml"
	NPMRC         = ".npmrc"
)

// Diagnostic codes.
const (
	CodeManifestInvalid = "observe.node_manifest_invalid"
	CodePatternInvalid  = "observe.node_pattern_invalid"
	CodeWalkBounded     = "observe.node_walk_bounded"
)

// lockfiles are the root lockfiles, in the order they are looked for, with the
// package manager each one belongs to.
var lockfiles = []struct{ name, manager string }{
	{"pnpm-lock.yaml", ManagerPNPM},
	{"yarn.lock", ManagerYarn},
	{"package-lock.json", ManagerNPM},
	{"npm-shrinkwrap.json", ManagerNPM},
}

// maxWalkEntries bounds the directory entries one Read visits.
var maxWalkEntries = 200000

// skippedDirs are never walked into.
var skippedDirs = map[string]bool{".git": true, "node_modules": true}

// Workspace is what the root files of a Node workspace declare.
type Workspace struct {
	// Manager is npm, pnpm or yarn.
	Manager string
	// Lockfile is the repo-relative root lockfile, empty when there is none.
	Lockfile string
	// Source is the file the member patterns come from: package.json or
	// pnpm-workspace.yaml.
	Source string
	// Patterns are the member patterns in declaration order; an excluding
	// pattern keeps its leading "!".
	Patterns []string
	// LinkRanges reports whether a version range a member satisfies links that
	// member. pnpm links one only when linkWorkspacePackages is set.
	LinkRanges bool
	// Members are the repo-relative member directories, sorted.
	Members []string
	// WatchedFiles are the repo-relative root files the answer depends on.
	WatchedFiles []string
}

// Manifest is the subset of a package.json the detector reads. It is not
// strict: a package.json carries many members the detector has no opinion on.
type Manifest struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	PackageManager       string            `json:"packageManager"`
	Workspaces           json.RawMessage   `json:"workspaces"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
}

// ReadManifest reads the package.json of dir, relative to root. It returns
// (nil, nil) when dir holds none.
func ReadManifest(root, dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path.Join(dir, PackageJSON))))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Read reads the Node workspace rooted at root. ok is false when root declares
// no members: neither packages in pnpm-workspace.yaml nor workspaces in its
// package.json.
// A file it cannot read or parse, an invalid pattern and a bounded walk are
// diagnostics, never errors.
func Read(root string) (ws Workspace, ok bool, diags []diag.Diagnostic) {
	manifest, err := ReadManifest(root, ".")
	if err != nil {
		diags = append(diags, diag.Warningf(CodeManifestInvalid, PackageJSON, "the root package.json cannot be read: %v", err))
	}
	pnpm, hasPNPM, pnpmDiags := readPNPMWorkspace(root)
	diags = append(diags, pnpmDiags...)

	switch {
	case hasPNPM && pnpm.declared:
		ws.Source = PNPMWorkspace
		ws.Patterns = pnpm.packages
	case manifest != nil && len(manifest.Workspaces) > 0:
		patterns, err := workspacePatterns(manifest.Workspaces)
		if err != nil {
			diags = append(diags, diag.Warningf(CodeManifestInvalid, PackageJSON+"#workspaces", "the workspaces of the root package.json cannot be read: %v", err))
			return Workspace{}, false, diags
		}
		ws.Source = PackageJSON
		ws.Patterns = patterns
	default:
		return Workspace{}, false, diags
	}

	ws.Manager = manager(manifest, hasPNPM, root)
	ws.Lockfile = lockfile(root, ws.Manager)
	ws.LinkRanges = ws.Manager != ManagerPNPM || pnpm.linkWorkspacePackages || npmrcLinksWorkspacePackages(root)
	// A root file that appears later changes the answer, so each one is
	// watched whether it exists or not.
	ws.WatchedFiles = []string{PackageJSON, PNPMWorkspace, NPMRC}
	for _, lock := range lockfiles {
		ws.WatchedFiles = append(ws.WatchedFiles, lock.name)
	}
	members, memberDiags := expandMembers(root, ws.Source, ws.Patterns)
	ws.Members = members
	return ws, true, append(diags, memberDiags...)
}

// workspacePatterns reads the workspaces member of a package.json: a list of
// patterns (npm, yarn), or an object whose packages are the patterns (yarn).
func workspacePatterns(raw json.RawMessage) ([]string, error) {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var object struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, errors.New("workspaces is neither a list of patterns nor an object with packages")
	}
	return object.Packages, nil
}

// manager names the package manager: the packageManager field, else pnpm when
// pnpm-workspace.yaml exists, else the manager of the lockfile, else npm.
func manager(m *Manifest, hasPNPM bool, root string) string {
	if m != nil {
		name, _, _ := strings.Cut(m.PackageManager, "@")
		switch name {
		case ManagerNPM, ManagerPNPM, ManagerYarn:
			return name
		}
	}
	if hasPNPM {
		return ManagerPNPM
	}
	for _, lock := range lockfiles {
		if fileExists(root, lock.name) {
			return lock.manager
		}
	}
	return ManagerNPM
}

// lockfile returns the root lockfile of the manager, else another root
// lockfile, else "".
func lockfile(root, manager string) string {
	for _, own := range []bool{true, false} {
		for _, lock := range lockfiles {
			if (lock.manager == manager) == own && fileExists(root, lock.name) {
				return lock.name
			}
		}
	}
	return ""
}

// npmrcLinksWorkspacePackages reports whether the root .npmrc sets
// link-workspace-packages to true or deep.
func npmrcLinksWorkspacePackages(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, NPMRC))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found && strings.TrimSpace(key) == "link-workspace-packages" {
			value = strings.TrimSpace(value)
			return value == "true" || value == "deep"
		}
	}
	return false
}

func fileExists(root, name string) bool {
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
	return err == nil && info.Mode().IsRegular()
}

// expandMembers returns the directories holding a package.json that an
// including pattern matches and no excluding pattern does, the root excepted.
func expandMembers(root, source string, patterns []string) ([]string, []diag.Diagnostic) {
	var include, exclude [][]string
	var diags []diag.Diagnostic
	for _, raw := range patterns {
		negated := strings.HasPrefix(raw, "!")
		alternatives, ok := expandBraces(strings.TrimPrefix(raw, "!"))
		var expanded [][]string
		for _, alternative := range alternatives {
			segments, valid := patternSegments(alternative)
			ok = ok && valid
			expanded = append(expanded, segments)
		}
		if !ok {
			diags = append(diags, diag.Warningf(CodePatternInvalid, source, "the workspace pattern %q is not a repository-relative glob and is ignored", raw))
			continue
		}
		if negated {
			exclude = append(exclude, expanded...)
		} else {
			include = append(include, expanded...)
		}
	}
	if len(include) == 0 {
		return nil, diags
	}

	found := map[string]bool{}
	entries := 0
	bounded := false
	for _, segments := range include {
		start := staticPrefix(segments)
		startDir := filepath.Join(root, filepath.FromSlash(path.Join(append([]string{"."}, start...)...)))
		_ = filepath.WalkDir(startDir, func(file string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.IsDir() {
				return nil
			}
			entries++
			if entries > maxWalkEntries {
				bounded = true
				return filepath.SkipAll
			}
			rel, relErr := filepath.Rel(root, file)
			if relErr != nil {
				return filepath.SkipDir
			}
			rel = filepath.ToSlash(rel)
			if rel != "." && skippedDirs[entry.Name()] {
				return filepath.SkipDir
			}
			parts := splitPath(rel)
			if !canMatchBelow(segments, parts) {
				return filepath.SkipDir
			}
			if rel != "." && matchSegments(segments, parts) && fileExists(root, path.Join(rel, PackageJSON)) {
				found[rel] = true
			}
			return nil
		})
		if bounded {
			break
		}
	}
	if bounded {
		diags = append(diags, diag.Warningf(CodeWalkBounded, source, "the workspace walk stopped after %d directories; members past it are left out", maxWalkEntries))
	}

	members := make([]string, 0, len(found))
	for dir := range found {
		if !excluded(exclude, splitPath(dir)) {
			members = append(members, dir)
		}
	}
	sort.Strings(members)
	return members, diags
}

func excluded(patterns [][]string, parts []string) bool {
	for _, segments := range patterns {
		if matchSegments(segments, parts) {
			return true
		}
	}
	return false
}

// patternSegments splits a workspace pattern into its path segments. It
// refuses an absolute pattern, one that leaves the root, and one whose
// segments are not valid globs.
func patternSegments(pattern string) ([]string, bool) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || strings.HasPrefix(pattern, "/") || strings.ContainsRune(pattern, '\\') {
		return nil, false
	}
	var segments []string
	for _, segment := range strings.Split(pattern, "/") {
		switch segment {
		case "", ".":
			continue
		case "..":
			return nil, false
		}
		if segment != "**" {
			if _, err := path.Match(segment, ""); err != nil {
				return nil, false
			}
		}
		segments = append(segments, segment)
	}
	if len(segments) == 0 {
		return nil, false
	}
	return segments, true
}

func splitPath(rel string) []string {
	if rel == "." || rel == "" {
		return nil
	}
	return strings.Split(rel, "/")
}

// staticPrefix returns the leading segments of a pattern that hold no glob.
func staticPrefix(segments []string) []string {
	for i, segment := range segments {
		if segment == "**" || strings.ContainsAny(segment, "*?[") {
			return segments[:i]
		}
	}
	return segments
}

// maxBraceAlternatives bounds the patterns one brace pattern expands to.
const maxBraceAlternatives = 256

// expandBraces expands the {a,b} alternatives of a pattern, nested ones
// included. ok is false for an unbalanced brace or too many alternatives.
func expandBraces(pattern string) ([]string, bool) {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		return []string{pattern}, !strings.ContainsRune(pattern, '}')
	}
	depth, start := 0, open+1
	var parts []string
	for i := open; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case ',':
			if depth == 1 {
				parts = append(parts, pattern[start:i])
				start = i + 1
			}
		case '}':
			depth--
			if depth > 0 {
				continue
			}
			parts = append(parts, pattern[start:i])
			var out []string
			for _, part := range parts {
				expanded, ok := expandBraces(pattern[:open] + part + pattern[i+1:])
				if !ok {
					return nil, false
				}
				out = append(out, expanded...)
				if len(out) > maxBraceAlternatives {
					return nil, false
				}
			}
			return out, true
		}
	}
	return nil, false
}

// matchSegment matches one path segment. A wildcard never matches a
// dot-directory; a pattern segment that starts with a dot does.
func matchSegment(pattern, part string) bool {
	if strings.HasPrefix(part, ".") && !strings.HasPrefix(pattern, ".") {
		return false
	}
	matched, err := path.Match(pattern, part)
	return err == nil && matched
}

// matchSegments reports whether a path matches a pattern, where "**" matches
// any number of segments, none included, but no dot-directory.
func matchSegments(pattern, parts []string) bool {
	if len(pattern) == 0 {
		return len(parts) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(parts); i++ {
			if matchSegments(pattern[1:], parts[i:]) {
				return true
			}
			if i < len(parts) && strings.HasPrefix(parts[i], ".") {
				return false
			}
		}
		return false
	}
	return len(parts) > 0 && matchSegment(pattern[0], parts[0]) && matchSegments(pattern[1:], parts[1:])
}

// canMatchBelow reports whether a directory, or one below it, can match the
// pattern, so the walk skips the directories that cannot.
func canMatchBelow(pattern, parts []string) bool {
	for i, part := range parts {
		if i >= len(pattern) {
			return false
		}
		if pattern[i] == "**" {
			for _, rest := range parts[i:] {
				if strings.HasPrefix(rest, ".") {
					return false
				}
			}
			return true
		}
		if !matchSegment(pattern[i], part) {
			return false
		}
	}
	return true
}
