package refstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Deadlines of one git command. A command that reaches the network gets the
// longer one. A write's own deadlines (writeBudget, reconcileBudget) cut a
// command that would outlast them.
const (
	localTimeout   = 15 * time.Second
	networkTimeout = 20 * time.Second
)

// repositoryVariables locate a repository or an index, or name the author
// of a commit. A provider started from a Git hook inherits them for the
// repository that ran the hook; every command here names its own.
var repositoryVariables = map[string]bool{
	"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_COMMON_DIR": true, "GIT_NAMESPACE": true,
	"GIT_CEILING_DIRECTORIES": true, "GIT_DISCOVERY_ACROSS_FILESYSTEM": true, "GIT_PREFIX": true,
	"GIT_QUARANTINE_PATH": true, "GIT_AUTHOR_NAME": true, "GIT_AUTHOR_EMAIL": true, "GIT_AUTHOR_DATE": true,
	"GIT_COMMITTER_NAME": true, "GIT_COMMITTER_EMAIL": true, "GIT_COMMITTER_DATE": true,
	"LC_ALL": true, "LANG": true, "LANGUAGE": true, "GIT_TERMINAL_PROMPT": true,
}

// configuration every command runs with: no hook runs, no background
// maintenance starts, a held ref lock is waited for, and nothing is signed.
var configuration = []string{
	"-c", "core.hooksPath=" + os.DevNull,
	"-c", "core.filesRefLockTimeout=5000",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "fetch.writeCommitGraph=false",
	"-c", "commit.gpgSign=false",
}

// Result is what one git command produced.
type Result struct {
	stdout []byte
	stderr []byte
	code   int
}

// Intercept lets a test observe or replace one git command: it receives the
// arguments and the real runner.
type Intercept func(ctx context.Context, args []string, run func() (Result, error)) (Result, error)

func execGit(ctx context.Context, env []string, stdin []byte, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := Result{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	var exit *exec.ExitError
	switch {
	case err == nil:
		return out, nil
	case ctx.Err() != nil:
		return out, fmt.Errorf("git %s did not finish: %w", verb(args), ctx.Err())
	case errors.As(err, &exit):
		out.code = exit.ExitCode()
		return out, nil
	default:
		return out, fmt.Errorf("run git: %w", err)
	}
}

// environment is the process environment without the repository variables,
// in the C locale, never prompting, plus extra.
func environment(extra []string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra)+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !repositoryVariables[key] {
			env = append(env, entry)
		}
	}
	env = append(env, "LC_ALL=C", "GIT_TERMINAL_PROMPT=0")
	return append(env, extra...)
}

// verb names a command for a message: its first argument that is not an
// option or an option value.
func verb(args []string) string {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "-C":
			i++
		case strings.HasPrefix(args[i], "-"):
		default:
			return args[i]
		}
	}
	return "command"
}

var (
	userinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]+@`)
	// urlTail is the query and fragment of a URL, which can carry a
	// credential.
	urlTail = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"?#]*)[?#][^\s'"]*`)
)

// summary is the first non-empty line of a command's standard error, with
// the userinfo, query and fragment of every URL removed, bounded. It is what
// a failure message quotes; the full output never leaves the provider.
func summary(stderr []byte) string {
	for line := range strings.Lines(string(stderr)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = urlTail.ReplaceAllString(userinfo.ReplaceAllString(line, "$1"), "$1")
		if len(line) > 300 {
			line = line[:300] + "…"
		}
		return line
	}
	return ""
}

func describe(args []string, out Result) string {
	message := fmt.Sprintf("git %s exited %d", verb(args), out.code)
	if detail := summary(out.stderr); detail != "" {
		message += ": " + detail
	}
	return message
}
