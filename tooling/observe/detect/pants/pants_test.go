package pants

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-pants"

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

const pantsTOML = `[GLOBAL]
pants_version = "2.25.0"
backend_packages = ["pants.backend.python"]

[python]
enable_resolves = true
interpreter_constraints = ["==3.12.*"]

[python.resolves]
python-default = "3rdparty/python/default.lock"
tools = "%(buildroot)s/3rdparty/python/tools.lock"
`

// export is a peek export in the shape Pants 2.20 to 2.25 writes: target
// generators and the targets they generate, with their expanded
// dependencies.
const export = `[
  {
    "address": "//:root",
    "target_type": "file",
    "dependencies": [],
    "dependencies_raw": null,
    "source_raw": "pants.toml",
    "sources": ["pants.toml"]
  },
  {
    "address": "3rdparty/python:reqs",
    "target_type": "python_requirements",
    "dependencies": ["3rdparty/python:reqs#fastapi", "3rdparty/python:reqs#pytest"],
    "source_raw": "requirements.txt",
    "sources": ["3rdparty/python/requirements.txt"]
  },
  {
    "address": "3rdparty/python:reqs#fastapi",
    "target_type": "python_requirement",
    "dependencies": [],
    "requirements": ["fastapi==0.115.6"]
  },
  {
    "address": "3rdparty/python/default.lock:_python-default_lockfile",
    "target_type": "_lockfiles",
    "dependencies": [],
    "sources": ["3rdparty/python/default.lock"]
  },
  {
    "address": "src/python/api:lib",
    "target_type": "python_sources",
    "dependencies": ["src/python/api/main.py:lib", "src/python/api/routes/quotes.py:../lib"],
    "dependencies_raw": null,
    "sources": ["src/python/api/main.py", "src/python/api/routes/quotes.py"],
    "sources_raw": ["*.py", "routes/*.py"]
  },
  {
    "address": "src/python/api/main.py:lib",
    "target_type": "python_source",
    "dependencies": ["3rdparty/python:reqs#fastapi", "src/python/api/routes/quotes.py:../lib"],
    "sources": ["src/python/api/main.py"]
  },
  {
    "address": "src/python/api/routes/quotes.py:../lib",
    "target_type": "python_source",
    "dependencies": ["src/python/pricing/pricing.py:lib@resolve=python-default"],
    "sources": ["src/python/api/routes/quotes.py"]
  },
  {
    "address": "src/python/pricing:lib@resolve=python-default",
    "target_type": "python_sources",
    "dependencies": ["src/python/pricing/pricing.py:lib@resolve=python-default"],
    "sources": ["src/python/pricing/pricing.py"]
  },
  {
    "address": "src/python/pricing/pricing.py:lib@resolve=python-default",
    "target_type": "python_source",
    "dependencies": ["3rdparty/python/default.lock:_python-default_lockfile"],
    "sources": ["src/python/pricing/pricing.py"]
  },
  {
    "address": "src/python/pricing/tests:tests",
    "target_type": "python_tests",
    "dependencies": ["src/python/pricing/tests/test_pricing.py"],
    "sources": ["src/python/pricing/tests/test_pricing.py"]
  },
  {
    "address": "src/python/pricing/tests/test_pricing.py",
    "target_type": "python_test",
    "dependencies": ["src/python/pricing/pricing.py:lib@resolve=python-default", "3rdparty/python:reqs#pytest"],
    "sources": ["src/python/pricing/tests/test_pricing.py"]
  }
]
`

func TestPeekTargetsAreGroupedByDirectory(t *testing.T) {
	spectest.Proves(t, feature, "pants-targets-are-components", "peek-targets-are-grouped-by-directory")
	spectest.Proves(t, feature, "pants-inferred-edges", "a-dependency-across-directories-is-an-edge")
	root := writeTree(t, map[string]string{"pants.toml": pantsTOML, DefaultGraphFile: export})
	got := Detect(root, Options{})
	want := []Component{
		{ID: ".", Source: evidence.SourceOrchestrator},
		{ID: "src/python/api", Source: evidence.SourceOrchestrator, DependsOn: []string{"src/python/pricing"}},
		{ID: "src/python/pricing", Source: evidence.SourceOrchestrator},
		{ID: "src/python/pricing/tests", Source: evidence.SourceOrchestrator, DependsOn: []string{"src/python/pricing"}},
	}
	if !reflect.DeepEqual(got.Components, want) {
		t.Errorf("components\n got %+v\nwant %+v", got.Components, want)
	}
	if len(got.Diagnostics) != 0 {
		t.Errorf("diagnostics %+v", got.Diagnostics)
	}
}

// symlinkExport is the export of a repository where st2tests/fixtures/core
// links to contrib/core, so Pants lists the targets of contrib/core under
// both paths, and vendor/ext links outside the repository.
const symlinkExport = `[
  {"address": "contrib/core:reqs#six", "target_type": "python_requirement", "dependencies": ["contrib/core/requirements.txt:reqs"]},
  {"address": "contrib/core/requirements.txt:reqs", "target_type": "_python_requirements_file", "dependencies": [], "sources": ["contrib/core/requirements.txt"]},
  {"address": "contrib/core/actions.py", "target_type": "python_source", "dependencies": ["contrib/core:reqs#six"], "sources": ["contrib/core/actions.py"]},
  {"address": "contrib/core/tests/test_actions.py", "target_type": "python_test", "dependencies": ["contrib/core/actions.py"], "sources": ["contrib/core/tests/test_actions.py"]},
  {"address": "st2tests/fixtures/core:reqs#six", "target_type": "python_requirement", "dependencies": ["st2tests/fixtures/core/requirements.txt:reqs"]},
  {"address": "st2tests/fixtures/core/requirements.txt:reqs", "target_type": "_python_requirements_file", "dependencies": [], "sources": ["st2tests/fixtures/core/requirements.txt"]},
  {"address": "st2tests/fixtures/core/actions.py", "target_type": "python_source", "dependencies": ["st2tests/fixtures/core:reqs#six", "st2tests/base.py"], "sources": ["st2tests/fixtures/core/actions.py"]},
  {"address": "st2tests/fixtures/core/tests/test_actions.py", "target_type": "python_test", "dependencies": ["st2tests/fixtures/core/actions.py"], "sources": ["st2tests/fixtures/core/tests/test_actions.py"]},
  {"address": "st2tests/base.py", "target_type": "python_source", "dependencies": [], "sources": ["st2tests/base.py"]},
  {"address": "st2tests/test_packs.py", "target_type": "python_test", "dependencies": ["st2tests/fixtures/core/actions.py", "st2tests/fixtures/core:reqs#six", "vendor/ext/lib.py"], "sources": ["st2tests/test_packs.py"]},
  {"address": "vendor/ext/lib.py", "target_type": "python_source", "dependencies": [], "sources": ["vendor/ext/lib.py"]}
]`

func TestASymlinkedDirectoryIsItsTargetDirectory(t *testing.T) {
	spectest.Proves(t, feature, "pants-inferred-edges", "a-symlinked-directory-is-its-target-directory")
	root := writeTree(t, map[string]string{
		"pants.toml":                         pantsTOML,
		DefaultGraphFile:                     symlinkExport,
		"contrib/core/BUILD":                 "python_requirements(name=\"reqs\")\npython_sources()\n",
		"contrib/core/requirements.txt":      "six\n",
		"contrib/core/actions.py":            "",
		"contrib/core/tests/BUILD":           "python_tests()\n",
		"contrib/core/tests/test_actions.py": "",
		"st2tests/BUILD":                     "python_sources()\npython_tests(name=\"tests\")\n",
		"st2tests/base.py":                   "",
		"st2tests/test_packs.py":             "",
	})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "lib.py"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"st2tests/fixtures/core": "../../contrib/core", "vendor/ext": outside} {
		file := filepath.Join(root, filepath.FromSlash(link))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, file); err != nil {
			t.Skipf("symbolic links are unavailable: %v", err)
		}
	}
	got := Detect(root, Options{})
	want := []Component{
		{ID: "contrib/core", Source: evidence.SourceOrchestrator},
		{ID: "contrib/core/tests", Source: evidence.SourceOrchestrator, DependsOn: []string{"contrib/core"}},
		{ID: "st2tests", Source: evidence.SourceOrchestrator, DependsOn: []string{"contrib/core"}},
	}
	if !reflect.DeepEqual(got.Components, want) {
		t.Errorf("components\n got %+v\nwant %+v", got.Components, want)
	}
	if !reflect.DeepEqual(got.Tested, []string{"contrib/core/tests", "st2tests"}) {
		t.Errorf("tested = %v", got.Tested)
	}
	// A requirement reached through the link is declared where it resolves.
	if files := got.RequiredFiles["st2tests"]; !reflect.DeepEqual(files, []string{"contrib/core/requirements.txt"}) {
		t.Errorf("st2tests required files = %v", files)
	}
	// A target through a link that leaves the repository is a warning.
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].Severity != diag.Warning || !strings.Contains(got.Diagnostics[0].Message, "vendor/ext/lib.py lies outside the repository") {
		t.Errorf("diagnostics %+v", got.Diagnostics)
	}

	// A link whose resolved directory the export lists no first-party target
	// in, such as one Pants ignores, carries its targets there.
	only := strings.Join([]string{
		`[{"address": "st2tests/fixtures/core/actions.py", "target_type": "python_source", "dependencies": [], "sources": ["st2tests/fixtures/core/actions.py"]},`,
		`{"address": "st2tests/test_packs.py", "target_type": "python_test", "dependencies": ["st2tests/fixtures/core/actions.py"], "sources": ["st2tests/test_packs.py"]}]`,
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, DefaultGraphFile), []byte(only), 0o644); err != nil {
		t.Fatal(err)
	}
	got = Detect(root, Options{})
	want = []Component{
		{ID: "contrib/core", Source: evidence.SourceOrchestrator},
		{ID: "st2tests", Source: evidence.SourceOrchestrator, DependsOn: []string{"contrib/core"}},
	}
	if !reflect.DeepEqual(got.Components, want) {
		t.Errorf("a link whose target is unlisted\n got %+v\nwant %+v", got.Components, want)
	}
}

func TestAThirdPartyTargetIsNoComponent(t *testing.T) {
	spectest.Proves(t, feature, "pants-third-party-targets", "a-third-party-target-is-no-component")
	spectest.Proves(t, feature, "pants-inferred-edges", "a-third-party-dependency-is-no-edge")
	root := writeTree(t, map[string]string{"pants.toml": pantsTOML, DefaultGraphFile: export})
	for _, c := range Detect(root, Options{}).Components {
		if strings.HasPrefix(c.ID, "3rdparty") {
			t.Errorf("component %+v", c)
		}
		for _, dep := range c.DependsOn {
			if strings.HasPrefix(dep, "3rdparty") {
				t.Errorf("%s depends on %s", c.ID, dep)
			}
		}
	}
	// The same directories have no component without the export.
	root = writeTree(t, map[string]string{
		"pants.toml":                 pantsTOML,
		"3rdparty/python/BUILD":      "python_requirements(\n    name=\"reqs\",\n)\n",
		"3rdparty/python/BUILD.lock": "__defaults__(all=dict(resolve=\"python-default\"))\n_lockfiles()\n",
		"3rdparty/uv/BUILD":          "uv_requirements(name=\"reqs\")\n",
		"3rdparty/jvm/BUILD":         "jvm_artifacts(name=\"jvm\")\n",
		"src/python/api/BUILD":       "python_sources()\n",
	})
	got := Detect(root, Options{}).Components
	if want := []Component{{ID: "src/python/api", Source: evidence.SourceWorkspace}}; !reflect.DeepEqual(got, want) {
		t.Errorf("components without the export %+v, want %+v", got, want)
	}
}

func TestAMalformedExportIsADiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "pants-targets-are-components", "a-malformed-export-is-a-diagnostic")
	for name, content := range map[string]string{
		"not JSON":     "pants peek ::\n",
		"an object":    `{"address": "src/python/api:lib"}`,
		"no address":   `[{"target_type": "python_sources"}]`,
		"JSON null":    `null`,
		"nested array": `[[]]`,
	} {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, map[string]string{
				"pants.toml":           pantsTOML,
				DefaultGraphFile:       content,
				"src/python/api/BUILD": "python_sources()\n",
			})
			got := Detect(root, Options{})
			if want := []Component{{ID: "src/python/api", Source: evidence.SourceWorkspace}}; !reflect.DeepEqual(got.Components, want) {
				t.Errorf("components %+v, want the BUILD file directories %+v", got.Components, want)
			}
			if len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != CodeGraphUnreadable || got.Diagnostics[0].Severity != diag.Warning {
				t.Errorf("diagnostics %+v", got.Diagnostics)
			}
		})
	}
}

func TestBuildDirectoriesAreComponentsWithoutAnExport(t *testing.T) {
	spectest.Proves(t, feature, "pants-without-an-export", "build-directories-are-components-without-an-export")
	root := writeTree(t, map[string]string{
		"pants.toml":                          pantsTOML,
		"BUILD.root":                          "__defaults__(all=dict(resolve=\"python-default\"))\n",
		"src/python/api/BUILD":                "python_sources(\n    dependencies=[\"src/python/pricing\"],\n)\n",
		"src/python/pricing/BUILD.pants":      "python_sources()\n\npython_tests(name=\"tests\")\n",
		"src/python/web/BUILD":                "# The web front end.\npex_binary(name=\"web\", entry_point=\"app.py\")\n",
		"src/python/empty/BUILD":              "",
		"dist/export/BUILD":                   "python_sources()\n",
		".venv/lib/BUILD":                     "python_sources()\n",
		"src/python/web/__pycache__/BUILD":    "python_sources()\n",
		"src/python/web/node_modules/x/BUILD": "python_sources()\n",
	})
	got := Detect(root, Options{}).Components
	want := []Component{
		{ID: "src/python/api", Source: evidence.SourceWorkspace},
		{ID: "src/python/pricing", Source: evidence.SourceWorkspace},
		{ID: "src/python/web", Source: evidence.SourceWorkspace},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("components\n got %+v\nwant %+v", got, want)
	}
}

func TestAMissingExportIsAnInfoDiagnostic(t *testing.T) {
	spectest.Proves(t, feature, "pants-without-an-export", "a-missing-export-is-an-info-diagnostic")
	root := writeTree(t, map[string]string{"pants.toml": pantsTOML, "src/python/api/BUILD": "python_sources()\n"})
	for name, want := range map[string]struct {
		opts     Options
		severity diag.Severity
	}{
		"default": {Options{}, diag.Info},
		"named":   {Options{GraphFile: filepath.Join(root, "dist", "graph.json")}, diag.Warning},
	} {
		t.Run(name, func(t *testing.T) {
			got := Detect(root, want.opts).Diagnostics
			if len(got) != 1 || got[0].Code != CodeGraphMissing || got[0].Severity != want.severity || !strings.Contains(got[0].Message, ExportCommand) {
				t.Errorf("diagnostics %+v", got)
			}
		})
	}
	// A repository without pants.toml is no Pants repository: no component,
	// no finding.
	plain := writeTree(t, map[string]string{"src/BUILD": "python_sources()\n"})
	if got := Detect(plain, Options{}); len(got.Components) != 0 || len(got.Diagnostics) != 0 || len(got.Lockfiles) != 0 {
		t.Errorf("detection without pants.toml %+v", got)
	}
}

func TestThePantsGraphOptionNamesTheExport(t *testing.T) {
	spectest.Proves(t, feature, "pants-graph-flag", "the-pants-graph-flag-names-the-export")
	root := writeTree(t, map[string]string{"pants.toml": pantsTOML, "dist/peek.json": export})
	for _, file := range []string{"dist/peek.json", filepath.Join(root, "dist", "peek.json")} {
		got := Detect(root, Options{GraphFile: file})
		if len(got.Components) != 4 || got.Components[1].Source != evidence.SourceOrchestrator || len(got.Diagnostics) != 0 {
			t.Errorf("%s: detection %+v", file, got)
		}
	}
	// The flag in a repository without pants.toml reads nothing, and says so.
	plain := writeTree(t, map[string]string{"dist/peek.json": export})
	got := Detect(plain, Options{GraphFile: "dist/peek.json"})
	if len(got.Components) != 0 || len(got.Diagnostics) != 1 || got.Diagnostics[0].Code != CodeGraphUnused || got.Diagnostics[0].Severity != diag.Warning {
		t.Errorf("the flag without pants.toml: %+v", got)
	}
}

func TestAResolveLockfileIsAPyPILockfile(t *testing.T) {
	spectest.Proves(t, feature, "pants-resolve-lockfiles", "a-resolve-lockfile-is-a-pypi-lockfile")
	root := writeTree(t, map[string]string{"pants.toml": pantsTOML, DefaultGraphFile: export})
	got := Detect(root, Options{})
	if want := []string{"3rdparty/python/default.lock", "3rdparty/python/tools.lock"}; !reflect.DeepEqual(got.Lockfiles, want) {
		t.Errorf("lockfiles %v, want %v", got.Lockfiles, want)
	}
	if !got.IsLockfile("3rdparty/python/default.lock") || got.IsLockfile("3rdparty/python/requirements.txt") {
		t.Errorf("IsLockfile")
	}
	invalid := writeTree(t, map[string]string{"pants.toml": "[python.resolves]\npython-default = \"../outside.lock\"\n"})
	if got := Detect(invalid, Options{}); len(got.Lockfiles) != 0 || !hasCode(got.Diagnostics, CodeConfigInvalid) {
		t.Errorf("a lockfile outside the repository: %+v", got)
	}
	broken := writeTree(t, map[string]string{"pants.toml": "[python\n"})
	if got := Detect(broken, Options{}); !hasCode(got.Diagnostics, CodeConfigInvalid) || !hasCode(got.Diagnostics, CodeGraphMissing) {
		t.Errorf("a pants.toml that does not parse: %+v", got)
	}
}

func TestAddressDirectories(t *testing.T) {
	g := graph{root: writeTree(t, map[string]string{"src/a/b/c.py": "", "requirements.txt": ""}), targets: map[string]target{}}
	for address, want := range map[string]string{
		"//:root":                          ".",
		"//:reqs#fastapi":                  ".",
		"//requirements.txt:reqs":          ".",
		"src/a":                            "src/a",
		"src/a:lib":                        "src/a",
		"src/a:lib@resolve=b":              "src/a",
		"src/a@resolve=b":                  "src/a",
		"src/a:reqs#github.com/x/y":        "src/a",
		"src/a/b/c.py":                     "src/a/b",
		"src/a/b/c.py:lib":                 "src/a/b",
		"src/a/b/c.py:../lib":              "src/a",
		"src/a/b/c.py:../../src":           "src",
		"src/a/b/c.py:../lib@resolve=b":    "src/a",
		"src/a/b/c.py:../lib#gen@k=v,l=w":  "src/a",
		"src/a/b/missing.py:../lib":        "src/a",
		"src/a/b/missing.py":               "src/a/b/missing.py",
		"src/a/b/c.py:../../../../outside": "",
	} {
		got, ok := g.dir(address)
		if want == "" {
			if ok {
				t.Errorf("%s: %q, want none", address, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%s: %q, want %q", address, got, want)
		}
	}
}

func hasCode(diags []diag.Diagnostic, code string) bool {
	for _, d := range diags {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestTestedNamesTheComponentsWithATestTarget(t *testing.T) {
	root := writeTree(t, map[string]string{"pants.toml": pantsTOML, DefaultGraphFile: export})
	got := Detect(root, Options{})
	if want := []string{"src/python/pricing/tests"}; !reflect.DeepEqual(got.Tested, want) {
		t.Errorf("tested from the export = %v, want %v", got.Tested, want)
	}
	if !got.IsTested("src/python/pricing/tests") || got.IsTested("src/python/api") {
		t.Errorf("IsTested disagrees with Tested %v", got.Tested)
	}
	root = writeTree(t, map[string]string{
		"pants.toml":                 pantsTOML,
		"src/python/api/BUILD":       "python_sources()\n",
		"src/python/pricing/BUILD":   "python_sources()\n\npython_tests(name=\"tests\")\n",
		"src/go/money/BUILD":         "go_package()\n",
		"src/go/money/money_test.go": "package money\n",
		"src/go/fx/BUILD":            "go_package()\n",
		"src/jvm/ledger/BUILD":       "junit_tests()\n",
	})
	if want := []string{"src/go/money", "src/jvm/ledger", "src/python/pricing"}; !reflect.DeepEqual(Detect(root, Options{}).Tested, want) {
		t.Errorf("tested from the BUILD files = %v, want %v", Detect(root, Options{}).Tested, want)
	}
}

func TestRequiresNamesTheThirdPartyDirectoriesOfEachComponent(t *testing.T) {
	root := writeTree(t, map[string]string{"pants.toml": pantsTOML, DefaultGraphFile: export})
	want := map[string][]string{"src/python/api": {"3rdparty/python"}, "src/python/pricing": {"3rdparty/python"}}
	if got := Detect(root, Options{}).Requires; !reflect.DeepEqual(got, want) {
		t.Errorf("requires = %v, want %v", got, want)
	}
}
