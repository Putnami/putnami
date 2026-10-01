package lint

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/typescript/extension/internal/parse"
)

// writeFixtureFile writes content at root/relative, creating parent directories.
func writeFixtureFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFixtureFile(t *testing.T, root, relative string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// nestedProjectWorkspace builds a workspace whose `app` project holds a nested
// Go project at clients/go, a nested project under the hidden .tools directory
// and a plain source directory at clients/ts, next to a sibling `lib` project.
// The node_modules and dist directories of `app` each hold a manifest the scan
// never reads, as the input hasher never reads them.
func nestedProjectWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFixtureFile(t, root, "biome.json", "{\"root\":true}\n")
	writeFixtureFile(t, root, "app/putnami.json", "{\"name\":\"app\"}\n")
	writeFixtureFile(t, root, "app/package.json", "{\"name\":\"app\"}\n")
	writeFixtureFile(t, root, "app/README.md", "# app\n")
	writeFixtureFile(t, root, "app/src/index.ts", "const a   = 1;\nexport { a };\n")
	writeFixtureFile(t, root, "app/test/index.test.ts", "const b   = 1;\nexport { b };\n")
	writeFixtureFile(t, root, "app/clients/ts/src/client.ts", "const c   = 1;\nexport { c };\n")
	writeFixtureFile(t, root, "app/clients/go/putnami.json", "{\"name\":\"go-client\",\"tags\":[\n\"go\"\n]}\n")
	writeFixtureFile(t, root, "app/clients/go/client.putnami.json", "{\"a\":[\n1\n]}\n")
	writeFixtureFile(t, root, "app/clients/go/tools/gen.ts", "const d   = 1;\nexport { d };\n")
	writeFixtureFile(t, root, "app/clients/go/tools/deeper/putnami.json", "{\"name\":\"deeper\"}\n")
	writeFixtureFile(t, root, "app/node_modules/dep/putnami.json", "{\"name\":\"dep\"}\n")
	writeFixtureFile(t, root, "app/dist/putnami.json", "{\"name\":\"built\"}\n")
	writeFixtureFile(t, root, "app/.tools/run.ts", "const r   = 1;\nexport { r };\n")
	writeFixtureFile(t, root, "app/.tools/sub/putnami.json", "{\"name\":\"tool\",\"tags\":[\n\"ts\"\n]}\n")
	writeFixtureFile(t, root, "lib/src/index.ts", "const l   = 1;\nexport { l };\n")
	return root
}

// nestedProjectFiles are the files of the nested projects of app: clients/go
// and the one under the hidden .tools directory.
var nestedProjectFiles = []string{
	"app/.tools/sub/putnami.json",
	"app/clients/go/putnami.json",
	"app/clients/go/client.putnami.json",
	"app/clients/go/tools/gen.ts",
	"app/clients/go/tools/deeper/putnami.json",
}

// appOwnTargets is the cover of the app project in nestedProjectWorkspace,
// relative to the workspace root, in directory order.
var appOwnTargets = []string{
	"app/.tools/run.ts",
	"app/README.md",
	"app/clients/ts",
	"app/dist",
	"app/node_modules",
	"app/package.json",
	"app/putnami.json",
	"app/src",
	"app/test",
}

func TestBiomeTargets_SkipNestedProjectRoots(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "lint-covers-the-project", "a-nested-project-is-never-linted-by-its-parent")
	root := nestedProjectWorkspace(t)

	runDir, targets, err := biomeTargets("", []string{filepath.Join(root, "app")}, filepath.Join(root, "biome.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runDir != root {
		t.Fatalf("run dir = %q, want %q", runDir, root)
	}
	if !reflect.DeepEqual(targets, appOwnTargets) {
		t.Fatalf("targets = %v, want %v", targets, appOwnTargets)
	}
}

func TestBiomeTargets_KeepOneTargetWithoutNestedProject(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "app/putnami.json", "{}\n")
	writeFixtureFile(t, root, "app/src/index.ts", "export const a = 1;\n")
	writeFixtureFile(t, root, "app/node_modules/dep/putnami.json", "{}\n")
	writeFixtureFile(t, root, "app/dist/putnami.json", "{}\n")
	app := filepath.Join(root, "app")

	runDir, targets, err := biomeTargets("", []string{app}, filepath.Join(root, "biome.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runDir != root || !reflect.DeepEqual(targets, []string{"app"}) {
		t.Fatalf("biomeTargets = %q %v, want %q [app]", runDir, targets, root)
	}

	runDir, targets, err = biomeTargets("", []string{app}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if runDir != app || !reflect.DeepEqual(targets, []string{"."}) {
		t.Fatalf("biomeTargets with an external config = %q %v, want %q [.]", runDir, targets, app)
	}
}

func TestBiomeTargets_ExternalConfigCoverIsProjectRelative(t *testing.T) {
	root := nestedProjectWorkspace(t)
	app := filepath.Join(root, "app")

	runDir, targets, err := biomeTargets("", []string{app}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(appOwnTargets))
	for _, target := range appOwnTargets {
		want = append(want, strings.TrimPrefix(target, "app/"))
	}
	if runDir != app || !reflect.DeepEqual(targets, want) {
		t.Fatalf("biomeTargets = %q %v, want %q %v", runDir, targets, app, want)
	}
}

func TestBiomeTargets_BatchCoversEachProject(t *testing.T) {
	root := nestedProjectWorkspace(t)

	runDir, targets, err := biomeTargets(
		root,
		[]string{filepath.Join(root, "app"), filepath.Join(root, "lib")},
		filepath.Join(root, "biome.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]string(nil), appOwnTargets...), "lib")
	if runDir != root || !reflect.DeepEqual(targets, want) {
		t.Fatalf("biomeTargets = %q %v, want %q %v", runDir, targets, root, want)
	}
}

func TestBiomeTargets_UnreadableProjectFails(t *testing.T) {
	root := t.TempDir()
	if _, _, err := biomeTargets("", []string{filepath.Join(root, "missing")}, filepath.Join(root, "biome.json")); err == nil {
		t.Fatal("expected an error for a project directory that cannot be read")
	}
}

func TestBiomeTargets_UnreadableSubdirectoryIsPassedWhole(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permissions do not deny reads here")
	}
	root := t.TempDir()
	writeFixtureFile(t, root, "app/putnami.json", "{}\n")
	writeFixtureFile(t, root, "app/sealed/index.ts", "export const s = 1;\n")
	writeFixtureFile(t, root, "app/clients/go/putnami.json", "{}\n")
	sealed := filepath.Join(root, "app", "sealed")
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })

	// A git-ignored bind mount owned by another user is the usual case: Biome
	// skipped it before the scan existed, and it must still receive it whole.
	_, targets, err := biomeTargets("", []string{filepath.Join(root, "app")}, filepath.Join(root, "biome.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app/putnami.json", "app/sealed"}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("targets = %v, want %v", targets, want)
	}
}

func TestBiomeTargets_SymlinksAreNamedNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privilege on Windows")
	}
	root := t.TempDir()
	writeFixtureFile(t, root, "app/putnami.json", "{}\n")
	writeFixtureFile(t, root, "app/clients/go/putnami.json", "{}\n")
	writeFixtureFile(t, root, "other/index.ts", "export const o = 1;\n")
	if err := os.Symlink(filepath.Join(root, "other"), filepath.Join(root, "app", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "app", "dangling")); err != nil {
		t.Fatal(err)
	}

	_, targets, err := biomeTargets("", []string{filepath.Join(root, "app")}, filepath.Join(root, "biome.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app/linked", "app/putnami.json"}
	if !reflect.DeepEqual(targets, want) {
		t.Fatalf("targets = %v, want %v", targets, want)
	}
}

func TestBiomeCommands_SkipBiomeWhenEveryFileIsNested(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "app/clients/go/putnami.json", "{}\n")
	app := filepath.Join(root, "app")
	config := filepath.Join(root, "biome.json")

	restore := SetExecRunForTesting(func(_ string, args []string, _ ...exec.Option) (*exec.Result, error) {
		t.Fatalf("Biome ran with %v although the project owns no file", args)
		return nil, nil
	})
	defer restore()

	commands := []struct {
		name string
		run  func() (parse.BiomeReport, bool, error)
	}{
		{"format", func() (parse.BiomeReport, bool, error) { return Format("biome", app, config, true) }},
		{"lint", func() (parse.BiomeReport, bool, error) { return Check("biome", app, config, true, 0, "") }},
		{"combined", func() (parse.BiomeReport, bool, error) { return CheckAll("biome", app, config, 0, "") }},
	}
	for _, command := range commands {
		report, ok, err := command.run()
		if err != nil || !ok || len(report.Diagnostics) != 0 {
			t.Fatalf("%s = %v %v %v, want an empty success", command.name, report, ok, err)
		}
	}
}

func TestBiomeCommands_FailWhenProjectsDoNotShareRoot(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	_, _, err := FormatProjects("biome", []string{filepath.Join(workspace, "a"), outside}, workspace, "", false)
	if err == nil || !strings.Contains(err.Error(), "biome format: selected projects do not share workspace root") {
		t.Fatalf("FormatProjects error = %v, want the shared-root error", err)
	}
}

// rewritingBiome is a stand-in Biome that walks every path argument the way
// Biome traverses a directory and rewrites each file it reaches, then prints an
// empty report.
func rewritingBiome(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in Biome is a POSIX shell script")
	}
	script := filepath.Join(t.TempDir(), "biome")
	content := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    format|lint|check|--*) ;;
    *) find "$arg" -type f -exec sh -c 'printf "rewritten\n" > "$1"' _ {} \; ;;
  esac
done
echo '{"diagnostics":[],"command":"","summary":{"errors":0,"warnings":0,"infos":0,"changed":0,"unchanged":0,"skipped":0}}'
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestBiomeCommands_NeverRewriteANestedProject(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "lint-covers-the-project", "a-nested-project-is-never-linted-by-its-parent")
	appOwnFiles := []string{"app/.tools/run.ts", "app/src/index.ts", "app/test/index.test.ts", "app/clients/ts/src/client.ts", "app/package.json"}
	batchOwnFiles := append(append([]string(nil), appOwnFiles...), "lib/src/index.ts")
	commands := []struct {
		name     string
		ownFiles []string
		run      func(bin, workspace string) error
	}{
		{"format", appOwnFiles, func(bin, workspace string) error {
			_, _, err := Format(bin, filepath.Join(workspace, "app"), filepath.Join(workspace, "biome.json"), true)
			return err
		}},
		{"lint", appOwnFiles, func(bin, workspace string) error {
			_, _, err := Check(bin, filepath.Join(workspace, "app"), filepath.Join(workspace, "biome.json"), true, 0, "")
			return err
		}},
		{"combined", appOwnFiles, func(bin, workspace string) error {
			_, _, err := CheckAll(bin, filepath.Join(workspace, "app"), filepath.Join(workspace, "biome.json"), 0, "")
			return err
		}},
		{"batch format", batchOwnFiles, func(bin, workspace string) error {
			projects := []string{filepath.Join(workspace, "app"), filepath.Join(workspace, "lib")}
			_, _, err := FormatProjects(bin, projects, workspace, filepath.Join(workspace, "biome.json"), true)
			return err
		}},
		{"batch lint", batchOwnFiles, func(bin, workspace string) error {
			projects := []string{filepath.Join(workspace, "app"), filepath.Join(workspace, "lib")}
			_, _, err := CheckProjects(bin, projects, workspace, filepath.Join(workspace, "biome.json"), true, -1, "")
			return err
		}},
		{"batch combined", batchOwnFiles, func(bin, workspace string) error {
			projects := []string{filepath.Join(workspace, "app"), filepath.Join(workspace, "lib")}
			_, _, err := CheckAllProjects(bin, projects, workspace, filepath.Join(workspace, "biome.json"), -1, "")
			return err
		}},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			workspace := nestedProjectWorkspace(t)
			before := make(map[string]string, len(nestedProjectFiles))
			for _, file := range nestedProjectFiles {
				before[file] = readFixtureFile(t, workspace, file)
			}
			if err := command.run(rewritingBiome(t), workspace); err != nil {
				t.Fatal(err)
			}
			for _, file := range nestedProjectFiles {
				if after := readFixtureFile(t, workspace, file); after != before[file] {
					t.Errorf("rewrote nested project file %s: %q", file, after)
				}
			}
			for _, file := range command.ownFiles {
				if got := readFixtureFile(t, workspace, file); got != "rewritten\n" {
					t.Errorf("did not reach the project's own file %s: %q", file, got)
				}
			}
		})
	}
}

// installedBiome returns the Biome binary installed above the test directory,
// or skips the test when none is installed.
func installedBiome(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		candidate := filepath.Join(dir, "node_modules", ".bin", "biome")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no Biome installed above the test directory")
		}
		dir = parent
	}
}

func TestBiome_FormatsTheProjectButNotItsNestedProject(t *testing.T) {
	biome := installedBiome(t)
	workspace := nestedProjectWorkspace(t)
	app := filepath.Join(workspace, "app")
	config := filepath.Join(workspace, "biome.json")
	before := make(map[string]string, len(nestedProjectFiles))
	for _, file := range nestedProjectFiles {
		before[file] = readFixtureFile(t, workspace, file)
	}

	if report, ok, err := Format(biome, app, config, true); err != nil || !ok {
		t.Fatalf("Format = %v %v %v, want success", report, ok, err)
	}
	if got := readFixtureFile(t, workspace, "app/src/index.ts"); got != "const a = 1;\nexport { a };\n" {
		t.Fatalf("Format left the project's own source unformatted: %q", got)
	}
	for _, file := range nestedProjectFiles {
		if after := readFixtureFile(t, workspace, file); after != before[file] {
			t.Fatalf("Format rewrote nested project file %s: %q", file, after)
		}
	}

	report, ok, err := CheckAll(biome, app, config, -1, "")
	if err != nil || !ok {
		t.Fatalf("CheckAll = %v %v %v, want success once the project is formatted", report, ok, err)
	}
	for _, diagnostic := range report.Diagnostics {
		if strings.Contains(diagnostic.File, "clients/go") {
			t.Fatalf("CheckAll reported a nested project file: %+v", diagnostic)
		}
	}
}
