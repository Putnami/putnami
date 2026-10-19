package extension

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
)

// firstPartyBatchPolicies is the batch kind and phase each first-party
// batchable task declares, by extension path and task name. The kind decides a
// batch member's tally and the phase its reconstructed phase name. A task
// declares each only where its step id or its silence would give a batch
// member a different tally or phase than its solo run reports.
var firstPartyBatchPolicies = map[string]map[string]proto.TaskBatchPolicy{
	filepath.Join("go", "extension"): {
		"lint-golangci-fix":      {Kind: proto.CommandKindDiagnostics, Phase: "lint"},
		"lint-golangci-readonly": {Kind: proto.CommandKindDiagnostics, Phase: "lint"},
		"lint-staticcheck":       {Kind: proto.CommandKindDiagnostics, Phase: "lint"},
		"test-exec":              {Kind: proto.CommandKindVerification},
	},
	filepath.Join("typescript", "extension"): {
		"build-compile":   {},
		"build-generate":  {},
		"build-transpile": {},
		"build-types":     {},
		"lint-all":        {Kind: proto.CommandKindDiagnostics, Phase: "lint"},
		"lint-check":      {Phase: "lint"},
		"lint-format":     {},
		"test-run":        {Kind: proto.CommandKindVerification},
	},
	filepath.Join("python", "extension"): {
		"lint-check-fix":       {Phase: "lint"},
		"lint-check-readonly":  {Phase: "lint"},
		"lint-format-fix":      {},
		"lint-format-readonly": {Phase: "format"},
		"test-run":             {Kind: proto.CommandKindVerification},
	},
}

// firstPartyStepPhases is the phase a batch member reports when its task
// declares none: the id of every step that runs the task.
var firstPartyStepPhases = map[string]string{
	"build-compile":   "compile",
	"build-generate":  "generate",
	"build-transpile": "transpile",
	"build-types":     "types",
	"lint-format":     "format",
	"lint-format-fix": "format",
}

// TestTheFirstPartyBatchableTasksKeepTheirTallyAndPhase pins the batch kind and
// phase of every batchable task the Go, TypeScript and Python extensions
// declare, and the step id that names the phase of a task that declares
// none, so a batch member keeps the tally and the phase its solo run reports.
func TestTheFirstPartyBatchableTasksKeepTheirTallyAndPhase(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "batch-parity-and-live-cells-follow-declared-kinds",
		"the-first-party-batchable-tasks-keep-their-tally-and-phase")
	repoRoot := findRepoRoot(t)
	for _, rel := range slices.Sorted(maps.Keys(firstPartyBatchPolicies)) {
		want := firstPartyBatchPolicies[rel]
		t.Run(filepath.ToSlash(rel), func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join(repoRoot, rel, ManifestFilename))
			if err != nil {
				t.Fatal(err)
			}
			manifest, diags := proto.ParseManifest(data)
			if manifest == nil || diag.HasErrors(diags) {
				t.Fatalf("the manifest does not parse: %v", diags)
			}
			batchable := map[string]bool{}
			for name, task := range manifest.Tasks {
				if task.Batchable == nil {
					continue
				}
				batchable[name] = true
				policy, pinned := want[name]
				if !pinned {
					t.Errorf("task %s is batchable with kind %q and phase %q; pin its tally and phase here",
						name, task.Batchable.Kind, task.Batchable.Phase)
					continue
				}
				if task.Batchable.Kind != policy.Kind || task.Batchable.Phase != policy.Phase {
					t.Errorf("task %s declares kind %q and phase %q, want %q and %q",
						name, task.Batchable.Kind, task.Batchable.Phase, policy.Kind, policy.Phase)
				}
			}
			for name := range want {
				if !batchable[name] {
					t.Errorf("task %s is pinned but not batchable", name)
				}
			}
			stepped := map[string]bool{}
			for command, def := range manifest.Commands {
				for _, step := range def.Run {
					task, ok := manifest.Tasks[step.Task]
					if !ok || task.Batchable == nil || task.Batchable.Phase != "" {
						continue
					}
					if phase, named := firstPartyStepPhases[step.Task]; named {
						stepped[step.Task] = true
						if step.ID != phase {
							t.Errorf("%s~%s runs %s, whose batch phase is its step id: want step id %q",
								command, step.ID, step.Task, phase)
						}
					}
				}
			}
			for name := range want {
				if _, named := firstPartyStepPhases[name]; named && !stepped[name] {
					t.Errorf("no command runs %s: the step id pin no longer covers it", name)
				}
			}
		})
	}
}
