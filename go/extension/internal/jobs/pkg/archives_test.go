package pkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	buildjob "go.putnami.dev/go/extension/internal/jobs/build"
	"go.putnami.dev/go/extension/internal/platform"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"

	"go.putnami.dev/protocol/features/spectest"
)

// TestCreateExtensionArchivesIncludesCommonFiles pins the archive's WHOLE root
// layout rather than the presence of one asset. An extension's own AI.md
// travels beside putnami.extension.json, byte for byte, on every declared
// platform, and any future asset that joins or leaves the archive root fails
// here instead of in a consumer's workspace.
func TestCreateExtensionArchivesIncludesCommonFiles(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "reproducible-archives", "independent-staging-produces-identical-archive-bytes")
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "go", "extension")
	outputRoot := t.TempDir()
	// CRLF line endings and a trailing blank line: guidance is copied, never
	// re-rendered, so the archived bytes are the authored bytes.
	aiDoc := "# Extension guidance\r\n\r\nUse the declared ecosystems.\r\n"

	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), conformingManifest)
	mustWrite(t, filepath.Join(projectRoot, "AI.md"), aiDoc)
	mustWrite(t, filepath.Join(projectRoot, "README.md"), "# testext\n")
	mustWrite(t, filepath.Join(projectRoot, "LICENSE.md"), "MIT\n")
	mustWrite(t, filepath.Join(projectRoot, "package.json"), `{"name":"@putnami/testext"}`)
	mustWrite(t, filepath.Join(projectRoot, "bin", "prepare"), "#!/bin/sh\n")
	mustWrite(t, filepath.Join(projectRoot, "config", "defaults.json"), "{}\n")
	mustWrite(t, filepath.Join(projectRoot, "templates", "service", "putnami.template.json"), "{}\n")
	mustWrite(t, filepath.Join(projectRoot, "tools", "versions.json"), `{"schemaVersion":1}`)
	// Sources stay behind: an archive ships the extension, not its repository.
	mustWrite(t, filepath.Join(projectRoot, "go.mod"), "module example.com/testext\n")
	mustWrite(t, filepath.Join(projectRoot, "internal", "jobs", "run.go"), "package jobs\n")
	// Framework guidance is a workspace asset and keeps its own subtree.
	mustWrite(t, filepath.Join(workspaceRoot, "go", "framework", "http", "AI.md"), "# http framework\n")

	ctx := &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "@putnami/testext",
			FullPath: projectRoot,
		},
		Params: pctx.Params{"platforms": []byte(`["linux/amd64","darwin/arm64"]`)},
	}
	if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); !ok {
		t.Fatal("createExtensionArchives failed")
	}

	want := []string{
		"AI.md",
		"LICENSE.md",
		"README.md",
		"bin/prepare",
		"config/defaults.json",
		"framework-docs/http/AI.md",
		"package.json",
		"putnami.extension.json",
		"templates/service/putnami.template.json",
		"tools/versions.json",
	}
	for _, suffix := range []string{"linux-x64", "darwin-arm64"} {
		archivePath := filepath.Join(outputRoot, "archives", "putnami-testext-"+suffix+".tar.gz")
		entries := readArchiveEntries(t, archivePath)
		if got := archiveLayout(entries); !reflect.DeepEqual(got, want) {
			t.Errorf("%s archive layout = %v, want %v", suffix, got, want)
		}
		if got := string(entries["AI.md"]); got != aiDoc {
			t.Errorf("%s archived AI.md = %q, want the authored bytes %q", suffix, got, aiDoc)
		}
	}
	archivePath := filepath.Join(outputRoot, "archives", "putnami-testext-linux-x64.tar.gz")
	first, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); !ok {
		t.Fatal("archive retry failed")
	}
	second, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("repackaging the same extension changed immutable archive bytes")
	}
}

// TestCreateExtensionArchivesOmitsAbsentGuidance is the compatibility half of
// the same rule: an extension that authored no AI.md still packages, and
// packaging invents nothing to fill the gap — not an empty file, and not the
// framework guidance that sits one directory away in the workspace. A consumer
// reading the archive can therefore tell "no guidance" from "guidance lost".
func TestCreateExtensionArchivesOmitsAbsentGuidance(t *testing.T) {
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "go", "extension")
	outputRoot := t.TempDir()

	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), conformingManifest)
	mustWrite(t, filepath.Join(projectRoot, "tools", "versions.json"), `{"schemaVersion":1}`)
	mustWrite(t, filepath.Join(workspaceRoot, "go", "framework", "http", "AI.md"), "# http framework\n")

	ctx := &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "@putnami/testext",
			FullPath: projectRoot,
		},
		Params: pctx.Params{"platforms": []byte(`["linux/amd64"]`)},
	}
	if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); !ok {
		t.Fatal("createExtensionArchives failed for an extension without guidance")
	}

	archivePath := filepath.Join(outputRoot, "archives", "putnami-testext-linux-x64.tar.gz")
	entries := readArchiveEntries(t, archivePath)
	want := []string{
		"framework-docs/http/AI.md",
		"putnami.extension.json",
		"tools/versions.json",
	}
	if got := archiveLayout(entries); !reflect.DeepEqual(got, want) {
		t.Errorf("archive layout = %v, want %v", got, want)
	}
	if got, ok := entries["AI.md"]; ok {
		t.Errorf("archive root carries AI.md = %q, but the extension authored none", got)
	}
	// The rest of the archive contract is untouched by the missing file: the
	// manifest is still staged and still stamped with the resolved version.
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(entries["putnami.extension.json"], &manifest); err != nil {
		t.Fatalf("staged manifest: %v", err)
	}
	if manifest.Version != "1.2.3" {
		t.Errorf("staged manifest version = %q, want 1.2.3", manifest.Version)
	}
}

func TestCreateExtensionArchivesCarriesDeclaredRuntimePerPlatform(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-matrix-honesty", "each-declared-platform-carries-its-own-runtime")
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()
	manifest := `{
		"name": "@putnami/testext",
		"version": "1.2.3",
		"cliContract": 4,
		"runtime": {
			"executable": "compiled/putnami-go",
			"prepare": {
				"command": "{extensionRoot}/bin/prepare",
				"args": ["--output", "{runtimeOutput}"],
				"inputs": ["cmd/**"]
			}
		},
		"commands": {
			"build": {"run": [{"id": "build", "task": "build"}]}
		},
		"tasks": {
			"build": {"kind": "command", "command": "{extensionRuntime}", "args": ["build"]}
		}
	}`
	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), manifest)
	mustWrite(t, filepath.Join(projectRoot, "cmd", "putnami-go", "main.go"), "package main\nfunc main() {}\n")
	for _, platform := range platform.ArchivePlatforms {
		binary := filepath.Join(outputRoot, "build", "bin", platform.Suffix, pkgmeta.ExecutableName(platform.GOOS, "putnami-go"))
		mustWrite(t, binary, "runtime-"+platform.Suffix)
		if err := os.Chmod(binary, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ctx := &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "@putnami/testext",
			FullPath: projectRoot,
		},
	}
	if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); !ok {
		t.Fatal("createExtensionArchives failed")
	}

	for _, platform := range platform.ArchivePlatforms {
		archivePath := filepath.Join(outputRoot, "archives", "putnami-testext-"+platform.Suffix+".tar.gz")
		entries := readArchiveEntries(t, archivePath)
		// A Windows archive carries the runtime as compiled/putnami-go.exe; the
		// manifest keeps the declared name, and the CLI adds the suffix.
		runtimePath := "compiled/putnami-go"
		if platform.GOOS == "windows" {
			runtimePath = "compiled/putnami-go.exe"
		}
		runtimeEntry, ok := entries[runtimePath]
		if !ok {
			t.Errorf("%s archive has no declared runtime executable %s; layout %v", platform.Suffix, runtimePath, archiveLayout(entries))
		} else if string(runtimeEntry) != "runtime-"+platform.Suffix {
			t.Errorf("%s runtime bytes = %q", platform.Suffix, runtimeEntry)
		}
		if mode := archiveEntryMode(t, archivePath, runtimePath); mode&0o111 == 0 {
			t.Errorf("%s %s mode = %o, want executable", platform.Suffix, runtimePath, mode)
		}
		var staged struct {
			Runtime struct {
				Executable string `json:"executable"`
			} `json:"runtime"`
			Tasks map[string]struct {
				Command string `json:"command"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(entries["putnami.extension.json"], &staged); err != nil {
			t.Fatalf("%s manifest: %v", platform.Suffix, err)
		}
		if staged.Runtime.Executable != "compiled/putnami-go" ||
			staged.Tasks["build"].Command != "{extensionRuntime}" {
			t.Errorf("%s packaged manifest drifted: %+v", platform.Suffix, staged)
		}
	}
}

// The staged manifest and the binary it ships must report ONE version — the one
// the orchestrator resolved for the project's line. A tagged commit carries the
// tag itself, with no suffix, which is the identity a release is addressed by.
func TestArchiveManifestMatchesRuntimeInfoVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("archive packaging test requires tar and executable mode bits")
	}
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "extension")
	outputRoot := filepath.Join(workspaceRoot, ".putnami", "out", "extension", "package")
	mustWrite(t, filepath.Join(projectRoot, "go.mod"), "module example.com/runtime\n\ngo 1.24.0\n")
	mustWrite(t, filepath.Join(projectRoot, "putnami.json"), `{
		"name": "@putnami/runtime-test",
		"options": {"@putnami/go": {"entrypoint": "."}}
	}`)
	mustWrite(t, filepath.Join(projectRoot, "main.go"), `package main

import (
	"encoding/json"
	"os"
	"runtime"
)

var runtimeVersion = "unstamped"

func main() {
	if len(os.Args) == 3 && os.Args[1] == "__putnami" && os.Args[2] == "runtime-info" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"extension": "@putnami/runtime-test",
			"version": runtimeVersion,
			"platform": runtime.GOOS + "/" + runtime.GOARCH,
			"cliContract": 4,
			"runtimeProtocol": 2,
			"runtimeABI": 1,
		})
	}
}
`)
	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), `{
		"name": "@putnami/runtime-test",
		"version": "0.0.0",
		"cliContract": 4,
		"runtime": {"executable": "compiled/putnami-runtime-test"},
		"commands": {
			"build": {"run": [{"id": "build", "task": "build-exec"}]}
		},
		"tasks": {
			"build-exec": {
				"kind": "command",
				"command": "{extensionRuntime}"
			}
		}
	}`)
	ctx := &pctx.Context{
		WorkspaceRoot: workspaceRoot,
		OutputPath:    outputRoot,
		Workspace:     pctx.Workspace{Version: "1.2.3"},
		Project: pctx.Project{
			Name:     "@putnami/runtime-test",
			Path:     "extension",
			FullPath: projectRoot,
		},
		Params: paramsWith(t, map[string]string{
			"version-var": "main.runtimeVersion",
		}),
		// A tagged commit: Full IS the tag's version.
		Version: &pctx.Version{Base: "1.2.3", Full: "1.2.3"},
	}
	target := runtime.GOOS + "/" + runtime.GOARCH
	status, data, err := buildjob.Run(ctx, jsonl.New(), []string{
		"--phase", "cross-compile",
		"--target", target,
		"--entrypoint", ".",
		"--version-var", "main.runtimeVersion",
	})
	if err != nil || status != "OK" {
		t.Fatalf("cross-compile = (%q, %#v, %v), want OK", status, data, err)
	}
	hostTarget := platform.SuffixFromDockerPlatform(target)
	hostBinary := filepath.Join(outputRoot, "bin", hostTarget, "putnami-runtime-test")
	for _, archiveTarget := range platform.ArchivePlatforms {
		targetBinary := filepath.Join(outputRoot, "bin", archiveTarget.Suffix, pkgmeta.ExecutableName(archiveTarget.GOOS, "putnami-runtime-test"))
		if targetBinary == hostBinary {
			continue
		}
		source, err := os.ReadFile(hostBinary)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, targetBinary, string(source))
		if err := os.Chmod(targetBinary, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	packageEvents := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err = Run(ctx, emit, []string{"--archives"})
	})
	if err != nil || status != "OK" {
		t.Fatalf("package = (%q, %v), want OK; events=%+v", status, err, packageEvents)
	}
	archivePath := filepath.Join(outputRoot, "archives", "putnami-runtime-test-"+hostTarget+".tar.gz")
	entries := readArchiveEntries(t, archivePath)
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(entries["putnami.extension.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	extracted := filepath.Join(t.TempDir(), "putnami-runtime-test")
	if err := os.WriteFile(extracted, entries["compiled/putnami-runtime-test"], 0o755); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(extracted, "__putnami", "runtime-info").Output()
	if err != nil {
		t.Fatalf("runtime-info: %v", err)
	}
	var info struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(output, &info); err != nil {
		t.Fatalf("runtime-info JSON: %v", err)
	}
	if manifest.Version != "1.2.3" || info.Version != manifest.Version {
		t.Fatalf("identities: manifest=%q runtime-info=%q, want both 1.2.3", manifest.Version, info.Version)
	}
}

func TestCreateExtensionArchivesRefusesMissingPreparedRuntime(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-matrix-honesty", "an-archive-whose-prepared-runtime-is-missing-is-refused")
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()
	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), `{
		"name": "@putnami/testext",
		"version": "1.2.3",
		"cliContract": 4,
		"runtime": {
			"executable": "compiled/putnami-go"
		},
		"commands": {
			"build": {"run": [{"id": "build", "task": "build"}]}
		},
		"tasks": {
			"build": {"kind": "command", "command": "{extensionRuntime}", "args": ["build"]}
		}
	}`)
	mustWrite(t, filepath.Join(projectRoot, "cmd", "putnami-go", "main.go"), "package main\nfunc main() {}\n")

	ctx := &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "@putnami/testext",
			FullPath: projectRoot,
		},
	}
	if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); ok {
		t.Fatal("createExtensionArchives succeeded without its prepared runtime binary")
	}
	if archives, err := filepath.Glob(filepath.Join(outputRoot, "archives", "*.tar.gz")); err != nil {
		t.Fatal(err)
	} else if len(archives) != 0 {
		t.Errorf("failed packaging emitted archives: %v", archives)
	}
}

func TestCreateTemplateArchivesStagesContentAndStampsVersion(t *testing.T) {
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()

	mustWrite(t, filepath.Join(projectRoot, "putnami.template.json"), `{
		"name": "@putnami/example-template",
		"version": "0.0.0",
		"description": "Example template"
	}`)
	mustWrite(t, filepath.Join(projectRoot, "README.md"), "# Example template\n")
	mustWrite(t, filepath.Join(projectRoot, "LICENSE.md"), "MIT\n")
	mustWrite(t, filepath.Join(projectRoot, "src", "main.go"), "package main\n")
	mustWrite(t, filepath.Join(projectRoot, "template.yaml"), "enabled: true\n")
	mustWrite(t, filepath.Join(projectRoot, ".private"), "do not package\n")
	mustWrite(t, filepath.Join(projectRoot, "node_modules", "dependency.js"), "do not package\n")

	ctx := &pctx.Context{
		Project: pctx.Project{
			Name:     "@putnami/example-template",
			FullPath: projectRoot,
		},
	}
	if ok := createTemplateArchives(ctx, jsonl.New(), "@putnami/example-template", "1.2.3", outputRoot, false); !ok {
		t.Fatal("createTemplateArchives failed")
	}

	for _, target := range platform.ArchivePlatforms {
		archivePath := filepath.Join(outputRoot, "archives", "putnami-example-template-"+target.Suffix+".tar.gz")
		entries := readArchiveEntries(t, archivePath)

		var manifest map[string]any
		if err := json.Unmarshal(entries["putnami.template.json"], &manifest); err != nil {
			t.Fatalf("%s template manifest: %v", target.Suffix, err)
		}
		if got := manifest["version"]; got != "1.2.3" {
			t.Errorf("%s template version = %v, want 1.2.3", target.Suffix, got)
		}
		for name, want := range map[string]string{
			"README.md":     "# Example template\n",
			"LICENSE.md":    "MIT\n",
			"src/main.go":   "package main\n",
			"template.yaml": "enabled: true\n",
		} {
			if got := string(entries[name]); got != want {
				t.Errorf("%s archive entry %s = %q, want %q", target.Suffix, name, got, want)
			}
		}
		for _, excluded := range []string{".private", "node_modules/dependency.js"} {
			if _, ok := entries[excluded]; ok {
				t.Errorf("%s archive unexpectedly contains %s", target.Suffix, excluded)
			}
		}
	}
}

// The CLI installs the same template archive on every OS, and a Windows CLI
// creates no link from an archive. A template that holds a link therefore
// fails packaging, for every platform, with the entry named, and no archive is
// written.
func TestCreateTemplateArchivesRefusesASymbolicLink(t *testing.T) {
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()
	mustWrite(t, filepath.Join(projectRoot, "putnami.template.json"), `{"name": "@putnami/example-template", "version": "0.0.0"}`)
	mustWrite(t, filepath.Join(projectRoot, "src", "main.go"), "package main\n")
	if err := os.Symlink("main.go", filepath.Join(projectRoot, "src", "current.go")); err != nil {
		t.Skipf("this host creates no symbolic link: %v", err)
	}
	ctx := &pctx.Context{Project: pctx.Project{Name: "@putnami/example-template", FullPath: projectRoot}}

	var ok bool
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		ok = createTemplateArchives(ctx, emit, "@putnami/example-template", "1.2.3", outputRoot, false)
	})
	if ok {
		t.Fatal("createTemplateArchives packaged a template that holds a symbolic link")
	}
	diagnostics := errorDiagnostics(events)
	want := "template archive entry src/current.go is a symbolic link to main.go: "
	if len(diagnostics) != 1 || !strings.HasPrefix(diagnostics[0], want) {
		t.Errorf("error diagnostics = %q, want one that starts with %q", diagnostics, want)
	}
	if archives, err := filepath.Glob(filepath.Join(outputRoot, "archives", "*.tar.gz")); err != nil {
		t.Fatal(err)
	} else if len(archives) != 0 {
		t.Errorf("refused packaging wrote archives: %v", archives)
	}
}

// TestDeclaredRuntimeManifestNeedsNoArchiveRewrite pins source/package parity:
// the same manifest names the resolved executable in local and installed
// modes, so archive creation never grows a second wrapper command shape.
func TestDeclaredRuntimeManifestNeedsNoArchiveRewrite(t *testing.T) {
	stageDir := t.TempDir()
	// The cache lifecycle is a typed TASK behind a reserved command since an
	// earlier migration; the parity claim is unchanged, only the section it is
	// declared in moved.
	mustWrite(t, filepath.Join(stageDir, "putnami.extension.json"), `{
		"runtime": {"executable": "compiled/putnami-go"},
		"tasks": {
			"cache-clean-exec": {
				"kind": "command",
				"command": "{extensionRuntime}",
				"args": ["cache-clean"]
			}
		}
	}`)
	mustWrite(t, filepath.Join(stageDir, "compiled", "putnami-go"), "runtime")
	if err := os.Chmod(filepath.Join(stageDir, "compiled", "putnami-go"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := validateStagedRuntimeExecutable(stageDir, "linux"); err != nil {
		t.Fatalf("declared packaged runtime: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(stageDir, "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}

	task := manifest.Tasks["cache-clean-exec"]
	if task.Command != "{extensionRuntime}" {
		t.Errorf("cache-clean-exec command = %q, want declared runtime", task.Command)
	}
	if len(task.Args) != 1 || task.Args[0] != "cache-clean" {
		t.Errorf("cache-clean-exec args = %v, want subcommand only", task.Args)
	}
}

// archiveLayout is the sorted set of regular-file paths an archive carries.
// Layout assertions compare it whole, so a root asset that appears or vanishes
// is a test failure rather than a silent packaging change.
func archiveLayout(entries map[string][]byte) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func readArchiveEntries(t *testing.T, archivePath string) map[string][]byte {
	t.Helper()
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	entries := make(map[string][]byte)
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[strings.TrimPrefix(header.Name, "./")] = data
	}
}

// TestCreateExtensionArchivesStagesTheDeclaredMatrixOnly is the release-channel
// half of channel scoping: the archive set covers the DECLARED platforms, and
// nothing widens it back to four.
//
// It reads the same plan-time `platforms` parameter the cross-compile step
// scoped its work to. If the two ever disagreed, this channel would either fail
// on a binary nobody built or publish an archive for a platform the release
// matrix no longer names.
func TestCreateExtensionArchivesStagesTheDeclaredMatrixOnly(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-matrix-honesty", "an-archive-stages-exactly-the-declared-platform-matrix")
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()

	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), conformingManifest)

	ctx := &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project: pctx.Project{
			Name:     "@putnami/testext",
			FullPath: projectRoot,
		},
		Params: pctx.Params{"platforms": []byte(`["linux/amd64","linux/arm64"]`)},
	}
	if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); !ok {
		t.Fatal("createExtensionArchives failed")
	}

	entries, err := os.ReadDir(filepath.Join(outputRoot, "archives"))
	if err != nil {
		t.Fatalf("read archives dir: %v", err)
	}
	staged := make([]string, 0, len(entries))
	for _, e := range entries {
		staged = append(staged, e.Name())
	}
	sort.Strings(staged)
	want := []string{"putnami-testext-linux-arm64.tar.gz", "putnami-testext-linux-x64.tar.gz"}
	if !reflect.DeepEqual(staged, want) {
		t.Errorf("archives = %v, want %v: the declared release matrix and nothing else", staged, want)
	}
}

// TestReleaseTargetsDefaultsToTheArchiveMatrix pins the no-widening half: a
// project that declares no `platforms` keeps the four archives it has always
// published, so the narrowing applies only where a project asked for it.
func TestReleaseTargetsDefaultsToTheArchiveMatrix(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-matrix-honesty", "the-release-target-set-defaults-to-the-archive-matrix")
	got, err := releaseTargets(&pctx.Context{Params: pctx.Params{}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, platform.ArchivePlatforms) {
		t.Errorf("releaseTargets(no declaration) = %v, want the archive matrix", got)
	}
	if _, err := releaseTargets(&pctx.Context{
		Params: pctx.Params{"platforms": []byte(`{"oops":1}`)},
	}); err == nil {
		t.Error("a malformed `platforms` parameter resolved silently; a typo must not become a release matrix")
	}
}

// TestArchiveParallelismStaysWithinTheGrant pins the arithmetic that keeps
// parallel packaging inside the task's CPU grant: never more workers than the
// grant or the platforms, and each pinned tool build gets grant/workers cores,
// at least one.
func TestArchiveParallelismStaysWithinTheGrant(t *testing.T) {
	for _, tc := range []struct {
		budget     string
		platforms  int
		workers    int
		goMaxProcs int
	}{
		{budget: "1", platforms: 4, workers: 1, goMaxProcs: 1},
		{budget: "2", platforms: 4, workers: 2, goMaxProcs: 1},
		{budget: "3", platforms: 4, workers: 3, goMaxProcs: 1},
		{budget: "8", platforms: 4, workers: 4, goMaxProcs: 2},
		{budget: "10", platforms: 4, workers: 4, goMaxProcs: 2},
		{budget: " 8 ", platforms: 4, workers: 4, goMaxProcs: 2},
		{budget: "5", platforms: 2, workers: 2, goMaxProcs: 2},
		{budget: "4", platforms: 1, workers: 1, goMaxProcs: 4},
	} {
		workers, goMaxProcs := archiveParallelism(tc.platforms, budgetEnv(tc.budget))
		if workers != tc.workers || goMaxProcs != tc.goMaxProcs {
			t.Errorf("archiveParallelism(%d platforms, budget %q) = (%d workers, GOMAXPROCS %d), want (%d, %d)",
				tc.platforms, tc.budget, workers, goMaxProcs, tc.workers, tc.goMaxProcs)
		}
	}

	// A missing or unusable budget falls back to GOMAXPROCS, never to zero
	// workers and never to an unbounded pool.
	grant := runtime.GOMAXPROCS(0)
	wantWorkers := max(1, min(4, grant))
	wantProcs := max(1, grant/wantWorkers)
	for _, budget := range []string{"", "0", "-2", "two", "1.5"} {
		workers, goMaxProcs := archiveParallelism(4, budgetEnv(budget))
		if workers != wantWorkers || goMaxProcs != wantProcs {
			t.Errorf("archiveParallelism(4 platforms, budget %q) = (%d, %d), want the GOMAXPROCS fallback (%d, %d)",
				budget, workers, goMaxProcs, wantWorkers, wantProcs)
		}
	}
}

// TestCreateExtensionArchivesRunsAtMostTheGrantOfPlatformsAtOnce pins the
// worker pool against the grant the scheduler exports. Each step waits until
// the pool has reached its bound once, so a pool that ran platforms one at a
// time cannot pass by being fast, and a pool that ran more than the grant is
// caught by the high-water mark.
func TestCreateExtensionArchivesRunsAtMostTheGrantOfPlatformsAtOnce(t *testing.T) {
	declared := archiveSuffixes(platform.ArchivePlatforms)
	for _, tc := range []struct {
		grant      int
		workers    int
		goMaxProcs int
	}{
		{grant: 1, workers: 1, goMaxProcs: 1},
		{grant: 2, workers: 2, goMaxProcs: 1},
		// A grant of two cores per platform runs every platform at once.
		{grant: 2 * len(declared), workers: len(declared), goMaxProcs: 2},
	} {
		t.Run("grant "+strconv.Itoa(tc.grant), func(t *testing.T) {
			t.Setenv(cpuBudgetEnv, strconv.Itoa(tc.grant))
			rec := recordPlatformSteps(t, func(rec *stepRecorder, _ platform.Target) error {
				rec.awaitHighWater(t, tc.workers)
				return nil
			})

			var ok bool
			events := captureEvents(t, func(emit *jsonl.Emitter) {
				ok = createExtensionArchives(archivePoolFixture(t), emit, "@putnami/testext", "1.2.3", t.TempDir(), false)
			})
			if !ok {
				t.Fatalf("createExtensionArchives failed; events=%v", events)
			}

			if rec.highWater > tc.grant {
				t.Errorf("%d platforms ran at the same time under a grant of %d", rec.highWater, tc.grant)
			}
			if rec.highWater != tc.workers {
				t.Errorf("at most %d platforms ran at the same time, want %d", rec.highWater, tc.workers)
			}
			if tc.grant == 1 && !reflect.DeepEqual(rec.started, declared) {
				t.Errorf("grant 1 started platforms in order %v, want the declared order %v", rec.started, declared)
			}
			for i, got := range rec.goMaxProcs {
				if got != tc.goMaxProcs {
					t.Errorf("platform %s got GOMAXPROCS %d, want %d", rec.started[i], got, tc.goMaxProcs)
				}
			}

			// One progress event per packaged platform, counted up in the order
			// they finished, never backwards.
			progress := eventsOfType(events, "progress")
			if len(progress) != len(declared) {
				t.Fatalf("progress events = %v, want one per platform", progress)
			}
			packaged := make([]string, 0, len(progress))
			for i, event := range progress {
				if event["current"] != float64(i+1) || event["total"] != float64(len(declared)) {
					t.Errorf("progress event %d = %v, want %d/%d", i, event, i+1, len(declared))
				}
				message, _ := event["message"].(string)
				packaged = append(packaged, strings.TrimPrefix(message, "Packaged "))
			}
			if tc.grant == 1 && !reflect.DeepEqual(packaged, declared) {
				t.Errorf("grant 1 packaged %v, want the declared order %v", packaged, declared)
			}
			sort.Strings(packaged)
			sortedDeclared := append([]string(nil), declared...)
			sort.Strings(sortedDeclared)
			if !reflect.DeepEqual(packaged, sortedDeclared) {
				t.Errorf("packaged platforms = %v, want every declared platform once", packaged)
			}
		})
	}
}

// TestCreateExtensionArchivesStopsStartingPlatformsAfterAFailure pins the
// serial behavior at a grant of 1: the platforms after a failed one never
// start, exactly as the loop this pool replaced returned at the first failure.
func TestCreateExtensionArchivesStopsStartingPlatformsAfterAFailure(t *testing.T) {
	t.Setenv(cpuBudgetEnv, "1")
	rec := recordPlatformSteps(t, func(_ *stepRecorder, p platform.Target) error {
		if p.Suffix == "linux-arm64" {
			return errors.New("packaging linux-arm64 failed")
		}
		return nil
	})
	if ok := createExtensionArchives(archivePoolFixture(t), jsonl.New(), "@putnami/testext", "1.2.3", t.TempDir(), false); ok {
		t.Fatal("createExtensionArchives succeeded although a platform failed")
	}
	if want := []string{"linux-x64", "linux-arm64"}; !reflect.DeepEqual(rec.started, want) {
		t.Errorf("started platforms = %v, want %v and nothing after the failure", rec.started, want)
	}
}

// TestCreateExtensionArchivesReportsTheFirstFailureInPlatformOrder pins the
// error contract of the pool: when two platforms fail, the task reports the
// one that comes first in platform order, with one diagnostic and one failed
// phase end, whichever of the two finished first.
func TestCreateExtensionArchivesReportsTheFirstFailureInPlatformOrder(t *testing.T) {
	for _, finishesFirst := range []string{"darwin-arm64", "linux-arm64"} {
		t.Run(finishesFirst+" finishes first", func(t *testing.T) {
			t.Setenv(cpuBudgetEnv, "4")
			var arrived sync.WaitGroup
			arrived.Add(2)
			bothStarted := make(chan struct{})
			go func() {
				arrived.Wait()
				close(bothStarted)
			}()
			firstFailed := make(chan struct{})
			recordPlatformSteps(t, func(_ *stepRecorder, p platform.Target) error {
				if p.Suffix != "linux-arm64" && p.Suffix != "darwin-arm64" {
					return nil
				}
				// Both failing platforms are running before either fails, so
				// both failures are real and only their finishing order varies.
				arrived.Done()
				awaitOrFail(t, bothStarted, "both failing platforms to start")
				if p.Suffix == finishesFirst {
					close(firstFailed)
				} else {
					awaitOrFail(t, firstFailed, finishesFirst+" to fail")
					time.Sleep(20 * time.Millisecond)
				}
				return errors.New("packaging " + p.Suffix + " failed")
			})

			var ok bool
			events := captureEvents(t, func(emit *jsonl.Emitter) {
				ok = createExtensionArchives(archivePoolFixture(t), emit, "@putnami/testext", "1.2.3", t.TempDir(), false)
			})
			if ok {
				t.Fatal("createExtensionArchives succeeded although two platforms failed")
			}

			var diagnostics []string
			for _, event := range eventsOfType(events, "diagnostic") {
				if event["severity"] == "error" {
					message, _ := event["message"].(string)
					diagnostics = append(diagnostics, message)
				}
			}
			if want := []string{"packaging linux-arm64 failed"}; !reflect.DeepEqual(diagnostics, want) {
				t.Errorf("error diagnostics = %q, want exactly %q", diagnostics, want)
			}
			failedEnds := 0
			for _, event := range eventsOfType(events, "phase") {
				if event["name"] == "package-archives" && event["action"] == "end" {
					if event["status"] != "failed" {
						t.Errorf("package-archives phase ended %v, want failed", event["status"])
					}
					failedEnds++
				}
			}
			if failedEnds != 1 {
				t.Errorf("package-archives phase ended %d times, want once", failedEnds)
			}
		})
	}
}

// TestCreateExtensionArchivesBytesDoNotDependOnTheGrant pins determinism under
// the pool: the same fixture packaged one platform at a time (grant 1) and all
// platforms at the same time (grant 4) yields byte-identical archives, for a
// project that stages each platform on its own and for one whose platforms
// all archive the shared base stage.
func TestCreateExtensionArchivesBytesDoNotDependOnTheGrant(t *testing.T) {
	const runtimeManifest = `{
		"name": "@putnami/testext",
		"version": "1.2.3",
		"cliContract": 4,
		"runtime": {"executable": "compiled/putnami-go"},
		"commands": {"build": {"run": [{"id": "build", "task": "build"}]}},
		"tasks": {"build": {"kind": "command", "command": "{extensionRuntime}", "args": ["build"]}}
	}`
	for _, tc := range []struct {
		name     string
		manifest string
		binary   bool
	}{
		{name: "per-platform stage", manifest: runtimeManifest, binary: true},
		{name: "shared base stage", manifest: conformingManifest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectRoot := t.TempDir()
			buildOutput := filepath.Join(t.TempDir(), "build")
			mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), tc.manifest)
			mustWrite(t, filepath.Join(projectRoot, "README.md"), "# testext\n")
			if tc.binary {
				mustWrite(t, filepath.Join(projectRoot, "cmd", "putnami-go", "main.go"), "package main\nfunc main() {}\n")
				for _, target := range platform.ArchivePlatforms {
					binary := filepath.Join(buildOutput, "bin", target.Suffix, pkgmeta.ExecutableName(target.GOOS, "putnami-go"))
					mustWrite(t, binary, "runtime-"+target.Suffix)
					if err := os.Chmod(binary, 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			ctx := &pctx.Context{
				WorkspaceRoot: projectRoot,
				OutputPath:    buildOutput,
				Project: pctx.Project{
					Name:     "@putnami/testext",
					FullPath: projectRoot,
				},
			}

			digests := make(map[string]map[string]string)
			for _, grant := range []string{"1", "4"} {
				t.Setenv(cpuBudgetEnv, grant)
				outputRoot := t.TempDir()
				if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); !ok {
					t.Fatalf("createExtensionArchives failed at grant %s", grant)
				}
				digests[grant] = archiveDigests(t, filepath.Join(outputRoot, "archives"))
			}
			if len(digests["1"]) != len(platform.ArchivePlatforms) {
				t.Fatalf("grant 1 produced archives %v, want one per platform", digests["1"])
			}
			if !reflect.DeepEqual(digests["1"], digests["4"]) {
				t.Errorf("archive SHA-256 differs by grant:\ngrant 1: %v\ngrant 4: %v", digests["1"], digests["4"])
			}
		})
	}
}

// stepRecorder replaces the per-platform step for one test and records what
// the worker pool did with it.
type stepRecorder struct {
	mu         sync.Mutex
	started    []string
	goMaxProcs []int
	running    int
	highWater  int
}

// recordPlatformSteps installs step as the per-platform step for the rest of
// the test, wrapped so every call is recorded.
func recordPlatformSteps(t *testing.T, step func(rec *stepRecorder, p platform.Target) error) *stepRecorder {
	t.Helper()
	rec := &stepRecorder{}
	original := packagePlatformStep
	t.Cleanup(func() { packagePlatformStep = original })
	packagePlatformStep = func(run *platformPackaging, p platform.Target) error {
		rec.mu.Lock()
		rec.started = append(rec.started, p.Suffix)
		rec.goMaxProcs = append(rec.goMaxProcs, run.goMaxProcs)
		rec.running++
		rec.highWater = max(rec.highWater, rec.running)
		rec.mu.Unlock()
		defer func() {
			rec.mu.Lock()
			rec.running--
			rec.mu.Unlock()
		}()
		return step(rec, p)
	}
	return rec
}

// awaitHighWater holds a step until want steps have run at the same time at
// least once. It gives up after a bound instead of hanging, and the caller's
// high-water assertion then reports the shortfall.
func (r *stepRecorder) awaitHighWater(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		reached := r.highWater >= want
		r.mu.Unlock()
		if reached {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// awaitOrFail waits for ch to close. It reports a failure instead of hanging
// the test when the pool never gets there.
func awaitOrFail(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Errorf("timed out waiting for %s", what)
	}
}

// archivePoolFixture is an extension with no binary, packaged for the four
// default platforms. The pool tests replace the per-platform step, so the
// fixture only has to bring createExtensionArchives to its worker pool.
func archivePoolFixture(t *testing.T) *pctx.Context {
	t.Helper()
	projectRoot := t.TempDir()
	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), conformingManifest)
	return &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(t.TempDir(), "build"),
		Project: pctx.Project{
			Name:     "@putnami/testext",
			FullPath: projectRoot,
		},
	}
}

func budgetEnv(budget string) func(string) string {
	return func(key string) string {
		if key == cpuBudgetEnv {
			return budget
		}
		return ""
	}
}

func archiveSuffixes(targets []platform.Target) []string {
	suffixes := make([]string, 0, len(targets))
	for _, target := range targets {
		suffixes = append(suffixes, target.Suffix)
	}
	return suffixes
}

func eventsOfType(events []map[string]any, eventType string) []map[string]any {
	var matched []map[string]any
	for _, event := range events {
		if event["type"] == eventType {
			matched = append(matched, event)
		}
	}
	return matched
}

// archiveDigests maps each archive file name in dir to its SHA-256.
func archiveDigests(t *testing.T, dir string) map[string]string {
	t.Helper()
	archives, err := filepath.Glob(filepath.Join(dir, "*.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	digests := make(map[string]string, len(archives))
	for _, archive := range archives {
		data, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		digests[filepath.Base(archive)] = hex.EncodeToString(sum[:])
	}
	return digests
}

// declaredExecutableFixture is an extension whose putnami.json declares one
// executable beside its runtime, with the runtime prebuilt for every archive
// platform. withEmitter decides whether the cross-compile output also carries
// the declared executable.
func declaredExecutableFixture(t *testing.T, withEmitter bool) (*pctx.Context, string) {
	t.Helper()
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()
	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), `{
		"name": "@putnami/testext",
		"version": "1.2.3",
		"cliContract": 4,
		"runtime": {"executable": "compiled/putnami-go"},
		"commands": {"build": {"run": [{"id": "build", "task": "build"}]}},
		"tasks": {"build": {"kind": "command", "command": "{extensionRuntime}", "args": ["build"]}}
	}`)
	mustWrite(t, filepath.Join(projectRoot, "putnami.json"), `{
		"name": "@putnami/testext",
		"options": {"@putnami/go": {
			"entrypoint": "./cmd/putnami-go",
			"executables": [{"name": "putnami-client-generate-go", "package": "example.com/api/cmd/clientgen"}]
		}}
	}`)
	mustWrite(t, filepath.Join(projectRoot, "cmd", "putnami-go", "main.go"), "package main\nfunc main() {}\n")
	for _, p := range platform.ArchivePlatforms {
		dir := filepath.Join(outputRoot, "build", "bin", p.Suffix)
		names := []string{"putnami-go"}
		if withEmitter {
			names = append(names, "putnami-client-generate-go")
		}
		for _, name := range names {
			file := pkgmeta.ExecutableName(p.GOOS, name)
			mustWrite(t, filepath.Join(dir, file), name+"-"+p.Suffix)
			if err := os.Chmod(filepath.Join(dir, file), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project:       pctx.Project{Name: "@putnami/testext", FullPath: projectRoot},
	}, outputRoot
}

// Every executable the project declares ships in compiled/ beside the runtime,
// with its own platform's bytes and its executable bit, in every archive. An
// archive that carries the runtime alone fails every consumer's
// clientgen~generate-go.
func TestCreateExtensionArchivesShipsEveryDeclaredExecutable(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-matrix-honesty", "each-declared-executable-ships-beside-the-runtime")
	ctx, outputRoot := declaredExecutableFixture(t, true)
	if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); !ok {
		t.Fatal("createExtensionArchives failed")
	}
	for _, p := range platform.ArchivePlatforms {
		archivePath := filepath.Join(outputRoot, "archives", "putnami-testext-"+p.Suffix+".tar.gz")
		entries := readArchiveEntries(t, archivePath)
		for _, name := range []string{"putnami-go", "putnami-client-generate-go"} {
			file := pkgmeta.ExecutableName(p.GOOS, name)
			if got, ok := entries["compiled/"+file]; !ok {
				t.Errorf("%s archive lacks compiled/%s; layout %v", p.Suffix, file, archiveLayout(entries))
			} else if string(got) != name+"-"+p.Suffix {
				t.Errorf("%s compiled/%s bytes = %q", p.Suffix, file, got)
			}
		}
		emitter := "compiled/" + pkgmeta.ExecutableName(p.GOOS, "putnami-client-generate-go")
		if mode := archiveEntryMode(t, archivePath, emitter); mode&0o111 == 0 {
			t.Errorf("%s %s mode = %o, want executable", p.Suffix, emitter, mode)
		}
	}
}

// A declared executable the cross-compile did not produce fails the package
// job, and no archive is written.
func TestCreateExtensionArchivesRefusesAMissingDeclaredExecutable(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "archive-matrix-honesty", "an-archive-missing-a-declared-executable-is-refused")
	ctx, outputRoot := declaredExecutableFixture(t, false)
	if ok := createExtensionArchives(ctx, jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); ok {
		t.Fatal("createExtensionArchives succeeded without the declared executable")
	}
	if archives, err := filepath.Glob(filepath.Join(outputRoot, "archives", "*.tar.gz")); err != nil {
		t.Fatal(err)
	} else if len(archives) != 0 {
		t.Errorf("failed packaging emitted archives: %v", archives)
	}
}

func archiveEntryMode(t *testing.T, archivePath, name string) int64 {
	t.Helper()
	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			t.Fatalf("%s has no entry %s", archivePath, name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimPrefix(h.Name, "./") == name {
			return h.Mode
		}
	}
}
