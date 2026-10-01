package pkg

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"

	"go.putnami.dev/protocol/features/spectest"
)

// --- resolveVersion ---

func TestResolveVersion_FromWorkspace(t *testing.T) {
	dir := t.TempDir()
	ctx := &pctx.Context{
		Workspace: pctx.Workspace{Version: "1.2.3"},
		Project: pctx.Project{
			Name:     "test-project",
			FullPath: dir,
		},
	}

	version, name := resolveVersion(ctx)
	if version != "1.2.3" {
		t.Errorf("version = %q, want %q", version, "1.2.3")
	}
	if name != "test-project" {
		t.Errorf("name = %q, want %q", name, "test-project")
	}
}

func TestResolveVersion_FromPackageJSON(t *testing.T) {
	dir := t.TempDir()
	pkgJSON := map[string]string{"name": "@putnami/go", "version": "2.0.0"}
	data, _ := json.Marshal(pkgJSON)
	os.WriteFile(filepath.Join(dir, "package.json"), data, 0o644)

	ctx := &pctx.Context{
		Project: pctx.Project{
			Name:     "fallback-name",
			FullPath: dir,
		},
	}

	version, name := resolveVersion(ctx)
	if version != "2.0.0" {
		t.Errorf("version = %q, want %q", version, "2.0.0")
	}
	if name != "@putnami/go" {
		t.Errorf("name = %q, want %q", name, "@putnami/go")
	}
}

func TestResolveVersion_WorkspaceOverridesPackageJSON(t *testing.T) {
	dir := t.TempDir()
	pkgJSON := map[string]string{"name": "@putnami/go", "version": "2.0.0"}
	data, _ := json.Marshal(pkgJSON)
	os.WriteFile(filepath.Join(dir, "package.json"), data, 0o644)

	ctx := &pctx.Context{
		Workspace: pctx.Workspace{Version: "1.0.0"},
		Project: pctx.Project{
			Name:     "fallback",
			FullPath: dir,
		},
	}

	version, name := resolveVersion(ctx)
	if version != "1.0.0" {
		t.Errorf("workspace version should take priority: got %q, want %q", version, "1.0.0")
	}
	if name != "@putnami/go" {
		t.Errorf("name should come from package.json: got %q, want %q", name, "@putnami/go")
	}
}

func TestResolveVersion_FallbackZero(t *testing.T) {
	dir := t.TempDir()
	ctx := &pctx.Context{
		Project: pctx.Project{
			Name:     "test",
			FullPath: dir,
		},
	}

	version, _ := resolveVersion(ctx)
	if version != "0.0.0" {
		t.Errorf("version = %q, want %q", version, "0.0.0")
	}
}

func TestResolveVersion_StripsPreRelease(t *testing.T) {
	dir := t.TempDir()
	ctx := &pctx.Context{
		Workspace: pctx.Workspace{Version: "1.2.3-beta.1"},
		Project: pctx.Project{
			Name:     "test",
			FullPath: dir,
		},
	}

	version, _ := resolveVersion(ctx)
	if version != "1.2.3" {
		t.Errorf("version = %q, want %q (pre-release should be stripped)", version, "1.2.3")
	}
}

// --- gitSuffix ---

func TestGitSuffix_NoGitRepo(t *testing.T) {
	dir := t.TempDir()
	result := gitSuffix(dir)
	if result != "nogit" {
		t.Errorf("gitSuffix for non-git dir = %q, want %q", result, "nogit")
	}
}

// --- resolveSuffix ---

// resolveSuffix must prefer ctx.Version.Suffix so that all projects in a run
// publish with the same suffix, even if jobs that ran earlier dirtied the tree
// with temp/output files.
func TestResolveSuffix_PrefersContextVersion(t *testing.T) {
	dir := t.TempDir()
	ctx := &pctx.Context{
		WorkspaceRoot: dir,
		Version:       &pctx.Version{SHA: "a77d9ae1", Suffix: "a77d9ae1"},
	}

	got := resolveSuffix(ctx)
	if got != "a77d9ae1" {
		t.Errorf("resolveSuffix = %q, want %q (must use ctx.Version.Suffix even with no git repo)", got, "a77d9ae1")
	}
}

func TestResolveSuffix_FallsBackToGitWhenNoVersion(t *testing.T) {
	dir := t.TempDir()
	ctx := &pctx.Context{WorkspaceRoot: dir}

	got := resolveSuffix(ctx)
	if got != "nogit" {
		t.Errorf("resolveSuffix with no ctx.Version and no git = %q, want %q", got, "nogit")
	}
}

// --- copyRel ---

func TestCopyRel_File(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	os.WriteFile(filepath.Join(srcDir, "test.txt"), []byte("content"), 0o644)
	copyRel(srcDir, dstDir, "test.txt")

	got, err := os.ReadFile(filepath.Join(dstDir, "test.txt"))
	if err != nil {
		t.Fatalf("file not copied: %v", err)
	}
	if string(got) != "content" {
		t.Errorf("content = %q, want %q", string(got), "content")
	}
}

func TestCopyRel_Directory(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	subDir := filepath.Join(srcDir, "subdir")
	os.MkdirAll(subDir, 0o755)
	os.WriteFile(filepath.Join(subDir, "file.txt"), []byte("nested"), 0o644)

	copyRel(srcDir, dstDir, "subdir")

	got, err := os.ReadFile(filepath.Join(dstDir, "subdir", "file.txt"))
	if err != nil {
		t.Fatalf("nested file not copied: %v", err)
	}
	if string(got) != "nested" {
		t.Errorf("content = %q, want %q", string(got), "nested")
	}
}

func TestCopyRel_NonexistentSource(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Should not panic or error, just silently skip
	copyRel(srcDir, dstDir, "nonexistent")

	if _, err := os.Stat(filepath.Join(dstDir, "nonexistent")); !os.IsNotExist(err) {
		t.Error("nonexistent source should not create destination")
	}
}

// --- safeName ---

func TestSafeName_Comprehensive(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"@putnami/go", "@putnami-go"},
		{"simple", "simple"},
		{"a:b", "a-b"},
		{"a/b/c", "a-b-c"},
		{"a\\b", "a-b"},
		{"a:b/c\\d", "a-b-c-d"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := safeName(tt.input)
			if got != tt.want {
				t.Errorf("safeName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- resolveString / resolveBool ---

func TestResolveString_FlagPriority(t *testing.T) {
	flags := map[string]string{"key": "from-flag"}
	params := pctx.Params{"key": json.RawMessage(`"from-param"`)}

	got := resolveString(flags, "key", params)
	if got != "from-flag" {
		t.Errorf("resolveString = %q, want %q (flag should take priority)", got, "from-flag")
	}
}

func TestResolveString_FallbackToParams(t *testing.T) {
	flags := map[string]string{}
	params := pctx.Params{"key": json.RawMessage(`"from-param"`)}

	got := resolveString(flags, "key", params)
	if got != "from-param" {
		t.Errorf("resolveString = %q, want %q", got, "from-param")
	}
}

func TestResolveString_FallbackParamKeys(t *testing.T) {
	flags := map[string]string{}
	params := pctx.Params{"altKey": json.RawMessage(`"from-alt"`)}

	got := resolveString(flags, "key", params, "altKey")
	if got != "from-alt" {
		t.Errorf("resolveString = %q, want %q", got, "from-alt")
	}
}

func TestResolveString_NoMatch(t *testing.T) {
	flags := map[string]string{}
	params := pctx.Params{}

	got := resolveString(flags, "key", params)
	if got != "" {
		t.Errorf("resolveString = %q, want empty", got)
	}
}

func TestResolveBool_FlagPriority(t *testing.T) {
	flags := map[string]string{"dry-run": "true"}
	params := pctx.Params{"dry-run": json.RawMessage(`false`)}

	got := resolveBool(flags, "dry-run", params)
	if !got {
		t.Error("resolveBool should return true from flag")
	}
}

func TestResolveBool_FallbackToParams(t *testing.T) {
	flags := map[string]string{}
	params := pctx.Params{"dryRun": json.RawMessage(`true`)}

	got := resolveBool(flags, "dry-run", params, "dryRun")
	if !got {
		t.Error("resolveBool should return true from params fallback key")
	}
}

func TestResolveBool_Default(t *testing.T) {
	flags := map[string]string{}
	params := pctx.Params{}

	got := resolveBool(flags, "dry-run", params)
	if got {
		t.Error("resolveBool should return default false")
	}
}

// --- stampManifestVersion ---

func TestStampManifestVersion(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "putnami.extension.json")
	manifest := map[string]any{"commands": map[string]any{}}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	os.WriteFile(manifestPath, data, 0o644)

	stampManifestVersion(dir, "1.2.3")

	got, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	json.Unmarshal(got, &result)

	if result["version"] != "1.2.3" {
		t.Errorf("version = %v, want %q", result["version"], "1.2.3")
	}
}

func TestStampManifestVersion_NoManifest(t *testing.T) {
	dir := t.TempDir()
	// Should not panic when manifest doesn't exist
	stampManifestVersion(dir, "1.0.0")
}

// --- updateDependencyVersions ---

func TestUpdateDependencyVersions_ReplacesWorkspaceDeps(t *testing.T) {
	dir := t.TempDir()

	// Create a go.work file
	goWorkContent := "go 1.21\n\nuse (\n\t./go/framework/http\n)\n"
	os.WriteFile(filepath.Join(dir, "go.work"), []byte(goWorkContent), 0o644)

	// Create the module's go.mod
	httpModDir := filepath.Join(dir, "go", "framework", "http")
	os.MkdirAll(httpModDir, 0o755)
	os.WriteFile(filepath.Join(httpModDir, "go.mod"), []byte("module go.putnami.dev/http\n\ngo 1.21\n"), 0o644)

	ctx := &pctx.Context{WorkspaceRoot: dir}

	goMod := "module test\n\ngo 1.21\n\nrequire (\n\tgo.putnami.dev/http v0.0.1\n)\n"
	result := updateDependencyVersions(goMod, "v1.0.0", ctx)

	if result == goMod {
		t.Error("expected go.mod to be updated")
	}
	expected := "module test\n\ngo 1.21\n\nrequire (\n\tgo.putnami.dev/http v1.0.0\n)\n"
	if result != expected {
		t.Errorf("result = %q, want %q", result, expected)
	}
}

func TestUpdateDependencyVersions_NoWorkspaceModules(t *testing.T) {
	dir := t.TempDir()
	ctx := &pctx.Context{WorkspaceRoot: dir}

	goMod := "module test\n\ngo 1.21\n"
	result := updateDependencyVersions(goMod, "v1.0.0", ctx)

	if result != goMod {
		t.Error("go.mod should be unchanged when no workspace modules found")
	}
}

func TestRunSkipsWhenNoPackageChannelIsSelected(t *testing.T) {
	ctx := &pctx.Context{
		WorkspaceRoot: t.TempDir(),
		Project: pctx.Project{
			Name:     "@scope/demo",
			Path:     "project",
			FullPath: t.TempDir(),
		},
	}

	status, data, err := Run(ctx, nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status != "SKIP" || data != nil {
		t.Fatalf("Run = (%q, %v), want (SKIP, nil)", status, data)
	}
}

func TestRunDryRunDispatchesEveryExplicitChannelAndRecordsEachInsideItsOwnOutput(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "package-channel-index", "every-dispatched-channel-is-recorded-inside-the-output-that-produced-it")
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "project")
	outputPath := filepath.Join(workspaceRoot, "job-output")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(projectRoot, "go.mod"),
		[]byte("module example.com/demo\n\ngo 1.26\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(outputPath, "bin", "linux-x64", "putnami-go")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("prepared-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		OutputPath:    outputPath,
		Workspace:     pctx.Workspace{Version: "1.2.3"},
		Project: pctx.Project{
			Name:     "@scope/demo",
			Path:     "project",
			FullPath: projectRoot,
		},
		// The orchestrator resolved the project's line at a tagged commit, so
		// Full IS the tag's version and the artifact carries it verbatim.
		Version: &pctx.Version{Base: "1.2.3", Full: "1.2.3"},
	}
	// A sibling packager's record, in the directory that sibling owns. This run
	// must leave it alone and the index must keep reporting it.
	legacyDir := pkgmeta.PackageOutputDir(workspaceRoot, "project", "legacy")
	if err := pkgmeta.WriteChannelRecord(legacyDir, pkgmeta.ChannelRecord{Channels: []string{"legacy"}}); err != nil {
		t.Fatal(err)
	}

	var status string
	var data map[string]any
	var runErr error
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, data, runErr = Run(ctx, emit, []string{
			"--archives",
			"--template-archives",
			"--docker",
			"--go",
			"--dry-run",
		})
	})
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if status != "OK" || data != nil {
		t.Fatalf("Run = (%q, %v), want (OK, nil)", status, data)
	}
	if len(events) == 0 {
		t.Fatal("package dispatch emitted no lifecycle events")
	}

	// Every dispatched channel is recorded, each inside the directory that
	// produced it — so a cache restore of one packager's output restores its
	// channel without a sibling having to run.
	index, err := pkgmeta.ReadChannelIndex(workspaceRoot, "project")
	if err != nil {
		t.Fatalf("ReadChannelIndex: %v", err)
	}
	wantChannels := []string{"archives", "template-archives", "docker", "go", "legacy"}
	for _, want := range wantChannels {
		if !index.HasChannel(want) {
			t.Fatalf("channels = %v, want %q recorded", index.Channels, want)
		}
	}
	if len(index.Channels) != len(wantChannels) {
		t.Fatalf("channels = %v, want exactly %v", index.Channels, wantChannels)
	}
	if owner := index.Records["go"]; !slices.Contains(owner.Channels, "go") {
		t.Fatalf("go/channel.json = %+v, want the go channel recorded by its own packager", owner)
	}
	if owner := index.Records["docker"]; !slices.Contains(owner.Channels, "docker") {
		t.Fatalf("docker/channel.json = %+v, want the docker channel recorded by its own packager", owner)
	}
	archives := index.Records["archives"]
	if !slices.Contains(archives.Channels, "archives") && !slices.Contains(archives.Channels, "template-archives") {
		t.Fatalf("archives/channel.json = %+v, want an archive channel", archives)
	}

	// The archive publication manifest is the archive packager's own output and
	// carries the identity the archive uploader reads.
	meta, err := pkgmeta.ReadPackageMetadata(workspaceRoot, "project")
	if err != nil {
		t.Fatalf("ReadPackageMetadata: %v", err)
	}
	if meta.Version != "1.2.3" || meta.Artifact != "scope-demo" {
		t.Fatalf("archive publication manifest identity = %+v", meta)
	}
	if !meta.HasChannel("archives") && !meta.HasChannel("template-archives") {
		t.Fatalf("archive publication manifest channels = %v", meta.Channels)
	}
}

// TestPackageWritesOnlyInsideDeclaredOutputs is the invariant that makes every
// task under `package` ordinarily cacheable: a packager writes only
// where its task declares an output, so a cache restore of that declaration
// reproduces everything the task produced. A write outside one — the shared
// channel index this repository used to keep at the package root — is what
// forced the whole command out of declared capture.
func TestPackageWritesOnlyInsideDeclaredOutputs(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "package-channel-index", "packaging-writes-only-inside-the-outputs-its-tasks-declare")
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "project")
	outputPath := filepath.Join(workspaceRoot, ".putnami", "out", "project", "package")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "go.mod"),
		[]byte("module example.com/demo\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "demo.go"),
		[]byte("package demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The binary build-cross-compile owns, where package-archives reads it.
	binaryPath := filepath.Join(outputPath, "bin", "linux-x64", "demo")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte("prepared-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		OutputPath:    outputPath,
		Workspace:     pctx.Workspace{Version: "1.2.3"},
		Project: pctx.Project{
			Name:     "demo",
			Path:     "project",
			FullPath: projectRoot,
		},
		Version: &pctx.Version{Base: "1.2.3", Full: "1.2.3"},
		Params:  pctx.Params{"platforms": []byte(`["linux/amd64"]`)},
	}

	var status string
	captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, _ = Run(ctx, emit, []string{"--archives", "--go"})
	})
	if status != "OK" {
		t.Fatalf("Run status = %q, want OK", status)
	}

	// Every declared output of the tasks that ran, and nothing else. bin/ and
	// VERSION belong to build-cross-compile, archives/ to package-archives,
	// go/ to package-go, metadata.json to package-archives.
	declared := map[string]bool{
		"archives": true, "go": true, "docker": true, "bin": true,
		"VERSION": true, "metadata.json": true,
	}
	entries, err := os.ReadDir(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !declared[entry.Name()] {
			t.Errorf("packaging wrote %q, which no task under `package` declares as an output", entry.Name())
		}
	}

	// Each channel is recorded where its own packager owns the bytes.
	for channel, dir := range map[string]string{"archives": "archives", "go": "go"} {
		record, err := os.ReadFile(filepath.Join(outputPath, dir, pkgmeta.ChannelRecordFile))
		if err != nil {
			t.Fatalf("read %s/%s: %v", dir, pkgmeta.ChannelRecordFile, err)
		}
		var decoded pkgmeta.ChannelRecord
		if err := json.Unmarshal(record, &decoded); err != nil {
			t.Fatalf("parse %s record: %v", dir, err)
		}
		if !slices.Contains(decoded.Channels, channel) {
			t.Errorf("%s record = %+v, want the %q channel", dir, decoded, channel)
		}
	}
	index, err := pkgmeta.ReadChannelIndex(workspaceRoot, "project")
	if err != nil {
		t.Fatalf("ReadChannelIndex: %v", err)
	}
	if !index.HasChannel("archives") || !index.HasChannel("go") {
		t.Fatalf("derived index = %v, want both packaged channels", index.Channels)
	}
}

func TestBuildDockerImageRejectsFlatHostBinary(t *testing.T) {
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "project")
	outputPath := filepath.Join(workspaceRoot, "job-output")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	flatBinary := filepath.Join(outputPath, "bin", "demo")
	if err := os.MkdirAll(filepath.Dir(flatBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flatBinary, []byte("host-only"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		OutputPath:    outputPath,
		Project:       pctx.Project{Name: "demo", FullPath: projectRoot},
	}
	var built bool
	captureEvents(t, func(emit *jsonl.Emitter) {
		built = buildDockerImage(ctx, emit, dockerParams{Platform: "linux/amd64", DryRun: true}, t.TempDir())
	})
	if built {
		t.Fatal("docker package accepted removed flat bin/ host-build fallback")
	}
}

func TestRunFallsBackToDeclaredPublishChannels(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "package-channel-index", "the-declared-publish-channels-are-the-fallback-selection")
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "project")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		Workspace:     pctx.Workspace{Version: "2.3.4"},
		Project: pctx.Project{
			Name:     "@scope/demo",
			Path:     "project",
			FullPath: projectRoot,
			Publish:  json.RawMessage(`["extension-archives"]`),
		},
		Version: &pctx.Version{Suffix: "abc1234"},
	}

	var status string
	captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, _ = Run(ctx, emit, []string{"--dry-run"})
	})
	if status != "OK" {
		t.Fatalf("Run status = %q, want OK", status)
	}

	index, err := pkgmeta.ReadChannelIndex(workspaceRoot, "project")
	if err != nil {
		t.Fatalf("ReadChannelIndex: %v", err)
	}
	if len(index.Channels) != 1 || index.Channels[0] != "archives" {
		t.Fatalf("channels = %v, want [archives]", index.Channels)
	}
	if got := index.Records["archives"].Version; got != "2.3.4-abc1234" {
		t.Fatalf("archives record version = %q, want the resolved version", got)
	}
	meta, err := pkgmeta.ReadPackageMetadata(workspaceRoot, "project")
	if err != nil {
		t.Fatalf("ReadPackageMetadata: %v", err)
	}
	if meta.Version != "2.3.4-abc1234" {
		t.Fatalf("archive publication manifest = %+v", meta)
	}
}

func TestRunReturnsFailedWhenSelectedPackagerFails(t *testing.T) {
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "project")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		OutputPath:    filepath.Join(workspaceRoot, "missing-build-output"),
		Workspace:     pctx.Workspace{Version: "1.2.3"},
		Project: pctx.Project{
			Name:     "@scope/demo",
			Path:     "project",
			FullPath: projectRoot,
		},
	}

	var status string
	var runErr error
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, runErr = Run(ctx, emit, []string{"--docker", "--dry-run"})
	})
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if status != "FAILED" {
		t.Fatalf("Run status = %q, want FAILED", status)
	}
	if len(events) == 0 {
		t.Fatal("failed package dispatch emitted no diagnostic")
	}
	if _, err := pkgmeta.ReadChannelIndex(workspaceRoot, "project"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed package dispatch recorded a channel: %v", err)
	}
	metadataPath := filepath.Join(workspaceRoot, ".putnami", "out", "project", "package", "metadata.json")
	if _, err := os.Stat(metadataPath); !os.IsNotExist(err) {
		t.Fatalf("failed package dispatch wrote the archive publication manifest: %v", err)
	}
}
