// Package genresult defines how the generate phase's manifest —
// <project>/.gen/generate-result.json — records the paths it points at.
//
// The manifest is a CACHED artifact. The scheduler captures the whole .gen tree
// as build-generate's output and restores it into other worktrees, other
// machines and other CI runners, and the job's result payload is stored in the
// same cache entry. Bytes that depend on WHERE THE CHECKOUT LIVES can therefore
// never round-trip: two equivalent live runs in two verification worktrees
// disagree byte-for-byte, and a restored manifest names a directory that may not
// exist on the machine reading it.
//
// So every path the manifest serializes — in `exports`, in `assets`, in
// `schemas` — is PROJECT-RELATIVE and slash-separated, which is what `schemas`
// always was. Producers keep absolute paths in memory (visitors and pre-build
// hooks report them, and the runner hands them to the rest of the same process
// unchanged) and call Relativize once, at the boundary where a value leaves the
// process. Consumers call Resolve to get an absolute path back, against the
// project root they already know.
//
// Both language runtimes serialize the same manifest shape — the Go extension's
// codegen runner and the TypeScript extension's generate job — so the rule lives
// here, in the SDK they both already depend on, instead of twice.
package genresult

import (
	"path/filepath"
	"strings"
)

// Relativize returns a copy of paths whose values are in the manifest's on-disk
// form: project-relative and slash-separated.
//
// A value that is already relative is kept as-is (only its separators are
// normalized): a pre-build hook may report "./loader.js", and such a value is
// already checkout-independent, which is the only property this rewrite owes.
//
// A value that resolves OUTSIDE projectRoot is kept verbatim and absolute.
// There is no relocatable string for it — expressing it relative to the project
// would encode the checkout's own depth ("../../../tmp/x"), which is exactly the
// checkout-dependence this package exists to remove. Such a value means its
// producer named something the project does not own; keeping it verbatim leaves
// it findable and visible rather than silently rewritten into a wrong path.
//
// The returned map is never nil, so callers can marshal it without a nil check.
func Relativize(projectRoot string, paths map[string]string) map[string]string {
	out := make(map[string]string, len(paths))
	for key, value := range paths {
		out[key] = RelativizePath(projectRoot, value)
	}
	return out
}

// RelativizeList is Relativize for an ordered list of paths, such as the Go
// manifest's `schemas`. A nil list stays nil so the serialized shape of a
// project that produced nothing is unchanged.
func RelativizeList(projectRoot string, paths []string) []string {
	if paths == nil {
		return nil
	}
	out := make([]string, len(paths))
	for i, value := range paths {
		out[i] = RelativizePath(projectRoot, value)
	}
	return out
}

// RelativizePath rewrites one value into the manifest's on-disk form. See
// Relativize for the rules.
func RelativizePath(projectRoot, value string) string {
	if value == "" {
		return ""
	}
	if !filepath.IsAbs(value) {
		return filepath.ToSlash(value)
	}
	rel, err := filepath.Rel(projectRoot, value)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return value
	}
	return filepath.ToSlash(rel)
}

// Resolve returns a copy of paths with every value resolved to an absolute path
// against projectRoot. The returned map is never nil.
func Resolve(projectRoot string, paths map[string]string) map[string]string {
	out := make(map[string]string, len(paths))
	for key, value := range paths {
		out[key] = ResolvePath(projectRoot, value)
	}
	return out
}

// ResolvePath resolves one manifest value against projectRoot.
//
// An absolute value is returned as it stands. That is what makes a manifest
// written before the project-relative rule — or restored from a cache entry an older extension
// produced — still readable: a stale absolute path resolves to itself instead
// of being joined onto the project root into nonsense.
func ResolvePath(projectRoot, value string) string {
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(projectRoot, filepath.FromSlash(value))
}
