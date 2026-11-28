package store

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"

	extensionproto "go.putnami.dev/protocol/extension"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// selectionWitness is what a memoized input digest read, kept so that a write
// inside its region drops it only when the write can move it (ADR 0109): the
// directory and patterns it was collected with and the files it selected.
//
// A digest gets a witness only when its selection is decided by the names of
// the files alone (newSelectionWitness): no input selector, no empty pattern
// set, no ".." segment, no glob character in the directory, and no link on the
// path of a file it read. Any other digest keeps the region rule.
type selectionWitness struct {
	// dir is the absolute, clean directory the files were collected under.
	dir string
	// folded is dir in comparison form (regionPath).
	folded string
	// selected holds the files the digest selected. The first write that
	// asks checks them (prepare): files then holds their comparison forms,
	// and usable says whether none was read through a link.
	selected []string
	prepared sync.Once
	usable   bool
	files    []string // sorted
	includes []string
	excludes []string
	// candidates says whether an include is a "git:" pattern: a .gitignore
	// or a write under .git can then change which files are candidates.
	candidates bool
	// cut is the fingerprint of the read of the candidate cut every Git
	// candidate was listed from (cutFingerprint), "" when Git listed them;
	// absent holds the selected candidates the listing named that were
	// absent from the work tree, relative to dir: each counts as read, as an
	// unstaged deletion is.
	cut    string
	absent []string
	// record is what the input record witnesses of the digest (ADR 0113),
	// nil when it records nothing; recordOnly marks a witness that holds
	// only that, for a digest the names alone do not decide, which no memo
	// keeps under this rule.
	record     *recordWitness
	recordOnly bool
	// unmemoized marks a digest no memo keeps: an ordinary pattern matched a
	// symbolic link, whose target a write can create or change anywhere.
	unmemoized bool
}

// witnessWalkLimit bounds the entries a write's listing reads. A write with
// more falls back to the region rule.
const witnessWalkLimit = 20000

// newSelectionWitness returns the witness of a digest collected under dir with
// patterns that selected files, or nil when the names alone do not decide its
// selection: an input selector, a pattern with a ".", ".." or empty segment
// or a character class, an ordinary pattern whose static prefix leads through
// a link or that is not recursive and holds a glob before its last segment
// (filepath.Glob follows links there).
func newSelectionWitness(dir string, patterns []string, files []fileEntry, facts collectionFacts) *selectionWitness {
	if len(patterns) == 0 || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir ||
		strings.ContainsAny(dir, globMetacharacters) {
		return nil
	}
	includes, excludes := wsproto.SplitFilePatterns(patterns)
	if len(includes) == 0 {
		return nil
	}
	candidates := false
	for _, pattern := range patterns {
		if extensionproto.NamesInputSelector(pattern) {
			return nil
		}
		excluded := strings.HasPrefix(pattern, "!")
		inner, git := wsproto.GitFilePattern(strings.TrimPrefix(pattern, "!"))
		candidates = candidates || git
		if strings.Contains(inner, "[") {
			return nil
		}
		segments := strings.Split(filepath.ToSlash(inner), "/")
		for i, segment := range segments {
			if segment == ".." || segment == "." || segment == "" {
				return nil
			}
			if !git && !excluded && !strings.Contains(inner, "**") && i < len(segments)-1 &&
				strings.ContainsAny(segment, globMetacharacters) {
				return nil
			}
		}
		if !git && !excluded {
			if root, _ := patternRoot(dir, pattern); linkTarget(root) != root {
				return nil
			}
		}
	}

	if resolved, err := filepath.EvalSymlinks(dir); err != nil || resolved != dir {
		return nil
	}
	w := &selectionWitness{
		dir:        dir,
		folded:     regionPath(dir),
		selected:   make([]string, 0, len(files)),
		includes:   includes,
		excludes:   excludes,
		candidates: candidates,
	}
	if candidates && facts.cut != nil {
		w.cut = cutFingerprint(*facts.cut)
	}
	w.absent = facts.absent
	for _, file := range files {
		if file.exact || !strings.HasPrefix(file.path, dir+string(filepath.Separator)) {
			return nil
		}
		w.selected = append(w.selected, file.path)
	}
	return w
}

// prepare checks, once, that no selected file is read through a link: the
// file itself or a directory between it and the witness's directory. A link
// found then, or a selected file gone, leaves the witness unusable, and the
// digest keeps the region rule. A file a write turned into a link since the
// digest read it lies at a path that write names, which moves the digest.
func (w *selectionWitness) prepare() bool {
	w.prepared.Do(w.check)
	return w.usable
}

func (w *selectionWitness) check() {
	dir := w.dir
	linkFree := map[string]bool{dir: true}
	var directoryLinkFree func(string) bool
	directoryLinkFree = func(d string) bool {
		if free, ok := linkFree[d]; ok {
			return free
		}
		free := false
		if parent := filepath.Dir(d); parent != d && strings.HasPrefix(d, dir+string(filepath.Separator)) {
			if info, err := os.Lstat(d); err == nil && info.Mode()&os.ModeSymlink == 0 {
				free = directoryLinkFree(parent)
			}
		}
		linkFree[d] = free
		return free
	}
	files := make([]string, 0, len(w.selected))
	for _, path := range w.selected {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !directoryLinkFree(filepath.Dir(path)) {
			return
		}
		files = append(files, regionPath(path))
	}
	for _, rel := range w.absent {
		files = append(files, regionPath(filepath.Join(dir, filepath.FromSlash(rel))))
	}
	slices.Sort(files)
	w.files, w.usable = files, true
}

// writtenListing is what a write left on disk at the paths an invalidation
// names, read once, when a witnessed digest first asks.
type writtenListing struct {
	once  sync.Once
	paths []string
	// folded holds paths in comparison form (regionPath).
	folded []string
	// entries holds, for each path at or under a written path, its absolute
	// path and whether it is a symbolic link; directories are left out.
	entries []writtenEntry
	// ok is false when a written path is not absolute, leads through a link,
	// names a directory whose file system folds case (foldsCase), or its
	// tree holds more than witnessWalkLimit entries.
	ok bool
}

// foldsCase reports whether a directory on path, below the root of its
// volume, is found under its name with the case of its letters swapped: the
// file system there folds case, so the spelling a write names may not be the
// one a walk of the tree reads, and names decide nothing.
func foldsCase(path string) bool {
	for current := path; ; {
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		name := filepath.Base(current)
		swapped := strings.Map(func(r rune) rune {
			if lower := unicode.ToLower(r); lower != r {
				return lower
			}
			return unicode.ToUpper(r)
		}, name)
		if swapped != name {
			if info, err := os.Lstat(filepath.Join(parent, swapped)); err == nil {
				if original, err := os.Lstat(current); err != nil || os.SameFile(info, original) {
					return true
				}
			}
		}
		current = parent
	}
}

type writtenEntry struct {
	path string
	link bool
}

func (l *writtenListing) read() bool {
	l.once.Do(func() {
		l.ok = true
		for _, written := range l.paths {
			l.folded = append(l.folded, regionPath(written))
		}
		count := 0
		for _, written := range l.paths {
			if !filepath.IsAbs(written) {
				l.ok = false
				return
			}
			written = filepath.Clean(written)
			if linkTarget(written) != written || foldsCase(written) {
				l.ok = false
				return
			}
			info, err := os.Lstat(written)
			if err != nil {
				// Nothing is there now: a removal, whose files the
				// recorded selection answers for.
				continue
			}
			if !info.IsDir() {
				l.entries = append(l.entries, writtenEntry{path: written, link: info.Mode()&os.ModeSymlink != 0})
				continue
			}
			err = filepath.WalkDir(written, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				count++
				if count > witnessWalkLimit {
					return filepath.SkipAll
				}
				if d.IsDir() {
					return nil
				}
				l.entries = append(l.entries, writtenEntry{path: p, link: d.Type()&os.ModeSymlink != 0})
				return nil
			})
			if err != nil || count > witnessWalkLimit {
				l.ok = false
				return
			}
		}
	})
	return l.ok
}

// decides reports whether the witness can answer for the written paths: each
// lies at or under its directory, spelled as the witness compares paths.
func (w *selectionWitness) decides(listing *writtenListing) bool {
	if w == nil || w.unmemoized || listing == nil || !listing.read() || !w.prepare() {
		return false
	}
	for _, folded := range listing.folded {
		if !rootsOverlap(w.folded, folded) || len(folded) < len(w.folded) {
			return false
		}
	}
	return true
}

// witnessAnswer is what a witness says of one write.
type witnessAnswer int

const (
	// writeKeeps: no file the digest read or could select is there.
	writeKeeps witnessAnswer = iota
	// writeMovesIfCandidate: only a "git:" pattern could select a file the
	// write left, so the write moves the digest unless it lies where Git
	// lists no candidate (ADR 0106).
	writeMovesIfCandidate
	// writeMoves: the write moves the digest.
	writeMoves
	// writeMovesIfCutMoved: only a "git:" pattern could select a file the
	// write left, and the digest listed its candidates from a read of the
	// cut: the write moves it only if the cut read after it lists other
	// candidates than that read (selectionWitness.cut).
	writeMovesIfCutMoved
)

// moves answers whether the write the listing read can move the witnessed
// digest: a file the digest read lies at or under a written path, or a file
// the write left there could be selected by the digest's patterns. It answers
// from names, as the collector selects, and errs toward moving: a link left
// by the write, a .gitignore or a path under .git where a "git:" pattern
// reads candidates, all move it.
func (w *selectionWitness) moves(listing *writtenListing) witnessAnswer {
	for i, written := range listing.paths {
		folded := listing.folded[i]
		if _, found := slices.BinarySearch(w.files, folded); found {
			return writeMoves
		}
		prefix := strings.TrimSuffix(folded, "/") + "/"
		if at, _ := slices.BinarySearch(w.files, prefix); at < len(w.files) && strings.HasPrefix(w.files[at], prefix) {
			return writeMoves
		}
		if w.candidates && (filepath.Base(written) == ".gitignore" ||
			slices.Contains(strings.Split(filepath.ToSlash(written), "/"), ".git")) {
			return writeMoves
		}
	}
	answer := writeKeeps
	for _, entry := range listing.entries {
		if entry.link {
			return writeMoves
		}
		if w.candidates && filepath.Base(entry.path) == ".gitignore" {
			return writeMoves
		}
		switch w.couldSelect(entry.path) {
		case writeMoves:
			return writeMoves
		case writeMovesIfCandidate:
			answer = writeMovesIfCandidate
		}
	}
	if answer == writeMovesIfCandidate && w.cut != "" {
		return writeMovesIfCutMoved
	}
	return answer
}

// cutFingerprint is the digest of what a read of the cut answers a key: its
// paths, in order, with the index mode and the unmerged mark of each.
func cutFingerprint(read CandidateCut) string {
	h := sha256.New()
	for _, path := range read.Paths {
		writeField(h, path)
		writeField(h, read.Modes[path])
		if read.Unmerged[path] {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// couldSelect answers whether a file at path, under the witness's directory,
// is one the collector would select with the witness's patterns. It decides a
// superset of collectPatternFilesWith's choice: for a "git:" pattern it does
// not ask whether the path is a Git candidate, and answers
// writeMovesIfCandidate instead.
func (w *selectionWitness) couldSelect(path string) witnessAnswer {
	rel, err := filepath.Rel(w.dir, path)
	if err != nil {
		return writeMoves
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || strings.HasPrefix(rel, "../") || rel == ".." {
		return writeMoves
	}
	if wsproto.MatchesAnyFilePattern(rel, w.excludes) {
		return writeKeeps
	}
	answer := writeKeeps
	for _, include := range w.includes {
		if inner, git := wsproto.GitFilePattern(include); git {
			if wsproto.MatchFilePattern(rel, inner) {
				answer = writeMovesIfCandidate
			}
			continue
		}
		slashed := filepath.ToSlash(include)
		if before, _, recursive := strings.Cut(slashed, "**"); recursive {
			if globDoubleStarReaches(rel, strings.Trim(strings.TrimSuffix(before, "/"), "/")) &&
				wsproto.MatchFilePattern(rel, slashed) {
				return writeMoves
			}
			continue
		}
		if matched, err := filepath.Match(filepath.Join(w.dir, include), path); err != nil || matched {
			return writeMoves
		}
	}
	return answer
}

// globDoubleStarReaches reports whether globDoubleStar, walking from the
// directory prefix names under its root, reaches the file at rel: rel lies
// under prefix, and no directory between them is one it skips.
func globDoubleStarReaches(rel, prefix string) bool {
	below := rel
	if prefix != "" {
		if rel == prefix {
			// The walk's root is the file itself.
			return true
		}
		var ok bool
		below, ok = strings.CutPrefix(rel, prefix+"/")
		if !ok {
			return false
		}
	}
	segments := strings.Split(below, "/")
	for _, segment := range segments[:len(segments)-1] {
		switch segment {
		case "node_modules", ".git", ".putnami", "out", "dist", "vendor":
			return false
		}
	}
	return true
}

// InputDigestTable is the table of the input record (fileindex.Record) that
// holds the input digests of cache keys, keyed by their memo key
// (lookupFileHash), each with its witness (tooling/cli ADR 0113 §1).
const InputDigestTable = "input-digest"

// recordKey is the input record's key of a digest of patterns, and false
// when the record may not answer it: the empty pattern set, or input
// selectors whose expander names no answer identity. A digest of input
// selectors is keyed by what their answers depend on beside their basis
// (recordIdentity), and one that reads Git candidates by the environment
// that selects the repository, its index and its configuration: their values
// decide the cut as much as the files do (ADR 0113 §1, §3). Whether a
// collected digest is recorded is decided from its witness
// (recordInputDigest).
func recordKey(key string, patterns []string, expander InputExpander) (string, bool) {
	git := false
	for _, pattern := range patterns {
		if extensionproto.NamesInputSelector(pattern) {
			identified, ok := expander.(interface{ RecordIdentity() string })
			if !ok || identified.RecordIdentity() == "" {
				return "", false
			}
			key += "\x00selectors\x00" + identified.RecordIdentity()
			break
		}
	}
	for _, pattern := range patterns {
		_, candidate := wsproto.GitFilePattern(strings.TrimPrefix(pattern, "!"))
		git = git || candidate
	}
	if len(patterns) == 0 {
		return "", false
	}
	if git {
		environment := []string{"HOME=" + os.Getenv("HOME"), "XDG_CONFIG_HOME=" + os.Getenv("XDG_CONFIG_HOME")}
		for _, variable := range os.Environ() {
			if strings.HasPrefix(variable, "GIT_") {
				environment = append(environment, variable)
			}
		}
		slices.Sort(environment)
		key += "\x00" + strings.Join(environment, "\x00")
	}
	return key, true
}

// recordedInputDigest answers the digest the input record holds at key when
// every part of its witness holds now, with the in-run witness of ADR 0109
// rebuilt from the files it recorded, and the record's witness, its parts:
// its value is the digest and how many of the witness's first parts are the
// files it selected.
func recordedInputDigest(record *fileindex.Record, key, dir string, patterns []string) (hash string, witness *selectionWitness, parts []fileindex.RecordPart, memoize, ok bool) {
	value, parts, ok := record.Answer(InputDigestTable, key, record.Hold)
	if !ok {
		return "", nil, nil, false, false
	}
	fields := strings.Split(string(value), "\x00")
	if len(fields) != 3 || fields[0] == "" {
		return "", nil, nil, false, false
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil || n < 0 || n > len(parts) {
		return "", nil, nil, false, false
	}
	files := make([]fileEntry, n)
	for i, part := range parts[:n] {
		files[i] = fileEntry{path: part.Path}
	}
	witness = newSelectionWitness(dir, patterns, files, collectionFacts{})
	if witness == nil {
		return fields[0], nil, parts, fields[2] == "memoized", true
	}
	// The record just took the stat of every part: when it shows each
	// selected file regular and each directory between it and dir a
	// directory, the witness is prepared from them here, outside the memo's
	// lock, instead of by the first write that asks (prepare), which stats
	// them again while every lookup waits on that lock.
	directories := make(map[string]bool, len(parts)-n)
	for _, part := range parts[n:] {
		if part.Stat.Mode&modeTypeMask == modeDirectory {
			directories[filepath.Clean(part.Path)] = true
		}
	}
	linkFree := true
	for _, part := range parts[:n] {
		if part.Stat.Mode&modeTypeMask != modeRegular {
			linkFree = false
			break
		}
		for d := filepath.Dir(part.Path); d != dir && d != filepath.Dir(d) && linkFree; d = filepath.Dir(d) {
			linkFree = directories[d]
		}
	}
	if linkFree {
		witness.prepared.Do(func() {
			prepared := make([]string, len(witness.selected))
			for i, path := range witness.selected {
				prepared[i] = regionPath(path)
			}
			slices.Sort(prepared)
			witness.files, witness.usable = prepared, true
		})
	}
	return fields[0], witness, parts, fields[2] == "memoized", true
}

// recordInputDigest offers a collected digest to the input record with its
// witness (ADR 0113 §1): the lstat of each file it selected, then of each
// path it read to select them. A path the collection found absent is
// witnessed by staying absent; a listed path that is missing, by the nearest
// directory above it that exists. A digest of Git candidates adds the witness
// of the cut (§3), and one of input selectors the paths their bases read.
//
// Nothing is offered for a digest that reads through a symbolic link: a
// selected file, a listed path or any directory on the way to one that is a
// link, or a collection directory that resolves elsewhere. A Git candidate is
// read as Git lists it, a link by its text, so a candidate that is a link is
// its own witness. Nothing is offered either for a digest with no record
// witness, one whose listing could not be named, or one of candidates Git
// listed; the record refuses one with a part written within the margin of
// its clock.
func recordInputDigest(record *fileindex.Record, key, hash string, witness *selectionWitness) {
	if witness == nil || witness.record == nil || witness.unmemoized {
		return
	}
	w := witness.record
	if w.unlisted || w.candidates && w.cutWitness == nil {
		return
	}
	// The selected files come first, each once, as the collection
	// selected them; then every other path, once.
	parts := make([]fileindex.RecordPart, 0, len(w.selected)+len(w.listed))
	var others []fileindex.RecordPart
	seen := make(map[string]bool, cap(parts))
	for _, path := range w.selected {
		seen[path] = true
	}
	add := func(path string, st fileindex.Stat) {
		if !seen[path] {
			seen[path] = true
			others = append(others, fileindex.RecordPart{Path: path, Stat: st})
		}
	}
	// direct reports whether the directory dir resolves, and witnesses each
	// symbolic link its resolution meets, a system one above the workspace
	// included, by the link's own lstat: pointing it elsewhere moves that
	// stat, and every path read below it is stated through it. Once per
	// directory.
	resolved := map[string]bool{}
	direct := func(dir string) bool {
		if known, ok := resolved[dir]; ok {
			return known
		}
		hops, ok := fileindex.LinkHops(dir)
		for _, hop := range hops {
			st, there := fileindex.LstatAny(hop)
			if !there {
				ok = false
				break
			}
			add(hop, st)
		}
		resolved[dir] = ok
		return ok
	}
	if !direct(w.dir) || w.keyDir != w.dir && !direct(w.keyDir) {
		return
	}
	// An answer's files hold while what the extension read holds: the
	// directories its basis lists, and the paths a content set selects,
	// each read as the basis digest reads it, a link by its text.
	var basisDirectories, contents []string
	for _, basis := range w.bases {
		directories, content, err := wsproto.InputBasisPaths(w.dir, basis)
		if err != nil {
			return
		}
		basisDirectories = append(basisDirectories, directories...)
		contents = append(contents, content...)
	}

	// stat takes a path's lstat for the witness: no link on its way, and
	// not a link itself unless linkRead (read by its text).
	stat := func(path string, linkRead bool) (fileindex.Stat, bool) {
		st, ok := fileindex.LstatAny(path)
		if !ok || !direct(filepath.Dir(path)) {
			return fileindex.Stat{}, false
		}
		return st, linkRead || st.Mode&modeTypeMask != modeSymlink
	}
	gitOnly := w.candidates && !w.ordinary && len(w.bases) == 0
	for _, path := range w.selected {
		st, ok := stat(path, gitOnly)
		if !ok || st.Mode&modeTypeMask != modeRegular && st.Mode&modeTypeMask != modeSymlink {
			return
		}
		parts = append(parts, fileindex.RecordPart{Path: path, Stat: st})
	}
	for _, path := range contents {
		st, ok := stat(path, true)
		if !ok {
			return
		}
		add(path, st)
	}
	// A path the collection found absent is witnessed by staying absent: it
	// must be absent still now, after the digest was computed, and nothing
	// above it may be a link.
	absent := func(path string) bool {
		if _, there := fileindex.LstatAny(path); there {
			return false
		}
		above := filepath.Dir(path)
		for _, there := fileindex.LstatAny(above); !there && filepath.Dir(above) != above; _, there = fileindex.LstatAny(above) {
			above = filepath.Dir(above)
		}
		if !direct(above) {
			return false
		}
		add(path, fileindex.Stat{})
		return true
	}
	for _, rel := range w.absent {
		if !absent(filepath.Join(w.dir, filepath.FromSlash(rel))) {
			return
		}
	}
	// A directory an exclusion excludes as a whole ("!**/.gen/**") holds
	// nothing the ordinary patterns select, whatever it lists. The
	// exclusions govern the ordinary listing alone: the cut's witness and an
	// answer's basis read what they read whatever the patterns exclude.
	excludedDirectory := func(path string) bool {
		rel, err := filepath.Rel(w.dir, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return false
		}
		rel = filepath.ToSlash(rel)
		for _, exclude := range w.excludes {
			if whole, ok := strings.CutSuffix(exclude, "/**"); ok {
				for at := rel; at != "."; at = pathpkg.Dir(at) {
					if wsproto.MatchFilePattern(at, whole) {
						return true
					}
				}
			}
		}
		return false
	}
	witnessListed := func(path string, ordinary bool) bool {
		if missing, ok := strings.CutPrefix(path, missingPrefix); ok {
			return ordinary && excludedDirectory(missing) || absent(missing)
		}
		if ordinary && excludedDirectory(path) {
			return true
		}
		// A missing path is witnessed by the nearest directory above it
		// that exists: its listing shows the path stays missing.
		existing := filepath.Clean(path)
		found, there := fileindex.LstatAny(existing)
		for !there && filepath.Dir(existing) != existing {
			existing = filepath.Dir(existing)
			found, there = fileindex.LstatAny(existing)
		}
		if there && existing != filepath.Clean(path) && found.Mode&modeTypeMask == modeSymlink {
			// The nearest existing directory is reached through a link, such
			// as a linked home: the link is a hop of its resolution, and the
			// directory it leads to lists the missing path.
			target, err := filepath.EvalSymlinks(existing)
			if err != nil || !direct(existing) {
				return false
			}
			st, ok := stat(target, false)
			if ok {
				add(target, st)
			}
			return ok
		}
		st, ok := stat(existing, false)
		if ok {
			add(existing, st)
		}
		return ok
	}
	for _, path := range w.listed {
		if !witnessListed(path, true) {
			return
		}
	}
	for _, path := range slices.Concat(w.cutWitness, basisDirectories) {
		if !witnessListed(path, false) {
			return
		}
	}
	parts = append(parts, others...)
	// The value is the digest, how many of the first parts are the files it
	// selected, and whether the run's memo keeps it.
	record.Put(InputDigestTable, key, []byte(hash+"\x00"+strconv.Itoa(len(w.selected))+"\x00memoized"), parts)
}

// modeTypeMask and modeRegular are the file-type bits of a stat's mode and
// their value for a regular file, the same on every platform whose stat the
// file index decodes.
const (
	modeTypeMask  = 0o170000
	modeRegular   = 0o100000
	modeSymlink   = 0o120000
	modeDirectory = 0o040000
)
