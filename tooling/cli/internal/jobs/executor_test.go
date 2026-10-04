package jobs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protocoljob "go.putnami.dev/protocol/job"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
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

func TestIsCacheEnabled(t *testing.T) {
	falseVal := false
	trueVal := true
	declared := &extension.TaskDeclaration{}

	tests := []struct {
		name     string
		job      *ScheduledJob
		noCache  bool
		expected bool
	}{
		{name: "cache enabled by default", job: declaredJob("build~test", "test", declared), expected: true},
		{name: "no-cache config disables", job: declaredJob("build~test", "test", declared), noCache: true, expected: false},
		{name: "missing declaration cannot be cached", job: declaredJob("build~test", "test", nil), expected: false},
		// A declared package step is cacheable like any other. The command
		// name decides nothing; the declaration does.
		{name: "declared package step can be cached", job: declaredJob("package~owned", "owned", declared), expected: true},
		{name: "task cache policy disabled", job: func() *ScheduledJob {
			job := declaredJob("build~test", "test", declared)
			job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Enabled: &falseVal}
			return job
		}(), expected: false},
		{name: "step cache override disabled", job: func() *ScheduledJob {
			job := declaredJob("build~test", "test", declared)
			job.Step.Cache = &extension.StepCacheOverride{Enabled: &falseVal}
			return job
		}(), expected: false},
		{name: "step cache override enabled", job: func() *ScheduledJob {
			job := declaredJob("build~test", "test", declared)
			job.Step.Cache = &extension.StepCacheOverride{Enabled: &trueVal}
			return job
		}(), expected: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCacheEnabled(tt.job, CacheBypass{All: tt.noCache}); got != tt.expected {
				t.Errorf("isCacheEnabled() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestTaskReadsVersionVar(t *testing.T) {
	mk := func(params []string, withKey bool) *ScheduledJob {
		jd := &extension.JobDefinition{}
		if withKey {
			jd.TaskCachePolicy = &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Params: params}}
		}
		return &ScheduledJob{JobDef: jd}
	}
	tests := []struct {
		name string
		job  *ScheduledJob
		want bool
	}{
		{"reads version-var", mk([]string{"target", "version-var"}, true), true},
		{"reads camelCase versionVar", mk([]string{"versionVar"}, true), true},
		{"no version param", mk([]string{"target", "ldflags"}, true), false},
		{"nil key", mk(nil, false), false},
		{"nil jobdef", &ScheduledJob{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := taskReadsVersionVar(tt.job); got != tt.want {
				t.Errorf("taskReadsVersionVar() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCacheKeyHashesOnlyDeclaredTaskParams(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Path: t.TempDir(),
		},
		JobDef: &extension.JobDefinition{
			Name:          "build~cross-compile",
			CommandName:   "build",
			ExtensionName: "@test/ext",
			Cache:         true,
			TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{
				Params: []string{"target"},
			}},
		},
	}
	declareCacheTestTask(job, "cross-compile")

	hashFor := func(params map[string]any) string {
		t.Helper()
		hash, err := computeJobCacheHash(ws, job, params, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return hash
	}

	plain := hashFor(map[string]any{"target": "linux/amd64"})
	fromPublish := hashFor(map[string]any{"target": "linux/amd64", "channel": "latest"})
	if plain != fromPublish {
		t.Fatal("undeclared parent-command parameter changed the task cache key")
	}
	if plain == hashFor(map[string]any{"target": "darwin/arm64", "channel": "latest"}) {
		t.Fatal("declared task parameter did not change the task cache key")
	}
}

func TestCacheKeyHashesLiteralStepBindings(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Path: t.TempDir(),
		},
		JobDef: &extension.JobDefinition{
			Name:          "test~generate",
			CommandName:   "test",
			ExtensionName: "@test/ext",
			Cache:         true,
			// Empty Params models a `from: task` or legacy input. The literal
			// binding itself still has to become cache-key material.
			TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{}},
			BoundParams:     map[string]any{"mode": "test", "nullable": nil, "foo-bar": "kebab", "fooBar": "camel"},
		},
	}
	declareCacheTestTask(job, "generate")

	resolved := taskCacheParams(ws, job, map[string]any{"mode": "cli"})
	if got := resolved["mode"]; got != "test" {
		t.Fatalf("cache params mode = %#v, want bound value test", got)
	}
	if got, present := resolved["nullable"]; !present || got != nil {
		t.Fatalf("cache params nullable = (%#v, %v), want explicit nil", got, present)
	}
	if got := resolved["fooBar"]; got != "camel" {
		t.Fatalf("cache params fooBar = %#v, want exact bound value camel", got)
	}

	hashFor := func(mode string) string {
		t.Helper()
		job.JobDef.BoundParams["mode"] = mode
		hash, err := computeJobCacheHash(ws, job, map[string]any{"mode": "cli"}, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return hash
	}
	testHash := hashFor("test")
	for i := 0; i < 128; i++ {
		if got := hashFor("test"); got != testHash {
			t.Fatalf("iteration %d: identical alias bindings produced cache hash %q, want %q", i, got, testHash)
		}
	}
	if testHash == hashFor("serve") {
		t.Fatal("different literal step bindings produced the same cache key")
	}
}

func TestCacheKeyHashesOwningCommandFlagsWhenTaskContractIsIncomplete(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Path: t.TempDir(),
			Jobs: map[string]*extension.JobDefinition{
				"test": {Flags: map[string]extension.FlagDefinition{"race": {Type: "boolean"}}},
			},
		},
		JobDef: &extension.JobDefinition{
			Name:          "test~test",
			CommandName:   "test",
			ExtensionName: "@test/ext",
			Cache:         true,
			// Models an older task contract that omitted a runtime-consumed flag.
			TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{
				Params: []string{"entrypoint"},
			}},
		},
	}
	declareCacheTestTask(job, "test-exec")

	hashFor := func(race bool, channel string) string {
		t.Helper()
		params := make(map[string]any)
		if race {
			params["race"] = true
		}
		if channel != "" {
			params["channel"] = channel
		}
		hash, err := computeJobCacheHash(ws, job, params, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return hash
	}

	plain := hashFor(false, "")
	if plain == hashFor(true, "") {
		t.Fatal("runtime-consumed owning-command flag did not change the task cache key")
	}
	if plain != hashFor(false, "latest") {
		t.Fatal("unrelated parent-command flag changed the task cache key")
	}
}

func TestCacheTaskNameUsesManifestTaskName(t *testing.T) {
	job := &ScheduledJob{JobDef: &extension.JobDefinition{Name: "lint-format"}}
	if got := cacheTaskName(job); got != "lint-format" {
		t.Fatalf("cache task name = %q, want lint-format", got)
	}
	sourceRewriter := declaredJob(
		"lint-format", "fix", &extension.TaskDeclaration{MutatesSources: true})
	if got := cacheTaskName(sourceRewriter); got != "lint-format@source-clean-v2" {
		t.Fatalf("source-rewriter cache task name = %q, want versioned clean-only identity", got)
	}
}

// The real Go test task must move its cache key when either workspace module
// file changes. This is the cache half of root-file impacted selection: merely
// scheduling the project is unsafe if an entry from the previous workspace
// graph can still be restored.
func TestGoWorkspaceModuleFilesMoveRealTaskCacheKey(t *testing.T) {
	repoRoot := findJobsRepoRoot(t)
	goExtension := extension.LoadExtensionFromDir(filepath.Join(repoRoot, "go", "extension"), "/go/extension")
	if goExtension == nil {
		t.Fatal("load real Go extension manifest")
	}
	// The test exercises manifest inputs, not mutable-runtime identity.
	goExtension.LocalSource = false
	task, ok := goExtension.Tasks["test-exec"]
	if !ok {
		t.Fatal("real Go manifest has no test-exec task")
	}
	derived := extension.DeriveTaskCacheKey(task.Inputs)

	fixture := makeExecutorTestWorkspace(t)
	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"}
	ws := workspace.NewWorkspace(fixture.Root, &wsproto.Config{}, []*workspace.Project{project})
	writeTestFile(t, filepath.Join(ws.Root, "proj", "go.mod"), "module example.com/proj\n\ngo 1.26\n")
	writeTestFile(t, filepath.Join(ws.Root, "proj", "main.go"), "package main\nfunc main() {}\n")
	workPath := filepath.Join(ws.Root, "go.work")
	sumPath := filepath.Join(ws.Root, "go.work.sum")
	writeTestFile(t, workPath, "go 1.26\n\nuse ./proj\n")
	writeTestFile(t, sumPath, "example.com/dependency v1.0.0 h1:first\n")

	job := &ScheduledJob{
		Project:   project,
		Extension: goExtension,
		Step:      &extension.PipelineStep{Task: "test-exec"},
		JobDef: &extension.JobDefinition{
			Name: "test~test", CommandName: "test", ExtensionName: goExtension.Name, Cache: true,
			TaskCachePolicy: &extension.TaskCachePolicy{Key: &derived},
		},
	}
	hashFor := func() string {
		t.Helper()
		cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		hash, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute Go test cache key: %v", err)
		}
		return hash
	}

	baseline := hashFor()
	writeTestFile(t, workPath, "go 1.26\n\nuse (\n\t./proj\n)\n")
	if changed := hashFor(); changed == baseline {
		t.Fatal("go.work content changed without moving the real Go test task cache key")
	}
	writeTestFile(t, workPath, "go 1.26\n\nuse ./proj\n")
	writeTestFile(t, sumPath, "example.com/dependency v1.0.0 h1:second\n")
	if changed := hashFor(); changed == baseline {
		t.Fatal("go.work.sum content changed without moving the real Go test task cache key")
	}
}

// TestCacheKeyVariesWithDeclaredPlatformSelection locks the cache-identity half
// of nature+intent-scoped builds.
//
// Ordinary `build` compiles for the host and reaches a wider platform set only
// when a project declares `platforms` (or passes `--target`). That request
// decides the BYTES the compile produces, so it has to reach the cache key. It
// does so as an ordinary plan-time parameter: the task declares
// `inputs.platforms.from = "params"`, DeriveTaskCacheKey lifts the name into
// Key.Params, and taskCacheParams resolves it through the same
// config→options→flags merge every other parameter uses.
//
// The failure this forbids is the one the analyst flagged: a task that read the
// platform set for itself at execution time (opening putnami.json the way
// platform.ReadGoEntrypoint does) would change what it builds without changing
// where it is stored, and the entry built for one platform set would be served
// as the answer for another. The control task — one that declares no platforms
// input — must NOT move, so the signal cannot over-invalidate lint or test.
func TestCacheKeyVariesWithDeclaredPlatformSelection(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	// The fixtures are real putnami.json bodies, decoded the way the workspace
	// loader decodes them. The contract under test is what a USER writes in a
	// project's config file, so a hand-built Go map that merely resembles that
	// file would pin the resemblance instead of the contract.
	const (
		noPlatforms = `{}`
		twoDeclared = `{"options":{"@putnami/go":{"platforms":["linux/amd64","darwin/arm64"]}}}`
		oneDeclared = `{"options":{"@putnami/go":{"platforms":["linux/amd64"]}}}`
	)
	projectWith := func(configJSON string) *workspace.Project {
		t.Helper()
		config := &wsproto.ProjectConfig{}
		if err := json.Unmarshal([]byte(configJSON), config); err != nil {
			t.Fatalf("decode project config fixture: %v", err)
		}
		return &workspace.Project{ID: "/proj", Name: "proj", Path: "proj", Config: config}
	}
	mkJob := func(project *workspace.Project, task string, params []string) *ScheduledJob {
		job := &ScheduledJob{
			Project:   project,
			Extension: &extension.ExtensionDescription{Name: "@putnami/go", Path: t.TempDir()},
			JobDef: &extension.JobDefinition{
				Name: "build~compile", CommandName: "build", ExtensionName: "@putnami/go", Cache: true,
				TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Params: params}},
			},
		}
		return declareCacheTestTask(job, task)
	}
	hashFor := func(job *ScheduledJob) string {
		t.Helper()
		hash, err := computeJobCacheHash(ws, job, nil, nil, cm, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return hash
	}

	// The declared platform inputs of build-compile.
	compileParams := []string{"platforms", "target"}

	hostOnly := hashFor(mkJob(projectWith(noPlatforms), "build-compile", compileParams))
	twoPlatforms := hashFor(mkJob(projectWith(twoDeclared), "build-compile", compileParams))
	onePlatform := hashFor(mkJob(projectWith(oneDeclared), "build-compile", compileParams))

	if hostOnly == twoPlatforms {
		t.Error("declaring `platforms` did not move the compile cache key; a host-only entry would be served " +
			"as the answer for a cross-compiled build")
	}
	if onePlatform == twoPlatforms {
		t.Error("changing the `platforms` set did not move the compile cache key; two different platform sets " +
			"share one entry")
	}

	// Control: a task that declares no platform input keeps one identity, so the
	// project-level declaration cannot invalidate work it does not affect.
	lintHostOnly := hashFor(mkJob(projectWith(noPlatforms), "lint-staticcheck", []string{"tags"}))
	lintPlatforms := hashFor(mkJob(projectWith(oneDeclared), "lint-staticcheck", []string{"tags"}))
	if lintHostOnly != lintPlatforms {
		t.Error("a task that declares no platforms input varied with the project's platform declaration " +
			"(over-invalidation)")
	}
}

// TestDeclaredHostPlatformReachesTheCacheKey is the other half of platform cache
// identity, and it covers the case the `platforms`
// parameter cannot: the DEFAULT.
//
// With no `platforms` and no `--target`, build-compile compiles for the machine
// it runs on. That makes the output — and, for a library, the compile-check
// VERDICT — a function of the host, which no parameter carries. The task
// therefore declares `hostPlatform` as a runtime input, and this test pins that
// the declaration is actually PLUMBED into the hash rather than being inert
// documentation, which is what `from: "runtime"` was before: DeriveTaskCacheKey
// collected the names into Key.Runtime and nothing ever read them.
//
// The value's effect (darwin vs linux producing different keys) is pinned in
// store.TestCacheKeyVariesWithHostPlatform, which can vary the host that this
// in-process test cannot.
func TestDeclaredHostPlatformReachesTheCacheKey(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	mkJob := func(runtimeInputs []string) *ScheduledJob {
		job := &ScheduledJob{
			Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
			Extension: &extension.ExtensionDescription{Name: "@putnami/go", Path: t.TempDir()},
			JobDef: &extension.JobDefinition{
				Name: "build~compile", CommandName: "build", ExtensionName: "@putnami/go", Cache: true,
				TaskCachePolicy: &extension.TaskCachePolicy{
					Key: &extension.TaskCacheKey{Runtime: runtimeInputs},
				},
			},
		}
		return declareCacheTestTask(job, "build-compile")
	}

	declaring := mkJob([]string{"hostPlatform", "extensionVersion"})
	if got, want := taskRuntimeIdentity(declaring), []string{"hostPlatform=" + HostPlatform()}; !reflect.DeepEqual(got, want) {
		t.Errorf("taskRuntimeIdentity = %v, want %v; `extensionVersion` must resolve to nothing because the "+
			"version is already an unconditional key field", got, want)
	}
	if got := taskRuntimeIdentity(mkJob(nil)); got != nil {
		t.Errorf("taskRuntimeIdentity for a task declaring no runtime input = %v, want nil", got)
	}

	hashFor := func(job *ScheduledJob) string {
		t.Helper()
		hash, err := computeJobCacheHash(ws, job, nil, nil, cm, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return hash
	}

	// Plumbing: the declaration must MOVE the key. If taskRuntimeIdentity were
	// dropped on the way into store.BuildCacheKey these would match.
	if hashFor(declaring) == hashFor(mkJob(nil)) {
		t.Error("declaring the `hostPlatform` runtime input did not move the cache key; the host platform " +
			"is not reaching the hash, so a darwin entry stays restorable on linux")
	}

	// A runtime input the CLI cannot answer contributes nothing, rather than an
	// empty placeholder that would claim the key covers something it does not.
	if got := taskRuntimeIdentity(mkJob([]string{"someFutureAmbientFact"})); got != nil {
		t.Errorf("unrecognized runtime input resolved to %v, want nil", got)
	}
}

// TestCacheKeyVersionAwareForVersionVarProjectUnderNonBuildCommand locks the fix
// for the stale-CLI-upgrade bug: a version-stamping build task in a project that
// declares version-var must vary its cache key with the commit version EVEN when
// the driving command (package/publish/deploy) does not surface version-var as a
// flag — otherwise a commit that leaves the project's own sources unchanged hits
// the cache and republishes a prior commit's binary under the new release tag.
// The control (a task that does not read version-var) must NOT vary, so the
// project-level signal cannot over-invalidate test/lint.
func TestCacheKeyVersionAwareForVersionVarProjectUnderNonBuildCommand(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	versionVarProject := &workspace.Project{
		ID: "/proj", Name: "proj", Path: "proj",
		Config: &wsproto.ProjectConfig{
			Options: map[string]map[string]any{
				// The runtime explicitly supports this camelCase spelling while the
				// task contract canonically declares the kebab-case name.
				"@putnami/go": {"versionVar": "go.putnami.dev/x/internal/cli.Version"},
			},
		},
	}
	mkJob := func(name string, params []string) *ScheduledJob {
		job := &ScheduledJob{
			Project:   versionVarProject,
			Extension: &extension.ExtensionDescription{Name: "@putnami/go", Path: t.TempDir()},
			JobDef: &extension.JobDefinition{
				Name: name, ExtensionName: "@putnami/go", Cache: true,
				TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Params: params}},
			},
		}
		return declareCacheTestTask(job, name)
	}
	// cross-compile reads version-var (stamps via ldflags); test-exec does not.
	crossCompile := mkJob("build-cross-compile", []string{"target", "version-var"})
	testLike := mkJob("test-exec", []string{"target"})

	verA := rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-5c82d3a1", Suffix: "5c82d3a1"})
	verB := rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-8e6fb533", Suffix: "8e6fb533"})

	// commandParams=nil models package/publish/deploy, which never surface a
	// version-var flag — the exact path the original bug shipped under.
	hashFor := func(job *ScheduledJob, ver RunVersions) string {
		t.Helper()
		h, err := computeJobCacheHash(ws, job, nil, ver, cm, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return h
	}

	if hashFor(crossCompile, verA) == hashFor(crossCompile, verB) {
		t.Error("cross-compile cache key did not vary with the commit version under a non-build command — a stale binary would be republished under the new tag")
	}
	if hashFor(testLike, verA) != hashFor(testLike, verB) {
		t.Error("test task cache key wrongly varied with the commit version (over-invalidation): version-stamping signal leaked into a task that does not stamp")
	}
}

// TestCacheKeyIncludesLocalRuntimeImplementationDigest locks the
// invariant after direct-exec deletion: a workspace-local prepared runtime can
// retain its dev version while its source changes, and consumer tasks must not
// replay the prior implementation's result. The fixture's task is a plain
// command because a runtime-backed preBuild hook can influence it even when the
// task itself does not reference {extensionRuntime}.
func TestCacheKeyIncludesLocalRuntimeImplementationDigest(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	extRoot := filepath.Join(ws.Root, "go", "extension")
	writeTestFile(t, filepath.Join(extRoot, "go.mod"), "module example.com/putnami-go\n\ngo 1.25\n")
	mainPath := filepath.Join(extRoot, "cmd", "putnami-go", "main.go")
	writeTestFile(t, mainPath, "package main\n\nfunc main() {}\n")

	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@putnami/go", Version: "0.1.0-dev", Path: extRoot, RelPath: "go/extension", LocalSource: true,
			Runtime: &extension.RuntimeDefinition{
				Executable: "compiled/putnami-go",
				Prepare: &extension.RuntimePrepare{
					Command: "{extensionRoot}/bin/prepare",
					Inputs:  []string{"go.mod", "cmd/**"},
				},
			},
			Hooks: &extension.ManifestHooks{
				PreBuild: &extension.HookDefinition{Command: "{extensionRuntime}"},
			},
		},
		JobDef: &extension.JobDefinition{
			Name:          "build",
			ExtensionName: "@putnami/go",
			Cache:         true,
			Command:       "true",
		},
	}

	hashForPlan := func() string {
		t.Helper()
		cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		hash, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("computeJobCacheHash: %v", err)
		}
		return hash
	}

	first := hashForPlan()
	if again := hashForPlan(); again != first {
		t.Fatalf("unchanged local runtime moved cache key: %s -> %s", first, again)
	}

	writeTestFile(t, mainPath, "package main\n\nfunc main() { println(\"changed\") }\n")
	if afterSource := hashForPlan(); afterSource == first {
		t.Fatal("local runtime source change did not re-key the consumer task; stale extension output could cache-hit")
	}
}

func TestCacheKeyIncludesAbsoluteLocalRuntimeImplementationDigest(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	extRoot := t.TempDir()
	writeTestFile(t, filepath.Join(extRoot, "go.mod"), "module example.com/putnami-go\n\ngo 1.25\n")
	mainPath := filepath.Join(extRoot, "cmd", "putnami-go", "main.go")
	writeTestFile(t, mainPath, "package main\n\nfunc main() {}\n")

	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@putnami/go", Version: "0.1.0-dev", Path: extRoot, LocalSource: true,
			Runtime: &extension.RuntimeDefinition{
				Executable: "compiled/putnami-go",
				Prepare: &extension.RuntimePrepare{
					Command: "{extensionRoot}/bin/prepare",
					Inputs:  []string{"go.mod", "cmd/**"},
				},
			},
		},
		JobDef: &extension.JobDefinition{
			Name: "build", ExtensionName: "@putnami/go", Cache: true,
			Command: "{extensionRuntime}",
			Args:    []string{"build"},
		},
	}

	hashForPlan := func() string {
		t.Helper()
		cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		hash, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("computeJobCacheHash: %v", err)
		}
		return hash
	}

	before := hashForPlan()
	writeTestFile(t, mainPath, "package main\n\nfunc main() { println(\"changed\") }\n")
	if after := hashForPlan(); after == before {
		t.Fatal("absolute local extension source change did not re-key the consumer task")
	}
}

// A workspace-local extension without a declared runtime keeps its version as
// its identity: the key does not hash the source beside its manifest. (An
// installed extension's whole tree is its identity; see
// TestInstalledExtensionTreeKeysItsTasks.)
func TestCacheKeyLeavesNonRuntimeExtensionSourcesOut(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)

	for _, tc := range []struct {
		name        string
		relPath     string
		localSource bool
		runtime     bool
		command     string
		args        []string
	}{
		{
			name:        "workspace non-runtime extension",
			relPath:     "extensions/non-direct",
			localSource: true,
			command:     "{extensionRoot}/bin/custom-run",
			args:        []string{"build"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dirName := strings.ReplaceAll(tc.name, " ", "-")
			extRoot := filepath.Join(ws.Root, "extensions", dirName)
			relPath := tc.relPath
			if relPath != "" {
				relPath = filepath.ToSlash(filepath.Join("extensions", dirName))
			}
			writeTestFile(t, filepath.Join(extRoot, "go.mod"), "module example.com/extension\n\ngo 1.25\n")
			mainPath := filepath.Join(extRoot, "cmd", "putnami-go", "main.go")
			writeTestFile(t, mainPath, "package main\n\nfunc main() {}\n")

			var runtimeDefinition *extension.RuntimeDefinition
			if tc.runtime {
				runtimeDefinition = &extension.RuntimeDefinition{
					Executable: "compiled/putnami-go",
					Prepare: &extension.RuntimePrepare{
						Command: "{extensionRoot}/bin/prepare",
						Inputs:  []string{"go.mod", "cmd/**"},
					},
				}
			}
			job := &ScheduledJob{
				Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
				Extension: &extension.ExtensionDescription{
					Name: "@putnami/go", Version: "0.1.0-dev", Path: extRoot, RelPath: relPath, LocalSource: tc.localSource,
					Runtime: runtimeDefinition,
				},
				JobDef: &extension.JobDefinition{
					Name: "build", ExtensionName: "@putnami/go", Cache: true,
					Command: tc.command, Args: tc.args,
				},
			}

			hashFor := func() string {
				t.Helper()
				cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
				hash, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
				if err != nil {
					t.Fatalf("computeJobCacheHash: %v", err)
				}
				return hash
			}

			before := hashFor()
			writeTestFile(t, mainPath, "package main\n\nfunc main() { println(\"changed\") }\n")
			if after := hashFor(); after != before {
				t.Fatalf("extension implementation unexpectedly changed %s cache key: %s -> %s", tc.name, before, after)
			}
		})
	}
}

func TestIsCacheableResult(t *testing.T) {
	metaEvent := RawJobEvent{Version: 1, Type: EventTypeMeta}
	logEvent := RawJobEvent{Version: 1, Type: EventTypeLog}

	deterministic := &ScheduledJob{
		JobDef: &extension.JobDefinition{
			TaskCachePolicy: &extension.TaskCachePolicy{Deterministic: true},
		},
	}
	nonDeterministic := &ScheduledJob{
		JobDef: &extension.JobDefinition{
			TaskCachePolicy: &extension.TaskCachePolicy{Deterministic: false},
		},
	}
	noPolicy := &ScheduledJob{JobDef: &extension.JobDefinition{}}

	tests := []struct {
		name     string
		job      *ScheduledJob
		result   *JobResult
		expected bool
	}{
		{
			name:     "success is cacheable regardless of determinism",
			job:      nonDeterministic,
			result:   &JobResult{Status: "success"},
			expected: true,
		},
		{
			name:     "deterministic skip after job ran (meta) is cacheable",
			job:      deterministic,
			result:   &JobResult{Status: "skipped", Events: []RawJobEvent{metaEvent, logEvent}},
			expected: true,
		},
		{
			name:     "non-deterministic skip is not cacheable (env-dependent)",
			job:      nonDeterministic,
			result:   &JobResult{Status: "skipped", Events: []RawJobEvent{metaEvent}},
			expected: false,
		},
		{
			name:     "skip with no cache policy is not cacheable",
			job:      noPolicy,
			result:   &JobResult{Status: "skipped", Events: []RawJobEvent{metaEvent}},
			expected: false,
		},
		{
			name:     "deterministic skip without meta (wrapper bail-out) is not cacheable",
			job:      deterministic,
			result:   &JobResult{Status: "skipped", Events: []RawJobEvent{logEvent}},
			expected: false,
		},
		{
			name:     "deterministic skip with no events is not cacheable",
			job:      deterministic,
			result:   &JobResult{Status: "skipped"},
			expected: false,
		},
		{
			name:     "failed is not cacheable",
			job:      deterministic,
			result:   &JobResult{Status: "failed", Events: []RawJobEvent{metaEvent}},
			expected: false,
		},
		{
			name:     "canceled is not cacheable",
			job:      deterministic,
			result:   &JobResult{Status: "canceled", Events: []RawJobEvent{metaEvent}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isCacheableResult(tt.job, tt.result)
			if got != tt.expected {
				t.Errorf("isCacheableResult() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestEntryResultFromJobResultKeepsOnlyCacheSummaryEvents(t *testing.T) {
	result := &JobResult{
		Status: "success",
		Events: []RawJobEvent{
			{Version: 1, Type: EventTypeMeta},
			{Version: 1, Type: EventTypeMetric, Data: map[string]any{"name": "tests-total", "value": float64(51)}},
			{Version: 1, Type: EventTypeLog, Message: "streaming output"},
			{Version: 1, Type: EventTypeProgress, Data: map[string]any{"current": float64(1), "total": float64(2)}},
			{Version: 1, Type: EventTypeSummary, Data: map[string]any{"message": "51/51 passed"}},
		},
	}

	entry := entryResultFromJobResult(result)
	if len(entry.Events) != 3 {
		t.Fatalf("cached events = %d, want meta+metric+summary only: %+v", len(entry.Events), entry.Events)
	}
	for _, ev := range entry.Events {
		if ev.Type == EventTypeLog || ev.Type == EventTypeProgress {
			t.Fatalf("non-summary event %q should not be cached: %+v", ev.Type, entry.Events)
		}
	}
}

func TestSchedulerLocalTaskCacheHitRestoresResultEvents(t *testing.T) {
	job := declaredJob("test~run", "run", &extension.TaskDeclaration{})
	job.JobDef.FilePatterns = []string{"src.txt"}
	f := newCaptureFixture(t, job)
	writeFileAt(t, filepath.Join(f.ws.Root, captureTestProject, "src.txt"), "stable\n")

	hash, err := computeJobCacheHash(f.ws, job, nil, nil, f.cache, nil)
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

	hash, err := computeJobCacheHash(ws, job, nil, nil, cm, nil)
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
			Writes: []extension.ResourceRef{{ID: genResourceID}},
		},
		Step: &extension.PipelineStep{Task: "generate"},
	}

	genDir := filepath.Join(projDir, ".gen")
	writeFileAt(t, filepath.Join(genDir, "public", "docs", "index.html"), "<html>docs</html>")
	writeFileAt(t, filepath.Join(genDir, "generate-result.json"), `{"hash":"abc"}`)

	prime := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache)
	hash, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
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
			Writes: []extension.ResourceRef{{ID: clientsResourceID}},
		},
		Step: &extension.PipelineStep{Task: "describe"},
	}

	clientDir := filepath.Join(projDir, "internal", "api-client")
	writeFileAt(t, filepath.Join(clientDir, "client.gen.go"), "package client\n")
	writeFileAt(t, filepath.Join(clientDir, "go.mod"), "module x\n")
	resultData := map[string]any{"clientOutput": "internal/api-client"}

	prime := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache)
	hash, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
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
			Writes: []extension.ResourceRef{{ID: genResourceID}, {ID: clientsResourceID}},
		},
		Step: &extension.PipelineStep{Task: "generate"},
	}

	genDir := filepath.Join(projDir, ".gen")
	clientDir := filepath.Join(projDir, "clients", "ts")
	writeFileAt(t, filepath.Join(genDir, "clientgen", "config.json"), `{"targets":["ts"]}`)
	writeFileAt(t, filepath.Join(clientDir, "src", "index.ts"), "export class WidgetsClient {}\n")
	resultData := map[string]any{"clientOutput": "clients/ts"}

	prime := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cache)
	hash, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
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

func TestParamHasVersionVar(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
		want   bool
	}{
		{"empty params", map[string]any{}, false},
		{"unrelated param", map[string]any{"target": "es2022"}, false},
		{"kebab-case set", map[string]any{"version-var": "pkg.Version"}, true},
		{"camelCase set", map[string]any{"versionVar": "pkg.Version"}, true},
		{"both set", map[string]any{"version-var": "a", "versionVar": "b"}, true},
		{"empty string ignored", map[string]any{"version-var": ""}, false},
		{"non-string ignored", map[string]any{"version-var": 42}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := paramHasVersionVar(tt.params); got != tt.want {
				t.Errorf("paramHasVersionVar(%v) = %v, want %v", tt.params, got, tt.want)
			}
		})
	}
}

func TestTaskCacheKey_VersionSuffixVariesHash_WhenVersionVarSet(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Path: t.TempDir(),
		},
		JobDef: &extension.JobDefinition{
			Name:          "build~cross-compile",
			ExtensionName: "@test/ext",
			Cache:         true,
			TaskCachePolicy: &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{
				Params: []string{"version-var"},
			}},
		},
	}
	declareCacheTestTask(job, "cross-compile")
	params := map[string]any{"version-var": "pkg.Version"}

	hashAbc, err := computeJobCacheHash(ws, job, params, rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-abc1234", Suffix: "abc1234"}), cache, nil)
	if err != nil {
		t.Fatalf("compute task key abc: %v", err)
	}
	hashDef, err := computeJobCacheHash(ws, job, params, rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-def5678", Suffix: "def5678"}), cache, nil)
	if err != nil {
		t.Fatalf("compute task key def: %v", err)
	}
	if hashAbc == hashDef {
		t.Errorf("expected distinct cache hashes for different version suffixes, got %s twice", hashAbc)
	}
}

func TestTaskCacheKey_VersionSuffixIgnored_WhenVersionVarAbsent(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Path: t.TempDir(),
		},
		JobDef: &extension.JobDefinition{
			Name:          "test~unit",
			ExtensionName: "@test/ext",
			Cache:         true,
		},
	}
	declareCacheTestTask(job, "unit")
	params := map[string]any{} // no version-var

	hashAbc, err := computeJobCacheHash(ws, job, params, rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-abc1234", Suffix: "abc1234"}), cache, nil)
	if err != nil {
		t.Fatalf("compute task key abc: %v", err)
	}
	hashDef, err := computeJobCacheHash(ws, job, params, rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-def5678", Suffix: "def5678"}), cache, nil)
	if err != nil {
		t.Fatalf("compute task key def: %v", err)
	}
	if hashAbc != hashDef {
		t.Errorf("expected same cache hash when version-var absent, got %s vs %s", hashAbc, hashDef)
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
			Writes: []extension.ResourceRef{{ID: genResourceID}},
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

	oldVersion := rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-5e748999", Suffix: "5e748999", SHA: "5e748999aa"})
	newVersion := rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-dec4e2ec", Suffix: "dec4e2ec", SHA: "dec4e2ecbb"})
	oldKeys, err := PrecomputeKeys(ws, planned, nil, oldVersion, cache, CacheBypass{})
	if err != nil {
		t.Fatalf("precompute old version: %v", err)
	}
	newKeys, err := PrecomputeKeys(ws, planned, nil, newVersion, cache, CacheBypass{})
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

func TestTaskIsVersionAware(t *testing.T) {
	tests := []struct {
		name string
		job  *ScheduledJob
		want bool
	}{
		{
			name: "nil policy",
			job:  &ScheduledJob{JobDef: &extension.JobDefinition{}},
			want: false,
		},
		{
			name: "policy without versionAware",
			job: &ScheduledJob{JobDef: &extension.JobDefinition{
				TaskCachePolicy: &extension.TaskCachePolicy{},
			}},
			want: false,
		},
		{
			name: "policy with versionAware",
			job: &ScheduledJob{JobDef: &extension.JobDefinition{
				TaskCachePolicy: &extension.TaskCachePolicy{VersionAware: true},
			}},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := taskIsVersionAware(tt.job); got != tt.want {
				t.Errorf("taskIsVersionAware() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Build tasks that stamp the publish version into their artifacts (npm
// packages, Go modules, version-stamped archives) declare cache.versionAware.
// Their cache key must vary with the commit suffix even though they carry no
// Go `version-var` param — otherwise repackaging at a new release id would be
// served stale bytes embedding the previous version.
func TestTaskCacheKey_VersionSuffixVariesHash_WhenVersionAware(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Path: t.TempDir(),
		},
		JobDef: &extension.JobDefinition{
			Name:            "build~npm",
			ExtensionName:   "@test/ext",
			Cache:           true,
			TaskCachePolicy: &extension.TaskCachePolicy{VersionAware: true},
		},
	}
	declareCacheTestTask(job, "npm")
	params := map[string]any{} // no version-var — versionAware is the only signal

	hashAbc, err := computeJobCacheHash(ws, job, params, rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-abc1234", Suffix: "abc1234"}), cache, nil)
	if err != nil {
		t.Fatalf("compute task key abc: %v", err)
	}
	hashDef, err := computeJobCacheHash(ws, job, params, rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-def5678", Suffix: "def5678"}), cache, nil)
	if err != nil {
		t.Fatalf("compute task key def: %v", err)
	}
	if hashAbc == hashDef {
		t.Errorf("expected distinct cache hashes for different version suffixes on a version-aware task, got %s twice", hashAbc)
	}
}

// Re-running a version-aware build task on the same commit (same full version)
// must produce the same cache key so the cache can hit safely.
func TestTaskCacheKey_VersionAware_SameVersionSameHash(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Path: t.TempDir(),
		},
		JobDef: &extension.JobDefinition{
			Name:            "build~go",
			ExtensionName:   "@test/ext",
			Cache:           true,
			TaskCachePolicy: &extension.TaskCachePolicy{VersionAware: true},
		},
	}
	declareCacheTestTask(job, "go")
	version := rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-abc1234", Suffix: "abc1234"})

	hash1, err := computeJobCacheHash(ws, job, nil, version, cache, nil)
	if err != nil {
		t.Fatalf("compute task key 1: %v", err)
	}
	hash2, err := computeJobCacheHash(ws, job, nil, version, cache, nil)
	if err != nil {
		t.Fatalf("compute task key 2: %v", err)
	}
	if hash1 != hash2 {
		t.Errorf("expected identical cache hash for the same version, got %s vs %s", hash1, hash2)
	}
}

// A task that is neither version-aware nor carries a version-var param must not
// vary its cache key with the commit suffix — build/test/lint outputs should
// stay content-oriented and not rebuild solely because the release id moved.
func TestTaskCacheKey_VersionSuffixIgnored_WhenNotVersionAware(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Path: t.TempDir(),
		},
		JobDef: &extension.JobDefinition{
			Name:            "build~transpile",
			ExtensionName:   "@test/ext",
			Cache:           true,
			TaskCachePolicy: &extension.TaskCachePolicy{Deterministic: true},
		},
	}
	declareCacheTestTask(job, "transpile")

	hashAbc, err := computeJobCacheHash(ws, job, nil, rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-abc1234", Suffix: "abc1234"}), cache, nil)
	if err != nil {
		t.Fatalf("compute task key abc: %v", err)
	}
	hashDef, err := computeJobCacheHash(ws, job, nil, rootLineVersions(&JobContextVersion{Base: "1.0.0", Full: "1.0.0-def5678", Suffix: "def5678"}), cache, nil)
	if err != nil {
		t.Fatalf("compute task key def: %v", err)
	}
	if hashAbc != hashDef {
		t.Errorf("expected same cache hash for a non-version-aware task, got %s vs %s", hashAbc, hashDef)
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

func TestCacheKeyIncludesProjectDeclaredFilePatterns(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)

	lockPath := filepath.Join(ws.Root, "proj", "content.lock.json")
	writeLock := func(content string) {
		t.Helper()
		if err := os.WriteFile(lockPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Models putnami.dev's real shape: the project references the extension by
	// PATH ("/typescript/extension") and keys its options by that same ref,
	// while the extension's canonical Name resolves to "@putnami/typescript" —
	// the mismatch that silently dropped the declaration.
	extPath := filepath.Join(ws.Root, "typescript", "extension")
	mkJob := func(optionsKey string) *ScheduledJob {
		proj := &workspace.Project{
			ID: "/proj", Name: "proj", Path: "proj",
			Config: &wsproto.ProjectConfig{Extensions: []string{"/typescript/extension"}},
		}
		if optionsKey != "" {
			proj.Config.Options = map[string]map[string]any{
				optionsKey: {"filePatterns": []any{"content.lock.json"}},
			}
		}
		return &ScheduledJob{
			Project:   proj,
			Extension: &extension.ExtensionDescription{Name: "@putnami/typescript", Path: extPath},
			JobDef: &extension.JobDefinition{
				Name: "build-generate", ExtensionName: "@putnami/typescript", Cache: true,
				// Baseline task patterns, like the real generate task: without
				// them policy.Files is empty and the hasher falls back to the
				// whole project tree, which would mask the opt-in behavior.
				FilePatterns: []string{"src/**/*"},
			},
		}
	}

	// A fresh CacheManager per computation models separate CLI invocations —
	// lookupFileHash memoizes per process, and a real lock bump is a commit
	// followed by a new build.
	hashFor := func(job *ScheduledJob) string {
		t.Helper()
		cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		h, err := computeJobCacheHash(ws, job, nil, nil, cm, nil)
		if err != nil {
			t.Fatalf("compute cache key: %v", err)
		}
		return h
	}

	// Project-declared filePatterns (options.{ext}.filePatterns) must reach the
	// cache key: an edit to the matched file re-keys the job. commandParams=nil
	// models a plain `putnami build` with no CLI flags — the path where the
	// declaration used to be silently dropped.
	for name, key := range map[string]string{
		"ref-keyed":            "/typescript/extension",
		"canonical-name-keyed": "@putnami/typescript",
	} {
		declared := mkJob(key)
		writeLock(`{"bundles":[]}`)
		before := hashFor(declared)
		writeLock(`{"bundles":[{"digest":"x"}]}`)
		after := hashFor(declared)
		if before == after {
			t.Errorf("%s: cache key did not vary with a project-declared filePatterns input — a content.lock.json bump would cache-hit stale generate output", name)
		}
	}

	// Without the declaration the file must stay out of the key (opt-in).
	undeclared := mkJob("")
	writeLock(`{"bundles":[]}`)
	first := hashFor(undeclared)
	writeLock(`{"bundles":[{"digest":"x"}]}`)
	second := hashFor(undeclared)
	if first != second {
		t.Error("cache key varied with an undeclared file — project filePatterns must be opt-in")
	}
}

// TestCacheKeyIncludesProjectDeclaredEnvInputs pins the env-gated-suite fix: a
// project that declares options.test.envInputs must fold those env vars' current
// values into its cache key, so an env-set run (a live-target run) keys
// differently from an env-less run (a skip-with-reason) instead of silently
// serving its cached result. Mirrors TestCacheKeyIncludesProjectDeclaredFilePatterns.
func TestCacheKeyIncludesProjectDeclaredEnvInputs(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)

	// Options keyed by the command layer ("test"), matching the issue's example
	// { "options": { "test": { "envInputs": [...] } } }. envInputs=nil models a
	// project with no declaration at all.
	mkJob := func(envInputs []any) *ScheduledJob {
		proj := &workspace.Project{ID: "/proj", Name: "proj", Path: "proj", Config: &wsproto.ProjectConfig{}}
		if envInputs != nil {
			proj.Config.Options = map[string]map[string]any{
				"test": {"envInputs": envInputs},
			}
		}
		return &ScheduledJob{
			Project:   proj,
			Extension: &extension.ExtensionDescription{Name: "@putnami/typescript", Path: t.TempDir()},
			JobDef:    &extension.JobDefinition{Name: "test", ExtensionName: "@putnami/typescript", Cache: true},
		}
	}

	// A fresh CacheManager per computation models separate CLI invocations; the
	// env hash is read live from the process environment on each call.
	hashFor := func(job *ScheduledJob) string {
		t.Helper()
		cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		h, err := computeJobCacheHash(ws, job, nil, nil, cm, nil)
		if err != nil {
			t.Fatalf("compute cache key: %v", err)
		}
		return h
	}

	// A literal declared env var keys on its value: same value → same key,
	// changed value → different key. This is the exact bug — an env-set run must
	// not reuse an env-less run's cached result.
	t.Run("literal name keys on value", func(t *testing.T) {
		job := mkJob([]any{"PUTNAMI_TEST_ENVINPUT_TARGET"})

		t.Setenv("PUTNAMI_TEST_ENVINPUT_TARGET", "https://cp.prod")
		prod := hashFor(job)
		if again := hashFor(job); prod != again {
			t.Fatal("same declared env value must produce the same cache key")
		}

		t.Setenv("PUTNAMI_TEST_ENVINPUT_TARGET", "https://cp.staging")
		if staging := hashFor(job); prod == staging {
			t.Fatal("changing a declared env var's value must re-key the job — otherwise an env-set run serves an env-less run's cached result")
		}
	})

	// An undeclared env var never affects the key: envInputs is strictly opt-in,
	// so declaring nothing leaves the whole environment out of the key.
	t.Run("undeclared env var is ignored", func(t *testing.T) {
		job := mkJob(nil)
		t.Setenv("PUTNAMI_TEST_ENVINPUT_TARGET", "https://cp.prod")
		prod := hashFor(job)
		t.Setenv("PUTNAMI_TEST_ENVINPUT_TARGET", "https://cp.staging")
		if staging := hashFor(job); prod != staging {
			t.Fatal("an undeclared env var must not affect the cache key — envInputs is opt-in")
		}
	})

	// A glob is expanded against the current environment: it folds every matching
	// set var's value into the key, so an env-set run keys differently from an
	// env-less run, and rotating any matched var re-keys again.
	t.Run("glob folds matching set vars", func(t *testing.T) {
		job := mkJob([]any{"PUTNAMI_TEST_ENVINPUT_*"})

		envless := hashFor(job) // no PUTNAMI_TEST_ENVINPUT_* set

		t.Setenv("PUTNAMI_TEST_ENVINPUT_TARGET", "https://cp.prod")
		t.Setenv("PUTNAMI_TEST_ENVINPUT_TOKEN", "secret")
		envset := hashFor(job)
		if envless == envset {
			t.Fatal("a glob envInput must key an env-set run differently from an env-less one")
		}

		t.Setenv("PUTNAMI_TEST_ENVINPUT_TOKEN", "rotated")
		if rotated := hashFor(job); rotated == envset {
			t.Fatal("changing a glob-matched var's value must re-key the job")
		}
	})

	// Declaring the same var both literally and via a glob that also matches it
	// must dedupe to a single entry — identical key to declaring it once. Only
	// one matching var is set so the glob resolves to exactly the literal.
	t.Run("literal and glob dedupe", func(t *testing.T) {
		t.Setenv("PUTNAMI_TEST_ENVINPUT_TARGET", "https://cp.prod")

		once := hashFor(mkJob([]any{"PUTNAMI_TEST_ENVINPUT_TARGET"}))
		twice := hashFor(mkJob([]any{"PUTNAMI_TEST_ENVINPUT_TARGET", "PUTNAMI_TEST_ENVINPUT_*"}))
		if once != twice {
			t.Fatal("declaring a var both literally and via a matching glob must yield the same key as declaring it once (dedupe/stability)")
		}
	})
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

// TestCacheKeyIgnoresTheRunSelection pins the invariant the `selection` wire
// block rests on: it is RUN STATE, not task identity.
//
// A task's verdict is a function of the files it read, and the selection says
// only which projects this invocation chose. Folding it in would give one task a
// different key per baseline and per `--projects` spelling — a permanent miss on
// every `--impacted` run, which is exactly the shape of run the member exists to
// describe.
func TestCacheKeyIgnoresTheRunSelection(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	newJob := func(selection *protocoljob.Selection) *ScheduledJob {
		job := &ScheduledJob{
			Project: &workspace.Project{ID: "/proj", Name: "proj", Path: "proj"},
			Extension: &extension.ExtensionDescription{
				Name: "@test/ext",
				Path: t.TempDir(),
			},
			JobDef: &extension.JobDefinition{
				Name:          "validate",
				CommandName:   "validate",
				ExtensionName: "@test/ext",
				Cache:         true,
			},
			Selection: selection,
		}
		declareCacheTestTask(job, "")
		return job
	}

	hashFor := func(selection *protocoljob.Selection) string {
		t.Helper()
		hash, err := computeJobCacheHash(ws, newJob(selection), nil, nil, cache, nil)
		if err != nil {
			t.Fatalf("compute task cache key: %v", err)
		}
		return hash
	}

	unselected := hashFor(nil)
	for _, selection := range []*protocoljob.Selection{
		{Mode: protocoljob.SelectionModeAll, ProjectIDs: []string{"/proj"}},
		{Mode: protocoljob.SelectionModeProjects, Scoped: true, ProjectIDs: []string{"/proj"}},
		{
			Mode: protocoljob.SelectionModeImpacted, Scoped: true,
			Baseline: "origin/main", BaselineSource: "trunk", ProjectIDs: []string{"/proj"},
		},
		{
			Mode: protocoljob.SelectionModeImpacted, Scoped: true,
			Baseline: "HEAD~5", BaselineSource: "explicit", ProjectIDs: []string{"/proj"},
		},
	} {
		if got := hashFor(selection); got != unselected {
			t.Errorf("selection %+v changed the task cache key", selection)
		}
	}
}
