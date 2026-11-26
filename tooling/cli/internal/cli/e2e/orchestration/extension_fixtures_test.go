package orchestration

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures of the extensions whose cases follow the language cases: each
// is one small project the extension serves, the least its smallest command
// that runs its jobs needs.

// shellFiles is a project the sample shell extension builds and tests with
// make.
var shellFiles = map[string]string{
	".gitignore":             ".putnami\nbuild\n",
	"putnami.workspace.json": `{"name": "counts", "extensions": ["/.ext/shell"], "includes": ["lib"]}` + "\n",
	"lib/putnami.json":       `{"name": "lib", "extensions": ["sample/shell-extension"]}` + "\n",
	"lib/Makefile":           "release:\n\tmkdir -p build\n\ttouch build/lib\n\ntest:\n\ttrue\n",
	"lib/lib.c":              "int add(int a, int b) { return a + b; }\n",
}

// webFiles is a TypeScript library that applies the framework extensions,
// whose hooks run before its build.
var webFiles = map[string]string{
	".gitignore":   ".putnami\nnode_modules\ndist\n*.tsbuildinfo\n.gen\n",
	"package.json": `{"name": "counts", "private": true, "packageManager": "bun@1.4.5", "workspaces": ["lib"]}` + "\n",
	"putnami.workspace.json": `{"name": "counts", "extensions": ["/.ext/typescript", "/.ext/dev", "/.ext/application",` +
		` "/.ext/web"], "includes": ["lib"]}` + "\n",
	"lib/putnami.json": `{"name": "lib", "extensions": ["@putnami/typescript", "@putnami/application",` +
		` "@putnami/web"]}` + "\n",
	"lib/package.json": `{"name": "lib", "type": "module", "main": "dist/index.js", "types": "dist/index.d.ts",` +
		` "scripts": {"build": "tsc -b"}}` + "\n",
	"lib/tsconfig.json": `{"compilerOptions": {"composite": true, "rootDir": "src", "outDir": "dist",` +
		` "module": "nodenext", "target": "es2022", "strict": true, "types": []}, "include": ["src"]}` + "\n",
	"lib/src/index.ts": "export const add = (a: number, b: number): number => a + b;\n",
}

// cloudFiles is a project @putnami/cloud validates: it declares no Config
// member, so its validation reads nothing remote.
var cloudFiles = map[string]string{
	".gitignore":             ".putnami\n",
	"putnami.workspace.json": `{"name": "counts", "extensions": ["/.ext/cloud"], "includes": ["lib"]}` + "\n",
	"lib/putnami.json":       `{"name": "lib", "extensions": ["@putnami/cloud"]}` + "\n",
}

// integrationFiles is a workspace that loads the integration at /.ext/<name>
// with @putnami/dev, its dependency, plus the files of its tool.
func integrationFiles(name string, files map[string]string) map[string]string {
	out := map[string]string{
		".gitignore":             ".putnami\nnode_modules\n",
		"putnami.workspace.json": `{"name": "counts", "extensions": ["/.ext/` + name + `", "/.ext/dev"]}` + "\n",
	}
	for rel, content := range files {
		out[rel] = content
	}
	return out
}

// bazelFiles is a Bazel module with one package, and the query export a CI
// writes, so the graph is read without bazel.
var bazelFiles = integrationFiles("bazel", map[string]string{
	"MODULE.bazel":     "module(name = \"counts\")\n",
	"lib/BUILD.bazel":  "sh_library(name = \"lib\", srcs = [\"lib.sh\"])\n",
	"lib/lib.sh":       "echo lib\n",
	"bazel-graph.json": `{"type":"RULE","rule":{"name":"//lib:lib","ruleClass":"sh_library","location":"/counts/lib/BUILD.bazel:1:11","ruleInput":["//lib:lib.sh"]}}` + "\n" + `{"type":"SOURCE_FILE","sourceFile":{"name":"//lib:lib.sh","location":"/counts/lib/lib.sh:1:1"}}` + "\n",
})

// pantsFiles is a Pants repository with one package, and the export a CI
// writes, so the graph is read without pants.
var pantsFiles = integrationFiles("pants", map[string]string{
	"pants.toml":       "[GLOBAL]\npants_version = \"2.25.0\"\nbackend_packages = [\"pants.backend.python\"]\n\n[source]\nroot_patterns = [\"/src\"]\n",
	"src/lib/BUILD":    "python_sources()\n",
	"src/lib/lib.py":   "def add(a, b):\n    return a + b\n",
	"pants-graph.json": `[{"address": "src/lib/lib.py", "target_type": "python_source", "dependencies": [], "sources": ["src/lib/lib.py"]}, {"address": "src/lib:lib", "target_type": "python_sources", "dependencies": ["src/lib/lib.py"], "sources": ["src/lib/lib.py"]}]` + "\n",
})

// moonFiles is a moon workspace with one project, and the project graph a CI
// writes, so the graph is read without moon.
var moonFiles = integrationFiles("moon", map[string]string{
	".moon/workspace.yml": "projects:\n  sources:\n    lib: 'lib'\n",
	"lib/moon.yml":        "language: 'bash'\ntasks:\n  build:\n    command: 'true'\n",
	"lib/lib.sh":          "echo lib\n",
	"moon-graph.json":     `{"graph": {"nodes": [{"id": "lib", "source": "lib"}], "node_holes": [], "edge_property": "directed", "edges": []}}` + "\n",
})

// turboFiles is a Turborepo with one package.
var turboFiles = integrationFiles("turbo", map[string]string{
	"package.json":              `{"name": "counts", "private": true, "packageManager": "bun@1.4.5", "workspaces": ["packages/*"]}` + "\n",
	"turbo.json":                `{"tasks": {"build": {"outputs": ["dist/**"]}}}` + "\n",
	"packages/lib/package.json": `{"name": "lib", "version": "1.0.0", "scripts": {"build": "true"}}` + "\n",
})

// nxFiles is an Nx workspace with one project.
var nxFiles = integrationFiles("nx", map[string]string{
	"nx.json":                   `{"neverConnectToCloud": true}` + "\n",
	"package.json":              `{"name": "counts", "private": true, "workspaces": ["packages/*"]}` + "\n",
	"packages/lib/package.json": `{"name": "lib", "version": "1.0.0", "scripts": {"build": "true"}}` + "\n",
	"packages/lib/project.json": `{"name": "lib", "targets": {"build": {"command": "true"}}}` + "\n",
})

// rushFiles is a Rush repository with one project.
var rushFiles = integrationFiles("rush", map[string]string{
	"rush.json": `{"rushVersion": "5.140.0", "pnpmVersion": "9.15.0", "projectFolderMinDepth": 2,` +
		` "projectFolderMaxDepth": 2, "projects": [{"packageName": "lib", "projectFolder": "libs/lib"}]}` + "\n",
	"libs/lib/package.json": `{"name": "lib", "version": "1.0.0", "scripts": {"build": "true"}}` + "\n",
})

// skippedExtensions are the extensions built here that hold no count, each
// with the reason. An extension of this repository is either the subject of
// an extension case or listed here (TestEveryExtensionBuiltHereIsCounted).
//
// Contributor CI provisions Go (actions/setup-go), Bun (oven-sh/setup-bun),
// uv (astral-sh/setup-uv), the Go lint tools of its restored cache and the
// toolchains putnami.lock.json pins, which its install installs; it fetches no
// language package offline: a case whose jobs need another tool never runs
// there.
var skippedExtensions = map[string]string{
	"@putnami/python": "its lint and test tasks resolve ruff and pytest with uv from the package index; " +
		"no Python tool is provisioned offline in Contributor CI",
	"@putnami/java":   "its tasks run Maven or Gradle on a JDK, which Contributor CI does not provision",
	"@putnami/dotnet": "its tasks run the .NET SDK, which Contributor CI does not provision",
	"@putnami/dart":   "its tasks run the Dart SDK, which Contributor CI does not provision",
	"@putnami/swift":  "its tasks run the Swift toolchain, which Contributor CI does not provision",
	"@putnami/ruby":   "its tasks run Ruby and Bundler, which Contributor CI does not provision",
	"@putnami/php": "its lint and test tasks run PHPStan and PHPUnit from vendor/bin, which Composer " +
		"fetches from Packagist; Contributor CI provisions no Composer package offline",
	"@putnami/analytics": "its manifest names its extension dependencies under no CLI contract, so a " +
		"workspace that loads it from this tree skips it",
	"@putnami/observe": "its one command prints the evidence document it writes in place of the run's " +
		"result, so the run's orchestration report is not read",
	"@putnami/scaffold": "its task compiles its runtime with the go.work of this repository, which a " +
		"workspace outside this repository does not have",
	"@putnami/contributor": "its commands print their own output in place of the run's result, so the " +
		"run's orchestration report is not read",
	"@putnami/dev": "its commands are interactive and print no run report, and it declares no hook or " +
		"provider: the fixtures that load it hold no process it starts",
	"@putnami/events": "it declares no command, task or hook: a run starts nothing of it",
	"@putnami/github-collaboration": "it is a collaboration provider with no command or task: it answers " +
		"provider requests against GitHub, which no run plans",
	"@putnami/gitlab-collaboration": "it is a collaboration provider with no command or task: it answers " +
		"provider requests against GitLab, which no run plans",
	"@putnami/jira-collaboration": "it is a collaboration provider with no command or task: it answers " +
		"provider requests against Jira, which no run plans",
	"@putnami/linear-collaboration": "it is a collaboration provider with no command or task: it answers " +
		"provider requests against Linear, which no run plans",
	"@putnami/local-collaboration": "it is a collaboration provider with no command or task: it answers " +
		"provider requests, which no run plans",
	"@putnami/memory-store": "it is a memory provider with no command or task: it answers provider " +
		"requests, which no run plans",
}

// TestEveryExtensionBuiltHereIsCounted fails when an extension of this
// repository is neither the subject of an extension case nor listed with the
// reason it holds no count, and when a case or a reason names an extension
// this repository no longer builds.
func TestEveryExtensionBuiltHereIsCounted(t *testing.T) {
	built := map[string]bool{}
	err := filepath.WalkDir(repoRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch name := entry.Name(); {
			case path != repoRoot && strings.HasPrefix(name, "."), name == "node_modules", name == "testdata",
				name == "dist", name == "target":
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != "putnami.extension.json" {
			return nil
		}
		var manifest struct{ Name string }
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		built[manifest.Name] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	counted := map[string]string{}
	for _, tc := range extensionCases {
		for _, name := range tc.extensions {
			counted[name] = tc.name
		}
	}
	for name := range built {
		_, skipped := skippedExtensions[name]
		if counted[name] == "" && !skipped {
			t.Errorf("%s is built here and holds no count: add an extension case, or its reason to skippedExtensions", name)
		}
		if counted[name] != "" && skipped {
			t.Errorf("%s holds a count in the %s case and is listed in skippedExtensions", name, counted[name])
		}
	}
	for name, by := range counted {
		if !built[name] {
			t.Errorf("the %s case counts %s, which this repository does not build", by, name)
		}
	}
	for name := range skippedExtensions {
		if !built[name] {
			t.Errorf("skippedExtensions lists %s, which this repository does not build", name)
		}
	}
}
