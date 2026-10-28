package cachekey

import (
	"go.putnami.dev/tooling/cli/internal/git"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// treeKeyedGit runs git in dir with a fixed identity and fixed dates, and
// returns its trimmed output.
func treeKeyedGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(git.Executable(), append([]string{
		"-c", "user.name=T", "-c", "user.email=t@t.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
	}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z", "GIT_AUTHOR_DATE=2026-01-02T03:04:05Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
