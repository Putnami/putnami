package extension

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	diag "go.putnami.dev/protocol/diagnostic"
)

// A reader before rung 11 has no representation for a command's kind,
// workload or effects, and would plan such a command as a plain one. A
// manifest that declares any of them therefore needs rung 11, and a lower
// stamp is refused with the reason.
func TestACommandKindRequiresTheCommandKindContract(t *testing.T) {
	t.Parallel()
	manifest := []byte(`{"name":"@test/lang","cliContract":10,"commands":{"check":{"traits":{"kind":"verification"},"run":[{"id":"check","task":"check"}]}},"tasks":{"check":{"kind":"command","command":"echo","timeoutMs":-1}}}`)
	_, err := NegotiateManifest("lang.json", manifest)
	if err == nil {
		t.Fatal("contract-10 stamp accepted a command kind")
	}
	for _, fragment := range []string{"requires 11 for its command kinds", "re-package the extension"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("refusal %q does not name %q", err, fragment)
		}
	}

	manifest = []byte(strings.Replace(string(manifest), `"cliContract":10`, `"cliContract":11`, 1))
	m, err := NegotiateManifest("lang.json", manifest)
	if err != nil {
		t.Fatalf("contract-11 stamp refused: %v", err)
	}
	if got := m.Commands["check"].Traits.Kind; got != CommandKindVerification {
		t.Fatalf("traits.kind = %q, want %q", got, CommandKindVerification)
	}
	if got := RequiredCLIContract(m); got != protocolcli.CommandKindContract {
		t.Fatalf("RequiredCLIContract = %d, want %d", got, protocolcli.CommandKindContract)
	}
	if got := RequiredLoadContract(m); got != protocolcli.CommandKindContract {
		t.Fatalf("RequiredLoadContract = %d, want %d: the rung binds every origin", got, protocolcli.CommandKindContract)
	}
}

// Each field raises the floor on its own, on a command, on a verb's default
// traits or on a task's batchable policy. Traits or a policy that declare none
// of them leave the floor where the manifest's other vocabulary puts it.
func TestEachCommandKindFieldAloneRaisesTheFloor(t *testing.T) {
	t.Parallel()
	command := func(traits *CommandTraits) *Manifest {
		return &Manifest{
			Commands: map[string]CommandDefinition{
				"plain": {Run: []PipelineStep{{ID: "a", Task: "a"}}},
				"check": {Traits: traits, Run: []PipelineStep{{ID: "a", Task: "a"}}},
			},
			Tasks: map[string]TaskDefinition{"a": {Kind: "command", Command: "echo", TimeoutMs: -1}},
		}
	}
	verb := func(traits *CommandTraits) *Manifest {
		return &Manifest{Verbs: []VerbDefinition{
			{Name: "build", Description: "Build."},
			{Name: "check", Description: "Check.", Traits: traits},
		}}
	}
	batch := func(policy *TaskBatchPolicy) *Manifest {
		return &Manifest{
			Commands: map[string]CommandDefinition{"lint": {Run: []PipelineStep{{ID: "a", Task: "a"}, {ID: "b", Task: "b"}}}},
			Tasks: map[string]TaskDefinition{
				"a": {Kind: "command", Command: "echo", TimeoutMs: -1, Batchable: &TaskBatchPolicy{Tool: "plain"}},
				"b": {Kind: "command", Command: "echo", TimeoutMs: -1, Batchable: policy},
			},
		}
	}

	for _, test := range []struct {
		name     string
		manifest *Manifest
		want     int
	}{
		{"command kind", command(&CommandTraits{Kind: CommandKindFiles}), protocolcli.CommandKindContract},
		{"command workload", command(&CommandTraits{Workload: WorkloadServe}), protocolcli.CommandKindContract},
		{"command rewritesSources", command(&CommandTraits{RewritesSources: true}), protocolcli.CommandKindContract},
		{"command consumesReleaseSet", command(&CommandTraits{ConsumesReleaseSet: true}), protocolcli.CommandKindContract},
		{"command traits without them", command(&CommandTraits{Heavy: true, SideEffects: SideEffectsCloud}), protocolcli.CurrentContract},
		{"command without traits", command(nil), protocolcli.CurrentContract},
		{"verb kind", verb(&CommandTraits{Kind: CommandKindCriteria}), protocolcli.CommandKindContract},
		{"verb workload", verb(&CommandTraits{RequiresRunnable: true, Workload: WorkloadOnce}), protocolcli.CommandKindContract},
		{"verb traits without them", verb(&CommandTraits{RequiresRunnable: true}), protocolcli.VerbVocabularyContract},
		{"batchable kind", batch(&TaskBatchPolicy{Tool: "lint", Kind: CommandKindDiagnostics}), protocolcli.CommandKindContract},
		{"batchable phase", batch(&TaskBatchPolicy{Tool: "lint", Phase: "lint"}), protocolcli.CommandKindContract},
		{"batchable without them", batch(&TaskBatchPolicy{Tool: "lint", MaxProjects: 4}), protocolcli.CurrentContract},
		{"no batchable", batch(nil), protocolcli.CurrentContract},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// Map iteration order is random; repeat so an early return on a
			// lower rung would show.
			for range 32 {
				if got := RequiredCLIContract(test.manifest); got != test.want {
					t.Fatalf("RequiredCLIContract = %d, want %d", got, test.want)
				}
				if got := RequiredLoadContract(test.manifest); got != test.want {
					t.Fatalf("RequiredLoadContract = %d, want %d", got, test.want)
				}
			}
		})
	}
}

// A kind, a workload or a phase outside its closed vocabulary fails strict
// validation, so it never earns a stamp. Every value inside it validates.
func TestAnUnknownKindWorkloadOrPhaseIsRefused(t *testing.T) {
	t.Parallel()
	invalid := []byte(`{
  "name": "@test/lang",
  "commands": {
    "check": {"traits": {"kind": "report"}, "run": [{"id": "a", "task": "a"}]},
    "watch": {"traits": {"workload": "daemon"}, "run": [{"id": "a", "task": "a"}]}
  },
  "tasks": {
    "a": {"kind": "command", "command": "echo", "timeoutMs": -1, "batchable": {"tool": "lint", "kind": "files", "phase": "Lint"}}
  },
  "verbs": [{"name": "check", "description": "Check.", "traits": {"kind": "status", "workload": "forever"}}]
}`)
	m, diags := ParseManifest(invalid)
	if diag.HasErrors(diags) {
		t.Fatal(diags)
	}
	diags = FullValidateManifest(m)
	want := []string{
		"invalid-enum",  // commands.check.traits.kind
		"invalid-enum",  // commands.watch.traits.workload
		"invalid-enum",  // tasks.a.batchable.kind
		"invalid-value", // tasks.a.batchable.phase
		"invalid-enum",  // verbs[0].traits.kind
		"invalid-enum",  // verbs[0].traits.workload
	}
	if got := diagCodes(diags); !reflect.DeepEqual(got, want) {
		t.Fatalf("codes = %v, want %v (%v)", got, want, diags)
	}
	fields := make([]string, 0, len(diags))
	for _, d := range diag.Errors(diags) {
		fields = append(fields, d.Field)
	}
	wantFields := []string{
		"commands.check.traits.kind", "commands.watch.traits.workload",
		"tasks.a.batchable.kind", "tasks.a.batchable.phase",
		"verbs[0].traits.kind", "verbs[0].traits.workload",
	}
	if !reflect.DeepEqual(fields, wantFields) {
		t.Fatalf("fields = %v, want %v", fields, wantFields)
	}

	for _, kind := range ValidCommandKinds {
		traits := &CommandTraits{Kind: kind}
		for _, workload := range ValidWorkloads {
			traits.Workload = workload
			if diags := ValidateTraits(traits, "traits"); len(diags) != 0 {
				t.Errorf("kind %q with workload %q refused: %v", kind, workload, diags)
			}
		}
	}
	for _, kind := range ValidBatchKinds {
		m := &Manifest{
			Commands: map[string]CommandDefinition{"lint": {Run: []PipelineStep{{ID: "a", Task: "a"}}}},
			Tasks: map[string]TaskDefinition{"a": {Kind: "command", Command: "echo", TimeoutMs: -1,
				Batchable: &TaskBatchPolicy{Tool: "lint", Kind: kind, Phase: "lint-check"}}},
		}
		if diags := diag.Errors(ValidateManifest(m)); len(diags) != 0 {
			t.Errorf("batchable kind %q refused: %v", kind, diags)
		}
	}
}

// The fields are omitted when empty. Leaving them out keeps the JSON of every
// traits object and batch policy that declares none of them unchanged, which
// keeps the contract digest, and so the cache key, of every task that does not
// declare them. A task that does declare them changes what its batch records,
// so its digest moves.
func TestTheCommandKindFieldsAreOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	traits, err := json.Marshal(CommandTraits{Heavy: true, SideEffects: SideEffectsRegistry})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := json.Marshal(TaskBatchPolicy{Tool: "lint", MaxProjects: 4})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(traits), `{"heavy":true,"sideEffects":"registry"}`; got != want {
		t.Errorf("traits serialize as %s, want %s", got, want)
	}
	if got, want := string(policy), `{"tool":"lint","maxProjects":4}`; got != want {
		t.Errorf("batchable serializes as %s, want %s", got, want)
	}

	task := func(policy TaskBatchPolicy) TaskDefinition {
		return TaskDefinition{Kind: "command", Command: "echo", TimeoutMs: -1, Batchable: &policy}
	}
	base := TaskContractDigest(task(TaskBatchPolicy{Tool: "lint"}))
	if got := TaskContractDigest(task(TaskBatchPolicy{Tool: "lint", Kind: "", Phase: ""})); got != base {
		t.Fatal("an empty kind and phase moved the task contract digest")
	}
	for name, policy := range map[string]TaskBatchPolicy{
		"kind":  {Tool: "lint", Kind: CommandKindDiagnostics},
		"phase": {Tool: "lint", Phase: "lint"},
	} {
		if TaskContractDigest(task(policy)) == base {
			t.Errorf("a declared batchable %s left the task contract digest unchanged", name)
		}
	}
}
