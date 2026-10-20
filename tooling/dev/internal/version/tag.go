package version

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	wire "go.putnami.dev/protocol/extension/hostrpc"
	"go.putnami.dev/sdk/extension/linetag"
)

// gitPushTimeout bounds a `git push` so a hung network operation (stalled TLS
// handshake, dead remote) eventually aborts instead of wedging the process. It
// is generous enough not to interrupt a legitimately large push.
const gitPushTimeout = 5 * time.Minute

// tagUsage is the usage line of `version tag`.
const tagUsage = "usage: putnami version tag [<tag>] [--scope <line>] [--push] [--dry-run] [--yes]"

// Tag runs `version tag [<tag>] [--scope <line>] [--push] [--dry-run]
// [--yes]` and returns the exit code. It prints text only: under json and
// jsonl it exits 2 before it reads anything.
func (c Command) Tag(ctx context.Context, args []string) int {
	if c.Output.IsStructured() {
		return c.report(protocolcli.Usagef("--output=%s is not supported by %q", c.Output, "version tag"))
	}
	if err := c.tag(ctx, args); err != nil {
		return c.report(err)
	}
	return protocolcli.ExitSuccess
}

// tag creates one line's release: the regenerated changelog, the release
// commit that carries it, then the annotated tag on that commit.
//
// The order is the whole point. The changelog is generated from the same
// commits the version is computed from, so it has to be committed BEFORE the
// tag exists — a tag created first would point at a tree without its own
// release notes, and the notes would then describe a commit they are not in.
func (c Command) tag(ctx context.Context, args []string) error {
	parsed, rest, err := parseFlags(args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return protocolcli.Usagef("%s", tagUsage)
	}
	result, err := c.versionLines(ctx, parsed.Scope)
	if err != nil {
		return err
	}
	// A release never reads its version from a recorded view: the view may
	// be older than the commits it releases.
	if recorded := result.RecordedGraph; recorded != nil {
		return fmt.Errorf("version: %s reads the dependency graph, which could not be refreshed: %s",
			supportCatalog, recorded.RefreshError)
	}
	if err := c.requireSourceRevisionAtHead(ctx, result.SourceRevision); err != nil {
		return err
	}
	line, err := resolveTagLine(result.Lines, parsed.Scope)
	if err != nil {
		return err
	}
	if line.Tagged {
		return protocolcli.InvalidConfigf("HEAD is already tagged %s", line.Tag)
	}
	version, tagName, err := proposedTag(line, firstOrEmpty(rest))
	if err != nil {
		return err
	}
	notes := RenderChangelog(printableLine(line.Line), version, line.Commits)

	if parsed.DryRun || c.DryRun {
		fmt.Fprintf(c.Stdout, "  Would release %s %s as %s\n", printableLine(line.Line), version, tagName)
		fmt.Fprintln(c.Stdout, notes)
		return nil
	}
	if err := c.requireCleanGitTree(ctx); err != nil {
		return err
	}
	if exists, err := c.gitTagExists(ctx, tagName); err != nil {
		return err
	} else if exists {
		return protocolcli.InvalidConfigf("git tag already exists: %s", tagName)
	}
	if !parsed.Yes {
		fmt.Fprintf(c.Stdout, "  Release %s %s as %s? Re-run with --yes to confirm.\n",
			printableLine(line.Line), version, tagName)
		return nil
	}
	return c.release(ctx, line.Line, version, tagName, notes, parsed.Push)
}

// release writes the changelog, commits it, tags the commit and, with push,
// pushes HEAD and the tag to origin.
func (c Command) release(ctx context.Context, scopePath, version, tagName, notes string, push bool) error {
	changelogPath := filepath.Join(c.Root, filepath.FromSlash(changelogRelPath(scopePath)))
	if err := PrependChangelog(changelogPath, notes); err != nil {
		return err
	}
	if err := c.runGit(ctx, "add", "--", changelogRelPath(scopePath)); err != nil {
		return err
	}
	message := fmt.Sprintf("chore(release): %s %s", printableLine(scopePath), version)
	if trailer := c.coAuthorTrailer(ctx); trailer != "" {
		message += "\n\n" + trailer
	}
	if err := c.runGit(ctx, "commit", "-m", message); err != nil {
		return err
	}
	// --cleanup=verbatim: the notes are markdown, and git's default cleanup
	// strips every line starting with "#" as a comment — which is every heading
	// of the changelog, leaving a tag message of bare bullets.
	if err := c.runGit(ctx, "tag", "--cleanup=verbatim", "-a", tagName, "-m", notes); err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "  Created %s on the release commit\n", tagName)
	if !push {
		return nil
	}
	pushCtx, cancel := context.WithTimeout(ctx, gitPushTimeout)
	defer cancel()
	if err := c.runGit(pushCtx, "push", "origin", "HEAD", tagName); err != nil {
		return err
	}
	fmt.Fprintf(c.Stdout, "  Pushed HEAD and %s\n", tagName)
	return nil
}

// requireSourceRevisionAtHead refuses a release while the host is bound to a
// commit other than HEAD. A release computes its version from HEAD's history
// and puts the release commit and its tag on top of HEAD, so a tag always
// names the checked-out commit. The bound commit may not even be in the
// repository, and releasing under it would record one commit's release
// against another's identity. A binding that names HEAD itself changes
// nothing, so it is accepted.
func (c Command) requireSourceRevisionAtHead(ctx context.Context, bound string) error {
	head, err := c.gitOutput(ctx, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("version tag: read HEAD: %w", err)
	}
	head = strings.TrimSpace(head)
	if bound != strings.ToLower(head) {
		return protocolcli.InvalidConfigf(
			"version tag releases the checked-out commit %s, but %s binds this run to %s; unset it to tag HEAD",
			head, sourceRevisionEnv, bound)
	}
	return nil
}

// resolveTagLine picks the line a release names. --scope is required as soon as
// the workspace has more than one line: tagging the wrong line publishes the
// wrong cohort, and a default would pick it silently. With a scope, the host
// answered that one line.
func resolveTagLine(lines []wire.VersionLine, scope string) (wire.VersionLine, error) {
	if scope == "" && len(lines) != 1 {
		return wire.VersionLine{}, protocolcli.InvalidConfigf(
			"this workspace declares %d version lines (%s); name one with --scope",
			len(lines), strings.Join(lineNames(lines), ", "))
	}
	return lines[0], nil
}

// proposedTag is the version this release takes and the tag that names it:
// the release the host proposes, or the explicit tag, which overrides the
// computation whole. That is how a human releases a version the commits did
// not ask for. A host that proposes no release for an untagged line, as an
// older host may, leaves nothing to release without an explicit tag.
func proposedTag(line wire.VersionLine, explicit string) (version, tagName string, err error) {
	if explicit == "" {
		if line.Release == nil {
			return "", "", protocolcli.InvalidConfigf(
				"the host proposes no release for line %s; name the tag to release", printableLine(line.Line))
		}
		return line.Release.Version, line.Release.Tag, nil
	}
	parsed, ok := linetag.Version(explicit, line.TagPattern)
	if !ok {
		return "", "", protocolcli.InvalidConfigf(
			"tag %q is not a release tag of line %s: the pattern %q with a MAJOR.MINOR.PATCH version",
			explicit, printableLine(line.Line), line.TagPattern)
	}
	return parsed, explicit, nil
}

func firstOrEmpty(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// coAuthorTrailer names the human running the command as the co-author of the
// release commit, read from THEIR git config only. A release commit is authored
// by a person; nothing in this path may claim someone else's identity.
func (c Command) coAuthorTrailer(ctx context.Context) string {
	name, nameErr := c.gitOutput(ctx, "config", "--get", "user.name")
	email, emailErr := c.gitOutput(ctx, "config", "--get", "user.email")
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	if nameErr != nil || emailErr != nil || name == "" || email == "" {
		return ""
	}
	return fmt.Sprintf("Co-Authored-By: %s <%s>", name, email)
}

func (c Command) requireCleanGitTree(ctx context.Context) error {
	output, err := c.gitOutput(ctx, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(output) != "" {
		return protocolcli.InvalidConfigf("git tree is not clean; commit or discard changes before tagging")
	}
	return nil
}

func (c Command) gitTagExists(ctx context.Context, tagName string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", c.Root, "rev-parse", "-q", "--verify", "refs/tags/"+tagName)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		exitErr := &exec.ExitError{}
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("check git tag: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return true, nil
}

func (c Command) gitOutput(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", c.Root}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// runGit runs a git verb that writes. Its standard output is the command's;
// its standard error is kept for the error.
func (c Command) runGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", c.Root}, args...)...)
	var stderr bytes.Buffer
	cmd.Stdout = c.Stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
