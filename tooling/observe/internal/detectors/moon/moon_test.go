package moon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/workspace"
)

const feature = "tooling/observe-moon"

// demo is a moon workspace: a Node front end, an API that declares its
// dependency on pricing in its moon.yml, and a Rust pricing library with no
// package.json, mapped by name.
var demo = map[string]string{
	".moon/workspace.yml": `# moon workspace
$schema: 'https://moonrepo.dev/schemas/workspace.json'
projects:
  globs:
    - 'apps/*'
  sources:
    pricing: 'packages/pricing'
vcs:
  manager: 'git'
  defaultBranch: 'main'
`,
	"apps/api/moon.yml": `language: 'typescript'
dependsOn:
  - 'pricing'
tasks:
  test:
    command: 'vitest run'
    inputs: ['src/**/*']
`,
	"apps/api/package.json":         `{"name": "@demo/api", "dependencies": {"zod": "3.24.1"}}`,
	"apps/api/src/index.ts":         "export const api = 1;\n",
	"apps/web/package.json":         `{"name": "@demo/web", "devDependencies": {"vite": "6.0.0"}}`,
	"apps/web/src/main.ts":          "export const web = 1;\n",
	"packages/pricing/moon.yml":     "language: 'rust'\nlayer: 'library'\n",
	"packages/pricing/Cargo.toml":   "[package]\nname = \"pricing\"\nversion = \"0.1.0\"\n",
	"packages/pricing/src/lib.rs":   "pub fn price() -> u32 { 1 }\n",
	"apps/.hidden/moon.yml":         "language: 'bash'\n",
	"apps/node_modules/x/README.md": "not a project\n",
}

// graphV1 is what moon 1.x `project-graph --json` writes for the demo: the
// projects are the nodes, and api's root-scope dependency on the root project
// is no edge.
const graphV1 = `{
  "graph": {
    "nodes": [
      {"id": "api", "source": "apps/api", "root": "/ci/apps/api", "alias": "@demo/api", "dependencies": [{"id": "pricing", "scope": "production", "source": "explicit"}], "language": "typescript", "layer": "application"},
      {"id": "pricing", "source": "packages/pricing", "root": "/ci/packages/pricing", "language": "rust", "layer": "library"},
      {"id": "web", "source": "apps/web", "root": "/ci/apps/web", "alias": "@demo/web", "language": "typescript"},
      {"id": "repo", "source": ".", "root": "/ci", "language": "unknown"}
    ],
    "node_holes": [],
    "edge_property": "directed",
    "edges": [[0, 1, "production"], [2, 3, "root"], [0, 0, "production"]]
  }
}`

// graphV2 is what moon 2.x writes: node indices, with the projects in data.
const graphV2 = `{
  "graph": {
    "nodes": [0, 1, 2],
    "node_holes": [],
    "edge_property": "directed",
    "edges": [[0, 1, "production"]]
  },
  "data": {
    "0": {"id": "api", "source": "apps/api", "dependencies": [{"id": "pricing", "scope": "production"}]},
    "1": {"id": "pricing", "source": "packages/pricing"},
    "2": {"id": "web", "source": "apps/web"}
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

// edges returns each project's path and its dependencies.
func edges(r workspace.ProbeResult) map[string][]string {
	out := map[string][]string{}
	for _, p := range r.Projects {
		out[p.Path] = p.Dependencies
	}
	return out
}

func codes(r workspace.ProbeResult) []string {
	var out []string
	for _, d := range r.Diagnostics {
		out = append(out, d.Code)
	}
	return out
}

func noDiagnostics(t *testing.T, r workspace.ProbeResult) {
	t.Helper()
	if len(r.Diagnostics) > 0 {
		t.Fatalf("diagnostics %+v", r.Diagnostics)
	}
}

func TestAMoonGraphExportGivesTheProjectsAndTheirEdges(t *testing.T) {
	spectest.Proves(t, feature, "moon-graph-export", "a-moon-graph-export-gives-the-projects-and-their-edges")
	t.Run("moon 1.x at the root", func(t *testing.T) {
		// The export wins over the configuration, which declares no edge here.
		r := probe(t, tree(t, with(demo, map[string]string{"moon-graph.json": graphV1, "apps/api/moon.yml": "language: 'typescript'\n"})), Options{})
		noDiagnostics(t, r)
		want := map[string][]string{".": nil, "apps/api": {"packages/pricing"}, "apps/web": nil, "packages/pricing": nil}
		if got := edges(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("edges = %v, want %v", got, want)
		}
		for _, p := range r.Projects {
			m, ok := ReadMetadata(p)
			if !ok || m.Ecosystem != Ecosystem || m.Source != evidence.SourceOrchestrator || m.Name != p.SourceName {
				t.Errorf("%s metadata %+v", p.Path, m)
			}
		}
		if got := r.Projects[0].SourceFile; got != "moon-graph.json" {
			t.Errorf("the root project's source file %q, want the export", got)
		}
		if got := r.Projects[3].SourceFile; got != "packages/pricing/moon.yml" {
			t.Errorf("pricing's source file %q, want its moon.yml", got)
		}
		if !reflect.DeepEqual(r.WatchedFiles, []string{WorkspaceFile, "moon-graph.json"}) {
			t.Errorf("watched files %v", r.WatchedFiles)
		}
	})
	t.Run("moon 2.x at a named path", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "graph.json")
		if err := os.WriteFile(outside, []byte(graphV2), 0o644); err != nil {
			t.Fatal(err)
		}
		r := probe(t, tree(t, demo), Options{GraphFile: outside})
		noDiagnostics(t, r)
		want := map[string][]string{"apps/api": {"packages/pricing"}, "apps/web": nil, "packages/pricing": nil}
		if got := edges(r); !reflect.DeepEqual(got, want) {
			t.Fatalf("edges = %v, want %v", got, want)
		}
		if r.Projects[1].SourceName != "web" || r.Projects[1].SourceFile != "" {
			t.Errorf("web %+v", r.Projects[1])
		}
	})
	t.Run("projects without a source or sharing one are skipped", func(t *testing.T) {
		graph := `{"graph": {"nodes": [{"id": "a", "source": "a"}, {"id": "b"}, {"id": "c", "source": "./a"}, {"id": "d", "source": "../out"}], "edges": [[2, 0, "production"], null]}}`
		r := probe(t, tree(t, map[string]string{"moon-graph.json": graph}), Options{})
		if got := edges(r); !reflect.DeepEqual(got, map[string][]string{"a": nil}) {
			t.Fatalf("edges = %v", got)
		}
		if got := codes(r); !reflect.DeepEqual(got, []string{CodeGraphUnreadable, CodeGraphUnreadable, CodeGraphUnreadable}) {
			t.Fatalf("codes = %v", got)
		}
	})
}

func TestAMalformedMoonGraphExportFallsBackToTheConfiguration(t *testing.T) {
	spectest.Proves(t, feature, "moon-graph-export", "a-malformed-moon-graph-export-falls-back-to-the-configuration")
	configured := map[string][]string{"apps/api": {"packages/pricing"}, "apps/web": nil, "packages/pricing": nil}
	for name, export := range map[string]string{
		"not JSON":             `{"graph": `,
		"moon 1.10 labels":     `{"nodes": [{"id": 0, "label": "api"}], "edges": [{"id": "0 -> 1", "source": 0, "target": 1}]}`,
		"nodes without source": `{"graph": {"nodes": [{"id": "api"}], "edges": []}}`,
		"a dangling index":     `{"graph": {"nodes": [7], "edges": []}, "data": {}}`,
		"an edge of strings":   `{"graph": {"nodes": [{"id": "api", "source": "apps/api"}], "edges": [["api", "web"]]}}`,
		"no node":              `{"graph": {"nodes": [], "edges": []}}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := probe(t, tree(t, with(demo, map[string]string{"moon-graph.json": export})), Options{})
			if got := edges(r); !reflect.DeepEqual(got, configured) {
				t.Fatalf("edges = %v, want the configuration's", got)
			}
			if got := codes(r); !reflect.DeepEqual(got, []string{CodeGraphUnreadable}) {
				t.Fatalf("codes = %v", got)
			}
		})
	}
	t.Run("a named export that does not exist", func(t *testing.T) {
		r := probe(t, tree(t, demo), Options{GraphFile: "out/moon-graph.json"})
		if got := edges(r); !reflect.DeepEqual(got, configured) {
			t.Fatalf("edges = %v", got)
		}
		if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != CodeGraphUnreadable || r.Diagnostics[0].Severity != diag.Warning {
			t.Fatalf("diagnostics %+v", r.Diagnostics)
		}
	})
	t.Run("the default export is optional", func(t *testing.T) {
		noDiagnostics(t, probe(t, tree(t, demo), Options{}))
	})
	t.Run("another tool's moon-graph.json outside a moon workspace is no finding", func(t *testing.T) {
		r := probe(t, tree(t, map[string]string{"moon-graph.json": `{"phases": []}`}), Options{})
		noDiagnostics(t, r)
		if len(r.Projects) != 0 {
			t.Fatalf("projects %+v", r.Projects)
		}
	})
}

func TestGlobbedAndMappedProjectsAreComponents(t *testing.T) {
	spectest.Proves(t, feature, "moon-projects", "globbed-and-mapped-projects-are-components")
	t.Run("globs and sources together", func(t *testing.T) {
		r := probe(t, tree(t, demo), Options{})
		noDiagnostics(t, r)
		var got []string
		for _, p := range r.Projects {
			got = append(got, p.Path+"="+p.SourceName)
		}
		// apps/.hidden and apps/node_modules are never globbed.
		if want := []string{"apps/api=api", "apps/web=web", "packages/pricing=pricing"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("projects = %v, want %v", got, want)
		}
		if r.Projects[2].SourceFile != "packages/pricing/moon.yml" || r.Projects[1].SourceFile != "" {
			t.Errorf("source files %q %q", r.Projects[2].SourceFile, r.Projects[1].SourceFile)
		}
		for _, p := range r.Projects {
			if m, ok := ReadMetadata(p); !ok || m.Source != evidence.SourceWorkspace || m.Name != p.SourceName {
				t.Errorf("%s metadata %+v", p.Path, m)
			}
		}
		want := []string{WorkspaceFile}
		if !reflect.DeepEqual(r.WatchedFiles, want) {
			t.Errorf("watched %v", r.WatchedFiles)
		}
		if got := r.Projects[0].WatchedFiles; !reflect.DeepEqual(got, []string{"apps/api/moon.yml", "apps/api/package.json"}) {
			t.Errorf("api watches %v", got)
		}
	})
	cases := map[string]struct {
		workspace string
		files     map[string]string
		want      []string
	}{
		"a list of globs, one ending in moon.yml, one negated, one with braces": {
			workspace: "projects: ['apps/*', 'tools/**/moon.yml', '!apps/legacy', 'libs/{a,b}']\n",
			files: map[string]string{
				"apps/one/x": "", "apps/legacy/x": "", "tools/deep/gen/moon.yml": "", "tools/deep/other/x": "",
				"libs/a/x": "", "libs/b/x": "", "libs/c/x": "",
			},
			want: []string{"apps/one=one", "libs/a=a", "libs/b=b", "tools/deep/gen=gen"},
		},
		"a map of ids to directories": {
			workspace: "projects:\n  api: 'apps/api'\n  \"core\": crates/core/\n",
			files:     map[string]string{"apps/api/x": "", "crates/core/x": ""},
			want:      []string{"apps/api=api", "crates/core=core"},
		},
		"the root project and a mapped directory a glob also matches": {
			workspace: "projects:\n  globs: ['.', 'apps/*']\n  sources:\n    gateway: apps/api\n",
			files:     map[string]string{"apps/api/x": "", "apps/web/x": ""},
			want:      []string{".=", "apps/api=gateway", "apps/web=web"},
		},
		"globFormat source-path names a globbed project by its path": {
			workspace: "projects:\n  globs: ['apps/*', 'packages/*']\n  globFormat: 'source-path'\n  sources:\n    core: libs/core\n",
			files:     map[string]string{"apps/api/moon.yml": "dependsOn: ['packages/types']\n", "packages/types/x": "", "libs/core/x": ""},
			want:      []string{"apps/api=apps/api", "libs/core=core", "packages/types=packages/types"},
		},
		"globFormat dir-name": {
			workspace: "projects:\n  globs: ['apps/*']\n  globFormat: dir-name\n",
			files:     map[string]string{"apps/api/x": ""},
			want:      []string{"apps/api=api"},
		},
		"a moon.yml id renames its project": {
			workspace: "projects:\n- apps/*\n",
			files:     map[string]string{"apps/api/moon.yml": "id: 'gateway'\n"},
			want:      []string{"apps/api=gateway"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			root := tree(t, with(c.files, map[string]string{WorkspaceFile: c.workspace}))
			r := probe(t, root, Options{})
			noDiagnostics(t, r)
			var got []string
			for _, p := range r.Projects {
				name := p.SourceName
				if p.Path == "." {
					name = strings.TrimPrefix(name, filepath.Base(root))
				}
				got = append(got, p.Path+"="+name)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("projects = %v, want %v", got, c.want)
			}
		})
	}
	t.Run("a mapped directory that does not exist or leaves the repository", func(t *testing.T) {
		r := probe(t, tree(t, map[string]string{WorkspaceFile: "projects:\n  gone: apps/gone\n  out: ../out\n  api: apps/api\n", "apps/api/x": ""}), Options{})
		if got := edges(r); !reflect.DeepEqual(got, map[string][]string{"apps/api": nil}) {
			t.Fatalf("edges = %v", got)
		}
		if got := codes(r); !reflect.DeepEqual(got, []string{CodeWorkspaceUnreadable, CodeWorkspaceUnreadable}) {
			t.Fatalf("codes = %v", got)
		}
	})
	t.Run("a repository without a moon workspace has no moon project", func(t *testing.T) {
		r := probe(t, tree(t, map[string]string{"apps/api/moon.yml": "dependsOn: []\n"}), Options{})
		noDiagnostics(t, r)
		if len(r.Projects) != 0 {
			t.Fatalf("projects %+v", r.Projects)
		}
	})
}

func TestAMalformedWorkspaceYMLIsAWarningWithNoProject(t *testing.T) {
	for name, text := range map[string]string{
		"an unclosed flow list":    "projects: ['apps/*'\n",
		"a scalar":                 "projects: apps\n",
		"an alias":                 "projects: *all\n",
		"a glob that is no string": "projects:\n  - [apps]\n",
		"a source that is no path": "projects:\n  api: [apps/api]\n",
		"an unknown globFormat":    "projects:\n  globs: ['apps/*']\n  globFormat: 'kebab'\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := probe(t, tree(t, with(demo, map[string]string{WorkspaceFile: text})), Options{})
			if len(r.Projects) != 0 {
				t.Fatalf("projects %+v", r.Projects)
			}
			if got := codes(r); !reflect.DeepEqual(got, []string{CodeWorkspaceUnreadable}) {
				t.Fatalf("codes = %v", got)
			}
		})
	}
}

func TestDependsOnAndPackageDependenciesAreEdges(t *testing.T) {
	spectest.Proves(t, feature, "moon-edges", "depends-on-and-package-dependencies-are-edges")
	files := map[string]string{
		WorkspaceFile: "projects:\n  - 'apps/*'\n  - 'libs/*'\n",
		// A string id, an object id, an alias, itself and an unknown project.
		"apps/api/moon.yml": `dependsOn:
  - 'pricing'
  - id: 'auth'
    scope: 'development'
  - { id: '@demo/ui', scope: 'peer' }
  - 'api'
  - 'missing'
`,
		"libs/pricing/moon.yml": "type: library\n",
		"libs/auth/Cargo.toml":  "[package]\nname = \"auth\"\n",
		"libs/ui/package.json":  `{"name": "@demo/ui"}`,
		// A package dependency on another project's package, and a registry one.
		"apps/web/package.json": `{"name": "@demo/web", "dependencies": {"@demo/ui": "workspace:*", "react": "19.0.0"}}`,
		// Task deps on another project's task, and targets that name no other project.
		"apps/web/moon.yml": `tasks:
  build:
    command: 'vite build'
    deps: ['tools:build', '^:build', '~:lint', 'lint', ':check', '#lib:build', 'web:test']
  generate:
    deps:
      - target: 'codegen:run'
        args: ['--all']
`,
		"libs/tools/x":            "",
		"libs/codegen/x":          "",
		"apps/admin/moon.yml":     "dependsOn: [web]\n",
		"apps/admin/package.json": `{"name": "admin", "devDependencies": {"@demo/web": "*"}}`,
	}
	r := probe(t, tree(t, files), Options{})
	noDiagnostics(t, r)
	want := map[string][]string{
		"apps/admin":   {"apps/web"},
		"apps/api":     {"libs/auth", "libs/pricing", "libs/ui"},
		"apps/web":     {"libs/codegen", "libs/tools", "libs/ui"},
		"libs/auth":    nil,
		"libs/codegen": nil,
		"libs/tools":   nil,
		"libs/pricing": nil,
		"libs/ui":      nil,
	}
	if got := edges(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("edges = %v, want %v", got, want)
	}
	t.Run("a moon.yml that does not read keeps its project without declared edges", func(t *testing.T) {
		r := probe(t, tree(t, with(files, map[string]string{"apps/api/moon.yml": "dependsOn: 'pricing'\n", "apps/admin/moon.yml": "dependsOn: [web\n"})), Options{})
		got := edges(r)
		if deps, ok := got["apps/api"]; !ok || deps != nil {
			t.Fatalf("api = %v (%v)", deps, ok)
		}
		if deps := got["apps/admin"]; !reflect.DeepEqual(deps, []string{"apps/web"}) {
			t.Fatalf("admin keeps its package edge: %v", deps)
		}
		if got := codes(r); !reflect.DeepEqual(got, []string{CodeManifestUnreadable, CodeManifestUnreadable}) {
			t.Fatalf("codes = %v", got)
		}
	})
	t.Run("tasks that do not read cost the task edges only", func(t *testing.T) {
		r := probe(t, tree(t, with(files, map[string]string{"apps/web/moon.yml": "dependsOn: ['tools']\ntasks:\n  build:\n    <<: *defaults\n    deps: ['codegen:run']\n"})), Options{})
		if deps := edges(r)["apps/web"]; !reflect.DeepEqual(deps, []string{"libs/tools", "libs/ui"}) {
			t.Fatalf("web = %v", deps)
		}
		if got := codes(r); !reflect.DeepEqual(got, []string{CodeManifestUnreadable}) {
			t.Fatalf("codes = %v", got)
		}
	})
	t.Run("only a plain dependsOn links the root project, as moon does", func(t *testing.T) {
		r := probe(t, tree(t, map[string]string{
			WorkspaceFile:         "projects:\n  globs: ['apps/*']\n  sources:\n    root: '.'\n",
			"package.json":        `{"name": "monorepo"}`,
			"apps/a/moon.yml":     "dependsOn: ['root', 'b']\n",
			"apps/b/moon.yml":     "dependsOn:\n  - id: root\n    scope: root\ntasks:\n  build:\n    deps: ['root:build']\n",
			"apps/b/package.json": `{"name": "b", "devDependencies": {"monorepo": "*"}}`,
		}), Options{})
		noDiagnostics(t, r)
		if got := edges(r); !reflect.DeepEqual(got, map[string][]string{".": nil, "apps/a": {".", "apps/b"}, "apps/b": nil}) {
			t.Fatalf("edges = %v", got)
		}
	})
	t.Run("a package.json that does not read keeps its project without package edges", func(t *testing.T) {
		r := probe(t, tree(t, with(files, map[string]string{"apps/web/package.json": "{"})), Options{})
		if deps := edges(r)["apps/web"]; !reflect.DeepEqual(deps, []string{"libs/codegen", "libs/tools"}) {
			t.Fatalf("web keeps only its task edges: %v", deps)
		}
		if got := codes(r); !reflect.DeepEqual(got, []string{CodeManifestUnreadable}) {
			t.Fatalf("codes = %v", got)
		}
	})
}

func TestTheMoonAnswerIsAValidProbeResult(t *testing.T) {
	spectest.Proves(t, feature, "moon-probe-answer", "the-moon-answer-is-a-valid-probe-result")
	for name, files := range map[string]map[string]string{
		"configuration": demo,
		"export":        with(demo, map[string]string{"moon-graph.json": graphV1}),
	} {
		t.Run(name, func(t *testing.T) {
			root := tree(t, files)
			request := workspace.ProbeRequest{Version: workspace.ProbeProtocolVersion, Extension: Extension}
			var stdin, stdout bytes.Buffer
			if err := workspace.EncodeProbeRequest(&stdin, request); err != nil {
				t.Fatal(err)
			}
			handled, err := workspace.ServeProbe(workspace.ProbeControlArgs(), &stdin, &stdout, Answer(root, Options{}))
			if !handled || err != nil {
				t.Fatalf("handled=%v err=%v", handled, err)
			}
			parsed, diags := workspace.ParseAndValidateProbeResult(bytes.TrimSpace(stdout.Bytes()))
			if diag.HasErrors(diags) {
				t.Fatal(diags)
			}
			if len(parsed.Projects) < 3 || parsed.Projects[0].Path > parsed.Projects[1].Path {
				t.Fatalf("projects %+v", parsed.Projects)
			}
			if workspace.ProbeResultDigest(*parsed) != workspace.ProbeResultDigest(probe(t, root, Options{})) {
				t.Fatal("the digest moves between two probes of one tree")
			}
		})
	}
	t.Run("a request that names paths gets only those projects", func(t *testing.T) {
		r := Probe(tree(t, demo), workspace.ProbeRequest{Version: workspace.ProbeProtocolVersion, Extension: Extension, Paths: []string{"./packages/pricing"}}, Options{})
		if len(r.Projects) != 1 || r.Projects[0].Path != "packages/pricing" {
			t.Fatalf("projects %+v", r.Projects)
		}
	})
	t.Run("metadata from another provider is not moon metadata", func(t *testing.T) {
		if _, ok := ReadMetadata(workspace.ProbeProject{Metadata: json.RawMessage(`{"ecosystem": "nx"}`)}); ok {
			t.Fatal("read Nx metadata as moon metadata")
		}
	})
}
