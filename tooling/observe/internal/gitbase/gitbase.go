// Package gitbase finds the commit a change is measured against and the files
// the change touches, in the repository a CI job checked out.
//
// The base is a commit or a branch of the origin remote. On a shallow clone the
// base and the history between it and the head may be missing: when fetching
// is allowed, the base is fetched and the history is deepened in bounded steps
// until the merge base of the base and the head is reachable. Fetching writes
// Git objects and the shallow boundary only; the working tree, the index and
// HEAD stay as they were. Every failure is a diagnostic, never an error.
package gitbase

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/tooling/observe/internal/ci"
)

// File statuses.
const (
	StatusAdded    = "added"
	StatusModified = "modified"
	StatusDeleted  = "deleted"
	StatusRenamed  = "renamed"
)

// Diagnostic codes.
const (
	CodeNotARepository       = "observe.not_a_repository"
	CodeHeadUnresolved       = "observe.head_unresolved"
	CodeBaseUnresolved       = "observe.base_unresolved"
	CodeMergeBaseUnreachable = "observe.merge_base_unreachable"
	CodeDiffFailed           = "observe.diff_failed"
	CodeTooManyChangedFiles  = "observe.too_many_changed_files"
)

// remote is the remote a CI checkout fetches from.
const remote = "origin"

// Bounds of the history walk: the first deepening step, and the number of
// steps, each twice the previous one. Together they reach about 65 000
// commits behind the shallow boundary.
const (
	firstDeepenStep = 64
	maxDeepenSteps  = 10
)

// MaxChangedFiles caps the changed files a result lists.
const MaxChangedFiles = 20000

// ChangedFile is one file of the diff, its path relative to the repository
// root.
type ChangedFile struct {
	Path         string `json:"path"`
	Status       string `json:"status"`
	PreviousPath string `json:"previousPath,omitempty"`
}

// Request says what to resolve.
type Request struct {
	// Dir is a directory inside the repository.
	Dir string
	// Head is the revision that was built.
	Head string
	// Base is the base candidate; nil resolves the head only.
	Base *ci.Base
	// Fetch allows fetching the base and deepening a shallow clone.
	Fetch bool
}

// Result is what Git answered.
type Result struct {
	// Root is the repository's top directory.
	Root string
	// HeadSHA is the commit Head names.
	HeadSHA string
	// BaseSHA is the merge base of the base and the head: the commit the change
	// is measured against.
	BaseSHA string
	// Deepened counts the commits the history was deepened by.
	Deepened int
	// ChangedFiles is the diff from BaseSHA to HeadSHA.
	ChangedFiles []ChangedFile
	Diagnostics  []diag.Diagnostic
}

// Resolve resolves req. Every failure is a diagnostic of the result.
func Resolve(ctx context.Context, req Request) Result {
	var r Result
	g := git{ctx: ctx, dir: req.Dir}
	root, err := g.output("rev-parse", "--show-toplevel")
	if err != nil {
		r.warn(CodeNotARepository, "", "%s is not inside a Git repository: %v", req.Dir, err)
		return r
	}
	r.Root = root
	g.dir = root
	if r.HeadSHA, err = g.commit(req.Head); err != nil {
		r.warn(CodeHeadUnresolved, "head", "the head %s names no commit: %v", req.Head, err)
		return r
	}
	if req.Base == nil {
		return r
	}
	base, ok := r.resolveBase(g, req)
	if !ok {
		return r
	}
	if !r.mergeBase(g, req, base) {
		return r
	}
	r.diff(g)
	return r
}

// resolveBase finds the base commit, fetching it when it is missing and
// fetching is allowed.
func (r *Result) resolveBase(g git, req Request) (string, bool) {
	base := req.Base
	if sha, ok := lookup(g, base); ok {
		return sha, true
	}
	if !req.Fetch {
		r.warn(CodeBaseUnresolved, "base", "the base %s is not in the local history and --no-fetch forbids fetching it", describe(base))
		return "", false
	}
	depth := 0
	if g.shallow() {
		depth = firstDeepenStep
	}
	var errs []string
	// A server that refuses a fetch by commit id still sends the commit
	// with the branch that holds it.
	if base.SHA != "" {
		if err := g.fetch(depth, base.SHA); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if branch, ok := branchOf(base.Ref); ok {
		if _, found := lookup(g, base); !found {
			if err := g.fetch(depth, "+refs/heads/"+branch+":refs/remotes/"+remote+"/"+branch); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if sha, ok := lookup(g, base); ok {
		return sha, true
	}
	reason := "it names no commit of " + remote
	if len(errs) > 0 {
		reason = strings.Join(errs, "; ")
	}
	r.warn(CodeBaseUnresolved, "base", "the base %s cannot be fetched: %s", describe(base), reason)
	return "", false
}

// mergeBase sets BaseSHA, deepening a shallow clone in bounded steps until the
// merge base of base and the head is reachable.
func (r *Result) mergeBase(g git, req Request, base string) bool {
	step := firstDeepenStep
	for round := 0; ; round++ {
		if sha, err := g.output("merge-base", base, r.HeadSHA); err == nil {
			r.BaseSHA = sha
			return true
		}
		shallow := g.shallow()
		switch {
		case !shallow:
			r.warn(CodeMergeBaseUnreachable, "base", "the base %s and the head %s share no commit", base, r.HeadSHA)
			return false
		case !req.Fetch:
			r.warn(CodeMergeBaseUnreachable, "base", "the clone is shallow and --no-fetch forbids deepening it to the merge base of %s and %s", base, r.HeadSHA)
			return false
		case round == maxDeepenSteps:
			r.warn(CodeMergeBaseUnreachable, "base", "the merge base of %s and %s is more than %d commits behind the shallow clone", base, r.HeadSHA, r.Deepened)
			return false
		}
		if err := g.deepen(step, base, r.HeadSHA); err != nil {
			r.warn(CodeMergeBaseUnreachable, "base", "the shallow clone cannot be deepened to the merge base of %s and %s: %v", base, r.HeadSHA, err)
			return false
		}
		r.Deepened += step
		step *= 2
	}
}

// diff lists the files changed from BaseSHA to HeadSHA, renames detected.
func (r *Result) diff(g git) {
	out, err := g.raw("-c", "core.quotePath=false", "diff", "--no-ext-diff", "--no-textconv", "--name-status", "-z", "-M", r.BaseSHA, r.HeadSHA, "--")
	if err != nil {
		r.warn(CodeDiffFailed, "", "the diff from %s to %s cannot be read: %v", r.BaseSHA, r.HeadSHA, err)
		return
	}
	files, err := parseNameStatus(out)
	if err != nil {
		r.warn(CodeDiffFailed, "", "the diff from %s to %s cannot be read: %v", r.BaseSHA, r.HeadSHA, err)
		return
	}
	if len(files) > MaxChangedFiles {
		r.warn(CodeTooManyChangedFiles, "", "the change touches %d files; the first %d are listed", len(files), MaxChangedFiles)
		files = files[:MaxChangedFiles]
	}
	r.ChangedFiles = files
}

// parseNameStatus reads `git diff --name-status -z`: a status, then one path,
// or two for a rename or a copy.
func parseNameStatus(out []byte) ([]ChangedFile, error) {
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return []ChangedFile{}, nil
	}
	files := []ChangedFile{}
	for i := 0; i < len(fields); i++ {
		status := fields[i]
		if status == "" || i+1 >= len(fields) {
			return nil, fmt.Errorf("unexpected name-status record %q", status)
		}
		switch status[0] {
		case 'R', 'C':
			if i+2 >= len(fields) {
				return nil, fmt.Errorf("unexpected name-status record %q", status)
			}
			if status[0] == 'R' {
				files = append(files, ChangedFile{Path: fields[i+2], Status: StatusRenamed, PreviousPath: fields[i+1]})
			} else {
				files = append(files, ChangedFile{Path: fields[i+2], Status: StatusAdded})
			}
			i += 2
		case 'A':
			files = append(files, ChangedFile{Path: fields[i+1], Status: StatusAdded})
			i++
		case 'D':
			files = append(files, ChangedFile{Path: fields[i+1], Status: StatusDeleted})
			i++
		case 'M', 'T':
			files = append(files, ChangedFile{Path: fields[i+1], Status: StatusModified})
			i++
		default:
			return nil, fmt.Errorf("unexpected name-status %q", status)
		}
	}
	return files, nil
}

// lookup resolves the base locally: the commit when the base names one, else
// the branch, read on origin first for a branch the CI named and as given
// first for a flag.
func lookup(g git, base *ci.Base) (string, bool) {
	if base.SHA != "" {
		sha, err := g.commit(base.SHA)
		return sha, err == nil
	}
	candidates := []string{base.Ref}
	if branch, ok := branchOf(base.Ref); ok && !strings.HasPrefix(base.Ref, remote+"/") {
		tracking := remote + "/" + branch
		if base.Source == ci.BaseFromFlag {
			candidates = append(candidates, tracking)
		} else {
			candidates = []string{tracking, base.Ref}
		}
	}
	for _, candidate := range candidates {
		if sha, err := g.commit(candidate); err == nil {
			return sha, true
		}
	}
	return "", false
}

// branchOf returns the origin branch a ref names: origin/<branch> or a bare
// branch name. A revision expression is no branch.
func branchOf(ref string) (string, bool) {
	branch := strings.TrimPrefix(ref, remote+"/")
	if branch == "" || strings.HasPrefix(branch, "-") || strings.ContainsAny(branch, " ~^:?*[\\") ||
		strings.Contains(branch, "..") || strings.Contains(branch, "@{") || strings.HasSuffix(branch, ".lock") ||
		strings.HasSuffix(branch, "/") || strings.HasPrefix(branch, "/") || branch == "HEAD" {
		return "", false
	}
	return branch, true
}

func describe(base *ci.Base) string {
	switch {
	case base.SHA != "" && base.Ref != "":
		return base.SHA + " (" + base.Ref + ")"
	case base.SHA != "":
		return base.SHA
	}
	return base.Ref
}

func (r *Result) warn(code, field, format string, args ...any) {
	r.Diagnostics = append(r.Diagnostics, diag.Warningf(code, field, format, args...))
}

// git runs git in one directory, with an environment that never prompts and
// that drops repository-locating variables the job inherited.
type git struct {
	ctx context.Context
	dir string
}

func (g git) commit(revision string) (string, error) {
	if revision == "" || strings.HasPrefix(revision, "-") {
		return "", fmt.Errorf("%q is not a revision", revision)
	}
	return g.output("rev-parse", "--verify", "--quiet", "--end-of-options", revision+"^{commit}")
}

func (g git) shallow() bool {
	out, err := g.output("rev-parse", "--is-shallow-repository")
	return err == nil && out == "true"
}

// fetch fetches refspec from origin, depth commits deep when depth > 0.
func (g git) fetch(depth int, refspec string) error {
	args := []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules"}
	if depth > 0 {
		args = append(args, fmt.Sprintf("--depth=%d", depth))
	}
	_, err := g.raw(append(args, "--end-of-options", remote, refspec)...)
	return err
}

// deepen deepens the shallow history behind the base and the head by step
// commits. A remote that refuses a fetch by commit id is deepened through
// its configured refspecs.
func (g git) deepen(step int, base, head string) error {
	args := []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", fmt.Sprintf("--deepen=%d", step), "--end-of-options", remote}
	if _, err := g.raw(append(args, head, base)...); err == nil {
		return nil
	}
	_, err := g.raw(args...)
	return err
}

// RemoteURL returns the URL of the origin remote of the repository at dir.
func RemoteURL(ctx context.Context, dir string) (string, error) {
	return git{ctx: ctx, dir: dir}.output("remote", "get-url", remote)
}

func (g git) output(args ...string) (string, error) {
	out, err := g.raw(args...)
	return strings.TrimSpace(string(out)), err
}

func (g git) raw(args ...string) ([]byte, error) {
	cmd := exec.CommandContext(g.ctx, "git", args...)
	cmd.Dir = g.dir
	cmd.Env = gitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("git %s: %s", args[0], firstLine(msg))
		}
		return nil, fmt.Errorf("git %s: %w", args[0], err)
	}
	return stdout.Bytes(), nil
}

// locating are the variables that point git at another repository or index.
var locating = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_COMMON_DIR": true, "GIT_NAMESPACE": true,
	"GIT_CEILING_DIRECTORIES": true, "GIT_DISCOVERY_ACROSS_FILESYSTEM": true,
}

func gitEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !locating[name] {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
