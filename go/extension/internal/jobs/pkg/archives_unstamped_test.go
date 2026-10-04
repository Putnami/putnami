package pkg

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	buildjob "go.putnami.dev/go/extension/internal/jobs/build"
	"go.putnami.dev/go/extension/internal/platform"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// unstampedRuntimeMain is a runtime with no version variable: like the Go
// and TypeScript extension runtimes, it reads any version it needs from its
// manifest, so nothing in its source names the build.
const unstampedRuntimeMain = "package main\n\nfunc main() { println(\"unstamped\") }\n"

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

// buildUnstampedRuntime cross-compiles the project for the host, the way the
// package pipeline's build-cross-compile task does, and returns the binary.
func buildUnstampedRuntime(t *testing.T, ctx *pctx.Context) []byte {
	t.Helper()
	target := runtime.GOOS + "/" + runtime.GOARCH
	status, data, err := buildjob.Run(ctx, jsonl.New(), []string{
		"--phase", "cross-compile", "--target", target, "--entrypoint", ".",
		"--trimpath", "--cgo", "false",
	})
	if err != nil || status != "OK" {
		t.Fatalf("cross-compile = (%q, %#v, %v), want OK", status, data, err)
	}
	binary, err := os.ReadFile(filepath.Join(ctx.OutputPath, "bin",
		platform.SuffixFromDockerPlatform(target), "putnami-unstamped"))
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

// A runtime built with buildvcs false and no version variable is the same
// bytes at two commits and two versions, and the archives packaged from it
// differ only in the version the manifest carries. That is what lets an
// installed extension's tree digest, which reads the manifest without its
// version, stay equal between canary builds of an unchanged implementation.
func TestUnstampedRuntimeArchivesDifferOnlyInTheManifestVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("archive packaging test requires tar and executable mode bits")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required to show the VCS stamp")
	}
	workspaceRoot := t.TempDir()
	projectRoot := filepath.Join(workspaceRoot, "extension")
	// The package step writes its archives below this root whatever the
	// context's output path is; the binaries share it, as in a real run.
	outputRoot := filepath.Join(workspaceRoot, ".putnami", "out", "extension", "package")
	mustWrite(t, filepath.Join(projectRoot, "go.mod"), "module example.com/unstamped\n\ngo 1.24.0\n")
	mustWrite(t, filepath.Join(projectRoot, "main.go"), unstampedRuntimeMain)
	mustWrite(t, filepath.Join(projectRoot, "putnami.json"), `{
		"name": "@putnami/unstamped",
		"options": {"@putnami/go": {"entrypoint": "."}}
	}`)
	mustWrite(t, filepath.Join(projectRoot, "putnami.extension.json"), `{
		"name": "@putnami/unstamped",
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
	}`)
	mustWrite(t, filepath.Join(workspaceRoot, "README.md"), "first\n")
	gitIn(t, workspaceRoot, "init", "-q")
	gitIn(t, workspaceRoot, "add", "-A")
	gitIn(t, workspaceRoot, "commit", "-q", "-m", "first")

	contextAt := func(full string, params pctx.Params) *pctx.Context {
		return &pctx.Context{
			WorkspaceRoot: workspaceRoot,
			OutputPath:    outputRoot,
			Workspace:     pctx.Workspace{Version: full},
			Project: pctx.Project{
				Name:     "@putnami/unstamped",
				Path:     "extension",
				FullPath: projectRoot,
			},
			Params:  params,
			Version: &pctx.Version{Base: "0.3.1", Full: full},
		}
	}
	unstamped := func() pctx.Params { return paramsWith(t, map[string]string{"buildvcs": "false"}) }
	packageArchive := func(ctx *pctx.Context, binary []byte) map[string][]byte {
		t.Helper()
		for _, archiveTarget := range platform.ArchivePlatforms {
			targetBinary := filepath.Join(ctx.OutputPath, "bin", archiveTarget.Suffix,
				pkgmeta.ExecutableName(archiveTarget.GOOS, "putnami-unstamped"))
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
		return readArchiveEntries(t, filepath.Join(outputRoot, "archives", "putnami-unstamped-"+suffix+".tar.gz"))
	}

	canaryCtx := contextAt("0.3.1-20261003152506-4da6833", unstamped())
	canaryBinary := buildUnstampedRuntime(t, canaryCtx)
	canary := packageArchive(canaryCtx, canaryBinary)

	mustWrite(t, filepath.Join(workspaceRoot, "README.md"), "second\n")
	gitIn(t, workspaceRoot, "commit", "-q", "-am", "second")
	nextCtx := contextAt("0.3.1-20261004094924-a389c95", unstamped())
	nextBinary := buildUnstampedRuntime(t, nextCtx)
	next := packageArchive(nextCtx, nextBinary)

	if !bytes.Equal(canaryBinary, nextBinary) {
		t.Fatal("an unstamped runtime changed bytes between two commits and two versions of the same sources")
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
	stampedBefore := buildUnstampedRuntime(t, contextAt("0.3.1-20261004094924-a389c95", pctx.Params{}))
	gitIn(t, workspaceRoot, "commit", "-q", "--allow-empty", "-m", "third")
	stampedAfter := buildUnstampedRuntime(t, contextAt("0.3.1-20261004094924-a389c95", pctx.Params{}))
	if bytes.Equal(stampedBefore, stampedAfter) {
		t.Error("Go's default VCS stamp kept the bytes across commits; the test no longer shows what buildvcs false removes")
	}
}
