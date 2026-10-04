package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// installAsRuntime places the running test binary at the runtime path
// declared, relative to root, the way an extracted archive holds the runtime,
// and returns that path. A hard link avoids copying the binary; the copy is
// the fallback when the two directories are on different file systems.
func installAsRuntime(t *testing.T, root, declared string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, filepath.FromSlash(pkgmeta.ExecutableName(runtime.GOOS, declared)))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(self, target); err == nil {
		return target
	}
	source, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	copied, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(copied, source); err != nil {
		copied.Close()
		t.Fatal(err)
	}
	if err := copied.Close(); err != nil {
		t.Fatal(err)
	}
	return target
}

// An installed runtime answers the handshake with the version of the manifest
// it is installed under, in the shape this extension publishes: its own source
// manifest, which names no extension, with the version the packager stamps.
// The CLI refuses a runtime whose answer differs from that manifest's version,
// so a runtime that ignored a manifest without a name would fail every task.
func TestInstalledRuntimeAnswersWithItsPublishedManifestVersion(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "putnami.extension.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	// The packager stamps a version and nothing that names the extension.
	const stamped = "0.3.1-20261004094924-a389c95"
	delete(manifest, "name")
	manifest["version"] = stamped
	declaration, _ := manifest["runtime"].(map[string]any)
	declared, _ := declaration["executable"].(string)
	if declared == "" {
		t.Fatal("the extension manifest declares no runtime executable")
	}
	stampedManifest, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "putnami.extension.json"), stampedManifest, 0o644); err != nil {
		t.Fatal(err)
	}
	executable := installAsRuntime(t, root, declared)

	args, err := json.Marshal([]string{"__putnami", "runtime-info"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestMainHelperProcess$")
	cmd.Env = append(os.Environ(), mainHelperArgs+"="+string(args))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("installed runtime-info: %v\n%s", err, stderr.String())
	}
	// Decoded the way the CLI decodes it: one known document, nothing after.
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	decoder.DisallowUnknownFields()
	var info runtimeproto.Info
	if err := decoder.Decode(&info); err != nil {
		t.Fatalf("decode runtime info: %v\n%s", err, stdout.String())
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("runtime info carries trailing data: %s", stdout.String())
	}
	if info.Extension != goExtensionName || info.Version != stamped {
		t.Fatalf("installed runtime answered %q@%q, want %q@%q from its manifest",
			info.Extension, info.Version, goExtensionName, stamped)
	}
}
