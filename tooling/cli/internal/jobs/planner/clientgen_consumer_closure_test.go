package planner

import (
	"path/filepath"
	"sort"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// A consumer selected alone reads every generated client of its dependency
// closure, whether or not that client plans a step of the consumer's command:
// a TypeScript client with no test file activates no test step, and a library
// client takes no serve step. Each of the consumer's jobs reaches the client's
// generation over functional edges, so the client is regenerated before the
// consumer reads it, and the generation's key, which folds the provider's
// committed contract digest, reaches the consumer's keys.

const (
	closureProviderContract = "services/items/schema/openapi.json"
	closureContract         = `{"openapi":"3.1.0","x-putnami-client":{"protocolVersion":1,` +
		`"service":{"id":"items","audience":"api://items"}},"paths":{}}`
	closureMovedContract = `{"openapi":"3.1.0","x-putnami-client":{"protocolVersion":1,` +
		`"service":{"id":"items","audience":"api://items"}},"paths":{"/items":{}}}`
)

// closureLanguage is one language's client and consumer in the fixture.
type closureLanguage struct {
	name      string
	client    string
	generator string
	consumer  string
	// reader is a consumer job of each command that compiles the client.
	readers map[string]string
}

var closureLanguages = []closureLanguage{
	{
		name:      "typescript",
		client:    "/clients/ts",
		generator: "/clients/ts:client-generate~ts",
		consumer:  "/consumers/ts",
		readers: map[string]string{
			"test":  "/consumers/ts:test~types",
			"serve": "/consumers/ts:serve~serve",
			"build": "/consumers/ts:build~types",
		},
	},
	{
		// The consumer reaches the client through a library with no test file
		// and no serve step, which plans no job of test or serve itself.
		name:      "typescript-chain",
		client:    "/clients/ts",
		generator: "/clients/ts:client-generate~ts",
		consumer:  "/consumers/ts-chain",
		readers: map[string]string{
			"test":  "/consumers/ts-chain:test~types",
			"serve": "/consumers/ts-chain:serve~serve",
			"build": "/consumers/ts-chain:build~types",
		},
	},
	{
		name:      "go",
		client:    "/clients/go",
		generator: "/clients/go:client-generate~go",
		consumer:  "/consumers/go",
		readers: map[string]string{
			"test":  "/consumers/go:test~test",
			"serve": "/consumers/go:serve~serve",
			"build": "/consumers/go:build~compile",
		},
	},
}

// writeClosureWorkspace writes a Go provider that commits its contract, a
// TypeScript and a Go library client of it, none with a test file, a consumer
// of each client in its language, and a TypeScript consumer that reaches the
// client through a library with no test file. The TypeScript consumer turns on
// the type check of its tests, the step the stale client failed in. The
// clients' generated trees are ignored, as a generated client is built, not
// committed.
func writeClosureWorkspace(t *testing.T) string {
	t.Helper()
	isolateTreeKeyedGit(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	treeKeyedGit(t, root, "init", "-q", "-b", "main")
	files := map[string]string{
		".gitignore": ".gen/\n.putnami/\nclients/ts/src/\nclients/go/client.gen.go\n",
		"putnami.workspace.json": `{"name":"ws","includes":["services/items","clients/ts","clients/go",` +
			`"libs/ts","consumers/ts","consumers/ts-chain","consumers/go"]}`,

		"services/items/putnami.json": `{"name":"items","type":"application","extensions":["@putnami/go"]}`,
		"services/items/go.mod":       "module example.com/items\n\ngo 1.26\n",
		"services/items/main.go":      "package main\n\nfunc main() {}\n",
		closureProviderContract:       closureContract,

		"clients/ts/putnami.json": `{"name":"items-client","type":"library","extensions":["@putnami/typescript"],` +
			`"generatedFrom":{"service":"items","language":"ts"}}`,
		"clients/ts/package.json":  `{"name":"@example/items-client","main":"src/index.ts"}`,
		"clients/ts/tsconfig.json": `{"include":["src"]}`,

		"clients/go/putnami.json": `{"name":"items-go-client","type":"library","extensions":["@putnami/go"],` +
			`"generatedFrom":{"service":"items","language":"go"}}`,
		"clients/go/go.mod": "module example.com/clients/items\n\ngo 1.26\n",

		"consumers/ts/putnami.json": `{"name":"ts-consumer","type":"application","extensions":["@putnami/typescript"],` +
			`"dependencies":["/clients/ts"],"options":{"test":{"types":true}}}`,
		"consumers/ts/package.json":      `{"name":"@example/ts-consumer","dependencies":{"@example/items-client":"workspace:*"}}`,
		"consumers/ts/tsconfig.json":     `{"include":["src","test"]}`,
		"consumers/ts/src/main.ts":       "import { ItemsClient } from '@example/items-client';\n\nexport const client = ItemsClient;\n",
		"consumers/ts/test/main.test.ts": "import { client } from '../src/main';\n\nclient;\n",
		"libs/ts/putnami.json": `{"name":"items-lib","type":"library","extensions":["@putnami/typescript"],` +
			`"dependencies":["/clients/ts"]}`,
		"libs/ts/package.json":  `{"name":"@example/items-lib","main":"src/index.ts"}`,
		"libs/ts/tsconfig.json": `{"include":["src"]}`,
		"libs/ts/src/index.ts":  "export { ItemsClient } from '@example/items-client';\n",
		"consumers/ts-chain/putnami.json": `{"name":"ts-chain-consumer","type":"application",` +
			`"extensions":["@putnami/typescript"],"dependencies":["/libs/ts"],"options":{"test":{"types":true}}}`,
		"consumers/ts-chain/package.json":      `{"name":"@example/ts-chain-consumer","dependencies":{"@example/items-lib":"workspace:*"}}`,
		"consumers/ts-chain/tsconfig.json":     `{"include":["src","test"]}`,
		"consumers/ts-chain/src/main.ts":       "import { ItemsClient } from '@example/items-lib';\n\nexport const client = ItemsClient;\n",
		"consumers/ts-chain/test/main.test.ts": "import { client } from '../src/main';\n\nclient;\n",
		"consumers/go/putnami.json":            `{"name":"go-consumer","type":"application","extensions":["@putnami/go"],"dependencies":["/clients/go"]}`,
		"consumers/go/go.mod":                  "module example.com/consumer\n\ngo 1.26\n\nrequire example.com/clients/items v0.0.0\n",
		"consumers/go/main.go":                 "package main\n\nimport _ \"example.com/clients/items\"\n\nfunc main() {}\n",
		"consumers/go/main_test.go":            "package main\n\nimport \"testing\"\n\nfunc TestMain(t *testing.T) {}\n",
		"services/items/service/items.go":      "package service\n",
	}
	for rel, content := range files {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(rel)), content)
	}
	treeKeyedGit(t, root, "add", "-A")
	treeKeyedGit(t, root, "commit", "-q", "-m", "fixture")
	workspace.InvalidateLoadCache(root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(root) })
	return root
}

// closureExtensions are the shipped Go, TypeScript and clientgen manifests.
func closureExtensions(t *testing.T) []*extension.ExtensionDescription {
	t.Helper()
	return []*extension.ExtensionDescription{
		loadShippedExtension(t, "go/extension", "@putnami/go"),
		loadShippedExtension(t, "typescript/extension", "@putnami/typescript"),
		loadShippedExtension(t, "tooling/clientgen-extension", "@putnami/clientgen"),
	}
}

// planConsumer loads the workspace from disk, as a run does, so the client
// binds to the contract the provider commits now, and plans command for the
// consumer alone.
func planConsumer(t *testing.T, root string, exts []*extension.ExtensionDescription, command, consumer string) (*workspace.Workspace, []*jobmodel.ScheduledJob) {
	t.Helper()
	workspace.InvalidateLoadCache(root)
	ws, err := workspace.Load(root)
	if err != nil {
		t.Fatalf("load the workspace: %v", err)
	}
	selected := ws.ProjectByID(consumer)
	if selected == nil {
		t.Fatalf("the workspace has no %s; it holds %v", consumer, ws.ProjectPaths())
	}
	planned, err := Plan(ws, []string{command}, []*workspace.Project{selected}, exts, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan %s of %s: %v", command, consumer, err)
	}
	return ws, planned
}

// functionalAncestors is every job a job reaches over DependsOn alone, the
// edges whose keys fold into its own.
func functionalAncestors(byKey map[string]*jobmodel.ScheduledJob, job *jobmodel.ScheduledJob) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string(nil), job.DependsOn...)
	for len(stack) > 0 {
		key := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[key] {
			continue
		}
		seen[key] = true
		if upstream := byKey[key]; upstream != nil {
			stack = append(stack, upstream.DependsOn...)
		}
	}
	return seen
}

func sortedJobKeys(planned []*jobmodel.ScheduledJob) []string {
	keys := make([]string, 0, len(planned))
	for _, job := range planned {
		keys = append(keys, job.Key())
	}
	sort.Strings(keys)
	return keys
}

// TestAConsumerOnlySelectionPlansTheGenerationOfItsClosureClients plans test,
// serve and build of each consumer alone. The client's generation is planned,
// and the job each command compiles the client in reaches it over functional
// edges. Test and serve depend on the closure's generation, so every job of
// theirs reaches it. Build reaches it through the client's own build, so only
// the jobs that compile the client do: a Go module tidy check, which CI plans
// in place of tidy, reads no client.
func TestAConsumerOnlySelectionPlansTheGenerationOfItsClosureClients(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-consumer-keys-on-the-generation-of-its-clients",
		"a-consumer-only-selection-plans-the-generation-of-its-closure-clients")
	root := writeClosureWorkspace(t)
	exts := closureExtensions(t)
	for _, language := range closureLanguages {
		for _, command := range []string{"test", "serve", "build"} {
			t.Run(language.name+"-"+command, func(t *testing.T) {
				_, planned := planConsumer(t, root, exts, command, language.consumer)
				byKey := JobsByPlanKey(planned)
				if byKey[language.generator] == nil {
					t.Fatalf("%s of %s alone plans no %s, so the consumer reads a client older than its contract; "+
						"the plan holds %v", command, language.consumer, language.generator, sortedJobKeys(planned))
				}
				reader := requirePlanKey(t, byKey, language.readers[command])
				if !functionalAncestors(byKey, reader)[language.generator] {
					t.Errorf("%s does not reach %s over functional edges: its key holds nothing of the client",
						reader.Key(), language.generator)
				}
				if command == "build" {
					return
				}
				for _, job := range planned {
					if job.Project.ID != language.consumer || jobmodel.IsFinalizerJob(job) ||
						!cachekey.IsCacheEnabled(job, cachekey.CacheBypass{}) {
						continue
					}
					if !functionalAncestors(byKey, job)[language.generator] {
						t.Errorf("%s does not reach %s over functional edges", job.Key(), language.generator)
					}
				}
			})
		}
	}
}

// consumerKeys precomputes the plan's keys, as a run does before any job
// starts, with a fresh cache manager standing for a new process.
func consumerKeys(t *testing.T, ws *workspace.Workspace, planned []*jobmodel.ScheduledJob, label string) map[string]string {
	t.Helper()
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), label)))
	cache.SetInputResolver(noListedFiles{})
	keys, err := cachekey.PrecomputeKeys(ws, planned, nil, nil, cache, cachekey.CacheBypass{})
	if err != nil {
		t.Fatalf("precompute keys: %v", err)
	}
	return keys
}

// TestAContractEditMovesTheKeysOfTheConsumerJobs is the stale-replay
// acceptance: the type check of a TypeScript consumer, and the test of a Go
// one, take a new key when the provider's committed contract changes, so a
// verdict stored against the previous client is not served. An edit to the
// provider's implementation that leaves the contract alone keeps every key.
func TestAContractEditMovesTheKeysOfTheConsumerJobs(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "a-consumer-keys-on-the-generation-of-its-clients",
		"a-contract-edit-moves-the-keys-of-the-consumer-jobs")
	exts := closureExtensions(t)
	for _, language := range closureLanguages {
		for _, command := range []string{"test", "build"} {
			t.Run(language.name+"-"+command, func(t *testing.T) {
				root := writeClosureWorkspace(t)
				reader := language.readers[command]
				keyOf := func(label string) string {
					t.Helper()
					ws, planned := planConsumer(t, root, exts, command, language.consumer)
					key := consumerKeys(t, ws, planned, label)[reader]
					if key == "" {
						t.Fatalf("%s has no key in %v", reader, sortedJobKeys(planned))
					}
					return key
				}
				base := keyOf("base")

				writeTestFile(t, filepath.Join(root, "services", "items", "main.go"), "package main\n\nfunc main() { _ = 1 }\n")
				if got := keyOf("implementation"); got != base {
					t.Errorf("a provider implementation edit moved the key of %s", reader)
				}

				writeTestFile(t, filepath.Join(root, filepath.FromSlash(closureProviderContract)), closureMovedContract)
				if got := keyOf("contract"); got == base {
					t.Errorf("a contract edit left the key of %s unmoved: a verdict stored against the previous client replays", reader)
				}
			})
		}
	}
}
