package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The native validation gate: `putnami validate` must reject new handwritten
// first-party transports and inconsistent committed clients for the WHOLE
// workspace, from any selection, and must judge a selected provider's client
// drift in the same session.
//
// Three properties make that true, and none is visible from the extension's
// own tests:
//
//  1. ACTIVATION. The guard is contributed as a `validate` command with
//     workspace-once activation. matchWorkspaceOnceJobs never consults a
//     project's extension list, so no project can opt out of the guard by not
//     depending on @putnami/clientgen — and it plans exactly one node however
//     many projects the selection resolves to.
//  2. GENERATION BEFORE THE GUARD. The guard's command depends on the
//     `!clientgen` session barrier: the planner plans `clientgen` for every
//     selected project that declares the extension (a provider), and orders
//     the guard after those leaves. A selected provider therefore regenerates
//     every target in place, with the engine judging drift on the generators'
//     declared outputs (task_drift.go), before the guard reads the tree. A
//     consumer-only selection plans no generation at all.
//  3. NO NESTED SESSION. The guard reads committed inputs and spawns no
//     Putnami. The pre-session artifact baseline it used to earn through a
//     workspace-scoped, uncacheable declaration is gone with the render it
//     served (ADR 0034); the workspace-scoped read stays, because the guard
//     still answers about the whole workspace. Its key is `git:**`, the
//     candidate cut the guard reads and nothing else (clientgen ADR 0006).
//
// These assertions read the REAL manifest. A guard that stops being planned,
// loses its barrier, or loses its key would keep passing its own unit tests
// while silently verifying the wrong tree or re-running on every invocation.

const clientgenExtensionName = "@putnami/clientgen"

func loadClientgenExtension(t *testing.T) *extension.ExtensionDescription {
	t.Helper()
	root := findJobsRepoRoot(t)
	path := filepath.Join(root, "tooling", "clientgen-extension", "putnami.extension.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("clientgen manifest unavailable: %v", err)
	}
	manifest, err := extension.LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	description := extension.Resolve(manifest, filepath.Dir(path))
	description.Name = clientgenExtensionName
	return description
}

// clientgenConsumerWorkspace is a workspace whose projects declare OTHER
// extensions. That is the point: the guard must reach a selection that never
// heard of @putnami/clientgen.
func clientgenConsumerWorkspace(t *testing.T, projects int) (*workspace.Workspace, []*workspace.Project) {
	t.Helper()
	root := t.TempDir()
	selected := make([]*workspace.Project, 0, projects)
	for index := range projects {
		name := string(rune('a' + index%26))
		selected = append(selected, &workspace.Project{
			ID: "/" + name, Name: name, Path: name, Type: "library",
			Extensions: []string{"@putnami/go"},
		})
	}
	return &workspace.Workspace{Root: root, Name: "consumers", Projects: selected}, selected
}

func planValidate(t *testing.T, projects int) []*ScheduledJob {
	t.Helper()
	description := loadClientgenExtension(t)
	ws, selected := clientgenConsumerWorkspace(t, projects)
	planned, err := Plan(ws, []string{"validate"}, selected,
		[]*extension.ExtensionDescription{description}, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan validate: %v", err)
	}
	return planned
}

func clientgenNodes(planned []*ScheduledJob) []*ScheduledJob {
	var nodes []*ScheduledJob
	for _, node := range planned {
		if node.JobDef != nil && node.JobDef.ExtensionName == clientgenExtensionName {
			nodes = append(nodes, node)
		}
	}
	return nodes
}

// TestNativeValidatePlansOneClientgenGuardForAnySelection is the activation
// invariant: one guard, whatever the selection contains, and even when no
// selected project lists the extension.
func TestNativeValidatePlansOneClientgenGuardForAnySelection(t *testing.T) {
	command := loadClientgenExtension(t).Jobs["validate"]
	if command == nil {
		t.Fatal("the clientgen manifest contributes no validate command; `putnami validate` runs no guard at all")
	}
	if command.Activation != workspaceOnceActivation {
		t.Fatalf("validate activation = %q, want %q: any other activation makes the guard depend on a project "+
			"listing the extension", command.Activation, workspaceOnceActivation)
	}
	for _, projects := range []int{1, 7, 40} {
		nodes := clientgenNodes(planValidate(t, projects))
		if len(nodes) != 1 {
			names := make([]string, 0, len(nodes))
			for _, node := range nodes {
				names = append(names, node.Project.ID+" "+node.JobDef.Name)
			}
			t.Fatalf("validate over %d project(s) planned %d clientgen guard(s) (%s), want exactly 1",
				projects, len(nodes), strings.Join(names, ", "))
		}
		if got := nodes[0].Step.Task; got != "clientgen-workspace-check" {
			t.Fatalf("guard task = %q, want clientgen-workspace-check", got)
		}
	}
}

// TestNativeValidateGuardWaitsForTheSelectedProvidersGeneration pins the
// barrier and the declaration pair: a selected provider's clientgen tasks are
// planned and the guard depends on their leaves; a consumer-only selection plans
// none and the guard depends on nothing.
func TestNativeValidateGuardWaitsForTheSelectedProvidersGeneration(t *testing.T) {
	description := loadClientgenExtension(t)
	command := description.Jobs["validate"]
	if command == nil || !slices.Contains(command.CommandDependsOn, "!clientgen") {
		t.Fatalf("validate dependsOn = %v, want the !clientgen session barrier: without it a selected provider's "+
			"clients are never regenerated before the guard reads them", command.CommandDependsOn)
	}

	// Consumer-only selection: one guard, no generation, no dependency.
	guard := clientgenNodes(planValidate(t, 3))[0]
	if len(guard.DependsOn) != 0 {
		t.Fatalf("a consumer-only validate gave the guard dependencies %v; nothing should have been planned for it", guard.DependsOn)
	}
	workspaceScoped := false
	for _, ref := range guard.JobDef.Reads {
		if ref.Scope == extension.ResourceScopeWorkspace {
			workspaceScoped = true
		}
	}
	if !workspaceScoped {
		t.Fatalf("guard reads = %+v; the guard answers about the whole workspace and must say so", guard.JobDef.Reads)
	}
	if !guard.JobDef.Cache {
		t.Fatal("the guard is not cacheable; it reads the Git candidate cut its git:** key hashes (clientgen ADR 0006)")
	}

	// A selected provider: its clientgen tasks are planned and the guard waits
	// for them.
	root := t.TempDir()
	provider := &workspace.Project{
		ID: "/provider", Name: "provider", Path: "provider", Type: "library",
		Extensions: []string{clientgenExtensionName},
	}
	consumer := &workspace.Project{
		ID: "/consumer", Name: "consumer", Path: "consumer", Type: "library",
		Extensions: []string{"@putnami/go"},
	}
	ws := &workspace.Workspace{Root: root, Name: "mixed", Projects: []*workspace.Project{provider, consumer}}
	planned, err := Plan(ws, []string{"validate"}, []*workspace.Project{provider, consumer},
		[]*extension.ExtensionDescription{description}, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan validate with a provider: %v", err)
	}
	var generators []string
	guard = nil
	for _, node := range clientgenNodes(planned) {
		switch {
		case node.Step != nil && node.Step.Task == "clientgen-workspace-check":
			guard = node
		case node.CommandName() == "clientgen":
			generators = append(generators, node.Key())
		}
	}
	sort.Strings(generators)
	if guard == nil {
		t.Fatal("no guard planned beside the provider's generation")
	}
	want := []string{"/provider:clientgen~generate-go", "/provider:clientgen~generate-ts"}
	if !slices.Equal(generators, want) {
		t.Fatalf("clientgen tasks planned for the selected provider = %v, want %v", generators, want)
	}
	for _, key := range want {
		if !slices.Contains(guard.DependsOn, key) {
			t.Fatalf("the guard %v does not wait for %s; it would read the client before it is regenerated", guard.DependsOn, key)
		}
	}
	for _, node := range clientgenNodes(planned) {
		if node.Project.ID == "/consumer" && node.CommandName() == "clientgen" {
			t.Fatalf("clientgen was planned for the consumer %s, which declares no provider", node.Key())
		}
	}
}

// TestNativeValidateGuardIsNotPlannedForAnotherCommand keeps the contribution
// scoped: `build` and `lint` must not drag a whole-workspace verification
// behind them.
func TestNativeValidateGuardIsNotPlannedForAnotherCommand(t *testing.T) {
	description := loadClientgenExtension(t)
	ws, selected := clientgenConsumerWorkspace(t, 3)
	for _, command := range []string{"build", "lint", "test"} {
		planned, err := Plan(ws, []string{command}, selected,
			[]*extension.ExtensionDescription{description}, nil, nil, nil)
		if err != nil {
			t.Fatalf("plan %s: %v", command, err)
		}
		if nodes := clientgenNodes(planned); len(nodes) != 0 {
			t.Fatalf("%s planned %d clientgen node(s); the guard belongs to validate only", command, len(nodes))
		}
	}
}

// TestNativeValidateGuardIsDisableable proves the escape hatch is the ordinary
// one — a workspace that disables the extension or the job — rather than an
// undocumented environment variable.
func TestNativeValidateGuardIsDisableable(t *testing.T) {
	description := loadClientgenExtension(t)
	ws, selected := clientgenConsumerWorkspace(t, 3)
	planned, err := Plan(ws, []string{"validate"}, selected,
		[]*extension.ExtensionDescription{description}, nil, nil, []string{clientgenExtensionName})
	if err != nil {
		t.Fatalf("plan validate with the extension disabled: %v", err)
	}
	if nodes := clientgenNodes(planned); len(nodes) != 0 {
		t.Fatalf("a workspace that disables %s still plans %d guard(s)", clientgenExtensionName, len(nodes))
	}
}

// TestProviderGenerationNeverOverlapsAGeneratedTreeRewrite pins the ordering
// guarantee against the two REAL manifests. `package` rewrites the
// provider's .gen through its own generate step, which no functional edge
// orders against `clientgen`. The generators read .gen/clientgen/config.json,
// so each one declares a read of the project `gen` resource, and the planner
// must then sequence it against every writer of that resource: a generator
// that ran while .gen was rewritten read no contract, reported no target, and
// the engine emptied the committed client under a successful task.
func TestProviderGenerationNeverOverlapsAGeneratedTreeRewrite(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "provider", "go.mod",
		"module example.com/provider\n\ngo 1.25\n\nrequire go.putnami.dev/app v0.0.0\n")
	writeProjectFile(t, root, "provider", "provider.go", "package main\n\nfunc main() {}\n")
	writeProjectFile(t, root, "provider", wsproto.ConfigFilename, `{"options":{"package":{"archives":true}}}`)
	provider := &workspace.Project{
		ID: "/provider", Name: "provider", Path: "provider", Type: "application",
		Extensions: []string{"@putnami/go", clientgenExtensionName},
		Config:     wsproto.LoadProjectConfig(filepath.Join(root, "provider")),
	}
	planned, err := Plan(testWorkspace(root, provider), []string{"package", "clientgen", "validate"},
		[]*workspace.Project{provider},
		[]*extension.ExtensionDescription{loadGoPlannerExtension(t), loadClientgenExtension(t)}, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan package,clientgen,validate: %v", err)
	}

	edges := make(map[string][]string, len(planned))
	var writers, generators []string
	for _, job := range planned {
		edges[job.Key()] = append(append([]string(nil), job.DependsOn...), job.SerializeAfter...)
		if job.Project == nil || job.Project.ID != "/provider" {
			continue
		}
		// describe declares only a read of gen, yet it writes the very file the
		// generators read, .gen/clientgen/config.json. It is ordered through the
		// generate step it depends on, so it must be ordered here too.
		if writesProjectGen(job) || (job.Step != nil && job.Step.Task == "build-describe") {
			writers = append(writers, job.Key())
		}
		if job.CommandName() == "clientgen" {
			generators = append(generators, job.Key())
		}
	}
	sort.Strings(writers)
	sort.Strings(generators)
	for _, want := range []string{"/provider:build~generate", "/provider:package~generate",
		"/provider:build~describe", "/provider:package~describe"} {
		if !slices.Contains(writers, want) {
			t.Fatalf(".gen writers planned = %v, want %s among them; the plan no longer reproduces the race "+
				"this test guards", writers, want)
		}
	}
	if want := []string{"/provider:clientgen~generate-go", "/provider:clientgen~generate-ts"}; !slices.Equal(generators, want) {
		t.Fatalf("clientgen tasks = %v, want %v", generators, want)
	}

	reaches := func(from, to string) bool {
		seen := map[string]bool{}
		stack := []string{from}
		for len(stack) > 0 {
			key := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if key == to {
				return true
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			stack = append(stack, edges[key]...)
		}
		return false
	}
	for _, generator := range generators {
		for _, writer := range writers {
			if !reaches(generator, writer) && !reaches(writer, generator) {
				t.Errorf("%s and %s are unordered: the generator can read .gen/clientgen/config.json while the "+
					"writer rewrites .gen", generator, writer)
			}
		}
	}
}
