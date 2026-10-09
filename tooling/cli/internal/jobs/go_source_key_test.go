package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// goSourceKeyFixture is a two-project Go workspace — one library and one app
// that depends on it — plus the jobs of one real @putnami/go task for each
// project, wired the way the manifest's `^<step>` edge wires them: the app's
// job folds the library's cache key.
type goSourceKeyFixture struct {
	ws     *workspace.Workspace
	lib    *ScheduledJob
	app    *ScheduledJob
	libDir string
	appDir string
}

// newGoSourceKeyFixture builds the fixture around the named manifest task, read
// from the committed go/extension manifest rather than a hand-written copy: the
// contract under test is what the manifest declares, and a fixture that
// restated the patterns would pin the restatement.
func newGoSourceKeyFixture(t *testing.T, taskName, jobName string, crossProjectEdge bool) *goSourceKeyFixture {
	t.Helper()
	repoRoot := findJobsRepoRoot(t)
	goExtension := extension.LoadExtensionFromDir(filepath.Join(repoRoot, "go", "extension"), "/go/extension")
	if goExtension == nil {
		t.Fatal("load real Go extension manifest")
	}
	// The test exercises manifest inputs, not mutable-runtime identity.
	goExtension.LocalSource = false
	goExtension.RelPath = "go/extension"
	task, ok := goExtension.Tasks[taskName]
	if !ok {
		t.Fatalf("real Go manifest has no %s task", taskName)
	}
	derived := extension.DeriveTaskCacheKey(task.Inputs)

	root := t.TempDir()
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{}, []*workspace.Project{lib, app})
	ws.Graph = workspace.BuildGraph(ws.Projects)

	fixture := &goSourceKeyFixture{
		ws:     ws,
		libDir: filepath.Join(root, "lib"),
		appDir: filepath.Join(root, "app"),
	}
	writeTestFile(t, filepath.Join(fixture.libDir, "go.mod"), "module example.com/lib\n\ngo 1.26\n")
	writeTestFile(t, filepath.Join(fixture.libDir, "lib.go"), "package lib\n\nfunc Answer() int { return 42 }\n")
	writeTestFile(t, filepath.Join(fixture.libDir, "lib_test.go"), "package lib\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {}\n")
	writeTestFile(t, filepath.Join(fixture.appDir, "go.mod"), "module example.com/app\n\ngo 1.26\n")
	writeTestFile(t, filepath.Join(fixture.appDir, "main.go"), "package main\n\nfunc main() {}\n")
	writeTestFile(t, filepath.Join(fixture.appDir, "main_test.go"), "package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {}\n")
	writeTestFile(t, filepath.Join(root, "go.work"), "go 1.26\n\nuse (\n\t./lib\n\t./app\n)\n")

	job := func(project *workspace.Project) *ScheduledJob {
		return &ScheduledJob{
			Project:   project,
			Extension: goExtension,
			Step:      &extension.PipelineStep{Task: taskName},
			JobDef: &extension.JobDefinition{
				Name: jobName, CommandName: "build", ExtensionName: goExtension.Name, Cache: true,
				TaskCachePolicy: &extension.TaskCachePolicy{Key: &derived},
			},
		}
	}
	fixture.lib = job(lib)
	fixture.app = job(app)
	if crossProjectEdge {
		fixture.app.DependsOn = []string{fixture.lib.Key()}
	}
	return fixture
}

// keys returns the library's key and the app's key, computed the way
// PrecomputeKeys computes them: the library first, its value folded into the
// app's. Each call builds a fresh CacheManager so the per-session file-hash memo
// cannot serve a digest from before the edit under test.
func (f *goSourceKeyFixture) keys(t *testing.T) (libKey, appKey string) {
	t.Helper()
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(f.ws.Root, ".putnami", "store")))
	libKey, err := computeJobCacheHash(f.ws, f.lib, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("compute dependency cache key: %v", err)
	}
	appKey, err = computeJobCacheHash(f.ws, f.app, nil, nil, cache, map[string]string{f.lib.Key(): libKey})
	if err != nil {
		t.Fatalf("compute dependent cache key: %v", err)
	}
	return libKey, appKey
}

// An embedded asset is Go source input even when no .go byte changes. This
// specifically prevents a cached describe from restoring an old migration
// bundle after its SQL changed, and pins the dependent's ^describe edge.
func TestGoDescribeKeyIncludesEmbeddedAssetOnlyEdit(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs", "go-describe-key-includes-embedded-asset-only-edit")
	fixture := newGoSourceKeyFixture(t, "build-describe", "build~describe", true)
	writeTestFile(t, filepath.Join(fixture.libDir, "embed.go"), "package lib\nimport _ \"embed\"\n//go:embed migration.sql\nvar SQL string\n")
	writeTestFile(t, filepath.Join(fixture.libDir, "migration.sql"), "SELECT 1;\n")
	baseLib, baseApp := fixture.keys(t)
	writeTestFile(t, filepath.Join(fixture.libDir, "migration.sql"), "SELECT 2;\n")
	changedLib, changedApp := fixture.keys(t)
	if changedLib == baseLib || changedApp == baseApp {
		t.Fatalf("embedded asset edit did not move describe and dependent keys: %s/%s -> %s/%s", baseLib, baseApp, changedLib, changedApp)
	}
	writeTestFile(t, filepath.Join(fixture.libDir, "lib_test.go"), "package lib\nfunc TestNoop() {}\n")
	stableLib, stableApp := fixture.keys(t)
	if stableLib != changedLib || stableApp != changedApp {
		t.Fatal("non-test describe key moved on test-only edit")
	}
}

func TestGoDescribeKeyIncludesBuildableSubpackageEmbedBehindBlockComment(t *testing.T) {
	fixture := newGoSourceKeyFixture(t, "build-describe", "build~describe", true)
	writeTestFile(t, filepath.Join(fixture.libDir, "out", "p", "embed.go"), "/*\n//go:build ignore\n*/\npackage p\nimport _ \"embed\"\n//go:embed payload.txt\nvar payload string\n")
	asset := filepath.Join(fixture.libDir, "out", "p", "payload.txt")
	writeTestFile(t, asset, "A")
	beforeLib, beforeApp := fixture.keys(t)
	writeTestFile(t, asset, "B")
	afterLib, afterApp := fixture.keys(t)
	if beforeLib == afterLib || beforeApp == afterApp {
		t.Fatalf("buildable out/p embed asset edit did not move library and dependent keys: %s/%s -> %s/%s", beforeLib, beforeApp, afterLib, afterApp)
	}
}

func TestGoDescribeKeyIncludesLexicalSourceInOutSubpackage(t *testing.T) {
	fixture := newGoSourceKeyFixture(t, "build-describe", "build~describe", true)
	source := filepath.Join(fixture.libDir, "out", "p", "source.go")
	writeTestFile(t, source, "package p\nconst Value = 1\n")
	beforeLib, beforeApp := fixture.keys(t)
	writeTestFile(t, source, "package p\nconst Value = 2\n")
	afterLib, afterApp := fixture.keys(t)
	if beforeLib == afterLib || beforeApp == afterApp {
		t.Fatalf("out/p lexical source edit did not move library/dependent keys: %s/%s -> %s/%s", beforeLib, beforeApp, afterLib, afterApp)
	}
}

func TestGoDescribeSourceSymlinkAssetEditMovesLibraryAndDependentKeys(t *testing.T) {
	fixture := newGoSourceKeyFixture(t, "build-describe", "build~describe", true)
	writeTestFile(t, filepath.Join(fixture.libDir, ".source.txt"), "package lib\nimport _ \"embed\"\n//go:embed migration.sql\nvar SQL string\n")
	if err := os.Symlink(".source.txt", filepath.Join(fixture.libDir, "embed.go")); err != nil {
		t.Skipf("source-file symlinks unavailable: %v", err)
	}
	asset := filepath.Join(fixture.libDir, "migration.sql")
	writeTestFile(t, asset, "SELECT 1;\n")
	initialLib, initialApp := fixture.keys(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(fixture.ws.Root, "source-link-cache")))
	output := filepath.Join(fixture.ws.Root, "described")
	writeTestFile(t, filepath.Join(output, "bundle.sql"), "SELECT 1;\n")
	meta := &store.EntryMetadata{Extension: "@putnami/go", Task: "build-describe", Project: "lib"}
	if err := cache.Save(initialLib, &store.EntryResult{Status: "success"}, meta, output); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, asset, "SELECT 2;\n")
	changedLib, changedApp := fixture.keys(t)
	if changedLib == initialLib || changedApp == initialApp {
		t.Fatalf("source-link asset edit left cache keys unchanged: lib %s/%s, app %s/%s", initialLib, changedLib, initialApp, changedApp)
	}
	if old, err := cache.Lookup(changedLib); err != nil || old != nil {
		t.Fatalf("old bundle answered new key: %v, %v", old, err)
	}
	projection, err := PortableInputs(fixture.ws, []*ScheduledJob{fixture.lib}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(projection.Tasks[0].Files, "lib/migration.sql") {
		t.Fatalf("source-link asset absent from portable inputs: %v", projection.Tasks[0].Files)
	}
	if !slices.Contains(projection.Tasks[0].Files, "lib/.source.txt") {
		t.Fatalf("source-link target absent from portable inputs: %v", projection.Tasks[0].Files)
	}
	if !slices.Contains(projection.Tasks[0].GoEmbedFiles, "lib/.source.txt") {
		t.Fatalf("source-link target lost semantic provenance: %v", projection.Tasks[0].GoEmbedFiles)
	}
}

func TestGoDescribeCachedOldBundleCannotRestoreAfterEmbeddedAssetEdit(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs", "go-describe-cached-old-bundle-cannot-restore-after-embedded-asset-edit")
	fixture := newGoSourceKeyFixture(t, "build-describe", "build~describe", false)
	writeTestFile(t, filepath.Join(fixture.appDir, "embed.go"), "package main\nimport _ \"embed\"\n//go:embed migration.sql\nvar SQL string\n")
	asset := filepath.Join(fixture.appDir, "migration.sql")
	writeTestFile(t, asset, "SELECT 'A';\n")
	// The app key is the one whose describe output owns the bundle.
	_, keyA := fixture.keys(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(fixture.ws.Root, "cache")))
	output := filepath.Join(fixture.ws.Root, "describe-output")
	writeTestFile(t, filepath.Join(output, "migration-bundle", "payload.sql"), "SELECT 'A';\n")
	meta := &store.EntryMetadata{Extension: "@putnami/go", Task: "build-describe", Project: "app"}
	if err := cache.Save(keyA, &store.EntryResult{Status: "success"}, meta, output); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, asset, "SELECT 'B';\n")
	_, keyB := fixture.keys(t)
	if keyA == keyB {
		t.Fatal("asset-only edit reused old describe identity")
	}
	if old, err := cache.Lookup(keyB); err != nil || old != nil {
		t.Fatalf("A entry answered B key: %v, %v", old, err)
	}
	writeTestFile(t, filepath.Join(output, "migration-bundle", "payload.sql"), "SELECT 'B';\n")
	if err := cache.Save(keyB, &store.EntryResult{Status: "success"}, meta, output); err != nil {
		t.Fatal(err)
	}
	entry, err := cache.Lookup(keyB)
	if err != nil || entry == nil {
		t.Fatalf("B bundle not stored: %v", err)
	}
	restored := filepath.Join(fixture.ws.Root, "restored")
	if _, err := cache.RestoreFiles(entry, restored); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(restored, "migration-bundle", "payload.sql"))
	if err != nil || string(got) != "SELECT 'B';\n" {
		t.Fatalf("final described bundle = %q, %v", got, err)
	}
}

func TestGoEmbedPortableInputsBindAssetAndFailOnDeletion(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs", "go-embed-portable-inputs-bind-asset-and-fail-on-deletion")
	fixture := newGoSourceKeyFixture(t, "build-describe", "build~describe", false)
	writeTestFile(t, filepath.Join(fixture.appDir, "embed.go"), "package main\nimport _ \"embed\"\n//go:embed local.sql\nvar SQL string\n")
	asset := filepath.Join(fixture.appDir, "local.sql")
	writeTestFile(t, asset, "SELECT 1;\n")
	projection, err := PortableInputs(fixture.ws, []*ScheduledJob{fixture.app}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range projection.Tasks[0].Files {
		if file == "app/local.sql" {
			found = true
		}
	}
	if !found {
		t.Fatalf("embedded asset missing from portable binding: %v", projection.Tasks[0].Files)
	}
	if !slices.Contains(projection.Tasks[0].GoEmbedFiles, "app/local.sql") {
		t.Fatalf("embedded asset lost semantic provenance: %v", projection.Tasks[0].GoEmbedFiles)
	}
	if err := os.Remove(asset); err != nil {
		t.Fatal(err)
	}
	if _, err := PortableInputs(fixture.ws, []*ScheduledJob{fixture.app}, nil); err == nil {
		t.Fatal("deleted embedded asset silently omitted from portable projection")
	}
}

func TestGoTestOnlyEmbedDoesNotMoveBuildKey(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs", "go-test-only-embed-does-not-move-build-key")
	build := newGoSourceKeyFixture(t, "build-compile", "build~compile", false)
	writeTestFile(t, filepath.Join(build.appDir, "payload_test.go"), "package main\nimport _ \"embed\"\n//go:embed fixture.txt\nvar fixture string\n")
	writeTestFile(t, filepath.Join(build.appDir, "fixture.txt"), "A")
	_, before := build.keys(t)
	writeTestFile(t, filepath.Join(build.appDir, "fixture.txt"), "B")
	_, after := build.keys(t)
	if before != after {
		t.Fatal("build key read a test-only embed")
	}

	test := newGoSourceKeyFixture(t, "test-exec", "test~test", false)
	writeTestFile(t, filepath.Join(test.appDir, "payload_test.go"), "package main\nimport _ \"embed\"\n//go:embed fixture.txt\nvar fixture string\n")
	writeTestFile(t, filepath.Join(test.appDir, "fixture.txt"), "A")
	_, testBefore := test.keys(t)
	writeTestFile(t, filepath.Join(test.appDir, "fixture.txt"), "B")
	_, testAfter := test.keys(t)
	if testBefore == testAfter {
		t.Fatal("test key omitted a test-only embed")
	}
}

// TestGoDescribeKeyIgnoresADependencyTestFile pins the cross-project half of the
// Go build graph's key contract.
//
// `^describe` is the edge every dependent reaches its dependencies through, and
// a dependent's compile and test sit behind its own describe, so whatever moves
// a library's describe key moves the identity of the entire dependent closure.
// build-describe compiles the project's main package and runs it; `go build`
// never reads a `_test.go` file, so a test-only edit must leave both keys where
// they are. A non-test edit must move both: under go.work the dependent
// compiles the dependency's sources, so its verdict is a function of them.
func TestGoDescribeKeyIgnoresADependencyTestFile(t *testing.T) {
	t.Parallel()
	fixture := newGoSourceKeyFixture(t, "build-describe", "build~describe", true)
	baseLib, baseApp := fixture.keys(t)

	writeTestFile(t, filepath.Join(fixture.libDir, "lib_test.go"),
		"package lib\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) { _ = Answer() }\n")
	libKey, appKey := fixture.keys(t)
	if libKey != baseLib {
		t.Error("a _test.go edit moved the library's own describe key; describe compiles no test file")
	}
	if appKey != baseApp {
		t.Error("a dependency's _test.go edit moved the dependent's describe key; " +
			"every dependent task behind ^describe would miss the cache for a file none of them reads")
	}

	writeTestFile(t, filepath.Join(fixture.libDir, "lib.go"), "package lib\n\nfunc Answer() int { return 43 }\n")
	libKey, appKey = fixture.keys(t)
	if libKey == baseLib {
		t.Error("a dependency source edit left the library's describe key unmoved")
	}
	if appKey == baseApp {
		t.Error("a dependency source edit left the dependent's describe key unmoved; " +
			"under go.work the dependent compiles that source, so a stored verdict would be served against different bytes")
	}
}

// TestGoCompileKeyIgnoresTestFiles pins the same contract on build-compile,
// which has no cross-project edge of its own but sits behind its project's
// describe: a test-only edit must leave a compile key where it is, in this
// project and in a dependency alike.
func TestGoCompileKeyIgnoresTestFiles(t *testing.T) {
	t.Parallel()
	fixture := newGoSourceKeyFixture(t, "build-compile", "build~compile", false)
	_, baseApp := fixture.keys(t)

	writeTestFile(t, filepath.Join(fixture.appDir, "main_test.go"),
		"package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) { main() }\n")
	if _, appKey := fixture.keys(t); appKey != baseApp {
		t.Error("a _test.go edit moved the project's compile key; go build compiles no test file")
	}

	writeTestFile(t, filepath.Join(fixture.appDir, "main.go"), "package main\n\nfunc main() { println(1) }\n")
	if _, appKey := fixture.keys(t); appKey == baseApp {
		t.Error("a source edit left the compile key unmoved")
	}
}

// TestGoTestKeyReadsTestFiles is the other half of the same contract: the
// exclusion belongs to the tasks that do not read test files, and never to the
// one that runs them. A test-exec key that ignored a `_test.go` edit would serve
// the previous verdict for a test the run never executed.
func TestGoTestKeyReadsTestFiles(t *testing.T) {
	t.Parallel()
	fixture := newGoSourceKeyFixture(t, "test-exec", "test~test", false)
	_, baseApp := fixture.keys(t)

	writeTestFile(t, filepath.Join(fixture.appDir, "main_test.go"),
		"package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) { t.Fatal(\"red\") }\n")
	if _, appKey := fixture.keys(t); appKey == baseApp {
		t.Error("a _test.go edit left the test key unmoved; the stored verdict predates the test that now runs")
	}
}

// TestGoBuildKeyIgnoresAnotherExtensionsOptions pins the project-config half of
// the same rule at the level the user meets it: a real @putnami/go task, a real
// project config, and an option block addressed to an extension that does not
// run it.
//
// `putnami.json` is a shared file — `options` holds one block per addressee — so
// hashing it whole made a deploy option of another extension re-run this
// project's build and test, and every dependent behind `^describe`.
func TestGoBuildKeyIgnoresAnotherExtensionsOptions(t *testing.T) {
	t.Parallel()
	fixture := newGoSourceKeyFixture(t, "build-describe", "build~describe", true)
	config := filepath.Join(fixture.appDir, "putnami.json")
	writeTestFile(t, config,
		`{"name":"app","options":{"@putnami/go":{"race":true},"@putnami/cloud":{"region":"eu"}}}`)
	_, base := fixture.keys(t)

	writeTestFile(t, config,
		`{"name":"app","options":{"@putnami/go":{"race":true},"@putnami/cloud":{"region":"us"}}}`)
	if _, key := fixture.keys(t); key != base {
		t.Error("another extension's option block moved a Go task's key; the task cannot read it")
	}

	writeTestFile(t, config,
		`{"name":"app","options":{"@putnami/go":{"race":false},"@putnami/cloud":{"region":"us"}}}`)
	if _, key := fixture.keys(t); key == base {
		t.Error("this extension's own option block left the key unmoved")
	}

	writeTestFile(t, config,
		`{"name":"app","tags":["service"],"options":{"@putnami/go":{"race":true},"@putnami/cloud":{"region":"us"}}}`)
	if _, key := fixture.keys(t); key == base {
		t.Error("a project identity field left the key unmoved")
	}
}

// TestGoBuildKeyIgnoresADependencyForeignOptions is the cross-project half: a
// dependency's config reaches a dependent through the upstream fold, so an
// option block neither project's Go tasks read must stop at the file it lives
// in.
func TestGoBuildKeyIgnoresADependencyForeignOptions(t *testing.T) {
	t.Parallel()
	fixture := newGoSourceKeyFixture(t, "build-describe", "build~describe", true)
	config := filepath.Join(fixture.libDir, "putnami.json")
	writeTestFile(t, config, `{"name":"lib","options":{"@putnami/cloud":{"region":"eu"}}}`)
	baseLib, baseApp := fixture.keys(t)

	writeTestFile(t, config, `{"name":"lib","options":{"@putnami/cloud":{"region":"us"}}}`)
	libKey, appKey := fixture.keys(t)
	if libKey != baseLib {
		t.Error("another extension's option block moved the dependency's own key")
	}
	if appKey != baseApp {
		t.Error("another extension's option block in a DEPENDENCY moved the dependent's key")
	}

	writeTestFile(t, config, `{"name":"lib","options":{"@putnami/go":{"race":true}}}`)
	if libKey, appKey = fixture.keys(t); libKey == baseLib || appKey == baseApp {
		t.Error("the dependency's own @putnami/go options left a key unmoved")
	}
}
