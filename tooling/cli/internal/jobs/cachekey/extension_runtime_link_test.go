package cachekey

import (
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/dirlink"
)

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

	before, err := ExtensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteRuntimeFile(t, filepath.Join(physicalSDK, "sdk.go"), "package sdk\n\nconst Edited = true\n", 0o644)
	after, err := ExtensionRuntimeDigest(ext)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("an edit behind the replaced module's directory link did not change the runtime digest")
	}
}
