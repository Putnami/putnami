package agentctx

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// constraintsPath is the human-maintained rules file the guidance block names.
// Putnami reads it and never writes it.
const constraintsPath = ".agents/constraints.md"

func TestWriteAgentEntrypointsCreatesOnlyTheEntrypoints(t *testing.T) {
	spectest.Proves(t, "cli/canonical-workspace-guidance", "onboarding-path", "generated-context-presents-the-workflow-path")
	spectest.Proves(t, "cli/canonical-workspace-guidance", "generated-agent-guidance", "generated-bootstrap-routes-to-live-mcp-guidance")
	dir := t.TempDir()

	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("WriteAgentEntrypoints: %v", err)
	}

	for _, relPath := range []string{ClaudeEntrypointPath, agentEntrypointPath} {
		if _, err := os.Stat(filepath.Join(dir, relPath)); err != nil {
			t.Fatalf("expected %s to exist: %v", relPath, err)
		}
	}
	for _, unwanted := range []string{".agents", mcpConfigPath} {
		if _, err := os.Stat(filepath.Join(dir, unwanted)); !os.IsNotExist(err) {
			t.Fatalf("Putnami created %s in a workspace it does not own: stat err %v", unwanted, err)
		}
	}

	if claude := readGuidance(t, dir, ClaudeEntrypointPath); claude != "# CLAUDE.md\n\n@AGENTS.md\n" {
		t.Errorf("CLAUDE.md = %q, want the AGENTS.md import", claude)
	}
	agent := readGuidance(t, dir, agentEntrypointPath)
	for _, want := range []string{
		"This is a Putnami workspace.",
		"`./putnamiw` when present, otherwise `putnami`",
		// This temp dir declares no putnami.ci.json, so the gate is the
		// documented fallback. Pinning the literal here is deliberate: the
		// generic line a workspace without a CI document receives must not
		// move, and the derived line is pinned against the real document by
		// TestGeneratedGuidanceMatchesRepositoryCIDocument instead.
		"`putnami " + defaultGateTasks + " --impacted --enforce-coverage`",
		"While iterating, select the projects you changed with `--projects <a>,<b>`",
		"If the MCP root differs from your worktree, use the local CLI.",
		"Skills and agents that an extension's agent content installs under `.agents/`, `.claude/` and `.codex/` are managed by Putnami.",
		"Read `.agents/constraints.md` when it exists.",
	} {
		if !strings.Contains(agent, want) {
			t.Errorf("AGENTS.md missing %q", want)
		}
	}
	// The guidance block is the whole of what Putnami writes, so it must stay
	// small enough to read at the top of an entrypoint the user owns.
	if len(agent) > 2048 {
		t.Fatalf("generated AGENTS.md is %d bytes, want at most 2048", len(agent))
	}
}

// repositoryRootForAIContext walks up from this source file to the directory
// holding putnami.workspace.json so repository-parity checks use the real
// generated entrypoints and CI declaration.
func repositoryRootForAIContext(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("putnami.workspace.json not found above " + file)
		}
		dir = parent
	}
}

// The human constraints file is read by the guidance block and written by
// nobody: an existing one keeps its bytes, and a missing one is not created.
func TestWriteAgentEntrypointsNeverWritesTheConstraintsFile(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "generated-context", "the-human-constraints-file-is-preserved")
	dir := t.TempDir()
	const custom = "# Shared AI Constraints\n\nKeep this line.\n"

	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(constraintsPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, constraintsPath), []byte(custom), 0o644); err != nil {
		t.Fatalf("write constraints: %v", err)
	}

	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("WriteAgentEntrypoints: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, constraintsPath))
	if err != nil {
		t.Fatalf("read constraints: %v", err)
	}
	if string(data) != custom {
		t.Errorf("constraints file was overwritten:\nwant %q\ngot  %q", custom, string(data))
	}

	// A workspace without the file keeps not having it.
	empty := t.TempDir()
	if err := WriteAgentEntrypoints(empty); err != nil {
		t.Fatalf("WriteAgentEntrypoints: %v", err)
	}
	if _, err := os.Stat(filepath.Join(empty, constraintsPath)); !os.IsNotExist(err) {
		t.Fatalf("a starter constraints file was created: stat err %v", err)
	}
}

// This repository's hand-maintained constraints name `putnami qualify` as the
// local proof for a reachable workload. The sentence that forbade a
// public qualification command predates the command and must not come back.
func TestConstraintsNameQualifyAsLocalProof(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRootForAIContext(t), constraintsPath))
	if err != nil {
		t.Fatalf("read constraints: %v", err)
	}
	constraints := string(data)
	for _, gone := range []string{
		"an unimplemented public qualification command",
		"owns the missing generic finite composition and evidence capability",
	} {
		if strings.Contains(constraints, gone) {
			t.Errorf("%s still carries the retired prohibition %q", constraintsPath, gone)
		}
	}
	for _, want := range []string{
		"Local execution proof for a reachable workload is `putnami qualify <project> --target local`; its verdict is the evidence.",
		"A library-only or docs-only change reports `NOT APPLICABLE` with the reason.",
	} {
		if !strings.Contains(constraints, want) {
			t.Errorf("%s is missing %q", constraintsPath, want)
		}
	}
}

func TestContextGenerateUsesLockedExtensionWhenStableStoreIsAhead(t *testing.T) {
	const (
		name         = "@putnami/test"
		lockedVer    = "1.0.0"
		aheadVer     = "2.0.0"
		lockedDigest = "1111111111111111111111111111111111111111111111111111111111111111"
		aheadDigest  = "2222222222222222222222222222222222222222222222222222222222222222"
		lockedGuide  = "# Locked guidance\n\nUse the lock-pinned workflow.\n"
		aheadGuide   = "# Ahead guidance\n\nDo not use this workflow.\n"
	)

	dir := t.TempDir()
	storeRoot := t.TempDir()
	t.Setenv("PUTNAMI_ARTIFACT_DIR", storeRoot)
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{name: "latest"}}}

	lockedDir := sharedtest.WriteContextTestExtension(t, storeRoot, lockedDigest, name, lockedVer, lockedGuide)
	manifestHash, err := lockfile.HashFile(filepath.Join(lockedDir, "putnami.extension.json"))
	if err != nil {
		t.Fatalf("hash locked manifest: %v", err)
	}
	lf := lockfile.NewLockFile()
	lf.SetExtension(name, lockfile.LockEntry{
		Version:      lockedVer,
		ManifestHash: manifestHash,
		Integrities: map[string]string{
			lockfile.PlatformKey(runtime.GOOS, runtime.GOARCH): lockedDigest,
		},
	})
	if err := lockfile.WriteLockFile(dir, lf); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	// The empty workspace resolves the lock-pinned artifact from the store.
	firstResult, err := ContextGenerateWithResult(dir, cfg, nil)
	if err != nil {
		t.Fatalf("generate from empty workspace: %v", err)
	}
	if !firstResult.GuidanceChanged {
		t.Fatalf("first generation result = %+v, want a guidance write", firstResult)
	}
	empty := readGeneratedGuidance(t, dir)

	// An ahead resident stable link must be replaced with the locked artifact
	// before generation. This is the regression fixture for a developer store
	// that has advanced beyond the checkout's putnami.lock.json.
	aheadDir := sharedtest.WriteContextTestExtension(t, storeRoot, aheadDigest, name, aheadVer, aheadGuide)
	if err := layout.LinkArtifactGlobal(dir, layout.Extensions, name, aheadDir); err != nil {
		t.Fatalf("link ahead extension: %v", err)
	}
	secondResult, err := ContextGenerateWithResult(dir, cfg, nil)
	if err != nil {
		t.Fatalf("generate from ahead store: %v", err)
	}
	if secondResult.GuidanceChanged || len(secondResult.Preserved) != 0 {
		t.Fatalf("byte-identical generation result = %+v, want no reported file change", secondResult)
	}
	ahead := readGeneratedGuidance(t, dir)
	for path, want := range empty {
		if got := ahead[path]; got != want {
			t.Errorf("%s changed with an ahead resident store", path)
		}
	}

	stableManifest, err := os.ReadFile(filepath.Join(layout.StableDir(dir, layout.Extensions, name), "putnami.extension.json"))
	if err != nil {
		t.Fatalf("read lock-faithful stable manifest: %v", err)
	}
	if !strings.Contains(string(stableManifest), `"version":"`+lockedVer+`"`) {
		t.Fatalf("stable extension was not restored to lock version %s: %s", lockedVer, stableManifest)
	}
}

func TestContextGenerateLockStoreMismatchLeavesGuidanceUntouched(t *testing.T) {
	const name = "@putnami/test"
	dir := t.TempDir()
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{name: "latest"}}}
	lf := lockfile.NewLockFile()
	lf.SetExtension(name, lockfile.LockEntry{Version: "1.0.0"})
	if err := lockfile.WriteLockFile(dir, lf); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	before := map[string]string{
		ClaudeEntrypointPath: "old claude\n",
		agentEntrypointPath:  "old agents\n",
	}
	for path, content := range before {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	t.Setenv("PUTNAMI_REGISTRY_URL", "http://127.0.0.1:1")
	t.Setenv(extension.HTTPRetryBackoffEnv, "0")
	err := ContextGenerateWithWriter(dir, cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "lock/store mismatch") || !strings.Contains(err.Error(), name) {
		t.Fatalf("ContextGenerateWithWriter error = %v, want useful lock/store mismatch", err)
	}
	for path, want := range before {
		got, readErr := os.ReadFile(filepath.Join(dir, path))
		if readErr != nil {
			t.Fatalf("read %s after failed generation: %v", path, readErr)
		}
		if string(got) != want {
			t.Errorf("%s was written before lock validation failed: got %q, want %q", path, got, want)
		}
	}
}

func readGeneratedGuidance(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := []string{ClaudeEntrypointPath, agentEntrypointPath}
	result := make(map[string]string, len(files))
	for _, path := range files {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			t.Fatalf("read generated %s: %v", path, err)
		}
		result[path] = string(data)
	}
	return result
}

func TestReadExtensionGuidanceBoundsAndRejectsSpecialFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "AI.md")
	exact := strings.Repeat("a", maxExtensionGuidanceSize)
	if err := os.WriteFile(path, []byte(exact), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadExtensionGuidance(root)
	if err != nil || got != exact {
		t.Fatalf("guidance at limit: len=%d err=%v", len(got), err)
	}

	if err := os.WriteFile(path, []byte(exact+"b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExtensionGuidance(root); err == nil || !strings.Contains(err.Error(), "guidance limit") {
		t.Fatalf("oversized guidance error = %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExtensionGuidance(root); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory guidance error = %v", err)
	}

	if runtime.GOOS != "windows" {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.md")
		if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadExtensionGuidance(root); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("symlink guidance error = %v", err)
		}
	}
}

func TestWriteAgentEntrypointsForceReclaimsPreservedGuidance(t *testing.T) {
	spectest.Proves(t, "cli/context-mcp-discovery", "read-preparation", "human-and-agent-artifact-ownership-files-are-preserved")
	dir := t.TempDir()
	if err := WriteAgentEntrypoints(dir); err != nil {
		t.Fatalf("seed generated guidance: %v", err)
	}
	paths := []string{ClaudeEntrypointPath, agentEntrypointPath}
	for _, path := range paths {
		if err := os.WriteFile(filepath.Join(dir, path), []byte("# hand written\n"), 0o644); err != nil {
			t.Fatalf("edit %s: %v", path, err)
		}
	}
	// Human constraints are never generated, so force must not touch them.
	constraints := filepath.Join(dir, constraintsPath)
	humanConstraints := "# my rules\nkeep me\n"
	if err := os.MkdirAll(filepath.Dir(constraints), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(constraints, []byte(humanConstraints), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := writeAgentEntrypoints(dir, true)
	if err != nil {
		t.Fatalf("forced regeneration: %v", err)
	}
	if len(report.Preserved) != 0 {
		t.Fatalf("forced regeneration still preserved %v", report.Preserved)
	}
	for _, path := range paths {
		data := readGuidance(t, dir, path)
		if withoutBlock(t, data) != "# hand written\n" {
			t.Errorf("forced %s changed bytes outside the block:\n%s", path, data)
		}
	}
	if data, err := os.ReadFile(constraints); err != nil || string(data) != humanConstraints {
		t.Fatalf("force rewrote human-maintained constraints: %q (%v)", data, err)
	}
}

func TestContextGenerateRejectsUnknownOptions(t *testing.T) {
	if force, err := parseContextGenerateArgs([]string{"--force"}); err != nil || !force {
		t.Fatalf("parseContextGenerateArgs(--force) = %v, %v", force, err)
	}
	if force, err := parseContextGenerateArgs(nil); err != nil || force {
		t.Fatalf("parseContextGenerateArgs(nil) = %v, %v", force, err)
	}
	_, err := parseContextGenerateArgs([]string{"--forse"})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("parseContextGenerateArgs typo error = %v, want it to name --force", err)
	}
}
