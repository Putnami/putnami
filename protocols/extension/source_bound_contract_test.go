package extension

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
)

// A reader before rung 10 stores and looks up a source-bound entry at its
// cache key alone, not at a key that also binds the project's source binding,
// so it would serve outputs that embed another tree's binding. A manifest with
// a task that declares cache.sourceBound therefore needs rung 10, and a lower
// stamp is refused with the reason.
func TestASourceBoundTaskRequiresTheSourceBoundCacheContract(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "each-additive-vocabulary-requires-its-contract")
	manifest := []byte(`{"name":"@test/go","cliContract":9,"commands":{"build":{"run":[{"id":"describe","task":"describe"}]}},"tasks":{"describe":{"kind":"command","command":"echo","inputs":{"sources":{"from":"project","files":["**/*.go"]}},"cache":{"enabled":true,"deterministic":true,"sourceBound":true}}}}`)
	_, err := NegotiateManifest("go.json", manifest)
	if err == nil {
		t.Fatal("contract-9 stamp accepted a source-bound task")
	}
	for _, fragment := range []string{"requires 10 for its source-bound cache policy", "re-package the extension"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("refusal %q does not name %q", err, fragment)
		}
	}

	manifest = []byte(strings.Replace(string(manifest), `"cliContract":9`, `"cliContract":10`, 1))
	m, err := NegotiateManifest("go.json", manifest)
	if err != nil {
		t.Fatalf("contract-10 stamp refused: %v", err)
	}
	if !m.Tasks["describe"].Cache.SourceBound {
		t.Fatal("cache.sourceBound did not parse")
	}
	if got := RequiredCLIContract(m); got != protocolcli.SourceBoundCacheContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.SourceBoundCacheContract)
	}
	if got := RequiredLoadContract(m); got != protocolcli.SourceBoundCacheContract {
		t.Fatalf("RequiredLoadContract = %d, want %d: the rung binds every origin", got, protocolcli.SourceBoundCacheContract)
	}
}

// Every task counts, and a task that does not declare the field, or declares it
// false, leaves the floor where its other vocabulary puts it.
func TestRequiredCLIContractRaisesToSourceBoundOnlyForADeclaringTask(t *testing.T) {
	spectest.Proves(t, "extension/manifest-contract", "an-additive-vocabulary-raises-the-contract-floor", "the-floor-rises-only-for-a-manifest-that-uses-the-vocabulary")
	bound := TaskDefinition{Kind: "command", Command: "echo", Cache: &TaskCachePolicy{SourceBound: true}}
	gitInput := TaskDefinition{Kind: "command", Command: "echo", Inputs: map[string]TaskInputPort{
		"sources": {From: TaskInputFromProject, Files: []string{"git:**"}},
	}}
	unbound := TaskDefinition{Kind: "command", Command: "echo", Cache: &TaskCachePolicy{Deterministic: true}}
	disabled := TaskDefinition{Kind: "command", Command: "echo"}

	for _, test := range []struct {
		name  string
		tasks map[string]TaskDefinition
		want  int
	}{
		{"no cache policy", map[string]TaskDefinition{"a": disabled}, protocolcli.CurrentContract},
		{"sourceBound false", map[string]TaskDefinition{"a": unbound}, protocolcli.CurrentContract},
		{"beside a git input rung", map[string]TaskDefinition{"a": gitInput, "b": bound}, protocolcli.SourceBoundCacheContract},
		{"git input only", map[string]TaskDefinition{"a": gitInput, "b": unbound}, protocolcli.GitInputModeContract},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Map iteration order is random; repeat so an early return on a
			// lower rung would show.
			for range 32 {
				if got := RequiredCLIContract(&Manifest{Tasks: test.tasks}); got != test.want {
					t.Fatalf("RequiredCLIContract = %d, want %d", got, test.want)
				}
			}
		})
	}
}

// The field is part of the object form of "cache" only. Leaving it out keeps
// the JSON of every other task's policy unchanged, which keeps their contract
// digests, and so their cache keys, unchanged.
func TestSourceBoundIsOmittedWhenFalse(t *testing.T) {
	var task TaskDefinition
	if err := json.Unmarshal([]byte(`{"kind":"command","command":"echo","cache":{"enabled":true,"deterministic":true}}`), &task); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if task.Cache.SourceBound {
		t.Fatal("an absent sourceBound parsed as true")
	}
	data, err := json.Marshal(task.Cache)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "sourceBound") {
		t.Fatalf("a false sourceBound is serialized: %s", data)
	}

	if err := json.Unmarshal([]byte(`{"kind":"command","command":"echo","cache":true}`), &task); err != nil {
		t.Fatalf("parse bool cache: %v", err)
	}
	if task.Cache != nil && task.Cache.SourceBound {
		t.Fatal("the bool form of cache declared sourceBound")
	}
}

// The published JSON schema accepts the field, so an authored manifest that
// declares it validates against the schema the CLI parses with.
func TestSchemaDeclaresSourceBound(t *testing.T) {
	properties := schemaDefProperties(t, "taskCachePolicy")
	if !slices.Contains(properties, "sourceBound") {
		t.Fatalf("taskCachePolicy schema has no sourceBound property: %v", properties)
	}
}
