package versioncmd

import (
	"os"
	"testing"

	registryproto "go.putnami.dev/protocol/registry"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/launch"
)

// fakeCLIReportsEnv turns a copy of this test binary into a CLI: run as
// `<copy> --version` with the variable set, it prints the variable's value and
// exits. It is how a Windows test ships a CLI that reports a version, because
// Windows runs no shell script.
const fakeCLIReportsEnv = "PUTNAMI_VERSIONCMD_FAKE_CLI_REPORTS"

// fakeCLIRegistryTokenEnv turns this test binary into the credential command
// `putnami cloud registry-token --host <host>`: it prints the variable's value
// as the bearer, and an empty value fails the way a CLI that is not signed in
// fails. `putnami pin` runs that command through its own executable, which in
// a test is this binary.
const fakeCLIRegistryTokenEnv = "PUTNAMI_VERSIONCMD_FAKE_REGISTRY_TOKEN"

// fakeCLINotSignedIn is the reason the fake credential command gives on stderr
// when it has no bearer.
const fakeCLINotSignedIn = "not signed in to Putnami Cloud"

// Registry fixtures in this package stand up their own HTTP server and select
// it through PUTNAMI_REGISTRY_URL or the workspace's registries.put entry. A
// hosted run exports an invocation broker that wins over every authored route
// and answers 401 to a test archive, so the process drops it before any test
// runs. Tests that exercise the broker set it themselves with t.Setenv.
func TestMain(m *testing.M) {
	if reports, ok := os.LookupEnv(fakeCLIReportsEnv); ok && len(os.Args) == 2 && os.Args[1] == "--version" {
		answerVersionAsFakeCLI(reports)
		os.Exit(0)
	}
	if token, ok := os.LookupEnv(fakeCLIRegistryTokenEnv); ok && len(os.Args) == 5 &&
		os.Args[1] == registryproto.SeamParentCommand && os.Args[2] == registryproto.SeamSubcommand {
		os.Exit(answerRegistryTokenAsFakeCLI(token))
	}
	os.Unsetenv(extension.PrivatePutRegistryURLEnv)
	os.Exit(m.Run())
}

// answerVersionAsFakeCLI prints reports the way a CLI run directly prints its
// version. A CLI that would relaunch through the workspace pin reports the
// pin instead, so a caller that forgets to disable the relaunch sees the
// difference.
func answerVersionAsFakeCLI(reports string) {
	if os.Getenv(launch.NoRelaunchEnv) != "1" || os.Getenv(launch.LaunchedEnv) != "" {
		reports += " (launched from workspace pin)"
	}
	_, _ = os.Stdout.WriteString(reports + "\n")
}

// answerRegistryTokenAsFakeCLI answers the credential command the way a CLI
// does. A CLI that runs its first-use install before the command prints the
// install's progress on stdout ahead of the bearer, and a CLI that relaunches
// runs other bytes: both happen unless the caller disables them, and both
// leave stdout that is not a bearer.
func answerRegistryTokenAsFakeCLI(token string) int {
	if os.Getenv("PUTNAMI_NO_AUTO_INSTALL") != "1" {
		_, _ = os.Stdout.WriteString("  ✓ Workspace setup completed\n")
	}
	if os.Getenv(launch.NoRelaunchEnv) != "1" {
		_, _ = os.Stdout.WriteString("  launched from workspace pin\n")
	}
	if token == "" {
		_, _ = os.Stderr.WriteString("warning: an extension was skipped\n" + fakeCLINotSignedIn + "\n")
		return 1
	}
	_, _ = os.Stdout.WriteString(token + "\n")
	return 0
}
