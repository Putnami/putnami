package pkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	proto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/typescript/extension/internal/toolchain"
)

// archiveFixtureManifest is an extension whose task and hook run Bun on entry
// points named without their extension, as @putnami/web's do.
const archiveFixtureManifest = `{
	"name": "@acme/greeter",
	"hooks": {
		"preBuild": {"kind": "command", "command": "bun", "args": ["run", "{extensionRoot}/bin/generate"], "cwd": "{projectRoot}", "timeoutMs": 60000},
		"onInstall": {"kind": "command", "command": "bun", "args": ["run", "{extensionRoot}/bin/setup"], "cwd": "{workspaceRoot}", "timeoutMs": 60000}
	},
	"commands": {
		"greet": {"description": "Greet", "run": [{"id": "greet", "task": "greet-exec"}]}
	},
	"tasks": {
		"greet-exec": {"kind": "command", "command": "bun", "args": ["run", "{extensionRoot}/bin/greet", "world"], "cwd": "{projectRoot}", "timeoutMs": 60000, "cache": {"enabled": false}}
	}
}`

// writeArchiveFixture writes a project whose entry point imports its own
// source and a package installed in node_modules, the two things an installed
// archive has no node_modules to resolve.
func writeArchiveFixture(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		proto.ManifestFilename:                   manifest,
		"package.json":                           `{"name": "@acme/greeter", "version": "0.0.0"}`,
		"README.md":                              "# Greeter\n",
		"AI.md":                                  "# Greeter API\n",
		"bin/greet.ts":                           "import { greeting } from '../src/greeting';\nconsole.log(greeting(process.argv[2] ?? 'nobody'));\n",
		"bin/generate.ts":                        "console.log('generated');\n",
		"bin/setup.ts":                           "console.log('set up');\n",
		"src/greeting.ts":                        "import { exclaim } from 'acme-exclaim';\nexport const greeting = (name: string): string => exclaim(`hello ${name}`);\n",
		"node_modules/acme-exclaim/package.json": `{"name": "acme-exclaim", "version": "1.0.0", "main": "index.js"}`,
		"node_modules/acme-exclaim/index.js":     "exports.exclaim = (text) => text + '!';\n",
	}
	for name, content := range files {
		file := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func archiveTestBun(t *testing.T) string {
	t.Helper()
	bun, err := toolchain.ResolveBun()
	if err != nil {
		t.Skip("bun is not installed")
	}
	return bun
}

func packageFixture(t *testing.T, bun, projectRoot string) (*ExtensionArchiveResult, string) {
	t.Helper()
	outputDir := filepath.Join(t.TempDir(), "archives")
	result, err := PackageExtensionArchives(ExtensionArchiveOptions{
		Bun:         bun,
		ProjectRoot: projectRoot,
		OutputDir:   outputDir,
		Version:     "1.2.3-20261118000000-abc123def456",
	})
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	return result, outputDir
}

// extractArchive unpacks a .tar.gz into a new directory and returns it.
func extractArchive(t *testing.T, archive string) string {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return dir
		}
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(dir, filepath.FromSlash(header.Name))
		if header.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, os.FileMode(header.Mode)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPackageExtensionArchives_RunsItsEntryPointsWithoutNodeModules(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "extension-archives", "an-extension-archive-runs-its-entry-points-without-node-modules")
	bun := archiveTestBun(t)
	result, _ := packageFixture(t, bun, writeArchiveFixture(t, archiveFixtureManifest))

	if !slices.Equal(result.Entries, []string{"bin/greet", "bin/setup"}) {
		t.Fatalf("entries = %v, want the hook's and the task's entry points", result.Entries)
	}
	installed := extractArchive(t, result.Archives[0])
	for _, name := range []string{"README.md", "AI.md"} {
		if _, err := os.Stat(filepath.Join(installed, name)); err != nil {
			t.Errorf("the archive must carry %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(installed, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("the archive must carry no node_modules, stat err = %v", err)
	}
	loaded, err := proto.LoadManifest(filepath.Join(installed, proto.ManifestFilename))
	if err != nil {
		t.Fatalf("the archived manifest must load: %v", err)
	}
	if loaded.Version != "1.2.3-20261118000000-abc123def456" {
		t.Errorf("archived manifest version = %q, want the packaged version", loaded.Version)
	}
	if loaded.CLIContract == 0 {
		t.Error("the archived manifest must carry the contract the gate stamped")
	}

	// The installed extension runs its task the way the CLI does: bun run on
	// the name the manifest uses, from a directory with no node_modules.
	run := osexec.Command(bun, "run", filepath.Join(installed, "bin", "greet"), "world")
	run.Dir = t.TempDir()
	out, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("bun run the archived entry point: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "hello world!" {
		t.Fatalf("archived entry point printed %q, want %q", got, "hello world!")
	}
}

func TestPackageExtensionArchives_EveryPlatformKeyGetsTheSameArchive(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "extension-archives", "every-registry-platform-key-gets-the-same-archive")
	bun := archiveTestBun(t)
	result, outputDir := packageFixture(t, bun, writeArchiveFixture(t, archiveFixtureManifest))

	var want []string
	for _, platform := range pkgmeta.ArchivePlatformSuffixes() {
		want = append(want, filepath.Join(outputDir, "acme-greeter-"+platform+".tar.gz"))
	}
	if !slices.Equal(result.Archives, want) {
		t.Fatalf("archives = %v, want one per registry platform key: %v", result.Archives, want)
	}
	first, err := os.ReadFile(result.Archives[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, archive := range result.Archives[1:] {
		data, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, data) {
			t.Errorf("%s differs from %s: a platform-independent archive is the same bytes under every key", archive, result.Archives[0])
		}
	}

	// Packaging again writes the same bytes: the archive is reproducible.
	again, _ := packageFixture(t, bun, writeArchiveFixture(t, archiveFixtureManifest))
	second, err := os.ReadFile(again.Archives[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("packaging the same project twice must write the same archive")
	}
}

func TestPackageExtensionArchives_ExtensionlessEntryPointRunsFromItsBundle(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "extension-archive-entry-points", "an-extensionless-entry-point-runs-from-its-bundle")
	bun := archiveTestBun(t)
	result, _ := packageFixture(t, bun, writeArchiveFixture(t, archiveFixtureManifest))
	installed := extractArchive(t, result.Archives[0])

	for _, entry := range result.Entries {
		if _, err := os.Stat(filepath.Join(installed, filepath.FromSlash(entry)+".js")); err != nil {
			t.Errorf("%s must be bundled as %s.js: %v", entry, entry, err)
		}
		if _, err := os.Stat(filepath.Join(installed, filepath.FromSlash(entry)+".ts")); !os.IsNotExist(err) {
			t.Errorf("the archive must not carry the source of %s", entry)
		}
		if !ResolvesForBunRun(installed, entry) {
			t.Errorf("%s must resolve in the archive the way bun run resolves it", entry)
		}
	}
	var manifest map[string]any
	data, err := os.ReadFile(filepath.Join(installed, proto.ManifestFilename))
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		t.Fatalf("read the archived manifest: %v", err)
	}
	if !strings.Contains(string(data), `"{extensionRoot}/bin/greet"`) {
		t.Error("the archived manifest must keep the name the source manifest uses")
	}
}

func TestPackageExtensionArchives_MissingNamedFileFailsPackaging(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "extension-archive-entry-points", "a-task-or-hook-naming-a-missing-file-fails-packaging")
	bun := archiveTestBun(t)
	for _, tc := range []struct {
		name, from, to, want string
	}{
		{
			name: "a task names a file the project does not hold",
			from: `"{extensionRoot}/bin/greet", "world"`, to: `"{extensionRoot}/bin/absent", "world"`,
			want: "bin/absent",
		},
		{
			name: "a task names its TypeScript source, which the archive holds as .js",
			from: `"{extensionRoot}/bin/greet", "world"`, to: `"{extensionRoot}/bin/greet.ts", "world"`,
			want: "bin/greet.ts",
		},
		{
			name: "a hook names a file the project does not hold",
			from: `"{extensionRoot}/bin/setup"`, to: `"{extensionRoot}/bin/missing-hook"`,
			want: "bin/missing-hook",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := strings.Replace(archiveFixtureManifest, tc.from, tc.to, 1)
			if manifest == archiveFixtureManifest {
				t.Fatal("the fixture no longer holds the text this case replaces")
			}
			outputDir := filepath.Join(t.TempDir(), "archives")
			_, err := PackageExtensionArchives(ExtensionArchiveOptions{
				Bun:         bun,
				ProjectRoot: writeArchiveFixture(t, manifest),
				OutputDir:   outputDir,
				Version:     "1.0.0",
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a failure naming %s", err, tc.want)
			}
			if _, statErr := os.Stat(outputDir); !os.IsNotExist(statErr) {
				t.Fatalf("a refused package must write no archive, stat err = %v", statErr)
			}
		})
	}
}

func TestExtensionRootEntries_RefusesANameOutsideTheRoot(t *testing.T) {
	manifest := strings.Replace(archiveFixtureManifest, `"{extensionRoot}/bin/greet"`, `"{extensionRoot}/../escape"`, 1)
	if _, err := ExtensionRootEntries([]byte(manifest)); err == nil || !strings.Contains(err.Error(), "greet-exec") {
		t.Fatalf("err = %v, want a refusal naming the task", err)
	}
}

func TestPackageExtensionArchives_SkipsAProjectWithoutExecutableSurface(t *testing.T) {
	for name, manifest := range map[string]string{
		"no manifest":                   "",
		"hook-less, task-less manifest": `{"name": "@acme/empty", "commands": {}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if manifest != "" {
				if err := os.WriteFile(filepath.Join(dir, proto.ManifestFilename), []byte(manifest), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := PackageExtensionArchives(ExtensionArchiveOptions{ProjectRoot: dir, OutputDir: filepath.Join(dir, "out"), Version: "1.0.0"})
			if !IsNoExtensionSurface(err) {
				t.Fatalf("err = %v, want the skip of a project with nothing to package", err)
			}
		})
	}
}

func TestPackageExtensionArchives_LeavesOutTheHooksTheTypeScriptExtensionRuns(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "extension-archives", "the-archive-leaves-out-the-hooks-the-typescript-extension-runs-from-node-modules")
	bun := archiveTestBun(t)
	manifest := strings.Replace(archiveFixtureManifest, `"onInstall"`, `"configExtract": {"kind": "command", "command": "bun", "args": ["run", "{extensionRoot}/bin/extract"], "timeoutMs": 60000},
		"onInstall"`, 1)
	result, _ := packageFixture(t, bun, writeArchiveFixture(t, manifest))
	installed := extractArchive(t, result.Archives[0])

	data, err := os.ReadFile(filepath.Join(installed, proto.ManifestFilename))
	if err != nil {
		t.Fatal(err)
	}
	var archived struct {
		Hooks map[string]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &archived); err != nil {
		t.Fatal(err)
	}
	if _, ok := archived.Hooks["preBuild"]; ok {
		t.Error("the archive must leave out preBuild: the TypeScript extension runs it from node_modules")
	}
	if _, ok := archived.Hooks["configExtract"]; ok {
		t.Error("the archive must leave out configExtract: the TypeScript extension runs it from node_modules")
	}
	if _, ok := archived.Hooks["onInstall"]; !ok {
		t.Error("the archive must keep onInstall, which the CLI runs from the archive")
	}
	// A left-out hook's entry point is neither required nor shipped: the
	// project holds no bin/extract at all, and packaging succeeded.
	if _, err := os.Stat(filepath.Join(installed, "bin", "generate.js")); !os.IsNotExist(err) {
		t.Errorf("the archive must not bundle the entry of a left-out hook, stat err = %v", err)
	}
	if !ResolvesForBunRun(installed, "bin/setup") {
		t.Error("the entry of a kept hook must be bundled")
	}

	// A manifest whose only hooks are left out keeps no hooks object.
	only, err := ArchivedManifest([]byte(`{"name": "@acme/app", "hooks": {"preBuild": {"command": "bun"}}, "commands": {}}`), "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(only), "hooks") {
		t.Errorf("archived manifest %s keeps an empty hooks object", only)
	}
}

func TestPackageExtensionArchives_StagesANamedDirectory(t *testing.T) {
	bun := archiveTestBun(t)
	manifest := strings.Replace(archiveFixtureManifest, `"world"]`, `"world", "--templates={extensionRoot}/templates"]`, 1)
	project := writeArchiveFixture(t, manifest)
	if err := os.MkdirAll(filepath.Join(project, "templates", "page"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "templates", "page", "index.tsx"), []byte("export {};\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, _ := packageFixture(t, bun, project)
	installed := extractArchive(t, result.Archives[0])
	if _, err := os.Stat(filepath.Join(installed, "templates", "page", "index.tsx")); err != nil {
		t.Fatalf("a directory a task names must be staged as is: %v", err)
	}
}

func TestPackageExtensionArchives_AMissingExtensionFileFailsInsteadOfSkipping(t *testing.T) {
	bun := archiveTestBun(t)
	// The manifest binds agent content whose authored source the project
	// does not hold: a missing file of the extension, not a project without
	// one, so the step must fail rather than skip.
	manifest := strings.Replace(archiveFixtureManifest, `"name": "@acme/greeter",`, `"name": "@acme/greeter",
	"agentContent": {"path": "agent-content", "source": "agent-src"},`, 1)
	outputDir := filepath.Join(t.TempDir(), "archives")
	_, err := PackageExtensionArchives(ExtensionArchiveOptions{
		Bun:         bun,
		ProjectRoot: writeArchiveFixture(t, manifest),
		OutputDir:   outputDir,
		Version:     "1.0.0",
	})
	if err == nil || IsNoExtensionSurface(err) {
		t.Fatalf("err = %v, want a failure that is not the skip of a project with nothing to package", err)
	}
}

func TestPackageExtensionArchives_AFailedRunKeepsThePreviousArchives(t *testing.T) {
	bun := archiveTestBun(t)
	result, outputDir := packageFixture(t, bun, writeArchiveFixture(t, archiveFixtureManifest))
	before, err := os.ReadFile(result.Archives[0])
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(archiveFixtureManifest, `"{extensionRoot}/bin/greet"`, `"{extensionRoot}/bin/absent"`, 1)
	if _, err := PackageExtensionArchives(ExtensionArchiveOptions{Bun: bun, ProjectRoot: writeArchiveFixture(t, broken), OutputDir: outputDir, Version: "2.0.0"}); err == nil {
		t.Fatal("a manifest naming a missing file must fail packaging")
	}
	after, err := os.ReadFile(result.Archives[0])
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("a failed run must leave the previous archives as they were (err = %v)", err)
	}
}

func TestPackageExtensionArchives_ASecondRunReplacesTheArchives(t *testing.T) {
	bun := archiveTestBun(t)
	result, outputDir := packageFixture(t, bun, writeArchiveFixture(t, archiveFixtureManifest))
	if err := os.WriteFile(filepath.Join(outputDir, "stale.tar.gz"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := PackageExtensionArchives(ExtensionArchiveOptions{Bun: bun, ProjectRoot: writeArchiveFixture(t, archiveFixtureManifest), OutputDir: outputDir, Version: "9.9.9"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(again.Archives, result.Archives) {
		t.Fatalf("archives = %v, want %v", again.Archives, result.Archives)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "stale.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("a second run must replace the archives directory whole, stat err = %v", err)
	}
	info, err := os.Stat(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("archives directory mode = %v, want 0755", info.Mode().Perm())
	}
	installed := extractArchive(t, again.Archives[0])
	loaded, err := proto.LoadManifest(filepath.Join(installed, proto.ManifestFilename))
	if err != nil || loaded.Version != "9.9.9" {
		t.Fatalf("the replaced archive must carry the new version: %+v, %v", loaded, err)
	}
	if !slices.Equal(again.LeftOutHooks, []string{"preBuild"}) {
		t.Errorf("left-out hooks = %v, want [preBuild]", again.LeftOutHooks)
	}
}

func TestPackageExtensionArchives_CarriesTheWorkspaceLicense(t *testing.T) {
	bun := archiveTestBun(t)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "LICENSE.md"), []byte("workspace license\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(t.TempDir(), "archives")
	result, err := PackageExtensionArchives(ExtensionArchiveOptions{
		Bun:           bun,
		ProjectRoot:   writeArchiveFixture(t, archiveFixtureManifest),
		WorkspaceRoot: workspace,
		OutputDir:     outputDir,
		Version:       "1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	installed := extractArchive(t, result.Archives[0])
	if got, err := os.ReadFile(filepath.Join(installed, "LICENSE.md")); err != nil || string(got) != "workspace license\n" {
		t.Fatalf("the archive must carry the workspace LICENSE.md: %q, %v", got, err)
	}
}
