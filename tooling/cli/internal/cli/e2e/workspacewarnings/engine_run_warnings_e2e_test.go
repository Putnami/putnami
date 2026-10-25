package workspacewarnings

import (
	"path/filepath"
	"strings"
	"testing"

	ciproto "go.putnami.dev/protocol/ci"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// gateRepository is the placement fixture as a repository whose gate is
// build, with a second commit that changes /app. Its gate task writes nothing. When declaresJobs is true,
// app's putnami.json declares a `jobs` key, which the load warns about. It
// returns the root and the first commit.
func gateRepository(t *testing.T, declaresJobs bool) (root, baseSHA string) {
	t.Helper()
	root = clitest.WhereFixture(t, false, false)
	// A gate task that writes nothing passes cache verify's write-closure check.
	clitest.WriteFile(t, filepath.Join(root, "extension", "gate.sh"), "exit 0\n")
	if declaresJobs {
		clitest.WriteFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app","extensions":["@fixture/gate"],"jobs":{}}`)
	}
	clitest.WriteFile(t, filepath.Join(root, ciproto.Filename), `{"version":3,"commands":["build"]}`)
	clitest.WriteFile(t, filepath.Join(root, ".gitignore"), ".putnami\n")
	clitest.InitGitRepo(t, root)
	baseSHA = strings.TrimSpace(clitest.GitOutput(t, root, "rev-parse", "HEAD"))
	clitest.WriteFile(t, filepath.Join(root, "app", "source.txt"), "head\n")
	clitest.RunGit(t, root, "add", "app/source.txt")
	clitest.RunGit(t, root, "commit", "-m", "head")
	return root, baseSHA
}

// TestEngineRunCommandsWriteTheWarningsOfTheirLoadOnce holds impact-plan and
// cache verify, whose quiet engine runs load the workspace, to the result
// envelope contract: each load warning is written once on stderr, unless
// --quiet, and the envelope carries it either way. A load with no warning
// reports none.
func TestEngineRunCommandsWriteTheWarningsOfTheirLoadOnce(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "a-command-reports-the-warnings-of-its-own-load-once")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("DO_NOT_TRACK", "1")

	for _, tc := range []struct {
		name string
		args func(baseSHA string) []string
	}{
		{"impact-plan", func(baseSHA string) []string {
			return []string{"impact-plan", "build", "--base", baseSHA, "--no-cache", "--output=json"}
		}},
		{"cache verify", func(string) []string { return []string{"cache", "verify", "--output=json"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
			root, baseSHA := gateRepository(t, true)
			args := tc.args(baseSHA)

			code, envelope, stdout, stderr := run(t, root, args...)
			if code != protocolcli.ExitSuccess {
				t.Fatalf("exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			assertJobsIgnoredWarning(t, envelope)
			line := "putnami: warning: " + envelope.WorkspaceWarnings[0].Message
			if count := strings.Count(stderr, line); count != 1 {
				t.Errorf("stderr carries the warning %d times, want once:\n%s", count, stderr)
			}

			code, envelope, stdout, stderr = run(t, root, append(args, "--quiet")...)
			if code != protocolcli.ExitSuccess {
				t.Fatalf("--quiet exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			assertJobsIgnoredWarning(t, envelope)
			if strings.Contains(stderr, "putnami: warning: ") {
				t.Errorf("--quiet wrote the warning to stderr:\n%s", stderr)
			}

			clean, cleanBase := gateRepository(t, false)
			code, _, stdout, stderr = run(t, clean, tc.args(cleanBase)...)
			if code != protocolcli.ExitSuccess {
				t.Fatalf("clean workspace exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
			}
			if strings.Contains(stdout, "workspaceWarnings") || strings.Contains(stderr, "putnami: warning: ") {
				t.Errorf("a load with no warning reported one:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
			}
		})
	}
}
