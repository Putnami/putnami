package cachekey

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.putnami.dev/tooling/cli/internal/extension"
)

// The extensions one synchronization digests replace the same module: one
// reading of its tree gives each of them the digest a fresh read gives, and a
// later reading sees an edit the earlier one held.
func TestOneReadingOfAReplacedModuleDigestsEveryExtensionThatReplacesIt(t *testing.T) {
	root := t.TempDir()
	extensionRoot, sdkRoot := writeRuntimeReplacementFixture(t, root)
	otherRoot := filepath.Join(root, "other")
	mustWriteRuntimeFile(t, filepath.Join(otherRoot, "go.mod"), "module example.dev/other\n\ngo 1.25\n\nrequire example.dev/sdk v0.0.0\n\nreplace example.dev/sdk => ../sdk\n", 0o644)
	mustWriteRuntimeFile(t, filepath.Join(otherRoot, "bin", "prepare"), "#!/bin/sh\n", 0o755)
	mustWriteRuntimeFile(t, filepath.Join(otherRoot, "cmd", "main.go"), "package main\n", 0o644)
	first, second := runtimeTestExtension(extensionRoot), runtimeTestExtension(otherRoot)
	second.Name = "other"

	reading := WithRuntimeTreeReading(context.Background())
	for _, ext := range []*extension.ExtensionDescription{first, second} {
		fresh, err := ExtensionRuntimeDigest(ext)
		if err != nil {
			t.Fatal(err)
		}
		read, err := ExtensionRuntimeDigestIn(reading, ext)
		if err != nil || read != fresh {
			t.Fatalf("%s: the reading's digest %q (%v), a fresh read %q", ext.Name, read, err, fresh)
		}
	}

	before, _ := ExtensionRuntimeDigest(second)
	mustWriteRuntimeFile(t, filepath.Join(sdkRoot, "sdk.go"), "package sdk\n\nconst Edited = true\n", 0o644)
	if held, err := ExtensionRuntimeDigestIn(reading, second); err != nil || held != before {
		t.Fatalf("the reading's digest after the edit = %q (%v), want the one it read %q", held, err, before)
	}
	after, err := ExtensionRuntimeDigestIn(WithRuntimeTreeReading(context.Background()), second)
	if err != nil || after == before {
		t.Fatalf("a new reading's digest = %q (%v), want the edit seen", after, err)
	}
	if fresh, _ := ExtensionRuntimeDigest(second); fresh != after {
		t.Fatalf("a new reading's digest %q, a fresh read %q", after, fresh)
	}

	// A tree whose walk fails is read again by each digest of the reading.
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory without permissions")
	}
	broken := WithRuntimeTreeReading(context.Background())
	if err := os.Chmod(sdkRoot, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sdkRoot, 0o755) })
	if _, err := ExtensionRuntimeDigestIn(broken, first); err == nil {
		t.Fatal("an unreadable replaced module was digested")
	}
	if err := os.Chmod(sdkRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := ExtensionRuntimeDigestIn(broken, first); err != nil || got == "" {
		t.Fatalf("the reading kept the failure: %q (%v)", got, err)
	}
}
