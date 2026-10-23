// Package workspacewarnings drives the structured workspace-reading commands
// through the real CLI on a workspace with no index: each one reports the load
// warning on stderr and in its result envelope's workspaceWarnings. A command
// that runs jobs does the same on a workspace that loads with a warning. These
// tests swap the process streams and the working directory, so they run in
// their own test binary.
package workspacewarnings

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// run invokes the CLI in root and returns its exit code, its stdout decoded as
// the one result envelope it must be, and its stderr.
func run(t *testing.T, root string, args ...string) (int, protocolcli.ResultV2, string, string) {
	t.Helper()
	var code int
	stdout, stderr := clitest.CaptureStreams(t, func() {
		app, err := cli.NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(root)
		code = app.Run(context.Background(), args)
	})
	if violations := protocolcli.ValidateDocument(protocolcli.DocumentResultEnvelope, []byte(stdout)); len(violations) != 0 {
		t.Fatalf("%v: the envelope violates the contract: %v\n%s", args, violations, stdout)
	}
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("%v: stdout is not one result envelope: %v\n%s", args, err, stdout)
	}
	return code, envelope, stdout, stderr
}

// assertProviderViewWarning fails unless the envelope carries exactly the
// provider-view warning, with its stable code.
func assertProviderViewWarning(t *testing.T, envelope protocolcli.ResultV2) {
	t.Helper()
	if len(envelope.WorkspaceWarnings) != 1 {
		t.Fatalf("workspaceWarnings = %+v, want the one provider-view warning", envelope.WorkspaceWarnings)
	}
	warning := envelope.WorkspaceWarnings[0]
	if warning.Code != string(workspace.WarningCodeProviderViewUnavailable) {
		t.Errorf("code = %q, want %q", warning.Code, workspace.WarningCodeProviderViewUnavailable)
	}
	if !strings.Contains(warning.Message, workspace.WorkspaceIndexFilename) {
		t.Errorf("message does not name the index file: %q", warning.Message)
	}
}

func TestStructuredCommandsCarryWorkspaceLoadWarnings(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "load-warnings-reach-stderr-and-the-result-envelope")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")

	for _, quiet := range []bool{false, true} {
		name := "default"
		if quiet {
			name = "quiet"
		}
		t.Run(name, func(t *testing.T) {
			root := sharedtest.UnindexedWorkspace(t)
			for _, args := range [][]string{
				{"projects", "list"},
				{"projects", "describe", "/web"},
				{"scopes", "list"},
				{"workspace", "describe"},
				{"projects", "describe", "/missing"},
			} {
				args = append(args, "--output=json")
				if quiet {
					args = append(args, "--quiet")
				}
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					code, envelope, _, stderr := run(t, root, args...)
					missing := args[2] == "/missing"
					if missing != (code != protocolcli.ExitSuccess) {
						t.Fatalf("exit code = %d (status %q)", code, envelope.Status)
					}
					assertProviderViewWarning(t, envelope)
					warning := envelope.WorkspaceWarnings[0].Message
					if quiet && strings.Contains(stderr, "putnami: warning: ") {
						t.Errorf("--quiet wrote the warning to stderr:\n%s", stderr)
					}
					if !quiet && !strings.Contains(stderr, "putnami: warning: "+warning) {
						t.Errorf("stderr does not carry the envelope's warning %q:\n%s", warning, stderr)
					}
				})
			}
		})
	}

	t.Run("projects list keeps its data payload", func(t *testing.T) {
		root := sharedtest.UnindexedWorkspace(t)
		_, _, stdout, _ := run(t, root, "projects", "list", "--output=json")
		var document struct {
			Data struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &document); err != nil || document.Data.ID != "/web" {
			t.Errorf("data does not hold the one project row (%v):\n%s", err, stdout)
		}
	})

	t.Run("an indexed workspace reports no warning", func(t *testing.T) {
		root := sharedtest.UnindexedWorkspace(t)
		sharedtest.RecordWorkspaceIndex(t, root)
		code, _, stdout, stderr := run(t, root, "projects", "list", "--output=json")
		if code != protocolcli.ExitSuccess {
			t.Fatalf("exit code = %d\n%s", code, stdout)
		}
		if strings.Contains(stdout, "workspaceWarnings") || strings.Contains(stderr, "putnami: warning: ") {
			t.Errorf("a load with no warning reported one:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
	})
}
