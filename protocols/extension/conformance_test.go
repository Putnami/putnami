package extension

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
)

// TestConformance_SchemaConditionalsValidateFixtures executes the conditional
// JSON Schema fragments against the same output fixtures the Go validator
// consumes. This is deliberately a small evaluator for the keywords used by
// extension.json (if/then, required, const, enum, not): it prevents the authoring
// schema from merely documenting rules that only Go enforces.
func TestConformance_SchemaConditionalsValidateFixtures(t *testing.T) {
	declaredOutput := readSchemaDefinition(t, "declaredOutput")
	for _, fixture := range []string{
		"sensitive-output-durable.json",
		"runtime-file-durable-scope.json",
		"sensitive-output-path-from-port.json",
		"invocation-output-with-root.json",
		"declared-output-exclude-undecidable.json",
		"declared-output-drift-uncommitted.json",
		"declared-output-drift-path-from-workspace.json",
		"declared-output-preserve-invalid.json",
	} {
		t.Run("reject "+fixture, func(t *testing.T) {
			outputs := fixtureDeclaredOutputs(t, filepath.Join("fixtures/invalid", fixture))
			violations := 0
			for _, output := range outputs {
				violations += conditionalSchemaViolations(declaredOutput, output)
			}
			if violations == 0 {
				t.Fatalf("%s violates a conditional output rule but the schema accepted every output", fixture)
			}
		})
	}

	for _, fixture := range []string{lifecycleFixturePath, "fixtures/valid/declared-output-drift.json", "fixtures/valid/declared-output-preserves.json"} {
		for _, output := range fixtureDeclaredOutputs(t, fixture) {
			if violations := conditionalSchemaViolations(declaredOutput, output); violations != 0 {
				t.Errorf("%s valid output violates %d schema conditions: %v", fixture, violations, output)
			}
		}
	}

	pipelineStep := readSchemaDefinition(t, "pipelineStep")
	validFinalizer := map[string]any{
		"id": "teardown", "task": "teardown", "runOn": "finally",
		"finalizes": map[string]any{"producer": "setup", "consumers": []any{"run"}},
	}
	if got := conditionalSchemaViolations(pipelineStep, validFinalizer); got != 0 {
		t.Fatalf("valid finalizer violates %d pipeline schema conditions", got)
	}
	for name, step := range map[string]map[string]any{
		"finally without relation": {
			"id": "teardown", "task": "teardown", "runOn": "finally",
		},
		"relation on success step": {
			"id": "teardown", "task": "teardown",
			"finalizes": map[string]any{"producer": "setup", "consumers": []any{"run"}},
		},
		"finally with ordinary dependency": {
			"id": "teardown", "task": "teardown", "runOn": "finally",
			"dependsOn": []any{"setup"},
			"finalizes": map[string]any{"producer": "setup", "consumers": []any{"run"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := conditionalSchemaViolations(pipelineStep, step); got == 0 {
				t.Fatal("pipeline conditional schema unexpectedly accepted the invalid step")
			}
		})
	}
}

func TestConformance_SessionPrerequisiteValueRejectsNull(t *testing.T) {
	definition := readSchemaDefinition(t, "sessionPrerequisiteParamBinding")
	properties, ok := definition["properties"].(map[string]any)
	if !ok {
		t.Fatal("sessionPrerequisiteParamBinding schema has no properties")
	}
	value, ok := properties["value"].(map[string]any)
	if !ok {
		t.Fatal("session prerequisite value has no schema")
	}
	not, ok := value["not"].(map[string]any)
	if !ok || not["type"] != "null" {
		t.Fatalf("session prerequisite value null rejection = %v, want not:type null", value)
	}

	manifest, parseDiags := ParseManifest([]byte(`{
		"commands": {"deploy": {
			"sessionPrerequisites": [{"command":"publish", "params":{"docker":{"value":null}}}],
			"run": [{"id":"deploy", "task":"work"}]
		}},
		"tasks": {"work":{"kind":"command", "command":"run"}}
	}`))
	if diag.HasErrors(parseDiags) {
		t.Fatalf("parse null fixture: %v", parseDiags)
	}
	errors := diag.Errors(ValidateManifest(manifest))
	if len(errors) != 1 || errors[0].Code != "invalid-session-prerequisite-param" {
		t.Fatalf("Go null validation = %v, want invalid-session-prerequisite-param", errors)
	}
}

func TestConformance_SessionPrerequisitesRequireCLIContract4(t *testing.T) {
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("extension schema has no properties")
	}
	cliContract, ok := properties["cliContract"].(map[string]any)
	if !ok || cliContract["minimum"] != float64(4) {
		t.Fatalf("cliContract schema = %v, want minimum 4", cliContract)
	}

	path := filepath.Join(t.TempDir(), ManifestFilename)
	manifest := `{
		"name":"@test/legacy",
		"cliContract":3,
		"commands":{
			"deploy":{
				"sessionPrerequisites":[{"command":"publish"}],
				"run":[{"id":"apply","task":"work"}]
			},
			"publish":{"run":[{"id":"publish","task":"work"}]}
		},
		"tasks":{"work":{"kind":"command","command":"run"}}
	}`
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadManifest(path)
	if err == nil || loaded != nil {
		t.Fatal("contract-3 reader-compatible stamp loaded sessionPrerequisites; an old CLI could silently deploy without its gates")
	}
	for _, want := range []string{"contract 3", "requires 4", "re-package"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("legacy contract error %q does not contain %q", err, want)
		}
	}
}

func readSchemaDefinition(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatal("schema has no $defs object")
	}
	def, ok := defs[name].(map[string]any)
	if !ok {
		t.Fatalf("schema has no object definition %q", name)
	}
	return def
}

func fixtureDeclaredOutputs(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	var outputs []map[string]any
	tasks, _ := document["tasks"].(map[string]any)
	for _, rawTask := range tasks {
		task, _ := rawTask.(map[string]any)
		declares, _ := task["declares"].(map[string]any)
		rawOutputs, _ := declares["outputs"].(map[string]any)
		for _, rawOutput := range rawOutputs {
			if output, ok := rawOutput.(map[string]any); ok {
				outputs = append(outputs, output)
			}
		}
	}
	return outputs
}

func conditionalSchemaViolations(schema, instance map[string]any) int {
	rules, _ := schema["allOf"].([]any)
	violations := 0
	for _, rawRule := range rules {
		rule, _ := rawRule.(map[string]any)
		ifSchema, _ := rule["if"].(map[string]any)
		thenSchema, _ := rule["then"].(map[string]any)
		if schemaSubsetMatches(ifSchema, instance) && !schemaSubsetMatches(thenSchema, instance) {
			violations++
		}
	}
	return violations
}

func schemaSubsetMatches(schema, instance map[string]any) bool {
	if required, ok := schema["required"].([]any); ok {
		for _, rawName := range required {
			name, _ := rawName.(string)
			if _, present := instance[name]; !present {
				return false
			}
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for name, rawProperty := range properties {
			value, present := instance[name]
			if !present {
				continue
			}
			property, _ := rawProperty.(map[string]any)
			if expected, hasConst := property["const"]; hasConst && !reflect.DeepEqual(value, expected) {
				return false
			}
			if allowed, hasEnum := property["enum"].([]any); hasEnum {
				member := false
				for _, candidate := range allowed {
					if reflect.DeepEqual(value, candidate) {
						member = true
						break
					}
				}
				if !member {
					return false
				}
			}
			if maxItems, ok := property["maxItems"].(float64); ok {
				items, isArray := value.([]any)
				if !isArray || len(items) > int(maxItems) {
					return false
				}
			}
		}
	}
	if rawNot, ok := schema["not"]; ok {
		notSchema, _ := rawNot.(map[string]any)
		if schemaSubsetMatches(notSchema, instance) {
			return false
		}
	}
	return true
}

// TestConformance_ProtocolVersion pins the current protocol version so any bump
// is intentional and surfaces in code review. The manifest has no on-the-wire
// version field, so this constant is the only anchor — bumping requires a
// migration story.
//
// v3's migration story is that there is nothing to migrate: the task contract
// is additive, v2 remains a valid shape, and ManifestProtocolVersion reports
// which one a given manifest exercises. TestConformance_V2FixturesStayV2 keeps
// that promise honest.
func TestConformance_ProtocolVersion(t *testing.T) {
	if ProtocolVersion != 3 {
		t.Fatalf("ProtocolVersion = %d, want 3 — bumping requires a migration story", ProtocolVersion)
	}
	if ProtocolVersionV2 != 2 || ProtocolVersionV3 != 3 {
		t.Fatalf("version constants drifted: v2=%d v3=%d", ProtocolVersionV2, ProtocolVersionV3)
	}
	if ProtocolVersion != ProtocolVersionV3 {
		t.Fatalf("ProtocolVersion = %d, want ProtocolVersionV3 (%d)", ProtocolVersion, ProtocolVersionV3)
	}
}

// v3ValidFixtures names the valid fixtures that exercise the v3 task contract.
// Every other fixture in fixtures/valid predates it and must still report v2.
// Keeping the set explicit is what makes TestConformance_FixtureProtocolVersions
// a two-sided check instead of a vacuous one: a v3 fixture that lost its
// `declares` block would otherwise silently start passing as "still v2".
//
// B3b/B3c/B3d add first-party manifests, not new rules — an added fixture is a
// line here plus a file.
var v3ValidFixtures = map[string]bool{
	"task-contract-v3.json":           true,
	"task-contract-v3-ownership.json": true,
	// The three lifecycle primitives of contract v3 in one manifest: a prepared
	// runtime, a workspace adapter, invocation-scoped sensitive outputs, and a
	// `runOn: finally` finalizer.
	"lifecycle-v3.json": true,
	// A committed generated output carrying a drift policy on each path shape:
	// a literal file at warn, a pathFrom directory at fail (protocol ADR 0004).
	"declared-output-drift.json": true,
	// A generated directory that is a workspace project of its own: the
	// project document and module files are preserved on a pathFrom output
	// (protocol ADR 0005).
	"declared-output-preserves.json": true,
}

// v3InvalidFixtureCodes maps each v3 conformance counter-example to the exact
// diagnostic codes it must produce, in order. It is the single source of truth
// shared by the harness conformance tests here and TestV3InvalidFixtures, so a
// new counter-example is one fixture file and one line.
var v3InvalidFixtureCodes = map[string][]string{
	// ONE OWNER PER OUTPUT — file nested in another task's declared subtree.
	"output-overlap.json": {"output-overlap"},
	// ONE OWNER PER OUTPUT — literally the same path, spelled differently, so
	// the check is proven to compare normalized paths and not raw strings.
	"output-overlap-same-path.json": {"output-overlap"},
	// HONEST EFFECTS — an external effect on an explicitly cacheable task.
	"effect-conflict-cached-registry.json": {"effect-conflict"},
	// HONEST EFFECTS — the same conflict with NO cache block at all, since
	// caching is enabled by default and silence is not a disclaimer.
	"effect-conflict-default-cache-process.json": {"effect-conflict"},
	"source-mutation-unserialized.json":          {"effect-conflict"},
	"no-output-with-declared-outputs.json":       {"effect-conflict"},
	// EXACTNESS — an escape above the root and a glob.
	"declared-output-escapes-root.json": {"invalid-output-path", "invalid-output-path"},
	// EXACTNESS — a template variable and a non-slash separator.
	"declared-output-template-path.json": {"invalid-output-path", "invalid-output-path"},
	// ONE OWNER PER OUTPUT — a carve-out only means something inside the output
	// that states it: ceding the output itself declares nothing, and ceding a
	// path outside it gives away a region the task never owned.
	"declared-output-exclude-not-inside.json": {"invalid-output-exclude", "invalid-output-exclude"},
	// ONE OWNER PER OUTPUT — one subpath ceded twice, spelled differently, so a
	// manifest never means two things and the check compares normalized paths.
	"declared-output-exclude-duplicate.json": {"duplicate-output-exclude"},
	// ONE OWNER PER OUTPUT — a carve-out nothing can decide from the manifest:
	// a file has no subpath, and a pathFrom value is unknown until the run.
	"declared-output-exclude-undecidable.json": {"invalid-output-exclude", "invalid-output-exclude"},
	// SENSITIVE OUTPUTS — a secret declared durable would be captured, which is
	// the one path its bytes may never take.
	"sensitive-output-durable.json": {"invocation-scope-required"},
	// SENSITIVE OUTPUTS — a runtime file is meaningless outside the invocation
	// that wrote it, so it cannot be durable either.
	"runtime-file-durable-scope.json": {"invocation-scope-required"},
	// SENSITIVE OUTPUTS — a pathFrom value travels in the task result, so it
	// would publish the path the sensitive flag exists to withhold.
	"sensitive-output-path-from-port.json": {"sensitive-path-leak"},
	// INVOCATION SCOPE — a cache hit would report success without the artifact.
	"invocation-output-cached.json": {"invocation-cache-conflict"},
	// INVOCATION SCOPE — the invocation scratch is not a staging root, so
	// naming one says two contradictory things about where the file lands.
	"invocation-output-with-root.json": {"invocation-scope-conflict"},
	// DRIFT — a policy compares committed bytes: the command-output root is
	// never committed, a runtime file is never in the tree after the run, and
	// an invocation-scoped output is discarded with the invocation.
	"declared-output-drift-uncommitted.json": {"invalid-output-drift", "invalid-output-drift", "invalid-output-drift"},
	// DRIFT — the vocabulary is closed, and a pathFrom output's reference is a
	// digest of its root taken before the path is known, which only the
	// project tree is bounded enough for.
	"declared-output-drift-path-from-workspace.json": {"invalid-enum", "invalid-output-drift"},
	// PRESERVES — a file has no subpath, an entry never escapes the output,
	// and one path is preserved once, however it is spelled.
	"declared-output-preserve-invalid.json": {"invalid-output-path", "duplicate-output-preserve", "invalid-output-preserve"},
}

// lifecycleInvalidFixtureCodes is the manifest-level counterpart of
// v3InvalidFixtureCodes: counter-examples for the runtime and workspace
// sections and for pipeline-step run conditions, which are validated by
// ValidateManifest rather than by the task-contract harness. Keeping the two
// maps apart is what proves each rule fires from the phase that owns it.
var lifecycleInvalidFixtureCodes = map[string][]string{
	// WORKSPACE-INDEPENDENT PREPARATION — a workspace-rooted token would make
	// the produced artifact depend on where it was built.
	"runtime-prepare-workspace-dependent.json": {"invalid-template-var"},
	// STRICT PATHS — an executable that escapes the extension root and an
	// absolute prepare input.
	"runtime-invalid-paths.json": {"invalid-runtime-path", "invalid-runtime-path"},
	// THE DIGEST SEES THE SOURCES — a prepare with no inputs digests to the
	// same value forever.
	"runtime-prepare-without-inputs.json": {"required-field"},
	// Every lifecycle pattern must be syntactically valid, not just bounded.
	"lifecycle-invalid-glob-syntax.json": {"invalid-runtime-path", "invalid-workspace-path", "invalid-workspace-path"},
	// THE MARKERS ARE INPUTS — a marker nobody hashes leaves the snapshot valid
	// and the probe answer stale.
	"workspace-marker-not-input.json": {"marker-not-input"},
	// The sync task must exist, or workspace synchronization is unrunnable.
	"workspace-unresolved-sync-task.json": {"unresolved-sync-task"},
	// One step-gating vocabulary, closed.
	"step-run-on-invalid.json": {"invalid-enum"},
	// A finalizer produces no result a dependent can consume.
	"finalizer-binding.json": {"finalizer-binding"},
	// A finalizer relation is reserved for the invocation-artifact primitive.
	"finalizer-producer-without-invocation-output.json": {"finalizer-producer-without-invocation-output"},
	// Neither task ports nor command outputs may export a finalizer result.
	"finalizer-result-exports.json": {"finalizer-export", "finalizer-export"},
}

// TestConformance_FixtureProtocolVersions pins that adding the v3 vocabulary did
// not reclassify a single existing manifest: every fixture that predates the
// task contract must still report v2, so "v2 keeps behaving exactly as today"
// is a checked property rather than a claim. The v3 fixtures must report v3,
// which keeps the v2 half from passing because the vocabulary stopped working.
func TestConformance_FixtureProtocolVersions(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(v3ValidFixtures))
	for _, path := range files {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			m, diags := ParseManifest(data)
			if diag.HasErrors(diags) {
				t.Fatalf("parse: %v", diags)
			}
			want := ProtocolVersionV2
			if v3ValidFixtures[name] {
				seen[name] = true
				want = ProtocolVersionV3
			}
			if got := ManifestProtocolVersion(m); got != want {
				t.Fatalf("%s reports v%d, want v%d", path, got, want)
			}
		})
	}
	for name := range v3ValidFixtures {
		if !seen[name] {
			t.Errorf("v3 fixture %q is listed but missing from fixtures/valid", name)
		}
	}
}

// TestConformance_TaskContracts_ValidFixtures runs the shared harness over every
// valid fixture. It is the positive control for the counter-example test below:
// the same entry point that rejects each violation must accept a manifest that
// exercises the whole declaration vocabulary — every kind, every root, literal
// and port-supplied paths, optional-empty outputs, every effect, and source
// mutation.
func TestConformance_TaskContracts_ValidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no valid fixtures found: %v", err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			m := loadFixtureManifest(t, path)
			if diags := ValidateTaskContracts(m); len(diags) != 0 {
				t.Fatalf("valid fixture %s failed the task-contract harness: %v", path, diags)
			}
		})
	}
}

// TestConformance_TaskContracts_CoversEveryFeature keeps the positive control
// above from going vacuous: a valid fixture could stop exercising a declaration
// feature (a root, a kind, an effect) and the harness would still pass it. Each
// feature must appear in at least one valid fixture.
func TestConformance_TaskContracts_CoversEveryFeature(t *testing.T) {
	covered := make(map[string]bool)
	for name := range v3ValidFixtures {
		m := loadFixtureManifest(t, filepath.Join("fixtures/valid", name))
		for _, task := range m.Tasks {
			if task.Declares == nil {
				continue
			}
			if task.Declares.MutatesSources {
				covered["mutatesSources"] = true
			}
			for _, effect := range task.Declares.Effects {
				covered["effect:"+effect] = true
			}
			for _, output := range task.Declares.Outputs {
				covered["kind:"+output.Kind] = true
				covered["root:"+output.EffectiveRoot()] = true
				covered["scope:"+output.EffectiveScope()] = true
				if output.PathFrom != "" {
					covered["pathFrom"] = true
				}
				if output.Path != "" {
					covered["path"] = true
				}
				if output.OptionalEmpty {
					covered["optionalEmpty"] = true
				}
				if len(output.Excludes) != 0 {
					covered["excludes"] = true
				}
				if output.Sensitive {
					covered["sensitive"] = true
				}
			}
		}
	}

	features := []string{
		"mutatesSources", "pathFrom", "path", "optionalEmpty", "sensitive", "excludes",
		"kind:" + OutputKindFile, "kind:" + OutputKindDirectory, "kind:" + OutputKindRuntimeFile,
		"root:" + OutputRootProject, "root:" + OutputRootWorkspace, "root:" + OutputRootCommandOutput,
		"root:" + OutputRootInvocation,
		"scope:" + OutputScopeDurable, "scope:" + OutputScopeInvocation,
	}
	want := make([]string, 0, len(features)+len(ValidTaskEffects))
	want = append(want, features...)
	for _, effect := range ValidTaskEffects {
		want = append(want, "effect:"+effect)
	}
	for _, feature := range want {
		if !covered[feature] {
			t.Errorf("no valid fixture exercises %s", feature)
		}
	}
}

// TestConformance_TaskContracts_InvalidFixtures proves each counter-example is
// caught by the harness ALONE — not by an unrelated structural rule that
// happens to fire in the same file — and with the exact codes a consumer keys
// on.
func TestConformance_TaskContracts_InvalidFixtures(t *testing.T) {
	for name, codes := range v3InvalidFixtureCodes {
		t.Run(name, func(t *testing.T) {
			m := loadFixtureManifest(t, filepath.Join("fixtures/invalid", name))
			diags := ValidateTaskContracts(m)
			if !reflect.DeepEqual(diagCodes(diags), codes) {
				t.Fatalf("harness codes = %v, want %v (%v)", diagCodes(diags), codes, diags)
			}
		})
	}
}

// TestConformance_TaskContracts_StrictPathAgrees pins that the harness IS the
// strict path's v3 half rather than a parallel implementation of it: every
// diagnostic the harness reports must also come out of ValidateManifest, which
// is what `putnami dev extension validate`, the package-time gate and the
// extension SDK's build-time gate run.
func TestConformance_TaskContracts_StrictPathAgrees(t *testing.T) {
	for name := range v3InvalidFixtureCodes {
		t.Run(name, func(t *testing.T) {
			m := loadFixtureManifest(t, filepath.Join("fixtures/invalid", name))
			strict := make(map[string]bool)
			for _, d := range ValidateManifest(m) {
				strict[d.Code+" "+d.Field] = true
			}
			harness := ValidateTaskContracts(m)
			if len(harness) == 0 {
				t.Fatal("harness reported nothing for a counter-example")
			}
			for _, d := range harness {
				if !strict[d.Code+" "+d.Field] {
					t.Errorf("strict validation misses harness diagnostic %s at %s", d.Code, d.Field)
				}
			}
		})
	}
}

// TestConformance_TaskContracts_AdditiveForV2 pins the additive promise at the
// harness level: a manifest with no declaration gets no verdict. The mutation
// is the control — attaching one defective declaration to the SAME manifest
// must make the harness fire, so "no diagnostics" means "nothing to say", not
// "nothing wired up".
func TestConformance_TaskContracts_AdditiveForV2(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	mutated := 0
	for _, path := range files {
		if v3ValidFixtures[filepath.Base(path)] {
			continue
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			m := loadFixtureManifest(t, path)
			if diags := ValidateTaskContracts(m); len(diags) != 0 {
				t.Fatalf("v2 fixture %s got a v3 verdict: %v", path, diags)
			}
			for _, name := range sortedKeys(m.Tasks) {
				task := m.Tasks[name]
				task.Declares = &TaskDeclaration{Outputs: map[string]DeclaredOutput{
					"bad": {Kind: OutputKindDirectory, Path: "dist/**"},
				}}
				m.Tasks[name] = task
				mutated++
				break
			}
			if len(m.Tasks) == 0 {
				return
			}
			if !hasCode(ValidateTaskContracts(m), "invalid-output-path") {
				t.Fatal("control: a defective declaration on this manifest must be reported")
			}
		})
	}
	if mutated == 0 {
		t.Fatal("control never ran: no v2 fixture carried a task to mutate")
	}
}

// TestConformance_TaskContracts_NilManifest keeps the harness usable as a
// standalone entry point: a nil manifest is inert rather than a panic.
func TestConformance_TaskContracts_NilManifest(t *testing.T) {
	if diags := ValidateTaskContracts(nil); diags != nil {
		t.Fatalf("nil manifest produced %v", diags)
	}
}

// TestConformance_Lifecycles_InvalidFixtures pins the exact codes each
// manifest-level counter-example produces through the entry point consumers
// call. Exact-match rather than contains: a fixture that started failing for a
// second, unrelated reason would stop certifying the rule it was written for.
func TestConformance_Lifecycles_InvalidFixtures(t *testing.T) {
	for name, codes := range lifecycleInvalidFixtureCodes {
		t.Run(name, func(t *testing.T) {
			m := loadFixtureManifest(t, filepath.Join("fixtures/invalid", name))
			diags := FullValidateManifest(m)
			if !reflect.DeepEqual(diagCodes(diags), codes) {
				t.Fatalf("codes = %v, want %v (%v)", diagCodes(diags), codes, diags)
			}
		})
	}
}

// TestConformance_Lifecycles_AdditiveForV2 is the additivity control for the
// two manifest-level sections: every fixture that predates them must get no
// verdict from either validator, and the mutation proves the validators are
// actually wired rather than silently inert.
func TestConformance_Lifecycles_AdditiveForV2(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	mutated := 0
	for _, path := range files {
		if filepath.Base(path) == "lifecycle-v3.json" {
			continue
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			m := loadFixtureManifest(t, path)
			if m.DeclaresRuntime() || m.DeclaresWorkspaceAdapter() {
				t.Fatalf("%s unexpectedly declares a lifecycle section", path)
			}
			if diags := ValidateRuntime(m); len(diags) != 0 {
				t.Fatalf("runtime validation spoke about a manifest without a runtime: %v", diags)
			}
			if diags := ValidateWorkspaceAdapter(m); len(diags) != 0 {
				t.Fatalf("workspace validation spoke about a manifest without an adapter: %v", diags)
			}

			m.Runtime = &RuntimeDefinition{Executable: "../escape"}
			m.Workspace = &WorkspaceAdapter{Markers: []string{"go.mod"}, Inputs: []string{"go.sum"}}
			mutated++
			if !hasCode(ValidateRuntime(m), "invalid-runtime-path") {
				t.Error("control: a defective runtime section must be reported")
			}
			if !hasCode(ValidateWorkspaceAdapter(m), "marker-not-input") {
				t.Error("control: a defective workspace adapter must be reported")
			}
		})
	}
	if mutated == 0 {
		t.Fatal("control never ran: no fixture was available to mutate")
	}
}

// TestConformance_Lifecycles_NilManifest keeps both section validators usable
// as standalone entry points.
func TestConformance_Lifecycles_NilManifest(t *testing.T) {
	if diags := ValidateRuntime(nil); diags != nil {
		t.Fatalf("nil manifest produced %v", diags)
	}
	if diags := ValidateWorkspaceAdapter(nil); diags != nil {
		t.Fatalf("nil manifest produced %v", diags)
	}
	NormalizeRuntime(nil)
	NormalizeWorkspaceAdapter(nil)
}

func TestConformance_ValidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no valid fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			m, diags := ParseAndValidateManifest(data)
			if diag.HasErrors(diags) {
				t.Errorf("valid fixture %s produced errors: %v", path, diags)
			}
			if m == nil {
				t.Errorf("valid fixture %s returned nil manifest", path)
			}
		})
	}
}

func TestConformance_InvalidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/invalid/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no invalid fixtures found")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			m, pDiags := ParseManifest(data)
			if diag.HasErrors(pDiags) {
				return // parse error is sufficient for invalid fixtures
			}

			// Run full validation including DAG and schema ref checks.
			vDiags := FullValidateManifest(m)
			pDiags = append(pDiags, vDiags...)
			if len(pDiags) == 0 {
				t.Errorf("invalid fixture %s should produce diagnostics but none found", path)
			}
		})
	}
}

func TestConformance_FullValidateManifest_ValidFixtures(t *testing.T) {
	files, err := filepath.Glob("fixtures/valid/*.json")
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			m, pDiags := ParseManifest(data)
			if diag.HasErrors(pDiags) {
				t.Skipf("parse error: %v", pDiags)
			}

			vDiags := FullValidateManifest(m)
			if diag.HasErrors(vDiags) {
				t.Errorf("valid fixture %s failed full validation: %v", path, vDiags)
			}
		})
	}
}
