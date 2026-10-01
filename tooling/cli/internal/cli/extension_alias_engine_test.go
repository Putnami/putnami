package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"go.putnami.dev/tooling/cli/internal/abort"
	"go.putnami.dev/tooling/cli/internal/commands/sessions"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// Extension aliases run through Engine.Run
// instead of a private scheduler construction. These tests pin what that has to
// mean and what it must NOT change.
//
// The invariants, in the order the acceptance criteria name them:
//
//  1. Ctrl-C exits 130, not 1 — the private executor had no signal path at all.
//  2. Successful-run markers are gated by the existing shouldRecordSuccessfulBuild
//     policy, so an alias with an explicit project selector cannot perturb the
//     last-build history an unrelated `--impacted` terminal run reads.
//  3. The marker key stays a function of the RAW job args. commandParams carries
//     the synthesized dry-run/output values, and a param value's Go type is part
//     of every key (store.hashParams, workspace_state.LastBuildParamsHash), so
//     folding them in would silently move the key.
//  4. Planning stays narrowed to the ONE extension the alias names.
//  5. Alias-produced session files are readable by `putnami sessions inspect`.
//
// The Observer seam is deliberately absent: the alias passes nil, which
// internal/engine/seam_test.go's TestOnlyTheTerminalAdapterSuppliesAnObserver
// enforces module-wide (ADR 0001 §4).

// TestAppRun_ExtensionAlias_CtrlCExitsSignalReceived is the user-visible fix:
// an earlier executor mapped every unsuccessful run onto ExitError, so
// an interrupted `putnami <group> <sub>` reported 1 — indistinguishable from a
// real failure for any CI gate reading the code.
func TestAppRun_ExtensionAlias_CtrlCExitsSignalReceived(t *testing.T) {
	requireShell(t)
	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)
	writeAliasFixtureScript(t, wsRoot, "#!/bin/sh\nexit 0\n")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	abort.Reset()
	abort.Record(syscall.SIGINT)
	t.Cleanup(abort.Reset)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var code int
	captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		code = app.Run(ctx, []string{"demo", "deploy", "app"})
	})
	if code != ExitSignalReceived {
		t.Fatalf("interrupted alias exit = %d, want %d (ExitSignalReceived)", code, ExitSignalReceived)
	}
}

// TestAppRun_ExtensionAlias_RunMarkerPolicyAndKey covers acceptance criteria R4
// and cache-key stability together, because they are the same risk seen from two
// sides: WHICH alias invocations may write last-build history, and under WHICH
// key. Both feed `--impacted` and remote skip-hit decisions for unrelated
// terminal runs.
func TestAppRun_ExtensionAlias_RunMarkerPolicyAndKey(t *testing.T) {
	requireShell(t)
	requireGit(t)

	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)
	// Always succeed: this test is about what a successful alias records, not
	// about the dry-run param the shared fixture script asserts.
	writeAliasFixtureScript(t, wsRoot, "#!/bin/sh\nexit 0\n")
	initAliasGitRepo(t, wsRoot)
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	sessStore := workspace_state.NewSessionStore(wsRoot)
	head, err := git.HeadSHA(wsRoot)
	if err != nil {
		t.Fatalf("head sha: %v", err)
	}
	// The two candidate keys. rawArgsKey is what buildCommandParams(RawJobArgs)
	// produces for `demo deploy --dry-run` (ParseArgs consumes --dry-run as a
	// global, so nothing reaches the raw args); jobParamsKey is what the JOB saw
	// after buildExtensionCommandParams re-synthesized it. Only the first may key
	// the marker.
	rawArgsKey, jobParamsKey := map[string]any{}, map[string]any{"dry-run": true}

	// A directly targeted alias must NOT record. shouldRecordSuccessfulBuild
	// requires --all or auto-selection precisely so a partial run cannot claim
	// the whole tree was built at this SHA.
	runAlias(t, wsRoot, []string{"demo", "deploy", "app"}, ExitSuccess)
	sha, err := sessStore.LastBuildSHA("main", []string{"deploy"}, rawArgsKey)
	if err != nil {
		t.Fatalf("last build sha: %v", err)
	}
	if sha != "" {
		t.Fatalf("project-targeted alias recorded a run marker (%q); shouldRecordSuccessfulBuild must gate it", sha)
	}

	// A bare alias auto-selects — but it STILL must not record, because the alias
	// plans against a single extension (Request.PlanExtension) while the marker
	// key carries no extension dimension.
	//
	// This assertion was inverted by the whole-epic integration review.
	// As originally written this test pinned that a bare alias DOES record HEAD
	// under (main, ["deploy"], rawArgs) — which encoded a real defect: if two
	// extensions declare the flat command `deploy`, that marker claims the whole
	// command is built, and a later bare `putnami deploy` narrows to
	// impacted-since-HEAD and silently skips the other extension's jobs on every
	// unchanged project. With a remote cache the marker propagates to other
	// machines too.
	//
	// A3a extracted the marker machinery under the terminal path's implicit
	// invariant that a run covers every provider of its commands; A3b broke that
	// invariant one slice later. Neither slice was wrong alone, which is why only
	// the whole-diff pass caught it.
	runAlias(t, wsRoot, []string{"demo", "deploy", "--dry-run"}, ExitSuccess)

	sha, err = sessStore.LastBuildSHA("main", []string{"deploy"}, rawArgsKey)
	if err != nil {
		t.Fatalf("last build sha: %v", err)
	}
	if sha != "" {
		t.Fatalf("bare alias recorded a run marker (%q) for a plan narrowed to one extension; "+
			"the key has no extension dimension, so it would claim every provider of %q is built at %q",
			sha, "deploy", head)
	}

	// The key-stability half is unchanged and still worth pinning: whatever the
	// alias path does record, it must never be keyed with the synthesized
	// dry-run param.
	perturbed, err := sessStore.LastBuildSHA("main", []string{"deploy"}, jobParamsKey)
	if err != nil {
		t.Fatalf("last build sha (perturbed key): %v", err)
	}
	if perturbed != "" {
		t.Fatalf("run marker was keyed with the synthesized dry-run param (%q); "+
			"RunMarkerParams must stay buildCommandParams(RawJobArgs)", perturbed)
	}
}

// TestAppRun_ExtensionAlias_WritesReadableSession pins two things at once:
// the alias now produces a session file at all (the private executor produced
// none), and that file is one `putnami sessions inspect` can read.
func TestAppRun_ExtensionAlias_WritesReadableSession(t *testing.T) {
	requireShell(t)
	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)
	writeAliasFixtureScript(t, wsRoot, "#!/bin/sh\nexit 0\n")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	runAlias(t, wsRoot, []string{"demo", "deploy", "app"}, ExitSuccess)

	sessStore := workspace_state.NewSessionStore(wsRoot)
	sessionID := sessStore.LatestID()
	if sessionID == "" {
		t.Fatal("alias run wrote no session file; routing through Engine.Run must record one")
	}
	for _, name := range []string{"session.json", "plan.json", "events.jsonl"} {
		if _, err := os.Stat(filepath.Join(sessStore.Root(), sessionID, name)); err != nil {
			t.Fatalf("alias session is missing %s: %v", name, err)
		}
	}

	captureStdoutStderr(t, func() {
		if err := sessions.SessionsInspect(wsRoot, []string{sessionID}, "json"); err != nil {
			t.Errorf("sessions inspect rejected an alias-produced session: %v", err)
		}
	})
}

// TestAppRun_ExtensionAlias_PlansOnlyTheOwningExtension pins Request.
// PlanExtension. `putnami <group> <sub>` names ONE extension's flat command;
// planning it against every discovered extension would schedule every other
// extension's job of the same name. The flat command itself keeps planning
// against all of them, which is the control case below.
func TestAppRun_ExtensionAlias_PlansOnlyTheOwningExtension(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)
	writeRivalDeployExtension(t, wsRoot, "")

	aliasPlan := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		if code := app.Run(context.Background(), []string{"demo", "deploy", "app", "--plan"}); code != ExitSuccess {
			t.Fatalf("alias --plan exit = %d, want %d", code, ExitSuccess)
		}
	})
	if strings.Contains(aliasPlan, "@putnami/rival-extension") {
		t.Fatalf("alias planned another extension's deploy job:\n%s", aliasPlan)
	}
	if !strings.Contains(aliasPlan, "1 jobs") {
		t.Fatalf("alias plan should hold exactly the owning extension's job:\n%s", aliasPlan)
	}

	// Control: the flat command is NOT narrowed, so both extensions plan. If
	// this ever stops holding, the assertion above stops proving anything.
	flatPlan := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		if code := app.Run(context.Background(), []string{"deploy", "app", "--plan"}); code != ExitSuccess {
			t.Fatalf("flat --plan exit = %d, want %d", code, ExitSuccess)
		}
	})
	if !strings.Contains(flatPlan, "2 jobs") {
		t.Fatalf("flat command should plan both extensions' deploy jobs:\n%s", flatPlan)
	}
}

// TestAppRun_FlatExtensionCommand_ForwardsExecutionGlobals exercises the real
// terminal parse -> engine -> scheduler -> extension context boundary. These
// flags are consumed as framework globals, so a handler-only test with a
// handcrafted params map cannot detect the propagation gap this pins.
func TestAppRun_FlatExtensionCommand_ForwardsExecutionGlobals(t *testing.T) {
	requireShell(t)

	t.Run("explicit controls reach the extension with stable types", func(t *testing.T) {
		wsRoot := t.TempDir()
		writeWorkspaceAliasFixture(t, wsRoot)
		writeAliasFixtureScript(t, wsRoot, `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
if grep -q '"max-parallel":"1"' "$ctx" && grep -q '"no-cache":true' "$ctx"; then
  exit 0
fi
printf '%s\n' '{"v":2,"type":"log","level":"error","message":"execution globals missing from extension params"}'
exit 9
`)
		t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

		runAlias(t, wsRoot, []string{"deploy", "app", "--max-parallel", "1", "--no-cache"}, ExitSuccess)
	})

	t.Run("omitted controls leave extension defaults untouched", func(t *testing.T) {
		wsRoot := t.TempDir()
		writeWorkspaceAliasFixture(t, wsRoot)
		writeAliasFixtureScript(t, wsRoot, `#!/bin/sh
ctx=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--putnamiContext" ]; then
    ctx="$2"
  fi
  shift
done
if grep -q '"max-parallel":' "$ctx" || grep -q '"no-cache":' "$ctx"; then
  printf '%s\n' '{"v":2,"type":"log","level":"error","message":"default execution globals leaked into extension params"}'
  exit 9
fi
exit 0
`)
		t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

		runAlias(t, wsRoot, []string{"deploy", "app"}, ExitSuccess)
	})
}

func TestAppRun_ExtensionAlias_RebindsOwnerAfterBeforeHook(t *testing.T) {
	requireShell(t)
	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)
	t.Setenv("PUTNAMI_NO_AUTO_INSTALL", "1")
	t.Setenv("PUTNAMI_STORE_DIR", filepath.Join(t.TempDir(), "store"))

	extensionDir := filepath.Join(wsRoot, "demo-extension")
	staleScript := "#!/bin/sh\ntouch \"$(dirname \"$0\")/stale-ran\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(extensionDir, "fixture.sh"), []byte(staleScript), 0o755); err != nil {
		t.Fatalf("write stale script: %v", err)
	}
	freshScript := "#!/bin/sh\ntouch \"$(dirname \"$0\")/fresh-ran\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(extensionDir, "fresh.sh"), []byte(freshScript), 0o755); err != nil {
		t.Fatalf("write fresh script: %v", err)
	}

	manifestPath := filepath.Join(extensionDir, "putnami.extension.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read extension manifest: %v", err)
	}
	freshManifest := strings.ReplaceAll(string(manifest), "{extensionRoot}/fixture.sh", "{extensionRoot}/fresh.sh")
	freshManifestPath := filepath.Join(extensionDir, "putnami.extension.fresh.json")
	if err := os.WriteFile(freshManifestPath, []byte(freshManifest), 0o644); err != nil {
		t.Fatalf("write fresh extension manifest: %v", err)
	}

	workspaceManifest := `{
  "name": "fixture-ws",
  "includes": ["demo-extension", "app"],
  "hooks": {
    "commands": {
      "deploy": {
        "before": ["cp demo-extension/putnami.extension.fresh.json demo-extension/putnami.extension.json"]
      }
    }
  }
}`
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.workspace.json"), []byte(workspaceManifest), 0o644); err != nil {
		t.Fatalf("write workspace hooks: %v", err)
	}

	runAlias(t, wsRoot, []string{"demo", "deploy", "app"}, ExitSuccess)

	if _, err := os.Stat(filepath.Join(extensionDir, "fresh-ran")); err != nil {
		t.Fatalf("post-hook extension command did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extensionDir, "stale-ran")); !os.IsNotExist(err) {
		t.Fatalf("pre-hook extension command ran; stale marker stat = %v", err)
	}
}

// TestAppRun_ExtensionAlias_AmbiguousSubcommandIsAUsageError pins the other half:
// alias resolution is first-match-wins across independently released
// manifests (extension.ResolveSubcommand), so two extensions claiming the same
// executable subcommand would otherwise dispatch to whichever discovery listed
// first. The dispatcher refuses instead and names both candidates. Different
// subcommands under one group stay composable — only a contested one is fatal.
func TestAppRun_ExtensionAlias_AmbiguousSubcommandIsAUsageError(t *testing.T) {
	wsRoot := t.TempDir()
	writeWorkspaceAliasFixture(t, wsRoot)
	writeRivalDeployExtension(t, wsRoot, rivalDemoGroup)

	var code int
	out := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		code = app.Run(context.Background(), []string{"demo", "deploy", "app"})
	})
	if code != ExitUsage {
		t.Fatalf("ambiguous alias exit = %d, want %d (ExitUsage)\n%s", code, ExitUsage, out)
	}
	for _, want := range []string{`"demo deploy"`, "@putnami/demo-extension", "@putnami/rival-extension"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ambiguity error does not name %s:\n%s", want, out)
		}
	}
}

func runAlias(t *testing.T, wsRoot string, argv []string, want int) {
	t.Helper()
	var code int
	out := captureStdoutStderr(t, func() {
		app, err := NewApp()
		if err != nil {
			t.Fatalf("NewApp: %v", err)
		}
		t.Chdir(wsRoot)
		code = app.Run(context.Background(), argv)
	})
	if code != want {
		t.Fatalf("%v exit = %d, want %d\n%s", argv, code, want, out)
	}
}

func requireShell(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable on this platform")
	}
}

func writeAliasFixtureScript(t *testing.T, wsRoot, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wsRoot, "demo-extension", "fixture.sh"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fixture script: %v", err)
	}
}

// rivalDemoGroup makes the rival extension claim the SAME `demo deploy`
// subcommand as the fixture extension, which is the ambiguity the dispatcher
// must refuse rather than resolve by discovery order.
const rivalDemoGroup = `
  "commandGroups": {
    "demo": {
      "description": "Rival demo group",
      "subcommands": { "deploy": { "command": "deploy" } }
    }
  },`

// writeRivalDeployExtension adds a second extension declaring the same flat
// "deploy" command. commandGroups is a JSON fragment (see rivalDemoGroup) that
// makes it also claim the alias group; empty leaves the group uncontested.
func writeRivalDeployExtension(t *testing.T, wsRoot, commandGroups string) {
	t.Helper()
	extDir := filepath.Join(wsRoot, "rival-extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir rival extension: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "putnami.json"), []byte(`{"name":"@putnami/rival-extension"}`), 0o644); err != nil {
		t.Fatalf("write rival putnami.json: %v", err)
	}
	manifest := `{
  "name": "@putnami/rival-extension",
  "version": "0.1.0",
  "cliContract": 4,` + commandGroups + `
  "commands": {
    "deploy": {
      "description": "Rival deploy fixture.",
      "activationFiles": ["demo.deploy"],
      "run": [{ "id": "deploy", "task": "rival-task" }]
    }
  },
  "tasks": {
    "rival-task": {
      "kind": "command",
      "command": "/bin/sh",
      "args": ["-c", "exit 0"],
      "cache": false,
      "timeoutMs": 10000
    }
  }
}`
	if err := os.WriteFile(filepath.Join(extDir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write rival manifest: %v", err)
	}

	wsManifest := `{
  "name": "fixture-ws",
  "includes": ["demo-extension", "rival-extension", "app"]
}`
	if err := os.WriteFile(filepath.Join(wsRoot, "putnami.workspace.json"), []byte(wsManifest), 0o644); err != nil {
		t.Fatalf("rewrite workspace: %v", err)
	}
	appManifest := `{"name":"app","extensions":["@putnami/demo-extension","@putnami/rival-extension"]}`
	if err := os.WriteFile(filepath.Join(wsRoot, "app", "putnami.json"), []byte(appManifest), 0o644); err != nil {
		t.Fatalf("rewrite app putnami.json: %v", err)
	}
}

func initAliasGitRepo(t *testing.T, dir string) {
	t.Helper()
	runAliasGit(t, dir, "init")
	runAliasGit(t, dir, "config", "user.email", "test@test.com")
	runAliasGit(t, dir, "config", "user.name", "Test")
	runAliasGit(t, dir, "config", "commit.gpgsign", "false")
	runAliasGit(t, dir, "add", "-A")
	runAliasGit(t, dir, "commit", "-m", "initial")
	runAliasGit(t, dir, "branch", "-M", "main")
}

func runAliasGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
