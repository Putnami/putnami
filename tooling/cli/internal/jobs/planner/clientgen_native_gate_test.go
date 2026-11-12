package planner

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The native validation gate: `putnami validate` must reject new handwritten
// first-party transports and inconsistent committed clients for the WHOLE
// workspace, from any selection.
//
// Three properties make that true, and none is visible from the extension's
// own tests:
//
//  1. ACTIVATION. The guard is contributed as a `validate` command with
//     workspace-once activation. matchWorkspaceOnceJobs never consults a
//     project's extension list, so no project can opt out of the guard by not
//     depending on @putnami/clientgen — and it plans exactly one node however
//     many projects the selection resolves to.
//  2. NO GENERATION. The guard's command depends on nothing, so validate
//     plans no generation for any selection: the guard reads the committed
//     tree, and a generated client is built, not committed (clientgen ADR
//     0007).
//  3. NO NESTED SESSION. The guard reads committed inputs and spawns no
//     Putnami. The pre-session artifact baseline it used to earn through a
//     workspace-scoped, uncacheable declaration is gone with the render it
//     served (ADR 0034); the workspace-scoped read stays, because the guard
//     still answers about the whole workspace. Its key is `git:**`, the
//     candidate cut the guard reads and nothing else (clientgen ADR 0006).
//
// These assertions read the REAL manifest. A guard that stops being planned,
// gains a dependency, or loses its key would keep passing its own unit tests
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

func planValidate(t *testing.T, projects int) []*jobmodel.ScheduledJob {
	t.Helper()
	description := loadClientgenExtension(t)
	ws, selected := clientgenConsumerWorkspace(t, projects)
	planned, err := Plan(ws, []string{"validate"}, selected,
		[]*extension.ExtensionDescription{description}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan validate: %v", err)
	}
	return planned
}

func clientgenNodes(planned []*jobmodel.ScheduledJob) []*jobmodel.ScheduledJob {
	var nodes []*jobmodel.ScheduledJob
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

// TestNativeValidatePlansNoGeneration pins that validate plans the guard and
// nothing else: not for a consumer-only selection, and not for a selection
// that holds a provider with an in-module target and a client project that
// names its provider in generatedFrom.
func TestNativeValidatePlansNoGeneration(t *testing.T) {
	spectest.Proves(t, "tooling/cross-language-rest-clients", "native-validation-gate", "validate-plans-no-generation")
	description := loadClientgenExtension(t)
	command := description.Jobs["validate"]
	if command == nil || len(command.CommandDependsOn) != 0 {
		t.Fatalf("validate = %+v, want a command that depends on nothing", command)
	}

	// Consumer-only selection: one guard, no dependency.
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

	// A provider with an in-module target and a client project: still the
	// guard alone.
	root := t.TempDir()
	writeProjectFile(t, root, "provider", "go.mod", "module example.com/provider\n\ngo 1.25\n")
	writeProjectFile(t, root, "provider", "clientgen.json", `{"targets":["go"],"go":{"output":"client"}}`)
	writeProjectFile(t, root, "client", "go.mod", "module example.com/client\n\ngo 1.25\n")
	writeProjectFile(t, root, "client", wsproto.ConfigFilename, `{"name":"client","generatedFrom":{"service":"items","language":"go"}}`)
	provider := &workspace.Project{
		ID: "/provider", Name: "provider", Path: "provider", Type: "library",
		Extensions: []string{clientgenExtensionName},
	}
	client := &workspace.Project{
		ID: "/client", Name: "client", Path: "client", Type: "library",
		Extensions: []string{"@putnami/go"},
		Config:     wsproto.LoadProjectConfig(filepath.Join(root, "client")),
	}
	ws := &workspace.Workspace{Root: root, Name: "mixed", Projects: []*workspace.Project{provider, client}}
	planned, err := Plan(ws, []string{"validate"}, []*workspace.Project{provider, client},
		[]*extension.ExtensionDescription{description}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan validate with a provider and a client: %v", err)
	}
	nodes := clientgenNodes(planned)
	if len(nodes) != 1 || nodes[0].Step == nil || nodes[0].Step.Task != "clientgen-workspace-check" {
		keys := make([]string, 0, len(nodes))
		for _, node := range nodes {
			keys = append(keys, node.Key())
		}
		t.Fatalf("validate planned %v, want the guard alone", keys)
	}
	if len(nodes[0].DependsOn) != 0 {
		t.Fatalf("the guard depends on %v; validate plans no generation", nodes[0].DependsOn)
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
			[]*extension.ExtensionDescription{description}, nil, nil, nil, nil)
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
		[]*extension.ExtensionDescription{description}, nil, nil, []string{clientgenExtensionName}, nil)
	if err != nil {
		t.Fatalf("plan validate with the extension disabled: %v", err)
	}
	if nodes := clientgenNodes(planned); len(nodes) != 0 {
		t.Fatalf("a workspace that disables %s still plans %d guard(s)", clientgenExtensionName, len(nodes))
	}
}

// TestProviderGenerationNeverOverlapsATestOfItsClient pins the order between an
// in-module generator and the test steps that compile its client: the
// provider's own and a consumer's. The generator writes the client inside the
// provider's tree. The provider's test waits for it because the Go test
// command depends on client-generate. The consumer waits for it because the
// generator declares its client readByDependents. The provider's test generate
// adopts its build generate, and the build's infra step, which declares the
// project's gen write, waits for the provider's test steps that read .gen.
func TestProviderGenerationNeverOverlapsATestOfItsClient(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "provider", "go.mod",
		"module example.com/provider\n\ngo 1.25\n\nrequire go.putnami.dev/app v0.0.0\n")
	writeProjectFile(t, root, "provider", "provider.go", "package main\n\nfunc main() {}\n")
	writeProjectFile(t, root, "provider", wsproto.ConfigFilename, `{}`)
	writeProjectFile(t, root, "provider", "clientgen.json", `{"targets":["go"],"go":{"output":"client"}}`)
	writeProjectFile(t, root, "consumer", "go.mod", "module example.com/consumer\n\ngo 1.25\n")
	writeProjectFile(t, root, "consumer", "consumer.go", "package consumer\n")
	writeProjectFile(t, root, "consumer", wsproto.ConfigFilename, `{}`)
	provider := &workspace.Project{
		ID: "/provider", Name: "provider", Path: "provider", Type: "application",
		Extensions: []string{"@putnami/go"},
		Config:     wsproto.LoadProjectConfig(filepath.Join(root, "provider")),
	}
	consumer := &workspace.Project{
		ID: "/consumer", Name: "consumer", Path: "consumer", Type: "library",
		Extensions:   []string{"@putnami/go"},
		Dependencies: []string{"provider"},
		Config:       wsproto.LoadProjectConfig(filepath.Join(root, "consumer")),
	}
	planned, err := Plan(testWorkspace(root, provider, consumer), []string{"lint", "test", "build"},
		[]*workspace.Project{provider, consumer},
		[]*extension.ExtensionDescription{loadGoPlannerExtension(t), loadClientgenExtension(t)}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("plan lint,test,build: %v", err)
	}
	byKey := JobsByPlanKey(planned)
	requireShared(t, byKey, "/provider:build~generate", "/provider:test~generate")
	ordered := func(a, b *jobmodel.ScheduledJob) bool {
		return planAncestors(byKey, a)[b.Key()] || planAncestors(byKey, b)[a.Key()]
	}
	var providerReaders []*jobmodel.ScheduledJob
	for _, job := range planned {
		if job.Project.ID == provider.ID && job.CommandName() == "test" && slices.ContainsFunc(job.JobDef.Reads, func(ref extension.ResourceRef) bool {
			return ref.ID == cachekey.GenResourceID
		}) {
			providerReaders = append(providerReaders, job)
		}
	}
	if len(providerReaders) == 0 {
		t.Fatal("control: no test step of /provider reads .gen")
	}
	consumerTest := requirePlanKey(t, byKey, "/consumer:test~test")
	for _, key := range []string{"/provider:client-generate~module"} {
		generator := requirePlanKey(t, byKey, key)
		for _, reader := range append(append([]*jobmodel.ScheduledJob(nil), providerReaders...), consumerTest) {
			if !ordered(generator, reader) {
				t.Errorf("%s and %s are unordered: the test can compile the client while the generator rewrites it",
					generator.Key(), reader.Key())
			}
		}
	}
	infra := requirePlanKey(t, byKey, "/provider:build~infra")
	for _, reader := range providerReaders {
		if !ordered(infra, reader) {
			t.Errorf("%s and %s are unordered: the infra step can write .gen while the test reads it", infra.Key(), reader.Key())
		}
	}
}
