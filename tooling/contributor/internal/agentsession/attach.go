package agentsession

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Attach runs `agent attach --issue <n>`: it declares issue-<n> the mission
// of this checkout's branch. Every session of the checkout and branch
// reports to that mission from its next sync. It writes the declaration under
// the session store, never in the checkout. A usage error exits 2 and a
// failure exits 1.
func Attach(ctx context.Context, e *Env, args []string, stdout, stderr io.Writer) int {
	issue, err := parseAttachArgs(args)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agent attach: %v\n", err)
		return 2
	}
	root := canonicalRoot(e.Root)
	if root == "" {
		_, _ = fmt.Fprintln(stderr, "agent attach: no workspace root")
		return 2
	}
	branch := e.checkoutBranch(ctx, root)
	if err := declareMissionIssue(e.sessionsDir(), root, branch, issue, e.Now()); err != nil {
		_, _ = fmt.Fprintf(stderr, "agent attach: %v\n", err)
		return 1
	}
	where := "on branch " + branch
	if branch == "" {
		where = "on a detached HEAD"
	}
	_, _ = fmt.Fprintf(stdout, "The sessions of this checkout %s now report to mission issue-%d.\n", where, issue)
	return 0
}

// parseAttachArgs reads `--issue <n>`, `--issue=<n>` or `--issue #<n>`.
func parseAttachArgs(args []string) (int, error) {
	value, seen := "", false
	for len(args) > 0 {
		raw := args[0]
		args = args[1:]
		name, inline, hasValue := strings.Cut(raw, "=")
		if name != "--issue" {
			return 0, fmt.Errorf("unexpected argument %q; use --issue <number>", boundedLine(raw, 64))
		}
		if seen {
			return 0, errors.New("--issue given twice")
		}
		seen = true
		if hasValue {
			value = inline
			continue
		}
		if len(args) == 0 {
			return 0, errors.New("--issue needs an issue number")
		}
		value, args = args[0], args[1:]
	}
	issue, err := strconv.Atoi(strings.TrimPrefix(value, "#"))
	if !seen || err != nil || issue <= 0 || issue > missionIssueMax {
		return 0, errors.New("--issue must be a positive issue number")
	}
	return issue, nil
}
