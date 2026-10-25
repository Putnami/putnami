// Package removedroots drives the command roots that left the core CLI through
// the real CLI: the argument parser, dispatch discovery, the workspace
// bootstrap and the engine, in a fixture workspace. These tests swap the
// process streams, the working directory and the environment, so they run in
// their own test binary.
package removedroots

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cli"
	"go.putnami.dev/tooling/cli/internal/cli/clitest"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// bootstrappedEnv is the variable the workspace bootstrap sets before it runs
// the implicit install.
const bootstrappedEnv = "PUTNAMI_WORKSPACE_BOOTSTRAPPED"

// noAutoInstallEnv turns the workspace bootstrap off. A CI host may set it, so
// the tests clear it: the bootstrap must be able to start for its absence to
// mean anything.
const noAutoInstallEnv = "PUTNAMI_NO_AUTO_INSTALL"

func TestMain(m *testing.M) {
	os.Exit(clitest.Main(m))
}

// TestRemovedRootsAreRefusedBeforeTheWorkspaceBootstrap holds every former
// spelling of the removed roots, and `putnami help` of each, to one usage
// error, raised before the implicit install and before any planning.
// `proposals finalize` is refused the same way where the workspace binds a
// collaboration contract, which makes proposals a command group. The job
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
		{"change-plan", "--base", "HEAD", "--output=json"},
		{"help", "change-plan"},
		{"infra", "plan"},
		{"infra", "plan", "--output=jsonl"},
		{"help", "infra"},
		{"dev", "extension", "validate"},
		{"dev", "template", "test", "--keep"},
		{"help", "dev"},
		{"contributor", "language", "text"},
		{"contributor", "language", "files", "--", "a.go"},
		{"contributor", "machine-load", "--output=json"},
		{"help", "contributor"},
		{"version"},
		{"version", "get", "--output=json"},
		{"version", "tag", "--scope", "tooling", "--yes"},
		{"version", "--help"},
		{"help", "version"},
		{"help", "version", "tag"},
		{"compose", "app"},
		{"compose", "up", "app", "--no-watch"},
		{"compose", "app", "--port", "4100", "--output=jsonl"},
		{"compose", "--help"},
		{"help", "compose"},
		{"qualify", "app", "--target", "local"},
		{"qualify", "run", "app", "--output=json"},
		{"qualify", "app", "--target", "https://app.example.test"},
		{"help", "qualify"},
		{"tree", "verify"},
		{"tree", "verify", "--record", ".context/dossier.json"},
		{"tree", "verify", "--help"},
		{"help", "tree", "verify"},
		{"proposals", "finalize"},
		{"proposals", "finalize", "--draft", "--base", "main"},
		{"proposals", "finalize", "--help"},
		{"help", "proposals", "finalize"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			requireRefusedBeforeBootstrap(t, clitest.WhereFixture(t, false, false), args)
		})
	}
	for _, args := range [][]string{
		{"proposals", "finalize"},
		{"proposals", "finalize", "--gate", "ci", "--output=json"},
		{"proposals", "finalize", "--help"},
		{"help", "proposals", "finalize"},
	} {
		t.Run(strings.Join(args, " ")+" with a collaboration binding", func(t *testing.T) {
			root := clitest.WhereFixture(t, false, false)
			clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"placement-fixture","includes":["extension","app"],`+
				`"options":{"collaboration":{"proposals":{"provider":"@fixture/absent","version":1}}}}`)
			requireRefusedBeforeBootstrap(t, root, args)
		})
	}

	t.Run("a job command bootstraps the same fixture", func(t *testing.T) {
		t.Setenv(bootstrappedEnv, "")
		t.Setenv(noAutoInstallEnv, "")
		root := clitest.WhereFixture(t, false, false)
		if code, output := clitest.RunGateArgs(t, root, "lint", "--projects", "app", "--no-cache"); code != 0 {
			t.Fatalf("lint exit = %d:\n%s", code, output)
		}
		if os.Getenv(bootstrappedEnv) == "" {
			t.Fatalf("%s is unset after a job command: the refusal's evidence is vacuous", bootstrappedEnv)
		}
	})
}

// requireRefusedBeforeBootstrap runs args in the workspace at root and fails
// unless the CLI refuses them before selection, planning and the implicit
// install.
func requireRefusedBeforeBootstrap(t *testing.T, root string, args []string) {
	t.Helper()
	t.Setenv(bootstrappedEnv, "")
	t.Setenv(noAutoInstallEnv, "")
	code, output := clitest.RunGateArgs(t, root, args...)
	if code != 2 {
		t.Fatalf("exit = %d, want 2:\n%s", code, output)
	}
	requireRefusal(t, removedPath(args), output)
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
		{"change-plan", "--base", "HEAD"},
		{"infra", "plan"},
		{"help", "infra"},
		{"dev", "template", "validate"},
		{"help", "dev"},
		{"contributor", "language", "commits", "HEAD~1..HEAD"},
		{"contributor", "machine-load"},
		{"help", "contributor"},
		{"version", "get"},
		{"help", "version"},
		{"compose", "app", "--no-watch"},
		{"help", "compose"},
		{"qualify", "app", "--target", "local"},
		{"help", "qualify"},
		{"tree", "verify", "--snapshot"},
		{"tree", "verify", "--help"},
		{"help", "tree", "verify"},
		{"proposals", "finalize", "--draft"},
		{"proposals", "finalize", "--help"},
		{"help", "proposals", "finalize"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, output := clitest.RunGateArgs(t, t.TempDir(), args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2:\n%s", code, output)
			}
			requireRefusal(t, removedPath(args), output)
		})
	}
}

// machineLoadShape is the human output of `doctor --machine`: the load, or
// unknown where the host does not report it.
var machineLoadShape = regexp.MustCompile(`^(unknown|\d+\.\d\d)\n$`)

// TestDoctorMachinePrintsTheLoadInEachFormat runs `putnami doctor --machine`,
// which reads the machine load in place of the removed `contributor
// machine-load`, through the real CLI in a workspace. The live load moves
// between two runs, so the test checks the shape of stdout in each format:
// the load line, or the doctor result whose data carries the load. The doctor
// package tests pin the exact bytes for a fixed sample.
func TestDoctorMachinePrintsTheLoadInEachFormat(t *testing.T) {
	t.Setenv(noAutoInstallEnv, "1")
	root := t.TempDir()
	clitest.WriteFile(t, filepath.Join(root, "putnami.workspace.json"), `{"name":"machine-load"}`)
	for _, format := range []string{"", "json"} {
		args := []string{"doctor", "--machine"}
		if format != "" {
			args = append(args, "--output="+format)
		}
		var code int
		stdout, stderr := clitest.CaptureStreams(t, func() {
			app, err := cli.NewApp()
			if err != nil {
				t.Fatalf("NewApp: %v", err)
			}
			t.Chdir(root)
			code = app.Run(context.Background(), args)
		})
		if code != 0 {
			t.Fatalf("%s: exit %d:\n%s%s", strings.Join(args, " "), code, stdout, stderr)
		}
		if format == "" {
			if !machineLoadShape.MatchString(stdout) {
				t.Errorf("human output has the wrong shape: %q", stdout)
			}
			continue
		}
		var result struct {
			Command string `json:"command"`
			Status  string `json:"status"`
			Data    struct {
				Known *bool `json:"known"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(stdout), &result); err != nil {
			t.Fatalf("structured output is not one JSON document: %v\n%s", err, stdout)
		}
		if result.Command != "doctor" || result.Status != "success" || result.Data.Known == nil {
			t.Errorf("structured output does not carry the doctor result and its load: %s", stdout)
		}
	}
}

// removedPath is the command path an invocation names: the root, which is the
// first argument or the command `putnami help` is asked about, or `tree verify`
// or `proposals finalize`, the subcommands that left a command word the core
// still answers.
func removedPath(args []string) string {
	if args[0] == "help" {
		args = args[1:]
	}
	if args[0] == "tree" || args[0] == "proposals" {
		return args[0] + " " + args[1]
	}
	return args[0]
}

// requireRefusal fails unless output carries the refusal of path with the
// reference that lists where it moved, and no remedy that needs a workspace.
func requireRefusal(t *testing.T, path, output string) {
	t.Helper()
	if want := "putnami: `putnami " + path + "` is no longer a core command"; !strings.Contains(output, want) {
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
// a job, and a command group named channel runs its subcommand. A removed
// subcommand stays refused although the extension declares it: a command group
// named proposals does not run finalize.
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
	manifest.Commands["proposals-finalize"] = extensionproto.CommandDefinition{ActivationFiles: []string{"gate.project"}, Run: run}
	manifest.CommandGroups = map[string]extensionproto.CommandGroupDefinition{
		"channel":   {Subcommands: map[string]extensionproto.SubcommandDefinition{"status": {Command: "channel-status"}}},
		"proposals": {Subcommands: map[string]extensionproto.SubcommandDefinition{"finalize": {Command: "proposals-finalize"}}},
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

	for _, args := range [][]string{{"proposals", "finalize", "--projects", "app", "--no-cache"}, {"help", "proposals", "finalize"}} {
		code, output := clitest.RunGateArgs(t, root, args...)
		if code != 2 {
			t.Fatalf("%s exit = %d, want 2 although the extension declares it:\n%s", strings.Join(args, " "), code, output)
		}
		requireRefusal(t, "proposals finalize", output)
	}
	if calls, err := os.ReadFile(filepath.Join(root, "app", "calls")); err != nil || strings.Count(string(calls), "ran\n") != 2 {
		t.Fatalf("calls = %q (%v): the refused proposals finalize ran a task", calls, err)
	}
}
