package pkg

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// scriptRuntimeManifest declares a runtime the packager does not build: the
// project ships bin/run and has no Go main package.
const scriptRuntimeManifest = `{
	"name": "@putnami/testext",
	"version": "1.2.3",
	"cliContract": 4,
	"runtime": {"executable": "bin/run"},
	"commands": {"build": {"run": [{"id": "build", "task": "build"}]}},
	"tasks": {"build": {"kind": "command", "command": "{extensionRuntime}", "args": ["build"]}}
}`

// binaryRuntimeManifest declares the runtime the cross-compile step builds.
const binaryRuntimeManifest = `{
	"name": "@putnami/testext",
	"version": "1.2.3",
	"cliContract": 4,
	"runtime": {"executable": "compiled/putnami-go"},
	"commands": {"build": {"run": [{"id": "build", "task": "build"}]}},
	"tasks": {"build": {"kind": "command", "command": "{extensionRuntime}", "args": ["build"]}}
}`

func windowsTargetContext(projectRoot, outputRoot, platforms string) *pctx.Context {
	return &pctx.Context{
		WorkspaceRoot: projectRoot,
		OutputPath:    filepath.Join(outputRoot, "build"),
		Project:       pctx.Project{Name: "@putnami/testext", FullPath: projectRoot},
		Params:        pctx.Params{"platforms": []byte(platforms)},
	}
}

func packagedArchive(outputRoot, suffix string) string {
	return filepath.Join(outputRoot, "archives", "putnami-testext-"+suffix+".tar.gz")
}

func errorDiagnostics(events []map[string]any) []string {
	var messages []string
	for _, event := range eventsOfType(events, "diagnostic") {
		if event["severity"] == "error" {
			message, _ := event["message"].(string)
			messages = append(messages, message)
		}
	}
	return messages
}

// requireNoWindowsArchive fails when the refused windows-x64 archive was
// written.
func requireNoWindowsArchive(t *testing.T, outputRoot string) {
	t.Helper()
	if _, err := os.Stat(packagedArchive(outputRoot, "windows-x64")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the refused windows-x64 archive exists (stat error %v)", err)
	}
}

// archiveLinks maps each symbolic link entry of the archive to its target.
func archiveLinks(t *testing.T, archivePath string) map[string]string {
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
	links := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return links
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeSymlink {
			links[strings.TrimPrefix(h.Name, "./")] = h.Linkname
		}
	}
}

// A project without a Go binary stages no Windows build of its runtime, and a
// Windows CLI runs bin/run.exe and nothing else, so the Windows archive is
// refused at package time and names the file it lacks. The Unix archive of the
// same run keeps the bytes a Unix-only matrix packages.
func TestWindowsArchiveRefusesARuntimeThePackagerDoesNotBuild(t *testing.T) {
	// One platform at a time: linux-x64 is archived before windows-x64 fails.
	t.Setenv(cpuBudgetEnv, "1")
	projectRoot := t.TempDir()
	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), scriptRuntimeManifest)
	mustWrite(t, filepath.Join(projectRoot, "bin", "run"), "#!/bin/sh\n")

	unixOnly := t.TempDir()
	if ok := createExtensionArchives(windowsTargetContext(projectRoot, unixOnly, `["linux/amd64"]`),
		jsonl.New(), "@putnami/testext", "1.2.3", unixOnly, false); !ok {
		t.Fatal("a Unix-only matrix no longer packages a runtime the packager does not build")
	}

	outputRoot := t.TempDir()
	var ok bool
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		ok = createExtensionArchives(windowsTargetContext(projectRoot, outputRoot, `["linux/amd64","windows/amd64"]`),
			emit, "@putnami/testext", "1.2.3", outputRoot, false)
	})
	if ok {
		t.Fatal("createExtensionArchives packaged a Windows archive without a Windows build of its runtime")
	}
	diagnostics := errorDiagnostics(events)
	if len(diagnostics) != 1 ||
		!strings.HasPrefix(diagnostics[0], "windows-x64 archive: the project builds no Go binary") ||
		!strings.Contains(diagnostics[0], `"bin/run.exe"`) {
		t.Errorf("error diagnostics = %q, want one that names windows-x64 and bin/run.exe", diagnostics)
	}
	requireNoWindowsArchive(t, outputRoot)

	got := archiveDigests(t, filepath.Join(outputRoot, "archives"))["putnami-testext-linux-x64.tar.gz"]
	want := archiveDigests(t, filepath.Join(unixOnly, "archives"))["putnami-testext-linux-x64.tar.gz"]
	if got == "" || got != want {
		t.Errorf("linux-x64 archive digest = %q beside a Windows target, want the Unix-only %q", got, want)
	}
}

// A project that ships its runtime under the Windows name packages a Windows
// archive, and the archive marks that file executable because it is the
// declared runtime.
func TestWindowsArchiveCarriesARuntimeShippedUnderItsWindowsName(t *testing.T) {
	projectRoot := t.TempDir()
	outputRoot := t.TempDir()
	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), scriptRuntimeManifest)
	mustWrite(t, filepath.Join(projectRoot, "bin", "run"), "#!/bin/sh\n")
	mustWrite(t, filepath.Join(projectRoot, "bin", "run.exe"), "MZ")

	if ok := createExtensionArchives(windowsTargetContext(projectRoot, outputRoot, `["windows/amd64"]`),
		jsonl.New(), "@putnami/testext", "1.2.3", outputRoot, false); !ok {
		t.Fatal("createExtensionArchives refused a runtime shipped under its Windows name")
	}
	archive := packagedArchive(outputRoot, "windows-x64")
	if got := string(readArchiveEntries(t, archive)["bin/run.exe"]); got != "MZ" {
		t.Errorf("windows-x64 bin/run.exe = %q, want the shipped bytes", got)
	}
	if mode := archiveEntryMode(t, archive, "bin/run.exe"); mode&0o111 == 0 {
		t.Errorf("windows-x64 bin/run.exe mode = %o, want executable", mode)
	}
}

// A Windows CLI creates no link from an archive, so a Windows archive whose
// stage holds one is refused at package time and names it, for the shared base
// stage and for a platform's own stage. The link is not copied as its target:
// the Unix archive of the same run still carries it as a link.
func TestWindowsArchiveRefusesASymbolicLink(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withBinary bool
	}{
		{name: "shared stage", withBinary: false},
		{name: "platform stage", withBinary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(cpuBudgetEnv, "1")
			projectRoot := t.TempDir()
			outputRoot := t.TempDir()
			manifest := conformingManifest
			if tc.withBinary {
				manifest = binaryRuntimeManifest
				mustWrite(t, filepath.Join(projectRoot, "cmd", "putnami-go", "main.go"), "package main\nfunc main() {}\n")
				for _, target := range []struct{ goos, suffix string }{{"linux", "linux-x64"}, {"windows", "windows-x64"}} {
					mustWrite(t, filepath.Join(outputRoot, "build", "bin", target.suffix,
						pkgmeta.ExecutableName(target.goos, "putnami-go")), "runtime-"+target.suffix)
				}
			}
			mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), manifest)
			mustWrite(t, filepath.Join(projectRoot, "config", "defaults.json"), "{}\n")
			if err := os.Symlink("defaults.json", filepath.Join(projectRoot, "config", "current.json")); err != nil {
				t.Skipf("this host creates no symbolic link: %v", err)
			}

			var ok bool
			events := captureEvents(t, func(emit *jsonl.Emitter) {
				ok = createExtensionArchives(windowsTargetContext(projectRoot, outputRoot, `["linux/amd64","windows/amd64"]`),
					emit, "@putnami/testext", "1.2.3", outputRoot, false)
			})
			if ok {
				t.Fatal("createExtensionArchives packaged a Windows archive that holds a symbolic link")
			}
			diagnostics := errorDiagnostics(events)
			want := "windows-x64 archive entry config/current.json is a symbolic link to defaults.json: "
			if len(diagnostics) != 1 || !strings.HasPrefix(diagnostics[0], want) {
				t.Errorf("error diagnostics = %q, want one that starts with %q", diagnostics, want)
			}
			requireNoWindowsArchive(t, outputRoot)
			links := archiveLinks(t, packagedArchive(outputRoot, "linux-x64"))
			if got := links["config/current.json"]; got != "defaults.json" {
				t.Errorf("linux-x64 links = %v, want config/current.json -> defaults.json", links)
			}
		})
	}
}
