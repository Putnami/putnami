// Package memberglob matches the member globs of a workspace manifest, such
// as the members and exclude lists of a uv or Cargo workspace, against the
// directories of a repository.
package memberglob

import (
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Segments splits a members or exclude glob into its segments. A segment is a
// path.Match pattern, or ** for any number of directories. An absolute
// pattern, a .. segment or an invalid segment is refused.
func Segments(pattern string) ([]string, bool) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || strings.HasPrefix(pattern, "/") || filepath.IsAbs(pattern) || strings.Contains(pattern, "//") {
		return nil, false
	}
	pattern = path.Clean(pattern)
	if pattern == "." || pattern == ".." || strings.HasPrefix(pattern, "../") {
		return nil, false
	}
	segments := strings.Split(pattern, "/")
	for _, segment := range segments {
		if segment == ".." || segment == "" {
			return nil, false
		}
		if _, err := path.Match(segment, ""); err != nil {
			return nil, false
		}
	}
	return segments, true
}

// Dirs walks the repository once and returns, in path order, the directories
// below the root that one of the patterns matches. It never enters a
// directory skip names; bounded is true when it stopped after max entries.
func Dirs(root string, patterns [][]string, skip func(name string) bool, max int) (dirs []string, bounded bool) {
	if len(patterns) == 0 {
		return nil, false
	}
	walked := 0
	_ = filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.IsDir() {
			return nil
		}
		if p == root {
			return nil
		}
		walked++
		if walked > max {
			bounded = true
			return fs.SkipAll
		}
		if skip(entry.Name()) {
			return fs.SkipDir
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fs.SkipDir
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		below := false
		for _, segments := range patterns {
			if Match(segments, parts) {
				dirs = append(dirs, strings.Join(parts, "/"))
				break
			}
		}
		for _, segments := range patterns {
			below = below || canMatchBelow(segments, parts)
		}
		if !below {
			return fs.SkipDir
		}
		return nil
	})
	slices.Sort(dirs)
	return slices.Compact(dirs), bounded
}

// Match reports whether the path parts match the pattern segments, **
// matching any number of parts.
func Match(pattern, parts []string) bool {
	if len(pattern) == 0 {
		return len(parts) == 0
	}
	if pattern[0] == "**" {
		// ** matches no part, or one more part and then ** again.
		return Match(pattern[1:], parts) || (len(parts) > 0 && Match(pattern, parts[1:]))
	}
	if len(parts) == 0 {
		return false
	}
	ok, _ := path.Match(pattern[0], parts[0])
	return ok && Match(pattern[1:], parts[1:])
}

// canMatchBelow reports whether a directory below the path parts can still
// match the pattern segments.
func canMatchBelow(pattern, parts []string) bool {
	for i, part := range parts {
		if i >= len(pattern) {
			return false
		}
		if pattern[i] == "**" {
			return true
		}
		if ok, _ := path.Match(pattern[i], part); !ok {
			return false
		}
	}
	return len(pattern) > len(parts)
}
