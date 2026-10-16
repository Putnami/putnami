package cachekey

import (
	"path/filepath"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/store"
)

// plannedWrite is a path a planned task may write: the file at Path, or, for a
// Tree, any path at or under it.
type plannedWrite struct {
	Path string
	Tree bool
}

// Region is the region of every path the write can create, change or delete.
func (w plannedWrite) Region() store.InputRegion {
	return store.PathsRegion([]string{w.Path})
}

// fileSelection is the set of files a key's file digest selects under one
// directory with one pattern set, kept to decide whether a planned write can
// change it. Region bounds it the way store.FileInputRegion does. Reaches is
// narrower: it is false when the patterns show that no file the write can
// touch is selected, for an ordinary include and its exclusions, and for a Go
// embed selector, whose sources and embed directives bound what it reads.
//
// The narrowing holds while the embed directives stay as they are scanned. A
// caller that plans a write ReachesDirectives reports true for decides with
// Widened instead.
type fileSelection struct {
	dir     string
	region  store.InputRegion
	bounded bool
	// includes are the ordinary include patterns, and excludes every
	// exclusion, which the digest applies to ordinary includes only.
	includes []string
	excludes []string
	// embeds are the Go embed selectors.
	embeds  []string
	scanner *selectionScanner
}

// selectionScanner builds fileSelections and scans the Go embed directives of
// one directory once. The zero value is ready to use, from one goroutine.
type selectionScanner struct {
	embeds map[embedScanKey]store.GoEmbedBound
}

type embedScanKey struct {
	dir      string
	selector string
}

// Select returns the selection of patterns under dir. A pattern set with no
// pattern or a Git candidate pattern is bounded by its region alone.
func (s *selectionScanner) Select(dir string, patterns []string) fileSelection {
	selection := fileSelection{dir: dir, region: store.FileInputRegion(dir, patterns), scanner: s}
	if len(patterns) == 0 {
		return selection
	}
	includes, excludes := wsproto.SplitFilePatterns(patterns)
	for _, pattern := range includes {
		if _, git := wsproto.GitFilePattern(pattern); git {
			return selection
		}
		if strings.HasPrefix(pattern, "go-embed:") {
			selection.embeds = append(selection.embeds, pattern)
		} else {
			selection.includes = append(selection.includes, pattern)
		}
	}
	selection.excludes = excludes
	selection.bounded = true
	return selection
}

// regionSelection is a selection bounded by region alone.
func regionSelection(region store.InputRegion) fileSelection {
	return fileSelection{region: region}
}

// Region bounds every file the selection can select.
func (s fileSelection) Region() store.InputRegion {
	return s.region
}

// Widened returns the selection bounded by its region alone.
func (s fileSelection) Widened() fileSelection {
	return regionSelection(s.region)
}

// Reaches reports whether w can create, change or delete a file the selection
// selects, or change which files it selects.
func (s fileSelection) Reaches(w plannedWrite) bool {
	written := w.Region()
	if !s.region.Overlaps(written) {
		return false
	}
	if !s.bounded {
		return true
	}
	rel, inside := s.relative(w.Path)
	if !inside {
		return true
	}
	for _, include := range s.includes {
		if s.includeReaches(include, rel, w) {
			return true
		}
	}
	for _, selector := range s.embeds {
		bound := s.scanner.embed(s.dir, selector)
		if !bound.Bounded || store.ReachesGoSource(rel, w.Tree) || bound.Assets.Overlaps(written) {
			return true
		}
	}
	return false
}

// ReachesDirectives reports whether w can change the Go sources an embed
// selector of the selection reads its directives from, or which sources it
// reads: the bound Reaches takes for that selector.
func (s fileSelection) ReachesDirectives(w plannedWrite) bool {
	if !s.bounded || len(s.embeds) == 0 || !s.region.Overlaps(w.Region()) {
		return false
	}
	rel, inside := s.relative(w.Path)
	return !inside || store.ReachesGoSource(rel, w.Tree)
}

// ReachesDirectivesIn is ReachesDirectives for a write bounded by region alone.
func (s fileSelection) ReachesDirectivesIn(region store.InputRegion) bool {
	return s.bounded && len(s.embeds) > 0 && s.region.Overlaps(region)
}

// includeReaches reports whether w can touch a file one ordinary include
// selects. Every path under w that an exclusion covers is out of reach, and a
// file write is selected only when the include matches it and no exclusion
// does, as the store's ordinary file collection decides.
func (s fileSelection) includeReaches(include, rel string, w plannedWrite) bool {
	if !store.FileInputRegion(s.dir, []string{include}).Overlaps(w.Region()) {
		return false
	}
	if wsproto.ExcludesTree(rel, s.excludes) {
		return false
	}
	if w.Tree {
		return true
	}
	return wsproto.MatchFilePattern(rel, include) && !wsproto.MatchesAnyFilePattern(rel, s.excludes)
}

// relative returns p relative to the selection's directory in slash form, and
// false when p is the directory, above it or outside it: such a write reaches
// whatever the region says it reaches.
func (s fileSelection) relative(p string) (string, bool) {
	dir, err := filepath.Abs(s.dir)
	if err != nil {
		return "", false
	}
	target, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// embed returns the bound of one Go embed selector of dir.
func (s *selectionScanner) embed(dir, selector string) store.GoEmbedBound {
	key := embedScanKey{dir: dir, selector: selector}
	if bound, ok := s.embeds[key]; ok {
		return bound
	}
	bound := store.ScanGoEmbedBound(dir, selector)
	if s.embeds == nil {
		s.embeds = make(map[embedScanKey]store.GoEmbedBound)
	}
	s.embeds[key] = bound
	return bound
}
