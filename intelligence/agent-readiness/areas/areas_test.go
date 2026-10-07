package areas

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/intelligence/agent-readiness/contract"
)

func reader(files map[string]string) Reader {
	return func(paths []string) map[string][]byte {
		out := map[string][]byte{}
		for _, file := range paths {
			if content, ok := files[file]; ok {
				out[file] = []byte(content)
			}
		}
		return out
	}
}

func paths(layout Layout) []string {
	out := make([]string, 0, len(layout.Areas))
	for _, area := range layout.Areas {
		out = append(out, area.Name+"="+area.Path)
	}
	return out
}

func TestDetectReadsEveryWorkspaceManifest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    []string
		contents map[string]string
		want     []string
		kind     string
	}{
		{
			name:     "pnpm with a negated glob",
			files:    []string{"pnpm-workspace.yaml", "apps/web/package.json", "apps/legacy/package.json", "packages/ui/package.json", "packages/ui/src/a.ts"},
			contents: map[string]string{"pnpm-workspace.yaml": "packages:\n  - 'apps/*'   # apps\n  - \"packages/**\"\n  - '!apps/legacy'\nother: 1\n"},
			want:     []string{"root=.", "web=apps/web", "ui=packages/ui"},
			kind:     "pnpm",
		},
		{
			name:     "package.json workspaces object",
			files:    []string{"package.json", "libs/a/package.json", "libs/b/package.json"},
			contents: map[string]string{"package.json": `{"workspaces":{"packages":["libs/*"]}}`},
			want:     []string{"root=.", "a=libs/a", "b=libs/b"},
			kind:     "npm-workspaces",
		},
		{
			name:     "go.work single and block uses",
			files:    []string{"go.work", "tools/go.mod", "svc/api/go.mod", "svc/api/main.go"},
			contents: map[string]string{"go.work": "go 1.23\n\nuse ./tools // tools\nuse (\n\t./svc/api\n\t../outside\n)\n"},
			want:     []string{"root=.", "api=svc/api", "tools=tools"},
			kind:     "go.work",
		},
		{
			name:     "cargo workspace across lines",
			files:    []string{"Cargo.toml", "crates/core/Cargo.toml", "crates/cli/Cargo.toml"},
			contents: map[string]string{"Cargo.toml": "[package]\nname = \"x\"\n[workspace]\nmembers = [\n  \"crates/*\",\n]\n"},
			want:     []string{"root=.", "cli=crates/cli", "core=crates/core"},
			kind:     "cargo",
		},
		{
			name:     "uv workspace",
			files:    []string{"pyproject.toml", "pkgs/one/pyproject.toml"},
			contents: map[string]string{"pyproject.toml": "[tool.uv.workspace]\nmembers = ['pkgs/*']\n"},
			want:     []string{"root=.", "one=pkgs/one"},
			kind:     "uv",
		},
		{
			name:     "putnami and nx projects",
			files:    []string{"putnami.workspace.json", "nx.json", "a/putnami.json", "a/lib/putnami.json", "b/lib/putnami.json", "b/lib/testdata/ws/putnami.json", "c/project.json"},
			contents: map[string]string{"putnami.workspace.json": "{}", "nx.json": "{}", "a/putnami.json": `{"includes":["lib"]}`, "a/lib/putnami.json": `{"name":"lib"}`},
			want:     []string{"root=.", "a/lib=a/lib", "b/lib=b/lib", "c=c"},
			kind:     "putnami",
		},
		{
			name:     "lerna default packages",
			files:    []string{"lerna.json", "packages/x/package.json"},
			contents: map[string]string{"lerna.json": "{}"},
			want:     []string{"root=.", "x=packages/x"},
			kind:     "lerna",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := Detect(tc.files, reader(tc.contents))
			if got := paths(layout); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("areas = %v, want %v", got, tc.want)
			}
			if !layout.Declared() || layout.Manifests[0].Kind != tc.kind {
				t.Fatalf("manifests = %+v, want kind %s", layout.Manifests, tc.kind)
			}
			for _, area := range layout.Areas {
				if want := contract.AreaFromManifest; area.Path != RootPath && area.Source != want {
					t.Fatalf("area %s source %s", area.Path, area.Source)
				}
			}
		})
	}
}

func TestDetectInfersAreasWithoutAManifest(t *testing.T) {
	layout := Detect([]string{"README.md", "api/go.mod", "api/main.go", "web/app/package.json", "node_modules/x/package.json", "has space/go.mod"}, reader(nil))
	if got, want := paths(layout), []string{"root=.", "api=api", "app=web/app"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("areas = %v, want %v", got, want)
	}
	if layout.Declared() || layout.Areas[1].Source != contract.AreaInferred {
		t.Fatalf("layout = %+v", layout)
	}
	flat := Detect([]string{"src/a.py", "docs/b.md", ".github/ci.yml"}, reader(nil))
	if got, want := paths(flat), []string{"root=.", "docs=docs", "src=src"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("directory areas = %v, want %v", got, want)
	}
	if got := flat.Assign("elsewhere/x"); got != 0 {
		t.Fatalf("Assign(elsewhere/x) = %d, want the root area", got)
	}
}

func TestDetectKeepsNamesUniqueAndFoldsLongPaths(t *testing.T) {
	long := strings.Repeat("d", 130) + "/package.json"
	layout := Detect([]string{"pnpm-workspace.yaml", "apps/api/package.json", "services/api/package.json", "tools/root/package.json", "x.md", long},
		reader(map[string]string{"pnpm-workspace.yaml": "packages: ['apps/*', 'services/*', 'tools/*', '*']\n"}))
	want := []string{".=.", "apps/api=apps/api", "services/api=services/api", "tools/root=tools/root"}
	if got := paths(layout); !reflect.DeepEqual(got, want) {
		t.Fatalf("areas = %v, want %v", got, want)
	}
	if got := layout.Assign("services/api/src/x.ts"); layout.Areas[got].Path != "services/api" {
		t.Fatalf("Assign picked %+v", layout.Areas[got])
	}
}

func TestDetectLeavesNoRootAreaWhenAreasCoverEverything(t *testing.T) {
	layout := Detect([]string{"a/go.mod"}, reader(nil))
	if got := paths(layout); !reflect.DeepEqual(got, []string{"a=a"}) {
		t.Fatalf("areas = %v", got)
	}
	if got := layout.Assign("b/x.go"); got != -1 {
		t.Fatalf("Assign outside every area = %d", got)
	}
}

func TestGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern, dir string
		want         bool
	}{
		{"packages/*", "packages/ui", true},
		{"packages/*", "packages/ui/nested", false},
		{"packages/**", "packages/ui/nested", true},
		{"**/ui", "a/b/ui", true},
		{"**", "", true},
		{"apps/[ab]*", "apps/api", true},
		{"apps", "apps/api", false},
	} {
		split := func(value string) []string {
			if value == "" {
				return nil
			}
			return strings.Split(value, "/")
		}
		if got := Glob(split(tc.pattern), split(tc.dir)); got != tc.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", tc.pattern, tc.dir, got, tc.want)
		}
	}
}

func TestYamlListReadsTheInlineForm(t *testing.T) {
	if got := yamlList("packages: ['a/*', \"b\"]\n", "packages"); !reflect.DeepEqual(got, []string{"a/*", "b"}) {
		t.Fatalf("yamlList = %v", got)
	}
	if got := tomlList("[workspace]\nresolver = \"2\"\n", "workspace", "members"); got != nil {
		t.Fatalf("tomlList without members = %v", got)
	}
	if got := packageWorkspaces([]byte("not json")); got != nil {
		t.Fatalf("packageWorkspaces(invalid) = %v", got)
	}
}

func TestDetectReadsMavenModules(t *testing.T) {
	files := []string{
		"pom.xml", "api/pom.xml", "api/src/A.java", "server/pom.xml", "server/src/S.java", "e2e/pom.xml", "e2e/src/E.java",
		"support/net/pom.xml", "support/net/src/N.java", "tools/legacy/pom.xml", "tools/legacy/L.java", "README.md",
	}
	layout := Detect(files, reader(map[string]string{
		"pom.xml": `<?xml version="1.0"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <!-- <module>tools/legacy</module> -->
  <modules>
    <module>api</module>
    <module> support/net </module>
    <module>server/pom.xml</module>
    <module>../outside</module>
  </modules>
  <profiles>
    <profile><id>e2e</id><modules><module>e2e</module></modules></profile>
  </profiles>
</project>
`,
	}))
	want := []string{"root=.", "api=api", "e2e=e2e", "server=server", "net=support/net"}
	if got := paths(layout); !reflect.DeepEqual(got, want) {
		t.Fatalf("areas = %v, want %v", got, want)
	}
	if len(layout.Manifests) != 1 || layout.Manifests[0].Kind != "maven" || layout.Manifests[0].Path != "pom.xml" {
		t.Fatalf("manifests = %+v", layout.Manifests)
	}
	if got := mavenModules([]byte("<project><modules>")); got != nil {
		t.Fatalf("an unreadable pom.xml declares %v", got)
	}
}

func TestWantedAddsEveryPutnamiProjectFile(t *testing.T) {
	got := Wanted([]string{"a/putnami.json", "a/main.go", "putnami.json", "web/package.json", "web/app/package.json", "vendor/package.json"})
	if !slices.Contains(got, "a/putnami.json") || !slices.Contains(got, "putnami.json") || !slices.Contains(got, "go.work") || slices.Contains(got, "a/main.go") {
		t.Fatalf("Wanted = %v", got)
	}
	if !slices.Contains(got, "web/package.json") || slices.Contains(got, "web/app/package.json") || slices.Contains(got, "vendor/package.json") {
		t.Fatalf("Wanted = %v", got)
	}
}

func TestDetectReadsNestedWorkspacesAndBazel(t *testing.T) {
	t.Run("workspaces declared in top-level directories", func(t *testing.T) {
		files := []string{
			"frontend/package.json", "frontend/web/package.json", "frontend/packages/ui/package.json", "frontend/tools/x.js",
			"backend/package.json", "backend/pnpm-workspace.yaml", "backend/shared/package.json", "backend/apps/api/package.json", "backend/legacy/package.json",
			"ai/pyproject.toml", "ai/main.py", "scripts/deploy.sh", "vendor/package.json",
		}
		layout := Detect(files, reader(map[string]string{
			"frontend/package.json":       `{"workspaces":["web","packages/*"]}`,
			"backend/package.json":        `{"name":"backend"}`,
			"backend/pnpm-workspace.yaml": "packages:\n  - shared\n  - apps/*\n",
			"vendor/package.json":         `{"workspaces":["*"]}`,
		}))
		want := []string{"root=.", "ai=ai", "backend=backend", "api=backend/apps/api", "shared=backend/shared", "frontend=frontend", "ui=frontend/packages/ui", "web=frontend/web"}
		if got := paths(layout); !reflect.DeepEqual(got, want) {
			t.Fatalf("areas = %v, want %v", got, want)
		}
		manifests := make([]string, 0, len(layout.Manifests))
		for _, manifest := range layout.Manifests {
			manifests = append(manifests, manifest.Kind+"="+manifest.Path)
		}
		if want := []string{"pnpm=backend/pnpm-workspace.yaml", "npm-workspaces=frontend/package.json"}; !reflect.DeepEqual(manifests, want) {
			t.Fatalf("manifests = %v, want %v", manifests, want)
		}
		for _, area := range layout.Areas {
			if want := contract.AreaFromManifest; area.Path == "ai" || area.Path == RootPath {
				want = contract.AreaInferred
				if area.Source != want {
					t.Fatalf("area %s source %s, want %s", area.Path, area.Source, want)
				}
			} else if area.Source != want {
				t.Fatalf("area %s source %s, want %s", area.Path, area.Source, want)
			}
		}
	})

	t.Run("bazel packages under a partial pnpm workspace", func(t *testing.T) {
		files := []string{
			"MODULE.bazel", "BUILD.bazel", "go.mod", "pnpm-workspace.yaml", "tools/lint/package.json", "tools/lint/BUILD",
			"lakshmi/cmd/BUILD.bazel", "lakshmi/cmd/main.go", "lakshmi/internal/a.go", "anubis/app/BUILD", "anubis/app/main.py",
			".github/BUILD", "docs/guide.md",
		}
		layout := Detect(files, reader(map[string]string{"pnpm-workspace.yaml": "packages:\n  - tools/*\n"}))
		want := []string{"root=.", "anubis=anubis", "lakshmi=lakshmi", "tools=tools", "lint=tools/lint"}
		if got := paths(layout); !reflect.DeepEqual(got, want) {
			t.Fatalf("areas = %v, want %v", got, want)
		}
		if layout.Manifests[0].Kind != "bazel" || layout.Manifests[0].Path != "MODULE.bazel" {
			t.Fatalf("manifests = %+v", layout.Manifests)
		}
	})

	t.Run("a deep manifest, then top-level directories", func(t *testing.T) {
		files := []string{
			"Makefile", "backend/back/src/pyproject.toml", "backend/back/src/app.py", "backend/back/Dockerfile",
			"frontend/package.json", "frontend/src/main.ts", "frontend/vendors/gantt/package.json", "frontend/vendors/gantt/a.js",
		}
		layout := Detect(files, reader(nil))
		want := []string{"root=.", "src=backend/back/src", "frontend=frontend"}
		if got := paths(layout); !reflect.DeepEqual(got, want) {
			t.Fatalf("areas = %v, want %v", got, want)
		}
		flat := Detect([]string{"go.mod", "ambergo/a.go", "ambergo/b.go", "anubis/c.py", "tools/x/package.json", "main.go"}, reader(nil))
		if got, want := paths(flat), []string{"root=.", "ambergo=ambergo", "anubis=anubis", "x=tools/x"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("areas = %v, want %v", got, want)
		}
	})
}

func TestDetectGivesSupportingAreasTheirRole(t *testing.T) {
	layout := Detect([]string{
		"README.md", "packages/server/package.json", "packages/tests/package.json", "www/package.json",
		"guide/docusaurus.config.ts", "guide/intro.md", "manual/conf.py", "manual/index.rst", "spec/models/a_spec.rb", "src/a.py",
	}, reader(nil))
	want := map[string]contract.AreaRole{
		".": contract.AreaCode, "packages/server": contract.AreaCode, "packages/tests": contract.AreaTests, "www": contract.AreaDocs,
		"guide": contract.AreaDocs, "manual": contract.AreaDocs, "spec": contract.AreaTests, "src": contract.AreaCode,
	}
	for _, area := range layout.Areas {
		if role, ok := want[area.Path]; !ok || area.Role != role {
			t.Errorf("area %s role = %q, want %q", area.Path, area.Role, want[area.Path])
		}
	}
	if len(layout.Areas) != len(want) {
		t.Fatalf("areas = %v", paths(layout))
	}
	for file, tree := range map[string]string{"spec/models/a_spec.rb": "spec", "pkg/a/__tests__/b.ts": "pkg/a/__tests__", "src/a.py": ""} {
		if got, ok := TestTree(file); got != tree || ok != (tree != "") {
			t.Errorf("TestTree(%s) = %q, %v, want %q", file, got, ok, tree)
		}
	}
}
