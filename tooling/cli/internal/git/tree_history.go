package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	protocoljob "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/procstat"
)

// CandidateFiles is the candidate cut of the work tree at root as a `git:`
// input enumerates it, each path with its index mode, read with one
// `git ls-files -t --stage --others --exclude-standard`: Git tags an untracked
// path "?" and lists it bare, and every other path with its stage entry. A
// tracked path carries its six-digit mode, an unmerged one "unmerged", an
// untracked one "untracked". Paths are relative to root, in slash form, sorted
// and unique; a path deleted from the work tree is listed. Outside every
// repository the error is an *gitread.UnmanagedError.
//
// The cut is read in-process (inprocess_*.go) when the repository's state is one
// the reader supports, and from Git otherwise (tooling/cli ADR 0112).
func CandidateFiles(root string) ([]protocoljob.CandidateFile, error) {
	if files, err := candidateFilesInProcess(root); !inProcessUnsupported(err) {
		return files, err
	}
	return CandidateFilesFromGit(root)
}

// CandidateFilesWitnessed is CandidateFiles with the paths whose stats witness
// the listing (tooling/cli ADR 0113 §3), when the reader read it in-process:
// the index, each directory the untracked walk read, each untracked nested
// repository it listed, each ignore file it read or could have read, and the
// files Git's configuration was read from. The witness is nil when Git listed
// the cut, and when the configuration is a reading the reader takes again each
// time: the stats would not say the listing is unchanged.
func CandidateFilesWitnessed(root string) ([]protocoljob.CandidateFile, []string, error) {
	repo, err := openLocal(root)
	if err == nil {
		var cut *localCut
		if cut, err = repo.readCut(); err == nil {
			files, sortErr := cutFiles(cut, root)
			return files, repo.cutWitness(cut), sortErr
		}
	}
	if !inProcessUnsupported(err) {
		return nil, nil, err
	}
	files, err := CandidateFilesFromGit(root)
	return files, nil, err
}

// cutWitness is the witness of a cut the reader read in repo (CandidateFilesWitnessed).
func (r *localRepo) cutWitness(cut *localCut) []string {
	config, kept := configCache.Load(r.root + "\x00" + os.Getenv("HOME") + "\x00" + os.Getenv("XDG_CONFIG_HOME"))
	cached, ok := config.(*cachedConfig)
	if !kept || !ok || cached.config != r.config {
		return nil
	}
	// The .git entry and the commondir file name the repository a linked
	// worktree reads; the index moves whenever Git writes it.
	witness := []string{
		filepath.Join(r.gitDir, "commondir"),
		filepath.Join(r.gitDir, "index"), filepath.Join(r.commonDir, "info", "exclude"),
	}
	// A .git file names the repository of a linked worktree; a .git
	// directory is witnessed by the root's listing, which the walk read,
	// and its own stat moves with every lock Git takes in it.
	if info, err := os.Lstat(filepath.Join(r.root, ".git")); err == nil && !info.IsDir() {
		witness = append(witness, filepath.Join(r.root, ".git"))
	}
	if excludes, err := r.config.excludesFile(); err == nil && excludes != "" {
		witness = append(witness, excludes)
	}
	witness = append(witness, cached.files...)
	for _, dir := range cut.Directories {
		witness = append(witness, filepath.Join(r.root, filepath.FromSlash(dir)))
	}
	for _, untracked := range cut.Untracked {
		if untracked.Repository {
			witness = append(witness, filepath.Join(r.root, filepath.FromSlash(untracked.Path)))
		}
	}
	return append(witness, cut.RuleFiles...)
}

// candidateFilesInProcess reads the cut of the work tree at root in-process;
// its error is unsupported (inProcessUnsupported) when Git must read it.
func candidateFilesInProcess(root string) ([]protocoljob.CandidateFile, error) {
	repo, err := openLocal(root)
	if err != nil {
		return nil, err
	}
	cut, err := repo.readCut()
	if err != nil {
		return nil, err
	}
	return cutFiles(cut, root)
}

// cutFiles is the listing of a cut the reader read, as CandidateFiles lists it.
func cutFiles(cut *localCut, root string) ([]protocoljob.CandidateFile, error) {
	files := make([]protocoljob.CandidateFile, 0, len(cut.Index.Entries)+len(cut.Untracked))
	for _, entry := range cut.Index.Entries {
		mode := fmt.Sprintf("%06o", entry.Mode)
		if entry.Stage != 0 {
			mode = protocoljob.CandidateModeUnmerged
		}
		files = append(files, protocoljob.CandidateFile{Path: entry.Path, Mode: mode})
	}
	for _, untracked := range cut.Untracked {
		files = append(files, protocoljob.CandidateFile{Path: untracked.Path, Mode: protocoljob.CandidateModeUntracked})
	}
	return sortedCut(files, root)
}

// CandidateFilesFromGit is CandidateFiles read from Git alone.
func CandidateFilesFromGit(root string) ([]protocoljob.CandidateFile, error) {
	output, stderr, err := gitread.RunCapture(root, "ls-files", "-z", "-t", "--stage", "--others", "--exclude-standard")
	if err != nil {
		if unmanaged := gitread.Unmanaged(root); unmanaged != nil {
			return nil, unmanaged
		}
		return nil, fmt.Errorf("git ls-files (in %s): %w: %s", root, err, strings.TrimSpace(stderr))
	}
	files := []protocoljob.CandidateFile{}
	for _, entry := range gitread.SplitNUL(output) {
		tag, rest, ok := strings.Cut(entry, " ")
		if !ok || len(tag) != 1 {
			return nil, fmt.Errorf("git ls-files listed %q, which carries no status tag", entry)
		}
		listed, mode := rest, protocoljob.CandidateModeUntracked
		if tag != "?" {
			match := stageEntry.FindStringSubmatch(rest)
			if match == nil {
				return nil, fmt.Errorf("git ls-files listed %q, which is no stage entry", entry)
			}
			listed, mode = match[3], match[1]
			if match[2] != "0" {
				mode = protocoljob.CandidateModeUnmerged
			}
		}
		// Git lists an untracked nested repository as its directory with a
		// trailing slash; the cut names the directory.
		files = append(files, protocoljob.CandidateFile{Path: strings.TrimSuffix(listed, "/"), Mode: mode})
	}
	return sortedCut(files, root)
}

// sortedCut checks that every path of files is a clean path inside root,
// sorts them by path and keeps the first entry of each path.
func sortedCut(files []protocoljob.CandidateFile, root string) ([]protocoljob.CandidateFile, error) {
	for _, file := range files {
		path := file.Path
		if path == "" || path == "." || pathpkg.Clean(path) != path || strings.HasPrefix(path, "../") {
			return nil, fmt.Errorf("git ls-files listed %q, which is not a clean path inside %s", path, root)
		}
	}
	slices.SortStableFunc(files, func(a, b protocoljob.CandidateFile) int { return strings.Compare(a.Path, b.Path) })
	// An unmerged path is listed once per stage, and the first entry of a path
	// carries the answer: its stages agree on "unmerged".
	files = slices.CompactFunc(files, func(a, b protocoljob.CandidateFile) bool { return a.Path == b.Path })
	return files, nil
}

// stageEntry is one tracked entry of `git ls-files --stage` after its status
// tag: "<mode> <object> <stage>\t<path>".
var stageEntry = regexp.MustCompile(`(?s)^([0-7]{6}) [0-9a-f]{40,64} ([0-3])\t(.+)$`)

// IgnoresDirectories returns which of rels, slash paths relative to the work
// tree at root, Git's ignore rules exclude as a whole, asked in one git call:
// Git then lists nothing below such a directory, whatever a task writes there.
// A missing directory may answer not excluded. A failure is an error: Git
// refuses the whole question when one path lies beyond a link or in a nested
// repository.
func IgnoresDirectories(root string, rels []string) (map[string]bool, error) {
	ignored := map[string]bool{}
	if len(rels) == 0 {
		return ignored, nil
	}
	var input bytes.Buffer
	for _, rel := range rels {
		input.WriteString(rel)
		input.WriteByte(0)
	}
	cmd := exec.Command(gitread.Executable(), "check-ignore", "-z", "--stdin")
	cmd.Dir = root
	cmd.Env = gitread.RepositoryEnvironment(os.Environ())
	cmd.Stdin = &input
	data, err := procstat.Output(cmd, procstat.Git)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		// Exit status 1: none of them is ignored.
		return ignored, nil
	}
	if err != nil {
		return nil, fmt.Errorf("git check-ignore: %w", err)
	}
	for _, rel := range bytes.Split(data, []byte{0}) {
		if len(rel) > 0 {
			ignored[string(rel)] = true
		}
	}
	return ignored, nil
}

// workspaceConfigMaxBytes bounds the workspace config a change history reads
// at the merge base.
const workspaceConfigMaxBytes = 8 << 20

// ReadChangeHistory reads the history of the change since baseline in the
// repository that holds root, the workspace root: whether the clone is
// shallow, and otherwise the merge base of baseline and HEAD, the commits of
// baseline..HEAD newest first, and the workspace config, file name config, as
// the merge base holds it at root. A baseline that names no commit is read as
// origin/<baseline>, as the impacted selection reads it. A config at the merge
// base that is no regular file, or that is larger than 8 MiB, is an error: the
// job that reads it then reads Git and reports it.
func ReadChangeHistory(root, baseline, config string) (*protocoljob.ChangeHistory, error) {
	history := &protocoljob.ChangeHistory{Baseline: baseline}
	shallow, err := gitread.IsShallow(root)
	if err != nil {
		return nil, err
	}
	if shallow {
		history.Shallow = true
		return history, nil
	}
	resolved, err := gitread.ResolveCommit(root, baseline)
	if err != nil && !strings.HasPrefix(baseline, "origin/") {
		if remote, remoteErr := gitread.ResolveCommit(root, "origin/"+baseline); remoteErr == nil {
			resolved, err = remote, nil
		}
	}
	if err != nil {
		return nil, err
	}
	output, stderr, err := gitread.RunCapture(root, "merge-base", resolved, "HEAD")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.TrimSpace(stderr) == "" {
			return nil, fmt.Errorf("find the merge base of %s and HEAD: they share no commit", baseline)
		}
		return nil, fmt.Errorf("find the merge base of %s and HEAD: %w: %s", baseline, err, strings.TrimSpace(stderr))
	}
	history.MergeBase = strings.ToLower(strings.TrimSpace(output))
	if !gitread.ValidObjectID(history.MergeBase) {
		return nil, fmt.Errorf("find the merge base of %s and HEAD: git returned an invalid object ID", baseline)
	}
	commits, err := gitread.CommitsSince(root, resolved, nil)
	if err != nil {
		return nil, err
	}
	history.Commits = make([]protocoljob.HistoryCommit, 0, len(commits))
	for _, commit := range commits {
		history.Commits = append(history.Commits, protocoljob.HistoryCommit{SHA: commit.SHA, Subject: commit.Subject, Body: commit.Body})
	}
	content, found, err := fileAtCommit(root, history.MergeBase, config)
	if err != nil {
		return nil, err
	}
	if found {
		history.WorkspaceConfig = &content
	}
	return history, nil
}

// fileAtCommit reads name, a path relative to root, as commit holds it: false
// when commit holds no entry there, an error when the entry is no regular file
// or larger than workspaceConfigMaxBytes.
func fileAtCommit(root, commit, name string) (string, bool, error) {
	prefix, err := gitread.Run(root, "rev-parse", "--show-prefix")
	if err != nil {
		return "", false, fmt.Errorf("locate %s in its repository: %w", root, err)
	}
	repoPath := strings.TrimSpace(prefix) + name
	output, err := gitread.Run(root, "ls-tree", "-z", "-l", "--full-tree", commit, "--", ":(literal)"+repoPath)
	if err != nil {
		return "", false, fmt.Errorf("inspect %s at %s: %w", repoPath, commit, err)
	}
	entries := gitread.SplitNUL(output)
	if len(entries) == 0 {
		return "", false, nil
	}
	// "<mode> SP <type> SP <object> SP+ <size> TAB <path>"
	metadata, path, ok := strings.Cut(entries[0], "\t")
	fields := strings.Fields(metadata)
	if len(entries) != 1 || !ok || path != repoPath || len(fields) != 4 {
		return "", false, fmt.Errorf("inspect %s at %s: unexpected ls-tree output", repoPath, commit)
	}
	if fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return "", false, fmt.Errorf("%s at %s is not a regular file", repoPath, commit)
	}
	size, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || size > workspaceConfigMaxBytes {
		return "", false, fmt.Errorf("%s at %s is larger than %d bytes", repoPath, commit, workspaceConfigMaxBytes)
	}
	content, err := gitread.Run(root, "cat-file", "blob", fields[2])
	if err != nil {
		return "", false, fmt.Errorf("read %s at %s: %w", repoPath, commit, err)
	}
	return content, true, nil
}
