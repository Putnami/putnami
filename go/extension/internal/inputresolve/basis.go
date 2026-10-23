package inputresolve

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/goembed"
)

// ignoredSourceExclusions remove from a basis set the paths the Go source
// scan skips: anything under testdata or vendor, and anything whose name, or
// whose directory's name, starts with "." or "_" (goembed.IgnoresSource).
var ignoredSourceExclusions = []string{"!**/testdata/**", "!**/vendor/**", "!**/.*/**", "!**/_*/**"}

// goModName matches go.mod in any ASCII letter case. The scan looks a go.mod
// up by name, and a case-insensitive file system answers that lookup for
// GO.MOD too, so the basis lists every spelling.
const goModName = "[gG][oO].[mM][oO][dD]"

// globMetacharacters are the bytes filepath.Glob reads as a pattern.
const globMetacharacters = `*?[\`

// starRun matches a run of "*". In one path.Match segment a run means what a
// single "*" means, but a whole "**" segment of a basis pattern matches any
// depth, so a directive copied into a basis keeps a single "*".
var starRun = regexp.MustCompile(`\*+`)

// resolution is one successful selection and what its basis is built from.
type resolution struct {
	// dir is the absolute project directory.
	dir string
	// includeTests is true for go-embed:test.
	includeTests bool
	// patterns are the query's ordinary patterns.
	patterns []string
	// files are the answer's files, relative slash paths, sorted.
	files []string
	// directives are the scan's //go:embed patterns, by lexical source path.
	directives map[string][]string
	// spelling caches directory listings for checkSpelling.
	spelling spellingCache
}

// basis checks every read the basis digest cannot see, and returns the
// basis of the selection.
//
// Content sets: the Go sources the scan reads, and the referent of each Go
// source link the answer lists. Listing sets: each go.mod the scan looks up;
// what each //go:embed directive matches and the directories it is resolved
// through; what the query's ordinary patterns select, without and then with
// their exclusions; the directories each referent is reached through; and the
// go.mod of each directory between the project and a listed file.
func (r *resolution) basis() (wsproto.InputBasis, error) {
	if err := r.checkScanModules(); err != nil {
		return wsproto.InputBasis{}, err
	}
	queryListing, err := r.queryListing()
	if err != nil {
		return wsproto.InputBasis{}, err
	}
	directiveListing, err := r.directiveListing()
	if err != nil {
		return wsproto.InputBasis{}, err
	}
	referents, referentListing, err := r.linkReferents()
	if err != nil {
		return wsproto.InputBasis{}, err
	}
	ancestors := r.fileAncestors()
	if err := r.checkFileModules(ancestors); err != nil {
		return wsproto.InputBasis{}, err
	}

	sources := append([]string{"**/*.go"}, ignoredSourceExclusions...)
	if !r.includeTests {
		sources = append(sources, "!**/*_test.go")
	}
	content := [][]string{sources}
	if len(referents) > 0 {
		content = append(content, referents)
	}

	modules := append([]string{"**/" + goModName}, ignoredSourceExclusions...)
	listing := [][]string{modules}
	if includes, _ := wsproto.SplitFilePatterns(r.patterns); len(includes) > 0 {
		listing = append(listing, append([]string(nil), r.patterns...))
	}
	literals := append(append(queryListing, directiveListing...), referentListing...)
	for _, ancestor := range ancestors {
		if !wsproto.SelectsPath(ancestor+"/go.mod", modules) {
			literals = append(literals, literalPattern(ancestor, false)+"/"+goModName)
		}
	}
	if literals = sortedUnique(literals); len(literals) > 0 {
		listing = append(listing, literals)
	}
	return wsproto.InputBasis{Content: content, Listing: listing}, nil
}

// checkScanModules walks the tree as the source scan walks it and refuses a
// go.mod that is a link in a directory the scan visits. The scan stats that
// go.mod, following the link, while the digest records only the link's
// target text.
func (r *resolution) checkScanModules() error {
	return filepath.WalkDir(r.dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == r.dir {
			return nil
		}
		rel, err := filepath.Rel(r.dir, p)
		if err != nil {
			return err
		}
		if goembed.IgnoresSource(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		present, err := r.moduleFile(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		if present {
			return filepath.SkipDir
		}
		return nil
	})
}

// moduleFile reports whether the project directory rel holds a go.mod, and
// refuses a go.mod that is a link.
func (r *resolution) moduleFile(rel string) (bool, error) {
	name := path.Join(rel, "go.mod")
	info, err := os.Lstat(filepath.Join(r.dir, filepath.FromSlash(name)))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case info.Mode()&fs.ModeSymlink != 0:
		return false, fmt.Errorf("%s is a symbolic link: whether it marks a module depends on its target, which the basis cannot read", name)
	}
	return true, nil
}

// fileAncestors returns every directory, other than the project itself,
// that holds a listed file, directly or below, sorted.
func (r *resolution) fileAncestors() []string {
	seen := map[string]bool{}
	for _, file := range r.files {
		for dir := path.Dir(file); dir != "." && !seen[dir]; dir = path.Dir(dir) {
			seen[dir] = true
		}
	}
	ancestors := make([]string, 0, len(seen))
	for dir := range seen {
		ancestors = append(ancestors, dir)
	}
	sort.Strings(ancestors)
	return ancestors
}

// checkFileModules refuses a go.mod in a directory between the project and a
// listed file. goembed rejects an embedded file or a link referent that
// crosses a nested module, and the scan never lists a source below one, so
// such a go.mod is a link, which the scan follows and the digest does not, or
// a write that landed during the scan.
func (r *resolution) checkFileModules(ancestors []string) error {
	for _, dir := range ancestors {
		present, err := r.moduleFile(dir)
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("%s/go.mod marks a nested module that a listed file sits in", dir)
		}
	}
	return nil
}

// queryListing checks how the query's includes reach the tree and returns
// the patterns that list the directories they are resolved through.
//
// An include with "**" is walked from its literal prefix, which the core
// opens by name. Any other include is expanded by filepath.Glob, which opens
// its leading literal segments by name, lists each directory a later segment
// is matched in, and follows a link on the way.
func (r *resolution) queryListing() ([]string, error) {
	includes, _ := wsproto.SplitFilePatterns(r.patterns)
	var patterns []string
	for _, include := range includes {
		if strings.Contains(include, "**") {
			listed, err := r.doubleStarListing(include)
			if err != nil {
				return nil, err
			}
			patterns = append(patterns, listed...)
			continue
		}
		listed, err := r.globListing(include)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, listed...)
	}
	return patterns, nil
}

// doubleStarListing checks the literal prefix of an include with "**": no
// directory on it is a link, each one present is spelled as on disk, and its
// letters are ASCII. It lists each directory of the prefix in any letter
// case, so creating one changes the digest.
func (r *resolution) doubleStarListing(include string) ([]string, error) {
	prefix := doubleStarPrefix(include)
	if prefix == "" {
		return nil, nil
	}
	if err := checkASCIILiteral(include, prefix); err != nil {
		return nil, err
	}
	segments := strings.Split(prefix, "/")
	patterns := make([]string, 0, len(segments))
	for k := range segments {
		patterns = append(patterns, literalPattern(path.Join(segments[:k+1]...), true))
	}
	for k := range segments {
		rel := path.Join(segments[:k+1]...)
		info, err := os.Lstat(filepath.Join(r.dir, filepath.FromSlash(rel)))
		if err != nil {
			break
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, fmt.Errorf("pattern %q is walked through symbolic link %s, which the basis cannot read through", include, rel)
		}
		if err := r.checkSpelling(rel); err != nil {
			return nil, fmt.Errorf("pattern %q: %w", include, err)
		}
		if !info.IsDir() {
			break
		}
	}
	return patterns, nil
}

// globListing checks an include without "**": the project directory holds no
// glob metacharacter, no path a proper prefix matches is a link, and its
// leading literal segments are ASCII and spelled as on disk. It lists every
// proper prefix, with the leading literal segments in any letter case, and
// the whole include in any letter case when it is all literal.
func (r *resolution) globListing(include string) ([]string, error) {
	if strings.ContainsAny(r.dir, globMetacharacters) {
		return nil, fmt.Errorf("pattern %q is expanded under project directory %s, whose glob metacharacters filepath.Glob reads as a pattern", include, r.dir)
	}
	segments := strings.Split(include, "/")
	run := literalRun(segments)
	if run > 0 {
		literal := path.Join(segments[:run]...)
		if err := checkASCIILiteral(include, literal); err != nil {
			return nil, err
		}
		if err := r.checkSpelling(literal); err != nil {
			return nil, fmt.Errorf("pattern %q: %w", include, err)
		}
	}
	if link := linkOnPrefixes(r.dir, segments); link != "" {
		return nil, fmt.Errorf("pattern %q is expanded through symbolic link %s, which the basis cannot read through", include, projectPath(r.dir, link))
	}
	last := len(segments) - 1
	if run == len(segments) {
		last = len(segments)
	}
	patterns := make([]string, 0, last)
	for k := 1; k <= last; k++ {
		parts := make([]string, k)
		for i, segment := range segments[:k] {
			if i < run {
				segment = literalPattern(segment, true)
			}
			parts[i] = segment
		}
		patterns = append(patterns, path.Join(parts...))
	}
	return patterns, nil
}

// directiveListing checks how each //go:embed directive reaches the tree and
// returns the patterns that list it: what the directive matches and
// everything below it, and each path a proper prefix of it matches.
// filepath.Glob resolves a directive under its package directory: it reads
// that directory's path as a pattern, opens the directive's leading literal
// segments by name, and follows a link a proper prefix matches.
func (r *resolution) directiveListing() ([]string, error) {
	sources := make([]string, 0, len(r.directives))
	for source := range r.directives {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	var patterns []string
	for _, source := range sources {
		pkg := path.Dir(source)
		pkgDir := filepath.Join(r.dir, filepath.FromSlash(pkg))
		if strings.ContainsAny(pkgDir, globMetacharacters) {
			return nil, fmt.Errorf("%s: package directory %s holds a glob metacharacter, which go:embed resolution reads as a pattern", source, pkgDir)
		}
		base := ""
		if pkg != "." {
			base = literalPattern(pkg, false) + "/"
		}
		for _, declared := range r.directives[source] {
			static, err := goembed.StaticPrefix(declared)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", source, err)
			}
			if static != "" {
				if err := r.checkSpelling(path.Join(pkg, static)); err != nil {
					return nil, fmt.Errorf("%s: go:embed %s: %w", source, declared, err)
				}
			}
			pattern := starRun.ReplaceAllString(strings.TrimPrefix(declared, "all:"), "*")
			segments := strings.Split(pattern, "/")
			if link := linkOnPrefixes(pkgDir, segments); link != "" {
				return nil, fmt.Errorf("%s: go:embed %s is resolved through symbolic link %s, which the basis cannot read through",
					source, declared, projectPath(r.dir, link))
			}
			patterns = append(patterns, guardPrefix(base+pattern+"/**"))
			for k := 1; k < len(segments); k++ {
				patterns = append(patterns, guardPrefix(base+path.Join(segments[:k]...)))
			}
		}
	}
	return patterns, nil
}

// linkReferents checks each listed file that is a Go source link and returns
// the content patterns of their referents and the listing patterns of the
// directories each referent is reached through. A link target that names a
// directory and then climbs out of it with ".." is refused: the scan resolves
// it through that directory, which may be a link, while the referent's path
// skips it. A referent that is not spelled as on disk is refused.
func (r *resolution) linkReferents() ([]string, []string, error) {
	var referents, listing []string
	for _, file := range r.files {
		abs := filepath.Join(r.dir, filepath.FromSlash(file))
		info, err := os.Lstat(abs)
		if err != nil {
			return nil, nil, err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(abs)
		if err != nil {
			return nil, nil, err
		}
		if climbsOutOfANamedDirectory(target) {
			return nil, nil, fmt.Errorf("go source link %s targets %q, which names a directory and climbs out of it; the basis cannot read through that directory", file, target)
		}
		referent, err := goembed.SourceReferent(r.dir, abs)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve Go source link %s: %w", file, err)
		}
		rel := projectPath(r.dir, referent)
		if err := r.checkSpelling(rel); err != nil {
			return nil, nil, fmt.Errorf("go source link %s: %w", file, err)
		}
		referents = append(referents, literalPattern(rel, false))
		segments := strings.Split(rel, "/")
		for k := 1; k < len(segments); k++ {
			listing = append(listing, literalPattern(path.Join(segments[:k]...), false))
		}
	}
	return sortedUnique(referents), listing, nil
}

// climbsOutOfANamedDirectory reports whether a link target has a ".."
// segment after a segment that names a directory. Leading ".." segments
// climb from the link's own directory, which the scan reached without a
// link.
func climbsOutOfANamedDirectory(target string) bool {
	named := false
	for _, segment := range strings.Split(filepath.ToSlash(target), "/") {
		switch segment {
		case "", ".":
		case "..":
			if named {
				return true
			}
		default:
			named = true
		}
	}
	return false
}

// linkOnPrefixes returns the first path under base that a proper prefix of
// the pattern segments matches, the way filepath.Glob reaches it, and that is
// a link, or "". Glob opens the leading literal segments by name, then
// matches each later segment against a directory's entries; it follows a
// link at any of those levels.
func linkOnPrefixes(base string, segments []string) string {
	run := literalRun(segments)
	dirs := []string{base}
	for level := 0; level < len(segments)-1 && len(dirs) > 0; level++ {
		var next []string
		for _, dir := range dirs {
			var candidates []string
			if level < run {
				candidates = []string{filepath.Join(dir, segments[level])}
			} else {
				entries, err := os.ReadDir(dir)
				if err != nil {
					continue
				}
				for _, entry := range entries {
					if ok, _ := path.Match(segments[level], entry.Name()); ok {
						candidates = append(candidates, filepath.Join(dir, entry.Name()))
					}
				}
			}
			for _, candidate := range candidates {
				info, err := os.Lstat(candidate)
				if err != nil {
					continue
				}
				if info.Mode()&fs.ModeSymlink != 0 {
					return candidate
				}
				if info.IsDir() {
					next = append(next, candidate)
				}
			}
		}
		dirs = next
	}
	return ""
}

// literalRun counts the leading segments that hold no glob metacharacter.
func literalRun(segments []string) int {
	for i, segment := range segments {
		if strings.ContainsAny(segment, globMetacharacters) {
			return i
		}
	}
	return len(segments)
}

// checkASCIILiteral refuses a literal directory of a query include with a
// byte outside ASCII. The basis lists a literal it opens by name in every
// ASCII letter case; it cannot list the Unicode case and normalization
// variants a file system may resolve that name to.
func checkASCIILiteral(include, literal string) error {
	for i := 0; i < len(literal); i++ {
		if literal[i] >= 0x80 {
			return fmt.Errorf("pattern %q opens %q by name, and the basis cannot list the Unicode spellings a file system resolves it to", include, literal)
		}
	}
	return nil
}

// spellingCache holds the entry names of the directories checkSpelling
// listed.
type spellingCache map[string]map[string]bool

// checkSpelling requires each component of rel that exists to be named on
// disk exactly as rel spells it. A case-insensitive file system opens a name
// in any case, while the digest matches names exactly; a missing component
// ends the check, because nothing was read through it.
func (r *resolution) checkSpelling(rel string) error {
	parent := r.dir
	for _, name := range strings.Split(rel, "/") {
		current := filepath.Join(parent, name)
		if _, err := os.Lstat(current); err != nil {
			return nil
		}
		names, ok := r.spelling[parent]
		if !ok {
			entries, err := os.ReadDir(parent)
			if err != nil {
				return err
			}
			names = make(map[string]bool, len(entries))
			for _, entry := range entries {
				names[entry.Name()] = true
			}
			r.spelling[parent] = names
		}
		if !names[name] {
			return fmt.Errorf("the file system opens %s under another spelling, and the basis matches names exactly", projectPath(r.dir, current))
		}
		parent = current
	}
	return nil
}

// literalPattern turns a relative slash path into a basis pattern that
// matches exactly that path: each glob metacharacter becomes a one-character
// class, and guardPrefix guards its first byte. With fold, each ASCII letter
// becomes a class of both its cases.
func literalPattern(rel string, fold bool) string {
	var b strings.Builder
	for i := 0; i < len(rel); i++ {
		c := rel[i]
		switch {
		case c == '*' || c == '?' || c == '[':
			b.WriteByte('[')
			b.WriteByte(c)
			b.WriteByte(']')
		case fold && c >= 'a' && c <= 'z':
			b.Write([]byte{'[', c, c - 'a' + 'A', ']'})
		case fold && c >= 'A' && c <= 'Z':
			b.Write([]byte{'[', c - 'A' + 'a', c, ']'})
		default:
			b.WriteByte(c)
		}
	}
	return guardPrefix(b.String())
}

// guardPrefix writes the first byte of a pattern that starts with "!" or
// "git:" as a one-character class. A basis reads a leading "!" as an
// exclusion and refuses a leading "git:", while path.Match reads both as
// literal bytes, so the guarded pattern matches the path named that way.
func guardPrefix(pattern string) string {
	if rest, ok := strings.CutPrefix(pattern, "!"); ok {
		return "[!]" + rest
	}
	if rest, ok := strings.CutPrefix(pattern, "g"); ok && strings.HasPrefix(rest, "it:") {
		return "[g]" + rest
	}
	return pattern
}

// sortedUnique sorts patterns and drops repeats.
func sortedUnique(patterns []string) []string {
	sort.Strings(patterns)
	return slices.Compact(patterns)
}
