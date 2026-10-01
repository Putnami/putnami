//go:build !windows

package recorded

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// executable writes the replay as a POSIX shell script; the test is skipped
// where /bin/sh does not exist.
func executable(tb testing.TB, exchange Exchange, branches []Branch) string {
	tb.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		tb.Skip("/bin/sh is not available")
	}
	dir := tb.TempDir()
	var script strings.Builder
	script.WriteString("#!/bin/sh\n")
	for index, branch := range branches {
		if !envName.MatchString(branch.Env) {
			tb.Fatalf("recorded branch environment name %q is not a shell variable name", branch.Env)
		}
		fmt.Fprintf(&script, "if [ \"${%s:-}\" = %s ]; then\n%sfi\n",
			branch.Env, shellQuote(branch.Value), replayLines(tb, dir, "branch-"+strconv.Itoa(index), branch.Exchange))
	}
	script.WriteString(replayLines(tb, dir, "default", exchange))
	path := filepath.Join(dir, "replay")
	if err := os.WriteFile(path, []byte(script.String()), 0o755); err != nil { //nolint:gosec // the replayed command must be executable
		tb.Fatal(err)
	}
	return path
}

// replayLines copies an exchange's streams next to the script, so the bytes the
// program writes are the recorded bytes and no shell quoting touches them.
func replayLines(tb testing.TB, dir, name string, exchange Exchange) string {
	tb.Helper()
	stdout := filepath.Join(dir, name+".stdout")
	stderr := filepath.Join(dir, name+".stderr")
	if err := os.WriteFile(stdout, exchange.stdout, 0o600); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(stderr, exchange.stderr, 0o600); err != nil {
		tb.Fatal(err)
	}
	return fmt.Sprintf("cat %s\ncat %s >&2\nexit %d\n", shellQuote(stdout), shellQuote(stderr), exchange.exitCode)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
