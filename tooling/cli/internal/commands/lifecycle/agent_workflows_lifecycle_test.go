package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	sdkagentartifact "go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// The agent-content lifecycle acceptance matrix. Every case below runs the
// real command entry point — WorkspaceInit, Install, the upgrade phase, the
// ensure and reconcile passes — against a real
// workspace on disk. Extensions are packaged by the SDK's real package step
// and served from a local registry; nothing between the archive bytes and the
// workspace files is replaced.
//
// A legacy agent artifact — a separately declared, separately pinned archive
// an earlier CLI installed — is never installed here. Where a case needs a
// clone that installed one, it writes that clone's state directly: the files,
// the ownership record, the lock pin and the declaration.

// testAgentArtifact is the first-party legacy artifact the fixture extension
// supersedes.
const testAgentArtifact = "@putnami/agent-workflows"

// secondAgentArtifact is a legacy artifact no extension supersedes.
const secondAgentArtifact = "@acme/agent-workflows"

// installStateMarker is the post-install bootstrap marker. It is the ONE file a
// successful `putnami install` rewrites on every run by design, so a no-op
// assertion excludes it by name rather than by widening the comparison.
const installStateMarker = ".putnami/install-state.json"

// agentSurfacePrefixes is what this feature owns: the materialized trees, the
// gitignored ownership record, and the lock. Failure-path assertions compare
// exactly this surface, because the commands under test legitimately write
// other files (the AI context, the bootstrap marker) before they reach the
// agent phase.
var agentSurfacePrefixes = []string{".agents", ".claude", ".codex", ".putnami/agent-artifacts", lockfile.LockFilename}

// snapshotWorkspace records every path under root with its content digest,
// including putnami.lock.json and the materializer's own ownership state, so
// "changed nothing" is asserted over the whole tree rather than over the files
// a test happened to think of. ignore drops exact workspace-relative paths.
func snapshotWorkspace(t *testing.T, root string, ignore ...string) map[string]string {
	t.Helper()
	skip := make(map[string]bool, len(ignore))
	for _, path := range ignore {
		skip[path] = true
	}
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		slashed := filepath.ToSlash(rel)
		if skip[slashed] {
			return nil
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			out[slashed] = "symlink:" + target
		case info.IsDir():
			out[slashed] = "dir"
		case info.Mode().IsRegular():
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			out[slashed] = "file:" + lockfile.HashBytes(data)
		default:
			out[slashed] = "other:" + info.Mode().String()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

// snapshotAgentSurface records only the paths this feature owns.
func snapshotAgentSurface(t *testing.T, root string) map[string]string {
	t.Helper()
	full := snapshotWorkspace(t, root)
	out := make(map[string]string, len(full))
	for path, value := range full {
		for _, prefix := range agentSurfacePrefixes {
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				out[path] = value
				break
			}
		}
	}
	return out
}

// assertNoAgentSurface fails when any agent content reached the workspace at
// all.
func assertNoAgentSurface(t *testing.T, root string) {
	t.Helper()
	for _, path := range []string{".agents", ".claude", ".codex", ".putnami/agent-artifacts"} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Errorf("%s exists in a workspace that never received agent content: %v", path, err)
		}
	}
}

func assertUnchangedWorkspace(t *testing.T, before, after map[string]string) {
	t.Helper()
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("%s disappeared", path)
			continue
		}
		if got != want {
			t.Errorf("%s changed: %s -> %s", path, want, got)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s appeared", path)
		}
	}
}

func readAgentFile(t *testing.T, root, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func writeAgentFile(t *testing.T, root, path, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readWorkspaceFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// runInstall runs the combined install with the workspace-install job stubbed:
// these tests exercise Install's agent phase, not the engine.
func runInstall(t *testing.T, root string) (string, error) {
	t.Helper()
	var buf strings.Builder
	_, err := captureStdout(t, func() error {
		return Install(context.Background(), root, wsproto.Load(root), nil, installTestEnv(&buf, WorkspaceJobOK))
	})
	return buf.String(), err
}

// agentWorkspaceDeclaring writes a workspace config declaring exactly these
// agentArtifacts entries.
func agentWorkspaceDeclaring(t *testing.T, references ...string) string {
	t.Helper()
	root := t.TempDir()
	config := `{"name":"agent-ws"}`
	if len(references) > 0 {
		config = `{"name":"agent-ws","agentArtifacts":` + jsonStrings(references) + `}`
	}
	if err := os.WriteFile(filepath.Join(root, wsproto.WorkspaceConfigFilename), []byte(config+"\n"), 0o644); err != nil {
		t.Fatalf("write workspace config: %v", err)
	}
	return root
}

// seedLegacyArtifact writes the state a clone reaches after an earlier CLI
// installed one legacy artifact: its files on disk, its ownership record and,
// when pinned, its v4 lock pin. It returns the pin.
func seedLegacyArtifact(t *testing.T, root, name, version string, files map[string]string, pinned bool) lockfile.AgentArtifactLockEntry {
	t.Helper()
	entry := lockfile.AgentArtifactLockEntry{
		Version:      version,
		Integrity:    lockfile.HashBytes([]byte("archive:" + name + "@" + version)),
		ManifestHash: lockfile.HashBytes([]byte("manifest:" + name + "@" + version)),
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	state := &agentartifacts.State{
		Version:         agentartifacts.StateVersion,
		Name:            name,
		ArtifactVersion: entry.Version,
		ArchiveDigest:   entry.Integrity,
		ManifestHash:    entry.ManifestHash,
	}
	for _, path := range paths {
		writeAgentFile(t, root, path, files[path])
		state.Files = append(state.Files, agentartifacts.File{Path: path, SHA256: lockfile.HashBytes([]byte(files[path]))})
	}
	if err := agentartifacts.WriteState(root, state); err != nil {
		t.Fatal(err)
	}
	if pinned {
		lock, err := lockfile.ReadLockFile(root)
		if err != nil {
			t.Fatal(err)
		}
		if lock == nil {
			lock = lockfile.NewLockFile()
		}
		lock.SetAgentArtifact(name, entry)
		if err := lockfile.WriteLockFile(root, lock); err != nil {
			t.Fatal(err)
		}
	}
	return entry
}

func agentPinNamed(t *testing.T, root, name string) (lockfile.AgentArtifactLockEntry, bool) {
	t.Helper()
	lf, err := lockfile.ReadLockFile(root)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if lf == nil {
		return lockfile.AgentArtifactLockEntry{}, false
	}
	return lf.GetAgentArtifact(name)
}

// starterOptsInto points one built-in starter at exactly this extension's
// agent content (none at all when name is empty) for the duration of a test.
// The starter table is production DATA, so this substitutes the datum rather
// than the code path under test.
func starterOptsInto(t *testing.T, starter, name string) {
	t.Helper()
	original, ok := initExtensions[starter]
	if !ok {
		t.Fatalf("unknown starter %q", starter)
	}
	updated := original
	updated.agentContent = name
	initExtensions[starter] = updated
	t.Cleanup(func() { initExtensions[starter] = original })
}

// productionInstallExtension is init's real extension install, captured before
// any test replaces the seam.
var productionInstallExtension = installExtension

// recordInitExtensionInstalls wraps the init extension-install seam: it
// records every name set init installs and installs the fixture content
// extension for real, from the test's registry.
func recordInitExtensionInstalls(t *testing.T, failure error) *[][]string {
	t.Helper()
	calls := &[][]string{}
	stubbed := installExtension
	installExtension = func(ctx context.Context, root string, cfg *wsproto.Config, names []string, format string) error {
		*calls = append(*calls, append([]string(nil), names...))
		if len(names) == 1 && names[0] == contentExtensionName {
			if failure != nil {
				return failure
			}
			return productionInstallExtension(ctx, root, cfg, names, format)
		}
		return stubbed(ctx, root, cfg, names, format)
	}
	t.Cleanup(func() { installExtension = stubbed })
	return calls
}

// A starter that opts into nothing adds nothing: no declaration, no extension,
// no install, no file.
func TestAgentWorkflows_InitWithoutOptInStarterAddsNothing(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "optin", "an-absent-declaration-skips-the-phase")
	root := t.TempDir()
	t.Chdir(root)
	hometest.Temp(t)
	stubWorkspaceInitStages(t, "", 0)
	starterOptsInto(t, "go", "")
	calls := recordInitExtensionInstalls(t, nil)

	if _, err := captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), "", []string{"--workspace", "agent-ws", "--extension", "go"}, LifecycleEnv{})
	}); err != nil {
		t.Fatalf("WorkspaceInit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, wsproto.WorkspaceConfigFilename))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "agentArtifacts") || strings.Contains(string(data), contentExtensionName) {
		t.Errorf("init declared agent content nobody asked for:\n%s", data)
	}
	if fmt.Sprint(*calls) != "[[@putnami/go]]" {
		t.Errorf("init installed %v, want only the language extension", *calls)
	}
	assertNoAgentSurface(t, root)
}

// A workspace that declares no agentArtifacts is untouched by install.
func TestAgentWorkflows_InstallWithoutDeclarationIsANoOp(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "optin", "an-absent-declaration-skips-the-phase")
	root := agentWorkspaceDeclaring(t)
	hometest.Temp(t)

	out, err := runInstall(t, root)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if strings.Contains(out, "Installing agent workflows") {
		t.Errorf("install announced a phase it must skip:\n%s", out)
	}
	assertNoAgentSurface(t, root)
}

// The production starter table, unstubbed: no built-in starter opts into agent
// content, so a new workspace holds none until someone asks for it.
func TestBuiltInStartersOptIntoNoAgentContent(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-agent-content-opt-in", "no-built-in-starter-opts-into-agent-content")
	if len(initExtensions) == 0 {
		t.Fatal("no built-in starters, so this check proves nothing")
	}
	for name, starter := range initExtensions {
		if starter.agentContent != "" {
			t.Errorf("starter %q opts into %q, want no agent content", name, starter.agentContent)
		}
		if got := initDeclaredExtensions(starter); strings.Join(got, ",") != starter.packageName {
			t.Errorf("starter %q declares extensions %v, want only %s", name, got, starter.packageName)
		}
		if got := initAgentArtifacts(t.TempDir(), starter); len(got) != 0 {
			t.Errorf("starter %q declares agentArtifacts %v, want none", name, got)
		}
	}
}

// init declares the starter's extension and opts into its content, installs
// that extension, and materializes the content at the release it pinned.
func TestAgentWorkflows_InitInstallsTheStarterContent(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-agent-content-opt-in", "init-installs-the-extension-and-its-content")
	registry := newContentRegistry(t)
	pkg := packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false)
	registry.publish(pkg)
	root := t.TempDir()
	t.Chdir(root)
	stubWorkspaceInitStages(t, "", 0)
	starterOptsInto(t, "go", contentExtensionName)
	recordInitExtensionInstalls(t, nil)

	if out, err := captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), "", []string{"--workspace", "agent-ws", "--extension", "go"}, LifecycleEnv{})
	}); err != nil {
		t.Fatalf("WorkspaceInit: %v\n%s", err, out)
	}
	config := readWorkspaceConfigAt(root)
	if config == nil || strings.Join(config.AgentArtifacts, ",") != contentExtensionOptIn {
		t.Fatalf("workspace config agentArtifacts = %+v, want the starter's opt-in", config)
	}
	if got := strings.Join(config.Extensions.Names(), ","); got != contentExtensionName+",@putnami/go" {
		t.Fatalf("workspace config extensions = %s", got)
	}
	if got := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md"); !strings.Contains(got, "Review revision r1.") {
		t.Fatalf("init did not materialize the content: %q", got)
	}
	if pin, ok := extensionPin(t, root, contentExtensionName); !ok || pin.Version != "1.0.0" {
		t.Fatalf("extension pin = %+v (present %v)", pin, ok)
	}
	assertNoAgentArtifactPins(t, root)
}

// Until the starter's extension resolves, init keeps both declarations,
// writes no agent file, reports the failure, and still finishes: dependencies
// and the project matter more than workflow files the user can install later.
func TestAgentWorkflows_InitWithUninstallableStarterContentPointsAtInstall(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-agent-content-opt-in", "a-failed-starter-install-points-at-install")
	root := t.TempDir()
	t.Chdir(root)
	hometest.Temp(t)
	stubWorkspaceInitStages(t, "", 0)
	starterOptsInto(t, "go", contentExtensionName)
	recordInitExtensionInstalls(t, errors.New("registry answered HTTP 404"))

	out, err := captureStdout(t, func() error {
		return WorkspaceInit(context.Background(), "", []string{"--workspace", "agent-ws", "--project", "app", "--extension", "go"}, LifecycleEnv{})
	})
	if err != nil {
		t.Fatalf("init must finish without its agent content: %v", err)
	}
	if !strings.Contains(out, "Agent content: install extension "+contentExtensionName) || !strings.Contains(out, "run `putnami install`") {
		t.Fatalf("init did not report the failure and the command that installs it later:\n%s", out)
	}
	config := readWorkspaceConfigAt(root)
	if config == nil || strings.Join(config.AgentArtifacts, ",") != contentExtensionOptIn {
		t.Fatalf("workspace config agentArtifacts = %+v, want the starter's opt-in kept", config)
	}
	assertNoAgentSurface(t, root)
}

// Re-initializing merges by what each entry names, and the committed entry
// wins: an existing opt-in keeps its place, and a legacy entry survives so the
// next command refuses it instead of orphaning its files.
func TestAgentWorkflows_InitPreservesAnExistingOptIn(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "reinit-preserves-optin", "init-force-unions-the-declaration")
	root := agentWorkspaceDeclaring(t, "extension:@acme/other", testAgentArtifact+":1.4.x", "extension:"+contentExtensionName)
	got := initAgentArtifacts(root, initExtensionConfig{packageName: "@putnami/go", agentContent: contentExtensionName})
	want := []string{"extension:@acme/other", "extension:" + contentExtensionName, testAgentArtifact + ":1.4.x"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("initAgentArtifacts = %v, want %v", got, want)
	}
	if got := initAgentArtifacts(t.TempDir(), initExtensionConfig{packageName: "@putnami/go", agentContent: contentExtensionName}); strings.Join(got, ",") != "extension:"+contentExtensionName {
		t.Fatalf("a fresh directory declares %v, want only the starter's opt-in", got)
	}
}

// A clone an earlier CLI installed a legacy artifact into — its declaration,
// its pin, its record and its files — is refused by install and upgrade with
// nothing written, and both say how to replace the declaration.
func TestAgentWorkflows_LegacyStateIsRefusedWithNothingWritten(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "legacy-declarations-refused", "install-and-upgrade-refuse-legacy-state-with-nothing-written")
	root := agentWorkspaceDeclaring(t, testAgentArtifact+":stable")
	hometest.Temp(t)
	seedLegacyArtifact(t, root, testAgentArtifact, "1.4.2", map[string]string{".agents/skills/fix/SKILL.md": "# fix\n"}, true)
	before := snapshotAgentSurface(t, root)

	_, err := runInstall(t, root)
	if err == nil || !strings.Contains(err.Error(), "no longer installs") || !strings.Contains(err.Error(), "replace each entry with extension:@putnami/contributor") {
		t.Fatalf("install error = %v, want the legacy declaration refused and its first-party successor named", err)
	}
	err = agentctx.AdoptAgentWorkflows(context.Background(), root, wsproto.Load(root), nil)
	if !errors.Is(err, protocolcli.ErrInvalidConfig) || !strings.Contains(err.Error(), "no longer installs") {
		t.Fatalf("upgrade error = %v", err)
	}
	var dryRun strings.Builder
	agentctx.DescribeAgentWorkflowPlan(root, wsproto.Load(root), &dryRun)
	if !strings.Contains(dryRun.String(), "no longer installs") || strings.Contains(dryRun.String(), "Next:") {
		t.Fatalf("dry run = %q, want the legacy declaration refused with no command to run", dryRun.String())
	}
	if err := agentctx.EnsureAgentWorkflows(context.Background(), root, wsproto.Load(root)); err == nil {
		t.Fatal("the first-run pass installed a legacy declaration")
	}
	assertUnchangedWorkspace(t, before, snapshotAgentSurface(t, root))
}

// A record the workspace no longer opts into is retired by the next upgrade:
// the files it still manages are removed, the one the user edited is kept and
// reported, then the pin and finally the record are dropped.
func TestAgentWorkflows_UpgradeRetiresARecordNothingDeclares(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "undeclared-artifact-retired", "upgrade-removes-a-withdrawn-owners-files-pin-and-record")
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "undeclared-artifact-retired", "an-edited-file-is-released-not-overwritten")
	root := agentWorkspaceDeclaring(t)
	seedLegacyArtifact(t, root, secondAgentArtifact, "1.0.0", map[string]string{
		".agents/skills/acme-change/SKILL.md": "# acme-change\n",
		".claude/skills/acme-change/SKILL.md": "See the canonical skill\n",
		".agents/skills/acme-check/SKILL.md":  "# acme-check\n",
	}, true)
	writeAgentFile(t, root, ".agents/skills/acme-check/SKILL.md", "my notes\n")

	cfg := wsproto.Load(root)
	applies, err := agentctx.AgentWorkflowPhaseApplies(root, cfg)
	if err != nil || !applies {
		t.Fatalf("phase applies = %v, %v; an undeclared record must still enter the phase", applies, err)
	}
	var dryRun strings.Builder
	agentctx.DescribeAgentWorkflowPlan(root, cfg, &dryRun)
	if !strings.Contains(dryRun.String(), "Would remove "+secondAgentArtifact) {
		t.Fatalf("dry run = %q, want the retirement described", dryRun.String())
	}

	var out strings.Builder
	if err := agentctx.AdoptAgentWorkflows(context.Background(), root, cfg, &out); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	for _, removed := range []string{".agents/skills/acme-change/SKILL.md", ".claude/skills/acme-change/SKILL.md"} {
		assertFileMissing(t, root, removed)
	}
	if got := readWorkspaceFile(t, root, ".agents/skills/acme-check/SKILL.md"); got != "my notes\n" {
		t.Fatalf("the edited file was not preserved: %q", got)
	}
	if !strings.Contains(out.String(), "kept your edited files") {
		t.Fatalf("report = %q, want the released file named", out.String())
	}
	if _, pinned := agentPinNamed(t, root, secondAgentArtifact); pinned {
		t.Fatal("the pin of a retired owner must be dropped")
	}
	if state, err := agentartifacts.LoadState(root, secondAgentArtifact); err != nil || state != nil {
		t.Fatalf("ownership record = %+v, %v; want it removed last", state, err)
	}
	applies, err = agentctx.AgentWorkflowPhaseApplies(root, wsproto.Load(root))
	if err != nil || applies {
		t.Fatalf("phase applies after retirement = %v, %v", applies, err)
	}
}

// No declaration can use a pin under agentArtifacts: an opt-in follows its
// extension's own pin. An explicit install drops every one and leaves the
// records and files alone, which upgrade retires. The implicit bootstrap
// restores the committed lock as it is, and a hosted install, which writes no
// lock, fails before its first phase and names the pins instead of dropping
// them.
func TestInstall_DropsEveryAgentArtifactPin(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "legacy-pins-dropped", "an-explicit-install-drops-every-pin")
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "legacy-pins-dropped", "a-hosted-install-that-would-drop-a-pin-fails")
	hometest.Temp(t)
	seeded := func(t *testing.T) string {
		t.Helper()
		root := agentWorkspaceDeclaring(t)
		seedLegacyArtifact(t, root, testAgentArtifact, "1.4.2", map[string]string{".agents/skills/fix/SKILL.md": "# fix\n"}, true)
		seedLegacyArtifact(t, root, secondAgentArtifact, "1.0.0", map[string]string{".agents/skills/acme-change/SKILL.md": "# acme-change\n"}, true)
		return root
	}
	var jobs []string
	install := func(ctx context.Context, root string) error {
		var out strings.Builder
		env := installTestEnv(&out, WorkspaceJobOK)
		env.RunJob = func(ctx context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
			jobs = append(jobs, recordedJob(ctx, req))
			return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
		}
		env.BeforeRepositoryCode = func(context.Context) error {
			jobs = append(jobs, beforeRepositoryCode)
			return nil
		}
		_, err := captureStdout(t, func() error {
			return Install(ctx, root, wsproto.Load(root), nil, env)
		})
		return err
	}

	t.Run("explicit", func(t *testing.T) {
		root := seeded(t)
		if err := install(context.Background(), root); err != nil {
			t.Fatalf("Install: %v", err)
		}
		assertNoAgentArtifactPins(t, root)
		for _, name := range []string{testAgentArtifact, secondAgentArtifact} {
			if state, err := agentartifacts.LoadState(root, name); err != nil || state == nil {
				t.Fatalf("ownership record of %s = %+v, %v; install retires nothing", name, state, err)
			}
		}
		if got := readWorkspaceFile(t, root, ".agents/skills/fix/SKILL.md"); got != "# fix\n" {
			t.Fatalf("install touched a recorded file: %q", got)
		}
	})

	t.Run("implicit", func(t *testing.T) {
		spectest.Proves(t, "cli/agent-workflow-lifecycle", "legacy-pins-dropped", "the-implicit-bootstrap-keeps-the-lock")
		root := seeded(t)
		before := readLock(t, root)
		if err := install(shared.WithImplicitInstall(context.Background()), root); err != nil {
			t.Fatalf("implicit Install: %v", err)
		}
		if after := readLock(t, root); string(after) != string(before) {
			t.Fatalf("the implicit bootstrap changed the committed lock:\n%s\nto\n%s", before, after)
		}
	})

	t.Run("hosted", func(t *testing.T) {
		root := seeded(t)
		before := readLock(t, root)
		t.Cleanup(runcredential.SetForTest("run-bearer"))
		jobs = nil
		err := install(context.Background(), root)
		if err == nil || !strings.Contains(err.Error(), "a hosted run writes no lock") ||
			!strings.Contains(err.Error(), secondAgentArtifact+", "+testAgentArtifact) {
			t.Fatalf("hosted Install = %v, want it refused, naming both pins", err)
		}
		if len(jobs) != 0 {
			t.Fatalf("a hosted install ran %q before it refused the pins", jobs)
		}
		if after := readLock(t, root); string(after) != string(before) {
			t.Fatalf("a hosted install changed the committed lock:\n%s\nto\n%s", before, after)
		}
	})
}

// An unparseable workspace config answers as an empty one. Retirement must not
// read that as "the user removed every declaration".
func TestAgentWorkflows_UnreadableConfigNeverRetires(t *testing.T) {
	root := agentWorkspaceDeclaring(t)
	seedLegacyArtifact(t, root, secondAgentArtifact, "1.0.0", map[string]string{".agents/skills/acme-change/SKILL.md": "# acme-change\n"}, true)
	if err := os.WriteFile(filepath.Join(root, wsproto.WorkspaceConfigFilename), []byte(`{"name":"agent-ws",}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotWorkspace(t, root)
	cfg := wsproto.Load(root)
	if _, err := agentctx.AgentWorkflowPhaseApplies(root, cfg); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("phase check error = %v, want an unreadable-config refusal", err)
	}
	if err := agentctx.AdoptAgentWorkflows(context.Background(), root, cfg, nil); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("upgrade error = %v, want an unreadable-config refusal", err)
	}
	assertUnchangedWorkspace(t, before, snapshotWorkspace(t, root))
}

// Extension-owned agent content, end to end. Every case below packages a
// content-only extension with the SDK's real package step, serves the archive
// from a local registry, and drives the real command entry points — Install,
// the upgrade phases, the ensure and reconcile passes — against a real
// workspace on disk. The only double is the registry's HTTP endpoint, which is
// an external boundary: nothing between the archive bytes and the workspace
// files is replaced.

const contentExtensionName = "@fixture/contributor"

const contentExtensionOptIn = "extension:" + contentExtensionName

// contentExtensionSourceFiles is the authored extension at one revision. The
// review skill carries a reference and a helper; the worker is projected onto
// both hosts. withPlan adds a second skill a later revision drops.
func contentExtensionSourceFiles(name, revision string, withPlan bool) map[string]string {
	files := map[string]string{}
	files["putnami.extension.json"] = `{"name":"` + name + `","agentContent":{"path":"agent-content","source":"agent-src"}}`
	files["putnami.json"] = `{"name":"` + name + `","options":{"agent-artifact":{"forbiddenContent":["password"],"requiredSkills":["fixture-review"]}}}`
	files["agent-src/skills/fixture-review/SKILL.md"] = "---\nname: fixture-review\ndescription: Review a change\n---\n\n" +
		"# fixture-review\n\nRead references/guide.md, then run scripts/check.sh. Review revision " + revision + ".\n"
	files["agent-src/skills/fixture-review/references/guide.md"] = "Review guide " + revision + ".\n"
	files["agent-src/skills/fixture-review/scripts/check.sh"] = "#!/bin/sh\necho " + revision + "\n"
	files["agent-src/agents/fixture-reviewer/AGENT.md"] = "Review the diff you are given (" + revision + ").\n"
	files["agent-src/agents/fixture-reviewer/claude.yaml"] = "name: fixture-reviewer\ndescription: Reviewer\n"
	files["agent-src/agents/fixture-reviewer/codex.toml"] = "name = \"fixture-reviewer\"\n"
	if withPlan {
		files["agent-src/skills/fixture-plan/SKILL.md"] = "---\nname: fixture-plan\ndescription: Plan a change\n---\n\n# fixture-plan\n\nPlan revision " + revision + ".\n"
	}
	return files
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// packageContentExtension runs the SDK's package step over a freshly authored
// source tree: build, policy, stamp, loader postcondition, archive.
func packageContentExtension(t *testing.T, name, revision, version string, withPlan bool) *sdkagentartifact.ExtensionPackage {
	t.Helper()
	source := t.TempDir()
	writeTree(t, source, contentExtensionSourceFiles(name, revision, withPlan))
	pkg, err := sdkagentartifact.PackageExtension(source, version)
	if err != nil {
		t.Fatalf("package %s@%s: %v", name, version, err)
	}
	return pkg
}

// contentRegistry serves packaged extensions the way the registry protocol
// does: a channel names a version or `latest`, the response carries the
// resolved version and the archive digest, and the same platform-independent
// bytes answer every os/arch.
type contentRegistry struct {
	mu       sync.Mutex
	packages map[string]map[string]*sdkagentartifact.ExtensionPackage
	latest   map[string]string
	hits     int
}

func newContentRegistry(t *testing.T) *contentRegistry {
	t.Helper()
	registry := &contentRegistry{
		packages: map[string]map[string]*sdkagentartifact.ExtensionPackage{},
		latest:   map[string]string{},
	}
	server := httptest.NewServer(http.HandlerFunc(registry.serve))
	t.Cleanup(server.Close)
	t.Setenv(extension.PutRegistryURLEnv, server.URL)
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	t.Setenv("PUTNAMI_ARTIFACT_DIR", t.TempDir())
	t.Setenv(artifactsEnsuredEnv, "")
	hometest.Temp(t)
	original := extension.ResolveRegistryToken
	extension.ResolveRegistryToken = func(string) (string, string) { return "", "" }
	t.Cleanup(func() { extension.ResolveRegistryToken = original })
	return registry
}

// publish makes pkg resolvable at its version and moves `latest` to it.
func (r *contentRegistry) publish(pkg *sdkagentartifact.ExtensionPackage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.packages[pkg.Name] == nil {
		r.packages[pkg.Name] = map[string]*sdkagentartifact.ExtensionPackage{}
	}
	r.packages[pkg.Name][pkg.Version] = pkg
	r.latest[pkg.Name] = pkg.Version
}

func (r *contentRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hits++
	for name, versions := range r.packages {
		ns, pkgName := extension.RegistryRef(name)
		if req.URL.Path != "/"+ns+"/"+pkgName+"/download" {
			continue
		}
		version := req.URL.Query().Get("channel")
		if version == "latest" {
			version = r.latest[name]
		}
		pkg := versions[version]
		if pkg == nil {
			break
		}
		w.Header().Set("X-Resolved-Version", pkg.Version)
		w.Header().Set("X-Integrity", pkg.ArchiveSHA256)
		_, _ = w.Write(pkg.Archive)
		return
	}
	http.NotFound(w, req)
}

// contentWorkspace declares the extensions and the agentArtifacts entries given.
func contentWorkspace(t *testing.T, extensions []string, agentArtifacts []string) string {
	t.Helper()
	root := t.TempDir()
	writeContentWorkspaceConfig(t, root, extensions, agentArtifacts)
	return root
}

func writeContentWorkspaceConfig(t *testing.T, root string, extensions []string, agentArtifacts []string) {
	t.Helper()
	config := fmt.Sprintf(`{"name":"content-ws","extensions":%s`, jsonStrings(extensions))
	if len(agentArtifacts) > 0 {
		config += `,"agentArtifacts":` + jsonStrings(agentArtifacts)
	}
	config += "}\n"
	if err := os.WriteFile(filepath.Join(root, wsproto.WorkspaceConfigFilename), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

func jsonStrings(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return "[" + strings.Join(quoted, ",") + "]"
}

// runContentUpgrade runs the registry half of `putnami upgrade` — extensions,
// templates and agent workflows — at the `latest` channel.
func runContentUpgrade(t *testing.T, root string) {
	t.Helper()
	selector := upgradeSelector{Mode: "channel", Display: "stable", RegistryTarget: "latest", DepsTarget: "latest"}
	var failed bool
	out, err := captureStdout(t, func() error {
		failed = upgradeArtifactPhases(context.Background(), root, wsproto.Load(root), selector, UpgradeFlags{Extensions: true})
		return nil
	})
	if err != nil || failed {
		t.Fatalf("upgrade failed (err %v):\n%s", err, out)
	}
}

func extensionPin(t *testing.T, root, name string) (lockfile.LockEntry, bool) {
	t.Helper()
	lf, err := lockfile.ReadLockFile(root)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if lf == nil {
		return lockfile.LockEntry{}, false
	}
	return lf.GetExtension(name)
}

func assertNoAgentArtifactPins(t *testing.T, root string) {
	t.Helper()
	lf, err := lockfile.ReadLockFile(root)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if lf != nil && len(lf.AgentArtifacts) != 0 {
		t.Fatalf("extension-owned content was pinned under agentArtifacts: %+v", lf.AgentArtifacts)
	}
}

// contentSurfacePrefixes is what extension-owned content writes: the three
// host trees and the ownership record. The lock is not in it, because the
// extension's own pin moves in the extensions phase of the same command.
var contentSurfacePrefixes = []string{".agents", ".claude", ".codex", ".putnami/agent-artifacts"}

func snapshotContentSurface(t *testing.T, root string) map[string]string {
	t.Helper()
	full := snapshotWorkspace(t, root)
	out := make(map[string]string, len(full))
	for path, value := range full {
		for _, prefix := range contentSurfacePrefixes {
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				out[path] = value
				break
			}
		}
	}
	return out
}

func assertFileMissing(t *testing.T, root, rel string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Errorf("%s exists: %v", rel, err)
	}
}

// The first executable vertical: a content-only extension is packaged, served,
// installed into a fresh workspace, read through each host's entry point,
// reinstalled as a byte-for-byte no-op, upgraded, and finally removed — with
// only its own version to resolve, and a user edit surviving the removal.
func TestExtensionAgentContent_InstallReinstallUpgradeRemove(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "extension-agent-content", "an-opted-in-extension-installs-upgrades-and-retires-its-content")
	registry := newContentRegistry(t)
	v1 := packageContentExtension(t, contentExtensionName, "r1", "1.0.0", true)
	registry.publish(v1)
	root := contentWorkspace(t, []string{contentExtensionName}, []string{contentExtensionOptIn})

	// Install.
	out, err := runInstall(t, root)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Agent workflow "+contentExtensionName+"@1.0.0 materialized") {
		t.Errorf("install did not report the content it materialized:\n%s", out)
	}
	claudeSkill := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md")
	if !strings.HasPrefix(claudeSkill, "---\nname: fixture-review\n") || !strings.Contains(claudeSkill, "Review revision r1.") {
		t.Fatalf("Claude host entry = %q", claudeSkill)
	}
	codexWorker := readAgentFile(t, root, ".codex/agents/fixture-reviewer.toml")
	if !strings.Contains(codexWorker, "developer_instructions = '''\nReview the diff you are given (r1).\n'''") {
		t.Fatalf("Codex host entry = %q", codexWorker)
	}
	if got := readAgentFile(t, root, ".claude/agents/fixture-reviewer.md"); !strings.Contains(got, "Review the diff you are given (r1).") {
		t.Fatalf("Claude worker = %q", got)
	}
	if got := readAgentFile(t, root, ".agents/skills/fixture-review/scripts/check.sh"); got != "#!/bin/sh\necho r1\n" {
		t.Fatalf("helper = %q", got)
	}
	readAgentFile(t, root, ".agents/skills/fixture-plan/SKILL.md")

	pin, ok := extensionPin(t, root, contentExtensionName)
	if !ok || pin.Version != "1.0.0" || pin.ManifestHash != lockfile.HashBytes(v1.Manifest) {
		t.Fatalf("extension pin = %+v (present %v), want 1.0.0 bound to the packaged manifest", pin, ok)
	}
	assertNoAgentArtifactPins(t, root)
	state, err := agentartifacts.LoadState(root, contentExtensionName)
	if err != nil || state == nil {
		t.Fatalf("ownership state = %+v, err %v", state, err)
	}
	if state.ArtifactVersion != "1.0.0" || state.ArchiveDigest != pin.ManifestHash || state.ManifestHash != v1.Content.ManifestSHA256 {
		t.Fatalf("ownership state %+v does not name the pinned extension release", state)
	}

	// Reinstall: nothing moves.
	before := snapshotWorkspace(t, root, installStateMarker)
	out, err = runInstall(t, root)
	if err != nil {
		t.Fatalf("reinstall: %v\n%s", err, out)
	}
	assertUnchangedWorkspace(t, before, snapshotWorkspace(t, root, installStateMarker))
	if strings.Contains(out, "Agent workflow ") {
		t.Errorf("a repeat install reported unchanged content as an action:\n%s", out)
	}

	// Upgrade: the content follows the extension's own version.
	v2 := packageContentExtension(t, contentExtensionName, "r2", "2.0.0", false)
	registry.publish(v2)
	runContentUpgrade(t, root)
	if got := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md"); !strings.Contains(got, "Review revision r2.") {
		t.Fatalf("upgraded Claude host entry = %q", got)
	}
	assertFileMissing(t, root, ".agents/skills/fixture-plan/SKILL.md")
	assertFileMissing(t, root, ".claude/skills/fixture-plan/SKILL.md")
	if pin, _ := extensionPin(t, root, contentExtensionName); pin.Version != "2.0.0" {
		t.Fatalf("extension pin after upgrade = %+v", pin)
	}
	assertNoAgentArtifactPins(t, root)
	if state, _ := agentartifacts.LoadState(root, contentExtensionName); state == nil || state.ArtifactVersion != "2.0.0" {
		t.Fatalf("ownership state after upgrade = %+v", state)
	}

	// Remove: withdraw the opt-in. The extension stays installed; its content
	// is retired, and the one file the user edited is kept, unmanaged.
	writeAgentFile(t, root, ".agents/skills/fixture-review/references/guide.md", "my notes\n")
	writeContentWorkspaceConfig(t, root, []string{contentExtensionName}, nil)
	runContentUpgrade(t, root)
	for _, rel := range []string{
		".agents/skills/fixture-review/SKILL.md",
		".claude/skills/fixture-review/SKILL.md",
		".claude/agents/fixture-reviewer.md",
		".codex/agents/fixture-reviewer.toml",
	} {
		assertFileMissing(t, root, rel)
	}
	if got := readAgentFile(t, root, ".agents/skills/fixture-review/references/guide.md"); got != "my notes\n" {
		t.Fatalf("the user's edit was not preserved: %q", got)
	}
	if state, err := agentartifacts.LoadState(root, contentExtensionName); err != nil || state != nil {
		t.Fatalf("ownership state after removal = %+v, err %v; want none", state, err)
	}
	if _, ok := extensionPin(t, root, contentExtensionName); !ok {
		t.Fatal("withdrawing the content opt-in must not remove the extension")
	}
}

// Installing an extension that carries content activates nothing until the
// workspace opts in.
func TestExtensionAgentContent_WithoutOptInActivatesNothing(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "extension-agent-content", "an-extension-without-the-opt-in-activates-nothing")
	registry := newContentRegistry(t)
	registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
	root := contentWorkspace(t, []string{contentExtensionName}, nil)

	if out, err := runInstall(t, root); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if _, ok := extensionPin(t, root, contentExtensionName); !ok {
		t.Fatal("the extension itself must still install")
	}
	assertNoAgentSurface(t, root)
}

// seededContentWorkspace installs v1 of the fixture extension into a fresh
// workspace and returns it, so a case can act on a settled install.
func seededContentWorkspace(t *testing.T) (string, *contentRegistry) {
	t.Helper()
	registry := newContentRegistry(t)
	registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
	root := contentWorkspace(t, []string{contentExtensionName}, []string{contentExtensionOptIn})
	if out, err := runInstall(t, root); err != nil {
		t.Fatalf("seed install: %v\n%s", err, out)
	}
	return root, registry
}

// Each way a target can be someone else's aborts the whole run before the
// first write: an unrecorded file, a user edit, a symlinked parent, and a
// second extension claiming the same paths.
func TestExtensionAgentContent_CollisionsAbortWithZeroWrites(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "extension-agent-content-integrity", "extension-content-collisions-abort-before-the-first-write")

	t.Run("an unrecorded file", func(t *testing.T) {
		registry := newContentRegistry(t)
		registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
		root := contentWorkspace(t, []string{contentExtensionName}, []string{contentExtensionOptIn})
		writeAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md", "# my own skill\n")

		_, err := runInstall(t, root)
		if !errors.Is(err, agentartifacts.ErrCollision) {
			t.Fatalf("error = %v, want a collision", err)
		}
		if !strings.Contains(err.Error(), ".claude/skills/fixture-review/SKILL.md [unmanaged]") {
			t.Fatalf("the collision must name the file and its reason: %v", err)
		}
		assertFileMissing(t, root, ".agents/skills/fixture-review/SKILL.md")
		assertFileMissing(t, root, ".putnami/agent-artifacts")
		if got := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md"); got != "# my own skill\n" {
			t.Fatalf("the user's file was overwritten: %q", got)
		}
	})

	t.Run("a local edit", func(t *testing.T) {
		root, registry := seededContentWorkspace(t)
		writeAgentFile(t, root, ".agents/skills/fixture-review/SKILL.md", "edited\n")
		registry.publish(packageContentExtension(t, contentExtensionName, "r2", "2.0.0", false))
		before := snapshotContentSurface(t, root)

		// `upgrade` moves the extension, then its content collides on the edit
		// and the whole content write is refused.
		selector := upgradeSelector{Mode: "channel", Display: "stable", RegistryTarget: "latest", DepsTarget: "latest"}
		var failed bool
		if _, err := captureStdout(t, func() error {
			failed = upgradeArtifactPhases(context.Background(), root, wsproto.Load(root), selector, UpgradeFlags{Extensions: true})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !failed {
			t.Fatal("an upgrade whose content collides must report a failure")
		}
		assertUnchangedWorkspace(t, before, snapshotContentSurface(t, root))

		err := agentctx.AdoptAgentWorkflows(context.Background(), root, wsproto.Load(root), nil)
		if !errors.Is(err, agentartifacts.ErrCollision) || !strings.Contains(err.Error(), ".agents/skills/fixture-review/SKILL.md [modified]") {
			t.Fatalf("error = %v, want a collision naming the edited file", err)
		}
		// The extension moved; its content stayed at the release the record
		// names until the edit is resolved.
		if pin, _ := extensionPin(t, root, contentExtensionName); pin.Version != "2.0.0" {
			t.Fatalf("extension pin = %+v, want the upgraded extension", pin)
		}
		if state, _ := agentartifacts.LoadState(root, contentExtensionName); state == nil || state.ArtifactVersion != "1.0.0" {
			t.Fatalf("ownership state = %+v, want the release whose files are on disk", state)
		}
		assertUnchangedWorkspace(t, before, snapshotContentSurface(t, root))
	})

	t.Run("a symlinked parent", func(t *testing.T) {
		registry := newContentRegistry(t)
		registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
		root := contentWorkspace(t, []string{contentExtensionName}, []string{contentExtensionOptIn})
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, ".codex")); err != nil {
			t.Fatal(err)
		}

		if _, err := runInstall(t, root); !errors.Is(err, agentartifacts.ErrCollision) || !strings.Contains(err.Error(), "[symlink]") {
			t.Fatalf("error = %v, want a symlink collision", err)
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("bytes were written through the symlink: %v (err %v)", entries, err)
		}
		assertFileMissing(t, root, ".agents")
		assertFileMissing(t, root, ".claude")
	})

	t.Run("a renamed opt-in keeps the old content until the new one can land", func(t *testing.T) {
		registry := newContentRegistry(t)
		const second = "@fixture/second"
		registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
		registry.publish(packageContentExtension(t, second, "other", "1.0.0", true))
		root := contentWorkspace(t, []string{contentExtensionName, second}, []string{contentExtensionOptIn})
		if out, err := runInstall(t, root); err != nil {
			t.Fatalf("seed install: %v\n%s", err, out)
		}
		// The opt-in moves to an extension that ships the same paths with other
		// bytes, plus one the user already holds.
		const held = ".agents/skills/fixture-plan/SKILL.md"
		writeContentWorkspaceConfig(t, root, []string{contentExtensionName, second}, []string{"extension:" + second})
		writeAgentFile(t, root, held, "# my own plan\n")
		before := snapshotContentSurface(t, root)

		err := agentctx.AdoptAgentWorkflows(context.Background(), root, wsproto.Load(root), nil)
		if !errors.Is(err, agentartifacts.ErrCollision) || !strings.Contains(err.Error(), held+" [unmanaged]") {
			t.Fatalf("error = %v, want the held file named", err)
		}
		if strings.Contains(err.Error(), "fixture-review") {
			t.Fatalf("a path retirement frees was reported as a collision: %v", err)
		}
		assertUnchangedWorkspace(t, before, snapshotContentSurface(t, root))

		// Once the file is moved, the rename lands: the old owner is retired and
		// the new content takes the shared paths.
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(held))); err != nil {
			t.Fatal(err)
		}
		if err := agentctx.AdoptAgentWorkflows(context.Background(), root, wsproto.Load(root), nil); err != nil {
			t.Fatalf("adopt after the file moved: %v", err)
		}
		if state, err := agentartifacts.LoadState(root, contentExtensionName); err != nil || state != nil {
			t.Fatalf("retired record = %+v, err %v; want none", state, err)
		}
		if got := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md"); !strings.Contains(got, "Review revision other.") {
			t.Fatalf("shared path = %q, want the new content", got)
		}
		readAgentFile(t, root, held)
	})

	t.Run("two extensions claiming one path", func(t *testing.T) {
		registry := newContentRegistry(t)
		const second = "@fixture/second"
		registry.publish(packageContentExtension(t, contentExtensionName, "r1", "1.0.0", false))
		registry.publish(packageContentExtension(t, second, "other", "1.0.0", false))
		root := contentWorkspace(t,
			[]string{contentExtensionName, second},
			[]string{contentExtensionOptIn, "extension:" + second})

		_, err := runInstall(t, root)
		if !errors.Is(err, agentartifacts.ErrCollision) || !strings.Contains(err.Error(), "overlapping targets") {
			t.Fatalf("error = %v, want an overlap refusal", err)
		}
		if !strings.Contains(err.Error(), contentExtensionName) || !strings.Contains(err.Error(), second) {
			t.Fatalf("the refusal must name both owners: %v", err)
		}
		assertNoAgentSurface(t, root)
	})
}

// The content is only ever the bytes the extension's pin binds: a tampered
// file or manifest in the installed release is refused before the workspace
// is touched.
func TestExtensionAgentContent_VerifiedPinsRefuseTamperedBytes(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "extension-agent-content-integrity", "extension-content-is-bound-to-the-extension-pin")

	tamper := func(t *testing.T, root, rel, content string) {
		t.Helper()
		installed, err := filepath.EvalSymlinks(layout.StableDir(root, layout.Extensions, contentExtensionName))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(installed, filepath.FromSlash(rel))
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a content file", func(t *testing.T) {
		root, _ := seededContentWorkspace(t)
		for _, rel := range []string{".agents", ".claude", ".codex"} {
			if err := os.RemoveAll(filepath.Join(root, rel)); err != nil {
				t.Fatal(err)
			}
		}
		tamper(t, root, "agent-content/.agents/skills/fixture-review/SKILL.md", "tampered\n")
		before := snapshotContentSurface(t, root)
		err := agentctx.InstallAgentWorkflows(context.Background(), root, wsproto.Load(root), nil)
		if err == nil || !strings.Contains(err.Error(), "drifted from its manifest") {
			t.Fatalf("error = %v, want the digest mismatch", err)
		}
		assertUnchangedWorkspace(t, before, snapshotContentSurface(t, root))
	})

	t.Run("the extension manifest", func(t *testing.T) {
		root, _ := seededContentWorkspace(t)
		tamper(t, root, "putnami.extension.json", fmt.Sprintf(
			`{"name":%q,"version":"1.0.0","cliContract":%d,"agentContent":{"path":"agent-content","manifestSha256":%q}}`,
			contentExtensionName, protocolcli.AgentContentContract, strings.Repeat("0", 64)))
		before := snapshotContentSurface(t, root)
		err := agentctx.InstallAgentWorkflows(context.Background(), root, wsproto.Load(root), nil)
		if err == nil || !strings.Contains(err.Error(), "does not match its pin") {
			t.Fatalf("error = %v, want the pin mismatch", err)
		}
		assertUnchangedWorkspace(t, before, snapshotContentSurface(t, root))
	})
}

// A run interrupted after its files were published converges on a rerun: the
// published bytes re-plan as unchanged, and the record is written then. A file
// a crashed run already wrote is adopted rather than reported as foreign.
func TestExtensionAgentContent_InterruptedRunConvergesOnRerun(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "extension-agent-content-integrity", "an-interrupted-extension-content-run-converges")
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not deny root")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no read-only directory: os.Chmod sets an attribute that does not deny creating files in it")
	}
	root, _ := seededContentWorkspace(t)
	for _, rel := range []string{".agents", ".claude", ".codex", ".putnami/agent-artifacts"} {
		if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	// A crashed run already published one file.
	stored, err := filepath.EvalSymlinks(layout.StableDir(root, layout.Extensions, contentExtensionName))
	if err != nil {
		t.Fatal(err)
	}
	published, err := os.ReadFile(filepath.Join(stored, "agent-content", ".agents", "skills", "fixture-review", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, root, ".agents/skills/fixture-review/SKILL.md", string(published))

	// Deny only the ownership-state write, leaving publication intact.
	stateDir := filepath.Join(root, ".putnami", agentartifacts.StateDirName)
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o555); err != nil {
		t.Fatal(err)
	}
	err = agentctx.InstallAgentWorkflows(context.Background(), root, wsproto.Load(root), nil)
	_ = os.Chmod(stateDir, 0o755)
	if err == nil || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("error = %v, want the convergent-failure report", err)
	}
	readAgentFile(t, root, ".codex/agents/fixture-reviewer.toml")

	if err := agentctx.InstallAgentWorkflows(context.Background(), root, wsproto.Load(root), nil); err != nil {
		t.Fatalf("rerun must converge: %v", err)
	}
	if state, err := agentartifacts.LoadState(root, contentExtensionName); err != nil || state == nil || state.ArtifactVersion != "1.0.0" {
		t.Fatalf("ownership state after the rerun = %+v, err %v", state, err)
	}
}

// The implicit first-run pass restores pinned content without an install, and
// the session-start reconcile reads nothing once the record names the pin.
func TestExtensionAgentContent_EnsureAndReconcileFollowThePin(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "extension-agent-content", "the-first-run-pass-restores-pinned-extension-content")
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "session-start-reconcile", "a-record-at-the-pinned-release-writes-nothing")
	root, _ := seededContentWorkspace(t)
	for _, rel := range []string{".agents", ".claude", ".codex", ".putnami/agent-artifacts"} {
		if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
	if err := agentctx.EnsureAgentWorkflows(context.Background(), root, wsproto.Load(root)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md"); !strings.Contains(got, "Review revision r1.") {
		t.Fatalf("ensure did not restore the content: %q", got)
	}
	before := snapshotWorkspace(t, root)
	if err := agentctx.EnsureAgentWorkflows(context.Background(), root, wsproto.Load(root)); err != nil {
		t.Fatalf("warm ensure: %v", err)
	}
	if err := agentctx.ReconcileAgentWorkflows(context.Background(), root, wsproto.Load(root)); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	assertUnchangedWorkspace(t, before, snapshotWorkspace(t, root))
}

// A local extension is the authoring workspace's own source: its content is
// built on every run with the packager's builder and is never pinned, and
// `context generate` carries a source edit to the host copies.
func TestExtensionAgentContent_LocalSourceIsBuiltAndNeverPinned(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "extension-agent-content", "a-local-extension-builds-its-content-and-is-never-pinned")
	root := contentWorkspace(t, []string{"/tools/contributor"}, []string{contentExtensionOptIn})
	files := contentExtensionSourceFiles(contentExtensionName, "local", false)
	files["putnami.extension.json"] = fmt.Sprintf(`{"name":%q,"cliContract":%d,"agentContent":{"path":"agent-content","source":"agent-src"}}`,
		contentExtensionName, protocolcli.AgentContentContract)
	writeTree(t, filepath.Join(root, "tools", "contributor"), files)

	if err := agentctx.InstallAgentWorkflows(context.Background(), root, wsproto.Load(root), nil); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md"); !strings.Contains(got, "Review revision local.") {
		t.Fatalf("local content = %q", got)
	}
	if lf, _ := lockfile.ReadLockFile(root); lf != nil {
		if _, pinned := lf.GetExtension(contentExtensionName); pinned || len(lf.AgentArtifacts) != 0 {
			t.Fatalf("a local extension's content was pinned: %+v", lf)
		}
	}
	state, err := agentartifacts.LoadState(root, contentExtensionName)
	if err != nil || state == nil || !strings.HasPrefix(state.ArtifactVersion, "0.0.0-local-") {
		t.Fatalf("ownership state = %+v, err %v; want a content-derived local version", state, err)
	}

	writeTree(t, filepath.Join(root, "tools", "contributor"), map[string]string{
		"agent-src/skills/fixture-review/SKILL.md": "---\nname: fixture-review\ndescription: Review a change\n---\n\n# fixture-review\n\nEdited locally.\n",
	})
	changed, err := agentctx.MaterializeLocalAgentContent(root, wsproto.Load(root), nil)
	if err != nil || !changed {
		t.Fatalf("context generate changed=%v err=%v, want the source edit carried over", changed, err)
	}
	if got := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md"); !strings.Contains(got, "Edited locally.") {
		t.Fatalf("the source edit did not reach the host copy: %q", got)
	}
}

func TestExtensionAgentContent_OptInReferences(t *testing.T) {
	root := t.TempDir()
	refs, err := agentctx.DeclaredAgentArtifacts(root, &wsproto.Config{AgentArtifacts: []string{contentExtensionOptIn}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := agentctx.AgentArtifactRef{Name: contentExtensionName, Extension: contentExtensionName}
	if len(refs) != 1 || refs[0] != want {
		t.Fatalf("refs = %+v, want %+v", refs, want)
	}
	for _, declared := range []string{"extension:", "extension: ", contentExtensionOptIn + ":1.0.0", "extension:../../outside", "extension:@Fixture/Contributor"} {
		if _, err := agentctx.DeclaredAgentArtifacts(root, &wsproto.Config{AgentArtifacts: []string{declared}}); err == nil {
			t.Errorf("%q must be refused", declared)
		}
	}
	// A legacy entry beside the opt-in is refused rather than silently picked.
	if _, err := agentctx.DeclaredAgentArtifacts(root, &wsproto.Config{AgentArtifacts: []string{contentExtensionName, contentExtensionOptIn}}); err == nil ||
		!strings.Contains(err.Error(), "no longer installs") {
		t.Fatalf("error = %v, want the legacy entry refused", err)
	}
	// An opt-in naming an extension the workspace does not declare fails.
	undeclared := contentWorkspace(t, nil, []string{contentExtensionOptIn})
	if err := agentctx.InstallAgentWorkflows(context.Background(), undeclared, wsproto.Load(undeclared), nil); err == nil ||
		!strings.Contains(err.Error(), "declares no extension "+contentExtensionName) {
		t.Fatalf("error = %v, want the undeclared extension named", err)
	}

	var out strings.Builder
	agentctx.DescribeAgentWorkflowPlan(undeclared, wsproto.Load(undeclared), &out)
	if !strings.Contains(out.String(), "agent content of extension "+contentExtensionName) {
		t.Fatalf("dry run = %q", out.String())
	}
}

// Agent content that supersedes separately declared artifacts. A consumer
// clone that an earlier CLI installed two pinned legacy artifacts into opts
// into the agent content of an extension that supersedes both. The extension
// is packaged by the SDK and served from the local registry; the legacy state
// is written directly, because no current command installs it.

const supersededMaintainer = "@putnami/maintainer-workflows"

// The paths the three sources share.
const (
	// The core artifact already ships the content's exact bytes.
	supersededUnchanged = ".agents/skills/fixture-review/SKILL.md"
	// Managed by the core artifact; the content ships other bytes.
	supersededChangedCore = ".agents/skills/fixture-review/references/guide.md"
	// Managed by the maintainer artifact; the content ships other bytes.
	supersededChangedMaintainer = ".claude/agents/fixture-reviewer.md"
	// Managed by an artifact; the content does not ship them.
	supersededOnlyCore       = ".agents/skills/legacy-check/SKILL.md"
	supersededOnlyMaintainer = ".agents/skills/legacy-release/SKILL.md"
	// Added by the content.
	supersededAdded = ".codex/agents/fixture-reviewer.toml"
)

// supersedingExtensionFiles is the fixture extension whose content declares
// the artifacts it supersedes.
func supersedingExtensionFiles(revision string, supersedes []string) map[string]string {
	files := contentExtensionSourceFiles(contentExtensionName, revision, false)
	contribution := `{"path":"agent-content","source":"agent-src"`
	if len(supersedes) > 0 {
		contribution += `,"supersedes":` + jsonStrings(supersedes)
	}
	files["putnami.extension.json"] = `{"name":"` + contentExtensionName + `","agentContent":` + contribution + `}}`
	return files
}

func packageSupersedingExtension(t *testing.T, revision, version string, supersedes []string) *sdkagentartifact.ExtensionPackage {
	t.Helper()
	source := t.TempDir()
	writeTree(t, source, supersedingExtensionFiles(revision, supersedes))
	pkg, err := sdkagentartifact.PackageExtension(source, version)
	if err != nil {
		t.Fatalf("package %s@%s: %v", contentExtensionName, version, err)
	}
	return pkg
}

// newSupersededFixture is a consumer clone in which an earlier CLI installed
// the core and the maintainer artifacts at pinned versions, and which declares
// — without opting into — the extension whose content supersedes both. It
// returns the root and the files the content ships.
func newSupersededFixture(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	registry := newContentRegistry(t)
	pkg := packageSupersedingExtension(t, "r1", "1.0.0", []string{testAgentArtifact, supersededMaintainer})
	registry.publish(pkg)
	for _, path := range []string{supersededUnchanged, supersededChangedCore, supersededChangedMaintainer, supersededAdded} {
		if _, ok := pkg.Content.Files[path]; !ok {
			t.Fatalf("the fixture content does not ship %s", path)
		}
	}
	root := contentWorkspace(t, []string{contentExtensionName}, nil)
	if out, err := runInstall(t, root); err != nil {
		t.Fatalf("install the superseding extension: %v\n%s", err, out)
	}
	writeContentWorkspaceConfig(t, root, []string{contentExtensionName}, []string{testAgentArtifact + ":stable", supersededMaintainer + ":stable"})
	seedLegacyArtifact(t, root, testAgentArtifact, "1.4.2", map[string]string{
		supersededUnchanged:   string(pkg.Content.Files[supersededUnchanged]),
		supersededChangedCore: "Review guide of the core workflows.\n",
		supersededOnlyCore:    "# legacy-check\n",
	}, true)
	seedLegacyArtifact(t, root, supersededMaintainer, "1.4.2", map[string]string{
		supersededChangedMaintainer: "The maintainer reviewer.\n",
		supersededOnlyMaintainer:    "# legacy-release\n",
	}, true)
	return root, pkg.Content.Files
}

// No ordinary command installs anything while a clone still declares or
// records an artifact the content supersedes: the legacy declarations, and
// the clone's legacy records once the declarations are gone, are refused with
// nothing written. Deleting the records the refusal names hands the files to
// the content: a file with the content's bytes is kept untouched, one with
// other bytes is a collision the user resolves, and one the content does not
// ship stays unmanaged. The install that succeeds drops the lock pins.
func TestSupersededOwners_AreRefusedUntilTheRecordIsDeleted(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "superseded-owners-refused", "a-clone-that-still-records-a-superseded-artifact-is-refused")
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "superseded-owners-refused", "deleting-the-record-hands-the-files-to-the-content")
	root, content := newSupersededFixture(t)
	surface := snapshotAgentSurface(t, root)

	_, err := runInstall(t, root)
	if err == nil || !strings.Contains(err.Error(), "no longer installs") ||
		!strings.HasSuffix(err.Error(), "entry: replace each entry with "+contentExtensionOptIn+", then run `putnami install`") {
		t.Fatalf("install error = %v, want the legacy declarations refused, the declared successor named and not declared again", err)
	}
	if next := protocolcli.SuggestedNext(err); next != "" {
		t.Fatalf("next step = %q, want none", next)
	}
	assertUnchangedWorkspace(t, surface, snapshotAgentSurface(t, root))

	// A mixed declaration is refused the same way, by install and upgrade.
	writeContentWorkspaceConfig(t, root, []string{contentExtensionName},
		[]string{testAgentArtifact + ":stable", supersededMaintainer + ":stable", contentExtensionOptIn})
	surface = snapshotAgentSurface(t, root)
	if _, err := runInstall(t, root); err == nil || !strings.Contains(err.Error(), "no longer installs") {
		t.Fatalf("install error = %v, want the mixed declaration refused", err)
	}
	if err := agentctx.AdoptAgentWorkflows(context.Background(), root, wsproto.Load(root), nil); err == nil ||
		!strings.Contains(err.Error(), "no longer installs") {
		t.Fatalf("upgrade error = %v, want the mixed declaration refused", err)
	}
	assertUnchangedWorkspace(t, surface, snapshotAgentSurface(t, root))

	// Withdrawing the legacy declarations still leaves this clone's records
	// claiming the files, and that is refused too, naming each record.
	writeContentWorkspaceConfig(t, root, []string{contentExtensionName}, []string{contentExtensionOptIn})
	_, installErr := runInstall(t, root)
	upgradeErr := agentctx.AdoptAgentWorkflows(context.Background(), root, wsproto.Load(root), nil)
	records := make([]string, 0, 2)
	for _, name := range []string{testAgentArtifact, supersededMaintainer} {
		rel, err := filepath.Rel(root, agentartifacts.StatePath(root, name))
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, filepath.ToSlash(rel))
	}
	want := "delete " + strings.Join(records, ", ") + ", then rerun"
	for name, err := range map[string]error{"install": installErr, "upgrade": upgradeErr} {
		if err == nil || !strings.Contains(err.Error(), "still records") || !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("%s error = %v, want %q", name, err, want)
		}
		if next := protocolcli.SuggestedNext(err); next != "" {
			t.Fatalf("%s next step = %q, want none", name, next)
		}
	}
	assertUnchangedWorkspace(t, surface, snapshotAgentSurface(t, root))

	// The user deletes what the refusal names: the records. The lock is not
	// edited by hand.
	for _, record := range records {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(record))); err != nil {
			t.Fatal(err)
		}
	}

	// A file whose bytes differ from the content is then a collision that
	// aborts the run before its first write.
	surface = snapshotAgentSurface(t, root)
	if _, err := runInstall(t, root); err == nil || !strings.Contains(err.Error(), supersededChangedCore) {
		t.Fatalf("install error = %v, want the differing file %s reported", err, supersededChangedCore)
	}
	assertUnchangedWorkspace(t, surface, snapshotAgentSurface(t, root))

	// Once the user resolves those files, the content installs. The file that
	// already had the content's bytes is kept as it is, not written again: its
	// modification time does not move.
	for _, path := range []string{supersededChangedCore, supersededChangedMaintainer} {
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			t.Fatal(err)
		}
	}
	unchanged := filepath.Join(root, filepath.FromSlash(supersededUnchanged))
	past := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(unchanged, past, past); err != nil {
		t.Fatal(err)
	}
	if out, err := runInstall(t, root); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	after, err := os.Stat(unchanged)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(past) {
		t.Fatalf("%s was written again (modified %s)", supersededUnchanged, after.ModTime())
	}
	for path, want := range content {
		if got := readAgentFile(t, root, path); got != string(want) {
			t.Fatalf("%s = %q, want the content's bytes", path, got)
		}
	}
	state, err := agentartifacts.LoadState(root, contentExtensionName)
	if err != nil || state == nil || len(state.Files) != len(content) {
		t.Fatalf("extension ownership record = %+v, err %v; want every file the content ships", state, err)
	}

	// A file only the old artifacts installed stays on disk, and no record
	// manages it. The install dropped every pin under agentArtifacts.
	for path, want := range map[string]string{supersededOnlyCore: "# legacy-check\n", supersededOnlyMaintainer: "# legacy-release\n"} {
		if got := readAgentFile(t, root, path); got != want {
			t.Fatalf("%s = %q, want the old artifact's bytes kept", path, got)
		}
		for _, file := range state.Files {
			if file.Path == path {
				t.Fatalf("the extension's record manages %s, which its content does not ship", path)
			}
		}
	}
	assertNoAgentArtifactPins(t, root)
}
