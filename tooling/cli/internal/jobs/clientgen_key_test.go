package jobs

import (
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The clientgen keys name no file a build of the run writes. build-describe
// (Go) and build-generate (TypeScript) write .gen/schema and .gen/clientgen
// during the run, so a key that hashed them would differ between the value the
// run computes before any task starts and the value the task computes when it
// starts. Those bytes are fixed by the producer's key instead, which the
// clientgen keys fold through build-infra: the Go describe declares both trees
// emptied, so they hold what describe's execution or restore wrote and nothing
// else. The committed generation contract, clientgen.json, and the committed
// sidecar schema/openapi.json are files no task writes, and an edit to either
// moves the key.
func TestClientgenKeyIgnoresTheDescribeOwnedTrees(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "clientgen-key-reads-no-run-written-tree", "clientgen-key-ignores-the-describe-owned-trees")
	repoRoot := findJobsRepoRoot(t)
	clientgen := extension.LoadExtensionFromDir(filepath.Join(repoRoot, "tooling", "clientgen-extension"), "/tooling/clientgen-extension")
	if clientgen == nil {
		t.Fatal("load real clientgen extension manifest")
	}
	clientgen.LocalSource = false
	clientgen.RelPath = "tooling/clientgen-extension"

	for _, task := range []struct{ name, job string }{{"clientgen-go", "clientgen~generate-go"}, {"clientgen-ts", "clientgen~generate-ts"}} {
		t.Run(task.name, func(t *testing.T) {
			definition, ok := clientgen.Tasks[task.name]
			if !ok || definition.Cache == nil || definition.Cache.Key == nil {
				t.Fatalf("real clientgen manifest has no keyed %s task", task.name)
			}
			root := t.TempDir()
			provider := &workspace.Project{ID: "/provider", Name: "provider", Path: "provider"}
			ws := workspace.NewWorkspace(root, &wsproto.Config{}, []*workspace.Project{provider})
			ws.Graph = workspace.BuildGraph(ws.Projects)
			dir := filepath.Join(root, "provider")
			writeTestFile(t, filepath.Join(dir, "go.mod"), "module example.com/provider\n\ngo 1.26\n")
			writeTestFile(t, filepath.Join(dir, "package.json"), "{\"name\":\"provider\"}\n")
			writeTestFile(t, filepath.Join(dir, "schema", "openapi.json"), "{\"openapi\":\"3.1.0\"}\n")
			writeTestFile(t, filepath.Join(dir, "clientgen.json"), "{\"targets\":[\"go\",\"ts\"]}\n")
			job := &ScheduledJob{
				Project:   provider,
				Extension: clientgen,
				Step:      &extension.PipelineStep{Task: task.name},
				JobDef: &extension.JobDefinition{
					Name: task.job, CommandName: "clientgen", ExtensionName: clientgen.Name, Cache: true,
					TaskCachePolicy: &extension.TaskCachePolicy{Key: definition.Cache.Key},
				},
			}
			key := func() string {
				t.Helper()
				cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
				value, err := computeJobCacheHash(ws, job, nil, nil, cache, nil)
				if err != nil {
					t.Fatalf("compute %s key: %v", task.name, err)
				}
				return value
			}

			before := key()
			writeTestFile(t, filepath.Join(dir, ".gen", "schema", "openapi.json"), "{\"openapi\":\"3.1.0\",\"paths\":{}}\n")
			writeTestFile(t, filepath.Join(dir, ".gen", "schema", "openapi.json.gz"), "compressed")
			writeTestFile(t, filepath.Join(dir, ".gen", "clientgen", "config.json"), "{\"targets\":[\"go\"]}\n")
			if after := key(); after != before {
				t.Fatal("a file a build of the run writes under .gen moved the key")
			}

			writeTestFile(t, filepath.Join(dir, "clientgen.json"), "{\"targets\":[\"go\"]}\n")
			contract := key()
			if contract == before {
				t.Fatal("an edit to the committed generation contract left the key unmoved")
			}
			writeTestFile(t, filepath.Join(dir, "schema", "openapi.json"), "{\"openapi\":\"3.1.1\"}\n")
			if key() == contract {
				t.Fatal("an edit to the committed sidecar contract left the key unmoved")
			}
		})
	}

	// The two trees the keys leave out are fixed by describe's key, which
	// they fold: the Go build-describe empties both before it executes and on
	// a restore of an entry that records them empty.
	goExtension := extension.LoadExtensionFromDir(filepath.Join(repoRoot, "go", "extension"), "/go/extension")
	if goExtension == nil {
		t.Fatal("load real Go extension manifest")
	}
	describe := &ScheduledJob{
		Project:   &workspace.Project{ID: "/provider", Name: "provider", Path: "provider"},
		Extension: goExtension,
		Step:      &extension.PipelineStep{Task: "build-describe"},
		JobDef:    &extension.JobDefinition{Name: "build~describe", Cache: true},
	}
	emptied := emptiedOutputIDs(describe)
	for _, id := range []string{"clientgen", "schema"} {
		if !slices.Contains(emptied, id) {
			t.Errorf("build-describe empties %v, want it to empty %q: a clientgen key that leaves the tree out would omit bytes no folded key fixes", emptied, id)
		}
	}
}
