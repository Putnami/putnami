package planner

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A consumer-only selection: a TypeScript application that imports a generated
// TypeScript client, planned alone. The client project holds no test file, so
// no `test` job of the plan runs in it, and the consumer reaches the client's
// generator only through the dependency its `test` command declares across the
// dependency closure (`^client-generate`).

const (
	consumerSelectionClient    = "/clients/items"
	consumerSelectionConsumer  = "/consumer"
	consumerSelectionGenerator = consumerSelectionClient + ":client-generate~ts"
)

// consumerSelectionTree builds a Git work tree that ignores the generated
// client, as a workspace does, and returns its root.
func consumerSelectionTree(t *testing.T) string {
	t.Helper()
	isolateTreeKeyedGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	treeKeyedGit(t, root, "init", "-q", "-b", "main")
	writeTestFile(t, filepath.Join(root, ".gitignore"), ".gen/\n.putnami/\n/clients/items/src/\n")
	writeProjectFile(t, root, "clients/items", "package.json", "{\"name\":\"@example/items-client\"}\n")
	writeProjectFile(t, root, "clients/items", "putnami.json",
		"{\"name\":\"@example/items-client\",\"generatedFrom\":{\"service\":\"catalog.items\",\"language\":\"ts\"}}\n")
	writeProjectFile(t, root, "clients/items", "src/index.ts", "export class ItemsClient {}\n")
	writeProjectFile(t, root, "consumer", "package.json", "{\"name\":\"@example/consumer\"}\n")
	writeProjectFile(t, root, "consumer", "tsconfig.json", "{}\n")
	writeProjectFile(t, root, "consumer", "src/main.ts",
		"import { ItemsClient } from '@example/items-client';\n\nexport const client = new ItemsClient();\n")
	writeProjectFile(t, root, "consumer", "test/main.test.ts", "import '../src/main';\n")
	return root
}

// planTestOnAConsumerAlone loads the tree as a command does, with the digest
// of the provider's committed contract the client's binding holds at that
// load, and plans `test` on the consumer alone with the given TypeScript
// extension and the shipped clientgen manifest.
func planTestOnAConsumerAlone(
	t *testing.T, root string, typescript *extension.ExtensionDescription, contractSHA256 string,
) *clientgenPlan {
	t.Helper()
	client := &workspace.Project{
		ID: consumerSelectionClient, Name: "@example/items-client", Path: "clients/items", Type: "library",
		Extensions: []string{typescript.Name},
		Config:     wsproto.LoadProjectConfig(filepath.Join(root, "clients", "items")),
		GeneratedClient: &workspace.GeneratedClientBinding{
			ServiceID:      "catalog.items",
			Language:       "ts",
			ContractSHA256: contractSHA256,
		},
	}
	consumer := &workspace.Project{
		ID: consumerSelectionConsumer, Name: "@example/consumer", Path: "consumer", Type: "application",
		Extensions:   []string{typescript.Name},
		Dependencies: []string{consumerSelectionClient},
		Config:       wsproto.LoadProjectConfig(filepath.Join(root, "consumer")),
	}
	ws := testWorkspace(root, client, consumer)
	exts := []*extension.ExtensionDescription{
		typescript,
		loadShippedExtension(t, "tooling/clientgen-extension", "@putnami/clientgen"),
	}
	planned, err := Plan(ws, []string{"test"}, []*workspace.Project{consumer}, exts, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan test on the consumer alone: %v", err)
	}
	if err := ValidatePlanDAG(planned); err != nil {
		t.Fatalf("the consumer-only plan is not a DAG: %v", err)
	}
	return &clientgenPlan{ws: ws, dir: filepath.Join(root, "consumer"), planned: planned}
}

// functionalEdges indexes the DependsOn edges of a plan, the ones a cache key
// folds, by the waiting job.
func functionalEdges(planned []*jobmodel.ScheduledJob) map[string][]string {
	edges := make(map[string][]string, len(planned))
	for _, job := range planned {
		edges[job.Key()] = slices.Clone(job.DependsOn)
	}
	return edges
}

// typescriptWithoutClosureGeneration is the shipped TypeScript extension whose
// `test` command depends on client-generate in its own project only.
func typescriptWithoutClosureGeneration(t *testing.T) *extension.ExtensionDescription {
	t.Helper()
	typescript := loadShippedExtension(t, "typescript/extension", "@putnami/typescript")
	test := typescript.Jobs["test"]
	if test == nil || !slices.Contains(test.CommandDependsOn, "^client-generate") {
		t.Fatalf("the shipped TypeScript test command does not depend on ^client-generate: %+v", test)
	}
	test.CommandDependsOn = slices.DeleteFunc(slices.Clone(test.CommandDependsOn), func(dependency string) bool {
		return dependency == "^client-generate"
	})
	return typescript
}

// TestPlan_AConsumerOnlySelectionGeneratesItsClientsAndKeysOnTheirContract
// plans the shipped manifests. A `test` of the consumer alone plans the
// generation of the client the consumer imports, and the consumer's type check
// and tests wait for it over functional edges. A change of the provider's
// committed contract, which the client's identity folds, moves the generator's
// key and the key of every consumer task behind it, so neither a stale client
// nor an earlier result is served. Without the dependency across the closure
// the plan holds no generator: no other job reaches the client project.
func TestPlan_AConsumerOnlySelectionGeneratesItsClientsAndKeysOnTheirContract(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "dependency-plan",
		"a-consumer-only-selection-generates-its-clients-and-keys-on-their-contract")
	const (
		contract      = "1111111111111111111111111111111111111111111111111111111111111111"
		movedContract = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	root := consumerSelectionTree(t)
	typescript := loadShippedExtension(t, "typescript/extension", "@putnami/typescript")
	selection := planTestOnAConsumerAlone(t, root, typescript, contract)

	keys := planKeySet(selection.planned)
	if !keys[consumerSelectionGenerator] {
		t.Fatalf("a consumer-only test plans no %s; it plans %v", consumerSelectionGenerator, keys)
	}
	edges := functionalEdges(selection.planned)
	consumerTasks := []string{
		consumerSelectionConsumer + ":test~generate",
		consumerSelectionConsumer + ":test~types",
		consumerSelectionConsumer + ":test~test",
	}
	for _, task := range consumerTasks {
		if !keys[task] {
			t.Fatalf("the consumer-only plan has no %s; it plans %v", task, keys)
		}
		if !planReaches(edges, task, consumerSelectionGenerator) {
			t.Errorf("%s does not depend on %s: it compiles the client without generating it, and its key ignores it",
				task, consumerSelectionGenerator)
		}
	}

	before := selection.precomputedKeys(t)
	if again := planTestOnAConsumerAlone(t, root, typescript, contract).precomputedKeys(t); !maps.Equal(again, before) {
		t.Fatalf("an unchanged contract moved a key between two loads: %v, then %v", before, again)
	}
	after := planTestOnAConsumerAlone(t, root, typescript, movedContract).precomputedKeys(t)
	for _, task := range append([]string{consumerSelectionGenerator}, consumerTasks...) {
		if before[task] == "" {
			t.Fatalf("the run computed no key for %s", task)
		}
		if after[task] == before[task] {
			t.Errorf("a contract change left the key of %s unmoved: the run replays the result of the earlier contract", task)
		}
	}

	stale := planTestOnAConsumerAlone(t, root, typescriptWithoutClosureGeneration(t), contract)
	if staleKeys := planKeySet(stale.planned); staleKeys[consumerSelectionGenerator] {
		t.Fatalf("without ^client-generate the plan still holds %s; the fixture no longer shows what the dependency adds: %v",
			consumerSelectionGenerator, staleKeys)
	}
}
