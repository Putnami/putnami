package versioncmd

import (
	"errors"
	"fmt"
	"os"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// versionCheckUsage is the usage line of `version check`.
const versionCheckUsage = "usage: putnami version check --base <ref> (--title <title> | --message-file <path>)"

// versionCheckFlags are the flags of `version check`.
type versionCheckFlags struct {
	Base        string
	Title       string
	MessageFile string
}

// VersionCheck holds the breaking marker across a squash merge. A squash
// commit keeps the title of the pull request, and its body when the host is
// set to, and drops the commits of the branch. The version bump and the API
// checks then read the squash commit alone, so a marker that sits only on a
// branch commit is lost.
//
// The check reads the commits between --base and HEAD and fails when one
// declares a breaking change and the squash commit message declares none. It
// names each such commit. The message is --title alone, or the content of
// --message-file, split as git splits a commit message: the subject, a blank
// line, then the body. The
// check reads every message with the reader the version bump uses, so the two
// cannot disagree.
func VersionCheck(wsRoot string, args []string) error {
	flags, err := parseVersionCheckFlags(args)
	if err != nil {
		return err
	}
	title, body := flags.Title, ""
	if flags.MessageFile != "" {
		data, readErr := os.ReadFile(flags.MessageFile)
		if readErr != nil {
			return cmderr.Usagef("version check: read --message-file: %v", readErr)
		}
		if title, body = splitCommitMessage(string(data)); title == "" {
			return cmderr.Usagef("version check: the first line of --message-file is empty; it is the subject of the squash commit")
		}
	}
	// A shallow clone holds a fraction of the range, so a branch commit that
	// declares a break may be missing: the check would pass without reading it.
	shallow, err := git.IsShallow(wsRoot)
	if err != nil {
		return fmt.Errorf("version check: %w", err)
	}
	if shallow {
		return fmt.Errorf("version check: the clone is shallow, so the commits since %s are incomplete; fetch the full history", flags.Base)
	}
	commits, err := git.CommitsSince(wsRoot, flags.Base, nil)
	if err != nil {
		return fmt.Errorf("version check: read the commits since %s: %w", flags.Base, err)
	}
	breaking := git.BreakingCommits(commits)
	if len(breaking) == 0 {
		iox.Fprintf(os.Stdout, "  No commit since %s declares a breaking change.\n", flags.Base)
		return nil
	}
	squash, conventional := protocolcli.ParseConventionalCommit(title, body)
	if conventional && squash.Breaking {
		iox.Fprintf(os.Stdout, "  The squash commit keeps the breaking change that %s since %s.\n",
			declaringCommits(len(breaking)), flags.Base)
		return nil
	}

	var message strings.Builder
	fmt.Fprintf(&message, "version check: the squash commit %q drops the breaking change that %s since %s:\n",
		title, declaringCommits(len(breaking)), flags.Base)
	for _, commit := range breaking {
		fmt.Fprintf(&message, "  %s %s\n", shortCommit(commit.SHA), commit.Subject)
	}
	if conventional {
		message.WriteString(`Add "!" before the colon of the title, or a "BREAKING CHANGE:" footer to the body when the squash commit keeps the body.`)
	} else {
		message.WriteString(`The title is not a conventional commit, so nothing in the body counts: write it as "type(scope)!: summary".`)
	}
	message.WriteString(" If the change breaks nothing, reword the commit.")
	return errors.New(message.String())
}

// splitCommitMessage splits a commit message into its subject and its body
// the way git does: the subject is every line before the first blank line,
// joined by a space, and the body is what follows. A footer written directly
// under the first line, with no blank line, is therefore part of the subject,
// where it declares nothing.
func splitCommitMessage(message string) (subject, body string) {
	lines := strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n")
	end := 0
	for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
		lines[end] = strings.TrimSpace(lines[end])
		end++
	}
	return strings.Join(lines[:end], " "), strings.Join(lines[min(end+1, len(lines)):], "\n")
}

// declaringCommits renders how many commits declare a breaking change, as the
// end of a relative clause: "1 commit declares", "3 commits declare".
func declaringCommits(n int) string {
	if n == 1 {
		return "1 commit declares"
	}
	return fmt.Sprintf("%d commits declare", n)
}

// shortCommit is the 12-character form of a commit ID, the length a version
// suffix uses.
func shortCommit(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// parseVersionCheckFlags reads the flags of `version check`. The CLI's argument
// parser has already split a "--flag=value" into two arguments.
func parseVersionCheckFlags(args []string) (versionCheckFlags, error) {
	var flags versionCheckFlags
	targets := map[string]*string{"--base": &flags.Base, "--title": &flags.Title, "--message-file": &flags.MessageFile}
	for i := 0; i < len(args); i++ {
		target, known := targets[args[i]]
		if !known {
			return flags, cmderr.Usagef("version check: unexpected argument %q\n  %s", args[i], versionCheckUsage)
		}
		i++
		if i >= len(args) {
			return flags, cmderr.Usagef("version check: %s requires a value\n  %s", args[i-1], versionCheckUsage)
		}
		*target = args[i]
	}
	hasTitle := strings.TrimSpace(flags.Title) != ""
	if flags.Base == "" || hasTitle == (flags.MessageFile != "") {
		return flags, cmderr.Usagef("version check: --base and one of --title or --message-file are required\n  %s", versionCheckUsage)
	}
	return flags, nil
}
