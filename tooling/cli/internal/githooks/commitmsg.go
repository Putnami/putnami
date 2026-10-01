package githooks

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// CommitMsg validates a commit message file against conventional commit format
// and exits with the resulting code. It is invoked directly by the git commit-msg
// hook (not via the JSONL job protocol).
func CommitMsg(args []string) {
	os.Exit(runCommitMsg(args, os.Stderr))
}

// runCommitMsg is the testable core of CommitMsg: it reads the commit message file
// named by args[0], validates it, and returns the process exit code (0 = accept,
// 1 = reject). Diagnostics are written to errOut.
func runCommitMsg(args []string, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(errOut, "Error: no commit message file provided")
		return 1
	}

	msgFile := args[0]
	data, err := os.ReadFile(msgFile)
	if err != nil {
		fmt.Fprintf(errOut, "Error reading commit message file: %v\n", err)
		return 1
	}

	message := strings.TrimSpace(string(data))

	// Skip merge commits and fixup/squash/amend commits.
	if strings.HasPrefix(message, "Merge ") ||
		strings.HasPrefix(message, "fixup! ") ||
		strings.HasPrefix(message, "squash! ") ||
		strings.HasPrefix(message, "amend! ") {
		return 0
	}

	if errMsg := Validate(message); errMsg != "" {
		fmt.Fprintf(errOut, "\n  Commit message validation failed:\n  %s\n\n", errMsg)
		fmt.Fprintln(errOut, "  Expected format: type(scope): subject")
		fmt.Fprintln(errOut, "  Example: feat(auth): add login flow")
		fmt.Fprintln(errOut, "  Valid types: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert")
		fmt.Fprintln(errOut)
		return 1
	}

	return 0
}
