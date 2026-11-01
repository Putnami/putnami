package nx

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/workspace"
)

const feature = "tooling/observe-nx"

// demo is the layout of the Nx demo repository: an npm workspace whose two
// projects Nx infers from their package.json, with the test outputs in the
// nx.json target defaults.
var demo = map[string]string{
	"nx.json": `{
  "neverConnectToCloud": true,
  "targetDefaults": {
    "build": {"dependsOn": ["^build"], "outputs": ["{projectRoot}/dist"], "cache": true},
    "test": {"dependsOn": ["^build"], "outputs": ["{projectRoot}/reports"], "cache": true}
  }
}`,
	"package.json": `{
  "name": "demo-observe-nx",
  "private": true,
  "workspaces": ["apps/*", "packages/*"],
  "scripts": {"build": "nx run-many -t build", "test": "nx run-many -t test"},
  "devDependencies": {"jest": "^30.0.0", "nx": "^21.0.0"}
}`,
	"package-lock.json": `{"lockfileVersion": 3}`,
	"apps/api/package.json": `{
  "name": "@demo/api",
  "version": "1.0.0",
  "scripts": {"build": "tsc -p tsconfig.json", "test": "jest", "start": "node dist/server.js"},
  "dependencies": {"@demo/pricing": "1.0.0"}
}`,
	"apps/api/src/quote.ts":   "export const quote = 1;\n",
	"apps/api/jest.config.js": "module.exports = {};\n",
	"packages/pricing/package.json": `{
  "name": "@demo/pricing",
  "version": "1.0.0",
  "scripts": {"build": "tsc -p tsconfig.json", "test": "jest"},
  "dependencies": {"decimal.js": "10.4.3"}
}`,
	"packages/pricing/src/index.ts":           "export const price = 1;\n",
	"packages/pricing/src/index.spec.ts":      "test('price', () => {});\n",
	"node_modules/@demo/pricing/package.json": `{"name": "@demo/pricing"}`,
}

// demoGraph is what `nx graph --file=graph.json` writes for the demo.
const demoGraph = `{
  "graph": {
    "nodes": {
      "@demo/api": {
        "name": "@demo/api",
        "type": "lib",
        "data": {
          "root": "apps/api",
          "name": "@demo/api",
          "sourceRoot": "apps/api",
          "projectType": "library",
          "tags": ["npm:private"],
          "implicitDependencies": [],
          "metadata": {"js": {"packageName": "@demo/api", "isInPackageManagerWorkspaces": true}},
          "targets": {
            "build": {"executor": "nx:run-script", "options": {"script": "build"}, "outputs": ["{projectRoot}/dist"], "dependsOn": ["^build"], "cache": true},
            "test": {"executor": "nx:run-script", "options": {"script": "test"}, "outputs": ["{projectRoot}/reports"], "dependsOn": ["^build"], "cache": true},
            "start": {"executor": "nx:run-script", "options": {"script": "start"}}
          }
        }
      },
      "@demo/pricing": {
        "name": "@demo/pricing",
        "type": "lib",
        "data": {
          "root": "packages/pricing",
          "name": "@demo/pricing",
          "sourceRoot": "packages/pricing",
          "projectType": "library",
          "tags": ["npm:public"],
          "targets": {
            "build": {"executor": "nx:run-script", "options": {"script": "build"}, "outputs": ["{projectRoot}/dist"]},
            "test": {"executor": "nx:run-script", "options": {"script": "test"}, "outputs": ["{projectRoot}/reports"]}
          }
        }
      }
    },
    "dependencies": {
      "@demo/api": [{"source": "@demo/api", "target": "@demo/pricing", "type": "static"}],
      "@demo/pricing": [{"source": "@demo/pricing", "target": "npm:decimal.js", "type": "static"}]
    }
  }
}`

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func with(base map[string]string, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func probe(t *testing.T, root string, opts Options) workspace.ProbeResult {
	t.Helper()
	return Probe(root, workspace.ProbeRequest{Version: workspace.ProbeProtocolVersion, Extension: Extension}, opts)
}

// project returns the probe project at path and its metadata.
func project(t *testing.T, r workspace.ProbeResult, path string) (workspace.ProbeProject, Metadata) {
	t.Helper()
	for _, p := range r.Projects {
		if p.Path == path {
			m, ok := ReadMetadata(p)
			if !ok {
				t.Fatalf("project %s carries no Nx metadata: %s", path, p.Metadata)
			}
			return p, m
		}
	}
	t.Fatalf("no project at %s in %v", path, paths(r))
	return workspace.ProbeProject{}, Metadata{}
}

func paths(r workspace.ProbeResult) []string {
	out := make([]string, 0, len(r.Projects))
	for _, p := range r.Projects {
		out = append(out, p.Path)
	}
	return out
}

func edges(r workspace.ProbeResult) map[string][]string {
	out := map[string][]string{}
	for _, p := range r.Projects {
		out[p.Path] = p.Dependencies
	}
	return out
}

func codes(diags []diag.Diagnostic) []string {
	out := make([]string, 0, len(diags))
	for _, d := range diags {
		out = append(out, d.Code)
	}
	return out
}

func TestAGraphExportGivesTheProjectsAndTheirEdges(t *testing.T) {
	spectest.Proves(t, feature, "graph-export", "a-graph-export-gives-the-projects-and-their-edges")
	t.Run("the demo", func(t *testing.T) {
		r := probe(t, tree(t, with(demo, map[string]string{"graph.json": demoGraph})), Options{})
		if len(r.Diagnostics) != 0 {
			t.Fatalf("diagnostics = %v", r.Diagnostics)
		}
		want := map[string][]string{"apps/api": {"packages/pricing"}, "packages/pricing": nil}
		if got := edges(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("edges = %v, want %v", got, want)
		}
		api, m := project(t, r, "apps/api")
		if api.SourceName != "@demo/api" || api.SourceFile != "apps/api/package.json" || api.Type != "library" {
			t.Fatalf("api = %+v", api)
		}
		if m.Source != evidence.SourceOrchestrator || m.Name != "@demo/api" {
			t.Fatalf("metadata = %+v", m)
		}
		if !reflect.DeepEqual(r.WatchedFiles, []string{"graph.json", "nx.json"}) {
			t.Fatalf("watched = %v", r.WatchedFiles)
		}
	})
	t.Run("edges only the export knows, a root project and a named file", func(t *testing.T) {
		graph := `{"graph": {
  "nodes": {
    "shell": {"name": "shell", "type": "app", "data": {"root": "apps/shell", "targets": {}}},
    "ui": {"name": "ui", "type": "lib", "data": {"root": "libs/ui"}},
    "e2e": {"name": "e2e", "type": "e2e", "data": {"root": "apps/e2e"}},
    "repo": {"name": "repo", "data": {"root": "."}},
    "outside": {"name": "outside", "data": {"root": "../elsewhere"}}
  },
  "dependencies": {
    "shell": [
      {"source": "shell", "target": "ui", "type": "dynamic"},
      {"source": "shell", "target": "shell", "type": "static"},
      {"source": "shell", "target": "npm:react", "type": "static"}
    ],
    "e2e": [{"source": "e2e", "target": "shell", "type": "implicit"}],
    "repo": []
  }
}}`
		root := tree(t, map[string]string{"apps/shell/project.json": `{"name": "shell"}`, "ci/nx-graph.json": graph})
		r := probe(t, root, Options{GraphFile: "ci/nx-graph.json"})
		want := map[string][]string{".": nil, "apps/e2e": {"apps/shell"}, "apps/shell": {"libs/ui"}, "libs/ui": nil}
		if got := edges(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("edges = %v, want %v", got, want)
		}
		if got := codes(r.Diagnostics); !reflect.DeepEqual(got, []string{CodeGraphUnreadable}) {
			t.Fatalf("diagnostics = %v", got)
		}
		shell, _ := project(t, r, "apps/shell")
		e2e, _ := project(t, r, "apps/e2e")
		if shell.SourceFile != "apps/shell/project.json" || shell.Type != "application" || e2e.Type != "application" {
			t.Fatalf("shell = %+v, e2e = %+v", shell, e2e)
		}
		if !reflect.DeepEqual(r.WatchedFiles, []string{"ci/nx-graph.json"}) {
			t.Fatalf("watched = %v", r.WatchedFiles)
		}
	})
}

func TestAMalformedGraphExportFallsBackToTheConfiguration(t *testing.T) {
	spectest.Proves(t, feature, "graph-export", "a-malformed-graph-export-falls-back-to-the-configuration")
	for name, content := range map[string]string{
		"not JSON":       "{",
		"not a graph":    `{"nodes": {}}`,
		"a graph object": `{"graph": {}}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := probe(t, tree(t, with(demo, map[string]string{"graph.json": content})), Options{})
			if got := codes(r.Diagnostics); !reflect.DeepEqual(got, []string{CodeGraphUnreadable}) {
				t.Fatalf("diagnostics = %v", got)
			}
			if got := edges(r); !reflect.DeepEqual(got, map[string][]string{"apps/api": {"packages/pricing"}, "packages/pricing": nil}) {
				t.Fatalf("edges = %v", got)
			}
			_, m := project(t, r, "packages/pricing")
			if m.Source != evidence.SourceWorkspace {
				t.Fatalf("source = %s", m.Source)
			}
		})
	}
	t.Run("an unrelated graph.json outside Nx is no finding", func(t *testing.T) {
		for _, content := range []string{`{"vertices": []}`, `{"graph": {"nodes": {"a": {}, "b": {}}}}`} {
			r := probe(t, tree(t, map[string]string{"graph.json": content}), Options{})
			if len(r.Diagnostics) != 0 || len(r.Projects) != 0 {
				t.Fatalf("result = %+v", r)
			}
		}
	})
	t.Run("an export without project roots", func(t *testing.T) {
		r := probe(t, tree(t, with(demo, map[string]string{"graph.json": `{"graph": {"nodes": {"a": {}, "b": {}}}}`})), Options{})
		if got := codes(r.Diagnostics); !reflect.DeepEqual(got, []string{CodeGraphUnreadable}) {
			t.Fatalf("diagnostics = %v", got)
		}
		if len(r.Projects) != 2 {
			t.Fatalf("projects = %v", paths(r))
		}
	})
	t.Run("a node without a root and a shared root are skipped", func(t *testing.T) {
		graph := `{"graph": {"nodes": {
  "repo": {"name": "repo", "data": {"root": "."}},
  "nameless": {"name": "nameless", "data": {}},
  "a": {"name": "a", "data": {"root": "libs/a"}},
  "b": {"name": "b", "data": {"root": "libs/a"}}
}, "dependencies": {}}}`
		var stdin, stdout bytes.Buffer
		if err := workspace.EncodeProbeRequest(&stdin, workspace.ProbeRequest{Extension: Extension}); err != nil {
			t.Fatal(err)
		}
		root := tree(t, map[string]string{"graph.json": graph})
		if _, err := workspace.ServeProbe(workspace.ProbeControlArgs(), &stdin, &stdout, Answer(root, Options{})); err != nil {
			t.Fatal(err)
		}
		r := probe(t, root, Options{})
		if got := paths(r); !reflect.DeepEqual(got, []string{".", "libs/a"}) {
			t.Fatalf("paths = %v", got)
		}
		a, _ := project(t, r, "libs/a")
		if a.SourceName != "a" {
			t.Fatalf("libs/a = %+v", a)
		}
		if got := codes(r.Diagnostics); !reflect.DeepEqual(got, []string{CodeGraphUnreadable, CodeGraphUnreadable}) {
			t.Fatalf("diagnostics = %v", got)
		}
		for i := 0; i < 10; i++ {
			if again := probe(t, root, Options{}); !reflect.DeepEqual(again.Diagnostics, r.Diagnostics) {
				t.Fatalf("diagnostics move between probes: %v, then %v", r.Diagnostics, again.Diagnostics)
			}
		}
	})
	t.Run("a graph export that is a directory", func(t *testing.T) {
		r := probe(t, tree(t, with(demo, map[string]string{"graph.json/keep": ""})), Options{})
		if got := codes(r.Diagnostics); !reflect.DeepEqual(got, []string{CodeGraphUnreadable}) {
			t.Fatalf("diagnostics = %v", got)
		}
		if len(r.Projects) != 2 {
			t.Fatalf("projects = %v", paths(r))
		}
	})
	t.Run("a named export that is missing is no finding", func(t *testing.T) {
		r := probe(t, tree(t, demo), Options{GraphFile: "out/graph.json"})
		if len(r.Diagnostics) != 0 || len(r.Projects) != 2 {
			t.Fatalf("result = %+v", r)
		}
	})
}

func TestProjectJSONAndWorkspacePackageJSONAreProjects(t *testing.T) {
	spectest.Proves(t, feature, "configuration", "project-json-and-workspace-package-json-are-projects")
	root := tree(t, map[string]string{
		"nx.json":      `{}`,
		"package.json": `{"name": "root", "workspaces": {"packages": ["packages/**", "!packages/legacy"]}}`,
		// project.json alone, named by it.
		"apps/web/project.json": `{"name": "web", "projectType": "application", "tags": ["scope:web"]}`,
		// project.json beside a package.json: project.json names it.
		"packages/a/package.json": `{"name": "@x/a", "version": "2.0.0"}`,
		"packages/a/project.json": `{"name": "a", "projectType": "library"}`,
		// package.json nx.name names it.
		"packages/nested/b/package.json": `{"name": "@x/b", "nx": {"name": "b", "projectType": "application"}}`,
		// package.json without a name: the directory names it.
		"packages/c/package.json": `{}`,
		// project.json without a name.
		"libs/d/project.json": `{}`,
		// Excluded by the workspace, not a project.
		"packages/legacy/package.json": `{"name": "@x/legacy"}`,
		// Outside the workspace patterns, not a project.
		"tools/script/package.json": `{"name": "script"}`,
		// Never read.
		"node_modules/x/project.json": `{"name": "nm"}`,
		"dist/libs/e/project.json":    `{"name": "dist"}`,
		".cache/f/project.json":       `{"name": "hidden"}`,
		// Malformed: a diagnostic, skipped.
		"libs/broken/project.json": `{"name": `,
	})
	r := probe(t, root, Options{})
	if got, want := paths(r), []string{"apps/web", "libs/d", "packages/a", "packages/c", "packages/nested/b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	names := map[string]string{}
	for _, p := range r.Projects {
		names[p.Path] = p.SourceName + " " + p.SourceFile + " " + p.Type
	}
	want := map[string]string{
		"apps/web":          "web apps/web/project.json application",
		"libs/d":            "d libs/d/project.json ",
		"packages/a":        "a packages/a/project.json library",
		"packages/c":        "c packages/c/package.json ",
		"packages/nested/b": "b packages/nested/b/package.json application",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	a, _ := project(t, r, "packages/a")
	if a.Version != "2.0.0" || !reflect.DeepEqual(a.WatchedFiles, []string{"packages/a/package.json", "packages/a/project.json"}) {
		t.Fatalf("a = %+v", a)
	}
	web, _ := project(t, r, "apps/web")
	if !reflect.DeepEqual(web.Tags, []string{"scope:web"}) {
		t.Fatalf("web tags = %v", web.Tags)
	}
	if got := codes(r.Diagnostics); !reflect.DeepEqual(got, []string{CodeManifestUnreadable}) {
		t.Fatalf("diagnostics = %v", got)
	}
	if !reflect.DeepEqual(r.WatchedFiles, []string{"nx.json", "package.json"}) {
		t.Fatalf("watched = %v", r.WatchedFiles)
	}

	t.Run("pnpm-workspace.yaml lists the packages", func(t *testing.T) {
		root := tree(t, map[string]string{
			"nx.json":      `{}`,
			"package.json": `{"name": "root"}`,
			"pnpm-workspace.yaml": `# packages
packages:
  - "apps/*"
  - 'libs/*' # libraries
  - "!libs/old"
catalog:
  - not/a/package
`,
			"apps/one/package.json": `{"name": "one"}`,
			"libs/two/package.json": `{"name": "two"}`,
			"libs/old/package.json": `{"name": "old"}`,
			"not/a/package.json":    `{"name": "nope"}`,
		})
		r := probe(t, root, Options{})
		if got := paths(r); !reflect.DeepEqual(got, []string{"apps/one", "libs/two"}) {
			t.Fatalf("paths = %v", got)
		}
		if !reflect.DeepEqual(r.WatchedFiles, []string{"nx.json", "package.json", "pnpm-workspace.yaml"}) {
			t.Fatalf("watched = %v", r.WatchedFiles)
		}
	})
	t.Run("package.json, pnpm-workspace.yaml and lerna.json add up", func(t *testing.T) {
		root := tree(t, map[string]string{
			"nx.json":                   `{}`,
			"package.json":              `{"workspaces": ["apps/*"]}`,
			"pnpm-workspace.yaml":       "packages: [\"libs/*\", 'tools/*']\n",
			"lerna.json":                `{"packages": ["modules/*"]}`,
			"apps/one/package.json":     `{"name": "one"}`,
			"libs/two/package.json":     `{"name": "two"}`,
			"tools/three/package.json":  `{"name": "three"}`,
			"modules/four/package.json": `{"name": "four"}`,
		})
		r := probe(t, root, Options{})
		if got := paths(r); !reflect.DeepEqual(got, []string{"apps/one", "libs/two", "modules/four", "tools/three"}) {
			t.Fatalf("paths = %v", got)
		}
		if !reflect.DeepEqual(r.WatchedFiles, []string{"lerna.json", "nx.json", "package.json", "pnpm-workspace.yaml"}) {
			t.Fatalf("watched = %v", r.WatchedFiles)
		}
	})
	t.Run("the package.json beside a project.json names it", func(t *testing.T) {
		root := tree(t, map[string]string{
			"nx.json":                  `{}`,
			"package.json":             `{"name": "solo", "version": "3.0.0"}`,
			"project.json":             `{}`,
			"apps/api/project.json":    `{}`,
			"apps/api/package.json":    `{"name": "@x/api", "dependencies": {"solo": "*"}}`,
			"apps/web/project.json":    `{"implicitDependencies": ["@x/api"]}`,
			"apps/broken/project.json": `{"name": "broken"}`,
			"apps/broken/package.json": `{`,
		})
		r := probe(t, root, Options{})
		names := map[string]string{}
		for _, p := range r.Projects {
			names[p.Path] = p.SourceName + " " + p.SourceFile + " " + p.Version
		}
		want := map[string]string{
			".":           "solo project.json 3.0.0",
			"apps/api":    "@x/api apps/api/project.json ",
			"apps/broken": "broken apps/broken/project.json ",
			"apps/web":    "web apps/web/project.json ",
		}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("names = %v, want %v", names, want)
		}
		if got := edges(r); !reflect.DeepEqual(got, map[string][]string{".": nil, "apps/api": {"."}, "apps/broken": nil, "apps/web": {"apps/api"}}) {
			t.Fatalf("edges = %v", got)
		}
		if got := codes(r.Diagnostics); !reflect.DeepEqual(got, []string{CodeManifestUnreadable}) {
			t.Fatalf("diagnostics = %v", got)
		}
	})
	t.Run("a malformed root package.json lists nothing", func(t *testing.T) {
		root := tree(t, map[string]string{
			"nx.json":               `{}`,
			"package.json":          `{`,
			"apps/one/package.json": `{"name": "one"}`,
			"apps/two/project.json": `{"name": "two"}`,
		})
		r := probe(t, root, Options{})
		if got := paths(r); !reflect.DeepEqual(got, []string{"apps/two"}) {
			t.Fatalf("paths = %v", got)
		}
		if got := codes(r.Diagnostics); !reflect.DeepEqual(got, []string{CodeManifestUnreadable}) {
			t.Fatalf("diagnostics = %v", got)
		}
	})
}

func TestEdgesComeFromImplicitDependenciesAndPackageDependencies(t *testing.T) {
	spectest.Proves(t, feature, "configuration", "edges-come-from-implicit-dependencies-and-package-dependencies")
	root := tree(t, map[string]string{
		"nx.json":                `{}`,
		"package.json":           `{"workspaces": ["apps/*", "libs/*"]}`,
		"apps/shop/project.json": `{"name": "shop", "implicitDependencies": ["ui-*", "!ui-legacy", "missing", "shop"]}`,
		"apps/shop/package.json": `{"name": "@x/shop",
  "dependencies": {"@x/core": "workspace:*", "react": "^19"},
  "devDependencies": {"@x/testing": "*"},
  "peerDependencies": {"@x/shop": "*"}}`,
		"apps/admin/package.json":     `{"name": "@x/admin", "optionalDependencies": {"@x/core": "1.0.0"}, "nx": {"implicitDependencies": ["shop"]}}`,
		"libs/core/package.json":      `{"name": "@x/core"}`,
		"libs/testing/package.json":   `{"name": "@x/testing"}`,
		"libs/ui-kit/project.json":    `{"name": "ui-kit"}`,
		"libs/ui-forms/project.json":  `{"name": "ui-forms"}`,
		"libs/ui-legacy/project.json": `{"name": "ui-legacy"}`,
	})
	r := probe(t, root, Options{})
	want := map[string][]string{
		"apps/admin":     {"apps/shop", "libs/core"},
		"apps/shop":      {"libs/core", "libs/testing", "libs/ui-forms", "libs/ui-kit"},
		"libs/core":      nil,
		"libs/testing":   nil,
		"libs/ui-forms":  nil,
		"libs/ui-kit":    nil,
		"libs/ui-legacy": nil,
	}
	if got := edges(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("edges = %v, want %v", got, want)
	}

	t.Run("star, tags and exclusions over scoped names", func(t *testing.T) {
		root := tree(t, map[string]string{
			"nx.json":                `{}`,
			"package.json":           `{"workspaces": ["libs/*", "apps/*"]}`,
			"apps/all/package.json":  `{"name": "@x/all", "nx": {"implicitDependencies": ["*", "!@x/b"]}, "dependencies": {"@x/b": "*"}}`,
			"apps/tags/package.json": `{"name": "@x/tags", "nx": {"implicitDependencies": ["tag:scope:shared", "@x/*-ui"]}}`,
			"libs/a/package.json":    `{"name": "@x/a", "nx": {"tags": ["scope:shared"]}}`,
			"libs/b/package.json":    `{"name": "@x/b"}`,
			"libs/c/package.json":    `{"name": "@x/c-ui"}`,
		})
		r := probe(t, root, Options{})
		want := map[string][]string{
			"apps/all":  {"apps/tags", "libs/a", "libs/c"},
			"apps/tags": {"libs/a", "libs/c"},
			"libs/a":    nil,
			"libs/b":    nil,
			"libs/c":    nil,
		}
		if got := edges(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("edges = %v, want %v", got, want)
		}
	})
}

func TestARepositoryWithoutNxHasNoNxProject(t *testing.T) {
	spectest.Proves(t, feature, "configuration", "a-repository-without-nx-has-no-nx-project")
	without := with(demo, nil)
	delete(without, "nx.json")
	for name, files := range map[string]map[string]string{
		"a Node workspace without nx.json": without,
		"an empty repository":              {},
		"nx.json and no project":           {"nx.json": `{}`, "package.json": `{"name": "solo"}`},
	} {
		t.Run(name, func(t *testing.T) {
			r := probe(t, tree(t, files), Options{})
			if len(r.Projects) != 0 || len(r.Diagnostics) != 0 {
				t.Fatalf("result = %+v", r)
			}
			if r.Version != workspace.ProbeProtocolVersion || r.Extension != Extension {
				t.Fatalf("result = %+v", r)
			}
		})
	}
}

func TestAProjectNamesItsTestOutputsAndItsLockfile(t *testing.T) {
	spectest.Proves(t, feature, "change-inputs", "a-project-names-its-test-outputs-and-its-lockfile")
	t.Run("the demo", func(t *testing.T) {
		for _, graph := range []bool{false, true} {
			files := demo
			if graph {
				files = with(demo, map[string]string{"graph.json": demoGraph})
			}
			r := probe(t, tree(t, files), Options{})
			_, m := project(t, r, "packages/pricing")
			if !reflect.DeepEqual(m.TestOutputs, []string{"packages/pricing/reports"}) || m.Lockfile != "package-lock.json" || m.Ecosystem != Ecosystem || m.Name != "@demo/pricing" {
				t.Fatalf("graph=%v metadata = %+v", graph, m)
			}
		}
	})
	t.Run("targets, defaults and tokens", func(t *testing.T) {
		root := tree(t, map[string]string{
			"nx.json": `{"targetDefaults": {
  "@nx/jest:jest": {"outputs": ["{workspaceRoot}/coverage/{projectRoot}"]},
  "e2e": {"outputs": ["{workspaceRoot}/dist/.playwright/{projectRoot}"]}
}}`,
			"package.json":   `{"workspaces": ["libs/*"]}`,
			"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
			"yarn.lock":      "",
			"apps/web/project.json": `{"name": "web", "targets": {
  "test": {"executor": "@nx/jest:jest", "options": {"jestConfig": "apps/web/jest.config.ts"}},
  "e2e": {"executor": "@nx/playwright:playwright"},
  "component-test": {"executor": "@nx/cypress:cypress", "outputs": ["{options.reportsDirectory}", "!{projectRoot}/tmp", "{options.missing}/x", "{options.unclosed"]},
  "unit": {"executor": "@nx/vite:test", "outputs": ["{projectRoot}/reports/**/*.xml", "{workspaceRoot}/coverage/{projectName}"], "options": {"reportsDirectory": "../../coverage/apps/web"}},
  "ci": {"command": "vitest run", "outputs": ["{projectRoot}/junit.xml", "{unknown}/x", "/abs/out"]},
  "build": {"executor": "@nx/js:tsc", "outputs": ["{projectRoot}/dist"]},
  "lint": {"executor": "@nx/eslint:lint"}
}}`,
			"libs/core/package.json": `{"name": "@x/core", "scripts": {"test:unit": "node --test", "build": "tsc"}, "nx": {"targets": {"test:unit": {"outputs": ["{projectRoot}/out"]}}}}`,
		})
		r := probe(t, root, Options{})
		_, web := project(t, r, "apps/web")
		want := []string{"apps/web/junit.xml", "apps/web/reports", "coverage/apps/web", "coverage/web", "dist/.playwright/apps/web"}
		if !reflect.DeepEqual(web.TestOutputs, want) {
			t.Fatalf("outputs = %v, want %v", web.TestOutputs, want)
		}
		if web.Lockfile != "pnpm-lock.yaml" {
			t.Fatalf("lockfile = %s", web.Lockfile)
		}
		_, core := project(t, r, "libs/core")
		if !reflect.DeepEqual(core.TestOutputs, []string{"libs/core/out"}) {
			t.Fatalf("core outputs = %v", core.TestOutputs)
		}
	})
	t.Run("a root project and no lockfile", func(t *testing.T) {
		r := probe(t, tree(t, map[string]string{
			"nx.json":      `{}`,
			"project.json": `{"name": "solo", "targets": {"test": {"executor": "@nx/vite:test", "outputs": ["{projectRoot}/coverage"]}}}`,
		}), Options{})
		_, m := project(t, r, ".")
		if !reflect.DeepEqual(m.TestOutputs, []string{"coverage"}) || m.Lockfile != "" {
			t.Fatalf("metadata = %+v", m)
		}
	})
}

func TestTheAnswerIsAValidProbeResult(t *testing.T) {
	spectest.Proves(t, feature, "probe-wire", "the-answer-is-a-valid-probe-result")
	for _, graph := range []bool{false, true} {
		files := demo
		if graph {
			files = with(demo, map[string]string{"graph.json": demoGraph})
		}
		root := tree(t, files)
		request := workspace.ProbeRequest{Version: workspace.ProbeProtocolVersion, Extension: Extension}
		var stdin, stdout bytes.Buffer
		if err := workspace.EncodeProbeRequest(&stdin, request); err != nil {
			t.Fatal(err)
		}
		handled, err := workspace.ServeProbe(workspace.ProbeControlArgs(), &stdin, &stdout, Answer(root, Options{}))
		if !handled || err != nil {
			t.Fatalf("graph=%v: handled=%v err=%v", graph, handled, err)
		}
		parsed, diags := workspace.ParseAndValidateProbeResult(bytes.TrimSpace(stdout.Bytes()))
		if diag.HasErrors(diags) {
			t.Fatalf("graph=%v: %v", graph, diags)
		}
		if got := paths(*parsed); !reflect.DeepEqual(got, []string{"apps/api", "packages/pricing"}) {
			t.Fatalf("graph=%v: paths = %v", graph, got)
		}
		first := workspace.ProbeResultDigest(*parsed)
		if again := workspace.ProbeResultDigest(probe(t, root, Options{})); again != first {
			t.Fatalf("graph=%v: the digest moves between two probes of one tree", graph)
		}
	}
	t.Run("a request that names paths gets only those projects", func(t *testing.T) {
		root := tree(t, demo)
		r := Probe(root, workspace.ProbeRequest{Version: workspace.ProbeProtocolVersion, Extension: Extension, Paths: []string{"./packages/pricing", "/abs"}}, Options{})
		if got := paths(r); !reflect.DeepEqual(got, []string{"packages/pricing"}) {
			t.Fatalf("paths = %v", got)
		}
	})
	t.Run("metadata from another provider is not Nx metadata", func(t *testing.T) {
		if _, ok := ReadMetadata(workspace.ProbeProject{Metadata: json.RawMessage(`{"ecosystem": "go"}`)}); ok {
			t.Fatal("read Go metadata as Nx metadata")
		}
		if _, ok := ReadMetadata(workspace.ProbeProject{}); ok {
			t.Fatal("read empty metadata as Nx metadata")
		}
	})
}

// owner returns the project whose path holds file: the deepest one.
func owner(r workspace.ProbeResult, file string) string {
	best := ""
	for _, p := range r.Projects {
		if (p.Path == workspace.ProbeRootPath || strings.HasPrefix(file, p.Path+"/")) && len(p.Path) > len(best) {
			best = p.Path
		}
	}
	return best
}

// dependents returns the projects that reach changed through their edges.
func dependents(r workspace.ProbeResult, changed map[string]bool) []string {
	reached := map[string]bool{}
	for grew := true; grew; {
		grew = false
		for _, p := range r.Projects {
			if changed[p.Path] || reached[p.Path] {
				continue
			}
			for _, dep := range p.Dependencies {
				if changed[dep] || reached[dep] {
					reached[p.Path] = true
					grew = true
					break
				}
			}
		}
	}
	out := make([]string, 0, len(reached))
	for p := range reached {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func TestTheNxDemoMapsChangedFilesWithAndWithoutAGraphExport(t *testing.T) {
	spectest.Proves(t, feature, "probe-wire", "the-nx-demo-maps-changed-files-with-and-without-a-graph-export")
	// The files the demo's merge request changes.
	changes := map[string]string{
		"package-lock.json":                  "",
		"packages/pricing/package.json":      "packages/pricing",
		"packages/pricing/src/index.spec.ts": "packages/pricing",
		"packages/pricing/src/index.ts":      "packages/pricing",
	}
	for name, files := range map[string]map[string]string{
		"without graph.json": demo,
		"with graph.json":    with(demo, map[string]string{"graph.json": demoGraph}),
	} {
		t.Run(name, func(t *testing.T) {
			r := probe(t, tree(t, files), Options{})
			changed := map[string]bool{}
			for file, want := range changes {
				got := owner(r, file)
				if got != want {
					t.Fatalf("%s belongs to %q, want %q", file, got, want)
				}
				if got != "" {
					changed[got] = true
				}
			}
			if got := dependents(r, changed); !reflect.DeepEqual(got, []string{"apps/api"}) {
				t.Fatalf("dependents = %v", got)
			}
			_, m := project(t, r, "packages/pricing")
			if m.Lockfile != "package-lock.json" {
				t.Fatalf("lockfile = %q", m.Lockfile)
			}
		})
	}
}
