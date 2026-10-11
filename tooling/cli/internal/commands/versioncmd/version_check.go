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
const versionCheckUsage = "usage: putnami version check --base <ref> --title <title> [--body-file <path>]"

// versionCheckFlags are the flags of `version check`.
type versionCheckFlags struct {
	Base     string
	Title    string
	BodyFile string
}

// VersionCheck holds the breaking marker across a squash merge. A squash
// commit keeps the title of the pull request, and its body when the host is
// set to, and drops the commits of the branch. The version bump and the API
// checks then read the squash commit alone, so a marker that sits only on a
// branch commit is lost.
//
// The check reads the commits between --base and HEAD and fails when one
// declares a breaking change and the squash message, --title with the content
// of --body-file, declares none. It names each such commit. It reads every
// message with the reader the version bump uses, so the two cannot disagree.
func VersionCheck(wsRoot string, args []string) error {
	flags, err := parseVersionCheckFlags(args)
	if err != nil {
		return err
	}
	body := ""
	if flags.BodyFile != "" {
		data, readErr := os.ReadFile(flags.BodyFile)
		if readErr != nil {
			return cmderr.Usagef("version check: read --body-file: %v", readErr)
		}
		body = string(data)
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
	squash, conventional := protocolcli.ParseConventionalCommit(flags.Title, body)
	if conventional && squash.Breaking {
		iox.Fprintf(os.Stdout, "  The squash commit keeps the breaking change that %s since %s.\n",
			declaringCommits(len(breaking)), flags.Base)
		return nil
	}

	var message strings.Builder
	fmt.Fprintf(&message, "version check: the squash commit %q drops the breaking change that %s since %s:\n",
		flags.Title, declaringCommits(len(breaking)), flags.Base)
	for _, commit := range breaking {
		fmt.Fprintf(&message, "  %s %s\n", shortCommit(commit.SHA), commit.Subject)
	}
	if conventional {
		message.WriteString(`Add "!" before the colon of the title, or a "BREAKING CHANGE:" footer to the body when the squash commit keeps it.`)
	} else {
		message.WriteString(`The title is not a conventional commit, so nothing in the body counts: write it as "type(scope)!: summary".`)
	}
	message.WriteString(" If the change breaks nothing, reword the commit.")
	return errors.New(message.String())
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

func parseVersionCheckFlags(args []string) (versionCheckFlags, error) {
	var flags versionCheckFlags
	targets := map[string]*string{"--base": &flags.Base, "--title": &flags.Title, "--body-file": &flags.BodyFile}
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		target, known := targets[name]
		if !known {
			return flags, cmderr.Usagef("version check: unexpected argument %q\n  %s", args[i], versionCheckUsage)
		}
		if !inline {
			i++
			if i >= len(args) {
				return flags, cmderr.Usagef("version check: %s requires a value\n  %s", name, versionCheckUsage)
			}
			value = args[i]
		}
		*target = value
	}
	if flags.Base == "" || strings.TrimSpace(flags.Title) == "" {
		return flags, cmderr.Usagef("version check: --base and --title are required\n  %s", versionCheckUsage)
	}
	return flags, nil
}
