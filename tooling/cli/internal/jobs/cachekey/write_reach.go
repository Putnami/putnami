package cachekey

import (
	"errors"
	"io/fs"
	"os"
	"path"
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
// Widened instead. It also holds only for a write the digest reads under the
// spelling it declares, and for files the digest does not read through a link:
// a write that cannot be decided so reaches whatever the region says it
// reaches. A write that meets the region only through a link on a root of
// either is such a write.
type fileSelection struct {
	dir      string
	region   store.InputRegion
	bounded  bool
	patterns []string
	// includes are the ordinary include patterns, includeRegions the region
	// of each, and excludes every exclusion, which the digest applies to
	// ordinary includes only.
	includes       []string
	includeRegions []store.InputRegion
	excludes       []string
	// embeds are the Go embed selectors.
	embeds  []string
	scanner *selectionScanner
}

// selectionScanner builds fileSelections and scans the Go embed directives of
// one directory, the links of one selection and the spelling of one path once.
// The zero value is ready to use, from one goroutine.
type selectionScanner struct {
	embeds  map[embedScanKey]store.GoEmbedBound
	links   map[string]bool
	spelled map[string]bool
}

type embedScanKey struct {
	dir      string
	selector string
}

// Select returns the selection of patterns under dir. A pattern set with no
// pattern, a Git candidate pattern, or a pattern that is not in clean slash
// form is bounded by its region alone.
func (s *selectionScanner) Select(dir string, patterns []string) fileSelection {
	selection := fileSelection{dir: dir, region: store.FileInputRegion(dir, patterns), patterns: patterns, scanner: s}
	if len(patterns) == 0 {
		return selection
	}
	includes, excludes := wsproto.SplitFilePatterns(patterns)
	for _, pattern := range append(append([]string(nil), includes...), excludes...) {
		if path.Clean(pattern) != pattern || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "\\") {
			return selection
		}
	}
	for _, pattern := range includes {
		if _, git := wsproto.GitFilePattern(pattern); git {
			return selection
		}
		if strings.HasPrefix(pattern, "go-embed:") {
			selection.embeds = append(selection.embeds, pattern)
		} else {
			selection.includes = append(selection.includes, pattern)
			selection.includeRegions = append(selection.includeRegions, store.FileInputRegion(dir, []string{pattern}))
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
	if !s.bounded || !s.region.OverlapsAsSpelled(written) {
		return true
	}
	rel, inside := s.relative(w.Path)
	if !inside {
		return true
	}
	for i, include := range s.includes {
		if s.includeReaches(include, s.includeRegions[i], rel, w, written) {
			return true
		}
	}
	for _, selector := range s.embeds {
		bound := s.scanner.embed(s.dir, selector)
		if !bound.Bounded || store.ReachesGoSource(rel, w.Tree) || bound.Assets.Overlaps(written) {
			return true
		}
	}
	return !s.scanner.spelledOnDisk(s.dir, rel) || s.scanner.selectsThroughLink(s.dir, s.patterns)
}

// ReachesDirectives reports whether w can change the Go sources an embed
// selector of the selection reads its directives from, or which sources it
// reads: the bound Reaches takes for that selector.
func (s fileSelection) ReachesDirectives(w plannedWrite) bool {
	written := w.Region()
	if !s.bounded || len(s.embeds) == 0 || !s.region.Overlaps(written) {
		return false
	}
	rel, inside := s.relative(w.Path)
	return !inside || !s.region.OverlapsAsSpelled(written) || store.ReachesGoSource(rel, w.Tree) || !s.scanner.spelledOnDisk(s.dir, rel)
}

// ReachesDirectivesIn is ReachesDirectives for a write bounded by region alone.
func (s fileSelection) ReachesDirectivesIn(region store.InputRegion) bool {
	return s.bounded && len(s.embeds) > 0 && s.region.Overlaps(region)
}

// includeReaches reports whether w, whose region is written, can touch a file
// one ordinary include, bounded by region, selects. Every path under w that an
// exclusion covers is out of reach, and a file write is selected only when the
// include matches it and no exclusion does, as the store's ordinary file
// collection decides. The collection spells a path's leading literal segments
// as the include does, so a write that spells them in another case, or that
// meets the include only through a link, is not decided by pattern.
func (s fileSelection) includeReaches(include string, region store.InputRegion, rel string, w plannedWrite, written store.InputRegion) bool {
	if !region.Overlaps(written) {
		return false
	}
	if !region.OverlapsAsSpelled(written) || !literalPrefixAgrees(include, rel) {
		return true
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

// literalPrefixAgrees reports whether rel spells the leading literal segments
// of pattern as the pattern does wherever the two compare equal without case.
func literalPrefixAgrees(pattern, rel string) bool {
	segments := strings.Split(rel, "/")
	for i, part := range strings.Split(pattern, "/") {
		if i >= len(segments) || strings.ContainsAny(part, "*?[\\") {
			return true
		}
		if part != segments[i] {
			return !strings.EqualFold(part, segments[i])
		}
	}
	return true
}

// spelledOnDisk reports whether every segment of rel that exists under dir
// exists under that exact name, so the digest reads the path as rel spells it.
// A segment that does not exist yet is created as rel spells it.
func (s *selectionScanner) spelledOnDisk(dir, rel string) bool {
	key := filepath.Join(dir, filepath.FromSlash(rel))
	if spelled, ok := s.spelled[key]; ok {
		return spelled
	}
	spelled := segmentsSpelledOnDisk(dir, rel)
	if s.spelled == nil {
		s.spelled = make(map[string]bool)
	}
	s.spelled[key] = spelled
	return spelled
}

func segmentsSpelledOnDisk(dir, rel string) bool {
	parent := dir
	for _, name := range strings.Split(rel, "/") {
		child := filepath.Join(parent, name)
		if _, err := os.Lstat(child); errors.Is(err, fs.ErrNotExist) {
			return true
		} else if err != nil {
			return false
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			return false
		}
		found := false
		for _, entry := range entries {
			if entry.Name() == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
		parent = child
	}
	return true
}

// selectsThroughLink memoizes store.SelectsThroughLink for one selection.
func (s *selectionScanner) selectsThroughLink(dir string, patterns []string) bool {
	key := dir + "\x00" + strings.Join(patterns, "\x00")
	if linked, ok := s.links[key]; ok {
		return linked
	}
	linked := store.SelectsThroughLink(dir, patterns)
	if s.links == nil {
		s.links = make(map[string]bool)
	}
	s.links[key] = linked
	return linked
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
