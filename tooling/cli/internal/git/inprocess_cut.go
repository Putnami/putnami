package git

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
)

// untrackedPath is a path the cut lists that the index does not track.
type untrackedPath struct {
	// Path is slash-separated and relative to the work tree's root.
	Path string
	// Repository is whether the path is a nested repository, which Git lists
	// as its directory and does not enter.
	Repository bool
}

// localCut is what `git ls-files -z -t --stage --others --exclude-standard` lists
// at the work tree's root: every entry of the index, and the untracked paths
// the ignore rules do not exclude.
type localCut struct {
	Index     *localIndex
	Untracked []untrackedPath
	// Directories are the directories the walk read, slash paths from the
	// root ("" for the root), in the order it read them; every other
	// directory is excluded as a whole, a gitlink, a nested repository or
	// outside the walk's bound.
	Directories []string
	// RuleFiles are the ignore files the walk read, absolute: info/exclude
	// and the global ignore file when they exist, and each .gitignore of a
	// directory it read. A rule file a directory lacks is told by that
	// directory's listing.
	RuleFiles []string
}

// readCut reads the cut of the repository at its work tree's root, as Git
// 2.43's ls-files lists it (dir.c read_directory). With prefixes, slash paths
// from the root, it lists what ls-files lists with their literal pathspecs:
// the index entries and the untracked paths at or below one of them. The walk
// then enters only the directories at, below or above a prefix: what a path
// is depends on its own directories alone.
func (r *localRepo) readCut(prefixes ...string) (*localCut, error) {
	cutReads.Add(1)
	index, err := r.index()
	if err != nil {
		return nil, err
	}
	walker, err := r.newWalker(index)
	if err != nil {
		return nil, err
	}
	walker.prefixes = prefixes
	if err := walker.walk("", nil); err != nil {
		return nil, err
	}
	if len(prefixes) > 0 {
		kept := make([]indexEntry, 0, len(index.Entries))
		for _, entry := range index.Entries {
			if underPrefixes(entry.Path, prefixes) {
				kept = append(kept, entry)
			}
		}
		index = &localIndex{Entries: kept}
	}
	// ls-files sorts what it lists by its bytes (cmp_dir_entry).
	sort.Slice(walker.untracked, func(i, j int) bool { return walker.untracked[i].Listed() < walker.untracked[j].Listed() })
	return &localCut{Index: index, Untracked: walker.untracked, Directories: walker.directories, RuleFiles: walker.ruleFiles}, nil
}

// cutReads counts the cuts the process read in-process.
var cutReads atomic.Int64

// InProcessCutReads is the number of cuts the process read in-process, whole or
// bounded: what a test counts where it counted the ls-files calls the reads
// replace.
func InProcessCutReads() int64 { return cutReads.Load() }

// underPrefixes reports whether path lies at or below one of prefixes, as a literal
// pathspec matches it.
func underPrefixes(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// above reports whether path is a directory above one of prefixes.
func above(path string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(prefix, path+"/") {
			return true
		}
	}
	return false
}

// walker walks the work tree for its untracked paths.
type walker struct {
	repo        *localRepo
	match       matcher
	files       map[string]bool // the index's paths, case folded under core.ignoreCase
	dirs        map[string]bool // the directories the index's paths lie in, likewise
	gitlinks    map[string]bool // the gitlinks of the index, likewise
	fileRules   []patternList   // info/exclude, then core.excludesFile
	untracked   []untrackedPath
	prefixes    []string // the walk's bound; none walks the whole tree
	directories []string
	ruleFiles   []string
}

func (r *localRepo) newWalker(index *localIndex) (*walker, error) {
	w := &walker{
		repo:     r,
		match:    matcher{ignoreCase: r.config.IgnoreCase},
		files:    map[string]bool{},
		dirs:     map[string]bool{},
		gitlinks: map[string]bool{},
	}
	for _, entry := range index.Entries {
		path := w.key(entry.Path)
		w.files[path] = true
		if entry.Mode == modeGitlink {
			w.gitlinks[path] = true
		}
		for i := strings.IndexByte(path, '/'); i >= 0; i = nextSlash(path, i) {
			w.dirs[path[:i]] = true
		}
		if entry.SkipWorktree && (entry.Path == ".gitignore" || strings.HasSuffix(entry.Path, "/.gitignore")) {
			// Git reads such a rule file from the index when the work tree
			// lacks it.
			if _, err := os.Lstat(filepath.Join(r.root, filepath.FromSlash(entry.Path))); err != nil {
				return nil, unsupported("a skip-worktree %s absent from the work tree", entry.Path)
			}
		}
	}
	excludes, err := r.config.excludesFile()
	if err != nil {
		return nil, err
	}
	// info/exclude is checked before core.excludesFile.
	for _, file := range []string{filepath.Join(r.commonDir, "info", "exclude"), excludes} {
		if file == "" {
			continue
		}
		list, err := readRuleFile(file, "", false)
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(file); err == nil {
			w.ruleFiles = append(w.ruleFiles, file)
		}
		w.fileRules = append(w.fileRules, list)
	}
	return w, nil
}

func nextSlash(path string, i int) int {
	next := strings.IndexByte(path[i+1:], '/')
	if next < 0 {
		return -1
	}
	return i + 1 + next
}

// key is how the walker looks a path up in the index: as is, or with ASCII
// case folded under core.ignoreCase.
func (w *walker) key(path string) string {
	if !w.match.ignoreCase {
		return path
	}
	return asciiLower(path)
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		b[i] = toLower(c)
	}
	return string(b)
}

// readRuleFile reads an ignore file whose rules apply at base. A missing
// file holds no rule. A per-directory .gitignore that is not a regular file,
// or any file it cannot read, is unsupported: Git warns and may read it from
// the index.
func readRuleFile(path, base string, perDirectory bool) (patternList, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, unsupported("read %s: %v", path, err)
	}
	if perDirectory && !info.Mode().IsRegular() {
		return nil, unsupported("a %s that is not a regular file", path)
	}
	if !perDirectory && info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, unsupported("read %s: %v", path, err)
		}
	}
	if !info.Mode().IsRegular() {
		return nil, unsupported("a %s that is not a regular file", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, unsupported("read %s: %v", path, err)
	}
	list := parsePatterns(content, base)
	if list.versionSensitive() {
		return nil, unsupported("a rule in %s whose meaning differs between Git versions", path)
	}
	return list, nil
}

// excluded reports whether path is excluded by the rules: the per-directory
// lists from the deepest, then the file lists. The first list with a
// matching rule decides (last_matching_pattern_from_lists).
func (w *walker) excluded(dirRules []patternList, path string, isDir bool) bool {
	for i := len(dirRules) - 1; i >= 0; i-- {
		if p, ok := w.match.lastMatch(dirRules[i], path, isDir); ok {
			return p.flags&patternNegative == 0
		}
	}
	for _, list := range w.fileRules {
		if p, ok := w.match.lastMatch(list, path, isDir); ok {
			return p.flags&patternNegative == 0
		}
	}
	return false
}

// walk lists the untracked paths of the directory dir (slash path from the
// root, "" for the root, else ending with a slash). parentRules are the
// per-directory lists from the root to dir's parent; the .gitignore of dir,
// when its listing holds one, follows them.
func (w *walker) walk(dir string, parentRules []patternList) error {
	directory := filepath.Join(w.repo.root, filepath.FromSlash(dir))
	entries, err := os.ReadDir(directory)
	if err != nil {
		return unsupported("read the directory %q: %v", dir, err)
	}
	w.directories = append(w.directories, strings.TrimSuffix(dir, "/"))
	dirRules := parentRules[:len(parentRules):len(parentRules)]
	for _, entry := range entries {
		name := entry.Name()
		if name == ".gitignore" {
			rules, err := readRuleFile(filepath.Join(directory, name), dir, true)
			if err != nil {
				return err
			}
			w.ruleFiles = append(w.ruleFiles, filepath.Join(directory, name))
			dirRules = append(dirRules, rules)
		} else if w.match.ignoreCase && asciiEqualFold([]byte(name), []byte(".gitignore")) {
			// Git opens .gitignore, which the file system may or may not
			// resolve to this name.
			return unsupported("a rule file named %q under core.ignoreCase", dir+name)
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" || (w.match.ignoreCase && asciiEqualFold([]byte(name), []byte(".git"))) {
			continue
		}
		if w.repo.config.PrecomposeUnicode && !isASCII(name) {
			return unsupported("a non-ASCII name under core.precomposeUnicode")
		}
		path := dir + name
		inScope := len(w.prefixes) == 0 || underPrefixes(path, w.prefixes)
		if !inScope && !above(path, w.prefixes) {
			continue
		}
		mode := entry.Type()
		isDir := mode.IsDir()
		if !isDir && w.files[w.key(path)] {
			continue
		}
		if w.excluded(dirRules, path, isDir) {
			continue
		}
		switch {
		case isDir:
			if err := w.directory(path, dirRules, inScope); err != nil {
				return err
			}
		case !inScope:
		case mode.IsRegular() || mode&fs.ModeSymlink != 0:
			w.untracked = append(w.untracked, untrackedPath{Path: path})
		}
	}
	return nil
}

// directory treats the directory at path, which no rule excludes
// (treat_directory without DIR_SHOW_OTHER_DIRECTORIES). A directory only
// above the walk's bound is entered, and a nested repository there is not
// listed, as Git treats a directory a pathspec matches only below it.
func (w *walker) directory(path string, dirRules []patternList, inScope bool) error {
	key := w.key(path)
	recurse := false
	if w.match.ignoreCase {
		// directory_exists_in_index_icase: the directories first.
		switch {
		case w.dirs[key]:
			recurse = true
		case w.gitlinks[key]:
			return nil
		}
	} else {
		// directory_exists_in_index: the index orders a gitlink before the
		// paths below it.
		switch {
		case w.gitlinks[key]:
			return nil
		case w.dirs[key]:
			recurse = true
		}
	}
	if !recurse {
		nested, err := nestedRepository(filepath.Join(w.repo.root, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		if nested {
			if inScope {
				w.untracked = append(w.untracked, untrackedPath{Path: path, Repository: true})
			}
			return nil
		}
	}
	return w.walk(path+"/", dirRules)
}

// nestedRepository answers Git's is_nonbare_repository_dir for dir: its
// .git is a repository, or a .git file Git cannot read.
func nestedRepository(dir string) (bool, error) {
	entry := filepath.Join(dir, ".git")
	info, err := os.Lstat(entry)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, unsupported("read %s: %v", entry, err)
	}
	switch {
	case info.IsDir():
		return isNestedGitDirectory(entry)
	case info.Mode().IsRegular():
		target, err := readGitfile(entry)
		if err != nil {
			return false, err
		}
		return isNestedGitDirectory(target)
	}
	return false, unsupported("a %s that is neither a directory nor a file", entry)
}

// isNestedGitDirectory answers is_git_directory for a nested repository's
// git directory, with its commondir.
func isNestedGitDirectory(gitDir string) (bool, error) {
	commonDir := gitDir
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		commonDir = strings.TrimRight(string(data), "\r\n")
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(gitDir, commonDir)
		}
	} else if !os.IsNotExist(err) {
		return false, unsupported("read the commondir of %s: %v", gitDir, err)
	}
	return isGitDirectory(gitDir, commonDir), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// Listed is the path as ls-files lists it: a nested repository as its
// directory with a trailing slash.
func (u untrackedPath) Listed() string {
	if u.Repository {
		return u.Path + "/"
	}
	return u.Path
}
