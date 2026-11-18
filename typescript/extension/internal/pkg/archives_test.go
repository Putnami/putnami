package pkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// archiveTestManifest names a task script and a hook script under
// {extensionRoot} without their extension, and a template directory.
const archiveTestManifest = `{
	"name": "@putnami/testext",
	"commands": {
		"sync": {"run": [{"id": "s", "task": "sync"}]}
	},
	"tasks": {
		"sync": {"kind": "command", "command": "bun", "args": ["run", "{extensionRoot}/bin/sync", "--templates", "{extensionRoot}/templates"]}
	},
	"hooks": {
		"preBuild": {"kind": "command", "command": "bun", "args": ["run", "{extensionRoot}/bin/generate"], "cwd": "{projectRoot}"}
	}
}`

// archiveWorkspace writes a workspace holding one extension project at
// ext/ with the given manifest, and returns the workspace root.
func archiveWorkspace(t *testing.T, manifest string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"LICENSE.md":                  "license\n",
		"ext/putnami.extension.json":  manifest,
		"ext/README.md":               "# testext\n",
		"ext/bin/sync.ts":             "console.log('sync');\n",
		"ext/bin/generate.ts":         "console.log('generate');\n",
		"ext/templates/page.tsx.tmpl": "page\n",
		"ext/src/unreferenced.ts":     "export {};\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// fakeBundle writes a bundle that records its source's name.
func fakeBundle(_, source, out string) error {
	return os.WriteFile(out, []byte("// bundle of "+filepath.Base(source)+"\n"), 0o644)
}

func archiveParams(root string) ArchivesParams {
	return ArchivesParams{
		WorkspaceRoot: root,
		ProjectPath:   "ext",
		PackageName:   "@putnami/testext",
		Version:       "1.2.3-abc",
		OutputRoot:    filepath.Join(root, "out"),
		Bundle:        fakeBundle,
	}
}

func tarEntries(t *testing.T, archive string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(gz)
	entries := map[string]string{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries[strings.TrimPrefix(header.Name, "./")] = string(content)
	}
}

// TestPackageArchives_HoldsEveryFileItsManifestRuns packages an extension
// whose manifest names scripts and a directory under {extensionRoot}: every
// platform archive holds the same bytes, the bundles of the scripts under the
// names Bun resolves, the directory, the stamped manifest and the license,
// and nothing else of the project.
func TestPackageArchives_HoldsEveryFileItsManifestRuns(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "extension-archive", "an-extension-archive-holds-every-file-its-manifest-runs")
	root := archiveWorkspace(t, archiveTestManifest)
	result, err := PackageArchives(archiveParams(root))
	if err != nil {
		t.Fatalf("PackageArchives: %v", err)
	}

	wantFiles := []string{"LICENSE.md", "README.md", "bin/generate.js", "bin/sync.js", "putnami.extension.json", "templates/page.tsx.tmpl"}
	if !reflect.DeepEqual(result.Files, wantFiles) {
		t.Errorf("files = %v, want %v", result.Files, wantFiles)
	}
	if result.Artifact != "putnami-testext" {
		t.Errorf("artifact = %q, want putnami-testext", result.Artifact)
	}
	if len(result.Archives) != len(pkgmeta.ArchivePlatformSuffixes()) {
		t.Fatalf("archives = %v, want one per platform %v", result.Archives, pkgmeta.ArchivePlatformSuffixes())
	}

	first, err := os.ReadFile(result.Archives[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, archive := range result.Archives[1:] {
		other, err := os.ReadFile(archive)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, other) {
			t.Errorf("%s differs from %s", filepath.Base(archive), filepath.Base(result.Archives[0]))
		}
	}
	entries := tarEntries(t, result.Archives[0])
	var names []string
	for name := range entries {
		names = append(names, name)
	}
	if len(names) != len(wantFiles) {
		t.Errorf("archive entries = %v, want %v", names, wantFiles)
	}
	if entries["bin/sync.js"] != "// bundle of sync.ts\n" {
		t.Errorf("bin/sync.js = %q, want the bundle of bin/sync.ts", entries["bin/sync.js"])
	}
	var manifest struct {
		Version     string `json:"version"`
		CLIContract int    `json:"cliContract"`
	}
	if err := json.Unmarshal([]byte(entries["putnami.extension.json"]), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "1.2.3-abc" || manifest.CLIContract == 0 {
		t.Errorf("archived manifest = %+v, want the version stamped and the contract gated", manifest)
	}

	var record pkgmeta.ChannelRecord
	data, err := os.ReadFile(filepath.Join(root, "out", ArchivesChannel, "channel.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Artifact != "putnami-testext" || record.Version != "1.2.3-abc" || !reflect.DeepEqual(record.Channels, []string{ArchivesChannel}) {
		t.Errorf("channel record = %+v", record)
	}
	if !fileExistsAt(pkgmeta.MetadataPath(root, "ext")) {
		t.Error("the archive publication manifest is missing")
	}
}

// TestPackageArchives_DryRunWritesNothing reports the archives it would write
// and writes none of them.
func TestPackageArchives_DryRunWritesNothing(t *testing.T) {
	root := archiveWorkspace(t, archiveTestManifest)
	params := archiveParams(root)
	params.DryRun = true
	result, err := PackageArchives(params)
	if err != nil {
		t.Fatalf("PackageArchives: %v", err)
	}
	if len(result.Archives) == 0 || len(result.Files) == 0 {
		t.Fatalf("dry run reports nothing: %+v", result)
	}
	if fileExistsAt(filepath.Join(root, "out")) {
		t.Error("a dry run wrote the output directory")
	}
}

// TestPackageArchives_RefusesAManifestNamingAFileTheProjectLacks fails
// packaging, naming where the manifest names the file.
func TestPackageArchives_RefusesAManifestNamingAFileTheProjectLacks(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "extension-archive", "a-manifest-naming-a-file-the-archive-lacks-fails-packaging")
	manifest := strings.Replace(archiveTestManifest, "{extensionRoot}/bin/generate", "{extensionRoot}/bin/missing", 1)
	root := archiveWorkspace(t, manifest)
	_, err := PackageArchives(archiveParams(root))
	if err == nil {
		t.Fatal("PackageArchives packaged a manifest that names a file the project lacks")
	}
	for _, want := range []string{"hooks.preBuild.args[1]", "{extensionRoot}/bin/missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if fileExistsAt(filepath.Join(root, "out")) {
		t.Error("a refused packaging wrote the output directory")
	}
}

// TestPackageArchives_RefusesAScriptNamedWithItsExtension refuses a manifest
// that names a TypeScript source by its extension: the archive holds its
// bundle under another name.
func TestPackageArchives_RefusesAScriptNamedWithItsExtension(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "extension-archive", "a-script-named-with-its-typescript-extension-fails-packaging")
	manifest := strings.Replace(archiveTestManifest, "{extensionRoot}/bin/sync", "{extensionRoot}/bin/sync.ts", 1)
	root := archiveWorkspace(t, manifest)
	_, err := PackageArchives(archiveParams(root))
	if err == nil || !strings.Contains(err.Error(), "name the script without its extension (bin/sync)") {
		t.Fatalf("err = %v, want a refusal naming bin/sync", err)
	}
}

// TestPackageArchives_ReportsABundleFailure names the script whose bundle
// failed.
func TestPackageArchives_ReportsABundleFailure(t *testing.T) {
	root := archiveWorkspace(t, archiveTestManifest)
	params := archiveParams(root)
	params.Bundle = func(_, source, _ string) error {
		return errors.New("cannot resolve './missing'")
	}
	_, err := PackageArchives(params)
	if err == nil || !strings.Contains(err.Error(), "bundle generate.ts: cannot resolve './missing'") {
		t.Fatalf("err = %v, want the bundle failure of generate.ts", err)
	}
}

// TestPackageArchives_RefusesAProjectWithoutAnExtensionManifest names the
// project.
func TestPackageArchives_RefusesAProjectWithoutAnExtensionManifest(t *testing.T) {
	root := archiveWorkspace(t, archiveTestManifest)
	if err := os.Remove(filepath.Join(root, "ext", "putnami.extension.json")); err != nil {
		t.Fatal(err)
	}
	_, err := PackageArchives(archiveParams(root))
	if err == nil || !strings.Contains(err.Error(), "ext has no putnami.extension.json") {
		t.Fatalf("err = %v, want a refusal naming ext", err)
	}
}

func TestExtensionReferences(t *testing.T) {
	refs, err := ExtensionReferences([]byte(archiveTestManifest))
	if err != nil {
		t.Fatal(err)
	}
	want := []ExtensionReference{
		{Path: "bin/generate", Location: "hooks.preBuild.args[1]"},
		{Path: "bin/sync", Location: "tasks.sync.args[1]"},
		{Path: "templates", Location: "tasks.sync.args[3]"},
	}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("references = %+v, want %+v", refs, want)
	}

	for name, value := range map[string]string{
		"no slash":       `{extensionRoot}bin`,
		"a placeholder":  `{extensionRoot}/bin/{projectName}`,
		"outside":        `{extensionRoot}/../bin`,
		"the root alone": `{extensionRoot}/`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ExtensionReferences([]byte(`{"tasks": {"t": {"args": ["` + value + `"]}}}`)); err == nil {
				t.Errorf("ExtensionReferences accepted %q", value)
			}
		})
	}

	refs, err = ExtensionReferences([]byte(`{"tasks": {"t": {"command": "{extensionRoot}", "cwd": "{projectRoot}"}}}`))
	if err != nil || len(refs) != 0 {
		t.Errorf("the root itself is no reference: %+v, %v", refs, err)
	}
	if _, err := ExtensionReferences([]byte(`{`)); err == nil {
		t.Error("ExtensionReferences accepted malformed JSON")
	}
}

func TestVerifyExtensionReferences(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(archiveTestManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	err := VerifyExtensionReferences(dir)
	if err == nil {
		t.Fatal("VerifyExtensionReferences accepted an empty extension directory")
	}
	for _, want := range []string{"the extension archive lacks what its manifest runs", "tasks.sync.args[1] names {extensionRoot}/bin/sync", "tasks.sync.args[3] names {extensionRoot}/templates"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}

	for rel, content := range map[string]string{"bin/sync.js": "", "bin/generate.ts": "", "templates/a": ""} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := VerifyExtensionReferences(dir); err != nil {
		t.Errorf("VerifyExtensionReferences: %v", err)
	}
	if err := VerifyExtensionReferences(t.TempDir()); err == nil {
		t.Error("VerifyExtensionReferences accepted a directory without a manifest")
	}
}

func TestArchiveArtifactName(t *testing.T) {
	for name, want := range map[string]string{"@putnami/web": "putnami-web", "plain": "plain"} {
		got, err := archiveArtifactName(name)
		if err != nil || got != want {
			t.Errorf("archiveArtifactName(%q) = %q, %v, want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"", "@", "a b", "a/../b"} {
		if _, err := archiveArtifactName(name); err == nil {
			t.Errorf("archiveArtifactName(%q) accepted an invalid name", name)
		}
	}
}

// TestBundleScript runs Bun's bundler for one self-contained ES module from
// the project root, and reports its failure.
func TestBundleScript(t *testing.T) {
	originalRun, originalResolve := execRunBun, resolveBunBinary
	t.Cleanup(func() { execRunBun, resolveBunBinary = originalRun, originalResolve })
	resolveBunBinary = func() (string, error) { return "/opt/bun", nil }

	dir := t.TempDir()
	out := filepath.Join(dir, "bin", "sync.js")
	var gotName string
	var gotArgs []string
	execRunBun = func(name string, args []string, _ ...exec.Option) (*exec.Result, error) {
		gotName, gotArgs = name, args
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return nil, err
		}
		return &exec.Result{Success: true}, os.WriteFile(out, []byte("bundle"), 0o644)
	}
	if err := bundleScript(dir, "bin/sync.ts", out); err != nil {
		t.Fatalf("bundleScript: %v", err)
	}
	wantArgs := []string{"build", "--target=bun", "--format=esm", "--env=disable", "--outfile=" + out, "bin/sync.ts"}
	if gotName != "/opt/bun" || !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("ran %s %v, want /opt/bun %v", gotName, gotArgs, wantArgs)
	}

	execRunBun = func(string, []string, ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: false, Stderr: "Could not resolve: \"./missing\""}, nil
	}
	if err := bundleScript(dir, "bin/sync.ts", out); err == nil || !strings.Contains(err.Error(), "Could not resolve") {
		t.Errorf("err = %v, want Bun's error", err)
	}

	execRunBun = func(string, []string, ...exec.Option) (*exec.Result, error) {
		return &exec.Result{Success: true}, nil
	}
	if err := bundleScript(dir, "bin/sync.ts", filepath.Join(dir, "none.js")); err == nil || !strings.Contains(err.Error(), "bun wrote no none.js") {
		t.Errorf("err = %v, want a missing bundle", err)
	}

	resolveBunBinary = func() (string, error) { return "", errors.New("no bun") }
	if err := bundleScript(dir, "bin/sync.ts", out); err == nil {
		t.Error("bundleScript ran without a Bun")
	}
}

func fileExistsAt(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
