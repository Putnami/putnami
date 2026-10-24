package workspacewarnings

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// jobWorkspace writes a workspace whose one project, example, runs a `build`
// command from a local extension. When declaresJobs is true, example's
// putnami.json declares a `jobs` key, which the load warns about.
func jobWorkspace(t *testing.T, declaresJobs bool) string {
	t.Helper()
	root := t.TempDir()
	project := `{"name":"example","extensions":["@fixture/build"]}`
	if declaresJobs {
		project = `{"name":"example","extensions":["@fixture/build"],"jobs":{}}`
	}
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"job-warnings","includes":["extension","example"]}`)
	clitest.WriteFile(t, filepath.Join(root, "example", "putnami.json"), project)
	clitest.WriteFile(t, filepath.Join(root, "example", "build.project"), "")
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.json"), `{"name":"@fixture/build"}`)
	disabled := false
	manifest := extensionproto.Manifest{
		Name: "@fixture/build", Version: "1.0.0", CLIContract: protocolcli.CurrentContract,
		Commands: map[string]extensionproto.CommandDefinition{"build": {
			ActivationFiles: []string{"build.project"},
			Run:             []extensionproto.PipelineStep{{ID: "build", Task: "build-app"}},
		}},
		Tasks: map[string]extensionproto.TaskDefinition{"build-app": {
			Kind: "command", Command: "/bin/sh", Args: []string{"-c", "exit 0"},
			Cache: &extensionproto.TaskCachePolicy{Enabled: &disabled}, TimeoutMs: -1,
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, filepath.Join(root, "extension", "putnami.extension.json"), string(data))
	return root
}

// assertJobsIgnoredWarning fails unless the envelope carries exactly the
// ignored-jobs warning, with its stable code.
func assertJobsIgnoredWarning(t *testing.T, envelope protocolcli.ResultV2) {
	t.Helper()
	if len(envelope.WorkspaceWarnings) != 1 {
		t.Fatalf("workspaceWarnings = %+v, want the one ignored-jobs warning", envelope.WorkspaceWarnings)
	}
	if code := envelope.WorkspaceWarnings[0].Code; code != string(workspace.WarningCodeJobsIgnored) {
		t.Errorf("code = %q, want %q", code, workspace.WarningCodeJobsIgnored)
	}
}

func TestJobRunEnvelopeCarriesWorkspaceLoadWarnings(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "a-job-run-envelope-carries-the-load-warnings")
	clitest.RequireShell(t)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("DO_NOT_TRACK", "1")

	for _, quiet := range []bool{false, true} {
		name := "default"
		if quiet {
			name = "quiet"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
			root := jobWorkspace(t, true)
			for _, tc := range []struct {
				args []string
				plan bool
			}{
				{[]string{"build", "--projects", "example"}, false},
				{[]string{"build", "--projects", "example", "--plan"}, true},
			} {
				args := append([]string{}, tc.args...)
				args = append(args, "--output=json")
				if quiet {
					args = append(args, "--quiet")
				}
				t.Run(strings.Join(args, " "), func(t *testing.T) {
					code, envelope, stdout, stderr := run(t, root, args...)
					if code != protocolcli.ExitSuccess {
						t.Fatalf("exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
					}
					if tc.plan && (envelope.Plan == nil || envelope.Run != nil) {
						t.Fatalf("--plan must write the plan-only envelope (plan set, no run):\n%s", stdout)
					}
					if !tc.plan && envelope.Run == nil {
						t.Fatalf("a plain run must write a run envelope:\n%s", stdout)
					}
					assertJobsIgnoredWarning(t, envelope)
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

	t.Run("a load with no warning reports none", func(t *testing.T) {
		t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))
		root := jobWorkspace(t, false)
		code, _, stdout, stderr := run(t, root, "build", "--projects", "example", "--output=json")
		if code != protocolcli.ExitSuccess {
			t.Fatalf("exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}
		if strings.Contains(stdout, "workspaceWarnings") || strings.Contains(stderr, "putnami: warning: ") {
			t.Errorf("a load with no warning reported one:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
		}
	})
}

// TestComposeEnvelopeCarriesTheWarningsOfItsOwnLoad holds compose to reporting
// the warnings of the workspace load it performs before it plans its members:
// a compose that fails before any engine run still carries them in its
// envelope and writes them on stderr, once.
func TestComposeEnvelopeCarriesTheWarningsOfItsOwnLoad(t *testing.T) {
	spectest.Proves(t, "cli/workspace-discovery-selection", "project-inventory", "a-command-reports-the-warnings-of-its-own-load-once")
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("DO_NOT_TRACK", "1")
	root := jobWorkspace(t, true)

	code, envelope, stdout, stderr := run(t, root, "compose", "missing", "--output=json")
	if code != protocolcli.ExitUsage || envelope.Status != protocolcli.StatusFailure {
		t.Fatalf("exit code = %d, status %q, want a usage failure\nstdout:\n%s\nstderr:\n%s", code, envelope.Status, stdout, stderr)
	}
	assertJobsIgnoredWarning(t, envelope)
	line := "putnami: warning: " + envelope.WorkspaceWarnings[0].Message
	if count := strings.Count(stderr, line); count != 1 {
		t.Errorf("stderr carries the warning %d times, want once:\n%s", count, stderr)
	}

	code, envelope, stdout, stderr = run(t, root, "compose", "missing", "--output=json", "--quiet")
	if code != protocolcli.ExitUsage {
		t.Fatalf("--quiet exit code = %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	assertJobsIgnoredWarning(t, envelope)
	if strings.Contains(stderr, "putnami: warning: ") {
		t.Errorf("--quiet wrote the warning to stderr:\n%s", stderr)
	}
}
