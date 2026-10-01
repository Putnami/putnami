package jobs

import (
	"path/filepath"
	"testing"

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
