package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/releaseset"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// cacheableJob builds a minimal cache-enabled job for a project.
func cacheableJob(name, projID, projName, projPath string, deps ...string) *ScheduledJob {
	const task = "cacheable"
	return &ScheduledJob{
		Project: &workspace.Project{ID: projID, Name: projName, Path: projPath},
		Extension: &extension.ExtensionDescription{
			Name: "@test/ext",
			Tasks: map[string]extension.TaskDefinition{
				task: {Declares: &extension.TaskDeclaration{}},
			},
		},
		JobDef: &extension.JobDefinition{
			Name:          name,
			ExtensionName: "@test/ext",
			Cache:         true,
		},
		Step:      &extension.PipelineStep{Task: task},
		DependsOn: deps,
	}
}

// TestPrecomputeKeys_MatchesLazyLookup verifies the plan-time pass produces the
// exact same keys that the lazy per-job lookup computes during execution,
// including the upstream-hash fold for a dependent job.
func TestPrecomputeKeys_MatchesExecutionKey(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	jobA := cacheableJob("a", "/proj", "proj", "proj")
	jobB := cacheableJob("b", "/test-proj", "test-proj", "test-proj", "/proj:a")

	keys, err := PrecomputeKeys(ws, []*ScheduledJob{jobB, jobA}, nil, nil, cache, CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}

	// Execution derives A without upstream keys, then folds A into B.
	lazyCache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	wantA, err := computeJobCacheHash(ws, jobA, nil, nil, lazyCache, nil)
	if err != nil {
		t.Fatalf("compute key A: %v", err)
	}
	wantB, err := computeJobCacheHash(ws, jobB, nil, nil, lazyCache, map[string]string{"/proj:a": wantA})
	if err != nil {
		t.Fatalf("compute key B: %v", err)
	}
	if keys["/proj:a"] != wantA {
		t.Errorf("key A = %s, want %s", keys["/proj:a"], wantA)
	}
	if keys["/test-proj:b"] != wantB {
		t.Errorf("key B = %s, want %s", keys["/test-proj:b"], wantB)
	}
}

func TestPrecomputeKeys_UpstreamChangePropagates(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	jobA := cacheableJob("a", "/proj", "proj", "proj")
	jobB := cacheableJob("b", "/test-proj", "test-proj", "test-proj", "/proj:a")
	planned := []*ScheduledJob{jobA, jobB}

	cache1 := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	keys1, err := PrecomputeKeys(ws, planned, nil, nil, cache1, CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys pass 1: %v", err)
	}

	if err := os.WriteFile(filepath.Join(ws.Root, "proj", "src.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	cache2 := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	keys2, err := PrecomputeKeys(ws, planned, nil, nil, cache2, CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys pass 2: %v", err)
	}
	if keys1["/proj:a"] == keys2["/proj:a"] {
		t.Error("dependency key did not change after its source changed")
	}
	if keys1["/test-proj:b"] == keys2["/test-proj:b"] {
		t.Error("dependent key did not change after upstream source changed")
	}
}

func TestPrecomputeKeys_SkipsDisabledJobs(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	jobA := cacheableJob("a", "/proj", "proj", "proj")
	jobA.JobDef.Cache = false
	jobB := cacheableJob("b", "/test-proj", "test-proj", "test-proj", "/proj:a")

	keys, err := PrecomputeKeys(ws, []*ScheduledJob{jobA, jobB}, nil, nil, cache, CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	if _, ok := keys["/proj:a"]; ok {
		t.Error("expected disabled job A to be absent from key set")
	}
	if _, ok := keys["/test-proj:b"]; !ok {
		t.Error("expected cacheable job B to have a key")
	}

	wantB, err := computeJobCacheHash(ws, jobB, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("compute key B: %v", err)
	}
	if keys["/test-proj:b"] != wantB {
		t.Errorf("key B = %s, want %s (no upstream contribution)", keys["/test-proj:b"], wantB)
	}
}

func TestInvocationProducerGoEmbedFailureReachesConsumerKeys(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	source := filepath.Join(ws.Root, "proj", "embed.go")
	if err := os.WriteFile(source, []byte("package proj\nimport _ \"embed\"\n//go:embed missing.sql\nvar payload string\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	producer := cacheableJob("prepare", "/proj", "proj", "proj")
	producer.JobDef.Cache = false // invocation-scoped producers do not publish keys
	producer.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Files: []string{"go-embed:build"}}}
	consumer := cacheableJob("consume", "/test-proj", "test-proj", "test-proj", producer.Key())
	consumer.InvocationProducer = producer
	cache := func() *store.CacheManager {
		return store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	}
	for _, test := range []struct {
		name string
		key  func() (string, error)
	}{
		{"precompute", func() (string, error) {
			keys, err := PrecomputeKeys(ws, []*ScheduledJob{producer, consumer}, nil, nil, cache(), CacheBypass{})
			return keys[consumer.Key()], err
		}},
		{"execution", func() (string, error) { return computeJobCacheHash(ws, consumer, nil, nil, cache(), nil) }},
		{"selection", func() (string, error) { return computeJobCacheHashWith(ws, consumer, nil, nil, cache(), nil, true) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			key, err := test.key()
			if !errors.Is(err, store.ErrGoEmbedInput) || key != "" || !strings.Contains(err.Error(), "missing.sql") {
				t.Fatalf("invalid invocation producer yielded consumer key %q, %v", key, err)
			}
		})
	}
	for _, tc := range []struct {
		name      string
		withCache bool
		bypass    bool
		cacheable bool
	}{
		{"declared", true, false, true},
		{"bypass", true, true, true},
		{"nil cache", false, false, true},
		{"uncacheable consumer", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			consumer.JobDef.Cache = tc.cacheable
			var manager *store.CacheManager
			if tc.withCache {
				manager = cache()
			}
			scheduler := newScheduler(ws, []*ScheduledJob{consumer}, nil, SchedulerConfig{NoCache: tc.bypass}, &mockRenderer{}, manager)
			var mu sync.Mutex
			key, row, release := scheduler.lookupRestoreOrClaim(context.Background(), consumer, tc.cacheable && !tc.bypass, &mu, map[string]string{})
			release()
			if key != "" || row == nil || row.Status != string(TaskStatusFailed) || row.Error == nil || !strings.Contains(row.Error.Message, "missing.sql") {
				t.Fatalf("consumer reached execution despite invalid producer: key=%q row=%+v", key, row)
			}
		})
	}
}

func TestClosureGoEmbedFailureTerminatesConsumerBeforeExecution(t *testing.T) {
	root := t.TempDir()
	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{lib, app})
	ws.Graph = workspace.BuildGraph(ws.Projects)
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "embed.go"), []byte("package lib\nimport _ \"embed\"\n//go:embed missing.sql\nvar payload string\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := cacheableJob("consume", app.ID, app.Name, app.Path)
	job.Project = app
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{ClosureFiles: []string{"go-embed:build"}}}
	cache := func() *store.CacheManager {
		return store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
	}
	if _, err := computeJobCacheHash(ws, job, nil, nil, cache(), nil); !errors.Is(err, store.ErrGoEmbedInput) {
		t.Fatalf("closure member failure lost typed semantic category: %v", err)
	}
	for _, tc := range []struct {
		name      string
		withCache bool
		bypass    bool
		cacheable bool
	}{
		{"declared", true, false, true},
		{"bypass", true, true, true},
		{"nil cache", false, false, true},
		{"uncacheable", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job.JobDef.Cache = tc.cacheable
			var manager *store.CacheManager
			if tc.withCache {
				manager = cache()
			}
			scheduler := newScheduler(ws, []*ScheduledJob{job}, nil, SchedulerConfig{NoCache: tc.bypass}, &mockRenderer{}, manager)
			var mu sync.Mutex
			key, row, release := scheduler.lookupRestoreOrClaim(context.Background(), job, tc.cacheable && !tc.bypass, &mu, map[string]string{})
			release()
			if key != "" || row == nil || row.Status != string(TaskStatusFailed) || row.Error == nil || !strings.Contains(row.Error.Message, "missing.sql") {
				t.Fatalf("closure failure reached execution: key=%q row=%+v", key, row)
			}
		})
	}
}

func TestSelectionFingerprintsPropagateInvocationProducerGoEmbedInputs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "library"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "library", "embed.go"), []byte("package library\nimport _ \"embed\"\n//go:embed missing.sql\nvar payload string\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := &workspace.Project{
		ID: "/library", Name: "@putnami/library", Path: "library", Type: "library",
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/library"),
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	memberKey := releaseset.MemberKey("npm", "@putnami/library")
	members := map[string]*workspace.Project{memberKey: project}
	producer := cacheableJob("prepare", project.ID, project.Name, project.Path)
	producer.Project = project
	producer.JobDef.Cache = false
	producer.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{Files: []string{"go-embed:build"}}}
	packageJob := packageTaskFixture(project, "test-provider", "1.0.0", t.TempDir())
	planned := []*ScheduledJob{producer, packageJob}
	fingerprint := func() (string, error) {
		cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
		got, err := SelectionFingerprints(ws, planned, nil, cache, testProfiles(), members)
		return got[memberKey], err
	}

	withoutProducer, err := fingerprint()
	if err != nil || withoutProducer == "" {
		t.Fatalf("no invocation producer should yield a selection fingerprint: %q, %v", withoutProducer, err)
	}
	packageJob.InvocationProducer = producer
	packageJob.DependsOn = []string{producer.Key()}
	if got, err := fingerprint(); err == nil || got != "" || !strings.Contains(err.Error(), "missing.sql") {
		t.Fatalf("invalid invocation producer yielded selection fingerprint %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(root, "library", "missing.sql"), []byte("SELECT 1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withProducer, err := fingerprint()
	if err != nil || withProducer == "" || withProducer == withoutProducer {
		t.Fatalf("valid invocation producer selection fingerprint = %q (without producer %q), %v", withProducer, withoutProducer, err)
	}
}

func TestPrecomputeKeys_NoCacheDisablesAll(t *testing.T) {
	t.Parallel()
	ws := makeExecutorTestWorkspace(t)
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))

	jobA := cacheableJob("a", "/proj", "proj", "proj")
	jobB := cacheableJob("b", "/test-proj", "test-proj", "test-proj", "/proj:a")

	keys, err := PrecomputeKeys(ws, []*ScheduledJob{jobA, jobB}, nil, nil, cache, CacheBypass{All: true})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected no keys when NoCache is set, got %d", len(keys))
	}
}

func TestTopoSortJobs_Order(t *testing.T) {
	t.Parallel()
	jobA := cacheableJob("a", "/proj", "proj", "proj")
	jobB := cacheableJob("b", "/proj", "proj", "proj", "/proj:a")
	jobC := cacheableJob("c", "/proj", "proj", "proj", "/proj:b")

	order, err := topoSortJobs([]*ScheduledJob{jobC, jobB, jobA})
	if err != nil {
		t.Fatalf("topoSortJobs: %v", err)
	}

	pos := make(map[string]int, len(order))
	for i, job := range order {
		pos[job.Key()] = i
	}
	if !(pos["/proj:a"] < pos["/proj:b"] && pos["/proj:b"] < pos["/proj:c"]) {
		t.Errorf("dependency order violated: %v", pos)
	}
}

func TestTopoSortJobs_IgnoresExternalDeps(t *testing.T) {
	t.Parallel()
	// jobA depends on a key that is not in the planned set; it must still be
	// orderable rather than treated as blocked.
	jobA := cacheableJob("a", "/proj", "proj", "proj", "/other:x")

	order, err := topoSortJobs([]*ScheduledJob{jobA})
	if err != nil {
		t.Fatalf("topoSortJobs: %v", err)
	}
	if len(order) != 1 {
		t.Fatalf("expected 1 ordered job, got %d", len(order))
	}
}

func TestTopoSortJobs_DetectsCycle(t *testing.T) {
	t.Parallel()
	jobX := cacheableJob("x", "/proj", "proj", "proj", "/proj:y")
	jobY := cacheableJob("y", "/proj", "proj", "proj", "/proj:x")

	if _, err := topoSortJobs([]*ScheduledJob{jobX, jobY}); err == nil {
		t.Error("expected cycle detection error, got nil")
	}
}

// D13: the selection fingerprint IS the package task's execution key minus the
// embedded version.
//
// So it moves with everything that shapes the packaging recipe — here, the
// packager's installed implementation — and stays put for everything that only
// shapes THIS invocation: the channels it advances, whether it is a dry run,
// the version stamp being applied, and the release-set plan bound into the job
// (which is derived from these very fingerprints and would otherwise define
// them in terms of themselves). A packager build that differs only in its
// version is the same recipe.
func TestSelectionFingerprintChangesWithPackagerIdentity(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-channel-advance", "selection-fingerprint-follows-the-package-key")

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "library"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "library", "index.ts"), []byte("export const a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := &workspace.Project{
		ID: "/library", Name: "@putnami/library", Path: "library", Version: "1.0.0", Type: "library",
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/library"),
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))

	packagerPath := t.TempDir()
	newPlan := func(version string) []*ScheduledJob {
		return []*ScheduledJob{packageTaskFixture(project, "test-provider", version, packagerPath)}
	}
	memberKey := releaseset.MemberKey("npm", "@putnami/library")
	members := map[string]*workspace.Project{memberKey: project}

	fingerprint := func(planned []*ScheduledJob, params map[string]any, versionInfo *JobContextVersion) string {
		t.Helper()
		got, err := SelectionFingerprints(ws, planned, params, cache, testProfiles(), members)
		if err != nil {
			t.Fatalf("SelectionFingerprints: %v", err)
		}
		if got[memberKey] == "" {
			t.Fatalf("no fingerprint for %s in %v", memberKey, got)
		}
		return got[memberKey]
	}

	base := fingerprint(newPlan("1.0.0"), map[string]any{"channel": "canary"}, nil)

	for name, params := range map[string]map[string]any{
		"another channel": {"channel": "next"},
		"a dry run":       {"channel": "canary", "dry-run": true},
		"no flags at all": nil,
	} {
		if got := fingerprint(newPlan("1.0.0"), params, nil); got != base {
			t.Fatalf("%s moved the selection fingerprint: %s != %s", name, got, base)
		}
	}

	// The version stamp is what the EXECUTION key carries and the selection key
	// does not: a re-run at a new stamp must not republish an unchanged member.
	stamped := fingerprint(newPlan("1.0.0"), map[string]any{"channel": "canary"}, &JobContextVersion{Base: "1.0.0", Full: "1.0.0-r42", Suffix: "r42"})
	if stamped != base {
		t.Fatalf("the version stamp moved the selection fingerprint: %s != %s", stamped, base)
	}

	// The bound release-set plan is derived FROM these fingerprints.
	withPlan := newPlan("1.0.0")
	withPlan[0].JobDef.BoundParams = extension.ParamMap{
		releaseset.ContextParamName: &releaseset.Plan{
			ProtocolVersion: distribution.ProtocolVersion, Namespace: "putnami", Channels: []string{"canary"},
		},
	}
	if got := fingerprint(withPlan, map[string]any{"channel": "canary"}, nil); got != base {
		t.Fatalf("the bound release-set plan moved the selection fingerprint: %s != %s", got, base)
	}

	// A packager build that differs from this one in its version alone is the
	// same packaging recipe, so it republishes nothing. The packaging recipe's
	// own identity DOES move it — the failure a tree hash never saw: a packager
	// upgrade, a new installed implementation, republishes every member it
	// packages.
	upgradedPath := t.TempDir()
	writeTestFile(t, filepath.Join(upgradedPath, "compiled", "packager"), "upgraded packager")
	for _, packager := range []struct {
		name      string
		plan      []*ScheduledJob
		republish bool
	}{
		{"a version-only packager build", newPlan("2.0.0"), false},
		{"a packager upgrade", []*ScheduledJob{packageTaskFixture(project, "test-provider", "2.0.0", upgradedPath)}, true},
	} {
		got := fingerprint(packager.plan, map[string]any{"channel": "canary"}, nil)
		if moved := got != base; moved != packager.republish {
			t.Fatalf("%s: selection fingerprint %s (base %s), want moved = %v", packager.name, got, base, packager.republish)
		}
	}

	// A source edit moves it too, through the same declared file inputs.
	if err := os.WriteFile(filepath.Join(root, "library", "index.ts"), []byte("export const a = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edited := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
	got, err := SelectionFingerprints(ws, newPlan("1.0.0"), map[string]any{"channel": "canary"}, edited, testProfiles(), members)
	if err != nil {
		t.Fatal(err)
	}
	if got[memberKey] == base {
		t.Fatalf("a source edit left the selection fingerprint at %s", base)
	}
}

// Each member names the exact package step that produces it. Plan order may
// never decide which recipe a member records: one project can expose several
// independently selected artifacts, including several in one ecosystem.
func TestSelectionFingerprintsFollowEachMembersDeclaredPackageStep(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "library"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := &workspace.Project{
		ID: "/library", Name: "@putnami/library", Path: "library", Type: "library",
		Metadata: releaseSetProjectMembers(t,
			releaseset.MemberDeclaration{Ecosystem: "npm", Coordinate: "@putnami/a", PackageStep: "npm-a", PublishStep: "npm-a"},
			releaseset.MemberDeclaration{Ecosystem: "npm", Coordinate: "@putnami/b", PackageStep: "npm-b", PublishStep: "npm-b"},
		),
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
	members := map[string]*workspace.Project{
		releaseset.MemberKey("npm", "@putnami/a"): project,
		releaseset.MemberKey("npm", "@putnami/b"): project,
	}
	stepA := packageTaskFixtureFor(project, "test-provider", "1.0.0", t.TempDir(), "npm-a")
	stepB := packageTaskFixtureFor(project, "test-provider", "1.0.0", t.TempDir(), "npm-b")

	forward, err := SelectionFingerprints(ws, []*ScheduledJob{stepA, stepB}, nil, cache, testProfiles(), members)
	if err != nil {
		t.Fatal(err)
	}
	reversed, err := SelectionFingerprints(ws, []*ScheduledJob{stepB, stepA}, nil, cache, testProfiles(), members)
	if err != nil {
		t.Fatal(err)
	}
	keyA := releaseset.MemberKey("npm", "@putnami/a")
	keyB := releaseset.MemberKey("npm", "@putnami/b")
	if forward[keyA] == "" || forward[keyB] == "" || forward[keyA] == forward[keyB] {
		t.Fatalf("fingerprints = %v, want one non-empty identity per package step", forward)
	}
	if forward[keyA] != reversed[keyA] || forward[keyB] != reversed[keyB] {
		t.Fatalf("plan order changed member routing: forward=%v reversed=%v", forward, reversed)
	}
}

// A member whose package task the run did not plan cannot be keyed, and the
// error names the project rather than producing a silent zero.
func TestSelectionFingerprintsRequireThePackageTaskOfTheDeclaringPublisher(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{
		ID: "/library", Name: "@putnami/library", Path: "library", Type: "library",
		Metadata: releaseSetProjectMetadata(t, "npm", "@putnami/library"),
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
	members := map[string]*workspace.Project{releaseset.MemberKey("npm", "@putnami/library"): project}

	// A package task owned by the profile owner is not automatically this
	// member's packager: the metadata envelope names the actual publisher.
	foreign := packageTaskFixture(project, "go-owner", "1.0.0", t.TempDir())
	_, err := SelectionFingerprints(ws, []*ScheduledJob{foreign}, nil, cache, testProfiles(), members)
	if err == nil || !strings.Contains(err.Error(), `"test-provider"`) || !strings.Contains(err.Error(), "/library") {
		t.Fatalf("error = %v, want it to name the declaring publisher and the project", err)
	}
}

func TestSelectionFingerprintsUseTheDeclaredPackagePublisher(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "release-set-channel-advance", "distinct-package-provider-fingerprint-follows-real-package-key")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "library"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "library", "main.go"), []byte("package library\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := &workspace.Project{
		ID: "/library", Name: "putnami/cloud", Path: "library", Type: "library",
		Metadata: releaseSetProjectMembers(t, releaseset.MemberDeclaration{
			Ecosystem: "npm", Coordinate: "@putnami/library",
			PackagePublisher: "go-packager", PackageStep: "archive", PublishStep: "archive",
		}),
	}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "putnami"}, []*workspace.Project{project})
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
	members := map[string]*workspace.Project{releaseset.MemberKey("npm", "@putnami/library"): project}
	packageJob := packageTaskFixtureFor(project, "go-packager", "1.0.0", t.TempDir(), "archive")

	got, err := SelectionFingerprints(ws, []*ScheduledJob{packageJob}, nil, cache, testProfiles(), members)
	if err != nil {
		t.Fatal(err)
	}
	memberKey := releaseset.MemberKey("npm", "@putnami/library")
	base := got[memberKey]
	if base == "" {
		t.Fatalf("fingerprints = %v, want the distinct package provider's task identity", got)
	}
	// An upgrade installs another implementation of the package provider.
	upgradedPath := t.TempDir()
	writeTestFile(t, filepath.Join(upgradedPath, "compiled", "packager"), "upgraded packager")
	upgraded := packageTaskFixtureFor(project, "go-packager", "2.0.0", upgradedPath, "archive")
	got, err = SelectionFingerprints(ws, []*ScheduledJob{upgraded}, nil, cache, testProfiles(), members)
	if err != nil {
		t.Fatal(err)
	}
	if got[memberKey] == base {
		t.Fatalf("package provider upgrade left the routed fingerprint at %s", base)
	}
	if err := os.WriteFile(filepath.Join(root, "library", "main.go"), []byte("package library\n\nconst Version = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	edited := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "edited-store")))
	got, err = SelectionFingerprints(ws, []*ScheduledJob{packageJob}, nil, edited, testProfiles(), members)
	if err != nil {
		t.Fatal(err)
	}
	if got[memberKey] == base {
		t.Fatalf("source edit left the routed fingerprint at %s", base)
	}

	foreign := packageTaskFixtureFor(project, "test-provider", "1.0.0", t.TempDir(), "archive")
	_, err = SelectionFingerprints(ws, []*ScheduledJob{foreign}, nil, cache, testProfiles(), members)
	if err == nil || !strings.Contains(err.Error(), `"go-packager"`) {
		t.Fatalf("wrong package publisher error = %v", err)
	}
}

// packageTaskFixture is shaped like the real npm/go packagers: it has a stable
// task identity but is deliberately not cache-restorable because its package
// command also owns a shared metadata merge point. Selection must still be
// able to fingerprint it.
func packageTaskFixture(project *workspace.Project, owner, version, path string) *ScheduledJob {
	return packageTaskFixtureFor(project, owner, version, path, "npm")
}

func packageTaskFixtureFor(project *workspace.Project, owner, version, path, task string) *ScheduledJob {
	description := &extension.ExtensionDescription{
		Name: owner, Version: version, Path: path,
		Tasks: map[string]extension.TaskDefinition{task: {Declares: &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{task: {
				Kind: extension.OutputKindDirectory,
				Root: extension.OutputRootCommandOutput,
				Path: task,
			}},
		}}},
	}
	return &ScheduledJob{
		Project:   project,
		Extension: description,
		Step:      &extension.PipelineStep{Task: task},
		JobDef: &extension.JobDefinition{
			Name: "package~" + task, CommandName: "package", ExtensionName: owner, Cache: true,
			TaskCachePolicy: &extension.TaskCachePolicy{
				Key: &extension.TaskCacheKey{Params: []string{"channel", "dry-run"}},
			},
		},
	}
}
