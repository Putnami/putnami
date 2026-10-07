// Package history reads the last 90 days of a repository's history in two
// passes: the non-merge commits with the lines each changed per file, and
// the first-parent changes with their total size. It never checks out a
// commit.
package history

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
)

// Window is the period every 90-day marker reads.
const Window = 90 * 24 * time.Hour

// FileChange is one file a commit changed.
type FileChange struct {
	Path    string
	Added   int
	Deleted int
}

// Commit is one non-merge commit of the window.
type Commit struct {
	Hash string
	// Time is the committer time in Unix seconds.
	Time int64
	// Author is the lowercase author email. It stays in memory: the payload
	// only carries a salted pseudonym of it.
	Author string
	// Subject is the commit's subject line. It stays in memory.
	Subject string
	// Agents are the agent tools the commit credits, in a Co-Authored-By
	// trailer or as its author, by the name Agent returns. It stays in
	// memory: the payload only carries counts.
	Agents []string
	// CoAuthors are the lowercase emails of the people its Co-Authored-By
	// trailers credit. They stay in memory.
	CoAuthors []string
	// Reverted is true when a later commit of the window reverts this one.
	Reverted bool
	// Files are the changed files the caller kept, in git's order.
	Files []FileChange
}

// Change is one commit on the first-parent line of HEAD: what landed on the
// branch, a merged pull request or a direct commit.
type Change struct {
	Hash    string
	Time    int64
	Subject string
	// Title is what the change says it does: the subject, or, for a GitHub
	// merge commit, the pull request title on the first line of its body.
	Title string
	// Lines is insertions plus deletions against the first parent.
	Lines int
	// Files are the changed paths the caller kept, against the first parent.
	Files []string
	// ViaPullRequest is true when the subject or the body is one a forge
	// writes when it merges a pull request or a merge request.
	ViaPullRequest bool
	// RevertedAt is the committer time, in Unix seconds, of the first revert
	// of the window that undoes the change; zero when none does.
	RevertedAt int64
}

// History is the window's commits, newest first.
type History struct {
	Since   time.Time
	Commits []Commit
	Changes []Change
	// Reverts are the window's revert commits, merges included.
	Reverts []Revert
}

// Read reads the window that ends at now. keep decides which changed paths
// the commits retain; a commit whose paths are all dropped still counts.
func Read(ctx context.Context, repo *gitrepo.Repo, now time.Time, keep func(path string) bool) (History, error) {
	since := now.Add(-Window)
	result := History{Since: since}
	sinceArg := fmt.Sprintf("--since=@%d", since.Unix())
	// The first-parent pass diffs the same trees again, so it runs beside the
	// commit pass rather than after it.
	var changes []Change
	changesErr := make(chan error, 1)
	go func() {
		var err error
		changes, err = readChanges(ctx, repo, sinceArg, keep)
		changesErr <- err
	}()
	var reverts []Revert
	revertsErr := make(chan error, 1)
	go func() {
		var err error
		reverts, err = readReverts(ctx, repo, sinceArg)
		revertsErr <- err
	}()
	var current *Commit
	flush := func() {
		if current != nil {
			result.Commits = append(result.Commits, *current)
			current = nil
		}
	}
	err := repo.Stream(ctx, func(line string) error {
		if rest, ok := strings.CutPrefix(line, "\x1e"); ok {
			flush()
			fields := strings.SplitN(rest, "\t", 5)
			if len(fields) != 5 {
				return fmt.Errorf("unexpected log header %q", rest)
			}
			at, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return err
			}
			author := strings.ToLower(strings.TrimSpace(fields[2]))
			trailers := strings.Split(fields[3], "\x1f")
			current = &Commit{Hash: fields[0], Time: at, Author: author, Subject: fields[4], Agents: agents(author, trailers), CoAuthors: coAuthors(trailers)}
			return nil
		}
		if current == nil || line == "" {
			return nil
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return nil
		}
		path := gitrepo.Unquote(fields[2])
		if keep != nil && !keep(path) {
			return nil
		}
		added, _ := strconv.Atoi(fields[0])
		deleted, _ := strconv.Atoi(fields[1])
		current.Files = append(current.Files, FileChange{Path: path, Added: added, Deleted: deleted})
		return nil
	}, "log", "--no-merges", "--no-renames", "--numstat", sinceArg, commitFormat, "HEAD")
	if waitErr := <-changesErr; waitErr != nil {
		return History{}, waitErr
	}
	if waitErr := <-revertsErr; waitErr != nil {
		return History{}, waitErr
	}
	if err != nil {
		return History{}, fmt.Errorf("read commits: %w", err)
	}
	flush()
	MarkReverted(result.Commits, reverts, mergedBranch(ctx, repo))
	MarkChangesReverted(changes, reverts, Landing(ctx, repo, changes))
	result.Changes = changes
	result.Reverts = reverts
	return result, nil
}

// commitFormat heads each commit of the numstat pass with its id, committer
// time, author email, Co-Authored-By trailer values and subject. The trailer
// key matches in any case, as git reads it.
const commitFormat = "--format=\x1e%H\t%ct\t%ae\t%(trailers:key=Co-authored-by,valueonly,unfold,separator=%x1f)\t%s"

// agents returns the agent tools a commit credits, as its author or in its
// Co-Authored-By trailers, each once, in the order found.
func agents(author string, trailers []string) []string {
	var found []string
	for _, identity := range append([]string{author}, trailers...) {
		if agent := Agent(identity); agent != "" && !slices.Contains(found, agent) {
			found = append(found, agent)
		}
	}
	return found
}

// coAuthors returns the people a commit's Co-Authored-By trailers credit,
// each once.
func coAuthors(trailers []string) []string {
	var found []string
	for _, identity := range trailers {
		if email := coAuthor(identity); email != "" && !slices.Contains(found, email) {
			found = append(found, email)
		}
	}
	return found
}

// readReverts reads the window's revert commits. A revert can land as a
// merge, so merges are read too.
func readReverts(ctx context.Context, repo *gitrepo.Repo, sinceArg string) ([]Revert, error) {
	var reverts []Revert
	err := repo.Stream(ctx, func(line string) error {
		if rest, ok := strings.CutPrefix(line, "\x1e"); ok {
			at, err := strconv.ParseInt(rest, 10, 64)
			if err != nil {
				return err
			}
			reverts = append(reverts, Revert{Time: at})
			return nil
		}
		if len(reverts) > 0 {
			reverts[len(reverts)-1].ReadRevert(line)
		}
		return nil
	}, "log", sinceArg, "-i", "--grep=revert", "--format=\x1e%ct%n%s%n%b", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("read reverts: %w", err)
	}
	return reverts, nil
}

// maxRevertedMerges bounds the git calls that list what a reverted merge
// brought in.
const maxRevertedMerges = 50

// mergedBranch lists the non-merge commits a merge brought in, for a revert
// that names a merge. It answers nothing for an id git does not know, and
// only the id itself for a commit that is not a merge.
func mergedBranch(ctx context.Context, repo *gitrepo.Repo) func(hash string) []string {
	calls := 0
	return func(hash string) []string {
		if calls >= maxRevertedMerges {
			return nil
		}
		calls++
		out, err := repo.Output(ctx, "rev-list", "--no-merges", hash+"^1.."+hash, "--")
		if err != nil {
			return nil
		}
		return strings.Fields(string(out))
	}
}

// ChangesCommand reproduces the first-parent pass from a shell.
const ChangesCommand = "git log --since=90.days --first-parent --diff-merges=first-parent --shortstat --format=%s"

// changeFormat ends each commit's body with a group separator, so that the
// parser knows where the body stops and the numstat lines start.
const changeFormat = "--format=\x1e%H\t%ct\t%s%n%b\x1d"

// githubMerge is the subject GitHub writes on a merge commit. The pull
// request's title is the first line of the body.
var githubMerge = regexp.MustCompile(`^Merge pull request #\d+ from `)

func readChanges(ctx context.Context, repo *gitrepo.Repo, sinceArg string, keep func(path string) bool) ([]Change, error) {
	var changes []Change
	inBody := false
	err := repo.Stream(ctx, func(line string) error {
		if rest, ok := strings.CutPrefix(line, "\x1e"); ok {
			fields := strings.SplitN(rest, "\t", 3)
			if len(fields) != 3 {
				return fmt.Errorf("unexpected log header %q", rest)
			}
			at, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return err
			}
			changes = append(changes, Change{
				Hash: fields[0], Time: at, Subject: fields[2], Title: fields[2],
				ViaPullRequest: IsPullRequestSubject(fields[2]),
			})
			inBody = true
			return nil
		}
		if len(changes) == 0 {
			return nil
		}
		last := &changes[len(changes)-1]
		if inBody {
			body, end := strings.CutSuffix(line, "\x1d")
			inBody = !end
			last.ViaPullRequest = last.ViaPullRequest || IsPullRequestBody(body)
			if title := strings.TrimSpace(body); title != "" && last.Title == last.Subject && githubMerge.MatchString(last.Subject) {
				last.Title = title
			}
			return nil
		}
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) != 3 {
			return nil
		}
		// A binary file reads "-" for both counts, and adds no line.
		added, _ := strconv.Atoi(fields[0])
		deleted, _ := strconv.Atoi(fields[1])
		last.Lines += added + deleted
		if path := gitrepo.Unquote(fields[2]); keep == nil || keep(path) {
			last.Files = append(last.Files, path)
		}
		return nil
	}, "log", "--first-parent", "--diff-merges=first-parent", "--no-renames", "--numstat", sinceArg, changeFormat, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("read changes: %w", err)
	}
	return changes, nil
}

var pullRequestSubjects = []*regexp.Regexp{
	regexp.MustCompile(`^Merge pull request #\d+`),           // GitHub merge commit
	regexp.MustCompile(`\(#\d+\)$`),                          // GitHub squash or rebase
	regexp.MustCompile(`^Merge branch '[^']+' into '[^']+'`), // GitLab merge request
	regexp.MustCompile(`\(pull request #\d+\)`),              // Bitbucket
	regexp.MustCompile(`^Merged PR \d+`),                     // Azure DevOps
}

var pullRequestBodies = []*regexp.Regexp{
	regexp.MustCompile(`^See merge request \S+!\d+`),         // GitLab merge commit
	regexp.MustCompile(`(^|\s)[\w.-]+(/[\w.-]+)+!\d+(\s|$)`), // GitLab reference in a custom merge template
	regexp.MustCompile(`^Reviewed-on: \S+`),                  // Gerrit
}

// IsPullRequestBody reports whether a body line is one a forge writes when it
// merges a merge request, such as GitLab's "See merge request group/repo!12".
func IsPullRequestBody(line string) bool {
	line = strings.TrimSpace(line)
	for _, pattern := range pullRequestBodies {
		if pattern.MatchString(line) {
			return true
		}
	}
	return false
}

// IsPullRequestSubject reports whether a forge wrote this subject when it
// merged a pull request.
func IsPullRequestSubject(subject string) bool {
	subject = strings.TrimSpace(subject)
	for _, pattern := range pullRequestSubjects {
		if pattern.MatchString(subject) {
			return true
		}
	}
	return false
}

// maxPathspecs bounds how many files one git command names.
const maxPathspecs = 1000

// Context is how many unchanged lines AddedMatches reads around each added
// line, so that a classifier sees the condition that guards it.
const Context = 3

// AddedMatches counts, per file, the lines the window added that match the
// extended regular expression pattern and that keep accepts. keep receives
// the file's path, the new side of the hunk, added and unchanged lines in
// order, and the index of the matching line; a nil keep accepts every match. It selects only the
// commits that changed the given files, which keeps the pass short on a
// large history: the caller names the files that match at HEAD. Those commits
// are diffed whole with rename detection, so a file that moved keeps its old
// lines.
func AddedMatches(ctx context.Context, repo *gitrepo.Repo, now time.Time, pattern string, files []string, keep func(file string, lines []string, i int) bool) (map[string]int, error) {
	match, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	added := map[string]int{}
	wanted := make(map[string]bool, len(files))
	for _, file := range files {
		wanted[file] = true
	}
	sinceArg := fmt.Sprintf("--since=@%d", now.Add(-Window).Unix())
	for start := 0; start < len(files); start += maxPathspecs {
		args := []string{
			"log", "--no-merges", "-M", "--full-diff", "--no-ext-diff", "--no-textconv", "-p", fmt.Sprintf("-U%d", Context), "--format=",
			"--src-prefix=a/", "--dst-prefix=b/", sinceArg, "-G" + pattern, "HEAD", "--",
		}
		for _, file := range files[start:min(start+maxPathspecs, len(files))] {
			args = append(args, ":(literal)"+file)
		}
		current, inHunk := "", false
		// hunk holds the new side of the current hunk; matches indexes its
		// added lines that match the pattern.
		var hunk []string
		var matches []int
		flush := func() {
			for _, i := range matches {
				if keep == nil || keep(current, hunk, i) {
					added[current]++
				}
			}
			hunk, matches = hunk[:0], matches[:0]
		}
		err := repo.Stream(ctx, func(line string) error {
			switch {
			case strings.HasPrefix(line, "diff --git "):
				flush()
				current, inHunk = "", false
			case !inHunk && strings.HasPrefix(line, "+++ "):
				// git ends the header with a tab when the path holds a space.
				current = strings.TrimPrefix(gitrepo.Unquote(strings.TrimSuffix(strings.TrimPrefix(line, "+++ "), "\t")), "b/")
			case strings.HasPrefix(line, "@@"):
				flush()
				inHunk = true
			case !inHunk || !wanted[current]:
			case strings.HasPrefix(line, "+"):
				if match.MatchString(line[1:]) {
					matches = append(matches, len(hunk))
				}
				hunk = append(hunk, line[1:])
			case strings.HasPrefix(line, " "):
				hunk = append(hunk, line[1:])
			}
			return nil
		}, args...)
		flush()
		if err != nil {
			return nil, fmt.Errorf("read added matches: %w", err)
		}
	}
	return added, nil
}
