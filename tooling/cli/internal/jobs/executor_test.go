package jobs

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/remotecache"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// declareCacheTestTask gives a cache-key fixture the same task-owned contract
// a planned cacheable job has. Key tests must not accidentally exercise a v2
// job, because the scheduler deliberately never looks up entries for one.
func declareCacheTestTask(job *ScheduledJob, task string) *ScheduledJob {
	if job.Extension.Tasks == nil {
		job.Extension.Tasks = make(map[string]extension.TaskDefinition)
	}
	job.Extension.Tasks[task] = extension.TaskDefinition{Declares: &extension.TaskDeclaration{}}
	job.Step = &extension.PipelineStep{Task: task}
	return job
}

func TestSchedulerLocalTaskCacheHitRestoresResultEvents(t *testing.T) {
	job := declaredJob("test~run", "run", &extension.TaskDeclaration{})
	job.JobDef.FilePatterns = []string{"src.txt"}
	f := newCaptureFixture(t, job)
	writeFileAt(t, filepath.Join(f.ws.Root, captureTestProject, "src.txt"), "stable\n")

	hash, err := cachekey.ComputeJobCacheHash(f.ws, job, nil, nil, f.cache, nil)
	if err != nil {
		t.Fatalf("compute task cache key: %v", err)
	}
	if !f.sched.storeDeclaredCapture(job, &JobResult{
		Status: "success",
		Events: []RawJobEvent{
			{Version: 1, Type: EventTypeMetric, Data: map[string]any{"name": "tests-total", "value": float64(51), "unit": "count"}},
			{Version: 1, Type: EventTypeMetric, Data: map[string]any{"name": "tests-passed", "value": float64(51), "unit": "count"}},
			{Version: 1, Type: EventTypeSummary, Data: map[string]any{"message": "51/51 passed"}},
		},
	}, hash) {
		t.Fatal("store declared task entry")
	}

	result := f.sched.Run(context.Background())
	got := result.Results[job.Key()]
	if got == nil || !got.CacheHit {
		t.Fatalf("result = %+v, want task-owned cache hit", got)
	}
	if len(got.Events) != 3 {
		t.Fatalf("restored events = %d, want 3: %+v", len(got.Events), got.Events)
	}
	if got.Events[0].Type != EventTypeMetric || got.Events[0].Data["name"] != "tests-total" {
		t.Errorf("first restored event = %+v, want tests-total metric", got.Events[0])
	}
}

func TestSchedulerDeclaredGenerateIgnoresFilesLessLegacyEntryAndRunsJob(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	ws := makeExecutorTestWorkspace(t)
	projDir := filepath.Join(ws.Root, "proj")
	if err := os.WriteFile(filepath.Join(projDir, "src.txt"), []byte("stable"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Path: t.TempDir(),
			Tasks: map[string]extension.TaskDefinition{
				"generate": {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"gen": dirDeclaration(extension.OutputRootProject, ".gen", false),
				}}},
			},
		},
		JobDef: &extension.JobDefinition{
			Name:          "build-generate",
			ExtensionName: "@test/ext",
			Command:       "/bin/sh",
			Args:          []string{"-c", "mkdir -p .gen && printf rebuilt > .gen/generate-result.json"},
			TimeoutMs:     unboundedJobTimeoutMs,
			Cache:         true,
			FilePatterns:  []string{"src.txt"},
			Writes:        []extension.ResourceRef{{ID: "gen"}},
		},
		Step: &extension.PipelineStep{Task: "generate"},
	}

	hash, err := cachekey.ComputeJobCacheHash(ws, job, nil, nil, cm, nil)
	if err != nil {
		t.Fatalf("compute task cache key: %v", err)
	}
	meta := &store.EntryMetadata{Extension: "@test/ext", Task: "build-generate", Project: "proj"}
	if err := cm.Save(hash, &store.EntryResult{Status: "success"}, meta, ""); err != nil {
		t.Fatalf("Save stale generate entry: %v", err)
	}

	renderer := &mockRenderer{}
	scheduler := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, renderer, cm)
	result := scheduler.Run(context.Background())

	got := result.Results[job.Key()]
	if got == nil {
		t.Fatal("missing generate result")
	}
	if got.CacheHit {
		t.Fatalf("legacy files-less entry was accepted as a task-owned cache hit: %+v", got)
	}
	if got.Status != "success" {
		t.Fatalf("generate result = %+v, want success", got)
	}
	if b, err := os.ReadFile(filepath.Join(projDir, ".gen", "generate-result.json")); err != nil {
		t.Fatalf("generate did not run after files-less cache hit: %v", err)
	} else if string(b) != "rebuilt" {
		t.Fatalf("generate-result.json = %q, want rebuilt", b)
	}
	if entry, err := cm.LookupTaskEntry(hash); err != nil || entry == nil {
		t.Fatalf("successful declared run did not replace the ignored legacy entry: entry=%v err=%v", entry, err)
	}
}

// A task-owned generate entry records its project-rooted .gen output. A warm
// scheduler run must restore that declared subtree, not infer it from the
// shared command-output directory.
func TestSchedulerGenerateTaskCacheHitRematerializesGenTree(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	projDir := filepath.Join(ws.Root, "proj")
	writeFileAt(t, filepath.Join(projDir, "src.txt"), "stable\n")

	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Tasks: map[string]extension.TaskDefinition{
				"generate": {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"gen": dirDeclaration(extension.OutputRootProject, ".gen", false),
				}}},
			},
		},
		JobDef: &extension.JobDefinition{
			Name: "build-generate", Cache: true, FilePatterns: []string{"src.txt"},
			Writes: []extension.ResourceRef{{ID: cachekey.GenResourceID}},
		},
		Step: &extension.PipelineStep{Task: "generate"},
	}

	genDir := filepath.Join(projDir, ".gen")
	writeFileAt(t, filepath.Join(genDir, "public", "docs", "index.html"), "<html>docs</html>")
	writeFileAt(t, filepath.Join(genDir, "generate-result.json"), `{"hash":"abc"}`)

	prime := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache)
	hash, err := cachekey.ComputeJobCacheHash(ws, job, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("compute task cache key: %v", err)
	}
	if !prime.storeDeclaredCapture(job, capturedResult(nil), hash) {
		t.Fatal("store declared generate entry")
	}
	if err := os.RemoveAll(genDir); err != nil {
		t.Fatal(err)
	}

	result := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache).Run(context.Background())
	got := result.Results[job.Key()]
	if got == nil || !got.CacheHit {
		t.Fatalf("result = %+v, want task-owned cache hit", got)
	}
	if got := readFileAt(t, filepath.Join(genDir, "public", "docs", "index.html")); got != "<html>docs</html>" {
		t.Errorf("restored doc = %q", got)
	}
	if _, err := os.Stat(filepath.Join(genDir, "generate-result.json")); err != nil {
		t.Errorf("generate-result.json not restored: %v", err)
	}
}

// A port-resolved declaration covers generated clients whose destination is a
// task result rather than a fixed convention path.
func TestSchedulerClientTaskCacheHitRematerializesConfiguredOutput(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	projDir := filepath.Join(ws.Root, "proj")
	writeFileAt(t, filepath.Join(projDir, "src.txt"), "stable\n")

	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Tasks: map[string]extension.TaskDefinition{
				"describe": {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"client": {Kind: extension.OutputKindDirectory, PathFrom: "clientOutput"},
				}}},
			},
		},
		JobDef: &extension.JobDefinition{
			Name: "build-describe", Cache: true, FilePatterns: []string{"src.txt"},
			Writes: []extension.ResourceRef{{ID: remotecache.ClientsResourceID}},
		},
		Step: &extension.PipelineStep{Task: "describe"},
	}

	clientDir := filepath.Join(projDir, "internal", "api-client")
	writeFileAt(t, filepath.Join(clientDir, "client.gen.go"), "package client\n")
	writeFileAt(t, filepath.Join(clientDir, "go.mod"), "module x\n")
	resultData := map[string]any{"clientOutput": "internal/api-client"}

	prime := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache)
	hash, err := cachekey.ComputeJobCacheHash(ws, job, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("compute task cache key: %v", err)
	}
	if !prime.storeDeclaredCapture(job, capturedResult(resultData), hash) {
		t.Fatal("store declared client entry")
	}
	if err := os.RemoveAll(clientDir); err != nil {
		t.Fatal(err)
	}

	result := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache).Run(context.Background())
	got := result.Results[job.Key()]
	if got == nil || !got.CacheHit {
		t.Fatalf("result = %+v, want task-owned cache hit", got)
	}
	if got := readFileAt(t, filepath.Join(clientDir, "client.gen.go")); got != "package client\n" {
		t.Errorf("restored client = %q", got)
	}
}

// One task may own outputs rooted both at the project and at a port-resolved
// client path. A warm hit must restore both independently.
func TestSchedulerGenerateTaskCacheHitRematerializesGenAndClients(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	projDir := filepath.Join(ws.Root, "proj")
	writeFileAt(t, filepath.Join(projDir, "src.txt"), "stable\n")

	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Tasks: map[string]extension.TaskDefinition{
				"generate": {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"gen":    dirDeclaration(extension.OutputRootProject, ".gen", false),
					"client": {Kind: extension.OutputKindDirectory, PathFrom: "clientOutput"},
				}}},
			},
		},
		JobDef: &extension.JobDefinition{
			Name: "build-generate", Cache: true, FilePatterns: []string{"src.txt"},
			Writes: []extension.ResourceRef{{ID: cachekey.GenResourceID}, {ID: remotecache.ClientsResourceID}},
		},
		Step: &extension.PipelineStep{Task: "generate"},
	}

	genDir := filepath.Join(projDir, ".gen")
	clientDir := filepath.Join(projDir, "clients", "ts")
	writeFileAt(t, filepath.Join(genDir, "clientgen", "config.json"), `{"targets":["ts"]}`)
	writeFileAt(t, filepath.Join(clientDir, "src", "index.ts"), "export class WidgetsClient {}\n")
	resultData := map[string]any{"clientOutput": "clients/ts"}

	prime := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache)
	hash, err := cachekey.ComputeJobCacheHash(ws, job, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("compute task cache key: %v", err)
	}
	if !prime.storeDeclaredCapture(job, capturedResult(resultData), hash) {
		t.Fatal("store declared multi-output entry")
	}
	if err := os.RemoveAll(genDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(projDir, "clients")); err != nil {
		t.Fatal(err)
	}

	result := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache).Run(context.Background())
	got := result.Results[job.Key()]
	if got == nil || !got.CacheHit {
		t.Fatalf("result = %+v, want task-owned cache hit", got)
	}
	if _, err := os.Stat(filepath.Join(genDir, "clientgen", "config.json")); err != nil {
		t.Errorf(".gen not restored: %v", err)
	}
	if got := readFileAt(t, filepath.Join(clientDir, "src", "index.ts")); got != "export class WidgetsClient {}\n" {
		t.Errorf("restored client = %q", got)
	}
}

// TestPrecomputeKeys_GenWriterIsCommitFreeWhileVersionEmbeddersInvalidateDependents
// replaces the inverse assertion this test used to make.
//
// Generation output is derived from declared content, so its key — and therefore
// every downstream key that mixes it in — must NOT move with the release suffix;
// keying it on the commit is what turned a metadata-only commit into a cold
// compile/test/describe closure. The half that survives is the one that was
// always the point: a task whose OUTPUT embeds the version (here a
// cache.versionAware packaging task) still re-keys on a new suffix, and still
// drags its dependents with it through the upstream hash.
//
// The store leg is the same assertion at the entry level: an entry published for
// the old suffix is RESOLVED by the new suffix's generation key (reuse), and is
// NOT resolved by the new suffix's packaging key (correct invalidation).
func TestPrecomputeKeys_GenWriterIsCommitFreeWhileVersionEmbeddersInvalidateDependents(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"}
	ext := &extension.ExtensionDescription{
		Name: "@test/ext",
		Path: t.TempDir(),
		Tasks: map[string]extension.TaskDefinition{
			"generate": {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
				"gen": dirDeclaration(extension.OutputRootProject, ".gen", false),
			}}},
			"compile": {Declares: &extension.TaskDeclaration{}},
			"archive": {Declares: &extension.TaskDeclaration{}},
			"attest":  {Declares: &extension.TaskDeclaration{}},
		},
	}

	generate := &ScheduledJob{
		Project: project, Extension: ext,
		JobDef: &extension.JobDefinition{
			Name: "build~generate", ExtensionName: "@test/ext", Cache: true,
			Writes: []extension.ResourceRef{{ID: cachekey.GenResourceID}},
		},
		Step: &extension.PipelineStep{Task: "generate"},
	}
	compile := &ScheduledJob{
		Project: project, Extension: ext,
		JobDef: &extension.JobDefinition{
			Name: "build~compile", ExtensionName: "@test/ext", Cache: true,
		},
		Step:      &extension.PipelineStep{Task: "compile"},
		DependsOn: []string{generate.Key()},
	}
	// A version-stamped archive: its bytes carry the release id, so it is the
	// task that MUST stay commit-aware.
	archive := &ScheduledJob{
		Project: project, Extension: ext,
		JobDef: &extension.JobDefinition{
			Name: "build~archive", ExtensionName: "@test/ext", Cache: true,
			TaskCachePolicy: &extension.TaskCachePolicy{VersionAware: true},
		},
		Step:      &extension.PipelineStep{Task: "archive"},
		DependsOn: []string{generate.Key()},
	}
	attest := &ScheduledJob{
		Project: project, Extension: ext,
		JobDef: &extension.JobDefinition{
			Name: "build~attest", ExtensionName: "@test/ext", Cache: true,
		},
		Step:      &extension.PipelineStep{Task: "attest"},
		DependsOn: []string{archive.Key()},
	}
	planned := []*ScheduledJob{generate, compile, archive, attest}

	oldVersion := rootLineVersions(&cachekey.JobContextVersion{Base: "1.0.0", Full: "1.0.0-5e748999", Suffix: "5e748999", SHA: "5e748999aa"})
	newVersion := rootLineVersions(&cachekey.JobContextVersion{Base: "1.0.0", Full: "1.0.0-dec4e2ec", Suffix: "dec4e2ec", SHA: "dec4e2ecbb"})
	oldKeys, err := cachekey.PrecomputeKeys(ws, planned, nil, oldVersion, cache, cachekey.CacheBypass{})
	if err != nil {
		t.Fatalf("precompute old version: %v", err)
	}
	newKeys, err := cachekey.PrecomputeKeys(ws, planned, nil, newVersion, cache, cachekey.CacheBypass{})
	if err != nil {
		t.Fatalf("precompute new version: %v", err)
	}

	if oldKeys[generate.Key()] != newKeys[generate.Key()] {
		t.Fatalf("generate key moved with the release suffix (%s → %s); a metadata-only commit re-runs every generator",
			oldKeys[generate.Key()], newKeys[generate.Key()])
	}
	if oldKeys[compile.Key()] != newKeys[compile.Key()] {
		t.Fatalf("compile key moved with the release suffix (%s → %s); the generate key fanned out downstream",
			oldKeys[compile.Key()], newKeys[compile.Key()])
	}
	if oldKeys[archive.Key()] == newKeys[archive.Key()] {
		t.Fatal("a cache.versionAware task did not vary with the release suffix; it would ship the prior commit's stamped bytes")
	}
	if oldKeys[attest.Key()] == newKeys[attest.Key()] {
		t.Fatal("the dependent of a version-aware task did not inherit its key change")
	}

	genDir := filepath.Join(ws.Root, "proj", ".gen")
	writeFileAt(t, filepath.Join(genDir, "version.json"), `{"version":"1.0.0-5e748999"}`)
	writeFileAt(t, filepath.Join(genDir, "openapi.json"), `{"openapi":"3.1.0"}`)
	producer := newScheduler(ws, planned, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache)
	if !producer.storeDeclaredCapture(generate, capturedResult(nil), oldKeys[generate.Key()]) {
		t.Fatal("store old declared generate entry")
	}
	if !producer.storeDeclaredCapture(archive, capturedResult(nil), oldKeys[archive.Key()]) {
		t.Fatal("store old declared archive entry")
	}

	if entry, err := cache.LookupTaskEntry(newKeys[generate.Key()]); err != nil || entry == nil {
		t.Fatalf("the new release suffix did not resolve the generation entry it should reuse: entry=%v err=%v", entry, err)
	}
	if entry, err := cache.LookupTaskEntry(newKeys[archive.Key()]); err != nil {
		t.Fatalf("lookup new archive key: %v", err)
	} else if entry != nil {
		t.Fatal("the new release suffix resolved the PRIOR commit's version-stamped archive entry")
	}
}

// makeExecutorTestWorkspace creates a temp workspace with required .putnami subdirectories
// and a project directory for test jobs.
func makeExecutorTestWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".putnami", "store"), 0o755)
	os.MkdirAll(filepath.Join(root, ".putnami", "out"), 0o755)
	os.MkdirAll(filepath.Join(root, "test-proj"), 0o755)
	os.MkdirAll(filepath.Join(root, "proj"), 0o755)
	return &workspace.Workspace{Root: root}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
