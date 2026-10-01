package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

func TestAppRun_InternalReleaseSetProviderCapabilityIsBoundToResolvedProvider(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	const token = "nested-release-set-provider-capability"
	wsRoot := t.TempDir()
	observation := filepath.Join(wsRoot, "capability-observations")
	hookObservation := filepath.Join(wsRoot, "stale-bootstrap-hook")
	writeReleaseSetProviderCapabilityFixture(t, wsRoot, observation, hookObservation)
	requestPath := filepath.Join(wsRoot, "resolve-request.json")
	if err := os.WriteFile(requestPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	publicationsPath := filepath.Join(wsRoot, "release-set-publications.json")
	if err := os.WriteFile(publicationsPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The parent coordinator resolved this exact reserved provider before it
	// launched the nested CLI. The workspace alias is deliberately hostile: the
	// internal invocation must parse the fixed provider argv without repo aliases.
	t.Setenv(jobs.InternalReleaseSetProviderCapabilityEnv,
		`{"extensionName":"@local/release-set-provider","version":"0.1.0","command":"cloud-release-set"}`)
	t.Setenv(extensionproto.CloudTokenEnv, token)
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, "")
	t.Setenv(runtimeproto.ReleaseSetPublishedImagesFileEnv, publicationsPath)
	t.Setenv("PUTNAMI_WORKSPACE_BOOTSTRAPPED", "")
	t.Setenv("PUTNAMI_ARTIFACTS_ENSURED", "")
	t.Chdir(wsRoot)

	var code int
	output := captureStdoutStderr(t, func() {
		code = (&App{}).Run(context.Background(), []string{
			distribution.CloudCommand,
			distribution.ReleaseSetCommand,
			distribution.ResolveCommand,
			distribution.RequestFileFlag,
			requestPath,
		})
	})
	if code != ExitSuccess {
		t.Fatalf("internal release-set provider exit = %d, want %d\n%s", code, ExitSuccess, output)
	}

	observed, err := os.ReadFile(observation)
	if err != nil {
		t.Fatalf("read provider observation: %v\n%s", err, output)
	}
	if got, want := strings.TrimSpace(string(observed)), "provider|"+token+"|||"+publicationsPath; got != want {
		t.Fatalf("capability observations = %q, want only %q; alias/install/ordinary work must not receive the bearer", got, want)
	}
	if _, err := os.Stat(hookObservation); !os.IsNotExist(err) {
		data, _ := os.ReadFile(hookObservation)
		t.Fatalf("stale bootstrap hook ran during internal provider dispatch: %q (err=%v)", data, err)
	}
	for _, name := range []string{
		jobs.InternalReleaseSetProviderCapabilityEnv,
		extensionproto.CloudTokenEnv,
		extensionproto.CloudCapabilityAfterEnv,
		runtimeproto.ReleaseSetPublishedImagesFileEnv,
	} {
		if _, present := os.LookupEnv(name); present {
			t.Fatalf("internal provider dispatch retained %s in the process environment", name)
		}
	}
}

func writeReleaseSetProviderCapabilityFixture(t *testing.T, wsRoot, observation, hookObservation string) {
	t.Helper()
	workspaceManifest := `{
  "name": "provider-capability-fixture",
  "includes": ["provider"],
  "extensions": ["/provider"],
  "aliases": { "cloud": "hostile" }
}`
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.workspace.json"), []byte(workspaceManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	extensionRoot := filepath.Join(wsRoot, "provider")
	if err := os.MkdirAll(extensionRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extensionRoot, "putnami.json"), []byte(`{"name":"@local/release-set-provider"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	script := fmt.Sprintf(`#!/bin/sh
set -eu
mode="$1"
shift
printf '%%s|%%s|%%s|%%s|%%s\n' "$mode" "${PUTNAMI_CLOUD_TOKEN-}" "${PUTNAMI_INTERNAL_RELEASE_SET_PROVIDER_CAPABILITY-}" "${PUTNAMI_CLOUD_CAPABILITY_AFTER-}" "${PUTNAMI_RELEASE_SET_PUBLICATIONS_FILE-}" >> %q
if [ "$mode" = provider ]; then
  printf '{}\n'
fi
`, observation)
	if err := os.WriteFile(filepath.Join(extensionRoot, "observe.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	hookScript := fmt.Sprintf(`#!/bin/sh
set -eu
printf 'hook|%%s|%%s|%%s|%%s\n' "${PUTNAMI_CLOUD_TOKEN-}" "${PUTNAMI_INTERNAL_RELEASE_SET_PROVIDER_CAPABILITY-}" "${PUTNAMI_CLOUD_CAPABILITY_AFTER-}" "${PUTNAMI_RELEASE_SET_PUBLICATIONS_FILE-}" > %q
`, hookObservation)
	if err := os.WriteFile(filepath.Join(extensionRoot, "on-install.sh"), []byte(hookScript), 0o755); err != nil {
		t.Fatal(err)
	}

	manifest := `{
  "name": "@local/release-set-provider",
  "version": "0.1.0",
  "cliContract": 4,
  "hooks": {
    "onInstall": {
      "kind": "command",
      "command": "{extensionRoot}/on-install.sh"
    }
  },
  "commandGroups": {
    "cloud": {
      "subcommands": {
        "release-set": { "command": "cloud-release-set", "interactive": true }
      }
    },
    "hostile": {
      "subcommands": {
        "release-set": { "command": "hostile-release-set", "interactive": true }
      }
    }
  },
  "commands": {
    "cloud-release-set": {
      "run": [{ "id": "provider", "task": "provider" }]
    },
    "hostile-release-set": {
      "run": [{ "id": "hostile", "task": "hostile" }]
    }
  },
  "tasks": {
    "provider": {
      "kind": "command",
      "command": "{extensionRoot}/observe.sh",
      "args": ["provider"],
      "cache": false
    },
    "hostile": {
      "kind": "command",
      "command": "{extensionRoot}/observe.sh",
      "args": ["hostile"],
      "cache": false
    }
  }
}`
	if err := os.WriteFile(filepath.Join(extensionRoot, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAppRun_ExtensionStructuredCommand_Interactive verifies the end-to-end
// path that the brief defines as the regression to prevent: parsing
// "putnami demo login" as a structured extension command, dispatching to the
// resolved task with stdio inherited, and producing no live-renderer chrome
// (no spinner, no "1 succeeded" summary, no target headers).
//
// The test wires a fixture extension into a temp workspace whose binary is a
// shell script that prints to stdout/stderr and exits cleanly.
func TestAppRun_ExtensionStructuredCommand_Interactive(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	output := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		code := app.Run(context.Background(), []string{"demo", "login"})
		if code != ExitSuccess {
			t.Fatalf("demo login exit = %d, want %d", code, ExitSuccess)
		}
	})

	// Fixture's stdout must be visible to the user.
	if !strings.Contains(output, "demo-login-stdout-marker") {
		t.Errorf("expected fixture stdout in output, got:\n%s", output)
	}

	// No live-renderer chrome should appear for an interactive command.
	mustNotContain(t, output, "succeeded")
	mustNotContain(t, output, "demo-login(@putnami/demo-extension)")
	for _, frame := range []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"} {
		mustNotContain(t, output, frame)
	}
}

// TestAppRun_ExtensionStructuredCommand_InheritsGroupFlagDefault verifies the
// runtime counterpart of help/completion inheritance: a group-level flag default
// (env=prod) reaches the params a subcommand's job receives, exactly like the
// flat command's own flag defaults do. The interactive login command does not
// declare env itself, so seeing it in the job context proves the group flag was
// inherited into the effective flag surface used for param resolution — not just
// rendered in help.
func TestAppRun_ExtensionStructuredCommand_InheritsGroupFlagDefault(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	output := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		if code := app.Run(context.Background(), []string{"demo", "login"}); code != ExitSuccess {
			t.Fatalf("demo login exit = %d, want %d", code, ExitSuccess)
		}
	})

	if !strings.Contains(output, "env-default-marker") {
		t.Errorf("group flag default did not reach the subcommand's params:\n%s", output)
	}
}

func TestAppRun_ExtensionStructuredCommand_InteractiveForwardsTailArgs(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	script := `#!/bin/sh
printf 'argv:%s\n' "$*"
case "$*" in
  *"ok tenant-1 --flag value --putnamiContext "*) exit 0 ;;
  *) exit 9 ;;
esac
`
	if err := os.WriteFile(filepath.Join(wsRoot, "demo-extension", "fixture.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}

	output := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		code := app.Run(context.Background(), []string{"demo", "login", "tenant-1", "--flag", "value"})
		if code != ExitSuccess {
			t.Fatalf("demo login exit = %d, want %d", code, ExitSuccess)
		}
	})

	if !strings.Contains(output, "argv:ok tenant-1 --flag value --putnamiContext ") {
		t.Fatalf("tail args were not forwarded to interactive extension command:\n%s", output)
	}
}

// TestAppRun_ExtensionStructuredCommand_PropagatesExitCode confirms that an
// interactive subcommand failing with a non-zero exit code surfaces that exit
// code (or the generic failure code) to the caller.
func TestAppRun_ExtensionStructuredCommand_PropagatesExitCode(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Chdir(wsRoot)
	code := app.Run(context.Background(), []string{"demo", "fail"})
	if code == ExitSuccess {
		t.Fatalf("expected non-zero exit code for failing subcommand, got %d", code)
	}
}

// TestAppRun_ExtensionStructuredCommand_UnknownSubcommand rejects an unknown
// subcommand under a known group with a clear error and a non-zero exit.
func TestAppRun_ExtensionStructuredCommand_UnknownSubcommand(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Chdir(wsRoot)
	code := app.Run(context.Background(), []string{"demo", "nonexistent"})
	if code != ExitError {
		t.Errorf("unknown subcommand exit = %d, want %d", code, ExitError)
	}
}

// TestAppRun_ExtensionSubcommandHelp_RendersManifest verifies that
// `putnami demo login --help` surfaces the subcommand's description, its flat
// command's declared flag, and its runnable example plus gloss — the per-
// subcommand help the flat group listing never rendered.
func TestAppRun_ExtensionSubcommandHelp_RendersManifest(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	output := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		if code := app.Run(context.Background(), []string{"demo", "login", "--help"}); code != ExitSuccess {
			t.Fatalf("demo login --help exit = %d, want %d", code, ExitSuccess)
		}
	})

	for _, want := range []string{
		"Authenticate with the demo backend.", // subcommand description
		"--token",                             // flat command flag name
		"Auth token to use.",                  // flag description
		"putnami demo login tenant-1",         // example command line
		"Log in to tenant-1.",                 // example gloss
	} {
		if !strings.Contains(output, want) {
			t.Errorf("login --help missing %q\nfull output:\n%s", want, output)
		}
	}
}

// TestAppRun_ExtensionSubcommandHelp_DiffersPerSubcommand kills the byte-
// identical regression: two subcommands' --help used to produce the same
// flat group listing. Their outputs must now differ.
func TestAppRun_ExtensionSubcommandHelp_DiffersPerSubcommand(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	run := func(sub string) string {
		return captureStdoutStderr(t, func() {
			app, err := NewApp()
			if err != nil {
				t.Fatalf("NewApp: %v", err)
			}
			t.Chdir(wsRoot)
			if code := app.Run(context.Background(), []string{"demo", sub, "--help"}); code != ExitSuccess {
				t.Fatalf("demo %s --help exit = %d, want %d", sub, code, ExitSuccess)
			}
		})
	}

	loginHelp := run("login")
	statusHelp := run("status")
	if loginHelp == statusHelp {
		t.Fatalf("login and status --help are byte-identical; per-subcommand help did not render:\n%s", loginHelp)
	}
	if !strings.Contains(statusHelp, "Show the demo deployment status.") {
		t.Errorf("status --help missing its description:\n%s", statusHelp)
	}
}

// TestAppRun_ExtensionSubcommandHelp_InheritsGroupFlag verifies group-level
// shared flags cascade into every subcommand's --help, and that a subcommand
// redeclaring the same flag wins in that subcommand's help (command < group <
// subcommand precedence, identical to completion and the runtime param path).
func TestAppRun_ExtensionSubcommandHelp_InheritsGroupFlag(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	run := func(sub string) string {
		return captureStdoutStderr(t, func() {
			app, err := NewApp()
			if err != nil {
				t.Fatalf("NewApp: %v", err)
			}
			t.Chdir(wsRoot)
			if code := app.Run(context.Background(), []string{"demo", sub, "--help"}); code != ExitSuccess {
				t.Fatalf("demo %s --help exit = %d, want %d", sub, code, ExitSuccess)
			}
		})
	}

	// login does not redeclare env, so it renders the group-inherited flag with
	// the group's default and description.
	loginHelp := run("login")
	for _, want := range []string{
		"--env",
		"Shared group environment flag.",
		"(default: prod)",
	} {
		if !strings.Contains(loginHelp, want) {
			t.Errorf("login --help missing inherited group flag %q\nfull output:\n%s", want, loginHelp)
		}
	}

	// status redeclares env, so its own definition wins over the group's.
	statusHelp := run("status")
	for _, want := range []string{
		"--env",
		"Status-specific environment override.",
		"(default: staging)",
	} {
		if !strings.Contains(statusHelp, want) {
			t.Errorf("status --help missing subcommand override %q\nfull output:\n%s", want, statusHelp)
		}
	}
	// The overridden subcommand help must not carry the group's values.
	if strings.Contains(statusHelp, "Shared group environment flag.") {
		t.Errorf("status --help leaked the group flag description despite override:\n%s", statusHelp)
	}
}

// TestAppRun_ExtensionHelpAlias exercises `putnami demo help` (group listing)
// and `putnami demo help login` (per-subcommand help) alias paths.
func TestAppRun_ExtensionHelpAlias(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	groupHelp := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		if code := app.Run(context.Background(), []string{"demo", "help"}); code != ExitSuccess {
			t.Fatalf("demo help exit = %d, want %d", code, ExitSuccess)
		}
	})
	if !strings.Contains(groupHelp, "Subcommands:") || !strings.Contains(groupHelp, "login") {
		t.Errorf("demo help did not render group listing:\n%s", groupHelp)
	}

	subHelp := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		if code := app.Run(context.Background(), []string{"demo", "help", "login"}); code != ExitSuccess {
			t.Fatalf("demo help login exit = %d, want %d", code, ExitSuccess)
		}
	})
	if !strings.Contains(subHelp, "Authenticate with the demo backend.") {
		t.Errorf("demo help login did not render per-subcommand help:\n%s", subHelp)
	}
}

// runHelp runs an argv against a fresh app in the fixture workspace, capturing
// only stdout so the machine-help stream stays parseable.
func runHelp(t *testing.T, wsRoot string, argv ...string) string {
	t.Helper()
	return captureStdout(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		if code := app.Run(context.Background(), argv); code != ExitSuccess {
			t.Fatalf("%v exit = %d, want %d", argv, code, ExitSuccess)
		}
	})
}

// TestAppRun_ExtensionSubcommandHelp_OutputJSONL is the core contract: an agent must
// read a subcommand's flags and examples from ONE stable JSON object instead of
// scraping the human help text.
func TestAppRun_ExtensionSubcommandHelp_OutputJSONL(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	out := runHelp(t, wsRoot, "demo", "login", "--help", "--output=jsonl")

	// jsonl is one compact object per line: a single subcommand yields one line.
	if trimmed := strings.TrimSpace(out); strings.Contains(trimmed, "\n") {
		t.Fatalf("--output=jsonl subcommand help is not a single line:\n%s", out)
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("parse jsonl help: %v\noutput: %s", err, out)
	}

	if got := obj["command"]; got != "demo login" {
		t.Errorf("command = %v, want %q", got, "demo login")
	}
	if got := obj["description"]; got != "Authenticate with the demo backend." {
		t.Errorf("description = %v, want the subcommand description", got)
	}

	tokenFlag := findMap(t, obj["flags"], "name", "token")
	if tokenFlag["short"] != "t" || tokenFlag["type"] != "string" || tokenFlag["description"] != "Auth token to use." {
		t.Errorf("token flag missing contract fields: %#v", tokenFlag)
	}

	example := findMap(t, obj["examples"], "command", "putnami demo login tenant-1")
	if example["description"] != "Log in to tenant-1." {
		t.Errorf("example description = %v, want the example gloss", example["description"])
	}
}

// TestAppRun_ExtensionSubcommandHelp_OutputJSON checks that --output=json emits
// the same object, pretty-printed as one indented document.
func TestAppRun_ExtensionSubcommandHelp_OutputJSON(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	out := runHelp(t, wsRoot, "demo", "login", "--help", "--output=json")

	if !strings.Contains(out, "\n  \"command\": \"demo login\"") {
		t.Errorf("--output=json is not indented:\n%s", out)
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("parse json help: %v\noutput: %s", err, out)
	}
	if obj["command"] != "demo login" {
		t.Errorf("command = %v, want %q", obj["command"], "demo login")
	}
	// flags/examples are always present arrays, never null, so agents can index.
	if _, ok := obj["flags"].([]any); !ok {
		t.Errorf("flags is not a JSON array: %#v", obj["flags"])
	}
	if _, ok := obj["examples"].([]any); !ok {
		t.Errorf("examples is not a JSON array: %#v", obj["examples"])
	}
}

// TestAppRun_ExtensionGroupHelp_OutputJSONL checks the group catalog: an agent
// enumerates the whole surface (one summary object per line) in a single call.
func TestAppRun_ExtensionGroupHelp_OutputJSONL(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	out := runHelp(t, wsRoot, "demo", "--help", "--output=jsonl")

	commands := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var summary map[string]any
		if err := json.Unmarshal([]byte(line), &summary); err != nil {
			t.Fatalf("parse group jsonl line %q: %v", line, err)
		}
		cmd, _ := summary["command"].(string)
		desc, _ := summary["description"].(string)
		commands[cmd] = desc
	}

	for _, want := range []string{"demo login", "demo status", "demo fail"} {
		if _, ok := commands[want]; !ok {
			t.Errorf("group catalog missing %q; got %v", want, commands)
		}
	}
	// "fail" declares no subcommand description; it must fall back to the flat
	// command target's description rather than emit an empty string.
	if commands["demo fail"] != "Demo failure fixture." {
		t.Errorf("demo fail description = %q, want the command-target fallback", commands["demo fail"])
	}
}

// TestAppRun_ExtensionGroupHelp_OutputJSON checks that --output=json emits the
// group catalog as a single JSON array.
func TestAppRun_ExtensionGroupHelp_OutputJSON(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	out := runHelp(t, wsRoot, "demo", "--help", "--output=json")

	var arr []map[string]any
	if err := json.Unmarshal([]byte(out), &arr); err != nil {
		t.Fatalf("parse json group help: %v\noutput: %s", err, out)
	}
	if len(arr) != 3 {
		t.Fatalf("group catalog length = %d, want 3\noutput: %s", len(arr), out)
	}
	if arr[0]["command"] == nil {
		t.Errorf("group catalog entry missing command: %#v", arr[0])
	}
}

// TestAppRun_ExtensionHelpAlias_OutputJSONL covers the `putnami <group> help
// <sub>` alias path (routed through runExtensionStructuredCommand, not the
// --help flag) under the machine format.
func TestAppRun_ExtensionHelpAlias_OutputJSONL(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	out := runHelp(t, wsRoot, "demo", "help", "login", "--output=jsonl")

	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("parse alias jsonl help: %v\noutput: %s", err, out)
	}
	if obj["command"] != "demo login" {
		t.Errorf("command = %v, want %q", obj["command"], "demo login")
	}
}

// findMap returns the first map in a JSON array whose string field `key` equals
// `want`, failing the test if none matches.
func findMap(t *testing.T, arr any, key, want string) map[string]any {
	t.Helper()
	items, ok := arr.([]any)
	if !ok {
		t.Fatalf("value is not a JSON array: %#v", arr)
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if s, _ := m[key].(string); s == want {
			return m
		}
	}
	t.Fatalf("no array entry with %s=%q; got %#v", key, want, arr)
	return nil
}

func TestAppRun_ExtensionStructuredCommand_NonInteractiveAliasUsesPlanner(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)

	output := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		code := app.Run(context.Background(), []string{"demo", "deploy", "app", "--plan"})
		if code != ExitSuccess {
			t.Fatalf("demo deploy --plan exit = %d, want %d", code, ExitSuccess)
		}
	})

	if !strings.Contains(output, "  app\n") {
		t.Fatalf("expected app project in plan, got:\n%s", output)
	}
	if !strings.Contains(output, "deploy~deploy") {
		t.Fatalf("expected deploy step in plan, got:\n%s", output)
	}
	if strings.Contains(output, "demo-extension") {
		t.Fatalf("alias positional project should select only app, got:\n%s", output)
	}
}

// TestAppRun_ExtensionStructuredCommand_PropagatesDryRunParam also pins
// engine.Request.ExecutesUnderDryRun: an extension command group
// declares its OWN dry-run flag, the CLI forwards the resolved value as a job
// param, and the job must therefore RUN. The exit code alone cannot prove that —
// the engine's plan-only dry-run preview also exits 0 — so the fixture records
// that it executed and the assertion reads the record.
func TestAppRun_ExtensionStructuredCommand_PropagatesDryRunParam(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)
	ran := filepath.Join(t.TempDir(), "ran")
	writeAliasFixtureScript(t, wsRoot, fmt.Sprintf(`#!/bin/sh
: > %q
ctx=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --putnamiContext)
      ctx="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
if grep -q '"dry-run":true' "$ctx"; then
  exit 0
fi
printf '%%s\n' '{"v":2,"type":"log","level":"error","message":"missing dry-run param"}'
exit 9
`, ran))

	var code int
	out := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		code = app.Run(context.Background(), []string{"demo", "deploy", "app", "--dry-run"})
	})
	if code != ExitSuccess {
		t.Fatalf("demo deploy --dry-run exit = %d, want %d\n%s", code, ExitSuccess, out)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Fatalf("--dry-run previewed the plan instead of running the extension job: %v\n%s", err, out)
	}
	if strings.Contains(out, "showing plan only") {
		t.Fatalf("alias --dry-run must not take the engine's plan-only preview:\n%s", out)
	}
}

func TestBuildExtensionCommandParams_DryRunRequiresDeclaredFlag(t *testing.T) {
	t.Parallel()
	parsed := &ParsedArgs{Global: GlobalFlags{DryRun: true}}

	withFlag := buildExtensionCommandParams(parsed, map[string]extension.FlagDefinition{
		"dry-run": {Type: "boolean"},
	})
	if withFlag["dry-run"] != true {
		t.Fatalf("dry-run param missing for command that declares the flag: %v", withFlag)
	}

	withoutFlag := buildExtensionCommandParams(parsed, map[string]extension.FlagDefinition{})
	if _, ok := withoutFlag["dry-run"]; ok {
		t.Fatalf("dry-run param leaked into command without dry-run flag: %v", withoutFlag)
	}

	// A dry-run flag inherited only from the group (not the flat command) must
	// still forward the param — the effective flag surface, not just the job,
	// gates forwarding.
	fromGroup := buildExtensionCommandParams(parsed, extension.MergeFlagLayers(
		nil,
		map[string]extension.FlagDefinition{"dry-run": {Type: "boolean"}},
		nil,
	))
	if fromGroup["dry-run"] != true {
		t.Fatalf("dry-run param missing for group-inherited dry-run flag: %v", fromGroup)
	}
}

// TestBuildExtensionCommandParams_ForwardsResolvedOutputMode pins the
// contract: no manifest may declare an `output` flag (the reserved global-flag
// registry), and the CLI consumes --output/--json before dispatch, so the
// resolved canonical mode must reach the extension through params or not at
// all. No mode selected (OutputAuto) or a non-canonical env/config string
// forwards nothing.
func TestBuildExtensionCommandParams_ForwardsResolvedOutputMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		resolved string // parsed.Global.Output after App.Run resolution
		want     string
		forward  bool
	}{
		{"resolved jsonl forwarded without a declared output flag", "jsonl", "jsonl", true},
		{"json shorthand resolves to json and is forwarded", "json", "json", true},
		{"explicit text forwarded verbatim", "text", "text", true},
		{"auto (no output flag) forwards nothing", "", "", false},
		{"non-canonical env value forwards nothing", "yaml", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parsed := &ParsedArgs{Global: GlobalFlags{Output: c.resolved}}
			// Empty flag surface = the adapted-manifest case: gating on a
			// declared `output` flag (like dry-run) would forward nothing here.
			params := buildExtensionCommandParams(parsed, map[string]extension.FlagDefinition{})
			got, ok := params["output"]
			if c.forward {
				if !ok || got != c.want {
					t.Fatalf("params[output] = %v (present=%v), want %q", got, ok, c.want)
				}
				return
			}
			if ok {
				t.Fatalf("params[output] = %v, want absent", got)
			}
		})
	}
}

// TestAppRun_ExtensionStructuredCommand_ForwardsResolvedOutputParam proves the
// end-to-end interactive path: --output=jsonl (and the --json shorthand,
// resolved to "json") on an extension subcommand lands in the job context's
// params, where the extension reads it exactly like a manifest-declared flag.
func TestAppRun_ExtensionStructuredCommand_ForwardsResolvedOutputParam(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"output jsonl", []string{"demo", "login", "--output=jsonl"}, "jsonl"},
		{"json shorthand", []string{"demo", "login", "--json"}, "json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wsRoot := t.TempDir()
			writeWorkspaceFixture(t, wsRoot)

			script := fmt.Sprintf(`#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
if [ -n "$ctx" ] && grep -q '"output":"%s"' "$ctx" 2>/dev/null; then
  exit 0
fi
exit 9
`, c.want)
			if err := os.WriteFile(filepath.Join(wsRoot, "demo-extension", "fixture.sh"), []byte(script), 0o755); err != nil {
				t.Fatalf("write fixture script: %v", err)
			}

			app, err := NewApp()
			if err != nil {
				t.Fatalf("NewApp: %v", err)
			}
			t.Chdir(wsRoot)
			if code := app.Run(context.Background(), c.argv); code != ExitSuccess {
				t.Fatalf("%v exit = %d, want %d (resolved output param did not reach the extension)", c.argv, code, ExitSuccess)
			}
		})
	}
}

// TestAppRun_ExtensionStructuredCommand_NoOutputFlagForwardsNothing pins the
// no-flag semantics: when the user selects no output mode, no `output`
// param is forced onto the extension, so its own default rendering survives.
func TestAppRun_ExtensionStructuredCommand_NoOutputFlagForwardsNothing(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	// Keep the run hermetic: a PUTNAMI_OUTPUT in the ambient test environment
	// would legitimately select a mode and defeat the no-flag assertion.
	t.Setenv("PUTNAMI_OUTPUT", "")

	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)

	script := `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
if [ -n "$ctx" ] && grep -q '"output":' "$ctx" 2>/dev/null; then
  exit 9
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(wsRoot, "demo-extension", "fixture.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}

	app, err := NewApp()
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	t.Chdir(wsRoot)
	if code := app.Run(context.Background(), []string{"demo", "login"}); code != ExitSuccess {
		t.Fatalf("demo login exit = %d, want %d (output param forced without an output flag)", code, ExitSuccess)
	}
}

// TestAppRun_ExtensionStructuredCommand_PlannedForwardsResolvedOutputParam
// proves the planned (non-interactive) dispatch path: the resolved mode
// flows through runPlannedExtensionAlias → scheduler → job context params, same
// as the interactive path, because both consume the shared
// buildExtensionCommandParams result.
func TestAppRun_ExtensionStructuredCommand_PlannedForwardsResolvedOutputParam(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)

	script := `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --putnamiContext)
      ctx="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
if grep -q '"output":"jsonl"' "$ctx"; then
  exit 0
fi
printf '%s\n' '{"v":2,"type":"log","level":"error","message":"missing output param"}'
exit 9
`
	if err := os.WriteFile(filepath.Join(wsRoot, "demo-extension", "fixture.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}

	var code int
	captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		code = app.Run(context.Background(), []string{"demo", "deploy", "app", "--output=jsonl"})
	})
	if code != ExitSuccess {
		t.Fatalf("demo deploy --output=jsonl exit = %d, want %d (resolved output param did not reach the planned extension job)", code, ExitSuccess)
	}
}

// TestExtensionAliasPlanSelection_CarriesIdentityAndInheritedFlags proves the
// planned alias passes stable owner identity plus group/subcommand layers to the
// engine. The freshly discovered flat command supplies the base layer later.
func TestExtensionAliasPlanSelection_CarriesIdentityAndInheritedFlags(t *testing.T) {
	t.Parallel()
	job := &extension.JobDefinition{
		Name: "cloud-status",
		Flags: map[string]extension.FlagDefinition{
			"token":     {Type: "string"},
			"base-only": {Type: "boolean"},
		},
	}
	ext := &extension.ExtensionDescription{
		Name: "@putnami/cloud",
		Jobs: map[string]*extension.JobDefinition{"cloud-status": job},
	}
	resolved := &extension.ResolvedSubcommand{
		Extension:     ext,
		CommandName:   "cloud-status",
		JobDefinition: job,
		GroupFlags:    map[string]extension.FlagDefinition{"env": {Type: "string", Default: "prod"}},
		Subdef: extension.SubcommandDefinition{
			Flags: map[string]extension.FlagDefinition{"token": {Type: "string", Default: "override"}},
		},
	}

	selection := extensionAliasPlanSelection(resolved)
	if selection.Name != "@putnami/cloud" || selection.Command != "cloud-status" {
		t.Fatalf("selection identity = (%q, %q), want extension owner and flat command",
			selection.Name, selection.Command)
	}
	got := selection.InheritedFlags
	if got["env"].Default != "prod" {
		t.Errorf("group flag default not carried into selection: %+v", got["env"])
	}
	if got["token"].Default != "override" {
		t.Errorf("subcommand flag not carried into selection: %+v", got["token"])
	}
	if _, ok := got["base-only"]; ok {
		t.Error("selection retained a stale flat-command base flag")
	}
}

func TestExtensionAliasPlanSelection_NoInheritance(t *testing.T) {
	t.Parallel()
	job := &extension.JobDefinition{Name: "cloud-status"}
	ext := &extension.ExtensionDescription{
		Name: "@putnami/cloud",
		Jobs: map[string]*extension.JobDefinition{"cloud-status": job},
	}
	resolved := &extension.ResolvedSubcommand{
		Extension:     ext,
		CommandName:   "cloud-status",
		JobDefinition: job,
	}
	got := extensionAliasPlanSelection(resolved)
	if got.Name != ext.Name || got.Command != "cloud-status" || got.InheritedFlags != nil {
		t.Fatalf("selection = %+v, want identity with no inherited flags", got)
	}
}

func writeWorkspaceAliasFixture(t *testing.T, wsRoot string) {
	t.Helper()
	// The fixture's command is deploy, which a root Git does not manage refuses.
	runAliasGit(t, wsRoot, "init")

	wsManifest := `{
  "name": "fixture-ws",
  "includes": ["demo-extension", "app"]
}`
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.workspace.json"), []byte(wsManifest), 0o644); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	extDir := filepath.Join(wsRoot, "demo-extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"), []byte(`{"name":"@putnami/demo-extension"}`), 0o644); err != nil {
		t.Fatalf("write extension putnami.json: %v", err)
	}
	fixtureScript := `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --putnamiContext)
      ctx="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
if grep -q '"dry-run":true' "$ctx"; then
  exit 0
fi
printf '%s\n' '{"v":2,"type":"log","level":"error","message":"missing dry-run param"}'
exit 9
`
	if err := os.WriteFile(filepath.Join(extDir, "fixture.sh"), []byte(fixtureScript), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}

	appDir := filepath.Join(wsRoot, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir app: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "putnami.json"), []byte(`{"name":"app","extensions":["@putnami/demo-extension"]}`), 0o644); err != nil {
		t.Fatalf("write app putnami.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "demo.deploy"), []byte(""), 0o644); err != nil {
		t.Fatalf("write activation file: %v", err)
	}

	manifest := `{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "name": "@putnami/demo-extension",
  "version": "0.1.0",
  "cliContract": 4,
  "commandGroups": {
    "demo": {
      "description": "Demo group",
      "subcommands": {
        "deploy": { "command": "deploy" }
      }
    }
  },
  "commands": {
    "deploy": {
      "description": "Demo deploy fixture.",
      "flags": {
        "dry-run": { "type": "boolean", "default": false, "description": "Preview without changes" }
      },
      "activationFiles": ["demo.deploy"],
      "run": [{ "id": "deploy", "task": "deploy-task" }]
    }
  },
  "tasks": {
    "deploy-task": {
      "kind": "command",
      "command": "{extensionRoot}/fixture.sh",
      "cache": false,
      "timeoutMs": 10000
    }
  }
}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write extension manifest: %v", err)
	}
}

// writeWorkspaceFixture sets up a minimal workspace containing a single
// extension under "demo-extension/" that exposes a "demo" command group with
// "login" (interactive) and "fail" (interactive, exits non-zero) subcommands.
func writeWorkspaceFixture(t *testing.T, wsRoot string) {
	t.Helper()

	// Workspace config (also used as putnami.workspace.json marker for FindRoot).
	wsManifest := `{
  "name": "fixture-ws",
  "includes": ["demo-extension"]
}`
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.workspace.json"), []byte(wsManifest), 0o644); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	extDir := filepath.Join(wsRoot, "demo-extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}

	// Fixture binary: prints a marker, echoes an env marker when the inherited
	// group flag default reached the job context, then exits with the code
	// derived from the first task arg.
	binPath := filepath.Join(extDir, "fixture.sh")
	binScript := `#!/bin/sh
echo "demo-login-stdout-marker"
first="$1"
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
if [ -n "$ctx" ] && grep -q '"env":"prod"' "$ctx" 2>/dev/null; then
  echo "env-default-marker"
fi
case "$first" in
  fail) exit 7 ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(binPath, []byte(binScript), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}

	// Project marker so the extension is discovered as a workspace project.
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"), []byte(`{"name":"@putnami/demo-extension"}`), 0o644); err != nil {
		t.Fatalf("write putnami.json: %v", err)
	}

	manifest := `{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "name": "@putnami/demo-extension",
  "version": "0.1.0",
  "cliContract": 4,
  "commandGroups": {
    "demo": {
      "description": "Demo group",
      "flags": {
        "env": { "type": "string", "default": "prod", "description": "Shared group environment flag." }
      },
      "subcommands": {
        "login": {
          "command": "demo-login",
          "interactive": true,
          "description": "Authenticate with the demo backend.",
          "examples": [
            {
              "command": "putnami demo login tenant-1",
              "description": "Log in to tenant-1."
            }
          ]
        },
        "status": {
          "command": "demo-status",
          "description": "Show the demo deployment status.",
          "flags": {
            "env": { "type": "string", "default": "staging", "description": "Status-specific environment override." }
          },
          "examples": [
            {
              "command": "putnami demo status --output=jsonl",
              "description": "Print machine-readable status."
            }
          ]
        },
        "fail":  { "command": "demo-fail",  "interactive": true }
      }
    }
  },
  "commands": {
    "demo-login": {
      "description": "Demo login fixture.",
      "flags": {
        "token": { "type": "string", "short": "t", "description": "Auth token to use.", "default": "" }
      },
      "run": [{ "id": "login", "task": "demo-login" }]
    },
    "demo-status": {
      "description": "Demo status fixture.",
      "run": [{ "id": "status", "task": "demo-status" }]
    },
    "demo-fail": {
      "description": "Demo failure fixture.",
      "run": [{ "id": "fail", "task": "demo-fail" }]
    }
  },
  "tasks": {
    "demo-login": {
      "kind": "command",
      "command": "{extensionRoot}/fixture.sh",
      "args": ["ok"],
      "cache": false,
      "timeoutMs": 10000
    },
    "demo-status": {
      "kind": "command",
      "command": "{extensionRoot}/fixture.sh",
      "args": ["ok"],
      "cache": false,
      "timeoutMs": 10000
    },
    "demo-fail": {
      "kind": "command",
      "command": "{extensionRoot}/fixture.sh",
      "args": ["fail"],
      "cache": false,
      "timeoutMs": 10000
    }
  }
}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write extension manifest: %v", err)
	}
}

// captureStdoutStderr captures both standard streams produced by fn.
func captureStdoutStderr(t *testing.T, fn func()) string {
	t.Helper()

	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe stdout: %v", err)
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		_ = wOut.Close()
		_ = rOut.Close()
		t.Fatalf("os.Pipe stderr: %v", err)
	}
	os.Stdout = wOut
	os.Stderr = wErr
	defer func() {
		os.Stdout = origOut
		os.Stderr = origErr
		_ = wOut.Close()
		_ = wErr.Close()
		_ = rOut.Close()
		_ = rErr.Close()
	}()

	outDone := drainCapturedStream(rOut)
	errDone := drainCapturedStream(rErr)
	fn()

	os.Stdout = origOut
	os.Stderr = origErr
	if err := wOut.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	if err := wErr.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	out := <-outDone
	if out.err != nil {
		t.Fatalf("read stdout: %v", out.err)
	}
	errBuf := <-errDone
	if errBuf.err != nil {
		t.Fatalf("read stderr: %v", errBuf.err)
	}
	return string(out.data) + string(errBuf.data)
}

func TestCaptureStdoutStderr_DrainsBothStreamsConcurrently(t *testing.T) {
	wantOut := strings.Repeat("o", 256*1024)
	wantErr := strings.Repeat("e", 256*1024)
	got := captureStdoutStderr(t, func() {
		if _, err := io.WriteString(os.Stdout, wantOut); err != nil {
			t.Fatalf("write stdout capacity probe: %v", err)
		}
		if _, err := io.WriteString(os.Stderr, wantErr); err != nil {
			t.Fatalf("write stderr capacity probe: %v", err)
		}
	})
	if want := wantOut + wantErr; got != want {
		t.Fatalf("captured %d bytes, want stdout-then-stderr %d", len(got), len(want))
	}
}

func mustNotContain(t *testing.T, output, needle string) {
	t.Helper()
	if strings.Contains(output, needle) {
		t.Errorf("output unexpectedly contained %q\nfull output:\n%s", needle, output)
	}
}

// writeContextDumpFixture replaces the fixture binary with one that prints the
// job context document it was handed, so a test can assert on what actually
// crossed the process boundary rather than on what the CLI intended to write.
func writeContextDumpFixture(t *testing.T, wsRoot string) {
	t.Helper()
	script := `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
if [ -n "$ctx" ]; then
  cat "$ctx"
  echo
fi
`
	if err := os.WriteFile(filepath.Join(wsRoot, "demo-extension", "fixture.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}
}

// contextSelectionJSON is the `selection` member of a job context document, read
// back from the bytes the subprocess actually received.
type contextSelectionJSON struct {
	Mode           string   `json:"mode"`
	Scoped         bool     `json:"scoped"`
	Baseline       string   `json:"baseline"`
	BaselineSource string   `json:"baselineSource"`
	ProjectIDs     []string `json:"projects"`
	EmptyImpact    bool     `json:"emptyImpact"`
}

// contextProjectRefJSON is one entry of `selectedProjects`, read back from the
// same bytes.
type contextProjectRefJSON struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	FullPath   string `json:"fullPath"`
	OutputPath string `json:"outputPath"`
	SourceName string `json:"sourceName"`
	Version    string `json:"version"`
}

// contextDocumentJSON is the part of a job context document these tests read.
type contextDocumentJSON struct {
	Selection        *contextSelectionJSON   `json:"selection"`
	SelectedProjects []contextProjectRefJSON `json:"selectedProjects"`
	Identity         *struct {
		Scope string `json:"scope"`
	} `json:"identity"`
}

// interactiveContextDocument runs an interactive extension subcommand and
// returns the context document the subprocess read.
func interactiveContextDocument(t *testing.T, wsRoot string, argv ...string) (*contextDocumentJSON, int) {
	t.Helper()
	var code int
	output := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		code = app.Run(context.Background(), argv)
	})
	start := strings.Index(output, "{")
	end := strings.LastIndex(output, "}")
	if start < 0 || end < start {
		return nil, code
	}
	var document contextDocumentJSON
	if err := json.Unmarshal([]byte(output[start:end+1]), &document); err != nil {
		t.Fatalf("the interactive context is not JSON: %v\n%s", err, output)
	}
	return &document, code
}

// interactiveContextSelection returns just the `selection` block of that
// document.
func interactiveContextSelection(t *testing.T, wsRoot string, argv ...string) (*contextSelectionJSON, int) {
	t.Helper()
	document, code := interactiveContextDocument(t, wsRoot, argv...)
	if document == nil {
		return nil, code
	}
	return document.Selection, code
}

// projectRefsByID indexes a context document's selected projects so a test
// compares two documents by identity instead of by position — the two command
// paths report the same set in their own orders (see
// TestAppRun_SelectedProjectsAgreeAcrossCommandPaths).
func projectRefsByID(refs []contextProjectRefJSON) map[string]contextProjectRefJSON {
	byID := make(map[string]contextProjectRefJSON, len(refs))
	for _, ref := range refs {
		byID[ref.ID] = ref
	}
	return byID
}

// TestAppRun_ExtensionStructuredCommand_InteractiveCarriesSelection pins the
// interactive half of the resolved-selection contract.
//
// This path is the ONE deliberate scheduler bypass, so nothing plans for it and
// nothing used to resolve its selection: the CLI parsed `--projects` and dropped
// it, leaving the subprocess to guess. The context document now carries the same
// resolved answer a planned job gets.
func TestAppRun_ExtensionStructuredCommand_InteractiveCarriesSelection(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)
	writeContextDumpFixture(t, wsRoot)

	selection, code := interactiveContextSelection(t, wsRoot, "demo", "login")
	if code != ExitSuccess {
		t.Fatalf("demo login exit = %d, want %d", code, ExitSuccess)
	}
	if selection == nil {
		t.Fatal("the interactive context carries no selection block")
	}
	if selection.Mode != "all" {
		t.Errorf("selection.mode = %v, want all", selection.Mode)
	}
	if selection.Scoped {
		t.Error("selection.scoped = true, want false for the whole-workspace default")
	}
	if len(selection.ProjectIDs) == 0 {
		t.Errorf("selection.projects = %v, want the workspace's projects", selection.ProjectIDs)
	}
}

// A narrowing flag must reach the subprocess as a narrowed selection. Before
// this, an extension had no way to tell `--projects x` from a whole-workspace
// run, so honoring the flag meant re-parsing argv it was never given.
func TestAppRun_ExtensionStructuredCommand_InteractiveHonorsProjectSelection(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)
	writeContextDumpFixture(t, wsRoot)

	selection, code := interactiveContextSelection(t, wsRoot,
		"demo", "login", "--projects", "@putnami/demo-extension")
	if code != ExitSuccess {
		t.Fatalf("demo login --projects exit = %d, want %d", code, ExitSuccess)
	}
	if selection == nil {
		t.Fatal("the interactive context carries no selection block")
	}
	if selection.Mode != "projects" {
		t.Errorf("selection.mode = %v, want projects", selection.Mode)
	}
	if !selection.Scoped {
		t.Error("selection.scoped = false, want true")
	}
	if len(selection.ProjectIDs) != 1 || selection.ProjectIDs[0] != "/demo-extension" {
		t.Errorf("selection.projects = %v, want [/demo-extension]", selection.ProjectIDs)
	}
}

// A selector that names nothing FAILS the command instead of running it
// unscoped. Silently dropping the member would hand the extension a
// whole-workspace run for a question the user narrowed — the wrong answer,
// delivered without a word.
func TestAppRun_ExtensionStructuredCommand_InteractiveRejectsUnmatchedSelection(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	writeWorkspaceFixture(t, wsRoot)
	writeContextDumpFixture(t, wsRoot)

	var code int
	output := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		code = app.Run(context.Background(), []string{"demo", "login", "--projects", "no-such-project"})
	})
	if code == ExitSuccess {
		t.Fatalf("an unmatched selector exited %d; the run must not proceed unscoped:\n%s", code, output)
	}
	if !strings.Contains(output, "no projects matched") {
		t.Errorf("the failure does not name the unmatched selection:\n%s", output)
	}
}

// TestAppRun_PlannedJobContextCarriesSelection pins the planned half of the
// resolved-selection contract: a job the engine scheduled reads how
// the invocation chose its scope, not just which project it happens to run for.
//
// The fixture writes the context document to disk instead of printing it: the
// planned path parses the subprocess's stdout as JSONL, so dumping a JSON
// document there would corrupt the event stream this test is not about.
func TestAppRun_PlannedJobContextCarriesSelection(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)

	dump := filepath.Join(wsRoot, "context-dump.json")
	script := `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --putnamiContext) ctx="$2"; shift 2 ;;
    *) shift ;;
  esac
done
cp "$ctx" "` + dump + `"
exit 0
`
	if err := os.WriteFile(filepath.Join(wsRoot, "demo-extension", "fixture.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}

	captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		if code := app.Run(context.Background(), []string{"demo", "deploy", "app"}); code != ExitSuccess {
			t.Fatalf("demo deploy app exit = %d, want %d", code, ExitSuccess)
		}
	})

	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the planned job wrote no context dump: %v", err)
	}
	var document struct {
		Selection *struct {
			Mode       string   `json:"mode"`
			Scoped     bool     `json:"scoped"`
			ProjectIDs []string `json:"projects"`
		} `json:"selection"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse planned job context: %v", err)
	}
	if document.Selection == nil {
		t.Fatal("a planned job context carries no selection block")
	}
	if document.Selection.Mode != "projects" || !document.Selection.Scoped {
		t.Errorf("selection = %+v, want a scoped explicit selector", *document.Selection)
	}
	if len(document.Selection.ProjectIDs) != 1 || document.Selection.ProjectIDs[0] != "/app" {
		t.Errorf("selection.projects = %v, want [/app]", document.Selection.ProjectIDs)
	}
}

// writeSelectionParityFixture sets up a two-project workspace whose single
// extension exposes BOTH command paths over the same selection: an interactive
// subcommand (`demo login`) and a workspace-once flat command (`demo-sync`).
// Both dump the job context document they were handed.
//
// The includes are declared demo-extension-first on purpose: discovery order is
// declaration order, so it disagrees with canonical id order (/app before
// /demo-extension) and the two paths' orderings are actually distinguishable.
//
// Versions are deliberately mixed: the extension project declares one, `app`
// declares none and must therefore report the workspace's.
func writeSelectionParityFixture(t *testing.T, wsRoot string) {
	t.Helper()

	wsManifest := `{
  "name": "fixture-ws",
  "includes": ["demo-extension", "app"]
}`
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.workspace.json"), []byte(wsManifest), 0o644); err != nil {
		t.Fatalf("write workspace: %v", err)
	}

	extDir := filepath.Join(wsRoot, "demo-extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"),
		[]byte(`{"name":"@putnami/demo-extension"}`), 0o644); err != nil {
		t.Fatalf("write extension putnami.json: %v", err)
	}

	appDir := filepath.Join(wsRoot, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir app: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "putnami.json"),
		[]byte(`{"name":"app","extensions":["@putnami/demo-extension"]}`), 0o644); err != nil {
		t.Fatalf("write app putnami.json: %v", err)
	}

	// The interactive path inherits the terminal, so the document goes to
	// stdout. The planned path parses stdout as JSONL, so its dump goes to a
	// file instead — a JSON document on that stream would corrupt the event
	// stream these tests are not about.
	dumpToStdout := `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --putnamiContext) ctx="$2"; shift 2 ;;
    *) shift ;;
  esac
done
cat "$ctx"
echo
`
	dumpToFile := `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --putnamiContext) ctx="$2"; shift 2 ;;
    *) shift ;;
  esac
done
cp "$ctx" "$PUTNAMI_WORKSPACE_ROOT/planned-context.json"
exit 0
`
	for name, script := range map[string]string{
		"dump-stdout.sh": dumpToStdout,
		"dump-file.sh":   dumpToFile,
	} {
		if err := os.WriteFile(filepath.Join(extDir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	manifest := `{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "name": "@putnami/demo-extension",
  "version": "0.1.0",
  "cliContract": 4,
  "commandGroups": {
    "demo": {
      "description": "Demo group",
      "subcommands": {
        "login": {
          "command": "demo-login",
          "interactive": true,
          "description": "Authenticate with the demo backend."
        }
      }
    }
  },
  "commands": {
    "demo-login": {
      "description": "Demo login fixture.",
      "run": [{ "id": "login", "task": "dump-stdout" }]
    },
    "demo-sync": {
      "description": "Demo workspace-once fixture.",
      "activation": "workspace-once",
      "run": [{ "id": "sync", "task": "dump-file" }]
    }
  },
  "tasks": {
    "dump-stdout": {
      "kind": "command",
      "command": "{extensionRoot}/dump-stdout.sh",
      "cache": false,
      "timeoutMs": 10000
    },
    "dump-file": {
      "kind": "command",
      "command": "{extensionRoot}/dump-file.sh",
      "cache": false,
      "timeoutMs": 10000
    }
  }
}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write extension manifest: %v", err)
	}
}

// plannedContextDocument runs a planned workspace-once extension command in the
// parity fixture and returns the context document the subprocess read.
func plannedContextDocument(t *testing.T, wsRoot string, argv ...string) *contextDocumentJSON {
	t.Helper()
	captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		if code := app.Run(context.Background(), argv); code != ExitSuccess {
			t.Fatalf("%v exit = %d, want %d", argv, code, ExitSuccess)
		}
	})
	data, err := os.ReadFile(filepath.Join(wsRoot, "planned-context.json"))
	if err != nil {
		t.Fatalf("the planned job wrote no context dump: %v", err)
	}
	var document contextDocumentJSON
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse planned job context: %v", err)
	}
	return &document
}

// TestAppRun_ExtensionStructuredCommand_InteractiveCarriesSelectedProjects
// closes the second half of the interactive selection contract.
//
// `selection` reports WHICH ids are in scope. It does not report a path, so an
// extension subcommand handed only that block could print the ids and nothing
// else: every SDD subcommand reads project files, and re-deriving a path would
// mean scanning the tree or shelling out to `putnami` — the two things the wire
// contract exists to make unnecessary.
func TestAppRun_ExtensionStructuredCommand_InteractiveCarriesSelectedProjects(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	writeSelectionParityFixture(t, wsRoot)

	document, code := interactiveContextDocument(t, wsRoot,
		"demo", "login", "--projects", "app,@putnami/demo-extension")
	if code != ExitSuccess {
		t.Fatalf("demo login exit = %d, want %d", code, ExitSuccess)
	}
	if document == nil || document.Selection == nil {
		t.Fatalf("the interactive context carries no selection: %+v", document)
	}
	if len(document.SelectedProjects) != 2 {
		t.Fatalf("selectedProjects = %+v, want one entry per selected project", document.SelectedProjects)
	}
	// Every id the selection reports must arrive with something to open. An id
	// with no matching entry is the defect this member closes.
	refs := projectRefsByID(document.SelectedProjects)
	for _, id := range document.Selection.ProjectIDs {
		ref, ok := refs[id]
		if !ok {
			t.Fatalf("selection names %q but selectedProjects has no such entry", id)
		}
		if ref.Name == "" || ref.Path == "" || ref.FullPath == "" {
			t.Errorf("selected project %q = %+v, want a name and both path forms", id, ref)
		}
	}
	// This path resolves once and runs one process, so it reports the
	// selection's canonical id order: selectedProjects[i].id IS selection[i].
	for i, ref := range document.SelectedProjects {
		if ref.ID != document.Selection.ProjectIDs[i] {
			t.Fatalf("selectedProjects = %+v, want the selection's order %v",
				document.SelectedProjects, document.Selection.ProjectIDs)
		}
	}
	// The version travels too. This fixture is not a git repository and declares
	// no line, so both projects report the degraded root-line answer — the point
	// being that BOTH entries answer the same way, from one resolver.
	if refs["/demo-extension"].Version != refs["/app"].Version {
		t.Errorf("versions = %q and %q, want one resolver's answer for both",
			refs["/demo-extension"].Version, refs["/app"].Version)
	}
	// A job that runs once for the whole workspace says so. This path always
	// did — its project is the synthetic workspace project — but with nothing
	// selected the plan had no way to report it.
	if document.Identity == nil || document.Identity.Scope != "workspace" {
		t.Errorf("identity = %+v, want scope workspace", document.Identity)
	}
}

// TestAppRun_SelectedProjectsAgreeAcrossCommandPaths is the anti-drift pin on
// the member itself: for the same flags over the same tree, the interactive
// path and the planned workspace-once path must describe the same projects the
// same way. A member that differs by path is a second selection contract, and
// an extension would have to know which path invoked it to read its own input.
//
// ORDER is the one member that legitimately differs, and the contract says so:
// `selectedProjects` is documented as run order. The planned path reports the
// engine's run order (workspace declaration order); the interactive path runs
// one process and has no per-project run order, so it reports the selection's
// canonical id order. The comparison is therefore by id, and both orders are
// asserted rather than left implicit.
func TestAppRun_SelectedProjectsAgreeAcrossCommandPaths(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	wsRoot := t.TempDir()
	writeSelectionParityFixture(t, wsRoot)

	const selector = "app,@putnami/demo-extension"
	interactive, code := interactiveContextDocument(t, wsRoot, "demo", "login", "--projects", selector)
	if code != ExitSuccess {
		t.Fatalf("demo login exit = %d, want %d", code, ExitSuccess)
	}
	planned := plannedContextDocument(t, wsRoot, "demo-sync", "--projects", selector)

	if len(interactive.SelectedProjects) != len(planned.SelectedProjects) {
		t.Fatalf("selectedProjects: interactive %+v, planned %+v",
			interactive.SelectedProjects, planned.SelectedProjects)
	}
	interactiveRefs := projectRefsByID(interactive.SelectedProjects)
	plannedRefs := projectRefsByID(planned.SelectedProjects)
	for id, want := range plannedRefs {
		got, ok := interactiveRefs[id]
		if !ok {
			t.Fatalf("planned selected %q, interactive did not: %+v", id, interactive.SelectedProjects)
		}
		if got.Name != want.Name || got.Path != want.Path || got.FullPath != want.FullPath {
			t.Errorf("project %q: interactive %+v, planned %+v", id, got, want)
		}
		if got.SourceName != want.SourceName {
			t.Errorf("project %q sourceName: interactive %q, planned %q", id, got.SourceName, want.SourceName)
		}
		if got.Version != want.Version {
			t.Errorf("project %q version: interactive %q, planned %q", id, got.Version, want.Version)
		}
		// outputPath ends in the COMMAND name, which is legitimately different
		// here (demo-login vs demo-sync). What must agree is the derivation: the
		// per-project output directory both commands write under.
		if filepath.Dir(got.OutputPath) != filepath.Dir(want.OutputPath) {
			t.Errorf("project %q outputPath: interactive %q, planned %q", id, got.OutputPath, want.OutputPath)
		}
	}

	// The selection block agrees too — same mode, same ids — so a consumer that
	// cross-checks the two members against each other gets the same answer on
	// both paths.
	if interactive.Selection == nil || planned.Selection == nil {
		t.Fatalf("selection: interactive %+v, planned %+v", interactive.Selection, planned.Selection)
	}
	if interactive.Selection.Mode != planned.Selection.Mode ||
		interactive.Selection.Scoped != planned.Selection.Scoped {
		t.Errorf("selection: interactive %+v, planned %+v", *interactive.Selection, *planned.Selection)
	}
	if strings.Join(interactive.Selection.ProjectIDs, ",") != strings.Join(planned.Selection.ProjectIDs, ",") {
		t.Errorf("selection.projects: interactive %v, planned %v",
			interactive.Selection.ProjectIDs, planned.Selection.ProjectIDs)
	}

	// The orders each path reports, stated rather than implied.
	interactiveOrder := make([]string, 0, len(interactive.SelectedProjects))
	for _, ref := range interactive.SelectedProjects {
		interactiveOrder = append(interactiveOrder, ref.ID)
	}
	plannedOrder := make([]string, 0, len(planned.SelectedProjects))
	for _, ref := range planned.SelectedProjects {
		plannedOrder = append(plannedOrder, ref.ID)
	}
	if strings.Join(interactiveOrder, ",") != "/app,/demo-extension" {
		t.Errorf("interactive order = %v, want the selection's canonical id order", interactiveOrder)
	}
	if strings.Join(plannedOrder, ",") != "/demo-extension,/app" {
		t.Errorf("planned order = %v, want the engine's run order", plannedOrder)
	}
}
