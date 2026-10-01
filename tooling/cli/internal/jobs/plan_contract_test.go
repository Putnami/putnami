package jobs

import (
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Plan-time enforcement of the v3 task contract.
// Every fixture here declares synthetic v3 blocks: the checks are keyed on
// `declares`, so the rest of this package's tests — all v2 — double as the
// vacuity control, and TestPlan_ContractChecksAreVacuousForV2 states it
// explicitly.

func contractWorkspace(t *testing.T, extensionName string) (*workspace.Workspace, *workspace.Project) {
	t.Helper()
	proj := &workspace.Project{ID: "/app", Name: "app", Path: "packages/app", Extensions: []string{extensionName}}
	ws := workspace.NewWorkspace("/ws", nil, []*workspace.Project{proj})
	ws.Name = "contract-ws"
	return ws, proj
}

// declaringExtension builds an extension whose commands each run one step, with
// the step's task carrying the given v3 declaration.
func declaringExtension(name string, commands map[string]extension.TaskDefinition) *extension.ExtensionDescription {
	ext := &extension.ExtensionDescription{
		Name:  name,
		Jobs:  map[string]*extension.JobDefinition{},
		Tasks: map[string]extension.TaskDefinition{},
	}
	for cmdName, task := range commands {
		taskName := cmdName + "-task"
		ext.Tasks[taskName] = task
		ext.Jobs[cmdName] = &extension.JobDefinition{
			ExtensionName: name,
			Name:          cmdName,
			Kind:          "command",
			PipelineSteps: []extension.PipelineStep{{ID: "step", Task: taskName}},
		}
	}
	return ext
}

func declaresOutput(kind, root, path string) *extension.TaskDeclaration {
	return &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"artifact": {Kind: kind, Root: root, Path: path},
		},
	}
}

func planCommands(t *testing.T, ws *workspace.Workspace, ext *extension.ExtensionDescription, commands ...string) error {
	t.Helper()
	_, err := Plan(ws, commands, ws.Projects, []*extension.ExtensionDescription{ext}, nil, nil, nil)
	return err
}

// TestPlan_RejectsOverlappingDeclaredOutputs pins ONE OWNER PER OUTPUT across
// the plan: two tasks claiming the same project subtree — here a directory and
// a file nested inside it — is a plan-time error naming BOTH owners.
func TestPlan_RejectsOverlappingDeclaredOutputs(t *testing.T) {
	t.Parallel()
	ws, _ := contractWorkspace(t, "@test/decl")
	ext := declaringExtension("@test/decl", map[string]extension.TaskDefinition{
		"build": {Kind: "command", Command: "true",
			Declares: declaresOutput(extension.OutputKindDirectory, extension.OutputRootProject, ".gen")},
		"package": {Kind: "command", Command: "true",
			Declares: declaresOutput(extension.OutputKindFile, extension.OutputRootProject, ".gen/client.ts")},
	})

	err := planCommands(t, ws, ext, "build", "package")
	if err == nil {
		t.Fatal("overlapping declared outputs planned cleanly")
	}
	for _, want := range []string{"overlapping declared outputs", "/app:build~step", "/app:package~step", ".gen"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestPlan_AcceptsDisjointDeclaredOutputs is the positive control: the same
// fixture with disjoint paths plans. Without it, a check that rejected
// everything would pass the test above.
func TestPlan_AcceptsDisjointDeclaredOutputs(t *testing.T) {
	t.Parallel()
	ws, _ := contractWorkspace(t, "@test/decl")
	ext := declaringExtension("@test/decl", map[string]extension.TaskDefinition{
		"build": {Kind: "command", Command: "true",
			Declares: declaresOutput(extension.OutputKindDirectory, extension.OutputRootProject, ".gen")},
		"package": {Kind: "command", Command: "true",
			Declares: declaresOutput(extension.OutputKindFile, extension.OutputRootProject, "dist/client.ts")},
	})

	if err := planCommands(t, ws, ext, "build", "package"); err != nil {
		t.Fatalf("disjoint declared outputs rejected: %v", err)
	}
}

// TestPlan_RejectsCrossRootOutputOverlap is the check the manifest-local half
// structurally cannot make: a workspace-rooted path that lands inside a project
// collides with that project's project-rooted output only once both roots are
// resolved to concrete directories, which happens at plan time.
func TestPlan_RejectsCrossRootOutputOverlap(t *testing.T) {
	t.Parallel()
	ws, _ := contractWorkspace(t, "@test/decl")
	ext := declaringExtension("@test/decl", map[string]extension.TaskDefinition{
		"build": {Kind: "command", Command: "true",
			Declares: declaresOutput(extension.OutputKindDirectory, extension.OutputRootProject, ".gen")},
		"package": {Kind: "command", Command: "true",
			Declares: declaresOutput(extension.OutputKindFile, extension.OutputRootWorkspace, "packages/app/.gen/client.ts")},
	})

	err := planCommands(t, ws, ext, "build", "package")
	if err == nil {
		t.Fatal("a workspace-rooted path inside the project planned cleanly")
	}
	if !strings.Contains(err.Error(), "overlapping declared outputs") {
		t.Errorf("error %q is not the ownership rejection", err)
	}

	// Control: the same workspace-rooted declaration outside the project does
	// not collide.
	other := declaringExtension("@test/decl", map[string]extension.TaskDefinition{
		"build": {Kind: "command", Command: "true",
			Declares: declaresOutput(extension.OutputKindDirectory, extension.OutputRootProject, ".gen")},
		"package": {Kind: "command", Command: "true",
			Declares: declaresOutput(extension.OutputKindFile, extension.OutputRootWorkspace, "packages/other/.gen/client.ts")},
	})
	if err := planCommands(t, ws, other, "build", "package"); err != nil {
		t.Fatalf("disjoint cross-root outputs rejected: %v", err)
	}
}

// TestPlan_CommandOutputOwnershipIsScopedToTheCommand pins the contract's
// command-output rule: the per-command output directory is SHARED by the steps
// of one command (so two of them naming one file collide) and separate between
// commands (so the same relative name in two commands does not).
func TestPlan_CommandOutputOwnershipIsScopedToTheCommand(t *testing.T) {
	t.Parallel()
	ws, _ := contractWorkspace(t, "@test/decl")

	t.Run("two commands may use the same relative name", func(t *testing.T) {
		ext := declaringExtension("@test/decl", map[string]extension.TaskDefinition{
			"build": {Kind: "command", Command: "true",
				Declares: declaresOutput(extension.OutputKindFile, extension.OutputRootCommandOutput, "report.json")},
			"test": {Kind: "command", Command: "true",
				Declares: declaresOutput(extension.OutputKindFile, extension.OutputRootCommandOutput, "report.json")},
		})
		if err := planCommands(t, ws, ext, "build", "test"); err != nil {
			t.Fatalf("same name in two command directories rejected: %v", err)
		}
	})

	t.Run("two steps of one command may not", func(t *testing.T) {
		ext := &extension.ExtensionDescription{
			Name: "@test/decl",
			Jobs: map[string]*extension.JobDefinition{
				"build": {
					ExtensionName: "@test/decl",
					Name:          "build",
					Kind:          "command",
					PipelineSteps: []extension.PipelineStep{
						{ID: "first", Task: "first-task"},
						{ID: "second", Task: "second-task"},
					},
				},
			},
			Tasks: map[string]extension.TaskDefinition{
				"first-task": {Kind: "command", Command: "true",
					Declares: declaresOutput(extension.OutputKindFile, extension.OutputRootCommandOutput, "report.json")},
				"second-task": {Kind: "command", Command: "true",
					Declares: declaresOutput(extension.OutputKindFile, extension.OutputRootCommandOutput, "report.json")},
			},
		}
		err := planCommands(t, ws, ext, "build")
		if err == nil {
			t.Fatal("two steps of one command claiming one output file planned cleanly")
		}
		for _, want := range []string{"/app:build~first", "/app:build~second"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})
}

// TestPlan_RejectsExternalEffectOnCacheableTask pins HONEST EFFECTS on the
// PLAN node: a task whose consequences a cache hit cannot reproduce may not be
// cacheable. The plan is where the effective answer lives — an installed
// extension's manifest never travels through `dev extension validate` in the
// consuming workspace.
func TestPlan_RejectsExternalEffectOnCacheableTask(t *testing.T) {
	t.Parallel()
	ws, _ := contractWorkspace(t, "@test/decl")
	ext := declaringExtension("@test/decl", map[string]extension.TaskDefinition{
		"publish": {Kind: "command", Command: "true",
			Declares: &extension.TaskDeclaration{Effects: []string{extension.EffectNetwork, extension.EffectRegistry}}},
	})

	err := planCommands(t, ws, ext, "publish")
	if err == nil {
		t.Fatal("a cacheable task declaring a registry push planned cleanly")
	}
	for _, want := range []string{"conflicting task effects", "/app:publish~step", "registry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	// `network` is declarable on a cacheable task by design — a cache hit
	// legitimately skips it — so the rejection must not name it.
	if strings.Contains(err.Error(), extension.EffectNetwork) {
		t.Errorf("error %q rejects the non-external effect %q", err, extension.EffectNetwork)
	}
}

// TestPlan_AcceptsExternalEffectOnUncacheableTask is the positive control: the
// same declaration on a task that says it is not cacheable is exactly what the
// contract asks for.
func TestPlan_AcceptsExternalEffectOnUncacheableTask(t *testing.T) {
	t.Parallel()
	ws, _ := contractWorkspace(t, "@test/decl")
	disabled := false
	ext := declaringExtension("@test/decl", map[string]extension.TaskDefinition{
		"publish": {Kind: "command", Command: "true",
			Cache:    &extension.TaskCachePolicy{Enabled: &disabled},
			Declares: &extension.TaskDeclaration{Effects: []string{extension.EffectRegistry}}},
	})

	if err := planCommands(t, ws, ext, "publish"); err != nil {
		t.Fatalf("uncacheable external-effect task rejected: %v", err)
	}
}

// TestPlan_ContractChecksAreVacuousForV2 is the mutation control for the whole
// file: strip the `declares` blocks and the very fixtures that are rejected
// above plan cleanly. A check that keyed on anything other than the declaration
// would fail here — and would have changed the verdict for every v2 manifest in
// the workspace.
func TestPlan_ContractChecksAreVacuousForV2(t *testing.T) {
	t.Parallel()
	ws, _ := contractWorkspace(t, "@test/decl")
	ext := declaringExtension("@test/decl", map[string]extension.TaskDefinition{
		"build":   {Kind: "command", Command: "true"},
		"package": {Kind: "command", Command: "true"},
		"publish": {Kind: "command", Command: "true"},
	})

	if err := planCommands(t, ws, ext, "build", "package", "publish"); err != nil {
		t.Fatalf("v2 tasks rejected by a v3 check: %v", err)
	}

	// Positive control for the vacuity claim: adding the declarations back to
	// the SAME fixture must reject it, or "vacuous" would just mean "inert".
	overlapping := declaringExtension("@test/decl", map[string]extension.TaskDefinition{
		"build": {Kind: "command", Command: "true",
			Declares: declaresOutput(extension.OutputKindDirectory, extension.OutputRootProject, ".gen")},
		"package": {Kind: "command", Command: "true",
			Declares: declaresOutput(extension.OutputKindFile, extension.OutputRootProject, ".gen/client.ts")},
		"publish": {Kind: "command", Command: "true"},
	})
	if err := planCommands(t, ws, overlapping, "build", "package", "publish"); err == nil {
		t.Fatal("the declared fixture planned cleanly; the vacuity control proves nothing")
	}
}

// TestPlan_ContractDiagnosticsAreDeterministic pins the diagnostic text itself:
// the same plan must produce byte-identical rejections, or a reviewer cannot
// tell a real change from map iteration order.
func TestPlan_ContractDiagnosticsAreDeterministic(t *testing.T) {
	t.Parallel()
	ws, _ := contractWorkspace(t, "@test/decl")
	ext := declaringExtension("@test/decl", map[string]extension.TaskDefinition{
		"build": {Kind: "command", Command: "true", Declares: &extension.TaskDeclaration{
			Outputs: map[string]extension.DeclaredOutput{
				"gen":      {Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: ".gen"},
				"manifest": {Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: ".gen/manifest.json"},
				"types":    {Kind: extension.OutputKindFile, Root: extension.OutputRootProject, Path: ".gen/types.d.ts"},
			},
		}},
	})

	first := ""
	for i := 0; i < 20; i++ {
		err := planCommands(t, ws, ext, "build")
		if err == nil {
			t.Fatal("overlapping outputs within one declaration planned cleanly")
		}
		if i == 0 {
			first = err.Error()
			continue
		}
		if err.Error() != first {
			t.Fatalf("diagnostic is nondeterministic:\n--- first ---\n%s\n--- run %d ---\n%s", first, i+1, err)
		}
	}
}

// TestResolveDeclaredOutput_PortRefsCompareByPort pins the unresolved half of
// the contract: an output whose path comes from a task output port is compared
// by port under the resolved root, and is never comparable to a literal path.
func TestResolveDeclaredOutput_PortRefsCompareByPort(t *testing.T) {
	t.Parallel()
	ws, proj := contractWorkspace(t, "@test/decl")
	job := &ScheduledJob{
		Project:   proj,
		Extension: &extension.ExtensionDescription{Name: "@test/decl"},
		JobDef:    &extension.JobDefinition{Name: "build~step"},
	}

	port, ok := resolveDeclaredOutput(job, extension.DeclaredOutput{
		Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, PathFrom: "clientDir",
	}, ws)
	if !ok {
		t.Fatal("port-backed output did not resolve")
	}
	if port.Resolved() {
		t.Errorf("port-backed ref %+v claims to be resolved", port)
	}

	samePort, _ := resolveDeclaredOutput(job, extension.DeclaredOutput{
		Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, PathFrom: "clientDir",
	}, ws)
	if !extension.OutputsOverlap(port, samePort) {
		t.Error("the same port under the same root does not overlap itself")
	}

	literal, _ := resolveDeclaredOutput(job, extension.DeclaredOutput{
		Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: "clients",
	}, ws)
	if extension.OutputsOverlap(port, literal) {
		t.Error("an unresolved port was compared against a literal path")
	}

	otherPort, _ := resolveDeclaredOutput(job, extension.DeclaredOutput{
		Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, PathFrom: "docsDir",
	}, ws)
	if extension.OutputsOverlap(port, otherPort) {
		t.Error("two different ports overlap")
	}
}

// TestResolveDeclaredOutput_SkipsUnownablePaths pins that a path the contract
// cannot own (a glob, an escape) yields no ownership claim at all: guessing at
// the region such a declaration covers would be worse than saying nothing, and
// manifest validation reports the path itself.
func TestResolveDeclaredOutput_SkipsUnownablePaths(t *testing.T) {
	t.Parallel()
	ws, proj := contractWorkspace(t, "@test/decl")
	job := &ScheduledJob{
		Project:   proj,
		Extension: &extension.ExtensionDescription{Name: "@test/decl"},
		JobDef:    &extension.JobDefinition{Name: "build~step"},
	}
	for _, path := range []string{"dist/**/*.js", "../outside", "."} {
		if _, ok := resolveDeclaredOutput(job, extension.DeclaredOutput{
			Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject, Path: path,
		}, ws); ok {
			t.Errorf("unownable path %q produced an ownership claim", path)
		}
	}
	if _, ok := resolveDeclaredOutput(job, extension.DeclaredOutput{
		Kind: extension.OutputKindDirectory, Root: "invented-root", Path: "dist",
	}, ws); ok {
		t.Error("an unknown output root produced an ownership claim")
	}
}

// TestPlanContract_OneTaskScheduledBySeveralCommandsIsOneOwner pins the
// exemption an earlier fix forced: a generate task declared once but scheduled by
// two commands (build and test) is ONE owner executing twice, not an
// overlapping pair — while two DIFFERENT tasks claiming the same path remain
// rejected (the positive control below).
func TestPlanContract_OneTaskScheduledBySeveralCommandsIsOneOwner(t *testing.T) {
	t.Parallel()
	proj := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := &workspace.Workspace{Root: t.TempDir(), Projects: []*workspace.Project{proj}}
	declares := &extension.TaskDeclaration{
		Outputs: map[string]extension.DeclaredOutput{
			"gen": {Kind: extension.OutputKindDirectory, Path: ".gen"},
		},
	}
	ext := &extension.ExtensionDescription{
		Name: "@x/ts",
		Tasks: map[string]extension.TaskDefinition{
			"build-generate": {Kind: "command", Command: "true", Declares: declares},
			"other-generate": {Kind: "command", Command: "true", Declares: declares},
		},
	}
	node := func(command, task string) *ScheduledJob {
		return &ScheduledJob{
			Project:   proj,
			Extension: ext,
			JobDef:    &extension.JobDefinition{Name: command + "~generate", CommandName: command, StepID: "generate"},
			Step:      &extension.PipelineStep{ID: "generate", Task: task},
		}
	}

	sameTaskTwice := []*ScheduledJob{node("build", "build-generate"), node("test", "build-generate")}
	if lines := overlappingOutputLines(sameTaskTwice, ws, 10); len(lines) != 0 {
		t.Fatalf("one task scheduled by two commands was reported as overlapping:\n%s", strings.Join(lines, "\n"))
	}

	// Positive control: two DIFFERENT manifest tasks, same path — still an error.
	pair := []*ScheduledJob{node("build", "build-generate"), node("test", "other-generate")}
	if lines := overlappingOutputLines(pair, ws, 10); len(lines) == 0 {
		t.Fatal("two different tasks claiming .gen were not reported")
	}
}

// TestPlanContract_CededSubpathHasOneOwner pins the carve-out at PLAN time, on
// the real shape the Go extension declares: one task owns <project>/.gen
// except .gen/migration-bundle, the next task owns that subpath, and the plan
// accepts it — the resolved-ref check must apply the same predicate the
// manifest-local one applies, or one of the two halves rejects a manifest the
// other accepted.
//
// Two controls keep it honest: without the carve-out the same pair is an
// overlap, and a THIRD task claiming the ceded subpath collides with the task
// that took it — a cede moves ownership, it does not open the region up.
func TestPlanContract_CededSubpathHasOneOwner(t *testing.T) {
	t.Parallel()
	proj := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	ws := &workspace.Workspace{Root: t.TempDir(), Projects: []*workspace.Project{proj}}

	genOutput := func(excludes ...string) extension.DeclaredOutput {
		return extension.DeclaredOutput{
			Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject,
			Path: ".gen", Excludes: excludes,
		}
	}
	bundleOutput := extension.DeclaredOutput{
		Kind: extension.OutputKindDirectory, Root: extension.OutputRootProject,
		Path: ".gen/migration-bundle", OptionalEmpty: true,
	}
	ext := func(gen extension.DeclaredOutput) *extension.ExtensionDescription {
		return &extension.ExtensionDescription{
			Name: "@putnami/go-like",
			Tasks: map[string]extension.TaskDefinition{
				"build-generate": {Kind: "command", Command: "true", Declares: &extension.TaskDeclaration{
					Outputs: map[string]extension.DeclaredOutput{"gen": gen},
				}},
				"build-describe": {Kind: "command", Command: "true", Declares: &extension.TaskDeclaration{
					Outputs: map[string]extension.DeclaredOutput{"migrationBundle": bundleOutput},
				}},
				"third-party": {Kind: "command", Command: "true", Declares: &extension.TaskDeclaration{
					Outputs: map[string]extension.DeclaredOutput{"bundleFile": {
						Kind: extension.OutputKindFile, Root: extension.OutputRootProject,
						Path: ".gen/migration-bundle/bundle.json",
					}},
				}},
			},
		}
	}
	node := func(ext *extension.ExtensionDescription, step, task string) *ScheduledJob {
		return &ScheduledJob{
			Project:   proj,
			Extension: ext,
			JobDef:    &extension.JobDefinition{Name: "build~" + step, CommandName: "build", StepID: step},
			Step:      &extension.PipelineStep{ID: step, Task: task},
		}
	}

	ceding := ext(genOutput(".gen/migration-bundle"))
	pair := []*ScheduledJob{
		node(ceding, "generate", "build-generate"),
		node(ceding, "describe", "build-describe"),
	}
	if lines := overlappingOutputLines(pair, ws, 10); len(lines) != 0 {
		t.Fatalf("a ceded subpath was reported as a second owner:\n%s", strings.Join(lines, "\n"))
	}

	// Control: the same pair without the carve-out is exactly the collision the
	// carve-out exists to resolve.
	owningWhole := ext(genOutput())
	whole := []*ScheduledJob{
		node(owningWhole, "generate", "build-generate"),
		node(owningWhole, "describe", "build-describe"),
	}
	if lines := overlappingOutputLines(whole, ws, 10); len(lines) == 0 {
		t.Fatal("a task claiming a subpath of another task's undivided subtree was not reported")
	}

	// Control: one task cannot cede a subpath to ITSELF. The pipeline orders two
	// tasks, so the owning task's restore follows the ceding task's; nothing
	// orders one task's own outputs, so the plan must keep rejecting that shape.
	selfCeding := &extension.ExtensionDescription{
		Name: "@putnami/go-like",
		Tasks: map[string]extension.TaskDefinition{
			"build-generate": {Kind: "command", Command: "true", Declares: &extension.TaskDeclaration{
				Outputs: map[string]extension.DeclaredOutput{
					"gen":             genOutput(".gen/migration-bundle"),
					"migrationBundle": bundleOutput,
				},
			}},
		},
	}
	if lines := overlappingOutputLines([]*ScheduledJob{node(selfCeding, "generate", "build-generate")}, ws, 10); len(lines) == 0 {
		t.Error("a task ceding a subpath to itself was accepted; its two outputs have no restore order")
	}

	// Control: the ceded region has ONE owner, not none.
	contested := append(append([]*ScheduledJob{}, pair...), node(ceding, "third", "third-party"))
	lines := overlappingOutputLines(contested, ws, 10)
	if len(lines) == 0 {
		t.Fatal("a third task claiming the ceded subpath was not reported")
	}
	for _, line := range lines {
		if !strings.Contains(line, "build~third") || !strings.Contains(line, "build~describe") {
			t.Errorf("collision line %q does not name the two claimants of the ceded region", line)
		}
	}
}
