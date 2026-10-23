package inputresolve

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
)

// selectedSourceLinks returns, as absolute paths, the Go source links that
// the query's ordinary patterns select under dir: each path an include
// matches whose extension is ".go", that is a symbolic link, and that no "!"
// pattern of the query excludes.
//
// It selects as the core selects a task input's ordinary patterns. An include
// with "**" walks the tree below its literal prefix without following a link
// (walkDoubleStar). Any other include is expanded by filepath.Glob, which
// follows a link it meets on the way; the basis refuses such a link
// (globListing). The core binds a selected Go source link and its
// referent, because the source scan skips the directories such a link can
// sit in.
func selectedSourceLinks(dir string, patterns []string) []string {
	includes, excludes := wsproto.SplitFilePatterns(patterns)
	seen := map[string]bool{}
	var links []string
	for _, include := range includes {
		var matches []string
		if strings.Contains(include, "**") {
			matches = walkDoubleStar(dir, include)
		} else {
			matches, _ = filepath.Glob(filepath.Join(dir, filepath.FromSlash(include)))
		}
		for _, match := range matches {
			if seen[match] || filepath.Ext(match) != ".go" {
				continue
			}
			info, err := os.Lstat(match)
			if err != nil || info.Mode()&fs.ModeSymlink == 0 {
				continue
			}
			rel, err := filepath.Rel(dir, match)
			if err != nil || wsproto.MatchesAnyFilePattern(filepath.ToSlash(rel), excludes) {
				continue
			}
			seen[match] = true
			links = append(links, match)
		}
	}
	sort.Strings(links)
	return links
}

// doubleStarPrefix is the literal directory an include with "**" is walked
// from: the text before its first "**", without surrounding slashes. The core
// reads it as a literal path, even when it holds a glob metacharacter.
func doubleStarPrefix(pattern string) string {
	before, _, _ := strings.Cut(filepath.ToSlash(pattern), "**")
	return strings.Trim(strings.TrimSuffix(before, "/"), "/")
}

// walkDoubleStar expands an include with "**" as the core does: it walks the
// tree under the include's literal prefix without following a link, skips
// node_modules, .git, .putnami, out, dist and vendor directories below that
// prefix, and returns every non-directory path the include matches. A walk
// error drops the subtree it hit.
func walkDoubleStar(dir, pattern string) []string {
	pattern = filepath.ToSlash(pattern)
	searchRoot := dir
	if prefix := doubleStarPrefix(pattern); prefix != "" {
		searchRoot = filepath.Join(dir, filepath.FromSlash(prefix))
	}
	var matches []string
	_ = filepath.WalkDir(searchRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p == searchRoot {
				return nil
			}
			switch d.Name() {
			case "node_modules", ".git", ".putnami", "out", "dist", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil
		}
		if wsproto.MatchFilePattern(filepath.ToSlash(rel), pattern) {
			matches = append(matches, p)
		}
		return nil
	})
	return matches
}
