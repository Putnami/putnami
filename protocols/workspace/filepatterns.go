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
//   - A leading "!" makes the pattern an EXCLUSION: a path any exclusion
//     matches is not selected, whichever include also matched it.
//   - A "git:" prefix restricts collection to the containing repository's
//     tracked and non-ignored untracked candidates. Paths remain project-relative;
//     "git:**" selects the whole repository, including siblings and ancestors.
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
func MatchFilePattern(rel, pattern string) bool {
	pattern, _ = GitFilePattern(pattern)
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	if strings.Contains(pattern, "**") {
		return matchFilePatternSegments(strings.Split(pattern, "/"), strings.Split(rel, "/"))
	}
	ok, _ := path.Match(pattern, rel)
	return ok
}

func matchFilePatternSegments(patterns, segments []string) bool {
	if len(patterns) == 0 {
		return len(segments) == 0
	}
	if patterns[0] == "**" {
		if matchFilePatternSegments(patterns[1:], segments) {
			return true
		}
		for i := range segments {
			if matchFilePatternSegments(patterns[1:], segments[i+1:]) {
				return true
			}
		}
		return false
	}
	if len(segments) == 0 {
		return false
	}
	ok, _ := path.Match(patterns[0], segments[0])
	return ok && matchFilePatternSegments(patterns[1:], segments[1:])
}

// SelectsPath reports whether a declared pattern set selects one named path,
// without touching the filesystem. An empty include list selects nothing: a
// caller that reads "no patterns" as "every file under the project root" — the
// cache hasher does — owns that reading, because it is a statement about a
// directory and this function is only ever asked about a path.
func SelectsPath(rel string, patterns []string) bool {
	includes, excludes := SplitFilePatterns(patterns)
	slashed := filepath.ToSlash(rel)
	if MatchesAnyFilePattern(slashed, excludes) {
		return false
	}
	return MatchesAnyFilePattern(slashed, includes)
}
