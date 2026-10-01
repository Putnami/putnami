// Package refstore is the Git memory backend: the store layout of package
// store, committed on one branch of a Git repository.
//
// Without a remote the records live on the branch of the local repository,
// and a write moves the branch with `git update-ref <ref> <new> <old>`, which
// Git performs under the ref's lock only if the branch still points at
// <old>. With a remote the remote branch is the store: a write builds its
// commit on the remote head it read and pushes it with
// `--force-with-lease=<ref>:<head>`, which the remote accepts only if its
// branch still points there. Either way a write that lost the race writes
// nothing, re-reads the store, and is rebuilt on the new head only if the
// record it changes is still the one it compared; otherwise it is a conflict.
//
// A write whose outcome is unknown — a push that died without reporting its
// ref — is reconciled before anything else happens: the remote branch is read
// again, and the write landed if the branch points at its commit or descends
// from it. When the remote cannot be read the write is unresolved, and it is
// never retried.
//
// Commits are built with plumbing (hash-object, read-tree into a private
// index, update-index, write-tree, commit-tree): no working tree, index,
// checkout or hook of the repository is touched, and nothing is signed.
package refstore

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/tooling/memory-store/internal/store"
)

// DefaultBranch is the branch that holds the records when the settings name
// none.
const DefaultBranch = "putnami-memory"

// maxAttempts bounds the attempts of a write that keeps losing the race to
// other writers; it then stops, having written nothing.
const maxAttempts = 8

// Write deadlines. Every command of a write's attempts ends within
// writeBudget of its start; reconciling a push whose answer was lost gets
// reconcileBudget more. Their sum stays below the manifest's tool timeout
// (180 s), so the provider answers before the orchestrator gives up on it.
var (
	writeBudget     = 100 * time.Second
	reconcileBudget = 60 * time.Second
)

// Commit identity. Records carry their own provenance; the commit names the
// provider, never the person or the organization.
const (
	committerName  = "Putnami memory"
	committerEmail = "memory@putnami.invalid"
)

// Config names a Git store.
type Config struct {
	// Path is the local repository, absolute: a clone, a bare repository, or
	// a directory the backend creates as a bare repository on first use.
	Path string
	// Remote is empty when the records live in Path, otherwise the name of a
	// remote of Path or a URL. It never carries credentials; Git's own
	// credential helpers and SSH agent authenticate.
	Remote string
	// Branch holds the records.
	Branch string
	// Now dates commits.
	Now func() time.Time
	// Environ is added to the environment of every git command.
	Environ []string
	// Intercept, when set, runs every git command.
	Intercept Intercept
}

// Store is one Git store.
type Store struct {
	cfg    Config
	env    []string
	gitDir string
}

// Open names a Git store. Nothing is created until a command needs the
// repository.
func Open(cfg Config) *Store {
	if cfg.Branch == "" {
		cfg.Branch = DefaultBranch
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Store{cfg: cfg, env: environment(cfg.Environ)}
}

// Kind is the reference source kind of a Git store.
func (s *Store) Kind() string { return "git" }

func (s *Store) branchRef() string { return "refs/heads/" + s.cfg.Branch }

// trackingRef is where a fetched remote head is kept, outside every
// namespace a person or another tool uses.
func (s *Store) trackingRef() string { return "refs/putnami/memory/" + s.cfg.Branch }

func (s *Store) run(ctx context.Context, timeout time.Duration, stdin []byte, extraEnv []string, args ...string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	full := slices.Clone(configuration)
	if s.gitDir != "" {
		full = append(full, "--git-dir="+s.gitDir)
	}
	full = append(full, args...)
	env := s.env
	if len(extraEnv) > 0 {
		env = append(slices.Clip(env), extraEnv...)
	}
	run := func() (Result, error) { return execGit(ctx, env, stdin, full...) }
	if s.cfg.Intercept != nil {
		return s.cfg.Intercept(ctx, args, run)
	}
	return run()
}

// Read returns a view of the store as the branch holds it now: with a remote,
// as the remote holds it.
func (s *Store) Read(ctx context.Context) (store.View, error) {
	exists, err := s.open(ctx, s.cfg.Remote != "")
	if err != nil {
		return nil, err
	}
	if !exists {
		return &view{s: s}, nil
	}
	head, err := s.refresh(ctx)
	if err != nil {
		return nil, err
	}
	if head == "" {
		return &view{s: s}, nil
	}
	meta, err := s.readMeta(ctx, head)
	if err != nil {
		return nil, err
	}
	return &view{s: s, commit: head, id: meta.ID}, nil
}

// Update writes one record in a new commit on the branch, conditional on the
// head the change was decided from.
func (s *Store) Update(ctx context.Context, id string, change store.Change) (*store.Document, string, error) {
	outer := ctx
	ctx, cancel := context.WithTimeout(ctx, writeBudget)
	defer cancel()
	if _, err := s.open(ctx, true); err != nil {
		return nil, "", err
	}
	if s.cfg.Remote == "" {
		if err := s.refuseCheckedOutBranch(ctx); err != nil {
			return nil, "", err
		}
	}
	for attempt := 1; ; attempt++ {
		base, err := s.refresh(ctx)
		if err != nil {
			return nil, "", err
		}
		var (
			meta    store.Meta
			current *store.Document
			created *store.Meta
		)
		if base != "" {
			if meta, err = s.readMeta(ctx, base); err != nil {
				return nil, "", err
			}
			if current, err = s.document(ctx, base, id); err != nil {
				return nil, "", err
			}
			if meta.ID == "" {
				if err := s.refuseOrphanRecords(ctx, base); err != nil {
					return nil, "", err
				}
			}
		}
		if meta.ID == "" {
			if meta, err = store.NewMeta(); err != nil {
				return nil, "", store.Failf(store.Unavailable, "store.unwritable", false, "%v", err)
			}
			created = &meta
		}
		next, err := change(meta.ID, current)
		if err != nil {
			return nil, "", err
		}
		record, err := store.Encode(next)
		if err != nil {
			return nil, "", err
		}
		commit, err := s.commit(ctx, base, created, id, next, record)
		if err != nil {
			return nil, "", err
		}
		state, detail := s.publish(ctx, base, commit)
		switch state {
		case published:
			return next, meta.ID, nil
		case moved:
			if attempt >= maxAttempts || ctx.Err() != nil {
				return nil, "", store.Failf(store.Unavailable, "store.contended", true,
					"the memory branch kept moving under %d attempts; nothing was written", attempt)
			}
			continue
		case refused:
			return nil, "", store.Failf(store.Denied, "store.refused", false, "the remote refused the write (%s); nothing was written", detail)
		case failed:
			return nil, "", store.Failf(store.Unavailable, "store.unwritable", true, "%s; nothing was written", detail)
		}
		return s.settleUncertain(outer, base, commit, next, meta.ID, detail)
	}
}

// settleUncertain reconciles a write whose outcome is unknown, before any
// retry: it landed, it did not, or nobody can tell and it is unresolved. It
// runs within reconcileBudget, even when the write's own deadline passed.
func (s *Store) settleUncertain(ctx context.Context, base, commit string, next *store.Document, storeID, detail string) (*store.Document, string, error) {
	reconcile, cancel := context.WithTimeout(context.WithoutCancel(ctx), reconcileBudget)
	defer cancel()
	head, err := s.head(reconcile)
	if err != nil {
		return nil, "", store.Failf(store.Unresolved, "store.unreconciled", false,
			"the write's outcome is unknown (%s), and the branch could not be read again to settle it: %v", detail, err)
	}
	landed, err := s.contains(reconcile, base, commit, head)
	switch {
	case err != nil:
		return nil, "", store.Failf(store.Unresolved, "store.unreconciled", false,
			"the write's outcome is unknown (%s), and the branch could not be compared with it: %v", detail, err)
	case landed:
		return next, storeID, nil
	}
	return nil, "", store.Failf(store.Unavailable, "store.unwritable", true,
		"the write did not land (%s); the branch does not contain it, so nothing was written", detail)
}

// contains reports whether head is commit or descends from it. A head that is
// absent or still at base does not.
func (s *Store) contains(ctx context.Context, base, commit, head string) (bool, error) {
	switch {
	case head == commit:
		return true, nil
	case head == "" || head == base:
		return false, nil
	}
	if s.cfg.Remote != "" {
		has, err := s.hasCommit(ctx, head)
		if err != nil {
			return false, err
		}
		if !has {
			if err := s.fetch(ctx); err != nil {
				return false, err
			}
		}
	}
	out, err := s.run(ctx, localTimeout, nil, nil, "merge-base", "--is-ancestor", commit, head)
	switch {
	case err != nil:
		return false, err
	case out.code == 0:
		return true, nil
	case out.code == 1:
		return false, nil
	}
	return false, errors.New(describe([]string{"merge-base"}, out))
}

// publication is what an attempt to move the branch did.
type publication int

const (
	// published: the branch points at the new commit.
	published publication = iota
	// moved: the branch moved away from the base first; nothing was written.
	moved
	// refused: the remote refused the write; nothing was written.
	refused
	// failed: nothing was written, for another reason.
	failed
	// uncertain: the write may have landed.
	uncertain
)

func (s *Store) publish(ctx context.Context, base, commit string) (publication, string) {
	if s.cfg.Remote == "" {
		return s.updateRef(ctx, base, commit)
	}
	return s.push(ctx, base, commit)
}

// updateRef moves the local branch from base (absent when empty) to commit.
// Git does it under the ref's lock and only from base, so its failure is
// read back rather than parsed.
func (s *Store) updateRef(ctx context.Context, base, commit string) (publication, string) {
	args := []string{"update-ref", "-m", "putnami memory checkpoint", s.branchRef(), commit, base}
	out, err := s.run(ctx, localTimeout, nil, nil, args...)
	if err == nil && out.code == 0 {
		return published, ""
	}
	detail := describe(args, out)
	if err != nil {
		detail = err.Error()
	}
	head, readErr := s.localHead(ctx, s.branchRef())
	if readErr != nil {
		return uncertain, detail
	}
	landed, compareErr := s.contains(ctx, base, commit, head)
	switch {
	case compareErr != nil:
		return uncertain, detail
	case landed:
		return published, ""
	case head == base:
		return failed, detail
	}
	return moved, detail
}

// push moves the remote branch from base (absent when empty) to commit with a
// lease on base. A ref status line in the porcelain output is the remote's
// answer; its absence means the push died before the remote answered.
func (s *Store) push(ctx context.Context, base, commit string) (publication, string) {
	ref := s.branchRef()
	args := []string{"push", "--porcelain", "--no-verify", "--force-with-lease=" + ref + ":" + base, s.cfg.Remote, commit + ":" + ref}
	out, err := s.run(ctx, networkTimeout, nil, nil, args...)
	detail := describe(args, out)
	if err != nil {
		detail = err.Error()
	}
	flag, status, found := porcelainStatus(out.stdout, ref)
	if !found {
		return uncertain, detail
	}
	switch flag {
	case ' ', '+', '*', '=':
		return published, ""
	case '!':
		head, readErr := s.remoteHead(ctx)
		switch {
		case readErr != nil:
			return failed, "the remote refused the write (" + status + ")"
		case head != base:
			return moved, status
		}
		return refused, status
	}
	return uncertain, detail
}

// porcelainStatus finds the status line of ref in `git push --porcelain`
// output: "<flag>\t<source>:<ref>\t<summary>".
func porcelainStatus(stdout []byte, ref string) (byte, string, bool) {
	for line := range strings.Lines(string(stdout)) {
		fields := strings.SplitN(strings.TrimRight(line, "\n"), "\t", 3)
		if len(fields) == 3 && len(fields[0]) == 1 && strings.HasSuffix(fields[1], ":"+ref) {
			return fields[0][0], fields[2], true
		}
	}
	return 0, "", false
}

// refresh returns the commit the store is at: the local branch, or with a
// remote the remote branch, fetched when its commit is not here yet. "" is
// an empty store.
func (s *Store) refresh(ctx context.Context) (string, error) {
	if s.cfg.Remote == "" {
		return s.localHead(ctx, s.branchRef())
	}
	head, err := s.remoteHead(ctx)
	if err != nil || head == "" {
		return "", err
	}
	has, err := s.hasCommit(ctx, head)
	if err != nil || has {
		return head, err
	}
	if err := s.fetch(ctx); err != nil {
		return "", err
	}
	return s.localHead(ctx, s.trackingRef())
}

// head is the branch's current commit without fetching it.
func (s *Store) head(ctx context.Context) (string, error) {
	if s.cfg.Remote == "" {
		return s.localHead(ctx, s.branchRef())
	}
	return s.remoteHead(ctx)
}

func (s *Store) localHead(ctx context.Context, ref string) (string, error) {
	out, err := s.run(ctx, localTimeout, nil, nil, "rev-parse", "--verify", "--quiet", ref)
	switch {
	case err != nil:
		return "", store.Failf(store.Unavailable, "store.unreadable", true, "read the memory branch: %v", err)
	case out.code == 1:
		return "", nil
	case out.code != 0:
		return "", store.Failf(store.Unavailable, "store.unreadable", true, "read the memory branch: %s", describe([]string{"rev-parse"}, out))
	}
	return strings.TrimSpace(string(out.stdout)), nil
}

func (s *Store) remoteHead(ctx context.Context) (string, error) {
	ref := s.branchRef()
	args := []string{"ls-remote", "--exit-code", s.cfg.Remote, ref}
	out, err := s.run(ctx, networkTimeout, nil, nil, args...)
	switch {
	case err != nil:
		return "", unreachable(err.Error())
	case out.code == 2:
		return "", nil
	case out.code != 0:
		return "", unreachable(describe(args, out))
	}
	for line := range strings.Lines(string(out.stdout)) {
		oid, name, found := strings.Cut(strings.TrimSpace(line), "\t")
		if found && name == ref {
			return oid, nil
		}
	}
	return "", nil
}

func (s *Store) fetch(ctx context.Context) error {
	args := []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules",
		s.cfg.Remote, "+" + s.branchRef() + ":" + s.trackingRef()}
	out, err := s.run(ctx, networkTimeout, nil, nil, args...)
	switch {
	case err != nil:
		return unreachable(err.Error())
	case out.code != 0:
		return unreachable(describe(args, out))
	}
	return nil
}

func unreachable(detail string) error {
	return store.Failf(store.Unavailable, "store.unreachable", true, "the memory remote could not be reached: %s", detail)
}

func (s *Store) hasCommit(ctx context.Context, oid string) (bool, error) {
	out, err := s.run(ctx, localTimeout, nil, nil, "cat-file", "-e", oid+"^{commit}")
	if err != nil {
		return false, store.Failf(store.Unavailable, "store.unreadable", true, "read the memory repository: %v", err)
	}
	return out.code == 0, nil
}

// open locates the repository at Path, creating a bare one when create is
// set and Path does not hold one yet. It reports whether a repository exists.
func (s *Store) open(ctx context.Context, create bool) (bool, error) {
	if s.gitDir != "" {
		return true, nil
	}
	info, err := os.Stat(s.cfg.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !create {
			return false, nil
		}
		if err := s.initialize(ctx); err != nil {
			return false, err
		}
	case err != nil:
		return false, store.Failf(store.Unavailable, "store.unreadable", true, "read the memory repository: %v", err)
	case !info.IsDir():
		return false, store.Failf(store.Invalid, "settings.invalid", false, "the memory repository path %s is not a directory", s.cfg.Path)
	}
	gitDir, err := s.locate(ctx)
	if err != nil {
		return false, err
	}
	if gitDir == "" {
		entries, readErr := os.ReadDir(s.cfg.Path)
		switch {
		case readErr != nil:
			return false, store.Failf(store.Unavailable, "store.unreadable", true, "read the memory repository: %v", readErr)
		case len(entries) > 0:
			return false, store.Failf(store.Invalid, "settings.invalid", false,
				"%s is neither a Git repository nor an empty directory", s.cfg.Path)
		case !create:
			return false, nil
		}
		if err := s.initIn(ctx, s.cfg.Path); err != nil {
			return false, err
		}
		gitDir = s.cfg.Path
	}
	s.gitDir = gitDir
	return true, nil
}

// locate returns the Git directory of Path, or "" when Path holds no
// repository. It never looks above Path.
func (s *Store) locate(ctx context.Context) (string, error) {
	if isBare(s.cfg.Path) {
		return s.cfg.Path, nil
	}
	if _, err := os.Stat(filepath.Join(s.cfg.Path, ".git")); err != nil {
		return "", nil
	}
	out, err := s.run(ctx, localTimeout, nil, nil, "-C", s.cfg.Path, "rev-parse", "--absolute-git-dir")
	switch {
	case err != nil:
		return "", store.Failf(store.Unavailable, "store.unreadable", true, "read the memory repository: %v", err)
	case out.code != 0:
		return "", store.Failf(store.Invalid, "settings.invalid", false, "%s is not a usable Git repository: %s", s.cfg.Path, summary(out.stderr))
	}
	return strings.TrimSpace(string(out.stdout)), nil
}

func isBare(path string) bool {
	for _, name := range []string{"HEAD", "objects", "refs"} {
		if _, err := os.Stat(filepath.Join(path, name)); err != nil {
			return false
		}
	}
	return true
}

// initialize creates the repository at Path. It is built in a sibling
// directory and renamed into place, so two first writers never see a half
// initialized repository: the one whose rename loses uses the other's.
func (s *Store) initialize(ctx context.Context) error {
	parent := filepath.Dir(s.cfg.Path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return store.Failf(store.Unavailable, "store.unwritable", false, "create the memory repository: %v", err)
	}
	temp, err := os.MkdirTemp(parent, "."+filepath.Base(s.cfg.Path)+".init-*")
	if err != nil {
		return store.Failf(store.Unavailable, "store.unwritable", false, "create the memory repository: %v", err)
	}
	defer os.RemoveAll(temp)
	if err := s.initIn(ctx, temp); err != nil {
		return err
	}
	if err := os.Rename(temp, s.cfg.Path); err != nil && !isBare(s.cfg.Path) {
		return store.Failf(store.Unavailable, "store.unwritable", false, "create the memory repository: %v", err)
	}
	return nil
}

func (s *Store) initIn(ctx context.Context, dir string) error {
	args := []string{"init", "--bare", "--quiet", "--initial-branch=" + s.cfg.Branch, dir}
	out, err := s.run(ctx, localTimeout, nil, nil, args...)
	switch {
	case err != nil:
		return store.Failf(store.Unavailable, "store.unwritable", false, "create the memory repository: %v", err)
	case out.code != 0:
		return store.Failf(store.Unavailable, "store.unwritable", false, "create the memory repository: %s", describe(args, out))
	}
	return nil
}

// refuseCheckedOutBranch refuses to move a branch a worktree of the
// repository has checked out: its files would silently stop matching it.
func (s *Store) refuseCheckedOutBranch(ctx context.Context) error {
	out, err := s.run(ctx, localTimeout, nil, nil, "worktree", "list", "--porcelain")
	switch {
	case err != nil:
		return store.Failf(store.Unavailable, "store.unreadable", true, "read the memory repository: %v", err)
	case out.code != 0:
		return store.Failf(store.Unavailable, "store.unreadable", true, "read the memory repository: %s", describe([]string{"worktree"}, out))
	}
	for line := range strings.Lines(string(out.stdout)) {
		if strings.TrimSpace(line) == "branch "+s.branchRef() {
			return store.Failf(store.Invalid, "settings.invalid", false,
				"the memory branch %s is checked out in a worktree of %s; bind another branch", s.cfg.Branch, s.cfg.Path)
		}
	}
	return nil
}

// commit builds the commit that writes doc, encoded as data, as record id
// (and the store identity, for a new store) on top of base, in a private
// index.
func (s *Store) commit(ctx context.Context, base string, created *store.Meta, id string, doc *store.Document, data []byte) (string, error) {
	record, err := s.hashObject(ctx, data)
	if err != nil {
		return "", err
	}
	updates := []string{"update-index", "--add", "--cacheinfo", "100644," + record + "," + store.RecordPath(id)}
	if created != nil {
		meta, err := s.hashObject(ctx, store.EncodeMeta(*created))
		if err != nil {
			return "", err
		}
		updates = append(updates, "--cacheinfo", "100644,"+meta+","+store.MetaPath)
	}
	dir, err := os.MkdirTemp("", "putnami-memory-index-")
	if err != nil {
		return "", store.Failf(store.Unavailable, "store.unwritable", false, "prepare the memory commit: %v", err)
	}
	defer os.RemoveAll(dir)
	index := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	read := []string{"read-tree", "--empty"}
	if base != "" {
		read = []string{"read-tree", base}
	}
	if _, err := s.plumbing(ctx, nil, index, read...); err != nil {
		return "", err
	}
	if _, err := s.plumbing(ctx, nil, index, updates...); err != nil {
		return "", err
	}
	tree, err := s.plumbing(ctx, nil, index, "write-tree")
	if err != nil {
		return "", err
	}
	date := "@" + strconv.FormatInt(s.cfg.Now().Unix(), 10) + " +0000"
	identity := []string{
		"GIT_AUTHOR_NAME=" + committerName, "GIT_AUTHOR_EMAIL=" + committerEmail, "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + committerName, "GIT_COMMITTER_EMAIL=" + committerEmail, "GIT_COMMITTER_DATE=" + date,
	}
	args := []string{"commit-tree", "--no-gpg-sign", "-m", "checkpoint " + id + " at revision " + doc.Revision()}
	if base != "" {
		args = append(args, "-p", base)
	}
	return s.plumbing(ctx, nil, identity, append(args, tree)...)
}

func (s *Store) hashObject(ctx context.Context, data []byte) (string, error) {
	return s.plumbing(ctx, data, nil, "hash-object", "-w", "--stdin")
}

// plumbing runs a local command that must succeed and returns its trimmed
// output. Its failure writes nothing a reader can see: objects and a private
// index are not the branch.
func (s *Store) plumbing(ctx context.Context, stdin []byte, env []string, args ...string) (string, error) {
	out, err := s.run(ctx, localTimeout, stdin, env, args...)
	switch {
	case err != nil:
		return "", store.Failf(store.Unavailable, "store.unwritable", true, "prepare the memory commit: %v", err)
	case out.code != 0:
		return "", store.Failf(store.Unavailable, "store.unwritable", true, "prepare the memory commit: %s", describe(args, out))
	}
	return strings.TrimSpace(string(out.stdout)), nil
}

// entry is one blob of a tree listing.
type entry struct {
	path string
	oid  string
	size int64
}

// entries lists the blobs at paths in commit ("records/" lists the records).
func (s *Store) entries(ctx context.Context, commit string, paths ...string) ([]entry, error) {
	args := append([]string{"ls-tree", "-z", "-l", "--full-tree", commit, "--"}, paths...)
	out, err := s.run(ctx, localTimeout, nil, nil, args...)
	switch {
	case err != nil:
		return nil, store.Failf(store.Unavailable, "store.unreadable", true, "read the memory branch: %v", err)
	case out.code != 0:
		return nil, store.Failf(store.Unavailable, "store.unreadable", true, "read the memory branch: %s", describe(args, out))
	}
	var listed []entry
	for _, record := range bytes.Split(out.stdout, []byte{0}) {
		header, path, found := strings.Cut(string(record), "\t")
		fields := strings.Fields(header)
		if !found || len(fields) != 4 || fields[1] != "blob" {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			continue
		}
		listed = append(listed, entry{path: path, oid: fields[2], size: size})
	}
	return listed, nil
}

// blobs reads the contents of listed blobs with one cat-file --batch.
func (s *Store) blobs(ctx context.Context, listed []entry) ([][]byte, error) {
	if len(listed) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, item := range listed {
		if item.size > store.MaxDocumentBytes {
			return nil, store.Failf(store.Unavailable, "store.invalid", false, "%s exceeds %d bytes", item.path, store.MaxDocumentBytes)
		}
		input.WriteString(item.oid + "\n")
	}
	out, err := s.run(ctx, localTimeout, []byte(input.String()), nil, "cat-file", "--batch")
	switch {
	case err != nil:
		return nil, store.Failf(store.Unavailable, "store.unreadable", true, "read the memory branch: %v", err)
	case out.code != 0:
		return nil, store.Failf(store.Unavailable, "store.unreadable", true, "read the memory branch: %s", describe([]string{"cat-file"}, out))
	}
	contents := make([][]byte, 0, len(listed))
	rest := out.stdout
	for _, item := range listed {
		header, body, found := bytes.Cut(rest, []byte{'\n'})
		fields := strings.Fields(string(header))
		if !found || len(fields) != 3 || fields[1] != "blob" {
			return nil, store.Failf(store.Unavailable, "store.unreadable", true, "read %s: unexpected cat-file answer %q", item.path, header)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 || size+1 > len(body) {
			return nil, store.Failf(store.Unavailable, "store.unreadable", true, "read %s: truncated cat-file answer", item.path)
		}
		contents = append(contents, body[:size])
		rest = body[size+1:]
	}
	return contents, nil
}

func (s *Store) readMeta(ctx context.Context, commit string) (store.Meta, error) {
	listed, err := s.entries(ctx, commit, store.MetaPath)
	if err != nil || len(listed) == 0 {
		return store.Meta{}, err
	}
	contents, err := s.blobs(ctx, listed)
	if err != nil {
		return store.Meta{}, err
	}
	meta, err := store.DecodeMeta(contents[0])
	if err != nil {
		return store.Meta{}, store.Failf(store.Unavailable, "store.invalid", false, "%v", err)
	}
	return meta, nil
}

func (s *Store) document(ctx context.Context, commit, id string) (*store.Document, error) {
	listed, err := s.entries(ctx, commit, store.RecordPath(id))
	if err != nil || len(listed) == 0 {
		return nil, err
	}
	contents, err := s.blobs(ctx, listed)
	if err != nil {
		return nil, err
	}
	doc, err := store.Decode(id, contents[0])
	if err != nil {
		return nil, store.Failf(store.Unavailable, "store.invalid", false, "%v", err)
	}
	return doc, nil
}

// records lists the record documents in commit, with their ids.
func (s *Store) records(ctx context.Context, commit string) ([]entry, map[string]string, error) {
	listed, err := s.entries(ctx, commit, store.RecordsDir+"/")
	if err != nil {
		return nil, nil, err
	}
	records := make([]entry, 0, len(listed))
	ids := make(map[string]string, len(listed))
	for _, item := range listed {
		name, found := strings.CutPrefix(item.path, store.RecordsDir+"/")
		if !found || strings.Contains(name, "/") {
			continue
		}
		if id, ok := store.IDFromFileName(name); ok {
			records = append(records, item)
			ids[item.path] = id
		}
	}
	return records, ids, nil
}

// refuseOrphanRecords refuses to give an identity to a branch that already
// holds a record: those records were written under an identity that is gone,
// and a new one would change the source of every reference to them.
func (s *Store) refuseOrphanRecords(ctx context.Context, commit string) error {
	records, _, err := s.records(ctx, commit)
	if err != nil {
		return err
	}
	if len(records) > 0 {
		return store.Failf(store.Unavailable, "store.invalid", false,
			"the memory branch holds records but no %s identity document; nothing was written", store.MetaPath)
	}
	return nil
}

func (s *Store) documents(ctx context.Context, commit string) ([]*store.Document, error) {
	records, ids, err := s.records(ctx, commit)
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].path < records[j].path })
	contents, err := s.blobs(ctx, records)
	if err != nil {
		return nil, err
	}
	docs := make([]*store.Document, 0, len(records))
	for i, item := range records {
		doc, err := store.Decode(ids[item.path], contents[i])
		if err != nil {
			return nil, store.Failf(store.Unavailable, "store.invalid", false, "%v", err)
		}
		docs = append(docs, doc)
	}
	return docs, nil
}

type view struct {
	s      *Store
	commit string
	id     string
}

func (v *view) StoreID() string { return v.id }

func (v *view) Get(ctx context.Context, id string) (*store.Document, error) {
	if v.commit == "" {
		return nil, nil
	}
	return v.s.document(ctx, v.commit, id)
}

func (v *view) List(ctx context.Context) ([]*store.Document, error) {
	if v.commit == "" {
		return nil, nil
	}
	return v.s.documents(ctx, v.commit)
}
