package lifecycle

import (
	"context"
	"encoding/json"
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

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	wsproto "go.putnami.dev/protocol/workspace"
	sdkagentartifact "go.putnami.dev/sdk/extension/agentartifact"
	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/commands/agentctx"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/flock"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// The agent-content lifecycle acceptance matrix. Every case below runs the
// real command entry point — WorkspaceInit, Install, the upgrade phase, the
// ensure and reconcile passes, `migrate agent-content` — against a real
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

// The production starter table, unstubbed: every built-in starter opts into
// the contributor extension's agent content.
func TestBuiltInStartersOptIntoTheContributorExtension(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-opts-into-contributor", "every-built-in-starter-opts-into-the-contributor-extension")
	if len(initExtensions) == 0 {
		t.Fatal("no built-in starters, so this check proves nothing")
	}
	for name, starter := range initExtensions {
		if starter.agentContent != "@putnami/contributor" {
			t.Errorf("starter %q opts into %q, want @putnami/contributor", name, starter.agentContent)
		}
		if got := initDeclaredExtensions(starter); strings.Join(got, ",") != starter.packageName+",@putnami/contributor" {
			t.Errorf("starter %q declares extensions %v", name, got)
		}
	}
}

// init declares the starter's extension and opts into its content, installs
// that extension, and materializes the content at the release it pinned.
func TestAgentWorkflows_InitInstallsTheStarterContent(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-opts-into-contributor", "init-installs-the-extension-and-its-content")
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
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "starter-opts-into-contributor", "a-failed-starter-install-points-at-install")
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
// next command names the migration instead of orphaning its files.
func TestAgentWorkflows_InitPreservesAnExistingOptIn(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "reinit-preserves-optin", "init-force-unions-the-declaration")
	root := agentWorkspaceDeclaring(t, "extension:@acme/other", testAgentArtifact+":1.4.x", "extension:"+starterAgentContent)
	got := initAgentArtifacts(root, initExtensionConfig{packageName: "@putnami/go", agentContent: starterAgentContent})
	want := []string{"extension:@acme/other", testAgentArtifact + ":1.4.x", "extension:" + starterAgentContent}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("initAgentArtifacts = %v, want %v", got, want)
	}
	if got := initAgentArtifacts(t.TempDir(), initExtensionConfig{packageName: "@putnami/go", agentContent: starterAgentContent}); strings.Join(got, ",") != "extension:"+starterAgentContent {
		t.Fatalf("a fresh directory declares %v, want only the starter's opt-in", got)
	}
}

// A clone an earlier CLI installed a legacy artifact into — its declaration,
// its pin, its record and its files — is refused by install and upgrade with
// nothing written, and both name the migration to the extension that
// supersedes it.
func TestAgentWorkflows_LegacyStateIsRefusedWithNothingWritten(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "legacy-declarations-refused", "install-and-upgrade-refuse-legacy-state-with-nothing-written")
	root := agentWorkspaceDeclaring(t, testAgentArtifact+":stable")
	hometest.Temp(t)
	seedLegacyArtifact(t, root, testAgentArtifact, "1.4.2", map[string]string{".agents/skills/fix/SKILL.md": "# fix\n"}, true)
	before := snapshotAgentSurface(t, root)

	_, err := runInstall(t, root)
	if err == nil || !strings.Contains(err.Error(), "no longer installs") {
		t.Fatalf("install error = %v, want the legacy declaration refused", err)
	}
	if next := protocolcli.SuggestedNext(err); next != "putnami migrate agent-content @putnami/contributor" {
		t.Fatalf("install next step = %q", next)
	}
	err = agentctx.AdoptAgentWorkflows(context.Background(), root, wsproto.Load(root), nil)
	if !errors.Is(err, protocolcli.ErrInvalidConfig) || protocolcli.SuggestedNext(err) != "putnami migrate agent-content @putnami/contributor" {
		t.Fatalf("upgrade error = %v", err)
	}
	var dryRun strings.Builder
	agentctx.DescribeAgentWorkflowPlan(root, wsproto.Load(root), &dryRun)
	if !strings.Contains(dryRun.String(), "Next: putnami migrate agent-content @putnami/contributor") {
		t.Fatalf("dry run = %q, want the migration named", dryRun.String())
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
		failed = upgradeArtifactPhases(context.Background(), root, wsproto.Load(root), selector, UpgradeFlags{Extensions: true}, true)
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
			failed = upgradeArtifactPhases(context.Background(), root, wsproto.Load(root), selector, UpgradeFlags{Extensions: true}, true)
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

// An npm extension: the root package.json declares it in devDependencies, the
// package manager installs it into node_modules, and discovery loads its
// commands and tools from there.

// npmPackageManager is the package manager of the npm cases, the one external
// boundary they replace. Its workspace-install copies into node_modules the
// package version the root package.json declares, with the package.json a
// real install leaves; its deps-upgrade moves the declaration to the latest
// version it holds. The packages are the SDK's real packaged trees.
type npmPackageManager struct {
	t        *testing.T
	name     string
	packages map[string]*sdkagentartifact.ExtensionPackage
	latest   string
	jobs     []string
}

func newNPMPackageManager(t *testing.T, name string) *npmPackageManager {
	t.Helper()
	hometest.Temp(t)
	t.Setenv(artifactsEnsuredEnv, "")
	return &npmPackageManager{t: t, name: name, packages: map[string]*sdkagentartifact.ExtensionPackage{}}
}

func (pm *npmPackageManager) publish(pkg *sdkagentartifact.ExtensionPackage) {
	pm.packages[pkg.Version] = pkg
	pm.latest = pkg.Version
}

func (pm *npmPackageManager) env(out *strings.Builder) LifecycleEnv {
	return LifecycleEnv{Out: out, RunJob: pm.run}
}

func (pm *npmPackageManager) run(_ context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
	pm.jobs = append(pm.jobs, req.Job)
	switch req.Job {
	case "deps-upgrade":
		declareNPMExtension(pm.t, req.WorkspaceRoot, pm.name, pm.latest)
	case "workspace-install":
		var manifest struct {
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if err := json.Unmarshal([]byte(readWorkspaceFile(pm.t, req.WorkspaceRoot, "package.json")), &manifest); err != nil {
			pm.t.Fatalf("parse package.json: %v", err)
		}
		version := manifest.DevDependencies[pm.name]
		pkg := pm.packages[version]
		if pkg == nil {
			return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, nil
		}
		dir := filepath.Join(req.WorkspaceRoot, "node_modules", filepath.FromSlash(pm.name))
		if err := os.RemoveAll(dir); err != nil {
			pm.t.Fatal(err)
		}
		files := map[string]string{"package.json": fmt.Sprintf(`{"name":%q,"version":%q}`, pm.name, version)}
		for member, data := range pkg.Members {
			files[member] = string(data)
		}
		writeTree(pm.t, dir, files)
	}
	return WorkspaceJobResult{Outcome: WorkspaceJobOK}, nil
}

// declareNPMExtension writes the root package.json declaring name@version in
// devDependencies.
func declareNPMExtension(t *testing.T, root, name, version string) {
	t.Helper()
	writeTree(t, root, map[string]string{
		"package.json": fmt.Sprintf(`{"name":"npm-ws","private":true,"devDependencies":{%q:%q}}`, name, version),
	})
}

// assertNPMContentAt checks that the content on disk, its ownership record and
// the directory discovery loads the extension's commands from all name the
// installed package at version.
func assertNPMContentAt(t *testing.T, root string, pkg *sdkagentartifact.ExtensionPackage, revision string) {
	t.Helper()
	if got := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md"); !strings.Contains(got, "Review revision "+revision+".") {
		t.Fatalf("content = %q, want revision %s", got, revision)
	}
	dir := filepath.Join(root, "node_modules", filepath.FromSlash(contentExtensionName))
	installed, err := os.ReadFile(filepath.Join(dir, extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	state, err := agentartifacts.LoadState(root, contentExtensionName)
	if err != nil || state == nil {
		t.Fatalf("ownership state = %+v, err %v", state, err)
	}
	if state.ArtifactVersion != pkg.Version || state.ArchiveDigest != lockfile.HashBytes(installed) || state.ManifestHash != pkg.Content.ManifestSHA256 {
		t.Fatalf("ownership state %+v does not name the installed package %s", state, pkg.Version)
	}
	discovered, err := extension.DiscoverExtensions(root, wsproto.Load(root), nil)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	for _, ext := range discovered {
		if ext.Name == contentExtensionName {
			if ext.Path != dir || ext.Version != pkg.Version {
				t.Fatalf("commands load from %s@%s, content came from %s@%s", ext.Path, ext.Version, dir, pkg.Version)
			}
			return
		}
	}
	t.Fatalf("discovery did not load %s", contentExtensionName)
}

// The content of an npm extension comes from the package its commands load
// from, through the real Install and upgrade entry points: install reads it
// after the package manager ran, a reinstall changes nothing, a moved
// declaration moves the content with the package, and an upgrade that moves
// the package does the same.
func TestExtensionAgentContent_NPMPackageFollowsThePackageManager(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "npm-extension-agent-content", "an-npm-extension-content-comes-from-the-package-its-commands-load-from")
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "npm-extension-agent-content", "install-and-upgrade-read-npm-content-after-the-package-manager")
	pm := newNPMPackageManager(t, contentExtensionName)
	v1 := packageContentExtension(t, contentExtensionName, "r1", "1.0.0", true)
	v2 := packageContentExtension(t, contentExtensionName, "r2", "2.0.0", false)
	v3 := packageContentExtension(t, contentExtensionName, "r3", "3.0.0", false)
	for _, pkg := range []*sdkagentartifact.ExtensionPackage{v1, v2, v3} {
		pm.publish(pkg)
	}
	root := contentWorkspace(t, nil, []string{contentExtensionOptIn})
	declareNPMExtension(t, root, contentExtensionName, "1.0.0")

	// A fresh clone: nothing is in node_modules until the package manager runs.
	var out strings.Builder
	if _, err := captureStdout(t, func() error {
		return Install(context.Background(), root, wsproto.Load(root), nil, pm.env(&out))
	}); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	assertNPMContentAt(t, root, v1, "r1")
	readAgentFile(t, root, ".agents/skills/fixture-plan/SKILL.md")
	assertNoAgentArtifactPins(t, root)
	if _, pinned := extensionPin(t, root, contentExtensionName); pinned {
		t.Fatal("an npm extension was pinned in putnami.lock.json; the package manager's lock pins it")
	}

	// Reinstall: nothing moves.
	before := snapshotWorkspace(t, root, installStateMarker)
	out.Reset()
	if _, err := captureStdout(t, func() error {
		return Install(context.Background(), root, wsproto.Load(root), nil, pm.env(&out))
	}); err != nil {
		t.Fatalf("reinstall: %v\n%s", err, out.String())
	}
	assertUnchangedWorkspace(t, before, snapshotWorkspace(t, root, installStateMarker))

	// The declaration moves: install moves the package, then its content.
	declareNPMExtension(t, root, contentExtensionName, "2.0.0")
	out.Reset()
	if _, err := captureStdout(t, func() error {
		return Install(context.Background(), root, wsproto.Load(root), nil, pm.env(&out))
	}); err != nil {
		t.Fatalf("install after the declaration moved: %v\n%s", err, out.String())
	}
	assertNPMContentAt(t, root, v2, "r2")
	assertFileMissing(t, root, ".agents/skills/fixture-plan/SKILL.md")

	// upgrade --deps moves the package through the package manager, and the
	// agent phase runs after it.
	pm.jobs = nil
	if _, err := captureStdout(t, func() error {
		return Upgrade(context.Background(), root, wsproto.Load(root), "", "", UpgradeFlags{Deps: true}, pm.env(&out))
	}); err != nil {
		t.Fatalf("upgrade --deps: %v", err)
	}
	if strings.Join(pm.jobs, ",") != "deps-upgrade,workspace-install" {
		t.Fatalf("package manager jobs = %v", pm.jobs)
	}
	assertNPMContentAt(t, root, v3, "r3")

	// The implicit passes read the package already installed.
	if err := os.RemoveAll(filepath.Join(root, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := agentctx.EnsureAgentWorkflows(context.Background(), root, wsproto.Load(root)); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	assertNPMContentAt(t, root, v3, "r3")
}

// An upgrade whose dependency phase fails skips the agent phase, as install
// does: node_modules may already hold the next package, but the content and
// its record stay at the previous release until the package manager succeeds.
func TestExtensionAgentContent_NPMUpgradeSkipsTheAgentPhaseWhenTheDependencyPhaseFails(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "npm-extension-agent-content", "install-and-upgrade-read-npm-content-after-the-package-manager")
	pm := newNPMPackageManager(t, contentExtensionName)
	v1 := packageContentExtension(t, contentExtensionName, "r1", "1.0.0", true)
	pm.publish(v1)
	root := contentWorkspace(t, nil, []string{contentExtensionOptIn})
	declareNPMExtension(t, root, contentExtensionName, "1.0.0")
	var out strings.Builder
	if _, err := captureStdout(t, func() error {
		return Install(context.Background(), root, wsproto.Load(root), nil, pm.env(&out))
	}); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	assertNPMContentAt(t, root, v1, "r1")

	// The package manager writes the next package, then reports a failure.
	pm.publish(packageContentExtension(t, contentExtensionName, "r2", "2.0.0", false))
	failing := func(ctx context.Context, req WorkspaceJobRequest) (WorkspaceJobResult, error) {
		result, err := pm.run(ctx, req)
		if req.Job == "workspace-install" {
			return WorkspaceJobResult{Outcome: WorkspaceJobFailed}, err
		}
		return result, err
	}
	before := snapshotContentSurface(t, root)
	pm.jobs = nil
	if _, err := captureStdout(t, func() error {
		return Upgrade(context.Background(), root, wsproto.Load(root), "", "", UpgradeFlags{Deps: true}, LifecycleEnv{Out: &out, RunJob: failing})
	}); err == nil {
		t.Fatal("an upgrade whose dependency phase failed reported success")
	}
	if strings.Join(pm.jobs, ",") != "deps-upgrade,workspace-install" {
		t.Fatalf("package manager jobs = %v", pm.jobs)
	}
	assertUnchangedWorkspace(t, before, snapshotContentSurface(t, root))
	if state, err := agentartifacts.LoadState(root, contentExtensionName); err != nil || state == nil || state.ArtifactVersion != "1.0.0" {
		t.Fatalf("ownership record = %+v, err %v; want the previous release", state, err)
	}
}

// One extension declared through two sources is refused with nothing written:
// the lock pins one release and the package manager installs another.
func TestExtensionAgentContent_NPMAndRegistryDeclarationsAreRefused(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "npm-extension-agent-content", "a-second-source-for-one-extension-is-refused")
	root, _ := seededContentWorkspace(t)
	declareNPMExtension(t, root, contentExtensionName, "1.0.0")
	before := snapshotContentSurface(t, root)
	err := agentctx.InstallAgentWorkflows(context.Background(), root, wsproto.Load(root), nil)
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("error = %v, want the two sources refused", err)
	}
	assertUnchangedWorkspace(t, before, snapshotContentSurface(t, root))
}

// Migrating separately declared artifacts to extension-owned content, end to
// end. A consumer clone that an earlier CLI installed two pinned legacy
// artifacts into moves them to the agent content of an extension that
// supersedes both, through the real `migrate agent-content` entry point. The
// extension is packaged by the SDK and served from the local registry; the
// legacy state is written directly, because no current command installs it.

const supersededMaintainer = "@putnami/maintainer-workflows"

// The paths the three sources share, and what the migration does to each.
const (
	// The core artifact already ships the content's exact bytes.
	migrationUnchanged = ".agents/skills/fixture-review/SKILL.md"
	// Managed by the core artifact; the content ships other bytes.
	migrationChangedCore = ".agents/skills/fixture-review/references/guide.md"
	// Managed by the maintainer artifact; the content ships other bytes.
	migrationChangedMaintainer = ".claude/agents/fixture-reviewer.md"
	// Managed by an artifact; the content no longer ships them.
	migrationRemovedCore       = ".agents/skills/legacy-check/SKILL.md"
	migrationRemovedMaintainer = ".agents/skills/legacy-release/SKILL.md"
	// Added by the content.
	migrationAdded = ".codex/agents/fixture-reviewer.toml"
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

// migrationFixture is a consumer clone in which an earlier CLI installed the
// core and the maintainer artifacts at pinned versions, and which declares —
// without opting into — the extension whose content supersedes both.
type migrationFixture struct {
	root    string
	content map[string][]byte
}

func newMigrationFixture(t *testing.T, supersedes []string) *migrationFixture {
	t.Helper()
	registry := newContentRegistry(t)
	pkg := packageSupersedingExtension(t, "r1", "1.0.0", supersedes)
	registry.publish(pkg)
	for _, path := range []string{migrationUnchanged, migrationChangedCore, migrationChangedMaintainer, migrationAdded} {
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
		migrationUnchanged:   string(pkg.Content.Files[migrationUnchanged]),
		migrationChangedCore: "Review guide of the core workflows.\n",
		migrationRemovedCore: "# legacy-check\n",
	}, true)
	seedLegacyArtifact(t, root, supersededMaintainer, "1.4.2", map[string]string{
		migrationChangedMaintainer: "The maintainer reviewer.\n",
		migrationRemovedMaintainer: "# legacy-release\n",
	}, true)
	return &migrationFixture{root: root, content: pkg.Content.Files}
}

// migrationLock is the migration command's lock file. It outlives every run
// by design, so a comparison of the workspace before and after a run ignores
// it by name, like the install marker.
var migrationLock = ".putnami/" + agentctx.AgentContentMigrationLockFilename

func snapshotMigration(t *testing.T, root string, ignore ...string) map[string]string {
	t.Helper()
	return snapshotWorkspace(t, root, append(ignore, migrationLock)...)
}

// runMigration runs `putnami migrate agent-content` in one mode.
func runMigration(t *testing.T, root, mode string) (string, error) {
	t.Helper()
	return captureStdout(t, func() error {
		return agentctx.MigrateAgentContent(context.Background(), root, wsproto.Load(root), contentExtensionName, mode, "")
	})
}

func migrationReport(t *testing.T, err error) agentctx.AgentContentMigrationReport {
	t.Helper()
	report, ok := shared.ResultData(err).(agentctx.AgentContentMigrationReport)
	if !ok {
		t.Fatalf("error %v carries no migration report", err)
	}
	return report
}

func workspaceDeclarations(t *testing.T, root string) []string {
	t.Helper()
	var config struct {
		AgentArtifacts []string `json:"agentArtifacts"`
	}
	if err := json.Unmarshal([]byte(readWorkspaceFile(t, root, wsproto.WorkspaceConfigFilename)), &config); err != nil {
		t.Fatal(err)
	}
	return config.AgentArtifacts
}

// assertMigrated checks the complete after state: one opt-in, no superseded
// pin, the extension pin untouched, one ownership record naming the pinned
// extension release, and exactly the content's bytes on disk.
func assertMigrated(t *testing.T, f *migrationFixture, extensionPinBefore lockfile.LockEntry) {
	t.Helper()
	if got := workspaceDeclarations(t, f.root); strings.Join(got, ",") != contentExtensionOptIn {
		t.Fatalf("agentArtifacts = %v, want only %s", got, contentExtensionOptIn)
	}
	assertNoAgentArtifactPins(t, f.root)
	pin, ok := extensionPin(t, f.root, contentExtensionName)
	if !ok || pin.Version != extensionPinBefore.Version || pin.ManifestHash != extensionPinBefore.ManifestHash {
		t.Fatalf("extension pin = %+v, want the unchanged %+v", pin, extensionPinBefore)
	}
	state, err := agentartifacts.LoadState(f.root, contentExtensionName)
	if err != nil || state == nil || state.ArtifactVersion != "1.0.0" || state.ArchiveDigest != pin.ManifestHash {
		t.Fatalf("extension ownership record = %+v, err %v", state, err)
	}
	if len(state.Files) != len(f.content) {
		t.Fatalf("the extension records %d files, want the %d its content ships", len(state.Files), len(f.content))
	}
	for _, name := range []string{testAgentArtifact, supersededMaintainer} {
		if record, err := agentartifacts.LoadState(f.root, name); err != nil || record != nil {
			t.Fatalf("superseded record %s = %+v, err %v; want none", name, record, err)
		}
	}
	for path, content := range f.content {
		if got := readAgentFile(t, f.root, path); got != string(content) {
			t.Fatalf("%s = %q, want the content's bytes", path, got)
		}
	}
	assertFileMissing(t, f.root, migrationRemovedCore)
	assertFileMissing(t, f.root, migrationRemovedMaintainer)
}

// Before the migration no ordinary command installs anything: the legacy
// declarations, and a clone's legacy records once the declarations are gone,
// are refused with nothing written and the migration named. The migration
// then leaves one owner.
func TestAgentContentMigration_LegacyStateIsRefusedUntilMigrated(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "superseded-owners-refused", "a-clone-that-still-records-a-superseded-artifact-is-refused")
	f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
	extensionPinBefore, _ := extensionPin(t, f.root, contentExtensionName)
	surface := snapshotAgentSurface(t, f.root)

	_, err := runInstall(t, f.root)
	if err == nil || !strings.Contains(err.Error(), "no longer installs") ||
		!strings.Contains(err.Error(), "Move the declarations, pins and ownership records to extension "+contentExtensionName) {
		t.Fatalf("install error = %v, want the legacy declarations refused", err)
	}
	if next := protocolcli.SuggestedNext(err); next != "putnami migrate agent-content "+contentExtensionName {
		t.Fatalf("next step = %q", next)
	}
	assertUnchangedWorkspace(t, surface, snapshotAgentSurface(t, f.root))

	// A mixed declaration is refused the same way, by install and upgrade.
	writeContentWorkspaceConfig(t, f.root, []string{contentExtensionName},
		[]string{testAgentArtifact + ":stable", supersededMaintainer + ":stable", contentExtensionOptIn})
	surface = snapshotAgentSurface(t, f.root)
	if _, err := runInstall(t, f.root); err == nil || !strings.Contains(err.Error(), "no longer installs") {
		t.Fatalf("install error = %v, want the mixed declaration refused", err)
	}
	if err := agentctx.AdoptAgentWorkflows(context.Background(), f.root, wsproto.Load(f.root), nil); err == nil ||
		!strings.Contains(err.Error(), "no longer installs") {
		t.Fatalf("upgrade error = %v, want the mixed declaration refused", err)
	}
	assertUnchangedWorkspace(t, surface, snapshotAgentSurface(t, f.root))

	// The migration resolves the mixed declaration to one opt-in.
	_, err = runMigration(t, f.root, agentctx.AgentContentMigrationCheck)
	if report := migrationReport(t, err); strings.Join(report.Declarations.After, ",") != contentExtensionOptIn {
		t.Fatalf("declarations after = %v, want the one opt-in", report.Declarations.After)
	}

	// Withdrawing the legacy declarations by hand still leaves this clone's
	// records claiming the files, and that is refused too.
	writeContentWorkspaceConfig(t, f.root, []string{contentExtensionName}, []string{contentExtensionOptIn})
	_, installErr := runInstall(t, f.root)
	upgradeErr := agentctx.AdoptAgentWorkflows(context.Background(), f.root, wsproto.Load(f.root), nil)
	for name, err := range map[string]error{"install": installErr, "upgrade": upgradeErr} {
		if err == nil || !strings.Contains(err.Error(), "still records") {
			t.Fatalf("%s error = %v, want the recorded superseded artifacts refused", name, err)
		}
		if next := protocolcli.SuggestedNext(err); next != "putnami migrate agent-content "+contentExtensionName+" --apply" {
			t.Fatalf("%s next step = %q", name, next)
		}
	}
	assertUnchangedWorkspace(t, surface, snapshotAgentSurface(t, f.root))

	if out, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}
	assertMigrated(t, f, extensionPinBefore)
}

// A clone that pulls a teammate's migration commit gets the committed opt-in
// and host files, but keeps its own records of the superseded artifacts. The
// ordinary commands refuse until one `migrate agent-content --apply` moves the
// records; it keeps the file the user edited, and afterwards every command is
// a byte-for-byte no-op.
func TestAgentContentMigration_AClonePullingTheMigrationConvergesWithApply(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "superseded-owners-refused", "a-clone-that-pulled-the-migration-converges-with-apply")
	root := contentWorkspace(t, []string{"/tools/contributor"}, []string{contentExtensionOptIn})
	files := supersedingExtensionFiles("local", nil)
	files["putnami.extension.json"] = fmt.Sprintf(`{"name":%q,"cliContract":%d,"agentContent":{"path":"agent-content","source":"agent-src","supersedes":[%q,%q]}}`,
		contentExtensionName, protocolcli.AgentContentContract, testAgentArtifact, supersededMaintainer)
	writeTree(t, filepath.Join(root, "tools", "contributor"), files)

	// The teammate's commit carries the host files the content builds.
	source, err := sdkagentartifact.BuildExtensionContent(filepath.Join(root, "tools", "contributor"), "agent-src", contentExtensionName, "0.0.0-local")
	if err != nil {
		t.Fatal(err)
	}
	for path, content := range source.Files {
		writeAgentFile(t, root, path, string(content))
	}
	// This clone's records still name the superseded artifacts: one path the
	// content ships again with other bytes, one git deleted on pull, and one
	// only a superseded artifact shipped, which the user edited.
	seedLegacyArtifact(t, root, testAgentArtifact, "1.4.2", map[string]string{
		migrationChangedCore: "Review guide of the core workflows.\n",
		migrationRemovedCore: "# legacy-check\n",
	}, false)
	seedLegacyArtifact(t, root, supersededMaintainer, "1.4.2", map[string]string{
		migrationRemovedMaintainer: "# legacy-release\n",
	}, false)
	writeAgentFile(t, root, migrationChangedCore, string(source.Files[migrationChangedCore]))
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(migrationRemovedCore))); err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, root, migrationRemovedMaintainer, "my own release notes\n")

	before := snapshotMigration(t, root)
	if _, err := agentctx.MaterializeLocalAgentContent(root, wsproto.Load(root), nil); err == nil ||
		protocolcli.SuggestedNext(err) != "putnami migrate agent-content "+contentExtensionName+" --apply" {
		t.Fatalf("context generate error = %v, want the recorded superseded artifacts refused", err)
	}
	assertUnchangedWorkspace(t, before, snapshotMigration(t, root))

	out, err := runMigration(t, root, agentctx.AgentContentMigrationApply)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if !strings.Contains(out, "released  "+migrationRemovedMaintainer) {
		t.Fatalf("the edited file is not reported as released:\n%s", out)
	}
	if got := readAgentFile(t, root, migrationRemovedMaintainer); got != "my own release notes\n" {
		t.Fatalf("the user's edit was lost: %q", got)
	}
	if got := workspaceDeclarations(t, root); strings.Join(got, ",") != contentExtensionOptIn {
		t.Fatalf("agentArtifacts = %v", got)
	}
	for _, name := range []string{testAgentArtifact, supersededMaintainer} {
		if record, err := agentartifacts.LoadState(root, name); err != nil || record != nil {
			t.Fatalf("superseded record %s = %+v, err %v; want none", name, record, err)
		}
	}
	for path, content := range source.Files {
		if got := readAgentFile(t, root, path); got != string(content) {
			t.Fatalf("%s = %q, want the committed content", path, got)
		}
	}
	settled := snapshotMigration(t, root)
	if changed, err := agentctx.MaterializeLocalAgentContent(root, wsproto.Load(root), nil); err != nil || changed {
		t.Fatalf("context generate after the migration changed=%v err=%v", changed, err)
	}
	if err := agentctx.InstallAgentWorkflows(context.Background(), root, wsproto.Load(root), nil); err != nil {
		t.Fatalf("install after the migration: %v", err)
	}
	assertUnchangedWorkspace(t, settled, snapshotMigration(t, root))
}

// The check lists both halves — what moves and what changes on disk — and
// writes nothing; apply then performs exactly that, and repeating it or any
// ordinary command afterwards changes no byte.
func TestAgentContentMigration_CheckThenApplyMovesDeclarationsPinsAndOwnership(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration", "check-lists-the-mechanical-and-the-semantic-change")
	f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
	extensionPinBefore, _ := extensionPin(t, f.root, contentExtensionName)
	before := snapshotMigration(t, f.root)

	out, err := runMigration(t, f.root, agentctx.AgentContentMigrationCheck)
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("check error = %v, want a pending migration (exit 2)", err)
	}
	assertUnchangedWorkspace(t, before, snapshotMigration(t, f.root))
	report := migrationReport(t, err)
	if report.Outcome != agentctx.AgentContentMigrationPending || report.Version != "1.0.0" {
		t.Fatalf("report = %+v", report)
	}
	wantSuperseded := []agentctx.AgentContentMigrationArtifact{
		{Name: testAgentArtifact, Declared: testAgentArtifact + ":stable", Pin: "1.4.2", Ownership: agentctx.AgentContentOwnershipRecord, Files: 3},
		{Name: supersededMaintainer, Declared: supersededMaintainer + ":stable", Pin: "1.4.2", Ownership: agentctx.AgentContentOwnershipRecord, Files: 2},
	}
	if fmt.Sprint(report.Superseded) != fmt.Sprint(wantSuperseded) {
		t.Fatalf("superseded = %+v, want %+v", report.Superseded, wantSuperseded)
	}
	if strings.Join(report.Declarations.After, ",") != contentExtensionOptIn {
		t.Fatalf("declarations after = %v", report.Declarations.After)
	}
	changes := map[string]string{}
	for _, file := range report.Files {
		changes[file.Path] = file.Change
	}
	for path, want := range map[string]string{
		migrationChangedCore:       agentctx.AgentContentFileChanged,
		migrationChangedMaintainer: agentctx.AgentContentFileChanged,
		migrationRemovedCore:       agentctx.AgentContentFileRemoved,
		migrationRemovedMaintainer: agentctx.AgentContentFileRemoved,
		migrationAdded:             agentctx.AgentContentFileAdded,
	} {
		if changes[path] != want {
			t.Errorf("%s change = %q, want %q", path, changes[path], want)
		}
	}
	if _, listed := changes[migrationUnchanged]; listed || report.Unchanged != 1 {
		t.Errorf("the identical file must count as unchanged: listed %v, unchanged %d", listed, report.Unchanged)
	}
	if len(report.Files) != len(f.content)-1+2 {
		t.Errorf("report lists %d changes, want every content file but the identical one plus the two removals", len(report.Files))
	}
	for _, want := range []string{"Moves (mechanical", "Content (semantic", "changed   " + migrationChangedCore, "removed   " + migrationRemovedMaintainer} {
		if !strings.Contains(out, want) {
			t.Errorf("check output lacks %q:\n%s", want, out)
		}
	}

	t.Run("apply", func(t *testing.T) {
		spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration", "apply-moves-declarations-pins-and-ownership")
		if out, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply); err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		assertMigrated(t, f, extensionPinBefore)

		// The migrated workspace is an ordinary opted-in workspace: install,
		// the first-run pass and the session-start reconcile change nothing.
		settled := snapshotMigration(t, f.root, installStateMarker)
		if out, err := runInstall(t, f.root); err != nil {
			t.Fatalf("install after the migration: %v\n%s", err, out)
		}
		if err := agentctx.EnsureAgentWorkflows(context.Background(), f.root, wsproto.Load(f.root)); err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if err := agentctx.ReconcileAgentWorkflows(context.Background(), f.root, wsproto.Load(f.root)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		assertUnchangedWorkspace(t, settled, snapshotMigration(t, f.root, installStateMarker))

		// Nothing is left to migrate, and saying so writes nothing.
		settled = snapshotMigration(t, f.root)
		for _, mode := range []string{agentctx.AgentContentMigrationCheck, agentctx.AgentContentMigrationApply} {
			out, err := runMigration(t, f.root, mode)
			if err != nil || !strings.Contains(out, "nothing to migrate") {
				t.Fatalf("%s after the migration: %v\n%s", mode, err, out)
			}
		}
		structured, err := captureStdout(t, func() error {
			return agentctx.MigrateAgentContent(context.Background(), f.root, wsproto.Load(f.root), contentExtensionName, agentctx.AgentContentMigrationCheck, "json")
		})
		if err != nil || !strings.Contains(structured, `"outcome":"clean"`) {
			t.Fatalf("structured check = %s (err %v)", structured, err)
		}
		assertUnchangedWorkspace(t, settled, snapshotMigration(t, f.root))
	})
}

// A user edit is never overwritten: one the content still ships blocks the
// whole migration with zero writes, and one the content no longer ships is
// kept, reported and released while the rest migrates.
func TestAgentContentMigration_UserEditsBlockOrAreReleased(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration-safety", "user-edits-block-or-are-released")

	for name, edit := range map[string]struct{ path, reason string }{
		"an edit the content still ships":           {migrationChangedCore, agentartifacts.ReasonModified},
		"an unrecorded file where content is added": {migrationAdded, agentartifacts.ReasonUnmanaged},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
			writeAgentFile(t, f.root, edit.path, "my own notes\n")
			before := snapshotMigration(t, f.root)
			for _, mode := range []string{agentctx.AgentContentMigrationCheck, agentctx.AgentContentMigrationApply} {
				_, err := runMigration(t, f.root, mode)
				if !errors.Is(err, agentartifacts.ErrCollision) || !strings.Contains(err.Error(), edit.path+" ["+edit.reason+"]") {
					t.Fatalf("%s error = %v, want a collision naming %s", mode, err, edit.path)
				}
				assertUnchangedWorkspace(t, before, snapshotMigration(t, f.root))
			}
		})
	}

	t.Run("an edit the content no longer ships", func(t *testing.T) {
		f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
		extensionPinBefore, _ := extensionPin(t, f.root, contentExtensionName)
		writeAgentFile(t, f.root, migrationRemovedCore, "my own notes\n")
		out, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply)
		if err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		if !strings.Contains(out, "released  "+migrationRemovedCore+" (edited; kept, no longer managed)") {
			t.Fatalf("the released file is not reported:\n%s", out)
		}
		if got := readAgentFile(t, f.root, migrationRemovedCore); got != "my own notes\n" {
			t.Fatalf("the user's edit was lost: %q", got)
		}
		state, _ := agentartifacts.LoadState(f.root, contentExtensionName)
		for _, file := range state.Files {
			if file.Path == migrationRemovedCore {
				t.Fatal("a released file is still managed")
			}
		}
		// Everything else migrated.
		if err := os.Remove(filepath.Join(f.root, filepath.FromSlash(migrationRemovedCore))); err != nil {
			t.Fatal(err)
		}
		assertMigrated(t, f, extensionPinBefore)
	})
}

// A clone without ownership records hands over no ownership: the content
// adopts only the files already identical to what it ships, a file with other
// bytes is the user's and blocks with zero writes, and once the user moves it
// the migration completes without touching anything it never proved.
func TestAgentContentMigration_ACloneWithoutRecordsAdoptsOnlyIdenticalBytes(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration", "a-clone-without-records-adopts-only-identical-bytes")
	f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
	if err := os.RemoveAll(filepath.Join(f.root, ".putnami", agentartifacts.StateDirName)); err != nil {
		t.Fatal(err)
	}
	before := snapshotMigration(t, f.root)
	_, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply)
	if !errors.Is(err, agentartifacts.ErrCollision) ||
		!strings.Contains(err.Error(), migrationChangedCore+" ["+agentartifacts.ReasonUnmanaged+"]") ||
		!strings.Contains(err.Error(), migrationChangedMaintainer+" ["+agentartifacts.ReasonUnmanaged+"]") {
		t.Fatalf("error = %v, want the unproven files blocking", err)
	}
	assertUnchangedWorkspace(t, before, snapshotMigration(t, f.root))

	for _, path := range []string{migrationChangedCore, migrationChangedMaintainer} {
		if err := os.Remove(filepath.Join(f.root, filepath.FromSlash(path))); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if !strings.Contains(out, testAgentArtifact+": "+testAgentArtifact+":stable; 1.4.2; ownership none (0 files)") {
		t.Fatalf("the missing ownership is not reported:\n%s", out)
	}
	for _, path := range []string{migrationRemovedCore, migrationRemovedMaintainer} {
		readAgentFile(t, f.root, path)
	}
	for path, content := range f.content {
		if got := readAgentFile(t, f.root, path); got != string(content) {
			t.Fatalf("%s = %q, want the content's bytes", path, got)
		}
	}
}

// The repository that authors its workflows moves its in-tree artifact to a
// local extension it also authors: nothing is pinned before or after, and the
// migrated workspace regenerates as a no-op.
func TestAgentContentMigration_InTreeArtifactMovesToALocalExtension(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration", "in-tree-artifacts-move-to-a-local-extension")
	root := contentWorkspace(t, []string{"/tools/contributor"}, []string{"/tools/workflows"})
	writeTree(t, filepath.Join(root, "tools", "workflows"), map[string]string{"putnami.json": `{"name":"@local/workflows"}`})
	files := supersedingExtensionFiles("local", nil)
	files["putnami.extension.json"] = fmt.Sprintf(`{"name":%q,"cliContract":%d,"agentContent":{"path":"agent-content","source":"agent-src","supersedes":["@local/workflows"]}}`,
		contentExtensionName, protocolcli.AgentContentContract)
	writeTree(t, filepath.Join(root, "tools", "contributor"), files)
	seedLegacyArtifact(t, root, "@local/workflows", "0.0.0-local-0123456789ab", map[string]string{
		".agents/skills/local-plan/SKILL.md": "---\nname: local-plan\ndescription: Plan locally\n---\n\nFirst body.\n",
	}, false)

	out, err := runMigration(t, root, agentctx.AgentContentMigrationApply)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if got := workspaceDeclarations(t, root); strings.Join(got, ",") != contentExtensionOptIn {
		t.Fatalf("agentArtifacts = %v", got)
	}
	assertFileMissing(t, root, ".agents/skills/local-plan/SKILL.md")
	if got := readAgentFile(t, root, ".claude/skills/fixture-review/SKILL.md"); !strings.Contains(got, "Review revision local.") {
		t.Fatalf("local content = %q", got)
	}
	if lf, _ := lockfile.ReadLockFile(root); lf != nil && len(lf.AgentArtifacts) != 0 {
		t.Fatalf("a local migration pinned something: %+v", lf.AgentArtifacts)
	}
	settled := snapshotMigration(t, root)
	if changed, err := agentctx.MaterializeLocalAgentContent(root, wsproto.Load(root), nil); err != nil || changed {
		t.Fatalf("context generate after the migration changed=%v err=%v", changed, err)
	}
	if err := agentctx.InstallAgentWorkflows(context.Background(), root, wsproto.Load(root), nil); err != nil {
		t.Fatalf("install after the migration: %v", err)
	}
	assertUnchangedWorkspace(t, settled, snapshotMigration(t, root))
}

// A clone that pulls the commit deleting an in-tree artifact's project still
// migrates: the entry is named by the superseded artifact its path names, and
// an entry that names none stays as declared instead of blocking the move.
func TestAgentContentMigration_AnInTreeArtifactWhoseProjectIsGoneStillMoves(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration", "in-tree-artifacts-move-to-a-local-extension")
	root := contentWorkspace(t, []string{"/tools/contributor"}, []string{"/tools/unrelated", "/tools/workflows"})
	files := supersedingExtensionFiles("local", nil)
	files["putnami.extension.json"] = fmt.Sprintf(`{"name":%q,"cliContract":%d,"agentContent":{"path":"agent-content","source":"agent-src","supersedes":["@local/workflows"]}}`,
		contentExtensionName, protocolcli.AgentContentContract)
	writeTree(t, filepath.Join(root, "tools", "contributor"), files)
	seedLegacyArtifact(t, root, "@local/workflows", "0.0.0-local-0123456789ab", map[string]string{
		".agents/skills/local-plan/SKILL.md": "---\nname: local-plan\ndescription: Plan locally\n---\n\nFirst body.\n",
	}, false)

	out, err := runMigration(t, root, agentctx.AgentContentMigrationCheck)
	report := migrationReport(t, err)
	if len(report.Superseded) != 1 || report.Superseded[0].Name != "@local/workflows" || report.Superseded[0].Declared != "/tools/workflows" {
		t.Fatalf("superseded = %+v, want /tools/workflows named @local/workflows\n%s", report.Superseded, out)
	}
	if out, err := runMigration(t, root, agentctx.AgentContentMigrationApply); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	if got := workspaceDeclarations(t, root); strings.Join(got, ",") != "/tools/unrelated,"+contentExtensionOptIn {
		t.Fatalf("agentArtifacts = %v, want the unrelated entry kept and the opt-in in place of the moved one", got)
	}
	assertFileMissing(t, root, ".agents/skills/local-plan/SKILL.md")
	if record, err := agentartifacts.LoadState(root, "@local/workflows"); err != nil || record != nil {
		t.Fatalf("superseded record = %+v, err %v; want none", record, err)
	}
}

// Two owners of one path with different bytes cannot be resolved by rule, and
// a content path another artifact keeps recording would leave two owners:
// both refuse with zero writes.
func TestAgentContentMigration_DuplicateOwnershipIsRefused(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration-safety", "duplicate-ownership-is-refused")

	t.Run("two superseded records", func(t *testing.T) {
		f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
		record, err := agentartifacts.LoadState(f.root, supersededMaintainer)
		if err != nil || record == nil {
			t.Fatalf("maintainer record = %+v, err %v", record, err)
		}
		record.Files = append(record.Files, agentartifacts.File{Path: migrationChangedCore, SHA256: lockfile.HashBytes([]byte("other bytes"))})
		if err := agentartifacts.WriteState(f.root, record); err != nil {
			t.Fatal(err)
		}
		before := snapshotMigration(t, f.root)
		_, err = runMigration(t, f.root, agentctx.AgentContentMigrationApply)
		if err == nil || !strings.Contains(err.Error(), "duplicate ownership: "+migrationChangedCore) ||
			!strings.Contains(err.Error(), testAgentArtifact) || !strings.Contains(err.Error(), supersededMaintainer) {
			t.Fatalf("error = %v, want the duplicate owners named", err)
		}
		assertUnchangedWorkspace(t, before, snapshotMigration(t, f.root))
	})

	t.Run("an artifact that keeps its record", func(t *testing.T) {
		f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
		if err := agentartifacts.WriteState(f.root, &agentartifacts.State{
			Version: agentartifacts.StateVersion, Name: secondAgentArtifact, ArtifactVersion: "1.0.0",
			ArchiveDigest: strings.Repeat("a", 64), ManifestHash: strings.Repeat("b", 64),
			Files: []agentartifacts.File{{Path: migrationAdded, SHA256: strings.Repeat("c", 64)}},
		}); err != nil {
			t.Fatal(err)
		}
		before := snapshotMigration(t, f.root)
		_, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply)
		if !errors.Is(err, agentartifacts.ErrCollision) || !strings.Contains(err.Error(), "duplicate ownership after the migration") ||
			!strings.Contains(err.Error(), migrationAdded) {
			t.Fatalf("error = %v, want the remaining owner refused", err)
		}
		assertUnchangedWorkspace(t, before, snapshotMigration(t, f.root))
	})
}

func interruptAgentContentMigrationAfter(t *testing.T, step string) {
	t.Helper()
	original := agentctx.AgentContentMigrationInterrupt
	agentctx.AgentContentMigrationInterrupt = func(current string) error {
		if step != "" && current == step {
			return errors.New("interrupted")
		}
		return nil
	}
	t.Cleanup(func() { agentctx.AgentContentMigrationInterrupt = original })
}

// A migration stopped after any step is refused by every ordinary command,
// then either finishes on a rerun or rolls back to the exact previous bytes.
func TestAgentContentMigration_AStoppedMigrationResumesOrRollsBack(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration-safety", "a-stopped-migration-resumes-or-rolls-back")
	steps := agentctx.AgentContentMigrationSteps[:len(agentctx.AgentContentMigrationSteps)-1]
	for _, step := range steps {
		for _, finish := range []string{agentctx.AgentContentMigrationApply, agentctx.AgentContentMigrationRollback} {
			t.Run(step+" then "+finish, func(t *testing.T) {
				f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
				extensionPinBefore, _ := extensionPin(t, f.root, contentExtensionName)
				original := snapshotMigration(t, f.root)

				interruptAgentContentMigrationAfter(t, step)
				_, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply)
				if err == nil || !strings.Contains(err.Error(), "stopped during "+step) {
					t.Fatalf("error = %v, want the stop reported", err)
				}
				interruptAgentContentMigrationAfter(t, "")

				// Between two steps the pieces describe neither side, so the
				// ordinary passes refuse instead of acting on either.
				stopped := snapshotMigration(t, f.root)
				if err := agentctx.InstallAgentWorkflows(context.Background(), f.root, wsproto.Load(f.root), nil); err == nil ||
					!strings.Contains(err.Error(), "did not finish") {
					t.Fatalf("install during a stopped migration = %v", err)
				}
				if err := agentctx.EnsureAgentWorkflows(context.Background(), f.root, wsproto.Load(f.root)); err == nil {
					t.Fatal("the first-run pass acted during a stopped migration")
				}
				assertUnchangedWorkspace(t, stopped, snapshotMigration(t, f.root))

				out, err := runMigration(t, f.root, finish)
				if err != nil {
					t.Fatalf("%s: %v\n%s", finish, err, out)
				}
				if finish == agentctx.AgentContentMigrationRollback {
					assertUnchangedWorkspace(t, original, snapshotMigration(t, f.root))
					return
				}
				if !strings.Contains(out, "resuming a migration that stopped") {
					t.Errorf("the rerun did not say it resumed:\n%s", out)
				}
				assertMigrated(t, f, extensionPinBefore)
			})
		}
	}
}

// A completed migration rolls back to the exact previous bytes — declarations,
// pins, ownership records and files. A rollback never overwrites a change made
// since, and a stopped rollback finishes on a rerun.
func TestAgentContentMigration_RollbackRestoresThePreviousState(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration-safety", "rollback-restores-the-previous-state")

	t.Run("after a completed migration", func(t *testing.T) {
		f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
		original := snapshotMigration(t, f.root)
		if out, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply); err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		out, err := runMigration(t, f.root, agentctx.AgentContentMigrationRollback)
		if err != nil {
			t.Fatalf("rollback: %v\n%s", err, out)
		}
		assertUnchangedWorkspace(t, original, snapshotMigration(t, f.root))
		// The restored state is the legacy one: this CLI refuses it again, and
		// names the migration.
		if _, err := runInstall(t, f.root); err == nil || !strings.Contains(err.Error(), "no longer installs") {
			t.Fatalf("install after the rollback = %v, want the legacy state refused", err)
		}
		if _, err := runMigration(t, f.root, agentctx.AgentContentMigrationRollback); !errors.Is(err, protocolcli.ErrNotFound) {
			t.Fatalf("a second rollback = %v, want nothing to roll back", err)
		}
	})

	t.Run("a change made since is never overwritten", func(t *testing.T) {
		f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
		if out, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply); err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		writeAgentFile(t, f.root, migrationAdded, "edited after the migration\n")
		before := snapshotMigration(t, f.root)
		_, err := runMigration(t, f.root, agentctx.AgentContentMigrationRollback)
		if !errors.Is(err, agentartifacts.ErrCollision) || !strings.Contains(err.Error(), migrationAdded) {
			t.Fatalf("rollback error = %v, want the edited file named", err)
		}
		assertUnchangedWorkspace(t, before, snapshotMigration(t, f.root))
	})

	t.Run("a second migration keeps the first rollback point", func(t *testing.T) {
		f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
		original := snapshotMigration(t, f.root)
		if out, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply); err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		migratedConfig := readWorkspaceFile(t, f.root, wsproto.WorkspaceConfigFilename)
		// A later pull declares a superseded artifact again: a second, smaller
		// migration would start a journal over the completed one.
		writeContentWorkspaceConfig(t, f.root, []string{contentExtensionName}, []string{testAgentArtifact + ":stable", contentExtensionOptIn})
		before := snapshotMigration(t, f.root)
		for _, mode := range []string{agentctx.AgentContentMigrationCheck, agentctx.AgentContentMigrationApply} {
			_, err := runMigration(t, f.root, mode)
			if err == nil || !strings.Contains(err.Error(), "keeps its rollback point") {
				t.Fatalf("%s error = %v, want the completed migration's rollback point kept", mode, err)
			}
			if next := protocolcli.SuggestedNext(err); next != "putnami migrate agent-content "+contentExtensionName+" --rollback" {
				t.Fatalf("%s next step = %q", mode, next)
			}
			assertUnchangedWorkspace(t, before, snapshotMigration(t, f.root))
		}
		// The completed migration still rolls back in full.
		writeAgentFile(t, f.root, wsproto.WorkspaceConfigFilename, migratedConfig)
		if out, err := runMigration(t, f.root, agentctx.AgentContentMigrationRollback); err != nil {
			t.Fatalf("rollback: %v\n%s", err, out)
		}
		assertUnchangedWorkspace(t, original, snapshotMigration(t, f.root))
	})

	t.Run("a stopped rollback finishes on a rerun", func(t *testing.T) {
		f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
		original := snapshotMigration(t, f.root)
		if out, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply); err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		interruptAgentContentMigrationAfter(t, "lock")
		if _, err := runMigration(t, f.root, agentctx.AgentContentMigrationRollback); err == nil || !strings.Contains(err.Error(), "stopped during lock") {
			t.Fatalf("rollback error = %v, want the stop reported", err)
		}
		interruptAgentContentMigrationAfter(t, "")
		if _, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply); err == nil || !strings.Contains(err.Error(), "rollback") {
			t.Fatalf("apply during a stopped rollback = %v, want it refused", err)
		}
		if out, err := runMigration(t, f.root, agentctx.AgentContentMigrationRollback); err != nil {
			t.Fatalf("rollback rerun: %v\n%s", err, out)
		}
		assertUnchangedWorkspace(t, original, snapshotMigration(t, f.root))
	})
}

// A release that states no supersession offers no migration, an extension
// that is declared but not installed is never resolved on the migration's
// behalf, and a journal this CLI cannot read stops every command.
func TestAgentContentMigration_UnsupportedStatesFailClearly(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration-safety", "unsupported-releases-fail-clearly")

	t.Run("a release without supersedes", func(t *testing.T) {
		f := newMigrationFixture(t, nil)
		before := snapshotMigration(t, f.root)
		_, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply)
		if err == nil || !strings.Contains(err.Error(), "names no agent artifact its content supersedes") {
			t.Fatalf("error = %v", err)
		}
		assertUnchangedWorkspace(t, before, snapshotMigration(t, f.root))
	})

	t.Run("an extension that is not installed", func(t *testing.T) {
		registry := newContentRegistry(t)
		registry.publish(packageSupersedingExtension(t, "r1", "1.0.0", []string{testAgentArtifact}))
		root := contentWorkspace(t, []string{contentExtensionName}, []string{testAgentArtifact})
		// The run takes its lock before it plans, which creates the gitignored
		// .putnami/ directory of a workspace that never had one.
		before := snapshotMigration(t, root, ".putnami")
		_, err := runMigration(t, root, agentctx.AgentContentMigrationApply)
		if err == nil || !strings.Contains(err.Error(), "pins no extension "+contentExtensionName) {
			t.Fatalf("error = %v, want the missing pin named", err)
		}
		registry.mu.Lock()
		hits := registry.hits
		registry.mu.Unlock()
		if hits != 0 {
			t.Fatalf("the migration reached the registry %d times", hits)
		}
		assertUnchangedWorkspace(t, before, snapshotMigration(t, root, ".putnami"))
	})

	t.Run("a journal of another version", func(t *testing.T) {
		f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
		writeAgentFile(t, f.root, ".putnami/agent-content-migrations/"+layout.EncodeName(contentExtensionName)+"/journal.json", `{"version":2}`+"\n")
		if err := agentctx.InstallAgentWorkflows(context.Background(), f.root, wsproto.Load(f.root), nil); err == nil ||
			!strings.Contains(err.Error(), "is unusable") {
			t.Fatalf("install error = %v, want the unreadable journal refused", err)
		}
		if _, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply); err == nil || !strings.Contains(err.Error(), "is unusable") {
			t.Fatalf("migration error = %v, want the unreadable journal refused", err)
		}
	})
}

// The content carries no backend: moving the task provider binding from one
// backend to another is a configuration change that rewrites no skill file.
func TestAgentContentMigration_BackendSubstitutionChangesNoContent(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration", "a-backend-substitution-changes-no-content")
	f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
	if out, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply); err != nil {
		t.Fatalf("apply: %v\n%s", err, out)
	}
	bind := func(provider string) {
		config := fmt.Sprintf(`{"name":"content-ws","extensions":[%q],"agentArtifacts":[%q],`+
			`"options":{"collaboration":{"tasks":{"provider":%q,"version":1}}}}`+"\n",
			contentExtensionName, contentExtensionOptIn, provider)
		if err := os.WriteFile(filepath.Join(f.root, wsproto.WorkspaceConfigFilename), []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bind("@putnami/local-collaboration")
	if out, err := runInstall(t, f.root); err != nil {
		t.Fatalf("install bound to the local provider: %v\n%s", err, out)
	}
	content := snapshotContentSurface(t, f.root)
	bind("@putnami/github-collaboration")
	if out, err := runInstall(t, f.root); err != nil {
		t.Fatalf("install bound to the GitHub provider: %v\n%s", err, out)
	}
	assertUnchangedWorkspace(t, content, snapshotContentSurface(t, f.root))
}

// Two runs that write never interleave: while one holds the workspace's
// migration lock, another apply or rollback changes nothing and says why.
func TestAgentContentMigration_ConcurrentRunsAreRefused(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "agent-content-migration-safety", "concurrent-migrations-are-refused")
	f := newMigrationFixture(t, []string{testAgentArtifact, supersededMaintainer})
	held, err := flock.Acquire(filepath.Join(f.root, filepath.FromSlash(migrationLock)), true, true)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotMigration(t, f.root)
	for _, mode := range []string{agentctx.AgentContentMigrationApply, agentctx.AgentContentMigrationRollback} {
		if _, err := runMigration(t, f.root, mode); err == nil || !strings.Contains(err.Error(), "another agent-content migration is running") {
			t.Fatalf("%s while another run holds the lock = %v", mode, err)
		}
	}
	assertUnchangedWorkspace(t, before, snapshotMigration(t, f.root))
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if out, err := runMigration(t, f.root, agentctx.AgentContentMigrationApply); err != nil {
		t.Fatalf("apply once the lock is free: %v\n%s", err, out)
	}
}
