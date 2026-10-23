package planner

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// clientgenTasks are the two generator tasks and the job each runs as.
var clientgenTasks = []struct{ name, job string }{
	{"clientgen-go", "clientgen~generate-go"},
	{"clientgen-ts", "clientgen~generate-ts"},
}

// loadShippedExtension loads an extension manifest of this repository the way
// a run loads an installed one, so a key never reads the extension's sources,
// which a concurrent build may rewrite.
func loadShippedExtension(t *testing.T, rel, name string) *extension.ExtensionDescription {
	t.Helper()
	ext := extension.LoadExtensionFromDir(filepath.Join(findJobsRepoRoot(t), filepath.FromSlash(rel)), name)
	if ext == nil {
		t.Fatalf("load the %s extension from %s", name, rel)
	}
	ext.LocalSource = false
	ext.RelPath = rel
	pinInstalledTree(t, ext)
	return ext
}

// pinInstalledTree points ext, loaded from this repository as an installed
// extension, at a copy of its manifest. The key of an installed extension's
// task digests every file under ext.Path, and a build of the same run writes
// into the repository's extension directory, so the installed tree is one only
// the test writes.
func pinInstalledTree(t *testing.T, ext *extension.ExtensionDescription) {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join(ext.Path, extension.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	ext.Path = t.TempDir()
	writeTestFile(t, filepath.Join(ext.Path, extension.ManifestFilename), string(manifest))
}

// isolateTreeKeyedGit keeps the machine's Git configuration and repository
// variables away from every git process of the test, its own and the ones the
// code under test starts: no global or system config, and no GIT_DIR,
// GIT_WORK_TREE or GIT_INDEX_FILE.
func isolateTreeKeyedGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// treeKeyedGit runs git in dir with a fixed identity and fixed dates, and
// returns its trimmed output.
func treeKeyedGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=T", "-c", "user.email=t@t.com",
		"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
	}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE=2026-01-02T03:04:05Z", "GIT_AUTHOR_DATE=2026-01-02T03:04:05Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// clientgenKeyFixture is a Git work tree that ignores .gen, as this
// repository does, with one provider project at provider/.
type clientgenKeyFixture struct {
	root string
	dir  string
	ws   *workspace.Workspace
}

func newClientgenKeyFixture(t *testing.T) *clientgenKeyFixture {
	t.Helper()
	isolateTreeKeyedGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	treeKeyedGit(t, root, "init", "-q", "-b", "main")
	writeTestFile(t, filepath.Join(root, ".gitignore"), ".gen/\n.putnami/\n")
	dir := filepath.Join(root, "provider")
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module example.com/provider\n\ngo 1.26\n")
	writeTestFile(t, filepath.Join(dir, "package.json"), "{\"name\":\"provider\"}\n")
	writeTestFile(t, filepath.Join(dir, "schema", "openapi.json"), "{\"openapi\":\"3.1.0\"}\n")
	writeTestFile(t, filepath.Join(dir, "clientgen.json"), "{\"targets\":[\"go\",\"ts\"]}\n")
	provider := &workspace.Project{ID: "/provider", Name: "provider", Path: "provider"}
	ws := workspace.NewWorkspace(root, &wsproto.Config{}, []*workspace.Project{provider})
	ws.Graph = workspace.BuildGraph(ws.Projects)
	return &clientgenKeyFixture{root: root, dir: dir, ws: ws}
}

// key computes the task's key with a fresh cache manager, as a new process
// does, so no memoized digest hides a change.
func (f *clientgenKeyFixture) key(t *testing.T, clientgen *extension.ExtensionDescription, task, jobName string) string {
	t.Helper()
	definition, ok := clientgen.Tasks[task]
	if !ok || definition.Cache == nil || definition.Cache.Key == nil {
		t.Fatalf("real clientgen manifest has no keyed %s task", task)
	}
	job := &jobmodel.ScheduledJob{
		Project:   f.ws.Projects[0],
		Extension: clientgen,
		Step:      &extension.PipelineStep{Task: task},
		JobDef: &extension.JobDefinition{
			Name: jobName, CommandName: "clientgen", ExtensionName: clientgen.Name, Cache: true,
			TaskCachePolicy: &extension.TaskCachePolicy{Key: definition.Cache.Key},
		},
	}
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(f.root, ".putnami", "store")))
	value, err := cachekey.ComputeJobCacheHash(f.ws, job, nil, nil, cache, nil)
	if err != nil {
		t.Fatalf("compute %s key: %v", task, err)
	}
	return value
}

// The clientgen keys name no file a build of the run writes. build-describe
// (Go) and build-generate (TypeScript) write .gen/schema and .gen/clientgen
// during the run, so a key that hashed them would differ between the value the
// run computes before any task starts and the value the task computes when it
// starts. Those bytes are fixed by the producer's key instead, which the
// clientgen keys fold through build-infra: the Go describe declares both trees
// emptied, and the TypeScript build-generate owns .gen whole as a required
// output, so each tree holds what its producer's execution or restore wrote
// and nothing else. The committed generation contract, clientgen.json, and the
// committed sidecar schema/openapi.json are files no task writes, and an edit
// to either moves the key.
func TestClientgenKeyIgnoresTheDescribeOwnedTrees(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "clientgen-key-reads-no-run-written-tree", "clientgen-key-ignores-the-describe-owned-trees")
	clientgen := loadShippedExtension(t, "tooling/clientgen-extension", "@putnami/clientgen")

	for _, task := range clientgenTasks {
		t.Run(task.name, func(t *testing.T) {
			f := newClientgenKeyFixture(t)
			before := f.key(t, clientgen, task.name, task.job)
			writeTestFile(t, filepath.Join(f.dir, ".gen", "schema", "openapi.json"), "{\"openapi\":\"3.1.0\",\"paths\":{}}\n")
			writeTestFile(t, filepath.Join(f.dir, ".gen", "schema", "openapi.json.gz"), "compressed")
			writeTestFile(t, filepath.Join(f.dir, ".gen", "clientgen", "config.json"), "{\"targets\":[\"go\"]}\n")
			if after := f.key(t, clientgen, task.name, task.job); after != before {
				t.Fatal("a file a build of the run writes under .gen moved the key")
			}

			writeTestFile(t, filepath.Join(f.dir, "clientgen.json"), "{\"targets\":[\"go\"]}\n")
			contract := f.key(t, clientgen, task.name, task.job)
			if contract == before {
				t.Fatal("an edit to the committed generation contract left the key unmoved")
			}
			writeTestFile(t, filepath.Join(f.dir, "schema", "openapi.json"), "{\"openapi\":\"3.1.1\"}\n")
			if f.key(t, clientgen, task.name, task.job) == contract {
				t.Fatal("an edit to the committed sidecar contract left the key unmoved")
			}
		})
	}

	// The two trees the keys leave out are fixed by a producer's key, which
	// they fold: the Go build-describe empties both before it executes and on
	// a restore of an entry that records them empty.
	goExtension := loadShippedExtension(t, "go/extension", "@putnami/go")
	describe := &jobmodel.ScheduledJob{
		Project:   &workspace.Project{ID: "/provider", Name: "provider", Path: "provider"},
		Extension: goExtension,
		Step:      &extension.PipelineStep{Task: "build-describe"},
		JobDef:    &extension.JobDefinition{Name: "build~describe", Cache: true},
	}
	emptied := cachekey.EmptiedOutputIDs(describe)
	for _, id := range []string{"clientgen", "schema"} {
		if !slices.Contains(emptied, id) {
			t.Errorf("build-describe empties %v, want it to empty %q: a clientgen key that leaves the tree out would omit bytes no folded key fixes", emptied, id)
		}
	}

	// The TypeScript build-generate writes both trees inside .gen. It owns .gen
	// whole: the output is required, so the engine empties it, and it cedes
	// neither tree to another task. An optional .gen, or a cede of either tree,
	// would leave bytes there that no folded key fixes.
	typescript := loadShippedExtension(t, "typescript/extension", "@putnami/typescript")
	generate := &jobmodel.ScheduledJob{
		Project:   &workspace.Project{ID: "/provider", Name: "provider", Path: "provider"},
		Extension: typescript,
		Step:      &extension.PipelineStep{Task: "build-generate"},
		JobDef:    &extension.JobDefinition{Name: "build~generate", Cache: true},
	}
	var gen *cachekey.EmptiedOutputDeclaration
	for _, declared := range cachekey.EmptiedOutputDeclarations(generate) {
		if declared.Rel == ".gen" {
			gen = &declared
		}
	}
	if gen == nil {
		t.Fatalf("the TypeScript build-generate empties %v, want it to empty .gen", cachekey.EmptiedOutputIDs(generate))
	}
	if gen.Output.OptionalEmpty {
		t.Errorf("the TypeScript build-generate .gen output %q is optional: a run that writes nothing there would keep an earlier run's trees", gen.ID)
	}
	atOrUnder := func(rel, root string) bool { return rel == root || strings.HasPrefix(rel, root+"/") }
	for _, kept := range cachekey.OutputSkippedSubpaths(gen.Rel, gen.Output) {
		for _, tree := range []string{"schema", "clientgen"} {
			if atOrUnder(tree, kept) || atOrUnder(kept, tree) {
				t.Errorf("the TypeScript build-generate .gen output keeps %q, which overlaps .gen/%s: the engine would leave bytes there that no folded key fixes", kept, tree)
			}
		}
	}
}

// A project whose describe is not planned, such as a Go provider whose go.mod
// does not name go.putnami.dev/app, can commit its generation contract at
// .gen/clientgen/config.json, and every reader takes that file before
// clientgen.json. No task of the run empties or restores it there, so no
// folded key fixes it, and each key reads it as a Git candidate. A committed
// file is a candidate: tracking it and editing it move the key. A built one in
// a workspace that ignores .gen is not, so it never moves the key.
func TestClientgenKeyReadsAContractTheProjectCommitsUnderGen(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "clientgen-key-reads-no-run-written-tree", "clientgen-key-reads-a-contract-the-project-commits-under-gen")
	clientgen := loadShippedExtension(t, "tooling/clientgen-extension", "@putnami/clientgen")

	for _, task := range clientgenTasks {
		t.Run(task.name, func(t *testing.T) {
			f := newClientgenKeyFixture(t)
			contract := filepath.Join(f.dir, ".gen", "clientgen", "config.json")
			before := f.key(t, clientgen, task.name, task.job)
			writeTestFile(t, contract, "{\"targets\":[\"go\"]}\n")
			if f.key(t, clientgen, task.name, task.job) != before {
				t.Fatal("an ignored, built .gen/clientgen/config.json moved the key")
			}
			writeTestFile(t, contract, "{\"targets\":[\"ts\"]}\n")
			if f.key(t, clientgen, task.name, task.job) != before {
				t.Fatal("an edit to an ignored, built .gen/clientgen/config.json moved the key")
			}

			treeKeyedGit(t, f.root, "add", "-f", "provider/.gen/clientgen/config.json")
			tracked := f.key(t, clientgen, task.name, task.job)
			if tracked == before {
				t.Fatal("a committed .gen/clientgen/config.json left the key unmoved")
			}
			writeTestFile(t, contract, "{\"targets\":[\"go\",\"ts\"]}\n")
			if f.key(t, clientgen, task.name, task.job) == tracked {
				t.Fatal("an edit to a committed .gen/clientgen/config.json left the key unmoved")
			}
		})
	}
}

// clientgenPlan is the clientgen command planned with the shipped Go and
// clientgen manifests on one Go provider whose go.mod names
// go.putnami.dev/app, so describe is planned.
type clientgenPlan struct {
	ws      *workspace.Workspace
	dir     string
	planned []*jobmodel.ScheduledJob
}

func planClientgenOnAGoProvider(t *testing.T) *clientgenPlan {
	t.Helper()
	isolateTreeKeyedGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	treeKeyedGit(t, root, "init", "-q", "-b", "main")
	writeTestFile(t, filepath.Join(root, ".gitignore"), ".gen/\n.putnami/\n")
	dir := filepath.Join(root, "provider")
	writeTestFile(t, filepath.Join(dir, "go.mod"), "module example.com/provider\n\ngo 1.26\n\nrequire go.putnami.dev/app v0.0.0\n")
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main\n\nimport _ \"go.putnami.dev/app\"\n\nfunc main() {}\n")
	writeTestFile(t, filepath.Join(dir, "schema", "openapi.json"), "{\"openapi\":\"3.1.0\"}\n")
	project := &workspace.Project{
		ID: "/provider", Name: "provider", Path: "provider", Type: "application",
		Extensions: []string{"@putnami/go", "@putnami/clientgen"},
		Config:     wsproto.LoadProjectConfig(dir),
	}
	ws := testWorkspace(root, project)
	exts := []*extension.ExtensionDescription{
		loadShippedExtension(t, "go/extension", "@putnami/go"),
		loadShippedExtension(t, "tooling/clientgen-extension", "@putnami/clientgen"),
	}
	planned, err := Plan(ws, []string{"clientgen"}, ws.Projects, exts, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan clientgen: %v", err)
	}
	return &clientgenPlan{ws: ws, dir: dir, planned: planned}
}

// job returns the planned job of the given name.
func (p *clientgenPlan) job(t *testing.T, name string) *jobmodel.ScheduledJob {
	t.Helper()
	for _, job := range p.planned {
		if job.JobDef.Name == name {
			return job
		}
	}
	names := make([]string, 0, len(p.planned))
	for _, job := range p.planned {
		names = append(names, job.JobDef.Name)
	}
	sort.Strings(names)
	t.Fatalf("the clientgen plan has no %s job; it plans %v", name, names)
	return nil
}

// startKeys keys the plan the way a run keys each job when it starts: in
// dependency order, each cacheable job on the tree its dependencies left, then
// after(job, keys) stands for what the job writes. A fresh cache manager
// stands for the run's own process.
func (p *clientgenPlan) startKeys(t *testing.T, after func(job *jobmodel.ScheduledJob, keys map[string]string)) map[string]string {
	t.Helper()
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(p.ws.Root, ".putnami", "start")))
	cache.SetInputResolver(noListedFiles{})
	byKey, indegree, dependents := cachekey.FunctionalDegrees(p.planned)
	var ready []string
	for _, job := range p.planned {
		if indegree[job.Key()] == 0 {
			ready = append(ready, job.Key())
		}
	}
	keys := make(map[string]string, len(p.planned))
	for len(ready) > 0 {
		sort.Strings(ready)
		job := byKey[ready[0]]
		ready = ready[1:]
		if cachekey.IsCacheEnabled(job, cachekey.CacheBypass{}) {
			hash, err := cachekey.ComputeJobCacheHash(p.ws, job, nil, nil, cache, keys)
			if err != nil {
				t.Fatalf("start key of %s: %v", job.Key(), err)
			}
			keys[job.Key()] = hash
		}
		if after != nil {
			after(job, keys)
		}
		for _, dependent := range dependents[job.Key()] {
			if indegree[dependent]--; indegree[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}
	return keys
}

// precomputedKeys keys the plan before any job starts, as a run does for its
// cache lookups.
func (p *clientgenPlan) precomputedKeys(t *testing.T) map[string]string {
	t.Helper()
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(p.ws.Root, ".putnami", "precompute")))
	cache.SetInputResolver(noListedFiles{})
	keys, err := cachekey.PrecomputeKeys(p.ws, p.planned, nil, nil, cache, cachekey.CacheBypass{})
	if err != nil {
		t.Fatalf("precompute keys: %v", err)
	}
	return keys
}

// In a real plan the clientgen key folds describe's key: the clientgen command
// depends on build, its generator job depends on build-infra, and build-infra
// depends on describe. A describe whose key moves therefore moves the
// clientgen key, which is what lets the clientgen key leave out the trees
// describe writes.
func TestClientgenKeyFoldsDescribesKeyInARealPlan(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "clientgen-key-reads-no-run-written-tree", "clientgen-key-folds-describes-key-in-a-real-plan")
	plan := planClientgenOnAGoProvider(t)
	describe := plan.job(t, "build~describe")
	infra := plan.job(t, "build~infra")
	for _, task := range clientgenTasks {
		generator := plan.job(t, task.job)
		if !slices.Contains(generator.DependsOn, infra.Key()) {
			t.Errorf("%s depends on %v, want it to depend on %s", generator.Key(), generator.DependsOn, infra.Key())
		}
	}
	if !slices.Contains(infra.DependsOn, describe.Key()) {
		t.Fatalf("%s depends on %v, want it to depend on %s", infra.Key(), infra.DependsOn, describe.Key())
	}

	unmoved := plan.startKeys(t, nil)
	moved := plan.startKeys(t, func(job *jobmodel.ScheduledJob, keys map[string]string) {
		if job.Key() == describe.Key() {
			keys[job.Key()] = "a describe key that moved"
		}
	})
	if moved[infra.Key()] == unmoved[infra.Key()] {
		t.Errorf("a moved describe key left the %s key unmoved", infra.Key())
	}
	for _, task := range clientgenTasks {
		generator := plan.job(t, task.job)
		if moved[generator.Key()] == "" || moved[generator.Key()] == unmoved[generator.Key()] {
			t.Errorf("a moved describe key left the %s key unmoved", generator.Key())
		}
	}
}

// A fresh checkout: describe executes in a provider whose .gen is empty and
// writes the generation contract and the OpenAPI document there. The key each
// clientgen job computes when it starts equals the key the run computed before
// any job started, so the job is served the entry the run looked up for it.
func TestClientgenStartKeyEqualsThePrecomputedKeyWhenDescribeWrites(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "clientgen-key-reads-no-run-written-tree", "clientgen-start-key-equals-the-precomputed-key-when-describe-writes")
	plan := planClientgenOnAGoProvider(t)
	describe := plan.job(t, "build~describe")
	if _, err := os.Stat(filepath.Join(plan.dir, ".gen")); !os.IsNotExist(err) {
		t.Fatalf("the provider starts with a .gen tree: %v", err)
	}
	precomputed := plan.precomputedKeys(t)

	wrote := false
	started := plan.startKeys(t, func(job *jobmodel.ScheduledJob, _ map[string]string) {
		if job.Key() != describe.Key() {
			return
		}
		wrote = true
		writeTestFile(t, filepath.Join(plan.dir, ".gen", "clientgen", "config.json"), "{\"targets\":[\"go\",\"ts\"]}\n")
		writeTestFile(t, filepath.Join(plan.dir, ".gen", "schema", "openapi.json"), "{\"openapi\":\"3.1.0\",\"paths\":{}}\n")
		writeTestFile(t, filepath.Join(plan.dir, ".gen", "schema", "openapi.json.gz"), "compressed")
	})
	if !wrote {
		t.Fatal("the run never reached describe")
	}
	for _, task := range clientgenTasks {
		key := plan.job(t, task.job).Key()
		if precomputed[key] == "" {
			t.Fatalf("the run precomputed no key for %s; it keyed %v", key, slices.Sorted(maps.Keys(precomputed)))
		}
		if started[key] != precomputed[key] {
			t.Errorf("%s starts with key %s, but the run looked up %s: a file written between the two computations reached the key", key, started[key], precomputed[key])
		}
	}
}

// noListedFiles answers every input selector with no file: the provider's Go
// sources embed nothing, so the Go extension's answers list none.
type noListedFiles struct{}

func (noListedFiles) ResolveInput(store.InputQuery) (store.InputExpansion, error) {
	return store.InputExpansion{}, nil
}

func (noListedFiles) PrimeInputs([]store.InputQuery) {}
