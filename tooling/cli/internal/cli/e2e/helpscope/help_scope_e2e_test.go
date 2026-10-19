// Package helpscope drives `putnami --help` through the real CLI in a fixture
// workspace: help lists the job commands, aliases and provider flags of the
// extensions the workspace loads. These tests swap the process streams and the
// working directory, so they run in their own test binary.
package helpscope

import (
	"os"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

func TestHelpListsWhatTheWorkspaceExtensionsProvide(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "workspace-scoped-help", "help-lists-what-the-workspace-extensions-provide")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	for _, args := range [][]string{{"--help"}, {"help"}, {"help", "build"}} {
		t.Run(strings.Join(args, " ")+" without a provider", func(t *testing.T) {
			code, output := clitest.RunGateArgs(t, clitest.WhereFixture(t, false, false), args...)
			if code != 0 {
				t.Fatalf("exit %d:\n%s", code, output)
			}
			// A hosted launcher reads --credential-fd from the pinned CLI's help
			// wherever it runs, so help always lists it.
			if args[0] == "--help" && !strings.Contains(output, "--credential-fd") {
				t.Errorf("help must always list --credential-fd:\n%s", output)
			}
			for _, hidden := range []string{"d=deploy", ", deploy", "putnami serve", "--where", "--cache-trust", "--providers"} {
				if strings.Contains(output, hidden) {
					t.Errorf("help lists %q, which the fixture's extension does not provide:\n%s", hidden, output)
				}
			}
		})
	}
	code, output := clitest.RunGateArgs(t, clitest.WhereFixture(t, false, false), "--help")
	// The fixture declares no vocabulary, so help lists its commands by name.
	if code != 0 || !strings.Contains(output, "build, format, lint, test, validate") {
		t.Errorf("help must list the fixture's job commands (exit %d):\n%s", code, output)
	}
	code, output = clitest.RunGateArgs(t, clitest.WhereFixture(t, false, true), "--help")
	if code != 0 || !strings.Contains(output, "--where") {
		t.Errorf("a runner provider gives --where meaning (exit %d):\n%s", code, output)
	}
}
