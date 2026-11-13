package workspace

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
)

// A delegated build tool (Pants, Bazel) states edges between targets, and its
// sync makes one project per directory. A package and its subpackages import
// each other, so the directory graph closes cycles the target graph does not.
// The tool orders its own targets, so a cycle whose every project only runs
// the tool's tasks loads; a cycle that holds a task Putnami orders itself
// does not.

// delegatedExtensionManifest is a delegated stack: its workspace adapter
// declares buildGraphEdges, so every task it runs is the build tool's.
const delegatedExtensionManifest = `{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "name": "@fixture/delegated",
  "version": "1.0.0",
  "cliContract": 21,
  "commands": {"build": {"run": [{"id": "build", "task": "build-exec"}]}},
  "tasks": {"build-exec": {"kind": "command", "command": "echo", "args": ["built"]}},
  "workspace": {"markers": ["BUILD"], "inputs": ["BUILD"], "dependencySources": true, "buildGraphEdges": true}
}`

// orderedExtensionManifest runs tasks Putnami orders itself: no build tool
// orders them inside a cycle.
const orderedExtensionManifest = `{
  "$schema": "https://putnami.dev/schemas/putnami-extension.json",
  "name": "@fixture/ordered",
  "version": "1.0.0",
  "cliContract": 21,
  "commands": {"build": {"run": [{"id": "build", "task": "build-exec"}]}},
  "tasks": {"build-exec": {"kind": "command", "command": "echo", "args": ["built"]}}
}`

// cyclePackage is one directory of a fixture: its marker file and the
// putnami.json a graph sync writes for it.
type cyclePackage struct {
	dir        string
	marker     string
	extensions []string
	deps       []string
}

// cycleWorkspaceConfig is the putnami.workspace.json of a fixture.
type cycleWorkspaceConfig struct {
	Includes   []string `json:"includes"`
	Extensions []string `json:"extensions"`
}

// cycleProjectConfig is the putnami.json a graph sync writes for a package.
type cycleProjectConfig struct {
	Extensions   []string `json:"extensions"`
	Dependencies []string `json:"dependencies"`
}

// cycleFixture writes a workspace holding the two fixture extensions and the
// packages, and returns its root.
func cycleFixture(t *testing.T, rootMarker string, packages []cyclePackage) string {
	t.Helper()
	root := t.TempDir()
	includes := []string{"tools/delegated", "tools/ordered"}
	for _, p := range packages {
		includes = append(includes, p.dir)
	}
	config, err := json.Marshal(cycleWorkspaceConfig{
		Includes:   includes,
		Extensions: []string{"/tools/delegated", "/tools/ordered"},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeFileAt(t, filepath.Join(root, "putnami.workspace.json"), string(config))
	writeFileAt(t, filepath.Join(root, rootMarker), "")
	writeFileAt(t, filepath.Join(root, "tools", "delegated", "putnami.extension.json"), delegatedExtensionManifest)
	writeFileAt(t, filepath.Join(root, "tools", "ordered", "putnami.extension.json"), orderedExtensionManifest)
	for _, p := range packages {
		project, err := json.Marshal(cycleProjectConfig{Extensions: p.extensions, Dependencies: p.deps})
		if err != nil {
			t.Fatal(err)
		}
		writeFileAt(t, filepath.Join(root, filepath.FromSlash(p.dir), p.marker), "")
		writeFileAt(t, filepath.Join(root, filepath.FromSlash(p.dir), "putnami.json"), string(project))
	}
	InvalidateLoadCache(root)
	t.Cleanup(func() { InvalidateLoadCache(root) })
	return root
}

// pantsCyclePackages is st2's longest directory cycle: eight packages of one
// Python package tree, each importing the next, the last importing the first.
func pantsCyclePackages(extension string) []cyclePackage {
	dirs := []string{
		"st2common/st2common",
		"st2common/st2common/constants",
		"st2common/st2common/util",
		"st2common/st2common/models",
		"st2common/st2common/models/db",
		"st2common/st2common/persistence",
		"st2common/st2common/services",
		"st2common/st2common/runners",
	}
	packages := make([]cyclePackage, len(dirs))
	for i, dir := range dirs {
		next := dirs[(i+1)%len(dirs)]
		packages[i] = cyclePackage{dir: dir, marker: "BUILD", extensions: []string{extension}, deps: []string{"/" + next}}
	}
	return packages
}

// bazelCyclePackages is buildbuddy's cycle: a proto package and its API
// subpackage depend on each other, with a consumer outside the cycle.
func bazelCyclePackages(extension string) []cyclePackage {
	return []cyclePackage{
		{dir: "proto", marker: "BUILD.bazel", extensions: []string{extension}, deps: []string{"/proto/api/v1"}},
		{dir: "proto/api/v1", marker: "BUILD.bazel", extensions: []string{extension}, deps: []string{"/proto"}},
		{dir: "server", marker: "BUILD.bazel", extensions: []string{extension}, deps: []string{"/proto/api/v1"}},
	}
}

func TestLoad_ACycleAmongDelegatedProjectsLoads(t *testing.T) {
	for _, tc := range []struct {
		name     string
		marker   string
		packages []cyclePackage
		cycle    []string
	}{
		{name: "pants", marker: "pants.toml", packages: pantsCyclePackages("@fixture/delegated"), cycle: []string{
			"/st2common/st2common", "/st2common/st2common/constants", "/st2common/st2common/models",
			"/st2common/st2common/models/db", "/st2common/st2common/persistence", "/st2common/st2common/runners",
			"/st2common/st2common/services", "/st2common/st2common/util",
		}},
		{name: "bazel", marker: "MODULE.bazel", packages: bazelCyclePackages("@fixture/delegated"), cycle: []string{
			"/proto", "/proto/api/v1",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := cycleFixture(t, tc.marker, tc.packages)
			ws, err := Load(root)
			if err != nil {
				t.Fatalf("a cycle among projects whose tasks a build tool orders refused the load: %v", err)
			}
			for _, id := range tc.cycle {
				if got := ws.Graph.ComponentOf(id); !slices.Equal(got, tc.cycle) {
					t.Errorf("component of %s = %v, want %v", id, got, tc.cycle)
				}
			}
		})
	}
}

func TestLoad_ACycleWithATaskPutnamiOrdersStillFails(t *testing.T) {
	packages := bazelCyclePackages("@fixture/delegated")
	packages[1].extensions = []string{"@fixture/delegated", "@fixture/ordered"}
	root := cycleFixture(t, "MODULE.bazel", packages)

	_, err := Load(root)
	if err == nil {
		t.Fatal("a cycle holding a project with a task Putnami orders loaded")
	}
	for _, want := range []string{"dependency cycle detected", "/proto → /proto/api/v1 → /proto", "/proto/api/v1", "@fixture/ordered"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestLoad_ACycleAmongProjectsThatNameNoExtensionStillFails(t *testing.T) {
	packages := bazelCyclePackages("@fixture/delegated")
	packages[0].extensions = nil
	root := cycleFixture(t, "MODULE.bazel", packages)

	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), "dependency cycle detected") {
		t.Fatalf("err = %v, want the cycle refused: a project that names no extension runs no delegated task", err)
	}
}

// The sync records the tool's edges in the workspace index. A recorded view
// that closes a delegated cycle is adopted, so `projects sync` and every later
// load keep the tool's edges instead of degrading to the authored config.
func TestLoad_ARecordedViewWithADelegatedCycleIsAdopted(t *testing.T) {
	packages := bazelCyclePackages("@fixture/delegated")
	for i := range packages {
		packages[i].deps = nil
	}
	root := cycleFixture(t, "MODULE.bazel", packages)
	ws, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	edges := func(path string, deps ...string) wsproto.ProbeProject {
		sources := make(map[string]wsproto.DependencySource, len(deps))
		for _, dep := range deps {
			sources[dep] = wsproto.DependencySourceBuildGraph
		}
		return wsproto.ProbeProject{Path: path, Extensions: []string{"@fixture/delegated"}, Dependencies: deps, DependencySources: sources}
	}
	recordProviderView(t, ws, probeAnswer("@fixture/delegated",
		edges("proto", "proto/api/v1"),
		edges("proto/api/v1", "proto"),
		edges("server", "proto/api/v1")))

	reloaded, err := Load(root)
	if err != nil {
		t.Fatalf("load with the recorded view: %v", err)
	}
	if !reloaded.HasProbeView() || reloaded.HasWarningCode(WarningCodeProviderViewUnavailable) {
		t.Fatalf("the recorded delegated cycle was not adopted: %v", reloaded.WarningMessages())
	}
	if got, want := reloaded.Graph.ComponentOf("/proto"), []string{"/proto", "/proto/api/v1"}; !slices.Equal(got, want) {
		t.Errorf("component of /proto = %v, want %v", got, want)
	}
}
