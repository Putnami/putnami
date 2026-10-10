package cloudcli

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestCompiledArchiveInstallsAgentWiring proves the release artifact does not
// depend on repository source files: build the production entrypoint with the
// workspace disabled, place only that binary in the release archive layout,
// extract it, and run cloud install with no putnami executable on PATH.
func TestCompiledArchiveInstallsAgentWiring(t *testing.T) {
	if testing.Short() {
		t.Skip("compiled release archive test")
	}
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	binaryName := "putnami-cloud"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	built := filepath.Join(staging, binaryName)
	build := exec.Command("go", "build", "-o", built, "./cmd/putnami-cloud")
	build.Dir = moduleRoot
	build.Env = append(os.Environ(), "GOWORK=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build compiled CLI: %v\n%s", err, output)
	}

	archive := filepath.Join(staging, "putnami-cloud-test.tar.gz")
	writeCompiledArchive(t, archive, built, binaryName)
	extracted := filepath.Join(t.TempDir(), "compiled", binaryName)
	extractCompiledBinary(t, archive, extracted, binaryName)

	runtimeInfo := exec.Command(extracted, "__putnami", "runtime-info")
	runtimeInfo.Env = []string{"PATH="}
	runtimeInfoOutput, err := runtimeInfo.Output()
	if err != nil {
		t.Fatalf("compiled runtime-info: %v", err)
	}
	var descriptor runtimeInfoResponse
	if err := json.Unmarshal(runtimeInfoOutput, &descriptor); err != nil {
		t.Fatalf("decode compiled runtime-info: %v\n%s", err, runtimeInfoOutput)
	}
	if descriptor.Extension != cloudRuntimeIdentity || descriptor.Version != cloudRuntimeVersion {
		t.Fatalf("compiled runtime-info identity = %+v", descriptor)
	}
	if descriptor.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("compiled runtime-info platform = %q", descriptor.Platform)
	}
	if descriptor.CLIContract != cloudCLIContract || descriptor.RuntimeProtocol != cloudRuntimeProtocol || descriptor.RuntimeABI != cloudRuntimeABI {
		t.Fatalf("compiled runtime-info contract = %+v", descriptor)
	}

	workspace := t.TempDir()
	manifest := map[string]any{
		"name": "archive-fixture",
		"options": map[string]any{"@putnami/cloud": map[string]any{
			"workspace": map[string]any{"workspace_id": "ws-archive", "control_plane_url": "https://api.putnami.cloud"},
			"intelligence": map[string]any{
				"enabled": true, "workspace": "archive-fixture",
			},
		}},
	}
	manifestData, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(workspace, "putnami.workspace.json"), manifestData, 0o644); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	install := exec.Command(extracted, "install", "--no-cache")
	install.Dir = workspace
	install.Env = []string{
		"HOME=" + home,
		"PUTNAMI_HOME=" + filepath.Join(home, ".putnami"),
		"PUTNAMI_WORKSPACE_ROOT=" + workspace,
		"PATH=",
	}
	if output, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install from compiled archive: %v\n%s", err, output)
	}
	// The agent wiring belongs to @putnami/intelligence: even an explicit
	// Intelligence opt-in leaves the Cloud installer's hands off host files.
	for _, relative := range []string{".mcp.json", ".codex/config.toml"} {
		if _, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(relative))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("compiled Cloud installer wrote %s: %v", relative, err)
		}
	}
}

// TestRuntimePrepareBuildsCompiledRuntime proves the source-checkout prepare
// recipe does not depend on the caller's cwd and produces the same executable
// identity document the packaged runtime exposes.
func TestRuntimePrepareBuildsCompiledRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("compiled runtime prepare test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("runtime prepare script requires a POSIX shell")
	}
	moduleRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	output := t.TempDir()
	prepare := exec.Command(filepath.Join(moduleRoot, "bin", "prepare-runtime"), "--output", output)
	prepare.Dir = t.TempDir()
	prepare.Env = append(os.Environ(), "GOWORK=off")
	if logs, err := prepare.CombinedOutput(); err != nil {
		t.Fatalf("prepare compiled runtime: %v\n%s", err, logs)
	}

	compiled := filepath.Join(output, "compiled", "putnami-cloud")
	info := exec.Command(compiled, "__putnami", "runtime-info")
	info.Env = []string{"PATH="}
	data, err := info.Output()
	if err != nil {
		t.Fatalf("prepared runtime-info: %v", err)
	}
	var descriptor runtimeInfoResponse
	if err := json.Unmarshal(data, &descriptor); err != nil {
		t.Fatalf("decode prepared runtime-info: %v\n%s", err, data)
	}
	if descriptor.Extension != cloudRuntimeIdentity || descriptor.Version != cloudRuntimeVersion {
		t.Fatalf("prepared runtime-info identity = %+v", descriptor)
	}
	if descriptor.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("prepared runtime-info platform = %q", descriptor.Platform)
	}
	if descriptor.CLIContract != cloudCLIContract || descriptor.RuntimeProtocol != cloudRuntimeProtocol || descriptor.RuntimeABI != cloudRuntimeABI {
		t.Fatalf("prepared runtime-info contract = %+v", descriptor)
	}
}

func writeCompiledArchive(t *testing.T, archive, binary, binaryName string) {
	t.Helper()
	out, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	header := &tar.Header{Name: filepath.ToSlash(filepath.Join("compiled", binaryName)), Mode: 0o755, Size: int64(len(data))}
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func extractCompiledBinary(t *testing.T, archive, target, binaryName string) {
	t.Helper()
	in, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	want := filepath.ToSlash(filepath.Join("compiled", binaryName))
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != want {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			t.Fatal(err)
		}
		if err := out.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("%s missing from archive", want)
}
