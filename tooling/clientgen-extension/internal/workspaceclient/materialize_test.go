package workspaceclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
)

func TestMaterializeWorkspaceContractsUsesSpawningPutnamiForAllProjects(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "workspace-one-pass", "sync-and-adopt-materialize-all-provider-contracts-themselves")
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable script")
	}
	root := t.TempDir()
	capture := filepath.Join(root, "putnami-call")
	executable := filepath.Join(root, "putnami")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CLIENTGEN_CAPTURE\"\n"
	if err := os.WriteFile(executable, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(registry.CLIExecutableEnv, executable)
	t.Setenv("CLIENTGEN_CAPTURE", capture)
	if err := MaterializeWorkspaceContracts(root); err != nil {
		t.Fatal(err)
	}
	if err := CompileWorkspaceConsumers(root); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture workspace has no readable project index, so materialization
	// cannot narrow to a provider and falls back to every project. A narrowed
	// selection derived from an unreadable index would match nothing and report
	// a clean workspace because it looked at nothing.
	if got := strings.TrimSpace(string(data)); got != "build --projects * --no-cache\nbuild --projects * --no-cache" {
		t.Fatalf("Putnami invocation = %q", got)
	}
}

// TestAWorkspaceWithNoProviderMaterializesNothing keeps the cost off a
// workspace that installs this extension and declares no client. The guard runs
// in every `putnami validate` there too, and a whole-workspace build would be
// charged to a workspace with nothing to verify.
func TestAWorkspaceWithNoProviderMaterializesNothing(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "workspace-one-pass", "a-workspace-that-declares-no-provider-builds-nothing")
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json",
		`{"version":4,"projects":[{"path":"services/portal"}]}`)
	writeWorkspaceFile(t, root, "services/portal/putnami.json", `{"name":"portal","extensions":["@putnami/typescript"]}`)

	if arguments := WorkspaceBuildArgs(root); arguments != nil {
		t.Fatalf("materialization invocation = %v, want none", arguments)
	}
	// It must also not need a spawning CLI it will never use.
	t.Setenv(registry.CLIExecutableEnv, "")
	if err := MaterializeWorkspaceContracts(root); err != nil {
		t.Fatalf("materialize with no provider: %v", err)
	}
}

// TestTheProviderSelectionReadsTheDeclarationNotTheGeneratedConfig pins the
// input that keeps a cold tree honest: a project that declares the extension is
// built even when the configuration this build produces does not exist yet.
func TestTheProviderSelectionReadsTheDeclarationNotTheGeneratedConfig(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json",
		`{"version":4,"projects":[{"path":"services/catalog"},{"path":"services/portal"}]}`)
	writeWorkspaceFile(t, root, "services/catalog/putnami.json",
		`{"name":"catalog","extensions":["/tooling/clientgen-extension"]}`)
	writeWorkspaceFile(t, root, "services/portal/putnami.json", `{"name":"portal","extensions":["@putnami/clientgen"]}`)

	providers, err := ClientgenProviderProjects(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(providers, []string{"/services/catalog", "/services/portal"}) {
		t.Fatalf("providers = %v, want both declaration spellings", providers)
	}
	if got := WorkspaceBuildArgs(root); !reflect.DeepEqual(got,
		[]string{
			"build",
			"--projects", "/services/catalog,/services/portal",
			"--no-cache-projects", "/services/catalog,/services/portal",
		}) {
		t.Fatalf("materialization invocation = %v", got)
	}
}

// TestTheCacheBypassNamesTheProvidersAndNotTheRun pins the shape of the
// materialization's cache refusal: the providers
// are re-derived from source, their dependency closure — planned only because
// `--projects` includes it — keeps the cache.
//
// It asserts the flag PAIR rather than the string, because the two halves have
// to name the same set: a `--no-cache-projects` that named a superset would
// recompile the closure again, and one that named a subset would let a provider
// be served a cached contract the check then verifies against itself.
func TestTheCacheBypassNamesTheProvidersAndNotTheRun(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "workspace-one-pass",
		"the-provider-build-refuses-the-cache-for-the-providers-alone")
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json",
		`{"version":4,"projects":[{"path":"services/catalog"},{"path":"libs/shared"}]}`)
	writeWorkspaceFile(t, root, "services/catalog/putnami.json",
		`{"name":"catalog","extensions":["@putnami/clientgen"]}`)
	writeWorkspaceFile(t, root, "libs/shared/putnami.json",
		`{"name":"shared","extensions":["/go/extension"]}`)

	arguments := WorkspaceBuildArgs(root)
	flags := map[string]string{}
	for i := 0; i+1 < len(arguments); i++ {
		if strings.HasPrefix(arguments[i], "--") {
			flags[arguments[i]] = arguments[i+1]
		}
	}
	for _, argument := range arguments {
		if argument == "--no-cache" {
			t.Fatalf("the materialization still refuses the cache run-wide: %v; "+
				"the provider's dependency closure is planned too and has no drift to detect", arguments)
		}
	}
	if flags["--projects"] != "/services/catalog" {
		t.Fatalf("selection = %q, want the declaring project alone", flags["--projects"])
	}
	if flags["--no-cache-projects"] != flags["--projects"] {
		t.Fatalf("cache bypass %q does not name the selection %q; the two must be the same set",
			flags["--no-cache-projects"], flags["--projects"])
	}
}

func TestMaterializeWorkspaceContractsRejectsUnadvertisedExecutable(t *testing.T) {
	t.Setenv(registry.CLIExecutableEnv, "")
	if err := MaterializeWorkspaceContracts(t.TempDir()); err == nil || !strings.Contains(err.Error(), registry.CLIExecutableEnv) {
		t.Fatalf("unadvertised CLI error = %v", err)
	}
}

// TestTheCheckSpawnsNoPutnami pins the check's behavior from the extension's
// side: the check reads committed inputs and never asks the spawning CLI for a
// build. A workspace with a real provider, a committed contract sidecar and a
// committed manifest is inspected while the advertised CLI records every call
// it receives — and receives none.
func TestTheCheckSpawnsNoPutnami(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "workspace-one-pass", "the-check-builds-nothing-and-renders-nothing")
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable script")
	}
	_, expected := analyzerFixture(t)
	root := t.TempDir()
	writeWorkspaceFile(t, root, ".putnami/workspace-index.json", `{"version":4,"projects":[{"path":"services/catalog"}]}`)
	writeWorkspaceFile(t, root, "services/catalog/putnami.json", `{"name":"catalog","extensions":["@putnami/clientgen"]}`)
	writeWorkspaceFile(t, root, "services/catalog/schema/openapi.json", markedOpenAPI("catalog"))
	for path, content := range expected {
		writeWorkspaceFile(t, root, path, string(content))
	}
	capture := filepath.Join(root, "putnami-call")
	executable := filepath.Join(root, "putnami")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CLIENTGEN_CAPTURE\"\n"
	if err := os.WriteFile(executable, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(registry.CLIExecutableEnv, executable)
	t.Setenv("CLIENTGEN_CAPTURE", capture)

	report := InspectCommitted(root, fixtureMembers(t, root))
	if len(report.Providers) != 1 {
		t.Fatalf("the committed provider was not discovered: %+v", report.Providers)
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		data, _ := os.ReadFile(capture)
		t.Fatalf("the check spawned the CLI: %q", string(data))
	}
	if _, ok := report.Timings[PhaseMaterialize]; ok {
		t.Fatal("the check reports a materialize phase it must not have")
	}
	if _, ok := report.Timings[PhaseRender]; ok {
		t.Fatal("the check reports a render phase it must not have")
	}
}

// TestWorkspaceBuildPlansEveryRealProviderContract replaces a fixture that
// copied a file and called it a contract. The invariant is about SELECTION, and
// selection is only observable in the plan: this asserts the exact invocation
// MaterializeWorkspaceContracts makes, against the real repository, plans a
// build for every project that declares this extension — including the ones the
// workspace excludes by default through a disabled tag, which is where a
// consumer-only impacted run would otherwise lose its provider.
//
// The plan is also where a regeneration loop would first be visible: the build
// sync spawns must contain no clientgen task, or the sync would regenerate
// through the build it spawned rather than through its own generators.
func TestWorkspaceBuildPlansEveryRealProviderContract(t *testing.T) {
	spectest.Proves(t, clientgenFeature, "workspace-one-pass", "the-explicit-selection-plans-a-build-for-every-provider")
	spectest.Proves(t, clientgenFeature, "workspace-one-pass", "an-explicit-selection-includes-default-excluded-provider-projects")
	executable := strings.TrimSpace(os.Getenv(registry.CLIExecutableEnv))
	if executable == "" {
		t.Skipf("%s is unset: this assertion needs the Putnami that launched the suite", registry.CLIExecutableEnv)
	}
	root := repositoryRootForTest(t)
	providers, excludedTags := clientgenProviderProjects(t, root)
	if len(providers) == 0 {
		t.Fatal("no repository project declares @putnami/clientgen; the materialization invariant has nothing to prove")
	}
	if len(excludedTags) == 0 {
		t.Fatal("no clientgen provider carries a workspace-excluded tag; the '*' selection claim is untested")
	}

	arguments := append(WorkspaceBuildArgs(root), "--plan", "--output=json")
	command := exec.Command(executable, arguments...) //nolint:gosec // the exact spawning Putnami advertised by the CLI
	command.Dir = root
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		// BOTH streams. This assertion failed intermittently on CI reporting
		// nothing but "exit status 2", because it quoted stderr alone and a
		// child that refuses a selection or plans nothing wrote its reason to
		// neither stream. The CLI now names that cause on stderr; the
		// stdout envelope is reported too, so a failure whose cause DOES travel
		// in the document is readable from the same log line.
		t.Fatalf("plan %v: %v\nstdout:\n%s\nstderr:\n%s",
			arguments, err, strings.TrimSpace(string(output)), strings.TrimSpace(stderr.String()))
	}
	var result struct {
		Plan struct {
			Tasks []struct {
				Identity struct {
					Project struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"project"`
					Task struct {
						Name    string `json:"name"`
						Command string `json:"command"`
					} `json:"task"`
				} `json:"identity"`
			} `json:"tasks"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode plan document: %v", err)
	}

	built := map[string]bool{}
	for _, task := range result.Plan.Tasks {
		if task.Identity.Task.Command == "build" {
			built["/"+strings.TrimPrefix(task.Identity.Project.ID, "/")] = true
		}
		if strings.HasPrefix(task.Identity.Task.Command, "clientgen") {
			t.Errorf("the provider build plans %q; sync regenerates through its own generators, not through the build it spawns",
				task.Identity.Task.Name)
		}
	}
	for _, project := range providers {
		if !built[project] {
			t.Errorf("the explicit selection plans no build for the clientgen provider %q; sync would regenerate "+
				"against that provider's stale contract", project)
		}
	}
}

// clientgenProviderProjects reads the repository itself for the projects that
// declare this extension, and reports which of them carry a tag the workspace
// disables by default. Reading rather than listing keeps the assertion true
// when a provider is added or a tag changes.
func clientgenProviderProjects(t *testing.T, root string) (providers []string, defaultExcluded []string) {
	t.Helper()
	workspaceData, err := os.ReadFile(filepath.Join(root, "putnami.workspace.json")) //nolint:gosec // repository root document
	if err != nil {
		t.Fatal(err)
	}
	var workspace struct {
		Disable struct {
			Tags []string `json:"tags"`
		} `json:"disable"`
	}
	if err := json.Unmarshal(workspaceData, &workspace); err != nil {
		t.Fatal(err)
	}
	disabled := map[string]bool{}
	for _, tag := range workspace.Disable.Tags {
		disabled[tag] = true
	}

	walkErr := filepath.WalkDir(root, func(full string, entry fs.DirEntry, walkErr error) error {
		// A directory a concurrent task removes during the walk holds no
		// project document.
		if walkErr != nil && full != root && errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", ".git", ".putnami", "out", "dist", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "putnami.json" {
			return nil
		}
		data, readErr := os.ReadFile(full) //nolint:gosec // repository project document
		if readErr != nil {
			return readErr
		}
		var project struct {
			Extensions []string `json:"extensions"`
			Tags       []string `json:"tags"`
		}
		if json.Unmarshal(data, &project) != nil {
			return nil
		}
		declares := false
		for _, extension := range project.Extensions {
			declares = declares || extension == "/tooling/clientgen-extension"
		}
		if !declares {
			return nil
		}
		rel, relErr := filepath.Rel(root, filepath.Dir(full))
		if relErr != nil {
			return relErr
		}
		id := "/" + filepath.ToSlash(rel)
		providers = append(providers, id)
		for _, tag := range project.Tags {
			if disabled[tag] {
				defaultExcluded = append(defaultExcluded, id)
				break
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repository for clientgen providers: %v", walkErr)
	}
	sort.Strings(providers)
	sort.Strings(defaultExcluded)
	return providers, defaultExcluded
}

func repositoryRootForTest(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "putnami.workspace.json")); err != nil {
		t.Fatalf("resolve repository root from %s: %v", root, err)
	}
	return root
}
