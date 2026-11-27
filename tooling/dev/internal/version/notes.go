package version

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/linetag"
)

// notesUsage is the usage line of `version notes`.
const notesUsage = "usage: putnami version notes <line>@<version>... [--dry-run] [--yes]"

// lineNotes is the release of one line that `version notes` writes.
type lineNotes struct {
	line    wire.VersionLine
	version string
	notes   string
	// last is the line's last release that HEAD reaches. Its Commit is empty
	// when the line never released.
	last linetag.Release
}

// String names the line and its version.
func (n lineNotes) String() string {
	return printableLine(n.line.Line) + " " + n.version
}

// Notes runs `version notes <line>@<version>... [--dry-run] [--yes]` and
// returns the exit code. It prints text only: under json and jsonl it exits 2
// before it reads anything.
func (c Command) Notes(ctx context.Context, args []string) int {
	if c.Output.IsStructured() {
		return c.report(protocolcli.Usagef("--output=%s is not supported by %q", c.Output, "version notes"))
	}
	if err := c.notes(ctx, args); err != nil {
		return c.report(err)
	}
	return protocolcli.ExitSuccess
}

// notes writes the changelog of every requested line in one commit on the
// current branch. It creates no tag and pushes nothing.
func (c Command) notes(ctx context.Context, args []string) error {
	parsed, rest, err := parseFlags(args)
	if err != nil {
		return err
	}
	if parsed.Scope != "" || parsed.Push {
		return protocolcli.Usagef("%s", notesUsage)
	}
	requests, err := parseNotesRequests(rest)
	if err != nil {
		return err
	}
	result, err := c.releasableLines(ctx, "version notes", "write the notes of HEAD", "")
	if err != nil {
		return err
	}
	releases := make([]lineNotes, 0, len(requests))
	for _, request := range requests {
		release, err := c.prepareNotes(ctx, result.Lines, request)
		if err != nil {
			return err
		}
		releases = append(releases, release)
	}
	others, err := c.otherReleaseFiles(ctx, result.Lines, releases)
	if err != nil {
		return err
	}

	if parsed.DryRun || c.DryRun {
		for _, release := range releases {
			fmt.Fprintf(c.Stdout, "  Would write %s into %s\n", release, changelogRelPath(release.line.Line))
			fmt.Fprintln(c.Stdout, release.notes)
		}
		c.printOtherReleaseFiles(others)
		return nil
	}
	if err := c.requireCleanGitTree(ctx, "writing the release notes"); err != nil {
		return err
	}
	branch, err := c.requireChangeBranch(ctx)
	if err != nil {
		return err
	}
	if !parsed.Yes {
		fmt.Fprintf(c.Stdout, "  Write these release notes in one commit on %s? Re-run with --yes to confirm.\n", branch)
		for _, release := range releases {
			fmt.Fprintf(c.Stdout, "    %s\n", release)
		}
		return nil
	}
	if err := c.commitNotes(ctx, releases); err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "  Committed the release notes on %s\n", branch)
	c.printOtherReleaseFiles(others)
	return nil
}

// notesRequest is one <line>@<version> argument.
type notesRequest struct {
	scope, version string
}

// parseNotesRequests reads the <line>@<version> arguments. A line loses its
// leading and trailing slashes, so / is the root line.
func parseNotesRequests(args []string) ([]notesRequest, error) {
	if len(args) == 0 {
		return nil, protocolcli.Usagef("%s", notesUsage)
	}
	requests := make([]notesRequest, 0, len(args))
	for _, arg := range args {
		at := strings.LastIndex(arg, "@")
		if strings.HasPrefix(arg, "-") || at <= 0 || at == len(arg)-1 {
			return nil, protocolcli.Usagef("%q is not <line>@<version>; %s", arg, notesUsage)
		}
		request := notesRequest{scope: strings.Trim(arg[:at], "/"), version: arg[at+1:]}
		if slices.ContainsFunc(requests, func(r notesRequest) bool { return r.scope == request.scope }) {
			return nil, protocolcli.Usagef("line %s is named twice", printableLine(request.scope))
		}
		requests = append(requests, request)
	}
	return requests, nil
}

// prepareNotes renders the notes of one request, or refuses it: its line is
// unknown, HEAD is already one of the line's releases, or the version is not
// one the line can take next.
func (c Command) prepareNotes(ctx context.Context, lines []wire.VersionLine, request notesRequest) (lineNotes, error) {
	index := slices.IndexFunc(lines, func(line wire.VersionLine) bool { return line.Line == request.scope })
	if index < 0 {
		return lineNotes{}, protocolcli.NotFoundf("no version line %s; this workspace declares %s",
			printableLine(request.scope), strings.Join(lineNames(lines), ", "))
	}
	release := lineNotes{line: lines[index], version: request.version}
	name, changelog := printableLine(release.line.Line), changelogRelPath(release.line.Line)
	if release.line.Tagged {
		return lineNotes{}, protocolcli.InvalidConfigf("HEAD is already tagged %s", release.line.Tag)
	}
	tagName := strings.Replace(release.line.TagPattern, wsproto.LineTagPlaceholder, release.version, 1)
	if _, ok := linetag.Version(tagName, release.line.TagPattern); !ok {
		return lineNotes{}, protocolcli.InvalidConfigf("version %q of line %s is not MAJOR.MINOR.PATCH", release.version, name)
	}
	last, released, err := linetag.LastRelease(c.Root, release.line.TagPattern)
	if err != nil {
		return lineNotes{}, err
	}
	if released && linetag.CompareVersions(release.version, last.Version) <= 0 {
		return lineNotes{}, protocolcli.InvalidConfigf("version %s of line %s is not above its last release %s",
			release.version, name, last.Version)
	}
	release.last = last
	if exists, err := c.gitTagExists(ctx, tagName); err != nil {
		return lineNotes{}, err
	} else if exists {
		return lineNotes{}, protocolcli.InvalidConfigf("git tag already exists: %s", tagName)
	}
	existing, err := os.ReadFile(c.changelogPath(release.line.Line))
	if err != nil && !os.IsNotExist(err) {
		return lineNotes{}, fmt.Errorf("read %s: %w", changelog, err)
	}
	if changelogHolds(string(existing), release.version) {
		return lineNotes{}, protocolcli.InvalidConfigf("%s already holds a section for %s", changelog, release.version)
	}
	release.notes = RenderChangelog(name, release.version, release.line.Commits)
	return release, nil
}

// changelogHolds reports whether a changelog has a level-two heading that
// starts with version.
func changelogHolds(changelog, version string) bool {
	return regexp.MustCompile(`(?m)^## ` + regexp.QuoteMeta(version) + `(?:\s|$)`).MatchString(changelog)
}

// requireChangeBranch returns the current branch. It refuses a detached HEAD
// and the default branch: the notes are a change to review.
func (c Command) requireChangeBranch(ctx context.Context) (string, error) {
	output, err := c.gitOutput(ctx, "branch", "--show-current")
	if err != nil {
		return "", err
	}
	branch := strings.TrimSpace(output)
	if branch == "" {
		return "", protocolcli.InvalidConfigf("HEAD is detached; create a branch for the release notes with git switch -c <branch>")
	}
	if trunk := gitread.TrunkRef(c.Root); trunk == branch || gitread.RefNamesBranch(trunk, branch) {
		return "", protocolcli.InvalidConfigf(
			"%s is the default branch; create a branch for the release notes with git switch -c <branch>", branch)
	}
	return branch, nil
}

// commitNotes writes every changelog and commits them together.
func (c Command) commitNotes(ctx context.Context, releases []lineNotes) error {
	add := []string{"add", "--"}
	message := "chore(release): prepare the release notes\n"
	for _, release := range releases {
		scopePath := release.line.Line
		if err := PrependChangelog(c.changelogPath(scopePath), changelogTitle(scopePath), release.notes); err != nil {
			return err
		}
		add = append(add, changelogRelPath(scopePath))
		message += fmt.Sprintf("\n- %s", release)
	}
	if err := c.runGit(ctx, add...); err != nil {
		return err
	}
	if trailer := c.coAuthorTrailer(ctx); trailer != "" {
		message += "\n\n" + trailer
	}
	return c.runGit(ctx, "commit", "-m", message)
}

// otherReleaseFiles returns the files, sorted, that the last release commits of
// releases modified, that are still in the workspace, and that are no line's
// changelog. The commit a line's last release tag names is a release commit
// when it changed the line's changelog.
func (c Command) otherReleaseFiles(ctx context.Context, lines []wire.VersionLine, releases []lineNotes) ([]string, error) {
	var others []string
	for _, release := range releases {
		if release.last.Commit == "" {
			continue
		}
		output, err := c.gitOutput(ctx, "show", "--first-parent", "-m", "--format=", "--name-status",
			"--no-renames", "--relative", "-z", release.last.Commit)
		if err != nil {
			return nil, err
		}
		// A record is a status letter and a path, each ended by a NUL.
		fields := strings.Split(output, "\x00")
		var modified []string
		released := false
		for i := 0; i+1 < len(fields); i += 2 {
			released = released || fields[i+1] == changelogRelPath(release.line.Line)
			if strings.TrimSpace(fields[i]) == "M" {
				modified = append(modified, fields[i+1])
			}
		}
		if released {
			others = append(others, modified...)
		}
	}
	slices.Sort(others)
	return slices.DeleteFunc(slices.Compact(others), func(path string) bool {
		_, err := os.Lstat(filepath.Join(c.Root, filepath.FromSlash(path)))
		return err != nil || slices.ContainsFunc(lines, func(line wire.VersionLine) bool {
			return changelogRelPath(line.Line) == path
		})
	}), nil
}

// printOtherReleaseFiles names the files a release may still have to update.
func (c Command) printOtherReleaseFiles(paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Fprintln(c.Stdout, "  The last release also modified these files. Update the ones this release needs:")
	for _, path := range paths {
		fmt.Fprintf(c.Stdout, "    %s\n", path)
	}
}
