// Package credentialcustody drives a whole CLI session against a workspace
// whose build hook and build task are hostile processes that hunt for the run
// credential. It proves two ends of ADR 0055:
//
//   - On a hosted run (the engine holds the run credential from --credential-fd),
//     a repository-controlled process finds the bearer nowhere: not in its
//     environment, the user's home, the workspace, the temporary directory,
//     and, on Linux, not in the /proc environ, memory or descriptors of the
//     engine that holds it, nor through a ptrace attach.
//   - Without --credential-fd, the same run's job and hook environment is the
//     one it always had: PUTNAMI_CACHE_TOKEN and PUTNAMI_CLOUD_TOKEN pass
//     through and no offline signal is added.
//
// The engine, the hostile hook and the hostile task are all this test binary
// re-executed in a role (custodyRoleEnv), the same pattern
// credential_fd_unix_test.go and toolchainlock use: it lets the hostile
// processes run real Go syscalls (/proc reads, ptrace) that a shell script
// could not, and it makes the engine take the REAL runcredential.Capture path,
// so the engine is genuinely non-dumpable while it holds the credential.
//
// Each scenario runs a full CLI session in a child process and reads files, so
// the outer tests touch no process-wide state and stay t.Parallel-safe; they
// live in their own e2e binary rather than holding internal/cli serial.
package credentialcustody

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

// The environment variables that select a re-executed role and hand it the
// data it needs. They start with PUTNAMI_TEST_ so no lifecycle or child-env
// scrubbing touches them: they are neither framework credentials nor the
// offline signal.
const (
	// custodyRoleEnv names the role this process runs as, such as "engine",
	// "provider", "fetch", "hook" or "task". Empty means the ordinary test
	// binary.
	custodyRoleEnv = "PUTNAMI_TEST_CUSTODY_ROLE"
	// custodyReportEnv names the file a hostile role appends its findings to,
	// one JSON object per line.
	custodyReportEnv = "PUTNAMI_TEST_CUSTODY_REPORT"
	// custodyWorkspaceEnv names the workspace the engine role changes into
	// before it runs `build`.
	custodyWorkspaceEnv = "PUTNAMI_TEST_CUSTODY_WS"
	// custodyCredentialFDEnv, when set, is the descriptor number the engine
	// role passes as --credential-fd: a hosted run. Absent means a local run.
	custodyCredentialFDEnv = "PUTNAMI_TEST_CUSTODY_FD"
	// custodyCacheEnv, when set, runs the engine role's build with the build
	// cache on. Absent means --no-cache.
	custodyCacheEnv = "PUTNAMI_TEST_CUSTODY_CACHE"
	// custodyTargetEnv names the file the "tamper" role overwrites.
	custodyTargetEnv = "PUTNAMI_TEST_CUSTODY_TARGET"
	// custodyArgsEnv, when set, is the engine role's command, one argument per
	// line, in place of `build --projects app`.
	custodyArgsEnv = "PUTNAMI_TEST_CUSTODY_ARGS"
)

// TestMain runs the tests, or, when this binary was re-executed in a role, that
// role: the engine that runs `build`, the cache provider, the workspace-fetch,
// a hook that overwrites the provider's executable, or a hostile hook or task
// that probes for the credential. A role process exits without running any
// test. As the runtime of the fixture extension, this binary first answers the
// CLI's runtime-info handshake, which inherits the engine's environment and so
// its role.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "__putnami" && os.Args[2] == "runtime-info" {
		fmt.Println(fixtureRuntimeInfo())
		os.Exit(0)
	}
	switch role := os.Getenv(custodyRoleEnv); role {
	case "":
		os.Exit(clitest.Main(m))
	case "engine":
		os.Exit(runEngineRole())
	case "provider":
		os.Exit(runCacheProviderRole())
	case "fetch":
		os.Exit(runFetchRole())
	case "tamper":
		os.Exit(runTamperRole())
	default:
		os.Exit(runHostileRole(role))
	}
}

// fixtureRuntimeInfo is the runtime-info document of the fixture extension,
// @fixture/cache 0.1.0, on this machine.
func fixtureRuntimeInfo() string {
	return fmt.Sprintf(
		`{"extension":"@fixture/cache","version":"0.1.0","platform":%q,"cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`,
		runtime.GOOS+"/"+runtime.GOARCH, protocolcli.CurrentContract,
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
}

// runEngineRole is the hosted or local engine: it changes into the fixture
// workspace and runs `build` through the real App.Run, so the real
// runcredential.Capture reads the descriptor, denies inspection of this
// process, and drops the framework tokens. With custodyCredentialFDEnv set,
// the run is hosted; without it, local. With custodyCacheEnv set, the build
// cache is on; without it, the build runs with --no-cache. With
// custodyArgsEnv set, it runs that command instead of `build`.
func runEngineRole() int {
	wsRoot := os.Getenv(custodyWorkspaceEnv)
	if err := os.Chdir(wsRoot); err != nil {
		return 1
	}
	args := []string{"build", "--projects", "app"}
	if command := os.Getenv(custodyArgsEnv); command != "" {
		args = strings.Split(command, "\n")
	}
	if os.Getenv(custodyCacheEnv) == "" {
		args = append(args, "--no-cache")
	}
	if fd := os.Getenv(custodyCredentialFDEnv); fd != "" {
		args = append(args, "--credential-fd", fd)
	}
	return (&cli.App{}).Run(context.Background(), args)
}
