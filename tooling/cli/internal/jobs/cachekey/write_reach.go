package cachekey

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	extproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/store"
)

// plannedWrite is a path a planned task may write: the file at Path, or, for a
// Tree, any path at or under it.
type plannedWrite struct {
	Path string
	Tree bool
	// region is Region once withRegion computed it.
	region *store.InputRegion
}

// Region is the region of every path the write can create, change or delete.
func (w plannedWrite) Region() store.InputRegion {
	if w.region != nil {
		return *w.region
	}
	return store.PathsRegion([]string{w.Path})
}

// withRegion returns w with its region computed once, for a planner that
// compares it with many selections.
func (w plannedWrite) withRegion() plannedWrite {
	region := w.Region()
	w.region = &region
	return w
}

// fileSelection is the set of files a key's file digest selects under one
// directory with one pattern set, kept to decide whether a planned write can
// change it. Region bounds it the way store.FileInputRegion does. Reaches is
// narrower: it is false when the patterns show that no file the write can
// touch is selected, for an ordinary include and its exclusions, and for an
// input selector, whose expansion's files and basis bound what it reads.
//
// The narrowing holds while every path the expansion's basis selects stays as
// it is: a caller that plans a write ReachesBasis reports true for decides
// with Widened instead. It also holds only for a write the digest
// reads under the spelling it declares, and for files the digest does not read
// through a link: a write that cannot be decided so reaches whatever the
// region says it reaches. A write that meets the region only through a link on
// a root of either is such a write.
type fileSelection struct {
	dir      string
	region   store.InputRegion
	bounded  bool
	patterns []string
	// includes are the ordinary include patterns, and excludes every
	// exclusion, which the digest applies to ordinary includes only.
	includes []string
	excludes []string
	// selectors are the input selectors, which expander expands.
	selectors []string
	expander  store.InputExpander
	scanner   *selectionScanner
}

// selectionScanner builds fileSelections and reads the expansion of one input
// selector, the links of one selection, the region of one pattern set and the
// spelling of one path once. The zero value is ready to use, from one
// goroutine.
type selectionScanner struct {
	expansions map[string]expansionResult
	links      map[string]bool
	regions    map[string]store.InputRegion
	spelled    map[string]bool
}

// expansionResult is one input selector's expansion, or the error that fails
// it.
type expansionResult struct {
	expansion store.InputExpansion
	err       error
}

// Select returns the selection of patterns under dir, whose input selectors
// expander expands. A pattern set with no pattern, a Git candidate pattern, an
// exclusion that names a selector, or a pattern that is not in clean slash
// form is bounded by its region alone.
func (s *selectionScanner) Select(dir string, patterns []string, expander store.InputExpander) fileSelection {
	selection := fileSelection{dir: dir, region: store.FileInputRegion(dir, patterns), patterns: patterns, expander: expander, scanner: s}
	if len(patterns) == 0 {
		return selection
	}
	includes, excludes := wsproto.SplitFilePatterns(patterns)
	for _, pattern := range append(append([]string(nil), includes...), excludes...) {
		if path.Clean(pattern) != pattern || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "\\") {
			return selection
		}
	}
	if slices.ContainsFunc(excludes, extproto.NamesInputSelector) {
		return selection
	}
	for _, pattern := range includes {
		if _, git := wsproto.GitFilePattern(pattern); git {
			return selection
		}
		if extproto.NamesInputSelector(pattern) {
			selection.selectors = append(selection.selectors, pattern)
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
	if !s.bounded || !s.region.OverlapsAsSpelled(written) {
		return true
	}
	rel, inside := s.relative(w.Path)
	if !inside {
		return true
	}
	for _, include := range s.includes {
		if s.includeReaches(include, rel, w, written) {
			return true
		}
	}
	for _, selector := range s.selectors {
		result := s.scanner.expand(s.dir, selector, s.expander)
		if result.err != nil || s.expansionReaches(result.expansion, rel, w, written) {
			return true
		}
	}
	return !s.scanner.spelledOnDisk(s.dir, rel) || s.scanner.selectsThroughLink(s.dir, s.patterns, s.expander)
}

// ReachesBasis reports whether w can change the expansion of an input selector
// of the selection: a path a content or listing set of its basis selects, or
// any path when the selector does not expand. Reaches holds for the expansion
// as it stands, so a caller widens such a selection.
func (s fileSelection) ReachesBasis(w plannedWrite) bool {
	written := w.Region()
	if !s.bounded || len(s.selectors) == 0 || !s.region.Overlaps(written) {
		return false
	}
	rel, inside := s.relative(w.Path)
	if !inside || !s.region.OverlapsAsSpelled(written) || !s.scanner.spelledOnDisk(s.dir, rel) {
		return true
	}
	for _, selector := range s.selectors {
		result := s.scanner.expand(s.dir, selector, s.expander)
		if result.err != nil || s.setsReach(result.expansion.Basis.Content, rel, w, written) ||
			s.setsReach(result.expansion.Basis.Listing, rel, w, written) {
			return true
		}
	}
	return false
}

// ReachesBasisIn is ReachesBasis for a write bounded by region alone.
func (s fileSelection) ReachesBasisIn(region store.InputRegion) bool {
	return s.bounded && len(s.selectors) > 0 && s.region.Overlaps(region)
}

// expansionReaches reports whether w, at rel with region written, can touch a
// file the expansion lists or a path a set of its basis selects.
func (s fileSelection) expansionReaches(expansion store.InputExpansion, rel string, w plannedWrite, written store.InputRegion) bool {
	for _, file := range expansion.Files {
		if file == rel || (w.Tree && strings.HasPrefix(file, rel+"/")) {
			return true
		}
	}
	return s.setsReach(expansion.Basis.Content, rel, w, written) || s.setsReach(expansion.Basis.Listing, rel, w, written)
}

// setsReach reports whether w, at rel with region written, can create, change
// or delete a path one of sets selects: rel itself, a directory above it the
// write may create, or, for a tree, a path under rel the set's region holds
// and its exclusions do not remove, as the basis digest walks it.
func (s fileSelection) setsReach(sets [][]string, rel string, w plannedWrite, written store.InputRegion) bool {
	for _, set := range sets {
		if wsproto.SelectsPath(rel, set) {
			return true
		}
		for parent := path.Dir(rel); parent != "."; parent = path.Dir(parent) {
			if wsproto.SelectsPath(parent, set) {
				return true
			}
		}
		if !w.Tree {
			continue
		}
		if _, excludes := wsproto.SplitFilePatterns(set); !wsproto.ExcludesTree(rel, excludes) && s.scanner.patternRegion(s.dir, set).Overlaps(written) {
			return true
		}
	}
	return false
}

// includeReaches reports whether w, whose region is written, can touch a file
// one ordinary include selects. Every path under w that an
// exclusion covers is out of reach, and a file write is selected only when the
// include matches it and no exclusion does, as the store's ordinary file
// collection decides. The collection spells a path's leading literal segments
// as the include does, so a write that spells them in another case, or that
// meets the include only through a link, is not decided by pattern.
func (s fileSelection) includeReaches(include, rel string, w plannedWrite, written store.InputRegion) bool {
	region := s.scanner.includeRegion(s.dir, include)
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
func (s *selectionScanner) selectsThroughLink(dir string, patterns []string, expander store.InputExpander) bool {
	key := dir + "\x00" + strings.Join(patterns, "\x00")
	if expander != nil {
		key += "\x00\x00" + expander.Identity()
	}
	if linked, ok := s.links[key]; ok {
		return linked
	}
	linked := store.SelectsThroughLink(dir, patterns, expander)
	if s.links == nil {
		s.links = make(map[string]bool)
	}
	s.links[key] = linked
	return linked
}

// includeRegion memoizes store.FileInputRegion for one ordinary include of dir.
func (s *selectionScanner) includeRegion(dir, include string) store.InputRegion {
	return s.patternRegion(dir, []string{include})
}

// patternRegion memoizes store.FileInputRegion for one pattern set of dir.
func (s *selectionScanner) patternRegion(dir string, patterns []string) store.InputRegion {
	key := dir + "\x00" + strings.Join(patterns, "\x00")
	if region, ok := s.regions[key]; ok {
		return region
	}
	region := store.FileInputRegion(dir, patterns)
	if s.regions == nil {
		s.regions = make(map[string]store.InputRegion)
	}
	s.regions[key] = region
	return region
}

// expand returns the expansion of one input selector of dir by expander, or
// the error that fails it: a selection without an expander fails every
// selector.
func (s *selectionScanner) expand(dir, selector string, expander store.InputExpander) expansionResult {
	if expander == nil {
		return expansionResult{err: store.ErrInputSelector}
	}
	key := dir + "\x00" + selector + "\x00" + expander.Identity()
	if result, ok := s.expansions[key]; ok {
		return result
	}
	expansion, err := expander.ExpandInput(dir, selector)
	result := expansionResult{expansion: expansion, err: err}
	if s.expansions == nil {
		s.expansions = make(map[string]expansionResult)
	}
	s.expansions[key] = result
	return result
}
