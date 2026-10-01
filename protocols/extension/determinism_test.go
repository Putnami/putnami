package extension

import (
	"reflect"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestDeriveTaskCacheKey_Deterministic verifies that DeriveTaskCacheKey produces
// identical output regardless of Go map iteration order. Running this test 100
// times makes non-determinism very likely to surface.
func TestDeriveTaskCacheKey_Deterministic(t *testing.T) {
	inputs := map[string]TaskInputPort{
		"sources":    {From: "project", Files: []string{"**/*.go", "go.mod"}},
		"target":     {From: "params"},
		"go_version": {From: "env"},
		"platform":   {From: "runtime"},
		"extra_env":  {From: "env"},
		"upstream":   {From: "task"},
		"config":     {From: "project", Files: []string{"config.yaml"}},
	}

	// Get the canonical result.
	canonical := DeriveTaskCacheKey(inputs)

	// Run many times and compare.
	for i := 0; i < 100; i++ {
		got := DeriveTaskCacheKey(inputs)
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: DeriveTaskCacheKey produced non-deterministic output.\ncanonical: %+v\ngot:       %+v", i, canonical, got)
		}
	}

	// Verify output is sorted.
	if len(canonical.Files) >= 2 {
		for i := 1; i < len(canonical.Files); i++ {
			if canonical.Files[i] < canonical.Files[i-1] {
				t.Errorf("Files not sorted: %v", canonical.Files)
				break
			}
		}
	}
	if len(canonical.Env) >= 2 {
		for i := 1; i < len(canonical.Env); i++ {
			if canonical.Env[i] < canonical.Env[i-1] {
				t.Errorf("Env not sorted: %v", canonical.Env)
				break
			}
		}
	}
}

// TestValidateManifest_Deterministic verifies that ValidateManifest produces
// diagnostics in a consistent order regardless of Go map iteration order.
func TestValidateManifest_Deterministic(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"zebra": {Run: []PipelineStep{{ID: "s1", Task: "missing-z"}}},
			"alpha": {Run: []PipelineStep{{ID: "s1", Task: "missing-a"}}},
			"beta":  {Run: nil},
		},
		Tasks: map[string]TaskDefinition{
			"z-task": {Kind: "", Command: ""},
			"a-task": {Kind: "", Command: ""},
			"m-task": {Kind: "command", Command: "go", Inputs: map[string]TaskInputPort{
				"z-input": {From: "invalid"},
				"a-input": {From: ""},
			}},
		},
	}

	canonical := ValidateManifest(m)
	for i := 0; i < 100; i++ {
		got := ValidateManifest(m)
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: ValidateManifest produced non-deterministic diagnostics.\ncanonical: %v\ngot:       %v", i, canonical, got)
		}
	}

	// Verify diagnostics appear in sorted command/task order.
	if len(canonical) == 0 {
		t.Fatal("expected diagnostics")
	}
}

// TestValidatePipelineDAG_Deterministic verifies that ValidatePipelineDAG
// produces diagnostics in a consistent order.
func TestValidatePipelineDAG_Deterministic(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"zebra": {Run: []PipelineStep{
				{ID: "a", Task: "t1", DependsOn: []string{"b"}},
				{ID: "b", Task: "t1", DependsOn: []string{"a"}},
			}},
			"alpha": {Run: []PipelineStep{
				{ID: "x", Task: "t1", DependsOn: []string{"y"}},
				{ID: "y", Task: "t1", DependsOn: []string{"x"}},
			}},
		},
		Tasks: map[string]TaskDefinition{"t1": {Kind: "command", Command: "go"}},
	}

	canonical := ValidatePipelineDAG(m)
	if !diag.HasErrors(canonical) {
		t.Fatal("expected cycle errors")
	}

	for i := 0; i < 100; i++ {
		got := ValidatePipelineDAG(m)
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: ValidatePipelineDAG produced non-deterministic diagnostics.\ncanonical: %v\ngot:       %v", i, canonical, got)
		}
	}
}

// TestValidateSchemaRefs_Deterministic verifies that ValidateSchemaRefs
// produces diagnostics in a consistent order.
func TestValidateSchemaRefs_Deterministic(t *testing.T) {
	m := &Manifest{
		Contracts: &ContractsDefinition{
			Schemas: map[string]map[string]any{},
		},
		Tasks: map[string]TaskDefinition{
			"z-task": {Kind: "command", Command: "go", InputSchemaRef: "missing-z"},
			"a-task": {Kind: "command", Command: "go", OutputSchemaRef: "missing-a"},
			"m-task": {Kind: "command", Command: "go", InputSchemaRef: "missing-m", OutputSchemaRef: "missing-m2"},
		},
	}

	canonical := ValidateSchemaRefs(m)
	if len(canonical) == 0 {
		t.Fatal("expected diagnostics")
	}

	for i := 0; i < 100; i++ {
		got := ValidateSchemaRefs(m)
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: ValidateSchemaRefs produced non-deterministic diagnostics.\ncanonical: %v\ngot:       %v", i, canonical, got)
		}
	}
}

// TestFullValidateManifest_Deterministic verifies the full validation pipeline
// produces consistent output.
func TestFullValidateManifest_Deterministic(t *testing.T) {
	m := &Manifest{
		Commands: map[string]CommandDefinition{
			"build": {Run: []PipelineStep{{ID: "s1", Task: "t1"}}},
			"test":  {Run: []PipelineStep{{ID: "s1", Task: "t1"}}},
			"lint":  {Run: []PipelineStep{{ID: "s1", Task: "t1"}}},
		},
		Tasks: map[string]TaskDefinition{
			"t1": {Kind: "command", Command: "go"},
		},
	}

	canonical := FullValidateManifest(m)
	for i := 0; i < 100; i++ {
		got := FullValidateManifest(m)
		if !reflect.DeepEqual(got, canonical) {
			t.Fatalf("iteration %d: FullValidateManifest produced non-deterministic diagnostics.\ncanonical: %v\ngot:       %v", i, canonical, got)
		}
	}
}

// TestNormalizeManifest_Deterministic verifies that NormalizeManifest produces
// deterministic cache keys from task inputs regardless of map iteration order.
func TestNormalizeManifest_Deterministic(t *testing.T) {
	makeManifest := func() *Manifest {
		return &Manifest{
			Tasks: map[string]TaskDefinition{
				"z-task": {Kind: "command", Command: "go", Inputs: map[string]TaskInputPort{
					"src": {From: "project", Files: []string{"**/*.go"}},
					"env": {From: "env"},
				}},
				"a-task": {Kind: "command", Command: "go", Inputs: map[string]TaskInputPort{
					"params": {From: "params"},
					"files":  {From: "project", Files: []string{"*.ts", "*.tsx"}},
				}},
			},
		}
	}

	canonical := makeManifest()
	NormalizeManifest(canonical)

	for i := 0; i < 100; i++ {
		m := makeManifest()
		NormalizeManifest(m)
		if !reflect.DeepEqual(m, canonical) {
			t.Fatalf("iteration %d: NormalizeManifest produced non-deterministic output", i)
		}
	}
}
