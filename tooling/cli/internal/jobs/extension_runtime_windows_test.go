//go:build windows

package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// A directory junction inside a replaced module is refused by the digest and
// by the staging walk alike, with its name, instead of being left out of both.
func TestRuntimeReplacementJunctionIsRefusedByDigestAndStaging(t *testing.T) {
	root := t.TempDir()
	extensionRoot, sdkRoot := writeRuntimeReplacementFixture(t, root)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "docs", "NOTICE"), "notice\n", 0o644)
	if err := dirlink.Create("docs", filepath.Join(sdkRoot, "alias")); err != nil {
		t.Fatal(err)
	}
	const want = `"alias" is a directory junction`

	ext := runtimeTestExtension(extensionRoot)
	ext.Runtime.Prepare.Inputs = append(ext.Runtime.Prepare.Inputs, "go.mod")
	if _, err := extensionRuntimeDigest(ext); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("digest error = %v, want a refusal containing %s", err, want)
	}
	inputs, err := collectRuntimeInputs(extensionRoot, ext.Runtime.Prepare.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	viewRoot := t.TempDir()
	if _, err := stageRuntimeSourceView(extensionRoot, viewRoot, inputs); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("staging error = %v, want a refusal containing %s", err, want)
	}
	if _, err := os.Lstat(filepath.Join(viewRoot, "sdk", "alias")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused junction reached the staged view: %v", err)
	}
}

// Staging a replaced module's file link without the symbolic-link privilege
// fails with an error that names Developer Mode and the source link.
func TestRuntimeFileLinkWithoutPrivilegeNamesDeveloperMode(t *testing.T) {
	if !symlinkPrivilegeMissing(&os.LinkError{Op: "symlink", Err: syscall.ERROR_PRIVILEGE_NOT_HELD}) {
		t.Fatal("ERROR_PRIVILEGE_NOT_HELD is not recognized as a missing symbolic-link privilege")
	}
	if symlinkPrivilegeMissing(&os.LinkError{Op: "symlink", Err: syscall.ERROR_ACCESS_DENIED}) {
		t.Fatal("ERROR_ACCESS_DENIED is reported as a missing symbolic-link privilege")
	}

	dir := t.TempDir()
	mustWriteRuntimeFile(t, filepath.Join(dir, "NOTICE"), "notice\n", 0o644)
	source := filepath.Join(dir, "source", "LICENSE")
	err := symlinkRuntimeFile(source)("NOTICE", filepath.Join(dir, "LICENSE"))
	if err == nil {
		t.Skip("this host grants symbolic links through Developer Mode or an administrator token")
	}
	if !errors.Is(err, syscall.ERROR_PRIVILEGE_NOT_HELD) || !strings.Contains(err.Error(), "Developer Mode") ||
		!strings.Contains(err.Error(), source) {
		t.Fatalf("file link error = %v, want the privilege error naming Developer Mode and %s", err, source)
	}
}

// An installed runtime shipped at its declared name without ".exe" is
// runtime.executable_missing on Windows, and the error names the Windows file
// the extension must provide.
func TestInstalledRuntimeWithoutExeSuffixIsMissingOnWindows(t *testing.T) {
	root := t.TempDir()
	mustWriteRuntimeFile(t, filepath.Join(root, "compiled", "putnami-script"), "#!/bin/sh\n", 0o755)
	ext := &extension.ExtensionDescription{
		Name:    "@putnami/script",
		Version: "1.0.0",
		Path:    root,
		Runtime: &extension.RuntimeDefinition{Executable: "compiled/putnami-script"},
	}

	_, err := prepareOrLoadExtensionRuntime(context.Background(), nil, ext)
	var runtimeErr *extensionRuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.code != extensionproto.FailureRuntimeExecutableMissing {
		t.Fatalf("error = %v, want %s", err, extensionproto.FailureRuntimeExecutableMissing)
	}
	if !strings.Contains(err.Error(), "compiled/putnami-script.exe") || !strings.Contains(err.Error(), "without the .exe suffix") {
		t.Fatalf("error = %v, want it to name compiled/putnami-script.exe", err)
	}
}
