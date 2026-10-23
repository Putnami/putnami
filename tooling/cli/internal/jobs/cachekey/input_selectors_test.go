package cachekey

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// stubResolver answers each input query with what answer returns for it, and
// records every query it answers and every priming call.
type stubResolver struct {
	answer func(query store.InputQuery) (store.InputExpansion, error)

	mu     sync.Mutex
	asked  []store.InputQuery
	primed [][]store.InputQuery
}

func (r *stubResolver) ResolveInput(query store.InputQuery) (store.InputExpansion, error) {
	r.mu.Lock()
	r.asked = append(r.asked, query)
	r.mu.Unlock()
	return r.answer(query)
}

func (r *stubResolver) PrimeInputs(queries []store.InputQuery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.primed = append(r.primed, slices.Clone(queries))
}

// listingResolver answers a query in a project directory whose base name
// lists files with those files, and any other query with no file.
func listingResolver(byProject map[string][]string) *stubResolver {
	return &stubResolver{answer: func(query store.InputQuery) (store.InputExpansion, error) {
		return store.InputExpansion{Files: byProject[filepath.Base(query.Dir)]}, nil
	}}
}

// selectorExtension is the @test/selectors extension: its runtime lists
// sel:build, and its compile task names the selector in a project input
// beside src/**.
func selectorExtension() *extension.ExtensionDescription {
	return &extension.ExtensionDescription{
		Name: "@test/selectors",
		Tasks: map[string]extension.TaskDefinition{
			"compile": {Inputs: map[string]extension.TaskInputPort{
				"sources": {From: extension.TaskInputFromProject, Files: []string{"src/**", "sel:build", "!src/gen/**"}},
			}},
		},
		Runtime: &extension.RuntimeDefinition{InputSelectors: []string{"sel:build"}},
	}
}

// selectorJob is the compile job of @test/selectors for project.
func selectorJob(ext *extension.ExtensionDescription, project *workspace.Project) *jobmodel.ScheduledJob {
	derived := extension.DeriveTaskCacheKey(ext.Tasks["compile"].Inputs)
	return &jobmodel.ScheduledJob{
		Project:   project,
		Extension: ext,
		Step:      &extension.PipelineStep{Task: "compile"},
		JobDef: &extension.JobDefinition{
			Name: "build~compile", CommandName: "build", ExtensionName: ext.Name, Cache: true,
			TaskCachePolicy: &extension.TaskCachePolicy{Key: &derived},
		},
	}
}

// selectorFixture is a library and an app that depends on it, each with one
// compile job of @test/selectors; the app's job folds the library's key.
type selectorFixture struct {
	ws       *workspace.Workspace
	lib, app *jobmodel.ScheduledJob
	libDir   string
}

func newSelectorFixture(t *testing.T) *selectorFixture {
	t.Helper()
	root := t.TempDir()
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{}, []*workspace.Project{lib, app})
	ws.Graph = workspace.BuildGraph(ws.Projects)
	writeProjectFile(t, root, "lib", "src/lib.txt", "lib")
	writeProjectFile(t, root, "lib", "assets/migration.sql", "SELECT 1;\n")
	writeProjectFile(t, root, "lib", "assets/other.sql", "SELECT 0;\n")
	writeProjectFile(t, root, "app", "src/app.txt", "app")
	ext := selectorExtension()
	fixture := &selectorFixture{ws: ws, lib: selectorJob(ext, lib), app: selectorJob(ext, app), libDir: filepath.Join(root, "lib")}
	fixture.app.DependsOn = []string{fixture.lib.Key()}
	return fixture
}

// keys computes the library's key and the app's key with resolver, in a fresh
// CacheManager so no memoized digest predates the edit under test.
func (f *selectorFixture) keys(t *testing.T, resolver store.InputResolver) (libKey, appKey string) {
	t.Helper()
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(f.ws.Root, ".putnami", "store")))
	cache.SetInputResolver(resolver)
	libKey, err := ComputeJobCacheHash(f.ws, f.lib, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("compute library key: %v", err)
	}
	appKey, err = ComputeJobCacheHash(f.ws, f.app, nil, nil, cache, map[string]string{f.lib.Key(): libKey})
	if err != nil {
		t.Fatalf("compute app key: %v", err)
	}
	return libKey, appKey
}

// A file the selector's answer lists keys by its exact bytes even when no
// other input changes, and its edit reaches every dependent through the
// upstream fold. A file the answer does not list keys nothing.
func TestListedFileEditMovesTheKeyAndItsDependents(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs", "a-listed-file-edit-moves-the-key-and-its-dependents")
	fixture := newSelectorFixture(t)
	resolver := listingResolver(map[string][]string{"lib": {"assets/migration.sql"}})
	baseLib, baseApp := fixture.keys(t, resolver)
	want := store.InputQuery{Extension: "@test/selectors", Dir: fixture.libDir, Selector: "sel:build", Patterns: []string{"src/**", "!src/gen/**"}}
	if !slices.ContainsFunc(resolver.asked, func(q store.InputQuery) bool { return inputQueryEqual(q, want) }) {
		t.Fatalf("the key asked %+v, want %+v", resolver.asked, want)
	}
	writeProjectFile(t, fixture.ws.Root, "lib", "assets/other.sql", "SELECT 2;\n")
	if lib, app := fixture.keys(t, resolver); lib != baseLib || app != baseApp {
		t.Fatal("an unlisted file moved a key")
	}
	writeProjectFile(t, fixture.ws.Root, "lib", "assets/migration.sql", "SELECT 2;\n")
	lib, app := fixture.keys(t, resolver)
	if lib == baseLib || app == baseApp {
		t.Fatalf("a listed file edit did not move the library and dependent keys: %s/%s -> %s/%s", baseLib, baseApp, lib, app)
	}
}

// An entry stored at the key of the listed file's old bytes never answers the
// key of its new bytes, so a stale output cannot be restored.
func TestAnOldEntryCannotRestoreAfterAListedFileEdit(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs", "an-old-entry-cannot-restore-after-a-listed-file-edit")
	fixture := newSelectorFixture(t)
	resolver := listingResolver(map[string][]string{"lib": {"assets/migration.sql"}})
	keyA, _ := fixture.keys(t, resolver)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(fixture.ws.Root, "cache")))
	output := filepath.Join(fixture.ws.Root, "output")
	writeTestFile(t, filepath.Join(output, "bundle.sql"), "SELECT 1;\n")
	meta := &store.EntryMetadata{Extension: "@test/selectors", Task: "compile", Project: "lib"}
	if err := cache.Save(keyA, &store.EntryResult{Status: "success"}, meta, output); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, fixture.ws.Root, "lib", "assets/migration.sql", "SELECT 2;\n")
	keyB, _ := fixture.keys(t, resolver)
	if keyA == keyB {
		t.Fatal("a listed file edit kept the key")
	}
	if old, err := cache.Lookup(keyB); err != nil || old != nil {
		t.Fatalf("the old entry answered the new key: %v, %v", old, err)
	}
}

// Portable inputs bind a listed file and mark it listed, after one priming
// call; a listed file that is gone fails the projection instead of dropping
// out of it.
func TestPortableInputsBindAListedFileAndFailOnItsDeletion(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs", "portable-inputs-bind-a-listed-file-and-fail-on-its-deletion")
	fixture := newSelectorFixture(t)
	resolver := listingResolver(map[string][]string{"lib": {"assets/migration.sql"}})
	projection, err := PortableInputs(fixture.ws, []*jobmodel.ScheduledJob{fixture.lib}, nil, resolver)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(projection.Tasks[0].Files, "lib/assets/migration.sql") || !slices.Contains(projection.Tasks[0].SelectedFiles, "lib/assets/migration.sql") {
		t.Fatalf("listed file absent from the portable binding: %+v", projection.Tasks[0])
	}
	if slices.Contains(projection.Tasks[0].SelectedFiles, "lib/src/lib.txt") {
		t.Fatalf("a pattern match was marked listed: %v", projection.Tasks[0].SelectedFiles)
	}
	if len(resolver.primed) != 1 || len(resolver.primed[0]) != 1 {
		t.Fatalf("priming calls = %+v, want one with the library's query", resolver.primed)
	}
	if err := os.Remove(filepath.Join(fixture.libDir, "assets", "migration.sql")); err != nil {
		t.Fatal(err)
	}
	if _, err := PortableInputs(fixture.ws, []*jobmodel.ScheduledJob{fixture.lib}, nil, resolver); !errors.Is(err, store.ErrInputSelector) {
		t.Fatalf("a deleted listed file left the projection: %v", err)
	}
}

// A failed or missing answer, and a selector a filePatterns option or flag
// names, fail the key and the unkeyed validation with ErrInputSelector.
func TestAFailedAnswerFailsTheKey(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "go-embed-cache-inputs", "a-failed-answer-fails-the-key")
	fixture := newSelectorFixture(t)
	failing := &stubResolver{answer: func(store.InputQuery) (store.InputExpansion, error) {
		return store.InputExpansion{}, errors.New("embed pattern ../x: invalid")
	}}
	compute := func(resolver store.InputResolver, params map[string]any) error {
		cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(fixture.ws.Root, ".putnami", "store")))
		cache.SetInputResolver(resolver)
		_, err := ComputeJobCacheHash(fixture.ws, fixture.lib, params, nil, cache, nil)
		return err
	}
	if err := compute(failing, nil); !errors.Is(err, store.ErrInputSelector) || !strings.Contains(err.Error(), "embed pattern ../x") {
		t.Errorf("a failed answer = %v, want ErrInputSelector with its text", err)
	}
	if err := compute(nil, nil); !errors.Is(err, store.ErrInputSelector) {
		t.Errorf("a key without a resolver = %v, want ErrInputSelector", err)
	}
	listing := listingResolver(nil)
	flag := map[string]any{"filePatterns": []any{"sel:build"}}
	if err := compute(listing, flag); !errors.Is(err, store.ErrInputSelector) || !strings.Contains(err.Error(), "requires an extension task input") {
		t.Errorf("a selector in --filePatterns = %v, want a refusal", err)
	}
	if err := ValidateUnkeyedInputSelectors(fixture.ws, fixture.lib, nil, failing); !errors.Is(err, store.ErrInputSelector) {
		t.Errorf("unkeyed validation of a failed answer = %v, want ErrInputSelector", err)
	}
	if err := ValidateUnkeyedInputSelectors(fixture.ws, fixture.lib, nil, listing); err != nil {
		t.Errorf("unkeyed validation of an answer = %v, want nil", err)
	}
	fixture.lib.Project.Config = &wsproto.ProjectConfig{Options: map[string]map[string]any{"build": {"filePatterns": []any{"sel:build"}}}}
	if err := compute(listing, nil); !errors.Is(err, store.ErrInputSelector) || !strings.Contains(err.Error(), "requires an extension task input") {
		t.Errorf("a selector in project filePatterns = %v, want a refusal", err)
	}
}

// A selector two inputs of one task name expands to the union of both
// answers, each asked with its own input's patterns; the expander's identity
// names its project and its queries.
func TestJobInputExpanderUnitesTheAnswersOfEveryInput(t *testing.T) {
	fixture := newSelectorFixture(t)
	ext := selectorExtension()
	ext.Tasks["compile"] = extension.TaskDefinition{Inputs: map[string]extension.TaskInputPort{
		"a": {From: extension.TaskInputFromProject, Files: []string{"sel:build", "src/**"}},
		"b": {From: extension.TaskInputFromProject, Files: []string{"assets/**", "sel:build"}},
	}}
	job := selectorJob(ext, fixture.lib.Project)
	resolver := &stubResolver{answer: func(query store.InputQuery) (store.InputExpansion, error) {
		if slices.Contains(query.Patterns, "src/**") {
			return store.InputExpansion{Files: []string{"b.txt", "a.txt"}, Basis: wsproto.InputBasis{Content: [][]string{{"src/**"}}}}, nil
		}
		return store.InputExpansion{Files: []string{"a.txt"}, Basis: wsproto.InputBasis{Listing: [][]string{{"assets/**"}}}}, nil
	}}
	expander := JobInputExpander(fixture.ws, job, nil, resolver)
	got, err := expander.ExpandInput(fixture.libDir, "sel:build")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Files, []string{"a.txt", "b.txt"}) || len(got.Basis.Content) != 1 || len(got.Basis.Listing) != 1 {
		t.Fatalf("expansion = %+v, want the union of both answers", got)
	}
	if _, err := expander.ExpandInput(filepath.Join(fixture.ws.Root, "app"), "sel:build"); !errors.Is(err, store.ErrInputSelector) {
		t.Errorf("an expansion outside the job's project = %v, want ErrInputSelector", err)
	}
	if _, err := expander.ExpandInput(fixture.libDir, "sel:other"); !errors.Is(err, store.ErrInputSelector) {
		t.Errorf("a selector no input names = %v, want ErrInputSelector", err)
	}
	if expander.Identity() == JobInputExpander(fixture.ws, fixture.lib, nil, resolver).Identity() {
		t.Error("two tasks with different selector inputs share an expander identity")
	}
	if expander.Identity() != JobInputExpander(fixture.ws, job, nil, listingResolver(nil)).Identity() {
		t.Error("the expander identity depends on the resolver instance")
	}
}

// A plan primes the selectors of its jobs and of the producers whose
// invocation-scoped resource they consume, in one call.
func TestPrimePlanInputsPrimesEveryJobAndProducer(t *testing.T) {
	fixture := newSelectorFixture(t)
	fixture.app.InvocationProducer = fixture.lib
	resolver := listingResolver(nil)
	cache := store.NewCacheManager(nil)
	cache.SetInputResolver(resolver)
	PrimePlanInputs(fixture.ws, []*jobmodel.ScheduledJob{fixture.app}, cache)
	if len(resolver.primed) != 1 || len(resolver.primed[0]) != 2 {
		t.Fatalf("priming calls = %+v, want one with the app's and its producer's queries", resolver.primed)
	}
	PrimePlanInputs(fixture.ws, nil, cache)
	PrimePlanInputs(fixture.ws, []*jobmodel.ScheduledJob{fixture.app}, store.NewCacheManager(nil))
	if len(resolver.primed) != 1 {
		t.Fatalf("an empty plan or a manager without a resolver primed: %+v", resolver.primed)
	}
}

func inputQueryEqual(a, b store.InputQuery) bool {
	return a.Extension == b.Extension && a.Dir == b.Dir && a.Selector == b.Selector && slices.Equal(a.Patterns, b.Patterns)
}
