// The .gen build stamp, the generation cache key, and what each one is allowed
// to invalidate.
//
// Three guarantees are pinned here, and they pull in opposite directions, which
// is why they are tested together on one fixture shape rather than separately:
//
//   - CONTENT-KEYED GENERATION — a new Git SHA must NOT move a generation key, nor any downstream
//     key that mixes it in. Content decides generation; the commit does not.
//   - CURRENT-REVISION STAMP — <project>/.gen/version.json must report the CURRENT revision after
//     a cache hit, on every restore leg (local, remote, coalesced), because
//     deploy/package/publish read it as the release identity. Because generation is
//     content-keyed, the
//     entry that hit may legitimately have been produced at an older revision,
//     so this is now a property of the RESTORE, not of the key.
//   - STABLE KEY INPUTS — no cache key may hash a value the run itself mutates between
//     lookup and store. The restore clobbers the stamp and the re-stamp puts it
//     back; the run must end holding the bytes its keys were hashed on, or a
//     project that takes one miss can never hit again.
package jobs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// stampCommitA and stampCommitB are two revisions of the same tree: identical
// content, different identity. Everything in this file is the question "which
// of these two may a given key or file legitimately depend on".
func stampCommitA() RunVersions {
	return rootLineVersions(&JobContextVersion{
		Base: "1.0.0", Full: "1.0.0-a1b2c3d4", Suffix: "a1b2c3d4",
		SHA: "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678", Branch: "main",
	})
}

func stampCommitB() RunVersions {
	return rootLineVersions(&JobContextVersion{
		Base: "1.0.0", Full: "1.0.0-f6e5d4c3", Suffix: "f6e5d4c3",
		SHA: "f6e5d4c3b2a1908172635445362718091a2b3c4d", Branch: "main",
	})
}

// genStampJob is a generation task shaped like the real build-generate: it owns
// the project-rooted .gen subtree — the tree that carries the build stamp — and
// keys on project sources only, exactly as the Go and TypeScript manifests
// declare (inputs: sources/modules/config; nothing under .gen).
func genStampJob() *ScheduledJob {
	job := declaredJob("build~generate", "generate", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"gen": dirDeclaration(extension.OutputRootProject, ".gen", false),
		},
	}, extension.ResourceRef{ID: genResourceID})
	job.JobDef.FilePatterns = []string{"src.txt"}
	// A declared base version, so the stamp's `version` is the deployable
	// "<base>-<suffix>" identity a release consumer reads rather than the 0.0.0
	// fallback.
	job.Project.Version = "1.0.0"
	return job
}

// readVersionStamp decodes a project's build stamp.
func readVersionStamp(t *testing.T, projectRoot string) VersionInfo {
	t.Helper()
	var stamp VersionInfo
	raw := readFileAt(t, filepath.Join(projectRoot, filepath.FromSlash(versionStampRelPath)))
	if err := json.Unmarshal([]byte(raw), &stamp); err != nil {
		t.Fatalf("decode build stamp %s: %v", raw, err)
	}
	return stamp
}

// assertStampReports fails unless the stamp on disk names the given revision.
func assertStampReports(t *testing.T, leg, projectRoot string, want RunVersions) {
	t.Helper()
	stamp := readVersionStamp(t, projectRoot)
	expected := want[""]
	if stamp.Version != expected.Full || stamp.SHA != expected.SHA {
		t.Fatalf("%s: .gen/version.json reports version=%q sha=%q, want %q/%q — a deploy reading it ships the wrong revision",
			leg, stamp.Version, stamp.SHA, expected.Full, expected.SHA)
	}
}

// mergeStampField merges one extension-owned key into a project's stamp, the way
// build.UpdateVersionContentHash and dockerpublish.mergeVersionJSON do: read the
// whole document, set one key, write it back.
func mergeStampField(t *testing.T, projectRoot, key string, value any) {
	t.Helper()
	path := filepath.Join(projectRoot, filepath.FromSlash(versionStampRelPath))
	var doc map[string]any
	if err := json.Unmarshal([]byte(readFileAt(t, path)), &doc); err != nil {
		t.Fatalf("decode stamp for merge: %v", err)
	}
	doc[key] = value
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, path, string(data)+"\n")
}

// assertStampCarries fails unless an extension-owned stamp field survived. The
// scheduler rewrites the stamp's identity; it must not take the rest of the
// document with it.
func assertStampCarries(t *testing.T, leg, projectRoot, key string, want any) {
	t.Helper()
	path := filepath.Join(projectRoot, filepath.FromSlash(versionStampRelPath))
	var doc map[string]any
	if err := json.Unmarshal([]byte(readFileAt(t, path)), &doc); err != nil {
		t.Fatalf("decode stamp: %v", err)
	}
	if doc[key] != want {
		t.Fatalf("%s: .gen/version.json %q = %v, want %v — the re-stamp destroyed a field the scheduler does not own",
			leg, key, doc[key], want)
	}
}

// stampKeyInput is the cache-key contribution of the build stamp, computed the
// way a task whose globs reach it (TypeScript lint's **/*.json) contributes it.
// A FRESH CacheManager each call defeats the per-session memo, so this really
// re-reads the file rather than replaying an earlier answer.
func stampKeyInput(t *testing.T, ws *workspace.Workspace, projectPath string) string {
	t.Helper()
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	digest, err := cm.HashFiles(filepath.Join(ws.Root, projectPath), []string{versionStampRelPath}, store.ProjectConfigScope{})
	if err != nil {
		t.Fatalf("hash build stamp: %v", err)
	}
	return digest
}

// producedContentHash is the value the fixture's generation run merges into the
// stamp, standing for the TypeScript build-generate task's real
// build.UpdateVersionContentHash — a field the SCHEDULER does not own and the
// running app reads back through getBuildInfo() to namespace its disk cache.
const producedContentHash = "sha256:c0ffee0011223344"

// primeGenerationEntry publishes a generation entry produced at `at`: the
// captured .gen carries THAT revision's stamp — identity, plus the extension's
// own contentHash — which is what a later hit restores over the consumer's tree.
func primeGenerationEntry(
	t *testing.T,
	f *captureFixture,
	job *ScheduledJob,
	at RunVersions,
	buildTime string,
) string {
	t.Helper()
	projectRoot := filepath.Join(f.ws.Root, captureTestProject)
	writeFileAt(t, filepath.Join(projectRoot, "src.txt"), "stable\n")
	// A producing run refreshes its own stamp before executing (refreshVersionFile)
	// and the generator then merges its own field into the same document.
	generateVersionFilesAt(f.ws, []*ScheduledJob{job}, at, buildTime, false)
	mergeStampField(t, projectRoot, "contentHash", producedContentHash)
	writeFileAt(t, filepath.Join(projectRoot, ".gen", "openapi.json"), `{"openapi":"3.1.0"}`)
	assertStampReports(t, "producer", projectRoot, at)

	hash, err := computeJobCacheHash(f.ws, job, nil, at, f.cache, nil)
	if err != nil {
		t.Fatalf("compute generation key: %v", err)
	}
	if !f.sched.storeDeclaredCapture(job, &JobResult{Status: "success", Duration: 2 * time.Second}, hash) {
		t.Fatal("the producing run published no generation entry")
	}
	return hash
}

// ---------------------------------------------------------------------------
// acceptance: a changed leaf does not invalidate an unchanged sibling leaf
// ---------------------------------------------------------------------------

// TestPrecomputeKeys_UnchangedLeafSurvivesASiblingLeafChange is the issue's
// two-leaf fixture. Two library projects and an app that depends on both are all
// in one plan; leaf A changes and the commit advances. Leaf B's generation,
// test, describe and compile entries must be BYTE-IDENTICAL — its content did
// not move, and the commit is not allowed to speak for it — while leaf A and the
// app that consumes it re-key.
//
// The build stamps are seeded exactly as Scheduler.prepareRun does, before any
// key is computed, so the fixture also proves the seeded stamp itself does not
// leak the revision into keys that never declared it.
func TestPrecomputeKeys_UnchangedLeafSurvivesASiblingLeafChange(t *testing.T) {
	ws := makeExecutorTestWorkspace(t)
	ext := &extension.ExtensionDescription{
		Name: "@test/ext",
		Path: t.TempDir(),
		Tasks: map[string]extension.TaskDefinition{
			"generate": {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
				"gen": dirDeclaration(extension.OutputRootProject, ".gen", false),
			}}},
			"compile":  {Declares: &extension.TaskDeclaration{}},
			"test":     {Declares: &extension.TaskDeclaration{}},
			"describe": {Declares: &extension.TaskDeclaration{}},
		},
	}

	// projectJobs builds one project's generate + the three tasks that hang off
	// it, plus the extra upstream keys an app inherits from its dependencies.
	projectJobs := func(name string, upstream []string) (map[string]*ScheduledJob, []*ScheduledJob) {
		project := &workspace.Project{ID: "/" + name, Name: name, Path: name}
		writeFileAt(t, filepath.Join(ws.Root, name, "src.txt"), "v1 of "+name+"\n")

		generate := &ScheduledJob{
			Project: project, Extension: ext,
			JobDef: &extension.JobDefinition{
				Name: "build~generate", ExtensionName: "@test/ext", Cache: true,
				FilePatterns: []string{"src.txt"},
				Writes:       []extension.ResourceRef{{ID: genResourceID}},
			},
			Step:      &extension.PipelineStep{Task: "generate"},
			DependsOn: upstream,
		}
		byTask := map[string]*ScheduledJob{"generate": generate}
		all := []*ScheduledJob{generate}
		for _, task := range []string{"compile", "test", "describe"} {
			job := &ScheduledJob{
				Project: project, Extension: ext,
				JobDef: &extension.JobDefinition{
					Name: "build~" + task, ExtensionName: "@test/ext", Cache: true,
					FilePatterns: []string{"src.txt"},
				},
				Step:      &extension.PipelineStep{Task: task},
				DependsOn: []string{generate.Key()},
			}
			byTask[task] = job
			all = append(all, job)
		}
		return byTask, all
	}

	leafA, jobsA := projectJobs("leaf-a", nil)
	leafB, jobsB := projectJobs("leaf-b", nil)
	app, jobsApp := projectJobs("app", []string{leafA["generate"].Key(), leafB["generate"].Key()})
	planned := append(append(append([]*ScheduledJob{}, jobsA...), jobsB...), jobsApp...)

	// A fresh CacheManager per invocation: lookupFileHash memoizes per process,
	// and the second gate run really is a second CLI invocation.
	keysAt := func(version RunVersions, buildTime string) map[string]string {
		t.Helper()
		cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
		preserveMatchingVersionFiles(ws, planned, version, buildTime)
		keys, err := PrecomputeKeys(ws, planned, nil, version, cm, CacheBypass{})
		if err != nil {
			t.Fatalf("precompute keys: %v", err)
		}
		return keys
	}

	before := keysAt(stampCommitA(), "2026-07-30T10:00:00Z")
	// The narrow change: one leaf's source, plus the new commit every project in
	// the plan is now built at.
	writeFileAt(t, filepath.Join(ws.Root, "leaf-a", "src.txt"), "v2 of leaf-a\n")
	after := keysAt(stampCommitB(), "2026-07-31T09:00:00Z")

	for _, task := range []string{"generate", "compile", "test", "describe"} {
		key := leafB[task].Key()
		if before[key] != after[key] {
			t.Errorf("unchanged leaf-b %s re-keyed across a metadata-only commit (%s → %s)",
				task, before[key], after[key])
		}
	}
	// The controls: without them the assertion above would also pass if keys were
	// blind to everything.
	if before[leafA["generate"].Key()] == after[leafA["generate"].Key()] {
		t.Error("leaf-a generate key did not move although its source changed")
	}
	for _, task := range []string{"compile", "test", "describe"} {
		key := leafA[task].Key()
		if before[key] == after[key] {
			t.Errorf("leaf-a %s did not inherit its own generate key change", task)
		}
	}
	if before[app["generate"].Key()] == after[app["generate"].Key()] {
		t.Error("the app did not inherit the changed leaf's key through its dependency")
	}
}

// ---------------------------------------------------------------------------
// acceptance: the stamp reports the current SHA after a LOCAL cache hit
// ---------------------------------------------------------------------------

// TestSchedulerGenerationLocalHitStampsTheCurrentRevision is the
// current-revision-stamp guarantee restated for a content-keyed generation entry: the entry was produced at
// commit A, is legitimately reused at commit B, and the restore must leave the
// stamp naming B before anything downstream reads it.
//
// All three parts of the fix are load-bearing here. Restore the commit to the
// key and the run misses instead of hitting; drop the re-stamp and the workspace
// is left holding commit A's identity — which is exactly the stale deploy bug
// reported; drop the merge from the stamp writer and the re-stamp destroys the
// generator's own `contentHash`, which after this issue nothing re-adds because
// generate no longer re-runs on a new SHA.
func TestSchedulerGenerationLocalHitStampsTheCurrentRevision(t *testing.T) {
	job := genStampJob()
	f := newCaptureFixture(t, job)
	projectRoot := filepath.Join(f.ws.Root, captureTestProject)

	produced := stampCommitA()
	current := stampCommitB()
	hash := primeGenerationEntry(t, f, job, produced, "2026-07-30T10:00:00Z")

	// A second checkout of the same content at the newer commit.
	currentHash, err := computeJobCacheHash(f.ws, job, nil, current, f.cache, nil)
	if err != nil {
		t.Fatalf("compute generation key at the current commit: %v", err)
	}
	if currentHash != hash {
		t.Fatalf("the generation key moved with the commit (%s → %s); an unchanged tree cannot reuse its own generation",
			hash, currentHash)
	}

	// The same worktree, one commit later: the fixture's scheduler is
	// reconfigured rather than rebuilt, so the plan and the store are the ones
	// the entry was published against.
	f.sched.cfg.VersionInfo = current
	result := f.sched.Run(context.Background())

	got := result.Results[job.Key()]
	if got == nil || !got.CacheHit {
		t.Fatalf("result = %+v, want a task-owned cache hit at the new commit", got)
	}
	// The restore really happened: the entry's payload is on disk.
	if content := readFileAt(t, filepath.Join(projectRoot, ".gen", "openapi.json")); content != `{"openapi":"3.1.0"}` {
		t.Fatalf(".gen/openapi.json = %q, want the restored generation output", content)
	}
	assertStampReports(t, "local hit", projectRoot, current)
	// The scheduler owns the identity fields of this document and nothing else.
	// contentHash came out of the entry; the re-stamp updated the identity around
	// it and left it standing, so the workload's disk-cache namespace is unchanged.
	assertStampCarries(t, "local hit", projectRoot, "contentHash", producedContentHash)
}

// TestSchedulerGenerationLocalHitLeavesTheStampKeyInputUnchanged is the
// stable-key-inputs guarantee on the
// restore path. prepareRun seeds the stamp BEFORE any key is computed, so its
// tree-only digest is a keyed input of every task whose globs reach it
// (TypeScript lint's **/*.json). The gen restore then overwrites that file with
// the producing run's bytes, and the re-stamp rewrites its identity. The digest
// reads neither revision, so the seed for a new revision leaves the keyed input
// where the producing run left it, and the run ends holding the value its keys
// were computed from. A project that took one miss can hit again.
func TestSchedulerGenerationLocalHitLeavesTheStampKeyInputUnchanged(t *testing.T) {
	job := genStampJob()
	f := newCaptureFixture(t, job)
	projectRoot := filepath.Join(f.ws.Root, captureTestProject)

	produced := stampCommitA()
	current := stampCommitB()
	primeGenerationEntry(t, f, job, produced, "2026-07-30T10:00:00Z")
	producedInput := stampKeyInput(t, f.ws, captureTestProject)

	// What prepareRun does: seed the current identity, then key on it.
	preserveMatchingVersionFiles(f.ws, []*ScheduledJob{job}, current, "2026-07-31T09:00:00Z")
	assertStampReports(t, "seed", projectRoot, current)
	keyedInput := stampKeyInput(t, f.ws, captureTestProject)
	if keyedInput != producedInput {
		t.Fatalf("seeding a new revision over an unchanged tree moved the stamp's key input: %s → %s", producedInput, keyedInput)
	}

	f.sched.cfg.VersionInfo = current
	if got := f.sched.Run(context.Background()).Results[job.Key()]; got == nil || !got.CacheHit {
		t.Fatalf("result = %+v, want a task-owned cache hit", got)
	}

	if after := stampKeyInput(t, f.ws, captureTestProject); after != keyedInput {
		t.Fatalf("the run mutated a value its own keys hashed: stamp digest %s → %s", keyedInput, after)
	}

	// The control: the digest still reads the stamp. Without it the equalities
	// above would also hold for a digest blind to the whole document.
	mergeStampField(t, projectRoot, "contentHash", "sha256:0000000000000000")
	if changed := stampKeyInput(t, f.ws, captureTestProject); changed == keyedInput {
		t.Fatal("a new contentHash kept the stamp's key input; the digest no longer reads the stamp")
	}
}

// TestGenerationCacheHitDoesNotChurnTheBuildTime is the other half of the
// stable-key-inputs guarantee: the
// re-stamp must not rewrite a stamp whose identity already matches, or an all-hit
// run would replace the producing build's buildTime on every invocation. The
// digest would survive that (buildTime is blanked before hashing) but the file
// would churn, and the "retain the metadata of the build that produced these
// artifacts" contract preserveMatchingVersionFiles states would be gone.
func TestGenerationCacheHitDoesNotChurnTheBuildTime(t *testing.T) {
	job := genStampJob()
	f := newCaptureFixture(t, job)
	current := stampCommitB()
	primeGenerationEntry(t, f, job, current, "2026-07-30T10:00:00Z")

	f.sched.cfg.VersionInfo = current
	if got := f.sched.Run(context.Background()).Results[job.Key()]; got == nil || !got.CacheHit {
		t.Fatalf("result = %+v, want a task-owned cache hit", got)
	}

	if stamp := readVersionStamp(t, filepath.Join(f.ws.Root, captureTestProject)); stamp.BuildTime != "2026-07-30T10:00:00Z" {
		t.Fatalf("buildTime = %q, want the producing build's %q preserved on an all-hit run",
			stamp.BuildTime, "2026-07-30T10:00:00Z")
	}
}

// ---------------------------------------------------------------------------
// acceptance: the same behavior over the REMOTE cache
// ---------------------------------------------------------------------------

// TestRemoteGenerationHitStampsTheCurrentRevision is the remote leg of the
// matrix: two machines, a real provider exchange, and the consumer starting from
// an EMPTY local store. It pins both halves at once — the key must be identical
// on the two machines (or nothing could be shared across a commit at all), and
// the restored stamp must name the consumer's revision, not the producer's.
func TestRemoteGenerationHitStampsTheCurrentRevision(t *testing.T) {
	server := newFakeCacheServer()
	produced := stampCommitA()
	current := stampCommitB()

	producer, producerRemote := genStampRemoteFixture(t, server, produced)
	producerJob := genStampJob()
	hash := primeGenerationEntry(t, producer, producerJob, produced, "2026-07-30T10:00:00Z")
	producerRemote.UploadTaskEntry(t.Context(), hash, producerJob, producer.cache)
	producerRemote.DrainUploads()
	if !server.has(store.RemoteTaskEntryKey(hash)) {
		t.Fatalf("the producing machine did not share its generation entry under %s", store.RemoteTaskEntryKey(hash))
	}

	consumer, _ := genStampRemoteFixture(t, server, current)
	consumerJob := genStampJob()
	consumerRoot := filepath.Join(consumer.ws.Root, captureTestProject)
	writeFileAt(t, filepath.Join(consumerRoot, "src.txt"), "stable\n")
	consumer.sched.versionBuildTime = "2026-07-31T09:00:00Z"
	// prepareRun's seed, before any key is computed.
	preserveMatchingVersionFiles(consumer.ws, []*ScheduledJob{consumerJob}, current, consumer.sched.versionBuildTime)

	if entry, err := consumer.cache.LookupTaskEntry(hash); err != nil || entry != nil {
		t.Fatalf("the consuming machine must start from an empty store, got %+v (err=%v)", entry, err)
	}
	result := consumer.runDeclaredLookup(t, consumerJob, hash)
	if result == nil || result.Reuse != ReuseRemoteCache {
		t.Fatalf("consumer leg = %+v, want a remote-cache hit at the newer commit", result)
	}
	if content := readFileAt(t, filepath.Join(consumerRoot, ".gen", "openapi.json")); content != `{"openapi":"3.1.0"}` {
		t.Fatalf(".gen/openapi.json = %q, want the remotely restored generation output", content)
	}
	assertStampReports(t, "remote hit", consumerRoot, current)
	// The consuming machine's own seed never had a contentHash; it arrived with
	// the entry and describes exactly the bytes that were restored, so carrying it
	// through is correct as well as necessary.
	assertStampCarries(t, "remote hit", consumerRoot, "contentHash", producedContentHash)
}

// genStampRemoteFixture is matrixFixture for this file's generation job: a
// workspace whose scheduler talks to `server` through a provider-backed remote
// cache, configured to build at `version`.
func genStampRemoteFixture(t *testing.T, server *fakeCacheServer, version RunVersions) (*captureFixture, *RemoteCache) {
	t.Helper()
	enableProviderRemoteCache(t)
	useSharedFakeProvider(t, server)

	f := newCaptureFixture(t, genStampJob())
	f.sched.cfg.VersionInfo = version
	remote, notice := LoadRemoteCache(context.Background(), f.ws.Root,
		[]*extension.ExtensionDescription{fakeProviderExtension(t, nil)}, nil, store.CacheTrustAny)
	if remote == nil || !remote.isProviderBacked() {
		t.Fatalf("expected a provider-backed remote cache, got %+v (notice %q)", remote, notice)
	}
	t.Cleanup(remote.Close)
	f.sched.setRemoteCache(remote)
	return f, remote
}

// ---------------------------------------------------------------------------
// the predicate that decides whether a restore touched the stamp
// ---------------------------------------------------------------------------

func TestRestoreReplacesVersionStamp(t *testing.T) {
	entry := func(outputs ...store.TaskEntryOutput) *store.TaskEntry {
		return &store.TaskEntry{Outputs: outputs}
	}
	dirOut := func(root, path, state string) store.TaskEntryOutput {
		return store.TaskEntryOutput{
			ID: "out", Kind: extension.OutputKindDirectory, Root: root, Path: path, State: state,
		}
	}
	fileOut := func(root, path, state string) store.TaskEntryOutput {
		return store.TaskEntryOutput{
			ID: "out", Kind: extension.OutputKindFile, Root: root, Path: path, State: state,
		}
	}

	tests := []struct {
		name  string
		entry *store.TaskEntry
		want  bool
	}{
		{"nil entry", nil, false},
		{"project .gen", entry(dirOut(extension.OutputRootProject, ".gen", store.TaskOutputPresent)), true},
		{"whole project root", entry(dirOut(extension.OutputRootProject, ".", store.TaskOutputPresent)), true},
		{"empty .gen leaves the destination alone",
			entry(dirOut(extension.OutputRootProject, ".gen", store.TaskOutputEmpty)), false},
		{"a sibling generated tree", entry(dirOut(extension.OutputRootProject, ".generated", store.TaskOutputPresent)), false},
		{"a subtree inside .gen", entry(dirOut(extension.OutputRootProject, ".gen/clientgen", store.TaskOutputPresent)), false},
		{"a project client directory", entry(dirOut(extension.OutputRootProject, "internal/api-client", store.TaskOutputPresent)), false},
		{"another root's .gen", entry(dirOut(extension.OutputRootWorkspace, ".gen", store.TaskOutputPresent)), false},
		{"a file output elsewhere in the project", entry(fileOut(
			extension.OutputRootProject, "coverage.json", store.TaskOutputPresent)), false},
		// A declaration that names the stamp directly overwrites exactly the bytes
		// a .gen tree swap would, so it has to be re-stamped for the same reason.
		// Nothing in the manifest forbids it; only this predicate stands between
		// such a declaration and a silent return of the stale stamp.
		{"a file output recorded AT the stamp", entry(fileOut(
			extension.OutputRootProject, versionStampRelPath, store.TaskOutputPresent)), true},
		{"the stamp path in unnormalized form", entry(fileOut(
			extension.OutputRootProject, "./.gen/version.json", store.TaskOutputPresent)), true},
		{"an absent stamp file leaves the destination alone", entry(fileOut(
			extension.OutputRootProject, versionStampRelPath, store.TaskOutputEmpty)), false},
		{"another root's stamp file", entry(fileOut(
			extension.OutputRootWorkspace, versionStampRelPath, store.TaskOutputPresent)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := restoreReplacesVersionStamp(tt.entry); got != tt.want {
				t.Errorf("restoreReplacesVersionStamp() = %v, want %v", got, tt.want)
			}
		})
	}
}
