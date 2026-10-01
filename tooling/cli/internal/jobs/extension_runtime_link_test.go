package jobs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
)

// A local extension root reached through a directory link, a symbolic link on
// Unix and a junction on Windows, is read as the directory it names: the
// collected inputs, the digest and the staged view are the ones the directory
// itself gives.
func TestRuntimeRootBehindADirectoryLinkIsReadAsItsDirectory(t *testing.T) {
	extensionRoot, _ := writeRuntimeReplacementFixture(t, t.TempDir())
	link := filepath.Join(t.TempDir(), "extension")
	if err := dirlink.Create(extensionRoot, link); err != nil {
		t.Fatal(err)
	}
	direct := runtimeTestExtension(extensionRoot)
	direct.Runtime.Prepare.Inputs = append(direct.Runtime.Prepare.Inputs, "go.mod")
	linked := runtimeTestExtension(link)
	linked.Runtime.Prepare.Inputs = direct.Runtime.Prepare.Inputs

	want, err := collectRuntimeInputs(extensionRoot, direct.Runtime.Prepare.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	got, err := collectRuntimeInputs(link, linked.Runtime.Prepare.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(want, "cmd/main.go") || !slices.Equal(got, want) {
		t.Fatalf("inputs through the link = %v, want %v (with cmd/main.go)", got, want)
	}

	wantDigest, err := extensionRuntimeDigest(direct)
	if err != nil {
		t.Fatal(err)
	}
	gotDigest, err := extensionRuntimeDigest(linked)
	if err != nil {
		t.Fatal(err)
	}
	if gotDigest != wantDigest {
		t.Fatalf("digest through the link = %s, want the directory's %s", gotDigest, wantDigest)
	}

	staged, err := stageRuntimeSourceView(link, t.TempDir(), got)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{filepath.Join(staged, "cmd", "main.go"), filepath.Join(filepath.Dir(staged), "sdk", "sdk.go")} {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("staging through the link left out %s: %v", file, err)
		}
	}
}

// A replaced module whose directory is a directory link is hashed as the
// directory it names, so an edit behind the link changes the digest.
func TestRuntimeReplacementBehindADirectoryLinkIsDigested(t *testing.T) {
	root := t.TempDir()
	extensionRoot, sdkRoot := writeRuntimeReplacementFixture(t, root)
	physicalSDK := filepath.Join(root, "sdk-source")
	if err := os.Rename(sdkRoot, physicalSDK); err != nil {
		t.Fatal(err)
	}
	if err := dirlink.Create(physicalSDK, sdkRoot); err != nil {
		t.Fatal(err)
	}
	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")

	before, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteRuntimeFile(t, filepath.Join(physicalSDK, "sdk.go"), "package sdk\n\nconst Edited = true\n", 0o644)
	after, err := extensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("an edit behind the replaced module's directory link did not change the runtime digest")
	}
}

// A replaced module whose directory is a directory link to another tree is
// staged where the extension's go.mod names it, next to the staged extension,
// not at the link target's position.
func TestRuntimeReplacementBehindADirectoryLinkIsStagedAtItsGoModPath(t *testing.T) {
	root := t.TempDir()
	extensionRoot, sdkRoot := writeRuntimeReplacementFixture(t, root)
	physicalSDK := filepath.Join(t.TempDir(), "sdk-source")
	if err := os.Rename(sdkRoot, physicalSDK); err != nil {
		t.Fatal(err)
	}
	if err := dirlink.Create(physicalSDK, sdkRoot); err != nil {
		t.Fatal(err)
	}
	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
	inputs, err := collectRuntimeInputs(extensionRoot, ext.Runtime.Prepare.Inputs)
	if err != nil {
		t.Fatal(err)
	}

	staged, err := stageRuntimeSourceView(extensionRoot, t.TempDir(), inputs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(staged, "..", "sdk", "sdk.go")); err != nil {
		t.Fatalf("the replacement is not staged at ../sdk, the path go.mod names: %v", err)
	}
	if _, err := os.Stat(filepath.Join(staged, "go.mod")); err != nil {
		t.Fatalf("the extension is not staged: %v", err)
	}
}

// Two different directories that would stage at one path fail staging by name
// instead of merging into one tree.
func TestRuntimeReplacementsStagedAtOnePathAreRefused(t *testing.T) {
	root := t.TempDir()
	extensionRoot, _ := writeRuntimeReplacementFixture(t, root)
	mustWriteRuntimeFile(t, filepath.Join(extensionRoot, "go.mod"), `module example.dev/extension

go 1.25

require (
	example.dev/sdk v0.0.0
	example.dev/lib v0.0.0
)

replace example.dev/sdk => ../sdk

replace example.dev/lib => ../lib
`, 0o644)
	// ../lib is a link to a tree whose own ../sdk is another directory: staged
	// beside the extension, both would land at ../sdk.
	other := t.TempDir()
	mustWriteRuntimeFile(t, filepath.Join(other, "lib", "go.mod"), "module example.dev/lib\n\ngo 1.25\n\nreplace example.dev/sdk => ../sdk\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(other, "sdk", "go.mod"), "module example.dev/sdk\n\ngo 1.25\n", 0o644)
	if err := dirlink.Create(filepath.Join(other, "lib"), filepath.Join(root, "lib")); err != nil {
		t.Fatal(err)
	}
	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
	inputs, err := collectRuntimeInputs(extensionRoot, ext.Runtime.Prepare.Inputs)
	if err != nil {
		t.Fatal(err)
	}

	_, err = stageRuntimeSourceView(extensionRoot, t.TempDir(), inputs)
	if err == nil || !strings.Contains(err.Error(), "both stage at") {
		t.Fatalf("stageRuntimeSourceView error = %v, want two sources refused at one staged path", err)
	}
}
