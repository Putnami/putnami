package workspace

import (
	"path"
	"path/filepath"
	"strings"
)

// A project declares its file inputs as `options.<layer>.filePatterns` in
// putnami.json, and two independent readers act on that one declaration: the
// build cache hashes the files a pattern set selects, and `--impacted` selects
// the declaring project when a changed file matches one. The grammar below is
// what both mean by a pattern.
//
// It is stated once here for the same reason ParseSelector states the
// project-selection grammar once: two dialects of one declaration drift, and
// the two directions of that drift are both silent. A file the cache hashes but
// selection ignores lands its change with the declaring project's jobs unrun; a
// file selection matches but the cache does not hash replays a stale verdict
// over it.
//
// The grammar:
//
//   - A pattern and a path are forward-slash relative paths, in the form
//     filepath.Rel produces against the project root — so an input OUTSIDE that
//     root keeps its "../" prefix, as in "../contributor/src/**".
//   - "*" matches inside one path segment, per path.Match.
//   - "**" matches any number of segments, including none.
//   - In an include without the "git:" prefix, no wildcard matches a ".."
//     segment: only a literal ".." does. The collector lists the files under
//     the pattern's static prefix, and a directory never lists "..", so "**"
//     selects the project's own tree and "../shared/**" the shared one, never
//     the whole workspace.
//   - A leading "!" makes the pattern an EXCLUSION: a path any exclusion
//     matches is not selected, whichever include also matched it.
//   - A "git:" prefix restricts collection to the containing repository's
//     tracked and non-ignored untracked candidates. Paths remain project-relative;
//     "git:**" selects the whole repository, including siblings and ancestors:
//     its wildcards match ".." segments.
//     Matching a changed path strips this prefix: deleted paths must still
//     select their readers, so impact never consults current Git membership.

// GitFilePattern separates a candidate input's source from its path pattern.
// The collector uses the source; impact uses the same path grammar as usual.
func GitFilePattern(pattern string) (string, bool) {
	return strings.CutPrefix(pattern, "git:")
}

// SplitFilePatterns partitions a declared pattern set into include patterns and
// exclude patterns — those written with a leading "!", returned with the marker
// stripped and normalized to forward slashes.
func SplitFilePatterns(patterns []string) (includes, excludes []string) {
	for _, p := range patterns {
		if excluded, ok := strings.CutPrefix(p, "!"); ok {
			excludes = append(excludes, filepath.ToSlash(excluded))
		} else {
			includes = append(includes, p)
		}
	}
	return includes, excludes
}

// MatchesAnyFilePattern reports whether the forward-slash relative path matches
// at least one of the patterns. It applies no exclusion rule of its own: a
// caller holding an unsplit declaration wants SelectsPath, and a caller that
// already split the two lists calls this on each and combines them itself.
func MatchesAnyFilePattern(rel string, patterns []string) bool {
	for _, pattern := range patterns {
		if MatchFilePattern(rel, pattern) {
			return true
		}
	}
	return false
}

// MatchFilePattern matches a forward-slash relative path against a single
// pattern, honoring "**" as a recursive segment wildcard.
//
// Its wildcards match ".." segments, which an exclusion relies on: "!**/gen/**"
// removes "../shared/gen/a.go" that "../shared/**" collected. SelectsPath
// matches an include through matchIncludePattern instead.
func MatchFilePattern(rel, pattern string) bool {
	return matchFilePattern(rel, pattern, true)
}

// matchIncludePattern matches a path against one include the way the
// collector selects files: a wildcard of an ordinary include never matches a
// ".." segment, and a "git:" include matches like MatchFilePattern.
func matchIncludePattern(rel, pattern string) bool {
	_, git := GitFilePattern(pattern)
	return matchFilePattern(rel, pattern, git)
}

// matchFilePattern matches rel against pattern. parentWildcard says whether
// "*" and "**" may match a ".." segment.
func matchFilePattern(rel, pattern string, parentWildcard bool) bool {
	pattern, _ = GitFilePattern(pattern)
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	if strings.Contains(pattern, "**") || !parentWildcard {
		return matchFilePatternSegments(strings.Split(pattern, "/"), strings.Split(rel, "/"), parentWildcard)
	}
	ok, _ := path.Match(pattern, rel)
	return ok
}

func matchFilePatternSegments(patterns, segments []string, parentWildcard bool) bool {
	if len(patterns) == 0 {
		return len(segments) == 0
	}
	if patterns[0] == "**" {
		for i := 0; i <= len(segments); i++ {
			if matchFilePatternSegments(patterns[1:], segments[i:], parentWildcard) {
				return true
			}
			// "**" goes on to consume segments[i]; a ".." stops it.
			if i < len(segments) && segments[i] == ".." && !parentWildcard {
				return false
			}
		}
		return false
	}
	if len(segments) == 0 {
		return false
	}
	if segments[0] == ".." && patterns[0] != ".." && !parentWildcard {
		return false
	}
	ok, _ := path.Match(patterns[0], segments[0])
	return ok && matchFilePatternSegments(patterns[1:], segments[1:], parentWildcard)
}

// ExcludesTree reports whether the exclusions remove every path at or under
// the forward-slash relative path rel, so that no include can select a file
// written there. It decides from the patterns alone: an exclusion "P/**"
// covers the tree when P matches rel or one of its ancestors, because "**"
// matches any rest, none included. Any other exclusion form is not taken to
// cover a tree, so the answer can be false for a tree that is in fact
// excluded, never true for one that is not.
func ExcludesTree(rel string, excludes []string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	if rel == "" || rel == "." {
		return false
	}
	segments := strings.Split(rel, "/")
	for _, exclude := range excludes {
		exclude, _ = GitFilePattern(exclude)
		exclude = strings.TrimPrefix(filepath.ToSlash(exclude), "./")
		if exclude == "**" {
			return true
		}
		prefix, ok := strings.CutSuffix(exclude, "/**")
		if !ok {
			continue
		}
		parts := strings.Split(prefix, "/")
		for k := 0; k <= len(segments); k++ {
			if matchFilePatternSegments(parts, segments[:k], true) {
				return true
			}
		}
	}
	return false
}

// SelectsPath reports whether a declared pattern set selects one named path,
// without touching the filesystem. An empty include list selects nothing: a
// caller that reads "no patterns" as "every file under the project root" — the
// cache hasher does — owns that reading, because it is a statement about a
// directory and this function is only ever asked about a path.
//
// An include selects the path the way the collector would: "**" selects
// "src/a.go" and never "../../docs/README.md", which the collector, walking the
// project root, never lists.
func SelectsPath(rel string, patterns []string) bool {
	includes, excludes := SplitFilePatterns(patterns)
	slashed := filepath.ToSlash(rel)
	if MatchesAnyFilePattern(slashed, excludes) {
		return false
	}
	for _, include := range includes {
		if matchIncludePattern(slashed, include) {
			return true
		}
	}
	return false
}
