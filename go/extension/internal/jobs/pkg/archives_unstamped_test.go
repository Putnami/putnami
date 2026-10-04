package pkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	buildjob "go.putnami.dev/go/extension/internal/jobs/build"
	"go.putnami.dev/go/extension/internal/platform"
	runtimeproto "go.putnami.dev/protocol/runtime"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/runtimeinfo"
)

// unstampedRuntimeMain is a runtime with no version variable: like the Go
// and TypeScript extension runtimes, it reads any version it needs from its
// manifest, so nothing in its source names the build.
const unstampedRuntimeMain = "package main\n\nfunc main() { println(\"unstamped\") }\n"

// unstampedManifest declares the runtime the way a published extension
// manifest does: without a name, which the CLI takes from the reference that
// installs the extension.
const unstampedManifest = `{
	"cliContract": 4,
	"runtime": {"executable": "compiled/putnami-unstamped"},
	"commands": {
		"build": {"run": [{"id": "build", "task": "build-exec"}]}
	},
	"tasks": {
		"build-exec": {
			"kind": "command",
			"command": "{extensionRuntime}"
		}
	}
}`

// extensionManifestPath is this extension's own manifest, relative to the
// package directory the tests run in.
var extensionManifestPath = filepath.Join("..", "..", "..", "putnami.extension.json")

// crossCompileArgs returns the arguments this extension's build-cross-compile
// task passes its runtime after the build subcommand, read from the manifest
// that declares the task, so the test builds what the package pipeline builds.
func crossCompileArgs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(extensionManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Tasks map[string]struct {
			Args []string `json:"args"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	args := manifest.Tasks["build-cross-compile"].Args
	if len(args) == 0 || args[0] != "build" {
		t.Fatalf("build-cross-compile args = %q, want the build subcommand first", args)
	}
	return append([]string(nil), args[1:]...)
}

// gitIn runs git in dir with a fixed identity and no signing or hooks, so the
// commit succeeds whatever the machine's own git configuration says.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{
		"-c", "user.name=Putnami Test", "-c", "user.email=test@putnami.invalid",
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

// unstampedWorkspace is a repository holding one extension project, the way a
// build machine checks it out.
type unstampedWorkspace struct {
	root, project, output string
}

// newUnstampedWorkspace checks out the extension project, with manifest as its
// extension manifest and putnamiJSON as its project file, into a directory of
// its own, committed with readme as the only difference between checkouts.
func newUnstampedWorkspace(t *testing.T, manifest, putnamiJSON, readme string) unstampedWorkspace {
	t.Helper()
	root := t.TempDir()
	ws := unstampedWorkspace{
		root:    root,
		project: filepath.Join(root, "extension"),
		// The package step writes its archives below this root whatever the
		// context's output path is; the binaries share it, as in a real run.
		output: filepath.Join(root, ".putnami", "out", "extension", "package"),
	}
	mustWrite(t, filepath.Join(ws.project, "go.mod"), "module example.com/unstamped\n\ngo 1.24.0\n")
	mustWrite(t, filepath.Join(ws.project, "main.go"), unstampedRuntimeMain)
	mustWrite(t, filepath.Join(ws.project, "putnami.json"), putnamiJSON)
	mustWrite(t, filepath.Join(ws.project, "putnami.extension.json"), manifest)
	mustWrite(t, filepath.Join(root, "README.md"), readme)
	gitIn(t, root, "init", "-q")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", readme)
	return ws
}

// context returns the job context of a package run of the project at version
// full with params.
func (ws unstampedWorkspace) context(name, full string, params pctx.Params) *pctx.Context {
	return &pctx.Context{
		WorkspaceRoot: ws.root,
		OutputPath:    ws.output,
		Workspace:     pctx.Workspace{Version: full},
		Project: pctx.Project{
			Name:     name,
			Path:     "extension",
			FullPath: ws.project,
		},
		Params:  params,
		Version: &pctx.Version{Base: "0.3.1", Full: full},
	}
}

// buildUnstampedRuntime cross-compiles the project for the host with the
// arguments of the package pipeline's build-cross-compile task, and returns
// the binary.
func buildUnstampedRuntime(t *testing.T, ctx *pctx.Context) []byte {
	t.Helper()
	target := runtime.GOOS + "/" + runtime.GOARCH
	args := append(crossCompileArgs(t), "--target", target, "--entrypoint", ".")
	status, data, err := buildjob.Run(ctx, jsonl.New(), args)
	if err != nil || status != "OK" {
		t.Fatalf("cross-compile %q = (%q, %#v, %v), want OK", args, status, data, err)
	}
	binary, err := os.ReadFile(filepath.Join(ctx.OutputPath, "bin",
		platform.SuffixFromDockerPlatform(target), "putnami-unstamped"))
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// packageArchives stages binary as the binaryName runtime of every platform
// ctx packages, runs the archive packager, and returns the path of the host
// platform's archive.
func packageArchives(t *testing.T, ctx *pctx.Context, binaryName string, binary []byte) string {
	t.Helper()
	targets, err := releaseTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := toResolverArtifactName(ctx.Project.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, archiveTarget := range targets {
		targetBinary := filepath.Join(ctx.OutputPath, "bin", archiveTarget.Suffix,
			pkgmeta.ExecutableName(archiveTarget.GOOS, binaryName))
		mustWrite(t, targetBinary, string(binary))
		if err := os.Chmod(targetBinary, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	events := captureEvents(t, func(emit *jsonl.Emitter) {
		status, _, err := Run(ctx, emit, []string{"--archives"})
		if err != nil || status != "OK" {
			t.Errorf("package = (%q, %v), want OK", status, err)
		}
	})
	if t.Failed() {
		t.Fatalf("package events: %+v", events)
	}
	suffix := platform.SuffixFromDockerPlatform(runtime.GOOS + "/" + runtime.GOARCH)
	return filepath.Join(ctx.OutputPath, "archives", artifact+"-"+suffix+".tar.gz")
}

// A runtime built by the package pipeline with buildvcs false and no version
// variable is the same bytes on two machines, at two commits and two versions,
// and the archives packaged from it differ only in the version the manifest
// carries. That is what lets an installed extension's tree digest, which reads
// the manifest without its version, stay equal between canary builds of an
// unchanged implementation.
func TestUnstampedRuntimeArchivesDifferOnlyInTheManifestVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("archive packaging test requires tar and executable mode bits")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required to show the VCS stamp")
	}
	const putnamiJSON = `{
		"name": "@putnami/unstamped",
		"options": {"@putnami/go": {"entrypoint": "."}}
	}`
	unstamped := func() pctx.Params { return paramsWith(t, map[string]string{"buildvcs": "false"}) }
	// Two checkouts in two directories, as on two build machines: a binary
	// that embeds its build path differs between them.
	canaryWS := newUnstampedWorkspace(t, unstampedManifest, putnamiJSON, "first\n")
	nextWS := newUnstampedWorkspace(t, unstampedManifest, putnamiJSON, "second\n")

	canaryCtx := canaryWS.context("@putnami/unstamped", "0.3.1-20261003152506-4da6833", unstamped())
	canaryBinary := buildUnstampedRuntime(t, canaryCtx)
	canary := readArchiveEntries(t, packageArchives(t, canaryCtx, "putnami-unstamped", canaryBinary))

	nextCtx := nextWS.context("@putnami/unstamped", "0.3.1-20261004094924-a389c95", unstamped())
	nextBinary := buildUnstampedRuntime(t, nextCtx)
	next := readArchiveEntries(t, packageArchives(t, nextCtx, "putnami-unstamped", nextBinary))

	if !bytes.Equal(canaryBinary, nextBinary) {
		t.Fatal("an unstamped runtime changed bytes between two checkouts, commits and versions of the same sources")
	}
	if got, want := archiveLayout(next), archiveLayout(canary); !reflect.DeepEqual(got, want) {
		t.Fatalf("archive layouts differ: %v vs %v", got, want)
	}
	for name, content := range canary {
		if name == "putnami.extension.json" {
			continue
		}
		if !bytes.Equal(content, next[name]) {
			t.Errorf("archive entry %s differs between version-only builds", name)
		}
	}
	versionless := func(raw []byte) (map[string]any, string) {
		t.Helper()
		var manifest map[string]any
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		version, _ := manifest["version"].(string)
		delete(manifest, "version")
		return manifest, version
	}
	canaryManifest, canaryVersion := versionless(canary["putnami.extension.json"])
	nextManifest, nextVersion := versionless(next["putnami.extension.json"])
	if canaryVersion == nextVersion || canaryVersion == "" {
		t.Errorf("manifest versions = %q and %q, want the two stamped versions", canaryVersion, nextVersion)
	}
	if !reflect.DeepEqual(canaryManifest, nextManifest) {
		t.Errorf("manifests differ beyond their version:\n%v\n%v", canaryManifest, nextManifest)
	}

	// Without buildvcs false, Go stamps the commit into the binary, so the
	// same sources at the next commit are different bytes.
	stampedBefore := buildUnstampedRuntime(t, canaryWS.context("@putnami/unstamped", "0.3.1-20261004094924-a389c95", pctx.Params{}))
	gitIn(t, canaryWS.root, "commit", "-q", "--allow-empty", "-m", "third")
	stampedAfter := buildUnstampedRuntime(t, canaryWS.context("@putnami/unstamped", "0.3.1-20261004094924-a389c95", pctx.Params{}))
	if bytes.Equal(stampedBefore, stampedAfter) {
		t.Error("Go's default VCS stamp kept the bytes across commits; the test no longer shows what buildvcs false removes")
	}
}

// runtimeInfoHelperEnv names the extension TestRuntimeInfoHelperProcess
// answers the handshake for.
const runtimeInfoHelperEnv = "PUTNAMI_PKG_RUNTIME_INFO_HELPER"

// TestRuntimeInfoHelperProcess is not a test: it is an extension runtime that
// answers the runtime-info handshake the way the Go and TypeScript extension
// runtimes do, and it skips unless a test started it as one.
func TestRuntimeInfoHelperProcess(t *testing.T) {
	extension := os.Getenv(runtimeInfoHelperEnv)
	if extension == "" {
		t.Skip("not the helper process")
	}
	handled, err := runtimeinfo.HandleFromManifest(
		[]string{runtimeinfo.Verb, runtimeinfo.Command}, os.Stdout, extension)
	if !handled || err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// extractArchive writes the regular files and directories of a .tar.gz
// archive below dest with the modes the archive records, as an install does.
func extractArchive(t *testing.T, archivePath, dest string) {
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
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." {
			continue
		}
		if !filepath.IsLocal(name) {
			t.Fatalf("archive entry %q leaves the archive", header.Name)
		}
		target := filepath.Join(dest, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode).Perm())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				t.Fatal(err)
			}
			if err := out.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The runtime of an extracted archive answers the runtime-info handshake with
// the version the packager stamped into the manifest beside it. The archive is
// packaged from this extension's own manifest, which names no extension, the
// shape every installed Go extension has; the CLI refuses a runtime whose
// answer differs from that manifest's version.
func TestPackagedRuntimeAnswersWithTheStampedManifestVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("archive packaging test requires tar and executable mode bits")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for the packaged checkout")
	}
	data, err := os.ReadFile(extensionManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	// The packager stamps a version and nothing that names the extension.
	delete(manifest, "name")
	published, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	ws := newUnstampedWorkspace(t, string(published), `{
		"name": "@putnami/go",
		"options": {"@putnami/go": {"entrypoint": "."}}
	}`, "first\n")
	const version = "0.3.1-20261004094924-a389c95"
	host := runtime.GOOS + "/" + runtime.GOARCH
	ctx := ws.context("@putnami/go", version, paramsWith(t, map[string]string{"platforms": host}))

	// The runtime is this test binary, which answers the handshake through
	// TestRuntimeInfoHelperProcess.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	installed := t.TempDir()
	extractArchive(t, packageArchives(t, ctx, "putnami-go", binary), installed)

	stamped := readArchiveManifestVersion(t, filepath.Join(installed, "putnami.extension.json"))
	if stamped != version {
		t.Fatalf("packaged manifest version = %q, want %q", stamped, version)
	}
	executable := filepath.Join(installed, filepath.FromSlash(pkgmeta.ExecutableName(runtime.GOOS, "compiled/putnami-go")))
	cmd := exec.Command(executable, "-test.run=^TestRuntimeInfoHelperProcess$")
	cmd.Env = append(os.Environ(), runtimeInfoHelperEnv+"=@putnami/go")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("packaged runtime-info: %v\n%s", err, stderr.String())
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	var info runtimeproto.Info
	if err := decoder.Decode(&info); err != nil {
		t.Fatalf("decode runtime info: %v\n%s", err, stdout.String())
	}
	if info.Extension != "@putnami/go" || info.Version != stamped {
		t.Fatalf("packaged runtime answered %q@%q, want @putnami/go@%q from its manifest",
			info.Extension, info.Version, stamped)
	}
}

// readArchiveManifestVersion returns the top-level version of the extension
// manifest at path.
func readArchiveManifestVersion(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "" {
		t.Fatalf("packaged manifest names %q; the published shape names no extension", manifest.Name)
	}
	return manifest.Version
}
