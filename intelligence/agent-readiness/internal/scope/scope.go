// Package scope decides which tracked files describe code people write. The
// collector counts lines and churn only for those: vendored dependencies,
// build output, lockfiles and generated files would drown the signal.
package scope

import (
	"context"
	"path"
	"sort"
	"strings"

	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
)

// skippedDirs are directory names whose content no one writes by hand:
// installed or vendored dependencies, a package manager's own release, and
// build output.
var skippedDirs = map[string]bool{
	"node_modules":     true,
	"vendor":           true,
	"vendors":          true,
	"third_party":      true,
	"bower_components": true,
	".yarn":            true,
	".gen":             true,
	"dist":             true,
}

// ExcludePathspecs are the git pathspecs that leave every skipped directory
// out of a git command, at any depth.
func ExcludePathspecs() []string {
	specs := make([]string, 0, len(skippedDirs))
	for dir := range skippedDirs {
		specs = append(specs, ":(exclude,glob)**/"+dir+"/**")
	}
	sort.Strings(specs)
	return specs
}

// Lockfiles are the dependency lockfiles the collector recognizes. They are
// written by tools, so they never count as code.
var Lockfiles = []string{
	"Cargo.lock", "Gemfile.lock", "Pipfile.lock", "bun.lock", "bun.lockb", "composer.lock",
	"go.sum", "gradle.lockfile", "package-lock.json", "pnpm-lock.yaml", "poetry.lock",
	"putnami.lock.json", "uv.lock", "yarn.lock",
}

var lockfileNames = func() map[string]bool {
	names := map[string]bool{}
	for _, name := range Lockfiles {
		names[name] = true
	}
	return names
}()

// generatedMarker matches the header generators write: Go's "Code generated
// ... DO NOT EDIT." line, and a comment that opens with the "@generated" tag
// other ecosystems use.
const generatedMarker = `^// Code generated .* DO NOT EDIT\.$|^[[:space:]]*(//|#|/?\*+)[[:space:]]*@generated`

// Scope holds the generated files found at HEAD.
type Scope struct {
	generated map[string]bool
}

// Load finds the generated files at HEAD in one git grep pass.
func Load(ctx context.Context, repo *gitrepo.Repo) (*Scope, error) {
	matches, err := repo.Grep(ctx, generatedMarker)
	if err != nil {
		return nil, err
	}
	generated := map[string]bool{}
	for _, match := range matches {
		generated[match.Path] = true
	}
	return &Scope{generated: generated}, nil
}

// Authored reports whether a path holds code someone writes by hand.
func (s *Scope) Authored(file string) bool {
	if s != nil && s.generated[file] {
		return false
	}
	return !Skipped(file)
}

// Skipped reports whether a path is excluded by its name alone: a lockfile, a
// minified bundle, or a file under a vendored or build-output directory.
func Skipped(file string) bool {
	base := path.Base(file)
	if lockfileNames[base] || strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.css") {
		return true
	}
	return InSkippedDir(file)
}

// InSkippedDir reports whether a path lies under a vendored or build-output
// directory.
func InSkippedDir(file string) bool {
	for _, segment := range strings.Split(path.Dir(file), "/") {
		if skippedDirs[segment] {
			return true
		}
	}
	return false
}
