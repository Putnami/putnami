package git

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/tooling/cli/internal/fileindex"
)

// A run answers the Git reads whose answer is a function of files it can
// witness from its input record, as the run before it read them (tooling/cli
// ADR 0136): HEAD, the branch, the commit time, the tags, the abbreviated
// name and the work tree's status. Each answer is kept with the stats of what
// Git read to give it, and a run whose stats all hold starts no Git process
// for it. A stat that moved, a state the in-process reader does not read
// (ADR 0112 §5), or a call outside the table runs Git as before.

// GitReadTable is the table of the input record that holds the Git answers.
const GitReadTable = "git-read"

// readClass says what a held answer is a function of.
type readClass int

const (
	// classNone is a call the record does not hold.
	classNone readClass = iota
	// classRepository is a call whose answer is a function of the git
	// directory: HEAD, the refs, the objects and the configuration.
	classRepository
	// classWorkTree is a call whose answer is also a function of the work
	// tree: the index, every tracked and untracked file, the directories the
	// untracked walk reads and the rule and attribute files.
	classWorkTree
	// classIgnore is a check of paths against the ignore rules: a function
	// of the index, of the paths and of the rule files above them.
	classIgnore
)

// heldReadCalls are the calls a run holds, by their arguments joined with a
// NUL, and what each is a function of.
var heldReadCalls = map[string]readClass{
	"symbolic-ref\x00--short\x00HEAD":                                                    classRepository,
	"rev-parse\x00--abbrev-ref\x00HEAD":                                                  classRepository,
	"rev-parse\x00--short\x00HEAD":                                                       classRepository,
	"rev-parse\x00HEAD":                                                                  classRepository,
	"rev-parse\x00--verify\x00--end-of-options\x00HEAD^{commit}":                         classRepository,
	"rev-parse\x00--verify\x00--quiet\x00HEAD^{commit}":                                  classRepository,
	"rev-parse\x00--is-shallow-repository":                                               classRepository,
	"rev-parse\x00--git-common-dir":                                                      classRepository,
	"rev-parse\x00--show-toplevel":                                                       classRepository,
	"rev-parse\x00--is-inside-work-tree\x00--is-shallow-repository\x00--show-toplevel":   classRepository,
	"rev-parse\x00--is-inside-work-tree\x00--is-shallow-repository\x00--show-prefix":     classRepository,
	"show\x00-s\x00--format=%ct\x00--end-of-options\x00HEAD":                             classRepository,
	"tag\x00--points-at\x00HEAD":                                                         classRepository,
	"for-each-ref\x00--format=%(refname)\x00refs/tags":                                   classRepository,
	"for-each-ref\x00--count=1\x00--format=%(refname)\x00refs/tags":                      classRepository,
	"cat-file\x00--batch-check":                                                          classRepository,
	"check-ignore\x00-z\x00--stdin":                                                      classIgnore,
	"status\x00--porcelain":                                                              classWorkTree,
	"diff\x00HEAD\x00--no-color":                                                         classWorkTree,
	"status\x00--porcelain=v2\x00-z\x00--no-renames\x00--ignore-submodules=none\x00-uno": classWorkTree,
	"ls-files\x00--others\x00--exclude-standard\x00-z":                                   classWorkTree,
	"ls-files\x00--others\x00--ignored\x00--exclude-standard\x00--directory":             classWorkTree,
	"ls-files\x00--others\x00--ignored\x00--exclude-standard\x00--directory\x00-z":       classWorkTree,
}

// heldHistoryCalls are the calls of the repository class that name a commit
// by its full object name: the tags HEAD reaches, as linetag reads them, the
// commit an object name resolves to, the commits from one to HEAD under
// pathspecs and the files a commit changes, as a version line reads them.
var heldHistoryCalls = regexp.MustCompile(`^(` +
	`for-each-ref\x00--merged=` + objectName + `\x00--format=%\(refname\) %\(objectname\)\x00refs/tags` +
	`|rev-parse\x00--verify\x00--end-of-options\x00` + objectName + `\^\{commit\}` +
	`|log\x00--format=%H%x00%s%x00%b\x1e\x00--no-show-signature\x00` + objectName + `\.\.HEAD` + pathspecs +
	`|diff-tree\x00-r\x00--root\x00--no-commit-id\x00--name-only\x00--no-renames\x00-z\x00--end-of-options\x00` + objectName + pathspecs +
	`)$`)

const (
	objectName = `[0-9a-f]{40}([0-9a-f]{24})?`
	pathspecs  = `(\x00--(\x00[^\x00]+)+)?`
)

func classify(args []string) readClass {
	call := strings.Join(args, "\x00")
	if class, ok := heldReadCalls[call]; ok {
		return class
	}
	if heldHistoryCalls.MatchString(call) {
		return classRepository
	}
	return classNone
}

// UseHeldReads makes the process answer the Git reads it holds from record
// and keep there the ones it reads (gitread.UseHeld), until restore. A nil
// record holds nothing.
func UseHeldReads(record *fileindex.Record) (restore func()) {
	if record == nil {
		return func() {}
	}
	return gitread.UseHeld(heldReads{record: record})
}

type heldReads struct{ record *fileindex.Record }

// heldReadAnswers counts the answers the record gave, for tests.
var heldReadAnswers atomic.Int64

// HeldReadAnswers is the number of Git calls this process answered from the
// input record.
func HeldReadAnswers() int64 { return heldReadAnswers.Load() }

func (h heldReads) Answer(dir, stdin string, env, args []string) (string, string, bool) {
	class := classify(args)
	k, ok := h.key(dir, stdin, env, args)
	if class == classNone || !ok {
		return "", "", false
	}
	value, _, ok := h.record.Answer(GitReadTable, k, holdsNow)
	if !ok {
		return "", "", false
	}
	stdout, stderr, ok := decodeHeldRead(value)
	if class == classWorkTree {
		stdout, stderr, ok = workTreeAnswer(value, args)
	}
	if ok {
		heldReadAnswers.Add(1)
	}
	return stdout, stderr, ok
}

func (h heldReads) Asking(dir, stdin string, env, args []string) func(stdout, stderr string) {
	if !h.record.Writes() {
		return nil
	}
	class := classify(args)
	k, ok := h.key(dir, stdin, env, args)
	if class == classNone || !ok {
		return nil
	}
	before, ok := heldWitness(dir, class, stdin)
	if !ok {
		return nil
	}
	return func(stdout, stderr string) {
		// The witness's paths are stated again once Git answered: an answer
		// read while a part of it moved, Git's own refresh of the index
		// included, is not kept.
		after := statParts(before)
		if !slices.Equal(before, after) {
			return
		}
		value := encodeHeldRead(stdout, stderr)
		if class == classWorkTree {
			// The work tree's answers share one entry and one witness: the
			// answers already kept under the same witness stay beside this one.
			var kept []byte
			if earlier, parts, found := h.record.Answer(GitReadTable, k, holdsNow); found && slices.Equal(parts, after) {
				kept = earlier
			}
			value = withWorkTreeAnswer(kept, args, stdout, stderr)
		}
		h.record.Put(GitReadTable, k, value, after)
	}
}

// holdsNow says whether every part has its recorded stat now. A held Git
// answer is checked with stats taken at the call, never with the stats the
// run took earlier: repository code, a hook or a task, can change the
// repository between two calls of one run.
func holdsNow(parts []fileindex.RecordPart) bool {
	return slices.Equal(statParts(parts), parts)
}

// witnesses holds, by directory and class, the witness the process took last:
// a later call whose paths all have the same stats takes the same witness
// without listing the refs or reading the cut again.
var witnesses sync.Map

type keptWitness struct {
	mu    sync.Mutex
	parts []fileindex.RecordPart
}

// heldWitness is readWitness, taken again only when a stat of the last one the
// process took for dir and class moved.
func heldWitness(dir string, class readClass, stdin string) ([]fileindex.RecordPart, bool) {
	value, _ := witnesses.LoadOrStore(filepath.Clean(dir)+"\x00"+strconv.Itoa(int(class))+"\x00"+stdin, &keptWitness{})
	kept, _ := value.(*keptWitness)
	kept.mu.Lock()
	defer kept.mu.Unlock()
	if kept.parts != nil {
		if now := statParts(kept.parts); slices.Equal(now, kept.parts) {
			return now, true
		}
	}
	parts, ok := readWitness(dir, class, stdin)
	if !ok {
		kept.parts = nil
		return nil, false
	}
	kept.parts = parts
	return parts, true
}

// statParts is parts with each path's stat now.
func statParts(parts []fileindex.RecordPart) []fileindex.RecordPart {
	w := &witness{paths: map[string]bool{}}
	for _, part := range parts {
		w.add(part.Path)
	}
	return w.parts()
}

// key is the record's key of a call in the table (heldReadKey): the work
// tree's calls share one key per directory, whose entry holds each of their
// answers.
func (h heldReads) key(dir, stdin string, env, args []string) (string, bool) {
	switch classify(args) {
	case classNone:
		return "", false
	case classWorkTree:
		return heldReadKey(dir, "", env, workTreeArgs)
	}
	return heldReadKey(dir, stdin, env, args)
}

// workTreeArgs names the entry of a directory's work tree answers in the
// record: no Git call has these arguments.
var workTreeArgs = []string{"\x00work-tree"}

// heldReadKey is the record's key of a call of Git with args in dir, with
// stdin and env: the call, the git that runs it, the user, and the variables
// of env Git reads, which the stats do not witness. It is false under a
// variable that points Git at another repository.
func heldReadKey(dir, stdin string, env, args []string) (string, bool) {
	executable, ok := gitExecutable()
	if !ok {
		return "", false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	hash := sha256.New()
	part := func(value string) {
		var length [8]byte
		binary.LittleEndian.PutUint64(length[:], uint64(len(value)))
		hash.Write(length[:])
		hash.Write([]byte(value))
	}
	part(filepath.Clean(abs))
	part(strings.Join(args, "\x00"))
	part(stdin)
	part(executable)
	part(strconv.Itoa(os.Getuid()))
	environment := slices.Clone(env)
	slices.Sort(environment)
	for _, variable := range environment {
		name, _, _ := strings.Cut(variable, "=")
		if slices.Contains(repositorySelectors, name) {
			return "", false
		}
		if strings.HasPrefix(name, "GIT_") || name == "HOME" || name == "XDG_CONFIG_HOME" ||
			name == "LANG" || strings.HasPrefix(name, "LC_") {
			part(variable)
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), true
}

// gitExecutable is the absolute path of the git the process runs.
func gitExecutable() (string, bool) {
	executable := gitread.Executable()
	if !filepath.IsAbs(executable) {
		found, err := exec.LookPath(executable)
		if err != nil {
			return "", false
		}
		executable = found
	}
	abs, err := filepath.Abs(executable)
	return abs, err == nil
}

// repositoryEnvironment is the environment of the gitread calls the
// in-process reader makes.
func repositoryEnvironment() []string { return gitread.RepositoryEnvironment(os.Environ()) }

// readWitness is what an answer of class in dir is a function of, as parts of
// the input record, and false when it has no witness the record can hold.
//
// Every class reads the git directory: HEAD, the .git file of a linked
// worktree, the absence of a .git in each directory between dir and the work
// tree's root, the files Git reads the
// configuration from, packed-refs, the shallow file, the grafts, the
// alternates, every directory and file under refs (a ref Git writes, creates
// or deletes moves it, or its directory), and the object directories (a new
// object can lengthen an abbreviated name; a new pack moves its directory).
// The work tree class also reads the index, every tracked path, every
// untracked file and directory the walk reads, the rule files and the
// attribute files. The git directory itself is no part: every lock Git takes
// moves it.
func readWitness(dir string, class readClass, stdin string) ([]fileindex.RecordPart, bool) {
	// Opening the repository reads Git's configuration, with a Git process
	// when the process keeps no reading of it. A witness takes that read once
	// per process: once it found no reading to keep, it asks for none again,
	// and Git answers the call.
	homes := "\x00" + os.Getenv("HOME") + "\x00" + os.Getenv("XDG_CONFIG_HOME")
	if top, tried := witnessTops.Load(filepath.Clean(dir) + homes); tried {
		root, _ := top.(string)
		if _, kept := configCache.Load(root + homes); !kept {
			return nil, false
		}
	}
	top, err := workTreeRootInProcess(dir)
	witnessTops.Store(filepath.Clean(dir)+homes, top)
	if err != nil {
		return nil, false
	}
	repo, err := openLocal(top)
	if err != nil {
		return nil, false
	}
	config, ok := keptConfigFiles(repo)
	if !ok {
		return nil, false
	}
	w := &witness{paths: map[string]bool{}}
	executable, ok := gitExecutable()
	if !ok || !w.addFile(executable) {
		return nil, false
	}
	// An object store another one borrows from is not witnessed here.
	if _, err := os.Lstat(filepath.Join(repo.commonDir, "objects", "info", "alternates")); !errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, false
	}
	// A link on the way to dir names the directory Git runs in.
	hops, ok := fileindex.LinkHops(abs)
	if !ok {
		return nil, false
	}
	for _, hop := range hops {
		w.add(hop)
	}
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, false
	}
	for current := physical; current != top; current = filepath.Dir(current) {
		if filepath.Dir(current) == current {
			return nil, false
		}
		w.add(filepath.Join(current, ".git"))
	}
	// A .git directory is the git directory, whose own stat every lock moves:
	// its files are the parts. A .git file names a linked worktree's.
	if repo.linked {
		w.add(filepath.Join(top, ".git"))
	}
	for _, name := range []string{"HEAD", "commondir", "config.worktree", "shallow"} {
		w.add(filepath.Join(repo.gitDir, name))
	}
	for _, name := range []string{"config", "packed-refs", "shallow", "reftable",
		filepath.Join("info", "grafts"), filepath.Join("objects", "info", "alternates"),
		filepath.Join("objects", "pack"), filepath.Join("objects", "info")} {
		w.add(filepath.Join(repo.commonDir, name))
	}
	for i := 0; i < 256; i++ {
		w.add(filepath.Join(repo.commonDir, "objects", hex.EncodeToString([]byte{byte(i)})))
	}
	for _, file := range config {
		if !w.addFile(file) {
			return nil, false
		}
	}
	refs := []string{filepath.Join(repo.commonDir, "refs")}
	if repo.linked {
		refs = append(refs, filepath.Join(repo.gitDir, "refs"))
	}
	for _, root := range refs {
		if !w.tree(root) {
			return nil, false
		}
	}
	if class == classWorkTree && !w.workTree(repo) {
		return nil, false
	}
	if class == classIgnore && !w.ignoreRules(repo, physical, stdin) {
		return nil, false
	}
	return w.parts(), true
}

// witnessTops holds, by directory and the HOME and XDG_CONFIG_HOME of the
// process, the work tree root a witness found for it ("" when it found none).
var witnessTops sync.Map

// keptConfigFiles is the files the process's kept configuration reading of
// repo depends on, and false when the reading is read again each time.
func keptConfigFiles(repo *localRepo) ([]string, bool) {
	value, ok := configCache.Load(repo.root + "\x00" + os.Getenv("HOME") + "\x00" + os.Getenv("XDG_CONFIG_HOME"))
	if !ok {
		return nil, false
	}
	cached, ok := value.(*cachedConfig)
	if !ok || cached.config != repo.config {
		return nil, false
	}
	return cached.files, true
}

type witness struct {
	paths map[string]bool
	list  []string
}

func (w *witness) add(path string) {
	path = filepath.Clean(path)
	if !w.paths[path] {
		w.paths[path] = true
		w.list = append(w.list, path)
	}
}

// addFile adds path, a file Git reads outside the repository, with each link
// on the way to it and the file it resolves to: a configuration file that is a
// link to a file elsewhere changes there.
func (w *witness) addFile(path string) bool {
	path = filepath.Clean(path)
	w.add(path)
	hops, ok := fileindex.LinkHops(path)
	if !ok {
		// An absent path, or one whose parent is absent, is witnessed by its
		// absence.
		_, err := os.Lstat(path)
		return errors.Is(err, fs.ErrNotExist)
	}
	if len(hops) == 0 {
		return true
	}
	for _, hop := range hops {
		w.add(hop)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	w.add(resolved)
	return true
}

// tree adds root and every directory and file below it.
func (w *witness) tree(root string) bool {
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == root {
				w.add(root)
				return nil
			}
			return err
		}
		w.add(path)
		return nil
	})
	return err == nil
}

// workTree adds what a status of repo's work tree reads.
func (w *witness) workTree(repo *localRepo) bool {
	index, err := repo.index()
	if err != nil {
		return false
	}
	// A witness of every tracked path would crowd the record's bound of
	// parts and push the input digests out of it.
	if len(index.Entries) > maxHeldTrackedPaths {
		return false
	}
	indexPath := filepath.Join(repo.gitDir, "index")
	w.add(indexPath)
	paths := sha256.New()
	for _, entry := range index.Entries {
		// A submodule's state is its own repository's.
		if entry.Mode == modeGitlink {
			return false
		}
		w.add(filepath.Join(repo.root, filepath.FromSlash(entry.Path)))
		paths.Write([]byte(entry.Path))
		paths.Write([]byte{0})
	}
	walk, ok := walkWitness(repo, hex.EncodeToString(paths.Sum(nil)), indexPath)
	if !ok {
		return false
	}
	for _, part := range walk {
		w.add(part.Path)
	}
	// The attribute files a diff reads besides those of the tree.
	w.add(filepath.Join(repo.commonDir, "info", "attributes"))
	if !w.addFile("/etc/gitattributes") {
		return false
	}
	if repo.config.AttributesFileSet {
		file := repo.config.AttributesFile
		if rest, ok := strings.CutPrefix(file, "~/"); ok {
			file = filepath.Join(os.Getenv("HOME"), rest)
		}
		if !filepath.IsAbs(file) {
			return false
		}
		return w.addFile(file)
	} else if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return w.addFile(filepath.Join(xdg, "git", "attributes"))
	} else if home := os.Getenv("HOME"); home != "" {
		return w.addFile(filepath.Join(home, ".config", "git", "attributes"))
	}
	return true
}

// ignoreRules adds what a check of the NUL-separated paths of stdin, relative
// to dir, against repo's ignore rules reads: the index, which keeps a tracked
// path out of the rules, each path, the .gitignore of each directory from the
// work tree's root down to it, info/exclude and the global ignore file.
func (w *witness) ignoreRules(repo *localRepo, dir, stdin string) bool {
	w.add(filepath.Join(repo.gitDir, "index"))
	w.add(filepath.Join(repo.commonDir, "info", "exclude"))
	excludes, err := repo.config.excludesFile()
	if err != nil {
		return false
	}
	if excludes != "" && !w.addFile(excludes) {
		return false
	}
	for _, rel := range strings.Split(stdin, "\x00") {
		if rel == "" {
			continue
		}
		path := filepath.Join(dir, filepath.FromSlash(rel))
		inside, err := filepath.Rel(repo.root, path)
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return false
		}
		w.add(path)
		for current := filepath.Dir(path); ; current = filepath.Dir(current) {
			w.add(filepath.Join(current, ".gitignore"))
			if current == repo.root || filepath.Dir(current) == current {
				break
			}
		}
	}
	return true
}

// maxHeldTrackedPaths is the largest index whose work tree answers the record
// holds.
const maxHeldTrackedPaths = 200_000

// walks holds, by work tree root, the paths the last untracked walk of the
// process read, for the tracked paths it was read with.
var walks sync.Map

type keptWalk struct {
	mu      sync.Mutex
	tracked string
	parts   []fileindex.RecordPart
}

// walkWitness is the witness of the untracked walk of repo's work tree, whose
// index tracks the paths that digest to tracked: each directory it read, each
// rule file, each untracked path. A walk is read again when the tracked paths
// changed or a stat of the last walk's witness moved; an index Git only
// refreshed reads none. The index itself, at indexPath, is not part of it.
func walkWitness(repo *localRepo, tracked, indexPath string) ([]fileindex.RecordPart, bool) {
	value, _ := walks.LoadOrStore(repo.root, &keptWalk{})
	kept, _ := value.(*keptWalk)
	kept.mu.Lock()
	defer kept.mu.Unlock()
	if kept.parts != nil && kept.tracked == tracked {
		if now := statParts(kept.parts); slices.Equal(now, kept.parts) {
			return now, true
		}
	}
	kept.parts = nil
	// A walk an earlier run kept, whose paths keep their stats, lists the
	// same paths: a run after an edit of a tracked file reads no cut for its
	// witness.
	record := fileindex.CurrentRecord()
	walkKey, keyed := heldReadKey(repo.root, tracked, repositoryEnvironment(), walkArgs)
	if record != nil && keyed {
		if _, parts, ok := record.Answer(GitReadTable, walkKey, holdsNow); ok {
			kept.tracked, kept.parts = tracked, parts
			return parts, true
		}
	}
	cut, err := repo.readCut()
	if err != nil {
		return nil, false
	}
	w := &witness{paths: map[string]bool{}}
	for _, path := range repo.cutWitness(cut) {
		switch {
		case path == indexPath:
		case within(path, repo.root):
			w.add(path)
		case !w.addFile(path):
			// A rule or configuration file outside the work tree, the global
			// ignore file among them, is followed through its links.
			return nil, false
		}
	}
	for _, untracked := range cut.Untracked {
		if untracked.Repository {
			return nil, false
		}
		w.add(filepath.Join(repo.root, filepath.FromSlash(untracked.Path)))
	}
	kept.tracked, kept.parts = tracked, w.parts()
	if record != nil && keyed && record.Writes() {
		record.Put(GitReadTable, walkKey, nil, kept.parts)
	}
	return kept.parts, true
}

// within reports whether path is dir or lies below it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// walkArgs names the untracked walk's witness in the record: no Git call has
// these arguments.
var walkArgs = []string{"\x00walk"}

// parts are the witness's paths with their stats now, an absent path by its
// absence.
func (w *witness) parts() []fileindex.RecordPart {
	parts := make([]fileindex.RecordPart, 0, len(w.list))
	for _, path := range w.list {
		if st, ok := fileindex.LstatAny(path); ok {
			parts = append(parts, fileindex.RecordPart{Path: path, Stat: st})
		} else {
			parts = append(parts, fileindex.AbsentPart(path))
		}
	}
	return parts
}

func encodeHeldRead(stdout, stderr string) []byte {
	var buf bytes.Buffer
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], uint64(len(stdout)))
	buf.Write(length[:])
	buf.WriteString(stdout)
	buf.WriteString(stderr)
	return buf.Bytes()
}

func decodeHeldRead(value []byte) (string, string, bool) {
	if len(value) < 8 {
		return "", "", false
	}
	n := binary.LittleEndian.Uint64(value[:8])
	if n > uint64(len(value)-8) {
		return "", "", false
	}
	return string(value[8 : 8+n]), string(value[8+n:]), true
}

// configArgs is the one configuration read of the in-process reader.
var configArgs = []string{"config", "-z", "--list", "--show-origin"}

// heldConfig is the configuration reading of repo an earlier run kept in the
// input record, with the files it depends on, when every one of them, known
// is among them, still has its recorded stat.
func heldConfig(repo *localRepo, known []string) (*localConfig, []string, bool) {
	record := fileindex.CurrentRecord()
	k, ok := heldReadKey(repo.root, "", repositoryEnvironment(), configArgs)
	if record == nil || !ok {
		return nil, nil, false
	}
	value, parts, ok := record.Answer(GitReadTable, k, holdsNow)
	if !ok {
		return nil, nil, false
	}
	files := make([]string, 0, len(parts))
	for _, part := range parts {
		files = append(files, part.Path)
	}
	for _, file := range known {
		if !slices.Contains(files, file) {
			return nil, nil, false
		}
	}
	output, _, ok := decodeHeldRead(value)
	if !ok {
		return nil, nil, false
	}
	config, _, cacheable, err := parseConfig(output, repo.root, filepath.Join(repo.commonDir, "config"))
	if err != nil || !cacheable {
		return nil, nil, false
	}
	config.Reading = sha256.Sum256([]byte(output))
	heldReadAnswers.Add(1)
	return config, files, true
}

// keepConfig keeps in the input record a reading the process kept (readConfig)
// with the files it depends on, when their stats are still the ones the
// process stamped.
func keepConfig(repo *localRepo, output string, files []string, stamp string) {
	record := fileindex.CurrentRecord()
	k, ok := heldReadKey(repo.root, "", repositoryEnvironment(), configArgs)
	if record == nil || !record.Writes() || !ok {
		return
	}
	w := &witness{paths: map[string]bool{}}
	for _, file := range files {
		if !w.addFile(file) {
			return
		}
	}
	parts := w.parts()
	if stampFiles(files) != stamp {
		return
	}
	record.Put(GitReadTable, k, encodeHeldRead(output, ""), parts)
}

// workTreeAnswer is the answer the entry of a work tree's answers, value,
// holds for args.
func workTreeAnswer(value []byte, args []string) (string, string, bool) {
	answers, ok := decodeWorkTreeAnswers(value)
	if !ok {
		return "", "", false
	}
	answer, ok := answers[strings.Join(args, "\x00")]
	return answer[0], answer[1], ok
}

// withWorkTreeAnswer is the entry of a work tree's answers kept, nil for none,
// with Git's answer to args added. Each answer is its call, its standard
// output and its standard error, each prefixed by its length: Git's output
// is bytes, a name that is not UTF-8 included.
func withWorkTreeAnswer(kept []byte, args []string, stdout, stderr string) []byte {
	answers, ok := decodeWorkTreeAnswers(kept)
	if !ok {
		answers = map[string][2]string{}
	}
	answers[strings.Join(args, "\x00")] = [2]string{stdout, stderr}
	calls := make([]string, 0, len(answers))
	for call := range answers {
		calls = append(calls, call)
	}
	slices.Sort(calls)
	var buf bytes.Buffer
	for _, call := range calls {
		for _, field := range []string{call, answers[call][0], answers[call][1]} {
			var length [8]byte
			binary.LittleEndian.PutUint64(length[:], uint64(len(field)))
			buf.Write(length[:])
			buf.WriteString(field)
		}
	}
	return buf.Bytes()
}

func decodeWorkTreeAnswers(value []byte) (map[string][2]string, bool) {
	answers := map[string][2]string{}
	var fields []string
	for len(value) > 0 {
		if len(value) < 8 {
			return nil, false
		}
		n := binary.LittleEndian.Uint64(value[:8])
		value = value[8:]
		if n > uint64(len(value)) {
			return nil, false
		}
		fields = append(fields, string(value[:n]))
		value = value[n:]
	}
	if len(fields)%3 != 0 {
		return nil, false
	}
	for i := 0; i < len(fields); i += 3 {
		answers[fields[i]] = [2]string{fields[i+1], fields[i+2]}
	}
	return answers, true
}
