package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// digestTask is a fully-populated v3 task used as the mutation base: every
// contract member is non-zero so a mutation that zeroes or changes one is
// guaranteed to alter the marshaled view if (and only if) the digest covers it.
func digestTask() TaskDefinition {
	enabled := true
	return TaskDefinition{
		Description:    "builds the thing",
		Visibility:     "public",
		Kind:           "exec",
		Command:        "tool",
		Args:           []string{"build", "--fast"},
		Cwd:            "sub",
		Env:            map[string]string{"MODE": "release"},
		Output:         "text",
		TimeoutMs:      120000,
		InputSchemaRef: "schemas/in.json",
		Inputs: map[string]TaskInputPort{
			"sources": {From: "project", Files: []string{"src/**/*.go"}},
		},
		Outputs: map[string]TaskOutputPort{
			"report": {Kind: "file", Path: "out/report.json", Description: "the report"},
		},
		Writes: []ResourceRef{{ID: "gen", Scope: ResourceScopeProject}},
		Reads:  []ResourceRef{{ID: "sources", Scope: ResourceScopeProject}},
		Cache: &TaskCachePolicy{
			Enabled: &enabled,
			Key:     &TaskCacheKey{Files: []string{"src/**"}},
		},
		Batchable: &TaskBatchPolicy{Tool: "tool", MaxProjects: 4},
		Declares: &TaskDeclaration{
			Outputs: map[string]DeclaredOutput{
				"report": {Kind: OutputKindFile, Path: "out/report.json", Description: "the report"},
			},
			Effects:        []string{"network"},
			MutatesSources: false,
		},
	}
}

func TestTaskContractDigest_DeterministicAcrossParses(t *testing.T) {
	// The same task authored with members in two different orders must digest
	// identically — canonical form cannot depend on authoring order or on map
	// iteration.
	a := []byte(`{"name":"x","version":"1.0.0","tasks":{"t":{
		"kind":"exec","command":"tool","args":["a","b"],
		"env":{"B":"2","A":"1"},
		"declares":{"effects":["network","locks"],"outputs":{"o":{"kind":"file","path":"out/a.txt"}}}
	}}}`)
	b := []byte(`{"version":"1.0.0","name":"x","tasks":{"t":{
		"declares":{"outputs":{"o":{"path":"out/a.txt","kind":"file"}},"effects":["locks","network"]},
		"env":{"A":"1","B":"2"},
		"args":["a","b"],"command":"tool","kind":"exec"
	}}}`)

	ma, diags := ParseManifest(a)
	if ma == nil {
		t.Fatalf("parse a: %v", diags)
	}
	mb, diags := ParseManifest(b)
	if mb == nil {
		t.Fatalf("parse b: %v", diags)
	}

	da := TaskContractDigest(ma.Tasks["t"])
	for i := 0; i < 20; i++ {
		if got := TaskContractDigest(ma.Tasks["t"]); got != da {
			t.Fatalf("digest unstable across calls: %s then %s", da, got)
		}
	}
	if db := TaskContractDigest(mb.Tasks["t"]); db != da {
		t.Errorf("authoring order moved the digest: %s vs %s", da, db)
	}
	if !strings.HasPrefix(da, "tc1:") {
		t.Errorf("digest %q does not carry the tc1 format prefix", da)
	}
}

func TestTaskContractDigest_SelfNormalizing(t *testing.T) {
	// An un-normalized task (unsorted effects, uncleaned path, cache policy
	// left for derivation) must digest identically to its normalized form:
	// the CLI's negotiating load path does not run NormalizeManifest.
	raw := TaskDefinition{
		Kind:    "exec",
		Command: "tool",
		Inputs: map[string]TaskInputPort{
			"sources": {From: "project", Files: []string{"src/**"}},
		},
		Declares: &TaskDeclaration{
			Outputs: map[string]DeclaredOutput{
				"o": {Kind: OutputKindDirectory, Path: "out/./nested/../nested"},
			},
			Effects: []string{"network", "locks"},
		},
	}
	normalized, err := deepCopyTask(raw)
	if err != nil {
		t.Fatal(err)
	}
	NormalizeTask(&normalized)
	if normalized.Cache == nil {
		t.Fatal("positive control: NormalizeTask should have derived a cache policy from inputs")
	}
	if got, want := normalized.Declares.Outputs["o"].Path, "out/nested"; got != want {
		t.Fatalf("positive control: normalized path = %q, want %q", got, want)
	}

	if dr, dn := TaskContractDigest(raw), TaskContractDigest(normalized); dr != dn {
		t.Errorf("normalization moved the digest: raw %s vs normalized %s", dr, dn)
	}
}

func TestTaskContractDigest_PresentationalMembersDoNotMove(t *testing.T) {
	base := TaskContractDigest(digestTask())

	edited := digestTask()
	edited.Description = "totally different prose"
	edited.Visibility = "internal"
	port := edited.Outputs["report"]
	port.Description = "changed"
	edited.Outputs["report"] = port
	out := edited.Declares.Outputs["report"]
	out.Description = "changed"
	edited.Declares.Outputs["report"] = out

	if got := TaskContractDigest(edited); got != base {
		t.Errorf("a docs-only edit moved the digest: %s vs %s", base, got)
	}
}

func TestTaskContractDigest_ContractMembersMove(t *testing.T) {
	// Every contract member must move the digest. Each case is a compiling
	// mutation; the presentational test above is the negative control.
	cases := map[string]func(*TaskDefinition){
		"command":  func(t *TaskDefinition) { t.Command = "other" },
		"args":     func(t *TaskDefinition) { t.Args = []string{"build"} },
		"argOrder": func(t *TaskDefinition) { t.Args = []string{"--fast", "build"} },
		"cwd":      func(t *TaskDefinition) { t.Cwd = "elsewhere" },
		"env":      func(t *TaskDefinition) { t.Env["MODE"] = "debug" },
		"timeout":  func(t *TaskDefinition) { t.TimeoutMs = 1 },
		"kind":     func(t *TaskDefinition) { t.Kind = "daemon" },
		"inputs": func(t *TaskDefinition) {
			t.Inputs["sources"] = TaskInputPort{From: "project", Files: []string{"lib/**"}}
		},
		"writes":      func(t *TaskDefinition) { t.Writes[0].ID = "other" },
		"reads":       func(t *TaskDefinition) { t.Reads = nil },
		"resources":   func(t *TaskDefinition) { t.Resources = map[string]int{"db-connections": 230} },
		"cachePolicy": func(t *TaskDefinition) { t.Cache.Deterministic = true },
		"batchPolicy": func(t *TaskDefinition) { t.Batchable.MaxProjects = 9 },
		"declaredPath": func(t *TaskDefinition) {
			o := t.Declares.Outputs["report"]
			o.Path = "out/other.json"
			t.Declares.Outputs["report"] = o
		},
		"declaredKind": func(t *TaskDefinition) {
			o := t.Declares.Outputs["report"]
			o.Kind = OutputKindDirectory
			t.Declares.Outputs["report"] = o
		},
		"declaredScope": func(t *TaskDefinition) {
			o := t.Declares.Outputs["report"]
			o.Scope = OutputScopeInvocation
			t.Declares.Outputs["report"] = o
		},
		"declaredSensitive": func(t *TaskDefinition) {
			o := t.Declares.Outputs["report"]
			o.Sensitive = true
			t.Declares.Outputs["report"] = o
		},
		"optionalEmpty": func(t *TaskDefinition) {
			o := t.Declares.Outputs["report"]
			o.OptionalEmpty = true
			t.Declares.Outputs["report"] = o
		},
		// The carve-out is contract, not presentation: ceding a subpath changes
		// which bytes a hit restores, so cache key v5 must miss rather than
		// serve an entry captured under the wider declaration.
		"declaredExcludes": func(t *TaskDefinition) {
			o := t.Declares.Outputs["report"]
			o.Kind = OutputKindDirectory
			o.Path = "out"
			o.Excludes = []string{"out/migration-bundle"}
			t.Declares.Outputs["report"] = o
		},
		"effects":        func(t *TaskDefinition) { t.Declares.Effects = []string{"process"} },
		"mutatesSources": func(t *TaskDefinition) { t.Declares.MutatesSources = true },
		"declaresGone":   func(t *TaskDefinition) { t.Declares = nil },
	}

	base := TaskContractDigest(digestTask())
	for name, mutate := range cases {
		task := digestTask()
		mutate(&task)
		if got := TaskContractDigest(task); got == base {
			t.Errorf("%s: contract mutation did not move the digest", name)
		}
	}
}

func TestTaskContractDigest_DoesNotMutateItsArgument(t *testing.T) {
	task := digestTask()
	task.Declares.Effects = []string{"network", "locks"} // deliberately unsorted
	task.Cache = nil                                     // deliberately underivable

	_ = TaskContractDigest(task)

	if !reflect.DeepEqual(task.Declares.Effects, []string{"network", "locks"}) {
		t.Errorf("digest sorted the caller's effects in place: %v", task.Declares.Effects)
	}
	if task.Cache != nil {
		t.Error("digest derived a cache policy into the caller's task")
	}
	if task.Description == "" {
		t.Error("digest cleared the caller's description")
	}
}

// TestTaskContractDigest_FixtureCorpus pins the digest of every task in every
// valid fixture manifest. This is the B0e determinism corpus: fixture-based on
// purpose (B3 rewrites the live first-party manifests and must not move it).
// A diff here means the canonical serialization changed — which invalidates
// every v5 cache entry at once — and must be a reviewed, deliberate event.
// Regenerate with UPDATE_DIGEST_GOLDENS=1.
func TestTaskContractDigest_FixtureCorpus(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("fixtures", "valid", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no valid fixtures found: %v", err)
	}

	got := make(map[string]string)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		// ParseManifest without normalization: the corpus must hold on the
		// un-normalized load path too.
		m, diags := ParseManifest(data)
		if m == nil {
			t.Fatalf("%s: %v", file, diags)
		}
		names := make([]string, 0, len(m.Tasks))
		for name := range m.Tasks {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			got[filepath.Base(file)+"#"+name] = TaskContractDigest(m.Tasks[name])
		}
	}

	goldenPath := filepath.Join("testdata", "task_digests.golden.json")
	if os.Getenv("UPDATE_DIGEST_GOLDENS") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		data, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d digests to %s", len(got), goldenPath)
		return
	}

	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_DIGEST_GOLDENS=1): %v", err)
	}
	var want map[string]string
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		for k, v := range want {
			if got[k] != v {
				t.Errorf("digest moved: %s\n  golden %s\n  tree   %s", k, v, got[k])
			}
		}
		for k := range got {
			if _, ok := want[k]; !ok {
				t.Errorf("new task not in golden: %s (regenerate deliberately)", k)
			}
		}
	}
}
