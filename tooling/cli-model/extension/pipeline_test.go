package extension

import (
	"reflect"
	"testing"
)

func TestStepJobName(t *testing.T) {
	if got := StepJobName("build", "transpile"); got != "build~transpile" {
		t.Errorf("StepJobName = %q, want %q", got, "build~transpile")
	}
}

func TestIsExternalRef(t *testing.T) {
	tests := []struct {
		ref      string
		expected bool
	}{
		{"^generate", true},
		{"/install", true},
		{"*build", true},
		{"generate", false},
		{"build~transpile", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := IsExternalRef(tt.ref); got != tt.expected {
			t.Errorf("IsExternalRef(%q) = %v, want %v", tt.ref, got, tt.expected)
		}
	}
}

func TestExpandPipeline_BasicSteps(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@putnami/typescript",
		Tasks: map[string]TaskDefinition{
			"build-generate": {
				Kind:    "command",
				Command: "bun",
				Args:    []string{"run", "build-generate"},
				Cache:   &TaskCachePolicy{Key: &TaskCacheKey{Files: []string{"src/**/*.ts"}}},
			},
			"build-transpile": {
				Kind:    "command",
				Command: "bun",
				Args:    []string{"run", "build-transpile"},
			},
		},
	}

	steps := []PipelineStep{
		{ID: "generate", Task: "build-generate"},
		{ID: "transpile", Task: "build-transpile", DependsOn: []string{"generate"}},
	}

	cmdDef := &JobDefinition{Name: "build", Priority: 10}

	expanded, err := ExpandPipeline("build", cmdDef, steps, ext, nil)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}

	if len(expanded) != 2 {
		t.Fatalf("expected 2 expanded steps, got %d", len(expanded))
	}

	// Check first step
	if expanded[0].JobDef.Name != "build~generate" {
		t.Errorf("step 0 name = %q, want %q", expanded[0].JobDef.Name, "build~generate")
	}
	if len(expanded[0].JobDef.DependsOn) != 0 {
		t.Errorf("step 0 should have no deps, got %v", expanded[0].JobDef.DependsOn)
	}

	// Check second step
	if expanded[1].JobDef.Name != "build~transpile" {
		t.Errorf("step 1 name = %q, want %q", expanded[1].JobDef.Name, "build~transpile")
	}
	if len(expanded[1].JobDef.DependsOn) != 1 || expanded[1].JobDef.DependsOn[0] != "build~generate" {
		t.Errorf("step 1 deps = %v, want [build~generate]", expanded[1].JobDef.DependsOn)
	}
}

func TestExpandPipeline_AlsoRunsPropagates(t *testing.T) {
	// Both conversion paths must carry alsoRuns: the planner reads it from
	// whichever definition the job map returns (a whole command or an
	// expanded step), so dropping it on either path silently disables the
	// request-level expansion.
	ext := &ExtensionDescription{
		Name: "@putnami/sdd",
		Tasks: map[string]TaskDefinition{
			"features-validate": {Kind: "command", Command: "run"},
			"specs-validate":    {Kind: "command", Command: "run"},
		},
	}
	steps := []PipelineStep{
		{ID: "features", Task: "features-validate"},
		{ID: "specs", Task: "specs-validate", DependsOn: []string{"features"}},
	}
	cmdDef := &JobDefinition{Name: "validate", AlsoRuns: []string{"validate-workspace"}}

	expanded, err := ExpandPipeline("validate", cmdDef, steps, ext, nil)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}
	for i, step := range expanded {
		if len(step.JobDef.AlsoRuns) != 1 || step.JobDef.AlsoRuns[0] != "validate-workspace" {
			t.Errorf("step %d alsoRuns = %v, want [validate-workspace]", i, step.JobDef.AlsoRuns)
		}
	}
}

func TestExpandAlsoRunCommands(t *testing.T) {
	jobMap := map[string][]*JobDefinition{
		"validate": {{Name: "validate", AlsoRuns: []string{"validate-workspace"}}},
		"chained":  {{Name: "chained", AlsoRuns: []string{"validate"}}},
		"cyclic":   {{Name: "cyclic", AlsoRuns: []string{"cyclic-back"}}},
		"cyclic-back": {{
			Name: "cyclic-back", AlsoRuns: []string{"cyclic"},
		}},
	}

	got := ExpandAlsoRunCommands([]string{"lint", "validate"}, jobMap)
	want := []string{"lint", "validate", "validate-workspace"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expand = %v, want %v", got, want)
	}

	// Chains close transitively; an explicitly requested companion is not
	// planned twice.
	got = ExpandAlsoRunCommands([]string{"chained", "validate-workspace"}, jobMap)
	want = []string{"chained", "validate-workspace", "validate"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chained expand = %v, want %v", got, want)
	}

	// A declaration cycle terminates instead of looping.
	got = ExpandAlsoRunCommands([]string{"cyclic"}, jobMap)
	want = []string{"cyclic", "cyclic-back"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cyclic expand = %v, want %v", got, want)
	}
}

func TestResolve_AlsoRunsPropagates(t *testing.T) {
	manifest := &Manifest{
		Name: "@putnami/sdd",
		Commands: map[string]CommandDefinition{
			"validate": {
				AlsoRuns: []string{"validate-workspace"},
				Run:      []PipelineStep{{ID: "specs", Task: "work"}},
			},
			"validate-workspace": {
				Activation: "workspace-once",
				Run:        []PipelineStep{{ID: "architecture", Task: "work"}},
			},
		},
		Tasks: map[string]TaskDefinition{"work": {Kind: "command", Command: "run"}},
	}
	desc := Resolve(manifest, "tooling/sdd-extension")
	var validate *JobDefinition
	for _, job := range desc.Jobs {
		if job.Name == "validate" {
			validate = job
		}
	}
	if validate == nil {
		t.Fatalf("resolved description has no validate job: %+v", desc.Jobs)
	}
	if len(validate.AlsoRuns) != 1 || validate.AlsoRuns[0] != "validate-workspace" {
		t.Fatalf("validate alsoRuns = %v, want [validate-workspace]", validate.AlsoRuns)
	}
}

func TestExpandPipeline_MaterializesLiteralBindingsPerStep(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@test/literals",
		Tasks: map[string]TaskDefinition{
			"run": {Kind: "command", Command: "runtime"},
		},
	}
	steps := []PipelineStep{
		{
			ID:   "bound",
			Task: "run",
			With: map[string]InputBinding{
				"mode":     {Value: "test", HasValue: true},
				"attempts": {Value: float64(3), HasValue: true},
				"nullable": {Value: nil, HasValue: true},
				"result":   {FromStep: "producer", Output: "value"},
			},
		},
		{ID: "plain", Task: "run"},
	}

	expanded, err := ExpandPipeline("test", &JobDefinition{Name: "test"}, steps, ext, nil)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}
	if len(expanded) != 2 {
		t.Fatalf("expanded steps = %d, want 2", len(expanded))
	}

	params := expanded[0].JobDef.BoundParams
	if got := params["mode"]; got != "test" {
		t.Errorf("bound mode = %#v, want test", got)
	}
	if got := params["attempts"]; got != float64(3) {
		t.Errorf("bound attempts = %#v, want float64(3)", got)
	}
	if got, present := params["nullable"]; !present || got != nil {
		t.Errorf("explicit null = (%#v, %v), want (nil, true)", got, present)
	}
	if _, present := params["result"]; present {
		t.Error("fromStep binding was materialized as a literal param")
	}
	if expanded[1].JobDef.BoundParams != nil {
		t.Errorf("plain sibling inherited bound params: %#v", expanded[1].JobDef.BoundParams)
	}
}

// TestExpandPipeline_NoOutputPropagates guards the inputs-derived policy
// rebuild: a task with declared inputs gets a fresh TaskCachePolicy carrying
// the derived key, and every manifest cache flag must be copied onto it. A
// dropped noOutput would silently re-fatten tidy/lint entries with the shared
// command output dir (sibling artifacts) and re-break their remote hits.
func TestExpandPipeline_NoOutputPropagates(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@putnami/go",
		Tasks: map[string]TaskDefinition{
			// Mirrors build-tidy: inputs declared (so the policy is rebuilt
			// around the derived key) and cache marked noOutput.
			"build-tidy": {
				Kind:    "command",
				Command: "runtime",
				Inputs: map[string]TaskInputPort{
					"modules": {From: "project", Files: []string{"go.mod", "go.sum"}},
				},
				Cache: &TaskCachePolicy{NoOutput: true},
			},
		},
	}

	steps := []PipelineStep{{ID: "tidy", Task: "build-tidy"}}
	cmdDef := &JobDefinition{Name: "build"}
	expanded, err := ExpandPipeline("build", cmdDef, steps, ext, nil)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}
	if len(expanded) != 1 {
		t.Fatalf("expected 1 expanded step, got %d", len(expanded))
	}
	policy := expanded[0].JobDef.TaskCachePolicy
	if policy == nil || !policy.NoOutput {
		t.Errorf("tidy step: expected noOutput cache policy to propagate, got %+v", policy)
	}
	if policy == nil || policy.Key == nil {
		t.Errorf("tidy step: derived key must survive alongside noOutput, got %+v", policy)
	}
}

func TestExpandPipeline_BatchablePolicyPropagates(t *testing.T) {
	policy := &TaskBatchPolicy{
		Tool:        "biome",
		ConfigFiles: []string{"{workspaceRoot}/biome.json"},
	}
	ext := &ExtensionDescription{
		Name: "@putnami/typescript",
		Tasks: map[string]TaskDefinition{
			"lint-all": {
				Kind:      "command",
				Command:   "runtime",
				Batchable: policy,
			},
		},
	}

	expanded, err := ExpandPipeline(
		"lint",
		&JobDefinition{Name: "lint"},
		[]PipelineStep{{ID: "check-only", Task: "lint-all"}},
		ext,
		nil,
	)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}
	if len(expanded) != 1 || expanded[0].JobDef.Batchable != policy {
		t.Fatalf("batchable policy did not propagate: %+v", expanded)
	}
}

func TestExpandPipeline_VersionAwarePropagates(t *testing.T) {
	disabled := false
	ext := &ExtensionDescription{
		Name: "@putnami/typescript",
		Tasks: map[string]TaskDefinition{
			// Version-stamped package task: marked version-aware, no inputs.
			"package-npm": {
				Kind:    "command",
				Command: "runtime",
				Cache:   &TaskCachePolicy{VersionAware: true},
			},
			// Side-effecting publish task: caching disabled.
			"publish-npm": {
				Kind:    "command",
				Command: "ci-run",
				Cache:   &TaskCachePolicy{Enabled: &disabled},
			},
		},
	}

	steps := []PipelineStep{
		{ID: "npm", Task: "package-npm"},
		{ID: "publish", Task: "publish-npm", DependsOn: []string{"npm"}},
	}

	cmdDef := &JobDefinition{Name: "package"}
	expanded, err := ExpandPipeline("package", cmdDef, steps, ext, nil)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}
	if len(expanded) != 2 {
		t.Fatalf("expected 2 expanded steps, got %d", len(expanded))
	}

	pkg := expanded[0].JobDef
	if pkg.TaskCachePolicy == nil || !pkg.TaskCachePolicy.VersionAware {
		t.Errorf("package step: expected versionAware cache policy, got %+v", pkg.TaskCachePolicy)
	}
	if !pkg.Cache {
		t.Errorf("package step: expected caching enabled, got disabled")
	}

	pub := expanded[1].JobDef
	if pub.Cache {
		t.Errorf("publish step: expected caching disabled so external effects are never cache-served")
	}
}

func TestExpandPipeline_ExternalRefs(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@putnami/typescript",
		Tasks: map[string]TaskDefinition{
			"build-generate":  {Kind: "command", Command: "bun"},
			"build-transpile": {Kind: "command", Command: "bun"},
		},
	}

	steps := []PipelineStep{
		{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
		{ID: "transpile", Task: "build-transpile", DependsOn: []string{"generate"}},
	}

	cmdDef := &JobDefinition{Name: "build"}
	expanded, err := ExpandPipeline("build", cmdDef, steps, ext, nil)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}

	// External ref should pass through
	if len(expanded[0].JobDef.DependsOn) != 1 || expanded[0].JobDef.DependsOn[0] != "^generate" {
		t.Errorf("external ref not preserved: %v", expanded[0].JobDef.DependsOn)
	}
}

func TestExpandPipeline_IfConditionExcludes(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@putnami/typescript",
		Tasks: map[string]TaskDefinition{
			"build-generate":  {Kind: "command", Command: "bun"},
			"build-transpile": {Kind: "command", Command: "bun"},
			"build-compile":   {Kind: "command", Command: "bun"},
		},
	}

	steps := []PipelineStep{
		{ID: "generate", Task: "build-generate"},
		{ID: "transpile", Task: "build-transpile", DependsOn: []string{"generate"},
			If: "params.transpile || !params.transpile && !params.types && !params.compile"},
		{ID: "compile", Task: "build-compile", DependsOn: []string{"generate"},
			If: "params.compile"},
	}

	cmdDef := &JobDefinition{Name: "build"}

	// With compile=true → transpile excluded, compile included
	params := map[string]any{"compile": true}
	expanded, err := ExpandPipeline("build", cmdDef, steps, ext, params)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}

	names := make([]string, len(expanded))
	for i, e := range expanded {
		names[i] = e.JobDef.Name
	}

	// Should have generate and compile, but not transpile
	if len(expanded) != 2 {
		t.Fatalf("expected 2 steps, got %d: %v", len(expanded), names)
	}
	if names[0] != "build~generate" || names[1] != "build~compile" {
		t.Errorf("steps = %v, want [build~generate, build~compile]", names)
	}
}

func TestComputeExcludedSteps_DeadCodeElimination(t *testing.T) {
	// Step A is only needed by step B. If B is excluded, A should also be excluded.
	steps := []PipelineStep{
		{ID: "setup", Task: "t1"}, // only needed by "process"
		{ID: "process", Task: "t2", DependsOn: []string{"setup"}, If: "params.run"},
		{ID: "finalize", Task: "t3"}, // leaf, no condition
	}

	// params.run = false → "process" excluded → "setup" dead code (only finalize survives)
	params := map[string]any{}
	excluded := computeExcludedSteps(steps, params, nil)

	if !excluded["process"] {
		t.Error("process should be excluded (if condition false)")
	}
	if !excluded["setup"] {
		t.Error("setup should be excluded (dead code — only feeds excluded step)")
	}
	if excluded["finalize"] {
		t.Error("finalize should not be excluded")
	}
}

func TestExpandPipeline_PlannerConditionContextIsFailClosedAndBackwardCompatible(t *testing.T) {
	library := "library"
	for _, expr := range []string{"params.commands.enabled", "params.commandParams.test", "params.project.type", "'commands.test' == 'commands.test'"} {
		if usesPlanConditionContext(expr) {
			t.Errorf("historical expression %q was mistaken for planner context", expr)
		}
	}
	ext := &ExtensionDescription{
		Name: "@test",
		Tasks: map[string]TaskDefinition{
			"legacy": {Kind: "command", Command: "cmd"},
			"aware":  {Kind: "command", Command: "cmd"},
		},
	}
	steps := []PipelineStep{
		{ID: "legacy", Task: "legacy", If: "params.enabled"},
		{ID: "aware", Task: "aware", If: "commands.test && commandParams.test.race == false && project.type == 'library'"},
	}

	// The old helper has no planner context. Its params-only condition keeps
	// historical behavior (excluded), while the new planner-aware condition is
	// retained rather than being evaluated against invented empty facts.
	expanded, err := ExpandPipeline("build", &JobDefinition{Name: "build"}, steps, ext, nil)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}
	if got := stepNames(expanded); len(got) != 1 || got[0] != "build~aware" {
		t.Fatalf("context-free expansion = %v, want [build~aware]", got)
	}

	opts := PipelineExpansionOptions{PlanContext: &WhenContext{
		Commands:      map[string]bool{"build": true, "test": true},
		CommandParams: map[string]ParamMap{"test": {"race": false}},
		ProjectType:   &library,
	}}
	expanded, err = ExpandPipelineWithOptions("build", &JobDefinition{Name: "build"}, steps, ext, nil, opts)
	if err != nil {
		t.Fatalf("ExpandPipelineWithOptions: %v", err)
	}
	if got := stepNames(expanded); len(got) != 1 || got[0] != "build~aware" {
		t.Fatalf("planner-aware expansion = %v, want [build~aware]", got)
	}
}

func TestExpandPipeline_UnknownTask(t *testing.T) {
	ext := &ExtensionDescription{
		Name:  "@putnami/typescript",
		Tasks: map[string]TaskDefinition{},
	}

	steps := []PipelineStep{
		{ID: "generate", Task: "nonexistent"},
	}

	cmdDef := &JobDefinition{Name: "build"}
	_, err := ExpandPipeline("build", cmdDef, steps, ext, nil)
	if err == nil {
		t.Error("expected error for unknown task")
	}
}

func TestExpandPipeline_FlagsNotInherited(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@test",
		Tasks: map[string]TaskDefinition{
			"task-a": {Kind: "command", Command: "cmd"},
		},
	}

	steps := []PipelineStep{
		{ID: "a", Task: "task-a"},
	}

	cmdDef := &JobDefinition{
		Name: "publish",
		Flags: map[string]FlagDefinition{
			"registry": {Type: "string"},
			"platform": {Type: "string"},
		},
	}

	expanded, err := ExpandPipeline("publish", cmdDef, steps, ext, nil)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}

	// Pipeline steps receive params via --putnamiContext, not CLI flags.
	if expanded[0].JobDef.Flags != nil {
		t.Fatal("step job should not inherit parent command flags")
	}
}

// stepNames returns the expanded job names in order, for assertions.
func stepNames(expanded []ExpandedStep) []string {
	names := make([]string, len(expanded))
	for i, e := range expanded {
		names[i] = e.JobDef.Name
	}
	return names
}

func findExpanded(expanded []ExpandedStep, name string) *ExpandedStep {
	for i := range expanded {
		if expanded[i].JobDef.Name == name {
			return &expanded[i]
		}
	}
	return nil
}

func describeExt() *ExtensionDescription {
	return &ExtensionDescription{
		Name: "@putnami/go",
		Tasks: map[string]TaskDefinition{
			"build-generate":      {Kind: "command", Command: "go"},
			"build-describe":      {Kind: "command", Command: "go"},
			"build-cross-compile": {Kind: "command", Command: "go"},
		},
	}
}

// describeSteps mirrors the Go build pipeline: generate → describe → compile,
// with describe gated by an activation predicate.
func describeSteps() []PipelineStep {
	return []PipelineStep{
		{ID: "generate", Task: "build-generate", DependsOn: []string{"^generate"}},
		{ID: "describe", Task: "build-describe", DependsOn: []string{"^describe", "generate"},
			Activation: &StepActivation{Contains: map[string]string{"go.mod": "go.putnami.dev/app"}}},
		{ID: "cross-compile", Task: "build-cross-compile", DependsOn: []string{"describe"}},
	}
}

func TestExpandPipeline_ActivationKeepsStepWhenActive(t *testing.T) {
	opts := PipelineExpansionOptions{StepActive: func(PipelineStep) bool { return true }}
	expanded, err := ExpandPipelineWithOptions("build", &JobDefinition{Name: "build"}, describeSteps(), describeExt(), nil, opts)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got := stepNames(expanded); len(got) != 3 {
		t.Fatalf("active describe should keep all steps, got %v", got)
	}
	cc := findExpanded(expanded, "build~cross-compile")
	if cc == nil || len(cc.JobDef.DependsOn) != 1 || cc.JobDef.DependsOn[0] != "build~describe" {
		t.Fatalf("cross-compile should depend on describe when active, got %+v", cc)
	}
}

func TestExpandPipeline_ActivationSplicesAndBridges(t *testing.T) {
	// describe inactive → spliced out; its dependent inherits describe's deps
	// (the external ^describe ref and the intra generate ref), preserving the
	// transitive ordering exactly.
	opts := PipelineExpansionOptions{StepActive: func(PipelineStep) bool { return false }}
	expanded, err := ExpandPipelineWithOptions("build", &JobDefinition{Name: "build"}, describeSteps(), describeExt(), nil, opts)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}

	got := stepNames(expanded)
	if len(expanded) != 2 {
		t.Fatalf("describe should be pruned, got steps %v", got)
	}
	for _, n := range got {
		if n == "build~describe" {
			t.Fatalf("describe should not be present, got %v", got)
		}
	}

	cc := findExpanded(expanded, "build~cross-compile")
	if cc == nil {
		t.Fatal("cross-compile missing")
	}
	want := map[string]bool{"^describe": true, "build~generate": true}
	if len(cc.JobDef.DependsOn) != len(want) {
		t.Fatalf("cross-compile deps = %v, want keys %v", cc.JobDef.DependsOn, want)
	}
	for _, d := range cc.JobDef.DependsOn {
		if !want[d] {
			t.Errorf("unexpected bridged dep %q (deps=%v)", d, cc.JobDef.DependsOn)
		}
	}

	// generate must survive: it is now depended on by cross-compile.
	if findExpanded(expanded, "build~generate") == nil {
		t.Error("generate should survive the splice (bridged dependency)")
	}
}

func TestExpandPipeline_ActivationBridgeDedupes(t *testing.T) {
	// A consumer that depends on BOTH describe and generate directly must not
	// end up with a duplicate generate edge after describe is bridged.
	steps := []PipelineStep{
		{ID: "generate", Task: "build-generate"},
		{ID: "describe", Task: "build-describe", DependsOn: []string{"generate"},
			Activation: &StepActivation{Contains: map[string]string{"go.mod": "x"}}},
		{ID: "cross-compile", Task: "build-cross-compile", DependsOn: []string{"describe", "generate"}},
	}
	opts := PipelineExpansionOptions{StepActive: func(PipelineStep) bool { return false }}
	expanded, err := ExpandPipelineWithOptions("build", &JobDefinition{Name: "build"}, steps, describeExt(), nil, opts)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	cc := findExpanded(expanded, "build~cross-compile")
	if cc == nil {
		t.Fatal("cross-compile missing")
	}
	if len(cc.JobDef.DependsOn) != 1 || cc.JobDef.DependsOn[0] != "build~generate" {
		t.Errorf("bridged deps should dedupe to [build~generate], got %v", cc.JobDef.DependsOn)
	}
}

func TestExpandPipeline_ActivationTransitiveBridge(t *testing.T) {
	// Two consecutive inactive steps: the surviving consumer must bridge all
	// the way back to the active producer.
	steps := []PipelineStep{
		{ID: "generate", Task: "build-generate"},
		{ID: "a", Task: "build-describe", DependsOn: []string{"generate"},
			Activation: &StepActivation{Contains: map[string]string{"go.mod": "x"}}},
		{ID: "b", Task: "build-describe", DependsOn: []string{"a"},
			Activation: &StepActivation{Contains: map[string]string{"go.mod": "x"}}},
		{ID: "compile", Task: "build-cross-compile", DependsOn: []string{"b"}},
	}
	opts := PipelineExpansionOptions{StepActive: func(PipelineStep) bool { return false }}
	expanded, err := ExpandPipelineWithOptions("build", &JobDefinition{Name: "build"}, steps, describeExt(), nil, opts)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if got := stepNames(expanded); len(got) != 2 {
		t.Fatalf("a and b should be pruned, got %v", got)
	}
	compile := findExpanded(expanded, "build~compile")
	if compile == nil || len(compile.JobDef.DependsOn) != 1 || compile.JobDef.DependsOn[0] != "build~generate" {
		t.Errorf("compile should bridge through a and b to generate, got %+v", compile)
	}
}

func TestExpandPipeline_ActivationIgnoredWithoutPredicate(t *testing.T) {
	// A nil StepActive predicate leaves activation-gated steps in place: the
	// step runs and skips at runtime exactly as before.
	expanded, err := ExpandPipeline("build", &JobDefinition{Name: "build"}, describeSteps(), describeExt(), nil)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	if findExpanded(expanded, "build~describe") == nil {
		t.Error("without a predicate, describe must remain in the plan")
	}
}

func TestExpandPipeline_CacheOverride(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@test",
		Tasks: map[string]TaskDefinition{
			"task-a": {Kind: "command", Command: "cmd", Cache: &TaskCachePolicy{}},
		},
	}

	disabled := false
	steps := []PipelineStep{
		{ID: "a", Task: "task-a", Cache: &StepCacheOverride{Enabled: &disabled}},
	}

	cmdDef := &JobDefinition{Name: "build"}
	expanded, err := ExpandPipeline("build", cmdDef, steps, ext, nil)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}

	if expanded[0].JobDef.Cache {
		t.Error("step cache should be disabled by override")
	}
}

// TestExpandPipeline_CarriesResourceClaims pins the plumbing for a task's
// named budget claims: the scheduler admits against JobDefinition
// .Resources, so a claim that stops at the manifest gates nothing at all.
func TestExpandPipeline_CarriesResourceClaims(t *testing.T) {
	ext := &ExtensionDescription{
		Name: "@putnami/go",
		Tasks: map[string]TaskDefinition{
			"test-exec": {
				Kind:      "command",
				Command:   "go",
				Resources: map[string]int{"db-connections": 230},
			},
			"lint-exec": {Kind: "command", Command: "golangci-lint"},
		},
	}
	steps := []PipelineStep{
		{ID: "test", Task: "test-exec"},
		{ID: "lint", Task: "lint-exec"},
	}

	expanded, err := ExpandPipeline("test", &JobDefinition{Name: "test"}, steps, ext, nil)
	if err != nil {
		t.Fatalf("ExpandPipeline: %v", err)
	}
	if got := expanded[0].JobDef.Resources["db-connections"]; got != 230 {
		t.Errorf("claiming step resources = %v, want db-connections=230", expanded[0].JobDef.Resources)
	}
	if len(expanded[1].JobDef.Resources) != 0 {
		t.Errorf("non-claiming step resources = %v, want none", expanded[1].JobDef.Resources)
	}
}
