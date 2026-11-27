package git

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"go.putnami.dev/sdk/extension/gitread"
)

// HeadSHA is what `git rev-parse HEAD` answers in root: the object HEAD
// names. It is read in-process when root is the work tree's root of a
// repository the reader opens and HEAD is a plain ref (tooling/cli ADR 0112),
// and from Git otherwise.
func HeadSHA(root string) (string, error) {
	if repo, err := openLocal(root); err == nil {
		if sha, err := repo.headObject(); err == nil {
			return sha, nil
		}
	}
	return gitread.HeadSHA(root)
}

// CurrentBranch is gitread.CurrentBranch in root: the current branch's short
// name, and "HEAD" on a detached HEAD. It is read in-process as HeadSHA is.
func CurrentBranch(root string) (string, error) {
	if repo, err := openLocal(root); err == nil {
		if branch, err := repo.currentBranch(); err == nil {
			return branch, nil
		}
	}
	return gitread.CurrentBranch(root)
}

// head reads HEAD: the ref it names, or the object it holds when detached.
// A HEAD that is a link, or a repository whose refs live in a reftable, is
// unsupported.
func (r *localRepo) head() (ref, object string, err error) {
	if _, err := os.Stat(filepath.Join(r.commonDir, "reftable")); err == nil {
		return "", "", unsupported("a reftable")
	}
	path := filepath.Join(r.gitDir, "HEAD")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", unsupported("a HEAD that is no regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", unsupported("read HEAD: %v", err)
	}
	text := strings.TrimRight(string(data), "\n")
	if target, ok := strings.CutPrefix(text, "ref: "); ok {
		if !strings.HasPrefix(target, "refs/") || strings.ContainsAny(target, " \t\r\n") {
			return "", "", unsupported("HEAD names %q", target)
		}
		return target, "", nil
	}
	if r.objectID(text) {
		return "", text, nil
	}
	return "", "", unsupported("a HEAD that holds neither a ref nor an object")
}

// objectID reports whether s is a full object name of the repository's hash.
func (r *localRepo) objectID(s string) bool {
	return len(s) == 2*r.hashSize() && leadingObjectName([]byte(s), len(s)) && strings.ToLower(s) == s
}

// headObject answers `git rev-parse HEAD`. A HEAD on another ref than a
// branch, which may live in the worktree's own git directory, and a branch it
// cannot read as a loose or packed object name, such as an unborn one, are
// unsupported.
func (r *localRepo) headObject() (string, error) {
	ref, object, err := r.head()
	if err != nil || ref == "" {
		return object, err
	}
	if _, err := branchName(ref); err != nil {
		return "", err
	}
	return r.resolveRef(ref)
}

// branchName is the name of a branch ref, refs/heads/<name>, when Git would
// accept it as one (check_refname_format): no empty or dot-led component, no
// "..", "@{", ".lock" ending, control or special character.
func branchName(ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || name == "" || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") || name == "@" ||
		strings.ContainsAny(name, " ~^:?*[\\\x7f") {
		return "", unsupported("HEAD names %s", ref)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return "", unsupported("HEAD names %s", ref)
		}
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 {
			return "", unsupported("HEAD names %s", ref)
		}
	}
	return name, nil
}

// resolveRef reads the object a branch or tag ref names: its loose file,
// else its packed-refs line. A symbolic loose ref is unsupported.
func (r *localRepo) resolveRef(ref string) (string, error) {
	path := filepath.Join(r.commonDir, filepath.FromSlash(ref))
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return "", unsupported("the loose ref %s is no regular file", ref)
	}
	data, err := os.ReadFile(path)
	if err == nil {
		object := strings.TrimRight(string(data), "\n")
		if !r.objectID(object) {
			return "", unsupported("the loose ref %s holds %q", ref, object)
		}
		return object, nil
	}
	if !os.IsNotExist(err) {
		return "", unsupported("read the ref %s: %v", ref, err)
	}
	object, found, err := r.packedRef(ref)
	if err != nil {
		return "", err
	}
	if !found {
		return "", unsupported("no object for %s", ref)
	}
	return object, nil
}

// packedRef reads ref's line of packed-refs, which Git reads only as it
// writes it: an optional "# pack-refs with: " header first, every line ended
// by a newline, no carriage return.
func (r *localRepo) packedRef(ref string) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join(r.commonDir, "packed-refs"))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, unsupported("read packed-refs: %v", err)
	}
	if len(data) == 0 {
		return "", false, nil
	}
	if data[len(data)-1] != '\n' || bytes.IndexByte(data, '\r') >= 0 {
		return "", false, unsupported("a packed-refs file Git would refuse")
	}
	lines := bytes.Split(data[:len(data)-1], []byte("\n"))
	// Git trusts a file that says it is sorted and searches it: one that is
	// not, or that names a ref twice, may answer otherwise than a scan.
	var previous []byte
	for i, line := range lines {
		if i == 0 && len(line) > 0 && line[0] == '#' || len(line) > 0 && line[0] == '^' {
			continue
		}
		if _, name, ok := bytes.Cut(line, []byte(" ")); ok {
			if previous != nil && bytes.Compare(name, previous) <= 0 {
				return "", false, unsupported("a packed-refs file out of order")
			}
			previous = name
		}
	}
	for i, line := range lines {
		switch {
		case len(line) > 0 && line[0] == '#':
			if i != 0 || !bytes.HasPrefix(line, []byte("# pack-refs with: ")) {
				return "", false, unsupported("a packed-refs header Git would refuse")
			}
			continue
		case len(line) > 0 && line[0] == '^':
			if !r.objectID(string(line[1:])) {
				return "", false, unsupported("a packed-refs line Git would refuse")
			}
			continue
		}
		object, name, ok := bytes.Cut(line, []byte(" "))
		if !ok || !r.objectID(string(object)) || len(name) == 0 {
			return "", false, unsupported("a packed-refs line Git would refuse")
		}
		if string(name) == ref {
			return string(object), true, nil
		}
	}
	return "", false, nil
}

// refExists reports whether ref exists, loose or packed.
func (r *localRepo) refExists(ref string) (bool, error) {
	// A per-worktree ref, such as refs/bisect/*, lives in the worktree's own
	// git directory.
	for _, dir := range []string{r.commonDir, r.gitDir} {
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(ref))); err == nil {
			return true, nil
		}
	}
	_, found, err := r.packedRef(ref)
	return found, err
}

// currentBranch answers gitread.CurrentBranch: `symbolic-ref --short HEAD`,
// whose short name of refs/heads/<name> is <name> unless a file <name> of
// the git directory, refs/<name> or refs/tags/<name> would make it ambiguous
// (shorten_unambiguous_ref); on a detached HEAD, `rev-parse --abbrev-ref
// HEAD`, which answers HEAD. A HEAD on another kind of ref, and an
// ambiguous name, are unsupported.
func (r *localRepo) currentBranch() (string, error) {
	ref, _, err := r.head()
	if err != nil {
		return "", err
	}
	if ref == "" {
		// rev-parse --abbrev-ref HEAD answers HEAD unless a ref named HEAD
		// makes the name ambiguous.
		for _, other := range []string{"refs/HEAD", "refs/tags/HEAD", "refs/heads/HEAD", "refs/remotes/HEAD", "refs/remotes/HEAD/HEAD"} {
			exists, err := r.refExists(other)
			if err != nil {
				return "", err
			}
			if exists {
				return "", unsupported("the ref %s makes HEAD ambiguous", other)
			}
		}
		return "HEAD", nil
	}
	name, err := branchName(ref)
	if err != nil {
		return "", err
	}
	// symbolic-ref follows a branch that is itself symbolic.
	branch := filepath.Join(r.commonDir, filepath.FromSlash(ref))
	if info, err := os.Lstat(branch); err == nil {
		if !info.Mode().IsRegular() {
			return "", unsupported("the branch %s is no regular file", ref)
		}
		data, err := os.ReadFile(branch)
		if err != nil || !r.objectID(strings.TrimRight(string(data), "\n")) {
			return "", unsupported("the branch %s is symbolic, unreadable or holds no object name", ref)
		}
	}
	for _, dir := range []string{r.gitDir, r.commonDir} {
		if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name))); err == nil {
			return "", unsupported("a file %s of the git directory", name)
		}
	}
	for _, other := range []string{"refs/" + name, "refs/tags/" + name} {
		exists, err := r.refExists(other)
		if err != nil {
			return "", err
		}
		if exists {
			return "", unsupported("the ref %s makes %s ambiguous", other, name)
		}
	}
	return name, nil
}

// IsShallow is gitread.IsShallow in root: whether the repository is a
// shallow clone, which its shallow file says. It is read in-process as
// HeadSHA is; an empty shallow file is Git's answer.
func IsShallow(root string) (bool, error) {
	if repo, err := openLocal(root); err == nil {
		info, err := os.Stat(filepath.Join(repo.commonDir, "shallow"))
		switch {
		case os.IsNotExist(err):
			return false, nil
		case err == nil && info.Mode().IsRegular() && info.Size() > 0:
			if repo.validShallow() {
				return true, nil
			}
		}
	}
	return gitread.IsShallow(root)
}

// validShallow reports whether every line of the shallow file is an object
// name, as Git reads it; any other file is Git's to refuse.
func (r *localRepo) validShallow() bool {
	data, err := os.ReadFile(filepath.Join(r.commonDir, "shallow"))
	if err != nil || len(data) == 0 || data[len(data)-1] != '\n' {
		return false
	}
	for _, line := range strings.Split(string(data[:len(data)-1]), "\n") {
		if !r.objectID(line) {
			return false
		}
	}
	return true
}
