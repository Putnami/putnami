// Package where drives the --where placement flag through the real CLI: the
// ordinary terminal adapter, discovery, engine, scheduler, subprocess and
// session writer, in a fixture workspace. The parser-level tests of the flag
// stay in internal/cli (where_flag_test.go); these swap the process streams
// and working directory, so they run in their own test binary.
package where

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// This uses the ordinary terminal adapter, discovery, engine, scheduler,
// subprocess and session writer. No runner/cache/auth provider is installed.
func TestWhereNoProviderRunsExactlyOnce(t *testing.T) {
	spectest.Proves(t, "cli/portable-runner", "optional-placement", "absent-provider-runs-once-locally")
	clitest.RequireShell(t)
	for _, where := range []string{"", "local", "remote"} {
		for _, fail := range []bool{false, true} {
			t.Run(where+map[bool]string{true: "-failure", false: "-success"}[fail], func(t *testing.T) {
				root := clitest.WhereFixture(t, fail, false)
				args := []string{"lint,test,build,validate,validate-workspace", "--projects", "app", "--no-cache", "--continue-on-error", "--output=json"}
				if where != "" {
					args = append(args, "--where", where)
				}
				code, output := clitest.RunGateArgs(t, root, args...)
				if (code != 0) != fail {
					t.Fatalf("exit=%d fail=%t: %s", code, fail, output)
				}
				calls, err := os.ReadFile(filepath.Join(root, "app", "calls"))
				if err != nil || strings.Count(string(calls), "ran\n") != 5 {
					t.Fatalf("expected exactly five task executions, got %q (%v)", calls, err)
				}
				paths, err := filepath.Glob(filepath.Join(root, ".putnami", "sessions", "[0-9]*", "session.json"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("expected one canonical session: %v (%v)", paths, err)
				}
				data, err := os.ReadFile(paths[0])
				if err != nil {
					t.Fatal(err)
				}
				if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, data); len(violations) != 0 {
					t.Fatalf("invalid canonical session: %v", violations)
				}
				var session protocolcli.SessionFile
				if err := json.Unmarshal(data, &session); err != nil {
					t.Fatal(err)
				}
				requested := where
				if requested == "" {
					requested = "local"
				}
				if session.Placement == nil || session.Placement.Requested != requested || session.Placement.Actual != "local" {
					t.Fatalf("placement=%+v, want requested %s actual local", session.Placement, requested)
				}
				if len(session.Tasks) != 5 || (session.Run.ExitCode != 0) != fail {
					t.Fatalf("session lost tasks or verdict: %+v", session.Run)
				}
			})
		}
	}
}

// An installed provider is never a fallback: when the source cannot be
// captured (here: the workspace is not a repository), the run fails with the
// precise reason, executes no task locally and records no session.
func TestWhereInstalledProviderFailsBeforeRunningAnyTask(t *testing.T) {
	root := clitest.WhereFixture(t, false, true)
	code, output := clitest.RunGateArgs(t, root, "build", "--projects", "app", "--where", "remote")
	if code != cli.ExitError || !strings.Contains(output, "remote execution: capture source") {
		t.Fatalf("installed provider over an uncapturable tree: exit %d: %s", code, output)
	}
	if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
		t.Fatalf("a task ran locally after a failed remote submission: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("a failed submission recorded a local session: %v", err)
	}
}

func TestWhereAbsenceDoesNotBypassRequiredTaskExtension(t *testing.T) {
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"missing-language","includes":["app"],"extensions":["@fixture/missing-language"]}`)
	clitest.WriteFile(t, filepath.Join(root, "app", "putnami.json"), `{"name":"app"}`)
	code, output := clitest.RunGateArgs(t, root, "build", "--projects", "app", "--where", "remote")
	if code == 0 || !strings.Contains(output, "@fixture/missing-language") {
		t.Fatalf("missing task extension incorrectly succeeded: %d %s", code, output)
	}
}
