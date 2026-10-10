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
//   - On a run that publishes through a publication-v1 credential provider,
//     the publication job, repository code, finds the publish credential
//     nowhere: the engine uploads what the job packed with a credential only
//     it receives.
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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
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
	// custodyHostsEnv is the comma list of hosts the "credential-provider"
	// role's publish credential serves.
	custodyHostsEnv = "PUTNAMI_TEST_CUSTODY_HOSTS"
	// custodyOrderEnv names the log every role of a publication run appends
	// to as it acts, so the test reads one order across processes.
	custodyOrderEnv = "PUTNAMI_TEST_CUSTODY_ORDER"
	// custodyLedgerEnv names the file the "credential-provider" role records
	// the open and release payloads it reads, and its ledger when the session
	// ends, one JSON object per line.
	custodyLedgerEnv = "PUTNAMI_TEST_CUSTODY_LEDGER"
	// custodySetupEnv names the JSON file the "credential-provider" role
	// reads its channel heads and artifact answers from (providerSetup).
	custodySetupEnv = "PUTNAMI_TEST_CUSTODY_SETUP"
	// custodyExecEnv names the program the "exec-probe" role runs once it has
	// probed: a real extension binary.
	custodyExecEnv = "PUTNAMI_TEST_CUSTODY_EXEC"
	// custodyGateEnv is the URL of the uploadGate the "upload-probe" role
	// waits on before it probes, and tells once it has reported.
	custodyGateEnv = "PUTNAMI_TEST_CUSTODY_GATE"
	// custodyProbeLogEnv names the log the workspace probe of a path
	// fixture's path extension appends to (runPathProbeRole).
	custodyProbeLogEnv = "PUTNAMI_TEST_CUSTODY_PROBE_LOG"
)

// TestMain runs the tests, or, when this binary was re-executed in a role, that
// role: the engine that runs `build`, the cache provider, the credential
// provider, the workspace-fetch of a store or a path extension, a hook that
// overwrites the provider's executable, a hostile hook, task or publication
// job that probes for the credential and packs an npm or Put registry member,
// a probe that runs while the engine uploads, a package step that stages a
// member, the bun a real extension packs with, a probe that then runs a real
// extension binary, or a v2 or v1 session reporter. A role process exits
// without running any test. As the
// runtime of a fixture extension, this binary first answers the CLI's
// runtime-info handshake and workspace probe, which inherit the engine's
// environment and so its role.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "__putnami" && os.Args[2] == "runtime-info" {
		fmt.Println(fixtureRuntimeInfo())
		os.Exit(0)
	}
	if wsproto.IsProbeInvocation(os.Args[1:]) {
		os.Exit(runPathProbeRole())
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
	case "path-fetch":
		os.Exit(runPathFetchRole())
	case "tamper":
		os.Exit(runTamperRole())
	case "credential-provider":
		os.Exit(runCredentialProviderRole())
	case "publication":
		os.Exit(runHostilePublicationRole())
	case "put-publication":
		os.Exit(runHostilePutPublicationRole())
	case "upload-probe":
		os.Exit(runUploadProbeRole())
	case "stage":
		os.Exit(runStageRole())
	case "bun":
		os.Exit(runBunRole())
	case "exec-probe":
		os.Exit(runExecProbeRole())
	case "reporter":
		os.Exit(runReporterRole(false))
	case "reporter-v1":
		os.Exit(runReporterRole(true))
	default:
		os.Exit(runHostileRole(role))
	}
}

// fixtureRuntimeInfo is the runtime-info document of the fixture extension
// this binary is the runtime of, on this machine. A copy placed at
// <extension>/compiled/runtime answers with the name and version of the
// manifest at <extension>/putnami.extension.json; any other copy answers as
// @fixture/cache 0.1.0.
func fixtureRuntimeInfo() string {
	name, version := "@fixture/cache", "0.1.0"
	if self, err := os.Executable(); err == nil {
		manifest := filepath.Join(filepath.Dir(filepath.Dir(self)), "putnami.extension.json")
		if data, err := os.ReadFile(manifest); err == nil {
			var identity struct{ Name, Version string }
			if json.Unmarshal(data, &identity) == nil && identity.Name != "" && identity.Version != "" {
				name, version = identity.Name, identity.Version
			}
		}
	}
	return fmt.Sprintf(
		`{"extension":%q,"version":%q,"platform":%q,"cliContract":%d,"runtimeProtocol":%d,"runtimeABI":%d}`,
		name, version, runtime.GOOS+"/"+runtime.GOARCH, protocolcli.CurrentContract,
		runtimeproto.MaxKnownProtocolVersion, runtimeproto.RuntimeABIVersion)
}

// warmRuntime has the host check the runtime copy at path before an engine
// bounds the copy's runtime-info handshake by its deadline
// (fixtureproc.WarmBinary), so the deadline measures the copy alone. Call it
// on the path the engine starts.
func warmRuntime(t *testing.T, path string) {
	t.Helper()
	fixtureproc.WarmBinary(t, path, "__putnami", "runtime-info")
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
