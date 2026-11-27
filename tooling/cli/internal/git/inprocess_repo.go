// The in-process reader answers what the engine asked Git about the
// local state of a repository: the index (its versions, its extensions and a
// split index) and the candidate cut it lists with the work tree's untracked
// files and the ignore rules. It reads what Git 2.43 reads, as Git reads it,
// and refuses as unsupported every state it does not read, so the caller asks
// Git instead (tooling/cli ADR 0112).

package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.putnami.dev/sdk/extension/gitread"
)

// inProcessUnsupported reports whether err says the in-process reader does not read
// the state it met, and that Git must answer instead.
func inProcessUnsupported(err error) bool {
	var unsupportedErr *unsupportedError
	return errors.As(err, &unsupportedErr)
}

// localRepo is a repository whose work tree's root is the directory it was opened
// at, with the configuration Git reads there.
type localRepo struct {
	root      string
	gitDir    string
	commonDir string
	linked    bool
	config    *localConfig
	// Spawned is the number of git processes opening it started: one when it
	// read the configuration, none when the process held a current read.
	Spawned int
}

// openLocal opens the repository whose work tree's root is root, as Git discovers
// it from root with the environment gitread.RepositoryEnvironment leaves:
// root holds a .git directory, or the .git file of a linked worktree. Its
// configuration is Git's reading in root (ReadConfig). Any other layout is
// unsupported: a root below the work tree's root, a submodule, a repository
// Git would refuse as owned by another user, a bare repository, a
// core.worktree.
func openLocal(root string) (*localRepo, error) {
	repo, err := discover(root)
	if err != nil {
		return nil, err
	}
	config, spawned, err := readConfig(repo)
	repo.Spawned = spawned
	if err != nil {
		return nil, spawnedError{err: err, spawned: spawned}
	}
	if config.Bare || config.WorkTree {
		return nil, unsupported("core.bare or core.worktree")
	}
	repo.config = config
	return repo, nil
}

// repositorySelectors are the variables that point Git at another
// repository, work tree, index or object store, bound its discovery, or
// change how it reads a pathspec. The
// Git calls the reader replaces run without them
// (gitread.RepositoryEnvironment); with one set, the reader steps aside all
// the same and Git answers.
var repositorySelectors = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
	// Git reads a pathspec under these as another magic than the literal one
	// a bounded listing names.
	"GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS",
}

// spawnedError is an error of openLocal after it started git processes.
type spawnedError struct {
	err     error
	spawned int
}

func (e spawnedError) Error() string { return e.err.Error() }
func (e spawnedError) Unwrap() error { return e.err }

// spawnedBy is the number of git processes openLocal started before it failed
// with err.
func spawnedBy(err error) int {
	var spawned spawnedError
	if errors.As(err, &spawned) {
		return spawned.spawned
	}
	return 0
}

// discover finds the git directories of the work tree whose root is root.
func discover(root string) (*localRepo, error) {
	for _, name := range repositorySelectors {
		if _, set := os.LookupEnv(name); set {
			return nil, unsupported("%s is set", name)
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, unsupported("resolve %s: %v", root, err)
	}
	entry := filepath.Join(root, ".git")
	info, err := os.Lstat(entry)
	if err != nil {
		return nil, unsupported("no .git at the root: %v", err)
	}
	repo := &localRepo{root: root}
	switch {
	case info.IsDir():
		repo.gitDir, repo.commonDir = entry, entry
		if _, err := os.Lstat(filepath.Join(entry, "commondir")); err == nil {
			return nil, unsupported("a .git directory with a commondir")
		}
	case info.Mode().IsRegular():
		target, err := readGitfile(entry)
		if err != nil {
			return nil, err
		}
		common, err := os.ReadFile(filepath.Join(target, "commondir"))
		if err != nil {
			// A submodule's or a separate git directory: Git reads it.
			return nil, unsupported("a .git file that names no linked worktree")
		}
		commonDir := strings.TrimRight(string(common), "\r\n")
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(target, commonDir)
		}
		repo.gitDir, repo.commonDir, repo.linked = target, filepath.Clean(commonDir), true
	default:
		return nil, unsupported("a .git that is neither a directory nor a file")
	}
	if !isGitDirectory(repo.gitDir, repo.commonDir) {
		return nil, unsupported("a .git that is no repository")
	}
	// Git checks the owner of the work tree, of the .git file of a linked
	// worktree and of the git directory.
	for _, dir := range []string{root, entry, repo.gitDir, repo.commonDir} {
		if err := ownedByCurrentUser(dir); err != nil {
			return nil, err
		}
	}
	return repo, nil
}

// workTreeRootInProcess answers `git rev-parse --show-toplevel` in dir: the physical
// path of the root of the work tree holding dir, the nearest directory at or
// above it with a .git entry. A layout openLocal does not read, a .git entry
// across a file system boundary, which Git does not cross, and a dir inside a
// git directory are unsupported.
func workTreeRootInProcess(dir string) (string, error) {
	physical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", unsupported("resolve %s: %v", dir, err)
	}
	physical, err = filepath.Abs(physical)
	if err != nil {
		return "", unsupported("resolve %s: %v", dir, err)
	}
	device, err := deviceOf(physical)
	if err != nil {
		return "", err
	}
	for current := physical; ; {
		if filepath.Base(current) == ".git" {
			return "", unsupported("a directory inside a git directory")
		}
		if _, err := os.Lstat(filepath.Join(current, ".git")); err != nil && isGitDirectory(current, current) {
			// Git finds a bare repository here: it answers no work tree.
			return "", unsupported("%s is a git directory", current)
		}
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			// openLocal refuses what would move Git's answer: a core.worktree, a
			// submodule, another owner.
			if _, err := openLocal(current); err != nil {
				return "", err
			}
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", unsupported("no .git at or above %s", dir)
		}
		if parentDevice, err := deviceOf(parent); err != nil || parentDevice != device {
			return "", unsupported("a file system boundary above %s", dir)
		}
		current = parent
	}
}

// readGitfile reads the directory a .git file names.
func readGitfile(path string) (string, error) {
	if info, err := os.Stat(path); err != nil || info.Size() > 1<<20 {
		// read_gitfile_gently refuses a .git file above 1 MiB.
		return "", unsupported("a .git file %s too large or unreadable", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", unsupported("read %s: %v", path, err)
	}
	// read_gitfile_gently drops the line ends alone, and holds the path as a
	// C string, which ends at its first NUL.
	text := strings.TrimRight(string(data), "\r\n")
	if nul := strings.IndexByte(text, 0); nul >= 0 {
		text = text[:nul]
	}
	target, ok := strings.CutPrefix(text, "gitdir: ")
	if !ok || target == "" {
		return "", unsupported("a .git file without a gitdir line")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return filepath.Clean(target), nil
}

// isGitDirectory answers Git's is_git_directory: a valid HEAD in gitDir, and
// objects and refs in commonDir.
func isGitDirectory(gitDir, commonDir string) bool {
	if !validHeadRef(filepath.Join(gitDir, "HEAD")) {
		return false
	}
	for _, name := range []string{"objects", "refs"} {
		if info, err := os.Stat(filepath.Join(commonDir, name)); err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

// validHeadRef answers Git's validate_headref: a symbolic link into refs/, a
// symbolic ref into refs/, or an object name.
func validHeadRef(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		return err == nil && strings.HasPrefix(target, "refs/")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if len(data) > 255 {
		data = data[:255]
	}
	if ref, ok := bytes.CutPrefix(data, []byte("ref:")); ok {
		return bytes.HasPrefix(bytes.TrimLeft(ref, " \t\n\r"), []byte("refs/"))
	}
	return leadingObjectName(data, 40) || leadingObjectName(data, 64)
}

func leadingObjectName(data []byte, length int) bool {
	if len(data) < length {
		return false
	}
	for _, c := range data[:length] {
		if !isHex(c) {
			return false
		}
	}
	return true
}

func isHex(c byte) bool { return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

// hashSize is the size of the repository's object names.
func (r *localRepo) hashSize() int {
	if r.config.SHA256 {
		return 32
	}
	return 20
}

// index reads the repository's index. A split index in a linked worktree is
// unsupported: Git looks for its shared index in two places. The entries are
// shared between the reads of the process that find the same index: a file
// of the same size that ends with the same checksum, which Git writes anew
// whenever the index changes. The caller does not change them.
func (r *localRepo) index() (*localIndex, error) {
	path := filepath.Join(r.gitDir, "index")
	key, keyed := indexKey(path, r.hashSize())
	if keyed {
		if value, ok := indexCache.Load(path); ok {
			if cached, ok := value.(*cachedIndex); ok && cached.key == key {
				return cached.index, nil
			}
		}
	}
	index, err := readIndex(path, r.hashSize(), r.gitDir)
	if err != nil {
		return nil, err
	}
	if index.split && r.linked {
		return nil, unsupported("a split index in a linked worktree")
	}
	if index.key != "" {
		indexCache.Store(path, &cachedIndex{key: index.key, index: index})
	}
	return index, nil
}

// indexCache holds, by index path, the last index read there.
var indexCache sync.Map

type cachedIndex struct {
	key   string
	index *localIndex
}

// indexKey is the size of the index at path and the checksum it ends with,
// and false when it cannot be read.
func indexKey(path string, hashSize int) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < int64(12+hashSize) {
		return "", false
	}
	trailer := make([]byte, hashSize)
	if _, err := file.ReadAt(trailer, info.Size()-int64(hashSize)); err != nil {
		return "", false
	}
	return fmt.Sprintf("%d %x", info.Size(), trailer), true
}

// readConfig returns the configuration Git reads in the repository's root,
// from one `git config -z --list --show-origin` there with
// gitread.RepositoryEnvironment's environment, the environment of every Git
// call it replaces: includes, conditional includes and every scope are Git's
// own reading. The process keeps the reading while none of the files it
// depends on changes (parseConfig): the files Git reads configuration from
// (configCandidates), HEAD, which a conditional include on the branch reads,
// the files the entries came from and the files the includes name.
//
// The files known before the read are stamped before and after it: a change
// while Git read them moves the stamp, and the reading is not kept. A file
// modified less than a second before the read began is too close to tell: a
// rewrite of the same size in the same tick of the file system's clock keeps
// its stamp, so that reading is not kept either. A reading that is not
// cacheable is read again each time.
func readConfig(repo *localRepo) (*localConfig, int, error) {
	// The files Git reads depend on HOME and XDG_CONFIG_HOME: a reading is
	// kept for the ones it was made with.
	key := repo.root + "\x00" + os.Getenv("HOME") + "\x00" + os.Getenv("XDG_CONFIG_HOME")
	if value, ok := configCache.Load(key); ok {
		if cached, ok := value.(*cachedConfig); ok && cached.stamp == stampFiles(cached.files) {
			return cached.config, 0, nil
		}
	}
	known := slices.Concat(repo.configCandidates(), []string{filepath.Join(repo.gitDir, "HEAD")})
	before := stampFiles(known)
	started := time.Now()
	output, stderr, err := gitread.RunCapture(repo.root, "config", "-z", "--list", "--show-origin")
	if err != nil {
		return nil, 1, unsupported("git config: %v: %s", err, strings.TrimSpace(stderr))
	}
	config, files, cacheable, err := parseConfig(output, repo.root, filepath.Join(repo.commonDir, "config"))
	if err != nil {
		return nil, 1, err
	}
	var learned []string
	for _, file := range files {
		if !slices.Contains(known, file) {
			learned = append(learned, file)
		}
	}
	watched := slices.Concat(known, learned)
	// The stamp kept is taken before the checks: a write after it moves the
	// stamp the next read compares.
	stamp := stampFiles(watched)
	if cacheable && stampFiles(known) == before && !modifiedSince(watched, started.Add(-time.Second)) {
		configCache.Store(key, &cachedConfig{files: watched, stamp: stamp, config: config})
	} else {
		configCache.Delete(key)
	}
	return config, 1, nil
}

// modifiedSince reports whether one of files was modified at or after since:
// a change during the read, or one too close to it to tell from a later one
// that keeps the size.
func modifiedSince(files []string, since time.Time) bool {
	for _, file := range files {
		if info, err := os.Stat(file); err == nil && !info.ModTime().Before(since) {
			return true
		}
	}
	return false
}

// configCache holds, by root, the last configuration read there.
var configCache sync.Map

type cachedConfig struct {
	files  []string
	stamp  string
	config *localConfig
}

// configCandidates are the files Git reads its configuration from in the
// repository, whether they exist or not: a new one changes the reading.
func (r *localRepo) configCandidates() []string {
	files := []string{
		filepath.Join(r.commonDir, "config"),
		filepath.Join(r.gitDir, "config.worktree"),
		"/etc/gitconfig",
	}
	if home := os.Getenv("HOME"); home != "" {
		files = append(files, filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config", "git", "config"))
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		files = append(files, filepath.Join(xdg, "git", "config"))
	}
	return files
}

// stampFiles is what the file system says of files: for each, whether it
// exists, its size, its modification time and its inode.
func stampFiles(files []string) string {
	var stamp strings.Builder
	for _, file := range files {
		stamp.WriteString(file)
		stamp.WriteByte(0)
		stamp.WriteString(fileStamp(file))
		stamp.WriteByte(0)
	}
	return stamp.String()
}
