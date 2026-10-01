package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/exec"
)

// On a Windows host the managed pack gives the archive the modes a Unix host
// packs before it takes the digest: the digest the registry is asked to hold
// is the digest of the archive the PUT carries.
func TestPackNPMArtifact_WindowsHostPacksTheUnixModes(t *testing.T) {
	originalGOOS, originalBun, originalRun := npmGOOS, resolveBunBin, npmExecRun
	t.Cleanup(func() { npmGOOS, resolveBunBin, npmExecRun = originalGOOS, originalBun, originalRun })
	npmGOOS = "windows"
	resolveBunBin = func() (string, error) { return managedTestBun, nil }

	workspace := t.TempDir()
	projectPath := "packages/probe"
	packageRoot := filepath.Join(workspace, ".putnami", "out", projectPath, "package")
	for path, content := range map[string]string{
		filepath.Join(packageRoot, "lib", "cli.js"):       "#!/usr/bin/env node\n",
		filepath.Join(packageRoot, "lib", "index.js"):     "export {};\n",
		filepath.Join(packageRoot, "npm", "package.json"): `{"name":"@test/probe","version":"1.0.0","bin":{"probe":"./bin/probe.js"}}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// bun on Windows writes 0666 for a file and 0777 for a bin entry.
	windowsModes := map[string]int64{
		"package/package.json": 0o666,
		"package/bin/probe.js": 0o777,
		"package/cli.js":       0o666,
		"package/index.js":     0o666,
	}
	npmExecRun = func(name string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if name != managedTestBun || len(args) < 2 || args[0] != "pm" || args[1] != "pack" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		var tarball bytes.Buffer
		writer := tar.NewWriter(&tarball)
		for _, name := range []string{"package/package.json", "package/bin/probe.js", "package/cli.js", "package/index.js"} {
			header := &tar.Header{Name: name, Mode: windowsModes[name], Size: 1, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		var compressed bytes.Buffer
		gz := gzip.NewWriter(&compressed)
		if _, err := gz.Write(tarball.Bytes()); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		destination := flagValue(args, "--destination")
		if err := os.WriteFile(filepath.Join(destination, "test-probe-1.0.0.tgz"), compressed.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return &exec.Result{Success: true}, nil
	}

	artifact, digest, cleanup, err := packNPMArtifact(&managedNPMConfig{}, workspace, projectPath, filepath.Join(packageRoot, "npm"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if want, err := sha256File(artifact); err != nil || digest != want {
		t.Fatalf("digest = %s, want the digest of the published archive %s (err %v)", digest, want, err)
	}
	data, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(gz)
	got := map[string]int64{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[header.Name] = header.Mode
	}
	for name, want := range map[string]int64{
		"package/package.json": 0o644,
		"package/bin/probe.js": 0o755,
		"package/cli.js":       0o755,
		"package/index.js":     0o644,
	} {
		if got[name] != want {
			t.Errorf("%s mode = %o, want %o", name, got[name], want)
		}
	}
}
