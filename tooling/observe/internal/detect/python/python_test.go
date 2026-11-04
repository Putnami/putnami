package python

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

const feature = "tooling/observe-python"

// writeTree writes files, keyed by slash path, under a new directory.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func byPath(t *testing.T, result wsproto.ProbeResult) map[string]wsproto.ProbeProject {
	t.Helper()
	projects := map[string]wsproto.ProbeProject{}
	for _, p := range result.Projects {
		projects[p.Path] = p
	}
	return projects
}

func metadata(t *testing.T, p wsproto.ProbeProject) Metadata {
	t.Helper()
	m, err := ReadMetadata(p)
	if err != nil {
		t.Fatalf("metadata of %s: %v", p.Path, err)
	}
	return m
}

func paths(result wsproto.ProbeResult) []string {
	out := make([]string, 0, len(result.Projects))
	for _, p := range result.Projects {
		out = append(out, p.Path)
	}
	return out
}

func noDiagnostics(t *testing.T, result wsproto.ProbeResult) {
	t.Helper()
	if len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics %v", result.Diagnostics)
	}
}

// uvWorkspace is a uv workspace whose api and worker depend on shared; the
// root is a virtual workspace, and tools/ is a project outside it.
var uvWorkspace = map[string]string{
	"pyproject.toml": `[tool.uv.workspace]
members = ["services/*", "libs/shared"]
exclude = ["services/legacy"]

[tool.uv.sources]
shared = { workspace = true }
`,
	"uv.lock":                                "version = 1\n",
	"services/api/pyproject.toml":            "[project]\nname = \"api\"\nversion = \"0.1.0\"\ndependencies = [\"fastapi>=0.115\", \"Shared>=0.1\"]\n",
	"services/api/src/api/main.py":           "app = None\n",
	"services/worker/pyproject.toml":         "[project]\nname = \"worker\"\ndependencies = [\"celery\"]\n\n[dependency-groups]\ndev = [\"shared[test]; python_version >= '3.12'\"]\n",
	"services/legacy/pyproject.toml":         "[project]\nname = \"legacy\"\ndependencies = [\"shared\"]\n",
	"services/notes.md":                      "not a member\n",
	"libs/shared/pyproject.toml":             "[project]\nname = \"shared\"\nversion = \"0.2.0\"\n",
	"tools/pyproject.toml":                   "[project]\nname = \"tools\"\ndependencies = [\"shared\"]\n\n[tool.uv.sources]\nshared = { path = \"../libs/shared\", editable = true }\n",
	".venv/lib/pyproject.toml":               "[project]\nname = \"venv-copy\"\n",
	"node_modules/x/pyproject.toml":          "[project]\nname = \"node-copy\"\n",
	"libs/shared/__pycache__/pyproject.toml": "[project]\nname = \"cache\"\n",
	"docs/pyproject.toml":                    "[tool.black]\nline-length = 100\n",
}

func TestUVWorkspaceMembersAreWorkspaceComponents(t *testing.T) {
	spectest.Proves(t, feature, "python-projects-are-components", "uv-workspace-members-are-workspace-components")
	root := writeTree(t, uvWorkspace)
	result := Detect(root)
	noDiagnostics(t, result)
	if got := paths(result); !reflect.DeepEqual(got, []string{"libs/shared", "services/api", "services/legacy", "services/worker", "tools"}) {
		t.Fatalf("components %v", got)
	}
	projects := byPath(t, result)
	for dir, source := range map[string]string{
		"libs/shared": SourceWorkspace, "services/api": SourceWorkspace, "services/worker": SourceWorkspace,
		"services/legacy": SourceManifest, "tools": SourceManifest,
	} {
		if got := metadata(t, projects[dir]).Source; got != source {
			t.Errorf("%s source %q, want %q", dir, got, source)
		}
	}
	api := projects["services/api"]
	if api.SourceName != "api" || api.SourceFile != "services/api/pyproject.toml" || api.Version != "0.1.0" {
		t.Errorf("api %+v", api)
	}
	// A member without a lockfile of its own takes the workspace's.
	if got := metadata(t, api).Lockfile; got != "uv.lock" {
		t.Errorf("api lockfile %q", got)
	}
	if got := metadata(t, projects["tools"]).Lockfile; got != "" {
		t.Errorf("tools lockfile %q", got)
	}
	if !reflect.DeepEqual(api.WatchedFiles, []string{"services/api/pyproject.toml", "uv.lock"}) {
		t.Errorf("api watched files %v", api.WatchedFiles)
	}
}

func TestARootProjectIsAWorkspaceMember(t *testing.T) {
	spectest.Proves(t, feature, "python-projects-are-components", "uv-workspace-members-are-workspace-components")
	root := writeTree(t, map[string]string{
		"pyproject.toml":              "[project]\nname = \"app\"\ndependencies = [\"lib\"]\n\n[tool.uv.workspace]\nmembers = [\"packages/*/\"]\n\n[tool.uv.sources]\nlib = { workspace = true }\n",
		"packages/lib/pyproject.toml": "[project]\nname = \"lib\"\n",
		"packages/lib/poetry.lock":    "",
	})
	result := Detect(root)
	noDiagnostics(t, result)
	projects := byPath(t, result)
	if got := paths(result); !reflect.DeepEqual(got, []string{".", "packages/lib"}) {
		t.Fatalf("components %v", got)
	}
	if metadata(t, projects["."]).Source != SourceWorkspace || !reflect.DeepEqual(projects["."].Dependencies, []string{"packages/lib"}) {
		t.Errorf("root %+v", projects["."])
	}
	if got := metadata(t, projects["packages/lib"]).Lockfile; got != "packages/lib/poetry.lock" {
		t.Errorf("lib lockfile %q", got)
	}
}

func TestAPyprojectOutsideTheWorkspaceIsAManifestComponent(t *testing.T) {
	spectest.Proves(t, feature, "python-projects-are-components", "a-pyproject-outside-the-workspace-is-a-manifest-component")
	root := writeTree(t, map[string]string{
		"backend/pyproject.toml": "[tool.poetry]\nname = \"backend\"\nversion = \"1.2.0\"\n\n[tool.poetry.dependencies]\npython = \"^3.12\"\n",
		"backend/poetry.lock":    "",
		"scripts/pyproject.toml": "[project]\nname = \"scripts\"\n",
		"pyproject.toml":         "[tool.ruff]\nline-length = 100\n",
	})
	result := Detect(root)
	noDiagnostics(t, result)
	if got := paths(result); !reflect.DeepEqual(got, []string{"backend", "scripts"}) {
		t.Fatalf("components %v", got)
	}
	backend := byPath(t, result)["backend"]
	if backend.SourceName != "backend" || backend.Version != "1.2.0" {
		t.Errorf("backend %+v", backend)
	}
	if m := metadata(t, backend); m.Source != SourceManifest || m.Lockfile != "backend/poetry.lock" {
		t.Errorf("backend metadata %+v", m)
	}
}

func TestAMalformedPyprojectIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "python-projects-are-components", "a-malformed-pyproject-is-a-diagnostic")
	root := writeTree(t, map[string]string{
		"pyproject.toml":        "[tool.uv.workspace]\nmembers = [\"a\", \"[\"]\n",
		"a/pyproject.toml":      "[project\nname = \"a\"\n",
		"b/pyproject.toml":      "[project]\nname = \"b\"\n",
		"broken/pyproject.toml": "name = \"x\"\nname = \"y\"\n",
	})
	result := Detect(root)
	if got := paths(result); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("components %v", got)
	}
	fields := make([]string, 0, len(result.Diagnostics))
	for _, d := range result.Diagnostics {
		if d.Code != CodeManifestInvalid {
			t.Errorf("diagnostic %s: %s", d.Code, d.Message)
		}
		fields = append(fields, d.Field)
	}
	if !reflect.DeepEqual(fields, []string{"pyproject.toml", "a/pyproject.toml", "broken/pyproject.toml"}) {
		t.Errorf("diagnostics %v", result.Diagnostics)
	}
	// A root manifest that does not parse is reported once, with no workspace.
	root = writeTree(t, map[string]string{"pyproject.toml": "[tool.uv.workspace\n", "a/pyproject.toml": "[project]\nname = \"a\"\n"})
	result = Detect(root)
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Field != "pyproject.toml" {
		t.Errorf("diagnostics %v", result.Diagnostics)
	}
	if got := paths(result); !reflect.DeepEqual(got, []string{"a"}) || metadata(t, result.Projects[0]).Source != SourceManifest {
		t.Errorf("components %v", result.Projects)
	}
}

func TestAUVWorkspaceSourceIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "python-project-edges", "a-uv-workspace-source-is-an-edge")
	result := Detect(writeTree(t, uvWorkspace))
	projects := byPath(t, result)
	for dir, want := range map[string][]string{
		// Through the root's sources, for a dependency each declares: api in
		// its dependencies, worker in a dependency group.
		"services/api":    {"libs/shared"},
		"services/worker": {"libs/shared"},
		// legacy is outside the workspace: the root's sources do not apply.
		"services/legacy": nil,
		"libs/shared":     nil,
	} {
		if got := projects[dir].Dependencies; !reflect.DeepEqual(got, want) {
			t.Errorf("%s depends on %v, want %v", dir, got, want)
		}
	}
	// A member's own source names a member by its normalized name.
	root := writeTree(t, map[string]string{
		"pyproject.toml":         "[tool.uv.workspace]\nmembers = [\"*\"]\n",
		"app/pyproject.toml":     "[project]\nname = \"app\"\ndependencies = [\"core-lib\"]\n\n[tool.uv.sources]\nCore_Lib = [\n  { workspace = true, marker = \"sys_platform == 'linux'\" },\n]\n",
		"core/pyproject.toml":    "[project]\nname = \"core.lib\"\n",
		"self/pyproject.toml":    "[project]\nname = \"self\"\ndependencies = [\"self\"]\n\n[tool.uv.sources]\nself = { workspace = true }\n",
		"unknown/pyproject.toml": "[project]\nname = \"unknown\"\ndependencies = [\"missing\"]\n\n[tool.uv.sources]\nmissing = { workspace = true }\n",
		// A source for a dependency the project does not declare is no edge.
		"undeclared/pyproject.toml": "[project]\nname = \"undeclared\"\n\n[tool.uv.sources]\ncore-lib = { workspace = true }\n",
	})
	projects = byPath(t, Detect(root))
	if got := projects["app"].Dependencies; !reflect.DeepEqual(got, []string{"core"}) {
		t.Errorf("app depends on %v", got)
	}
	for _, dir := range []string{"self", "unknown", "undeclared"} {
		if projects[dir].Dependencies != nil {
			t.Errorf("%s depends on %v", dir, projects[dir].Dependencies)
		}

	}
}

func TestALocalPathDependencyIsAnEdge(t *testing.T) {
	spectest.Proves(t, feature, "python-project-edges", "a-local-path-dependency-is-an-edge")
	projects := byPath(t, Detect(writeTree(t, uvWorkspace)))
	if got := projects["tools"].Dependencies; !reflect.DeepEqual(got, []string{"libs/shared"}) {
		t.Errorf("tools depends on %v", got)
	}
	root := writeTree(t, map[string]string{
		"api/pyproject.toml": `[tool.poetry]
name = "api"

[tool.poetry.dependencies]
python = "^3.12"
models = { path = "../models", develop = true }

[tool.poetry.group.test.dependencies]
fixtures = [
  { path = "../fixtures", markers = "sys_platform == 'linux'" },
]

[tool.poetry.dev-dependencies]
lint = { path = "../lint" }
`,
		"models/pyproject.toml":   "[tool.poetry]\nname = \"models\"\n",
		"fixtures/pyproject.toml": "[project]\nname = \"fixtures\"\n",
		"lint/pyproject.toml":     "[project]\nname = \"lint\"\n",
		// The root's sources apply to its members only, through a path
		// relative to the root.
		"pyproject.toml":     "[tool.uv.workspace]\nmembers = [\"svc\"]\n\n[tool.uv.sources]\nmodels = { path = \"models\" }\n",
		"svc/pyproject.toml": "[project]\nname = \"svc\"\ndependencies = [\"models\"]\n",
	})
	projects = byPath(t, Detect(root))
	if got := projects["api"].Dependencies; !reflect.DeepEqual(got, []string{"fixtures", "lint", "models"}) {
		t.Errorf("api depends on %v", got)
	}
	if got := projects["svc"].Dependencies; !reflect.DeepEqual(got, []string{"models"}) {
		t.Errorf("svc depends on %v", got)
	}
}

func TestARegistryDependencyIsNoEdge(t *testing.T) {
	spectest.Proves(t, feature, "python-project-edges", "a-registry-dependency-is-no-edge")
	root := writeTree(t, map[string]string{
		"app/pyproject.toml": `[project]
name = "app"
dependencies = ["requests>=2", "lib", "outside", "absolute", "nothing"]

[tool.uv.sources]
requests = { index = "internal" }
lib = { git = "https://github.com/example/lib", tag = "v1" }
outside = { path = "../../elsewhere" }
absolute = { path = "/opt/lib" }
nothing = { path = "../docs" }

[tool.poetry.dependencies]
django = "^5"
wheel = { path = "../dist/wheel-1.0.whl" }
`,
		"lib/pyproject.toml": "[project]\nname = \"lib\"\n",
		"docs/index.md":      "docs\n",
		"dist/wheel-1.0.whl": "",
	})
	projects := byPath(t, Detect(root))
	if got := projects["app"].Dependencies; got != nil {
		t.Errorf("app depends on %v", got)
	}
}

func TestTheAnswerIsAValidProbeResult(t *testing.T) {
	spectest.Proves(t, feature, "python-probe-answer", "the-answer-is-a-valid-probe-result")
	root := writeTree(t, uvWorkspace)
	candidates, diags := Candidates(root)
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	var request bytes.Buffer
	if err := wsproto.EncodeProbeRequest(&request, wsproto.ProbeRequest{
		Version: wsproto.ProbeProtocolVersion, Extension: ExtensionName, Reason: wsproto.ProbeReasonLoad, Paths: candidates,
	}); err != nil {
		t.Fatal(err)
	}
	var answer bytes.Buffer
	handled, err := wsproto.ServeProbe(wsproto.ProbeControlArgs(), &request, &answer, Answer(root))
	if !handled || err != nil {
		t.Fatalf("served %v: %v", handled, err)
	}
	result, problems := wsproto.ParseAndValidateProbeResult(answer.Bytes())
	if diag.HasErrors(problems) {
		t.Fatalf("the answer does not validate: %v\n%s", problems, answer.Bytes())
	}
	if !reflect.DeepEqual(byPath(t, *result)["services/api"].Dependencies, []string{"libs/shared"}) {
		t.Fatalf("the answer lost the edge: %s", answer.Bytes())
	}
	if strings.Contains(answer.String(), root) {
		t.Errorf("the answer names the checkout directory: %s", answer.Bytes())
	}
}

func TestTheHelpersTolerateEdgeInputs(t *testing.T) {
	if _, diags := Candidates(filepath.Join(t.TempDir(), "missing")); len(diags) != 0 {
		t.Errorf("a missing root %v", diags)
	}
	if got := normalize(" Zope.Interface__Ext "); got != "zope-interface-ext" {
		t.Errorf("normalize = %q", got)
	}
	if m, err := ReadMetadata(wsproto.ProbeProject{}); err != nil || m != (Metadata{}) {
		t.Errorf("empty metadata %+v %v", m, err)
	}
	if _, err := ReadMetadata(wsproto.ProbeProject{Metadata: []byte("{")}); err == nil {
		t.Error("malformed metadata reads")
	}
}

func TestNestedMembersMatchADoubleStarGlob(t *testing.T) {
	spectest.Proves(t, feature, "python-projects-are-components", "uv-workspace-members-are-workspace-components")
	root := writeTree(t, map[string]string{
		"pyproject.toml":                           "[tool.uv.workspace]\nmembers = [\"./packages/**\", \"libs/*\"]\nexclude = [\"packages/**/experimental\"]\n\n[tool.uv.sources]\ncore = { workspace = true }\n",
		"packages/grp/b/pyproject.toml":            "[project]\nname = \"b\"\ndependencies = [\"core\"]\n",
		"packages/grp/experimental/pyproject.toml": "[project]\nname = \"experimental\"\ndependencies = [\"core\"]\n",
		"packages/.hidden/pyproject.toml":          "[project]\nname = \"hidden\"\n",
		"libs/core/pyproject.toml":                 "[project]\nname = \"core\"\n",
		"libs/core/deep/pyproject.toml":            "[project]\nname = \"deep\"\n",
	})
	result := Detect(root)
	noDiagnostics(t, result)
	projects := byPath(t, result)
	for dir, source := range map[string]string{
		"packages/grp/b": SourceWorkspace, "libs/core": SourceWorkspace,
		"packages/grp/experimental": SourceManifest, "libs/core/deep": SourceManifest,
	} {
		if got := metadata(t, projects[dir]).Source; got != source {
			t.Errorf("%s source %q, want %q", dir, got, source)
		}
	}
	if got := projects["packages/grp/b"].Dependencies; !reflect.DeepEqual(got, []string{"libs/core"}) {
		t.Errorf("b depends on %v", got)
	}
	if _, found := projects["packages/.hidden"]; found {
		t.Error("a hidden directory is a member")
	}
}

func TestAnInvalidWorkspacePatternIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "python-projects-are-components", "a-malformed-pyproject-is-a-diagnostic")
	root := writeTree(t, map[string]string{
		"pyproject.toml":   "\uFEFF[tool.uv.workspace]\nmembers = [\"a\", \"../outside\", \"/abs\"]\nexclude = [\"[\"]\n",
		"a/pyproject.toml": "[project]\nname = \"a\"\n",
	})
	result := Detect(root)
	if got := paths(result); !reflect.DeepEqual(got, []string{"a"}) || metadata(t, result.Projects[0]).Source != SourceWorkspace {
		t.Fatalf("components %v", result.Projects)
	}
	if len(result.Diagnostics) != 3 {
		t.Errorf("diagnostics %v", result.Diagnostics)
	}
	for _, d := range result.Diagnostics {
		if d.Code != CodeManifestInvalid || d.Field != "pyproject.toml" {
			t.Errorf("diagnostic %s %s: %s", d.Code, d.Field, d.Message)
		}
	}
	for pattern, want := range map[string]bool{"a/**": true, "./a/": true, "packages/./a": true, "x/../packages/a": true, "": false, ".": false, "a//b": false, "../a": false, "a/../../b": false} {
		if _, ok := patternSegments(pattern); ok != want {
			t.Errorf("patternSegments(%q) = %v, want %v", pattern, ok, want)
		}
	}
}
