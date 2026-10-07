package pkg

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/goembed"
	"go.putnami.dev/sdk/extension/jsonl"
)

func TestGoBuildAndTestTaskInputsDeclareRespectiveEmbedSelectors(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "embedded-source-cache-inputs", "go-build-and-test-task-inputs-declare-respective-embed-selectors")
	manifest, err := extproto.LoadManifest(filepath.Join(findRepoRoot(t), "go", "extension", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	buildTasks, testTasks := 0, 0
	for name, task := range manifest.Tasks {
		sources, ok := task.Inputs["sources"]
		if !ok {
			continue
		}
		if sources.From != extproto.TaskInputFromProject {
			t.Fatalf("%s sources are not project inputs", name)
		}
		selected := goembed.TestSelector
		if slices.Contains(sources.Files, "!**/*_test.go") {
			selected = goembed.BuildSelector
			buildTasks++
		} else {
			testTasks++
		}
		if !slices.Contains(sources.Files, selected) {
			t.Errorf("%s misses %s", name, selected)
		}
	}
	if buildTasks < 4 || testTasks == 0 {
		t.Fatalf("unexpected task coverage: %d build, %d test", buildTasks, testTasks)
	}
}

func TestGoModulePackagerSharesEmbedTargetResolutionWithTaskInputs(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "embedded-source-cache-inputs", "go-module-packager-shares-embed-target-resolution-with-task-inputs")
	project := t.TempDir()
	stage := t.TempDir()
	source := "package example\nimport _ \"embed\"\n//go:embed \"assets/report one.json\"\nvar data []byte\n"
	mustWrite(t, filepath.Join(project, "embed.go"), source)
	mustWrite(t, filepath.Join(project, "assets", "report one.json"), `{"value":1}`)
	mustWrite(t, filepath.Join(stage, "embed.go"), source)
	inputs, err := goembed.Resolve(project, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 {
		t.Fatalf("resolved inputs = %v", inputs)
	}
	if err := stageEmbedTargets(project, stage); err != nil {
		t.Fatal(err)
	}
	staged, err := os.ReadFile(filepath.Join(stage, "assets", "report one.json"))
	if err != nil || string(staged) != `{"value":1}` {
		t.Fatalf("staged input = %q, %v", staged, err)
	}
}

// Every //go:embed target must ship in the staged module source and the
// published zip, whatever its name or extension. A module that embeds
// conformance/manifest.json must not lose it to the staging allowlist, or every
// downstream `go build` fails with "no matching files found".
func TestPrepareGoModuleStagesEmbedTargets(t *testing.T) {
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()

	mustWrite(t, filepath.Join(projectRoot, "go.mod"), "module example.com/parent\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(projectRoot, "embed.go"),
		"package parent\n\nimport _ \"embed\"\n\n//go:embed conformance/manifest.json\nvar manifest []byte\n")
	mustWrite(t, filepath.Join(projectRoot, "conformance", "manifest.json"), `{"cases":[]}`)
	mustWrite(t, filepath.Join(projectRoot, "conformance", "unrelated.json"), `{}`)

	// A glob pattern in a subdirectory package, plus an all:-prefixed
	// directory pattern that must pick up dot-files.
	mustWrite(t, filepath.Join(projectRoot, "sub", "sub.go"),
		"package sub\n\nimport \"embed\"\n\n//go:embed schemas/*.json all:static\nvar assets embed.FS\n")
	mustWrite(t, filepath.Join(projectRoot, "sub", "schemas", "a.json"), `{}`)
	mustWrite(t, filepath.Join(projectRoot, "sub", "schemas", "b.json"), `{}`)
	mustWrite(t, filepath.Join(projectRoot, "sub", "schemas", "notes.txt"), "not embedded")
	mustWrite(t, filepath.Join(projectRoot, "sub", "static", ".hidden"), "hidden but embedded via all:")
	mustWrite(t, filepath.Join(projectRoot, "sub", "static", "app.css"), "body{}")

	ctx := &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "parent",
			Path:     "parent",
			FullPath: projectRoot,
		},
	}

	if ok := prepareGoModule(ctx, jsonl.New(), "1.2.3", outputRoot, false); !ok {
		t.Fatal("prepareGoModule failed")
	}

	stageDir := filepath.Join(outputRoot, "go", "source")
	for _, want := range []string{
		filepath.Join("conformance", "manifest.json"),
		filepath.Join("sub", "schemas", "a.json"),
		filepath.Join("sub", "schemas", "b.json"),
		filepath.Join("sub", "static", ".hidden"),
		filepath.Join("sub", "static", "app.css"),
	} {
		if _, err := os.Stat(filepath.Join(stageDir, want)); err != nil {
			t.Errorf("staged module is missing embed target %s: %v", want, err)
		}
	}
	for _, unwanted := range []string{
		filepath.Join("conformance", "unrelated.json"),
		filepath.Join("sub", "schemas", "notes.txt"),
	} {
		if _, err := os.Stat(filepath.Join(stageDir, unwanted)); !os.IsNotExist(err) {
			t.Errorf("non-embedded file %s was staged; want it excluded (err=%v)", unwanted, err)
		}
	}

	zipPath := filepath.Join(outputRoot, "go", "example.com/parent@v1.2.3.zip")
	entries := zipEntryNames(t, zipPath)
	for _, want := range []string{
		"example.com/parent@v1.2.3/conformance/manifest.json",
		"example.com/parent@v1.2.3/sub/schemas/a.json",
		"example.com/parent@v1.2.3/sub/static/.hidden",
	} {
		if !entries[want] {
			t.Errorf("module zip is missing embed target entry %s", want)
		}
	}
}

// A go:embed pattern that matches nothing means the published module can
// never compile, so packaging must fail rather than produce the zip.
func TestPrepareGoModuleFailsWhenEmbedTargetMissing(t *testing.T) {
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()

	mustWrite(t, filepath.Join(projectRoot, "go.mod"), "module example.com/parent\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(projectRoot, "embed.go"),
		"package parent\n\nimport _ \"embed\"\n\n//go:embed conformance/manifest.json\nvar manifest []byte\n")

	ctx := &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "parent",
			Path:     "parent",
			FullPath: projectRoot,
		},
	}

	if ok := prepareGoModule(ctx, jsonl.New(), "1.2.3", outputRoot, false); ok {
		t.Fatal("prepareGoModule succeeded with a go:embed pattern that matches no files; want failure")
	}
}

// verifyZipEmbedTargets is the artifact-level backstop: it must reject a
// finished zip whose .go entries declare embed patterns with no matching zip
// entry, and accept one where every pattern resolves.
func TestVerifyZipEmbedTargets(t *testing.T) {
	const prefix = "example.com/parent@v1.2.3/"
	goSource := "package parent\n\nimport _ \"embed\"\n\n//go:embed conformance/manifest.json\nvar manifest []byte\n"

	broken := writeTestZip(t, "broken.zip", map[string]string{
		prefix + "go.mod":   "module example.com/parent\n\ngo 1.24\n",
		prefix + "embed.go": goSource,
	})
	if err := verifyZipEmbedTargets(broken, "example.com/parent", "v1.2.3"); err == nil {
		t.Error("verifyZipEmbedTargets accepted a zip missing an embed target; want rejection")
	} else if !strings.Contains(err.Error(), "conformance/manifest.json") {
		t.Errorf("rejection should name the missing pattern, got: %v", err)
	}

	complete := writeTestZip(t, "complete.zip", map[string]string{
		prefix + "go.mod":                    "module example.com/parent\n\ngo 1.24\n",
		prefix + "embed.go":                  goSource,
		prefix + "conformance/manifest.json": `{"cases":[]}`,
	})
	if err := verifyZipEmbedTargets(complete, "example.com/parent", "v1.2.3"); err != nil {
		t.Errorf("verifyZipEmbedTargets rejected a complete zip: %v", err)
	}
}

// Explicit coverage for the module that shipped broken: packaging the real
// protocols/transaction project must place conformance/manifest.json in the
// zip, and must rewrite its single-line diagnostic requirement to the publish
// version (the v0.0.0 placeholder is unpublishable and 404s for consumers).
func TestPrepareGoModulePackagesTransactionProtocol(t *testing.T) {
	repoRoot := findRepoRoot(t)
	projectRoot := filepath.Join(repoRoot, "protocols", "transaction")
	if _, err := os.Stat(filepath.Join(projectRoot, "conformance", "manifest.json")); err != nil {
		t.Skipf("protocols/transaction not available: %v", err)
	}
	outputRoot := t.TempDir()

	ctx := &pctx.Context{
		WorkspaceRoot: repoRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "go.putnami.dev/protocol/transaction",
			Path:     "protocols/transaction",
			FullPath: projectRoot,
		},
	}

	if ok := prepareGoModule(ctx, jsonl.New(), "0.1.0-embedtest", outputRoot, false); !ok {
		t.Fatal("prepareGoModule failed for protocols/transaction")
	}

	zipPath := filepath.Join(outputRoot, "go", "go.putnami.dev/protocol/transaction@v0.1.0-embedtest.zip")
	entries := zipEntryNames(t, zipPath)
	if !entries["go.putnami.dev/protocol/transaction@v0.1.0-embedtest/conformance/manifest.json"] {
		t.Error("transaction module zip is missing conformance/manifest.json")
	}

	stagedGoMod, err := os.ReadFile(filepath.Join(outputRoot, "go", "source", "go.mod"))
	if err != nil {
		t.Fatalf("read staged go.mod: %v", err)
	}
	if strings.Contains(string(stagedGoMod), "v0.0.0") {
		t.Errorf("staged go.mod still pins a workspace dep at v0.0.0:\n%s", stagedGoMod)
	}
	if !strings.Contains(string(stagedGoMod), "go.putnami.dev/protocol/diagnostic v0.1.0-embedtest") {
		t.Errorf("staged go.mod did not rewrite the diagnostic requirement to the publish version:\n%s", stagedGoMod)
	}
}

func TestRewriteWorkspaceDeps(t *testing.T) {
	modules := []string{"go.putnami.dev/protocol/diagnostic", "go.putnami.dev/app"}

	singleLine := "module m\n\ngo 1.24\n\nrequire go.putnami.dev/protocol/diagnostic v0.0.0\n"
	got := rewriteWorkspaceDeps(singleLine, "v0.1.0-abc", modules)
	if !strings.Contains(got, "require go.putnami.dev/protocol/diagnostic v0.1.0-abc") {
		t.Errorf("single-line require not rewritten:\n%s", got)
	}

	block := "module m\n\ngo 1.24\n\nrequire (\n\tgo.putnami.dev/app v0.0.1\n\texample.com/other v1.0.0\n)\n"
	got = rewriteWorkspaceDeps(block, "v0.1.0-abc", modules)
	if !strings.Contains(got, "\tgo.putnami.dev/app v0.1.0-abc") {
		t.Errorf("block require entry not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "example.com/other v1.0.0") {
		t.Errorf("non-workspace dep must keep its version:\n%s", got)
	}
}

// Only whole "." / ".." path elements are invalid in embed patterns; names
// merely containing dots (report..json) are legal and must resolve.
func TestResolveEmbedPatternDotHandling(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "assets", "report..json"), `{}`)

	files, err := resolveEmbedPattern(dir, "assets/report..json")
	if err != nil {
		t.Fatalf("pattern with dot-dot inside a name must resolve: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("resolveEmbedPattern matched %d files, want 1", len(files))
	}

	for _, bad := range []string{"../secret.json", "./x.json", "a/../b.json", "/abs.json", "a//b.json"} {
		if _, err := resolveEmbedPattern(dir, bad); err == nil {
			t.Errorf("pattern %q must be rejected", bad)
		}
	}
}

// A symlink matched directly by an embed pattern must fail packaging (go
// build rejects it as an irregular file), never be followed — following it
// would stage out-of-module content under an in-module path. Inside an
// embedded directory tree, symlinks are skipped silently like go build does.
func TestStageEmbedTargetsSymlinks(t *testing.T) {
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "ci-credential")

	projectRoot := t.TempDir()
	outputRoot := t.TempDir()
	mustWrite(t, filepath.Join(projectRoot, "go.mod"), "module example.com/parent\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(projectRoot, "embed.go"),
		"package parent\n\nimport _ \"embed\"\n\n//go:embed assets/link.txt\nvar leaked []byte\n")
	if err := os.MkdirAll(filepath.Join(projectRoot, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(projectRoot, "assets", "link.txt")); err != nil {
		t.Fatal(err)
	}

	ctx := &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project:       pctx.Project{Name: "parent", Path: "parent", FullPath: projectRoot},
	}
	if ok := prepareGoModule(ctx, jsonl.New(), "1.2.3", outputRoot, false); ok {
		t.Fatal("prepareGoModule staged a symlinked embed target; want failure")
	}

	// Directory embedding: the symlink is skipped, the regular sibling ships.
	projectRoot2 := t.TempDir()
	outputRoot2 := t.TempDir()
	mustWrite(t, filepath.Join(projectRoot2, "go.mod"), "module example.com/parent\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(projectRoot2, "embed.go"),
		"package parent\n\nimport \"embed\"\n\n//go:embed assets\nvar assets embed.FS\n")
	mustWrite(t, filepath.Join(projectRoot2, "assets", "real.txt"), "shipped")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(projectRoot2, "assets", "link.txt")); err != nil {
		t.Fatal(err)
	}
	ctx2 := &pctx.Context{
		WorkspaceRoot: projectRoot2,
		OutputPath:    filepath.Join(outputRoot2, "build"),
		Project:       pctx.Project{Name: "parent", Path: "parent", FullPath: projectRoot2},
	}
	if ok := prepareGoModule(ctx2, jsonl.New(), "1.2.3", outputRoot2, false); !ok {
		t.Fatal("prepareGoModule failed on a directory embed containing a symlink")
	}
	stageDir := filepath.Join(outputRoot2, "go", "source")
	if _, err := os.Stat(filepath.Join(stageDir, "assets", "real.txt")); err != nil {
		t.Errorf("regular file in embedded directory was not staged: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(stageDir, "assets", "link.txt")); !os.IsNotExist(err) {
		t.Errorf("symlink inside embedded directory was staged; want it skipped (err=%v)", err)
	}
}

// go build rejects embedding across a nested-module boundary ("in different
// module"); packaging must too, instead of shipping a file from a directory
// whose go.mod the staging walk skipped.
func TestStageEmbedTargetsRejectsNestedModuleTarget(t *testing.T) {
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()
	mustWrite(t, filepath.Join(projectRoot, "go.mod"), "module example.com/parent\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(projectRoot, "embed.go"),
		"package parent\n\nimport _ \"embed\"\n\n//go:embed submodule/config.json\nvar cfg []byte\n")
	mustWrite(t, filepath.Join(projectRoot, "submodule", "go.mod"), "module example.com/parent/submodule\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(projectRoot, "submodule", "config.json"), `{}`)

	ctx := &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project:       pctx.Project{Name: "parent", Path: "parent", FullPath: projectRoot},
	}
	if ok := prepareGoModule(ctx, jsonl.New(), "1.2.3", outputRoot, false); ok {
		t.Fatal("prepareGoModule embedded a file from a nested module; want failure")
	}
}

// Files the build never compiles must not contribute embed directives: a
// conventional `//go:build ignore` generator script may embed generator-only
// inputs absent from the module, and go build still succeeds. Platform-gated
// files are NOT excluded — their embeds must ship for that platform's builds.
func TestEmbedScanningHonorsBuildConstraints(t *testing.T) {
	if patterns, err := goEmbedPatterns("gen.go",
		[]byte("//go:build ignore\n\npackage main\n\nimport _ \"embed\"\n\n//go:embed gen-input.json\nvar in []byte\n")); err != nil || patterns != nil {
		t.Errorf("ignore-constrained file contributed patterns %v (err=%v); want none", patterns, err)
	}
	if patterns, err := goEmbedPatterns("gen.go",
		[]byte("// +build ignore\n\npackage main\n\nimport _ \"embed\"\n\n//go:embed gen-input.json\nvar in []byte\n")); err != nil || patterns != nil {
		t.Errorf("legacy +build ignore file contributed patterns %v (err=%v); want none", patterns, err)
	}
	patterns, err := goEmbedPatterns("win.go",
		[]byte("//go:build windows\n\npackage parent\n\nimport _ \"embed\"\n\n//go:embed win.json\nvar w []byte\n"))
	if err != nil || len(patterns) != 1 {
		t.Errorf("platform-constrained file must keep its patterns, got %v (err=%v)", patterns, err)
	}

	// End to end: an ignore-tagged generator with a missing input packages
	// fine, and the zip verifier accepts the shipped file too.
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()
	mustWrite(t, filepath.Join(projectRoot, "go.mod"), "module example.com/parent\n\ngo 1.24\n")
	mustWrite(t, filepath.Join(projectRoot, "parent.go"), "package parent\n")
	mustWrite(t, filepath.Join(projectRoot, "gen.go"),
		"//go:build ignore\n\npackage main\n\nimport _ \"embed\"\n\n//go:embed gen-input.json\nvar in []byte\n\nfunc main() {}\n")

	ctx := &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project:       pctx.Project{Name: "parent", Path: "parent", FullPath: projectRoot},
	}
	if ok := prepareGoModule(ctx, jsonl.New(), "1.2.3", outputRoot, false); !ok {
		t.Fatal("prepareGoModule failed on an ignore-tagged generator script with a missing embed input")
	}
}

func TestSplitEmbedPatterns(t *testing.T) {
	got, err := splitEmbedPatterns(` conformance/manifest.json "with space.json" all:static schemas/*.json`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"conformance/manifest.json", "with space.json", "all:static", "schemas/*.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitEmbedPatterns = %v, want %v", got, want)
	}
}

// findRepoRoot walks up from the test's working directory to the workspace
// root (identified by go.work + putnami.workspace.json), skipping the test
// when it runs outside the repository (e.g. from a published module).
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("workspace root not found; skipping repo-integration test")
		}
		dir = parent
	}
}

func zipEntryNames(t *testing.T, zipPath string) map[string]bool {
	t.Helper()
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open module zip %s: %v", zipPath, err)
	}
	defer zr.Close()
	names := make(map[string]bool, len(zr.File))
	for _, f := range zr.File {
		names[f.Name] = true
	}
	return names
}

func writeTestZip(t *testing.T, name string, entries map[string]string) string {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), name)
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(f)
	for path, body := range entries {
		w, err := zw.Create(path)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", path, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry %s: %v", path, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
	return zipPath
}
