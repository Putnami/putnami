package versioncmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// gitPushTimeout bounds a `git push` so a hung network operation (stalled TLS
// handshake, dead remote) eventually aborts instead of wedging the process. It
// is generous enough not to interrupt a legitimately large push.
const gitPushTimeout = 5 * time.Minute

// versionFlags are the flags of `version get` and `version tag`. A version is
// derived from git per line, so the only selector is the line.
type versionFlags struct {
	Scope  string
	Push   bool
	DryRun bool
	Yes    bool
}

// versionResultEntry is the --output=jsonl shape for `version get`: one record
// per version line.
type versionResultEntry struct {
	Action  string `json:"action"` // "get"
	Line    string `json:"line"`   // the line's scope path; "" is the root line
	Tag     string `json:"tag,omitempty"`
	Version string `json:"version"`
	Tagged  bool   `json:"tagged,omitempty"`
}

func emitVersionResultJSONL(entry versionResultEntry) {
	data, _ := json.Marshal(entry)
	iox.Fprintln(os.Stdout, string(data))
}

// VersionGet prints the version every line of the workspace is at: one line per
// version line, "<line> <computed full version>".
//
// It computes rather than reads. Nothing declares a version any more, so this
// is the same derivation a build stamps with, asked directly.
func VersionGet(wsRoot string, args []string, outputFormat string) error {
	flags, rest, err := parseVersionFlags(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("usage: putnami version get [--scope <line>]")
	}
	specs, err := lineSpecs(wsRoot)
	if err != nil {
		return err
	}
	if flags.Scope != "" {
		spec, selectErr := selectLine(specs, flags.Scope)
		if selectErr != nil {
			return selectErr
		}
		specs = []git.LineSpec{spec}
	}
	for _, spec := range specs {
		info, infoErr := git.GetVersionInfo(wsRoot, spec)
		if infoErr != nil {
			return infoErr
		}
		if outputFormat == "jsonl" {
			emitVersionResultJSONL(versionResultEntry{
				Action: "get", Line: spec.ScopePath, Tag: info.Tag, Version: info.Full, Tagged: info.Tagged,
			})
			continue
		}
		iox.Fprintf(os.Stdout, "  %s %s\n", printableLine(spec.ScopePath), info.Full)
	}
	return nil
}

// VersionTag creates one line's release: the regenerated changelog, the release
// commit that carries it, then the annotated tag on that commit (D30, R9).
//
// The order is the whole point. The changelog is generated from the same
// commits the version is computed from, so it has to be committed BEFORE the
// tag exists — a tag created first would point at a tree without its own
// release notes, and the notes would then describe a commit they are not in.
func VersionTag(ctx context.Context, wsRoot string, args []string) error {
	flags, rest, err := parseVersionFlags(args)
	if err != nil {
		return err
	}
	if len(rest) > 1 {
		return fmt.Errorf("usage: putnami version tag [<tag>] [--scope <line>] [--push] [--dry-run] [--yes]")
	}
	if err := requireSourceRevisionAtHead(wsRoot); err != nil {
		return err
	}
	specs, err := lineSpecs(wsRoot)
	if err != nil {
		return err
	}
	spec, err := resolveTagLine(specs, flags.Scope)
	if err != nil {
		return err
	}
	info, err := git.GetVersionInfo(wsRoot, spec)
	if err != nil {
		return err
	}
	if info.Tagged {
		return cmderr.InvalidConfigf("HEAD is already tagged %s", info.Tag)
	}
	version, tagName, err := proposedTag(wsRoot, spec, firstOrEmpty(rest))
	if err != nil {
		return err
	}
	commits, err := lineCommits(wsRoot, spec)
	if err != nil {
		return err
	}
	notes := RenderChangelog(printableLine(spec.ScopePath), version, commits)

	if flags.DryRun {
		iox.Fprintf(os.Stdout, "  Would release %s %s as %s\n", printableLine(spec.ScopePath), version, tagName)
		iox.Fprintln(os.Stdout, notes)
		return nil
	}
	if err := requireCleanGitTree(ctx, wsRoot); err != nil {
		return err
	}
	if exists, existsErr := gitTagExists(ctx, wsRoot, tagName); existsErr != nil {
		return existsErr
	} else if exists {
		return cmderr.InvalidConfigf("git tag already exists: %s", tagName)
	}
	if !flags.Yes {
		iox.Fprintf(os.Stdout, "  Release %s %s as %s? Re-run with --yes to confirm.\n",
			printableLine(spec.ScopePath), version, tagName)
		return nil
	}

	changelogPath := filepath.Join(wsRoot, filepath.FromSlash(changelogRelPath(spec.ScopePath)))
	if err := PrependChangelog(changelogPath, notes); err != nil {
		return err
	}
	if err := runGit(ctx, wsRoot, "add", "--", changelogRelPath(spec.ScopePath)); err != nil {
		return err
	}
	message := fmt.Sprintf("chore(release): %s %s", printableLine(spec.ScopePath), version)
	if trailer := coAuthorTrailer(ctx, wsRoot); trailer != "" {
		message += "\n\n" + trailer
	}
	if err := runGit(ctx, wsRoot, "commit", "-m", message); err != nil {
		return err
	}
	// --cleanup=verbatim: the notes are markdown, and git's default cleanup
	// strips every line starting with "#" as a comment — which is every heading
	// of the changelog, leaving a tag message of bare bullets.
	if err := runGit(ctx, wsRoot, "tag", "--cleanup=verbatim", "-a", tagName, "-m", notes); err != nil {
		return err
	}
	iox.Fprintf(os.Stdout, "  Created %s on the release commit\n", tagName)
	if flags.Push {
		// git push is a network op: bound it so a hung remote eventually aborts.
		pushCtx, cancel := context.WithTimeout(ctx, gitPushTimeout)
		defer cancel()
		if err := runGit(pushCtx, wsRoot, "push", "origin", "HEAD", tagName); err != nil {
			return err
		}
		iox.Fprintf(os.Stdout, "  Pushed HEAD and %s\n", tagName)
	}
	return nil
}

// requireSourceRevisionAtHead refuses a release while PUTNAMI_SOURCE_REVISION
// binds the run to a commit other than HEAD. `version tag` computes the version
// from HEAD's history and puts the release commit and its tag on top of HEAD,
// so a tag always names the checked-out commit. The bound commit may not even
// be in the repository, and releasing under it would record one commit's
// release against another's identity. A binding that names HEAD itself
// changes nothing, so it is accepted.
func requireSourceRevisionAtHead(wsRoot string) error {
	revision, set, err := git.SourceRevisionOverride()
	if err != nil {
		return cmderr.InvalidConfigf("version tag: %v", err)
	}
	if !set {
		return nil
	}
	head, err := git.HeadSHA(wsRoot)
	if err != nil {
		return fmt.Errorf("version tag: read HEAD: %w", err)
	}
	if revision != strings.ToLower(head) {
		return cmderr.InvalidConfigf(
			"version tag releases the checked-out commit %s, but %s binds this run to %s; unset it to tag HEAD",
			head, git.SourceRevisionEnv, revision)
	}
	return nil
}

// proposedTag computes the version this release takes and the tag that names
// it. An explicit name overrides the computation whole, which is how a human
// releases a version the commits did not ask for.
func proposedTag(wsRoot string, spec git.LineSpec, explicit string) (version, tagName string, err error) {
	if explicit != "" {
		parsed, ok := versionFromPattern(explicit, spec.TagPattern)
		if !ok {
			return "", "", cmderr.InvalidConfigf(
				"tag %q does not match the pattern %q of line %s", explicit, spec.TagPattern, printableLine(spec.ScopePath))
		}
		return parsed, explicit, nil
	}
	lastTag, lastCommit, found, err := lastLineTag(wsRoot, spec)
	if err != nil {
		return "", "", err
	}
	last := "0.0.0"
	bump := git.BumpNone
	if found {
		last, _ = versionFromPattern(lastTag, spec.TagPattern)
		commits, commitsErr := git.CommitsSince(wsRoot, lastCommit, spec.Pathspecs)
		if commitsErr != nil {
			return "", "", commitsErr
		}
		bump = git.BumpFor(commits, majorOf(last) == 0)
	}
	version = git.NextVersion(last, bump, false)
	return version, wsproto.RenderLineTag(spec.TagPattern, version), nil
}

// lineCommits are the commits the changelog renders: everything since the
// line's last tag that touches it, or its whole history when it has none.
func lineCommits(wsRoot string, spec git.LineSpec) ([]git.Commit, error) {
	_, lastCommit, found, err := lastLineTag(wsRoot, spec)
	if err != nil {
		return nil, err
	}
	if !found {
		lastCommit = ""
	}
	return git.CommitsSince(wsRoot, lastCommit, spec.Pathspecs)
}

// lastLineTag is the line's last reachable tag, matched against the complete
// pattern so a similarly-prefixed tag from another convention cannot become
// the version baseline.
func lastLineTag(wsRoot string, spec git.LineSpec) (tag, commit string, ok bool, err error) {
	if strings.Count(spec.TagPattern, wsproto.LineTagPlaceholder) != 1 {
		return "", "", false, cmderr.InvalidConfigf(
			"line %s has tag pattern %q, which carries no %s placeholder",
			printableLine(spec.ScopePath), spec.TagPattern, wsproto.LineTagPlaceholder)
	}
	return git.LastReachableTag(wsRoot, spec.TagPattern)
}

// lineSpecs are the workspace's version lines, in sorted scope-path order, each
// bound to the paths whose commits advance it.
func lineSpecs(wsRoot string) ([]git.LineSpec, error) {
	ws, err := workspace.Load(wsRoot)
	if err != nil {
		return nil, fmt.Errorf("load workspace: %w", err)
	}
	paths := make([]string, 0, len(ws.Lines))
	for path := range ws.Lines {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	specs := make([]git.LineSpec, 0, len(paths))
	for _, path := range paths {
		spec := git.LineSpec{ScopePath: path, TagPattern: ws.Lines[path]}
		if path != "" {
			spec.Pathspecs = []string{path}
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		specs = append(specs, git.LineSpec{TagPattern: wsproto.LineTagPattern("", nil)})
	}
	return specs, nil
}

// resolveTagLine picks the line a release names. --scope is required as soon as
// the workspace has more than one line: tagging the wrong line publishes the
// wrong cohort, and a default would pick it silently.
func resolveTagLine(specs []git.LineSpec, scope string) (git.LineSpec, error) {
	if scope != "" {
		return selectLine(specs, scope)
	}
	if len(specs) != 1 {
		names := make([]string, 0, len(specs))
		for _, spec := range specs {
			names = append(names, printableLine(spec.ScopePath))
		}
		return git.LineSpec{}, cmderr.InvalidConfigf(
			"this workspace declares %d version lines (%s); name one with --scope",
			len(specs), strings.Join(names, ", "))
	}
	return specs[0], nil
}

// selectLine resolves a --scope value to one declared line.
func selectLine(specs []git.LineSpec, scope string) (git.LineSpec, error) {
	wanted := strings.Trim(scope, "/")
	for _, spec := range specs {
		if spec.ScopePath == wanted {
			return spec, nil
		}
	}
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, printableLine(spec.ScopePath))
	}
	return git.LineSpec{}, cmderr.NotFoundf("no version line %q; this workspace declares %s",
		scope, strings.Join(names, ", "))
}

// printableLine names a line for a human: the root line has no path, so it is
// spelled with the workspace's own marker rather than an empty column.
func printableLine(scopePath string) string {
	if scopePath == "" {
		return "(workspace root)"
	}
	return scopePath
}

// changelogRelPath is where a line's changelog lives: inside the line's scope
// directory, or at the repository root for the root line.
func changelogRelPath(scopePath string) string {
	if scopePath == "" {
		return "CHANGELOG.md"
	}
	return scopePath + "/CHANGELOG.md"
}

// versionFromPattern reads the version a tag carries under a line pattern.
func versionFromPattern(tag, pattern string) (string, bool) {
	prefix, suffix, found := strings.Cut(pattern, wsproto.LineTagPlaceholder)
	if !found || len(tag) <= len(prefix)+len(suffix) {
		return "", false
	}
	if !strings.HasPrefix(tag, prefix) || !strings.HasSuffix(tag, suffix) {
		return "", false
	}
	return tag[len(prefix) : len(tag)-len(suffix)], true
}

// majorOf reads the major number of a version, or 0 when it has none.
func majorOf(version string) int {
	major := 0
	if _, err := fmt.Sscanf(strings.TrimPrefix(version, "v"), "%d", &major); err != nil {
		return 0
	}
	return major
}

func firstOrEmpty(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func parseVersionFlags(args []string) (versionFlags, []string, error) {
	var flags versionFlags
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--scope":
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "-") {
				return flags, nil, fmt.Errorf("--scope requires a value")
			}
			flags.Scope = strings.Trim(args[i], "/")
		case "--push":
			flags.Push = true
		case "--dry-run":
			flags.DryRun = true
		case "--yes":
			flags.Yes = true
		default:
			rest = append(rest, arg)
		}
	}
	return flags, rest, nil
}

// coAuthorTrailer names the human running the command as the co-author of the
// release commit, read from THEIR git config only. A release commit is authored
// by a person; nothing in this path may claim someone else's identity.
func coAuthorTrailer(ctx context.Context, repoRoot string) string {
	name, nameErr := gitOutput(ctx, repoRoot, "config", "--get", "user.name")
	email, emailErr := gitOutput(ctx, repoRoot, "config", "--get", "user.email")
	name, email = strings.TrimSpace(name), strings.TrimSpace(email)
	if nameErr != nil || emailErr != nil || name == "" || email == "" {
		return ""
	}
	return fmt.Sprintf("Co-Authored-By: %s <%s>", name, email)
}

func requireCleanGitTree(ctx context.Context, repoRoot string) error {
	output, err := gitOutput(ctx, repoRoot, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(output) != "" {
		return cmderr.InvalidConfigf("git tree is not clean; commit or discard changes before tagging")
	}
	return nil
}

func gitTagExists(ctx context.Context, repoRoot, tagName string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "-q", "--verify", "refs/tags/"+tagName)
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

func gitOutput(ctx context.Context, repoRoot string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// runGit runs a git verb that writes: add runs the clean filters, commit and
// push run the repository's hooks. So it counts as repository code
// (runcredential.MarkRepositoryCodeStarted).
func runGit(ctx context.Context, repoRoot string, args ...string) error {
	runcredential.MarkRepositoryCodeStarted("git " + args[0])
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repoRoot}, args...)...)
	var stderr bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
