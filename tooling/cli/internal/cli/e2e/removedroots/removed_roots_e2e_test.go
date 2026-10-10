// Package removedroots drives the command roots that left the core CLI through
// the real CLI: the argument parser, dispatch discovery, the workspace
// bootstrap and the engine, in a fixture workspace. These tests swap the
// process streams, the working directory and the environment, so they run in
// their own test binary.
package removedroots

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// bootstrappedEnv is the variable the workspace bootstrap sets before it runs
// the implicit install.
const bootstrappedEnv = "PUTNAMI_WORKSPACE_BOOTSTRAPPED"

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// TestRemovedRootsAreRefusedBeforeTheWorkspaceBootstrap holds every former
// spelling of the removed roots, and `putnami help` of each, to one usage
// error, raised before the implicit install and before any planning. The job
// command run last proves the bootstrap does start in the same fixture, so its
// absence is evidence.
func TestRemovedRootsAreRefusedBeforeTheWorkspaceBootstrap(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "command-catalog", "a-root-that-left-the-core-is-refused-unless-an-extension-declares-it")
	clitest.RequireShell(t)
	for _, args := range [][]string{
		{"channel", "status", "latest"},
		{"channel", "set", "latest", "--from", "canary"},
		{"ci"},
		{"ci", "validate"},
		{"ci", "init", "--force", "--output=json"},
		{"ci", "--help"},
		{"help", "ci"},
		{"help", "ci", "validate"},
		{"help", "channel"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Setenv(bootstrappedEnv, "")
			root := clitest.WhereFixture(t, false, false)
			code, output := clitest.RunGateArgs(t, root, args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2:\n%s", code, output)
			}
			requireRefusal(t, removedRoot(args), output)
			if strings.Contains(output, "no project matched") || strings.Contains(output, "no job matched") {
				t.Fatalf("the root reached selection or planning:\n%s", output)
			}
			if got := os.Getenv(bootstrappedEnv); got != "" {
				t.Fatalf("%s = %q: the workspace bootstrap started", bootstrappedEnv, got)
			}
			if _, err := os.Stat(filepath.Join(root, ".putnami", "install-state.json")); !os.IsNotExist(err) {
				t.Fatalf("the implicit install recorded its state: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "app", "calls")); !os.IsNotExist(err) {
				t.Fatalf("a task ran: %v", err)
			}
		})
	}

	t.Run("a job command bootstraps the same fixture", func(t *testing.T) {
		t.Setenv(bootstrappedEnv, "")
		root := clitest.WhereFixture(t, false, false)
		if code, output := clitest.RunGateArgs(t, root, "lint", "--projects", "app", "--no-cache"); code != 0 {
			t.Fatalf("lint exit = %d:\n%s", code, output)
		}
		if os.Getenv(bootstrappedEnv) == "" {
			t.Fatalf("%s is unset after a job command: the refusal's evidence is vacuous", bootstrappedEnv)
		}
	})
}

// TestRemovedRootsAreRefusedOutsideAWorkspace holds the refusal, and the
// help of a removed root, to the same message outside any workspace, where an
// empty user scope declares nothing.
func TestRemovedRootsAreRefusedOutsideAWorkspace(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "command-catalog", "a-root-that-left-the-core-is-refused-unless-an-extension-declares-it")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(extension.UserScopeRepairEnv, "")
	t.Setenv(extension.PrivatePutRegistryURLEnv, "")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(home, "artifacts"))
	for _, args := range [][]string{
		{"ci", "validate"},
		{"channel", "status", "latest"},
		{"help", "ci"},
		{"help", "channel"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, output := clitest.RunGateArgs(t, t.TempDir(), args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2:\n%s", code, output)
			}
			requireRefusal(t, removedRoot(args), output)
		})
	}
}

// removedRoot is the root an invocation names: the first argument, or the
// command `putnami help` is asked about.
func removedRoot(args []string) string {
	if args[0] == "help" {
		return args[1]
	}
	return args[0]
}

// requireRefusal fails unless output carries the refusal of root with the
// reference that lists where it moved, and no remedy that needs a workspace.
func requireRefusal(t *testing.T, root, output string) {
	t.Helper()
	if want := "putnami: `putnami " + root + "` is no longer a core command"; !strings.Contains(output, want) {
		t.Fatalf("output does not carry %q:\n%s", want, output)
	}
	if !strings.Contains(output, commandmeta.CommandsThatLeftTheCoreURL) {
		t.Fatalf("output does not cite %s:\n%s", commandmeta.CommandsThatLeftTheCoreURL, output)
	}
	if strings.Contains(output, "extensions list") {
		t.Fatalf("output points at a command that lists only installed extensions:\n%s", output)
	}
}

// TestAnExtensionThatDeclaresARemovedRootServesIt keeps dispatch unchanged for
// a workspace whose extension declares the exact root: a job named ci runs as
// a job, and a command group named channel runs its subcommand.
func TestAnExtensionThatDeclaresARemovedRootServesIt(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "command-catalog", "a-root-that-left-the-core-is-refused-unless-an-extension-declares-it")
	clitest.RequireShell(t)
	root := clitest.WhereFixture(t, false, false)
	manifestPath := filepath.Join(root, "extension", "putnami.extension.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest extensionproto.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	run := []extensionproto.PipelineStep{{ID: "check", Task: "check"}}
	manifest.Commands["ci"] = extensionproto.CommandDefinition{ActivationFiles: []string{"gate.project"}, Run: run}
	manifest.Commands["channel-status"] = extensionproto.CommandDefinition{ActivationFiles: []string{"gate.project"}, Run: run}
	manifest.CommandGroups = map[string]extensionproto.CommandGroupDefinition{
		"channel": {Subcommands: map[string]extensionproto.SubcommandDefinition{"status": {Command: "channel-status"}}},
	}
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	clitest.WriteFile(t, manifestPath, string(data))

	for runs, args := range [][]string{
		{"ci", "--projects", "app", "--no-cache"},
		{"channel", "status", "--projects", "app", "--no-cache"},
	} {
		code, output := clitest.RunGateArgs(t, root, args...)
		if code != 0 {
			t.Fatalf("%s exit = %d:\n%s", strings.Join(args, " "), code, output)
		}
		if strings.Contains(output, "is no longer a core command") {
			t.Fatalf("%s was refused although the extension declares it:\n%s", strings.Join(args, " "), output)
		}
		calls, err := os.ReadFile(filepath.Join(root, "app", "calls"))
		if err != nil || strings.Count(string(calls), "ran\n") != runs+1 {
			t.Fatalf("%s: calls = %q (%v), want %d task executions", strings.Join(args, " "), calls, err, runs+1)
		}
	}

	for _, args := range [][]string{{"help", "ci"}, {"help", "channel"}} {
		code, output := clitest.RunGateArgs(t, root, args...)
		if code != 0 || strings.Contains(output, "is no longer a core command") {
			t.Fatalf("%s exit = %d, want 0 and no refusal, because the extension declares the root:\n%s", strings.Join(args, " "), code, output)
		}
	}
}
