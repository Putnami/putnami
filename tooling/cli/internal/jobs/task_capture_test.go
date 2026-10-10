package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dockerpublish"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

const captureTestProject = "proj"

type captureFixture struct {
	ws    *workspace.Workspace
	cache *store.CacheManager
	sched *Scheduler
}

func newCaptureFixture(t *testing.T, jobs ...*ScheduledJob) *captureFixture {
	t.Helper()
	ws := makeExecutorTestWorkspace(t)
	cm := store.NewCacheManager(store.NewLocalStore(filepath.Join(ws.Root, ".putnami", "store")))
	return &captureFixture{
		ws:    ws,
		cache: cm,
		sched: newScheduler(ws, jobs, nil, SchedulerConfig{MaxParallel: 1}, &mockRenderer{}, cm),
	}
}

// declaredJob builds a plan node whose manifest task carries a v3 declaration.
// The declaration hangs off the extension's task table (never off JobDefinition),
// exactly as extension.Resolve leaves it, so the tests exercise the same lookup
// the scheduler does.
func declaredJob(name, task string, declaration *extension.TaskDeclaration, writes ...extension.ResourceRef) *ScheduledJob {
	return &ScheduledJob{
		Project: &workspace.Project{ID: "/" + captureTestProject, Name: captureTestProject, Path: captureTestProject},
		Extension: &extension.ExtensionDescription{
			Name:  "@test/ext",
			Tasks: map[string]extension.TaskDefinition{task: {Declares: declaration}},
		},
		JobDef: &extension.JobDefinition{Name: name, Cache: true, Writes: writes},
		Step:   &extension.PipelineStep{Task: task},
	}
}

func dirDeclaration(root, path string, optional bool) extension.DeclaredOutput {
	return extension.DeclaredOutput{Kind: extension.OutputKindDirectory, Root: root, Path: path, OptionalEmpty: optional}
}

func fileDeclaration(root, path string, optional bool) extension.DeclaredOutput {
	return extension.DeclaredOutput{Kind: extension.OutputKindFile, Root: root, Path: path, OptionalEmpty: optional}
}

func writeFileAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileAt(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// hitOrFail runs the full read side of a declared-capture job and fails unless
// it produced a cache hit.
func (f *captureFixture) hitOrFail(t *testing.T, job *ScheduledJob, hash string) *JobResult {
	t.Helper()
	entry, err := f.cache.LookupTaskEntry(hash)
	if err != nil || entry == nil {
		t.Fatalf("no task-owned entry published for %s: entry=%v err=%v", hash, entry, err)
	}
	var mu sync.Mutex
	result := f.sched.restoreDeclaredCacheHit(t.Context(), job, hash, entry, &mu, map[string]string{})
	if result == nil {
		t.Fatal("declared restore returned no result for a published entry")
	}
	return result
}

func (f *captureFixture) commandDir(command string) string {
	return filepath.Join(f.ws.Root, ".putnami", "out", captureTestProject, command)
}

func capturedResult(data map[string]any) *JobResult {
	return &JobResult{Status: "success", Data: data}
}

const captureHashA = "aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee"
const captureHashB = "bb11223344556677889900aabbccddeeff00112233445566778899aabbccddee"

// ---------------------------------------------------------------------------
// cold → ingest → hit → materialize, once per declaration root
// ---------------------------------------------------------------------------

// A declared output is captured from its real location and restored to it, for
// every root the contract defines. The restore is checked against a MUTATED
// tree, not an absent one: a test that only deletes the output cannot tell a
// real restore from a no-op that happened to leave correct bytes behind.
func TestDeclaredCaptureRoundTripPerRoot(t *testing.T) {
	cases := []struct {
		name    string
		root    string
		relPath string
		// dest resolves the output's real absolute location.
		dest func(f *captureFixture) string
	}{
		{
			name:    "command-output",
			root:    extension.OutputRootCommandOutput,
			relPath: "lib",
			dest:    func(f *captureFixture) string { return filepath.Join(f.commandDir("build"), "lib") },
		},
		{
			name:    "project",
			root:    extension.OutputRootProject,
			relPath: ".gen",
			dest:    func(f *captureFixture) string { return filepath.Join(f.ws.Root, captureTestProject, ".gen") },
		},
		{
			name:    "workspace",
			root:    extension.OutputRootWorkspace,
			relPath: "dist/shared",
			dest:    func(f *captureFixture) string { return filepath.Join(f.ws.Root, "dist", "shared") },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := declaredJob("build~emit", "emit", &extension.TaskDeclaration{
				Outputs: map[string]extension.DeclaredOutput{
					"out": dirDeclaration(tc.root, tc.relPath, false),
				},
			})
			f := newCaptureFixture(t, job)
			if !usesDeclaredCapture(job) {
				t.Fatal("job with a covering declaration did not take the declared path")
			}

			// Cold: the task writes its declared output where it really lives.
			dest := tc.dest(f)
			writeFileAt(t, filepath.Join(dest, "index.js"), "cold\n")
			writeFileAt(t, filepath.Join(dest, "nested", "chunk.js"), "chunk\n")

			if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
				t.Fatal("declared capture did not publish an entry")
			}

			// The published entry records the declaration, not a directory walk.
			entry, err := f.cache.LookupTaskEntry(captureHashA)
			if err != nil || entry == nil {
				t.Fatalf("lookup task entry: %v %v", entry, err)
			}
			recorded, ok := entry.Output("out")
			if !ok || !recorded.Present() {
				t.Fatalf("entry did not record the declared output: %+v", entry.Outputs)
			}
			if recorded.Root != tc.root || recorded.Path != tc.relPath || recorded.Files != 2 {
				t.Fatalf("recorded output = %+v, want root %q path %q with 2 files", recorded, tc.root, tc.relPath)
			}
			// No legacy entry exists for this key: the two models are disjoint, and
			// writing both would double the store for one result.
			if legacy, err := f.cache.Lookup(captureHashA); err != nil || legacy != nil {
				t.Fatalf("declared capture also wrote a legacy entry: %v (err=%v)", legacy, err)
			}

			// Warm: the tree is WRONG on disk (stale bytes plus a file the cold run
			// never produced). A correct restore replaces it with the captured tree.
			writeFileAt(t, filepath.Join(dest, "index.js"), "stale\n")
			writeFileAt(t, filepath.Join(dest, "leftover.js"), "gone on the next build\n")

			result := f.hitOrFail(t, job, captureHashA)
			if !result.Reuse.CacheHit() {
				t.Fatalf("restored result reuse = %v, want a cache hit", result.Reuse)
			}
			if got := readFileAt(t, filepath.Join(dest, "index.js")); got != "cold\n" {
				t.Errorf("restored index.js = %q, want the captured bytes", got)
			}
			if got := readFileAt(t, filepath.Join(dest, "nested", "chunk.js")); got != "chunk\n" {
				t.Errorf("restored nested/chunk.js = %q", got)
			}
			if _, err := os.Stat(filepath.Join(dest, "leftover.js")); err == nil {
				t.Error("restore left a file the captured tree does not contain")
			}
		})
	}
}

// A declared FILE output round-trips too, and its restore replaces whatever is
// at the path rather than merging into it.
func TestDeclaredCaptureRoundTripsFileOutput(t *testing.T) {
	job := declaredJob("test~run", "run", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"coverage": fileDeclaration(extension.OutputRootCommandOutput, "lcov.info", true),
		},
	})
	f := newCaptureFixture(t, job)

	coverage := filepath.Join(f.commandDir("test"), "lcov.info")
	writeFileAt(t, coverage, "TN:\nSF:a.ts\n")
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("declared capture did not publish an entry")
	}

	writeFileAt(t, coverage, "stale\n")
	f.hitOrFail(t, job, captureHashA)
	if got := readFileAt(t, coverage); got != "TN:\nSF:a.ts\n" {
		t.Errorf("restored coverage = %q", got)
	}
}

// ---------------------------------------------------------------------------
// ceded subpaths
// ---------------------------------------------------------------------------

// cededGenJobs builds the real shape the Go extension declares: one task that
// owns <project>/.gen except the subpaths a LATER task produces, and one that
// owns those subpaths at the documented contract paths inside it.
//
// Both halves are the manifest's, verbatim in shape: the converged contract tree
// (.gen/schema) and the migration bundle are ceded by build-generate and claimed
// by build-describe, while build-generate keeps its own pre-merge mirror at
// .gen/generate-staging — a path it still owns, which is how the hand-off
// between the two tasks survives a generate cache hit.
func cededGenJobs() (generate, describe *ScheduledJob) {
	generate = declaredJob("build~generate", "generate", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"gen": {
				Kind:     extension.OutputKindDirectory,
				Root:     extension.OutputRootProject,
				Path:     ".gen",
				Excludes: []string{".gen/migration-bundle", ".gen/schema"},
			},
		},
	})
	describe = declaredJob("build~describe", "describe", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"migrationBundle": dirDeclaration(extension.OutputRootProject, ".gen/migration-bundle", true),
			"schema":          dirDeclaration(extension.OutputRootProject, ".gen/schema", true),
		},
	})
	return generate, describe
}

// THE ROUND TRIP the issues ask for: a project whose build writes .gen, then a
// migration bundle and a CONVERGED contract tree inside it, `.gen` deleted, both
// tasks served from cache, and both back on disk with the bytes describe
// produced — not the stub generate staged, and not nothing.
//
// The two restores run in pipeline order, because that order is the contract:
// generate's restore swaps a staged tree over .gen and therefore deletes every
// ceded subtree, and describe — which every Go command schedules after generate
// — is what puts back exactly what the current sources produce. The assertions
// between the two restores state that dependency instead of leaving it to a
// reader to infer.
func TestCededSubpathSurvivesABuildServedEntirelyFromCache(t *testing.T) {
	generate, describe := cededGenJobs()
	f := newCaptureFixture(t, generate, describe)

	gen := filepath.Join(f.ws.Root, captureTestProject, ".gen")
	bundleJSON := filepath.Join(gen, "migration-bundle", "bundle.json")
	payload := filepath.Join(gen, "migration-bundle", "payload", "sql", "0001.sql")
	contract := filepath.Join(gen, "schema", "openapi.json")
	mirror := filepath.Join(gen, "generate-staging", "schema", "openapi.json")
	const bundleBytes = `{"bundleProtocol":"migration-bundle.v1","appName":"demo"}` + "\n"
	const stub = `{"openapi":"3.1.0","paths":{"/a":{"get":{}}}}` + "\n"
	const converged = `{"openapi":"3.1.0","paths":{"/a":{"get":{"operationId":"getA"}}}}` + "\n"

	// Cold build, in pipeline order: generate writes .gen — its own manifest, its
	// private mirror of the ceded contract tree, and the staged stub at the
	// contract path — then the describe binary writes the bundle and converges the
	// contract over that stub.
	writeFileAt(t, filepath.Join(gen, "generate-result.json"), `{"schemas":[".gen/schema/openapi.json"]}`)
	writeFileAt(t, mirror, stub)
	writeFileAt(t, contract, stub)
	if !f.sched.storeDeclaredCapture(generate, capturedResult(nil), captureHashA) {
		t.Fatal("generate did not publish an entry")
	}
	writeFileAt(t, bundleJSON, bundleBytes)
	writeFileAt(t, payload, "create table t (id uuid);\n")
	writeFileAt(t, contract, converged)
	if !f.sched.storeDeclaredCapture(describe, capturedResult(nil), captureHashB) {
		t.Fatal("describe did not publish an entry")
	}

	// The two entries divide the tree exactly once: generate's holds .gen WITHOUT
	// the ceded subtrees, describe's holds the bundle and the converged contract.
	generateEntry, err := f.cache.LookupTaskEntry(captureHashA)
	if err != nil || generateEntry == nil {
		t.Fatalf("lookup generate entry: %v %v", generateEntry, err)
	}
	for _, file := range generateEntry.Manifest.Files {
		if strings.Contains(file.Path, "migration-bundle") || strings.Contains(file.Path, "gen/schema/") {
			t.Errorf("generate captured a ceded path it does not own: %s", file.Path)
		}
	}
	if recorded, ok := generateEntry.Output("gen"); !ok || recorded.Files != 2 {
		t.Errorf("generate recorded %+v, want the two files outside the ceded subtrees", recorded)
	}
	describeEntry, err := f.cache.LookupTaskEntry(captureHashB)
	if err != nil || describeEntry == nil {
		t.Fatalf("lookup describe entry: %v %v", describeEntry, err)
	}
	if recorded, ok := describeEntry.Output("migrationBundle"); !ok || recorded.Files != 2 {
		t.Errorf("describe recorded %+v, want the bundle's two files", recorded)
	}
	if recorded, ok := describeEntry.Output("schema"); !ok || recorded.Files != 1 {
		t.Errorf("describe recorded %+v, want the converged contract", recorded)
	}

	// Warm build from an empty tree.
	if err := os.RemoveAll(gen); err != nil {
		t.Fatal(err)
	}
	f.hitOrFail(t, generate, captureHashA)
	for _, path := range []string{bundleJSON, contract} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("generate's restore materialized the ceded %s, which its entry must not hold", path)
		}
	}
	// What generate DOES restore is its own mirror, which is what lets a describe
	// that misses here rebuild the same merge input it had on a cold run.
	if got := readFileAt(t, mirror); got != stub {
		t.Errorf("restored mirror = %q, want generate's staged stub", got)
	}

	f.hitOrFail(t, describe, captureHashB)

	if got := readFileAt(t, bundleJSON); got != bundleBytes {
		t.Errorf("restored bundle.json = %q, want the captured bytes", got)
	}
	if got := readFileAt(t, payload); got != "create table t (id uuid);\n" {
		t.Errorf("restored payload = %q", got)
	}
	// The point of the whole carve-out: the contract path holds describe's
	// converged document, not the stub generate staged.
	if got := readFileAt(t, contract); got != converged {
		t.Errorf("restored .gen/schema/openapi.json = %q, want describe's converged contract %q", got, converged)
	}
}

// THE PRODUCTION SHAPE behind the remote-cache recurrence: three commands of
// one session share the
// project tree. `package` executes its own describe, which writes the bundle at
// the contract path; `test` is served from cache; `publish` reads the contract
// path and depends on package, not on test. The log order was
//
//	test~generate    remote-cache
//	publish~cloud-publish-migration FAILED: .gen/migration-bundle/bundle.json does not exist
//	test~describe    remote-cache
//
// so the in-pipeline order the carve-out pins (describe after generate) held
// and was not enough: generate's restore deleted a subtree it never owned, and
// the consumer read the tree in the window before describe's restore. A restore
// must leave a ceded subpath exactly as an execution would — untouched — while
// still replacing everything the task does own.
func TestCedingTaskRestoreLeavesTheCededSubtreeInPlace(t *testing.T) {
	generate, _ := cededGenJobs()
	f := newCaptureFixture(t, generate)

	gen := filepath.Join(f.ws.Root, captureTestProject, ".gen")
	manifest := filepath.Join(gen, "generate-result.json")
	bundleJSON := filepath.Join(gen, "migration-bundle", "bundle.json")
	payload := filepath.Join(gen, "migration-bundle", "payload", "sql", "0001.sql")
	contract := filepath.Join(gen, "schema", "openapi.json")
	stale := filepath.Join(gen, "infra", "old-fragment.json")
	const bundleBytes = `{"bundleProtocol":"migration-bundle.v1","appName":"put-server"}` + "\n"

	// Cold: generate captured .gen before any describe wrote into it.
	writeFileAt(t, manifest, `{"schemas":[]}`)
	if !f.sched.storeDeclaredCapture(generate, capturedResult(nil), captureHashA) {
		t.Fatal("generate did not publish an entry")
	}

	// Later, in the same session: another command's describe EXECUTED and wrote
	// the bundle and the converged contract; the tree also drifted in a region
	// generate does own, and generate's own manifest was rewritten.
	writeFileAt(t, manifest, `{"schemas":["drifted"]}`)
	writeFileAt(t, stale, `{}`)
	writeFileAt(t, bundleJSON, bundleBytes)
	writeFileAt(t, payload, "create table t (id uuid);\n")
	writeFileAt(t, contract, `{"openapi":"3.1.0"}`)

	f.hitOrFail(t, generate, captureHashA)

	// What generate owns is the entry's, byte for byte, drift included.
	if got := readFileAt(t, manifest); got != `{"schemas":[]}` {
		t.Errorf("restored generate-result.json = %q, want the captured bytes", got)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("generate's restore kept a file inside the region it owns; the swap did not happen")
	}
	// What it cedes is exactly what was there.
	if got := readFileAt(t, bundleJSON); got != bundleBytes {
		t.Errorf("generate's restore lost the ceded bundle: %q", got)
	}
	if got := readFileAt(t, payload); got != "create table t (id uuid);\n" {
		t.Errorf("generate's restore lost the ceded payload: %q", got)
	}
	if got := readFileAt(t, contract); got != `{"openapi":"3.1.0"}` {
		t.Errorf("generate's restore lost the ceded contract: %q", got)
	}
}

// The negative half, and the reason the staging skip is load-bearing rather
// than cosmetic: the ceding task must not adopt bytes an EARLIER build left in
// a ceded subtree. If it did, its entry would carry a bundle and a contract its
// own sources never produced, and every future hit on that key would resurrect
// them — for a project that now contributes no migration operation, and on every
// machine that pulls the entry.
func TestCededSubpathIsNeverAdoptedByTheCedingTask(t *testing.T) {
	generate, _ := cededGenJobs()
	f := newCaptureFixture(t, generate)

	gen := filepath.Join(f.ws.Root, captureTestProject, ".gen")
	staleBundle := filepath.Join(gen, "migration-bundle", "bundle.json")
	staleContract := filepath.Join(gen, "schema", "openapi.json")
	writeFileAt(t, filepath.Join(gen, "generate-result.json"), `{"schemas":[]}`)
	writeFileAt(t, staleBundle, `{"bundleProtocol":"migration-bundle.v1","appName":"gone"}`)
	writeFileAt(t, staleContract, `{"openapi":"3.1.0","paths":{"/removed":{"get":{}}}}`)

	if !f.sched.storeDeclaredCapture(generate, capturedResult(nil), captureHashA) {
		t.Fatal("generate did not publish an entry")
	}
	entry, err := f.cache.LookupTaskEntry(captureHashA)
	if err != nil || entry == nil {
		t.Fatalf("lookup task entry: %v %v", entry, err)
	}
	for _, file := range entry.Manifest.Files {
		if strings.Contains(file.Path, "migration-bundle") || strings.Contains(file.Path, "gen/schema/") {
			t.Fatalf("the ceding task captured %s, so a hit would resurrect a stale artifact", file.Path)
		}
	}

	if err := os.RemoveAll(gen); err != nil {
		t.Fatal(err)
	}
	f.hitOrFail(t, generate, captureHashA)
	if got := readFileAt(t, filepath.Join(gen, "generate-result.json")); got != `{"schemas":[]}` {
		t.Errorf("restored .gen/generate-result.json = %q", got)
	}
	for _, path := range []string{filepath.Join(gen, "migration-bundle"), filepath.Join(gen, "schema")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("a ceding task's cache hit materialized %s, which it never owned", path)
		}
	}
}

// A remote provider may omit ActionResult.Events when it does not advertise
// action-events, and old cache entries predate that field entirely. The
// task-owned descriptor still proves that these two reserved files were
// produced. Restoring their artifact records keeps the spec gate identical on
// cold and warm runs without throwing away the cache hit.
func TestDeclaredRestoreRecoversReservedArtifactsWithoutCachedEvents(t *testing.T) {
	tests := []struct {
		name       string
		job        string
		outputID   string
		path       string
		artifactID string
	}{
		{
			name:       "feature verification report",
			job:        "test~run",
			outputID:   "featureVerification",
			path:       features.VerificationReportFilename,
			artifactID: features.VerificationReportArtifactID,
		},
		{
			name:       "spec criteria projection",
			job:        "validate~specs",
			outputID:   "criteria",
			path:       features.SpecCriteriaProjectionFilename,
			artifactID: features.SpecCriteriaProjectionArtifactID,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			job := declaredJob(test.job, "producer", &extension.TaskDeclaration{
				Outputs: map[string]extension.DeclaredOutput{
					test.outputID: fileDeclaration(extension.OutputRootCommandOutput, test.path, true),
				},
			})
			f := newCaptureFixture(t, job)
			artifactPath := filepath.Join(f.commandDir(job.CommandName()), test.path)
			writeFileAt(t, artifactPath, "{}\n")

			// No Events simulates an entry restored from a provider without the
			// optional action-events capability.
			if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
				t.Fatal("declared capture did not publish an entry")
			}
			if err := os.Remove(artifactPath); err != nil {
				t.Fatal(err)
			}

			result := f.hitOrFail(t, job, captureHashA)
			artifacts := TaskResultOf(job, result).Artifacts
			if len(artifacts) != 1 || artifacts[0].ID != test.artifactID || artifacts[0].Path != test.path {
				t.Fatalf("restored artifacts = %+v, want %s at %s", artifacts, test.artifactID, test.path)
			}
		})
	}
}

// An optional output recorded EMPTY must remain empty even if a prior session
// left a file at the same shared command-output path. Recovery follows the
// verified entry descriptor, never ambient filesystem state.
func TestDeclaredRestoreDoesNotRecoverStaleEmptyReservedArtifact(t *testing.T) {
	job := declaredJob("test~run", "run", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"featureVerification": fileDeclaration(
				extension.OutputRootCommandOutput, features.VerificationReportFilename, true),
		},
	})
	f := newCaptureFixture(t, job)
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("declared capture did not publish an empty entry")
	}
	writeFileAt(t, filepath.Join(f.commandDir("test"), features.VerificationReportFilename), "stale\n")

	result := f.hitOrFail(t, job, captureHashA)
	if artifacts := TaskResultOf(job, result).Artifacts; len(artifacts) != 0 {
		t.Fatalf("empty cache output recovered stale artifacts: %+v", artifacts)
	}
}

// A prior session may leave a dangling command-output symlink after its cache
// entry is reclaimed. A task that declares a command-output subtree must still
// receive a real writable directory before its restore or execution begins.
func TestDeclaredCommandOutputCreatesDirectoryAfterDanglingLink(t *testing.T) {
	job := declaredJob("build~transpile", "transpile", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"lib": dirDeclaration(extension.OutputRootCommandOutput, "lib", false),
		},
	})
	f := newCaptureFixture(t, job)
	commandDir := f.commandDir("build")
	if err := os.MkdirAll(filepath.Dir(commandDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "reclaimed"), commandDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if err := f.sched.ensureCommandOutputWritable(job); err != nil {
		t.Fatalf("ensure command output: %v", err)
	}
	info, err := os.Lstat(commandDir)
	if err != nil {
		t.Fatalf("command output missing: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("command output = %v, want a real directory", info.Mode())
	}
}

// Task-owned materialization must detach a surviving prior-session link before
// restoring just this task's named subtree. The unrelated prior output remains
// available to siblings, while writes cannot mutate the link target's bytes.
func TestDeclaredRestoreDetachesPriorSessionCommandOutputLink(t *testing.T) {
	job := declaredJob("build~transpile", "transpile", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"lib": dirDeclaration(extension.OutputRootCommandOutput, "lib", false),
		},
	})
	f := newCaptureFixture(t, job)
	commandDir := f.commandDir("build")
	writeFileAt(t, filepath.Join(commandDir, "lib", "index.js"), "cold\n")
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("store declared entry")
	}

	prior := t.TempDir()
	priorFile := filepath.Join(prior, "bin", "tool")
	writeFileAt(t, priorFile, "prior\n")
	if err := os.RemoveAll(commandDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(prior, commandDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	f.hitOrFail(t, job, captureHashA)
	info, err := os.Lstat(commandDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("declared restore left command output as a symlink")
	}
	if got := readFileAt(t, priorFile); got != "prior\n" {
		t.Fatalf("restore mutated prior-session source = %q", got)
	}
	if got := readFileAt(t, filepath.Join(commandDir, "bin", "tool")); got != "prior\n" {
		t.Errorf("prior sibling output was not preserved: %q", got)
	}
	if got := readFileAt(t, filepath.Join(commandDir, "lib", "index.js")); got != "cold\n" {
		t.Errorf("declared output was not restored: %q", got)
	}
}

// ---------------------------------------------------------------------------
// optional-empty, end to end
// ---------------------------------------------------------------------------

// An optionalEmpty output the task did not produce is RECORDED empty, and
// restoring it must not touch its destination. Command-output paths are shared
// between the steps of one command, so "this entry has nothing for it" must
// never be read as "this path should be empty" — that would delete a sibling
// task's artifacts.
func TestDeclaredCaptureOptionalEmptyLeavesSiblingsAlone(t *testing.T) {
	job := declaredJob("test~run", "run", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"coverage": fileDeclaration(extension.OutputRootCommandOutput, "lcov.info", true),
			"junit":    fileDeclaration(extension.OutputRootCommandOutput, "results.junit.xml", true),
		},
	})
	f := newCaptureFixture(t, job)

	// The run produced a junit report but no coverage (coverage was not enabled).
	junit := filepath.Join(f.commandDir("test"), "results.junit.xml")
	writeFileAt(t, junit, "<testsuites/>\n")

	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("declared capture did not publish an entry")
	}
	entry, err := f.cache.LookupTaskEntry(captureHashA)
	if err != nil || entry == nil {
		t.Fatalf("lookup task entry: %v %v", entry, err)
	}
	coverageRecord, ok := entry.Output("coverage")
	if !ok || coverageRecord.Present() {
		t.Fatalf("absent optional output recorded as %+v, want an explicit empty state", coverageRecord)
	}

	// A sibling step of the SAME command has since written its own artifact into
	// the shared directory, and something else is sitting at the empty output's
	// path. Neither may be disturbed by the restore.
	sibling := filepath.Join(f.commandDir("test"), "sibling", "report.txt")
	writeFileAt(t, sibling, "sibling output\n")
	coverage := filepath.Join(f.commandDir("test"), "lcov.info")
	writeFileAt(t, coverage, "written by something else\n")
	os.Remove(junit)

	f.hitOrFail(t, job, captureHashA)

	if got := readFileAt(t, junit); got != "<testsuites/>\n" {
		t.Errorf("present output was not restored: %q", got)
	}
	if got := readFileAt(t, coverage); got != "written by something else\n" {
		t.Errorf("empty output touched its destination: %q", got)
	}
	if got := readFileAt(t, sibling); got != "sibling output\n" {
		t.Errorf("restore disturbed a sibling step's artifact: %q", got)
	}
}

// build-infra's shape: a required manifest beside an optional runtime defaults
// file, both under the project root. An entry stored while the workload
// authored its runtime records the defaults file as empty. A hit on that entry
// restores the manifest and leaves the defaults file an earlier run wrote in
// place, because restoring an empty output never touches its destination. The
// Go and TypeScript build-infra descriptions say so instead of promising that
// the file is gone.
func TestDeclaredCaptureEmptyProjectFileLeavesAnEarlierFileInPlace(t *testing.T) {
	job := declaredJob("build~infra", "infra", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"requirements":    fileDeclaration(extension.OutputRootProject, ".gen/requirements.json", false),
			"runtimeDefaults": fileDeclaration(extension.OutputRootProject, ".gen/infra/runtime.json", true),
		},
	})
	f := newCaptureFixture(t, job)
	requirements := filepath.Join(f.ws.Root, captureTestProject, ".gen", "requirements.json")
	defaults := filepath.Join(f.ws.Root, captureTestProject, ".gen", "infra", "runtime.json")

	// The run under an authored runtime wrote the manifest and no defaults file.
	const authored = `{"runtime":"authored"}` + "\n"
	writeFileAt(t, requirements, authored)
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("declared capture did not publish an entry")
	}
	entry, err := f.cache.LookupTaskEntry(captureHashA)
	if err != nil || entry == nil {
		t.Fatalf("lookup task entry: %v %v", entry, err)
	}
	if record, ok := entry.Output("runtimeDefaults"); !ok || record.Present() {
		t.Fatalf("absent runtime defaults recorded as %+v, want an explicit empty state", record)
	}

	// An earlier build, before the workload authored its runtime, left both
	// files with the defaults.
	const defaultsRuntime = `{"runtime":"defaults"}` + "\n"
	writeFileAt(t, requirements, defaultsRuntime)
	writeFileAt(t, defaults, defaultsRuntime)

	f.hitOrFail(t, job, captureHashA)

	if got := readFileAt(t, requirements); got != authored {
		t.Errorf("the hit did not restore the manifest: %q", got)
	}
	if got := readFileAt(t, defaults); got != defaultsRuntime {
		t.Errorf("the hit touched the empty output's destination: %q", got)
	}
}

// An optional output whose path comes from a task output port is simply absent
// from the entry when the port reports nothing — and present when it does. The
// positive control is what keeps this from passing for the wrong reason.
func TestDeclaredCapturePortPathOutput(t *testing.T) {
	newJob := func() *ScheduledJob {
		return declaredJob("build~describe", "describe", &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{
				"client": {Kind: extension.OutputKindDirectory, PathFrom: "clientOutput", OptionalEmpty: true},
			},
		}, extension.ResourceRef{ID: clientsResourceID, Scope: extension.ResourceScopeProject})
	}

	t.Run("port reports a path", func(t *testing.T) {
		job := newJob()
		f := newCaptureFixture(t, job)
		clientDir := filepath.Join(f.ws.Root, captureTestProject, "src", "clients")
		writeFileAt(t, filepath.Join(clientDir, "api.ts"), "export const api = 1\n")

		if !f.sched.storeDeclaredCapture(job, capturedResult(map[string]any{"clientOutput": "src/clients"}), captureHashA) {
			t.Fatal("declared capture did not publish an entry")
		}
		if err := os.RemoveAll(clientDir); err != nil {
			t.Fatal(err)
		}
		f.hitOrFail(t, job, captureHashA)
		if got := readFileAt(t, filepath.Join(clientDir, "api.ts")); got != "export const api = 1\n" {
			t.Errorf("client tree not restored: %q", got)
		}
	})

	t.Run("port reports nothing", func(t *testing.T) {
		job := newJob()
		f := newCaptureFixture(t, job)
		if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
			t.Fatal("a project that generates no client must still cache its status")
		}
		entry, err := f.cache.LookupTaskEntry(captureHashA)
		if err != nil || entry == nil {
			t.Fatalf("lookup task entry: %v %v", entry, err)
		}
		if len(entry.Outputs) != 0 {
			t.Fatalf("entry recorded %+v, want no outputs for an unresolved optional port", entry.Outputs)
		}
	})
}

// ---------------------------------------------------------------------------
// completeness: a required output the task did not produce
// ---------------------------------------------------------------------------

// A missing REQUIRED declared output must leave the key uncached rather than
// publishing an entry that restores less than the declaration promises. It must
// also not silently fall back to writing a legacy entry, which nothing on this
// path would ever read.
func TestDeclaredCaptureRefusesToPublishWithoutARequiredOutput(t *testing.T) {
	job := declaredJob("build~generate", "generate", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"gen":      dirDeclaration(extension.OutputRootProject, ".gen", false),
			"optional": dirDeclaration(extension.OutputRootCommandOutput, "extras", true),
		},
	}, extension.ResourceRef{ID: genResourceID, Scope: extension.ResourceScopeProject})
	f := newCaptureFixture(t, job)

	// The optional output exists; the required one does not.
	writeFileAt(t, filepath.Join(f.commandDir("build"), "extras", "note.txt"), "extra\n")

	if f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("published an entry without the required declared output")
	}
	if entry, err := f.cache.LookupTaskEntry(captureHashA); err != nil || entry != nil {
		t.Fatalf("a refused capture left an entry behind: %v (err=%v)", entry, err)
	}
	if legacy, err := f.cache.Lookup(captureHashA); err != nil || legacy != nil {
		t.Fatalf("a refused capture fell back to a legacy entry: %v (err=%v)", legacy, err)
	}

	// Positive control: producing the required output makes the very same call
	// publish, so the refusal is about completeness and nothing else.
	writeFileAt(t, filepath.Join(f.ws.Root, captureTestProject, ".gen", "types.ts"), "gen\n")
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("a complete capture was refused")
	}
}

// A cacheable in-job SKIP produced no files, so it is captured from an empty
// staging root: the paths its declaration names may still hold an earlier run's
// bytes, and publishing those under this key would attribute a tree to an
// execution that never made it.
func TestDeclaredCaptureOfASkipNeverAdoptsAnEarlierRunsBytes(t *testing.T) {
	job := declaredJob("build~transpile", "transpile", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"lib": dirDeclaration(extension.OutputRootCommandOutput, "lib", true),
		},
	})
	f := newCaptureFixture(t, job)

	// An earlier run left a lib/ tree behind.
	lib := filepath.Join(f.commandDir("build"), "lib")
	writeFileAt(t, filepath.Join(lib, "index.js"), "from an earlier run\n")

	skipped := &JobResult{Status: "skipped"}
	if !f.sched.storeDeclaredCapture(job, skipped, captureHashA) {
		t.Fatal("an all-optional declaration must still cache a deterministic skip")
	}
	entry, err := f.cache.LookupTaskEntry(captureHashA)
	if err != nil || entry == nil {
		t.Fatalf("lookup task entry: %v %v", entry, err)
	}
	recorded, ok := entry.Output("lib")
	if !ok || recorded.Present() {
		t.Fatalf("a skip captured %+v, want an explicit empty state", recorded)
	}
	// And restoring that entry leaves the tree exactly as it is.
	f.hitOrFail(t, job, captureHashA)
	if got := readFileAt(t, filepath.Join(lib, "index.js")); got != "from an earlier run\n" {
		t.Errorf("restoring a skip disturbed the tree: %q", got)
	}

	// Positive control: the SAME job and the same on-disk tree DO get captured
	// when the execution succeeded, so the emptiness comes from the skip.
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashB) {
		t.Fatal("successful capture failed")
	}
	success, err := f.cache.LookupTaskEntry(captureHashB)
	if err != nil || success == nil {
		t.Fatalf("lookup task entry: %v %v", success, err)
	}
	if recorded, _ := success.Output("lib"); !recorded.Present() {
		t.Fatal("a successful run did not capture the declared output; the control proves nothing")
	}
}

// ---------------------------------------------------------------------------
// eligibility: which tasks take the declared path
// ---------------------------------------------------------------------------

func TestUsesDeclaredCaptureEligibility(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "declared-output-cache-boundary", "local-docker-candidate-is-task-owned-cache-eligible")
	cases := []struct {
		name string
		job  *ScheduledJob
		want bool
	}{
		{name: "v2 task without a declaration", job: declaredJob("build~transpile", "transpile", nil), want: false},
		{name: "empty declaration is a status-only contract", job: declaredJob("build~lint", "lint", &extension.TaskDeclaration{}), want: true},
		{name: "declared command output", job: declaredJob("build~transpile", "transpile", &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{"lib": dirDeclaration(extension.OutputRootCommandOutput, "lib", true)},
		}), want: true},
		{name: "effects-only declaration is still explicit", job: declaredJob("deps-upgrade~exec", "exec", &extension.TaskDeclaration{
			Effects: []string{extension.EffectNetwork},
		}), want: true},
		{name: "package step with a status-only contract", job: declaredJob("package~verify", "verify", &extension.TaskDeclaration{}), want: true},
		{name: "package candidate directory", job: declaredJob("package~docker", "docker", &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{"docker": dirDeclaration(extension.OutputRootCommandOutput, "docker", false)},
		}), want: true},
		{name: "package step owning a directory and a file", job: declaredJob("package~owned", "owned", &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{
				"owned":    dirDeclaration(extension.OutputRootCommandOutput, "owned", true),
				"manifest": fileDeclaration(extension.OutputRootCommandOutput, "manifest.json", true),
			},
		}), want: true},
		{name: "source rewriter uses a clean-only status entry", job: declaredJob("lint~fix", "fix", &extension.TaskDeclaration{MutatesSources: true}), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usesDeclaredCapture(tc.job); got != tc.want {
				t.Errorf("usesDeclaredCapture = %v, want %v", got, tc.want)
			}
		})
	}
}
func TestDeclaredCaptureStoresStatusOnlyForNoOutputTasks(t *testing.T) {
	job := declaredJob("lint~staticcheck", "staticcheck", &extension.TaskDeclaration{})
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}
	f := newCaptureFixture(t, job)

	if !usesDeclaredCapture(job) {
		t.Fatal("a noOutput task with an empty declaration must take the declared path")
	}
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("status-only declared capture failed")
	}
	entry, err := f.cache.LookupTaskEntry(captureHashA)
	if err != nil || entry == nil || len(entry.Outputs) != 0 {
		t.Fatalf("status-only entry = %+v (err=%v)", entry, err)
	}
	if result := f.hitOrFail(t, job, captureHashA); result.Status != "success" {
		t.Fatalf("status-only hit returned %q", result.Status)
	}
}

// TestPackageDeclaredCaptureRoundTrip exercises the whole flow in one execution:
// a package step whose channel record lives INSIDE the output it owns is
// captured, and a restore brings the artifact and the record back together —
// so the channel a publisher reads survives a cache hit that skipped the task.
//
// Before the fix this was unreachable: the step recorded its channel in a file
// outside its declaration, so the command was refused declared capture
// altogether and every packaging task executed on every run.
func TestPackageDeclaredCaptureRoundTrip(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "declared-output-cache-boundary", "a-restored-package-output-carries-the-channel-it-recorded")
	declaration := &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"owned": dirDeclaration(extension.OutputRootCommandOutput, "owned", true),
		},
	}
	job := declaredJob("package~owned", "owned", declaration)
	f := newCaptureFixture(t, job)

	if !usesDeclaredCapture(job) || !isCacheEnabled(job, CacheBypass{}) {
		t.Fatal("a package step whose declaration covers what it writes must use task-owned caching")
	}
	// The same declaration under another command is eligible too: eligibility is
	// the declaration, not the command it runs in.
	if built := declaredJob("build~owned", "owned", declaration); !usesDeclaredCapture(built) {
		t.Fatal("the same declaration outside package lost task-owned caching")
	}

	// Cold: the packager writes its artifact AND its channel record inside the
	// directory it owns.
	ownedDir := filepath.Join(f.commandDir("package"), "owned")
	writeFileAt(t, filepath.Join(ownedDir, "artifact.tgz"), "packaged bytes\n")
	if err := pkgmeta.WriteChannelRecord(ownedDir, pkgmeta.ChannelRecord{
		Version: "1.2.3", Artifact: "team/app", Channels: []string{"owned"},
	}); err != nil {
		t.Fatalf("write channel record: %v", err)
	}
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("declared capture did not publish an entry for a package step")
	}

	// The command output directory is wiped, as a fresh checkout would leave it.
	if err := os.RemoveAll(ownedDir); err != nil {
		t.Fatal(err)
	}
	if index, err := pkgmeta.ReadChannelIndex(f.ws.Root, captureTestProject); err == nil {
		t.Fatalf("the wiped tree still reports channels %v; the control proves nothing", index.Channels)
	}

	result := f.hitOrFail(t, job, captureHashA)
	if !result.Reuse.CacheHit() {
		t.Fatalf("restored result reuse = %v, want a cache hit", result.Reuse)
	}
	if got := readFileAt(t, filepath.Join(ownedDir, "artifact.tgz")); got != "packaged bytes\n" {
		t.Errorf("restored artifact = %q", got)
	}
	// The channel traveled with the artifact: a publisher reading the derived
	// index after a hit sees exactly what the executed run recorded.
	index, err := pkgmeta.ReadChannelIndex(f.ws.Root, captureTestProject)
	if err != nil {
		t.Fatalf("ReadChannelIndex after a restore: %v", err)
	}
	if !index.HasChannel("owned") {
		t.Fatalf("restored channels = %v, want the packaged channel", index.Channels)
	}
	if got := index.Records["owned"].Version; got != "1.2.3" {
		t.Errorf("restored record version = %q, want 1.2.3", got)
	}
}

func TestRestoredDockerCandidateFeedsSelectedPublishAlongsideSiblingMetadata(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "declared-output-cache-boundary", "restored-docker-candidate-feeds-selected-publish")
	t.Setenv("DOCKER_REGISTRY", "")
	job := declaredJob("package~docker", "docker", &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
		"docker": dirDeclaration(extension.OutputRootCommandOutput, "docker", false),
	}})
	f := newCaptureFixture(t, job)
	dockerDir := filepath.Join(f.commandDir("package"), "docker")
	writeFileAt(t, filepath.Join(dockerDir, "manifest.json"), `{"image":"team/app","tags":["team/app:c-candidate"],"version":"1.2.3","contentHash":"candidate","layout":"oci"}`)
	writeFileAt(t, filepath.Join(dockerDir, "oci", "index.json"), "candidate-layout\n")
	if !f.sched.storeDeclaredCapture(job, capturedResult(nil), captureHashA) {
		t.Fatal("docker candidate capture did not publish an entry")
	}
	if err := os.RemoveAll(dockerDir); err != nil {
		t.Fatal(err)
	}
	// A sibling packager records ITS channel in the directory IT owns, after the
	// Docker task was restored. It must not suppress the already-selected typed
	// Docker publication.
	if err := pkgmeta.WriteChannelRecord(filepath.Join(f.commandDir("package"), "npm"), pkgmeta.ChannelRecord{
		Version: "1.2.3", Artifact: "team/app", Channels: []string{"npm"},
	}); err != nil {
		t.Fatalf("write sibling record: %v", err)
	}
	f.hitOrFail(t, job, captureHashA)
	if got := readFileAt(t, filepath.Join(dockerDir, "oci", "index.json")); got != "candidate-layout\n" {
		t.Fatalf("restored layout = %q", got)
	}
	ctx := &pctx.Context{WorkspaceRoot: f.ws.Root, Project: pctx.Project{Name: "team/app", Path: captureTestProject}}
	status, data, err := dockerpublish.Publish(ctx, jsonl.New(), []string{"--dry-run"})
	if err != nil || status != "OK" || data["contentTag"] != "c-candidate" {
		t.Fatalf("Publish(restored candidate) = (%q, %+v, %v), want selected Docker dry run", status, data, err)
	}
}

// A clean source rewriter publishes an ordinary status-only entry. This is the
// warm-lint fast path: once a formatter proves the keyed tree needs no edits,
// another run can reuse that verdict without replaying source bytes.
func TestCleanSourceRewriterPublishesReusableStatusEntry(t *testing.T) {
	sources := extension.ResourceRef{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}
	job := declaredJob("lint~fix", "fix", &extension.TaskDeclaration{MutatesSources: true}, sources)
	job.JobDef.FilePatterns = []string{"index.ts"}
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}
	job.DependsOn = []string{"/workspace-install"}
	f := newCaptureFixture(t, job)

	sourcePath := filepath.Join(f.ws.Root, captureTestProject, "index.ts")
	writeFileAt(t, sourcePath, "const clean = true;\n")
	hash, err := computeJobCacheHash(f.ws, job, nil, nil, f.cache, map[string]string{
		"/workspace-install": "dependency-before",
	})
	if err != nil || hash == "" {
		t.Fatalf("compute source-rewriter key: %q (err=%v)", hash, err)
	}
	if !isCacheEnabled(job, CacheBypass{}) {
		t.Fatal("a declared source rewriter did not reach clean-only caching")
	}
	sourceHash, err := sourceInputDigest(f.ws, job, nil, f.cache)
	if err != nil || sourceHash == "" {
		t.Fatalf("compute pre-run source digest: %q (err=%v)", sourceHash, err)
	}

	var mu sync.Mutex
	// Moving a non-source key component must not poison an otherwise clean
	// formatter result. The old whole-key comparison marked this as a mutation.
	hashes := map[string]string{"/workspace-install": "dependency-after"}
	result := f.sched.finalizeExecutedJob(t.Context(), job, capturedResult(nil),
		true, hash, sourceHash, &mu, hashes)
	if result.SourceMutated {
		t.Fatal("an unchanged source tree was marked as mutated")
	}
	if hit := f.hitOrFail(t, job, hash); !hit.CacheHit || hit.SourceMutated {
		t.Fatalf("clean source-rewriter restore = %+v, want an ordinary clean hit", hit)
	}
}

// A successful fixer whose keyed inputs changed still publishes a marker so a
// coalesced waiter wakes promptly, but no worktree may consume it as green.
func TestMutatingSourceRewriterEntryIsNeverReused(t *testing.T) {
	sources := extension.ResourceRef{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}
	job := declaredJob("lint~fix", "fix", &extension.TaskDeclaration{MutatesSources: true}, sources)
	job.JobDef.FilePatterns = []string{"index.ts"}
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}
	f := newCaptureFixture(t, job)

	sourcePath := filepath.Join(f.ws.Root, captureTestProject, "index.ts")
	writeFileAt(t, sourcePath, "const dirty=true\n")
	hash, err := computeJobCacheHash(f.ws, job, nil, nil, f.cache, nil)
	if err != nil || hash == "" {
		t.Fatalf("compute pre-fix key: %q (err=%v)", hash, err)
	}
	sourceHash, err := sourceInputDigest(f.ws, job, nil, f.cache)
	if err != nil || sourceHash == "" {
		t.Fatalf("compute pre-fix source digest: %q (err=%v)", sourceHash, err)
	}
	writeFileAt(t, sourcePath, "const dirty = true;\n")

	var mu sync.Mutex
	result := f.sched.finalizeExecutedJob(t.Context(), job, capturedResult(nil),
		true, hash, sourceHash, &mu, map[string]string{})
	if !result.SourceMutated {
		t.Fatal("a changed source tree was recorded as clean")
	}

	entry, err := f.cache.LookupTaskEntry(hash)
	if err != nil || entry == nil {
		t.Fatalf("lookup mutation marker: entry=%v err=%v", entry, err)
	}
	stored := jobResultFromEntryResult(entry.Result)
	if !stored.SourceMutated {
		t.Fatal("task-owned entry lost the source-mutation marker")
	}
	if hit := f.sched.restoreDeclaredCacheHit(t.Context(), job, hash, entry, &mu, map[string]string{}); hit != nil {
		t.Fatalf("mutating source-rewriter entry restored as green: %+v", hit)
	}
}

// A rewriter that declares NO keyed file patterns must never publish a reusable
// verdict. Its key cannot see the sources it rewrites, so a digest comparison
// over an empty pattern set compares a constant to itself and reports "clean"
// forever: the first green would be served to every later run and the fixer
// would stop running against any tree state. The detector fails closed instead.
func TestPatternlessSourceRewriterIsNeverProvenClean(t *testing.T) {
	sources := extension.ResourceRef{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}
	job := declaredJob("lint~fix", "fix", &extension.TaskDeclaration{MutatesSources: true}, sources)
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}
	f := newCaptureFixture(t, job)

	writeFileAt(t, filepath.Join(f.ws.Root, captureTestProject, "index.ts"), "const dirty=true\n")
	if _, err := sourceInputDigest(f.ws, job, nil, f.cache); err == nil {
		t.Fatal("a source rewriter with no keyed patterns produced a digest; its key is blind to what it rewrites")
	}

	hash, err := computeJobCacheHash(f.ws, job, nil, nil, f.cache, nil)
	if err != nil || hash == "" {
		t.Fatalf("compute patternless key: %q (err=%v)", hash, err)
	}
	var mu sync.Mutex
	result := f.sched.finalizeExecutedJob(t.Context(), job, capturedResult(nil),
		true, hash, "", &mu, map[string]string{})
	if !result.SourceMutated {
		t.Fatal("an unprovable source-rewriter result was published as clean")
	}
	entry, err := f.cache.LookupTaskEntry(hash)
	if err != nil || entry == nil {
		t.Fatalf("lookup patternless entry: entry=%v err=%v", entry, err)
	}
	if hit := f.sched.restoreDeclaredCacheHit(t.Context(), job, hash, entry, &mu, map[string]string{}); hit != nil {
		t.Fatalf("patternless source-rewriter entry restored as green: %+v", hit)
	}
}

// The detector reads the workspace-relative half of the key too. A task keyed by
// a shared root file that rewrites it would otherwise compare only its project
// tree, find it unchanged, and publish the pre-fix workspace key as clean.
func TestSourceRewriterDetectsWorkspaceKeyedRewrite(t *testing.T) {
	sources := extension.ResourceRef{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}
	job := declaredJob("lint~fix", "fix", &extension.TaskDeclaration{MutatesSources: true}, sources)
	job.JobDef.FilePatterns = []string{"index.ts"}
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{
		NoOutput: true,
		Key:      &extension.TaskCacheKey{WorkspaceFiles: []string{"biome.json"}},
	}
	f := newCaptureFixture(t, job)

	writeFileAt(t, filepath.Join(f.ws.Root, captureTestProject, "index.ts"), "const a = 1;\n")
	workspaceFile := filepath.Join(f.ws.Root, "biome.json")
	writeFileAt(t, workspaceFile, "{\"indentWidth\":2}\n")

	hash, err := computeJobCacheHash(f.ws, job, nil, nil, f.cache, nil)
	if err != nil || hash == "" {
		t.Fatalf("compute key: %q (err=%v)", hash, err)
	}
	sourceHash, err := sourceInputDigest(f.ws, job, nil, f.cache)
	if err != nil || sourceHash == "" {
		t.Fatalf("compute pre-fix digest: %q (err=%v)", sourceHash, err)
	}

	// The project tree is untouched; only the keyed workspace file is rewritten.
	writeFileAt(t, workspaceFile, "{\"indentWidth\": 2}\n")

	var mu sync.Mutex
	result := f.sched.finalizeExecutedJob(t.Context(), job, capturedResult(nil),
		true, hash, sourceHash, &mu, map[string]string{})
	if !result.SourceMutated {
		t.Fatal("a rewrite of a keyed workspace file was recorded as clean")
	}
}

// A v2 source writer still executes uncached: no inferred directory capture
// remains after the v3 cutover.
func TestV2SourceWriterStaysUncachedWithoutDeclaration(t *testing.T) {
	sources := extension.ResourceRef{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}
	job := declaredJob("lint~fix", "fix", nil, sources)
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}
	if isCacheEnabled(job, CacheBypass{}) {
		t.Fatal("a v2 source writer reached a cache path without a declaration")
	}
}

// A source rewriter invalidates memoized input digests regardless of whether
// caching is enabled for this invocation.
//
// Without this invalidation, a formatter would leave every later job in the
// scheduler computing its key from pre-fix bytes — a cache key that no longer
// describes the worktree.
func TestSourceRewritersStillInvalidateMemoizedInputDigests(t *testing.T) {
	sources := extension.ResourceRef{ID: extension.ResourceIDSources, Scope: extension.ResourceScopeProject}
	fixer := declaredJob("lint~fix", "fix", &extension.TaskDeclaration{MutatesSources: true}, sources)
	fixer.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{NoOutput: true}

	// A downstream consumer of the same project's sources: its key is what must
	// move once the fixer rewrote them.
	consumer := declaredJob("build~transpile", "transpile", nil)
	consumer.JobDef.FilePatterns = []string{"**/*.ts"}

	f := newCaptureFixture(t, fixer, consumer)
	sourcePath := filepath.Join(f.ws.Root, captureTestProject, "index.ts")
	writeFileAt(t, sourcePath, "const a=1\n")

	before, err := computeJobCacheHash(f.ws, consumer, nil, nil, f.cache, nil)
	if err != nil || before == "" {
		t.Fatalf("computeJobCacheHash: %q (err=%v)", before, err)
	}

	// The fixer rewrites the source it read. Without the invalidation the memo
	// still holds the pre-fix digest for this (dir, patterns) pair.
	writeFileAt(t, sourcePath, "const a = 1;\n")
	if memoized, err := computeJobCacheHash(f.ws, consumer, nil, nil, f.cache, nil); err != nil || memoized != before {
		t.Fatalf("the memo did not hold the pre-fix digest, so this test cannot prove anything: %q vs %q (err=%v)",
			memoized, before, err)
	}

	var mu sync.Mutex
	f.sched.finalizeExecutedJob(t.Context(), fixer, capturedResult(nil),
		false, "", "", &mu, map[string]string{})

	after, err := computeJobCacheHash(f.ws, consumer, nil, nil, f.cache, nil)
	if err != nil {
		t.Fatalf("computeJobCacheHash after the rewrite: %v", err)
	}
	if after == before {
		t.Fatal("a v3 source rewriter left the memoized input digests in place; " +
			"every later job in this run would key off the pre-fix tree")
	}
}

// Capture resolves every declared command-output path, including one at the
// root of the directory. It used to refuse a closed set of "unowned merge
// point" paths; a later fix gave those paths owners, so the refusal would now reject
// the declaration that fixed them. Who may own a path is decided where
// ownership is decided — the manifest validator and validatePlanContract.
func TestResolveDeclaredOutputsResolvesAnOwnedCommandOutputFile(t *testing.T) {
	for _, path := range []string{"metadata.json", "npm/metadata.json"} {
		job := declaredJob("package~owned", "owned", &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{
				"manifest": fileDeclaration(extension.OutputRootCommandOutput, path, false),
			},
		})
		plans, err := resolveDeclaredOutputs(workspaceRoots{root: "/ws"}, job, nil)
		if err != nil {
			t.Fatalf("declared command-output file %q failed to resolve: %v", path, err)
		}
		if len(plans) != 1 || plans[0].spec.Path != path {
			t.Fatalf("resolved %+v, want the declared path %q", plans, path)
		}
	}
}

func TestResolveDeclaredOutputsResolvesRootsAbsolutely(t *testing.T) {
	job := declaredJob("build~emit", "emit", &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"cmd":  dirDeclaration(extension.OutputRootCommandOutput, "lib", false),
			"proj": dirDeclaration(extension.OutputRootProject, ".gen", false),
			"ws":   fileDeclaration(extension.OutputRootWorkspace, "bun.lock", false),
		},
	})
	plans, err := resolveDeclaredOutputs(workspaceRoots{root: "/ws"}, job, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got := map[string]string{}
	for _, plan := range plans {
		got[plan.spec.ID] = plan.src
	}
	want := map[string]string{
		"cmd":  filepath.Join("/ws", ".putnami", "out", captureTestProject, "build", "lib"),
		"proj": filepath.Join("/ws", captureTestProject, ".gen"),
		"ws":   filepath.Join("/ws", "bun.lock"),
	}
	for id, wantPath := range want {
		if got[id] != wantPath {
			t.Errorf("output %q resolved to %q, want %q", id, got[id], wantPath)
		}
	}
}

// Capture and ownership must honor the SAME carve-outs. A cede on a pathFrom
// output is undecidable from the manifest — validation rejects it, and the
// ownership refs drop it — so capture ignores it too. Honoring it on one side
// only is how a subtree ends up owned by one task and captured by neither.
func TestDeclaredCarveOutsAreIgnoredOnAPortPath(t *testing.T) {
	port := extension.DeclaredOutput{
		Kind:     extension.OutputKindDirectory,
		PathFrom: "clientOutputs",
		Excludes: []string{"clients/go/vendor"},
	}
	if got := declaredExcludesOf(port); got != nil {
		t.Errorf("declaredExcludesOf(port output) = %v, want nothing", got)
	}
	if got := outputRelativeExcludes("clients/go", declaredExcludesOf(port)); got != nil {
		t.Errorf("capture honored %v on a runtime-reported path the ownership check drops", got)
	}

	// Control: the same carve-out on a literal path IS honored and is rebased
	// onto the output, so the assertions above are about the port rather than
	// about carve-outs in general.
	literal := extension.DeclaredOutput{
		Kind:     extension.OutputKindDirectory,
		Root:     extension.OutputRootProject,
		Path:     ".gen",
		Excludes: []string{".gen/migration-bundle"},
	}
	got := outputRelativeExcludes(".gen", declaredExcludesOf(literal))
	if len(got) != 1 || got[0] != "migration-bundle" {
		t.Errorf("literal-path carve-out = %v, want the output-relative subpath", got)
	}
}
