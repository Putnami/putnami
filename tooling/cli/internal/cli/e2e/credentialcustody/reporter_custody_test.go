package credentialcustody

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// The lines the session reporter of a reporter fixture appends to its log.
const (
	reporterStarted          = "reporter started"
	reporterStartedWithToken = "reporter started with its environment token"
	reporterHeldACredential  = "reporter environment holds the run credential"
	reporterAuthenticated    = "reporter authenticated with the run credential"
	reporterReadChunkFirst   = "reporter read a chunk first"
	reporterRefusedALine     = "reporter refused a line"
	reporterReadTheBearer    = "a v1 reporter read the run credential"
	reporterDelivered        = "reporter received the final events marker"
	// reporterEnvToken is the session reporter token the engine environment
	// exports.
	reporterEnvToken = "reporter-env-token-0d41"
)

// runReporterRole is the session reporter of a reporter fixture: it records
// its start and whether its environment holds a token or the run credential.
// A v2 reporter accepts initialize and records an authenticate that carries
// the run credential; a v1 reporter parses every line as a chunk and exits on
// any other. Both record the first line that is a chunk, acknowledge every
// chunk, and record the events final marker.
func runReporterRole(v1 bool) int {
	log := os.Getenv(custodyReportEnv)
	started := reporterStarted
	if os.Getenv(protocolcli.SessionReporterTokenEnv)+os.Getenv(protocolcli.LogReporterTokenEnv) != "" {
		started = reporterStartedWithToken
	}
	if slices.ContainsFunc(os.Environ(), func(entry string) bool { return strings.Contains(entry, custodyBearer) }) {
		started = reporterHeldACredential
	}
	if err := appendLog(log, started); err != nil {
		return 1
	}
	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 4096), protocolcli.SessionReportingLineBytes)
	first := true
	for in.Scan() {
		line := in.Bytes()
		if v1 && bytes.Contains(line, []byte(custodyBearer)) {
			_ = appendLog(log, reporterReadTheBearer)
		}
		if handshake, err := protocolcli.ParseSessionReportingHandshake(line); err == nil && !v1 {
			result := handshake.Accept()
			if handshake.Op == protocolcli.SessionReportingOpAuthenticate {
				if handshake.RunCredential != custodyBearer {
					result = handshake.Refuse("unauthorized")
				} else if err := appendLog(log, reporterAuthenticated); err != nil {
					return 1
				}
			}
			first = false
			_ = out.Encode(result)
			continue
		}
		chunk, err := protocolcli.ParseSessionReportingChunk(line)
		if err != nil {
			_ = appendLog(log, reporterRefusedALine)
			return 1
		}
		if first {
			first = false
			if err := appendLog(log, reporterReadChunkFirst); err != nil {
				return 1
			}
		}
		_ = out.Encode(chunk.Ack())
		if chunk.Final && chunk.Artifact == "events.jsonl" {
			_ = appendLog(log, reporterDelivered)
			return 0
		}
	}
	return 0
}

// writeReporterFixture writes a workspace whose one extension, admitted into
// the artifact store under home and pinned by the lock, serves its session
// reporter as its native runtime, a copy of this binary in role. The
// extension also declares an onInstall hook, a workspace-install and the
// build, and the workspace a before-hook of the build. Every step appends what
// it did to the returned log.
func writeReporterFixture(t *testing.T, home, role string) (wsRoot, log string) {
	t.Helper()
	wsRoot, log = t.TempDir(), filepath.Join(t.TempDir(), "order.log")
	logStep := func(line string) string {
		return jsonString(fmt.Sprintf("printf '%%s\\n' %s >> %s", shellQuote(line), shellQuote(log)))
	}
	manifest := fmt.Sprintf(`{
  "name": "@fixture/reporter",
  "version": "0.1.0",
  "cliContract": %d,
  "runtime": { "executable": "compiled/runtime" },
  "hooks": {
    "onInstall": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s] }
  },
  "commands": {
    %s: {
      "visibility": "internal",
      "run": [{ "id": "reporter", "task": "reporter-task" }]
    },
    "workspace-install": {
      "description": "Install workspace dependencies.",
      "activation": "workspace",
      "run": [{ "id": "install", "task": "install-task" }]
    },
    "build": {
      "description": "Build.",
      "activationFiles": ["app.project"],
      "run": [{ "id": "build", "task": "build-task" }]
    }
  },
  "tasks": {
    "reporter-task": {
      "kind": "command",
      "command": "{extensionRuntime}",
      "args": [%[3]s],
      "env": { %s: %s, %s: %s }
    },
    "install-task": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s], "cache": false, "timeoutMs": 30000 },
    "build-task": { "kind": "command", "command": "/bin/sh", "args": ["-c", %s], "cache": false, "timeoutMs": 30000 }
  }
}`,
		protocolcli.CurrentContract, logStep(installHookRan), jsonString(protocolcli.SessionReporterCommand),
		jsonString(custodyRoleEnv), jsonString(role), jsonString(custodyReportEnv), jsonString(log),
		logStep(installerRan), logStep(taskRan))

	digest := lockfile.HashBytes([]byte("archive of @fixture/reporter 0.1.0"))
	stored, err := artifactstore.New(artifactDir(home)).Admit(digest, func(stage string) error {
		fixtureproc.Binary(t, filepath.Join(stage, "compiled", "runtime"))
		return os.WriteFile(filepath.Join(stage, "putnami.extension.json"), []byte(manifest), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	lock := lockfile.NewLockFile()
	lock.SetExtension("@fixture/reporter", lockfile.LockEntry{
		Version:      "0.1.0",
		ManifestHash: lockfile.HashBytes([]byte(manifest)),
		Integrities:  map[string]string{lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): digest},
	})
	if err := lockfile.WriteLockFile(wsRoot, lock); err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(wsRoot, "putnami.workspace.json"), fmt.Sprintf(`{
  "name": "reporter-ws",
  "includes": ["app"],
  "extensions": { "@fixture/reporter": "0.1.0" },
  "hooks": { "commands": { "build": { "before": [%s] } } }
}`, logStep(hookRan)))
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "putnami.json"), `{"name":"app","extensions":["@fixture/reporter"]}`)
	clitest.WriteFile(t, filepath.Join(wsRoot, "app", "app.project"), "")
	link := layout.StableDir(wsRoot, layout.Extensions, "@fixture/reporter")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(stored, link); err != nil {
		t.Fatal(err)
	}
	return wsRoot, log
}

// runReporterBuild runs `build` in a reporter fixture whose reporter runs in
// role, with the session reporter selected and its token exported, the
// implicit install on or off, hosted or not. It returns the output and the
// log.
func runReporterBuild(t *testing.T, role string, hosted, install bool) (output string, log []string) {
	t.Helper()
	clitest.RequireShell(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	wsRoot, logPath := writeReporterFixture(t, home, role)
	autoInstall := "PUTNAMI_NO_AUTO_INSTALL=1"
	if install {
		autoInstall = "PUTNAMI_NO_AUTO_INSTALL="
	}
	code, output := runEngine(t, self, wsRoot, home, hosted, autoInstall,
		protocolcli.SessionReporterEnv+"=@fixture/reporter", protocolcli.SessionReporterTokenEnv+"="+reporterEnvToken)
	if code != 0 {
		t.Fatalf("build exit=%d, want 0\n%s", code, output)
	}
	if strings.Contains(output, reporterEnvToken) {
		t.Errorf("the reporter token reached the engine output:\n%s", output)
	}
	return output, readLog(t, logPath)
}

// The engine names the reporter token it ignores on a hosted run.
const removedReporterToken = "putnami: --credential-fd: removed " + protocolcli.SessionReporterTokenEnv +
	" from the environment; the run credential replaces it"

// On a hosted run the session reporter receives the run credential over the
// protocol, never in its environment, though the engine's environment exports
// a reporter token: the engine removes that token and says so. The reporter
// starts before the first repository code, the implicit install's onInstall
// hook or the build's before-hook, authenticates there, and the same process
// then receives the session.
func TestAHostedReporterStartsBeforeRepositoryCodeAndHoldsTheCredential(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "providers-receive-it-over-rpc", "reporters-authenticate-after-initialize")
	spectest.Proves(t, "cli/credential-custody", "hostile-process-finds-nothing", "no-credential-after-repository-code")
	for name, tc := range map[string]struct {
		install bool
		want    []string
	}{
		"implicit install": {install: true, want: []string{reporterStarted, reporterAuthenticated,
			installHookRan, installerRan, hookRan, taskRan, reporterDelivered}},
		"installed workspace": {install: false, want: []string{reporterStarted, reporterAuthenticated,
			hookRan, taskRan, reporterDelivered}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			output, got := runReporterBuild(t, "reporter", true, tc.install)
			if !slices.Equal(got, tc.want) {
				t.Errorf("hosted build log = %q, want %q\n%s", got, tc.want, output)
			}
			if !strings.Contains(output, removedReporterToken) {
				t.Errorf("output lacks %q:\n%s", removedReporterToken, output)
			}
		})
	}
}

// On a hosted run a v1 session reporter, which does not accept initialize,
// never reads the run credential and gets no token: the engine says it starts
// without a credential, and a process started without one delivers the
// session.
func TestAHostedV1ReporterGetsNoCredential(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "providers-receive-it-over-rpc", "a-reporter-that-cannot-hold-it-gets-none")
	output, got := runReporterBuild(t, "reporter-v1", true, false)
	want := []string{reporterStarted, reporterRefusedALine, hookRan, taskRan, reporterStarted, reporterReadChunkFirst, reporterDelivered}
	if !slices.Equal(got, want) {
		t.Errorf("hosted build log = %q, want %q\n%s", got, want, output)
	}
	for _, line := range []string{removedReporterToken,
		"the session reporter of @fixture/reporter cannot hold the run credential: it did not accept initialize of session reporting protocol 2; it starts without a credential"} {
		if !strings.Contains(output, line) {
			t.Errorf("output lacks %q:\n%s", line, output)
		}
	}
}

// Without --credential-fd the session reporter starts as it always did: at its
// first chunk, after the hooks and the task, with its token in its
// environment, and its first line is a chunk.
func TestFlagOffStartsTheReporterAsBefore(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/credential-custody", "flag-off-changes-nothing", "flag-off-reporter-frames-unchanged")
	output, got := runReporterBuild(t, "reporter", false, true)
	want := []string{installHookRan, installerRan, hookRan, taskRan, reporterStartedWithToken, reporterReadChunkFirst, reporterDelivered}
	if !slices.Equal(got, want) {
		t.Errorf("local build log = %q, want %q\n%s", got, want, output)
	}
	if strings.Contains(output, "removed "+protocolcli.SessionReporterTokenEnv) {
		t.Errorf("a local run removed the reporter token:\n%s", output)
	}
}
